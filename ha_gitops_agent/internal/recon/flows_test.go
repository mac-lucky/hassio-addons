package recon

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/addonopts"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/applier"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/flows"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/regapply"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/registries"
)

// --- ReconcileNow(): wiring internal/flows -------------------------------

func TestReconcileNowPlansIntegrationOpsWhenDeclared(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.flows.desired = flows.Desired{
		Integrations: []map[string]any{{"id": "workday_main", "domain": "workday", "title": "Workday", "data": map[string]any{}}},
	}
	fakes.flows.planOps = []registries.RegOp{
		{Kind: flows.KindCreate, RType: "integration", Key: "workday_main", DiffText: "+domain: workday\n"},
	}
	opts := baseOpts()
	opts.ReconcileIntegrations = true
	r := fakes.reconciler(opts)

	r.ReconcileNow(context.Background())

	status := r.Status()
	if status.State != StateDriftPending {
		t.Errorf("state = %q, want drift_pending", status.State)
	}
	want := []PendingRegOp{{RType: "integration", Key: "workday_main", Kind: "create", DiffText: "+domain: workday\n"}}
	if !reflect.DeepEqual(status.PendingRegistry, want) {
		t.Errorf("pending_registry = %+v, want %+v", status.PendingRegistry, want)
	}
	if len(fakes.flows.planCalls) != 1 {
		t.Fatalf("plan_calls = %d, want 1", len(fakes.flows.planCalls))
	}
	if fakes.registryApplier.fetchIntegrationEntriesCalls != 1 {
		t.Errorf("fetch_integration_entries_calls = %d, want 1", fakes.registryApplier.fetchIntegrationEntriesCalls)
	}
}

func TestReconcileNowIntegrationsIsIndependentOfOtherToggles(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.flows.desired = flows.Desired{
		Integrations: []map[string]any{{"id": "x", "domain": "moon", "title": "Moon", "data": map[string]any{}}},
	}
	fakes.flows.planOps = []registries.RegOp{{Kind: flows.KindCreate, RType: "integration", Key: "x", DiffText: "+x"}}
	opts := baseOpts()
	opts.ReconcileRegistries = false
	opts.ReconcileDashboards = false
	opts.ReconcileAddonOptions = false
	opts.ReconcileIntegrations = true
	r := fakes.reconciler(opts)

	r.ReconcileNow(context.Background())

	status := r.Status()
	if len(status.PendingRegistry) != 1 {
		t.Fatalf("pending_registry = %+v, want 1 entry", status.PendingRegistry)
	}
}

func TestReconcileNowSkipsIntegrationFetchWhenToggleOff(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.flows.desired = flows.Desired{
		Integrations: []map[string]any{{"id": "x", "domain": "moon", "title": "Moon", "data": map[string]any{}}},
	}
	opts := baseOpts()
	opts.ReconcileIntegrations = false
	r := fakes.reconciler(opts)

	r.ReconcileNow(context.Background())

	if len(fakes.flows.loadManifestCalls) != 0 {
		t.Errorf("load_manifest_calls = %v, want none", fakes.flows.loadManifestCalls)
	}
	if fakes.registryApplier.fetchIntegrationEntriesCalls != 0 {
		t.Errorf("fetch_integration_entries_calls = %d, want 0", fakes.registryApplier.fetchIntegrationEntriesCalls)
	}
}

func TestReconcileNowSkipsIntegrationFetchWhenNoWork(t *testing.T) {
	fakes := newReconcilerFakes()
	opts := baseOpts()
	opts.ReconcileIntegrations = true
	r := fakes.reconciler(opts)

	r.ReconcileNow(context.Background())

	if len(fakes.flows.loadManifestCalls) != 1 {
		t.Errorf("load_manifest_calls = %v, want a single call regardless", fakes.flows.loadManifestCalls)
	}
	if fakes.registryApplier.fetchIntegrationEntriesCalls != 0 {
		t.Errorf("fetch_integration_entries_calls = %d, want 0 (no work, never fetched)", fakes.registryApplier.fetchIntegrationEntriesCalls)
	}
	if len(fakes.flows.planCalls) != 0 {
		t.Errorf("plan_calls = %d, want 0", len(fakes.flows.planCalls))
	}
}

