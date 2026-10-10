// Package secretshape decides whether a config file carries a secret in
// the clear: a literal value under a secret-shaped key. internal/gitsync
// refuses to push such a file and internal/differ masks it in a published
// diff, so both read the same rule from here.
//
// It reports key paths only. No function here returns, logs or formats a
// value, so a caller can put its answer in an event, an error or the
// dashboard as it is.
package secretshape

import (
	"bytes"
	"errors"
	"io"
	"path"
	"regexp"
	"strconv"
	"strings"

	yaml "go.yaml.in/yaml/v3"
)

// KeyRegex is the set of mapping keys whose values are secrets. Anchored
// and case-insensitive on a whole key, since matching a substring would
// catch "monkey" and "keyboard"; the alternation accepts a secret word only
// as a full underscore-separated suffix.
//
// "pin" is deliberately absent: in this domain a pin is a GPIO number, and
// a real PIN belongs in secrets.yaml behind a !secret reference.
const KeyRegex = `(?i)^(password|passwd|pwd|secret|secrets|token|credential|credentials|auth|authorization|psk|keys?|api_?key|.*_(password|passwd|pwd|secret|token|keys?|credential|credentials|psk|auth))$`

// keyRe is KeyRegex compiled once.
var keyRe = regexp.MustCompile(KeyRegex)

// IsSecretKey reports whether key names a secret value.
func IsSecretKey(key string) bool {
	return keyRe.MatchString(key)
}

// stringRefRe matches a value written as a reference in a plain string
// rather than a tag: Zigbee2MQTT's "'!secret password'" and
// "'!secret.yaml password'" forms, which name a key in another file.
var stringRefRe = regexp.MustCompile(`(?i)^!(secret|[^\s!]+\.ya?ml)\s+\S+\s*$`)

// legacyBooleans are the plain scalars YAML 1.1 reads as booleans and 1.2
// reads as strings. Home Assistant parses 1.1, so "auth: yes" is a flag
// there, not a secret.
var legacyBooleans = map[string]bool{
	"y": true, "n": true,
	"yes": true, "no": true,
	"on": true, "off": true,
}

