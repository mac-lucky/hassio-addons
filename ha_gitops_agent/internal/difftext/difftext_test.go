package difftext

import (
	"math"
	"reflect"
	"strings"
	"testing"
)

// --- SortedKeys -------------------------------------------------------------

func TestSortedKeysIsAscendingAndTotal(t *testing.T) {
	got := SortedKeys(map[string]any{"level": 1, "aliases": nil, "name": "x", "Name": "y"})
	want := []string{"Name", "aliases", "level", "name"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("SortedKeys = %v, want %v", got, want)
	}
}

func TestSortedKeysOfAnEmptyOrNilMapIsAnEmptyNonNilSlice(t *testing.T) {
	for name, m := range map[string]map[string]any{"empty": {}, "nil": nil} {
		t.Run(name, func(t *testing.T) {
			got := SortedKeys(m)
			if got == nil || len(got) != 0 {
				t.Errorf("SortedKeys = %#v, want an empty non-nil slice", got)
			}
		})
	}
}

// --- PyRepr -----------------------------------------------------------------

func TestPyRepr(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		// The plan lines internal/registries' own tests assert on.
		{"plain", "Ground floor", `'Ground floor'`},
		{"colons are not special", "0:05:00", `'0:05:00'`},
		{"empty", "", `''`},
		{"single quote", "it's", `'it\'s'`},
		{"backslash", `C:\path`, `'C:\\path'`},
		{"both, backslash first", `a\'b`, `'a\\\'b'`},
		{"newline is not escaped", "a\nb", "'a\nb'"},
		{"multi-byte runes pass through", "Wohnzimmer \u00e4\u00f6\u00fc", "'Wohnzimmer \u00e4\u00f6\u00fc'"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := PyRepr(c.in); got != c.want {
				t.Errorf("PyRepr(%q) = %s, want %s", c.in, got, c.want)
			}
		})
	}
}

// TestPyReprPreservesInvalidUTF8 pins the byte iteration: ranging over
// runes would rewrite raw bytes to U+FFFD on their way into a diff.
func TestPyReprPreservesInvalidUTF8(t *testing.T) {
	in := "a\xffb"
	want := "'a\xffb'"
	if got := PyRepr(in); got != want {
		t.Errorf("PyRepr(%q) = %q, want %q", in, got, want)
	}
}

// --- ReprValue --------------------------------------------------------------

func TestReprValue(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want string
	}{
		// Scalars, as addonopts' and entities' plan lines assert them
		// ("dirsfirst: False -> True", "log_level: 'debug' -> None").
		{"nil", nil, "None"},
		{"true", true, "True"},
		{"false", false, "False"},
		{"string", "debug", `'debug'`},
		{"string needing escapes", `it's a \ thing`, `'it\'s a \\ thing'`},

		// Numbers, as registries' plan lines assert them straight from
		// YAML ints ("level: 0", "level: 1").
		{"int", 0, "0"},
		{"int again", 1, "1"},
		{"negative int", -7, "-7"},
		{"int64", int64(9007199254740993), "9007199254740993"},
		{"whole float64 from JSON", float64(8080), "8080"},
		{"whole float64 that Go would print in exponent form", float64(1000000), "1000000"},
		{"negative whole float64", float64(-42), "-42"},
		{"fractional float64", 1.5, "1.5"},
		{"float32 has no literal form and falls back to Go", float32(1.5), "1.5"},

		// Lists, as registries' input_select options lines assert them:
		// "options: ['c', 'b', 'a']".
		{"empty list", []any{}, "[]"},
		{"list of strings", []any{"c", "b", "a"}, `['c', 'b', 'a']`},
		{"list of strings, sorted", []any{"a", "b", "c"}, `['a', 'b', 'c']`},
		{"mixed list", []any{1, "a", true, nil}, `[1, 'a', True, None]`},
		{"nested list", []any{[]any{1, 2}}, "[[1, 2]]"},

		// Mappings, keys sorted and quoted through PyRepr.
		{"empty map", map[string]any{}, "{}"},
		{"map", map[string]any{"b": 2, "a": "x"}, `{'a': 'x', 'b': 2}`},
		{"map key needing escapes", map[string]any{`it's`: 1}, `{'it\'s': 1}`},
		{
			"nested map",
			map[string]any{"network": map[string]any{"port": 8080}},
			`{'network': {'port': 8080}}`,
		},
		{
			"list of maps",
			[]any{map[string]any{"port": 8080}, map[string]any{"port": 443}},
			`[{'port': 8080}, {'port': 443}]`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ReprValue(c.in); got != c.want {
				t.Errorf("ReprValue(%#v) = %s, want %s", c.in, got, c.want)
			}
		})
	}
}

