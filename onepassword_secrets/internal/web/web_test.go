package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mac-lucky/hassio-addons/onepassword_secrets/internal/engine"
	"github.com/mac-lucky/hassio-addons/onepassword_secrets/internal/state"
)

type fakeAgent struct {
	mu       sync.Mutex
	status   engine.Status
	synced   int
	restarts int
}

func (f *fakeAgent) Status() engine.Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status
}

func (f *fakeAgent) SyncNow() {
	f.mu.Lock()
	f.synced++
	f.mu.Unlock()
}

func (f *fakeAgent) RestartCoreNow(context.Context) error {
	f.mu.Lock()
	f.restarts++
	f.mu.Unlock()
	return nil
}

func (f *fakeAgent) History(int) []state.Entry {
	return []state.Entry{{Time: time.Unix(100, 0), Kind: state.KindChange, Title: "Updated 1 key", Keys: []string{"wifi_password"}}}
}

func (f *fakeAgent) KeyHistory(key string, _ int) []state.Entry {
	return []state.Entry{{Time: time.Unix(100, 0), Kind: state.KindChange, Title: "Updated " + key, Keys: []string{key}}}
}

func sampleStatus() engine.Status {
	now := time.Now().UTC()
	return engine.Status{
		Version: "test", State: engine.StateHealthy, Headline: "In sync", Embedded: true,
		LastSync: now, Token: engine.TokenInfo{Known: true, ExpiresAt: now.Add(300 * 24 * time.Hour), DaysLeft: 300},
		Connect: engine.ConnectInfo{Reachable: true, Synced: true, ServerVersion: "1.8.3"},
		Files:   []engine.FileInfo{{Path: "secrets.yaml", Exists: true, Keys: 2, Managed: 1, Unmanaged: []string{"hand"}}},
		Secrets: []engine.SecretRow{
			{Key: "wifi_password", State: engine.RowSynced, Source: "homeassistant > wifi > wifi_password", VaultName: "homeassistant", ItemTitle: "wifi", FieldLabel: "wifi_password", Ref: "op://homeassistant/wifi/wifi_password", Files: []string{"secrets.yaml"}, UsedBy: []engine.Use{{Kind: "esphome", Label: "node", File: "esphome/node.yaml", Line: 3}}, RotatedAt: now.Add(-400 * 24 * time.Hour), Due: now.Add(-35 * 24 * time.Hour), Overdue: true, RotationPct: 100, Interval: "365d"},
			{Key: "hand", State: engine.RowUnmanaged, Files: []string{"secrets.yaml"}},
			{Key: "evil\u202ekey", State: engine.RowMissing},
		},
		Counts:   engine.Counts{Keys: 3, Managed: 1, Unmanaged: 1, Missing: 1, Overdue: 1},
		Problems: []engine.Problem{{Severity: "error", Title: "evil\u202ekey is missing", Fix: "Add a field."}},
		Activity: []state.Entry{{Time: now, Kind: state.KindRefresh, Title: "Reloaded template", Keys: []string{"wifi_password"}}},
	}
}

func request(t *testing.T, h http.Handler, method, target, remote string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, target, nil)
	r.RemoteAddr = remote
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

const ingress = ingressProxyAddr + ":40000"

func TestIngressGate(t *testing.T) {
	h := New(&fakeAgent{status: sampleStatus()})
	if w := request(t, h, "GET", "/", "172.30.33.5:1234", nil); w.Code != http.StatusForbidden {
		t.Fatalf("non-ingress GET = %d", w.Code)
	}
	if w := request(t, h, "GET", "/status.json", "172.30.33.5:1234", nil); w.Code != http.StatusForbidden {
		t.Fatalf("non-ingress status.json = %d", w.Code)
	}
	if w := request(t, h, "GET", "/", ingress, nil); w.Code != http.StatusOK {
		t.Fatalf("ingress GET = %d", w.Code)
	}
}

func TestSameOriginGuard(t *testing.T) {
	agent := &fakeAgent{status: sampleStatus()}
	h := New(agent)
	if w := request(t, h, "POST", "/sync", ingress, map[string]string{"Sec-Fetch-Site": "cross-site"}); w.Code != http.StatusForbidden {
		t.Fatalf("cross-site POST = %d", w.Code)
	}
	if agent.synced != 0 {
		t.Fatal("cross-site POST reached the agent")
	}
	if w := request(t, h, "POST", "/sync", ingress, map[string]string{"Sec-Fetch-Site": "same-origin"}); w.Code != http.StatusAccepted {
		t.Fatalf("same-origin POST = %d", w.Code)
	}
	if agent.synced != 1 {
		t.Fatal("sync not requested")
	}
}

func TestRestartRefusedWhileSyncing(t *testing.T) {
	s := sampleStatus()
	s.Syncing = true
	agent := &fakeAgent{status: s}
	if w := request(t, New(agent), "POST", "/restart-core", ingress, nil); w.Code != http.StatusConflict {
		t.Fatalf("restart while syncing = %d", w.Code)
	}
}

