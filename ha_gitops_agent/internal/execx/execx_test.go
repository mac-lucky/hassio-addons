package execx

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// --- Redact ---------------------------------------------------------------

func TestRedactStripsTokenFromText(t *testing.T) {
	token := "ghp_TESTTOKEN123"
	text := "fatal: unable to access 'https://agent:" + token + "@host/repo.git/': failure"

	redacted := Redact(text, token)
	if strings.Contains(redacted, token) {
		t.Errorf("Redact() = %q, still contains token", redacted)
	}
	if !strings.Contains(redacted, "***REDACTED***") {
		t.Errorf("Redact() = %q, want a ***REDACTED*** marker", redacted)
	}

	if Redact(text, "") != text {
		t.Error("Redact() with empty token should be a no-op")
	}
}

// TestRedactStripsEveryOccurrence: sops echoes the same identity once
// per rule it tried, so replacing only the first would leak the rest.
func TestRedactStripsEveryOccurrence(t *testing.T) {
	secret := "AGE-SECRET-KEY-EXAMPLE"
	text := "failed with " + secret + " and again with " + secret

	redacted := Redact(text, secret)
	if strings.Contains(redacted, secret) {
		t.Errorf("Redact() = %q, still contains the secret", redacted)
	}
	if got := strings.Count(redacted, "***REDACTED***"); got != 2 {
		t.Errorf("Redact() left %d markers, want 2", got)
	}
}

// TestRedactLeavesTextWithoutTheSecretAlone is the ordinary case: output
// that never mentions the credential must come back unchanged.
func TestRedactLeavesTextWithoutTheSecretAlone(t *testing.T) {
	text := "fatal: could not read from remote repository"
	if got := Redact(text, "ghp_TESTTOKEN123"); got != text {
		t.Errorf("Redact() = %q, want unchanged %q", got, text)
	}
}

// --- CommandRunner --------------------------------------------------------
// Real subprocesses, but only /bin/sh: these assert this package's own
// contract, nothing about git or sops.

func TestCommandRunnerCapturesBothStreamsAndZeroExit(t *testing.T) {
	result, err := CommandRunner{}.Run(context.Background(), t.TempDir(), os.Environ(),
		"/bin/sh", "-c", "printf out; printf err >&2")
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if result.Stdout != "out" {
		t.Errorf("Stdout = %q, want %q", result.Stdout, "out")
	}
	if result.Stderr != "err" {
		t.Errorf("Stderr = %q, want %q", result.Stderr, "err")
	}
	if result.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", result.ExitCode)
	}
}

// TestCommandRunnerReportsNonZeroExitWithoutError: a command that ran and
// failed is a RunResult, not an error, so the caller decides what the
// code means (sops's 203 "already encrypted" is not a failure).
func TestCommandRunnerReportsNonZeroExitWithoutError(t *testing.T) {
	result, err := CommandRunner{}.Run(context.Background(), t.TempDir(), os.Environ(),
		"/bin/sh", "-c", "printf nope >&2; exit 3")
	if err != nil {
		t.Fatalf("Run() error = %v, want nil for a non-zero exit", err)
	}
	if result.ExitCode != 3 {
		t.Errorf("ExitCode = %d, want 3", result.ExitCode)
	}
	if result.Stderr != "nope" {
		t.Errorf("Stderr = %q, want %q", result.Stderr, "nope")
	}
}

// TestCommandRunnerErrorsWhenTheBinaryIsMissing: failing to start is the
// one non-timeout error, and callers tell the two apart with
// errors.Is(err, context.DeadlineExceeded).
func TestCommandRunnerErrorsWhenTheBinaryIsMissing(t *testing.T) {
	_, err := CommandRunner{}.Run(context.Background(), t.TempDir(), os.Environ(),
		filepath.Join(t.TempDir(), "no-such-binary"))
	if err == nil {
		t.Fatal("Run() error = nil, want a launch failure")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Run() error = %v, must not be reported as a deadline", err)
	}
}

// TestCommandRunnerReportsADeadlineAsDeadlineExceeded: a killed process
// exits non-zero, so without the ctx.Err() check first a timeout would
// read as an ordinary failure.
func TestCommandRunnerReportsADeadlineAsDeadlineExceeded(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	_, err := CommandRunner{}.Run(ctx, t.TempDir(), os.Environ(), "/bin/sh", "-c", "sleep 5")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run() error = %v, want context.DeadlineExceeded", err)
	}
}

// TestCommandRunnerRunsInDirWithExactlyTheGivenEnv covers Run's two
// non-argv arguments: env replaces the process environment, so no
// inherited SOPS_AGE_KEY can reach an encrypt call.
func TestCommandRunnerRunsInDirWithExactlyTheGivenEnv(t *testing.T) {
	t.Setenv("EXECX_INHERITED", "leaked")

	dir := t.TempDir()
	result, err := CommandRunner{}.Run(context.Background(), dir, []string{"EXECX_GIVEN=given"},
		"/bin/sh", "-c", "pwd; printf '%s\\n' \"${EXECX_GIVEN}\" \"${EXECX_INHERITED-unset}\"")
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}

	lines := strings.Split(strings.TrimSpace(result.Stdout), "\n")
	if len(lines) != 3 {
		t.Fatalf("Stdout = %q, want three lines", result.Stdout)
	}
	// EvalSymlinks: macOS temp dirs are /var/folders/..., which the
	// shell's pwd reports as the /private/var real path.
	wantDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("resolving %s: %v", dir, err)
	}
	if lines[0] != wantDir {
		t.Errorf("working directory = %q, want %q", lines[0], wantDir)
	}
	if lines[1] != "given" {
		t.Errorf("EXECX_GIVEN = %q, want %q", lines[1], "given")
	}
	if lines[2] != "unset" {
		t.Errorf("EXECX_INHERITED = %q, want it never to reach the subprocess", lines[2])
	}
}
