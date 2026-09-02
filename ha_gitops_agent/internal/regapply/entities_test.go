package regapply

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/registries"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/wsclient"
)

func entityOp(kind, entityID string, params map[string]any) registries.RegOp {
	if params == nil {
		params = map[string]any{}
	}
	return registries.RegOp{Kind: kind, RType: "entity", Key: entityID, Params: params, LiveID: entityID, DiffText: "..."}
}

// --- ApplyEntityPlan(): happy path, first management -----------------------

func TestApplyEntityPlanFirstManagementRecordsOriginalsAndWritesStash(t *testing.T) {
	stashDir := t.TempDir()
	ops := []registries.RegOp{entityOp(registries.KindUpdate, "light.x", map[string]any{"name": "New"})}
	originals := map[string]map[string]any{}
	ws := newFakeWS()
	ws.results["config/entity_registry/list"] = []any{[]any{map[string]any{"entity_id": "light.x", "name": "Old"}}}

	result := ApplyEntityPlan(context.Background(), staticDialer(ws), ops, originals, stashDir)

	if !result.OK {
		t.Fatalf("result = %+v", result)
	}
	if !reflect.DeepEqual(originals, map[string]map[string]any{"entity:light.x": {"name": "Old"}}) {
		t.Errorf("originals = %+v", originals)
	}
	calls := ws.callsFor("config/entity_registry/update")
	want := map[string]any{"entity_id": "light.x", "name": "New"}
	if len(calls) != 1 || !reflect.DeepEqual(calls[0].params, want) {
		t.Errorf("update call = %+v, want %+v", calls, want)
	}

	stash := readStash(t, stashDir)
	kinds := stashOpKinds(t, stash)
	if !reflect.DeepEqual(kinds, []string{"update"}) {
		t.Errorf("stash kinds = %+v", kinds)
	}
}

func TestApplyEntityPlanNewFieldOnAlreadyManagedEntityOnlyRecordsTheNewField(t *testing.T) {
	stashDir := t.TempDir()
	ops := []registries.RegOp{entityOp(registries.KindUpdate, "light.x", map[string]any{"name": "Same", "icon": "mdi:new"})}
	originals := map[string]map[string]any{"entity:light.x": {"name": "OriginalName"}}
	ws := newFakeWS()
	ws.results["config/entity_registry/list"] = []any{[]any{map[string]any{"entity_id": "light.x", "name": "Same", "icon": "mdi:old"}}}

	result := ApplyEntityPlan(context.Background(), staticDialer(ws), ops, originals, stashDir)

	if !result.OK {
		t.Fatalf("result = %+v", result)
	}
	// "name"'s true original must survive this op's pre-update live value;
	// only the newly declared "icon" is recorded.
	want := map[string]map[string]any{"entity:light.x": {"name": "OriginalName", "icon": "mdi:old"}}
	if !reflect.DeepEqual(originals, want) {
		t.Errorf("originals = %+v, want %+v", originals, want)
	}
}

// Something else may set hidden_by/disabled_by to a non-user value after
// Plan's guard ran - the plan is cached and applied later, so the window is
// the whole dry-run review. The stale op must be refused, not written: firing
// it would overwrite the integration's ownership and record a clamped null in
// place of its actual state.
func TestApplyEntityPlanRefusesStaleOpWhenLiveByFieldClaimedSincePlan(t *testing.T) {
	stashDir := t.TempDir()
	ops := []registries.RegOp{entityOp(registries.KindUpdate, "light.x", map[string]any{"hidden_by": "user"})}
	originals := map[string]map[string]any{}
	ws := newFakeWS()
	ws.results["config/entity_registry/list"] = []any{[]any{map[string]any{"entity_id": "light.x", "hidden_by": "integration"}}}

	result := ApplyEntityPlan(context.Background(), staticDialer(ws), ops, originals, stashDir)

	if result.OK {
		t.Fatalf("result = %+v, want refusal", result)
	}
	if !strings.Contains(result.Error, "no longer user-owned") || !strings.Contains(result.Error, `hidden by "integration"`) {
		t.Errorf("error = %q, want it to name the stale ownership", result.Error)
	}
	if len(ws.callsFor("config/entity_registry/update")) != 0 {
		t.Errorf("update calls = %+v, want none - the stale op must never fire", ws.callsFor("config/entity_registry/update"))
	}
	if len(originals) != 0 {
		t.Errorf("originals = %+v, want empty - nothing was applied, nothing may be recorded", originals)
	}
}

