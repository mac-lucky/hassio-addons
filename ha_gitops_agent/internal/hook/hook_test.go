package hook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

type fakeAgent struct {
	ctxs  []context.Context
	mu    sync.Mutex
	calls int
	done  chan struct{}
}

func newFakeAgent() *fakeAgent {
	return &fakeAgent{done: make(chan struct{}, 8)}
}

func (f *fakeAgent) SyncNow(ctx context.Context) {
	f.mu.Lock()
	f.calls++
	f.ctxs = append(f.ctxs, ctx)
	f.mu.Unlock()
	f.done <- struct{}{}
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
		t.Fatal("SyncNow was not called in time")
	}
}

func doReq(handler http.Handler, method, target string, headers map[string]string) *httptest.ResponseRecorder {
	return doBodyReq(handler, method, target, nil, headers)
}

func doBodyReq(handler http.Handler, method, target string, body []byte, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, bytes.NewReader(body))
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

const pushBody = `{"ref":"refs/heads/main","after":"0123456789abcdef"}`

// sign is what a forge puts in its signature header: a hex HMAC-SHA256
// of the raw body keyed by the webhook secret.
func sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func TestWebhookValidSignatureReturns202AndTriggersOnce(t *testing.T) {
	body := []byte(pushBody)
	for _, tc := range []struct{ header, value string }{
		{"X-Hub-Signature-256", "sha256=" + sign("s3cret", body)},
		{"X-Forgejo-Signature", sign("s3cret", body)},
		{"X-Gitea-Signature", sign("s3cret", body)},
	} {
		t.Run(tc.header, func(t *testing.T) {
			agent := newFakeAgent()
			handler := New(context.Background(), agent, "s3cret")

			rec := doBodyReq(handler, http.MethodPost, "/webhook", body, map[string]string{tc.header: tc.value})

			if rec.Code != http.StatusAccepted {
				t.Fatalf("status = %d, want 202", rec.Code)
			}
			agent.waitForCall(t)
			time.Sleep(20 * time.Millisecond)
			if agent.callCount() != 1 {
				t.Errorf("reconcile calls = %d, want 1", agent.callCount())
			}
		})
	}
}

func TestWebhookForgejoDeliveryWithEverySignatureHeaderReturns202(t *testing.T) {
	// Forgejo signs one delivery under its own, Gitea's and GitHub's
	// header names at once.
	agent := newFakeAgent()
	handler := New(context.Background(), agent, "s3cret")
	body := []byte(pushBody)
	sig := sign("s3cret", body)

	rec := doBodyReq(handler, http.MethodPost, "/webhook", body, map[string]string{
		"X-Forgejo-Signature": sig,
		"X-Gitea-Signature":   sig,
		"X-Hub-Signature-256": "sha256=" + sig,
	})

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}
	agent.waitForCall(t)
}

func TestWebhookOneBadSignatureAmongSeveralReturns403(t *testing.T) {
	// Every signature header present must verify, not just the first.
	agent := newFakeAgent()
	handler := New(context.Background(), agent, "s3cret")
	body := []byte(pushBody)

	rec := doBodyReq(handler, http.MethodPost, "/webhook", body, map[string]string{
		"X-Hub-Signature-256": "sha256=" + sign("s3cret", body),
		"X-Gitea-Signature":   sign("other-secret", body),
	})

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
	time.Sleep(20 * time.Millisecond)
	if agent.callCount() != 0 {
		t.Errorf("reconcile calls = %d, want 0", agent.callCount())
	}
}

func TestWebhookSignatureOverAModifiedBodyReturns403AndCountsTowardLockout(t *testing.T) {
	agent := newFakeAgent()
	handler := New(context.Background(), agent, "s3cret")
	body := []byte(pushBody)
	sig := sign("s3cret", body)
	tampered := bytes.Clone(body)
	tampered[len(tampered)-3] ^= 0x01

	for i := range maxFailures {
		rec := doBodyReq(handler, http.MethodPost, "/webhook", tampered, map[string]string{"X-Forgejo-Signature": sig})
		if rec.Code != http.StatusForbidden {
			t.Fatalf("attempt %d: status = %d, want 403", i+1, rec.Code)
		}
	}
	// Those failures spent the window's budget, so even the untouched
	// body is refused now.
	rec := doBodyReq(handler, http.MethodPost, "/webhook", body, map[string]string{"X-Forgejo-Signature": sig})
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429 once the failures reach the lockout", rec.Code)
	}
	time.Sleep(20 * time.Millisecond)
	if agent.callCount() != 0 {
		t.Errorf("reconcile calls = %d, want 0", agent.callCount())
	}
}

