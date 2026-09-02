package sopscrypt

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"path"
	"reflect"
	"regexp"
	"strings"

	yaml "go.yaml.in/yaml/v3"
)

// SecretKeyRegex is the set of mapping keys whose values are treated as
// secrets: passed to sops as --encrypted-regex and written into the managed
// .sops.yaml. Anchored and case-insensitive on a whole key, since matching
// a substring would catch "monkey" and "keyboard"; the alternation accepts
// a secret word only as a full underscore-separated suffix.
//
// "pin" is deliberately absent: in this domain a pin is a GPIO number, and
// a real PIN belongs in secrets.yaml where every value is encrypted anyway.
// A matching key is not sufficient on its own - NeedsEncryption also
// requires a plain scalar value, since sops destroys a "!secret" tag.
const SecretKeyRegex = `(?i)^(password|passwd|pwd|secret|secrets|token|credential|credentials|auth|authorization|psk|keys?|api_?key|.*_(password|passwd|pwd|secret|token|keys?|credential|credentials|psk|auth))$`

// secretKeyRe is SecretKeyRegex compiled once.
var secretKeyRe = regexp.MustCompile(SecretKeyRegex)

// secretsFileRe matches the basename Home Assistant reserves for the file
// every !secret reference resolves against.
var secretsFileRe = regexp.MustCompile(`(?i)^secrets\.ya?ml$`)

