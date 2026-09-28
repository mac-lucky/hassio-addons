// Package hook is the add-on's optional webhook trigger: a separate HTTP
// server, never the ingress dashboard, that lets a caller ask for an
// immediate sync cycle. It starts only when webhook_secret is configured,
// though config.yaml declares its port either way.
package hook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"regexp"
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
	// SyncNow runs the cycle a delivery accepted at acceptedAt asked for
	// (see recon.Reconciler.SyncNow for why the time matters).
	SyncNow(ctx context.Context, acceptedAt time.Time)
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
	// streamed through the hashes, never held (see verifySignature);
	// forge push payloads are far smaller.
	maxBodyBytes = 5 << 20

	// refPrefixBytes is how much of a push body is kept to read its "ref"
	// from. Forges put "ref" first in a push payload, so this is ample; a
	// ref past it reads as none, which triggers a cycle (see pushRef).
	refPrefixBytes = 64 << 10

	// seenWindow and seenMax bound the memory of signed bodies already
	// accepted, which is what turns a replayed delivery into a no-op. 4096
	// hashes is 128 KiB - far more distinct deliveries than a repository
	// sends in an hour, so cycling captured ones through to push each out
	// of the memory again is not practical.
	seenWindow = time.Hour
	seenMax    = 4096
)

// minCycleSpacing is the least time between two webhook-triggered cycles.
// Deliveries inside it are not lost: they fold into the one cycle that
// runs when it is up (see worker). A var so tests can shorten it.
var minCycleSpacing = 10 * time.Second

// eventHeaders name the forge event of a delivery; the first present wins.
// A request with none of them - a script, an automation - is treated as a
// plain "sync now".
var eventHeaders = []string{"X-GitHub-Event", "X-Forgejo-Event", "X-Gitea-Event"}

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
// branch is the tracked branch: a forge's push to any other one triggers
// nothing. ctx is the app's lifetime context: the worker that runs the
// cycles lives as long as it does, and the cycles run on it (see worker).
func New(ctx context.Context, agent Agent, secret, branch string) http.Handler {
	mux := http.NewServeMux()
	limiter := &rateLimiter{interval: mismatchLogInterval}
	replayLog := &rateLimiter{interval: mismatchLogInterval}
	ignoreLog := &rateLimiter{interval: mismatchLogInterval}
	attempts := &attemptLimiter{window: failureWindow, maxFailures: maxFailures}
	seen := &seenBodies{window: seenWindow, max: seenMax}
	trigger := newPendingTrigger()
	go worker(ctx, agent, trigger, minCycleSpacing)

	mux.HandleFunc("POST /webhook", func(w http.ResponseWriter, r *http.Request) {
		if attempts.blocked() {
			http.Error(w, http.StatusText(http.StatusTooManyRequests), http.StatusTooManyRequests)
			return
		}
		// Every refusal counts toward the lockout, an oversized or
		// unreadable body included: none of them proved the secret.
		auth := authenticate(w, r, secret)
		if auth.status != http.StatusAccepted {
			attempts.recordFailure()
			limiter.warn("hook: rejected webhook request", "reason", auth.reason, "remote", httpx.RemoteHost(r))
			http.Error(w, http.StatusText(auth.status), auth.status)
			return
		}

		// A signature proves who sent a body, not when: a captured signed
		// delivery replays verbatim. The identical body again within the
		// window is answered and dropped - before anything else, since the
		// headers beside it are not signed and a replay may change them.
		// Token requests are not deduped: their sender knows the secret,
		// and a script posting an empty body twice means it twice.
		if auth.signed && !seen.add(auth.digest, time.Now()) {
			replayLog.warn("hook: dropped a signed delivery already accepted once (a redelivery or a replay)",
				"remote", httpx.RemoteHost(r))
			writeText(w, http.StatusOK, "duplicate delivery ignored")
			return
		}

		event := deliveryEvent(r)
		switch event {
		case "", "push":
		case "ping":
			// A forge sends one when the webhook is created or tested.
			writeText(w, http.StatusOK, "pong")
			return
		default:
			ignoreLog.info("hook: ignoring a delivery that is not a push", "event", event)
			writeText(w, http.StatusOK, "ignored: not a push event")
			return
		}

		if event == "push" {
			prefix := auth.prefix
			if !auth.signed {
				// A token-authenticated forge delivery: read the start of
				// it now, for the ref.
				prefix, _ = io.ReadAll(io.LimitReader(http.MaxBytesReader(w, r.Body, maxBodyBytes), refPrefixBytes))
			}
			if ref := pushRef(prefix); ref != "" && ref != "refs/heads/"+branch {
				ignoreLog.info("hook: ignoring a push to another branch", "ref", ref)
				writeText(w, http.StatusOK, "ignored: push to another branch than "+branch)
				return
			}
		}

		// Never blocks: a trigger already waiting covers this delivery
		// too, since the cycle it starts fetches whatever is newest.
		trigger.set(time.Now())
		slog.Info("hook: delivery accepted, cycle queued", "event", event)

		// 202, not 200: the cycle runs after the response, possibly after
		// the one in progress.
		w.WriteHeader(http.StatusAccepted)
	})

	return mux
}