func TestReconcileNowFetchesIntegrationsWhenOnlyManagedExists(t *testing.T) {
	// The "manifest emptied but still managed" case: nothing declared,
	// but a managed integration may still need deleting.
	fakes := newReconcilerFakes()
	fakes.applier.state.IntegrationManaged = map[string]string{"integration:workday_main": "abc123"}
	opts := baseOpts()
	opts.ReconcileIntegrations = true
	r := fakes.reconciler(opts)

	r.ReconcileNow(context.Background())

	if fakes.registryApplier.fetchIntegrationEntriesCalls != 1 {
		t.Fatalf("fetch_integration_entries_calls = %d, want 1", fakes.registryApplier.fetchIntegrationEntriesCalls)
	}
	if len(fakes.flows.planCalls) != 1 {
		t.Fatalf("plan_calls = %d, want 1", len(fakes.flows.planCalls))
	}
	if !reflect.DeepEqual(fakes.flows.planCalls[0].managed, fakes.applier.state.IntegrationManaged) {
		t.Errorf("plan managed = %+v, want %+v", fakes.flows.planCalls[0].managed, fakes.applier.state.IntegrationManaged)
	}
}

func TestReconcileNowPassesIntegrationAttemptsToPlan(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.applier.state.IntegrationAttempts = map[string]map[string]any{
		"integration:esphome_main": {"hash": "abc", "error": "flow step 'user' has no declared data"},
	}
	fakes.flows.desired = flows.Desired{
		Integrations: []map[string]any{{"id": "workday_main", "domain": "workday", "title": "Workday", "data": map[string]any{}}},
	}
	opts := baseOpts()
	opts.ReconcileIntegrations = true
	r := fakes.reconciler(opts)

	r.ReconcileNow(context.Background())

	if len(fakes.flows.planCalls) != 1 {
		t.Fatalf("plan_calls = %d, want 1", len(fakes.flows.planCalls))
	}
	if !reflect.DeepEqual(fakes.flows.planCalls[0].attempts, fakes.applier.state.IntegrationAttempts) {
		t.Errorf("plan attempts = %+v, want %+v", fakes.flows.planCalls[0].attempts, fakes.applier.state.IntegrationAttempts)
	}
}

func TestReconcileNowIntegrationManifestErrorSurfacesVerbatim(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.flows.manifestErr = &flows.ManifestError{Problems: []string{"integrations.yaml: integration 'x' has an invalid or missing 'domain'"}}
	opts := baseOpts()
	opts.ReconcileIntegrations = true
	r := fakes.reconciler(opts)

	r.ReconcileNow(context.Background())

	status := r.Status()
	if status.State != StateError {
		t.Errorf("state = %q, want error", status.State)
	}
	if !strings.Contains(status.LastError, "invalid or missing 'domain'") {
		t.Errorf("last_error = %q", status.LastError)
	}
}

func TestReconcileNowIntegrationsRunAfterAddonOptionsOnlyIfItSucceeded(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.addonOpts.manifestErr = errors.New("addons.yaml: boom")
	opts := baseOpts()
	opts.ReconcileAddonOptions = true
	opts.ReconcileIntegrations = true
	r := fakes.reconciler(opts)

	r.ReconcileNow(context.Background())

	if len(fakes.flows.loadManifestCalls) != 0 {
		t.Errorf("load_manifest_calls = %v, want none - the cycle must have failed before reaching integrations", fakes.flows.loadManifestCalls)
	}
	status := r.Status()
	if status.State != StateError {
		t.Errorf("state = %q, want error", status.State)
	}
}

