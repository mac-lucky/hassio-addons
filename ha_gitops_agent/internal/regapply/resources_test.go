package regapply

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/registries"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/wsclient"
)

func resourceOp(kind, id string, params map[string]any, liveID string) registries.RegOp {
	if params == nil {
		params = map[string]any{}
	}
	return registries.RegOp{Kind: kind, RType: "resource", Key: id, Params: params, LiveID: liveID, DiffText: "..."}
}

func liveResourceList(items ...map[string]any) []any {
	list := make([]any, len(items))
	for i, item := range items {
		list[i] = item
	}
	return []any{list}
}

// --- FetchLiveResources() ------------------------------------------------

func TestFetchLiveResourcesStorageModeListsOverOneDial(t *testing.T) {
	ws := newFakeWS()
	ws.results["lovelace/info"] = []any{map[string]any{"resource_mode": "storage"}}
	ws.results["lovelace/resources/list"] = liveResourceList(
		map[string]any{"id": "abc", "url": "/local/card.js", "type": "module"})
	dials := 0
	dialer := func(context.Context) (WSClient, error) { dials++; return ws, nil }

	resources, mode, err := FetchLiveResources(context.Background(), dialer)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if mode != "storage" {
		t.Errorf("mode = %q", mode)
	}
	if !reflect.DeepEqual(resources, []map[string]any{{"id": "abc", "url": "/local/card.js", "type": "module"}}) {
		t.Errorf("resources = %+v", resources)
	}
	if dials != 1 || !ws.closed {
		t.Errorf("dials = %d, closed = %v, want one dial, closed after", dials, ws.closed)
	}
	if got := ws.callTypes(); !reflect.DeepEqual(got, []string{"lovelace/info", "lovelace/resources/list"}) {
		t.Errorf("calls = %+v", got)
	}
}

func TestFetchLiveResourcesYAMLModeSkipsTheList(t *testing.T) {
	ws := newFakeWS()
	ws.results["lovelace/info"] = []any{map[string]any{"resource_mode": "yaml"}}

	resources, mode, err := FetchLiveResources(context.Background(), staticDialer(ws))

	if err != nil || mode != "yaml" || resources != nil {
		t.Fatalf("resources = %+v, mode = %q, err = %v", resources, mode, err)
	}
	if got := ws.callTypes(); !reflect.DeepEqual(got, []string{"lovelace/info"}) {
		t.Errorf("calls = %+v, want lovelace/info only", got)
	}
}

// Home Assistant before 2026.2 has no lovelace/info. The list command is
// registered only in storage mode, so whether it answers tells the mode.
func TestFetchLiveResourcesWithoutLovelaceInfoAsksTheListInstead(t *testing.T) {
	unknown := func() error { return &wsclient.Error{Code: "unknown_command", Message: "Unknown command."} }

	ws := newFakeWS()
	ws.raiseOn["lovelace/info"] = []error{unknown()}
	ws.results["lovelace/resources/list"] = liveResourceList(
		map[string]any{"id": "abc", "url": "/local/card.js", "type": "module"})
	resources, mode, err := FetchLiveResources(context.Background(), staticDialer(ws))
	if err != nil || mode != "storage" || len(resources) != 1 {
		t.Errorf("storage: resources = %+v, mode = %q, err = %v", resources, mode, err)
	}

	ws = newFakeWS()
	ws.raiseOn["lovelace/info"] = []error{unknown()}
	ws.raiseOn["lovelace/resources/list"] = []error{unknown()}
	resources, mode, err = FetchLiveResources(context.Background(), staticDialer(ws))
	if err != nil || mode != "yaml" || resources != nil {
		t.Errorf("yaml: resources = %+v, mode = %q, err = %v", resources, mode, err)
	}
}

