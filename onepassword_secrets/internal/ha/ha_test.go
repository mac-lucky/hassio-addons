package ha

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSafeCheckReasonDropsValues(t *testing.T) {
	in := "Invalid config for 'xiaomi_miio' at configuration.yaml, line 41: expected str for dictionary value 'token', got 'SENTINEL-value'\n" +
		"Invalid config for 'rest' at packages/net.yaml, line 3: invalid url 'https://user:SENTINEL-pw@host'\n" +
		"Invalid config for 'xiaomi_miio' at configuration.yaml, line 41: again"
	got := SafeCheckReason(in)
	if strings.Contains(got, "SENTINEL") {
		t.Fatalf("value leaked: %s", got)
	}
	want := "Invalid config for 'xiaomi_miio' at configuration.yaml, line 41; Invalid config for 'rest' at packages/net.yaml, line 3 (details in Home Assistant's log)"
	if got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
	if got := SafeCheckReason("Platform error: SENTINEL-x"); strings.Contains(got, "SENTINEL") {
		t.Fatalf("unknown shape leaked: %s", got)
	}
}

// No answer and a gateway error are "no verdict" (err), never "invalid";
// an error from Core itself is a failed check.
func TestCheckConfigNoAnswerIsNotInvalid(t *testing.T) {
	answer := func(code int, body string) *Client {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(code)
			_, _ = io.WriteString(w, body)
		}))
		t.Cleanup(srv.Close)
		return &Client{Base: srv.URL}
	}
	gone := httptest.NewServer(http.NotFoundHandler())
	invalid := `{"result":"invalid","errors":"Invalid config for 'mqtt' at configuration.yaml, line 2: got 'SENTINEL'"}`
	cases := []struct {
		name     string
		c        *Client
		valid    bool
		answered bool
		reason   string
	}{
		{"valid", answer(200, `{"result":"valid","errors":null}`), true, true, ""},
		{"invalid", answer(200, invalid), false, true, "Invalid config for 'mqtt' at configuration.yaml, line 2 (details in Home Assistant's log)"},
		{"proxy 502", answer(502, "Bad Gateway"), false, false, ""},
		{"503", answer(503, ""), false, false, ""},
		{"core 500", answer(500, "SENTINEL"), false, true, "check_config returned HTTP 500"},
		{"core 404", answer(404, ""), false, true, "check_config returned HTTP 404"},
		{"no answer", &Client{Base: gone.URL}, false, false, ""},
	}
	// Closed only once every other server holds its port, so none of them
	// can be given this one.
	gone.Close()
	for _, tc := range cases {
		valid, reason, err := tc.c.CheckConfig(context.Background())
		if valid != tc.valid || (err == nil) != tc.answered || reason != tc.reason {
			t.Errorf("%s: valid %v reason %q err %v", tc.name, valid, reason, err)
		}
	}
}
