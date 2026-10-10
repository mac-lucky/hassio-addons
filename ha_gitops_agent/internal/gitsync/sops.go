package gitsync

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"strings"

	yaml "go.yaml.in/yaml/v3"
)

// Versions before 0.9.0 could encrypt values with SOPS on their way into
// git and decrypt them on the way back. This one does neither: secrets live
// in 1Password and reach Home Assistant through secrets.yaml, which is
// never synced. A repository still holding SOPS ciphertext is therefore a
// hard stop, the same kind as a tracked secrets file - applying it would
// write ENC[...] strings into the live config.

// sopsValueMarker opens every value SOPS encrypts, in every format.
const sopsValueMarker = "ENC[AES256_GCM,"

// sopsGrepPatterns pick candidate files out of a whole tree in one "git
// grep" (extended regexes): an encrypted value, the flat dotenv/INI
// metadata keys, and a top-level YAML or JSON "sops" key. Candidates are
// confirmed by isSopsDocument, so a broad pattern costs a blob read, never
// a false refusal.
var sopsGrepPatterns = []string{
	`ENC\[AES256_GCM,`,
	`^[[:space:]]*sops_(mac|lastmodified)[[:space:]]*=`,
	`^sops[[:space:]]*:`,
	`"sops"[[:space:]]*:`,
}

// maxSopsFilesNamed bounds how many paths SopsTrackedError's message
// lists; Files keeps them all.
const maxSopsFilesNamed = 10

// SopsTrackedError is GuardSecretsAt's refusal for tracked files carrying
// SOPS metadata. A hard stop like SecretsTrackedError: no apply, no
// capture, no deletion until they are out of the tracked tree.
type SopsTrackedError struct {
	// Files is the sorted list of offending paths.
	Files []string
}

// Error names the files (paths only) and the way out. Complete on its own:
// recon prefixes only "refusing to sync: ".
func (e *SopsTrackedError) Error() string {
	named := e.Files
	more := ""
	if len(named) > maxSopsFilesNamed {
		more = fmt.Sprintf(" and %d more", len(named)-maxSopsFilesNamed)
		named = named[:maxSopsFilesNamed]
	}
	return "SOPS-encrypted files tracked in repository: " + strings.Join(named, ", ") + more +
		" - this version no longer decrypts SOPS files: move those values to 1Password" +
		" (the 1Password Secrets add-on renders secrets.yaml), reference them with !secret," +
		" then remove the encrypted files and .sops.yaml from the repository"
}

// sopsFilesAt returns the paths among files whose blob at sha carries SOPS
// metadata, sorted. One "git grep" over the tree finds candidates; each is
// then read and confirmed. Nothing is checked out.
func (g *GitSync) sopsFilesAt(ctx context.Context, sha string, files []string) ([]string, error) {
	if sha == "" || len(files) == 0 {
		return nil, nil
	}
	// No -I: it skips whatever the tree's .gitattributes calls binary, and
	// "*.yaml -diff" would let ciphertext through. Candidates are confirmed
	// by isSopsDocument anyway.
	args := []string{"grep", "-z", "-l", "-E"}
	for _, pattern := range sopsGrepPatterns {
		args = append(args, "-e", pattern)
	}
	args = append(args, sha, "--")
	result, err := g.runGitRaw(ctx, args, "", nil, 0)
	if err != nil {
		return nil, err
	}
	switch result.ExitCode {
	case 0:
	case 1:
		// git grep's "nothing matched".
		return nil, nil
	default:
		return nil, newCommandError("git grep failed (exit %d): %s", result.ExitCode, g.redactCredentials(strings.TrimSpace(result.Stderr)))
	}

	wanted := make(map[string]bool, len(files))
	for _, f := range files {
		wanted[f] = true
	}
	var found []string
	// With a tree-ish, -z -l prints "<sha>:<path>\0" per file, the path
	// verbatim.
	for _, entry := range strings.Split(result.Stdout, "\x00") {
		p := strings.TrimPrefix(entry, sha+":")
		if p == "" || p == entry || !wanted[p] {
			continue
		}
		blob, err := g.runGitRaw(ctx, []string{"show", sha + ":" + p}, "", nil, 0)
		if err != nil {
			return nil, err
		}
		if blob.ExitCode == 0 && isSopsDocument([]byte(blob.Stdout)) {
			found = append(found, p)
		}
	}
	sort.Strings(found)
	return found, nil
}

// isSopsDocument reports whether data carries SOPS metadata in any of the
// shapes SOPS writes: an encrypted value, a flat "sops_mac=" or
// "sops_lastmodified=" line (dotenv and INI), or a top-level "sops" mapping
// holding a mac or a lastmodified (YAML and JSON). The last is parsed
// rather than sniffed, so a config that merely has a key called sops
// somewhere is not refused.
func isSopsDocument(data []byte) bool {
	if bytes.Contains(data, []byte(sopsValueMarker)) {
		return true
	}
	// Every other shape spells "sops"; without it there is nothing to parse.
	if !bytes.Contains(data, []byte("sops")) {
		return false
	}
	for _, raw := range strings.Split(string(data), "\n") {
		if key, _, ok := strings.Cut(strings.TrimSpace(raw), "="); ok {
			switch strings.TrimSpace(key) {
			case "sops_mac", "sops_lastmodified":
				return true
			}
		}
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return false
	}
	root := &doc
	if root.Kind == yaml.DocumentNode && len(root.Content) == 1 {
		root = root.Content[0]
	}
	meta := mappingValue(root, "sops")
	if meta == nil || meta.Kind != yaml.MappingNode {
		return false
	}
	return mappingValue(meta, "mac") != nil || mappingValue(meta, "lastmodified") != nil
}

// mappingValue returns the value node for key in a mapping, or nil if node
// is not a mapping or has no such key.
func mappingValue(node *yaml.Node, key string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}