func TestFetchLiveResourcesErrors(t *testing.T) {
	infoFails := newFakeWS()
	infoFails.raiseOn["lovelace/info"] = []error{&wsclient.Error{Code: "unknown_error", Message: "boom"}}
	if _, _, err := FetchLiveResources(context.Background(), staticDialer(infoFails)); err == nil {
		t.Error("lovelace/info failing: want an error")
	}

	noMode := newFakeWS()
	noMode.results["lovelace/info"] = []any{map[string]any{}}
	if _, _, err := FetchLiveResources(context.Background(), staticDialer(noMode)); err == nil || !strings.Contains(err.Error(), "resource_mode") {
		t.Errorf("no resource_mode: err = %v", err)
	}

	listFails := newFakeWS()
	listFails.results["lovelace/info"] = []any{map[string]any{"resource_mode": "storage"}}
	listFails.raiseOn["lovelace/resources/list"] = []error{&wsclient.Error{Code: "unknown_error", Message: "boom"}}
	if _, _, err := FetchLiveResources(context.Background(), staticDialer(listFails)); err == nil {
		t.Error("list failing: want an error")
	}

	dialFails := func(context.Context) (WSClient, error) {
		return nil, &wsclient.Error{Code: "transport", Message: "down"}
	}
	if _, _, err := FetchLiveResources(context.Background(), dialFails); err == nil {
		t.Error("dial failing: want an error")
	}
}

// --- ApplyDashboardPlan(): resource ops ------------------------------------

func TestApplyDashboardPlanResourceCreateSendsResType(t *testing.T) {
	stashDir := t.TempDir()
	ops := []registries.RegOp{resourceOp(registries.KindCreate, "card", map[string]any{"url": "/local/card.js", "type": "module"}, "")}
	ws := newFakeWS()
	ws.results["lovelace/resources/create"] = []any{map[string]any{"id": "abc", "url": "/local/card.js", "type": "module"}}
	managed := map[string]string{"dashboard:home": "d1"}

	result := ApplyDashboardPlan(context.Background(), staticDialer(ws), ops, managed, stashDir)

	if !result.OK || !reflect.DeepEqual(result.Applied, []string{"create resource:card"}) {
		t.Fatalf("result = %+v", result)
	}
	createCalls := ws.callsFor("lovelace/resources/create")
	want := map[string]any{"url": "/local/card.js", "res_type": "module"}
	if len(createCalls) != 1 || !reflect.DeepEqual(createCalls[0].params, want) {
		t.Errorf("create calls = %+v, want %+v", createCalls, want)
	}
	if !reflect.DeepEqual(managed, map[string]string{"dashboard:home": "d1", "resource:card": "abc"}) {
		t.Errorf("managed = %+v", managed)
	}
	// Neither a prior-state list nor any dashboard content is needed for a
	// create.
	if len(ws.callsFor("lovelace/resources/list")) != 0 || len(ws.callsFor("lovelace/config")) != 0 {
		t.Errorf("calls = %+v", ws.callTypes())
	}
	if kinds := stashOpKinds(t, readStash(t, stashDir)); !reflect.DeepEqual(kinds, []string{"create"}) {
		t.Errorf("stash kinds = %+v", kinds)
	}
}

func TestApplyDashboardPlanResourceUpdateSendsOnlyChangedFields(t *testing.T) {
	stashDir := t.TempDir()
	ops := []registries.RegOp{resourceOp(registries.KindUpdate, "card", map[string]any{"type": "module"}, "abc")}
	ws := newFakeWS()
	ws.results["lovelace/resources/list"] = liveResourceList(
		map[string]any{"id": "abc", "url": "/hacsfiles/card/card.js?hacstag=5", "type": "js"})
	managed := map[string]string{"resource:card": "abc"}

	result := ApplyDashboardPlan(context.Background(), staticDialer(ws), ops, managed, stashDir)

	if !result.OK {
		t.Fatalf("result = %+v", result)
	}
	updateCalls := ws.callsFor("lovelace/resources/update")
	want := map[string]any{"resource_id": "abc", "res_type": "module"}
	if len(updateCalls) != 1 || !reflect.DeepEqual(updateCalls[0].params, want) {
		t.Errorf("update calls = %+v, want %+v", updateCalls, want)
	}
	// A resource's Key is not a url_path: no lovelace/config fetch for it.
	if len(ws.callsFor("lovelace/config")) != 0 {
		t.Errorf("calls = %+v, want no dashboard content fetch", ws.callTypes())
	}

	ops2, _ := readStash(t, stashDir)["ops"].([]any)
	entry, _ := ops2[0].(map[string]any)
	if !reflect.DeepEqual(entry["live_object"], map[string]any{"type": "js"}) ||
		!reflect.DeepEqual(entry["forward_params"], map[string]any{"type": "module"}) {
		t.Errorf("stash entry = %+v", entry)
	}
	if adopted, _ := entry["adopted"].(bool); adopted {
		t.Errorf("stash entry = %+v, want adopted false - the key was already managed", entry)
	}
}

