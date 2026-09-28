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

// deviceOp is a devices.Plan op: Key the manifest id, LiveID the device id.
func deviceOp(kind, key, deviceID string, params map[string]any) registries.RegOp {
	if params == nil {
		params = map[string]any{}
	}
	return registries.RegOp{Kind: kind, RType: "device", Key: key, Params: params, LiveID: deviceID, DiffText: "..."}
}

func deviceList(devices ...map[string]any) []any {
	list := make([]any, len(devices))
	for i, d := range devices {
		list[i] = d
	}
	return []any{list}
}

// --- FetchLiveDevices() -------------------------------------------------------

func TestFetchLiveDevicesListsTheRegistryOverOneDial(t *testing.T) {
	ws := newFakeWS()
	ws.results["config/device_registry/list"] = deviceList(map[string]any{"id": "D1", "name": "Plug"})
	dials := 0
	dialer := func(context.Context) (WSClient, error) {
		dials++
		return ws, nil
	}

	got, err := FetchLiveDevices(context.Background(), dialer)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !reflect.DeepEqual(got, []map[string]any{{"id": "D1", "name": "Plug"}}) {
		t.Errorf("devices = %+v", got)
	}
	if dials != 1 || !ws.closed || !reflect.DeepEqual(ws.callTypes(), []string{"config/device_registry/list"}) {
		t.Errorf("dials = %d, closed = %v, calls = %+v", dials, ws.closed, ws.callTypes())
	}
}

func TestFetchLiveDevicesReportsDialAndListFailures(t *testing.T) {
	if _, err := FetchLiveDevices(context.Background(), nil); err == nil {
		t.Error("nil dialer: want an error")
	}
	dialErr := errors.New("connection refused")
	failing := func(context.Context) (WSClient, error) { return nil, dialErr }
	if _, err := FetchLiveDevices(context.Background(), failing); !errors.Is(err, dialErr) {
		t.Errorf("err = %v, want the dial error", err)
	}
	ws := newFakeWS()
	ws.raiseOn["config/device_registry/list"] = []error{&wsclient.Error{Code: "unknown_error", Message: "boom"}}
	if _, err := FetchLiveDevices(context.Background(), staticDialer(ws)); err == nil {
		t.Error("list failure: want an error")
	}
}

// --- ApplyDevicePlan(): happy path, first management -------------------------

func TestApplyDevicePlanFirstManagementRecordsOriginalsAndWritesStash(t *testing.T) {
	stashDir := t.TempDir()
	ops := []registries.RegOp{deviceOp(registries.KindUpdate, "plug", "D1", map[string]any{"name_by_user": "Desk plug"})}
	originals := map[string]map[string]any{}
	ws := newFakeWS()
	ws.results["config/device_registry/list"] = deviceList(map[string]any{"id": "D1", "name": "Plug", "name_by_user": nil})

	result := ApplyDevicePlan(context.Background(), staticDialer(ws), ops, originals, stashDir)

	if !result.OK || !reflect.DeepEqual(result.Applied, []string{"update device:plug"}) {
		t.Fatalf("result = %+v", result)
	}
	if !reflect.DeepEqual(originals, map[string]map[string]any{"device:D1": {"name_by_user": nil}}) {
		t.Errorf("originals = %+v", originals)
	}
	calls := ws.callsFor("config/device_registry/update")
	want := map[string]any{"device_id": "D1", "name_by_user": "Desk plug"}
	if len(calls) != 1 || !reflect.DeepEqual(calls[0].params, want) {
		t.Errorf("update call = %+v, want %+v", calls, want)
	}

	stash := readStash(t, stashDir)
	stashOps, _ := stash["ops"].([]any)
	if len(stashOps) != 1 {
		t.Fatalf("stash = %+v", stash)
	}
	got, _ := stashOps[0].(map[string]any)
	if got["kind"] != "update" || got["rtype"] != "device" || got["key"] != "plug" || got["live_id"] != "D1" {
		t.Errorf("stash entry = %+v", got)
	}
}

