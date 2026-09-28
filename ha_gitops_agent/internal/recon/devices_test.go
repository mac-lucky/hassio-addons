package recon

import (
	"context"
	"errors"
	"testing"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/applier"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/dashboards"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/devices"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/entities"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/regapply"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/registries"
)

// --- ReconcileNow(): wiring internal/devices ---------------------------

func deviceFakes() *reconcilerFakes {
	fakes := newReconcilerFakes()
	fakes.devices.desired = devices.Desired{Devices: []devices.Device{
		{ID: "kitchen_strip", Match: devices.Match{Name: "LED Kitchen"}, Fields: map[string]any{"name": "Kitchen strip"}},
	}}
	fakes.devices.planOps = []registries.RegOp{
		{Kind: registries.KindUpdate, RType: "device", Key: "kitchen_strip", LiveID: "d1", DiffText: "+name_by_user"},
	}
	fakes.registryApplier.fetchDevicesResult = []map[string]any{{"id": "d1", "name": "LED Kitchen"}}
	return fakes
}

func TestReconcileNowPlansDeviceOpsBetweenRegistriesAndEntities(t *testing.T) {
	fakes := deviceFakes()
	fakes.registries.desired = registries.Desired{Floors: []map[string]any{{"id": "ground", "name": "Ground"}}}
	fakes.registries.planOps = []registries.RegOp{{Kind: registries.KindCreate, RType: "floor", Key: "ground"}}
	fakes.entities.desired = entities.Desired{Entities: []map[string]any{{"entity_id": "light.x", "name": "X"}}}
	fakes.entities.planOps = []registries.RegOp{{Kind: entities.KindUpdate, RType: "entity", Key: "light.x"}}
	opts := baseOpts()
	opts.ReconcileRegistries = true
	r := fakes.reconciler(opts)

	r.ReconcileNow(context.Background())

	var order []string
	for _, op := range r.Status().PendingRegistry {
		order = append(order, op.RType)
	}
	if len(order) != 3 || order[0] != "floor" || order[1] != "device" || order[2] != "entity" {
		t.Errorf("pending order = %v, want [floor device entity]", order)
	}
	if fakes.registryApplier.fetchDevicesCalls != 1 {
		t.Errorf("fetch_devices_calls = %d, want 1", fakes.registryApplier.fetchDevicesCalls)
	}
	if len(fakes.devices.planCalls) != 1 || len(fakes.devices.planCalls[0].live) != 1 {
		t.Errorf("device plan calls = %+v, want one with the fetched devices", fakes.devices.planCalls)
	}
}

// The device list is only read when there is device work - a declared
// entry, or originals left to restore.
func TestReconcileNowSkipsTheDeviceFetchWithNothingToDo(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.registries.desired = registries.Desired{Floors: []map[string]any{{"id": "ground", "name": "Ground"}}}
	opts := baseOpts()
	opts.ReconcileRegistries = true
	r := fakes.reconciler(opts)

	r.ReconcileNow(context.Background())

	if fakes.registryApplier.fetchDevicesCalls != 0 {
		t.Errorf("fetch_devices_calls = %d, want 0", fakes.registryApplier.fetchDevicesCalls)
	}
}

func TestReconcileNowFetchesDevicesWhenOnlyOriginalsRemain(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.applier.state.DeviceOriginals = map[string]map[string]any{"device:d1": {"name_by_user": nil}}
	opts := baseOpts()
	opts.ReconcileRegistries = true
	r := fakes.reconciler(opts)

	r.ReconcileNow(context.Background())

	if fakes.registryApplier.fetchDevicesCalls != 1 || len(fakes.devices.planCalls) != 1 {
		t.Errorf("fetch/plan = %d/%d, want 1/1 - a restore still needs planning",
			fakes.registryApplier.fetchDevicesCalls, len(fakes.devices.planCalls))
	}
}

