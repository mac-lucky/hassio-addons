// Package ignorekeys is the YAML half of the ignore_keys option: mapping
// keys inside a synced file that belong to the app writing that file, so
// git does not manage them. Zigbee2MQTT is the case it exists for: its
// settings write persists the MQTT login it is handed through the
// environment into its own configuration.yaml, every time it writes.
//
// Two operations carry the whole rule. Canonical is how such a file is
// compared and published: as YAML 1.2 data with the keys taken out, so
// neither the keys nor indentation, quoting, comments or the spelling of a
// number count as a difference.
// Overlay is how one is written: one side's content with the other side's
// value at each key - an apply keeps what the app wrote, a capture keeps
// it out of git.
//
// Nothing here returns, logs or formats a value, and no error carries the
// parser's own text, which can quote the file, so a caller may log any
// error as it is. Which files are allowed to have entries is the caller's
// question (gitsync.SetIgnoredKeys).
package ignorekeys

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	yaml "go.yaml.in/yaml/v3"
)

// Example is a well-formed entry, quoted in the syntax errors.
const Example = "zigbee2mqtt/configuration.yaml:mqtt.password"

// KeyPath is a dotted key path split into its mapping keys: "mqtt.password"
// is {"mqtt", "password"}. Sequence indexes are not part of the syntax.
type KeyPath []string

// String renders the path the way the option writes it.
func (k KeyPath) String() string { return strings.Join(k, ".") }

// Entry is one ignore_keys entry: a config-relative YAML file and a key
// path inside it.
type Entry struct {
	File string
	Key  KeyPath
}

var (
	// ErrNotYAML is a file that is not exactly one YAML document: the
	// decoder rejects it, it holds a NUL byte or invalid UTF-8, or it is a
	// stream of several documents, which no key path could address alone.
	ErrNotYAML = errors.New("does not parse as a single YAML document")
	// ErrAnchored is an ignored key whose value holds a YAML anchor or an
	// alias. Moving one between files could leave an alias pointing at
	// nothing, or repoint one that already resolved, so it is not done.
	ErrAnchored = errors.New("has an ignored key whose value holds a YAML anchor or alias")
	// ErrAmbiguous is a mapping on an ignored key's path that repeats the
	// key looked up there or merges another mapping in with "<<", or a
	// step on the path that is an alias or anchored: the value could sit
	// in more than one place, so none is used.
	ErrAmbiguous = errors.New("repeats a key on an ignored key's path, merges a mapping into it, or shares one through an anchor or alias")

	// ErrTarget and ErrSource say which side of an Overlay could not be
	// used: the content being written, or the one the values come from.
	ErrTarget = errors.New("the copy being written")
	ErrSource = errors.New("the copy the ignored values come from")
)

// ParseEntry reads one ignore_keys entry, "<file>:<dotted.key.path>", and
// checks its syntax: a path to a .yaml or .yml file relative to the config
// directory, written with "/", then a key path of non-empty mapping keys.
// The first ":" ends the file. The error says what is wrong without
// quoting the entry.
func ParseEntry(entry string) (Entry, error) {
	switch {
	case strings.TrimSpace(entry) == "":
		return Entry{}, errors.New("is empty")
	case strings.TrimSpace(entry) != entry:
		return Entry{}, errors.New("has leading or trailing spaces")
	}
	for _, r := range entry {
		// Cf too: a zero-width space or a byte-order mark pasted with the
		// entry would make it name a key no file has.
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return Entry{}, errors.New("contains a control character or an invisible formatting character")
		}
	}

	file, key, ok := strings.Cut(entry, ":")
	switch {
	case !ok:
		return Entry{}, fmt.Errorf("has no ':' between the file and the key, e.g. %s", Example)
	case file == "":
		return Entry{}, fmt.Errorf("names no file before the ':', e.g. %s", Example)
	case key == "":
		return Entry{}, fmt.Errorf("names no key after the ':', e.g. %s", Example)
	case strings.TrimSpace(file) != file || strings.TrimSpace(key) != key:
		return Entry{}, errors.New("has a space next to the ':'")
	}
	if err := checkFile(file); err != nil {
		return Entry{}, err
	}

	segments := strings.Split(key, ".")
	for _, seg := range segments {
		switch {
		case seg == "":
			return Entry{}, errors.New("has an empty key segment (a leading, trailing or doubled '.')")
		case strings.ContainsAny(seg, "[]"):
			return Entry{}, errors.New("indexes a list; only mapping keys can be ignored")
		case strings.ContainsFunc(seg, unicode.IsSpace) || strings.ContainsAny(seg, ":="):
			// "mqtt.password: hunter2", "mqtt.password:hunter2" or
			// "mqtt.password=hunter2" pasted whole. Refused rather than
			// kept, because key paths are shown on the dashboard; a YAML
			// key with a ':' in it cannot be listed.
			return Entry{}, errors.New("has a space, ':' or '=' in the key; it looks like a value was pasted after the key")
		}
	}
	return Entry{File: file, Key: segments}, nil
}