func TestReconcileNowMixesIntegrationOpsIntoOnePendingList(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.addonOpts.desired = addonopts.Desired{
		Addons: []map[string]any{{"slug": "x", "options": map[string]any{"a": 1}, "restart_on_change": true}},
	}
	fakes.addonOpts.planOps = []registries.RegOp{{Kind: addonopts.KindUpdate, RType: "addon", Key: "x", DiffText: "+z"}}
	fakes.flows.desired = flows.Desired{
		Integrations: []map[string]any{{"id": "y", "domain": "moon", "title": "Moon", "data": map[string]any{}}},
	}
	fakes.flows.planOps = []registries.RegOp{{Kind: flows.KindCreate, RType: "integration", Key: "y", DiffText: "+y"}}
	opts := baseOpts()
	opts.ReconcileAddonOptions = true
	opts.ReconcileIntegrations = true
	r := fakes.reconciler(opts)

	r.ReconcileNow(context.Background())

	status := r.Status()
	if len(status.PendingRegistry) != 2 {
		t.Fatalf("pending_registry = %+v, want 2 entries", status.PendingRegistry)
	}
}

// --- ApplyNow(): integrations run after addon options, only if it succeeded

func TestApplyNowAppliesIntegrationsAfterAddonOptionsInOrder(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.addonOpts.desired = addonopts.Desired{
		Addons: []map[string]any{{"slug": "x", "options": map[string]any{"a": 1}, "restart_on_change": true}},
	}
	fakes.addonOpts.planOps = []registries.RegOp{{Kind: addonopts.KindUpdate, RType: "addon", Key: "x", DiffText: "+a"}}
	fakes.flows.desired = flows.Desired{
		Integrations: []map[string]any{{"id": "y", "domain": "moon", "title": "Moon", "data": map[string]any{}}},
	}
	fakes.flows.planOps = []registries.RegOp{{Kind: flows.KindCreate, RType: "integration", Key: "y", DiffText: "+y"}}
	fakes.registryApplier.applyAddonResult = regapply.RegistryApplyResult{OK: true, Applied: []string{"update addon:x"}}
	fakes.registryApplier.applyFlowResult = regapply.RegistryApplyResult{OK: true, Applied: []string{"create integration:y"}}
	opts := baseOpts()
	opts.DryRun = false
	opts.ReconcileAddonOptions = true
	opts.ReconcileIntegrations = true
	r := fakes.reconciler(opts)
	r.ReconcileNow(context.Background())

	result := r.ApplyNow(context.Background(), true)

	if !result.OK {
		t.Fatalf("result = %+v", result)
	}
	if len(fakes.registryApplier.applyAddonPlanCalls) != 1 {
		t.Fatalf("apply_addon_plan_calls = %+v", fakes.registryApplier.applyAddonPlanCalls)
	}
	if len(fakes.registryApplier.applyFlowPlanCalls) != 1 || fakes.registryApplier.applyFlowPlanCalls[0].ops[0].RType != "integration" {
		t.Fatalf("apply_flow_plan_calls = %+v", fakes.registryApplier.applyFlowPlanCalls)
	}
	stash1 := fakes.registryApplier.applyAddonPlanCalls[0].stashDir
	stash2 := fakes.registryApplier.applyFlowPlanCalls[0].stashDir
	if stash1 != stash2 {
		t.Errorf("stash dirs differ: %q, %q", stash1, stash2)
	}
	status := r.Status()
	if status.State != StateInSync {
		t.Errorf("state = %q, want in_sync", status.State)
	}
}

