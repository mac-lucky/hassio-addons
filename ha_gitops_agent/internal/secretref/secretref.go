// Package secretref resolves "secret://<name>" references declared in a
// gitops manifest against the LIVE Home Assistant secrets file, so a
// credential never has to be written into the repository: the manifest
// carries the reference, secrets.yaml on the box carries the value (the
// repository's own copy stays SOPS-encrypted, see internal/sopscrypt).
//
// Three layers use it - internal/flows, internal/subentries and
// internal/addonopts - each resolving at PLAN time on a COPY of the
// declared data. The split is the whole point:
//
//   - the resolved copy is hashed and sent to Home Assistant, so rotating
//     a secret changes the hash and the layer converges onto the new value;
//   - the UNRESOLVED original is what gets persisted (state.json, the
//     per-apply stash), so neither ever holds the credential itself.
//
// This package never renders a resolved value: every error it returns
// names only the secrets key, the file or the reference. Callers that put
// a resolved value on the wire carry it in registries.RegOp.Secrets so
// their own failures can be scrubbed of it (internal/regapply's
// redactValues).
package secretref

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/difftext"
	yaml "go.yaml.in/yaml/v3"
)

// Scheme prefixes every reference. A declared string is a reference only
// when it starts with this AND names something after it - see isRef.
const Scheme = "secret://"

// fileName is the live secrets file inside the config root - exactly the
// file Home Assistant's own "!secret" tag reads, so a reference and a
// "!secret" always mean the same value.
const fileName = "secrets.yaml"

// isRef reports whether s is a well-formed "secret://<name>" reference.
// The name is permissive (a secrets.yaml key is an ordinary YAML key) but
// must be non-empty and free of whitespace and control characters, and is
// never trimmed - a stray space is malformed, not a different key.
func isRef(s string) bool {
	if !hasScheme(s) {
		return false
	}
	name := s[len(Scheme):]
	if name == "" {
		return false
	}
	for _, r := range name {
		if r <= ' ' || r == 0x7f {
			return false
		}
	}
	return true
}

// hasScheme reports whether s is WRITTEN as a reference, well-formed or
// not. Kept apart from isRef so "secret://" with nothing after it is an
// actionable error rather than a literal that reaches HA as a password.
func hasScheme(s string) bool { return strings.HasPrefix(s, Scheme) }

// RefAt returns m[key] and true when that value is, or contains, a
// reference - the predicate internal/addonopts masks a rendered plan line
// with and internal/regapply keeps the resolved credential out of the
// rollback stash with. Nil map, absent key and plain value are all false.
func RefAt(m map[string]any, key string) (any, bool) {
	value, present := m[key]
	if !present || !ContainsRef(value) {
		return nil, false
	}
	return value, true
}

// UnresolvedMessage is the one sentence every planner reports when a
// declared payload names a secret the live file could not answer for, so
// the three layers cannot drift into three phrasings. noun and id are the
// layer's own ("integration", "subentry"). A string, not an error, because
// callers fold it into a per-item error op.
func UnresolvedMessage(noun, id string, err error) string {
	return fmt.Sprintf("declared data for %s '%s' references a secret that could not be resolved: %v", noun, id, err)
}

// ContainsRef reports whether v is, or contains at any depth, a value
// written as a reference. Callers rendering a declared value use it to
// decide the value must be masked: a diff line reaches the dashboard and
// the log, and a reference resolves to a credential. Malformed references
// count too - they never resolve, so masking one costs nothing.
//
// map[any]any is walked alongside map[string]any because yaml/v3 decodes a
// mapping with any non-string key that way, and declared data is only
// validated as map[string]any at its top two levels: "ports: {1883:
// secret://x}" three levels down is the shape.
func ContainsRef(v any) bool {
	switch value := v.(type) {
	case string:
		return hasScheme(value)
	case map[string]any:
		for _, inner := range value {
			if ContainsRef(inner) {
				return true
			}
		}
	case map[any]any:
		for _, inner := range value {
			if ContainsRef(inner) {
				return true
			}
		}
	case []any:
		for _, inner := range value {
			if ContainsRef(inner) {
				return true
			}
		}
	}
	return false
}