func TestApplyDevicePlanNewFieldOnManagedDeviceOnlyRecordsTheNewField(t *testing.T) {
	stashDir := t.TempDir()
	ops := []registries.RegOp{deviceOp(registries.KindUpdate, "plug", "D1",
		map[string]any{"name_by_user": "Desk plug", "area_id": "kitchen"})}
	originals := map[string]map[string]any{"device:D1": {"name_by_user": nil}}
	ws := newFakeWS()
	ws.results["config/device_registry/list"] = deviceList(
		map[string]any{"id": "D1", "name": "Plug", "name_by_user": "Desk plug", "area_id": "hall"})

	result := ApplyDevicePlan(context.Background(), staticDialer(ws), ops, originals, stashDir)

	if !result.OK {
		t.Fatalf("result = %+v", result)
	}
	// name_by_user's true original (null) must survive this op's pre-update
	// live value; only the newly declared area_id is recorded.
	want := map[string]map[string]any{"device:D1": {"name_by_user": nil, "area_id": "hall"}}
	if !reflect.DeepEqual(originals, want) {
		t.Errorf("originals = %+v, want %+v", originals, want)
	}
}

// --- ApplyDevicePlan(): disabled_by guard on fresh live data -------------------

func TestApplyDevicePlanRefusesStaleDisabledOpWhenIntegrationDisabledItSincePlan(t *testing.T) {
	stashDir := t.TempDir()
	ops := []registries.RegOp{deviceOp(registries.KindUpdate, "plug", "D1", map[string]any{"disabled_by": nil})}
	originals := map[string]map[string]any{}
	ws := newFakeWS()
	ws.results["config/device_registry/list"] = deviceList(map[string]any{"id": "D1", "disabled_by": "config_entry"})

	result := ApplyDevicePlan(context.Background(), staticDialer(ws), ops, originals, stashDir)

	if result.OK {
		t.Fatalf("result = %+v, want refusal", result)
	}
	if !strings.Contains(result.Error, "no longer user-owned") || !strings.Contains(result.Error, `disabled by "config_entry"`) {
		t.Errorf("error = %q, want it to name the stale ownership", result.Error)
	}
	if len(ws.callsFor("config/device_registry/update")) != 0 {
		t.Errorf("update calls = %+v, want none", ws.callsFor("config/device_registry/update"))
	}
	if len(originals) != 0 {
		t.Errorf("originals = %+v, want empty", originals)
	}
}

func TestApplyDevicePlanGuardRunsOnlyWhenDisabledByIsSent(t *testing.T) {
	stashDir := t.TempDir()
	ops := []registries.RegOp{deviceOp(registries.KindUpdate, "plug", "D1", map[string]any{"name_by_user": "Desk plug"})}
	originals := map[string]map[string]any{}
	ws := newFakeWS()
	ws.results["config/device_registry/list"] = deviceList(map[string]any{"id": "D1", "disabled_by": "integration"})

	result := ApplyDevicePlan(context.Background(), staticDialer(ws), ops, originals, stashDir)

	if !result.OK {
		t.Fatalf("result = %+v", result)
	}
}

func TestApplyDevicePlanUserDisabledDeviceStillApplies(t *testing.T) {
	stashDir := t.TempDir()
	ops := []registries.RegOp{deviceOp(registries.KindUpdate, "plug", "D1", map[string]any{"disabled_by": nil})}
	originals := map[string]map[string]any{}
	ws := newFakeWS()
	ws.results["config/device_registry/list"] = deviceList(map[string]any{"id": "D1", "disabled_by": "user"})

	result := ApplyDevicePlan(context.Background(), staticDialer(ws), ops, originals, stashDir)

	if !result.OK {
		t.Fatalf("result = %+v", result)
	}
	if !reflect.DeepEqual(originals, map[string]map[string]any{"device:D1": {"disabled_by": "user"}}) {
		t.Errorf("originals = %+v", originals)
	}
}