func TestReconcileNowDeviceFetchFailureEndsTheCycle(t *testing.T) {
	fakes := deviceFakes()
	fakes.registryApplier.fetchDevicesErr = errors.New("ws closed")
	opts := baseOpts()
	opts.ReconcileRegistries = true
	r := fakes.reconciler(opts)

	r.ReconcileNow(context.Background())

	if status := r.Status(); status.State != StateError {
		t.Errorf("state = %q, want error", status.State)
	}
}

func TestApplyNowRunsDeviceOpsThroughApplyDevicePlan(t *testing.T) {
	fakes := deviceFakes()
	opts := baseOpts()
	opts.DryRun = false
	opts.ReconcileRegistries = true
	r := fakes.reconciler(opts)
	r.ReconcileNow(context.Background())

	r.ApplyNow(context.Background(), true)

	if len(fakes.registryApplier.applyDevicePlanCalls) != 1 {
		t.Fatalf("apply_device_plan_calls = %d, want 1", len(fakes.registryApplier.applyDevicePlanCalls))
	}
	if len(fakes.registryApplier.applyPlanCalls) != 0 {
		t.Errorf("device ops reached ApplyPlan, which would run them as a helper domain: %+v",
			fakes.registryApplier.applyPlanCalls)
	}
}

// A registries layer that failed may have left an area gone that a device
// op points at, so devices wait; entities wait on devices in turn.
func TestApplyNowDevicesAndEntitiesWaitOnAFailedRegistryLayer(t *testing.T) {
	fakes := deviceFakes()
	fakes.registries.desired = registries.Desired{Floors: []map[string]any{{"id": "ground", "name": "Ground"}}}
	fakes.registries.planOps = []registries.RegOp{{Kind: registries.KindCreate, RType: "floor", Key: "ground"}}
	fakes.registryApplier.applyResult = regapply.RegistryApplyResult{OK: false, Error: "boom", RolledBack: true}
	fakes.entities.desired = entities.Desired{Entities: []map[string]any{{"entity_id": "light.x", "name": "X"}}}
	fakes.entities.planOps = []registries.RegOp{{Kind: entities.KindUpdate, RType: "entity", Key: "light.x"}}
	opts := baseOpts()
	opts.DryRun = false
	opts.ReconcileRegistries = true
	r := fakes.reconciler(opts)
	r.ReconcileNow(context.Background())

	r.ApplyNow(context.Background(), true)

	if len(fakes.registryApplier.applyDevicePlanCalls) != 0 || len(fakes.registryApplier.applyEntityPlanCalls) != 0 {
		t.Errorf("device/entity layers ran after registries failed: %d/%d",
			len(fakes.registryApplier.applyDevicePlanCalls), len(fakes.registryApplier.applyEntityPlanCalls))
	}
}

func TestApplyNowEntitiesWaitOnAFailedDeviceLayer(t *testing.T) {
	fakes := deviceFakes()
	fakes.registryApplier.applyDeviceResult = regapply.RegistryApplyResult{OK: false, Error: "boom", RolledBack: true}
	fakes.entities.desired = entities.Desired{Entities: []map[string]any{{"entity_id": "light.x", "name": "X"}}}
	fakes.entities.planOps = []registries.RegOp{{Kind: entities.KindUpdate, RType: "entity", Key: "light.x"}}
	opts := baseOpts()
	opts.DryRun = false
	opts.ReconcileRegistries = true
	r := fakes.reconciler(opts)
	r.ReconcileNow(context.Background())

	r.ApplyNow(context.Background(), true)

	if len(fakes.registryApplier.applyEntityPlanCalls) != 0 {
		t.Errorf("entities ran after devices failed: %+v", fakes.registryApplier.applyEntityPlanCalls)
	}
}

