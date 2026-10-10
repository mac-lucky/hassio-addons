// Package httperr renders a failed HTTP response's body into a short,
// single-line string, so an error carries more than a bare status code.
// Supervisor and Core both put their account in a top-level "message",
// which Detail prefers, falling back to the raw bytes for bare text or a
// proxy's HTML. The result is always bounded and always one line: it is
// rendered one error per row.
package httperr

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"unicode"
)

const (
	// readLimit caps the bytes read, so a multi-megabyte error page
	// cannot make this package allocate it.
	readLimit = 16 << 10
	// maxDetailChars bounds the rendered detail: room for a full
	// Supervisor validation message, still one readable row.
	maxDetailChars = 400
	// truncationSuffix marks a detail that was cut short.
	truncationSuffix = " ... (truncated)"
)

// Detail reads resp's body and renders it as one bounded, single-line
// string, or "" when there is nothing printable. The body is consumed;
// callers use this only on the non-2xx path.
func Detail(resp *http.Response) string {
	if resp == nil || resp.Body == nil {
		return ""
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, readLimit+1))
	if err != nil && len(raw) == 0 {
		return ""
	}
	overRead := len(raw) > readLimit
	if overRead {
		raw = raw[:readLimit]
	}
	return render(raw, overRead)
}

// Suffix is Detail formatted for appending to an error message: ": " and
// the detail, or "" when there is no usable body.
func Suffix(resp *http.Response) string {
	return SuffixOf(Detail(resp))
}

// SuffixOf formats an already-read detail as Suffix formats its own, for
// a caller that also logs it and so may read the body only once.
func SuffixOf(detail string) string {
	if detail == "" {
		return ""
	}
	return ": " + detail
}

// render turns one already-bounded body into the final detail string.
// overRead means reading stopped at readLimit with bytes still pending,
// announced even when the rendered text needs no truncating itself.
func render(raw []byte, overRead bool) string {
	text := messageField(raw)
	if text == "" {
		text = string(raw)
	}
	text = oneLine(text)
	if text == "" {
		return ""
	}

	chars := []rune(text)
	if len(chars) > maxDetailChars {
		return string(chars[:maxDetailChars]) + truncationSuffix
	}
	if overRead {
		return text + truncationSuffix
	}
	return text
}

// messageField returns raw's top-level "message" when raw is a JSON
// object carrying a string one - the shape of both Supervisor's error
// envelope and Core's json_message. Anything else returns "".
func messageField(raw []byte) string {
	var decoded struct {
		Message any `json:"message"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return ""
	}
	s, _ := decoded.Message.(string)
	return s
}

// oneLine collapses whitespace runs into single spaces and drops
// non-printable runes, so even an HTML error page renders as one row.
func oneLine(s string) string {
	cleaned := strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return ' '
		}
		if !unicode.IsPrint(r) {
			return -1
		}
		return r
	}, s)
	return strings.Join(strings.Fields(cleaned), " ")
}
