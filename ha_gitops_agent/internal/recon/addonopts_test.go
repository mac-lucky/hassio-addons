package recon

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/addonopts"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/applier"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/dashboards"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/entities"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/regapply"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/registries"
)

// --- ReconcileNow(): wiring internal/addonopts --------------------------

func TestReconcileNowPlansAddonOpsWhenAddonsDeclared(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.addonOpts.desired = addonopts.Desired{
		Addons: []map[string]any{{"slug": "core_configurator", "options": map[string]any{"dirsfirst": true}, "restart_on_change": true}},
	}
	fakes.addonOpts.planOps = []registries.RegOp{
		{Kind: addonopts.KindUpdate, RType: "addon", Key: "core_configurator", DiffText: "+dirsfirst: True\n"},
	}
	fakes.registryApplier.fetchSelfAddonSlugResult = "ha_gitops_agent"
	opts := baseOpts()
	opts.ReconcileAddonOptions = true
	r := fakes.reconciler(opts)

	r.ReconcileNow(context.Background())

	status := r.Status()
	if status.State != StateDriftPending {
		t.Errorf("state = %q, want drift_pending", status.State)
	}
	want := []PendingRegOp{{RType: "addon", Key: "core_configurator", Kind: "update", DiffText: "+dirsfirst: True\n"}}
	if !reflect.DeepEqual(status.PendingRegistry, want) {
		t.Errorf("pending_registry = %+v, want %+v", status.PendingRegistry, want)
	}
	if len(fakes.addonOpts.planCalls) != 1 {
		t.Fatalf("plan_calls = %d, want 1", len(fakes.addonOpts.planCalls))
	}
	if fakes.addonOpts.planCalls[0].selfSlug != "ha_gitops_agent" {
		t.Errorf("plan self_slug = %q, want ha_gitops_agent", fakes.addonOpts.planCalls[0].selfSlug)
	}
	if len(fakes.registryApplier.fetchAddonInfoCalls) != 1 || !reflect.DeepEqual(fakes.registryApplier.fetchAddonInfoCalls[0], []string{"core_configurator"}) {
		t.Errorf("fetch_addon_info_calls = %+v, want [[core_configurator]]", fakes.registryApplier.fetchAddonInfoCalls)
	}
	if fakes.registryApplier.fetchSelfAddonSlugCalls != 1 {
		t.Errorf("fetch_self_addon_slug_calls = %d, want 1", fakes.registryApplier.fetchSelfAddonSlugCalls)
	}
}

func TestReconcileNowAddonOptionsIsIndependentOfOtherToggles(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.addonOpts.desired = addonopts.Desired{
		Addons: []map[string]any{{"slug": "x", "options": map[string]any{"a": 1}, "restart_on_change": true}},
	}
	fakes.addonOpts.planOps = []registries.RegOp{{Kind: addonopts.KindUpdate, RType: "addon", Key: "x", DiffText: "+x"}}
	opts := baseOpts()
	opts.ReconcileRegistries = false
	opts.ReconcileDashboards = false
	opts.ReconcileAddonOptions = true
	r := fakes.reconciler(opts)

	r.ReconcileNow(context.Background())

	status := r.Status()
	if len(status.PendingRegistry) != 1 {
		t.Fatalf("pending_registry = %+v, want 1 entry", status.PendingRegistry)
	}
}

func TestReconcileNowSkipsAddonFetchWhenToggleOff(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.addonOpts.desired = addonopts.Desired{
		Addons: []map[string]any{{"slug": "x", "options": map[string]any{"a": 1}, "restart_on_change": true}},
	}
	opts := baseOpts()
	opts.ReconcileAddonOptions = false
	r := fakes.reconciler(opts)

	r.ReconcileNow(context.Background())

	if len(fakes.addonOpts.loadManifestCalls) != 0 {
		t.Errorf("load_manifest_calls = %v, want none", fakes.addonOpts.loadManifestCalls)
	}
	if len(fakes.registryApplier.fetchAddonInfoCalls) != 0 {
		t.Errorf("fetch_addon_info_calls = %d, want 0", len(fakes.registryApplier.fetchAddonInfoCalls))
	}
	if fakes.registryApplier.fetchSelfAddonSlugCalls != 0 {
		t.Errorf("fetch_self_addon_slug_calls = %d, want 0", fakes.registryApplier.fetchSelfAddonSlugCalls)
	}
}