// The apply-time guard mirrors the plan-time one: null and "user" pass, so a
// live state the plan already saw does not block its own apply.
func TestApplyEntityPlanUserOwnedByFieldStillApplies(t *testing.T) {
	stashDir := t.TempDir()
	ops := []registries.RegOp{entityOp(registries.KindUpdate, "light.x", map[string]any{"hidden_by": nil})}
	originals := map[string]map[string]any{}
	ws := newFakeWS()
	ws.results["config/entity_registry/list"] = []any{[]any{map[string]any{"entity_id": "light.x", "hidden_by": "user"}}}

	result := ApplyEntityPlan(context.Background(), staticDialer(ws), ops, originals, stashDir)

	if !result.OK {
		t.Fatalf("result = %+v", result)
	}
	want := map[string]map[string]any{"entity:light.x": {"hidden_by": "user"}}
	if !reflect.DeepEqual(originals, want) {
		t.Errorf("originals = %+v, want %+v", originals, want)
	}
}

// --- ApplyEntityPlan(): restore ---------------------------------------------

func TestApplyEntityPlanRestoreSendsOriginalsAndDropsMapping(t *testing.T) {
	stashDir := t.TempDir()
	originals := map[string]map[string]any{"entity:light.x": {"name": "Original", "icon": "mdi:old"}}
	ops := []registries.RegOp{entityOp("restore", "light.x", map[string]any{"name": "Original", "icon": "mdi:old"})}
	ws := newFakeWS()
	ws.results["config/entity_registry/list"] = []any{[]any{map[string]any{"entity_id": "light.x", "name": "Managed", "icon": "mdi:new"}}}

	result := ApplyEntityPlan(context.Background(), staticDialer(ws), ops, originals, stashDir)

	if !result.OK {
		t.Fatalf("result = %+v", result)
	}
	if len(originals) != 0 {
		t.Errorf("originals = %+v, want empty", originals)
	}
	calls := ws.callsFor("config/entity_registry/update")
	want := map[string]any{"entity_id": "light.x", "name": "Original", "icon": "mdi:old"}
	if len(calls) != 1 || !reflect.DeepEqual(calls[0].params, want) {
		t.Errorf("update call = %+v, want %+v", calls, want)
	}
}

// --- ApplyEntityPlan(): shares registry_stash.json with ApplyPlan ----------

func TestApplyEntityPlanAppendsToStashApplyPlanAlreadyWrote(t *testing.T) {
	stashDir := t.TempDir()

	regPlan := []registries.RegOp{regOp(registries.KindCreate, "floor", "ground", map[string]any{"name": "Ground"}, "")}
	regWS := newFakeWS()
	regWS.results["config/floor_registry/create"] = []any{map[string]any{"floor_id": "F1", "name": "Ground"}}
	regResult := ApplyPlan(context.Background(), staticDialer(regWS), regPlan, map[string]string{}, stashDir)
	if !regResult.OK {
		t.Fatalf("registries apply result = %+v", regResult)
	}

	entOps := []registries.RegOp{entityOp(registries.KindUpdate, "light.x", map[string]any{"name": "New"})}
	entWS := newFakeWS()
	entWS.results["config/entity_registry/list"] = []any{[]any{map[string]any{"entity_id": "light.x", "name": "Old"}}}
	originals := map[string]map[string]any{}
	entResult := ApplyEntityPlan(context.Background(), staticDialer(entWS), entOps, originals, stashDir)
	if !entResult.OK {
		t.Fatalf("entity apply result = %+v", entResult)
	}

	stash := readStash(t, stashDir)
	kinds := stashOpKinds(t, stash)
	if !reflect.DeepEqual(kinds, []string{"create", "update"}) {
		t.Errorf("stash kinds = %+v, want the floor create preserved ahead of the entity update", kinds)
	}
}

func TestApplyEntityPlanToleratesMissingStashFile(t *testing.T) {
	stashDir := t.TempDir() // no ApplyPlan call first, no registry_stash.json yet
	ops := []registries.RegOp{entityOp(registries.KindUpdate, "light.x", map[string]any{"name": "New"})}
	ws := newFakeWS()
	ws.results["config/entity_registry/list"] = []any{[]any{map[string]any{"entity_id": "light.x", "name": "Old"}}}

	result := ApplyEntityPlan(context.Background(), staticDialer(ws), ops, map[string]map[string]any{}, stashDir)

	if !result.OK {
		t.Fatalf("result = %+v", result)
	}
}

// --- ApplyEntityPlan(): error ops are skipped -------------------------------

func TestApplyEntityPlanSkipsErrorOps(t *testing.T) {
	errOp := registries.RegOp{Kind: registries.KindError, RType: "entity", Key: "light.missing", Error: "entity not found"}
	updateOp := entityOp(registries.KindUpdate, "light.x", map[string]any{"name": "New"})
	ws := newFakeWS()
	ws.results["config/entity_registry/list"] = []any{[]any{map[string]any{"entity_id": "light.x", "name": "Old"}}}

	result := ApplyEntityPlan(
		context.Background(), staticDialer(ws), []registries.RegOp{errOp, updateOp}, map[string]map[string]any{}, t.TempDir())

	if !result.OK {
		t.Fatalf("result = %+v", result)
	}
	if len(result.SkippedErrors) != 1 || result.SkippedErrors[0].Key != "light.missing" {
		t.Errorf("skipped = %+v", result.SkippedErrors)
	}
	if !reflect.DeepEqual(result.Applied, []string{"update entity:light.x"}) {
		t.Errorf("applied = %+v", result.Applied)
	}
}

