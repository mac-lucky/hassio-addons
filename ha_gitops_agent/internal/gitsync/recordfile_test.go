package gitsync

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/execx"
)

const recordPath = "gitops/addon-versions.yaml"

const recordMessage = "versions: record installed add-on versions"

// recordFixture is the state every test here starts from: a bare remote
// with one commit on main, and a GitSync cloned and checked out at it,
// the way a reconcile leaves the workdir.
type recordFixture struct {
	gs   *GitSync
	bare string
	work string
	sha  string
}

func newRecordFixture(t *testing.T) recordFixture {
	t.Helper()
	tmp := t.TempDir()
	bare, work := makeRemote(t, tmp, "remote")
	commitFile(t, work, "automations.yaml", "- id: demo\n", "commit")

	gs := New(makeOpts("file://"+bare), filepath.Join(tmp, "clone"))
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
	return recordFixture{gs: gs, bare: bare, work: work, sha: sha}
}

// remoteTip is the commit main currently points at in the bare remote,
// read without going through GitSync at all.
func remoteTip(t *testing.T, bare string) string {
	t.Helper()
	result, err := execx.CommandRunner{}.Run(context.Background(), bare, os.Environ(), "git", "--git-dir="+bare, "rev-parse", "main")
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("git rev-parse main: %v (exit %d) %s", err, result.ExitCode, result.Stderr)
	}
	return strings.TrimSpace(result.Stdout)
}

func TestRecordFileCommitsToTheTrackedBranchAndRestoresTheCheckout(t *testing.T) {
	f := newRecordFixture(t)
	ctx := context.Background()
	content := []byte("core_samba:\n  name: Samba share\n  version: 12.3.2\n")

	committed, err := f.gs.RecordFile(ctx, recordPath, content, recordMessage)
	if err != nil {
		t.Fatalf("RecordFile: %v", err)
	}
	if !committed {
		t.Fatal("committed = false, want true on the first record")
	}

	pushed, ok := showAtRef(t, f.bare, "main", recordPath)
	if !ok {
		t.Fatalf("%s missing from main after RecordFile", recordPath)
	}
	if pushed != string(content) {
		t.Errorf("recorded content = %q, want %q", pushed, content)
	}

	// Unlike CommitBack, the record lands on the tracked branch itself:
	// no throwaway branch is left behind, remote or local.
	for _, name := range listRemoteBranches(t, f.bare) {
		if name != "main" {
			t.Errorf("remote grew an extra branch %q, want main only", name)
		}
	}
	if got := f.gs.CurrentSHA(ctx); got != f.sha {
		t.Errorf("CurrentSHA() after RecordFile = %q, want %q (detached checkout must be restored)", got, f.sha)
	}
	if _, err := os.Stat(filepath.Join(f.gs.Workdir, recordPath)); !os.IsNotExist(err) {
		t.Errorf("%s still in the worktree after RecordFile, want the restored checkout to be clean", recordPath)
	}
}

// The no-op path is what makes RecordFile safe to call every cycle.
func TestRecordFileIsANoOpWhenTheCommittedBlobAlreadyMatches(t *testing.T) {
	f := newRecordFixture(t)
	ctx := context.Background()
	content := []byte("core_samba:\n  name: Samba share\n  version: 12.3.2\n")

	if _, err := f.gs.RecordFile(ctx, recordPath, content, recordMessage); err != nil {
		t.Fatalf("first RecordFile: %v", err)
	}
	tipAfterFirst := remoteTip(t, f.bare)

	committed, err := f.gs.RecordFile(ctx, recordPath, content, recordMessage)
	if err != nil {
		t.Fatalf("second RecordFile: %v", err)
	}
	if committed {
		t.Error("committed = true on an unchanged record, want false")
	}
	if got := remoteTip(t, f.bare); got != tipAfterFirst {
		t.Errorf("main moved to %q on a no-op record, want it left at %q", got, tipAfterFirst)
	}
	if got := f.gs.CurrentSHA(ctx); got != f.sha {
		t.Errorf("CurrentSHA() after a no-op record = %q, want the checkout untouched at %q", got, f.sha)
	}
}

