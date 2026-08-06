// Package difftext renders the plan text for a registry-shaped change,
// shared by all five manifest layers so a plan reads the same whichever
// produced it. The value literals approximate Python's repr (single
// quotes, True/False/None), matching the Python implementation this
// add-on replaced.
package difftext

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/pmezard/go-difflib/difflib"
)

// diffContext is how many unchanged lines frame each hunk; 3 is Python
// difflib.unified_diff's default.
const diffContext = 3

// int64Bound bounds ReprValue's whole-number shortcut: int64 holds
// [-int64Bound, int64Bound), and a conversion outside it saturates,
// which would render 1e21 as "9223372036854775807".
const int64Bound = 1 << 63

// UnifiedDiff renders a and b (each a []string of complete,
// "\n"-terminated lines) as a Python difflib.unified_diff-compatible
// unified diff.
func UnifiedDiff(a, b []string, fromFile, toFile string) string {
	diff := difflib.UnifiedDiff{
		A: a, B: b,
		FromFile: fromFile, ToFile: toFile,
		Context: diffContext,
	}
	text, err := difflib.GetUnifiedDiffString(diff)
	if err != nil {
		// Only errors if SequenceMatcher setup fails, which cannot happen
		// for the plain []string inputs the manifest layers build.
		return ""
	}
	return text
}

// SortedKeys returns m's keys in ascending order, so a manifest renders
// the same plan text on every run rather than in Go's map order. Generic
// over the value type: internal/secretref folds a set through it too.
func SortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// PyRepr approximates Python's repr() for a string: single-quoted, with
// backslashes and single quotes escaped and nothing else touched.
// Iterates bytes, not runes, so invalid UTF-8 survives unrewritten.
func PyRepr(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('\'')
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '\\':
			b.WriteString(`\\`)
		case '\'':
			b.WriteString(`\'`)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('\'')
	return b.String()
}

// ReprValue approximates Python's repr() for a decoded YAML/JSON value:
// strings through PyRepr, booleans capitalized, nil as "None", lists and
// mappings in Python literal style with keys sorted, anything else %v.
//
// Whole-number floats lose the decimal point (up to int64Bound), so an
// integer field does not read as changed just for crossing the JSON
// boundary that decoded it to float64.
func ReprValue(v any) string {
	return ReprValueWithSentinel(v, nil)
}

// ReprValueWithSentinel renders v as ReprValue does, except sentinel
// (when non-nil) is consulted first for v and every nested value, and its
// string wins. For internal/addonopts, whose "no value at all" marker is
// an ordinary mapping that must render as "(unset)", not "None".
func ReprValueWithSentinel(v any, sentinel func(any) (string, bool)) string {
	if sentinel != nil {
		if s, ok := sentinel(v); ok {
			return s
		}
	}
	switch vv := v.(type) {
	case nil:
		return "None"
	case bool:
		if vv {
			return "True"
		}
		return "False"
	case string:
		return PyRepr(vv)
	case int:
		return strconv.Itoa(vv)
	case int64:
		return strconv.FormatInt(vv, 10)
	case float64:
		// Checked against int64's asymmetric bounds, so -2^63 keeps its
		// spelling; NaN fails Trunc and the infinities fail the range.
		if vv == math.Trunc(vv) && vv >= -int64Bound && vv < int64Bound {
			return strconv.FormatInt(int64(vv), 10)
		}
		return strconv.FormatFloat(vv, 'g', -1, 64)
	case []any:
		parts := make([]string, len(vv))
		for i, item := range vv {
			parts[i] = ReprValueWithSentinel(item, sentinel)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case map[string]any:
		keys := SortedKeys(vv)
		parts := make([]string, len(keys))
		for i, k := range keys {
			parts[i] = PyRepr(k) + ": " + ReprValueWithSentinel(vv[k], sentinel)
		}
		return "{" + strings.Join(parts, ", ") + "}"
	default:
		return fmt.Sprintf("%v", vv)
	}
}

// DeepEqual compares two decoded-YAML/JSON values exactly: an int and a
// float64 of the same magnitude are NOT equal, and list order matters.
// Used by internal/dashboards, where both sides are already JSON-shaped
// so a numeric-type difference is a real one; hand-authored fields want
// DeepEqualNumbersByValue.
func DeepEqual(a, b any) bool {
	return deepEqual(a, b, DeepEqual)
}

// DeepEqualNumbersByValue is DeepEqual except numeric leaves compare by
// value at every depth, so internal/registries does not see drift where
// YAML's int 8080 met a live float64. A number is still never equal to
// the string "8080".
func DeepEqualNumbersByValue(a, b any) bool {
	if af, aok := asFloat(a); aok {
		bf, bok := asFloat(b)
		return bok && af == bf
	}
	return deepEqual(a, b, DeepEqualNumbersByValue)
}

// deepEqual is the structural walk both exported comparisons share; elem
// is how each leaf is compared, so recursion goes back out through it.
func deepEqual(a, b any, elem func(a, b any) bool) bool {
	switch av := a.(type) {
	case []any:
		bv, ok := b.([]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for i := range av {
			if !elem(av[i], bv[i]) {
				return false
			}
		}
		return true
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for k, v := range av {
			bvv, present := bv[k]
			if !present || !elem(v, bvv) {
				return false
			}
		}
		return true
	default:
		return a == b
	}
}

// asFloat reports v's numeric value, for the numeric types a YAML or JSON
// decoder in this add-on can produce.
func asFloat(v any) (float64, bool) {
	switch vv := v.(type) {
	case int:
		return float64(vv), true
	case int64:
		return float64(vv), true
	case float64:
		return vv, true
	case float32:
		return float64(vv), true
	default:
		return 0, false
	}
}
