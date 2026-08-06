package httperr

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func respWithBody(body string) *http.Response {
	return &http.Response{StatusCode: http.StatusBadRequest, Body: io.NopCloser(strings.NewReader(body))}
}

// --- Detail(): which part of a body is surfaced -------------------------

func TestDetailRendersTheUsefulPartOfABody(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "supervisor error envelope surfaces its message",
			body: `{"result":"error","message":"App a0d7b954_chrony has invalid options: ` +
				`Missing required option 'log_level' in chrony (a0d7b954_chrony)."}`,
			want: "App a0d7b954_chrony has invalid options: " +
				"Missing required option 'log_level' in chrony (a0d7b954_chrony).",
		},
		{
			name: "core json_message body surfaces its message",
			body: `{"message":"User input malformed: expected str @ data['display_options']"}`,
			want: "User input malformed: expected str @ data['display_options']",
		},
		{
			name: "non-json body surfaces its raw text",
			body: "401: Unauthorized",
			want: "401: Unauthorized",
		},
		{
			name: "json carrying no message falls back to the raw body",
			body: `{"result":"error"}`,
			want: `{"result":"error"}`,
		},
		{
			name: "json carrying a non-string message falls back to the raw body",
			body: `{"message":["a","b"]}`,
			want: `{"message":["a","b"]}`,
		},
		{
			name: "json carrying an empty message falls back to the raw body",
			body: `{"message":""}`,
			want: `{"message":""}`,
		},
		{
			name: "an empty body has no detail at all",
			body: "",
			want: "",
		},
		{
			name: "a whitespace-only body has no detail at all",
			body: "\n\n  \t ",
			want: "",
		},
		{
			name: "non-printable bytes are dropped rather than rendered",
			body: "bad\x00value",
			want: "badvalue",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Detail(respWithBody(tt.body)); got != tt.want {
				t.Errorf("Detail() = %q, want %q", got, tt.want)
			}
		})
	}
}

// --- Detail(): one line, always -----------------------------------------

func TestDetailCollapsesAMultiLineBodyOntoOneLine(t *testing.T) {
	body := "Traceback (most recent call last):\n  File \"x.py\", line 1\n\n\tValueError: nope\n"
	got := Detail(respWithBody(body))

	if strings.ContainsAny(got, "\n\r\t") {
		t.Errorf("Detail() = %q, want no line breaks or tabs", got)
	}
	want := `Traceback (most recent call last): File "x.py", line 1 ValueError: nope`
	if got != want {
		t.Errorf("Detail() = %q, want %q", got, want)
	}
}

func TestDetailCollapsesAMultiLineJSONMessageOntoOneLine(t *testing.T) {
	got := Detail(respWithBody(`{"message":"first line\nsecond line\n\nthird line"}`))

	want := "first line second line third line"
	if got != want {
		t.Errorf("Detail() = %q, want %q", got, want)
	}
}

// --- Detail(): bounded ----------------------------------------------------

func TestDetailTruncatesAnOversizedRawBody(t *testing.T) {
	got := Detail(respWithBody(strings.Repeat("x", 5000)))

	if !strings.HasSuffix(got, truncationSuffix) {
		t.Fatalf("Detail() = %q, want it to say it was truncated", got)
	}
	body := strings.TrimSuffix(got, truncationSuffix)
	if len([]rune(body)) != maxDetailChars {
		t.Errorf("Detail() kept %d chars, want %d", len([]rune(body)), maxDetailChars)
	}
}

func TestDetailTruncatesAnOversizedJSONMessage(t *testing.T) {
	long := strings.Repeat("a", 5000)
	got := Detail(respWithBody(`{"result":"error","message":"` + long + `"}`))

	if !strings.HasSuffix(got, truncationSuffix) {
		t.Fatalf("Detail() = %q..., want it to say it was truncated", got[:min(len(got), 60)])
	}
	if want := strings.Repeat("a", maxDetailChars) + truncationSuffix; got != want {
		t.Errorf("Detail() = %q..., want the message truncated to %d chars", got[:min(len(got), 60)], maxDetailChars)
	}
}

// countingReader records how much of a body was consumed, so the read
// limit itself can be asserted, not only its effect.
type countingReader struct {
	src  io.Reader
	read int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.src.Read(p)
	c.read += n
	return n, err
}

func TestDetailNeverReadsMoreThanTheReadLimit(t *testing.T) {
	counter := &countingReader{src: strings.NewReader(strings.Repeat("x", 4<<20))}
	resp := &http.Response{StatusCode: http.StatusBadGateway, Body: io.NopCloser(counter)}

	Detail(resp)

	if counter.read > readLimit+1 {
		t.Errorf("read %d bytes off the body, want no more than %d", counter.read, readLimit+1)
	}
}

// --- Detail()/Suffix(): degenerate responses -------------------------------

func TestDetailToleratesAMissingResponseOrBody(t *testing.T) {
	if got := Detail(nil); got != "" {
		t.Errorf("Detail(nil) = %q, want empty", got)
	}
	if got := Detail(&http.Response{StatusCode: http.StatusBadRequest}); got != "" {
		t.Errorf("Detail() with no body = %q, want empty", got)
	}
}

func TestSuffixPrefixesADetailAndOmitsAnEmptyOne(t *testing.T) {
	if got := Suffix(respWithBody(`{"message":"nope"}`)); got != ": nope" {
		t.Errorf("Suffix() = %q, want %q", got, ": nope")
	}
	if got := Suffix(respWithBody("")); got != "" {
		t.Errorf("Suffix() on an empty body = %q, want empty", got)
	}
	if got := SuffixOf("already read"); got != ": already read" {
		t.Errorf("SuffixOf() = %q, want %q", got, ": already read")
	}
	if got := SuffixOf(""); got != "" {
		t.Errorf("SuffixOf(\"\") = %q, want empty", got)
	}
}