func TestReconcileNowSkipsAddonFetchWhenNoWork(t *testing.T) {
	fakes := newReconcilerFakes()
	opts := baseOpts()
	opts.ReconcileAddonOptions = true
	r := fakes.reconciler(opts)

	r.ReconcileNow(context.Background())

	if len(fakes.addonOpts.loadManifestCalls) != 1 {
		t.Errorf("load_manifest_calls = %v, want a single call regardless", fakes.addonOpts.loadManifestCalls)
	}
	if len(fakes.registryApplier.fetchAddonInfoCalls) != 0 {
		t.Errorf("fetch_addon_info_calls = %d, want 0 (no work, never fetched)", len(fakes.registryApplier.fetchAddonInfoCalls))
	}
	if len(fakes.addonOpts.planCalls) != 0 {
		t.Errorf("plan_calls = %d, want 0", len(fakes.addonOpts.planCalls))
	}
}

func TestReconcileNowFetchesAddonsWhenOnlyOriginalsExist(t *testing.T) {
	// The "manifest emptied but still managed" case: nothing declared,
	// but a managed addon may still need restoring.
	fakes := newReconcilerFakes()
	fakes.applier.state.AddonOriginals = map[string]map[string]any{"addon:core_configurator": {"dirsfirst": false}}
	opts := baseOpts()
	opts.ReconcileAddonOptions = true
	r := fakes.reconciler(opts)

	r.ReconcileNow(context.Background())

	if len(fakes.registryApplier.fetchAddonInfoCalls) != 1 {
		t.Fatalf("fetch_addon_info_calls = %d, want 1", len(fakes.registryApplier.fetchAddonInfoCalls))
	}
	if !reflect.DeepEqual(fakes.registryApplier.fetchAddonInfoCalls[0], []string{"core_configurator"}) {
		t.Errorf("fetch_addon_info_calls[0] = %+v, want [core_configurator]", fakes.registryApplier.fetchAddonInfoCalls[0])
	}
	if len(fakes.addonOpts.planCalls) != 1 {
		t.Fatalf("plan_calls = %d, want 1", len(fakes.addonOpts.planCalls))
	}
	if !reflect.DeepEqual(fakes.addonOpts.planCalls[0].originals, fakes.applier.state.AddonOriginals) {
		t.Errorf("plan originals = %+v", fakes.addonOpts.planCalls[0].originals)
	}
}

func TestReconcileNowAddonSelfSlugResolvedOnceAndCachedAcrossCycles(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.addonOpts.desired = addonopts.Desired{
		Addons: []map[string]any{{"slug": "x", "options": map[string]any{"a": 1}, "restart_on_change": true}},
	}
	fakes.registryApplier.fetchSelfAddonSlugResult = "ha_gitops_agent"
	opts := baseOpts()
	opts.ReconcileAddonOptions = true
	r := fakes.reconciler(opts)

	r.ReconcileNow(context.Background())
	r.ReconcileNow(context.Background())

	if fakes.registryApplier.fetchSelfAddonSlugCalls != 1 {
		t.Errorf("fetch_self_addon_slug_calls = %d, want 1 (cached after first resolution)", fakes.registryApplier.fetchSelfAddonSlugCalls)
	}
}

func TestReconcileNowAddonSelfSlugFailureFailsCycleAndIsNotCached(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.addonOpts.desired = addonopts.Desired{
		Addons: []map[string]any{{"slug": "x", "options": map[string]any{"a": 1}, "restart_on_change": true}},
	}
	fakes.registryApplier.fetchSelfAddonSlugErr = errors.New("supervisor unreachable")
	opts := baseOpts()
	opts.ReconcileAddonOptions = true
	r := fakes.reconciler(opts)

	r.ReconcileNow(context.Background())
	status := r.Status()
	if status.State != StateError {
		t.Errorf("state = %q, want error", status.State)
	}
	if !strings.Contains(status.LastError, "supervisor unreachable") {
		t.Errorf("last_error = %q", status.LastError)
	}

	fakes.registryApplier.fetchSelfAddonSlugErr = nil
	fakes.registryApplier.fetchSelfAddonSlugResult = "ha_gitops_agent"
	r.ReconcileNow(context.Background())
	if fakes.registryApplier.fetchSelfAddonSlugCalls != 2 {
		t.Errorf("fetch_self_addon_slug_calls = %d, want 2 (retried, not cached after failure)", fakes.registryApplier.fetchSelfAddonSlugCalls)
	}
	if r.Status().State != StateInSync {
		t.Errorf("state = %q, want in_sync after recovery", r.Status().State)
	}
}