// checkFile is ParseEntry's check of the file half.
func checkFile(file string) error {
	switch {
	case strings.HasPrefix(file, "/"):
		return fmt.Errorf("names an absolute path; write it from the config directory, e.g. %s", Example)
	case strings.ContainsRune(file, '\\'):
		return errors.New("uses a backslash; separate directories with /")
	case strings.ContainsAny(file, "*?["):
		return errors.New("is a pattern; name one file")
	}
	for _, seg := range strings.Split(file, "/") {
		switch seg {
		case "":
			return errors.New("has an empty path segment (//, or a trailing /)")
		case ".", "..":
			return fmt.Errorf("has a %q segment; write the path from the config directory, e.g. %s", seg, Example)
		}
	}
	if ext := strings.ToLower(path.Ext(file)); ext != ".yaml" && ext != ".yml" {
		return errors.New("names a file that is not YAML; only .yaml and .yml files have keys to ignore")
	}
	return nil
}

// Canonical returns data as a file with ignore_keys entries is compared
// and published: the one document decoded, every key in keys removed,
// every scalar reduced to its YAML 1.2 value (normalize), comments and
// presentation styles cleared, re-encoded with two-space indentation. Two
// copies that differ only in the ignored keys, in indentation, quoting,
// flow-versus-block layout, comments or the spelling of a number, boolean
// or null come out byte-identical; key order and every value still count.
//
// A mapping on an ignored key's path that is left empty, or was empty or
// null to begin with, is removed as well, so a side where the app added
// "mqtt:" just to hold its keys matches one that never had it. An empty
// or comment-only file is "". Errors are ErrNotYAML, ErrAnchored and
// ErrAmbiguous.
func Canonical(keys []KeyPath, data []byte) ([]byte, error) {
	doc, err := parse(data)
	if err != nil {
		return nil, err
	}
	if doc == nil || isNull(doc.Content[0]) {
		return []byte{}, nil
	}
	if root := rootMapping(doc); root != nil {
		for _, key := range keys {
			if err := unambiguous(root, key); err != nil {
				return nil, err
			}
		}
		for _, key := range keys {
			if err := strip(root, key, true); err != nil {
				return nil, err
			}
		}
	}
	normalize(doc, false)
	plain(doc)
	return encode(doc)
}

// Equal reports whether a and b say the same thing once keys are set
// aside: Canonical of each, compared.
func Equal(keys []KeyPath, a, b []byte) (bool, error) {
	ca, err := Canonical(keys, a)
	if err != nil {
		return false, err
	}
	cb, err := Canonical(keys, b)
	if err != nil {
		return false, err
	}
	return bytes.Equal(ca, cb), nil
}