func TestApplyNowIntegrationFailureDoesNotUndoAddonSuccess(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.addonOpts.desired = addonopts.Desired{
		Addons: []map[string]any{{"slug": "x", "options": map[string]any{"a": 1}, "restart_on_change": true}},
	}
	fakes.addonOpts.planOps = []registries.RegOp{{Kind: addonopts.KindUpdate, RType: "addon", Key: "x", DiffText: "+a"}}
	fakes.flows.desired = flows.Desired{
		Integrations: []map[string]any{{"id": "y", "domain": "moon", "title": "Moon", "data": map[string]any{}}},
	}
	fakes.flows.planOps = []registries.RegOp{{Kind: flows.KindCreate, RType: "integration", Key: "y", DiffText: "+y"}}
	fakes.registryApplier.applyAddonResult = regapply.RegistryApplyResult{OK: true, Applied: []string{"update addon:x"}}
	// RolledBack is always false here: per-op isolation rolls nothing back
	// (see regapply/flows.go, ApplyFlowPlan).
	fakes.registryApplier.applyFlowResult = regapply.RegistryApplyResult{
		OK: false, Error: "create integration:y failed: boom", RolledBack: false,
	}
	opts := baseOpts()
	opts.DryRun = false
	opts.ReconcileAddonOptions = true
	opts.ReconcileIntegrations = true
	r := fakes.reconciler(opts)
	r.ReconcileNow(context.Background())

	result := r.ApplyNow(context.Background(), true)

	if result.OK {
		t.Errorf("result = %+v, want ok=false", result)
	}
	if !strings.Contains(result.Error, "create integration:y failed") {
		t.Errorf("result.Error = %q", result.Error)
	}
	if len(fakes.registryApplier.applyAddonPlanCalls) != 1 {
		t.Errorf("apply_addon_plan_calls = %d, want 1 (the addon layer ran and succeeded)", len(fakes.registryApplier.applyAddonPlanCalls))
	}
	status := r.Status()
	if status.State != StateError {
		t.Errorf("state = %q, want error", status.State)
	}
}

func TestApplyNowEventLogUsesIndependentWordingForIntegrationsFailure(t *testing.T) {
	// Under per-op isolation a failed apply can still carry a non-empty
	// Applied: count what stayed applied, never say "rolled back".
	fakes := newReconcilerFakes()
	fakes.flows.desired = flows.Desired{
		Integrations: []map[string]any{{"id": "y", "domain": "moon", "title": "Moon", "data": map[string]any{}}},
	}
	fakes.flows.planOps = []registries.RegOp{{Kind: flows.KindCreate, RType: "integration", Key: "y", DiffText: "+y"}}
	fakes.registryApplier.applyFlowResult = regapply.RegistryApplyResult{
		OK: false, Applied: []string{"create integration:y"}, Error: "create integration:z failed: boom", RolledBack: false,
	}
	opts := baseOpts()
	opts.DryRun = false
	opts.ReconcileIntegrations = true
	r := fakes.reconciler(opts)
	r.ReconcileNow(context.Background())

	r.ApplyNow(context.Background(), true)

	var msg string
	for _, e := range r.Status().Events {
		if strings.Contains(e.Message, "integrations:") {
			msg = e.Message
		}
	}
	if !strings.Contains(msg, "integrations: create integration:z failed: boom") {
		t.Errorf("event %q, want the integrations-specific wording naming the failed op", msg)
	}
	if strings.Contains(msg, "rolled back") {
		t.Errorf("event %q, want no rollback wording - per-op isolation never rolls anything back", msg)
	}
	if !strings.Contains(msg, "1 registry change(s) stayed applied") {
		t.Errorf("event %q, want it to report the sibling op that stayed applied", msg)
	}
}

func TestApplyNowIntegrationsNeverRunWhenAddonApplyItselfFails(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.addonOpts.desired = addonopts.Desired{
		Addons: []map[string]any{{"slug": "x", "options": map[string]any{"a": 1}, "restart_on_change": true}},
	}
	fakes.addonOpts.planOps = []registries.RegOp{{Kind: addonopts.KindUpdate, RType: "addon", Key: "x", DiffText: "+a"}}
	fakes.flows.desired = flows.Desired{
		Integrations: []map[string]any{{"id": "y", "domain": "moon", "title": "Moon", "data": map[string]any{}}},
	}
	fakes.flows.planOps = []registries.RegOp{{Kind: flows.KindCreate, RType: "integration", Key: "y", DiffText: "+y"}}
	fakes.registryApplier.applyAddonResult = regapply.RegistryApplyResult{
		OK: false, Error: "update addon:x failed: boom", RolledBack: true,
	}
	opts := baseOpts()
	opts.DryRun = false
	opts.ReconcileAddonOptions = true
	opts.ReconcileIntegrations = true
	r := fakes.reconciler(opts)
	r.ReconcileNow(context.Background())

	r.ApplyNow(context.Background(), true)

	if len(fakes.registryApplier.applyFlowPlanCalls) != 0 {
		t.Errorf("apply_flow_plan_calls = %+v, want none", fakes.registryApplier.applyFlowPlanCalls)
	}
}