func TestWebhookMalformedSignatureReturns403(t *testing.T) {
	body := []byte(pushBody)
	sig := sign("s3cret", body)
	for name, headers := range map[string]map[string]string{
		"github bad hex":   {"X-Hub-Signature-256": "sha256=zz" + sig[2:]},
		"forgejo bad hex":  {"X-Forgejo-Signature": "not-hex"},
		"gitea odd length": {"X-Gitea-Signature": sig[1:]},
		// The right digest, so only the prefix check can refuse these.
		"github without prefix": {"X-Hub-Signature-256": sig},
		"github sha1 prefix":    {"X-Hub-Signature-256": "sha1=" + sig},
	} {
		t.Run(name, func(t *testing.T) {
			agent := newFakeAgent()
			handler := New(context.Background(), agent, "s3cret")

			rec := doBodyReq(handler, http.MethodPost, "/webhook", body, headers)

			if rec.Code != http.StatusForbidden {
				t.Errorf("status = %d, want 403", rec.Code)
			}
			time.Sleep(20 * time.Millisecond)
			if agent.callCount() != 0 {
				t.Errorf("reconcile calls = %d, want 0", agent.callCount())
			}
		})
	}
}

func TestWebhookSignedBodyOverTheLimitReturns413AndDoesNotTrigger(t *testing.T) {
	agent := newFakeAgent()
	handler := New(context.Background(), agent, "s3cret")
	body := bytes.Repeat([]byte("a"), maxBodyBytes+1)

	rec := doBodyReq(handler, http.MethodPost, "/webhook", body, map[string]string{"X-Hub-Signature-256": "sha256=" + sign("s3cret", body)})

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", rec.Code)
	}
	time.Sleep(20 * time.Millisecond)
	if agent.callCount() != 0 {
		t.Errorf("reconcile calls = %d, want 0", agent.callCount())
	}
}

func TestWebhookSignedBodyAtTheLimitReturns202(t *testing.T) {
	agent := newFakeAgent()
	handler := New(context.Background(), agent, "s3cret")
	body := bytes.Repeat([]byte("a"), maxBodyBytes)

	rec := doBodyReq(handler, http.MethodPost, "/webhook", body, map[string]string{"X-Hub-Signature-256": "sha256=" + sign("s3cret", body)})

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}
	agent.waitForCall(t)
}

func TestWebhookValidTokenIsAcceptedWhateverTheSignature(t *testing.T) {
	// A forge can sign with a secret of its own while the URL carries
	// ?token=; that worked before signatures were checked and still must.
	agent := newFakeAgent()
	handler := New(context.Background(), agent, "s3cret")
	body := []byte(pushBody)

	rec := doBodyReq(handler, http.MethodPost, "/webhook?token=s3cret", body, map[string]string{"X-Gitea-Signature": sign("forge-side-secret", body)})

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}
	agent.waitForCall(t)
}

func TestWebhookWrongTokenIsNotRescuedByAValidSignature(t *testing.T) {
	agent := newFakeAgent()
	handler := New(context.Background(), agent, "s3cret")
	body := []byte(pushBody)

	rec := doBodyReq(handler, http.MethodPost, "/webhook", body, map[string]string{
		"X-Gitops-Token":      "wrong",
		"X-Hub-Signature-256": "sha256=" + sign("s3cret", body),
	})

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 - a presented token decides alone", rec.Code)
	}
	time.Sleep(20 * time.Millisecond)
	if agent.callCount() != 0 {
		t.Errorf("reconcile calls = %d, want 0", agent.callCount())
	}
}

func TestWebhookEmptySecretRejectsASignatureMadeWithAnEmptyKey(t *testing.T) {
	agent := newFakeAgent()
	handler := New(context.Background(), agent, "")
	body := []byte(pushBody)

	rec := doBodyReq(handler, http.MethodPost, "/webhook", body, map[string]string{"X-Hub-Signature-256": "sha256=" + sign("", body)})

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 (a webhook with no configured secret must never accept)", rec.Code)
	}
}

func TestWebhookBodyWithoutTokenOrSignatureReturns403(t *testing.T) {
	agent := newFakeAgent()
	handler := New(context.Background(), agent, "s3cret")

	rec := doBodyReq(handler, http.MethodPost, "/webhook", []byte(pushBody), map[string]string{"Content-Type": "application/json"})

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
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
	// The handler never inspects what SyncNow decides to do, so a
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

func (p *panickingAgent) SyncNow(ctx context.Context) {
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

// The cycle now applies, and SyncNow starts no apply once its context is
// cancelled - which it never was while the hook detached it from the app's
// lifetime, so a shutdown mid-reconcile still began an apply.
func TestWebhookCycleStopsWithTheApp(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	agent := newFakeAgent()
	handler := New(ctx, agent, "s3cret-s3cret-s3cret")

	doReq(handler, http.MethodPost, "/webhook", map[string]string{"X-Gitops-Token": "s3cret-s3cret-s3cret"})
	agent.waitForCall(t)
	cancel()

	agent.mu.Lock()
	defer agent.mu.Unlock()
	if len(agent.ctxs) != 1 || agent.ctxs[0].Err() == nil {
		t.Error("the cycle's context outlives the app; a shutdown cannot stop it before an apply")
	}
}
