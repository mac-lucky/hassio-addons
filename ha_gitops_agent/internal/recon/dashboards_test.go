package recon

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/applier"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/dashboards"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/entities"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/regapply"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/registries"
)

// --- ReconcileNow(): wiring internal/dashboards -------------------------

func TestReconcileNowPlansDashboardOpsWhenDashboardsDeclared(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.dashboards.desired = dashboards.Desired{
		Dashboards: []map[string]any{{"id": "home", "title": "Home", "config": "home.yaml"}},
		Content:    map[string]dashboards.DashboardContent{"home": {Data: map[string]any{"views": []any{}}}},
	}
	fakes.dashboards.planOps = []registries.RegOp{
		{Kind: dashboards.KindCreate, RType: "dashboard", Key: "home", DiffText: "+title: 'Home'\n"},
	}
	opts := baseOpts()
	opts.ReconcileDashboards = true
	r := fakes.reconciler(opts)

	r.ReconcileNow(context.Background())

	status := r.Status()
	if status.State != StateDriftPending {
		t.Errorf("state = %q, want drift_pending", status.State)
	}
	want := []PendingRegOp{{RType: "dashboard", Key: "home", Kind: "create", DiffText: "+title: 'Home'\n"}}
	if !reflect.DeepEqual(status.PendingRegistry, want) {
		t.Errorf("pending_registry = %+v, want %+v", status.PendingRegistry, want)
	}
	if len(fakes.dashboards.planCalls) != 1 {
		t.Fatalf("plan_calls = %d, want 1", len(fakes.dashboards.planCalls))
	}
	if len(fakes.registryApplier.fetchDashboardsCalls) != 1 {
		t.Fatalf("fetch_dashboards_calls = %d, want 1", len(fakes.registryApplier.fetchDashboardsCalls))
	}
	if !reflect.DeepEqual(fakes.registryApplier.fetchDashboardsCalls[0], []string{"home"}) {
		t.Errorf("fetch_dashboards_calls[0] = %+v, want [home]", fakes.registryApplier.fetchDashboardsCalls[0])
	}
}

func TestReconcileNowDashboardsIsIndependentOfReconcileRegistries(t *testing.T) {
	// Unlike entities, dashboards need no floor/area/label state.
	fakes := newReconcilerFakes()
	fakes.dashboards.desired = dashboards.Desired{
		Dashboards: []map[string]any{{"id": "home", "title": "Home", "config": "home.yaml"}},
		Content:    map[string]dashboards.DashboardContent{"home": {Data: map[string]any{}}},
	}
	fakes.dashboards.planOps = []registries.RegOp{
		{Kind: dashboards.KindCreate, RType: "dashboard", Key: "home", DiffText: "+x"},
	}
	opts := baseOpts()
	opts.ReconcileRegistries = false
	opts.ReconcileDashboards = true
	r := fakes.reconciler(opts)

	r.ReconcileNow(context.Background())

	status := r.Status()
	if len(status.PendingRegistry) != 1 {
		t.Fatalf("pending_registry = %+v, want 1 entry", status.PendingRegistry)
	}
}

func TestReconcileNowSkipsDashboardFetchWhenToggleOff(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.dashboards.desired = dashboards.Desired{
		Dashboards: []map[string]any{{"id": "home", "title": "Home", "config": "home.yaml"}},
		Content:    map[string]dashboards.DashboardContent{"home": {Data: map[string]any{}}},
	}
	opts := baseOpts()
	opts.ReconcileDashboards = false
	r := fakes.reconciler(opts)

	r.ReconcileNow(context.Background())

	if len(fakes.dashboards.loadManifestCalls) != 0 {
		t.Errorf("load_manifest_calls = %v, want none", fakes.dashboards.loadManifestCalls)
	}
	if len(fakes.registryApplier.fetchDashboardsCalls) != 0 {
		t.Errorf("fetch_dashboards_calls = %d, want 0", len(fakes.registryApplier.fetchDashboardsCalls))
	}
}