func TestReconcileNowAddonManifestErrorSurfacesVerbatim(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.addonOpts.manifestErr = &addonopts.ManifestError{Problems: []string{"addons.yaml: addon 'x' has a missing or empty 'options'"}}
	opts := baseOpts()
	opts.ReconcileAddonOptions = true
	r := fakes.reconciler(opts)

	r.ReconcileNow(context.Background()) // must not panic

	status := r.Status()
	if status.State != StateError {
		t.Errorf("state = %q, want error", status.State)
	}
	if !strings.Contains(status.LastError, "missing or empty 'options'") {
		t.Errorf("last_error = %q", status.LastError)
	}
}

func TestReconcileNowAddonsRunAfterDashboardsOnlyIfItSucceeded(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.dashboards.manifestErr = errors.New("dashboards.yaml: boom")
	opts := baseOpts()
	opts.ReconcileDashboards = true
	opts.ReconcileAddonOptions = true
	r := fakes.reconciler(opts)

	r.ReconcileNow(context.Background())

	if len(fakes.addonOpts.loadManifestCalls) != 0 {
		t.Errorf("load_manifest_calls = %v, want none - the cycle must have failed before reaching addons", fakes.addonOpts.loadManifestCalls)
	}
	status := r.Status()
	if status.State != StateError {
		t.Errorf("state = %q, want error", status.State)
	}
}

func TestReconcileNowMixesEveryLayerInOnePendingList(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.registries.desired = registries.Desired{Floors: []map[string]any{{"id": "ground", "name": "Ground"}}}
	fakes.registries.planOps = []registries.RegOp{{Kind: registries.KindCreate, RType: "floor", Key: "ground", DiffText: "+x"}}
	fakes.dashboards.desired = dashboards.Desired{
		Dashboards: []map[string]any{{"id": "home", "title": "Home", "config": "home.yaml"}},
		Content:    map[string]dashboards.DashboardContent{"home": {Data: map[string]any{}}},
	}
	fakes.dashboards.planOps = []registries.RegOp{{Kind: dashboards.KindCreate, RType: "dashboard", Key: "home", DiffText: "+y"}}
	fakes.addonOpts.desired = addonopts.Desired{
		Addons: []map[string]any{{"slug": "x", "options": map[string]any{"a": 1}, "restart_on_change": true}},
	}
	fakes.addonOpts.planOps = []registries.RegOp{{Kind: addonopts.KindUpdate, RType: "addon", Key: "x", DiffText: "+z"}}
	opts := baseOpts()
	opts.ReconcileRegistries = true
	opts.ReconcileDashboards = true
	opts.ReconcileAddonOptions = true
	r := fakes.reconciler(opts)

	r.ReconcileNow(context.Background())

	status := r.Status()
	if len(status.PendingRegistry) != 3 {
		t.Fatalf("pending_registry = %+v, want 3 entries", status.PendingRegistry)
	}
}

// --- ApplyNow(): addon options runs after dashboards, only if it succeeded -

func TestApplyNowAppliesAddonsAfterDashboardsInOrder(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.dashboards.desired = dashboards.Desired{
		Dashboards: []map[string]any{{"id": "home", "title": "Home", "config": "home.yaml"}},
		Content:    map[string]dashboards.DashboardContent{"home": {Data: map[string]any{}}},
	}
	fakes.dashboards.planOps = []registries.RegOp{{Kind: dashboards.KindCreate, RType: "dashboard", Key: "home", DiffText: "+z"}}
	fakes.addonOpts.desired = addonopts.Desired{
		Addons: []map[string]any{{"slug": "x", "options": map[string]any{"a": 1}, "restart_on_change": true}},
	}
	fakes.addonOpts.planOps = []registries.RegOp{{Kind: addonopts.KindUpdate, RType: "addon", Key: "x", DiffText: "+a"}}
	fakes.registryApplier.applyDashboardResult = regapply.RegistryApplyResult{OK: true, Applied: []string{"create dashboard:home"}}
	fakes.registryApplier.applyAddonResult = regapply.RegistryApplyResult{OK: true, Applied: []string{"update addon:x"}}
	opts := baseOpts()
	opts.DryRun = false
	opts.ReconcileDashboards = true
	opts.ReconcileAddonOptions = true
	r := fakes.reconciler(opts)
	r.ReconcileNow(context.Background())

	result := r.ApplyNow(context.Background(), true)

	if !result.OK {
		t.Fatalf("result = %+v", result)
	}
	if len(fakes.registryApplier.applyDashboardPlanCalls) != 1 {
		t.Fatalf("apply_dashboard_plan_calls = %+v", fakes.registryApplier.applyDashboardPlanCalls)
	}
	if len(fakes.registryApplier.applyAddonPlanCalls) != 1 || fakes.registryApplier.applyAddonPlanCalls[0].ops[0].RType != "addon" {
		t.Fatalf("apply_addon_plan_calls = %+v", fakes.registryApplier.applyAddonPlanCalls)
	}
	if fakes.registryApplier.applyAddonPlanCalls[0].declaredRestartOnChange["x"] != true {
		t.Errorf("declared_restart_on_change = %+v, want x:true", fakes.registryApplier.applyAddonPlanCalls[0].declaredRestartOnChange)
	}
	stash1 := fakes.registryApplier.applyDashboardPlanCalls[0].stashDir
	stash2 := fakes.registryApplier.applyAddonPlanCalls[0].stashDir
	if stash1 != stash2 {
		t.Errorf("stash dirs differ: %q, %q", stash1, stash2)
	}
	status := r.Status()
	if status.State != StateInSync {
		t.Errorf("state = %q, want in_sync", status.State)
	}
}

