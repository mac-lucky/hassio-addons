package recon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/differ"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/gitsync"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/statusd"
)

// unbornBranch is what gitsync.Fetch returns for a remote that has no such
// branch: wrapped, because recon matches it with errors.Is and not by text.
func unbornBranch() error {
	return fmt.Errorf("%w: main", gitsync.ErrRemoteBranchMissing)
}

func TestAnUnbornRemoteBranchIsNotAnError(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.git.fetchErr = unbornBranch()
	r := fakes.reconciler(baseOpts())

	r.ReconcileNow(context.Background())

	status := r.Status()
	if status.State != StateUnseeded {
		t.Errorf("state = %q, want %q", status.State, StateUnseeded)
	}
	if status.LastError != "" {
		t.Errorf("last_error = %q, want empty: an unseeded repository is not a failure", status.LastError)
	}
	if len(fakes.status.pushes) == 0 {
		t.Fatal("no sensor push")
	}
	if last := fakes.status.pushes[len(fakes.status.pushes)-1]; last.state != StateUnseeded {
		t.Errorf("sensor state = %q, want %q", last.state, StateUnseeded)
	}
}

// The sensor drops any state absent from statusd.States, so shipping the
// state without registering it would stop the sensor updating in
// production while every fake in this package kept passing.
func TestUnseededStateIsInTheSensorVocabulary(t *testing.T) {
	if !statusd.States[StateUnseeded] {
		t.Fatalf("statusd.States is missing %q, so the sensor would silently stop updating", StateUnseeded)
	}
}

func TestUnseededCycleWritesNoHistoryRow(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.git.fetchErr = unbornBranch()
	r := fakes.reconciler(baseOpts())

	for range 3 {
		r.ReconcileNow(context.Background())
	}

	if got := fakes.history.records(); len(got) != 0 {
		t.Errorf("recorded %d runs, want 0: a standing refusal is not a run", len(got))
	}
	if got := r.Status().History; len(got) != 0 {
		t.Errorf("status history = %d rows, want 0", len(got))
	}
}

func TestUnseededCycleLogsOnceNotOncePerInterval(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.git.fetchErr = unbornBranch()
	r := fakes.reconciler(baseOpts())

	for range 3 {
		r.ReconcileNow(context.Background())
	}

	if n := countEventsContaining(r.Status().Events, "does not exist"); n != 1 {
		t.Errorf("logged the condition %d times, want 1: it repeats every interval", n)
	}
}

func TestUnseededCycleClearsAStalePlanAndRefusesApply(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.differ.changes = []differ.Change{{Path: "automations.yaml", Kind: "modified"}}
	r := fakes.reconciler(baseOpts())

	r.ReconcileNow(context.Background())
	if got := r.Status().PendingCount; got != 1 {
		t.Fatalf("pending = %d, want 1 before the branch goes missing", got)
	}

	fakes.git.fetchErr = unbornBranch()
	r.ReconcileNow(context.Background())

	if got := r.Status().PendingCount; got != 0 {
		t.Errorf("pending = %d, want 0: the plan describes a tree no longer checked out", got)
	}
	res := r.ApplyNow(context.Background(), true)
	if res.OK {
		t.Fatal("apply went ahead with no plan")
	}
	if !strings.Contains(res.Error, "does not exist") {
		t.Errorf("refusal = %q, want it to name the missing branch rather than a failed reconcile", res.Error)
	}
}

// The classification fence: only a definite absence is the sentinel, so an
// auth failure or an unreachable host must still park the agent in error.
func TestAHardFetchFailureIsStillAnError(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.git.fetchErr = errors.New("Authentication failed")
	r := fakes.reconciler(baseOpts())

	r.ReconcileNow(context.Background())

	status := r.Status()
	if status.State != StateError {
		t.Errorf("state = %q, want %q", status.State, StateError)
	}
	if status.LastError == "" {
		t.Error("last_error is empty, want the git failure")
	}
	if got := fakes.history.records(); len(got) != 1 {
		t.Errorf("recorded %d runs, want 1: a real failure is a run", len(got))
	}
}

func TestSeedingLeavesTheUnseededState(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.git.fetchErr = unbornBranch()
	r := fakes.reconciler(baseOpts())

	r.ReconcileNow(context.Background())
	if got := r.Status().State; got != StateUnseeded {
		t.Fatalf("state = %q, want %q", got, StateUnseeded)
	}

	// What a seed looks like from here: the branch now resolves.
	fakes.git.fetchErr = nil
	r.ReconcileNow(context.Background())

	if got := r.Status().State; got != StateInSync {
		t.Errorf("state = %q, want %q once the branch exists", got, StateInSync)
	}
	if res := r.ApplyNow(context.Background(), true); !res.OK {
		t.Errorf("apply still refused after the branch appeared: %s", res.Error)
	}
}
