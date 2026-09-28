// Package hook is the add-on's optional webhook trigger: a separate HTTP
// server, never the ingress dashboard, that lets a caller ask for an
// immediate sync cycle. It starts only when webhook_secret is configured,
// though config.yaml declares its port either way.
package hook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/httpx"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/recon"
)

// Agent is what the webhook handler needs from the reconciler: only "run a
// cycle now", deliberately narrower than web.Agent. A cycle is what the
// timer runs - reconcile, then apply when dry_run is off and nothing holds
// it - so a push lands when it is announced rather than an interval later.
type Agent interface {
	SyncNow(ctx context.Context)
}

var _ Agent = (*recon.Reconciler)(nil)

const (
	tokenHeader = "X-Gitops-Token" // #nosec G101 -- a header NAME, not a credential value
	tokenParam  = "token"

	// mismatchLogInterval bounds how often a rejected request gets a log
	// line, so a flood of guesses cannot spam the process log.
	mismatchLogInterval = time.Minute

	// MinSecretLen is the shortest webhook_secret main will serve. The
	// comparison is constant-time, but nothing else slows a guesser down,
	// so the secret itself has to carry the entropy.
	MinSecretLen = 16

	// maxFailures failed attempts within failureWindow lock the endpoint
	// (HTTP 429) until the window rolls over - guessing gets a real
	// budget, not just a quieter log.
	maxFailures   = 30
	failureWindow = time.Minute

	// maxBodyBytes bounds how much of a signed request's body the agent
	// hashes before it knows whether the signature is good. The body is
	// streamed into the HMAC, never held; forge push payloads are far
	// smaller.
	maxBodyBytes = 5 << 20
)

// signatureHeaders are the forge headers that carry a hex HMAC-SHA256 of
// the raw request body keyed by the shared secret. Only GitHub's has an
// algorithm prefix. Forgejo sends its own, Gitea's and GitHub's on the
// same delivery, so one request can carry several.
var signatureHeaders = []struct{ name, prefix string }{
	{"X-Hub-Signature-256", "sha256="},
	{"X-Forgejo-Signature", ""},
	{"X-Gitea-Signature", ""},
}

// New builds the webhook trigger's http.Handler: one POST /webhook
// route, gated by secret - either a constant-time comparison against the
// X-Gitops-Token header or a ?token= query parameter, or a forge's
// HMAC-SHA256 signature of the body (see authenticate). Bind it to its
// own *http.Server, never the ingress dashboard's.
//
// ctx is the app's lifetime context, which the cycles it triggers run on
// (see below).
func New(ctx context.Context, agent Agent, secret string) http.Handler {
	mux := http.NewServeMux()
	limiter := &rateLimiter{interval: mismatchLogInterval}
	attempts := &attemptLimiter{window: failureWindow, maxFailures: maxFailures}

	mux.HandleFunc("POST /webhook", func(w http.ResponseWriter, r *http.Request) {
		if attempts.blocked() {
			http.Error(w, http.StatusText(http.StatusTooManyRequests), http.StatusTooManyRequests)
			return
		}
		// Every refusal counts toward the lockout, an oversized or
		// unreadable body included: none of them proved the secret.
		if status, reason := authenticate(w, r, secret); status != http.StatusAccepted {
			attempts.recordFailure()
			limiter.warn("hook: rejected webhook request", "reason", reason, "remote", httpx.RemoteHost(r))
			http.Error(w, http.StatusText(status), status)
			return
		}

		// Fire-and-forget on the app's lifetime rather than the request's,
		// so a caller that gives up waiting cannot abort a running cycle.
		// NOT detached from the lifetime as well: a cycle now applies, and
		// SyncNow starts no apply once that context is cancelled - a
		// shutdown landing mid-reconcile must stop there, not begin a
		// backup and a config write it will be killed halfway through. The
		// apply itself detaches once started. main's WaitIdle covers this
		// goroutine once SyncNow takes the op-lock, not before it.
		go func() {
			defer recoverReconcile()
			agent.SyncNow(ctx)
		}()

		// 202, not 200: the cycle is asynchronous and a busy agent
		// absorbs the trigger entirely (SyncNow uses TryLock).
		w.WriteHeader(http.StatusAccepted)
	})

	return mux
}