func TestApplyNowAddonFailureDoesNotUndoDashboardSuccess(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.dashboards.desired = dashboards.Desired{
		Dashboards: []map[string]any{{"id": "home", "title": "Home", "config": "home.yaml"}},
		Content:    map[string]dashboards.DashboardContent{"home": {Data: map[string]any{}}},
	}
	fakes.dashboards.planOps = []registries.RegOp{{Kind: dashboards.KindCreate, RType: "dashboard", Key: "home", DiffText: "+z"}}
	fakes.addonOpts.desired = addonopts.Desired{
		Addons: []map[string]any{{"slug": "x", "options": map[string]any{"a": 1}, "restart_on_change": true}},
	}
	fakes.addonOpts.planOps = []registries.RegOp{{Kind: addonopts.KindUpdate, RType: "addon", Key: "x", DiffText: "+a"}}
	fakes.registryApplier.applyDashboardResult = regapply.RegistryApplyResult{OK: true, Applied: []string{"create dashboard:home"}}
	fakes.registryApplier.applyAddonResult = regapply.RegistryApplyResult{
		OK: false, Error: "update addon:x failed: boom", RolledBack: true,
	}
	opts := baseOpts()
	opts.DryRun = false
	opts.ReconcileDashboards = true
	opts.ReconcileAddonOptions = true
	r := fakes.reconciler(opts)
	r.ReconcileNow(context.Background())

	result := r.ApplyNow(context.Background(), true)

	if result.OK {
		t.Errorf("result = %+v, want ok=false", result)
	}
	if !strings.Contains(result.Error, "update addon:x failed") {
		t.Errorf("result.Error = %q", result.Error)
	}
	if len(fakes.registryApplier.applyDashboardPlanCalls) != 1 {
		t.Errorf("apply_dashboard_plan_calls = %d, want 1 (the dashboard layer ran and succeeded)", len(fakes.registryApplier.applyDashboardPlanCalls))
	}
	status := r.Status()
	if status.State != StateError {
		t.Errorf("state = %q, want error", status.State)
	}
}

func TestApplyNowAddonsNeverRunWhenDashboardApplyItselfFails(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.dashboards.desired = dashboards.Desired{
		Dashboards: []map[string]any{{"id": "home", "title": "Home", "config": "home.yaml"}},
		Content:    map[string]dashboards.DashboardContent{"home": {Data: map[string]any{}}},
	}
	fakes.dashboards.planOps = []registries.RegOp{{Kind: dashboards.KindCreate, RType: "dashboard", Key: "home", DiffText: "+z"}}
	fakes.addonOpts.desired = addonopts.Desired{
		Addons: []map[string]any{{"slug": "x", "options": map[string]any{"a": 1}, "restart_on_change": true}},
	}
	fakes.addonOpts.planOps = []registries.RegOp{{Kind: addonopts.KindUpdate, RType: "addon", Key: "x", DiffText: "+a"}}
	fakes.registryApplier.applyDashboardResult = regapply.RegistryApplyResult{
		OK: false, Error: "create dashboard:home failed: boom", RolledBack: true,
	}
	opts := baseOpts()
	opts.DryRun = false
	opts.ReconcileDashboards = true
	opts.ReconcileAddonOptions = true
	r := fakes.reconciler(opts)
	r.ReconcileNow(context.Background())

	r.ApplyNow(context.Background(), true)

	if len(fakes.registryApplier.applyAddonPlanCalls) != 0 {
		t.Errorf("apply_addon_plan_calls = %+v, want none", fakes.registryApplier.applyAddonPlanCalls)
	}
}

