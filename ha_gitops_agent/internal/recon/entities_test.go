package recon

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/applier"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/entities"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/regapply"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/registries"
)

// --- ReconcileNow(): wiring internal/entities -------------------------

func TestReconcileNowPlansEntityOpsWhenEntitiesDeclared(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.entities.desired = entities.Desired{Entities: []map[string]any{{"entity_id": "light.x", "name": "X"}}}
	fakes.entities.planOps = []registries.RegOp{
		{Kind: entities.KindUpdate, RType: "entity", Key: "light.x", DiffText: "+name: 'X'\n"},
	}
	opts := baseOpts()
	opts.ReconcileRegistries = true
	r := fakes.reconciler(opts)

	r.ReconcileNow(context.Background())

	status := r.Status()
	if status.State != StateDriftPending {
		t.Errorf("state = %q, want drift_pending", status.State)
	}
	want := []PendingRegOp{{RType: "entity", Key: "light.x", Kind: "update", DiffText: "+name: 'X'\n"}}
	if !reflect.DeepEqual(status.PendingRegistry, want) {
		t.Errorf("pending_registry = %+v, want %+v", status.PendingRegistry, want)
	}
	if fakes.registryApplier.fetchLiveCalls != 1 {
		t.Errorf("fetch_live_calls = %d, want 1", fakes.registryApplier.fetchLiveCalls)
	}
	if len(fakes.registryApplier.fetchLiveIncEntity) != 1 || !fakes.registryApplier.fetchLiveIncEntity[0] {
		t.Errorf("fetch_live_inc_entity = %+v, want [true]", fakes.registryApplier.fetchLiveIncEntity)
	}
	if len(fakes.entities.planCalls) != 1 {
		t.Fatalf("plan_calls = %d, want 1", len(fakes.entities.planCalls))
	}
}