func TestReconcileNowSkipsDashboardFetchWhenNoWork(t *testing.T) {
	fakes := newReconcilerFakes()
	opts := baseOpts()
	opts.ReconcileDashboards = true
	r := fakes.reconciler(opts)

	r.ReconcileNow(context.Background())

	if len(fakes.dashboards.loadManifestCalls) != 1 {
		t.Errorf("load_manifest_calls = %v, want a single call regardless", fakes.dashboards.loadManifestCalls)
	}
	if len(fakes.registryApplier.fetchDashboardsCalls) != 0 {
		t.Errorf("fetch_dashboards_calls = %d, want 0 (no work, never fetched)", len(fakes.registryApplier.fetchDashboardsCalls))
	}
	if len(fakes.dashboards.planCalls) != 0 {
		t.Errorf("plan_calls = %d, want 0", len(fakes.dashboards.planCalls))
	}
}

func TestReconcileNowFetchesDashboardsWhenOnlyManagedKeysExist(t *testing.T) {
	// The "manifest emptied but still managed" case: nothing declared,
	// but a managed dashboard may still need deleting.
	fakes := newReconcilerFakes()
	fakes.applier.state.DashboardManaged = map[string]string{"dashboard:home": "abc123"}
	opts := baseOpts()
	opts.ReconcileDashboards = true
	r := fakes.reconciler(opts)

	r.ReconcileNow(context.Background())

	if len(fakes.registryApplier.fetchDashboardsCalls) != 1 {
		t.Fatalf("fetch_dashboards_calls = %d, want 1", len(fakes.registryApplier.fetchDashboardsCalls))
	}
	if len(fakes.dashboards.planCalls) != 1 {
		t.Fatalf("plan_calls = %d, want 1", len(fakes.dashboards.planCalls))
	}
	if !reflect.DeepEqual(fakes.dashboards.planCalls[0].managed, fakes.applier.state.DashboardManaged) {
		t.Errorf("plan managed = %+v", fakes.dashboards.planCalls[0].managed)
	}
}

func TestReconcileNowOnlyFetchesContentForIDsThatLoadedSuccessfully(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.dashboards.desired = dashboards.Desired{
		Dashboards: []map[string]any{
			{"id": "home", "title": "Home", "config": "home.yaml"},
			{"id": "broken", "title": "Broken", "config": "broken.yaml"},
		},
		Content: map[string]dashboards.DashboardContent{
			"home":   {Data: map[string]any{}},
			"broken": {Err: "could not read config file"},
		},
	}
	opts := baseOpts()
	opts.ReconcileDashboards = true
	r := fakes.reconciler(opts)

	r.ReconcileNow(context.Background())

	if len(fakes.registryApplier.fetchDashboardsCalls) != 1 {
		t.Fatalf("fetch_dashboards_calls = %d, want 1", len(fakes.registryApplier.fetchDashboardsCalls))
	}
	if !reflect.DeepEqual(fakes.registryApplier.fetchDashboardsCalls[0], []string{"home"}) {
		t.Errorf("fetch_dashboards_calls[0] = %+v, want [home] (broken's config never loaded)", fakes.registryApplier.fetchDashboardsCalls[0])
	}
}

func TestReconcileNowMixesRegistryEntityAndDashboardOpsInOnePendingList(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.registries.desired = registries.Desired{Floors: []map[string]any{{"id": "ground", "name": "Ground"}}}
	fakes.registries.planOps = []registries.RegOp{{Kind: registries.KindCreate, RType: "floor", Key: "ground", DiffText: "+x"}}
	fakes.dashboards.desired = dashboards.Desired{
		Dashboards: []map[string]any{{"id": "home", "title": "Home", "config": "home.yaml"}},
		Content:    map[string]dashboards.DashboardContent{"home": {Data: map[string]any{}}},
	}
	fakes.dashboards.planOps = []registries.RegOp{
		{Kind: dashboards.KindCreate, RType: "dashboard", Key: "home", DiffText: "+y"},
	}
	opts := baseOpts()
	opts.ReconcileRegistries = true
	opts.ReconcileDashboards = true
	r := fakes.reconciler(opts)

	r.ReconcileNow(context.Background())

	status := r.Status()
	if len(status.PendingRegistry) != 2 {
		t.Fatalf("pending_registry = %+v, want 2 entries", status.PendingRegistry)
	}
	if status.PendingCount != 2 {
		t.Errorf("pending_count = %d, want 2", status.PendingCount)
	}
}