// Overlay returns target with each key in keys holding source's value, or
// removed where source has no such key; a nil source has none of them. An
// apply passes the repository's copy as target and the live one as
// source, so what the app wrote stays; a capture passes them the other way
// round, so it never reaches git.
//
// target comes back byte for byte when every key already agrees (by
// sameValue, so a value spelled another way agrees), and a file nothing
// needs changing in is not reformatted. Otherwise it is re-encoded with
// two-space indentation (tidy first); its comments and quoting carry over. A key
// target lacks is added at the end of its mapping, creating the mappings
// on its path, or filling in one written as null; one under anything else
// (a list, a scalar, an alias) is left as target has it. A target with no
// top-level mapping is returned as it is.
//
// Errors wrap ErrTarget or ErrSource, naming the side that could not be
// used, together with ErrNotYAML, ErrAnchored or ErrAmbiguous.
func Overlay(keys []KeyPath, target, source []byte) ([]byte, error) {
	tdoc, err := parse(target)
	if err != nil {
		return nil, fmt.Errorf("ignorekeys: %w %w", ErrTarget, err)
	}
	sdoc, err := parse(source)
	if err != nil {
		return nil, fmt.Errorf("ignorekeys: %w %w", ErrSource, err)
	}
	troot := rootMapping(tdoc)
	sroot := rootMapping(sdoc)
	for _, key := range keys {
		if err := unambiguous(troot, key); err != nil {
			return nil, fmt.Errorf("ignorekeys: %w %w", ErrTarget, err)
		}
		if err := unambiguous(sroot, key); err != nil {
			return nil, fmt.Errorf("ignorekeys: %w %w", ErrSource, err)
		}
	}
	if troot == nil {
		return target, nil
	}

	changed := false
	for _, key := range keys {
		want := lookup(sroot, key)
		have := lookup(troot, key)
		if want != nil && anchored(want) {
			return nil, fmt.Errorf("ignorekeys: %w %w", ErrSource, ErrAnchored)
		}
		switch {
		case want == nil && have == nil:
		case want == nil:
			// strip checks every occurrence it removes for an anchor.
			if err := strip(troot, key, false); err != nil {
				return nil, fmt.Errorf("ignorekeys: %w %w", ErrTarget, err)
			}
			changed = true
		case have != nil && sameValue(have, want):
		default:
			if have != nil && anchored(have) {
				return nil, fmt.Errorf("ignorekeys: %w %w", ErrTarget, ErrAnchored)
			}
			if set(troot, key, clone(want)) {
				changed = true
			}
		}
	}
	if !changed {
		return target, nil
	}
	tidy(tdoc, false)
	out, err := encode(tdoc)
	if err != nil {
		return nil, fmt.Errorf("ignorekeys: %w %w", ErrTarget, err)
	}
	return out, nil
}

// parse decodes data as at most one YAML document. nil means the stream
// holds none (an empty or comment-only file); more than one document, and
// anything the decoder rejects, is ErrNotYAML - never the decoder's own
// error, which can quote the file.
func parse(data []byte) (*yaml.Node, error) {
	if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		return nil, ErrNotYAML
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var doc yaml.Node
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, nil
		}
		return nil, ErrNotYAML
	}
	if len(doc.Content) != 1 {
		return nil, ErrNotYAML
	}
	var next yaml.Node
	if err := dec.Decode(&next); !errors.Is(err, io.EOF) {
		return nil, ErrNotYAML
	}
	return &doc, nil
}

// encode renders doc the one way every function here does.
func encode(doc *yaml.Node) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return nil, ErrNotYAML
	}
	if err := enc.Close(); err != nil {
		return nil, ErrNotYAML
	}
	return buf.Bytes(), nil
}

// rootMapping returns doc's top-level mapping, nil when it has none.
func rootMapping(doc *yaml.Node) *yaml.Node {
	if doc == nil || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil
	}
	return doc.Content[0]
}

// find returns the index of seg's key node in mapping m, or -1. Keys are
// matched on their text, so "1:" and "'1':" are both "1".
func find(m *yaml.Node, seg string) int {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if k := m.Content[i]; k.Kind == yaml.ScalarNode && k.Value == seg {
			return i
		}
	}
	return -1
}

// unambiguous returns ErrAmbiguous when a mapping on key's path, from m
// down, holds the segment looked up in it more than once or a "<<" merge
// key, or when a step before the last is an alias or carries an anchor.
// lookup, set and the "already equal" check read the first occurrence of
// one mapping only, so a repeat, a merged-in value or a mapping shared
// through an alias could carry the key past them. A nil m, or a path that
// ends early, is fine.
func unambiguous(m *yaml.Node, key KeyPath) error {
	for i, seg := range key {
		if m == nil || m.Kind != yaml.MappingNode {
			return nil
		}
		at, n := -1, 0
		for i := 0; i+1 < len(m.Content); i += 2 {
			k := m.Content[i]
			if k.Kind != yaml.ScalarNode {
				continue
			}
			if k.Tag == "!!merge" || k.Value == "<<" {
				return ErrAmbiguous
			}
			if k.Value == seg {
				n++
				if at < 0 {
					at = i
				}
			}
		}
		if n > 1 {
			return ErrAmbiguous
		}
		if at < 0 {
			return nil
		}
		next := m.Content[at+1]
		if i < len(key)-1 && (next.Kind == yaml.AliasNode || next.Anchor != "") {
			// "mqtt: *creds" puts the key under a mapping that also lives
			// somewhere else, unremoved; an anchored one is shared the
			// other way round.
			return ErrAmbiguous
		}
		m = next
	}
	return nil
}