func TestApplyDevicePlanDeviceGoneSincePlanFails(t *testing.T) {
	ops := []registries.RegOp{deviceOp(registries.KindUpdate, "plug", "D1", map[string]any{"name_by_user": "X"})}
	ws := newFakeWS()
	ws.results["config/device_registry/list"] = deviceList()

	result := ApplyDevicePlan(context.Background(), staticDialer(ws), ops, map[string]map[string]any{}, t.TempDir())

	if result.OK || !strings.Contains(result.Error, "device D1 no longer exists") {
		t.Fatalf("result = %+v", result)
	}
	if len(ws.callsFor("config/device_registry/update")) != 0 {
		t.Errorf("update calls = %+v, want none", ws.callsFor("config/device_registry/update"))
	}
}

// --- ApplyDevicePlan(): restore ------------------------------------------------

func TestApplyDevicePlanRestoreSendsOriginalsAndDropsMapping(t *testing.T) {
	stashDir := t.TempDir()
	originals := map[string]map[string]any{"device:D1": {"name_by_user": nil, "area_id": "hall"}}
	ops := []registries.RegOp{deviceOp("restore", "D1", "D1", map[string]any{"name_by_user": nil, "area_id": "hall"})}
	ws := newFakeWS()
	ws.results["config/device_registry/list"] = deviceList(
		map[string]any{"id": "D1", "name_by_user": "Desk plug", "area_id": "kitchen"})

	result := ApplyDevicePlan(context.Background(), staticDialer(ws), ops, originals, stashDir)

	if !result.OK {
		t.Fatalf("result = %+v", result)
	}
	if len(originals) != 0 {
		t.Errorf("originals = %+v, want empty", originals)
	}
	calls := ws.callsFor("config/device_registry/update")
	want := map[string]any{"device_id": "D1", "name_by_user": nil, "area_id": "hall"}
	if len(calls) != 1 || !reflect.DeepEqual(calls[0].params, want) {
		t.Errorf("update call = %+v, want %+v", calls, want)
	}
}

// --- ApplyDevicePlan(): shares registry_stash.json with earlier layers -------

func TestApplyDevicePlanAppendsToStashEarlierLayersWrote(t *testing.T) {
	stashDir := t.TempDir()

	regPlan := []registries.RegOp{regOp(registries.KindCreate, "floor", "ground", map[string]any{"name": "Ground"}, "")}
	regWS := newFakeWS()
	regWS.results["config/floor_registry/create"] = []any{map[string]any{"floor_id": "F1", "name": "Ground"}}
	if !ApplyPlan(context.Background(), staticDialer(regWS), regPlan, map[string]string{}, stashDir).OK {
		t.Fatal("registries apply setup failed")
	}

	entOps := []registries.RegOp{entityOp(registries.KindUpdate, "light.x", map[string]any{"name": "New"})}
	entWS := newFakeWS()
	entWS.results["config/entity_registry/list"] = []any{[]any{map[string]any{"entity_id": "light.x", "name": "Old"}}}
	if !ApplyEntityPlan(context.Background(), staticDialer(entWS), entOps, map[string]map[string]any{}, stashDir).OK {
		t.Fatal("entity apply setup failed")
	}

	devOps := []registries.RegOp{deviceOp(registries.KindUpdate, "plug", "D1", map[string]any{"name_by_user": "Desk plug"})}
	devWS := newFakeWS()
	devWS.results["config/device_registry/list"] = deviceList(map[string]any{"id": "D1", "name_by_user": nil})
	if result := ApplyDevicePlan(context.Background(), staticDialer(devWS), devOps, map[string]map[string]any{}, stashDir); !result.OK {
		t.Fatalf("device apply result = %+v", result)
	}

	stash := readStash(t, stashDir)
	if kinds := stashOpKinds(t, stash); !reflect.DeepEqual(kinds, []string{"create", "update", "update"}) {
		t.Errorf("stash kinds = %+v, want both earlier entries preserved ahead of the device update", kinds)
	}
}

func TestApplyDevicePlanToleratesMissingStashFile(t *testing.T) {
	ops := []registries.RegOp{deviceOp(registries.KindUpdate, "plug", "D1", map[string]any{"name_by_user": "X"})}
	ws := newFakeWS()
	ws.results["config/device_registry/list"] = deviceList(map[string]any{"id": "D1"})

	if result := ApplyDevicePlan(context.Background(), staticDialer(ws), ops, map[string]map[string]any{}, t.TempDir()); !result.OK {
		t.Fatalf("result = %+v", result)
	}
}

