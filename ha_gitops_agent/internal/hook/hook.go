// Package hook is the add-on's optional webhook trigger: a separate HTTP
// server, never the ingress dashboard, that lets a caller ask for an
// immediate reconcile. It starts only when webhook_secret is configured,
// though config.yaml declares its port either way.
package hook

import (
	"context"
	"crypto/subtle"
	"log/slog"
	"net/http"
	"runtime/debug"
	"sync"
	"time"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/differ"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/httpx"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/recon"
)

// Agent is what the webhook handler needs from the reconciler: only
// "reconcile now", deliberately narrower than web.Agent.
type Agent interface {
	ReconcileNow(ctx context.Context) []differ.Change
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
)

// New builds the webhook trigger's http.Handler: one POST /webhook
// route, gated by a constant-time comparison of secret against the
// X-Gitops-Token header or a ?token= query parameter. Bind it to its own
// *http.Server, never the ingress dashboard's.
//
// ctx is the app's lifetime context; the reconciles it triggers are
// detached from it (see below).
func New(ctx context.Context, agent Agent, secret string) http.Handler {
	mux := http.NewServeMux()
	limiter := &rateLimiter{interval: mismatchLogInterval}
	attempts := &attemptLimiter{window: failureWindow, maxFailures: maxFailures}

	// Computed once from the app's lifetime, not per-request, to make the
	// detachment explicit rather than incidental.
	detachedCtx := context.WithoutCancel(ctx)

	mux.HandleFunc("POST /webhook", func(w http.ResponseWriter, r *http.Request) {
		if attempts.blocked() {
			http.Error(w, http.StatusText(http.StatusTooManyRequests), http.StatusTooManyRequests)
			return
		}
		if !validToken(r, secret) {
			attempts.recordFailure()
			limiter.warn("hook: rejected webhook request: invalid or missing token", "remote", httpx.RemoteHost(r))
			http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
			return
		}

		// Fire-and-forget: detached from the request so a caller that
		// gives up waiting cannot abort a running reconcile, and from
		// the app's lifetime too. main's WaitIdle covers this goroutine
		// once ReconcileNow takes the op-lock, not before it.
		go func() {
			defer recoverReconcile()
			agent.ReconcileNow(detachedCtx)
		}()

		// 202, not 200: the reconcile is asynchronous and a busy agent
		// absorbs the trigger entirely (ReconcileNow uses TryLock).
		w.WriteHeader(http.StatusAccepted)
	})

	return mux
}

// recoverReconcile turns a panic inside the detached reconcile into a
// logged error instead of a dead add-on: net/http's own recover does not
// reach a goroutine, and nothing under ReconcileNow recovers either.
// internal/web guards its action routes the same way, on purpose - hook
// must not import web.
func recoverReconcile() {
	if v := recover(); v != nil {
		slog.Error("hook: webhook-triggered reconcile panicked", "panic", v, "stack", string(debug.Stack()))
	}
}

// validToken reports whether r carries secret via the X-Gitops-Token
// header or a ?token= query parameter, compared in constant time. An
// empty secret or an empty candidate never matches.
func validToken(r *http.Request, secret string) bool {
	if secret == "" {
		return false
	}
	candidate := r.Header.Get(tokenHeader)
	if candidate == "" {
		candidate = r.URL.Query().Get(tokenParam)
	}
	if candidate == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(candidate), []byte(secret)) == 1
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

// attemptLimiter refuses every request once maxFailures bad tokens have
// arrived inside the current window. Global rather than per-remote: the
// endpoint has exactly one legitimate caller shape (a forge webhook with
// the right token), and a locked-out minute costs it nothing - the next
// interval reconciles anyway.
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
