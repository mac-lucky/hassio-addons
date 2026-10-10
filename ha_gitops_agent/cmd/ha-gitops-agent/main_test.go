package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/options"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/recon"
)

// --- awaitSecretRefs ------------------------------------------------------

// secretsDir points awaitSecretRefs at a temp config root and shrinks its
// timings for one test.
func secretsDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	prevRoot, prevRetry, prevLog := secretsRoot, secretRetryInterval, secretLogInterval
	secretsRoot, secretRetryInterval, secretLogInterval = dir, 5*time.Millisecond, time.Hour
	t.Cleanup(func() { secretsRoot, secretRetryInterval, secretLogInterval = prevRoot, prevRetry, prevLog })
	return dir
}

func writeSecrets(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "secrets.yaml"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// On a fresh box secrets.yaml may not exist yet: the agent waits, says
// what for (the key and the option, never a value), and starts once the
// file answers.
func TestAwaitSecretRefsWaitsForTheKeyToAppear(t *testing.T) {
	dir := secretsDir(t)
	logged := captureLogs(t)

	reasons := make(chan string, 100)
	done := make(chan options.Options, 1)
	errs := make(chan error, 1)
	go func() {
		opts, err := awaitSecretRefs(context.Background(),
			options.Options{GitToken: "secret://forge_token", WebhookSecret: "literal-hook-secret-value"},
			nil, func(r string) {
				select {
				case reasons <- r:
				default: // never block the loop on a slow reader
				}
			})
		errs <- err
		done <- opts
	}()

	first := <-reasons
	if !strings.Contains(first, "forge_token") || !strings.Contains(first, "git_token") {
		t.Errorf("waiting reason = %q, want the key and the option named", first)
	}
	// The file appears without the key: still waiting.
	writeSecrets(t, dir, "other: x\n")
	for r := range reasons {
		if strings.Contains(r, "has no key") {
			break
		}
	}
	writeSecrets(t, dir, "forge_token: tok-RESOLVED-VALUE\n")

	if err := <-errs; err != nil {
		t.Fatalf("awaitSecretRefs: %v", err)
	}
	opts := <-done
	if opts.GitToken != "tok-RESOLVED-VALUE" || opts.WebhookSecret != "literal-hook-secret-value" {
		t.Errorf("opts = %+v, want the reference resolved and the literal kept", opts)
	}
	if out := logged(); strings.Contains(out, "tok-RESOLVED-VALUE") {
		t.Errorf("the log carries the resolved value: %s", out)
	}
	if out := logged(); strings.Count(out, "waiting for secrets.yaml key") != 1 {
		t.Errorf("logged the wait %d times, want once per secretLogInterval:\n%s", strings.Count(out, "waiting for secrets.yaml key"), out)
	}
}

func TestAwaitSecretRefsWithNothingToResolveDoesNotWait(t *testing.T) {
	secretsDir(t)
	called := false
	opts, err := awaitSecretRefs(context.Background(), options.Options{GitToken: "literal"}, nil, func(string) { called = true })
	if err != nil || opts.GitToken != "literal" {
		t.Errorf("awaitSecretRefs = (%+v, %v), want the literal back at once", opts, err)
	}
	if called {
		t.Error("reported a wait with nothing to wait for")
	}
}

// No wait fixes a typo, so a malformed reference stays fatal.
func TestAwaitSecretRefsMalformedReferenceIsFatal(t *testing.T) {
	secretsDir(t)
	_, err := awaitSecretRefs(context.Background(), options.Options{WebhookSecret: "secret://"}, nil, func(string) {})
	if err == nil || errors.Is(err, context.Canceled) {
		t.Fatalf("awaitSecretRefs() error = %v, want a refusal", err)
	}
}

func TestAwaitSecretRefsStopsOnShutdownAndOnAServerFailure(t *testing.T) {
	secretsDir(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := awaitSecretRefs(ctx, options.Options{GitToken: "secret://x"}, nil, func(string) {}); !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}

	abort := make(chan error, 1)
	abort <- errors.New("address already in use")
	_, err := awaitSecretRefs(context.Background(), options.Options{GitToken: "secret://x"}, abort, func(string) {})
	if !errors.Is(err, errServeStopped) {
		t.Errorf("error = %v, want errServeStopped", err)
	}
}

func TestWaitingStatusShowsTheReason(t *testing.T) {
	opts := options.Options{RepoURL: "https://x.invalid/r.git", Branch: "main"}
	if got := waitingStatus(opts, nil); got.WaitingForSecret == "" || got.State != recon.StateWaiting {
		t.Errorf("waitingStatus(nil) = %+v, want a placeholder reason and the waiting state", got)
	}
	reason := "waiting for secrets.yaml key 'k' (git_token): no file"
	if got := waitingStatus(opts, &reason); got.WaitingForSecret != reason || got.Branch != "main" {
		t.Errorf("waitingStatus = %+v", got)
	}
}

// --- awaitLoops ---------------------------------------------------------

// awaitLoops' whole output is a warning line, so these cases assert on
// which loops it says did not stop.

// shrinkShutdownTimeout makes the wait short enough to spend in full
// several times over.
func shrinkShutdownTimeout(t *testing.T) {
	t.Helper()
	prev := shutdownTimeout
	shutdownTimeout = 20 * time.Millisecond
	t.Cleanup(func() { shutdownTimeout = prev })
}

// captureLogs redirects slog for one test and returns everything written.
func captureLogs(t *testing.T) (logged func() string) {
	t.Helper()
	prev := slog.Default()
	var buf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf.String
}

func closedChan() <-chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}