func TestRecordFileCommitsAgainWhenTheContentChanges(t *testing.T) {
	f := newRecordFixture(t)
	ctx := context.Background()

	if _, err := f.gs.RecordFile(ctx, recordPath, []byte("core_samba:\n  version: 12.3.2\n"), recordMessage); err != nil {
		t.Fatalf("first RecordFile: %v", err)
	}
	updated := []byte("core_samba:\n  version: 12.4.0\n")
	committed, err := f.gs.RecordFile(ctx, recordPath, updated, recordMessage)
	if err != nil {
		t.Fatalf("second RecordFile: %v", err)
	}
	if !committed {
		t.Error("committed = false on a changed record, want true")
	}
	if got, _ := showAtRef(t, f.bare, "main", recordPath); got != string(updated) {
		t.Errorf("recorded content = %q, want %q", got, updated)
	}
	// The record commits one path; nothing else on the branch is rewritten.
	if got, ok := showAtRef(t, f.bare, "main", "automations.yaml"); !ok || got != "- id: demo\n" {
		t.Errorf("automations.yaml = %q (ok=%v), want it untouched", got, ok)
	}
}

// restoreDetachedCheckout is best-effort, so an earlier operation in the
// same cycle can leave staged changes the record must not publish.
func TestRecordFileCommitsOnlyItsOwnPathFromADirtyIndex(t *testing.T) {
	f := newRecordFixture(t)
	ctx := context.Background()

	// The leftover: a staged change unrelated to the record.
	leaked := "- id: leaked-from-live\n"
	if err := os.WriteFile(filepath.Join(f.gs.Workdir, "automations.yaml"), []byte(leaked), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitHelper(t, f.gs.Workdir, "add", "--", "automations.yaml")

	content := []byte("core_samba:\n  version: 12.3.2\n")
	committed, err := f.gs.RecordFile(ctx, recordPath, content, recordMessage)
	if err != nil {
		t.Fatalf("RecordFile: %v", err)
	}
	if !committed {
		t.Fatal("committed = false, want true")
	}

	if got, _ := showAtRef(t, f.bare, "main", recordPath); got != string(content) {
		t.Errorf("recorded content = %q, want %q", got, content)
	}
	if got, _ := showAtRef(t, f.bare, "main", "automations.yaml"); got == leaked {
		t.Error("the staged leftover was pushed to the tracked branch, want it left out of the commit")
	}
	if got := changedPathsAtRef(t, f.bare, "main"); len(got) != 1 || got[0] != recordPath {
		t.Errorf("commit touched %v, want only [%s]", got, recordPath)
	}
}

// changedPathsAtRef lists what the commit at ref changed against its
// parent, read straight from the bare remote.
func changedPathsAtRef(t *testing.T, bare, ref string) []string {
	t.Helper()
	cmd := exec.Command("git", "--git-dir="+bare, "diff-tree", "--no-commit-id", "--name-only", "-r", ref) // #nosec G204 -- see showAtRef
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git diff-tree: %v", err)
	}
	var paths []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line != "" {
			paths = append(paths, line)
		}
	}
	return paths
}

// racingRunner fires a side effect just before each of the first pushes,
// moving the remote between RecordFile's fetch and push without timing games.
type racingRunner struct {
	inner  Runner
	races  int
	onPush func()
}

func (r *racingRunner) Run(ctx context.Context, dir string, env []string, args ...string) (RunResult, error) {
	if len(args) >= 2 && args[1] == "push" && r.races > 0 {
		r.races--
		r.onPush()
	}
	return r.inner.Run(ctx, dir, env, args...)
}

func TestRecordFileRetriesOnceWhenTheBranchMovedUnderIt(t *testing.T) {
	f := newRecordFixture(t)
	ctx := context.Background()

	competing := 0
	f.gs.Runner = &racingRunner{inner: execx.CommandRunner{}, races: 1, onPush: func() {
		competing++
		commitFile(t, f.work, "scripts.yaml", "- id: from-the-user\n", "user commit")
	}}

	content := []byte("core_samba:\n  version: 12.3.2\n")
	committed, err := f.gs.RecordFile(ctx, recordPath, content, recordMessage)
	if err != nil {
		t.Fatalf("RecordFile: %v", err)
	}
	if !committed {
		t.Error("committed = false, want true - the retry should have landed the record")
	}
	if competing != 1 {
		t.Fatalf("competing pushes = %d, want 1", competing)
	}
	if got, _ := showAtRef(t, f.bare, "main", recordPath); got != string(content) {
		t.Errorf("recorded content = %q, want %q", got, content)
	}
	// The point of the fast-forward-only push: the winning commit survives.
	if _, ok := showAtRef(t, f.bare, "main", "scripts.yaml"); !ok {
		t.Error("the competing user commit is gone from main, want it preserved")
	}
}

