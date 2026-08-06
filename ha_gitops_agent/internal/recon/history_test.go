package recon

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/applier"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/differ"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/gitsync"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/history"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/regapply"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/registries"
)

// oneChange is the smallest plan that gives a reconcile drift to report
// and an apply something to do.
func oneChange() []differ.Change {
	return []differ.Change{{Path: "automations.yaml", Kind: "update", DiffText: "+x"}}
}

// onlyRecord returns the single recorded run: recording twice is as wrong
// as recording none.
func onlyRecord(t *testing.T, f *fakeHistory) history.Record {
	t.Helper()
	got := f.records()
	if len(got) != 1 {
		t.Fatalf("recorded %d runs, want exactly 1: %+v", len(got), got)
	}
	return got[0]
}

// run builds a distinguishable record for the ordering and cap tests.
func run(n int) history.Record {
	return history.Record{
		Kind:       history.KindReconcile,
		StartedUTC: "2026-08-05T12:00:00Z",
		DurationMS: int64(n),
		Outcome:    history.OutcomeInSync,
	}
}

// What a previous process recorded has to be on the page after a restart.
func TestHistoryIsHydratedFromDiskAtNew(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.history.loaded = []history.Record{run(1), run(2), run(3)}
	r := fakes.reconciler(baseOpts())

	got := r.Status().History
	if len(got) != 3 {
		t.Fatalf("Status().History has %d records, want the 3 loaded from disk", len(got))
	}
	if fakes.history.loadCalls != 1 {
		t.Errorf("Load was called %d times, want exactly 1 (hydration is a startup read, not a per-poll one)",
			fakes.history.loadCalls)
	}
}

// Status is rebuilt on every poll and again to hash the fragment, so a
// re-read here would be a permanent cost.
func TestStatusDoesNotRereadHistoryOnEveryCall(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.history.loaded = []history.Record{run(1)}
	r := fakes.reconciler(baseOpts())

	for i := 0; i < 5; i++ {
		_ = r.Status()
	}
	if fakes.history.loadCalls != 1 {
		t.Errorf("Load was called %d times across 5 Status calls, want 1", fakes.history.loadCalls)
	}
}

func TestStatusHistoryIsNewestFirst(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.history.loaded = []history.Record{run(1), run(2), run(3)}
	r := fakes.reconciler(baseOpts())

	got := r.Status().History
	if len(got) != 3 {
		t.Fatalf("Status().History has %d records, want 3", len(got))
	}
	for i, want := range []int64{3, 2, 1} {
		if got[i].DurationMS != want {
			t.Errorf("record %d is #%d, want #%d (Status.History must be newest-first, unlike Events)",
				i, got[i].DurationMS, want)
		}
	}
}

func TestStatusHistoryIsCappedAtHistoryStatusMax(t *testing.T) {
	fakes := newReconcilerFakes()
	for i := 1; i <= historyStatusMax+15; i++ {
		fakes.history.loaded = append(fakes.history.loaded, run(i))
	}
	r := fakes.reconciler(baseOpts())

	got := r.Status().History
	if len(got) != historyStatusMax {
		t.Fatalf("Status().History has %d records, want the cap of %d", len(got), historyStatusMax)
	}
	// The cap keeps the newest, not the first ones off the front.
	if want := int64(historyStatusMax + 15); got[0].DurationMS != want {
		t.Errorf("newest record is #%d, want #%d", got[0].DurationMS, want)
	}
}

// A nil slice serializes as null while every sibling list emits [].
func TestStatusHistoryIsEmptyNotNilWithNoRuns(t *testing.T) {
	r := newReconcilerFakes().reconciler(baseOpts())

	if got := r.Status().History; got == nil {
		t.Error("Status().History is nil with no runs, want an empty slice")
	} else if len(got) != 0 {
		t.Errorf("Status().History has %d records with no runs, want 0", len(got))
	}
}

// Callers must not reach the live ring through Status, the same guarantee
// Pending and Events give.
func TestStatusHistoryIsACopy(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.history.loaded = []history.Record{run(1)}
	r := fakes.reconciler(baseOpts())

	got := r.Status().History
	got[0].Outcome = "tampered"

	if again := r.Status().History; again[0].Outcome != history.OutcomeInSync {
		t.Errorf("mutating the returned slice reached the reconciler's own state: %q", again[0].Outcome)
	}
}