func TestReconcileNowDashboardsManifestErrorSurfacesVerbatim(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.dashboards.manifestErr = &dashboards.ManifestError{Problems: []string{"dashboards.yaml: dashboards[0] has an invalid or missing 'id'"}}
	opts := baseOpts()
	opts.ReconcileDashboards = true
	r := fakes.reconciler(opts)

	r.ReconcileNow(context.Background()) // must not panic

	status := r.Status()
	if status.State != StateError {
		t.Errorf("state = %q, want error", status.State)
	}
	if !strings.Contains(status.LastError, "invalid or missing 'id'") {
		t.Errorf("last_error = %q", status.LastError)
	}
}

func TestReconcileNowDashboardsRunsAfterEntitiesOnlyIfItSucceeded(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.entities.manifestErr = errors.New("entities.yaml: boom")
	opts := baseOpts()
	opts.ReconcileRegistries = true
	opts.ReconcileDashboards = true
	r := fakes.reconciler(opts)

	r.ReconcileNow(context.Background())

	if len(fakes.dashboards.loadManifestCalls) != 0 {
		t.Errorf("load_manifest_calls = %v, want none - the cycle must have failed before reaching dashboards", fakes.dashboards.loadManifestCalls)
	}
	status := r.Status()
	if status.State != StateError {
		t.Errorf("state = %q, want error", status.State)
	}
}

// --- ApplyNow(): dashboards run after entities, only if it succeeded -----

func TestApplyNowAppliesDashboardsAfterEntitiesInOrder(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.registries.desired = registries.Desired{Floors: []map[string]any{{"id": "ground", "name": "Ground"}}}
	fakes.registries.planOps = []registries.RegOp{{Kind: registries.KindCreate, RType: "floor", Key: "ground", DiffText: "+x"}}
	fakes.entities.desired = entities.Desired{Entities: []map[string]any{{"entity_id": "light.x", "name": "X"}}}
	fakes.entities.planOps = []registries.RegOp{{Kind: "update", RType: "entity", Key: "light.x", DiffText: "+y"}}
	fakes.dashboards.desired = dashboards.Desired{
		Dashboards: []map[string]any{{"id": "home", "title": "Home", "config": "home.yaml"}},
		Content:    map[string]dashboards.DashboardContent{"home": {Data: map[string]any{}}},
	}
	fakes.dashboards.planOps = []registries.RegOp{{Kind: dashboards.KindCreate, RType: "dashboard", Key: "home", DiffText: "+z"}}
	fakes.registryApplier.applyResult = regapply.RegistryApplyResult{OK: true, Applied: []string{"create floor:ground"}}
	fakes.registryApplier.applyEntityResult = regapply.RegistryApplyResult{OK: true, Applied: []string{"update entity:light.x"}}
	fakes.registryApplier.applyDashboardResult = regapply.RegistryApplyResult{OK: true, Applied: []string{"create dashboard:home"}}
	opts := baseOpts()
	opts.DryRun = false
	opts.ReconcileRegistries = true
	opts.ReconcileDashboards = true
	r := fakes.reconciler(opts)
	r.ReconcileNow(context.Background())

	result := r.ApplyNow(context.Background(), true)

	if !result.OK {
		t.Fatalf("result = %+v", result)
	}
	if len(fakes.registryApplier.applyPlanCalls) != 1 || fakes.registryApplier.applyPlanCalls[0].plan[0].RType != "floor" {
		t.Fatalf("apply_plan_calls = %+v", fakes.registryApplier.applyPlanCalls)
	}
	if len(fakes.registryApplier.applyEntityPlanCalls) != 1 {
		t.Fatalf("apply_entity_plan_calls = %+v", fakes.registryApplier.applyEntityPlanCalls)
	}
	if len(fakes.registryApplier.applyDashboardPlanCalls) != 1 || fakes.registryApplier.applyDashboardPlanCalls[0].ops[0].RType != "dashboard" {
		t.Fatalf("apply_dashboard_plan_calls = %+v", fakes.registryApplier.applyDashboardPlanCalls)
	}
	stash1 := fakes.registryApplier.applyPlanCalls[0].stashDir
	stash2 := fakes.registryApplier.applyEntityPlanCalls[0].stashDir
	stash3 := fakes.registryApplier.applyDashboardPlanCalls[0].stashDir
	if stash1 != stash2 || stash2 != stash3 {
		t.Errorf("stash dirs differ: %q, %q, %q", stash1, stash2, stash3)
	}
	status := r.Status()
	if status.State != StateInSync {
		t.Errorf("state = %q, want in_sync", status.State)
	}
}