func TestReconcileNowMixesRegistryAndEntityOpsInOnePendingList(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.registries.desired = registries.Desired{Floors: []map[string]any{{"id": "ground", "name": "Ground"}}}
	fakes.registries.planOps = []registries.RegOp{
		{Kind: registries.KindCreate, RType: "floor", Key: "ground", DiffText: "+x"},
	}
	fakes.entities.desired = entities.Desired{Entities: []map[string]any{{"entity_id": "light.x", "name": "X"}}}
	fakes.entities.planOps = []registries.RegOp{
		{Kind: entities.KindUpdate, RType: "entity", Key: "light.x", DiffText: "+y"},
	}
	opts := baseOpts()
	opts.ReconcileRegistries = true
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

func TestReconcileNowSkipsWsFetchWhenNeitherRegistriesNorEntitiesHaveWork(t *testing.T) {
	fakes := newReconcilerFakes()
	opts := baseOpts()
	opts.ReconcileRegistries = true
	r := fakes.reconciler(opts)

	r.ReconcileNow(context.Background())

	if fakes.registryApplier.fetchLiveCalls != 0 {
		t.Errorf("fetch_live_calls = %d, want 0", fakes.registryApplier.fetchLiveCalls)
	}
	if len(fakes.entities.loadManifestCalls) != 1 {
		t.Errorf("load_manifest_calls = %v, want a single call regardless", fakes.entities.loadManifestCalls)
	}
	if len(fakes.entities.planCalls) != 0 {
		t.Errorf("plan_calls = %d, want 0 (no work, never planned)", len(fakes.entities.planCalls))
	}
}

func TestReconcileNowFetchesEntitiesWhenOnlyEntityOriginalsNonEmpty(t *testing.T) {
	// The "manifest emptied but still managed" case: nothing declared,
	// but managed entities may still need restoring.
	fakes := newReconcilerFakes()
	fakes.applier.state.EntityOriginals = map[string]map[string]any{"entity:light.x": {"name": "Original"}}
	opts := baseOpts()
	opts.ReconcileRegistries = true
	r := fakes.reconciler(opts)

	r.ReconcileNow(context.Background())

	if fakes.registryApplier.fetchLiveCalls != 1 {
		t.Errorf("fetch_live_calls = %d, want 1", fakes.registryApplier.fetchLiveCalls)
	}
	if len(fakes.entities.planCalls) != 1 {
		t.Fatalf("plan_calls = %d, want 1", len(fakes.entities.planCalls))
	}
	if !reflect.DeepEqual(fakes.entities.planCalls[0].originals, fakes.applier.state.EntityOriginals) {
		t.Errorf("plan originals = %+v", fakes.entities.planCalls[0].originals)
	}
}

func TestReconcileNowIncludeEntitiesGatedIndependentlyFromRegistries(t *testing.T) {
	// Floors are declared but entities are not: the WS connection opens
	// for the floor fetch, yet the entity list must stay unfetched.
	fakes := newReconcilerFakes()
	fakes.registries.desired = registries.Desired{Floors: []map[string]any{{"id": "ground", "name": "Ground"}}}
	fakes.registries.planOps = []registries.RegOp{{Kind: registries.KindCreate, RType: "floor", Key: "ground"}}
	opts := baseOpts()
	opts.ReconcileRegistries = true
	r := fakes.reconciler(opts)

	r.ReconcileNow(context.Background())

	if len(fakes.registryApplier.fetchLiveIncEntity) != 1 || fakes.registryApplier.fetchLiveIncEntity[0] {
		t.Errorf("fetch_live_inc_entity = %+v, want [false]", fakes.registryApplier.fetchLiveIncEntity)
	}
	if len(fakes.entities.planCalls) != 0 {
		t.Errorf("plan_calls = %d, want 0", len(fakes.entities.planCalls))
	}
}

func TestReconcileNowEntitiesManifestErrorSurfacesVerbatim(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.entities.manifestErr = &entities.ManifestError{Problems: []string{"entities.yaml: entities[0] has an invalid or missing 'entity_id'"}}
	opts := baseOpts()
	opts.ReconcileRegistries = true
	r := fakes.reconciler(opts)

	r.ReconcileNow(context.Background()) // must not panic

	status := r.Status()
	if status.State != StateError {
		t.Errorf("state = %q, want error", status.State)
	}
	if !strings.Contains(status.LastError, "invalid or missing 'entity_id'") {
		t.Errorf("last_error = %q", status.LastError)
	}
}

// --- ApplyNow(): splitting registry vs entity ops -----------------------

func TestApplyNowSplitsOpsAndAppliesEntitiesAfterRegistries(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.registries.desired = registries.Desired{Floors: []map[string]any{{"id": "ground", "name": "Ground"}}}
	fakes.registries.planOps = []registries.RegOp{{Kind: registries.KindCreate, RType: "floor", Key: "ground", DiffText: "+x"}}
	fakes.entities.desired = entities.Desired{Entities: []map[string]any{{"entity_id": "light.x", "name": "X"}}}
	fakes.entities.planOps = []registries.RegOp{{Kind: entities.KindUpdate, RType: "entity", Key: "light.x", DiffText: "+y"}}
	fakes.registryApplier.applyResult = regapply.RegistryApplyResult{OK: true, Applied: []string{"create floor:ground"}}
	fakes.registryApplier.applyEntityResult = regapply.RegistryApplyResult{OK: true, Applied: []string{"update entity:light.x"}}
	opts := baseOpts()
	opts.DryRun = false
	opts.ReconcileRegistries = true
	r := fakes.reconciler(opts)
	r.ReconcileNow(context.Background())

	result := r.ApplyNow(context.Background(), true)

	if !result.OK {
		t.Fatalf("result = %+v", result)
	}
	if len(fakes.registryApplier.applyPlanCalls) != 1 || len(fakes.registryApplier.applyPlanCalls[0].plan) != 1 {
		t.Fatalf("apply_plan_calls = %+v", fakes.registryApplier.applyPlanCalls)
	}
	if fakes.registryApplier.applyPlanCalls[0].plan[0].RType != "floor" {
		t.Errorf("apply_plan received %+v, want only the floor op", fakes.registryApplier.applyPlanCalls[0].plan)
	}
	if len(fakes.registryApplier.applyEntityPlanCalls) != 1 || len(fakes.registryApplier.applyEntityPlanCalls[0].ops) != 1 {
		t.Fatalf("apply_entity_plan_calls = %+v", fakes.registryApplier.applyEntityPlanCalls)
	}
	if fakes.registryApplier.applyEntityPlanCalls[0].ops[0].RType != "entity" {
		t.Errorf("apply_entity_plan received %+v, want only the entity op", fakes.registryApplier.applyEntityPlanCalls[0].ops)
	}
	if fakes.registryApplier.applyPlanCalls[0].stashDir != fakes.registryApplier.applyEntityPlanCalls[0].stashDir {
		t.Errorf("stash dirs differ: %q vs %q",
			fakes.registryApplier.applyPlanCalls[0].stashDir, fakes.registryApplier.applyEntityPlanCalls[0].stashDir)
	}
	status := r.Status()
	if status.State != StateInSync {
		t.Errorf("state = %q, want in_sync", status.State)
	}
}

func TestApplyNowEntityFailureDoesNotUndoRegistrySuccess(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.registries.desired = registries.Desired{Floors: []map[string]any{{"id": "ground", "name": "Ground"}}}
	fakes.registries.planOps = []registries.RegOp{{Kind: registries.KindCreate, RType: "floor", Key: "ground", DiffText: "+x"}}
	fakes.entities.desired = entities.Desired{Entities: []map[string]any{{"entity_id": "light.x", "name": "X"}}}
	fakes.entities.planOps = []registries.RegOp{{Kind: entities.KindUpdate, RType: "entity", Key: "light.x", DiffText: "+y"}}
	fakes.registryApplier.applyResult = regapply.RegistryApplyResult{OK: true, Applied: []string{"create floor:ground"}}
	fakes.registryApplier.applyEntityResult = regapply.RegistryApplyResult{
		OK: false, Error: "update entity:light.x failed: boom", RolledBack: true,
	}
	opts := baseOpts()
	opts.DryRun = false
	opts.ReconcileRegistries = true
	r := fakes.reconciler(opts)
	r.ReconcileNow(context.Background())

	result := r.ApplyNow(context.Background(), true)

	if result.OK {
		t.Errorf("result = %+v, want ok=false", result)
	}
	if !strings.Contains(result.Error, "update entity:light.x failed") {
		t.Errorf("result.Error = %q", result.Error)
	}
	if len(fakes.registryApplier.applyPlanCalls) != 1 {
		t.Errorf("apply_plan_calls = %d, want 1 (the registries layer ran)", len(fakes.registryApplier.applyPlanCalls))
	}
	status := r.Status()
	if status.State != StateError {
		t.Errorf("state = %q, want error", status.State)
	}
}

func TestApplyNowEntitiesNeverRunWhenRegistryApplyItselfFails(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.registries.desired = registries.Desired{Floors: []map[string]any{{"id": "ground", "name": "Ground"}}}
	fakes.registries.planOps = []registries.RegOp{{Kind: registries.KindCreate, RType: "floor", Key: "ground", DiffText: "+x"}}
	fakes.entities.desired = entities.Desired{Entities: []map[string]any{{"entity_id": "light.x", "name": "X"}}}
	fakes.entities.planOps = []registries.RegOp{{Kind: entities.KindUpdate, RType: "entity", Key: "light.x", DiffText: "+y"}}
	fakes.registryApplier.applyResult = regapply.RegistryApplyResult{
		OK: false, Error: "create floor:ground failed: boom", RolledBack: true,
	}
	opts := baseOpts()
	opts.DryRun = false
	opts.ReconcileRegistries = true
	r := fakes.reconciler(opts)
	r.ReconcileNow(context.Background())

	r.ApplyNow(context.Background(), true)

	if len(fakes.registryApplier.applyEntityPlanCalls) != 0 {
		t.Errorf("apply_entity_plan_calls = %+v, want none", fakes.registryApplier.applyEntityPlanCalls)
	}
}

func TestApplyNowOnlyEntityOpsNeverCallsApplyPlan(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.entities.desired = entities.Desired{Entities: []map[string]any{{"entity_id": "light.x", "name": "X"}}}
	fakes.entities.planOps = []registries.RegOp{{Kind: entities.KindUpdate, RType: "entity", Key: "light.x", DiffText: "+y"}}
	fakes.registryApplier.applyEntityResult = regapply.RegistryApplyResult{OK: true, Applied: []string{"update entity:light.x"}}
	opts := baseOpts()
	opts.DryRun = false
	opts.ReconcileRegistries = true
	r := fakes.reconciler(opts)
	r.ReconcileNow(context.Background())

	result := r.ApplyNow(context.Background(), true)

	if !result.OK {
		t.Fatalf("result = %+v", result)
	}
	if len(fakes.registryApplier.applyPlanCalls) != 0 {
		t.Errorf("apply_plan_calls = %+v, want none", fakes.registryApplier.applyPlanCalls)
	}
	if len(fakes.registryApplier.applyEntityPlanCalls) != 1 {
		t.Errorf("apply_entity_plan_calls = %+v, want 1", fakes.registryApplier.applyEntityPlanCalls)
	}
}

func TestApplyNowKeepsSkippedEntityErrorsPending(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.entities.desired = entities.Desired{Entities: []map[string]any{{"entity_id": "light.x", "name": "X"}}}
	fakes.entities.planOps = []registries.RegOp{{Kind: entities.KindUpdate, RType: "entity", Key: "light.x", DiffText: "+y"}}
	fakes.registryApplier.applyEntityResult = regapply.RegistryApplyResult{
		OK: true,
		SkippedErrors: []registries.RegOp{
			{Kind: registries.KindError, RType: "entity", Key: "light.other", Error: "entity not found"},
		},
	}
	opts := baseOpts()
	opts.DryRun = false
	opts.ReconcileRegistries = true
	r := fakes.reconciler(opts)
	r.ReconcileNow(context.Background())

	r.ApplyNow(context.Background(), true)

	status := r.Status()
	if len(status.PendingRegistry) != 1 || status.PendingRegistry[0].Key != "light.other" {
		t.Errorf("pending_registry = %+v", status.PendingRegistry)
	}
	if status.State != StateDriftPending {
		t.Errorf("state = %q, want drift_pending", status.State)
	}
}

// --- Rollback(): entity originals threaded through -----------------------

func TestRollbackPassesEntityOriginalsThrough(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.applier.state.EntityOriginals = map[string]map[string]any{"entity:light.x": {"name": "Original"}}
	fakes.applier.applyResult = applier.Result{OK: true, StashDir: t.TempDir()}
	opts := baseOpts()
	opts.DryRun = false
	r := fakes.reconciler(opts)
	r.ApplyNow(context.Background(), true)
	writeFile(t, fakes.applier.applyResult.StashDir+"/registry_stash.json", `{"ops": []}`)

	r.Rollback(context.Background())

	if len(fakes.registryApplier.rollbackOriginals) != 1 {
		t.Fatalf("rollback_originals = %+v", fakes.registryApplier.rollbackOriginals)
	}
	if !reflect.DeepEqual(fakes.registryApplier.rollbackOriginals[0], fakes.applier.state.EntityOriginals) {
		t.Errorf("rollback originals = %+v, want %+v", fakes.registryApplier.rollbackOriginals[0], fakes.applier.state.EntityOriginals)
	}
}