// worker runs the cycles deliveries ask for, one at a time, until ctx is
// done. trigger holds at most one request, so a delivery that arrives
// during a cycle - or during the spacing after one - queues exactly one
// more, and any number arriving together still cost a single cycle: the
// one queued fetches whatever is newest when it starts.
//
// The cycles run on the app's lifetime ctx rather than a request's, so a
// caller that gives up waiting cannot abort a running cycle. NOT detached
// from the lifetime as well: a cycle applies, and SyncNow starts no apply
// once that context is cancelled - a shutdown landing mid-reconcile must
// stop there, not begin a backup and a config write it will be killed
// halfway through. The apply itself detaches once started. main's
// WaitIdle covers a cycle once SyncNow takes the op-lock.
func worker(ctx context.Context, agent Agent, trigger *pendingTrigger, spacing time.Duration) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-trigger.ready:
		}
		acceptedAt := trigger.take()
		func() {
			defer recoverReconcile()
			agent.SyncNow(ctx, acceptedAt)
		}()
		select {
		case <-ctx.Done():
			return
		case <-time.After(spacing):
		}
	}
}

// eventNameRe is what a forge's event name looks like. Anything else in an
// event header - which no signature covers - is reported as "unrecognized"
// rather than carried into the log.
var eventNameRe = regexp.MustCompile(`^[a-z_]{1,64}$`)

// pendingTrigger is the worker's queue: at most one cycle waiting, and
// when the newest delivery it stands for was accepted. The time goes
// along to SyncNow, which drops a cycle a Roll Back has overtaken - a
// delivery that arrived before the rollback, however long it then waited.
// The newest time wins because one delivery after the rollback is reason
// enough to run.
type pendingTrigger struct {
	ready chan struct{}

	mu sync.Mutex
	at time.Time
}

func newPendingTrigger() *pendingTrigger {
	return &pendingTrigger{ready: make(chan struct{}, 1)}
}

// set records a delivery accepted at at and wakes the worker, if it is
// not already due to wake.
func (p *pendingTrigger) set(at time.Time) {
	p.mu.Lock()
	if at.After(p.at) {
		p.at = at
	}
	p.mu.Unlock()
	select {
	case p.ready <- struct{}{}:
	default:
	}
}

// take returns the newest acceptance time and clears it.
func (p *pendingTrigger) take() time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	at := p.at
	p.at = time.Time{}
	return at
}

// deliveryEvent is the forge event a request names, lower-cased, or "".
func deliveryEvent(r *http.Request) string {
	for _, h := range eventHeaders {
		if v := strings.ToLower(strings.TrimSpace(r.Header.Get(h))); v != "" {
			if !eventNameRe.MatchString(v) {
				return "unrecognized"
			}
			return v
		}
	}
	return ""
}

// pushRef is the "ref" of a push payload read from the start of its body,
// or "" when that is not the JSON a forge sends (a form-encoded delivery,
// say) or "ref" does not come before the cut. "" triggers a cycle: a
// delivery the agent cannot read is not a reason to miss a push. Walked
// token by token rather than unmarshalled, because the prefix is usually
// a truncated document.
func pushRef(prefix []byte) string {
	dec := json.NewDecoder(bytes.NewReader(prefix))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return ""
	}
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return ""
		}
		if key == "ref" {
			var ref string
			if dec.Decode(&ref) != nil {
				return ""
			}
			return ref
		}
		var skip json.RawMessage
		if dec.Decode(&skip) != nil {
			return ""
		}
	}
	return ""
}

// cappedBuffer keeps the first max bytes written to it and discards the
// rest, never failing a write: it rides an io.MultiWriter whose other
// writers need every byte.
type cappedBuffer struct {
	max int
	buf []byte
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if room := c.max - len(c.buf); room > 0 {
		c.buf = append(c.buf, p[:min(room, len(p))]...)
	}
	return len(p), nil
}

// writeText answers with a short plain-text body, which a forge shows in
// its delivery log. Only fixed text and the configured branch go in it,
// never anything the request carried.
func writeText(w http.ResponseWriter, status int, text string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, text+"\n")
}

// seenBodies remembers the SHA-256 of signed bodies accepted within the
// last window, at most max of them, oldest dropped first. Safe for
// concurrent use.
type seenBodies struct {
	window time.Duration
	max    int

	mu    sync.Mutex
	order [][sha256.Size]byte
	at    map[[sha256.Size]byte]time.Time
}