func TestApplyNowDashboardFailureDoesNotUndoEntitySuccess(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.entities.desired = entities.Desired{Entities: []map[string]any{{"entity_id": "light.x", "name": "X"}}}
	fakes.entities.planOps = []registries.RegOp{{Kind: "update", RType: "entity", Key: "light.x", DiffText: "+y"}}
	fakes.dashboards.desired = dashboards.Desired{
		Dashboards: []map[string]any{{"id": "home", "title": "Home", "config": "home.yaml"}},
		Content:    map[string]dashboards.DashboardContent{"home": {Data: map[string]any{}}},
	}
	fakes.dashboards.planOps = []registries.RegOp{{Kind: dashboards.KindCreate, RType: "dashboard", Key: "home", DiffText: "+z"}}
	fakes.registryApplier.applyEntityResult = regapply.RegistryApplyResult{OK: true, Applied: []string{"update entity:light.x"}}
	fakes.registryApplier.applyDashboardResult = regapply.RegistryApplyResult{
		OK: false, Error: "create dashboard:home failed: boom", RolledBack: true,
	}
	opts := baseOpts()
	opts.DryRun = false
	opts.ReconcileRegistries = true
	opts.ReconcileDashboards = true
	r := fakes.reconciler(opts)
	r.ReconcileNow(context.Background())

	result := r.ApplyNow(context.Background(), true)

	if result.OK {
		t.Errorf("result = %+v, want ok=false", result)
	}
	if !strings.Contains(result.Error, "create dashboard:home failed") {
		t.Errorf("result.Error = %q", result.Error)
	}
	if len(fakes.registryApplier.applyEntityPlanCalls) != 1 {
		t.Errorf("apply_entity_plan_calls = %d, want 1 (the entity layer ran and succeeded)", len(fakes.registryApplier.applyEntityPlanCalls))
	}
	status := r.Status()
	if status.State != StateError {
		t.Errorf("state = %q, want error", status.State)
	}
}

func TestApplyNowDashboardsNeverRunWhenEntityApplyItselfFails(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.entities.desired = entities.Desired{Entities: []map[string]any{{"entity_id": "light.x", "name": "X"}}}
	fakes.entities.planOps = []registries.RegOp{{Kind: "update", RType: "entity", Key: "light.x", DiffText: "+y"}}
	fakes.dashboards.desired = dashboards.Desired{
		Dashboards: []map[string]any{{"id": "home", "title": "Home", "config": "home.yaml"}},
		Content:    map[string]dashboards.DashboardContent{"home": {Data: map[string]any{}}},
	}
	fakes.dashboards.planOps = []registries.RegOp{{Kind: dashboards.KindCreate, RType: "dashboard", Key: "home", DiffText: "+z"}}
	fakes.registryApplier.applyEntityResult = regapply.RegistryApplyResult{
		OK: false, Error: "update entity:light.x failed: boom", RolledBack: true,
	}
	opts := baseOpts()
	opts.DryRun = false
	opts.ReconcileRegistries = true
	opts.ReconcileDashboards = true
	r := fakes.reconciler(opts)
	r.ReconcileNow(context.Background())

	r.ApplyNow(context.Background(), true)

	if len(fakes.registryApplier.applyDashboardPlanCalls) != 0 {
		t.Errorf("apply_dashboard_plan_calls = %+v, want none", fakes.registryApplier.applyDashboardPlanCalls)
	}
}