// --- history_all --------------------------------------------------------

// The whole ring, not the cut Status carries - the difference /history
// exists for.
func TestHistoryAllReturnsEveryRunNewestFirst(t *testing.T) {
	fakes := newReconcilerFakes()
	for i := 1; i <= historyStatusMax+15; i++ {
		fakes.history.loaded = append(fakes.history.loaded, run(i))
	}
	r := fakes.reconciler(baseOpts())

	got := r.HistoryAll()

	if want := historyStatusMax + 15; len(got) != want {
		t.Fatalf("HistoryAll() has %d records, want all %d", len(got), want)
	}
	if want := int64(historyStatusMax + 15); got[0].DurationMS != want {
		t.Errorf("first record is #%d, want the newest, #%d", got[0].DurationMS, want)
	}
	if got[len(got)-1].DurationMS != 1 {
		t.Errorf("last record is #%d, want the oldest, #1", got[len(got)-1].DurationMS)
	}
}

// The ring outlives every caller, so what a caller gets must not alias it.
func TestHistoryAllIsACopy(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.history.loaded = []history.Record{run(1), run(2)}
	r := fakes.reconciler(baseOpts())

	got := r.HistoryAll()
	got[0].Outcome = "tampered"
	got[1] = history.Record{}

	again := r.HistoryAll()
	if again[0].Outcome != history.OutcomeInSync || again[1].Outcome != history.OutcomeInSync {
		t.Errorf("mutating the returned slice reached the reconciler's own ring: %+v", again)
	}
}

// The page is served from the same in-memory ring as the dashboard, which
// is what makes 200 rows free to render.
func TestHistoryAllDoesNotRereadTheFile(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.history.loaded = []history.Record{run(1)}
	r := fakes.reconciler(baseOpts())

	for i := 0; i < 5; i++ {
		_ = r.HistoryAll()
	}

	if fakes.history.loadCalls != 1 {
		t.Errorf("Load was called %d times across 5 HistoryAll calls, want 1 (the startup hydration)",
			fakes.history.loadCalls)
	}
}

func TestHistoryAllIsEmptyNotNilWithNoRuns(t *testing.T) {
	r := newReconcilerFakes().reconciler(baseOpts())

	if got := r.HistoryAll(); got == nil {
		t.Error("HistoryAll() is nil with no runs, want an empty slice")
	} else if len(got) != 0 {
		t.Errorf("HistoryAll() has %d records with no runs, want 0", len(got))
	}
}

// The ring's real size, not the card's: the dashboard compares it against
// len(History) to decide whether to link to the longer list.
func TestStatusHistoryTotalCountsEveryRunHeld(t *testing.T) {
	fakes := newReconcilerFakes()
	for i := 1; i <= historyStatusMax+15; i++ {
		fakes.history.loaded = append(fakes.history.loaded, run(i))
	}
	r := fakes.reconciler(baseOpts())

	status := r.Status()

	if want := historyStatusMax + 15; status.HistoryTotal != want {
		t.Errorf("history_total = %d, want %d", status.HistoryTotal, want)
	}
	if len(status.History) != historyStatusMax {
		t.Errorf("Status().History has %d records, want the card's cap of %d",
			len(status.History), historyStatusMax)
	}
}

// Nothing to link to on an agent whose card already shows everything.
func TestStatusHistoryTotalEqualsTheShownRunsWhenTheyAllFit(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.history.loaded = []history.Record{run(1), run(2)}
	r := fakes.reconciler(baseOpts())

	if status := r.Status(); status.HistoryTotal != len(status.History) {
		t.Errorf("history_total = %d, want %d - the card is showing all of them",
			status.HistoryTotal, len(status.History))
	}
}

// --- reconcile ----------------------------------------------------------

func TestACompletedReconcileRecordsExactlyOneRun(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.differ.changes = oneChange()
	r := fakes.reconciler(baseOpts())

	r.ReconcileNow(context.Background())

	rec := onlyRecord(t, fakes.history)
	if rec.Kind != history.KindReconcile {
		t.Errorf("kind = %q, want %q", rec.Kind, history.KindReconcile)
	}
	if rec.Outcome != history.OutcomeDrift {
		t.Errorf("outcome = %q, want %q", rec.Outcome, history.OutcomeDrift)
	}
	if rec.Files != 1 {
		t.Errorf("files = %d, want 1", rec.Files)
	}
	if rec.SHA != "deadbeef" {
		t.Errorf("sha = %q, want the fetched deadbeef", rec.SHA)
	}
	if rec.StartedUTC == "" {
		t.Error("started_utc is empty, want the run's start time")
	}
	if rec.DurationMS < 0 {
		t.Errorf("duration_ms = %d, want a non-negative elapsed time", rec.DurationMS)
	}
}