// Resolver reads the live secrets file at most ONCE per instance, caching
// a successful parse and a failure alike. One instance per reconcile cycle
// (internal/recon's reconcileNow), shared by every layer: a second read
// mid-cycle could resolve two references to two generations of the file,
// and the hashes derived from them would disagree.
//
// The zero Resolver is not usable; a nil *Resolver is, and refuses every
// reference with an error rather than passing one through unresolved.
type Resolver struct {
	path string

	mu     sync.Mutex
	loaded bool
	loads  int
	values map[string]*yaml.Node
	err    error
}

// NewResolver returns a Resolver reading <configRoot>/secrets.yaml - the
// LIVE file, never the repository's encrypted copy.
//
// Symlinks are followed deliberately, unlike internal/differ and
// internal/applier: their escape guard exists because those paths come
// from the REPOSITORY. Nothing here is repository-controlled (two fixed
// components, a read), and Home Assistant's own "!secret" opens the same
// path, so a guard could only refuse a layout HA itself accepts.
func NewResolver(configRoot string) *Resolver {
	return &Resolver{path: filepath.Join(configRoot, fileName)}
}

// LoadCount is how many times the secrets file was actually read, so the
// once-per-cycle contract is assertable: a whole cycle must leave it at 1.
func (r *Resolver) LoadCount() int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.loads
}

