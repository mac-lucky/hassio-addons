package main

import (
	"bytes"
	"log/slog"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/gitsync"
)

// configureEncryption is the one place the encryption switch and the
// Crypter are set together; these cases pin that "switch on, no Crypter"
// stays unreachable. Each restores the switch, which is process-global.

func TestConfigureEncryptionWithoutAKeyLeavesTheSwitchOff(t *testing.T) {
	t.Cleanup(func() { gitsync.SetEncryptionEnabled(false) })

	for _, key := range []string{"", "   ", "\n"} {
		crypter, err := configureEncryption(key)
		if err != nil {
			t.Fatalf("configureEncryption(%q) = %v, want no error", key, err)
		}
		if crypter.Enabled() {
			t.Errorf("configureEncryption(%q) returned an enabled Crypter, want none", key)
		}
		if gitsync.EncryptionEnabled() {
			t.Errorf("configureEncryption(%q) turned the encryption switch on", key)
		}
	}
}

func TestConfigureEncryptionRejectsAMalformedKeyWithoutFlippingTheSwitch(t *testing.T) {
	t.Cleanup(func() { gitsync.SetEncryptionEnabled(false) })

	crypter, err := configureEncryption("not-an-age-key")
	if err == nil {
		t.Fatal("configureEncryption() = nil error, want a refusal for a malformed key")
	}
	if crypter.Enabled() {
		t.Error("configureEncryption() returned an enabled Crypter for a malformed key")
	}
	// The dangerous outcome: the switch on lets secrets.yaml into the
	// repository with no key to protect it.
	if gitsync.EncryptionEnabled() {
		t.Error("a malformed key still turned the encryption switch on")
	}
}

// testAgeIdentity is an age key used only by this test file.
const testAgeIdentity = "AGE-SECRET-KEY-1QUUCUYTP2443EWJWQKK6LCAAUGS09XXGDHLVQV82Z2Y6200NDGAQJ8SUFT"

func TestConfigureEncryptionEnablesBothTogether(t *testing.T) {
	if _, err := exec.LookPath("sops"); err != nil {
		t.Skip("sops is not installed; configureEncryption probes the real binary")
	}
	t.Cleanup(func() { gitsync.SetEncryptionEnabled(false) })

	crypter, err := configureEncryption(testAgeIdentity)
	if err != nil {
		t.Fatalf("configureEncryption: %v", err)
	}
	if !crypter.Enabled() {
		t.Error("configureEncryption() returned a disabled Crypter for a valid key")
	}
	// The switch must go on only after the probe proves sops runs.
	if !gitsync.EncryptionEnabled() {
		t.Error("a valid key left the encryption switch off")
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