// Literals returns the key paths ("wifi.password", "users[0].token") in
// data, the content of the file at repository-relative relPath, that hold
// a literal value under a secret-shaped key, in file order and without
// duplicates. nil means there are none.
//
// YAML and JSON are recognized by extension and parsed. An extensionless
// file is read as KEY=value lines, the shape wmbusmeters writes its meter
// definitions in. Every other file is not inspected.
//
// A value is NOT a literal when it is a reference to the secret rather
// than the secret itself: any custom tag ("!secret wifi_pw", "!env_var
// TOKEN", "!include"), a string written as Zigbee2MQTT's "!secret key" or
// "!secret.yaml key", or a value that holds nothing (null, an empty string,
// a boolean). A YAML or JSON file that does not parse fails closed: every
// secret-shaped key on a line of its own is reported, since nothing can
// tell where its value ends.
func Literals(relPath string, data []byte) []string {
	switch ext := strings.ToLower(path.Ext(strings.ReplaceAll(relPath, `\`, "/"))); ext {
	case ".yaml", ".yml", ".json":
		return documentLiterals(data)
	case "":
		return assignmentLiterals(data)
	default:
		return nil
	}
}

// documentLiterals is Literals for YAML and JSON. JSON needs no path of
// its own: it is a YAML subset, and the walk only reads keys and scalars.
func documentLiterals(data []byte) []string {
	docs, err := parseDocuments(data)
	if err != nil {
		return lineLiterals(data)
	}
	var found keyList
	for _, doc := range docs {
		walk(doc, "", &found)
	}
	return found.keys
}

// parseDocuments decodes every YAML document in data as a node tree, tags
// intact. The whole stream, not just the first document: a secret in the
// second half must not sail past.
func parseDocuments(data []byte) ([]*yaml.Node, error) {
	var docs []*yaml.Node
	dec := yaml.NewDecoder(bytes.NewReader(data))
	for {
		var doc yaml.Node
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			return docs, nil
		}
		if err != nil {
			return nil, err
		}
		docs = append(docs, &doc)
	}
}

// walk records every secret-shaped key under node whose value is a
// literal. Aliases are not followed into: the node an alias points at is
// walked where its anchor sits, and following them would let a crafted
// file expand without bound.
func walk(node *yaml.Node, at string, found *keyList) {
	if node == nil {
		return
	}
	switch node.Kind {
	case yaml.DocumentNode:
		for _, child := range node.Content {
			walk(child, at, found)
		}
	case yaml.MappingNode:
		// Mapping content alternates key, value, key, value.
		for i := 0; i+1 < len(node.Content); i += 2 {
			key, value := node.Content[i], node.Content[i+1]
			name := ""
			if key.Kind == yaml.ScalarNode {
				name = key.Value
			}
			child := joinKey(at, name)
			if name != "" && IsSecretKey(name) && isLiteral(value) {
				found.add(child)
			}
			walk(value, child, found)
		}
	case yaml.SequenceNode:
		for i, item := range node.Content {
			walk(item, at+"["+strconv.Itoa(i)+"]", found)
		}
	}
}

// joinKey extends a key path by one mapping key.
func joinKey(at, name string) string {
	if at == "" {
		return name
	}
	return at + "." + name
}

// isLiteral reports whether a secret-shaped key's value is the secret
// itself: a literal scalar, or a list holding one. A nested mapping is
// not ("auth:" introduces a provider block); its own keys are walked.
func isLiteral(node *yaml.Node) bool {
	node = deref(node)
	if node == nil {
		return false
	}
	switch node.Kind {
	case yaml.ScalarNode:
		return isLiteralScalar(node)
	case yaml.SequenceNode:
		for _, item := range node.Content {
			if item = deref(item); item != nil && item.Kind == yaml.ScalarNode && isLiteralScalar(item) {
				return true
			}
		}
	}
	return false
}

// isLiteralScalar is isLiteral for one scalar.
func isLiteralScalar(node *yaml.Node) bool {
	switch {
	case isCustomTag(node.Tag):
		return false
	case node.Tag == "!!null" || node.Tag == "!!bool":
		return false
	case node.Style == 0 && legacyBooleans[strings.ToLower(node.Value)]:
		return false
	case strings.TrimSpace(node.Value) == "":
		return false
	case stringRefRe.MatchString(node.Value):
		return false
	}
	return true
}

// isCustomTag reports whether tag is application-defined (one leading "!")
// rather than one of YAML's own "!!str"/"!!int" family. Matching the shape
// rather than a list of Home Assistant's tags also covers whatever a
// custom component invents.
func isCustomTag(tag string) bool {
	return strings.HasPrefix(tag, "!") && !strings.HasPrefix(tag, "!!")
}

// deref follows an alias to what it points at. Bounded, against a node
// graph this package did not build.
func deref(node *yaml.Node) *yaml.Node {
	for i := 0; node != nil && node.Kind == yaml.AliasNode && i < 10; i++ {
		node = node.Alias
	}
	if node != nil && node.Kind == yaml.AliasNode {
		return nil
	}
	return node
}

// assignmentLiterals is Literals for an extensionless file: every
// "KEY=value" line (an "export " prefix allowed) whose key is secret-shaped
// and whose value is not empty. Lines that are not assignments are
// skipped, so a file that is not a list of settings at all reports
// nothing unless it also carries one of those lines.
func assignmentLiterals(data []byte) []string {
	var found keyList
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(strings.TrimSuffix(raw, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		if IsSecretKey(key) && strings.Trim(strings.TrimSpace(value), `"'`) != "" {
			found.add(key)
		}
	}
	return found.keys
}

// quotedKeyRe finds a quoted mapping key anywhere in a line, which is how a
// key hides inside compact JSON that no parser would accept.
var quotedKeyRe = regexp.MustCompile(`["']([A-Za-z0-9_.-]+)["']\s*:\s*(.?)`)

// lineLiterals is the fail-closed reading of a YAML or JSON file that does
// not parse: each line's "key: value", and each quoted key inside one, is
// reported when the key is secret-shaped and a value follows that is not
// a tag. Key names only, without a path, since there is no tree.
func lineLiterals(data []byte) []string {
	var found keyList
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if key, value, ok := strings.Cut(strings.TrimPrefix(line, "- "), ":"); ok {
			key = strings.Trim(strings.TrimSpace(key), `"'`)
			value = strings.TrimSpace(value)
			if IsSecretKey(key) && value != "" && !strings.HasPrefix(value, "!") && !strings.HasPrefix(value, "#") {
				found.add(key)
			}
		}
		for _, m := range quotedKeyRe.FindAllStringSubmatch(line, -1) {
			if IsSecretKey(m[1]) && m[2] != "!" {
				found.add(m[1])
			}
		}
	}
	return found.keys
}

// keyList collects key paths in first-seen order, without duplicates.
type keyList struct {
	keys []string
	seen map[string]bool
}

func (l *keyList) add(key string) {
	if l.seen == nil {
		l.seen = map[string]bool{}
	}
	if l.seen[key] {
		return
	}
	l.seen[key] = true
	l.keys = append(l.keys, key)
}