func TestApplyNowOnlyDashboardOpsNeverCallsApplyPlanOrApplyEntityPlan(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.dashboards.desired = dashboards.Desired{
		Dashboards: []map[string]any{{"id": "home", "title": "Home", "config": "home.yaml"}},
		Content:    map[string]dashboards.DashboardContent{"home": {Data: map[string]any{}}},
	}
	fakes.dashboards.planOps = []registries.RegOp{{Kind: dashboards.KindCreate, RType: "dashboard", Key: "home", DiffText: "+z"}}
	fakes.registryApplier.applyDashboardResult = regapply.RegistryApplyResult{OK: true, Applied: []string{"create dashboard:home"}}
	opts := baseOpts()
	opts.DryRun = false
	opts.ReconcileDashboards = true
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
	if len(fakes.registryApplier.applyDashboardPlanCalls) != 1 {
		t.Errorf("apply_dashboard_plan_calls = %+v, want 1", fakes.registryApplier.applyDashboardPlanCalls)
	}
}

func TestApplyNowKeepsSkippedDashboardErrorsPending(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.dashboards.desired = dashboards.Desired{
		Dashboards: []map[string]any{{"id": "home", "title": "Home", "config": "home.yaml"}},
		Content:    map[string]dashboards.DashboardContent{"home": {Data: map[string]any{}}},
	}
	fakes.dashboards.planOps = []registries.RegOp{{Kind: dashboards.KindCreate, RType: "dashboard", Key: "home", DiffText: "+z"}}
	fakes.registryApplier.applyDashboardResult = regapply.RegistryApplyResult{
		OK: true,
		SkippedErrors: []registries.RegOp{
			{Kind: registries.KindError, RType: "dashboard", Key: "other", Error: "dashboard config file could not be loaded"},
		},
	}
	opts := baseOpts()
	opts.DryRun = false
	opts.ReconcileDashboards = true
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

// --- Rollback(): dashboard managed threaded through -----------------------

func TestRollbackPassesDashboardManagedThrough(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.applier.state.DashboardManaged = map[string]string{"dashboard:home": "abc123"}
	fakes.applier.applyResult = applier.Result{OK: true, StashDir: t.TempDir()}
	opts := baseOpts()
	opts.DryRun = false
	r := fakes.reconciler(opts)
	r.ApplyNow(context.Background(), true)
	writeFile(t, fakes.applier.applyResult.StashDir+"/registry_stash.json", `{"ops": []}`)

	r.Rollback(context.Background())

	if len(fakes.registryApplier.rollbackDashboardManage) != 1 {
		t.Fatalf("rollback_dashboard_managed = %+v", fakes.registryApplier.rollbackDashboardManage)
	}
	if !reflect.DeepEqual(fakes.registryApplier.rollbackDashboardManage[0], fakes.applier.state.DashboardManaged) {
		t.Errorf("rollback dashboard managed = %+v, want %+v",
			fakes.registryApplier.rollbackDashboardManage[0], fakes.applier.state.DashboardManaged)
	}
}

// --- status: pending_dashboard_ops -----------------------------------------

func TestPushStatusReportsPendingDashboardOpsSeparatelyFromRegistryOps(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.registries.desired = registries.Desired{Floors: []map[string]any{{"id": "ground", "name": "Ground"}}}
	fakes.registries.planOps = []registries.RegOp{{Kind: registries.KindCreate, RType: "floor", Key: "ground", DiffText: "+x"}}
	fakes.dashboards.desired = dashboards.Desired{
		Dashboards: []map[string]any{{"id": "home", "title": "Home", "config": "home.yaml"}},
		Content:    map[string]dashboards.DashboardContent{"home": {Data: map[string]any{}}},
	}
	fakes.dashboards.planOps = []registries.RegOp{{Kind: dashboards.KindCreate, RType: "dashboard", Key: "home", DiffText: "+y"}}
	opts := baseOpts()
	opts.ReconcileRegistries = true
	opts.ReconcileDashboards = true
	r := fakes.reconciler(opts)

	r.ReconcileNow(context.Background())

	var pushed map[string]any
	for _, call := range fakes.status.pushes {
		pushed = call.attrs
	}
	if pushed["pending_registry_ops"] != 2 {
		t.Errorf("pending_registry_ops = %v, want 2", pushed["pending_registry_ops"])
	}
	if pushed["pending_dashboard_ops"] != 1 {
		t.Errorf("pending_dashboard_ops = %v, want 1", pushed["pending_dashboard_ops"])
	}
}
