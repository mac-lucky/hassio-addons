package hook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
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

func (f *fakeAgent) SyncNow(ctx context.Context, acceptedAt time.Time) {
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
	handler := New(t.Context(), agent, "s3cret", "main")

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
	handler := New(t.Context(), agent, "s3cret", "main")

	rec := doReq(handler, http.MethodPost, "/webhook?token=s3cret", nil)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}
	agent.waitForCall(t)
}

func TestWebhookHeaderTokenTakesPrecedenceOverQuery(t *testing.T) {
	// Documents which one wins: validToken checks the header first.
	agent := newFakeAgent()
	handler := New(t.Context(), agent, "s3cret", "main")

	rec := doReq(handler, http.MethodPost, "/webhook?token=wrong", map[string]string{"X-Gitops-Token": "s3cret"})

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}
}

func TestWebhookMismatchedTokenReturns403AndDoesNotTrigger(t *testing.T) {
	agent := newFakeAgent()
	handler := New(t.Context(), agent, "s3cret", "main")

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
	handler := New(t.Context(), agent, "s3cret", "main")

	rec := doReq(handler, http.MethodPost, "/webhook", nil)

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
}

func TestWebhookEmptySecretAlwaysRejects(t *testing.T) {
	agent := newFakeAgent()
	handler := New(t.Context(), agent, "", "main")

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
			handler := New(t.Context(), agent, "s3cret", "main")

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
	handler := New(t.Context(), agent, "s3cret", "main")
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
	handler := New(t.Context(), agent, "s3cret", "main")
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
	handler := New(t.Context(), agent, "s3cret", "main")
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
			handler := New(t.Context(), agent, "s3cret", "main")

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
	handler := New(t.Context(), agent, "s3cret", "main")
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
	handler := New(t.Context(), agent, "s3cret", "main")
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
	handler := New(t.Context(), agent, "s3cret", "main")
	body := []byte(pushBody)

	rec := doBodyReq(handler, http.MethodPost, "/webhook?token=s3cret", body, map[string]string{"X-Gitea-Signature": sign("forge-side-secret", body)})

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}
	agent.waitForCall(t)
}

func TestWebhookWrongTokenIsNotRescuedByAValidSignature(t *testing.T) {
	agent := newFakeAgent()
	handler := New(t.Context(), agent, "s3cret", "main")
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
	handler := New(t.Context(), agent, "", "main")
	body := []byte(pushBody)

	rec := doBodyReq(handler, http.MethodPost, "/webhook", body, map[string]string{"X-Hub-Signature-256": "sha256=" + sign("", body)})

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 (a webhook with no configured secret must never accept)", rec.Code)
	}
}

func TestWebhookBodyWithoutTokenOrSignatureReturns403(t *testing.T) {
	agent := newFakeAgent()
	handler := New(t.Context(), agent, "s3cret", "main")

	rec := doBodyReq(handler, http.MethodPost, "/webhook", []byte(pushBody), map[string]string{"Content-Type": "application/json"})

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
}