// --- ApplyEntityPlan(): mid-plan failure inverts only entity entries -------

func TestApplyEntityPlanMidPlanFailureInvertsOnlyEntityEntriesPreservingPrefix(t *testing.T) {
	stashDir := t.TempDir()

	regPlan := []registries.RegOp{regOp(registries.KindCreate, "floor", "ground", map[string]any{"name": "Ground"}, "")}
	regWS := newFakeWS()
	regWS.results["config/floor_registry/create"] = []any{map[string]any{"floor_id": "F1", "name": "Ground"}}
	if !ApplyPlan(context.Background(), staticDialer(regWS), regPlan, map[string]string{}, stashDir).OK {
		t.Fatal("registries apply setup failed")
	}

	entOps := []registries.RegOp{
		entityOp(registries.KindUpdate, "light.a", map[string]any{"name": "A-new"}),
		entityOp(registries.KindUpdate, "light.b", map[string]any{"name": "B-new"}),
	}
	entWS := newFakeWS()
	entWS.results["config/entity_registry/list"] = []any{[]any{
		map[string]any{"entity_id": "light.a", "name": "A-old"},
		map[string]any{"entity_id": "light.b", "name": "B-old"},
	}}
	entWS.raiseOn["config/entity_registry/update"] = []error{
		nil, // light.a succeeds
		&wsclient.Error{Code: "unknown_error", Message: "boom"}, // light.b fails
	}
	originals := map[string]map[string]any{}

	result := ApplyEntityPlan(context.Background(), staticDialer(entWS), entOps, originals, stashDir)

	if result.OK || !result.RolledBack {
		t.Fatalf("result = %+v", result)
	}
	if len(originals) != 0 {
		t.Errorf("originals = %+v, want empty (light.a's first-management was inverted)", originals)
	}

	// The floor create from the earlier, already-succeeded registries
	// layer must survive on disk untouched.
	stash := readStash(t, stashDir)
	kinds := stashOpKinds(t, stash)
	if !reflect.DeepEqual(kinds, []string{"create"}) {
		t.Errorf("stash kinds = %+v, want only the preserved floor create", kinds)
	}

	updateCalls := entWS.callsFor("config/entity_registry/update")
	if len(updateCalls) != 3 { // a-forward, b-forward(fails), a-inverse
		t.Fatalf("update calls = %d, want 3: %+v", len(updateCalls), updateCalls)
	}
	inverse := updateCalls[2]
	want := map[string]any{"entity_id": "light.a", "name": "A-old"}
	if !reflect.DeepEqual(inverse.params, want) {
		t.Errorf("inverse params = %+v, want %+v", inverse.params, want)
	}
}

// --- RollbackRegistry(, nil): inverts a combined registries+entity stash --------

func TestRollbackRegistryInvertsCombinedStashInReverseOrder(t *testing.T) {
	stashDir := t.TempDir()

	regPlan := []registries.RegOp{regOp(registries.KindCreate, "floor", "ground", map[string]any{"name": "Ground"}, "")}
	regWS := newFakeWS()
	regWS.results["config/floor_registry/create"] = []any{map[string]any{"floor_id": "F1", "name": "Ground"}}
	managed := map[string]string{}
	if !ApplyPlan(context.Background(), staticDialer(regWS), regPlan, managed, stashDir).OK {
		t.Fatal("registries apply setup failed")
	}

	entOps := []registries.RegOp{entityOp(registries.KindUpdate, "light.x", map[string]any{"name": "New"})}
	entWS := newFakeWS()
	entWS.results["config/entity_registry/list"] = []any{[]any{map[string]any{"entity_id": "light.x", "name": "Old"}}}
	originals := map[string]map[string]any{}
	if !ApplyEntityPlan(context.Background(), staticDialer(entWS), entOps, originals, stashDir).OK {
		t.Fatal("entity apply setup failed")
	}
	if !reflect.DeepEqual(originals, map[string]map[string]any{"entity:light.x": {"name": "Old"}}) {
		t.Fatalf("originals after apply = %+v", originals)
	}

	rollbackWS := newFakeWS()
	result := RollbackRegistry(context.Background(), staticDialer(rollbackWS), stashDir, managed, originals, nil)

	if !result.OK || !result.RolledBack {
		t.Fatalf("result = %+v", result)
	}
	if len(originals) != 0 {
		t.Errorf("originals = %+v, want empty (entity update inverted)", originals)
	}
	if len(managed) != 0 {
		t.Errorf("managed = %+v, want empty (floor create inverted)", managed)
	}

	// Reverse order: the entity update (applied last) must invert before
	// the floor create (applied first).
	entityInvertIdx, floorInvertIdx := -1, -1
	for i, c := range rollbackWS.calls {
		if c.msgType == "config/entity_registry/update" {
			entityInvertIdx = i
		}
		if c.msgType == "config/floor_registry/delete" {
			floorInvertIdx = i
		}
	}
	if entityInvertIdx == -1 || floorInvertIdx == -1 || entityInvertIdx > floorInvertIdx {
		t.Errorf("calls = %+v, want entity invert before floor invert", rollbackWS.calls)
	}
	entityInverse := rollbackWS.calls[entityInvertIdx]
	want := map[string]any{"entity_id": "light.x", "name": "Old"}
	if !reflect.DeepEqual(entityInverse.params, want) {
		t.Errorf("entity inverse params = %+v, want %+v", entityInverse.params, want)
	}
}

