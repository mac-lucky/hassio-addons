package secretref

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	yaml "go.yaml.in/yaml/v3"
)

// resolved is the value every test here plants in secrets.yaml, made
// unmistakable because several assertions are "this string appears
// nowhere".
const resolved = "S3CRET-resolved"

// writeSecrets creates a config root holding contents as its secrets.yaml
// and returns a Resolver over it.
func writeSecrets(t *testing.T, contents string) (*Resolver, string) {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, fileName)
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return NewResolver(root), path
}

// --- shape ----------------------------------------------------------------

func TestIsRefAndTheNameItRefersTo(t *testing.T) {
	cases := []struct {
		in    string
		isRef bool
		name  string
	}{
		{"secret://anker_password", true, "anker_password"},
		{"secret://UPPER-and.dots:1", true, "UPPER-and.dots:1"},
		// Nothing after the scheme names nothing.
		{"secret://", false, ""},
		// Not trimmed: a space is malformed, never a lookup of "a".
		{"secret:// a", false, ""},
		{"secret://a b", false, ""},
		{"secret://a\tb", false, ""},
		{"secret://a\nb", false, ""},
		// Ordinary values, including near misses.
		{"", false, ""},
		{"hunter2", false, ""},
		{"secret:/anker", false, ""},
		{"SECRET://anker", false, ""},
		{"prefixed secret://anker", false, ""},
	}
	for _, tc := range cases {
		if got := isRef(tc.in); got != tc.isRef {
			t.Errorf("isRef(%q) = %v, want %v", tc.in, got, tc.isRef)
		}
		// A reference names everything after the scheme, untrimmed - the
		// same slice resolveValue takes.
		if !tc.isRef {
			continue
		}
		if got := tc.in[len(Scheme):]; got != tc.name {
			t.Errorf("name of %q = %q, want %q", tc.in, got, tc.name)
		}
	}
}

func TestContainsRefWalksNestedShapes(t *testing.T) {
	if !ContainsRef(map[string]any{"a": []any{map[string]any{"b": "secret://x"}}}) {
		t.Error("a reference nested under a slice under a map must be found")
	}
	if !ContainsRef(map[any]any{1: "secret://x"}) {
		t.Error("a non-string-keyed mapping must be walked too")
	}
	// Malformed counts: a caller masking on this must not print it.
	if !ContainsRef("secret://") {
		t.Error("a malformed reference is still written as one")
	}
	if ContainsRef(map[string]any{"a": []any{"plain", 3, true}}) {
		t.Error("nothing here is a reference")
	}
}

// --- resolution -----------------------------------------------------------

func TestResolveReturnsTheValue(t *testing.T) {
	r, _ := writeSecrets(t, "anker_password: "+resolved+"\n")
	got, err := r.resolve("anker_password")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != resolved {
		t.Errorf("Resolve = %q, want %q", got, resolved)
	}
}

