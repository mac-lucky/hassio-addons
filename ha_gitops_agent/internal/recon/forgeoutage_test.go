package recon

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/differ"
)

// What git prints when the forge's proxy answers while the forge itself is
// stopped - the daily case this grace exists for.
var errForge502 = errors.New("git fetch failed (exit 128): fatal: unable to access " +
	"'https://git.example/ha-config.git/': The requested URL returned error: 502")

func withForgeOutageGrace(t *testing.T, d time.Duration) {
	t.Helper()
	old := forgeOutageGrace
	forgeOutageGrace = d
	t.Cleanup(func() { forgeOutageGrace = old })
}

// A forge stopped for a few minutes every morning used to put the agent in
// the error state every morning, and drop the plan waiting for review.
func TestAShortForgeOutageKeepsStateAndPlan(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.differ.changes = []differ.Change{{Path: "automations.yaml", Kind: "update", DiffText: "+x"}}
	r := fakes.reconciler(baseOpts())

	r.ReconcileNow(context.Background())
	before := r.Status()
	if before.State == StateError {
		t.Fatalf("setup: state = %q", before.State)
	}
	rows := len(fakes.history.appended)

	fakes.git.fetchErr = errForge502
	r.ReconcileNow(context.Background())
	r.ReconcileNow(context.Background())

	status := r.Status()
	if status.State != before.State {
		t.Errorf("state = %q, want %q kept through the outage", status.State, before.State)
	}
	if status.LastError != "" {
		t.Errorf("last_error = %q, want none within the grace", status.LastError)
	}
	if len(status.Pending) != 1 {
		t.Errorf("pending = %+v, want the plan kept", status.Pending)
	}
	if status.PlanID != before.PlanID {
		t.Errorf("plan_id changed during the outage: %q -> %q", before.PlanID, status.PlanID)
	}
	if status.FetchFailingSince == "" {
		t.Error("fetch_failing_since is empty during an outage")
	}
	if got := len(fakes.history.appended); got != rows {
		t.Errorf("history rows = %d, want %d - an outage tick writes none", got, rows)
	}
	if n := countEventsContaining(status.Events, "git host unreachable"); n != 1 {
		t.Errorf("unreachable events = %d, want exactly 1 per outage", n)
	}

	fakes.git.fetchErr = nil
	r.ReconcileNow(context.Background())
	status = r.Status()
	if status.FetchFailingSince != "" {
		t.Errorf("fetch_failing_since = %q after recovery, want empty", status.FetchFailingSince)
	}
	if !hasEventContaining(status.Events, "git host reachable again") {
		t.Errorf("no recovery event; events = %+v", status.Events)
	}
}

func TestAForgeOutagePastTheGraceIsAnError(t *testing.T) {
	withForgeOutageGrace(t, time.Millisecond)
	fakes := newReconcilerFakes()
	r := fakes.reconciler(baseOpts())
	r.ReconcileNow(context.Background())

	fakes.git.fetchErr = errForge502
	r.ReconcileNow(context.Background())
	time.Sleep(5 * time.Millisecond)
	r.ReconcileNow(context.Background())

	status := r.Status()
	if status.State != StateError {
		t.Errorf("state = %q, want error once the outage outlasts the grace", status.State)
	}
	if status.FetchFailingSince == "" {
		t.Error("fetch_failing_since cleared by the error; it should say since when")
	}
}

// With no fetch yet in this process there is no plan worth keeping, and an
// unreachable forge at startup is worth reporting at once.
func TestAForgeOutageBeforeTheFirstFetchIsAnError(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.git.fetchErr = errForge502
	r := fakes.reconciler(baseOpts())

	r.ReconcileNow(context.Background())

	if status := r.Status(); status.State != StateError {
		t.Errorf("state = %q, want error", status.State)
	}
}

// Only an outage is ridden out: a rejected token or a missing repository
// is wrong until somebody fixes it.
func TestANonTransientFetchFailureIsAnErrorAtOnce(t *testing.T) {
	fakes := newReconcilerFakes()
	r := fakes.reconciler(baseOpts())
	r.ReconcileNow(context.Background())

	fakes.git.fetchErr = errors.New("git fetch failed (exit 128): remote: Invalid username or password.")
	r.ReconcileNow(context.Background())

	status := r.Status()
	if status.State != StateError {
		t.Errorf("state = %q, want error", status.State)
	}
	if status.FetchFailingSince != "" {
		t.Errorf("fetch_failing_since = %q for a non-transient failure", status.FetchFailingSince)
	}
}