// IsSecretsFile reports whether p's basename is secrets.yaml (or .yml): the
// file encrypted whole rather than key-by-key, and the only secret-shaped
// path internal/gitsync tolerates in a repository once encryption is on.
func IsSecretsFile(p string) bool {
	return secretsFileRe.MatchString(path.Base(strings.ReplaceAll(p, `\`, "/")))
}

// IsYAMLFile reports whether p is a YAML file by extension - the one
// question that is about YAML rather than encryption, asked by
// internal/differ's line-oriented mask. To ask whether this package handles
// a path at all, use EncryptablePath.
func IsYAMLFile(p string) bool {
	ext := extOf(p)
	return ext == ".yaml" || ext == ".yml"
}

// extOf is p's lowercased extension, with backslash separators normalized
// first so a path that arrived in Windows form is read the same way.
func extOf(p string) string {
	return strings.ToLower(path.Ext(strings.ReplaceAll(p, `\`, "/")))
}

// Format is the sops store a file's content is handled through. FormatNone
// covers everything else, which is never encrypted, never decrypted and
// never compared as anything but bytes.
//
// Picking the wrong store is a different operation, not a degraded one: for
// a file it cannot place by extension, sops falls back to its BINARY store,
// base64s the WHOLE file into one "data" field, discards --encrypted-regex
// and emits JSON. So dotenv, the one format no extension announces, is
// claimed only for content that proves it (see FormatFor).
type Format string

const (
	FormatNone   Format = ""
	FormatYAML   Format = "yaml"
	FormatJSON   Format = "json"
	FormatDotenv Format = "dotenv"
)

// dotenvKeyLineRe matches the "KEY=" opening of one dotenv assignment. No
// leading whitespace: sops rejects an indented line ("invalid dotenv input
// line"), so accepting one would claim the format for a file sops refuses.
var dotenvKeyLineRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

// formatFromPath returns the format p's extension settles on its own, or
// FormatNone when only the content can decide - or nothing can.
func formatFromPath(p string) Format {
	switch extOf(p) {
	case ".yaml", ".yml":
		return FormatYAML
	case ".json":
		return FormatJSON
	}
	return FormatNone
}

// FormatFor reports how the file at repository-relative path relPath, whose
// plaintext content is data, is handled by this package.
//
// YAML and JSON go by extension, case-insensitively; sops's own inference
// is case-SENSITIVE, which is why the store is named on every call instead
// (see storeFlags). dotenv is announced by nothing in the path - the files
// that prompted it (wmbusmeters meter definitions) have no extension - so
// it is claimed conservatively: NO extension at all, every non-blank
// non-comment line a "KEY=value" assignment, and at least one key matching
// SecretKeyRegex. That last condition keeps a merely line-oriented file
// from being claimed, where the cost is a whole-file blob (see Format).
func FormatFor(relPath string, data []byte) Format {
	if f := formatFromPath(relPath); f != FormatNone {
		return f
	}
	if extOf(relPath) != "" || !looksLikeSecretDotenv(data) {
		return FormatNone
	}
	return FormatDotenv
}

// EncryptablePath reports whether relPath could be a file this package
// encrypts, judged from the path alone - the cheap pre-filter before a file
// read. Deliberately a SUPERSET of FormatFor (every extensionless path
// passes), so a true must still be confirmed by FormatFor or IsEncrypted.
func EncryptablePath(relPath string) bool {
	return formatFromPath(relPath) != FormatNone || extOf(relPath) == ""
}

// dotenvPairs returns data's assignment lines in file order, dropping blank
// lines and comments; ok=false means some other line was not an assignment,
// so the file is not dotenv. Lines come back raw, as sops round-trips them.
func dotenvPairs(data []byte) (pairs []string, ok bool) {
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSuffix(raw, "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if !dotenvKeyLineRe.MatchString(line) {
			return nil, false
		}
		pairs = append(pairs, line)
	}
	return pairs, true
}

// dotenvKey is the key half of one assignment line.
func dotenvKey(pair string) string {
	key, _, _ := strings.Cut(pair, "=")
	return key
}

// looksLikeSecretDotenv reports whether data is a dotenv file carrying a
// secret-shaped key - the content half of FormatFor's dotenv rule.
func looksLikeSecretDotenv(data []byte) bool {
	pairs, ok := dotenvPairs(data)
	if !ok {
		return false
	}
	for _, pair := range pairs {
		if secretKeyRe.MatchString(dotenvKey(pair)) {
			return true
		}
	}
	return false
}

// NeedsEncryption reports whether the file at repository-relative path
// relPath, whose current plaintext content is data, must be encrypted
// before it enters the git worktree. A file with no Format is never
// encrypted; per format see yamlNeedsEncryption, jsonNeedsEncryption and
// dotenvNeedsEncryption.
//
// need is true when the file holds secret material sops can encrypt and
// hand back unchanged. refusal, mutually exclusive with it, reads as the
// tail of "<path> <refusal>" and means the file holds something secret that
// cannot be encrypted safely: the caller must fail and quote it, since the
// alternatives are a plaintext secret on the remote or a corrupted config.
func NeedsEncryption(relPath string, data []byte) (need bool, refusal string) {
	switch FormatFor(relPath, data) {
	case FormatYAML:
		return yamlNeedsEncryption(relPath, data)
	case FormatJSON:
		return jsonNeedsEncryption(data)
	case FormatDotenv:
		return dotenvNeedsEncryption(data)
	default:
		// An extensionless file carrying a secret-shaped assignment that
		// did not qualify as dotenv (a stray prose line, a leading space,
		// an "export " prefix) is refused rather than committed in the
		// clear - unlike YAML, an ordinary content edit is enough to land
		// here. Keyed on the secret shape, so an extensionless README with
		// "title=notes" is still ignored.
		if extOf(relPath) == "" && hasSecretShapedAssignment(data) {
			if bytes.HasPrefix(data, utf8BOM) {
				// Named separately, and not stripped: sops reads a BOM as
				// part of the first key, so --encrypted-regex could not
				// match it and the value would stay in the clear while the
				// agent reported success.
				return false, "starts with a UTF-8 byte order mark, which SOPS reads as part of the first setting's " +
					"name, so its secret cannot be encrypted reliably: save the file without a BOM"
			}
			return false, "carries a secret-shaped setting but is not a plain list of KEY=value lines " +
				"(no indentation, no 'export', nothing but assignments and comments), so its secret can be " +
				"neither encrypted nor safely committed: fix the file, or move it out of the config directory"
		}
		return false, ""
	}
}

// utf8BOM is the byte order mark some editors write at the head of a file.
var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// hasSecretShapedAssignment reports whether any line assigns a value to a
// secret-shaped key, without requiring the whole file to be dotenv - the
// looser counterpart of looksLikeSecretDotenv, used only to decide whether
// an unplaceable file is refused or ignored.
func hasSecretShapedAssignment(data []byte) bool {
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(strings.TrimSuffix(raw, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, value, found := strings.Cut(line, "=")
		if !found || strings.TrimSpace(value) == "" {
			continue
		}
		if secretKeyRe.MatchString(strings.TrimSpace(key)) {
			return true
		}
	}
	return false
}

// jsonNeedsEncryption is NeedsEncryption for a .json file, sharing YAML's
// node walk because JSON is a YAML subset. Custom tags and the YAML 1.1
// boolean hazard cannot occur here, so those checks are not made. Refused:
// a root that is not an object ("SOPS only supports JSON files with a
// top-level object", exit 2), a top-level "sops" key (exit 203), and
// content failing json.Valid but carrying a secret-shaped line - a .json
// holding YAML would pass the parse and fail inside sops, which picks its
// store from the extension.
func jsonNeedsEncryption(data []byte) (need bool, refusal string) {
	docs, err := parseDocuments(data)
	if err != nil || !json.Valid(data) {
		// Fail closed, as the YAML path does: a file this package cannot
		// read is one it cannot clear for the remote.
		if hasSecretShapedLine(data) {
			return false, "does not parse as JSON, so it can be neither encrypted nor safely committed: fix the file"
		}
		return false, ""
	}
	if len(docs) == 0 {
		return false, ""
	}
	found := false
	for _, doc := range docs {
		if hasInlineSecret(doc) {
			found = true
			break
		}
	}
	if !found {
		return false, ""
	}
	for _, doc := range docs {
		root := documentRoot(doc)
		if root == nil || root.Kind != yaml.MappingNode {
			return false, "holds a secret value but its top level is not a JSON object, which SOPS cannot encrypt: " +
				`wrap it in an object, e.g. {"data": [...]}`
		}
		if mappingValue(root, "sops") != nil {
			return false, `has a top-level "sops" key, which SOPS reserves for its own metadata: rename that key`
		}
	}
	return true, ""
}

// dotenvNeedsEncryption is NeedsEncryption for an extensionless dotenv
// file: one assignment per line, so the scan is the parse. A SecretKeyRegex
// key with a non-empty value triggers; an empty value does not, since
// encrypting it would churn the file on every import for nothing.
//
// The one refusal is a pre-existing "sops_" key. A format without nesting
// has nowhere to put sops's metadata mapping, so sops spreads it across
// top-level "sops_" keys - and IsEncrypted reads a "sops_mac=" plus a
// "sops_version=" as ciphertext, so a plaintext file carrying both would be
// taken for encrypted and committed in the clear.
func dotenvNeedsEncryption(data []byte) (need bool, refusal string) {
	pairs, ok := dotenvPairs(data)
	if !ok {
		return false, ""
	}
	found := false
	for _, pair := range pairs {
		key, value, _ := strings.Cut(pair, "=")
		if secretKeyRe.MatchString(key) && value != "" {
			found = true
			break
		}
	}
	if !found {
		return false, ""
	}
	for _, pair := range pairs {
		if key := dotenvKey(pair); isSopsReservedKey(key) {
			return false, `has a "` + key + `" key, and SOPS reserves the whole "sops_" prefix for its own metadata ` +
				"in formats that cannot nest it: rename that key"
		}
	}
	return true, ""
}

// isSopsReservedKey reports whether a flat-format top-level key collides
// with the metadata sops writes.
func isSopsReservedKey(key string) bool {
	lower := strings.ToLower(key)
	return lower == "sops" || strings.HasPrefix(lower, "sops_")
}

// yamlNeedsEncryption is NeedsEncryption for a YAML file. need is true for
// secrets.yaml/.yml (encrypted whole) or for a SecretKeyRegex key at any
// depth whose value is a plain scalar or a list of them. A custom-tagged
// value ("password: !secret mqtt_pw") is NOT a trigger - sops would rewrite
// the node as an encrypted string and discard the tag - and neither is a
// nested structure, since "auth:" introduces an auth provider block.
//
// Refusals, each verified against the sops binary:
//
//   - a Home Assistant custom tag (!secret, !include*, !input) anywhere in
//     a file needing inline encryption; sops does not round-trip them.
//   - a root that is not a mapping, the normal shape of automations.yaml
//     (sops exits 2).
//   - an unquoted YAML 1.1 boolean (yes/no/on/off/y/n) anywhere in a
//     key-by-key file: sops re-serializes the WHOLE document and quotes
//     them, and Home Assistant's 1.1 parser then reads a string.
//   - a literal top-level "sops" key (exit 203).
//   - content that does not parse as YAML but carries a secret-shaped key.
//
// An empty or comment-only secrets.yaml returns false: sops refuses a
// document-less stream, which would wedge a new user's first import.
func yamlNeedsEncryption(relPath string, data []byte) (need bool, refusal string) {
	secretsFile := IsSecretsFile(relPath)

	docs, err := parseDocuments(data)
	if err != nil {
		// Fail closed: a file this package cannot read is one it cannot
		// clear for the remote, so anything secret-shaped is refused.
		if secretsFile || hasSecretShapedLine(data) {
			return false, "does not parse as YAML, so it can be neither encrypted nor safely committed: fix the file, or move the secret into secrets.yaml and reference it with !secret"
		}
		return false, ""
	}
	if len(docs) == 0 {
		return false, ""
	}

	if !secretsFile {
		found := false
		for _, doc := range docs {
			if hasInlineSecret(doc) {
				found = true
				break
			}
		}
		if !found {
			return false, ""
		}
	}

	if anyCustomTag(docs) {
		return false, "holds a secret value inline alongside a Home Assistant custom tag (!secret, !include..., !input), " +
			"which SOPS cannot encrypt without destroying the tag: move that value into secrets.yaml and " +
			"reference it with !secret"
	}
	for _, doc := range docs {
		root := documentRoot(doc)
		if root == nil || root.Kind != yaml.MappingNode {
			return false, "holds a secret value but its top level is not a mapping, which SOPS cannot encrypt: " +
				"move that value into secrets.yaml and reference it with !secret"
		}
		if mappingValue(root, "sops") != nil {
			return false, `has a top-level "sops" key, which SOPS reserves for its own metadata: rename that key, ` +
				"or move the secret into secrets.yaml and reference it with !secret"
		}
	}
	// Only the key-by-key branch leaves values outside the ciphertext for
	// sops to rewrite; in secrets.yaml every value is encrypted.
	if !secretsFile {
		for _, doc := range docs {
			if token, ok := firstLegacyBoolean(doc); ok {
				return false, "holds a secret value inline alongside the unquoted value " + token +
					", which SOPS would rewrite as a quoted string and change how Home Assistant reads it: " +
					"move the secret into secrets.yaml and reference it with !secret"
			}
		}
	}
	return true, ""
}

// legacyBooleans are the plain scalars YAML 1.1 reads as booleans and 1.2
// reads as strings. Home Assistant parses 1.1; go-yaml and sops parse 1.2,
// and the disagreement only shows after a sops round trip quotes them.
var legacyBooleans = map[string]bool{
	"y": true, "n": true,
	"yes": true, "no": true,
	"on": true, "off": true,
}

// firstLegacyBoolean returns the first plain scalar in node that YAML 1.1
// reads as a boolean and 1.2 as a string. Quoted forms are exempt: they
// already say "string" in both versions.
func firstLegacyBoolean(node *yaml.Node) (string, bool) {
	if node == nil {
		return "", false
	}
	if node.Kind == yaml.ScalarNode && node.Style == 0 && node.Tag == "!!str" && legacyBooleans[strings.ToLower(node.Value)] {
		return node.Value, true
	}
	for _, child := range node.Content {
		if token, ok := firstLegacyBoolean(child); ok {
			return token, true
		}
	}
	return "", false
}

// hasSecretShapedLine reports whether data has a line that looks like a
// secret-shaped mapping key with a value, without parsing it. Used only
// once the parser has given up, and serves YAML and JSON both.
func hasSecretShapedLine(data []byte) bool {
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, found := strings.Cut(line, ":")
		if found && strings.TrimSpace(value) != "" &&
			secretKeyRe.MatchString(strings.Trim(strings.TrimSpace(key), `"'`)) {
			return true
		}
		// One key per line misses compact JSON ({"password":"x"}) and
		// NDJSON, which fail both json.Valid and the YAML parse - so this
		// scan is all that stands between their secret and the remote.
		for _, m := range quotedKeyRe.FindAllStringSubmatch(line, -1) {
			if secretKeyRe.MatchString(m[1]) {
				return true
			}
		}
	}
	return false
}

// quotedKeyRe finds a quoted mapping key anywhere in a line, which is how
// a key hides inside compact JSON that no parser would accept.
var quotedKeyRe = regexp.MustCompile(`["']([A-Za-z0-9_.-]+)["']\s*:`)

// parseDocuments decodes every YAML document in data as a node tree, tags
// intact. The whole stream, not just the first document: yaml.Unmarshal
// stops after one, and a secret in the second half would sail past.
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

// hasInlineSecret reports whether node holds, at any depth, a mapping key
// matching SecretKeyRegex whose value is encryptable (see NeedsEncryption).
func hasInlineSecret(node *yaml.Node) bool {
	if node == nil {
		return false
	}
	if node.Kind == yaml.MappingNode {
		// Mapping content alternates key, value, key, value.
		for i := 0; i+1 < len(node.Content); i += 2 {
			key, value := node.Content[i], node.Content[i+1]
			if key.Kind == yaml.ScalarNode && secretKeyRe.MatchString(key.Value) && isEncryptableValue(value) {
				return true
			}
		}
	}
	for _, child := range node.Content {
		if hasInlineSecret(child) {
			return true
		}
	}
	return false
}

// isEncryptableValue reports whether a mapping value is secret material
// sops can encrypt and give back unchanged: a plain scalar, or a non-empty
// list of them.
func isEncryptableValue(node *yaml.Node) bool {
	switch node.Kind {
	case yaml.ScalarNode:
		return isPlainScalar(node)
	case yaml.SequenceNode:
		if len(node.Content) == 0 {
			return false
		}
		for _, item := range node.Content {
			if !isPlainScalar(item) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

// isPlainScalar reports whether node is a scalar carrying an ordinary YAML
// type. A null ("password:" with nothing after it) is not one: encrypting
// it would churn the file on every import for nothing.
func isPlainScalar(node *yaml.Node) bool {
	return node.Kind == yaml.ScalarNode && node.Tag != "!!null" && !isCustomTag(node.Tag)
}

// isCustomTag reports whether tag is application-defined (one leading "!")
// rather than one of YAML's own "!!str"/"!!int"/"!!map" family. Matching
// the shape rather than a list of Home Assistant's !secret/!include*/
// !input/!env_var also catches whatever a custom component invents.
func isCustomTag(tag string) bool {
	return strings.HasPrefix(tag, "!") && !strings.HasPrefix(tag, "!!")
}

// anyCustomTag reports whether any node in docs carries a custom tag.
// Whole-tree rather than near the secret: sops rewrites the entire
// document, so one !include at the far end makes encrypting it lossy.
func anyCustomTag(docs []*yaml.Node) bool {
	for _, doc := range docs {
		if nodeHasCustomTag(doc) {
			return true
		}
	}
	return false
}

func nodeHasCustomTag(node *yaml.Node) bool {
	if node == nil {
		return false
	}
	if isCustomTag(node.Tag) {
		return true
	}
	for _, child := range node.Content {
		if nodeHasCustomTag(child) {
			return true
		}
	}
	return false
}

// SemanticallyEqual reports whether two YAML documents (or document
// streams) carry the same values, ignoring how they are written down.
//
// sops does not round-trip a YAML file byte-for-byte - it re-emits from its
// own parse, re-indenting blocks, unquoting former ENC[...] strings and
// making an empty value an explicit null - so a byte comparison would
// report drift on every cycle forever. Comparing decoded values costs only
// changes to comments and to mapping key ORDER, both inert to Home
// Assistant. Anything that does not parse on either side compares unequal,
// so the caller falls back to the raw difference it already has.
//
// dotenv content is compared as its assignment lines, in order, and tested
// FIRST: the YAML decoder does not fail on a dotenv file, it folds one into
// a single scalar joining lines with spaces, so "a=1\nb=2" and "a=1 b=2"
// would come back equal - missed drift. Bytes are wrong the other way,
// since sops preserves a dotenv file's comments but DROPS its blank lines,
// so such a file would be re-encrypted (ciphertext is nondeterministic) and
// committed on every single import.
func SemanticallyEqual(a, b []byte) bool {
	pairsA, dotenvA := dotenvContent(a)
	pairsB, dotenvB := dotenvContent(b)
	if dotenvA || dotenvB {
		return dotenvA && dotenvB && equalLines(pairsA, pairsB)
	}

	valuesA, err := decodeValues(a)
	if err != nil {
		return false
	}
	valuesB, err := decodeValues(b)
	if err != nil {
		return false
	}
	return valuesEqual(valuesA, valuesB)
}

// dotenvContent is dotenvPairs restricted to files holding at least one
// assignment, so an empty or comment-only file goes to the YAML path rather
// than comparing equal to every other such file.
func dotenvContent(data []byte) ([]string, bool) {
	pairs, ok := dotenvPairs(data)
	if !ok || len(pairs) == 0 {
		return nil, false
	}
	return pairs, true
}

// equalLines compares two assignment lists in content and order. Order
// matters: a dotenv file can repeat a key, so a set comparison would call
// "a=1\na=2" and "a=2\na=1" the same file.
func equalLines(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// valuesEqual is reflect.DeepEqual with one exception: two numbers that
// differ only in how they were written down are equal. sops re-emits from
// its own parse and drops numeric representation, so a "0.00" comes back as
// "0" and decodes as an int where the original was a float, which DeepEqual
// would report as drift forever (one file in a real 6791-file config hit
// it). Integers are compared as integers, so two large and genuinely
// different int64s cannot be rounded into agreement.
func valuesEqual(a, b any) bool {
	switch av := a.(type) {
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for k, x := range av {
			y, ok := bv[k]
			if !ok || !valuesEqual(x, y) {
				return false
			}
		}
		return true
	case []any:
		bv, ok := b.([]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for i := range av {
			if !valuesEqual(av[i], bv[i]) {
				return false
			}
		}
		return true
	}
	if ai, aok := asInt(a); aok {
		if bi, bok := asInt(b); bok {
			return ai == bi
		}
	}
	if af, aok := asFloat(a); aok {
		if bf, bok := asFloat(b); bok {
			return af == bf
		}
	}
	return reflect.DeepEqual(a, b)
}

// asInt reports whether v is one of go-yaml's integer decodings.
func asInt(v any) (int64, bool) {
	switch n := v.(type) {
	case int:
		return int64(n), true
	case int64:
		return n, true
	case uint64:
		if n <= 1<<63-1 {
			return int64(n), true
		}
	}
	return 0, false
}

// asFloat reports whether v is any number, integer or not. Only reached
// when the two sides are not both integers.
func asFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint64:
		return float64(n), true
	}
	return 0, false
}

// decodeValues decodes every document in a YAML stream to plain Go values -
// the whole stream, for the same reason parseDocuments reads it all.
func decodeValues(data []byte) ([]any, error) {
	var docs []any
	dec := yaml.NewDecoder(bytes.NewReader(data))
	for {
		var doc any
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			return docs, nil
		}
		if err != nil {
			return nil, err
		}
		docs = append(docs, doc)
	}
}

// IsEncrypted reports whether data is a sops-encrypted document. A cheap
// substring sniff first, then a parse confirming a TOP-LEVEL "sops" mapping
// carrying both a mac and a version - the confirmation keeps a config that
// merely mentions sops in a comment from being read as "already encrypted"
// and pushed in the clear. JSON needs no separate path (YAML subset).
//
// dotenv does, since it has no mapping to nest the metadata in and sops
// writes flat "sops_*=" lines: a wrong answer there makes internal/differ
// compare ciphertext against live plaintext (permanent drift) and the
// applier write ENC[...] strings into /homeassistant.
func IsEncrypted(data []byte) bool {
	if !bytes.Contains(data, []byte("sops")) {
		return false
	}
	// Structured formats first, flat dotenv only as a fallback: a YAML
	// document can carry lines that look like dotenv metadata, and letting
	// the flat scan answer would shadow the checks that read the real
	// "sops:" mapping - UnsupportedKeySource among them.
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err == nil {
		meta := mappingValue(documentRoot(&doc), "sops")
		// Tolerant about the shape of mac and version: the point is only
		// that this is a sops document, not which version wrote it. The key
		// source is required on top, because a spoofed "sops: {mac: x,
		// version: y}" block over plaintext values satisfies the first two
		// but could never decrypt - classifying it as "not encrypted" here
		// lets GuardSecretsAt refuse it honestly instead of the pipeline
		// failing later with "encrypted, but undecryptable". Note the
		// asymmetry: hasKeySource fails toward "encrypted" when it cannot
		// decode the metadata (see its comment) - do not "make it
		// consistent" with the nil-returning checks around it.
		if mappingValue(meta, "mac") != nil && mappingValue(meta, "version") != nil && hasKeySource(meta) {
			return true
		}
	}
	return isDotenvCiphertext(data)
}

// keySourceMetadataKeys are the metadata keys sops stores recipients
// under: every remote backend plus age and Shamir key_groups. A real sops
// file always carries at least one, or nothing could decrypt it. Derived
// from remoteKeySources so a backend added there cannot drift out of here.
var keySourceMetadataKeys = append([]string{"age", "key_groups"}, remoteKeySources...)

// hasKeySource reports whether meta names at least one key source. It
// decodes rather than walking nodes: merge keys ("<<: *anchor") resolve
// only during decode, and a key source hidden behind one must still count.
// UnsupportedKeySource returns early on "not encrypted", so missing one
// here would fail that guard OPEN; for the same reason a mapping that does
// not decode counts as having one - let the guard look and refuse.
func hasKeySource(meta *yaml.Node) bool {
	if meta == nil {
		return false
	}
	var decoded map[string]any
	if err := meta.Decode(&decoded); err != nil {
		return true
	}
	for _, key := range keySourceMetadataKeys {
		if v, ok := decoded[key]; ok && v != nil {
			return true
		}
	}
	return false
}

// isDotenvCiphertext reports whether data is sops ciphertext from the
// dotenv store, which has nowhere to nest the "sops:" mapping and so
// spreads it across top-level keys ("sops_mac=", "sops_version=",
// "sops_age__list_0__map_recipient="). Both halves are required, for the
// reason IsEncrypted requires them.
func isDotenvCiphertext(data []byte) bool {
	// The file must actually BE dotenv, not merely contain two lines that
	// spell sops metadata: otherwise "sops_mac=x", "sops_version=3" and a
	// plaintext "mqtt_password: hunter2" pass for ciphertext - and
	// GuardSecretsAt asks exactly this before clearing a repository.
	if _, ok := dotenvPairs(data); !ok {
		return false
	}
	mac, version := false, false
	for _, raw := range strings.Split(string(data), "\n") {
		key, value, found := strings.Cut(strings.TrimSuffix(raw, "\r"), "=")
		if !found || value == "" {
			continue
		}
		switch key {
		case "sops_mac":
			mac = true
		case "sops_version":
			version = true
		}
	}
	return mac && version
}

// remoteKeySources are the sops master-key backends whose decryption reaches
// out over the network. age is the only one this agent configures or uses.
var remoteKeySources = []string{"kms", "gcp_kms", "azure_kv", "hc_vault", "pgp"}

// UnsupportedKeySource returns the name of a non-age master key the
// document declares ("" when it declares none), and whether the metadata
// could be read at all. The metadata is repository content and sops
// honours it, so a planted hc_vault key would make the decrypt call
// connect to a URL the repository picked and leak that URL through
// decryptFailures into last_error, the dashboard and /status.json - for
// a document this agent's age identity cannot decrypt anyway.
//
// The metadata is read by VALUE decoding, not by walking the yaml.Node
// tree: sops reads it the same way (yaml.Unmarshal into its metadata
// struct), which resolves aliases and merge keys, while a node walk sees
// only the literal document - so "hc_vault: *v" or "<<: *m" would smuggle
// a backend past a node-based check that sops then honours. Every shape
// assertion fails CLOSED with verifiable=false: a mapping with any
// non-string key decodes as map[any]any, so a single tagged key planted
// beside a real hc_vault entry would otherwise make a comma-ok assertion
// silently drop the whole mapping and wave the document through. The
// caller must refuse to decrypt an unverifiable document rather than let
// sops interpret metadata this check could not read.
func UnsupportedKeySource(data []byte) (source string, verifiable bool) {
	if !IsEncrypted(data) {
		return "", true
	}
	if isDotenvCiphertext(data) {
		return dotenvKeySource(data), true
	}
	var doc map[string]any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return "", false
	}
	meta, ok := doc["sops"].(map[string]any)
	if !ok {
		// IsEncrypted's node walk found sops metadata that this value
		// view cannot read as a plain mapping: a differential, not a
		// document sops writes.
		return "", false
	}
	if source := remoteKeySourceIn(meta); source != "" {
		return source, true
	}
	// key_groups holds the same per-backend lists one level down, so a
	// document can name a backend without a top-level entry for it. An
	// absent key and an explicit null both land as nil here.
	if groupsRaw := meta["key_groups"]; groupsRaw != nil {
		groups, ok := groupsRaw.([]any)
		if !ok {
			return "", false
		}
		for _, group := range groups {
			gm, ok := group.(map[string]any)
			if !ok {
				return "", false
			}
			if source := remoteKeySourceIn(gm); source != "" {
				return source, true
			}
		}
	}
	return "", true
}

// remoteKeySourceIn returns the first remote backend meta carries a
// non-empty entry for, or "".
func remoteKeySourceIn(meta map[string]any) string {
	for _, source := range remoteKeySources {
		if declaresValue(meta[source]) {
			return source
		}
	}
	return ""
}

// dotenvKeySource is UnsupportedKeySource's flat-format half. The dotenv
// store flattens the metadata tree into the key names - a list index
// becomes "__list_N", a nested mapping key "__map_<name>" - so a top-level
// kms list arrives as "sops_kms__list_0__map_arn". Whole "__"-delimited
// segments are matched, not substrings, so "kms" does not also match
// "gcp_kms", and only the KEY is matched so a base64 value cannot spell a
// backend name into existence.
func dotenvKeySource(data []byte) string {
	for _, raw := range strings.Split(string(data), "\n") {
		key, value, found := strings.Cut(strings.TrimSuffix(raw, "\r"), "=")
		if !found || value == "" || !strings.HasPrefix(key, "sops_") {
			continue
		}
		segments := strings.Split(key, "__")
		for _, source := range remoteKeySources {
			if segments[0] == "sops_"+source {
				return source
			}
			for _, segment := range segments[1:] {
				if segment == "map_"+source {
					return source
				}
			}
		}
	}
	return ""
}

// declaresValue reports whether v is a non-empty metadata entry.
// Emptiness matters: sops writes several of these as empty lists on every
// document, and counting those would refuse every file it wrote. A value
// of a shape sops never writes counts as declared, so an odd encoding
// errs toward refusing.
func declaresValue(v any) bool {
	switch value := v.(type) {
	case nil:
		return false
	case []any:
		return len(value) > 0
	case map[string]any:
		return len(value) > 0
	case string:
		return value != ""
	default:
		return true
	}
}

// documentRoot unwraps a DocumentNode to the value it contains.
func documentRoot(node *yaml.Node) *yaml.Node {
	if node != nil && node.Kind == yaml.DocumentNode && len(node.Content) == 1 {
		return node.Content[0]
	}
	return node
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
