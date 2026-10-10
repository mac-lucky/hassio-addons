// Package render produces a secrets file's new content from its current
// content and the values 1Password holds for it. It edits the parsed YAML
// tree, not lines of text: a managed key's value node is replaced, a new
// key is appended, everything else - other keys, comments, order - stays.
// That makes adopting an existing, hand-written file safe, and leaves no
// room for the regex edits that break on multi-line values or a "$&" in a
// password.
package render

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/mac-lucky/hassio-addons/onepassword_secrets/internal/secret"
)

// Header is the comment that marks a file this add-on writes.
const Header = "Managed by the 1Password Secrets add-on. Keys that come from 1Password\n" +
	"are rewritten on every sync; edit them in 1Password. Other keys are kept."

// Desired is one key this file should hold.
type Desired struct {
	Key        string
	Value      secret.Value
	Structured bool
}

// Input is one file's render job.
type Input struct {
	// Current is the file's content; nil when it does not exist yet.
	Current []byte
	Desired []Desired
	// Keep are keys that are not desired this time but must stay as they
	// are: a conflicted key keeps its last good value.
	Keep map[string]bool
	// PreviouslyManaged are keys this add-on wrote to the file before.
	// Only these may ever be removed.
	PreviouslyManaged map[string]bool
	// Referenced are keys some config still uses. A previously managed
	// key that is gone from 1Password but still referenced is kept
	// (Orphaned) rather than removed out from under the config.
	Referenced map[string]bool
}

// Plan is the outcome. Lists hold key names only.
type Plan struct {
	Content   []byte
	Added     []string
	Changed   []string
	Unchanged []string
	Removed   []string
	Orphaned  []string
	// Unmanaged keys are in the file but not in 1Password and were never
	// written by this add-on: the migration's to-do list.
	Unmanaged []string
	// KeyErrors are keys whose value could not be rendered (a _json value
	// this renderer cannot represent): they keep what the file had, and
	// the rest of the file is still written. Reasons quote no value.
	KeyErrors map[string]string
}

// Touched reports whether any managed value moved.
func (p Plan) Touched() bool {
	return len(p.Added)+len(p.Changed)+len(p.Removed) > 0
}

func (p *Plan) keyError(key string, err error) {
	if p.KeyErrors == nil {
		p.KeyErrors = map[string]string{}
	}
	p.KeyErrors[key] = err.Error()
}

// ErrNotMapping is returned for a file whose top level is not a mapping.
var ErrNotMapping = errors.New("render: the file's top level is not a mapping of keys to values")

// Render computes the file's new content.
func Render(in Input) (Plan, error) {
	var doc yaml.Node
	if len(bytes.TrimSpace(in.Current)) > 0 {
		if err := yaml.Unmarshal(in.Current, &doc); err != nil {
			// yaml.v3's error quotes no content, only a line number.
			return Plan{}, fmt.Errorf("render: the current file is not valid YAML: %w", err)
		}
	}
	root, err := ensureMapping(&doc)
	if err != nil {
		return Plan{}, err
	}

	var plan Plan
	desired := make(map[string]Desired, len(in.Desired))
	for _, d := range in.Desired {
		desired[d.Key] = d
	}

	// Walk the existing pairs: update, keep, or drop.
	seen := map[string]bool{}
	kept := root.Content[:0:0]
	for i := 0; i+1 < len(root.Content); i += 2 {
		k, v := root.Content[i], root.Content[i+1]
		key := k.Value
		if seen[key] && desired[key].Key != "" && k.Anchor == "" && v.Anchor == "" {
			// A duplicate of a key we manage: Home Assistant would read
			// one of the two, so only the updated first one stays.
			continue
		}
		seen[key] = true
		d, isDesired := desired[key]
		switch {
		case isDesired:
			same, err := equalValue(v, d)
			if err != nil {
				plan.keyError(key, err)
				break
			}
			if same {
				plan.Unchanged = append(plan.Unchanged, key)
				break
			}
			nv, err := valueNode(d)
			if err != nil {
				plan.keyError(key, err)
				break
			}
			// An alias elsewhere may point at this value: keep its anchor.
			nv.Anchor = v.Anchor
			nv.LineComment = v.LineComment
			v = nv
			plan.Changed = append(plan.Changed, key)
		case in.Keep[key]:
		case in.PreviouslyManaged[key]:
			switch {
			case in.Referenced[key]:
				plan.Orphaned = append(plan.Orphaned, key)
			case k.Anchor != "" || v.Anchor != "":
				// An alias may still refer to it; removing it would
				// leave the file unloadable.
				plan.Orphaned = append(plan.Orphaned, key)
			default:
				plan.Removed = append(plan.Removed, key)
				continue
			}
		default:
			plan.Unmanaged = append(plan.Unmanaged, key)
		}
		kept = append(kept, k, v)
	}
	root.Content = kept

	// Append the keys the file does not have yet, sorted.
	var missing []string
	for key := range desired {
		if !seen[key] {
			missing = append(missing, key)
		}
	}
	sort.Strings(missing)
	for _, key := range missing {
		nv, err := valueNode(desired[key])
		if err != nil {
			plan.keyError(key, err)
			continue
		}
		root.Content = append(root.Content, keyNode(key), nv)
		plan.Added = append(plan.Added, key)
	}

	setHeader(&doc, root)

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return Plan{}, fmt.Errorf("render: encoding: %w", err)
	}
	if err := enc.Close(); err != nil {
		return Plan{}, fmt.Errorf("render: encoding: %w", err)
	}
	plan.Content = buf.Bytes()
	for _, l := range [][]string{plan.Changed, plan.Unchanged, plan.Orphaned, plan.Unmanaged, plan.Removed} {
		sort.Strings(l)
	}
	return plan, nil
}

