// Package humanize renders values for people rather than for machines.
// A standard-library-only leaf, copied from the GitOps agent.
package humanize

import (
	"strings"
	"unicode/utf8"
)

// Truncate bounds s at maxLen bytes, marking that it did so. Cuts on a
// rune boundary so non-ASCII bytes do not become a replacement char in
// whatever renders the result (the event ring, a sensor attribute).
func Truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	const marker = " ... (truncated)"
	cut := maxLen - len(marker)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return strings.TrimRight(s[:cut], " \t\n") + marker
}
