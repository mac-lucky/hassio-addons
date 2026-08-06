package gitsync

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

var fixedDriftTime = time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)

// showAtRef reads a path at ref straight from the bare remote, the
// independent check that a push landed what was expected.
func showAtRef(t *testing.T, bare, ref, path string) (string, bool) {
	t.Helper()
	cmd := exec.Command("git", "--git-dir="+bare, "show", ref+":"+path) // #nosec G204 -- fixed "git" binary; args are test-controlled fixture values
	out, err := cmd.Output()
	if err != nil {
		return "", false
	}
	return string(out), true
}

func listRemoteBranches(t *testing.T, bare string) []string {
	t.Helper()
	cmd := exec.Command("git", "--git-dir="+bare, "branch", "--format=%(refname:short)") // #nosec G204 -- see showAtRef
	// Pinned the way production's gitEnv does: git localizes this output,
	// and a localized detached-HEAD entry flakes across machines.
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git branch: %v", err)
	}
	var names []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line != "" {
			names = append(names, line)
		}
	}
	return names
}

func TestCommitBackCreatesBranchAndPushesWithoutTouchingConfiguredBranch(t *testing.T) {
	tmp := t.TempDir()
	bare, work := makeRemote(t, tmp, "remote")
	commitFile(t, work, "automations.yaml", "- id: demo\n  alias: Original\n", "commit")

	workdir := filepath.Join(tmp, "clone")
	gs := New(makeOpts("file://"+bare), workdir)
	ctx := context.Background()
	if err := gs.EnsureClone(ctx); err != nil {
		t.Fatalf("EnsureClone: %v", err)
	}
	sha, err := gs.Fetch(ctx)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if err := gs.Checkout(ctx, sha); err != nil {
		t.Fatalf("Checkout: %v", err)
	}

	configRoot := filepath.Join(tmp, "homeassistant")
	if err := os.MkdirAll(configRoot, 0o750); err != nil {
		t.Fatal(err)
	}
	liveContent := "- id: demo\n  alias: Hand-edited live\n"
	if err := os.WriteFile(filepath.Join(configRoot, "automations.yaml"), []byte(liveContent), 0o600); err != nil {
		t.Fatal(err)
	}

	branch, err := gs.CommitBack(ctx, []DriftFile{{Path: "automations.yaml", Kind: "update"}}, configRoot, sha, fixedDriftTime)
	if err != nil {
		t.Fatalf("CommitBack: %v", err)
	}
	if want := "gitops/drift-20260802T120000Z"; branch != want {
		t.Errorf("branch = %q, want %q", branch, want)
	}

	pushed, ok := showAtRef(t, bare, branch, "automations.yaml")
	if !ok {
		t.Fatalf("pushed branch %q does not contain automations.yaml", branch)
	}
	if pushed != liveContent {
		t.Errorf("pushed content = %q, want %q", pushed, liveContent)
	}

	mainContent, ok := showAtRef(t, bare, "main", "automations.yaml")
	if !ok || mainContent != "- id: demo\n  alias: Original\n" {
		t.Errorf("main content = %q (ok=%v), want the untouched original", mainContent, ok)
	}

	if got := gs.CurrentSHA(ctx); got != sha {
		t.Errorf("CurrentSHA() after CommitBack = %q, want %q (detached checkout must be restored)", got, sha)
	}
	data, err := os.ReadFile(filepath.Join(workdir, "automations.yaml")) // #nosec G304 -- workdir is a t.TempDir() fixture path this test created
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "- id: demo\n  alias: Original\n" {
		t.Errorf("workdir content after CommitBack = %q, want the repo's original (live write must be cleaned up)", data)
	}
}

func TestCommitBackStagesDeletionForFileGoneFromLive(t *testing.T) {
	tmp := t.TempDir()
	bare, work := makeRemote(t, tmp, "remote")
	commitFile(t, work, "scripts.yaml", "- id: demo\n", "commit")

	workdir := filepath.Join(tmp, "clone")
	gs := New(makeOpts("file://"+bare), workdir)
	ctx := context.Background()
	if err := gs.EnsureClone(ctx); err != nil {
		t.Fatalf("EnsureClone: %v", err)
	}
	sha, err := gs.Fetch(ctx)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if err := gs.Checkout(ctx, sha); err != nil {
		t.Fatalf("Checkout: %v", err)
	}

	// No scripts.yaml under configRoot at all: it was deleted live.
	configRoot := filepath.Join(tmp, "homeassistant")
	if err := os.MkdirAll(configRoot, 0o750); err != nil {
		t.Fatal(err)
	}

	branch, err := gs.CommitBack(ctx, []DriftFile{{Path: "scripts.yaml", Kind: "delete"}}, configRoot, sha, fixedDriftTime)
	if err != nil {
		t.Fatalf("CommitBack: %v", err)
	}

	if _, ok := showAtRef(t, bare, branch, "scripts.yaml"); ok {
		t.Error("scripts.yaml still present on the pushed branch, want it removed")
	}
	if _, ok := showAtRef(t, bare, "main", "scripts.yaml"); !ok {
		t.Error("scripts.yaml missing from main, want it untouched")
	}
}

