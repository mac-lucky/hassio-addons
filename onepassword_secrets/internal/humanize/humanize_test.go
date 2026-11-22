package humanize

import "testing"

func TestTruncate(t *testing.T) {
	cases := map[string]string{
		"short":                   "short",
		"0123456789012345678901":  "0123456789012345678901",
		"01234567890123456789012": "012345 ... (truncated)",
		// The cut lands inside a two-byte rune and backs off to its start.
		"a\u017c\u00f3\u0142\u01070123456789abcdef": "a\u017c\u00f3 ... (truncated)",
	}
	for in, want := range cases {
		if got := Truncate(in, 22); got != want {
			t.Errorf("Truncate(%q, 22) = %q, want %q", in, got, want)
		}
	}
}
