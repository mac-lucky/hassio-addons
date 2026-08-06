package recon

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/applier"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/differ"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/flows"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/hacs"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/regapply"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/registries"
)

// declaredHacs is one manifest item in the shape hacs.LoadManifest returns.
func declaredHacs(id, repository string) map[string]any {
	return map[string]any{"id": id, "repository": repository, "category": "integration"}
}

func hacsInstallOp(key string) registries.RegOp {
	return registries.RegOp{
		Kind: hacs.KindCreate, RType: "hacs", Key: key,
		Params:   map[string]any{"repository": "owner/" + key, "category": "integration"},
		DiffText: "+repository: owner/" + key + "\n",
	}
}

// --- ReconcileNow(): wiring internal/hacs --------------------------------

func TestReconcileNowPlansHacsOpsWhenDeclared(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.hacs.desired = hacs.Desired{Repos: []map[string]any{declaredHacs("anker_solix", "thomluther/ha-anker-solix")}}
	fakes.hacs.planOps = []registries.RegOp{hacsInstallOp("anker_solix")}
	opts := baseOpts()
	opts.ReconcileHacs = true
	r := fakes.reconciler(opts)

	r.ReconcileNow(context.Background())

	status := r.Status()
	if status.State != StateDriftPending {
		t.Errorf("state = %q, want drift_pending", status.State)
	}
	want := []PendingRegOp{{
		RType: "hacs", Key: "anker_solix", Kind: "create",
		DiffText: "+repository: owner/anker_solix\n",
	}}
	if !reflect.DeepEqual(status.PendingRegistry, want) {
		t.Errorf("pending_registry = %+v, want %+v", status.PendingRegistry, want)
	}
	if fakes.registryApplier.fetchHacsLiveCalls != 1 {
		t.Errorf("fetch_hacs_live_calls = %d, want 1", fakes.registryApplier.fetchHacsLiveCalls)
	}
}

// The manifest, ownership records and reminders are what let the fetch
// read one repository instead of the whole HACS store.
func TestReconcileNowTellsTheHacsFetchWhatTheCycleAlreadyKnows(t *testing.T) {
	fakes := newReconcilerFakes()
	declared := declaredHacs("anker_solix", "thomluther/ha-anker-solix")
	fakes.hacs.desired = hacs.Desired{Repos: []map[string]any{declared}}
	fakes.applier.state.HacsManaged = map[string]string{"hacs:anker_solix": "1234"}
	fakes.applier.state.HacsRestartPending = []string{"anker_solix"}
	opts := baseOpts()
	opts.ReconcileHacs = true
	r := fakes.reconciler(opts)

	r.ReconcileNow(context.Background())

	if len(fakes.registryApplier.fetchHacsLiveRequests) == 0 {
		t.Fatal("fetch_hacs_live_requests is empty, want the cycle's own state handed down")
	}
	req := fakes.registryApplier.fetchHacsLiveRequests[len(fakes.registryApplier.fetchHacsLiveRequests)-1]
	if !reflect.DeepEqual(req.Desired.Repos, []map[string]any{declared}) {
		t.Errorf("request desired = %+v, want this cycle's manifest", req.Desired.Repos)
	}
	if req.Managed["hacs:anker_solix"] != "1234" {
		t.Errorf("request managed = %+v, want the recorded repository id", req.Managed)
	}
	if !reflect.DeepEqual(req.RestartPending, []string{"anker_solix"}) {
		t.Errorf("request restart pending = %v, want the standing reminder", req.RestartPending)
	}
}

// Off by default: a box that never asked for this has its HACS panel left
// unread.
func TestReconcileNowSkipsHacsWhenTheToggleIsOff(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.hacs.desired = hacs.Desired{Repos: []map[string]any{declaredHacs("anker_solix", "a/b")}}
	fakes.hacs.planOps = []registries.RegOp{hacsInstallOp("anker_solix")}
	r := fakes.reconciler(baseOpts())

	r.ReconcileNow(context.Background())

	if len(r.Status().PendingRegistry) != 0 {
		t.Errorf("pending_registry = %+v, want none", r.Status().PendingRegistry)
	}
	if fakes.registryApplier.fetchHacsLiveCalls != 0 {
		t.Errorf("fetch_hacs_live_calls = %d, want 0", fakes.registryApplier.fetchHacsLiveCalls)
	}
}