func TestApplyNowOnlyAddonOpsNeverCallsOtherApplyPlans(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.addonOpts.desired = addonopts.Desired{
		Addons: []map[string]any{{"slug": "x", "options": map[string]any{"a": 1}, "restart_on_change": true}},
	}
	fakes.addonOpts.planOps = []registries.RegOp{{Kind: addonopts.KindUpdate, RType: "addon", Key: "x", DiffText: "+a"}}
	fakes.registryApplier.applyAddonResult = regapply.RegistryApplyResult{OK: true, Applied: []string{"update addon:x"}}
	opts := baseOpts()
	opts.DryRun = false
	opts.ReconcileAddonOptions = true
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
	if len(fakes.registryApplier.applyAddonPlanCalls) != 1 {
		t.Errorf("apply_addon_plan_calls = %+v, want 1", fakes.registryApplier.applyAddonPlanCalls)
	}
}

func TestApplyNowKeepsSkippedAddonErrorsPending(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.addonOpts.desired = addonopts.Desired{
		Addons: []map[string]any{{"slug": "x", "options": map[string]any{"a": 1}, "restart_on_change": true}},
	}
	fakes.addonOpts.planOps = []registries.RegOp{{Kind: addonopts.KindUpdate, RType: "addon", Key: "x", DiffText: "+a"}}
	fakes.registryApplier.applyAddonResult = regapply.RegistryApplyResult{
		OK: true,
		SkippedErrors: []registries.RegOp{
			{Kind: registries.KindError, RType: "addon", Key: "other", Error: "add-on not installed"},
		},
	}
	opts := baseOpts()
	opts.DryRun = false
	opts.ReconcileAddonOptions = true
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

func TestApplyNowUsesEntityLayerAfterAddonsWiredCorrectlyTogether(t *testing.T) {
	// All four layers in one apply, each receiving only its own ops.
	fakes := newReconcilerFakes()
	fakes.entities.desired = entities.Desired{Entities: []map[string]any{{"entity_id": "light.x", "name": "X"}}}
	fakes.entities.planOps = []registries.RegOp{{Kind: entities.KindUpdate, RType: "entity", Key: "light.x", DiffText: "+y"}}
	fakes.addonOpts.desired = addonopts.Desired{
		Addons: []map[string]any{{"slug": "x", "options": map[string]any{"a": 1}, "restart_on_change": true}},
	}
	fakes.addonOpts.planOps = []registries.RegOp{{Kind: addonopts.KindUpdate, RType: "addon", Key: "x", DiffText: "+a"}}
	fakes.registryApplier.applyEntityResult = regapply.RegistryApplyResult{OK: true, Applied: []string{"update entity:light.x"}}
	fakes.registryApplier.applyAddonResult = regapply.RegistryApplyResult{OK: true, Applied: []string{"update addon:x"}}
	opts := baseOpts()
	opts.DryRun = false
	opts.ReconcileRegistries = true
	opts.ReconcileAddonOptions = true
	r := fakes.reconciler(opts)
	r.ReconcileNow(context.Background())

	result := r.ApplyNow(context.Background(), true)

	if !result.OK {
		t.Fatalf("result = %+v", result)
	}
	if len(fakes.registryApplier.applyEntityPlanCalls) != 1 || fakes.registryApplier.applyEntityPlanCalls[0].ops[0].RType != "entity" {
		t.Errorf("apply_entity_plan_calls = %+v", fakes.registryApplier.applyEntityPlanCalls)
	}
	if len(fakes.registryApplier.applyAddonPlanCalls) != 1 || fakes.registryApplier.applyAddonPlanCalls[0].ops[0].RType != "addon" {
		t.Errorf("apply_addon_plan_calls = %+v", fakes.registryApplier.applyAddonPlanCalls)
	}
}

// --- Rollback(): addon originals/restart_on_change threaded through -----

func TestRollbackPassesAddonOriginalsAndRestartOnChangeThrough(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.applier.state.AddonOriginals = map[string]map[string]any{"addon:x": {"a": false}}
	fakes.applier.state.AddonRestartOnChange = map[string]bool{"addon:x": true}
	fakes.applier.applyResult = applier.Result{OK: true, StashDir: t.TempDir()}
	opts := baseOpts()
	opts.DryRun = false
	r := fakes.reconciler(opts)
	r.ApplyNow(context.Background(), true)
	writeFile(t, fakes.applier.applyResult.StashDir+"/addon_stash.json", `{"ops": []}`)

	r.Rollback(context.Background())

	if len(fakes.registryApplier.rollbackAddonOriginals) != 1 {
		t.Fatalf("rollback_addon_originals = %+v", fakes.registryApplier.rollbackAddonOriginals)
	}
	if !reflect.DeepEqual(fakes.registryApplier.rollbackAddonOriginals[0], fakes.applier.state.AddonOriginals) {
		t.Errorf("rollback addon originals = %+v, want %+v",
			fakes.registryApplier.rollbackAddonOriginals[0], fakes.applier.state.AddonOriginals)
	}
	if !reflect.DeepEqual(fakes.registryApplier.rollbackAddonRestartOnChange[0], fakes.applier.state.AddonRestartOnChange) {
		t.Errorf("rollback addon restart_on_change = %+v, want %+v",
			fakes.registryApplier.rollbackAddonRestartOnChange[0], fakes.applier.state.AddonRestartOnChange)
	}
}

func TestRollbackSkipsAddonRollbackWhenNoAddonStash(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.applier.applyResult = applier.Result{OK: true, StashDir: t.TempDir()}
	opts := baseOpts()
	opts.DryRun = false
	r := fakes.reconciler(opts)
	r.ApplyNow(context.Background(), true)
	// No addon_stash.json written this time.

	r.Rollback(context.Background())

	if len(fakes.registryApplier.rollbackAddonCalls) != 0 {
		t.Errorf("rollback_addon_calls = %+v, want none", fakes.registryApplier.rollbackAddonCalls)
	}
}

func TestRollbackRunsAddonRollbackEvenWhenNoRegistryStash(t *testing.T) {
	// The two stash files are independent: addon_stash.json alone is enough.
	fakes := newReconcilerFakes()
	fakes.applier.applyResult = applier.Result{OK: true, StashDir: t.TempDir()}
	opts := baseOpts()
	opts.DryRun = false
	r := fakes.reconciler(opts)
	r.ApplyNow(context.Background(), true)
	writeFile(t, fakes.applier.applyResult.StashDir+"/addon_stash.json", `{"ops": []}`)

	result := r.Rollback(context.Background())

	if !result.OK {
		t.Fatalf("result = %+v", result)
	}
	if len(fakes.registryApplier.rollbackAddonCalls) != 1 {
		t.Errorf("rollback_addon_calls = %+v, want 1", fakes.registryApplier.rollbackAddonCalls)
	}
	if len(fakes.registryApplier.rollbackCalls) != 0 {
		t.Errorf("rollback_calls (registry) = %+v, want none", fakes.registryApplier.rollbackCalls)
	}
}

// --- status: pending_addon_ops -------------------------------------------

func TestPushStatusReportsPendingAddonOpsSeparatelyFromRegistryOps(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.registries.desired = registries.Desired{Floors: []map[string]any{{"id": "ground", "name": "Ground"}}}
	fakes.registries.planOps = []registries.RegOp{{Kind: registries.KindCreate, RType: "floor", Key: "ground", DiffText: "+x"}}
	fakes.addonOpts.desired = addonopts.Desired{
		Addons: []map[string]any{{"slug": "x", "options": map[string]any{"a": 1}, "restart_on_change": true}},
	}
	fakes.addonOpts.planOps = []registries.RegOp{{Kind: addonopts.KindUpdate, RType: "addon", Key: "x", DiffText: "+y"}}
	opts := baseOpts()
	opts.ReconcileRegistries = true
	opts.ReconcileAddonOptions = true
	r := fakes.reconciler(opts)

	r.ReconcileNow(context.Background())

	var pushed map[string]any
	for _, call := range fakes.status.pushes {
		pushed = call.attrs
	}
	if pushed["pending_registry_ops"] != 2 {
		t.Errorf("pending_registry_ops = %v, want 2", pushed["pending_registry_ops"])
	}
	if pushed["pending_addon_ops"] != 1 {
		t.Errorf("pending_addon_ops = %v, want 1", pushed["pending_addon_ops"])
	}
}
