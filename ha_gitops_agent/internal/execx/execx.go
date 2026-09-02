// Package execx runs one external command, reports what it did, and
// scrubs a secret back out of what it said. Shared by internal/gitsync
// (git) and internal/sopscrypt (sops); a standard-library-only leaf, so
// neither has to depend on the other.
package execx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"syscall"
	"time"
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
	// CommandContext's default cancel kills only the direct child; a timed
	// out git fetch would orphan its git-remote-https grandchild, which
	// keeps its connection and keeps running until it finishes on its own.
	// Start the child in its own process group and kill the whole group.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	// The buffers below make Run wait on pipe-copy goroutines that finish
	// only when every DESCENDANT closes the write end. The group kill above
	// reaps the usual holders, but a descendant that called setsid escapes
	// it, and without a WaitDelay that wait is forever, past the deadline,
	// with the caller's operation lock held.
	cmd.WaitDelay = 10 * time.Second

	stdout := &cappedBuffer{limit: maxOutputBytes}
	stderr := &cappedBuffer{limit: maxOutputBytes}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	err := cmd.Run()
	if err != nil {
		// A killed process exits non-zero, so a deadline also surfaces
		// as *exec.ExitError; ctx has to be checked first.
		if ctx.Err() != nil {
			return RunResult{}, ctx.Err()
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return RunResult{Stdout: stdout.buf.String(), Stderr: stderr.buf.String(), ExitCode: exitErr.ExitCode()}, nil
		}
		return RunResult{}, err
	}
	return RunResult{Stdout: stdout.buf.String(), Stderr: stderr.buf.String(), ExitCode: 0}, nil
}

// maxOutputBytes bounds each captured stream. Any blob a caller needs in
// full (git show, a sops decrypt) fits comfortably; an unbounded stream
// would grow the add-on into its memory limit and be OOM-killed mid-apply
// instead of failing the one call.
const maxOutputBytes = 64 << 20

// cappedBuffer is a bytes.Buffer that refuses to grow past limit. Refusal,
// not truncation: the callers use stdout as file CONTENT, and a silently
// truncated decrypt or git show must never be written anywhere.
type cappedBuffer struct {
	buf   bytes.Buffer
	limit int
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if c.buf.Len()+len(p) > c.limit {
		return 0, fmt.Errorf("subprocess output exceeded %d MiB", c.limit>>20)
	}
	return c.buf.Write(p)
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

// credentialInURL matches a "user:password@" credential ahead of a host,
// including the scheme-less shapes url.Parse reads as an opaque path with
// no Userinfo ("github.com/u:token@repo"). An scp-style "git@host:" has no
// colon before its "@" and stays untouched.
var credentialInURL = regexp.MustCompile(`([^/@:\s]+):([^@\s]+)@`)

// RedactURL returns rawURL safe to log or display: userinfo is stripped,
// any query string is dropped (tokens ride there too), and a string
// url.Parse cannot decompose at all is replaced rather than trusted.
func RedactURL(rawURL string) string {
	if rawURL == "" {
		return ""
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "(unparseable url, redacted)"
	}
	parsed.User = nil
	parsed.RawQuery = ""
	return credentialInURL.ReplaceAllString(parsed.String(), "${1}:***REDACTED***@")
}