func TestApplyNowOnlyIntegrationOpsNeverCallsOtherApplyPlans(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.flows.desired = flows.Desired{
		Integrations: []map[string]any{{"id": "y", "domain": "moon", "title": "Moon", "data": map[string]any{}}},
	}
	fakes.flows.planOps = []registries.RegOp{{Kind: flows.KindCreate, RType: "integration", Key: "y", DiffText: "+y"}}
	fakes.registryApplier.applyFlowResult = regapply.RegistryApplyResult{OK: true, Applied: []string{"create integration:y"}}
	opts := baseOpts()
	opts.DryRun = false
	opts.ReconcileIntegrations = true
	r := fakes.reconciler(opts)
	r.ReconcileNow(context.Background())

	result := r.ApplyNow(context.Background(), true)

	if !result.OK {
		t.Fatalf("result = %+v", result)
	}
	if len(fakes.registryApplier.applyPlanCalls) != 0 {
		t.Errorf("apply_plan_calls = %+v, want none", fakes.registryApplier.applyPlanCalls)
	}
	if len(fakes.registryApplier.applyEntityPlanCalls) != 0 {
		t.Errorf("apply_entity_plan_calls = %+v, want none", fakes.registryApplier.applyEntityPlanCalls)
	}
	if len(fakes.registryApplier.applyDashboardPlanCalls) != 0 {
		t.Errorf("apply_dashboard_plan_calls = %+v, want none", fakes.registryApplier.applyDashboardPlanCalls)
	}
	if len(fakes.registryApplier.applyAddonPlanCalls) != 0 {
		t.Errorf("apply_addon_plan_calls = %+v, want none", fakes.registryApplier.applyAddonPlanCalls)
	}
	if len(fakes.registryApplier.applyFlowPlanCalls) != 1 {
		t.Errorf("apply_flow_plan_calls = %+v, want 1", fakes.registryApplier.applyFlowPlanCalls)
	}
}

func TestApplyNowKeepsSkippedIntegrationErrorsPending(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.flows.desired = flows.Desired{
		Integrations: []map[string]any{{"id": "y", "domain": "moon", "title": "Moon", "data": map[string]any{}}},
	}
	fakes.flows.planOps = []registries.RegOp{{Kind: flows.KindCreate, RType: "integration", Key: "y", DiffText: "+y"}}
	fakes.registryApplier.applyFlowResult = regapply.RegistryApplyResult{
		OK: true,
		SkippedErrors: []registries.RegOp{
			{Kind: registries.KindError, RType: "integration", Key: "other", Error: "declared data changed after it was created"},
		},
	}
	opts := baseOpts()
	opts.DryRun = false
	opts.ReconcileIntegrations = true
	r := fakes.reconciler(opts)
	r.ReconcileNow(context.Background())

	r.ApplyNow(context.Background(), true)

	status := r.Status()
	if len(status.PendingRegistry) != 1 || status.PendingRegistry[0].Key != "other" {
		t.Errorf("pending_registry = %+v", status.PendingRegistry)
	}
	if status.State != StateDriftPending {
		t.Errorf("state = %q, want drift_pending", status.State)
	}
}

