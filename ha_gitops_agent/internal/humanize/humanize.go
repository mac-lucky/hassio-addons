// Package humanize renders values for people rather than for machines.
// A standard-library-only leaf, so gitsync, recon and web can all use it
// without importing each other (execx, fsx and httpx exist for the same
// reason).
package humanize

import (
	"fmt"
	"strings"
	"time"
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

// Bytes renders n as "512 B", "1.5 KB", "100.0 MB". Binary units, like
// Home Assistant's frontend and the 100 << 20 limits this is read
// against.
func Bytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

// Duration renders d as the run-history card reads it: "0.9s", "4.2s",
// "3m 12s". One decimal below a minute, where almost every run lands;
// whole seconds above it, which a backup wait reaches. A negative d
// renders as "0.0s" rather than "-0.3s".
func Duration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	return fmt.Sprintf("%dm %02ds", int64(d/time.Minute), int64(d%time.Minute/time.Second))
}