// --- ApplyDevicePlan(): error ops are skipped ----------------------------------

func TestApplyDevicePlanSkipsErrorOps(t *testing.T) {
	errOp := registries.RegOp{Kind: registries.KindError, RType: "device", Key: "lamp", Error: "no device matches name \"Lamp\""}
	updateOp := deviceOp(registries.KindUpdate, "plug", "D1", map[string]any{"name_by_user": "X"})
	ws := newFakeWS()
	ws.results["config/device_registry/list"] = deviceList(map[string]any{"id": "D1"})

	result := ApplyDevicePlan(
		context.Background(), staticDialer(ws), []registries.RegOp{errOp, updateOp}, map[string]map[string]any{}, t.TempDir())

	if !result.OK {
		t.Fatalf("result = %+v", result)
	}
	if len(result.SkippedErrors) != 1 || result.SkippedErrors[0].Key != "lamp" {
		t.Errorf("skipped = %+v", result.SkippedErrors)
	}
	if !reflect.DeepEqual(result.Applied, []string{"update device:plug"}) {
		t.Errorf("applied = %+v", result.Applied)
	}
}

func TestApplyDevicePlanOnlyErrorOpsNeverDials(t *testing.T) {
	errOp := registries.RegOp{Kind: registries.KindError, RType: "device", Key: "lamp", Error: "x"}
	dialer := func(context.Context) (WSClient, error) {
		t.Error("dialed with nothing executable")
		return newFakeWS(), nil
	}
	if result := ApplyDevicePlan(context.Background(), dialer, []registries.RegOp{errOp}, nil, ""); !result.OK {
		t.Errorf("result = %+v", result)
	}
}

// --- ApplyDevicePlan(): mid-plan failure inverts only device entries ----------

func TestApplyDevicePlanMidPlanFailureInvertsOnlyDeviceEntriesPreservingPrefix(t *testing.T) {
	stashDir := t.TempDir()

	regPlan := []registries.RegOp{regOp(registries.KindCreate, "floor", "ground", map[string]any{"name": "Ground"}, "")}
	regWS := newFakeWS()
	regWS.results["config/floor_registry/create"] = []any{map[string]any{"floor_id": "F1", "name": "Ground"}}
	if !ApplyPlan(context.Background(), staticDialer(regWS), regPlan, map[string]string{}, stashDir).OK {
		t.Fatal("registries apply setup failed")
	}

	devOps := []registries.RegOp{
		deviceOp(registries.KindUpdate, "a", "DA", map[string]any{"name_by_user": "A-new"}),
		deviceOp(registries.KindUpdate, "b", "DB", map[string]any{"name_by_user": "B-new"}),
	}
	devWS := newFakeWS()
	devWS.results["config/device_registry/list"] = deviceList(
		map[string]any{"id": "DA", "name_by_user": "A-old"},
		map[string]any{"id": "DB", "name_by_user": nil},
	)
	devWS.raiseOn["config/device_registry/update"] = []error{
		nil, // DA succeeds
		&wsclient.Error{Code: "unknown_error", Message: "boom"}, // DB fails
	}
	originals := map[string]map[string]any{}

	result := ApplyDevicePlan(context.Background(), staticDialer(devWS), devOps, originals, stashDir)

	if result.OK || !result.RolledBack {
		t.Fatalf("result = %+v", result)
	}
	if !strings.Contains(result.Error, "update device:b failed") {
		t.Errorf("error = %q", result.Error)
	}
	if len(originals) != 0 {
		t.Errorf("originals = %+v, want empty (DA's first management was inverted)", originals)
	}
	if kinds := stashOpKinds(t, readStash(t, stashDir)); !reflect.DeepEqual(kinds, []string{"create"}) {
		t.Errorf("stash kinds = %+v, want only the preserved floor create", kinds)
	}

	updateCalls := devWS.callsFor("config/device_registry/update")
	if len(updateCalls) != 3 { // a-forward, b-forward(fails), a-inverse
		t.Fatalf("update calls = %d, want 3: %+v", len(updateCalls), updateCalls)
	}
	want := map[string]any{"device_id": "DA", "name_by_user": "A-old"}
	if !reflect.DeepEqual(updateCalls[2].params, want) {
		t.Errorf("inverse params = %+v, want %+v", updateCalls[2].params, want)
	}
}