// Unlike every other layer, ownership records alone are not work: this one
// plans no delete or restore for an undeclared item.
func TestReconcileNowSkipsTheHacsFetchWithNothingDeclaredOrPending(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.applier.state.HacsManaged = map[string]string{"hacs:anker_solix": "1234"}
	opts := baseOpts()
	opts.ReconcileHacs = true
	r := fakes.reconciler(opts)

	r.ReconcileNow(context.Background())

	if fakes.registryApplier.fetchHacsLiveCalls != 0 {
		t.Errorf("fetch_hacs_live_calls = %d, want 0 - nothing is declared and no reminder stands",
			fakes.registryApplier.fetchHacsLiveCalls)
	}
	// The record stays: this layer never gives ownership up on its own.
	if got := r.Status().Managed.Hacs; !reflect.DeepEqual(got, []string{"anker_solix"}) {
		t.Errorf("managed hacs = %v, want the record kept", got)
	}
}

// A standing reminder is work even with an empty manifest: only a cycle
// reading the loaded components ever clears one.
func TestReconcileNowStillFetchesForAStandingRestartReminder(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.applier.state.HacsRestartPending = []string{"anker_solix"}
	fakes.registryApplier.fetchHacsLiveResult = regapply.HacsLive{Components: []string{"hacs", "anker_solix"}}
	opts := baseOpts()
	opts.ReconcileHacs = true
	r := fakes.reconciler(opts)

	r.ReconcileNow(context.Background())

	if fakes.registryApplier.fetchHacsLiveCalls != 1 {
		t.Fatalf("fetch_hacs_live_calls = %d, want 1", fakes.registryApplier.fetchHacsLiveCalls)
	}
	if got := r.Status().HacsRestartPending; len(got) != 0 {
		t.Errorf("hacs_restart_pending = %v, want it cleared by the loaded domain", got)
	}
}

// HACS not installed never clears on its own, so failing the cycle would
// stop the unrelated file sync forever. The layer skips itself and says so.
func TestReconcileNowSkipsTheLayerWhenHacsIsMissingRatherThanFailingTheCycle(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.hacs.desired = hacs.Desired{Repos: []map[string]any{declaredHacs("anker_solix", "a/b")}}
	fakes.differ.changes = []differ.Change{{Path: "automations.yaml", Kind: "update", DiffText: "+x"}}
	fakes.registryApplier.fetchHacsLiveErr = fmt.Errorf(
		"%w: home assistant does not know the 'hacs/repositories/list' command", regapply.ErrHacsNotInstalled)
	opts := baseOpts()
	opts.ReconcileHacs = true
	r := fakes.reconciler(opts)

	r.ReconcileNow(context.Background())

	status := r.Status()
	if status.State != StateDriftPending {
		t.Errorf("state = %q, want the cycle to have completed normally", status.State)
	}
	if status.LastError != "" {
		t.Errorf("last_error = %q, want none - the other layers ran", status.LastError)
	}
	// The file the cycle was really there for is still planned.
	if len(status.Pending) != 1 {
		t.Errorf("pending = %+v, want the file layer's own plan", status.Pending)
	}
	// A standing flag, because a feed line would scroll away.
	if !status.HacsUnavailable {
		t.Error("hacs_unavailable = false, want the standing health flag raised")
	}
	if !status.HasHealthWarnings() {
		t.Error("the health chip row is not raised by an unusable hacs layer")
	}
	if !hasEventContaining(status.Events, "hacs layer skipped") {
		t.Errorf("events = %+v, want one line saying the layer was skipped", status.Events)
	}
}