func TestApplyNowPassesManagedHashesAndDataThrough(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.applier.state.IntegrationManaged = map[string]string{"integration:existing": "abc123"}
	fakes.applier.state.IntegrationHashes = map[string]string{"integration:existing": "h"}
	fakes.applier.state.IntegrationData = map[string]map[string]any{"integration:existing": {"user": map[string]any{}}}
	fakes.applier.state.IntegrationAttempts = map[string]map[string]any{"integration:broken": {"hash": "h2", "error": "boom"}}
	fakes.flows.desired = flows.Desired{
		Integrations: []map[string]any{{"id": "y", "domain": "moon", "title": "Moon", "data": map[string]any{}}},
	}
	fakes.flows.planOps = []registries.RegOp{{Kind: flows.KindCreate, RType: "integration", Key: "y", DiffText: "+y"}}
	fakes.registryApplier.applyFlowResult = regapply.RegistryApplyResult{OK: true, Applied: []string{"create integration:y"}}
	opts := baseOpts()
	opts.DryRun = false
	opts.ReconcileIntegrations = true
	r := fakes.reconciler(opts)
	r.ReconcileNow(context.Background())

	r.ApplyNow(context.Background(), true)

	if len(fakes.registryApplier.applyFlowPlanCalls) != 1 {
		t.Fatalf("apply_flow_plan_calls = %+v", fakes.registryApplier.applyFlowPlanCalls)
	}
	call := fakes.registryApplier.applyFlowPlanCalls[0]
	if !reflect.DeepEqual(call.managed, fakes.applier.state.IntegrationManaged) {
		t.Errorf("managed = %+v, want %+v", call.managed, fakes.applier.state.IntegrationManaged)
	}
	if !reflect.DeepEqual(call.hashes, fakes.applier.state.IntegrationHashes) {
		t.Errorf("hashes = %+v, want %+v", call.hashes, fakes.applier.state.IntegrationHashes)
	}
	if !reflect.DeepEqual(call.dataSnapshots, fakes.applier.state.IntegrationData) {
		t.Errorf("data_snapshots = %+v, want %+v", call.dataSnapshots, fakes.applier.state.IntegrationData)
	}
	if !reflect.DeepEqual(call.attempts, fakes.applier.state.IntegrationAttempts) {
		t.Errorf("attempts = %+v, want %+v", call.attempts, fakes.applier.state.IntegrationAttempts)
	}
}

// --- Rollback(): integration stash, independent of the other two --------

func TestRollbackPassesIntegrationBookkeepingThrough(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.applier.state.IntegrationManaged = map[string]string{"integration:y": "entryY"}
	fakes.applier.state.IntegrationHashes = map[string]string{"integration:y": "h"}
	fakes.applier.state.IntegrationData = map[string]map[string]any{"integration:y": {}}
	fakes.applier.applyResult = applier.Result{OK: true, StashDir: t.TempDir()}
	opts := baseOpts()
	opts.DryRun = false
	r := fakes.reconciler(opts)
	r.ApplyNow(context.Background(), true)
	writeFile(t, fakes.applier.applyResult.StashDir+"/integration_stash.json", `{"ops": []}`)

	r.Rollback(context.Background())

	if len(fakes.registryApplier.rollbackFlowManaged) != 1 {
		t.Fatalf("rollback_flow_managed = %+v", fakes.registryApplier.rollbackFlowManaged)
	}
	if !reflect.DeepEqual(fakes.registryApplier.rollbackFlowManaged[0], fakes.applier.state.IntegrationManaged) {
		t.Errorf("rollback managed = %+v, want %+v", fakes.registryApplier.rollbackFlowManaged[0], fakes.applier.state.IntegrationManaged)
	}
	if !reflect.DeepEqual(fakes.registryApplier.rollbackFlowHashes[0], fakes.applier.state.IntegrationHashes) {
		t.Errorf("rollback hashes = %+v, want %+v", fakes.registryApplier.rollbackFlowHashes[0], fakes.applier.state.IntegrationHashes)
	}
	if !reflect.DeepEqual(fakes.registryApplier.rollbackFlowDataSnaps[0], fakes.applier.state.IntegrationData) {
		t.Errorf("rollback data = %+v, want %+v", fakes.registryApplier.rollbackFlowDataSnaps[0], fakes.applier.state.IntegrationData)
	}
}