// lookup returns the value at key under mapping m, or nil when any step
// is missing or not a mapping. Aliases are not followed, and a repeated
// key is read at its first occurrence.
func lookup(m *yaml.Node, key KeyPath) *yaml.Node {
	for i, seg := range key {
		if m == nil || m.Kind != yaml.MappingNode {
			return nil
		}
		j := find(m, seg)
		if j < 0 {
			return nil
		}
		if i == len(key)-1 {
			return m.Content[j+1]
		}
		m = m.Content[j+1]
	}
	return nil
}

// strip removes key from mapping m - every occurrence, should the file
// repeat it - then each mapping on its path that this left empty. With
// pruneEmpty, a mapping on the path that was empty or null already goes
// too, which is Canonical's rule that such a parent counts as absent;
// Overlay passes false and so removes only what it emptied itself.
// ErrAnchored when a value it would remove holds an anchor or alias.
func strip(m *yaml.Node, key KeyPath, pruneEmpty bool) error {
	if len(key) == 1 {
		for i := 0; i+1 < len(m.Content); {
			if k := m.Content[i]; k.Kind == yaml.ScalarNode && k.Value == key[0] {
				if anchored(m.Content[i+1]) {
					return ErrAnchored
				}
				m.Content = slices.Delete(m.Content, i, i+2)
				continue
			}
			i += 2
		}
		return nil
	}

	i := find(m, key[0])
	if i < 0 {
		return nil
	}
	child := m.Content[i+1]
	switch {
	case child.Kind == yaml.MappingNode:
		before := len(child.Content)
		if err := strip(child, key[1:], pruneEmpty); err != nil {
			return err
		}
		emptied := len(child.Content) == 0 && (pruneEmpty || before > 0)
		if emptied && child.Anchor == "" {
			m.Content = slices.Delete(m.Content, i, i+2)
		}
	case pruneEmpty && isNull(child) && child.Anchor == "":
		m.Content = slices.Delete(m.Content, i, i+2)
	}
	return nil
}

// set puts value at key under mapping m, adding the mappings m lacks on
// the way and filling in a null one, and reports whether it could: a step
// that is a list, a scalar or an alias is not replaced. An existing key
// takes the value at its first occurrence, and any repeat of it is
// dropped; a new one goes at the end of its mapping.
func set(m *yaml.Node, key KeyPath, value *yaml.Node) bool {
	for _, seg := range key[:len(key)-1] {
		i := find(m, seg)
		if i < 0 {
			child := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			m.Content = append(m.Content, keyNode(seg), child)
			m = child
			continue
		}
		child := m.Content[i+1]
		switch {
		case child.Kind == yaml.MappingNode:
		case isNull(child) && child.Anchor == "":
			// In place, so a comment on the line survives.
			child.Kind, child.Tag, child.Value, child.Style = yaml.MappingNode, "!!map", "", 0
		default:
			return false
		}
		m = child
	}

	leaf := key[len(key)-1]
	i := find(m, leaf)
	if i < 0 {
		m.Content = append(m.Content, keyNode(leaf), value)
		return true
	}
	m.Content[i+1] = value
	for j := i + 2; j+1 < len(m.Content); {
		if k := m.Content[j]; k.Kind == yaml.ScalarNode && k.Value == leaf {
			m.Content = slices.Delete(m.Content, j, j+2)
			continue
		}
		j += 2
	}
	return true
}

// keyNode is a new mapping key. No tag, so the encoder writes it plain
// whenever it can, the way the file's own keys almost always are.
func keyNode(name string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Value: name}
}

// isNull reports whether n is a null scalar: "key:", "~" or "null".
func isNull(n *yaml.Node) bool {
	return n.Kind == yaml.ScalarNode && n.ShortTag() == "!!null"
}

