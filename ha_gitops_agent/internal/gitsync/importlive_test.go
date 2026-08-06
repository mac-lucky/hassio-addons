package gitsync

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/execx"
)

// fixedImportTime keeps the throwaway branch name deterministic.
var fixedImportTime = time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)

// Derived so a driftBranchTimeFormat change follows here rather than
// surfacing as an argv mismatch that never mentions the format.
var fixedImportBranch = "gitops/import-" + fixedImportTime.UTC().Format(driftBranchTimeFormat)

// importGitSync builds a GitSync cloned from bare, with no credentials.
func importGitSync(t *testing.T, bare, workdir string) *GitSync {
	t.Helper()
	opts := makeOpts("file://" + bare)
	opts.GitUsername = ""
	opts.GitToken = ""
	gs := New(opts, workdir)
	if err := gs.EnsureClone(context.Background()); err != nil {
		t.Fatalf("EnsureClone: %v", err)
	}
	return gs
}

// The happy path: live files land on the tracked branch, nothing else does.
func TestImportSeedsTrackedBranchFastForward(t *testing.T) {
	tmp := t.TempDir()
	bare, work := makeRemote(t, tmp, "repo")
	commitFile(t, work, "README.md", "readme\n", "init")

	configRoot := filepath.Join(tmp, "config")
	writeLive(t, configRoot, "configuration.yaml", 0)
	if err := os.WriteFile(filepath.Join(configRoot, "configuration.yaml"), []byte("live: yes\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeLive(t, configRoot, "packages/lights.yaml", 8)

	gs := importGitSync(t, bare, filepath.Join(tmp, "workdir"))
	before := gs.CurrentSHA(context.Background())

	res, err := gs.Import(context.Background(), configRoot, generousLimits(), fixedImportTime)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if res.Files != 2 {
		t.Errorf("Files = %d, want 2", res.Files)
	}
	if res.Created {
		t.Error("Created = true, want false (the branch already existed)")
	}

	got, ok := showAtRef(t, bare, "main", "configuration.yaml")
	if !ok || got != "live: yes\n" {
		t.Errorf("main:configuration.yaml = %q (ok=%v), want the live content", got, ok)
	}
	if _, ok := showAtRef(t, bare, "main", "packages/lights.yaml"); !ok {
		t.Error("packages/lights.yaml missing from the tracked branch")
	}

	// The throwaway branch is local only; the refspec target is all that
	// reaches the remote.
	for _, b := range listRemoteBranches(t, bare) {
		if b != "main" {
			t.Errorf("remote gained branch %q, want only main", b)
		}
	}
	// Every other GitSync method assumes a plain detached checkout between
	// calls, so the workdir goes back exactly where it was.
	if after := gs.CurrentSHA(context.Background()); after != before {
		t.Errorf("workdir left at %s, want it restored to %s", after, before)
	}
	if branches := localBranches(t, gs.Workdir); contains(branches, fixedImportBranch) {
		t.Errorf("local branches = %v, want the throwaway %s deleted", branches, fixedImportBranch)
	}
}

func TestImportCreatesTrackedBranchOnEmptyRemote(t *testing.T) {
	tmp := t.TempDir()
	bare := filepath.Join(tmp, "empty.git")
	runGitHelper(t, tmp, "init", "--bare", "-b", "main", bare)

	configRoot := filepath.Join(tmp, "config")
	writeLive(t, configRoot, "configuration.yaml", 12)

	workdir := filepath.Join(tmp, "workdir")
	gs := New(makeOpts("file://"+bare), workdir)
	if err := gs.EnsureClone(context.Background()); err != nil {
		t.Fatalf("EnsureClone: %v", err)
	}

	res, err := gs.Import(context.Background(), configRoot, generousLimits(), fixedImportTime)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if !res.Created {
		t.Error("Created = false, want true (the branch did not exist on the remote)")
	}
	if res.BaseSHA != "" {
		t.Errorf("BaseSHA = %q, want empty (nothing to build on)", res.BaseSHA)
	}
	if _, ok := showAtRef(t, bare, "main", "configuration.yaml"); !ok {
		t.Error("configuration.yaml missing from the newly created branch")
	}
}

// The repository legitimately holds paths that never exist live.
func TestImportNeverDeletesRepoOnlyPaths(t *testing.T) {
	tmp := t.TempDir()
	bare, work := makeRemote(t, tmp, "repo")
	commitFile(t, work, "README.md", "readme\n", "init")
	commitFile(t, work, "gitops/registries.yaml", "areas: []\n", "manifests")

	configRoot := filepath.Join(tmp, "config")
	writeLive(t, configRoot, "configuration.yaml", 4)

	gs := importGitSync(t, bare, filepath.Join(tmp, "workdir"))
	if _, err := gs.Import(context.Background(), configRoot, generousLimits(), fixedImportTime); err != nil {
		t.Fatalf("Import: %v", err)
	}

	for _, p := range []string{"README.md", "gitops/registries.yaml"} {
		if _, ok := showAtRef(t, bare, "main", p); !ok {
			t.Errorf("%s was removed from the tracked branch; import must never delete", p)
		}
	}
}

// interceptRunner runs everything for real but calls before() the first
// time trigger matches, moving the remote under an in-flight import.
type interceptRunner struct {
	inner   Runner
	trigger []string
	before  func()
	fired   bool
}

func (r *interceptRunner) Run(ctx context.Context, dir string, env []string, args ...string) (RunResult, error) {
	if !r.fired && len(args) >= 2 && args[1] == r.trigger[0] {
		r.fired = true
		r.before()
	}
	return r.inner.Run(ctx, dir, env, args...)
}

// Import pushes to the branch the user works on, so a competing push
// landing between its fetch and its push must make the import lose cleanly.
func TestImportRejectedWhenRemoteMovedNonFastForward(t *testing.T) {
	tmp := t.TempDir()
	bare, work := makeRemote(t, tmp, "repo")
	commitFile(t, work, "README.md", "readme\n", "init")

	configRoot := filepath.Join(tmp, "config")
	writeLive(t, configRoot, "configuration.yaml", 6)

	gs := importGitSync(t, bare, filepath.Join(tmp, "workdir"))
	before := gs.CurrentSHA(context.Background())

	// Keyed on the subcommand, not the full argv: a future push flag would
	// otherwise silently stop the competing commit from ever being made.
	ir := &interceptRunner{
		inner:   execx.CommandRunner{},
		trigger: []string{"push"},
		before: func() {
			commitFile(t, work, "someone-else.yaml", "theirs\n", "concurrent")
		},
	}
	gs.Runner = ir

	_, err := gs.Import(context.Background(), configRoot, generousLimits(), fixedImportTime)
	if !ir.fired {
		t.Fatal("the interceptor never fired, so no competing commit was made and this test did not exercise the race at all")
	}
	if !errors.Is(err, ErrImportRejected) {
		t.Fatalf("Import error = %v, want ErrImportRejected", err)
	}

	if _, ok := showAtRef(t, bare, "main", "someone-else.yaml"); !ok {
		t.Fatal("the concurrent commit was clobbered - import must never force a push")
	}
	if _, ok := showAtRef(t, bare, "main", "configuration.yaml"); ok {
		t.Error("the rejected import's content reached the tracked branch anyway")
	}
	if after := gs.CurrentSHA(context.Background()); after != before {
		t.Errorf("workdir left at %s, want it restored to %s", after, before)
	}
}

func TestImportRespectsRepositoryGitignore(t *testing.T) {
	tmp := t.TempDir()
	bare, work := makeRemote(t, tmp, "repo")
	commitFile(t, work, ".gitignore", "local/\n", "ignore local")

	configRoot := filepath.Join(tmp, "config")
	writeLive(t, configRoot, "configuration.yaml", 4)
	writeLive(t, configRoot, "local/scratch.yaml", 4)

	gs := importGitSync(t, bare, filepath.Join(tmp, "workdir"))
	if _, err := gs.Import(context.Background(), configRoot, generousLimits(), fixedImportTime); err != nil {
		t.Fatalf("Import: %v", err)
	}

	if _, ok := showAtRef(t, bare, "main", "local/scratch.yaml"); ok {
		t.Error("a gitignored path was imported; .gitignore is the supported way to shape an import")
	}
	if _, ok := showAtRef(t, bare, "main", "configuration.yaml"); !ok {
		t.Error("the ordinary file did not land")
	}
}

func TestImportSkipsSymlinksExcludedAndSecrets(t *testing.T) {
	tmp := t.TempDir()
	bare, work := makeRemote(t, tmp, "repo")
	commitFile(t, work, "README.md", "readme\n", "init")

	configRoot := filepath.Join(tmp, "config")
	writeLive(t, configRoot, "configuration.yaml", 4)
	writeLive(t, configRoot, "secrets.yaml", 4)
	writeLive(t, configRoot, "home-assistant_v2.db", 4)
	writeLive(t, configRoot, ".storage/core.entity_registry", 4)
	writeLive(t, configRoot, "deps/lib.py", 4)
	writeLive(t, configRoot, "id_rsa", 4)
	if err := os.Symlink("/etc/passwd", filepath.Join(configRoot, "notes.yaml")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	gs := importGitSync(t, bare, filepath.Join(tmp, "workdir"))
	if _, err := gs.Import(context.Background(), configRoot, generousLimits(), fixedImportTime); err != nil {
		t.Fatalf("Import: %v", err)
	}

	for _, p := range []string{"secrets.yaml", "home-assistant_v2.db", ".storage/core.entity_registry", "deps/lib.py", "id_rsa", "notes.yaml"} {
		if _, ok := showAtRef(t, bare, "main", p); ok {
			t.Errorf("%s reached the repository, want it filtered out", p)
		}
	}
	if _, ok := showAtRef(t, bare, "main", "configuration.yaml"); !ok {
		t.Error("the ordinary file did not land")
	}
}

func TestImportRestoresWorkdirOnPushFailure(t *testing.T) {
	tmp := t.TempDir()
	bare, work := makeRemote(t, tmp, "repo")
	commitFile(t, work, "README.md", "readme\n", "init")

	configRoot := filepath.Join(tmp, "config")
	writeLive(t, configRoot, "configuration.yaml", 4)

	gs := importGitSync(t, bare, filepath.Join(tmp, "workdir"))
	before := gs.CurrentSHA(context.Background())
	gs.Opts.RepoURL = "file://" + filepath.Join(tmp, "does-not-exist.git")

	if _, err := gs.Import(context.Background(), configRoot, generousLimits(), fixedImportTime); err == nil {
		t.Fatal("Import succeeded against a nonexistent remote, want an error")
	}
	if after := gs.CurrentSHA(context.Background()); after != before {
		t.Errorf("workdir left at %s, want %s", after, before)
	}
	if branches := localBranches(t, gs.Workdir); contains(branches, fixedImportBranch) {
		t.Errorf("local branches = %v, want the throwaway branch cleaned up", branches)
	}
}

func TestImportRefusesEmptyLiveTree(t *testing.T) {
	tmp := t.TempDir()
	bare, work := makeRemote(t, tmp, "repo")
	commitFile(t, work, "README.md", "readme\n", "init")

	gs := importGitSync(t, bare, filepath.Join(tmp, "workdir"))
	before := gs.CurrentSHA(context.Background())

	_, err := gs.Import(context.Background(), t.TempDir(), generousLimits(), fixedImportTime)
	if err == nil || !strings.Contains(err.Error(), "no importable files") {
		t.Fatalf("Import error = %v, want a no-importable-files refusal", err)
	}
	if after := gs.CurrentSHA(context.Background()); after != before {
		t.Error("an empty scan disturbed the checkout; it must fail before any git command")
	}
}

// No --force, no --force-with-lease, no "+" refspec prefix, ever.
func TestImportPushArgvIsFastForwardOnlyAndCredentialScoped(t *testing.T) {
	tmp := t.TempDir()
	workdir := filepath.Join(tmp, "workdir")
	if err := os.MkdirAll(workdir, 0o750); err != nil {
		t.Fatal(err)
	}
	configRoot := filepath.Join(tmp, "config")
	writeLive(t, configRoot, "configuration.yaml", 4)

	token := "ghp_IMPORTTOKEN789"
	opts := makeOpts("https://git.example.invalid/repo.git")
	opts.GitUsername = "agent"
	opts.GitToken = token

	gs := New(opts, workdir)
	// "diff --cached --quiet" must exit non-zero to mean "something is
	// staged", or Import refuses the empty commit and never pushes.
	runner := &scriptedGitRunner{
		failOnArgs: []string{"diff", "--cached", "--quiet"},
		failResult: RunResult{ExitCode: 1},
	}
	gs.Runner = runner

	if _, err := gs.Import(context.Background(), configRoot, generousLimits(), fixedImportTime); err != nil {
		t.Fatalf("Import: %v", err)
	}

	var push recordedRun
	for _, c := range runner.calls {
		if len(c.args) >= 2 && c.args[1] == "push" {
			push = c
		}
		for _, a := range c.args {
			if strings.Contains(a, token) {
				t.Fatalf("token leaked into argv: %v", c.args)
			}
		}
	}
	wantArgs := []string{"git", "push", opts.RepoURL, fixedImportBranch + ":refs/heads/main"}
	if !equalArgs(push.args, wantArgs) {
		t.Fatalf("push argv = %v, want exactly %v (no --force, no --force-with-lease, no + prefix)", push.args, wantArgs)
	}

	var hasCredential bool
	for _, kv := range push.env {
		if strings.HasPrefix(kv, "GIT_CONFIG_VALUE_0=") {
			hasCredential = true
		}
	}
	if !hasCredential {
		t.Error("push env missing the credential override")
	}
	for _, c := range runner.calls {
		if len(c.args) < 2 {
			continue
		}
		switch c.args[1] {
		case "push", "fetch", "ls-remote":
			continue
		}
		for _, kv := range c.env {
			if strings.HasPrefix(kv, "GIT_CONFIG_COUNT") {
				t.Errorf("non-network call %v carries credential env", c.args)
			}
		}
	}
}

func TestImportRefusesMalformedBranchName(t *testing.T) {
	for _, branch := range []string{"", "--force", "a:b", "has space"} {
		tmp := t.TempDir()
		workdir := filepath.Join(tmp, "workdir")
		if err := os.MkdirAll(workdir, 0o750); err != nil {
			t.Fatal(err)
		}
		opts := makeOpts("https://git.example.invalid/repo.git")
		opts.Branch = branch
		gs := New(opts, workdir)
		fr := &fakeRunner{}
		gs.Runner = fr

		if _, err := gs.Import(context.Background(), t.TempDir(), generousLimits(), fixedImportTime); err == nil {
			t.Errorf("branch %q: Import succeeded, want a refusal", branch)
		}
		for _, c := range fr.calls {
			if len(c.args) >= 2 && (c.args[1] == "push" || c.args[1] == "commit") {
				t.Errorf("branch %q: reached %v before validating the branch name", branch, c.args)
			}
		}
	}
}

// A large import used to do all its work and then die at the 60-second
// default right at the push.
func TestImportUsesExtendedTimeoutForAddCommitPush(t *testing.T) {
	tmp := t.TempDir()
	workdir := filepath.Join(tmp, "workdir")
	if err := os.MkdirAll(workdir, 0o750); err != nil {
		t.Fatal(err)
	}
	configRoot := filepath.Join(tmp, "config")
	writeLive(t, configRoot, "configuration.yaml", 4)

	gs := New(makeOpts("https://git.example.invalid/repo.git"), workdir)
	runner := &deadlineRunner{
		scriptedGitRunner: scriptedGitRunner{
			failOnArgs: []string{"diff", "--cached", "--quiet"},
			failResult: RunResult{ExitCode: 1},
		},
	}
	gs.Runner = runner

	if _, err := gs.Import(context.Background(), configRoot, generousLimits(), fixedImportTime); err != nil {
		t.Fatalf("Import: %v", err)
	}

	for _, sub := range []string{"add", "commit", "push"} {
		budget, ok := runner.budgets[sub]
		if !ok {
			t.Errorf("no %q call recorded", sub)
			continue
		}
		if budget <= DefaultGitTimeout {
			t.Errorf("%q ran with a %s budget, want more than the %s default", sub, budget, DefaultGitTimeout)
		}
	}
}

// deadlineRunner records how much time each git subcommand was given.
type deadlineRunner struct {
	scriptedGitRunner
	budgets map[string]time.Duration
}

func (r *deadlineRunner) Run(ctx context.Context, dir string, env []string, args ...string) (RunResult, error) {
	if r.budgets == nil {
		r.budgets = map[string]time.Duration{}
	}
	if deadline, ok := ctx.Deadline(); ok && len(args) >= 2 {
		// Rounded up: the deadline was set a hair before this runs.
		r.budgets[args[1]] = time.Until(deadline).Round(time.Second)
	}
	return r.scriptedGitRunner.Run(ctx, dir, env, args...)
}

// localBranches lists the branch refs in workdir's clone.
func localBranches(t *testing.T, workdir string) []string {
	t.Helper()
	cmd := exec.Command("git", "-C", workdir, "branch", "--format=%(refname:short)") // #nosec G204 -- fixed "git" binary; workdir is a test temp dir
	// Pinned the way production's gitEnv does: git localizes this output,
	// and a localized detached-HEAD entry flakes across machines.
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	raw, err := cmd.Output()
	if err != nil {
		t.Fatalf("git branch in %s: %v", workdir, err)
	}
	out := string(raw)
	var names []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line != "" {
			names = append(names, line)
		}
	}
	return names
}

// A live tree churns, so a scanned file can be gone by the time it is
// copied; the scan's count would then describe a truncated snapshot.
func TestImportReportsOnlyWhatItActuallyStaged(t *testing.T) {
	tmp := t.TempDir()
	bare, work := makeRemote(t, tmp, "repo")
	commitFile(t, work, "README.md", "readme\n", "init")

	configRoot := filepath.Join(tmp, "config")
	writeLive(t, configRoot, "stays.yaml", 6)
	writeLive(t, configRoot, "vanishes.yaml", 6)

	gs := importGitSync(t, bare, filepath.Join(tmp, "workdir"))
	ir := &interceptRunner{
		inner:   execx.CommandRunner{},
		trigger: []string{"clean"}, // after the scan, before staging
		before: func() {
			if err := os.Remove(filepath.Join(configRoot, "vanishes.yaml")); err != nil {
				t.Errorf("removing the live file mid-import: %v", err)
			}
		},
	}
	gs.Runner = ir

	res, err := gs.Import(context.Background(), configRoot, generousLimits(), fixedImportTime)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if !ir.fired {
		t.Fatal("the interceptor never fired; the file never vanished and this test proved nothing")
	}
	if res.Files != 1 {
		t.Errorf("Files = %d, want 1 - the count must reflect what was staged, not what the scan found", res.Files)
	}
	if _, ok := showAtRef(t, bare, "main", "stays.yaml"); !ok {
		t.Error("the surviving file did not land")
	}
	if _, ok := showAtRef(t, bare, "main", "vanishes.yaml"); ok {
		t.Error("a file that no longer existed was somehow committed")
	}
}

// "exit 2" means no such branch yet; "exit 128" means the remote was
// unreachable. Conflating them sends an auth failure down the seeding path.
func TestImportPropagatesLsRemoteFailure(t *testing.T) {
	tmp := t.TempDir()
	configRoot := filepath.Join(tmp, "config")
	writeLive(t, configRoot, "configuration.yaml", 4)

	workdir := filepath.Join(tmp, "workdir")
	if err := os.MkdirAll(workdir, 0o750); err != nil {
		t.Fatal(err)
	}
	gs := New(makeOpts("https://git.example.invalid/repo.git"), workdir)
	runner := &scriptedGitRunner{
		failOnArgs: []string{"--heads", "https://git.example.invalid/repo.git", "main"},
		failResult: RunResult{ExitCode: 128, Stderr: "fatal: Authentication failed\n"},
	}
	gs.Runner = runner

	_, err := gs.Import(context.Background(), configRoot, generousLimits(), fixedImportTime)
	if err == nil {
		t.Fatal("Import succeeded despite an unreachable remote")
	}
	if !strings.Contains(err.Error(), "Authentication failed") {
		t.Errorf("error = %v, want git's own reason surfaced", err)
	}
	for _, c := range runner.calls {
		if len(c.args) >= 2 && (c.args[1] == "checkout" || c.args[1] == "commit" || c.args[1] == "push") {
			t.Fatalf("reached %v after ls-remote failed; an unreachable remote must not start a seed", c.args)
		}
	}
}

// The guard that stops an empty commit reaching the user's branch.
func TestImportRefusesWhenEverythingScannedIsGitignored(t *testing.T) {
	tmp := t.TempDir()
	bare, work := makeRemote(t, tmp, "repo")
	commitFile(t, work, ".gitignore", "*.yaml\n", "ignore all yaml")

	configRoot := filepath.Join(tmp, "config")
	writeLive(t, configRoot, "configuration.yaml", 4)
	writeLive(t, configRoot, "packages/lights.yaml", 4)

	gs := importGitSync(t, bare, filepath.Join(tmp, "workdir"))
	before := gs.CurrentSHA(context.Background())

	_, err := gs.Import(context.Background(), configRoot, generousLimits(), fixedImportTime)
	if err == nil || !strings.Contains(err.Error(), "nothing to import") {
		t.Fatalf("Import error = %v, want a nothing-to-import refusal", err)
	}
	if after := gs.CurrentSHA(context.Background()); after != before {
		t.Errorf("workdir left at %s, want %s", after, before)
	}
	if branches := localBranches(t, gs.Workdir); contains(branches, fixedImportBranch) {
		t.Errorf("local branches = %v, want the throwaway branch cleaned up", branches)
	}
}

// The UX premise is "review that commit in your repository", so the commit
// has to be identifiable.
func TestImportCommitIsAttributedToTheAgent(t *testing.T) {
	tmp := t.TempDir()
	bare, work := makeRemote(t, tmp, "repo")
	commitFile(t, work, "README.md", "readme\n", "init")

	configRoot := filepath.Join(tmp, "config")
	writeLive(t, configRoot, "configuration.yaml", 4)

	gs := importGitSync(t, bare, filepath.Join(tmp, "workdir"))
	if _, err := gs.Import(context.Background(), configRoot, generousLimits(), fixedImportTime); err != nil {
		t.Fatalf("Import: %v", err)
	}

	out := gitShowFormat(t, bare, "main", "%s|%an|%ae")
	want := ImportCommitMessage + "|" + commitAuthorName + "|" + commitAuthorEmail
	if out != want {
		t.Errorf("commit subject|author = %q, want %q", out, want)
	}
}

// gitShowFormat returns one --format field set from the tip of ref.
func gitShowFormat(t *testing.T, bare, ref, format string) string {
	t.Helper()
	cmd := exec.Command("git", "--git-dir="+bare, "show", "-s", "--format="+format, ref) // #nosec G204 -- fixed "git" binary; test-controlled args
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git show: %v", err)
	}
	return strings.TrimSpace(string(out))
}