// The plan kept through an outage is the last good cycle's, which had its
// own chance to apply; the timer must not apply it blind.
func TestNoAutomaticApplyDuringAForgeOutage(t *testing.T) {
	fakes := newReconcilerFakes()
	opts := baseOpts()
	opts.DryRun = false
	r := fakes.reconciler(opts)
	fakes.differ.changes = []differ.Change{{Path: "automations.yaml", Kind: "update", DiffText: "+x"}}
	_ = r.SetPaused(true)
	r.runCycle(context.Background())
	_ = r.SetPaused(false)

	fakes.git.fetchErr = errForge502
	r.runCycle(context.Background())

	if got := len(fakes.applier.applyCalls); got != 0 {
		t.Errorf("apply calls = %d, want 0 during an outage", got)
	}
}

// A webhook cycle waits for a running operation now instead of being
// dropped - except behind a Roll Back, which pauses and asks for the
// repository to be fixed first: a capture straight after it would push the
// restored files over the commit that was rolled back. Any delivery
// accepted before the rollback finished is dropped, whether it waited
// here or in the webhook's queue.
func TestSyncNowWaitsForABusyAgentButNotPastARollback(t *testing.T) {
	for _, rollback := range []bool{false, true} {
		fakes := newReconcilerFakes()
		r := fakes.reconciler(baseOpts())
		acceptedAt := time.Now()

		r.opLock.Lock()
		done := make(chan struct{})
		go func() {
			r.SyncNow(context.Background(), acceptedAt)
			close(done)
		}()
		time.Sleep(50 * time.Millisecond)
		if rollback {
			r.withMu(func() { r.lastRollbackAt = time.Now() })
		}
		r.opLock.Unlock()
		<-done

		ran := fakes.git.fetchCalls > 0
		if ran == rollback {
			t.Errorf("rollback during the wait = %v: cycle ran = %v", rollback, ran)
		}
	}
}

func TestSyncNowRunsForADeliveryAfterTheRollback(t *testing.T) {
	fakes := newReconcilerFakes()
	r := fakes.reconciler(baseOpts())
	r.withMu(func() { r.lastRollbackAt = time.Now().Add(-time.Minute) })

	r.SyncNow(context.Background(), time.Now())

	if fakes.git.fetchCalls == 0 {
		t.Error("a delivery that arrived after the rollback did not run")
	}
}

// The refresh after an import must not ride out an outage: the plan it
// would keep predates the import and would write the old repository
// content over the edits just imported.
func TestReconcileAfterImportDoesNotRideOutAnOutage(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.differ.changes = []differ.Change{{Path: "automations.yaml", Kind: "update", DiffText: "+x"}}
	r := fakes.reconciler(baseOpts())
	r.ReconcileNow(context.Background())

	fakes.git.fetchErr = errForge502
	r.reconcileNowWith(context.Background(), false)

	status := r.Status()
	if status.State != StateError || len(status.Pending) != 0 {
		t.Errorf("state = %q pending = %d, want error with the stale plan dropped", status.State, len(status.Pending))
	}
}

// An apply can fill lastSHA in from state.json before any fetch has
// worked; an outage then must still be an error, since there is no plan
// from this process to keep.
func TestAForgeOutageAfterAnApplyButBeforeAnyFetchIsAnError(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.applier.state.LastGoodSHA = "4f9c2a7e8b1d0c3f6a5e9d8c7b6a5f4e3d2c1b0a"
	opts := baseOpts()
	opts.DryRun = false
	r := fakes.reconciler(opts)
	r.ApplyNow(context.Background(), true)

	fakes.git.fetchErr = errForge502
	r.ReconcileNow(context.Background())

	if status := r.Status(); status.State != StateError {
		t.Errorf("state = %q, want error: nothing has been fetched since startup", status.State)
	}
}