func TestAnInSyncReconcileRecordsOutcomeInSync(t *testing.T) {
	fakes := newReconcilerFakes()
	r := fakes.reconciler(baseOpts())

	r.ReconcileNow(context.Background())

	rec := onlyRecord(t, fakes.history)
	if rec.Outcome != history.OutcomeInSync {
		t.Errorf("outcome = %q, want %q", rec.Outcome, history.OutcomeInSync)
	}
	if rec.Files != 0 || rec.RegOps != 0 {
		t.Errorf("counts = %d/%d, want 0/0 for an in-sync cycle", rec.Files, rec.RegOps)
	}
}

func TestAReconcileRecordsItsRegistryOpCount(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.registries.desired = registries.Desired{Floors: []map[string]any{{"id": "ground", "name": "Ground"}}}
	fakes.registries.planOps = []registries.RegOp{
		{Kind: registries.KindCreate, RType: "floor", Key: "ground", Params: map[string]any{"name": "Ground"}, DiffText: "+x"},
	}
	opts := baseOpts()
	opts.ReconcileRegistries = true
	r := fakes.reconciler(opts)

	r.ReconcileNow(context.Background())

	rec := onlyRecord(t, fakes.history)
	if rec.RegOps == 0 {
		t.Errorf("reg_ops = 0, want the planned registry ops counted: %+v", rec)
	}
}

// Every failCycle path is one run that ended in an error, and only one.
func TestEachFailCyclePathRecordsExactlyOneErrorRun(t *testing.T) {
	cases := []struct {
		name string
		fail func(*reconcilerFakes)
	}{
		{"ensure_clone", func(f *reconcilerFakes) { f.git.ensureCloneErr = errors.New("clone refused") }},
		{"fetch", func(f *reconcilerFakes) { f.git.fetchErr = errors.New("no such ref") }},
		{"tracked_raw", func(f *reconcilerFakes) { f.git.trackedRawErr = errors.New("ls-tree failed") }},
		{"secrets_guard", func(f *reconcilerFakes) { f.git.secretsErr = errors.New("secrets.yaml is tracked") }},
		{"tracked", func(f *reconcilerFakes) { f.git.trackedErr = errors.New("ls-files failed") }},
		{"checkout", func(f *reconcilerFakes) { f.git.checkoutErr = errors.New("detached head") }},
		{"decrypt", func(f *reconcilerFakes) { f.differ.decryptFailures = []string{"secrets.yaml: no key"} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fakes := newReconcilerFakes()
			tc.fail(fakes)
			r := fakes.reconciler(baseOpts())

			r.ReconcileNow(context.Background())

			rec := onlyRecord(t, fakes.history)
			if rec.Kind != history.KindReconcile {
				t.Errorf("kind = %q, want %q", rec.Kind, history.KindReconcile)
			}
			if rec.Outcome != history.OutcomeError {
				t.Errorf("outcome = %q, want %q", rec.Outcome, history.OutcomeError)
			}
			if rec.Error == "" {
				t.Error("error is empty, want the reason the cycle stopped")
			}
		})
	}
}

// A failure before the fetch has no commit to name; naming the previous
// cycle's would claim this one got further than it did.
func TestAFetchFailureRecordsNoSHA(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.git.fetchErr = errors.New("no such ref")
	r := fakes.reconciler(baseOpts())

	r.ReconcileNow(context.Background())

	if rec := onlyRecord(t, fakes.history); rec.SHA != "" {
		t.Errorf("sha = %q, want empty for a cycle that never fetched", rec.SHA)
	}
}

func TestAPostFetchFailureRecordsTheFetchedSHA(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.git.checkoutErr = errors.New("detached head")
	r := fakes.reconciler(baseOpts())

	r.ReconcileNow(context.Background())

	if rec := onlyRecord(t, fakes.history); rec.SHA != "deadbeef" {
		t.Errorf("sha = %q, want deadbeef", rec.SHA)
	}
}

