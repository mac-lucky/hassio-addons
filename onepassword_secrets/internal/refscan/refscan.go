// Package refscan finds every place the live config uses a secret: the
// `!secret key` tags Home Assistant and ESPHome resolve, the
// `'!secret key'` strings Zigbee2MQTT resolves, and (in addons.go) the
// `!secret key` / `secret://key` values in other add-ons' options. The
// result drives two things: which secrets file a key is written to, and
// what has to be reloaded or restarted when its value changes.
package refscan

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Kind tells how a reference is resolved.
type Kind int

const (
	// TagRef is a YAML `!secret key` tag, resolved by Home Assistant and
	// ESPHome from the nearest secrets.yaml walking up from the file.
	TagRef Kind = iota
	// StringRef is a string value "!secret key" or "!<file>.yaml key",
	// resolved by Zigbee2MQTT from that file in its own directory.
	StringRef
)

// Ref is one use of a key in a config file.
type Ref struct {
	Key  string
	File string // config-relative, forward slashes
	Line int
	Kind Kind
	// Target is, for a StringRef, the config-relative secrets file it
	// names (dir/secret.yaml for "!secret key"). Empty for a TagRef: its
	// file is decided by walking up against the configured targets.
	Target string
	// Domain is the Home Assistant integration whose config holds the
	// reference, found through configuration.yaml's include graph. Empty
	// for files Home Assistant does not load (ESPHome, Zigbee2MQTT).
	Domain string
}

// Result is one scan.
type Result struct {
	Refs []Ref
	// Files is how many YAML files were read.
	Files int
	// Fingerprint changes whenever any scanned file's name, size or mtime
	// does; Changed compares it to skip a rescan.
	Fingerprint string
	// Warnings are files that could not be read or parsed; their refs
	// were still collected by a line scan where possible.
	Warnings []string
}

// Limits keep a scan bounded on a config tree full of surprises.
const (
	maxFileBytes = 2 << 20
	maxFiles     = 20000
)

// skipDirs are directories never descended into: runtime state, caches,
// downloaded code. Hidden directories are skipped as a rule.
var skipDirs = map[string]bool{
	"custom_components": true,
	"www":               true,
	"deps":              true,
	"tts":               true,
	"backups":           true,
	"node_modules":      true,
	"__pycache__":       true,
	"image":             true,
}

// skipPaths are config-relative directories never descended into:
// esphome/archive is where the ESPHome dashboard moves the configuration
// of a deleted device, which nothing compiles any more.
var skipPaths = map[string]bool{
	"esphome/archive": true,
}

var (
	stringRefRe = regexp.MustCompile(`^!(secret|[A-Za-z0-9_.-]+\.ya?ml)\s+(\S+)\s*$`)
	lineTagRe   = regexp.MustCompile(`!secret\s+([A-Za-z0-9_.-]+)`)
)

// Options configures a scan.
type Options struct {
	// Root is the config directory, /homeassistant in the add-on.
	Root string
	// Skip reports whether a config-relative file must not be read (the
	// rendered secrets files themselves).
	Skip func(rel string) bool
}

// Fingerprint walks the tree without parsing anything and returns what
// Result.Fingerprint would be, so a caller can skip a full scan when
// nothing moved.
func Fingerprint(o Options) (string, error) {
	files, err := walk(o)
	if err != nil {
		return "", err
	}
	return fingerprintOf(files), nil
}

type fileInfo struct {
	rel   string
	size  int64
	mtime int64
}

func walk(o Options) ([]fileInfo, error) {
	var files []fileInfo
	err := filepath.WalkDir(o.Root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == o.Root {
				return err
			}
			return nil
		}
		rel, relErr := filepath.Rel(o.Root, p)
		if relErr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if p != o.Root && (strings.HasPrefix(d.Name(), ".") || skipDirs[d.Name()] || skipPaths[rel]) {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || !isYAML(d.Name()) || strings.HasPrefix(d.Name(), ".") {
			return nil
		}
		if o.Skip != nil && o.Skip(rel) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		files = append(files, fileInfo{rel: rel, size: info.Size(), mtime: info.ModTime().UnixNano()})
		if len(files) >= maxFiles {
			return fs.SkipAll
		}
		return nil
	})
	sort.Slice(files, func(i, j int) bool { return files[i].rel < files[j].rel })
	return files, err
}

