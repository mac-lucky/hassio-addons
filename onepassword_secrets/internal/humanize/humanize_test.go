package humanize

import "testing"

func TestTruncate(t *testing.T) {
	cases := map[string]string{
		"short":                   "short",
		"0123456789012345678901":  "0123456789012345678901",
		"01234567890123456789012": "012345 ... (truncated)",
		// The cut lands inside a two-byte rune and backs off to its start.
		"ażółć0123456789abcdef": "ażó ... (truncated)",
	}
	for in, want := range cases {
		if got := Truncate(in, 22); got != want {
			t.Errorf("Truncate(%q, 22) = %q, want %q", in, got, want)
		}
	}
}