// A no-drift adopt has nothing to send: it records the mapping and a stash
// entry marking the adoption, and that is all.
func TestApplyDashboardPlanResourceAdoptWithNoDriftIsBookkeepingOnly(t *testing.T) {
	stashDir := t.TempDir()
	ops := []registries.RegOp{resourceOp(registries.KindUpdate, "card", nil, "abc")}
	ws := newFakeWS()
	ws.results["lovelace/resources/list"] = liveResourceList(
		map[string]any{"id": "abc", "url": "/hacsfiles/card/card.js?hacstag=5", "type": "module"})
	managed := map[string]string{}

	result := ApplyDashboardPlan(context.Background(), staticDialer(ws), ops, managed, stashDir)

	if !result.OK || !reflect.DeepEqual(result.Applied, []string{"update resource:card"}) {
		t.Fatalf("result = %+v", result)
	}
	if len(ws.callsFor("lovelace/resources/update")) != 0 {
		t.Errorf("calls = %+v, want no update sent", ws.callTypes())
	}
	if managed["resource:card"] != "abc" {
		t.Errorf("managed = %+v, want the adopt recorded", managed)
	}
	ops2, _ := readStash(t, stashDir)["ops"].([]any)
	entry, _ := ops2[0].(map[string]any)
	if adopted, _ := entry["adopted"].(bool); !adopted {
		t.Errorf("stash entry = %+v, want adopted true", entry)
	}
}

func TestApplyDashboardPlanResourceUpdateOfVanishedResourceFails(t *testing.T) {
	ops := []registries.RegOp{resourceOp(registries.KindUpdate, "card", map[string]any{"type": "module"}, "abc")}
	ws := newFakeWS()
	ws.results["lovelace/resources/list"] = liveResourceList()
	managed := map[string]string{}

	result := ApplyDashboardPlan(context.Background(), staticDialer(ws), ops, managed, t.TempDir())

	if result.OK || !strings.Contains(result.Error, "update resource:card failed: resource abc no longer exists") {
		t.Fatalf("result = %+v", result)
	}
	if len(managed) != 0 || len(ws.callsFor("lovelace/resources/update")) != 0 {
		t.Errorf("managed = %+v, calls = %+v", managed, ws.callTypes())
	}
}

func TestApplyDashboardPlanResourceDeleteStashesPriorURLAndType(t *testing.T) {
	stashDir := t.TempDir()
	ops := []registries.RegOp{resourceOp(registries.KindDelete, "card", nil, "abc")}
	ws := newFakeWS()
	ws.results["lovelace/resources/list"] = liveResourceList(
		map[string]any{"id": "abc", "url": "/local/card.js?v=3", "type": "module"})
	managed := map[string]string{"resource:card": "abc", "dashboard:home": "d1"}

	result := ApplyDashboardPlan(context.Background(), staticDialer(ws), ops, managed, stashDir)

	if !result.OK {
		t.Fatalf("result = %+v", result)
	}
	deleteCalls := ws.callsFor("lovelace/resources/delete")
	if len(deleteCalls) != 1 || !reflect.DeepEqual(deleteCalls[0].params, map[string]any{"resource_id": "abc"}) {
		t.Errorf("delete calls = %+v", deleteCalls)
	}
	if !reflect.DeepEqual(managed, map[string]string{"dashboard:home": "d1"}) {
		t.Errorf("managed = %+v", managed)
	}
	ops2, _ := readStash(t, stashDir)["ops"].([]any)
	entry, _ := ops2[0].(map[string]any)
	if !reflect.DeepEqual(entry["live_object"], map[string]any{"url": "/local/card.js?v=3", "type": "module"}) {
		t.Errorf("stashed prior = %+v", entry["live_object"])
	}
}

