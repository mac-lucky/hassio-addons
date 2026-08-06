// Package execx runs one external command, reports what it did, and
// scrubs a secret back out of what it said. Shared by internal/gitsync
// (git) and internal/sopscrypt (sops); a standard-library-only leaf, so
// neither has to depend on the other.
package execx

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"strings"
)

// RunResult is the outcome of one Runner.Run call.
type RunResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// Runner executes one external command; CommandRunner is production, a
// fake stands in for tests.
//
// An error means the command never ran to completion - on a deadline it
// must satisfy errors.Is(err, context.DeadlineExceeded). A non-zero exit
// is not an error: it arrives as RunResult.ExitCode for the caller to
// interpret.
type Runner interface {
	Run(ctx context.Context, dir string, env []string, args ...string) (RunResult, error)
}

// CommandRunner is the production Runner: a real subprocess via
// exec.CommandContext.
type CommandRunner struct{}

// Run executes args[0] with args[1:] in dir with exactly env, capturing
// both output streams. env replaces the process environment rather than
// extending it, so no inherited age key can reach an encrypt call.
func (CommandRunner) Run(ctx context.Context, dir string, env []string, args ...string) (RunResult, error) {
	cmd := exec.CommandContext(ctx, args[0], args[1:]...) // #nosec G204 -- argv is built by the calling package from fixed git/sops subcommands plus repo config and paths, never unsanitized input. Covers internal/gitsync and internal/sopscrypt; a new caller must re-justify it.
	cmd.Dir = dir
	cmd.Env = env

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		// A killed process exits non-zero, so a deadline also surfaces
		// as *exec.ExitError; ctx has to be checked first.
		if ctx.Err() != nil {
			return RunResult{}, ctx.Err()
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return RunResult{Stdout: stdout.String(), Stderr: stderr.String(), ExitCode: exitErr.ExitCode()}, nil
		}
		return RunResult{}, err
	}
	return RunResult{Stdout: stdout.String(), Stderr: stderr.String(), ExitCode: 0}, nil
}

// Redact returns text with every occurrence of secret replaced, applied
// to subprocess output before it is logged or wrapped in an error. An
// empty secret is a no-op, so callers can redact unconditionally.
func Redact(text, secret string) string {
	if secret == "" {
		return text
	}
	return strings.ReplaceAll(text, secret, "***REDACTED***")
}