// anchored reports whether n, or anything under it, carries an anchor or
// is an alias.
func anchored(n *yaml.Node) bool {
	if n.Anchor != "" || n.Kind == yaml.AliasNode {
		return true
	}
	return slices.ContainsFunc(n.Content, anchored)
}

// sameValue reports whether two nodes hold the same data the way
// Canonical compares it: scalars by their YAML 1.2 value (scalarValue),
// mapping keys as strings (keyText), comments and styles ignored.
func sameValue(a, b *yaml.Node) bool {
	if a.Kind != b.Kind || len(a.Content) != len(b.Content) {
		return false
	}
	switch a.Kind {
	case yaml.ScalarNode:
		at, av := scalarValue(a)
		bt, bv := scalarValue(b)
		return at == bt && av == bv
	case yaml.AliasNode:
		return a.Value == b.Value
	case yaml.MappingNode:
		for i := 0; i+1 < len(a.Content); i += 2 {
			ka, kb := a.Content[i], b.Content[i]
			if ka.Kind == yaml.ScalarNode && kb.Kind == yaml.ScalarNode {
				if keyText(ka) != keyText(kb) {
					return false
				}
			} else if !sameValue(ka, kb) {
				return false
			}
			if !sameValue(a.Content[i+1], b.Content[i+1]) {
				return false
			}
		}
		return true
	}
	for i := range a.Content {
		if !sameValue(a.Content[i], b.Content[i]) {
			return false
		}
	}
	return true
}

// Scalars are compared as the YAML 1.2 core schema reads them, which is
// how js-yaml - and so Zigbee2MQTT - reads them, and not by their text: a
// settings write turns "pan_id: 0x1a62" into 6754, "True" into true and
// "~" into null, and none of that is a change. The patterns follow
// js-yaml's, underscores included.
var (
	coreIntRe   = regexp.MustCompile(`^[-+]?(?:0b[01_]+|0o[0-7_]+|0x[0-9a-fA-F_]+|[0-9][0-9_]*)$`)
	coreFloatRe = regexp.MustCompile(`^(?:[-+]?[0-9][0-9_]*(?:\.[0-9_]*)?(?:[eE][-+]?[0-9]+)?|\.[0-9_]+(?:[eE][-+]?[0-9]+)?|[-+]?\.(?:inf|Inf|INF)|\.(?:nan|NaN|NAN))$`)
)

// maxExactFloat is the largest integer a float64 holds exactly, 2^53: a
// float up to it with nothing after the point is that integer, as it is
// to JavaScript, which has one number type.
const maxExactFloat = 1 << 53

// coreScalar resolves the text of a plain, untagged scalar under the YAML
// 1.2 core schema: its tag and one spelling of its value, or ok=false for
// anything that is not a null, a boolean or a number there.
func coreScalar(v string) (tag, value string, ok bool) {
	switch v {
	case "", "~", "null", "Null", "NULL":
		return "!!null", "null", true
	case "true", "True", "TRUE":
		return "!!bool", "true", true
	case "false", "False", "FALSE":
		return "!!bool", "false", true
	}
	if strings.HasSuffix(v, "_") {
		return "", "", false
	}
	if coreIntRe.MatchString(v) {
		if n, ok := coreInt(v); ok {
			return "!!int", n.String(), true
		}
		return "", "", false
	}
	if !coreFloatRe.MatchString(v) {
		return "", "", false
	}
	switch lower := strings.ToLower(strings.TrimPrefix(v, "+")); lower {
	case ".inf", "-.inf", ".nan":
		return "!!float", lower, true
	}
	f, err := strconv.ParseFloat(strings.ReplaceAll(v, "_", ""), 64)
	if err != nil {
		return "", "", false
	}
	if f == math.Trunc(f) && math.Abs(f) <= maxExactFloat {
		return "!!int", strconv.FormatInt(int64(f), 10), true
	}
	return "!!float", strconv.FormatFloat(f, 'g', -1, 64), true
}