func TestApplyDashboardPlanResourceDeleteAlreadyGoneSendsNothing(t *testing.T) {
	stashDir := t.TempDir()
	ops := []registries.RegOp{resourceOp(registries.KindDelete, "card", nil, "abc")}
	ws := newFakeWS()
	ws.results["lovelace/resources/list"] = liveResourceList()
	managed := map[string]string{"resource:card": "abc"}

	result := ApplyDashboardPlan(context.Background(), staticDialer(ws), ops, managed, stashDir)

	if !result.OK {
		t.Fatalf("result = %+v", result)
	}
	if len(ws.callsFor("lovelace/resources/delete")) != 0 || len(managed) != 0 {
		t.Errorf("calls = %+v, managed = %+v", ws.callTypes(), managed)
	}
}

func TestApplyDashboardPlanResourceForgetAndRollbackRestoresIt(t *testing.T) {
	stashDir := t.TempDir()
	ops := []registries.RegOp{resourceOp(registries.KindForget, "card", nil, "abc")}
	managed := map[string]string{"resource:card": "abc", "resource:other": "def"}
	ws := newFakeWS()

	result := ApplyDashboardPlan(context.Background(), staticDialer(ws), ops, managed, stashDir)

	if !result.OK || !reflect.DeepEqual(result.Applied, []string{"forget resource:card"}) {
		t.Fatalf("result = %+v", result)
	}
	if calls := nonListCalls(ws); len(calls) != 0 {
		t.Errorf("apply sent %+v, want nothing but the listing", calls)
	}
	if !reflect.DeepEqual(managed, map[string]string{"resource:other": "def"}) {
		t.Errorf("managed after apply = %+v", managed)
	}

	rollbackWS := newFakeWS()
	rb := RollbackRegistry(context.Background(), staticDialer(rollbackWS), stashDir, map[string]string{}, nil, managed, nil)
	if !rb.OK || len(rollbackWS.calls) != 0 {
		t.Fatalf("rollback result = %+v, calls = %+v", rb, rollbackWS.calls)
	}
	if !reflect.DeepEqual(managed, map[string]string{"resource:card": "abc", "resource:other": "def"}) {
		t.Errorf("managed after rollback = %+v", managed)
	}
}

// A failure mid-plan inverts every op already executed, dashboard and
// resource alike, over the same stash.
func TestApplyDashboardPlanResourceFailureInvertsEarlierOps(t *testing.T) {
	stashDir := t.TempDir()
	ops := []registries.RegOp{
		dashboardOp(registries.KindCreate, "home", map[string]any{"metadata": map[string]any{"title": "Home", "show_in_sidebar": true}}, ""),
		resourceOp(registries.KindCreate, "a", map[string]any{"url": "/local/a.js", "type": "module"}, ""),
		resourceOp(registries.KindUpdate, "b", map[string]any{"url": "/local/b2.js"}, "rb"),
	}
	ws := newFakeWS()
	ws.results["lovelace/dashboards/create"] = []any{map[string]any{"id": "d1"}}
	ws.results["lovelace/resources/create"] = []any{map[string]any{"id": "ra"}}
	ws.results["lovelace/resources/list"] = liveResourceList(map[string]any{"id": "rb", "url": "/local/b.js", "type": "module"})
	ws.raiseOn["lovelace/resources/update"] = []error{&wsclient.Error{Code: "invalid_format", Message: "update boom"}}
	managed := map[string]string{"resource:b": "rb"}

	result := ApplyDashboardPlan(context.Background(), staticDialer(ws), ops, managed, stashDir)

	if result.OK || !result.RolledBack {
		t.Fatalf("result = %+v, want failure fully rolled back", result)
	}
	if !strings.Contains(result.Error, "update resource:b failed: invalid_format: update boom") {
		t.Errorf("error = %q", result.Error)
	}
	resDeletes := ws.callsFor("lovelace/resources/delete")
	if len(resDeletes) != 1 || !reflect.DeepEqual(resDeletes[0].params, map[string]any{"resource_id": "ra"}) {
		t.Errorf("resource delete calls = %+v, want a's create inverted", resDeletes)
	}
	dashDeletes := ws.callsFor("lovelace/dashboards/delete")
	if len(dashDeletes) != 1 || dashDeletes[0].params["dashboard_id"] != "d1" {
		t.Errorf("dashboard delete calls = %+v, want home's create inverted", dashDeletes)
	}
	if !reflect.DeepEqual(managed, map[string]string{"resource:b": "rb"}) {
		t.Errorf("managed = %+v, want only the untouched b", managed)
	}
	if ops2, _ := readStash(t, stashDir)["ops"].([]any); len(ops2) != 0 {
		t.Errorf("stash ops = %+v, want empty after successful inversion", ops2)
	}
}