// One line on the way in, not one per interval: an event per cycle would
// fill the 200-entry feed with the same sentence inside a day.
func TestTheHacsMissingEventIsLoggedOnlyOnTheTransition(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.hacs.desired = hacs.Desired{Repos: []map[string]any{declaredHacs("anker_solix", "a/b")}}
	fakes.registryApplier.fetchHacsLiveErr = fmt.Errorf("%w: nope", regapply.ErrHacsNotInstalled)
	opts := baseOpts()
	opts.ReconcileHacs = true
	r := fakes.reconciler(opts)

	r.ReconcileNow(context.Background())
	r.ReconcileNow(context.Background())
	r.ReconcileNow(context.Background())

	if got := countEventsContaining(r.Status().Events, "hacs layer skipped"); got != 1 {
		t.Errorf("skipped events = %d, want exactly 1", got)
	}

	// Recovery gets its own line; the chip disappearing is the only other sign.
	fakes.registryApplier.fetchHacsLiveErr = nil
	r.ReconcileNow(context.Background())

	if r.Status().HacsUnavailable {
		t.Error("hacs_unavailable = true, want it cleared once HACS answered")
	}
	if !hasEventContaining(r.Status().Events, "hacs layer recovered") {
		t.Errorf("events = %+v, want the recovery said out loud", r.Status().Events)
	}
}

// Every other failure still ends the cycle: a timed-out fetch says nothing
// about whether the plan on screen is still true.
func TestReconcileNowStillFailsTheCycleOnAnOrdinaryHacsFetchFailure(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.hacs.desired = hacs.Desired{Repos: []map[string]any{declaredHacs("anker_solix", "a/b")}}
	fakes.registryApplier.fetchHacsLiveErr = errors.New("hacs command hacs/repositories/list failed: timeout")
	opts := baseOpts()
	opts.ReconcileHacs = true
	r := fakes.reconciler(opts)

	r.ReconcileNow(context.Background())

	status := r.Status()
	if status.State != StateError {
		t.Errorf("state = %q, want error", status.State)
	}
	if status.HacsUnavailable {
		t.Error("hacs_unavailable = true, want the health flag reserved for a missing HACS")
	}
}

// HACS downloads the code an integration's config entry needs, so its ops
// are planned - and shown - before the integrations layer's.
func TestReconcileNowPlansHacsBeforeIntegrations(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.hacs.desired = hacs.Desired{Repos: []map[string]any{declaredHacs("anker_solix", "a/b")}}
	fakes.hacs.planOps = []registries.RegOp{hacsInstallOp("anker_solix")}
	fakes.flows.desired = flows.Desired{
		Integrations: []map[string]any{{"id": "solix", "domain": "anker_solix", "title": "Solix", "data": map[string]any{}}},
	}
	fakes.flows.planOps = []registries.RegOp{{Kind: flows.KindCreate, RType: "integration", Key: "solix", DiffText: "+s"}}
	opts := baseOpts()
	opts.ReconcileHacs = true
	opts.ReconcileIntegrations = true
	r := fakes.reconciler(opts)

	r.ReconcileNow(context.Background())

	var rtypes []string
	for _, op := range r.Status().PendingRegistry {
		rtypes = append(rtypes, op.RType)
	}
	if !reflect.DeepEqual(rtypes, []string{"hacs", "integration"}) {
		t.Errorf("pending rtypes = %v, want the download planned before the entry that needs it", rtypes)
	}
}

// --- ApplyNow(): executing the layer -------------------------------------

// pendingHacsReconciler leaves one HACS op and one integration op pending.
func pendingHacsReconciler(t *testing.T, fakes *reconcilerFakes) *Reconciler {
	t.Helper()
	fakes.hacs.desired = hacs.Desired{Repos: []map[string]any{declaredHacs("anker_solix", "a/b")}}
	fakes.hacs.planOps = []registries.RegOp{hacsInstallOp("anker_solix")}
	fakes.flows.desired = flows.Desired{
		Integrations: []map[string]any{{"id": "solix", "domain": "anker_solix", "title": "Solix", "data": map[string]any{}}},
	}
	fakes.flows.planOps = []registries.RegOp{{Kind: flows.KindCreate, RType: "integration", Key: "solix", DiffText: "+s"}}
	opts := baseOpts()
	opts.DryRun = false
	opts.ReconcileHacs = true
	opts.ReconcileIntegrations = true
	r := fakes.reconciler(opts)
	r.ReconcileNow(context.Background())
	return r
}