func TestWebhookWrongMethodNotFound(t *testing.T) {
	agent := newFakeAgent()
	handler := New(t.Context(), agent, "s3cret", "main")

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
	handler := New(t.Context(), agent, "s3cret", "main")

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

func (p *panickingAgent) SyncNow(ctx context.Context, acceptedAt time.Time) {
	defer close(p.entered)
	panic("gitsync exploded")
}

// A panic in the detached reconcile must not take the process down:
// net/http recovers a panicking handler, and this goroutine is not one.
// A regression crashes the test binary rather than failing.
func TestPanicInTheTriggeredReconcileDoesNotKillTheProcess(t *testing.T) {
	agent := &panickingAgent{entered: make(chan struct{})}
	handler := New(t.Context(), agent, "s3cret", "main")

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
	handler := New(ctx, agent, "s3cret-s3cret-s3cret", "main")

	doReq(handler, http.MethodPost, "/webhook", map[string]string{"X-Gitops-Token": "s3cret-s3cret-s3cret"})
	agent.waitForCall(t)
	cancel()

	agent.mu.Lock()
	defer agent.mu.Unlock()
	if len(agent.ctxs) != 1 || agent.ctxs[0].Err() == nil {
		t.Error("the cycle's context outlives the app; a shutdown cannot stop it before an apply")
	}
}

func withCycleSpacing(t *testing.T, d time.Duration) {
	t.Helper()
	old := minCycleSpacing
	minCycleSpacing = d
	t.Cleanup(func() { minCycleSpacing = old })
}

func signedHeaders(secret string, body []byte, event string) map[string]string {
	h := map[string]string{"X-Forgejo-Signature": sign(secret, body)}
	if event != "" {
		h["X-Forgejo-Event"] = event
	}
	return h
}

func expectNoCall(t *testing.T, agent *fakeAgent) {
	t.Helper()
	time.Sleep(50 * time.Millisecond)
	if n := agent.callCount(); n != 0 {
		t.Errorf("reconcile calls = %d, want 0", n)
	}
}

// A forge pings when the webhook is created or tested; answering it with a
// cycle would be harmless but misleading in the forge's delivery log.
func TestWebhookPingAnswersWithoutACycle(t *testing.T) {
	agent := newFakeAgent()
	handler := New(t.Context(), agent, "s3cret", "main")
	body := []byte(`{"zen":"hello"}`)

	rec := doBodyReq(handler, http.MethodPost, "/webhook", body, signedHeaders("s3cret", body, "ping"))

	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "pong") {
		t.Errorf("status = %d body = %q, want 200 pong", rec.Code, rec.Body.String())
	}
	expectNoCall(t, agent)
}

func TestWebhookOtherEventsAreIgnored(t *testing.T) {
	agent := newFakeAgent()
	handler := New(t.Context(), agent, "s3cret", "main")
	body := []byte(`{"action":"opened"}`)

	rec := doBodyReq(handler, http.MethodPost, "/webhook", body, signedHeaders("s3cret", body, "issues"))

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	expectNoCall(t, agent)
}

// A push to a feature branch says nothing about the tracked one.
func TestWebhookPushToAnotherBranchIsIgnored(t *testing.T) {
	agent := newFakeAgent()
	handler := New(t.Context(), agent, "s3cret", "main")
	body := []byte(`{"ref":"refs/heads/feature/x","after":"abc"}`)

	rec := doBodyReq(handler, http.MethodPost, "/webhook", body, signedHeaders("s3cret", body, "push"))

	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "another branch") {
		t.Errorf("status = %d body = %q, want 200 ignoring the push", rec.Code, rec.Body.String())
	}
	expectNoCall(t, agent)
}

// The same filter for a forge still configured with ?token=: its body is
// read after the token check, bounded the same way.
func TestWebhookTokenPushToAnotherBranchIsIgnored(t *testing.T) {
	agent := newFakeAgent()
	handler := New(t.Context(), agent, "s3cret", "main")
	body := []byte(`{"ref":"refs/tags/v1.0.0"}`)

	rec := doBodyReq(handler, http.MethodPost, "/webhook?token=s3cret", body, map[string]string{"X-GitHub-Event": "push"})

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	expectNoCall(t, agent)
}

// A body the agent cannot read is not a reason to miss a push.
func TestWebhookPushWithUnreadableBodyStillTriggers(t *testing.T) {
	agent := newFakeAgent()
	handler := New(t.Context(), agent, "s3cret", "main")
	body := []byte("payload=%7B%22ref%22%3A%22refs%2Fheads%2Fmain%22%7D")

	rec := doBodyReq(handler, http.MethodPost, "/webhook", body, signedHeaders("s3cret", body, "push"))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}
	agent.waitForCall(t)
}