// add records a body's SHA-256 at now and reports whether it was new:
// false when the identical body was accepted within the window.
func (s *seenBodies) add(sum [sha256.Size]byte, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.at == nil {
		s.at = map[[sha256.Size]byte]time.Time{}
	}
	for len(s.order) > 0 {
		oldest := s.order[0]
		if now.Sub(s.at[oldest]) <= s.window && len(s.order) < s.max {
			break
		}
		delete(s.at, oldest)
		s.order = s.order[1:]
	}
	if when, ok := s.at[sum]; ok && now.Sub(when) <= s.window {
		return false
	}
	s.at[sum] = now
	s.order = append(s.order, sum)
	return true
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

// authResult is authenticate's verdict. status is http.StatusAccepted when
// r may trigger a cycle, else the status to answer with and reason is for
// the log. signed says the request was judged by its signature, in which
// case digest is the SHA-256 of the exact bytes verified and prefix their
// first refPrefixBytes; a token request's body has not been read.
type authResult struct {
	status int
	reason string
	signed bool
	digest [sha256.Size]byte
	prefix []byte
}

// authenticate decides whether r may trigger a cycle. An empty secret
// never matches anything.
//
// A presented token decides alone, right or wrong. A valid one is
// accepted without looking at any signature, as before signatures
// existed (a forge may sign with some other secret of its own and still
// carry ?token=). A wrong one is a failed attempt even when the body is
// validly signed: a stale or guessed token is not rescued by a second
// credential. Only a request with no token at all is judged by its
// signature.
func authenticate(w http.ResponseWriter, r *http.Request, secret string) authResult {
	if secret == "" {
		return authResult{status: http.StatusForbidden, reason: "no secret configured"}
	}
	// The header is checked first, as it always was.
	candidate := r.Header.Get(tokenHeader)
	if candidate == "" {
		candidate = r.URL.Query().Get(tokenParam)
	}
	if candidate != "" {
		if subtle.ConstantTimeCompare([]byte(candidate), []byte(secret)) != 1 {
			return authResult{status: http.StatusForbidden, reason: "invalid token"}
		}
		return authResult{status: http.StatusAccepted}
	}
	return verifySignature(w, r, secret)
}

// verifySignature checks r's forge signature headers against an
// HMAC-SHA256 of its raw body. Every signature header present must
// verify, not just one: a real delivery signs them all with the same
// secret, so a mismatch among them is never a forge's doing. Malformed
// headers are refused before the body is read. The body is streamed
// through the HMAC and a SHA-256 (the handler's replay memory) and never
// held whole - an unauthenticated sender must not be able to make the
// agent buffer megabytes per connection - bar its first refPrefixBytes,
// kept for the ref.
func verifySignature(w http.ResponseWriter, r *http.Request, secret string) authResult {
	refuse := func(status int, reason string) authResult {
		return authResult{status: status, reason: reason}
	}
	var sigs [][]byte
	for _, h := range signatureHeaders {
		v := r.Header.Get(h.name)
		if v == "" {
			continue
		}
		digest, ok := strings.CutPrefix(v, h.prefix)
		if !ok {
			return refuse(http.StatusForbidden, "malformed signature")
		}
		sig, err := hex.DecodeString(digest)
		if err != nil {
			return refuse(http.StatusForbidden, "malformed signature")
		}
		sigs = append(sigs, sig)
	}
	if len(sigs) == 0 {
		return refuse(http.StatusForbidden, "missing token or signature")
	}

	mac := hmac.New(sha256.New, []byte(secret))
	digest := sha256.New()
	prefix := &cappedBuffer{max: refPrefixBytes}
	if _, err := io.Copy(io.MultiWriter(mac, digest, prefix), http.MaxBytesReader(w, r.Body, maxBodyBytes)); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return refuse(http.StatusRequestEntityTooLarge, "body too large")
		}
		return refuse(http.StatusBadRequest, "unreadable body")
	}
	want := mac.Sum(nil)
	for _, sig := range sigs {
		if !hmac.Equal(sig, want) {
			return refuse(http.StatusForbidden, "invalid signature")
		}
	}
	result := authResult{status: http.StatusAccepted, signed: true, prefix: prefix.buf}
	copy(result.digest[:], digest.Sum(nil))
	return result
}

// rateLimiter caps warn to one log line per interval. Safe for
// concurrent use: net/http dispatches requests to one handler.
type rateLimiter struct {
	interval time.Duration

	mu   sync.Mutex
	last time.Time
}

func (l *rateLimiter) warn(msg string, args ...any) {
	if l.allow() {
		slog.Warn(msg, args...)
	}
}

// info is warn at INFO, for lines a sender can repeat at will that are not
// a sign of anything wrong (an ignored event, a push to another branch).
func (l *rateLimiter) info(msg string, args ...any) {
	if l.allow() {
		slog.Info(msg, args...)
	}
}

func (l *rateLimiter) allow() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if !l.last.IsZero() && now.Sub(l.last) < l.interval {
		return false
	}
	l.last = now
	return true
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