// Why a Resolver rather than a free function: three layers resolve in one
// cycle and must all see one generation of the file.
func TestResolverReadsTheFileOnce(t *testing.T) {
	r, path := writeSecrets(t, "k: "+resolved+"\n")

	first, err := r.resolve("k")
	if err != nil {
		t.Fatalf("first Resolve: %v", err)
	}

	// Rewritten mid-cycle: a second read would hand back the new value and
	// two layers would disagree about what they applied.
	if err := os.WriteFile(path, []byte("k: rotated\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := r.resolve("k")
	if err != nil {
		t.Fatalf("second Resolve: %v", err)
	}

	if first != resolved || second != resolved {
		t.Errorf("Resolve = %q then %q, want %q both times", first, second, resolved)
	}
	if r.LoadCount() != 1 {
		t.Errorf("LoadCount = %d, want 1", r.LoadCount())
	}
}

// A missing file is cached too: twenty references, one stat.
func TestResolverCachesAFailedLoad(t *testing.T) {
	r := NewResolver(t.TempDir())
	for i := 0; i < 3; i++ {
		if _, err := r.resolve("k"); err == nil {
			t.Fatal("Resolve must fail when there is no secrets file")
		}
	}
	if r.LoadCount() != 1 {
		t.Errorf("LoadCount = %d, want 1", r.LoadCount())
	}
}

func TestResolveErrorClasses(t *testing.T) {
	missingFile := NewResolver(filepath.Join(t.TempDir(), "nope"))
	badYAML, _ := writeSecrets(t, "k: [unterminated\n")
	notMapping, _ := writeSecrets(t, "- one\n- two\n")
	populated, _ := writeSecrets(t, "k: v\nstructured:\n  a: 1\nnothing:\n")

	cases := []struct {
		what string
		r    *Resolver
		name string
		want string
	}{
		{"missing file", missingFile, "k", "does not exist"},
		{"unparseable", badYAML, "k", "not valid YAML"},
		{"not a mapping", notMapping, "k", "must be a mapping"},
		{"missing key", populated, "anker_password", "secrets.yaml has no key 'anker_password'"},
		{"mapping value", populated, "structured", "is not a scalar value"},
		{"no value at all", populated, "nothing", "is not a scalar value"},
	}
	for _, tc := range cases {
		_, err := tc.r.resolve(tc.name)
		if err == nil {
			t.Errorf("%s: want an error", tc.what)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error = %q, want it to contain %q", tc.what, err, tc.want)
		}
	}
}

func TestResolveRendersScalarsAsText(t *testing.T) {
	r, _ := writeSecrets(t, "port: 8123\nflag: true\nratio: 1.5\nquoted: \"0123\"\n")
	want := map[string]string{"port": "8123", "flag": "true", "ratio": "1.5", "quoted": "0123"}
	for name, expected := range want {
		got, err := r.resolve(name)
		if err != nil {
			t.Fatalf("Resolve(%q): %v", name, err)
		}
		if got != expected {
			t.Errorf("Resolve(%q) = %q, want %q", name, got, expected)
		}
	}
}

func TestNilResolverRefusesAReferenceRatherThanPassingItThrough(t *testing.T) {
	var r *Resolver
	if _, err := r.resolve("k"); err == nil {
		t.Fatal("a nil resolver must refuse")
	}
	if _, _, err := r.ResolveMap(map[string]any{"user": map[string]any{"password": "secret://k"}}); err == nil {
		t.Fatal("a nil resolver must refuse a map carrying a reference")
	}
	// No reference: nothing to resolve, nothing to fail.
	out, values, err := r.ResolveMap(map[string]any{"user": map[string]any{"country": "PL"}})
	if err != nil {
		t.Fatalf("ResolveMap: %v", err)
	}
	if len(values) != 0 {
		t.Errorf("values = %v, want none", values)
	}
	if !reflect.DeepEqual(out, map[string]any{"user": map[string]any{"country": "PL"}}) {
		t.Errorf("out = %+v", out)
	}
}

// --- ResolveMap -----------------------------------------------------------

func TestResolveMapReplacesNestedReferences(t *testing.T) {
	r, _ := writeSecrets(t, "pw: "+resolved+"\nuser: someone\n")
	in := map[string]any{
		"user": map[string]any{
			"password": "secret://pw",
			"username": "secret://user",
			"country":  "PL",
			"hosts":    []any{"a", "secret://pw", map[string]any{"deep": "secret://pw"}},
		},
	}
	out, values, err := r.ResolveMap(in)
	if err != nil {
		t.Fatalf("ResolveMap: %v", err)
	}

	want := map[string]any{
		"user": map[string]any{
			"password": resolved,
			"username": "someone",
			"country":  "PL",
			"hosts":    []any{"a", resolved, map[string]any{"deep": resolved}},
		},
	}
	if !reflect.DeepEqual(out, want) {
		t.Errorf("out = %+v, want %+v", out, want)
	}
	// Sorted and deduplicated, so an op built from this is byte-stable.
	if !reflect.DeepEqual(values, []string{resolved, "someone"}) {
		t.Errorf("values = %+v, want the two distinct values sorted", values)
	}
}

// The input map is what the caller persists, so mutating it would put a
// credential into state.json.
func TestResolveMapNeverMutatesItsInput(t *testing.T) {
	r, _ := writeSecrets(t, "pw: "+resolved+"\n")
	nested := map[string]any{"password": "secret://pw"}
	list := []any{"secret://pw"}
	in := map[string]any{"user": nested, "hosts": list}

	if _, _, err := r.ResolveMap(in); err != nil {
		t.Fatalf("ResolveMap: %v", err)
	}

	if nested["password"] != "secret://pw" {
		t.Errorf("nested map was mutated: %+v", nested)
	}
	if list[0] != "secret://pw" {
		t.Errorf("slice was mutated: %+v", list)
	}
	if !reflect.DeepEqual(in, map[string]any{
		"user":  map[string]any{"password": "secret://pw"},
		"hosts": []any{"secret://pw"},
	}) {
		t.Errorf("input was mutated: %+v", in)
	}
}

func TestResolveMapPassesNonReferencesThrough(t *testing.T) {
	r, _ := writeSecrets(t, "pw: "+resolved+"\n")
	in := map[string]any{"user": map[string]any{"name": "Workday", "count": 3, "on": true, "none": nil}}
	out, values, err := r.ResolveMap(in)
	if err != nil {
		t.Fatalf("ResolveMap: %v", err)
	}
	if !reflect.DeepEqual(out, in) {
		t.Errorf("out = %+v, want an unchanged copy", out)
	}
	if values != nil {
		t.Errorf("values = %+v, want nil when nothing was substituted", values)
	}
	// The map itself, not a deep copy: nothing was substituted, so there is
	// nothing to keep out of the caller's original.
	if reflect.ValueOf(out).Pointer() != reflect.ValueOf(in).Pointer() {
		t.Error("ResolveMap deep-copied a payload with no reference in it")
	}
}

func TestResolveMapRefusesAMalformedReference(t *testing.T) {
	r, _ := writeSecrets(t, "pw: "+resolved+"\n")
	_, _, err := r.ResolveMap(map[string]any{"user": map[string]any{"password": "secret://"}})
	if err == nil {
		t.Fatal("a reference naming nothing must be refused, not passed through as a password")
	}
	if !strings.Contains(err.Error(), "not a usable secret reference") {
		t.Errorf("error = %q", err)
	}
}

func TestResolveMapReportsTheMissingKeyAndNoValue(t *testing.T) {
	r, _ := writeSecrets(t, "other: "+resolved+"\n")
	_, _, err := r.ResolveMap(map[string]any{"user": map[string]any{"password": "secret://anker_password"}})
	if err == nil {
		t.Fatal("want an error for a key that is not in the file")
	}
	if !strings.Contains(err.Error(), "no key 'anker_password'") {
		t.Errorf("error = %q, want it to name the missing key", err)
	}
	if strings.Contains(err.Error(), resolved) {
		t.Errorf("error = %q, want no secrets file value in it", err)
	}
}

func TestResolveMapHandlesNilAndEmpty(t *testing.T) {
	r, _ := writeSecrets(t, "pw: "+resolved+"\n")
	out, values, err := r.ResolveMap(nil)
	if err != nil {
		t.Fatalf("ResolveMap(nil): %v", err)
	}
	if len(out) != 0 || values != nil {
		t.Errorf("out = %+v, values = %+v, want an empty map and no values", out, values)
	}
}

// An empty secrets file is an empty mapping, not a parse failure.
func TestEmptySecretsFileIsAnEmptyMapping(t *testing.T) {
	r, _ := writeSecrets(t, "")
	_, err := r.resolve("k")
	if err == nil || !strings.Contains(err.Error(), "no key 'k'") {
		t.Errorf("error = %v, want the missing-key message", err)
	}
}

// --- fidelity: tags and scalar text --------------------------------------

// A real secrets.yaml may hold these, and decoding into a plain `any`
// would DISCARD the tag and hand "more_secrets.yaml" or "MYTOK" to an
// integration as the credential.
func TestResolveRefusesHomeAssistantLoaderTags(t *testing.T) {
	r, _ := writeSecrets(t, "extra: !include more_secrets.yaml\ntok: !env_var MYTOK\nnested: !secret other\n")

	for _, tc := range []struct{ name, tag string }{
		{"extra", "!include"},
		{"tok", "!env_var"},
		{"nested", "!secret"},
	} {
		value, err := r.resolve(tc.name)
		if err == nil {
			t.Errorf("Resolve(%q) = %q, want a refusal: the tag is not a value", tc.name, value)
			continue
		}
		if !strings.Contains(err.Error(), tc.tag) {
			t.Errorf("Resolve(%q) error = %q, want it to name the tag %s", tc.name, err, tc.tag)
		}
	}
}

// The value goes on the wire as the file spells it; Go's own types would
// retype it on the way past.
func TestResolveKeepsTheFilesOwnScalarText(t *testing.T) {
	r, _ := writeSecrets(t,
		"pin: 007\n"+
			"token: 123456789012345678901234567890\n"+
			"version: 1.50\n"+
			"hex: 0x1f\n"+
			"padded: \"  spaced  \"\n")

	for name, want := range map[string]string{
		"pin":     "007",
		"token":   "123456789012345678901234567890",
		"version": "1.50",
		"hex":     "0x1f",
		"padded":  "  spaced  ",
	} {
		got, err := r.resolve(name)
		if err != nil {
			t.Errorf("Resolve(%q): %v", name, err)
			continue
		}
		if got != want {
			t.Errorf("Resolve(%q) = %q, want %q", name, got, want)
		}
	}
}

// A key YAML reads as an int stays addressable by its text, and one exotic
// entry does not disable every other reference.
func TestResolveIndexesKeysByTheirOwnText(t *testing.T) {
	r, _ := writeSecrets(t, "1883: mqtt-port\ntrue: yes-key\nanker_password: "+resolved+"\n")

	for name, want := range map[string]string{
		"1883":           "mqtt-port",
		"true":           "yes-key",
		"anker_password": resolved,
	} {
		got, err := r.resolve(name)
		if err != nil {
			t.Errorf("Resolve(%q): %v", name, err)
			continue
		}
		if got != want {
			t.Errorf("Resolve(%q) = %q, want %q", name, got, want)
		}
	}
}

// An anchor and an alias are an ordinary way to write a shared value.
func TestResolveFollowsAnAlias(t *testing.T) {
	r, _ := writeSecrets(t, "primary: &shared "+resolved+"\nsecondary: *shared\n")
	got, err := r.resolve("secondary")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != resolved {
		t.Errorf("Resolve = %q, want %q", got, resolved)
	}
}

func TestResolveRefusesStructuredValues(t *testing.T) {
	r, _ := writeSecrets(t, "list:\n  - a\nmapping:\n  a: 1\n")
	for _, name := range []string{"list", "mapping"} {
		if _, err := r.resolve(name); err == nil || !strings.Contains(err.Error(), "not a scalar value") {
			t.Errorf("Resolve(%q) error = %v, want the not-a-scalar refusal", name, err)
		}
	}
}

// yaml/v3 decodes a nested mapping with any non-string key as
// map[any]any, and declared data is only validated as map[string]any at
// its top two levels.
func TestResolveMapWalksNonStringKeyedMappings(t *testing.T) {
	r, _ := writeSecrets(t, "pw: "+resolved+"\n")

	var declared any
	if err := yaml.Unmarshal([]byte("user:\n  ports:\n    1883: secret://pw\n"), &declared); err != nil {
		t.Fatal(err)
	}
	in, _ := declared.(map[string]any)
	if _, isNonString := in["user"].(map[string]any)["ports"].(map[any]any); !isNonString {
		t.Fatal("the fixture no longer produces a map[any]any, so this test guards nothing")
	}

	out, values, err := r.ResolveMap(in)
	if err != nil {
		t.Fatalf("ResolveMap: %v", err)
	}
	ports := out["user"].(map[string]any)["ports"].(map[any]any)
	if ports[1883] != resolved {
		t.Errorf("ports = %+v, want the nested reference resolved", ports)
	}
	if !reflect.DeepEqual(values, []string{resolved}) {
		t.Errorf("values = %+v", values)
	}
	if !ContainsRef(in) {
		t.Error("ContainsRef must see through a map[any]any too")
	}
}