func TestPageRendersAndFragmentShortCircuits(t *testing.T) {
	h := New(&fakeAgent{status: sampleStatus()})
	w := request(t, h, "GET", "/", ingress, nil)
	body := w.Body.String()
	for _, want := range []string{"wifi_password", "rotation 35 days overdue", "Only in file", "&lt;U+202E&gt;", "static/app.css?v=" + assetVersion, `href="#` + keyID("wifi_password") + `"`, `id="` + keyID("wifi_password") + `"`, "esphome/node.yaml:3"} {
		if !strings.Contains(body, want) {
			t.Errorf("page missing %q", want)
		}
	}
	if strings.Contains(body, "\u202e") {
		t.Error("a bidi control reached the page unescaped")
	}
	if w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("security header missing")
	}

	frag := request(t, h, "GET", "/fragment", ingress, nil)
	if frag.Code != http.StatusOK {
		t.Fatalf("fragment = %d", frag.Code)
	}
	start := strings.Index(frag.Body.String(), "fragment?h=")
	hash := frag.Body.String()[start+len("fragment?h="):]
	hash = hash[:strings.IndexAny(hash, `"&`)]
	if again := request(t, h, "GET", "/fragment?h="+hash, ingress, nil); again.Code != http.StatusNoContent {
		t.Fatalf("unchanged fragment = %d", again.Code)
	}
}

func TestIdleFragmentCarriesOnlyTheCheckTime(t *testing.T) {
	agent := &fakeAgent{status: sampleStatus()}
	h := New(agent)
	first := request(t, h, "GET", "/fragment", ingress, nil).Body.String()
	start := strings.Index(first, "fragment?h=")
	hash := first[start+len("fragment?h="):]
	hash = hash[:strings.IndexAny(hash, `"&`)]

	agent.mu.Lock()
	agent.status.LastSync = agent.status.LastSync.Add(time.Minute)
	agent.mu.Unlock()
	w := request(t, h, "GET", "/fragment?h="+hash, ingress, nil)
	if w.Code != http.StatusNoContent || !strings.Contains(w.Header().Get("HX-Trigger"), "lastSync") {
		t.Fatalf("a check-time-only change = %d %q, want 204 with HX-Trigger", w.Code, w.Header().Get("HX-Trigger"))
	}
	agent.mu.Lock()
	agent.status.Headline = "Something else"
	agent.mu.Unlock()
	if w := request(t, h, "GET", "/fragment?h="+hash, ingress, nil); w.Code != http.StatusOK {
		t.Fatalf("a real change = %d, want 200", w.Code)
	}
}

func TestOddKeysStayLinkedAndQueryEscaped(t *testing.T) {
	s := sampleStatus()
	s.Secrets = append(s.Secrets, engine.SecretRow{Key: "my key&k=wifi_password", State: engine.RowMissing})
	body := request(t, New(&fakeAgent{status: s}), "GET", "/", ingress, nil).Body.String()
	id := keyID("my key&k=wifi_password")
	if !strings.Contains(body, `id="`+id+`"`) || !strings.Contains(body, `href="#`+id+`"`) {
		t.Fatal("tick and row ids differ for a key with a space")
	}
	if !strings.Contains(body, `key?k=my&#43;key%26k%3Dwifi_password`) && !strings.Contains(body, `key?k=my+key%26k%3Dwifi_password`) {
		t.Fatalf("history URL not query-escaped")
	}
}

func TestDueText(t *testing.T) {
	now := time.Now()
	cases := map[string]engine.SecretRow{
		"rotation overdue since today": {Due: now.Add(-time.Hour), Overdue: true},
		"rotation 3 days overdue":      {Due: now.Add(-3*24*time.Hour - time.Hour), Overdue: true},
		"rotation due in 10 days":      {Due: now.Add(10*24*time.Hour + time.Hour)},
		"rotate on compromise":         {Interval: "on-compromise"},
	}
	for want, row := range cases {
		if got := dueText(row); got != want {
			t.Errorf("dueText = %q, want %q", got, want)
		}
	}
}

func TestOtherRoutes(t *testing.T) {
	h := New(&fakeAgent{status: sampleStatus()})
	if w := request(t, h, "GET", "/key?k=wifi_password", ingress, nil); w.Code != 200 || !strings.Contains(w.Body.String(), "Updated wifi_password") {
		t.Fatalf("key = %d %s", w.Code, w.Body.String())
	}
	if w := request(t, h, "GET", "/history", ingress, nil); w.Code != 200 || !strings.Contains(w.Body.String(), "Updated 1 key") {
		t.Fatalf("history = %d", w.Code)
	}
	w := request(t, h, "GET", "/status.json", ingress, nil)
	var s engine.Status
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &s) != nil || s.Headline != "In sync" {
		t.Fatalf("status.json = %d %s", w.Code, w.Body.String())
	}
	if w := request(t, h, "GET", "/static/app.css", ingress, nil); w.Code != 200 || w.Header().Get("Cache-Control") != staticCacheControl {
		t.Fatalf("static = %d %q", w.Code, w.Header().Get("Cache-Control"))
	}
	if w := request(t, h, "GET", "/static/nope.css", ingress, nil); w.Code != 404 || w.Header().Get("Cache-Control") == staticCacheControl {
		t.Fatalf("missing static cached: %d", w.Code)
	}
}

func TestUnconfiguredShowsSetup(t *testing.T) {
	s := engine.Status{State: engine.StateUnconfigured, Headline: "Not set up yet", Setup: []engine.SetupStep{{Label: "Connect credentials set"}, {Label: "Access token set"}}}
	w := request(t, New(&fakeAgent{status: s}), "GET", "/", ingress, nil)
	if !strings.Contains(w.Body.String(), "op connect server create homeassistant") || !strings.Contains(w.Body.String(), "0 of 2 done") {
		t.Fatalf("setup not shown:\n%s", w.Body.String())
	}
}