// resolve returns the value secrets.yaml holds under name, as the file
// spells it. Each failure mode gets its own message, and none of them ever
// quotes the value back - only the key, the file and the tag.
func (r *Resolver) resolve(name string) (string, error) {
	if r == nil {
		return "", fmt.Errorf("no secrets file is available to resolve %s%s", Scheme, name)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.loadLocked(); err != nil {
		return "", err
	}

	node, present := r.values[name]
	if !present {
		return "", fmt.Errorf("%s has no key '%s'", fileName, name)
	}
	return scalarValue(name, node)
}

// scalarTags are the YAML tags a reference can stand for; everything else,
// including every HA loader tag (!secret, !include, !env_var), is refused
// by name. This is why the file is read as nodes: decoding into a plain
// `any` DISCARDS an unknown tag and keeps the scalar text, so
// "extra: !include more_secrets.yaml" would resolve to the filename and
// submit it to an integration as the credential.
var scalarTags = map[string]bool{"!!str": true, "!!int": true, "!!float": true, "!!bool": true}

// scalarValue renders one secrets.yaml node as node.Value - the file's own
// bytes. Byte-for-byte on purpose: going through Go's types retypes the
// value ("007" -> 7, a long token loses its tail past float64's 15 digits,
// 1.50 -> 1.5), and a secret is a string to whatever consumes it.
func scalarValue(name string, node *yaml.Node) (string, error) {
	node = deref(node)
	switch {
	case node == nil:
		return "", fmt.Errorf("%s key '%s' has no usable value", fileName, name)
	case node.Kind != yaml.ScalarNode:
		return "", fmt.Errorf("%s key '%s' is not a scalar value", fileName, name)
	case node.Tag == "!!null":
		return "", fmt.Errorf("%s key '%s' is not a scalar value", fileName, name)
	case !scalarTags[node.Tag]:
		return "", fmt.Errorf("%s key '%s' carries the tag %s, which a secret reference cannot stand for", fileName, name, node.Tag)
	}
	return node.Value, nil
}

// deref follows an alias node ("b: *anchor") to what it points at, which
// would otherwise be refused as "not a scalar". Bounded rather than
// recursive, against a node graph this package did not build.
func deref(node *yaml.Node) *yaml.Node {
	for i := 0; node != nil && node.Kind == yaml.AliasNode && i < 10; i++ {
		node = node.Alias
	}
	return node
}

// loadLocked reads and parses the secrets file on first need; callers hold
// r.mu. The outcome is cached either way, so a missing file is not
// re-stat'ed once per reference.
func (r *Resolver) loadLocked() error {
	if r.loaded {
		return r.err
	}
	r.loaded = true
	r.loads++

	data, err := os.ReadFile(r.path) // #nosec G304 -- r.path is <configRoot>/secrets.yaml, built by NewResolver
	if err != nil {
		if os.IsNotExist(err) {
			r.err = fmt.Errorf("%s does not exist, so %s references cannot be resolved", r.path, Scheme)
		} else {
			r.err = fmt.Errorf("could not read %s: %w", r.path, err)
		}
		return r.err
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		r.err = fmt.Errorf("%s is not valid YAML: %w", r.path, err)
		return r.err
	}
	r.values = map[string]*yaml.Node{}
	if len(doc.Content) == 0 {
		// An empty file is an empty mapping, not a parse failure.
		return nil
	}
	root := deref(doc.Content[0])
	if root == nil || root.Kind != yaml.MappingNode {
		r.err = fmt.Errorf("%s must be a mapping of secret name to value", r.path)
		return r.err
	}
	for i := 0; i+1 < len(root.Content); i += 2 {
		key := deref(root.Content[i])
		// Indexed by the key's own text whatever its tag, so secret://1883
		// reaches an int key. A key with structure has no text and is
		// skipped rather than failing the whole file.
		if key == nil || key.Kind != yaml.ScalarNode || key.Value == "" {
			continue
		}
		r.values[key.Value] = root.Content[i+1]
	}
	return nil
}

// ResolveMap returns a copy of m with every reference replaced by the
// value it names, plus every distinct value substituted, sorted and
// deduplicated.
//
// m is never mutated at any depth: the caller's map is what gets
// persisted, and persisting a resolved credential is what this mechanism
// exists to avoid. A manifest declaring no reference gets its own map back
// rather than a deep copy, so the result may ALIAS m - callers only read,
// hash and send it.
func (r *Resolver) ResolveMap(m map[string]any) (map[string]any, []string, error) {
	if m == nil {
		return map[string]any{}, nil, nil
	}
	if !ContainsRef(m) {
		return m, nil, nil
	}

	found := map[string]bool{}
	resolved, err := r.resolveValue(m, found)
	if err != nil {
		return nil, nil, err
	}
	out, _ := resolved.(map[string]any)
	if out == nil {
		out = map[string]any{}
	}
	if len(found) == 0 {
		return out, nil, nil
	}
	return out, difftext.SortedKeys(found), nil
}

// resolveValue is ResolveMap's recursion. found accumulates the distinct
// values substituted so far.
func (r *Resolver) resolveValue(v any, found map[string]bool) (any, error) {
	switch value := v.(type) {
	case string:
		if !hasScheme(value) {
			return value, nil
		}
		if !isRef(value) {
			return nil, fmt.Errorf("'%s' is not a usable secret reference; write %s<name>", value, Scheme)
		}
		secret, err := r.resolve(value[len(Scheme):])
		if err != nil {
			return nil, err
		}
		if secret != "" {
			found[secret] = true
		}
		return secret, nil

	case map[string]any:
		out := make(map[string]any, len(value))
		for k, inner := range value {
			replaced, err := r.resolveValue(inner, found)
			if err != nil {
				return nil, err
			}
			out[k] = replaced
		}
		return out, nil

	case map[any]any:
		out := make(map[any]any, len(value))
		for k, inner := range value {
			replaced, err := r.resolveValue(inner, found)
			if err != nil {
				return nil, err
			}
			out[k] = replaced
		}
		return out, nil

	case []any:
		out := make([]any, len(value))
		for i, inner := range value {
			replaced, err := r.resolveValue(inner, found)
			if err != nil {
				return nil, err
			}
			out[i] = replaced
		}
		return out, nil
	}
	return v, nil
}