func TestCommitBackNothingToStageReturnsErrorAndPushesNoBranch(t *testing.T) {
	tmp := t.TempDir()
	bare, work := makeRemote(t, tmp, "remote")
	commitFile(t, work, "automations.yaml", "- id: demo\n", "commit")

	workdir := filepath.Join(tmp, "clone")
	gs := New(makeOpts("file://"+bare), workdir)
	ctx := context.Background()
	if err := gs.EnsureClone(ctx); err != nil {
		t.Fatalf("EnsureClone: %v", err)
	}
	sha, err := gs.Fetch(ctx)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if err := gs.Checkout(ctx, sha); err != nil {
		t.Fatalf("Checkout: %v", err)
	}
	before := listRemoteBranches(t, bare)

	configRoot := filepath.Join(tmp, "homeassistant")
	if err := os.MkdirAll(configRoot, 0o750); err != nil {
		t.Fatal(err)
	}
	// Neither tracked nor live: nothing to stage in either direction.
	_, err = gs.CommitBack(ctx, []DriftFile{{Path: "never-existed.yaml", Kind: "update"}}, configRoot, sha, fixedDriftTime)
	if err == nil {
		t.Fatal("CommitBack() error = nil, want a \"nothing to stage\" error")
	}

	after := listRemoteBranches(t, bare)
	if len(after) != len(before) {
		t.Errorf("remote branches = %v, want unchanged %v (no push on nothing-to-stage)", after, before)
	}
	if got := gs.CurrentSHA(ctx); got != sha {
		t.Errorf("CurrentSHA() = %q, want %q (workdir must still be restored on failure)", got, sha)
	}
}

func TestCommitBackRejectsEmptyFilesOrBaseSHA(t *testing.T) {
	gs := New(makeOpts("file:///unused"), t.TempDir())
	ctx := context.Background()

	if _, err := gs.CommitBack(ctx, nil, "/unused", "deadbeef", fixedDriftTime); err == nil {
		t.Error("CommitBack() with no files: error = nil, want an error")
	}
	if _, err := gs.CommitBack(ctx, []DriftFile{{Path: "x.yaml", Kind: "update"}}, "/unused", "", fixedDriftTime); err == nil {
		t.Error("CommitBack() with no baseSHA: error = nil, want an error")
	}
}

// --- credential handling (fakeRunner, no real git process) -----------------