// A busy reconcile hands back the existing plan: a refusal, not a run.
func TestABusyReconcileRecordsNoRun(t *testing.T) {
	fakes := newReconcilerFakes()
	r := fakes.reconciler(baseOpts())
	r.opLock.Lock()
	defer r.opLock.Unlock()

	r.ReconcileNow(context.Background())

	if got := fakes.history.records(); len(got) != 0 {
		t.Errorf("recorded %d runs for a busy refusal, want 0: %+v", len(got), got)
	}
}

// --- apply --------------------------------------------------------------

// applyFakes drives a reconcile so there is a plan, then clears the store
// so it holds only the apply's own record.
func applyFakes(t *testing.T) (*reconcilerFakes, *Reconciler) {
	t.Helper()
	fakes := newReconcilerFakes()
	fakes.differ.changes = oneChange()
	opts := baseOpts()
	opts.DryRun = false
	r := fakes.reconciler(opts)
	r.ReconcileNow(context.Background())
	fakes.history.appended = nil
	return fakes, r
}

func TestASuccessfulApplyRecordsOneRun(t *testing.T) {
	fakes, r := applyFakes(t)
	fakes.applier.applyResult = applier.Result{
		OK: true, Changed: []string{"automations.yaml"}, StashDir: "/data/backup/x",
	}

	r.ApplyNow(context.Background(), true)

	rec := onlyRecord(t, fakes.history)
	if rec.Kind != history.KindApply {
		t.Errorf("kind = %q, want %q", rec.Kind, history.KindApply)
	}
	if rec.Outcome != history.OutcomeOK {
		t.Errorf("outcome = %q, want %q", rec.Outcome, history.OutcomeOK)
	}
	if rec.Files != 1 {
		t.Errorf("files = %d, want the 1 change that landed", rec.Files)
	}
	if rec.StashDir != "/data/backup/x" {
		t.Errorf("stash_dir = %q, want /data/backup/x", rec.StashDir)
	}
	if rec.Error != "" {
		t.Errorf("error = %q, want empty on a clean apply", rec.Error)
	}
}

// The apply row names the commit whose plan it executed, so it lines up
// with the reconcile row above it.
func TestAnApplyRecordsThePlannedSHA(t *testing.T) {
	fakes, r := applyFakes(t)

	r.ApplyNow(context.Background(), true)

	if rec := onlyRecord(t, fakes.history); rec.SHA != "deadbeef" {
		t.Errorf("sha = %q, want the planned deadbeef", rec.SHA)
	}
}

func TestARolledBackApplyRecordsOutcomeRolledBack(t *testing.T) {
	fakes, r := applyFakes(t)
	fakes.applier.applyResult = applier.Result{
		OK: false, Error: "check_config: invalid", RolledBack: true, StashDir: "/data/backup/failed-1",
	}

	r.ApplyNow(context.Background(), true)

	rec := onlyRecord(t, fakes.history)
	if rec.Outcome != history.OutcomeRolledBack {
		t.Errorf("outcome = %q, want %q", rec.Outcome, history.OutcomeRolledBack)
	}
	if rec.Error == "" {
		t.Error("error is empty, want the check_config failure")
	}
	if rec.StashDir != "/data/backup/failed-1" {
		t.Errorf("stash_dir = %q, want the stash the files came back from", rec.StashDir)
	}
}

// Only claim a rollback that happened: an apply that failed without one
// has left something behind.
func TestAFailedApplyThatDidNotRollBackRecordsOutcomeError(t *testing.T) {
	fakes, r := applyFakes(t)
	fakes.applier.applyResult = applier.Result{
		OK: false, Error: "could not restore", RolledBack: false, StashDir: "/data/backup/failed-2",
	}

	r.ApplyNow(context.Background(), true)

	if rec := onlyRecord(t, fakes.history); rec.Outcome != history.OutcomeError {
		t.Errorf("outcome = %q, want %q", rec.Outcome, history.OutcomeError)
	}
}