// A transport failure kills the connection (see the package doc), so the
// inverse must go over a fresh dial rather than the dead one.
func TestApplyDevicePlanTransportFailureRedialsForTheInverse(t *testing.T) {
	devOps := []registries.RegOp{
		deviceOp(registries.KindUpdate, "a", "DA", map[string]any{"name_by_user": "A-new"}),
		deviceOp(registries.KindUpdate, "b", "DB", map[string]any{"name_by_user": "B-new"}),
	}
	first := newFakeWS()
	first.results["config/device_registry/list"] = deviceList(map[string]any{"id": "DA"}, map[string]any{"id": "DB"})
	first.raiseOn["config/device_registry/update"] = []error{nil, &wsclient.Error{Code: "transport", Message: "gone"}}
	second := newFakeWS()
	conns := []*fakeWS{first, second}
	dialer := func(context.Context) (WSClient, error) {
		c := conns[0]
		conns = conns[1:]
		return c, nil
	}
	originals := map[string]map[string]any{}

	result := ApplyDevicePlan(context.Background(), dialer, devOps, originals, t.TempDir())

	if result.OK || !result.RolledBack {
		t.Fatalf("result = %+v", result)
	}
	inverse := second.callsFor("config/device_registry/update")
	if len(inverse) != 1 || !reflect.DeepEqual(inverse[0].params, map[string]any{"device_id": "DA", "name_by_user": nil}) {
		t.Errorf("inverse calls on the redialed connection = %+v", inverse)
	}
	if len(originals) != 0 {
		t.Errorf("originals = %+v, want empty", originals)
	}
}

// --- RollbackRegistry(): device entries ----------------------------------------

func TestRollbackRegistryInvertsADeviceUpdateAfterAFloorCreate(t *testing.T) {
	stashDir := t.TempDir()

	regPlan := []registries.RegOp{regOp(registries.KindCreate, "floor", "ground", map[string]any{"name": "Ground"}, "")}
	regWS := newFakeWS()
	regWS.results["config/floor_registry/create"] = []any{map[string]any{"floor_id": "F1", "name": "Ground"}}
	managed := map[string]string{}
	if !ApplyPlan(context.Background(), staticDialer(regWS), regPlan, managed, stashDir).OK {
		t.Fatal("registries apply setup failed")
	}

	devOps := []registries.RegOp{deviceOp(registries.KindUpdate, "plug", "D1",
		map[string]any{"name_by_user": "Desk plug", "disabled_by": "user"})}
	devWS := newFakeWS()
	devWS.results["config/device_registry/list"] = deviceList(
		map[string]any{"id": "D1", "name": "Plug", "name_by_user": nil, "disabled_by": nil})
	deviceOriginals := map[string]map[string]any{}
	if !ApplyDevicePlan(context.Background(), staticDialer(devWS), devOps, deviceOriginals, stashDir).OK {
		t.Fatal("device apply setup failed")
	}

	rollbackWS := newFakeWS()
	result := RollbackRegistry(context.Background(), staticDialer(rollbackWS), stashDir, managed,
		map[string]map[string]any{}, map[string]string{}, deviceOriginals)

	if !result.OK || !result.RolledBack {
		t.Fatalf("result = %+v", result)
	}
	if len(deviceOriginals) != 0 {
		t.Errorf("deviceOriginals = %+v, want empty (device update inverted)", deviceOriginals)
	}
	if len(managed) != 0 {
		t.Errorf("managed = %+v, want empty (floor create inverted)", managed)
	}

	types := rollbackWS.callTypes()
	want := []string{"config/device_registry/update", "config/floor_registry/delete"}
	if !reflect.DeepEqual(types, want) {
		t.Fatalf("calls = %+v, want %+v (reverse order)", types, want)
	}
	wantParams := map[string]any{"device_id": "D1", "name_by_user": nil, "disabled_by": nil}
	if !reflect.DeepEqual(rollbackWS.calls[0].params, wantParams) {
		t.Errorf("device inverse params = %+v, want %+v", rollbackWS.calls[0].params, wantParams)
	}
}

