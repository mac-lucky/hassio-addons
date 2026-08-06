package gitsync

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// exitCodeRunner answers per git subcommand (args[1]), so a test can fail
// both the fetch and the probe after it. scriptedGitRunner cannot: it
// fails only the first match, which would let the probe succeed and hide
// exactly the confusion these tests exist to catch.
type exitCodeRunner struct {
	byCommand map[string]RunResult
	calls     [][]string
}

func (r *exitCodeRunner) Run(_ context.Context, _ string, _ []string, args ...string) (RunResult, error) {
	r.calls = append(r.calls, append([]string(nil), args...))
	if len(args) < 2 {
		return RunResult{}, nil
	}
	if res, ok := r.byCommand[args[1]]; ok {
		return res, nil
	}
	return RunResult{}, nil
}

func TestFetchReportsAnUnbornRemoteBranchAsItsOwnError(t *testing.T) {
	tmp := t.TempDir()
	bare := filepath.Join(tmp, "empty.git")
	runGitHelper(t, tmp, "init", "--bare", "-b", "main", bare)

	gs := New(makeOpts("file://"+bare), filepath.Join(tmp, "workdir"))
	if err := gs.EnsureClone(context.Background()); err != nil {
		t.Fatalf("EnsureClone: %v", err)
	}

	_, err := gs.Fetch(context.Background())
	if !errors.Is(err, ErrRemoteBranchMissing) {
		t.Fatalf("Fetch error = %v, want ErrRemoteBranchMissing", err)
	}
	if !strings.Contains(err.Error(), "main") {
		t.Errorf("error = %q, want it to name the branch", err)
	}
}

// A typo'd branch on a populated remote is the same fact as an unseeded
// one - the branch is not there - and must land in the same bucket.
func TestFetchReportsAMissingBranchOnANonEmptyRemote(t *testing.T) {
	tmp := t.TempDir()
	bare, _ := makeRemote(t, tmp, "populated")

	opts := makeOpts("file://" + bare)
	opts.Branch = "does-not-exist"
	gs := New(opts, filepath.Join(tmp, "workdir"))
	if err := gs.EnsureClone(context.Background()); err != nil {
		t.Fatalf("EnsureClone: %v", err)
	}

	_, err := gs.Fetch(context.Background())
	if !errors.Is(err, ErrRemoteBranchMissing) {
		t.Fatalf("Fetch error = %v, want ErrRemoteBranchMissing", err)
	}
}

// The classification fence. An unreachable host also exits 128, and
// calling that an unseeded repository would send the agent to a banner
// telling the user to import onto a remote it cannot even reach.
func TestFetchDoesNotClaimAMissingBranchWhenTheRemoteIsUnreachable(t *testing.T) {
	tmp := t.TempDir()
	gs := New(makeOpts("https://127.0.0.1:1/nope.git"), filepath.Join(tmp, "workdir"))
	gs.Timeout = 15 * time.Second

	_, err := gs.Fetch(context.Background())
	if err == nil {
		t.Fatal("Fetch succeeded against an unreachable remote")
	}
	if errors.Is(err, ErrRemoteBranchMissing) {
		t.Fatalf("unreachable remote reported as a missing branch: %v", err)
	}
}

// The other half of the fence, with the probe failing the same way the
// fetch did - which is what a bad token actually looks like.
func TestFetchDoesNotClaimAMissingBranchOnAnAuthFailure(t *testing.T) {
	authFailed := RunResult{ExitCode: 128, Stderr: "fatal: Authentication failed for 'https://git.example.invalid/repo.git/'"}
	fr := &exitCodeRunner{byCommand: map[string]RunResult{
		"fetch":     authFailed,
		"ls-remote": authFailed,
	}}

	gs := New(makeOpts("https://git.example.invalid/repo.git"), "/unused/workdir")
	gs.Runner = fr

	_, err := gs.Fetch(context.Background())
	if err == nil {
		t.Fatal("Fetch succeeded despite an auth failure")
	}
	if errors.Is(err, ErrRemoteBranchMissing) {
		t.Fatalf("auth failure reported as a missing branch: %v", err)
	}
	if !strings.Contains(err.Error(), "Authentication failed") {
		t.Errorf("error = %q, want git's own reason", err)
	}
}

// The probe is failure-only, which is what keeps the happy path at two
// calls - a property TestFetchCredentialNeverInArgvOnlyInEnv asserts
// exactly, and which an unconditional pre-check would break.
func TestFetchProbesTheRemoteOnlyAfterAFetchFails(t *testing.T) {
	fr := &fakeRunner{}
	gs := New(makeOpts("https://git.example.invalid/repo.git"), "/unused/workdir")
	gs.Runner = fr

	if _, err := gs.Fetch(context.Background()); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	for _, call := range fr.calls {
		if len(call.args) >= 2 && call.args[1] == "ls-remote" {
			t.Fatalf("probed the remote on the happy path: %v", call.args)
		}
	}
}

// The probe carries the same credentials the fetch does, and the same way:
// in the environment, never in argv.
func TestFetchProbeCarriesCredentialsInEnvOnly(t *testing.T) {
	token := "ghp_PROBETOKEN456"
	opts := makeOpts("https://git.example.invalid/repo.git")
	opts.GitUsername = "agent"
	opts.GitToken = token

	fr := &exitCodeRunner{byCommand: map[string]RunResult{
		"fetch":     {ExitCode: 128, Stderr: "fatal: couldn't find remote ref main"},
		"ls-remote": {ExitCode: 2},
	}}
	gs := New(opts, "/unused/workdir")
	gs.Runner = fr

	if _, err := gs.Fetch(context.Background()); !errors.Is(err, ErrRemoteBranchMissing) {
		t.Fatalf("Fetch error = %v, want ErrRemoteBranchMissing", err)
	}

	var probed bool
	for _, args := range fr.calls {
		if len(args) < 2 || args[1] != "ls-remote" {
			continue
		}
		probed = true
		for _, a := range args {
			if strings.Contains(a, token) {
				t.Fatalf("token leaked into the probe's argv: %v", args)
			}
		}
	}
	if !probed {
		t.Fatal("the probe never ran")
	}
}

func TestRemoteHasBranchDistinguishesAbsenceFromFailure(t *testing.T) {
	cases := []struct {
		name    string
		result  RunResult
		want    bool
		wantErr bool
	}{
		{"present", RunResult{ExitCode: 0}, true, false},
		{"absent", RunResult{ExitCode: 2}, false, false},
		{"unreachable", RunResult{ExitCode: 128, Stderr: "fatal: could not read from remote"}, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gs := New(makeOpts("https://git.example.invalid/repo.git"), "/unused/workdir")
			gs.Runner = &exitCodeRunner{byCommand: map[string]RunResult{"ls-remote": tc.result}}

			has, err := gs.RemoteHasBranch(context.Background())
			if tc.wantErr && err == nil {
				t.Fatal("RemoteHasBranch returned no error, want one")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("RemoteHasBranch: %v", err)
			}
			if has != tc.want {
				t.Errorf("has = %v, want %v", has, tc.want)
			}
		})
	}
}
