package recon

import (
	"context"
	"testing"
	"time"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/applier"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/differ"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/regapply"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/registries"
)

func failingPlanFakes() *reconcilerFakes {
	fakes := newReconcilerFakes()
	fakes.applier.applyResult = applier.Result{
		OK: false, Error: "check_config: invalid", RolledBack: true, StashDir: "/data/backup/failed-1",
	}
	fakes.differ.changes = []differ.Change{{Path: "automations.yaml", Kind: "update", DiffText: "+x"}}
	return fakes
}

// A commit check_config rejects used to be re-applied every interval, each
// time behind a full Supervisor backup that nothing pruned.
func TestAFailedPlanIsNotReappliedByTheTimer(t *testing.T) {
	fakes := failingPlanFakes()
	opts := baseOpts()
	opts.DryRun = false
	r := fakes.reconciler(opts)

	r.runCycle(context.Background())
	r.runCycle(context.Background())
	r.runCycle(context.Background())

	if got := len(fakes.applier.applyCalls); got != 1 {
		t.Errorf("apply calls = %d, want 1", got)
	}
	if fakes.snapshot.backupCalls != 1 {
		t.Errorf("backups = %d, want 1", fakes.snapshot.backupCalls)
	}
	if fakes.snapshot.pruneCalls != 1 {
		t.Errorf("backup prunes = %d, want 1 - a failed apply must prune too", fakes.snapshot.pruneCalls)
	}
	status := r.Status()
	if status.ApplyHeld != "check_config: invalid" {
		t.Errorf("apply_held = %q, want the failure reason", status.ApplyHeld)
	}
	held := 0
	for _, e := range status.Events {
		if hasEventContaining([]Event{e}, "automatic apply held") {
			held++
		}
	}
	if held != 1 {
		t.Errorf("held events = %d, want exactly 1; events = %+v", held, status.Events)
	}
}

func TestAChangedPlanIsAppliedAgain(t *testing.T) {
	fakes := failingPlanFakes()
	opts := baseOpts()
	opts.DryRun = false
	r := fakes.reconciler(opts)
	r.runCycle(context.Background())

	fakes.differ.changes = []differ.Change{{Path: "automations.yaml", Kind: "update", DiffText: "+fixed"}}
	r.runCycle(context.Background())

	if got := len(fakes.applier.applyCalls); got != 2 {
		t.Errorf("apply calls = %d, want 2 - the plan changed", got)
	}
}

func TestANewCommitIsAppliedAgain(t *testing.T) {
	fakes := failingPlanFakes()
	opts := baseOpts()
	opts.DryRun = false
	r := fakes.reconciler(opts)
	r.runCycle(context.Background())

	fakes.git.sha = "cafef00d"
	r.runCycle(context.Background())

	if got := len(fakes.applier.applyCalls); got != 2 {
		t.Errorf("apply calls = %d, want 2 - a new commit is a new plan", got)
	}
}

func TestTheApplyButtonIgnoresTheHoldAndASuccessClearsIt(t *testing.T) {
	fakes := failingPlanFakes()
	opts := baseOpts()
	opts.DryRun = false
	r := fakes.reconciler(opts)
	r.runCycle(context.Background())

	fakes.applier.applyResult = applier.Result{OK: true, Changed: []string{"automations.yaml"}, StashDir: t.TempDir()}
	r.ApplyNow(context.Background(), true)

	if got := len(fakes.applier.applyCalls); got != 2 {
		t.Fatalf("apply calls = %d, want 2", got)
	}
	if got := r.Status().ApplyHeld; got != "" {
		t.Errorf("apply_held = %q after a successful apply, want it cleared", got)
	}
}

// A registry failure holds what is left of the plan - the ops that failed
// or never ran - which is what the next reconcile plans again.
func TestAFailedRegistryPlanIsHeldToo(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.differ.changes = nil
	fakes.registries.desired = registries.Desired{Floors: []map[string]any{{"id": "ground", "name": "Ground"}}}
	fakes.registries.planOps = []registries.RegOp{
		{RType: "floor", Key: "ground", Kind: registries.KindCreate, DiffText: "+name: Ground\n"},
	}
	fakes.registryApplier.applyResult = regapply.RegistryApplyResult{OK: false, Error: "name already in use", RolledBack: true}
	fakes.applier.applyResult = applier.Result{OK: true}
	fakes.applier.makeStashDirResult = t.TempDir()
	opts := baseOpts()
	opts.DryRun = false
	opts.ReconcileRegistries = true
	r := fakes.reconciler(opts)

	r.runCycle(context.Background())
	r.runCycle(context.Background())

	if got := len(fakes.registryApplier.applyPlanCalls); got != 1 {
		t.Errorf("registry apply calls = %d, want 1", got)
	}
	if got := r.Status().ApplyHeld; got == "" {
		t.Error("apply_held is empty after a registry failure")
	}
}

// A capture whose restore never ran leaves HEAD on its own commit; the
// apply must write the planned tree and record the planned commit.
func TestApplyChecksOutThePlannedCommitWhenHEADMoved(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.differ.changes = oneChange()
	fakes.applier.applyResult = applier.Result{OK: true, Changed: []string{"automations.yaml"}, StashDir: t.TempDir()}
	opts := baseOpts()
	opts.DryRun = false
	r := fakes.reconciler(opts)
	r.ReconcileNow(context.Background())

	fakes.git.headSHA = "c0ffee00"
	r.ApplyNow(context.Background(), true)

	if n := len(fakes.git.checkoutCalls); n == 0 || fakes.git.checkoutCalls[n-1] != "deadbeef" {
		t.Errorf("checkouts = %v, want the planned deadbeef last", fakes.git.checkoutCalls)
	}
	saves := fakes.applier.stateSaveCalls
	if len(saves) == 0 || saves[len(saves)-1].LastGoodSHA != "deadbeef" {
		t.Errorf("last_good_sha saved = %+v, want deadbeef", saves)
	}
}