func fingerprintOf(files []fileInfo) string {
	h := sha256.New()
	for _, f := range files {
		_, _ = fmt.Fprintf(h, "%s\x00%d\x00%d\n", f.rel, f.size, f.mtime)
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

func isYAML(name string) bool {
	ext := strings.ToLower(path.Ext(name))
	return ext == ".yaml" || ext == ".yml"
}

// Scan reads every YAML file under o.Root and collects its references.
func Scan(o Options) (Result, error) {
	files, err := walk(o)
	if err != nil {
		return Result{}, err
	}
	res := Result{Files: len(files), Fingerprint: fingerprintOf(files)}
	docs := make(map[string]*yaml.Node, len(files))
	for _, f := range files {
		if f.size > maxFileBytes {
			res.Warnings = append(res.Warnings, f.rel+": over 2 MB, not scanned")
			continue
		}
		data, err := os.ReadFile(filepath.Join(o.Root, filepath.FromSlash(f.rel))) // #nosec G304 -- walked under Root
		if err != nil {
			res.Warnings = append(res.Warnings, f.rel+": unreadable")
			continue
		}
		var doc yaml.Node
		if err := yaml.Unmarshal(data, &doc); err != nil {
			// Not valid YAML to this parser (HA accepts a few things
			// yaml.v3 does not). A line scan still finds tag refs.
			res.Warnings = append(res.Warnings, f.rel+": not parsable as YAML, line-scanned")
			res.Refs = append(res.Refs, lineScan(f.rel, data)...)
			continue
		}
		docs[f.rel] = &doc
	}

	domains := domainMap(docs)
	for _, f := range files {
		doc := docs[f.rel]
		if doc == nil {
			continue
		}
		collect(doc, f.rel, domains, &res.Refs)
	}
	sort.Slice(res.Refs, func(i, j int) bool {
		a, b := res.Refs[i], res.Refs[j]
		if a.File != b.File {
			return a.File < b.File
		}
		return a.Line < b.Line
	})
	return res, nil
}

// fileDomains maps a config-relative file to how its content maps onto
// integration domains.
type fileDomains struct {
	// domain is the one domain the whole file belongs to ("" when the
	// file's own top-level keys are domains, as in a package).
	domain string
	// topLevel marks a file whose top-level keys are domains:
	// configuration.yaml itself and package files.
	topLevel bool
	// packageMerge marks a !include_dir_merge_named package file: its
	// top-level keys are package names, one level down are domains.
	packageMerge bool
}

// domainMap walks configuration.yaml's includes and returns, per file,
// which domain its references belong to.
func domainMap(docs map[string]*yaml.Node) map[string]fileDomains {
	out := map[string]fileDomains{}
	const root = "configuration.yaml"
	doc := docs[root]
	if doc == nil {
		return out
	}
	out[root] = fileDomains{topLevel: true}
	top := mappingOf(doc)
	if top == nil {
		return out
	}
	for i := 0; i+1 < len(top.Content); i += 2 {
		key := top.Content[i].Value
		if key == "homeassistant" {
			markPackages(top.Content[i+1], root, docs, out)
		}
		markIncludes(top.Content[i+1], root, key, docs, out, 0)
	}
	return out
}

// markPackages finds homeassistant: packages: and marks its files.
func markPackages(ha *yaml.Node, from string, docs map[string]*yaml.Node, out map[string]fileDomains) {
	m := ha
	if m.Kind == yaml.DocumentNode && len(m.Content) > 0 {
		m = m.Content[0]
	}
	if m.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value != "packages" {
			continue
		}
		pk := m.Content[i+1]
		switch {
		case pk.Kind == yaml.ScalarNode && strings.HasPrefix(pk.Tag, "!include_dir"):
			for _, f := range includedFiles(from, pk, docs) {
				out[f] = fileDomains{topLevel: pk.Tag == "!include_dir_named" || pk.Tag == "!include_dir_list", packageMerge: pk.Tag == "!include_dir_merge_named"}
			}
		case pk.Kind == yaml.MappingNode:
			for j := 0; j+1 < len(pk.Content); j += 2 {
				v := pk.Content[j+1]
				if v.Kind == yaml.ScalarNode && v.Tag == "!include" {
					for _, f := range includedFiles(from, v, docs) {
						out[f] = fileDomains{topLevel: true}
					}
				}
			}
		}
	}
}

// markIncludes gives every file included under one top-level key that
// key's domain, following nested includes. depth bounds a cycle.
func markIncludes(n *yaml.Node, from, domain string, docs map[string]*yaml.Node, out map[string]fileDomains, depth int) {
	if n == nil || depth > 8 {
		return
	}
	if n.Kind == yaml.ScalarNode && strings.HasPrefix(n.Tag, "!include") {
		for _, f := range includedFiles(from, n, docs) {
			if fd, ok := out[f]; ok && (fd.topLevel || fd.packageMerge) {
				continue // a package file keeps its own domains
			}
			if _, seen := out[f]; seen {
				continue
			}
			out[f] = fileDomains{domain: domain}
			if d := docs[f]; d != nil {
				markIncludes(d, f, domain, docs, out, depth+1)
			}
		}
		return
	}
	for _, c := range n.Content {
		markIncludes(c, from, domain, docs, out, depth)
	}
}

// includedFiles resolves an include tag's path against the including
// file's directory to the scanned files it names.
func includedFiles(from string, n *yaml.Node, docs map[string]*yaml.Node) []string {
	target := path.Clean(path.Join(path.Dir(from), strings.TrimSpace(n.Value)))
	if strings.HasPrefix(target, "../") || target == ".." {
		return nil
	}
	if n.Tag == "!include" {
		if _, ok := docs[target]; ok {
			return []string{target}
		}
		return nil
	}
	var out []string
	for f := range docs {
		if strings.HasPrefix(f, target+"/") {
			out = append(out, f)
		}
	}
	sort.Strings(out)
	return out
}

// collect walks one parsed file and appends its references.
func collect(doc *yaml.Node, rel string, domains map[string]fileDomains, refs *[]Ref) {
	fd := domains[rel]
	top := mappingOf(doc)
	if top != nil && (fd.topLevel || fd.packageMerge) {
		for i := 0; i+1 < len(top.Content); i += 2 {
			if fd.packageMerge {
				inner := top.Content[i+1]
				if inner.Kind == yaml.MappingNode {
					for j := 0; j+1 < len(inner.Content); j += 2 {
						walkNode(inner.Content[j+1], rel, inner.Content[j].Value, refs)
					}
				}
				continue
			}
			walkNode(top.Content[i+1], rel, top.Content[i].Value, refs)
		}
		return
	}
	walkNode(doc, rel, fd.domain, refs)
}

func walkNode(n *yaml.Node, rel, domain string, refs *[]Ref) {
	if n == nil {
		return
	}
	if n.Kind == yaml.ScalarNode {
		if n.Tag == "!secret" {
			if key := strings.TrimSpace(n.Value); key != "" {
				*refs = append(*refs, Ref{Key: key, File: rel, Line: n.Line, Kind: TagRef, Domain: domain})
			}
			return
		}
		if n.Tag == "!!str" || n.Tag == "" {
			if m := stringRefRe.FindStringSubmatch(n.Value); m != nil {
				name := m[1]
				if name == "secret" {
					name = "secret.yaml"
				}
				*refs = append(*refs, Ref{
					Key: m[2], File: rel, Line: n.Line, Kind: StringRef,
					Target: path.Join(path.Dir(rel), name), Domain: domain,
				})
			}
		}
		return
	}
	for _, c := range n.Content {
		walkNode(c, rel, domain, refs)
	}
}

func lineScan(rel string, data []byte) []Ref {
	var refs []Ref
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 64<<10), maxFileBytes)
	line := 0
	for sc.Scan() {
		line++
		text := sc.Text()
		if i := strings.Index(text, "#"); i >= 0 && !strings.Contains(text[:i], "'") && !strings.Contains(text[:i], `"`) {
			text = text[:i]
		}
		for _, m := range lineTagRe.FindAllStringSubmatch(text, -1) {
			refs = append(refs, Ref{Key: m[1], File: rel, Line: line, Kind: TagRef})
		}
	}
	return refs
}

func mappingOf(doc *yaml.Node) *yaml.Node {
	n := doc
	if n.Kind == yaml.DocumentNode {
		if len(n.Content) == 0 {
			return nil
		}
		n = n.Content[0]
	}
	if n.Kind != yaml.MappingNode {
		return nil
	}
	return n
}

// TargetFor returns which of targets (config-relative secrets files) a
// reference resolves against: the StringRef's own file when configured,
// for a TagRef the nearest configured secrets.yaml walking up from the
// referencing file's directory. ok is false when none is configured.
func TargetFor(r Ref, targets []string) (string, bool) {
	set := make(map[string]bool, len(targets))
	for _, t := range targets {
		set[t] = true
	}
	if r.Kind == StringRef {
		return r.Target, set[r.Target]
	}
	dir := path.Dir(r.File)
	for {
		candidate := path.Join(dir, "secrets.yaml")
		if dir == "." {
			candidate = "secrets.yaml"
		}
		if set[candidate] {
			return candidate, true
		}
		if dir == "." || dir == "/" || dir == "" {
			return "", false
		}
		dir = path.Dir(dir)
	}
}