// A signature proves the sender, not the moment: a captured delivery
// replays verbatim, and used to run a cycle each time.
func TestWebhookReplayedSignedDeliveryIsDropped(t *testing.T) {
	withCycleSpacing(t, time.Millisecond)
	agent := newFakeAgent()
	handler := New(t.Context(), agent, "s3cret", "main")
	body := []byte(pushBody)

	first := doBodyReq(handler, http.MethodPost, "/webhook", body, signedHeaders("s3cret", body, "push"))
	agent.waitForCall(t)
	time.Sleep(20 * time.Millisecond)
	replay := doBodyReq(handler, http.MethodPost, "/webhook", body, signedHeaders("s3cret", body, "push"))

	if first.Code != http.StatusAccepted {
		t.Fatalf("first status = %d, want 202", first.Code)
	}
	if replay.Code != http.StatusOK || !strings.Contains(replay.Body.String(), "duplicate") {
		t.Errorf("replay status = %d body = %q, want 200 duplicate", replay.Code, replay.Body.String())
	}
	time.Sleep(50 * time.Millisecond)
	if n := agent.callCount(); n != 1 {
		t.Errorf("reconcile calls = %d, want 1", n)
	}

	// A different push is a different body.
	next := []byte(`{"ref":"refs/heads/main","after":"fedcba9876543210"}`)
	if rec := doBodyReq(handler, http.MethodPost, "/webhook", next, signedHeaders("s3cret", next, "push")); rec.Code != http.StatusAccepted {
		t.Errorf("new push status = %d, want 202", rec.Code)
	}
	agent.waitForCall(t)
}

// A script posting the same (empty) body twice means it twice, and it
// knows the secret anyway, so token requests are never deduplicated.
func TestWebhookTokenRequestsAreNotDeduplicated(t *testing.T) {
	withCycleSpacing(t, time.Millisecond)
	agent := newFakeAgent()
	handler := New(t.Context(), agent, "s3cret", "main")

	for range 2 {
		if rec := doReq(handler, http.MethodPost, "/webhook", map[string]string{"X-Gitops-Token": "s3cret"}); rec.Code != http.StatusAccepted {
			t.Fatalf("status = %d, want 202", rec.Code)
		}
		agent.waitForCall(t)
	}
}

// blockingAgent holds its first cycle until released, standing in for a
// cycle that is past its fetch when the next push is announced.
type blockingAgent struct {
	release chan struct{}
	started chan struct{}
	mu      sync.Mutex
	calls   int
}

func (b *blockingAgent) SyncNow(ctx context.Context, acceptedAt time.Time) {
	b.mu.Lock()
	b.calls++
	first := b.calls == 1
	b.mu.Unlock()
	b.started <- struct{}{}
	if first {
		<-b.release
	}
}

func (b *blockingAgent) callCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls
}

// A push announced during a cycle used to be dropped: the cycle had
// already fetched, and the trigger found the agent busy. Now it queues one
// more cycle - one, however many deliveries arrive meanwhile.
func TestWebhookDeliveriesDuringACycleQueueExactlyOneMore(t *testing.T) {
	withCycleSpacing(t, time.Millisecond)
	agent := &blockingAgent{release: make(chan struct{}), started: make(chan struct{}, 8)}
	handler := New(t.Context(), agent, "s3cret", "main")
	token := map[string]string{"X-Gitops-Token": "s3cret"}

	doReq(handler, http.MethodPost, "/webhook", token)
	select {
	case <-agent.started:
	case <-time.After(2 * time.Second):
		t.Fatal("first cycle never started")
	}
	for range 3 {
		if rec := doReq(handler, http.MethodPost, "/webhook", token); rec.Code != http.StatusAccepted {
			t.Fatalf("status = %d, want 202 while busy", rec.Code)
		}
	}
	close(agent.release)

	select {
	case <-agent.started:
	case <-time.After(2 * time.Second):
		t.Fatal("the queued cycle never ran")
	}
	time.Sleep(50 * time.Millisecond)
	if n := agent.callCount(); n != 2 {
		t.Errorf("cycles = %d, want 2 - the running one and exactly one queued", n)
	}
}