// The files are live, so the row must not say the run did nothing - which
// is what OutcomePartial exists for.
func TestARegistryFailureRecordsPartialWithTheFilesStillCounted(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.differ.changes = oneChange()
	fakes.registries.desired = registries.Desired{Floors: []map[string]any{{"id": "ground", "name": "Ground"}}}
	fakes.registries.planOps = []registries.RegOp{
		{Kind: registries.KindCreate, RType: "floor", Key: "ground", Params: map[string]any{"name": "Ground"}, DiffText: "+x"},
	}
	fakes.applier.applyResult = applier.Result{
		OK: true, Changed: []string{"automations.yaml"}, StashDir: "/data/backup/x",
	}
	fakes.registryApplier.applyResult = regapply.RegistryApplyResult{
		OK: false, Error: "floor create failed", RolledBack: true,
	}
	opts := baseOpts()
	opts.DryRun = false
	opts.ReconcileRegistries = true
	r := fakes.reconciler(opts)
	r.ReconcileNow(context.Background())
	fakes.history.appended = nil

	r.ApplyNow(context.Background(), true)

	rec := onlyRecord(t, fakes.history)
	if rec.Outcome != history.OutcomePartial {
		t.Errorf("outcome = %q, want %q: the files landed and are live", rec.Outcome, history.OutcomePartial)
	}
	if rec.Files != 1 {
		t.Errorf("files = %d, want the 1 file that stayed applied", rec.Files)
	}
	if rec.Error == "" {
		t.Error("error is empty, want the registry failure alongside the counts")
	}
}

// An unwritten state.json is exactly when someone needs to know what landed.
func TestAStateSaveFailureRecordsOutcomePartial(t *testing.T) {
	fakes, r := applyFakes(t)
	fakes.applier.stateSaveErr = errors.New("/data is read-only")

	r.ApplyNow(context.Background(), true)

	rec := onlyRecord(t, fakes.history)
	if rec.Outcome != history.OutcomePartial {
		t.Errorf("outcome = %q, want %q", rec.Outcome, history.OutcomePartial)
	}
	if rec.Files != 1 {
		t.Errorf("files = %d, want the 1 change that landed before the save failed", rec.Files)
	}
}

// A refusal is not a run, and the dry-run one fires on every interval tick.
func TestARefusedApplyRecordsNoRun(t *testing.T) {
	cases := []struct {
		name  string
		force bool
		setup func(*reconcilerFakes, *Reconciler)
	}{
		{"dry_run", false, func(f *reconcilerFakes, r *Reconciler) { r.opts.DryRun = true }},
		{"busy", true, func(f *reconcilerFakes, r *Reconciler) { r.opLock.Lock() }},
		{"last_cycle_failed", true, func(f *reconcilerFakes, r *Reconciler) {
			r.mu.Lock()
			r.lastCycleFailed = true
			r.mu.Unlock()
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fakes, r := applyFakes(t)
			tc.setup(fakes, r)

			r.ApplyNow(context.Background(), tc.force)

			if got := fakes.history.records(); len(got) != 0 {
				t.Errorf("recorded %d runs for the %s refusal, want 0: %+v", len(got), tc.name, got)
			}
		})
	}
}

// --- rollback -----------------------------------------------------------

func TestRollbackRecordsOneRunWithNoSHA(t *testing.T) {
	fakes, r := applyFakes(t)
	r.ApplyNow(context.Background(), true)
	fakes.applier.rollbackResult = applier.Result{
		OK: true, RolledBack: true, Changed: []string{"automations.yaml"},
	}
	fakes.history.appended = nil

	r.Rollback(context.Background())

	rec := onlyRecord(t, fakes.history)
	if rec.Kind != history.KindRollback {
		t.Errorf("kind = %q, want %q", rec.Kind, history.KindRollback)
	}
	if rec.Outcome != history.OutcomeOK {
		t.Errorf("outcome = %q, want %q", rec.Outcome, history.OutcomeOK)
	}
	// A rollback moves live away from a commit, so naming one would read
	// as "rolled back to deadbeef" - the opposite of what happened.
	if rec.SHA != "" {
		t.Errorf("sha = %q, want empty on a rollback row", rec.SHA)
	}
	if rec.Files != 1 {
		t.Errorf("files = %d, want the 1 file restored", rec.Files)
	}
}

func TestAFailedRollbackRecordsOutcomeError(t *testing.T) {
	fakes, r := applyFakes(t)
	r.ApplyNow(context.Background(), true)
	fakes.applier.rollbackResult = applier.Result{OK: false, Error: "stash is gone"}
	fakes.history.appended = nil

	r.Rollback(context.Background())

	rec := onlyRecord(t, fakes.history)
	if rec.Outcome != history.OutcomeError {
		t.Errorf("outcome = %q, want %q", rec.Outcome, history.OutcomeError)
	}
	if rec.Error == "" {
		t.Error("error is empty, want why the rollback failed")
	}
}