// coreInt parses an integer coreIntRe matched, exactly, whatever its size.
func coreInt(v string) (*big.Int, bool) {
	neg := strings.HasPrefix(v, "-")
	digits := strings.ReplaceAll(strings.TrimLeft(v, "+-"), "_", "")
	base := 10
	switch {
	case strings.HasPrefix(digits, "0b"):
		base, digits = 2, digits[2:]
	case strings.HasPrefix(digits, "0o"):
		base, digits = 8, digits[2:]
	case strings.HasPrefix(digits, "0x"):
		base, digits = 16, digits[2:]
	}
	n, ok := new(big.Int).SetString(digits, base)
	if !ok {
		return nil, false
	}
	if neg {
		n.Neg(n)
	}
	return n, true
}

// isPlain reports whether a scalar was written with no quotes, no block
// style and no tag, the only kind whose type the schema decides.
func isPlain(n *yaml.Node) bool {
	return n.Style&(yaml.TaggedStyle|yaml.DoubleQuotedStyle|yaml.SingleQuotedStyle|yaml.LiteralStyle|yaml.FoldedStyle) == 0
}

// scalarValue is a scalar's tag and value as Canonical compares them. A
// plain one is resolved by coreScalar, and one the core schema reads as a
// string is a string even where the YAML 1.1 rules the decoder keeps for
// compatibility say otherwise ("011" is eleven, not nine). Quoted, block
// and tagged scalars keep the decoder's tag and their text.
func scalarValue(n *yaml.Node) (tag, value string) {
	if !isPlain(n) {
		return n.ShortTag(), n.Value
	}
	if tag, value, ok := coreScalar(n.Value); ok {
		return tag, value
	}
	switch n.ShortTag() {
	case "!!null", "!!bool", "!!int", "!!float":
		return "!!str", n.Value
	}
	return n.ShortTag(), n.Value
}

// keyText is a scalar mapping key as the string it names, which is all a
// key is once js-yaml has read it into an object: "1:", "'1':" and
// "0x1:" are one key. A merge key stays "<<".
func keyText(k *yaml.Node) string {
	if tag, value := scalarValue(k); tag != "!!str" && tag != "!!merge" {
		return value
	}
	return k.Value
}

// normalize rewrites every scalar under n to its scalarValue, and every
// scalar mapping key to its keyText as a string, so that encoding prints
// one spelling per value. key says n is a mapping key.
func normalize(n *yaml.Node, key bool) {
	switch n.Kind {
	case yaml.ScalarNode:
		if key {
			if n.ShortTag() == "!!merge" {
				n.Tag, n.Value = "", "<<"
				return
			}
			n.Tag, n.Value = "!!str", keyText(n)
			return
		}
		n.Tag, n.Value = scalarValue(n)
	case yaml.MappingNode:
		for i, child := range n.Content {
			normalize(child, i%2 == 0)
		}
	default:
		for _, child := range n.Content {
			normalize(child, false)
		}
	}
}

// tidy prepares a tree Overlay changed for encoding, where the encoder
// would otherwise change what it says. An empty null in a flow collection
// or as a mapping key comes out as an empty quoted string, so it is
// spelled "null"; and a merge key comes out as "!!merge <<", so its tag is
// left to the encoder. inFlow says n sits in a flow collection.
func tidy(n *yaml.Node, inFlow bool) {
	flow := inFlow || n.Style&yaml.FlowStyle != 0
	for i, child := range n.Content {
		isKey := n.Kind == yaml.MappingNode && i%2 == 0
		if child.Kind == yaml.ScalarNode {
			if isKey && child.ShortTag() == "!!merge" {
				child.Tag = ""
			}
			if child.Value == "" && isNull(child) && (flow || isKey) {
				child.Value = "null"
			}
			continue
		}
		tidy(child, flow)
	}
}

// clone deep-copies n. Only called on a node anchored has cleared, so
// there is no alias whose target would need copying too.
func clone(n *yaml.Node) *yaml.Node {
	c := *n
	if n.Content != nil {
		c.Content = make([]*yaml.Node, len(n.Content))
		for i, child := range n.Content {
			c.Content[i] = clone(child)
		}
	}
	return &c
}

// plain clears every comment and presentation style under n, leaving the
// encoder to pick quoting and layout from the data alone.
func plain(n *yaml.Node) {
	n.HeadComment, n.LineComment, n.FootComment = "", "", ""
	n.Style = 0
	for _, child := range n.Content {
		plain(child)
	}
}