func TestRecordFileGivesUpAfterASecondRejection(t *testing.T) {
	f := newRecordFixture(t)
	ctx := context.Background()

	competing := 0
	f.gs.Runner = &racingRunner{inner: execx.CommandRunner{}, races: 2, onPush: func() {
		competing++
		commitFile(t, f.work, "scripts.yaml", strings.Repeat("- id: user\n", competing), "user commit")
	}}

	committed, err := f.gs.RecordFile(ctx, recordPath, []byte("core_samba:\n  version: 12.3.2\n"), recordMessage)
	if committed {
		t.Error("committed = true, want false when both pushes were rejected")
	}
	if err == nil || !strings.Contains(err.Error(), "moved on the remote twice") {
		t.Fatalf("error = %v, want it to report losing the race twice", err)
	}
	if competing != 2 {
		t.Errorf("competing pushes = %d, want 2 (one per attempt)", competing)
	}
	if _, ok := showAtRef(t, f.bare, "main", recordPath); ok {
		t.Errorf("%s landed on main after two rejections, want nothing recorded", recordPath)
	}
	if got := f.gs.CurrentSHA(ctx); got != f.sha {
		t.Errorf("CurrentSHA() after a failed record = %q, want the checkout restored to %q", got, f.sha)
	}
}

func TestRecordFileRefusesUnsafePaths(t *testing.T) {
	tests := []struct {
		name    string
		relPath string
		wantErr string
	}{
		{name: "secret-shaped", relPath: "secrets.yaml", wantErr: "secret-shaped"},
		{name: "secret-shaped at depth", relPath: "gitops/id_rsa", wantErr: "secret-shaped"},
		{name: "escapes the worktree", relPath: "../outside.yaml", wantErr: "outside root"},
		{name: "absolute", relPath: "/etc/passwd", wantErr: "absolute path"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newRecordFixture(t)
			committed, err := f.gs.RecordFile(context.Background(), tt.relPath, []byte("x\n"), recordMessage)
			if committed {
				t.Error("committed = true, want false")
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want it to mention %q", err, tt.wantErr)
			}
			if got := remoteTip(t, f.bare); got != f.sha {
				t.Errorf("main moved to %q on a refused record, want %q", got, f.sha)
			}
		})
	}
}

// Refused by guardWriteBranch before anything is fetched or written.
func TestRecordFileRefusesAMalformedBranch(t *testing.T) {
	f := newRecordFixture(t)
	f.gs.Opts.Branch = "--mirror"

	_, err := f.gs.RecordFile(context.Background(), recordPath, []byte("x\n"), recordMessage)
	if err == nil || !strings.Contains(err.Error(), "starting with '-'") {
		t.Fatalf("error = %v, want the leading-dash refusal", err)
	}
}

// guardWriteBranch is shared by Import and RecordFile, so each caller's
// wording is pinned verbatim. It runs before the first git subprocess.
func TestWriteBranchRefusalsAreWordedPerOperation(t *testing.T) {
	call := map[string]func(*GitSync) error{
		"import": func(gs *GitSync) error {
			_, err := gs.Import(context.Background(), t.TempDir(), DefaultImportLimits(), fixedDriftTime)
			return err
		},
		"record": func(gs *GitSync) error {
			_, err := gs.RecordFile(context.Background(), recordPath, []byte("x\n"), recordMessage)
			return err
		},
	}
	tests := []struct {
		op     string
		branch string
		want   string
	}{
		{op: "import", branch: "", want: "gitsync: import: no branch configured to import onto"},
		{op: "record", branch: "", want: "gitsync: record: no branch configured to record onto"},
		{
			op: "import", branch: "--mirror",
			want: "gitsync: import: refusing to use a branch name starting with '-': --mirror",
		},
		{
			op: "record", branch: "--mirror",
			want: "gitsync: record: refusing to use a branch name starting with '-': --mirror",
		},
	}

	for _, tt := range tests {
		t.Run(tt.op+" branch="+tt.branch, func(t *testing.T) {
			opts := makeOpts("file:///unused")
			opts.Branch = tt.branch
			err := call[tt.op](New(opts, filepath.Join(t.TempDir(), "clone")))
			if err == nil || err.Error() != tt.want {
				t.Errorf("error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestRecordFilePushUsesCredentialEnvNeverArgv(t *testing.T) {
	token := "ghp_TESTTOKEN123"
	opts := makeOpts("https://git.example.invalid/repo.git")
	opts.GitUsername = "agent"
	opts.GitToken = token

	gs := New(opts, filepath.Join(t.TempDir(), "clone"))
	fr := &fakeRunner{}
	gs.Runner = fr

	if _, err := gs.RecordFile(context.Background(), recordPath, []byte("x\n"), recordMessage); err != nil {
		t.Fatalf("RecordFile: %v", err)
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
	want := []string{"git", "push", opts.RepoURL, recordBranch + ":refs/heads/main"}
	if !equalArgs(pushCall.args, want) {
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
	}
}