func TestARefusedRollbackRecordsNoRun(t *testing.T) {
	// No apply has run, so there is no stash to roll back to.
	fakes := newReconcilerFakes()
	r := fakes.reconciler(baseOpts())

	r.Rollback(context.Background())

	if got := fakes.history.records(); len(got) != 0 {
		t.Errorf("recorded %d runs for a rollback with nothing to restore, want 0: %+v", len(got), got)
	}
}

// --- import -------------------------------------------------------------

// Two rows: an import runs a reconcile against the new tip afterwards, and
// collapsing them would hide which half failed.
func TestImportRecordsOneImportRunAndOneReconcileRun(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.git.importResult = gitsync.ImportResult{Files: 12, Bytes: 4096, CommitSHA: "abc1234def", Created: true}
	opts := baseOpts()
	opts.AllowImport = true
	r := fakes.reconciler(opts)

	if _, err := r.ImportLive(context.Background()); err != nil {
		t.Fatalf("ImportLive: %v", err)
	}

	got := fakes.history.records()
	if len(got) != 2 {
		t.Fatalf("recorded %d runs, want 2 (the import and the reconcile after it): %+v", len(got), got)
	}
	if got[0].Kind != history.KindImport {
		t.Errorf("first record kind = %q, want %q", got[0].Kind, history.KindImport)
	}
	if got[0].Outcome != history.OutcomeOK {
		t.Errorf("import outcome = %q, want %q", got[0].Outcome, history.OutcomeOK)
	}
	if got[0].Files != 12 {
		t.Errorf("import files = %d, want the 12 committed", got[0].Files)
	}
	if got[0].SHA != "abc1234def" {
		t.Errorf("import sha = %q, want the commit it created", got[0].SHA)
	}
	if got[1].Kind != history.KindReconcile {
		t.Errorf("second record kind = %q, want %q", got[1].Kind, history.KindReconcile)
	}
}

func TestAFailedImportRecordsOneErrorRun(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.git.importErr = errors.New("import exceeds the size cap")
	opts := baseOpts()
	opts.AllowImport = true
	r := fakes.reconciler(opts)

	if _, err := r.ImportLive(context.Background()); err == nil {
		t.Fatal("ImportLive succeeded, want the injected failure")
	}

	rec := onlyRecord(t, fakes.history)
	if rec.Kind != history.KindImport || rec.Outcome != history.OutcomeError {
		t.Errorf("record = %+v, want a failed import", rec)
	}
	// The record is about the run; the sync state is about live versus the
	// repository, which an import failure says nothing about.
	if got := r.Status().State; got == StateError {
		t.Error("state = error after a failed import, want it left alone")
	}
}

func TestARefusedImportRecordsNoRun(t *testing.T) {
	fakes := newReconcilerFakes()
	r := fakes.reconciler(baseOpts()) // AllowImport is off

	if _, err := r.ImportLive(context.Background()); err == nil {
		t.Fatal("ImportLive succeeded, want the disabled refusal")
	}

	if got := fakes.history.records(); len(got) != 0 {
		t.Errorf("recorded %d runs for a disabled import, want 0: %+v", len(got), got)
	}
}

// --- write failures -----------------------------------------------------

// The run already happened, whatever the file says.
func TestAHistoryWriteFailureDoesNotFailTheRun(t *testing.T) {
	fakes, r := applyFakes(t)
	fakes.history.setAppendErr(errors.New("/data is read-only"))

	result := r.ApplyNow(context.Background(), true)

	if !result.OK {
		t.Errorf("apply returned %+v, want ok despite the history write failing", result)
	}
	if got := r.Status().State; got == StateError {
		t.Errorf("state = %q, want the apply's own outcome", got)
	}
}

// The card keeps working for as long as the process lives; only a restart
// loses the run.
func TestAHistoryWriteFailureStillShowsTheRunOnThePage(t *testing.T) {
	fakes, r := applyFakes(t)
	fakes.history.setAppendErr(errors.New("/data is read-only"))

	r.ApplyNow(context.Background(), true)

	// Specifically the apply's own row: "non-empty" would pass on the
	// reconcile row applyFakes already left behind.
	got := r.Status().History
	if len(got) == 0 {
		t.Fatal("Status().History is empty after a failed write, want the in-memory row")
	}
	if got[0].Kind != history.KindApply {
		t.Errorf("newest row is a %q, want the %q whose write failed", got[0].Kind, history.KindApply)
	}
}

