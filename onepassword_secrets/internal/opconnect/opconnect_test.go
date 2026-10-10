package opconnect

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const sentinel = "s3ntinel-connect-value"

func fakeServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"name":"1Password Connect API","version":"1.8.3","dependencies":[{"service":"account_data","status":"ACTIVE"}]}`))
	})
	auth := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer good" {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"status":401,"message":"Invalid bearer token"}`))
				return
			}
			next(w, r)
		}
	}
	mux.HandleFunc("GET /v1/vaults", auth(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"id":"v1","name":"homeassistant","contentVersion":7,"items":1}]`))
	}))
	mux.HandleFunc("GET /v1/vaults/v1/items", auth(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"id":"i1","title":"homeassistant fan","version":3,"vault":{"id":"v1"},"updatedAt":"2026-10-01T10:00:00Z"}]`))
	}))
	mux.HandleFunc("GET /v1/vaults/v1/items/i1", auth(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, `{"id":"i1","title":"homeassistant fan","version":3,"vault":{"id":"v1"},
			"sections":[{"id":"s1","label":"meta"}],
			"fields":[{"id":"f1","type":"CONCEALED","label":"fan_token","value":%q},
			          {"id":"f2","type":"STRING","label":"rotation","value":"r3-manual","section":{"id":"s1"}},
			          {"id":"f3","type":"STRING","label":"odd","value":42}]}`, sentinel)
	}))
	mux.HandleFunc("GET /v1/vaults/broken/items", auth(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, `[{"id":"x","title":%q`, sentinel)
	}))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestClient(t *testing.T) {
	srv := fakeServer(t)
	c := &Client{BaseURL: srv.URL, Token: "good"}
	ctx := context.Background()

	h, err := c.Health(ctx)
	if err != nil || h.Version != "1.8.3" || !h.Synced() {
		t.Fatalf("Health = %+v, %v", h, err)
	}
	vaults, err := c.Vaults(ctx)
	if err != nil || len(vaults) != 1 || vaults[0].Name != "homeassistant" {
		t.Fatalf("Vaults = %+v, %v", vaults, err)
	}
	items, err := c.Items(ctx, "v1")
	if err != nil || len(items) != 1 || items[0].Version != 3 {
		t.Fatalf("Items = %+v, %v", items, err)
	}
	item, err := c.Item(ctx, "v1", "i1")
	if err != nil {
		t.Fatal(err)
	}
	if len(item.Fields) != 3 || item.Fields[0].Value.Reveal() != sentinel || item.Fields[1].Section != "meta" {
		t.Fatalf("Item fields = %+v", item.Fields)
	}
	if !item.Fields[2].Value.IsZero() {
		t.Fatal("a non-string value should be dropped")
	}
	if strings.Contains(fmt.Sprintf("%+v %v", item, item), sentinel) {
		t.Fatal("item formatting leaked a value")
	}
}

func TestClientErrorsCarryNoBody(t *testing.T) {
	srv := fakeServer(t)
	bad := &Client{BaseURL: srv.URL, Token: "bad"}
	_, err := bad.Vaults(context.Background())
	if !IsUnauthorized(err) || !strings.Contains(err.Error(), "Invalid bearer token") {
		t.Fatalf("want a 401 StatusError, got %v", err)
	}
	good := &Client{BaseURL: srv.URL, Token: "good"}
	_, err = good.Items(context.Background(), "broken")
	if err == nil || strings.Contains(err.Error(), sentinel) {
		t.Fatalf("decode error must not quote the body: %v", err)
	}
	_, err = good.Item(context.Background(), "v1", "nope")
	if !IsNotFound(err) {
		t.Fatalf("want 404, got %v", err)
	}
}

func TestClientScrubsURLUserinfo(t *testing.T) {
	c := &Client{BaseURL: "http://user:pa55word@127.0.0.1:1", Token: "x"}
	_, err := c.Vaults(context.Background())
	if err == nil || strings.Contains(err.Error(), "pa55word") {
		t.Fatalf("error leaked userinfo: %v", err)
	}
}

func TestTokenExpiry(t *testing.T) {
	enc := func(payload string) string {
		return "eyJhbGciOiJFUzI1NiJ9." + base64.RawURLEncoding.EncodeToString([]byte(payload)) + ".sig"
	}
	exp, ok := TokenExpiry(enc(`{"exp":1791763200,"sub":"x"}`))
	if !ok || !exp.Equal(time.Unix(1791763200, 0).UTC()) {
		t.Fatalf("TokenExpiry = %v, %v", exp, ok)
	}
	for _, tok := range []string{"", "a.b", enc(`{"sub":"x"}`), enc(`not json`), "a.!!!.c", enc(`{"exp":-1}`)} {
		if _, ok := TokenExpiry(tok); ok {
			t.Fatalf("TokenExpiry(%q) should fail", tok)
		}
	}
}