func TestRollbackSkipsIntegrationRollbackWhenNoIntegrationStash(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.applier.applyResult = applier.Result{OK: true, StashDir: t.TempDir()}
	opts := baseOpts()
	opts.DryRun = false
	r := fakes.reconciler(opts)
	r.ApplyNow(context.Background(), true)
	// No integration_stash.json written this time.

	r.Rollback(context.Background())

	if len(fakes.registryApplier.rollbackFlowCalls) != 0 {
		t.Errorf("rollback_flow_calls = %+v, want none", fakes.registryApplier.rollbackFlowCalls)
	}
}

func TestRollbackRunsIntegrationRollbackEvenWhenNoRegistryOrAddonStash(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.applier.applyResult = applier.Result{OK: true, StashDir: t.TempDir()}
	opts := baseOpts()
	opts.DryRun = false
	r := fakes.reconciler(opts)
	r.ApplyNow(context.Background(), true)
	writeFile(t, fakes.applier.applyResult.StashDir+"/integration_stash.json", `{"ops": []}`)

	result := r.Rollback(context.Background())

	if !result.OK {
		t.Fatalf("result = %+v", result)
	}
	if len(fakes.registryApplier.rollbackFlowCalls) != 1 {
		t.Errorf("rollback_flow_calls = %+v, want 1", fakes.registryApplier.rollbackFlowCalls)
	}
	if len(fakes.registryApplier.rollbackCalls) != 0 {
		t.Errorf("rollback_calls (registry) = %+v, want none", fakes.registryApplier.rollbackCalls)
	}
	if len(fakes.registryApplier.rollbackAddonCalls) != 0 {
		t.Errorf("rollback_addon_calls = %+v, want none", fakes.registryApplier.rollbackAddonCalls)
	}
}

func TestRollbackFailureOfIntegrationLayerFailsOverallResult(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.applier.applyResult = applier.Result{OK: true, StashDir: t.TempDir()}
	fakes.registryApplier.rollbackFlowResult = regapply.RegistryApplyResult{OK: false, Error: "boom", RolledBack: false}
	opts := baseOpts()
	opts.DryRun = false
	r := fakes.reconciler(opts)
	r.ApplyNow(context.Background(), true)
	writeFile(t, fakes.applier.applyResult.StashDir+"/integration_stash.json", `{"ops": []}`)

	result := r.Rollback(context.Background())

	if result.OK {
		t.Errorf("result = %+v, want ok=false", result)
	}
	if !strings.Contains(result.Error, "boom") {
		t.Errorf("result.Error = %q", result.Error)
	}
}

// --- status: pending_integration_ops -------------------------------------

func TestPushStatusReportsPendingIntegrationOpsSeparately(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.addonOpts.desired = addonopts.Desired{
		Addons: []map[string]any{{"slug": "x", "options": map[string]any{"a": 1}, "restart_on_change": true}},
	}
	fakes.addonOpts.planOps = []registries.RegOp{{Kind: addonopts.KindUpdate, RType: "addon", Key: "x", DiffText: "+y"}}
	fakes.flows.desired = flows.Desired{
		Integrations: []map[string]any{{"id": "y", "domain": "moon", "title": "Moon", "data": map[string]any{}}},
	}
	fakes.flows.planOps = []registries.RegOp{{Kind: flows.KindCreate, RType: "integration", Key: "y", DiffText: "+y"}}
	opts := baseOpts()
	opts.ReconcileAddonOptions = true
	opts.ReconcileIntegrations = true
	r := fakes.reconciler(opts)

	r.ReconcileNow(context.Background())

	var pushed map[string]any
	for _, call := range fakes.status.pushes {
		pushed = call.attrs
	}
	if pushed["pending_addon_ops"] != 1 {
		t.Errorf("pending_addon_ops = %v, want 1", pushed["pending_addon_ops"])
	}
	if pushed["pending_integration_ops"] != 1 {
		t.Errorf("pending_integration_ops = %v, want 1", pushed["pending_integration_ops"])
	}
}