func TestCommitBackPushUsesCredentialEnvNeverArgv(t *testing.T) {
	token := "ghp_TESTTOKEN123"
	opts := makeOpts("https://git.example.invalid/repo.git")
	opts.GitUsername = "agent"
	opts.GitToken = token

	tmp := t.TempDir()
	configRoot := filepath.Join(tmp, "homeassistant")
	if err := os.MkdirAll(configRoot, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configRoot, "automations.yaml"), []byte("live\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	gs := New(opts, filepath.Join(tmp, "clone"))
	fr := &fakeRunner{}
	gs.Runner = fr

	branch, err := gs.CommitBack(context.Background(), []DriftFile{{Path: "automations.yaml", Kind: "update"}}, configRoot, strings.Repeat("a", 40), fixedDriftTime)
	if err != nil {
		t.Fatalf("CommitBack: %v", err)
	}

	var pushCall *recordedRun
	for i := range fr.calls {
		if len(fr.calls[i].args) >= 2 && fr.calls[i].args[1] == "push" {
			pushCall = &fr.calls[i]
		}
	}
	if pushCall == nil {
		t.Fatalf("no push call recorded among %+v", fr.calls)
	}
	if want := []string{"git", "push", opts.RepoURL, branch}; !equalArgs(pushCall.args, want) {
		t.Errorf("push args = %v, want %v", pushCall.args, want)
	}

	var hasCredentialEnv bool
	for _, kv := range pushCall.env {
		if strings.HasPrefix(kv, "GIT_CONFIG_VALUE_0=") {
			hasCredentialEnv = true
		}
	}
	if !hasCredentialEnv {
		t.Errorf("push env missing the credential override: %v", pushCall.env)
	}

	for _, call := range fr.calls {
		for _, a := range call.args {
			if strings.Contains(a, token) {
				t.Fatalf("token leaked into argv: %v", call.args)
			}
		}
		if call.args[1] != "push" {
			for _, kv := range call.env {
				if strings.HasPrefix(kv, "GIT_CONFIG_VALUE_0=") {
					t.Errorf("non-push call %v carries the credential env: %v", call.args, call.env)
				}
			}
		}
	}
}

// --- path-traversal guard ---------------------------------------------------

func TestCommitBackRejectsPathTraversal(t *testing.T) {
	tmp := t.TempDir()
	bare, work := makeRemote(t, tmp, "remote")
	commitFile(t, work, "automations.yaml", "- id: demo\n", "commit")

	workdir := filepath.Join(tmp, "clone")
	gs := New(makeOpts("file://"+bare), workdir)
	ctx := context.Background()
	if err := gs.EnsureClone(ctx); err != nil {
		t.Fatalf("EnsureClone: %v", err)
	}
	sha, err := gs.Fetch(ctx)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if err := gs.Checkout(ctx, sha); err != nil {
		t.Fatalf("Checkout: %v", err)
	}
	configRoot := filepath.Join(tmp, "homeassistant")
	if err := os.MkdirAll(configRoot, 0o750); err != nil {
		t.Fatal(err)
	}

	for _, p := range []string{"../outside.yaml", "/etc/passwd", "sub/../../outside.yaml"} {
		_, err := gs.CommitBack(ctx, []DriftFile{{Path: p, Kind: "update"}}, configRoot, sha, fixedDriftTime)
		if err == nil {
			t.Errorf("CommitBack(%q) error = nil, want a path-traversal refusal", p)
			continue
		}
		if !strings.Contains(err.Error(), "refusing to touch") && !strings.Contains(err.Error(), "escapes root") {
			t.Errorf("CommitBack(%q) error = %v, want a guardDriftPath refusal", p, err)
		}
	}
}

// --- B2/B3: symlink traversal + defense-in-depth filtering -----------------

func TestStageDriftRejectsLiveSymlinkEscapingConfigRoot(t *testing.T) {
	tmp := t.TempDir()
	bare, work := makeRemote(t, tmp, "remote")
	commitFile(t, work, "automations.yaml", "- id: demo\n", "commit")

	workdir := filepath.Join(tmp, "clone")
	gs := New(makeOpts("file://"+bare), workdir)
	ctx := context.Background()
	if err := gs.EnsureClone(ctx); err != nil {
		t.Fatalf("EnsureClone: %v", err)
	}
	sha, err := gs.Fetch(ctx)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if err := gs.Checkout(ctx, sha); err != nil {
		t.Fatalf("Checkout: %v", err)
	}

	// A secret living outside configRoot entirely.
	outsideSecret := filepath.Join(tmp, "outside-secret.txt")
	if err := os.WriteFile(outsideSecret, []byte("wifi_password: hunter2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	configRoot := filepath.Join(tmp, "homeassistant")
	if err := os.MkdirAll(configRoot, 0o750); err != nil {
		t.Fatal(err)
	}
	// automations.yaml under configRoot is a symlink to that secret.
	if err := os.Symlink(outsideSecret, filepath.Join(configRoot, "automations.yaml")); err != nil {
		t.Fatal(err)
	}

	_, err = gs.CommitBack(ctx, []DriftFile{{Path: "automations.yaml", Kind: "update"}}, configRoot, sha, fixedDriftTime)
	if err == nil {
		t.Fatal("CommitBack() error = nil, want a symlink-escape refusal")
	}
	if !strings.Contains(err.Error(), "escapes root") {
		t.Errorf("CommitBack() error = %v, want a guardDriftPath symlink refusal", err)
	}

	if _, ok := showAtRef(t, bare, "gitops/drift-20260802T120000Z", "automations.yaml"); ok {
		t.Error("outside secret content must never have been pushed")
	}
}

func TestStageDriftRejectsLiveSymlinkToExcludedNameInBounds(t *testing.T) {
	// The link's own name is innocuous but resolves, still inside
	// configRoot, to a secret-shaped one: secrets.yaml as "automations.yaml".
	tmp := t.TempDir()
	bare, work := makeRemote(t, tmp, "remote")
	commitFile(t, work, "automations.yaml", "- id: demo\n", "commit")

	workdir := filepath.Join(tmp, "clone")
	gs := New(makeOpts("file://"+bare), workdir)
	ctx := context.Background()
	if err := gs.EnsureClone(ctx); err != nil {
		t.Fatalf("EnsureClone: %v", err)
	}
	sha, err := gs.Fetch(ctx)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if err := gs.Checkout(ctx, sha); err != nil {
		t.Fatalf("Checkout: %v", err)
	}

	configRoot := filepath.Join(tmp, "homeassistant")
	if err := os.MkdirAll(configRoot, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configRoot, "secrets.yaml"), []byte("wifi_password: hunter2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(configRoot, "secrets.yaml"), filepath.Join(configRoot, "automations.yaml")); err != nil {
		t.Fatal(err)
	}

	_, err = gs.CommitBack(ctx, []DriftFile{{Path: "automations.yaml", Kind: "update"}}, configRoot, sha, fixedDriftTime)
	if err == nil {
		t.Fatal("CommitBack() error = nil, want a refusal for a symlink resolving to a secret-shaped path")
	}
	if !strings.Contains(err.Error(), "excluded/secret-shaped") {
		t.Errorf("CommitBack() error = %v, want the excluded/secret-shaped-resolution refusal", err)
	}
}

func TestStageDriftRejectsRepoSymlinkEscapingWorkdir(t *testing.T) {
	// Once "git checkout -B" materializes a tracked symlink, os.WriteFile
	// must not follow it outside the workdir. Reachable under dry_run too.
	tmp := t.TempDir()
	bare, work := makeRemote(t, tmp, "remote")
	commitFile(t, work, "unrelated.yaml", "- id: demo\n", "commit")

	// Track a symlink at "automations.yaml" pointing outside the repo.
	escapeTarget := filepath.Join(tmp, "outside-repo-target")
	if err := os.MkdirAll(escapeTarget, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(escapeTarget, filepath.Join(work, "automations.yaml")); err != nil {
		t.Fatal(err)
	}
	runGitHelper(t, work, "add", "automations.yaml")
	runGitHelper(t, work, "commit", "-m", "track a symlink")
	runGitHelper(t, work, "push", "origin", "main")

	workdir := filepath.Join(tmp, "clone")
	gs := New(makeOpts("file://"+bare), workdir)
	ctx := context.Background()
	if err := gs.EnsureClone(ctx); err != nil {
		t.Fatalf("EnsureClone: %v", err)
	}
	sha, err := gs.Fetch(ctx)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if err := gs.Checkout(ctx, sha); err != nil {
		t.Fatalf("Checkout: %v", err)
	}

	configRoot := filepath.Join(tmp, "homeassistant")
	if err := os.MkdirAll(configRoot, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configRoot, "automations.yaml"), []byte("live content\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err = gs.CommitBack(ctx, []DriftFile{{Path: "automations.yaml", Kind: "update"}}, configRoot, sha, fixedDriftTime)
	if err == nil {
		t.Fatal("CommitBack() error = nil, want a symlink-escape refusal on the repo/workdir side")
	}
	if !strings.Contains(err.Error(), "escapes root") {
		t.Errorf("CommitBack() error = %v, want a guardDriftPath symlink refusal", err)
	}
	if entries, _ := os.ReadDir(escapeTarget); len(entries) != 0 {
		t.Errorf("escapeTarget now contains %v, want nothing ever written there", entries)
	}
}

func TestStageDriftFiltersExcludedAndSecretNamesEvenWhenPassedExplicitly(t *testing.T) {
	// Defense in depth: differ.Compute already excludes these, but a caller
	// handing stageDrift one directly must still be refused.
	tmp := t.TempDir()
	bare, work := makeRemote(t, tmp, "remote")
	commitFile(t, work, "automations.yaml", "- id: demo\n", "commit")

	workdir := filepath.Join(tmp, "clone")
	gs := New(makeOpts("file://"+bare), workdir)
	ctx := context.Background()
	if err := gs.EnsureClone(ctx); err != nil {
		t.Fatalf("EnsureClone: %v", err)
	}
	sha, err := gs.Fetch(ctx)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if err := gs.Checkout(ctx, sha); err != nil {
		t.Fatalf("Checkout: %v", err)
	}
	configRoot := filepath.Join(tmp, "homeassistant")
	if err := os.MkdirAll(configRoot, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configRoot, "secrets.yaml"), []byte("wifi_password: hunter2\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, p := range []string{"secrets.yaml", ".ssh/id_rsa"} {
		_, err := gs.CommitBack(ctx, []DriftFile{{Path: p, Kind: "update"}}, configRoot, sha, fixedDriftTime)
		if err == nil {
			t.Errorf("CommitBack(%q) error = nil, want an excluded/secret-shaped refusal", p)
			continue
		}
		if !strings.Contains(err.Error(), "excluded/secret-shaped") {
			t.Errorf("CommitBack(%q) error = %v, want the excluded/secret-shaped refusal", p, err)
		}
	}
}

// --- S1: what an "add" means here is decided by live, not by the label ------

// "add" is differ's only name for "tracked, absent from live", covering
// both a live deletion and a not-yet-applied repo file. Indistinguishable
// here, so both are captured as live says: a removal on the drift branch.
func TestCommitBackCapturesAddKindPathMissingFromLiveAsADeletion(t *testing.T) {
	tmp := t.TempDir()
	bare, work := makeRemote(t, tmp, "remote")
	commitFile(t, work, "automations.yaml", "- id: demo\n", "commit")
	commitFile(t, work, "new_from_repo.yaml", "- id: new\n", "commit 2")

	workdir := filepath.Join(tmp, "clone")
	gs := New(makeOpts("file://"+bare), workdir)
	ctx := context.Background()
	if err := gs.EnsureClone(ctx); err != nil {
		t.Fatalf("EnsureClone: %v", err)
	}
	sha, err := gs.Fetch(ctx)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if err := gs.Checkout(ctx, sha); err != nil {
		t.Fatalf("Checkout: %v", err)
	}

	// Live has a hand-edited automations.yaml and no new_from_repo.yaml,
	// the shape differ labels "add".
	configRoot := filepath.Join(tmp, "homeassistant")
	if err := os.MkdirAll(configRoot, 0o750); err != nil {
		t.Fatal(err)
	}
	liveContent := "- id: demo\n  alias: Hand-edited live\n"
	if err := os.WriteFile(filepath.Join(configRoot, "automations.yaml"), []byte(liveContent), 0o600); err != nil {
		t.Fatal(err)
	}

	branch, err := gs.CommitBack(ctx, []DriftFile{
		{Path: "automations.yaml", Kind: "update"},
		{Path: "new_from_repo.yaml", Kind: "add"},
	}, configRoot, sha, fixedDriftTime)
	if err != nil {
		t.Fatalf("CommitBack: %v", err)
	}

	// The branch records live, which has no new_from_repo.yaml.
	if _, ok := showAtRef(t, bare, branch, "new_from_repo.yaml"); ok {
		t.Error("new_from_repo.yaml still on the drift branch, want it removed to match live")
	}
	// main keeps it: the drift branch is the only thing that ever moves.
	if _, ok := showAtRef(t, bare, "main", "new_from_repo.yaml"); !ok {
		t.Error("new_from_repo.yaml missing from main, want it untouched")
	}
	// The content drift IS captured.
	pushedAutomations, ok := showAtRef(t, bare, branch, "automations.yaml")
	if !ok || pushedAutomations != liveContent {
		t.Errorf("automations.yaml on drift branch = %q (ok=%v), want the hand-edited live content", pushedAutomations, ok)
	}
}

// --- S4: gitignored paths and empty commits ---------------------------------

func TestCommitBackSkipsGitignoredPathAndStillCommitsOthers(t *testing.T) {
	tmp := t.TempDir()
	bare, work := makeRemote(t, tmp, "remote")
	commitFile(t, work, ".gitignore", "ignored.yaml\n", "add gitignore")
	commitFile(t, work, "automations.yaml", "- id: demo\n", "commit")

	workdir := filepath.Join(tmp, "clone")
	gs := New(makeOpts("file://"+bare), workdir)
	ctx := context.Background()
	if err := gs.EnsureClone(ctx); err != nil {
		t.Fatalf("EnsureClone: %v", err)
	}
	sha, err := gs.Fetch(ctx)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if err := gs.Checkout(ctx, sha); err != nil {
		t.Fatalf("Checkout: %v", err)
	}

	configRoot := filepath.Join(tmp, "homeassistant")
	if err := os.MkdirAll(configRoot, 0o750); err != nil {
		t.Fatal(err)
	}
	liveAutomations := "- id: demo\n  alias: Hand-edited\n"
	if err := os.WriteFile(filepath.Join(configRoot, "automations.yaml"), []byte(liveAutomations), 0o600); err != nil {
		t.Fatal(err)
	}
	// ignored.yaml is live and .gitignore'd, so git really refuses the
	// "add"; gitAddSkippingIgnored's tolerance keeps the call alive.
	if err := os.WriteFile(filepath.Join(configRoot, "ignored.yaml"), []byte("- id: new\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	branch, err := gs.CommitBack(ctx, []DriftFile{
		{Path: "automations.yaml", Kind: "update"},
		{Path: "ignored.yaml", Kind: "add"},
	}, configRoot, sha, fixedDriftTime)
	if err != nil {
		t.Fatalf("CommitBack: %v", err)
	}

	pushed, ok := showAtRef(t, bare, branch, "automations.yaml")
	if !ok || pushed != liveAutomations {
		t.Errorf("automations.yaml on drift branch = %q (ok=%v), want the hand-edited live content", pushed, ok)
	}
	if _, ok := showAtRef(t, bare, branch, "ignored.yaml"); ok {
		t.Error("ignored.yaml must never have been added to the drift branch")
	}
}

// scriptedGitRunner cans a success for every git call except the one whose
// argv ends with failOnArgs, faking a refusal real git may not reach here.
type scriptedGitRunner struct {
	calls       []recordedRun
	failOnArgs  []string
	failResult  RunResult
	failApplied bool
}

func (r *scriptedGitRunner) Run(_ context.Context, dir string, env []string, args ...string) (RunResult, error) {
	r.calls = append(r.calls, recordedRun{dir: dir, env: append([]string(nil), env...), args: append([]string(nil), args...)})
	if !r.failApplied && matchesTail(args, r.failOnArgs) {
		r.failApplied = true
		return r.failResult, nil
	}
	if len(args) >= 2 && args[1] == "rev-parse" {
		return RunResult{Stdout: strings.Repeat("a", 40) + "\n"}, nil
	}
	return RunResult{}, nil
}

func matchesTail(args, tail []string) bool {
	if len(args) < len(tail) {
		return false
	}
	for i, want := range tail {
		if args[len(args)-len(tail)+i] != want {
			return false
		}
	}
	return true
}

func TestCommitBackGitignoredPathAloneIsSkippedNotFatal(t *testing.T) {
	tmp := t.TempDir()
	workdir := filepath.Join(tmp, "workdir")
	if err := os.MkdirAll(workdir, 0o750); err != nil {
		t.Fatal(err)
	}
	gs := New(makeOpts("file:///unused"), workdir)
	runner := &scriptedGitRunner{
		failOnArgs: []string{"add", "--", "ignored.yaml"},
		failResult: RunResult{
			ExitCode: 1,
			Stderr: "The following paths are ignored by one of your .gitignore files:\n" +
				"ignored.yaml\nhint: Use -f if you really want to add them.\n",
		},
	}
	gs.Runner = runner

	configRoot := filepath.Join(tmp, "homeassistant")
	if err := os.MkdirAll(configRoot, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configRoot, "ignored.yaml"), []byte("- id: demo\n  alias: Hand-edited\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// The only drifted path fails its "add" with git's real ignore text.
	// It must be skipped, landing on the clean "nothing to stage" refusal.
	_, err := gs.CommitBack(context.Background(), []DriftFile{{Path: "ignored.yaml", Kind: "update"}}, configRoot, "deadbeef", fixedDriftTime)
	if err == nil {
		t.Fatal("CommitBack() error = nil, want a \"nothing to stage\" refusal")
	}
	if !strings.Contains(err.Error(), "nothing to stage") {
		t.Errorf("CommitBack() error = %v, want the nothing-to-stage refusal, not a bare git failure", err)
	}
}

func TestCommitBackGitignoredPathSkippedButOthersStillLand(t *testing.T) {
	tmp := t.TempDir()
	workdir := filepath.Join(tmp, "workdir")
	if err := os.MkdirAll(workdir, 0o750); err != nil {
		t.Fatal(err)
	}
	gs := New(makeOpts("file:///unused"), workdir)
	runner := &scriptedGitRunner{
		failOnArgs: []string{"add", "--", "ignored.yaml"},
		failResult: RunResult{
			ExitCode: 1,
			Stderr:   "The following paths are ignored by one of your .gitignore files:\nignored.yaml\n",
		},
	}
	gs.Runner = runner

	configRoot := filepath.Join(tmp, "homeassistant")
	if err := os.MkdirAll(configRoot, 0o750); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"ignored.yaml", "automations.yaml"} {
		if err := os.WriteFile(filepath.Join(configRoot, name), []byte("- id: demo\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	branch, err := gs.CommitBack(context.Background(), []DriftFile{
		{Path: "ignored.yaml", Kind: "update"},
		{Path: "automations.yaml", Kind: "update"},
	}, configRoot, "deadbeef", fixedDriftTime)
	if err != nil {
		t.Fatalf("CommitBack: %v, want the whole call to still succeed for automations.yaml", err)
	}
	if branch == "" {
		t.Error("branch = \"\", want a real branch name")
	}

	addCalls := 0
	for _, c := range runner.calls {
		if len(c.args) >= 2 && c.args[1] == "add" {
			addCalls++
		}
	}
	if addCalls != 2 {
		t.Errorf("git add calls = %d, want 2 (both attempted; only ignored.yaml's failed and was tolerated)", addCalls)
	}
}

func TestCommitBackNothingToCommitIsACleanError(t *testing.T) {
	tmp := t.TempDir()
	bare, work := makeRemote(t, tmp, "remote")
	commitFile(t, work, "automations.yaml", "- id: demo\n", "commit")

	workdir := filepath.Join(tmp, "clone")
	gs := New(makeOpts("file://"+bare), workdir)
	ctx := context.Background()
	if err := gs.EnsureClone(ctx); err != nil {
		t.Fatalf("EnsureClone: %v", err)
	}
	sha, err := gs.Fetch(ctx)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if err := gs.Checkout(ctx, sha); err != nil {
		t.Fatalf("Checkout: %v", err)
	}

	// Live is byte-identical to sha, so "git add" stages nothing and the
	// commit fails clean despite stageDrift believing it staged something.
	configRoot := filepath.Join(tmp, "homeassistant")
	if err := os.MkdirAll(configRoot, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configRoot, "automations.yaml"), []byte("- id: demo\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err = gs.CommitBack(ctx, []DriftFile{{Path: "automations.yaml", Kind: "update"}}, configRoot, sha, fixedDriftTime)
	if err == nil {
		t.Fatal("CommitBack() error = nil, want a clean \"nothing to commit\" error")
	}
	if !strings.Contains(err.Error(), "nothing to commit") {
		t.Errorf("CommitBack() error = %v, want a clear \"nothing to commit\" message, not a bare git failure", err)
	}
	if strings.Contains(err.Error(), "(exit 1): \"") || strings.HasSuffix(strings.TrimSpace(err.Error()), "):") {
		t.Errorf("CommitBack() error = %v, looks like the bare unexplained git failure this fix removes", err)
	}
}

// --- VM e2e: real-hardware live deletion never reached the drift branch -----
//
// Uninstalling a HACS card removed a tracked file and every cycle logged
// "nothing to stage among [{www/community/... add}]": a live deletion
// carries no Kind but "add", so skipping those made it uncapturable.

// driftClone seeds a bare remote, a clone checked out at its tip, and an
// empty live config dir.
func driftClone(t *testing.T, files map[string]string) (gs *GitSync, bare, configRoot, sha string) {
	t.Helper()
	tmp := t.TempDir()
	bare, work := makeRemote(t, tmp, "remote")
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names) // fixed commit order, so the tip sha is not map-order dependent
	for _, name := range names {
		commitFile(t, work, name, files[name], "seed "+name)
	}

	gs = New(makeOpts("file://"+bare), filepath.Join(tmp, "clone"))
	ctx := context.Background()
	if err := gs.EnsureClone(ctx); err != nil {
		t.Fatalf("EnsureClone: %v", err)
	}
	sha, err := gs.Fetch(ctx)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if err := gs.Checkout(ctx, sha); err != nil {
		t.Fatalf("Checkout: %v", err)
	}

	configRoot = filepath.Join(tmp, "homeassistant")
	if err := os.MkdirAll(configRoot, 0o750); err != nil {
		t.Fatal(err)
	}
	return gs, bare, configRoot, sha
}

func writeLiveFile(t *testing.T, configRoot, rel, content string) {
	t.Helper()
	full := filepath.Join(configRoot, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCommitBackCapturesLiveDeletionOfTrackedFileReportedAsAdd(t *testing.T) {
	// The e2e shape: one tracked path, gone from live, and the only drift -
	// which is what turned the silent skip into a failure every cycle.
	const card = "www/community/gitops-e2e-fake-card/gitops-e2e-fake-card.js"
	gs, bare, configRoot, sha := driftClone(t, map[string]string{card: "console.log('card');\n"})

	branch, err := gs.CommitBack(context.Background(), []DriftFile{{Path: card, Kind: "add"}}, configRoot, sha, fixedDriftTime)
	if err != nil {
		t.Fatalf("CommitBack: %v", err)
	}
	if _, ok := showAtRef(t, bare, branch, card); ok {
		t.Errorf("%s still on the drift branch, want the live deletion captured as a removal", card)
	}
	if _, ok := showAtRef(t, bare, "main", card); !ok {
		t.Errorf("%s missing from main, want it untouched", card)
	}
}

func TestCommitBackKeepsPathWhoseLiveFileExistsButCannotBeStatted(t *testing.T) {
	// differ reports "add" for every stat failure, not only a missing file
	// (see diffTrackedPath), so an unreadable one is not a deletion.
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission bits do not apply")
	}
	gs, bare, configRoot, sha := driftClone(t, map[string]string{
		"locked/still-there.yaml": "- id: repo\n",
		"automations.yaml":        "- id: demo\n",
	})

	writeLiveFile(t, configRoot, "locked/still-there.yaml", "- id: live\n")
	liveEdit := "- id: demo\n  alias: Hand-edited live\n"
	writeLiveFile(t, configRoot, "automations.yaml", liveEdit)

	locked := filepath.Join(configRoot, "locked")
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Skipf("chmod unavailable: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o750) })

	branch, err := gs.CommitBack(context.Background(), []DriftFile{
		{Path: "locked/still-there.yaml", Kind: "add"},
		{Path: "automations.yaml", Kind: "update"},
	}, configRoot, sha, fixedDriftTime)
	if err != nil {
		t.Fatalf("CommitBack: %v", err)
	}

	kept, ok := showAtRef(t, bare, branch, "locked/still-there.yaml")
	if !ok {
		t.Fatal("locked/still-there.yaml was removed from the drift branch - an unreadable live file is not a deletion")
	}
	if kept != "- id: repo\n" {
		t.Errorf("locked/still-there.yaml on the drift branch = %q, want the repo's own untouched content", kept)
	}
	if pushed, ok := showAtRef(t, bare, branch, "automations.yaml"); !ok || pushed != liveEdit {
		t.Errorf("automations.yaml on the drift branch = %q (ok=%v), want the live edit still captured", pushed, ok)
	}
}

func TestCommitBackCapturesDeletionAndContentDriftInOneCommit(t *testing.T) {
	gs, bare, configRoot, sha := driftClone(t, map[string]string{
		"scripts.yaml":     "- id: script\n",
		"automations.yaml": "- id: demo\n",
	})
	// scripts.yaml is deleted live; automations.yaml is hand-edited.
	liveEdit := "- id: demo\n  alias: Hand-edited live\n"
	writeLiveFile(t, configRoot, "automations.yaml", liveEdit)

	branch, err := gs.CommitBack(context.Background(), []DriftFile{
		{Path: "scripts.yaml", Kind: "add"},
		{Path: "automations.yaml", Kind: "update"},
	}, configRoot, sha, fixedDriftTime)
	if err != nil {
		t.Fatalf("CommitBack: %v", err)
	}

	if _, ok := showAtRef(t, bare, branch, "scripts.yaml"); ok {
		t.Error("scripts.yaml still on the drift branch, want the deletion captured")
	}
	if pushed, ok := showAtRef(t, bare, branch, "automations.yaml"); !ok || pushed != liveEdit {
		t.Errorf("automations.yaml on the drift branch = %q (ok=%v), want the live edit", pushed, ok)
	}
}

func TestCommitBackDeletionOfUntrackedPathDoesNotSinkTheRest(t *testing.T) {
	// "git rm" on an untracked path is a hard git error, so it is passed
	// over rather than costing the drift that is capturable beside it.
	gs, bare, configRoot, sha := driftClone(t, map[string]string{"automations.yaml": "- id: demo\n"})
	liveEdit := "- id: demo\n  alias: Hand-edited live\n"
	writeLiveFile(t, configRoot, "automations.yaml", liveEdit)

	branch, err := gs.CommitBack(context.Background(), []DriftFile{
		{Path: "never-tracked.yaml", Kind: "add"},
		{Path: "automations.yaml", Kind: "update"},
	}, configRoot, sha, fixedDriftTime)
	if err != nil {
		t.Fatalf("CommitBack: %v", err)
	}
	if pushed, ok := showAtRef(t, bare, branch, "automations.yaml"); !ok || pushed != liveEdit {
		t.Errorf("automations.yaml on the drift branch = %q (ok=%v), want the live edit", pushed, ok)
	}
}

func equalArgs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
