package recon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/differ"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/gitsync"
)

// --- driftSetHash ----------------------------------------------------------

func TestDriftSetHashStableForSameInput(t *testing.T) {
	a := []differ.Change{{Path: "automations.yaml", Kind: "update"}, {Path: "scripts.yaml", Kind: "add"}}
	b := []differ.Change{{Path: "automations.yaml", Kind: "update"}, {Path: "scripts.yaml", Kind: "add"}}
	if driftSetHash(a) != driftSetHash(b) {
		t.Error("driftSetHash differs for identical input")
	}
}

func TestDriftSetHashChangesWithPathKindOrEmptiness(t *testing.T) {
	base := driftSetHash([]differ.Change{{Path: "automations.yaml", Kind: "update"}})
	if got := driftSetHash([]differ.Change{{Path: "automations.yaml", Kind: "delete"}}); got == base {
		t.Error("hash unchanged when kind changed")
	}
	if got := driftSetHash([]differ.Change{{Path: "scripts.yaml", Kind: "update"}}); got == base {
		t.Error("hash unchanged when path changed")
	}
	if got := driftSetHash(nil); got == base {
		t.Error("hash unchanged for empty set")
	}
}

// --- CommitDriftBack (manual button path) ----------------------------------

func TestCommitDriftBackRefusesWithNoPendingFileDrift(t *testing.T) {
	fakes := newReconcilerFakes()
	opts := baseOpts()
	opts.CommitBack = true
	r := fakes.reconciler(opts)
	r.ReconcileNow(context.Background()) // in sync, no pending changes

	branch, err := r.CommitDriftBack(context.Background())

	if branch != "" || !errors.Is(err, errNoFileDrift) {
		t.Fatalf("CommitDriftBack() = (%q, %v), want (\"\", errNoFileDrift)", branch, err)
	}
	if len(fakes.git.commitBackCalls) != 0 {
		t.Errorf("commit_back_calls = %+v, want none", fakes.git.commitBackCalls)
	}
	// The button re-renders the same page, so "nothing to commit" and
	// "committed" look identical without this entry.
	if events := r.Status().Events; !hasEventContaining(events, "commit-back skipped: no pending file drift") {
		t.Errorf("events = %+v, want a skipped-commit-back entry", events)
	}
}

func TestCommitDriftBackPushesBranchAndPersistsState(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.differ.changes = []differ.Change{{Path: "automations.yaml", Kind: "update", DiffText: "+alias: Demo\n"}}
	fakes.git.sha = "deadbeef00112233445566778899aabbccddeef0"
	fakes.git.commitBackBranch = "gitops/drift-20260802T120000Z"
	opts := baseOpts()
	opts.CommitBack = true
	// DryRun off isolates the manual button path: with it on, the
	// automatic policy would fire too and double commitBackCalls.
	opts.DryRun = false
	r := fakes.reconciler(opts)
	r.ReconcileNow(context.Background())

	branch, err := r.CommitDriftBack(context.Background())
	if err != nil {
		t.Fatalf("CommitDriftBack() error = %v", err)
	}
	if branch != "gitops/drift-20260802T120000Z" {
		t.Errorf("branch = %q", branch)
	}
	if len(fakes.git.commitBackCalls) != 1 {
		t.Fatalf("commit_back_calls = %+v, want 1", fakes.git.commitBackCalls)
	}
	call := fakes.git.commitBackCalls[0]
	if want := []gitsync.DriftFile{{Path: "automations.yaml", Kind: "update"}}; len(call.files) != 1 || call.files[0] != want[0] {
		t.Errorf("files = %v, want %v", call.files, want)
	}
	if call.configRoot != ConfigRoot {
		t.Errorf("config_root = %q, want %q", call.configRoot, ConfigRoot)
	}
	if call.baseSHA != fakes.git.sha {
		t.Errorf("base_sha = %q, want %q", call.baseSHA, fakes.git.sha)
	}

	if fakes.applier.state.LastDriftBranch != branch {
		t.Errorf("state.LastDriftBranch = %q, want %q", fakes.applier.state.LastDriftBranch, branch)
	}
	if fakes.applier.state.LastDriftBackHash == "" {
		t.Error("state.LastDriftBackHash not set")
	}

	status := r.Status()
	if status.LastDriftBranch != branch {
		t.Errorf("status.LastDriftBranch = %q, want %q", status.LastDriftBranch, branch)
	}
}