// --- RollbackRegistry(): inverts resource entries -------------------------

func TestRollbackRegistryInvertsResourceCreate(t *testing.T) {
	stashDir := t.TempDir()
	ops := []registries.RegOp{resourceOp(registries.KindCreate, "card", map[string]any{"url": "/local/card.js", "type": "module"}, "")}
	ws := newFakeWS()
	ws.results["lovelace/resources/create"] = []any{map[string]any{"id": "abc"}}
	managed := map[string]string{}
	if !ApplyDashboardPlan(context.Background(), staticDialer(ws), ops, managed, stashDir).OK {
		t.Fatal("apply setup failed")
	}

	rollbackWS := newFakeWS()
	result := RollbackRegistry(context.Background(), staticDialer(rollbackWS), stashDir, map[string]string{}, nil, managed, nil)

	if !result.OK || !result.RolledBack {
		t.Fatalf("result = %+v", result)
	}
	deleteCalls := rollbackWS.callsFor("lovelace/resources/delete")
	if len(deleteCalls) != 1 || !reflect.DeepEqual(deleteCalls[0].params, map[string]any{"resource_id": "abc"}) {
		t.Errorf("delete calls = %+v", deleteCalls)
	}
	if len(managed) != 0 {
		t.Errorf("managed = %+v, want empty", managed)
	}
}

func TestRollbackRegistryInvertsResourceUpdateRestoringPriorURLAndType(t *testing.T) {
	stashDir := t.TempDir()
	ops := []registries.RegOp{resourceOp(registries.KindUpdate, "card", map[string]any{"url": "/local/card.js?v=2", "type": "module"}, "abc")}
	ws := newFakeWS()
	ws.results["lovelace/resources/list"] = liveResourceList(map[string]any{"id": "abc", "url": "/local/card.js?v=1", "type": "js"})
	managed := map[string]string{"resource:card": "abc"}
	if !ApplyDashboardPlan(context.Background(), staticDialer(ws), ops, managed, stashDir).OK {
		t.Fatal("apply setup failed")
	}
	forward := ws.callsFor("lovelace/resources/update")
	wantForward := map[string]any{"resource_id": "abc", "url": "/local/card.js?v=2", "res_type": "module"}
	if len(forward) != 1 || !reflect.DeepEqual(forward[0].params, wantForward) {
		t.Fatalf("forward update = %+v, want %+v", forward, wantForward)
	}

	rollbackWS := newFakeWS()
	result := RollbackRegistry(context.Background(), staticDialer(rollbackWS), stashDir, map[string]string{}, nil, managed, nil)

	if !result.OK {
		t.Fatalf("result = %+v", result)
	}
	updateCalls := rollbackWS.callsFor("lovelace/resources/update")
	want := map[string]any{"resource_id": "abc", "url": "/local/card.js?v=1", "res_type": "js"}
	if len(updateCalls) != 1 || !reflect.DeepEqual(updateCalls[0].params, want) {
		t.Errorf("restore calls = %+v, want %+v", updateCalls, want)
	}
	if managed["resource:card"] != "abc" {
		t.Errorf("managed = %+v, want the already-managed key kept", managed)
	}
}