func TestApplyNowAppliesHacsBeforeIntegrations(t *testing.T) {
	fakes := newReconcilerFakes()
	var order []string
	fakes.registryApplier.onApplyHacsPlan = func(map[string]string, map[string]map[string]any, *[]string) {
		order = append(order, "hacs")
	}
	fakes.registryApplier.onApplyFlowPlan = func(map[string]string, map[string]string, map[string]map[string]any, map[string]map[string]any) {
		order = append(order, "integrations")
	}
	r := pendingHacsReconciler(t, fakes)

	if result := r.ApplyNow(context.Background(), true); !result.OK {
		t.Fatalf("result = %+v", result)
	}

	if !reflect.DeepEqual(order, []string{"hacs", "integrations"}) {
		t.Errorf("layer order = %v, want the code downloaded before the entry that needs it", order)
	}
}

// The layer writes bookkeeping into the state it is handed; ApplyNow
// persists it in the same save as everything else.
func TestApplyNowPersistsHacsOwnershipAndRestartReminder(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.applier.state.HacsManaged = map[string]string{}
	fakes.applier.state.HacsAttempts = map[string]map[string]any{}
	fakes.registryApplier.onApplyHacsPlan = func(
		managed map[string]string, _ map[string]map[string]any, restartPending *[]string,
	) {
		managed["hacs:anker_solix"] = "1234"
		*restartPending = append(*restartPending, "anker_solix")
	}
	r := pendingHacsReconciler(t, fakes)

	r.ApplyNow(context.Background(), true)

	saved := fakes.applier.stateSaveCalls[len(fakes.applier.stateSaveCalls)-1]
	if saved.HacsManaged["hacs:anker_solix"] != "1234" {
		t.Errorf("saved hacs_managed = %+v, want the repository id recorded", saved.HacsManaged)
	}
	if !reflect.DeepEqual(saved.HacsRestartPending, []string{"anker_solix"}) {
		t.Errorf("saved hacs_restart_pending = %v, want the downloaded domain", saved.HacsRestartPending)
	}
	status := r.Status()
	if !reflect.DeepEqual(status.HacsRestartPending, []string{"anker_solix"}) {
		t.Errorf("status hacs_restart_pending = %v, want the reminder on the dashboard", status.HacsRestartPending)
	}
	if !reflect.DeepEqual(status.Managed.Hacs, []string{"anker_solix"}) {
		t.Errorf("managed hacs = %v, want the ownership on the dashboard", status.Managed.Hacs)
	}
}

// A failed download is remembered: the next plan refuses to repeat it, and
// the Recorded failures card renders the record.
func TestApplyNowRecordsAHacsFailureAsABlockedItem(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.applier.state.HacsAttempts = map[string]map[string]any{}
	fakes.registryApplier.applyHacsResult = regapply.RegistryApplyResult{
		OK: false, Error: "create hacs:anker_solix failed: no release tagged 9.9.9",
	}
	fakes.registryApplier.onApplyHacsPlan = func(
		_ map[string]string, attempts map[string]map[string]any, _ *[]string,
	) {
		attempts["hacs:anker_solix"] = map[string]any{"hash": "h", "error": "no release tagged 9.9.9"}
	}
	r := pendingHacsReconciler(t, fakes)

	result := r.ApplyNow(context.Background(), true)

	if result.OK {
		t.Fatalf("result = %+v, want ok=false", result)
	}
	want := []BlockedItem{{
		Key: "hacs:anker_solix", RType: "hacs", Name: "anker_solix", Error: "no release tagged 9.9.9",
	}}
	if got := r.Status().Blocked; !reflect.DeepEqual(got, want) {
		t.Errorf("blocked = %+v, want %+v", got, want)
	}
	// This layer has no rollback to claim, so the event names the layer and
	// never says "rolled back".
	var msg string
	for _, e := range r.Status().Events {
		if strings.Contains(e.Message, "hacs:") {
			msg = e.Message
		}
	}
	if !strings.Contains(msg, "hacs: create hacs:anker_solix failed") || strings.Contains(msg, "rolled back") {
		t.Errorf("event = %q, want the layer named and no rollback claimed", msg)
	}
	// The integrations layer never ran, so its own op is still pending.
	if fakes.registryApplier.applyFlowPlanCalls != nil {
		t.Errorf("apply_flow_plan_calls = %+v, want the layer after the failure skipped",
			fakes.registryApplier.applyFlowPlanCalls)
	}
}