func TestCommitDriftBackRefusesWhenBusy(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.differ.changes = []differ.Change{{Path: "automations.yaml", Kind: "update"}}
	opts := baseOpts()
	opts.CommitBack = true
	opts.DryRun = false // see TestCommitDriftBackPushesBranchAndPersistsState's own comment
	r := fakes.reconciler(opts)
	r.ReconcileNow(context.Background())
	if !r.opLock.TryLock() {
		t.Fatal("could not seize opLock for the test")
	}
	defer r.opLock.Unlock()

	_, err := r.CommitDriftBack(context.Background())

	if !errors.Is(err, errBusy) {
		t.Errorf("err = %v, want errBusy", err)
	}
	if len(fakes.git.commitBackCalls) != 0 {
		t.Errorf("commit_back_calls = %+v, want none", fakes.git.commitBackCalls)
	}
	if events := r.Status().Events; !hasEventContaining(events, "commit-back skipped: another operation") {
		t.Errorf("events = %+v, want a skipped-commit-back entry", events)
	}
}

func TestCommitDriftBackFailureIsLoggedButDoesNotSetLastErrorOrState(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.differ.changes = []differ.Change{{Path: "automations.yaml", Kind: "update"}}
	fakes.git.commitBackErr = errors.New("push failed: authentication required")
	opts := baseOpts()
	opts.CommitBack = true
	opts.DryRun = false // see TestCommitDriftBackPushesBranchAndPersistsState's own comment
	r := fakes.reconciler(opts)
	r.ReconcileNow(context.Background())
	stateBefore := r.Status().State

	_, err := r.CommitDriftBack(context.Background())

	if err == nil || !strings.Contains(err.Error(), "authentication required") {
		t.Errorf("err = %v", err)
	}
	status := r.Status()
	if status.State != stateBefore {
		t.Errorf("state = %q, want unchanged %q", status.State, stateBefore)
	}
	if status.LastError != "" {
		t.Errorf("last_error = %q, want empty (commit-back failure must not overwrite it)", status.LastError)
	}
	if !hasEventContaining(status.Events, "commit-back failed") {
		t.Errorf("events = %+v, want a commit-back failure entry", status.Events)
	}
}

func TestCommitDriftBackRefusedWhenOptionOff(t *testing.T) {
	// The gate lives in CommitDriftBack, not only in whether the UI
	// renders the button: POST /commitback can be hit directly.
	fakes := newReconcilerFakes()
	fakes.differ.changes = []differ.Change{{Path: "automations.yaml", Kind: "update"}}
	opts := baseOpts()
	opts.CommitBack = false
	r := fakes.reconciler(opts)
	r.ReconcileNow(context.Background())

	branch, err := r.CommitDriftBack(context.Background())

	if branch != "" || !errors.Is(err, errCommitBackDisabled) {
		t.Fatalf("CommitDriftBack() = (%q, %v), want (\"\", errCommitBackDisabled)", branch, err)
	}
	if len(fakes.git.commitBackCalls) != 0 {
		t.Errorf("commit_back_calls = %+v, want none", fakes.git.commitBackCalls)
	}
	if events := r.Status().Events; !hasEventContaining(events, "commit-back skipped: commit_back is disabled") {
		t.Errorf("events = %+v, want a skipped-commit-back entry", events)
	}
}

// --- automatic commit-back policy -------------------------------------------