// A read-only /data does not fix itself, so a line per run would push a
// day of real history out of a 200-line feed.
func TestAHistoryWriteFailureIsLoggedOncePerTransition(t *testing.T) {
	fakes, r := applyFakes(t)
	fakes.history.setAppendErr(errors.New("/data is read-only"))

	for i := 0; i < 3; i++ {
		r.ReconcileNow(context.Background())
	}

	var failures int
	for _, e := range r.Status().Events {
		if strings.Contains(e.Message, "could not record run history") {
			failures++
		}
	}
	if failures != 1 {
		t.Errorf("logged the write failure %d times across 3 runs, want 1", failures)
	}

	// And one line back, or the feed leaves it looking still broken.
	fakes.history.setAppendErr(nil)
	r.ReconcileNow(context.Background())
	if !hasEventContaining(r.Status().Events, "run history is being recorded again") {
		t.Error("recovery was not logged, want one line when writing starts working again")
	}
}

// --- the ring -----------------------------------------------------------

func TestTheInMemoryRingIsBoundedByHistoryKeep(t *testing.T) {
	fakes := newReconcilerFakes()
	r := fakes.reconciler(baseOpts())

	for i := 0; i < historyKeep+5; i++ {
		r.recordRun(history.Record{Kind: history.KindReconcile, DurationMS: int64(i)})
	}

	r.mu.Lock()
	got := len(r.runs)
	oldest := r.runs[0].DurationMS
	r.mu.Unlock()

	if got != historyKeep {
		t.Errorf("ring holds %d records, want the cap of %d", got, historyKeep)
	}
	if oldest != 5 {
		t.Errorf("oldest retained record is #%d, want #5 (the first 5 should have been dropped)", oldest)
	}
}

// --- runs that never reached a finish -----------------------------------

// What the deferred abandon exists for: tick and web.recoverOp keep the
// process alive, so without it the panicking run is the one with no row.
func TestARunThatPanickedIsStillRecorded(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.differ.panicOnCompute = true
	r := fakes.reconciler(baseOpts())

	r.tick(context.Background())

	rec := onlyRecord(t, fakes.history)
	if rec.Kind != history.KindReconcile {
		t.Errorf("kind = %q, want %q", rec.Kind, history.KindReconcile)
	}
	if rec.Outcome != history.OutcomeError {
		t.Errorf("outcome = %q, want %q", rec.Outcome, history.OutcomeError)
	}
	if rec.Error == "" {
		t.Error("error is empty, want it to say the run did not complete")
	}
}

// abandon must never turn a completed run into a second row.
func TestAbandonDoesNotDoubleRecordACompletedRun(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.differ.changes = oneChange()
	r := fakes.reconciler(baseOpts())

	r.ReconcileNow(context.Background())

	if rec := onlyRecord(t, fakes.history); rec.Outcome != history.OutcomeDrift {
		t.Errorf("outcome = %q, want the finished %q rather than an abandon", rec.Outcome, history.OutcomeDrift)
	}
}

// --- normalization ------------------------------------------------------

// The dashboard's copy and the disk's must agree, or the same run reads
// one way now and another after a restart.
func TestARecordedErrorIsBoundedInMemoryAsWellAsOnDisk(t *testing.T) {
	fakes, r := applyFakes(t)
	huge := strings.Repeat("e", 10_000)
	fakes.applier.applyResult = applier.Result{OK: false, Error: huge, RolledBack: true}

	r.ApplyNow(context.Background(), true)

	onDisk := onlyRecord(t, fakes.history)
	if len(onDisk.Error) > history.ErrorMaxLen {
		t.Errorf("the recorded error is %d chars, want at most %d", len(onDisk.Error), history.ErrorMaxLen)
	}
	inMemory := r.Status().History
	if len(inMemory) == 0 {
		t.Fatal("Status().History is empty, want the apply row")
	}
	if len(inMemory[0].Error) > history.ErrorMaxLen {
		t.Errorf("the error Status serves is %d chars, want at most %d - the page and the file must agree",
			len(inMemory[0].Error), history.ErrorMaxLen)
	}
}