// The escape hatch for a cause outside the repository (a GitHub rate
// limit): Retry clears the record and the next cycle plans it again.
func TestRetryBlockedClearsAHacsAttempt(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.applier.state.HacsAttempts = map[string]map[string]any{
		"hacs:anker_solix": {"hash": "h", "error": "github rate limit"},
	}
	fakes.hacs.desired = hacs.Desired{Repos: []map[string]any{declaredHacs("anker_solix", "a/b")}}
	fakes.hacs.planOps = []registries.RegOp{hacsInstallOp("anker_solix")}
	opts := baseOpts()
	opts.ReconcileHacs = true
	r := fakes.reconciler(opts)
	if len(r.Status().Blocked) != 1 {
		t.Fatalf("blocked = %+v, want the record hydrated at startup", r.Status().Blocked)
	}

	if err := r.RetryBlocked("hacs:anker_solix"); err != nil {
		t.Fatalf("RetryBlocked: %v", err)
	}

	saved := fakes.applier.stateSaveCalls[len(fakes.applier.stateSaveCalls)-1]
	if len(saved.HacsAttempts) != 0 {
		t.Errorf("saved hacs_attempts = %+v, want the record cleared", saved.HacsAttempts)
	}
	if got := r.Status().Blocked; len(got) != 0 {
		t.Errorf("blocked = %+v, want the row gone", got)
	}

	r.ReconcileNow(context.Background())

	if len(fakes.hacs.planCalls) != 1 {
		t.Fatalf("plan_calls = %d, want the item planned again", len(fakes.hacs.planCalls))
	}
	if len(fakes.hacs.planCalls[0].attempts) != 0 {
		t.Errorf("attempts handed to the plan = %+v, want none", fakes.hacs.planCalls[0].attempts)
	}
}

// The reminder's lifecycle: an apply raises it, and the first cycle that
// sees the domain loaded clears it without needing an apply of its own.
func TestRestartReminderIsRaisedByAnApplyAndClearedByACycle(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.applier.state.HacsManaged = map[string]string{}
	fakes.applier.state.HacsAttempts = map[string]map[string]any{}
	fakes.registryApplier.onApplyHacsPlan = func(
		_ map[string]string, _ map[string]map[string]any, restartPending *[]string,
	) {
		*restartPending = append(*restartPending, "anker_solix")
	}
	r := pendingHacsReconciler(t, fakes)

	r.ApplyNow(context.Background(), true)

	if got := r.Status().HacsRestartPending; !reflect.DeepEqual(got, []string{"anker_solix"}) {
		t.Fatalf("hacs_restart_pending = %v, want the reminder raised by the download", got)
	}

	// Restarted since: the domain is loaded and the layer plans nothing.
	fakes.registryApplier.fetchHacsLiveResult = regapply.HacsLive{Components: []string{"hacs", "anker_solix"}}
	fakes.hacs.planOps = nil
	fakes.registryApplier.onApplyHacsPlan = nil
	r.ReconcileNow(context.Background())

	if got := r.Status().HacsRestartPending; len(got) != 0 {
		t.Errorf("hacs_restart_pending = %v, want the reminder cleared", got)
	}
	// The next apply writes it down, so it survives an add-on restart.
	r.ApplyNow(context.Background(), true)
	saved := fakes.applier.stateSaveCalls[len(fakes.applier.stateSaveCalls)-1]
	if len(saved.HacsRestartPending) != 0 {
		t.Errorf("saved hacs_restart_pending = %v, want it cleared on disk too", saved.HacsRestartPending)
	}
}