// recoverReconcile turns a panic inside the detached cycle into a
// logged error instead of a dead add-on: net/http's own recover does not
// reach a goroutine, and nothing under SyncNow recovers either.
// internal/web guards its action routes the same way, on purpose - hook
// must not import web.
func recoverReconcile() {
	if v := recover(); v != nil {
		slog.Error("hook: webhook-triggered cycle panicked", "panic", v, "stack", string(debug.Stack()))
	}
}

// authenticate decides whether r may trigger a cycle, returning
// http.StatusAccepted if so, or the status to answer with and a reason
// for the log. An empty secret never matches anything.
//
// A presented token decides alone, right or wrong. A valid one is
// accepted without looking at any signature, as before signatures
// existed (a forge may sign with some other secret of its own and still
// carry ?token=). A wrong one is a failed attempt even when the body is
// validly signed: a stale or guessed token is not rescued by a second
// credential. Only a request with no token at all is judged by its
// signature.
func authenticate(w http.ResponseWriter, r *http.Request, secret string) (int, string) {
	if secret == "" {
		return http.StatusForbidden, "no secret configured"
	}
	// The header is checked first, as it always was.
	candidate := r.Header.Get(tokenHeader)
	if candidate == "" {
		candidate = r.URL.Query().Get(tokenParam)
	}
	if candidate != "" {
		if subtle.ConstantTimeCompare([]byte(candidate), []byte(secret)) != 1 {
			return http.StatusForbidden, "invalid token"
		}
		return http.StatusAccepted, ""
	}
	return verifySignature(w, r, secret)
}

// verifySignature checks r's forge signature headers against an
// HMAC-SHA256 of its raw body. Every signature header present must
// verify, not just one: a real delivery signs them all with the same
// secret, so a mismatch among them is never a forge's doing. Malformed
// headers are refused before the body is read.
func verifySignature(w http.ResponseWriter, r *http.Request, secret string) (int, string) {
	var sigs [][]byte
	for _, h := range signatureHeaders {
		v := r.Header.Get(h.name)
		if v == "" {
			continue
		}
		digest, ok := strings.CutPrefix(v, h.prefix)
		if !ok {
			return http.StatusForbidden, "malformed signature"
		}
		sig, err := hex.DecodeString(digest)
		if err != nil {
			return http.StatusForbidden, "malformed signature"
		}
		sigs = append(sigs, sig)
	}
	if len(sigs) == 0 {
		return http.StatusForbidden, "missing token or signature"
	}

	mac := hmac.New(sha256.New, []byte(secret))
	if _, err := io.Copy(mac, http.MaxBytesReader(w, r.Body, maxBodyBytes)); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return http.StatusRequestEntityTooLarge, "body too large"
		}
		return http.StatusBadRequest, "unreadable body"
	}
	want := mac.Sum(nil)
	for _, sig := range sigs {
		if !hmac.Equal(sig, want) {
			return http.StatusForbidden, "invalid signature"
		}
	}
	return http.StatusAccepted, ""
}

// rateLimiter caps warn to one log line per interval. Safe for
// concurrent use: net/http dispatches requests to one handler.
type rateLimiter struct {
	interval time.Duration

	mu   sync.Mutex
	last time.Time
}

func (l *rateLimiter) warn(msg string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if !l.last.IsZero() && now.Sub(l.last) < l.interval {
		return
	}
	l.last = now
	slog.Warn(msg, args...)
}

// attemptLimiter refuses every request once maxFailures failed attempts
// (bad tokens or bad signatures) have arrived inside the current window.
// Global rather than per-remote: the endpoint has exactly one legitimate
// caller shape (a forge webhook with the right token or signature), and a
// locked-out minute costs it nothing - the next interval reconciles
// anyway.
type attemptLimiter struct {
	window      time.Duration
	maxFailures int

	mu       sync.Mutex
	start    time.Time
	failures int
}

func (l *attemptLimiter) roll(now time.Time) {
	if l.start.IsZero() || now.Sub(l.start) > l.window {
		l.start = now
		l.failures = 0
	}
}

func (l *attemptLimiter) blocked() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.roll(time.Now())
	return l.failures >= l.maxFailures
}

func (l *attemptLimiter) recordFailure() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.roll(time.Now())
	l.failures++
}