func TestRollbackPassesDeviceOriginalsThrough(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.applier.state.DeviceOriginals = map[string]map[string]any{"device:d1": {"name_by_user": nil}}
	fakes.applier.applyResult = applier.Result{OK: true, StashDir: t.TempDir()}
	opts := baseOpts()
	opts.DryRun = false
	r := fakes.reconciler(opts)
	r.ApplyNow(context.Background(), true)
	writeFile(t, fakes.applier.applyResult.StashDir+"/registry_stash.json", `{"ops": []}`)

	r.Rollback(context.Background())

	if len(fakes.registryApplier.rollbackDeviceOriginals) != 1 ||
		fakes.registryApplier.rollbackDeviceOriginals[0]["device:d1"] == nil {
		t.Errorf("rollback device originals = %+v, want state.DeviceOriginals", fakes.registryApplier.rollbackDeviceOriginals)
	}
}

// --- ReconcileNow(): Lovelace resources ride the dashboard layer ---------

func resourceFakes() *reconcilerFakes {
	fakes := newReconcilerFakes()
	fakes.dashboards.desired = dashboards.Desired{Resources: []dashboards.Resource{
		{ID: "bubble_card", URL: "/hacsfiles/Bubble-Card/bubble-card.js", Type: "module"},
	}}
	fakes.dashboards.resourceOps = []registries.RegOp{
		{Kind: registries.KindCreate, RType: "resource", Key: "bubble_card", DiffText: "+url"},
	}
	return fakes
}

func TestReconcileNowPlansResourcesWithoutFetchingDashboards(t *testing.T) {
	fakes := resourceFakes()
	opts := baseOpts()
	opts.ReconcileDashboards = true
	r := fakes.reconciler(opts)

	r.ReconcileNow(context.Background())

	if fakes.registryApplier.fetchResourcesCalls != 1 || len(fakes.dashboards.resourcePlanCalls) != 1 {
		t.Fatalf("fetch/plan resources = %d/%d, want 1/1",
			fakes.registryApplier.fetchResourcesCalls, len(fakes.dashboards.resourcePlanCalls))
	}
	if fakes.dashboards.resourcePlanCalls[0].mode != "storage" {
		t.Errorf("resource mode = %q, want the fetched one", fakes.dashboards.resourcePlanCalls[0].mode)
	}
	if len(fakes.registryApplier.fetchDashboardsCalls) != 0 {
		t.Errorf("dashboards fetched with only resources declared: %+v", fakes.registryApplier.fetchDashboardsCalls)
	}
}

func TestReconcileNowPlansResourcesWhenOnlyManagedOnesRemain(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.applier.state.DashboardManaged = map[string]string{"resource:bubble_card": "abc"}
	opts := baseOpts()
	opts.ReconcileDashboards = true
	r := fakes.reconciler(opts)

	r.ReconcileNow(context.Background())

	if fakes.registryApplier.fetchResourcesCalls != 1 {
		t.Errorf("fetch_resources_calls = %d, want 1 - a delete still needs planning", fakes.registryApplier.fetchResourcesCalls)
	}
	if len(fakes.registryApplier.fetchDashboardsCalls) != 0 {
		t.Errorf("dashboards fetched with only a resource managed: %+v", fakes.registryApplier.fetchDashboardsCalls)
	}
}

func TestApplyNowRunsResourceOpsThroughTheDashboardLayer(t *testing.T) {
	fakes := resourceFakes()
	opts := baseOpts()
	opts.DryRun = false
	opts.ReconcileDashboards = true
	r := fakes.reconciler(opts)
	r.ReconcileNow(context.Background())

	r.ApplyNow(context.Background(), true)

	if len(fakes.registryApplier.applyDashboardPlanCalls) != 1 ||
		fakes.registryApplier.applyDashboardPlanCalls[0].ops[0].RType != "resource" {
		t.Errorf("apply_dashboard_plan_calls = %+v, want the resource op", fakes.registryApplier.applyDashboardPlanCalls)
	}
	if len(fakes.registryApplier.applyPlanCalls) != 0 {
		t.Errorf("resource ops reached ApplyPlan: %+v", fakes.registryApplier.applyPlanCalls)
	}
}