// Rolling back a no-drift adopt sends nothing and releases the key, so a
// later manifest removal cannot delete a HACS resource the agent no
// longer manages.
func TestRollbackRegistryInvertsResourceAdoptByReleasingTheKey(t *testing.T) {
	stashDir := t.TempDir()
	ops := []registries.RegOp{resourceOp(registries.KindUpdate, "card", nil, "abc")}
	ws := newFakeWS()
	ws.results["lovelace/resources/list"] = liveResourceList(map[string]any{"id": "abc", "url": "/hacsfiles/c/c.js?hacstag=1", "type": "module"})
	managed := map[string]string{}
	if !ApplyDashboardPlan(context.Background(), staticDialer(ws), ops, managed, stashDir).OK {
		t.Fatal("apply setup failed")
	}

	rollbackWS := newFakeWS()
	result := RollbackRegistry(context.Background(), staticDialer(rollbackWS), stashDir, map[string]string{}, nil, managed, nil)

	if !result.OK || len(rollbackWS.calls) != 0 {
		t.Fatalf("result = %+v, calls = %+v", result, rollbackWS.calls)
	}
	if len(managed) != 0 {
		t.Errorf("managed = %+v, want the adopt released", managed)
	}
}

// A managed key whose resource vanished re-adopts another one on the same
// path. That is an adoption too: rolling it back must release the key,
// not leave it on a resource (HACS's) the apply merely found.
func TestRollbackRegistryReleasesAResourceReAdoption(t *testing.T) {
	stashDir := t.TempDir()
	ops := []registries.RegOp{resourceOp(registries.KindUpdate, "card", nil, "new")}
	ws := newFakeWS()
	ws.results["lovelace/resources/list"] = liveResourceList(map[string]any{"id": "new", "url": "/hacsfiles/c/c.js?hacstag=2", "type": "module"})
	managed := map[string]string{"resource:card": "gone"}
	if !ApplyDashboardPlan(context.Background(), staticDialer(ws), ops, managed, stashDir).OK {
		t.Fatal("apply setup failed")
	}
	if managed["resource:card"] != "new" {
		t.Fatalf("managed = %+v, want the re-adoption recorded", managed)
	}

	result := RollbackRegistry(context.Background(), staticDialer(newFakeWS()), stashDir, map[string]string{}, nil, managed, nil)

	if !result.OK {
		t.Fatalf("result = %+v", result)
	}
	if id, still := managed["resource:card"]; still && id == "new" {
		t.Errorf("managed = %+v, want the re-adopted resource released", managed)
	}
}

func TestRollbackRegistryInvertsResourceDeleteRecreatingUnderANewID(t *testing.T) {
	stashDir := t.TempDir()
	ops := []registries.RegOp{resourceOp(registries.KindDelete, "card", nil, "abc")}
	ws := newFakeWS()
	ws.results["lovelace/resources/list"] = liveResourceList(map[string]any{"id": "abc", "url": "/local/card.js?v=3", "type": "module"})
	managed := map[string]string{"resource:card": "abc"}
	if !ApplyDashboardPlan(context.Background(), staticDialer(ws), ops, managed, stashDir).OK {
		t.Fatal("apply setup failed")
	}

	rollbackWS := newFakeWS()
	rollbackWS.results["lovelace/resources/create"] = []any{map[string]any{"id": "new-id"}}
	result := RollbackRegistry(context.Background(), staticDialer(rollbackWS), stashDir, map[string]string{}, nil, managed, nil)

	if !result.OK {
		t.Fatalf("result = %+v", result)
	}
	createCalls := rollbackWS.callsFor("lovelace/resources/create")
	want := map[string]any{"url": "/local/card.js?v=3", "res_type": "module"}
	if len(createCalls) != 1 || !reflect.DeepEqual(createCalls[0].params, want) {
		t.Errorf("create calls = %+v, want %+v", createCalls, want)
	}
	if managed["resource:card"] != "new-id" {
		t.Errorf("managed = %+v, want resource:card remapped to new-id", managed)
	}
}

// A hand-edited or truncated stash must not recreate a resource with a
// null url or type, which HA would reject anyway.
func TestInvertResourceDeleteWithIncompletePriorFails(t *testing.T) {
	ws := newFakeWS()
	entry := stashEntry{Kind: registries.KindDelete, RType: "resource", Key: "card", LiveID: "abc", PriorObject: map[string]any{"url": "/local/card.js"}}

	err := invertResourceOp(context.Background(), ws, entry, map[string]string{})

	if err == nil || !strings.Contains(err.Error(), "cannot recreate resource:card") {
		t.Errorf("err = %v", err)
	}
	if len(ws.calls) != 0 {
		t.Errorf("calls = %+v, want none", ws.calls)
	}
}