func TestRollbackRegistryDeviceRestoreInverseReAddsOriginals(t *testing.T) {
	stashDir := t.TempDir()
	deviceOriginals := map[string]map[string]any{"device:D1": {"name_by_user": nil, "labels": []any{}}}
	devOps := []registries.RegOp{deviceOp("restore", "D1", "D1", map[string]any{"name_by_user": nil, "labels": []any{}})}
	devWS := newFakeWS()
	devWS.results["config/device_registry/list"] = deviceList(
		map[string]any{"id": "D1", "name_by_user": "Desk plug", "labels": []any{"lighting"}})
	if !ApplyDevicePlan(context.Background(), staticDialer(devWS), devOps, deviceOriginals, stashDir).OK {
		t.Fatal("device apply setup failed")
	}
	if len(deviceOriginals) != 0 {
		t.Fatalf("deviceOriginals after restore = %+v, want empty", deviceOriginals)
	}

	rollbackWS := newFakeWS()
	result := RollbackRegistry(context.Background(), staticDialer(rollbackWS), stashDir,
		map[string]string{}, map[string]map[string]any{}, map[string]string{}, deviceOriginals)

	if !result.OK {
		t.Fatalf("result = %+v", result)
	}
	want := map[string]map[string]any{"device:D1": {"name_by_user": nil, "labels": []any{}}}
	if !reflect.DeepEqual(deviceOriginals, want) {
		t.Errorf("deviceOriginals = %+v, want %+v (restore's inverse re-adds the mapping)", deviceOriginals, want)
	}
	calls := rollbackWS.callsFor("config/device_registry/update")
	wantParams := map[string]any{"device_id": "D1", "name_by_user": "Desk plug", "labels": []any{"lighting"}}
	if len(calls) != 1 || !reflect.DeepEqual(calls[0].params, wantParams) {
		t.Errorf("inverse call = %+v, want %+v", calls, wantParams)
	}
}

// A second update of an already-managed device inverts back to its
// pre-op originals, not to "unmanaged".
func TestRollbackRegistryDeviceUpdateInverseRestoresPriorOriginals(t *testing.T) {
	stashDir := t.TempDir()
	deviceOriginals := map[string]map[string]any{"device:D1": {"name_by_user": nil}}
	devOps := []registries.RegOp{deviceOp(registries.KindUpdate, "plug", "D1",
		map[string]any{"name_by_user": "New", "area_id": "kitchen"})}
	devWS := newFakeWS()
	devWS.results["config/device_registry/list"] = deviceList(
		map[string]any{"id": "D1", "name_by_user": "Old", "area_id": "hall"})
	if !ApplyDevicePlan(context.Background(), staticDialer(devWS), devOps, deviceOriginals, stashDir).OK {
		t.Fatal("device apply setup failed")
	}

	result := RollbackRegistry(context.Background(), staticDialer(newFakeWS()), stashDir,
		map[string]string{}, map[string]map[string]any{}, map[string]string{}, deviceOriginals)

	if !result.OK {
		t.Fatalf("result = %+v", result)
	}
	want := map[string]map[string]any{"device:D1": {"name_by_user": nil}}
	if !reflect.DeepEqual(deviceOriginals, want) {
		t.Errorf("deviceOriginals = %+v, want %+v", deviceOriginals, want)
	}
}

// --- forget: a managed device gone from the registry -------------------------