// With the layer off, the cycle never looked at the loaded components, so
// the apply must not wipe reminders it knows nothing about.
func TestApplyNowLeavesTheReminderAloneWhenTheLayerDidNotRun(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.applier.state.HacsRestartPending = []string{"anker_solix"}
	fakes.differ.changes = []differ.Change{{Path: "automations.yaml", Kind: "update", DiffText: "+x"}}
	opts := baseOpts()
	opts.DryRun = false
	r := fakes.reconciler(opts)
	r.ReconcileNow(context.Background())

	r.ApplyNow(context.Background(), true)

	saved := fakes.applier.stateSaveCalls[len(fakes.applier.stateSaveCalls)-1]
	if !reflect.DeepEqual(saved.HacsRestartPending, []string{"anker_solix"}) {
		t.Errorf("saved hacs_restart_pending = %v, want it untouched", saved.HacsRestartPending)
	}
}

// PruneStashDirs keeps five, so a stash for a plan that touches nothing
// evicts a real rollback point. An adopt has nothing to stash.
func TestApplyNowAllocatesNoStashForAnAdoptOnlyPlan(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.hacs.desired = hacs.Desired{Repos: []map[string]any{declaredHacs("adaptive_lighting", "a/b")}}
	fakes.hacs.planOps = []registries.RegOp{{
		Kind: hacs.KindUpdate, RType: "hacs", Key: "adaptive_lighting", LiveID: "5678",
		Params:   map[string]any{"adopt": true, "repository": "a/b", "repository_id": "5678"},
		DiffText: "adopted...",
	}}
	fakes.applier.applyResult = applier.Result{OK: true}
	opts := baseOpts()
	opts.DryRun = false
	opts.ReconcileHacs = true
	r := fakes.reconciler(opts)
	r.ReconcileNow(context.Background())

	result := r.ApplyNow(context.Background(), true)

	if !result.OK {
		t.Fatalf("result = %+v", result)
	}
	if fakes.applier.makeStashDirCalls != 0 {
		t.Errorf("make_stash_dir_calls = %d, want none for a plan that touches nothing",
			fakes.applier.makeStashDirCalls)
	}
	// The ownership still had to be recorded.
	if len(fakes.registryApplier.applyHacsPlanCalls) != 1 {
		t.Errorf("apply_hacs_plan_calls = %d, want 1", len(fakes.registryApplier.applyHacsPlanCalls))
	}
}

// A download does need one: the gate is whether the plan can do something
// whose bookkeeping must be persistable. Registry-only, so no file stash.
func TestApplyNowStillAllocatesAStashForADownload(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.applier.applyResult = applier.Result{OK: true}
	r := pendingHacsReconciler(t, fakes)

	r.ApplyNow(context.Background(), true)

	if fakes.applier.makeStashDirCalls == 0 {
		t.Error("make_stash_dir_calls = 0, want a stash allocated for a plan that changes the box")
	}
}

// The mirror is rebuilt from disk, which still carries a reminder this
// cycle retired but no apply has persisted yet.
func TestAnUnrelatedOperationDoesNotResurrectAClearedReminder(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.applier.state.HacsRestartPending = []string{"anker_solix"}
	fakes.applier.state.HacsAttempts = map[string]map[string]any{
		"hacs:broken": {"hash": "h", "error": "github rate limit"},
	}
	fakes.registryApplier.fetchHacsLiveResult = regapply.HacsLive{Components: []string{"hacs", "anker_solix"}}
	opts := baseOpts()
	opts.ReconcileHacs = true
	r := fakes.reconciler(opts)

	r.ReconcileNow(context.Background())
	if got := r.Status().HacsRestartPending; len(got) != 0 {
		t.Fatalf("hacs_restart_pending = %v, want the cycle to have cleared it", got)
	}

	// Nothing to do with HACS, and it reloads the state from disk.
	if err := r.RetryBlocked("hacs:broken"); err != nil {
		t.Fatalf("RetryBlocked: %v", err)
	}

	if got := r.Status().HacsRestartPending; len(got) != 0 {
		t.Errorf("hacs_restart_pending = %v, want the cleared reminder to stay cleared", got)
	}
}