// reconcile.yaml_files was parsed and then ignored: every tracked file was
// still written into /homeassistant.
func TestYAMLFilesOffPlansNoFiles(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.differ.changes = oneChange()
	fakes.applier.applyResult = applier.Result{OK: true}
	fakes.applier.state = applier.State{Manifest: []string{}, RegistryManaged: map[string]string{}, LastGoodSHA: "old"}
	fakes.registries.desired = registries.Desired{Floors: []map[string]any{{"id": "ground", "name": "Ground"}}}
	fakes.registries.planOps = []registries.RegOp{
		{RType: "floor", Key: "ground", Kind: registries.KindCreate, DiffText: "+name: Ground\n"},
	}
	fakes.registryApplier.applyResult = regapply.RegistryApplyResult{OK: true, Applied: []string{"create floor:ground"}}
	fakes.applier.makeStashDirResult = t.TempDir()
	opts := baseOpts()
	opts.DryRun = false
	opts.ReconcileYAMLFiles = false
	opts.ReconcileRegistries = true
	r := fakes.reconciler(opts)

	r.runCycle(context.Background())

	if n := fakes.differ.computeCalls; n != 0 {
		t.Errorf("differ ran %d time(s) with yaml_files off", n)
	}
	for _, call := range fakes.applier.applyCalls {
		if len(call) != 0 {
			t.Errorf("applied files %v with yaml_files off", call)
		}
	}
	if n := len(fakes.registryApplier.applyPlanCalls); n != 1 {
		t.Errorf("registry apply calls = %d, want 1 - the other layers still run", n)
	}
	saves := fakes.applier.stateSaveCalls
	if len(saves) == 0 || saves[len(saves)-1].LastGoodSHA != "old" {
		t.Errorf("last_good_sha saved = %+v, want it left at old", saves)
	}
}

func TestApplyReviewedRefusesAPlanThatChanged(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.differ.changes = oneChange()
	r := fakes.reconciler(baseOpts())
	r.ReconcileNow(context.Background())
	reviewed := r.Status().PlanID
	if reviewed == "" {
		t.Fatal("a plan with a change has no plan id")
	}

	fakes.differ.changes = []differ.Change{{Path: "automations.yaml", Kind: "update", DiffText: "+a newer commit"}}
	r.ReconcileNow(context.Background())
	res := r.ApplyReviewed(context.Background(), reviewed)

	if res.OK || len(fakes.applier.applyCalls) != 0 {
		t.Errorf("result = %+v, apply calls = %d; want a refusal and nothing applied", res, len(fakes.applier.applyCalls))
	}

	res = r.ApplyReviewed(context.Background(), r.Status().PlanID)
	if len(fakes.applier.applyCalls) != 1 {
		t.Errorf("apply calls = %d with the current plan id, want 1 (result %+v)", len(fakes.applier.applyCalls), res)
	}
}

// Dropping a stale ownership record changes nothing in Home Assistant, and
// used to cost a full Supervisor backup all the same.
func TestAForgetOnlyPlanTakesNoSupervisorBackup(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.differ.changes = nil
	fakes.registries.desired = registries.Desired{}
	fakes.applier.state = applier.State{Manifest: []string{}, RegistryManaged: map[string]string{"area:gone": "gone"}}
	fakes.registries.planOps = []registries.RegOp{
		{RType: "area", Key: "gone", Kind: registries.KindForget, LiveID: "gone", DiffText: "stop tracking area:gone\n"},
	}
	fakes.registryApplier.applyResult = regapply.RegistryApplyResult{OK: true, Applied: []string{"forget area:gone"}}
	fakes.applier.applyResult = applier.Result{OK: true}
	fakes.applier.makeStashDirResult = t.TempDir()
	opts := baseOpts()
	opts.ReconcileRegistries = true
	r := fakes.reconciler(opts)
	r.ReconcileNow(context.Background())

	r.ApplyNow(context.Background(), true)

	if fakes.snapshot.backupCalls != 0 {
		t.Errorf("backups = %d for a forget-only plan, want 0", fakes.snapshot.backupCalls)
	}
	if n := len(fakes.registryApplier.applyPlanCalls); n != 1 {
		t.Errorf("registry apply calls = %d, want the forget applied", n)
	}
}

// A failure need not be the plan's fault (Home Assistant restarting
// mid-apply), and a hold that never expired stopped unattended syncing for
// good.
func TestAHeldPlanIsRetriedOnceTheHoldExpires(t *testing.T) {
	prev := heldPlanRetryAfter
	heldPlanRetryAfter = -time.Second
	t.Cleanup(func() { heldPlanRetryAfter = prev })
	fakes := failingPlanFakes()
	opts := baseOpts()
	opts.DryRun = false
	r := fakes.reconciler(opts)

	r.runCycle(context.Background())
	r.runCycle(context.Background())

	if got := len(fakes.applier.applyCalls); got != 2 {
		t.Errorf("apply calls = %d, want the expired hold retried", got)
	}
}

func TestARepeatFailureHoldsLonger(t *testing.T) {
	fakes := failingPlanFakes()
	opts := baseOpts()
	opts.DryRun = false
	r := fakes.reconciler(opts)
	r.runCycle(context.Background())
	first := r.heldPlanUntil

	r.ApplyNow(context.Background(), true) // the same plan fails again

	if got := r.heldPlanUntil.Sub(first); got < heldPlanRetryAfter/2 {
		t.Errorf("second hold ends %v after the first, want it roughly doubled", got)
	}
}