func TestReconcileNowAutoCommitsBackWhenEnabledAndDryRunWithFileDrift(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.differ.changes = []differ.Change{{Path: "automations.yaml", Kind: "update"}}
	opts := baseOpts()
	opts.CommitBack = true
	opts.DryRun = true
	r := fakes.reconciler(opts)

	r.ReconcileNow(context.Background())

	if len(fakes.git.commitBackCalls) != 1 {
		t.Fatalf("commit_back_calls = %+v, want 1", fakes.git.commitBackCalls)
	}
	if fakes.applier.state.LastDriftBranch == "" {
		t.Error("state.LastDriftBranch not persisted by the automatic path")
	}
}

func TestReconcileNowAutoCommitBackSkipsUnchangedDriftSet(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.differ.changes = []differ.Change{{Path: "automations.yaml", Kind: "update"}}
	opts := baseOpts()
	opts.CommitBack = true
	opts.DryRun = true
	r := fakes.reconciler(opts)

	r.ReconcileNow(context.Background())
	r.ReconcileNow(context.Background())
	r.ReconcileNow(context.Background())

	if len(fakes.git.commitBackCalls) != 1 {
		t.Errorf("commit_back_calls = %d, want 1 (same drift set must not re-trigger)", len(fakes.git.commitBackCalls))
	}
}

func TestReconcileNowAutoCommitBackRunsAgainWhenDriftSetChanges(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.differ.changes = []differ.Change{{Path: "automations.yaml", Kind: "update"}}
	opts := baseOpts()
	opts.CommitBack = true
	opts.DryRun = true
	r := fakes.reconciler(opts)
	r.ReconcileNow(context.Background())

	fakes.differ.changes = []differ.Change{
		{Path: "automations.yaml", Kind: "update"},
		{Path: "scripts.yaml", Kind: "add"},
	}
	r.ReconcileNow(context.Background())

	if len(fakes.git.commitBackCalls) != 2 {
		t.Errorf("commit_back_calls = %d, want 2 (drift set shape changed)", len(fakes.git.commitBackCalls))
	}
}

func TestReconcileNowSkipsAutoCommitBackWhenOptionOff(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.differ.changes = []differ.Change{{Path: "automations.yaml", Kind: "update"}}
	opts := baseOpts()
	opts.CommitBack = false
	opts.DryRun = true
	r := fakes.reconciler(opts)

	r.ReconcileNow(context.Background())

	if len(fakes.git.commitBackCalls) != 0 {
		t.Errorf("commit_back_calls = %+v, want none", fakes.git.commitBackCalls)
	}
}

func TestReconcileNowSkipsAutoCommitBackWhenDryRunOff(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.differ.changes = []differ.Change{{Path: "automations.yaml", Kind: "update"}}
	opts := baseOpts()
	opts.CommitBack = true
	opts.DryRun = false
	r := fakes.reconciler(opts)

	r.ReconcileNow(context.Background())

	if len(fakes.git.commitBackCalls) != 0 {
		t.Errorf("commit_back_calls = %+v, want none (a real apply already reconciles the drift)", fakes.git.commitBackCalls)
	}
}

func TestReconcileNowSkipsAutoCommitBackWhenNoFileDrift(t *testing.T) {
	fakes := newReconcilerFakes()
	opts := baseOpts()
	opts.CommitBack = true
	opts.DryRun = true
	r := fakes.reconciler(opts)

	r.ReconcileNow(context.Background())

	if len(fakes.git.commitBackCalls) != 0 {
		t.Errorf("commit_back_calls = %+v, want none", fakes.git.commitBackCalls)
	}
}

// --- internal guard: no fetched tip yet -------------------------------------

func TestCommitDriftBackRefusesWithoutAFetchedTip(t *testing.T) {
	fakes := newReconcilerFakes()
	r := fakes.reconciler(baseOpts())
	// No ReconcileNow, so r.lastSHA stays "" as it is at startup.
	changes := []differ.Change{{Path: "automations.yaml", Kind: "update"}}

	_, err := r.commitDriftBack(context.Background(), changes)

	if !errors.Is(err, errNoFetchedTip) {
		t.Errorf("err = %v, want errNoFetchedTip", err)
	}
	if events := r.Status().Events; !hasEventContaining(events, "commit-back skipped: no fetched tip") {
		t.Errorf("events = %+v, want a skipped-commit-back entry", events)
	}
}