// TestReprValueFloatsTheIntegerShortcutMustNotClaim covers float64s that
// cannot survive the cast to int64: an out-of-range conversion saturates
// rather than failing, so 1e21 once rendered as "9223372036854775807".
func TestReprValueFloatsTheIntegerShortcutMustNotClaim(t *testing.T) {
	for _, c := range []struct {
		in   float64
		want string
	}{
		{math.Inf(1), "+Inf"},
		{math.Inf(-1), "-Inf"},
		{math.NaN(), "NaN"},
		{1e21, "1e+21"},
		{-1e21, "-1e+21"},
		{math.MaxFloat64, "1.7976931348623157e+308"},
	} {
		if got := ReprValue(c.in); got != c.want {
			t.Errorf("ReprValue(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestReprValueIntegerShortcutBoundsAreInt64s covers both ends of the
// asymmetric range: -2^63 keeps its exact spelling, +2^63 falls back to
// the float one. A magnitude check would be wrong by one value.
func TestReprValueIntegerShortcutBoundsAreInt64s(t *testing.T) {
	cases := []struct {
		name string
		in   float64
		want string
	}{
		{"the most negative int64", -9223372036854775808, "-9223372036854775808"},
		{"one past the largest int64", 9223372036854775808, "9.223372036854776e+18"},
		{"one below the most negative int64", -9223372036854777856, "-9.223372036854778e+18"},
		{"2^62", float64(1 << 62), "4611686018427387904"},
		{"-2^62", float64(-(1 << 62)), "-4611686018427387904"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ReprValue(c.in); got != c.want {
				t.Errorf("ReprValue(%v) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestReprValueIsDeterministicForMaps guards registries.ValuesEqual,
// which sorts list elements by their ReprValue: a map rendered in Go's
// map order would make drift detection nondeterministic.
func TestReprValueIsDeterministicForMaps(t *testing.T) {
	m := map[string]any{"a": 1, "b": 2, "c": 3, "d": 4, "e": 5, "f": 6, "g": 7, "h": 8}
	first := ReprValue(m)
	for i := 0; i < 50; i++ {
		if got := ReprValue(m); got != first {
			t.Fatalf("ReprValue = %s on run %d, want %s every time", got, i, first)
		}
	}
}

// --- ReprValueWithSentinel --------------------------------------------------

// absentish stands in for addonopts' AbsentMarker: a one-key mapping
// that must render as "(unset)", not "None".
func absentish(v any) (string, bool) {
	m, ok := v.(map[string]any)
	if !ok || len(m) != 1 {
		return "", false
	}
	if marked, ok := m["__absent__"].(bool); ok && marked {
		return "(unset)", true
	}
	return "", false
}

func TestReprValueWithSentinelReplacesMatchesAtEveryDepth(t *testing.T) {
	marker := map[string]any{"__absent__": true}
	cases := []struct {
		name string
		in   any
		want string
	}{
		{"top level", marker, "(unset)"},
		{"inside a list", []any{"debug", marker}, `['debug', (unset)]`},
		{"inside a map", map[string]any{"log_level": marker}, `{'log_level': (unset)}`},
		{"a null is still None", nil, "None"},
		{
			"a lookalike with extra keys is an ordinary map",
			map[string]any{"__absent__": true, "x": 1},
			`{'__absent__': True, 'x': 1}`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ReprValueWithSentinel(c.in, absentish); got != c.want {
				t.Errorf("ReprValueWithSentinel(%#v) = %s, want %s", c.in, got, c.want)
			}
		})
	}
}

func TestReprValueWithSentinelNilSentinelMatchesReprValue(t *testing.T) {
	v := map[string]any{"a": []any{1, "x", nil}, "b": true}
	if got, want := ReprValueWithSentinel(v, nil), ReprValue(v); got != want {
		t.Errorf("ReprValueWithSentinel(v, nil) = %s, want %s", got, want)
	}
}

// --- UnifiedDiff ------------------------------------------------------------

func TestUnifiedDiffRendersChangedLinesWithBothFileHeaders(t *testing.T) {
	before := []string{"level: 0\n", "name: 'Ground floor'\n"}
	after := []string{"level: 1\n", "name: 'Ground Level'\n"}

	got := UnifiedDiff(before, after, "live/floor/ground", "manifest/floor/ground")

	for _, want := range []string{
		"--- live/floor/ground",
		"+++ manifest/floor/ground",
		"-level: 0",
		"+level: 1",
		"-name: 'Ground floor'",
		"+name: 'Ground Level'",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("diff = %q, missing %q", got, want)
		}
	}
}

func TestUnifiedDiffAgainstNilRendersACreateAndADelete(t *testing.T) {
	lines := []string{"name: 'Ground floor'\n"}

	create := UnifiedDiff(nil, lines, "live/floor/ground", "manifest/floor/ground")
	if !strings.Contains(create, "+name: 'Ground floor'") {
		t.Errorf("create diff = %q, want the line added", create)
	}
	if strings.Contains(create, "-name:") {
		t.Errorf("create diff = %q, want nothing removed", create)
	}

	del := UnifiedDiff(lines, nil, "live/floor/ground", "manifest/floor/ground")
	if !strings.Contains(del, "-name: 'Ground floor'") {
		t.Errorf("delete diff = %q, want the line removed", del)
	}
	if strings.Contains(del, "+name:") {
		t.Errorf("delete diff = %q, want nothing added", del)
	}
}

func TestUnifiedDiffOfIdenticalInputIsEmpty(t *testing.T) {
	lines := []string{"level: 0\n", "name: 'Ground floor'\n"}
	if got := UnifiedDiff(lines, lines, "a", "b"); got != "" {
		t.Errorf("diff = %q, want empty", got)
	}
}

// TestUnifiedDiffKeepsThreeLinesOfContext pins the hunk shape to Python
// difflib.unified_diff's default, which the plan text was written for.
func TestUnifiedDiffKeepsThreeLinesOfContext(t *testing.T) {
	before := make([]string, 0, 10)
	after := make([]string, 0, 10)
	for _, k := range []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"} {
		before = append(before, k+": 1\n")
		after = append(after, k+": 1\n")
	}
	after[0] = "a: 2\n"

	got := UnifiedDiff(before, after, "live", "manifest")

	if !strings.Contains(got, "d: 1") {
		t.Errorf("diff = %q, want three lines of trailing context (through 'd')", got)
	}
	if strings.Contains(got, "e: 1") {
		t.Errorf("diff = %q, want context to stop after three lines (no 'e')", got)
	}
}

// --- DeepEqual --------------------------------------------------------------

func TestDeepEqual(t *testing.T) {
	cases := []struct {
		name string
		a, b any
		want bool
	}{
		{"identical scalars", "x", "x", true},
		{"different scalars", "x", "y", false},
		{"nil and nil", nil, nil, true},
		{"nil and a value", nil, "x", false},

		// The reflect.DeepEqual footgun this avoids: an empty container
		// from "{}" and a nil one from a missing key are the same here.
		{"empty map and nil map", map[string]any{}, map[string]any(nil), true},
		{"empty list and nil list", []any{}, []any(nil), true},

		{"maps of different length", map[string]any{"a": 1}, map[string]any{"a": 1, "b": 2}, false},
		{"maps with a missing key", map[string]any{"a": 1}, map[string]any{"b": 1}, false},
		{"lists of different length", []any{1}, []any{1, 2}, false},
		{"a map and a list", map[string]any{}, []any{}, false},
		{
			"nested equal",
			map[string]any{"views": []any{map[string]any{"cards": []any{"x"}}}},
			map[string]any{"views": []any{map[string]any{"cards": []any{"x"}}}},
			true,
		},
		{
			"nested differing",
			map[string]any{"views": []any{map[string]any{"cards": []any{"x"}}}},
			map[string]any{"views": []any{map[string]any{"cards": []any{"y"}}}},
			false,
		},

		// Order matters here, unlike registries.ValuesEqual.
		{"reordered list", []any{"a", "b"}, []any{"b", "a"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := DeepEqual(c.a, c.b); got != c.want {
				t.Errorf("DeepEqual(%#v, %#v) = %v, want %v", c.a, c.b, got, c.want)
			}
		})
	}
}

// TestDeepEqualDoesNotCoerceNumerics is why there are two equality
// functions: dashboards compares documents JSON-shaped on both sides, so
// a numeric-type difference there is a real one.
func TestDeepEqualDoesNotCoerceNumerics(t *testing.T) {
	cases := []struct {
		name string
		a, b any
	}{
		{"bare scalar", int(8080), float64(8080)},
		{"nested in a map", map[string]any{"port": int(8080)}, map[string]any{"port": float64(8080)}},
		{"nested in a list", []any{int(8080)}, []any{float64(8080)}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if DeepEqual(c.a, c.b) {
				t.Errorf("DeepEqual(%#v, %#v) = true, want false", c.a, c.b)
			}
		})
	}
}

// --- DeepEqualNumbersByValue ------------------------------------------------

// TestDeepEqualNumbersByValueNormalizesNestedNumerics pins the guarantee
// at every depth: without it `options: {network: {port: 8080}}` reports
// drift every cycle, restarting the add-on if restart_on_change is set.
func TestDeepEqualNumbersByValueNormalizesNestedNumerics(t *testing.T) {
	cases := []struct {
		name   string
		before any
		after  any
	}{
		{"bare scalar", int(8080), float64(8080)},
		{"int64 against float64", int64(8080), float64(8080)},
		{"float32 against float64", float32(8080), float64(8080)},
		{"top-level list of scalars", []any{int(8080), int(443)}, []any{float64(8080), float64(443)}},
		{
			"nested map (the port-8080 repro)",
			map[string]any{"network": map[string]any{"port": int(8080)}},
			map[string]any{"network": map[string]any{"port": float64(8080)}},
		},
		{
			"list of maps",
			[]any{map[string]any{"port": int(8080)}, map[string]any{"port": int(443)}},
			[]any{map[string]any{"port": float64(8080)}, map[string]any{"port": float64(443)}},
		},
		{
			"mixed depths",
			map[string]any{"count": int(3), "network": map[string]any{"ports": []any{int(80), int(443)}}},
			map[string]any{"count": float64(3), "network": map[string]any{"ports": []any{float64(80), float64(443)}}},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if !DeepEqualNumbersByValue(c.before, c.after) {
				t.Errorf("DeepEqualNumbersByValue(%#v, %#v) = false, want true", c.before, c.after)
			}
		})
	}
}

// TestDeepEqualNumbersByValueStillDetectsDrift keeps the normalization
// narrow: only same value, different numeric type may compare equal.
func TestDeepEqualNumbersByValueStillDetectsDrift(t *testing.T) {
	cases := []struct {
		name   string
		before any
		after  any
	}{
		{
			"nested value genuinely differs",
			map[string]any{"network": map[string]any{"port": int(8080)}},
			map[string]any{"network": map[string]any{"port": int(9090)}},
		},
		{"a number is not its string spelling", int(8080), "8080"},
		{"a string is not a number", "8080", int(8080)},
		{"a number is not nil", int(0), nil},
		{"a number is not a bool", int(1), true},
		{"non-numeric leaves still compare exactly", []any{"a"}, []any{"b"}},
		{"list order still matters", []any{int(1), int(2)}, []any{int(2), int(1)}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if DeepEqualNumbersByValue(c.before, c.after) {
				t.Errorf("DeepEqualNumbersByValue(%#v, %#v) = true, want false", c.before, c.after)
			}
		})
	}
}

// TestDeepEqualNumbersByValueKeepsTheStructuralRules checks the swapped
// leaf comparison kept DeepEqual's structural behavior.
func TestDeepEqualNumbersByValueKeepsTheStructuralRules(t *testing.T) {
	if !DeepEqualNumbersByValue(map[string]any{}, map[string]any(nil)) {
		t.Error("empty map and nil map should compare equal")
	}
	if !DeepEqualNumbersByValue([]any{}, []any(nil)) {
		t.Error("empty list and nil list should compare equal")
	}
	if DeepEqualNumbersByValue(map[string]any{"a": 1}, map[string]any{"a": 1, "b": 2}) {
		t.Error("maps of different length should not compare equal")
	}
}