// ensureMapping makes doc a document holding a mapping and returns it.
func ensureMapping(doc *yaml.Node) (*yaml.Node, error) {
	if doc.Kind == 0 {
		doc.Kind = yaml.DocumentNode
	}
	if doc.Kind != yaml.DocumentNode {
		return nil, ErrNotMapping
	}
	if len(doc.Content) == 0 {
		doc.Content = []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}
	}
	root := doc.Content[0]
	if root.Kind == yaml.ScalarNode && root.Tag == "!!null" {
		// A file holding only comments parses as one null scalar.
		comments := []string{root.HeadComment, root.LineComment, root.FootComment}
		root = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", HeadComment: strings.TrimSpace(strings.Join(comments, "\n"))}
		doc.Content[0] = root
	}
	if root.Kind != yaml.MappingNode {
		return nil, ErrNotMapping
	}
	root.Style = 0 // block style, even if the file was "{}"
	return root, nil
}

// setHeader puts Header on top, once.
func setHeader(doc, root *yaml.Node) {
	for _, c := range []*string{&doc.HeadComment, &root.HeadComment} {
		if strings.Contains(*c, "1Password Secrets add-on") {
			return
		}
	}
	h := "# " + strings.ReplaceAll(Header, "\n", "\n# ")
	if doc.HeadComment != "" {
		doc.HeadComment = h + "\n\n" + doc.HeadComment
		return
	}
	doc.HeadComment = h
}

// valueNode builds the YAML node for one desired value. Every string is
// written double-quoted (a multi-line one as a literal block): Home
// Assistant, Supervisor and ESPHome read YAML 1.1 with PyYAML, where a
// plain yes, on, 12:30 or = is a bool, a number or an error.
func valueNode(d Desired) (*yaml.Node, error) {
	if d.Structured {
		return jsonNode(d.Value.Reveal())
	}
	return stringNode(d.Value.Reveal()), nil
}

func stringNode(s string) *yaml.Node {
	n := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: s, Style: yaml.DoubleQuotedStyle}
	if strings.Contains(s, "\n") && !strings.ContainsAny(s, "\r\t") && !hasTrailingSpaceLine(s) {
		n.Style = yaml.LiteralStyle
	}
	return n
}

// keyNode quotes a key only where YAML 1.1 would read it as something
// other than a string (a key labelled on, yes or 0123).
func keyNode(key string) *yaml.Node {
	n := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}
	if yaml11NotString(key) {
		n.Style = yaml.DoubleQuotedStyle
	}
	return n
}

var yaml11Special = regexp.MustCompile(`^(?:[yYnN~=]|<<|yes|Yes|YES|no|No|NO|true|True|TRUE|false|False|FALSE|on|On|ON|off|Off|OFF|null|Null|NULL|[-+]?[0-9_.:]+(?:[eE][-+]?[0-9]+)?|[-+]?\.(?:inf|Inf|INF|nan|NaN|NAN)|0[bxo][0-9a-fA-F_]+)$`)

func yaml11NotString(s string) bool { return s == "" || yaml11Special.MatchString(s) }

func hasTrailingSpaceLine(s string) bool {
	for line := range strings.SplitSeq(s, "\n") {
		if strings.HasSuffix(line, " ") {
			return true
		}
	}
	return strings.HasPrefix(s, " ") || strings.HasPrefix(s, "\n")
}

// jsonNode turns a JSON object or list into a block-style YAML node with
// encoding/json's tokenizer (the same parser discovery validated it with):
// key order is kept, numbers keep their text, strings are written quoted.
func jsonNode(text string) (*yaml.Node, error) {
	dec := json.NewDecoder(strings.NewReader(text))
	dec.UseNumber()
	n, err := jsonValue(dec, 0)
	if err != nil {
		return nil, errors.New("structured value is not valid JSON")
	}
	if n.Kind != yaml.MappingNode && n.Kind != yaml.SequenceNode {
		return nil, errors.New("structured value is not a JSON object or list")
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("structured value has trailing data after the JSON")
	}
	return n, nil
}