func TestApplyDevicePlanForgetOnlyDropsOriginalsWithoutDialing(t *testing.T) {
	stashDir := t.TempDir()
	deviceOriginals := map[string]map[string]any{
		"device:gone": {"name_by_user": nil, "area_id": "hall"},
		"device:D1":   {"name_by_user": nil},
	}
	ops := []registries.RegOp{deviceOp(registries.KindForget, "gone", "gone", nil)}
	dialer := func(context.Context) (WSClient, error) {
		t.Error("dialed for a plan that sends nothing")
		return newFakeWS(), nil
	}

	result := ApplyDevicePlan(context.Background(), dialer, ops, deviceOriginals, stashDir)

	if !result.OK || !reflect.DeepEqual(result.Applied, []string{"forget device:gone"}) {
		t.Fatalf("result = %+v", result)
	}
	if !reflect.DeepEqual(deviceOriginals, map[string]map[string]any{"device:D1": {"name_by_user": nil}}) {
		t.Errorf("deviceOriginals = %+v, want only D1 left", deviceOriginals)
	}

	stashOps, _ := readStash(t, stashDir)["ops"].([]any)
	if len(stashOps) != 1 {
		t.Fatalf("stash ops = %+v", stashOps)
	}
	got, _ := stashOps[0].(map[string]any)
	if got["kind"] != "forget" || got["rtype"] != "device" || got["live_id"] != "gone" ||
		got["entity_originals_existed"] != true ||
		!reflect.DeepEqual(got["entity_originals_snapshot"], map[string]any{"name_by_user": nil, "area_id": "hall"}) {
		t.Errorf("stash entry = %+v, want the forget carrying the dropped originals", got)
	}
}

func TestApplyDevicePlanMidPlanFailureInvertsAForgetWithoutACall(t *testing.T) {
	deviceOriginals := map[string]map[string]any{"device:gone": {"name_by_user": nil}}
	ops := []registries.RegOp{
		deviceOp(registries.KindForget, "gone", "gone", nil),
		deviceOp(registries.KindUpdate, "a", "DA", map[string]any{"name_by_user": "A"}),
	}
	ws := newFakeWS()
	ws.results["config/device_registry/list"] = deviceList(map[string]any{"id": "DA"})
	ws.raiseOn["config/device_registry/update"] = []error{&wsclient.Error{Code: "unknown_error", Message: "boom"}}

	result := ApplyDevicePlan(context.Background(), staticDialer(ws), ops, deviceOriginals, t.TempDir())

	if result.OK || !result.RolledBack {
		t.Fatalf("result = %+v", result)
	}
	if !reflect.DeepEqual(deviceOriginals, map[string]map[string]any{"device:gone": {"name_by_user": nil}}) {
		t.Errorf("deviceOriginals = %+v, want the forgotten entry back", deviceOriginals)
	}
	if calls := ws.callsFor("config/device_registry/update"); len(calls) != 1 {
		t.Errorf("update calls = %+v, want only the failed forward one - a forget's inverse sends nothing", calls)
	}
}

func TestRollbackRegistryForgetInverseReAddsOriginalsWithoutACall(t *testing.T) {
	stashDir := t.TempDir()
	deviceOriginals := map[string]map[string]any{"device:gone": {"name_by_user": nil, "labels": []any{}}}
	ops := []registries.RegOp{deviceOp(registries.KindForget, "gone", "gone", nil)}
	if !ApplyDevicePlan(context.Background(), nil, ops, deviceOriginals, stashDir).OK {
		t.Fatal("device apply setup failed")
	}
	if len(deviceOriginals) != 0 {
		t.Fatalf("deviceOriginals after forget = %+v, want empty", deviceOriginals)
	}

	rollbackWS := newFakeWS()
	result := RollbackRegistry(context.Background(), staticDialer(rollbackWS), stashDir,
		map[string]string{}, map[string]map[string]any{}, map[string]string{}, deviceOriginals)

	if !result.OK || !result.RolledBack {
		t.Fatalf("result = %+v", result)
	}
	want := map[string]map[string]any{"device:gone": {"name_by_user": nil, "labels": []any{}}}
	if !reflect.DeepEqual(deviceOriginals, want) {
		t.Errorf("deviceOriginals = %+v, want %+v", deviceOriginals, want)
	}
	if len(rollbackWS.calls) != 0 {
		t.Errorf("calls = %+v, want none", rollbackWS.calls)
	}
}