func TestAwaitLoopsReturnsQuietlyWhenBothLoopsStopped(t *testing.T) {
	shrinkShutdownTimeout(t)
	logged := captureLogs(t)

	start := time.Now()
	awaitLoops(closedChan(), closedChan())

	if elapsed := time.Since(start); elapsed >= shutdownTimeout {
		t.Errorf("took %v, want a return without waiting out the %v window", elapsed, shutdownTimeout)
	}
	if out := logged(); strings.Contains(out, "did not stop") {
		t.Errorf("warned about a loop that had already stopped: %s", out)
	}
}

func TestAwaitLoopsNamesTheLoopThatDidNotStop(t *testing.T) {
	shrinkShutdownTimeout(t)
	logged := captureLogs(t)

	awaitLoops(closedChan(), make(chan struct{}))

	out := logged()
	if !strings.Contains(out, "add-on update loop did not stop") {
		t.Errorf("did not warn about the add-on update loop: %s", out)
	}
	if strings.Contains(out, "reconcile loop did not stop") {
		t.Errorf("warned about the reconcile loop, which had stopped: %s", out)
	}
}

func TestAwaitLoopsWarnsAboutBothWhenNeitherStops(t *testing.T) {
	shrinkShutdownTimeout(t)
	logged := captureLogs(t)

	awaitLoops(make(chan struct{}), make(chan struct{}))

	out := logged()
	for _, want := range []string{"reconcile loop did not stop", "add-on update loop did not stop"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in: %s", want, out)
		}
	}
}

// The case the non-blocking pre-check exists for: once the window is
// spent, a plain select over (done, ctx.Done()) picks at random and
// blames a stopped loop. Repeated because a random choice passes half
// the time.
func TestAwaitLoopsDoesNotSlanderAStoppedLoopAfterTheWindowIsSpent(t *testing.T) {
	shrinkShutdownTimeout(t)

	for i := 0; i < 20; i++ {
		logged := captureLogs(t)

		awaitLoops(make(chan struct{}), closedChan())

		out := logged()
		if !strings.Contains(out, "reconcile loop did not stop") {
			t.Fatalf("run %d: did not warn about the loop that really was running: %s", i, out)
		}
		if strings.Contains(out, "add-on update loop did not stop") {
			t.Fatalf("run %d: warned about a loop that had already stopped: %s", i, out)
		}
	}
}