// The automatic policy runs every poll, so a refusal that logs would fill
// the log. Repeated cycles, because a single cycle passes either way.
func TestAutoCommitBackNeverWritesARefusalEventOnRepeatedCycles(t *testing.T) {
	for _, tc := range []struct {
		name    string
		changes []differ.Change
	}{
		{"nothing pending at all", nil},
		{"the same drift standing unresolved", []differ.Change{{Path: "automations.yaml", Kind: "update"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fakes := newReconcilerFakes()
			fakes.differ.changes = tc.changes
			opts := baseOpts()
			opts.CommitBack = true
			opts.DryRun = true
			r := fakes.reconciler(opts)

			r.ReconcileNow(context.Background())
			r.ReconcileNow(context.Background())
			r.ReconcileNow(context.Background())

			if events := r.Status().Events; hasEventContaining(events, "commit-back skipped") {
				t.Errorf("events = %+v, want no refusal entry from the automatic policy", events)
			}
			// One push for a real drift set, none for no drift.
			wantPushes := 0
			if len(tc.changes) > 0 {
				wantPushes = 1
			}
			if got := len(fakes.git.commitBackCalls); got != wantPushes {
				t.Errorf("commit_back_calls = %d, want %d", got, wantPushes)
			}
		})
	}
}

// Held-back files are named on the feed after a commit-back that pushed
// the rest.
func TestCommitDriftBackReportsHeldBackFiles(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.differ.changes = []differ.Change{{Path: "a.yaml", Kind: "update"}, {Path: "b.yaml", Kind: "update"}}
	fakes.git.commitBackHeldBack = []gitsync.HeldBack{{Path: "b.yaml", Keys: []string{"token"}}}
	opts := baseOpts()
	opts.CommitBack = true
	opts.DryRun = false
	r := fakes.reconciler(opts)
	r.ReconcileNow(context.Background())

	if _, err := r.CommitDriftBack(context.Background()); err != nil {
		t.Fatalf("CommitDriftBack() error = %v", err)
	}
	if !hasEventContaining(r.Status().Events, "commit-back left out 1 file(s) holding a literal value under a secret-shaped key: b.yaml (token)") {
		t.Errorf("events = %+v, want the held-back file named", r.Status().Events)
	}
}

// When every file is held back the same drift set fails the same way
// until someone edits it. The automatic half keeps trying - the edit that
// fixes it changes content, not the drift set's paths - but says so once.
func TestAutoCommitBackReportsAnAllHeldBackDriftSetOnce(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.differ.changes = []differ.Change{{Path: "b.yaml", Kind: "update"}}
	fakes.git.commitBackErr = fmt.Errorf("gitsync: commit-back: nothing to commit: %w", gitsync.ErrAllHeldBack)
	fakes.git.commitBackHeldBack = []gitsync.HeldBack{{Path: "b.yaml", Keys: []string{"token"}}}
	opts := baseOpts()
	opts.CommitBack = true
	r := fakes.reconciler(opts)

	r.ReconcileNow(context.Background())
	r.ReconcileNow(context.Background())

	if n := countEventsContaining(r.Status().Events, "commit-back skipped"); n != 1 {
		t.Errorf("held-back commit-back logged %d times over two identical cycles, want once", n)
	}
	if fakes.applier.state.LastDriftBackHash != "" {
		t.Error("a drift set that pushed nothing was recorded as committed back")
	}

	// The value is replaced with a reference: same paths, and now it pushes.
	fakes.git.commitBackErr = nil
	fakes.git.commitBackHeldBack = nil
	r.ReconcileNow(context.Background())
	if fakes.applier.state.LastDriftBranch == "" {
		t.Error("the fixed drift set was never committed back")
	}
}
