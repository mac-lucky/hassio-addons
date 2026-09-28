package gitsync

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// scriptedFetchRunner answers each fetch with the next scripted result
// and everything else with success.
type scriptedFetchRunner struct {
	fetches []RunResult
	calls   int
}

func (s *scriptedFetchRunner) Run(_ context.Context, _ string, _ []string, args ...string) (RunResult, error) {
	if len(args) >= 2 && args[1] == "fetch" {
		res := RunResult{}
		if s.calls < len(s.fetches) {
			res = s.fetches[s.calls]
		}
		s.calls++
		return res, nil
	}
	if len(args) >= 2 && args[1] == "rev-parse" {
		return RunResult{Stdout: strings.Repeat("b", 40) + "\n"}, nil
	}
	return RunResult{}, nil
}

// Forgejo restarting behind its proxy answers 502 for a few seconds, and
// every such blip used to fail the cycle.
func TestFetchRetriesATransientFailure(t *testing.T) {
	gs := New(makeOpts("https://git.example.invalid/repo.git"), "/unused/workdir")
	runner := &scriptedFetchRunner{fetches: []RunResult{{
		ExitCode: 128,
		Stderr:   "fatal: unable to access 'https://git.example.invalid/repo.git/': The requested URL returned error: 502",
	}}}
	gs.Runner = runner

	sha, err := gs.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v, want the retry to succeed", err)
	}
	if sha == "" || runner.calls != 2 {
		t.Errorf("sha = %q, fetches = %d; want a sha after 2 fetches", sha, runner.calls)
	}
}

func TestFetchDoesNotRetryAnAuthFailure(t *testing.T) {
	gs := New(makeOpts("https://git.example.invalid/repo.git"), "/unused/workdir")
	authFailed := RunResult{ExitCode: 128, Stderr: "fatal: Authentication failed for 'https://git.example.invalid/repo.git/'"}
	runner := &scriptedFetchRunner{fetches: []RunResult{authFailed, authFailed, authFailed}}
	gs.Runner = runner

	if _, err := gs.Fetch(context.Background()); err == nil {
		t.Fatal("Fetch succeeded on an auth failure")
	}
	if runner.calls != 1 {
		t.Errorf("fetches = %d, want 1 - a bad token does not heal in seconds", runner.calls)
	}
}

func TestFetchGivesUpAfterTheLastRetry(t *testing.T) {
	gs := New(makeOpts("https://git.example.invalid/repo.git"), "/unused/workdir")
	down := RunResult{ExitCode: 128, Stderr: "fatal: unable to access: Could not resolve host: git.example.invalid"}
	runner := &scriptedFetchRunner{fetches: []RunResult{down, down, down, down}}
	gs.Runner = runner

	if _, err := gs.Fetch(context.Background()); err == nil {
		t.Fatal("Fetch succeeded with the host down")
	}
	if want := len(fetchRetryDelays) + 1; runner.calls != want {
		t.Errorf("fetches = %d, want %d", runner.calls, want)
	}
}

// A clone killed partway used to leave a .git with the right origin and no
// history, which EnsureClone then accepted forever.
func TestAFailedCloneLeavesNothingBehind(t *testing.T) {
	tmp := t.TempDir()
	workdir := filepath.Join(tmp, "repo")
	gs := New(makeOpts("file://"+filepath.Join(tmp, "missing.git")), workdir)

	if err := gs.EnsureClone(context.Background()); err == nil {
		t.Fatal("EnsureClone succeeded from a repository that does not exist")
	}

	for _, p := range []string{workdir, workdir + ".partial"} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s exists after a failed clone (err %v)", p, err)
		}
	}
}

// A forge that accepts the connection and then sends nothing held a fetch,
// and the operation lock, for the whole ten-minute network budget.
func TestFetchCarriesAStallLimit(t *testing.T) {
	gs := New(makeOpts("https://git.example.invalid/repo.git"), "/unused/workdir")
	fr := &fakeRunner{}
	gs.Runner = fr

	if _, err := gs.Fetch(context.Background()); err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	for _, call := range fr.calls {
		if len(call.args) > 1 && call.args[1] == "fetch" {
			if !slices.Contains(call.env, "GIT_HTTP_LOW_SPEED_LIMIT=1024") || !slices.Contains(call.env, "GIT_HTTP_LOW_SPEED_TIME=30") {
				t.Errorf("fetch env = %v, want the low-speed limit", call.env)
			}
			return
		}
	}
	t.Fatal("no fetch was run")
}