func TestRollbackRegistryRestoreInverseReAddsOriginals(t *testing.T) {
	stashDir := t.TempDir()
	originals := map[string]map[string]any{"entity:light.x": {"name": "Original"}}
	entOps := []registries.RegOp{entityOp("restore", "light.x", map[string]any{"name": "Original"})}
	entWS := newFakeWS()
	entWS.results["config/entity_registry/list"] = []any{[]any{map[string]any{"entity_id": "light.x", "name": "Managed"}}}
	if !ApplyEntityPlan(context.Background(), staticDialer(entWS), entOps, originals, stashDir).OK {
		t.Fatal("entity apply setup failed")
	}
	if len(originals) != 0 {
		t.Fatalf("originals after restore = %+v, want empty", originals)
	}

	rollbackWS := newFakeWS()
	result := RollbackRegistry(context.Background(), staticDialer(rollbackWS), stashDir, map[string]string{}, originals, nil)

	if !result.OK {
		t.Fatalf("result = %+v", result)
	}
	want := map[string]map[string]any{"entity:light.x": {"name": "Original"}}
	if !reflect.DeepEqual(originals, want) {
		t.Errorf("originals = %+v, want %+v (restore's inverse re-adds the mapping)", originals, want)
	}
	calls := rollbackWS.callsFor("config/entity_registry/update")
	wantParams := map[string]any{"entity_id": "light.x", "name": "Managed"}
	if len(calls) != 1 || !reflect.DeepEqual(calls[0].params, wantParams) {
		t.Errorf("inverse call = %+v, want %+v", calls, wantParams)
	}
}

// --- FetchLive(): entity gating ---------------------------------------------

func TestFetchLiveOmitsEntitiesByDefault(t *testing.T) {
	ws := newFakeWS()
	live, err := FetchLive(context.Background(), ws, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := live["entity"]; ok {
		t.Errorf("live[entity] present, want it omitted when includeEntities is false")
	}
	if contains(ws.callTypes(), "config/entity_registry/list") {
		t.Errorf("calls = %+v, entity list should not have been fetched", ws.callTypes())
	}
}

func TestFetchLiveIncludesEntitiesWhenRequested(t *testing.T) {
	ws := newFakeWS()
	ws.results["config/entity_registry/list"] = []any{[]any{map[string]any{"entity_id": "light.x", "name": "X"}}}
	live, err := FetchLive(context.Background(), ws, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []map[string]any{{"entity_id": "light.x", "name": "X"}}
	if !reflect.DeepEqual(live["entity"], want) {
		t.Errorf("live[entity] = %+v, want %+v", live["entity"], want)
	}
}

// --- misc --------------------------------------------------------------

func TestApplyEntityPlanGenericExceptionStillTriggersInverseReplay(t *testing.T) {
	stashDir := t.TempDir()
	entOps := []registries.RegOp{
		entityOp(registries.KindUpdate, "light.a", map[string]any{"name": "New"}),
		entityOp(registries.KindUpdate, "light.b", map[string]any{"name": "New"}),
	}
	ws := newFakeWS()
	ws.results["config/entity_registry/list"] = []any{[]any{
		map[string]any{"entity_id": "light.a", "name": "Old-a"},
		map[string]any{"entity_id": "light.b", "name": "Old-b"},
	}}
	ws.raiseOn["config/entity_registry/update"] = []error{nil, errors.New("not a wsclient error")}
	originals := map[string]map[string]any{}

	result := ApplyEntityPlan(context.Background(), staticDialer(ws), entOps, originals, stashDir)

	if result.OK || !result.RolledBack {
		t.Fatalf("result = %+v", result)
	}
	if !strings.Contains(result.Error, "not a wsclient error") {
		t.Errorf("error = %q", result.Error)
	}
}