func jsonValue(dec *json.Decoder, depth int) (*yaml.Node, error) {
	if depth > 64 {
		return nil, errors.New("too deep")
	}
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			n := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				k, ok := kt.(string)
				if !ok {
					return nil, errors.New("object key is not a string")
				}
				v, err := jsonValue(dec, depth+1)
				if err != nil {
					return nil, err
				}
				n.Content = append(n.Content, keyNodeQuotedIfNeeded(k), v)
			}
			_, err := dec.Token() // }
			return n, err
		case '[':
			n := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
			for dec.More() {
				v, err := jsonValue(dec, depth+1)
				if err != nil {
					return nil, err
				}
				n.Content = append(n.Content, v)
			}
			_, err := dec.Token() // ]
			return n, err
		}
		return nil, errors.New("unexpected delimiter")
	case string:
		n := stringNode(t)
		if n.Style == yaml.LiteralStyle {
			// Nested multi-line strings stay double-quoted: exact and
			// independent of indentation.
			n.Style = yaml.DoubleQuotedStyle
		}
		return n, nil
	case json.Number:
		tag := "!!float"
		if _, err := t.Int64(); err == nil {
			tag = "!!int"
		}
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: tag, Value: t.String()}, nil
	case bool:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: fmt.Sprint(t)}, nil
	case nil:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null", Value: "null"}, nil
	}
	return nil, errors.New("unexpected token")
}

func keyNodeQuotedIfNeeded(k string) *yaml.Node {
	n := keyNode(k)
	if strings.ContainsAny(k, ":#{}[],&*!|>'\"%@`\n") || strings.TrimSpace(k) != k {
		n.Style = yaml.DoubleQuotedStyle
	}
	return n
}

// equalValue reports whether node already holds d's value. A scalar whose
// text equals the value is equal whatever its YAML type: a hand-written
// "port: 1883" stays an int instead of turning into "1883".
func equalValue(node *yaml.Node, d Desired) (bool, error) {
	if d.Structured {
		want, err := jsonNode(d.Value.Reveal())
		if err != nil {
			return false, err
		}
		if node.Kind != yaml.MappingNode && node.Kind != yaml.SequenceNode {
			return false, nil
		}
		return canonical(node) == canonical(want), nil
	}
	if node.Kind != yaml.ScalarNode || node.Tag == "!!null" {
		return false, nil
	}
	if node.Tag != "!!str" && node.Tag != "!!int" && node.Tag != "!!bool" && node.Tag != "!!float" {
		return false, nil
	}
	return secret.New(node.Value).Equal(d.Value), nil
}

// canonical renders a node tree as a comparable string: mapping keys
// sorted, scalars as resolved tag plus text. No decoding into Go numbers,
// so a value like 1e400 compares equal to itself instead of failing.
func canonical(n *yaml.Node) string {
	var b strings.Builder
	var walk func(*yaml.Node)
	walk = func(n *yaml.Node) {
		switch n.Kind {
		case yaml.DocumentNode:
			for _, c := range n.Content {
				walk(c)
			}
		case yaml.MappingNode:
			type pair struct{ k, v string }
			var pairs []pair
			for i := 0; i+1 < len(n.Content); i += 2 {
				var kb strings.Builder
				kb.WriteString(strconv.Quote(n.Content[i].Value))
				var vb strings.Builder
				sub := canonical(n.Content[i+1])
				vb.WriteString(sub)
				pairs = append(pairs, pair{kb.String(), vb.String()})
			}
			sort.Slice(pairs, func(i, j int) bool { return pairs[i].k < pairs[j].k })
			b.WriteByte('{')
			for _, p := range pairs {
				b.WriteString(p.k + ":" + p.v + ",")
			}
			b.WriteByte('}')
		case yaml.SequenceNode:
			b.WriteByte('[')
			for _, c := range n.Content {
				walk(c)
				b.WriteByte(',')
			}
			b.WriteByte(']')
		case yaml.AliasNode:
			if n.Alias != nil {
				walk(n.Alias)
			}
		default:
			b.WriteString(n.ShortTag() + "=" + strconv.Quote(n.Value))
		}
	}
	walk(n)
	return b.String()
}

// Keys parses a secrets file and returns its top-level keys, for the UI's
// view of a file it does not manage yet. No value leaves this function.
func Keys(content []byte) ([]string, error) {
	var doc yaml.Node
	if len(bytes.TrimSpace(content)) == 0 {
		return nil, nil
	}
	if err := yaml.Unmarshal(content, &doc); err != nil {
		return nil, fmt.Errorf("render: not valid YAML: %w", err)
	}
	root, err := ensureMapping(&doc)
	if err != nil {
		return nil, err
	}
	var out []string
	for i := 0; i+1 < len(root.Content); i += 2 {
		out = append(out, root.Content[i].Value)
	}
	return out, nil
}