func TestSeenBodiesForgetsAfterTheWindowAndPastTheCap(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	s := &seenBodies{window: time.Minute, max: 2}

	a, b, c := sha256.Sum256([]byte("a")), sha256.Sum256([]byte("b")), sha256.Sum256([]byte("c"))
	if !s.add(a, now) {
		t.Fatal("first add of a reported a duplicate")
	}
	if s.add(a, now.Add(30*time.Second)) {
		t.Error("a within the window was not reported as a duplicate")
	}
	if !s.add(a, now.Add(2*time.Minute)) {
		t.Error("a after the window was still a duplicate")
	}

	s = &seenBodies{window: time.Hour, max: 2}
	s.add(a, now)
	s.add(b, now)
	s.add(c, now)
	if !s.add(a, now) {
		t.Error("a was not evicted by the cap")
	}
}

// The ref sits at the top of a forge's push payload; the prefix the agent
// keeps is usually a cut-off document, and must still yield it.
func TestPushRefReadsTheRefFromATruncatedPayload(t *testing.T) {
	cases := []struct{ name, prefix, want string }{
		{"whole", `{"ref":"refs/heads/main","after":"abc"}`, "refs/heads/main"},
		{"cut after ref", `{"ref":"refs/heads/dev","commits":[{"id":"ab`, "refs/heads/dev"},
		{"ref after other keys", `{"secret":"","ref":"refs/tags/v1","before":"0"}`, "refs/tags/v1"},
		{"cut before ref", `{"before":"0000","commits":[{"id":"ab`, ""},
		{"not json", "payload=%7B%22ref", ""},
		{"not an object", `["refs/heads/main"]`, ""},
		{"empty", "", ""},
	}
	for _, c := range cases {
		if got := pushRef([]byte(c.prefix)); got != c.want {
			t.Errorf("%s: pushRef = %q, want %q", c.name, got, c.want)
		}
	}
}

// An event header is not signed: garbage in it is neither logged nor
// honoured, and a replay cannot get past the duplicate check by changing
// it.
func TestWebhookUnsignedEventHeaderCannotDodgeTheReplayCheck(t *testing.T) {
	withCycleSpacing(t, time.Millisecond)
	agent := newFakeAgent()
	handler := New(t.Context(), agent, "s3cret", "main")
	body := []byte(pushBody)

	if rec := doBodyReq(handler, http.MethodPost, "/webhook", body, signedHeaders("s3cret", body, "push")); rec.Code != http.StatusAccepted {
		t.Fatalf("first status = %d, want 202", rec.Code)
	}
	agent.waitForCall(t)

	replay := doBodyReq(handler, http.MethodPost, "/webhook", body, signedHeaders("s3cret", body, strings.Repeat("x", 4096)))
	if replay.Code != http.StatusOK || !strings.Contains(replay.Body.String(), "duplicate") {
		t.Errorf("replay status = %d body = %q, want 200 duplicate", replay.Code, replay.Body.String())
	}
}

func TestDeliveryEventRejectsWhatNoForgeSends(t *testing.T) {
	for header, want := range map[string]string{
		"Push":                  "push",
		" ping ":                "ping",
		"pull_request":          "pull_request",
		"push\nforged log line": "unrecognized",
		strings.Repeat("a", 65): "unrecognized",
	} {
		req := httptest.NewRequest(http.MethodPost, "/webhook", nil)
		req.Header.Set("X-Gitea-Event", header)
		if got := deliveryEvent(req); got != want {
			t.Errorf("deliveryEvent(%q) = %q, want %q", header, got, want)
		}
	}
}

// The queued cycle carries when its newest delivery arrived, which is what
// lets SyncNow drop a cycle a Roll Back overtook while it was queued.
func TestPendingTriggerKeepsTheNewestAcceptanceTime(t *testing.T) {
	p := newPendingTrigger()
	early := time.Unix(1_000, 0)
	late := early.Add(time.Minute)

	p.set(late)
	p.set(early)
	if got := p.take(); !got.Equal(late) {
		t.Errorf("take = %v, want the newest %v", got, late)
	}
	if got := p.take(); !got.IsZero() {
		t.Errorf("second take = %v, want zero once taken", got)
	}
	select {
	case <-p.ready:
	default:
		t.Error("set did not wake the worker")
	}
}
