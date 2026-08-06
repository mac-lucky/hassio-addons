package hook

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/differ"
)

type fakeAgent struct {
	mu    sync.Mutex
	calls int
	done  chan struct{}
}

func newFakeAgent() *fakeAgent {
	return &fakeAgent{done: make(chan struct{}, 8)}
}

func (f *fakeAgent) ReconcileNow(ctx context.Context) []differ.Change {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	f.done <- struct{}{}
	return nil
}

func (f *fakeAgent) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeAgent) waitForCall(t *testing.T) {
	t.Helper()
	select {
	case <-f.done:
	case <-time.After(2 * time.Second):
		t.Fatal("ReconcileNow was not called in time")
	}
}

func doReq(handler http.Handler, method, target string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	req.RemoteAddr = "203.0.113.7:54321"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestWebhookMatchingHeaderTokenReturns202AndTriggersReconcile(t *testing.T) {
	agent := newFakeAgent()
	handler := New(context.Background(), agent, "s3cret")

	rec := doReq(handler, http.MethodPost, "/webhook", map[string]string{"X-Gitops-Token": "s3cret"})

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}
	agent.waitForCall(t)
	if agent.callCount() != 1 {
		t.Errorf("reconcile calls = %d, want 1", agent.callCount())
	}
}

func TestWebhookMatchingQueryTokenReturns202(t *testing.T) {
	agent := newFakeAgent()
	handler := New(context.Background(), agent, "s3cret")

	rec := doReq(handler, http.MethodPost, "/webhook?token=s3cret", nil)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}
	agent.waitForCall(t)
}

func TestWebhookHeaderTokenTakesPrecedenceOverQuery(t *testing.T) {
	// Documents which one wins: validToken checks the header first.
	agent := newFakeAgent()
	handler := New(context.Background(), agent, "s3cret")

	rec := doReq(handler, http.MethodPost, "/webhook?token=wrong", map[string]string{"X-Gitops-Token": "s3cret"})

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}
}

func TestWebhookMismatchedTokenReturns403AndDoesNotTrigger(t *testing.T) {
	agent := newFakeAgent()
	handler := New(context.Background(), agent, "s3cret")

	rec := doReq(handler, http.MethodPost, "/webhook", map[string]string{"X-Gitops-Token": "wrong"})

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
	time.Sleep(20 * time.Millisecond)
	if agent.callCount() != 0 {
		t.Errorf("reconcile calls = %d, want 0", agent.callCount())
	}
}

func TestWebhookMissingTokenReturns403(t *testing.T) {
	agent := newFakeAgent()
	handler := New(context.Background(), agent, "s3cret")

	rec := doReq(handler, http.MethodPost, "/webhook", nil)

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
}

func TestWebhookEmptySecretAlwaysRejects(t *testing.T) {
	agent := newFakeAgent()
	handler := New(context.Background(), agent, "")

	rec := doReq(handler, http.MethodPost, "/webhook", map[string]string{"X-Gitops-Token": ""})

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 (a webhook with no configured secret must never accept)", rec.Code)
	}
}

func TestWebhookWrongMethodNotFound(t *testing.T) {
	agent := newFakeAgent()
	handler := New(context.Background(), agent, "s3cret")

	rec := doReq(handler, http.MethodGet, "/webhook", map[string]string{"X-Gitops-Token": "s3cret"})

	if rec.Code == http.StatusAccepted {
		t.Errorf("status = %d, want anything but 202 for a GET", rec.Code)
	}
	if agent.callCount() != 0 {
		t.Errorf("reconcile calls = %d, want 0", agent.callCount())
	}
}

func TestWebhookBusyAgentStillReturns202(t *testing.T) {
	// The handler never inspects what ReconcileNow decides to do, so a
	// busy reconciler still gets a 202.
	agent := newFakeAgent()
	handler := New(context.Background(), agent, "s3cret")

	rec := doReq(handler, http.MethodPost, "/webhook", map[string]string{"X-Gitops-Token": "s3cret"})

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 regardless of busy state", rec.Code)
	}
}

func TestRateLimiterSuppressesRapidRepeats(t *testing.T) {
	l := &rateLimiter{interval: time.Hour}
	// slog output is not observable here, so this exercises the gate:
	// last is only updated once within the interval.
	l.warn("first")
	first := l.last
	l.warn("second")
	if !l.last.Equal(first) {
		t.Error("last timestamp advanced on a call within the interval")
	}
}

func TestRateLimiterAllowsAfterInterval(t *testing.T) {
	l := &rateLimiter{interval: time.Millisecond}
	l.warn("first")
	first := l.last
	time.Sleep(5 * time.Millisecond)
	l.warn("second")
	if l.last.Equal(first) {
		t.Error("last timestamp did not advance after the interval elapsed")
	}
}

// panickingAgent stands in for a reconcile that blows up under gitsync
// or differ. entered closes while the panic unwinds, so the test can
// wait for it rather than racing it.
type panickingAgent struct {
	entered chan struct{}
}

func (p *panickingAgent) ReconcileNow(ctx context.Context) []differ.Change {
	defer close(p.entered)
	panic("gitsync exploded")
}

// A panic in the detached reconcile must not take the process down:
// net/http recovers a panicking handler, and this goroutine is not one.
// A regression crashes the test binary rather than failing.
func TestPanicInTheTriggeredReconcileDoesNotKillTheProcess(t *testing.T) {
	agent := &panickingAgent{entered: make(chan struct{})}
	handler := New(context.Background(), agent, "s3cret")

	rec := doReq(handler, http.MethodPost, "/webhook", map[string]string{"X-Gitops-Token": "s3cret"})

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}
	select {
	case <-agent.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the reconcile goroutine never ran")
	}
	// Let the recover finish, then prove the server still answers.
	time.Sleep(20 * time.Millisecond)
	if got := doReq(handler, http.MethodPost, "/webhook", nil).Code; got != http.StatusForbidden {
		t.Errorf("status = %d, want 403 - the handler should still be serving", got)
	}
}
