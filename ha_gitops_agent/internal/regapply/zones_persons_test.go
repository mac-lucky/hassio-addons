package regapply

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/registries"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/wsclient"
)

var liveZoneOffice = map[string]any{
	"id": "office", "name": "Office", "latitude": 52.2297, "longitude": 21.0122,
	"radius": 100.0, "passive": false, "icon": "mdi:briefcase",
}

// Shaped like PersonStorageCollection's items: user_id and picture are
// null until set in the UI.
var livePersonAnna = map[string]any{
	"id": "anna", "name": "Anna", "user_id": "0d3f4a1b2c",
	"device_trackers": []any{"device_tracker.anna_phone", "device_tracker.anna_watch"},
	"picture":         "/api/image/serve/abc/512x512",
}

func personListResult(storage, config []any) map[string]any {
	return map[string]any{"storage": storage, "config": config}
}

// --- FetchLive: person/list ----------------------------------------------

// person/list is {"storage": [...], "config": [...]}; read as a list it
// came back empty and every declared person was created again each cycle.
func TestFetchLiveUnwrapsPersonListIntoStorageAndYAMLBuckets(t *testing.T) {
	ws := newFakeWS()
	yamlBob := map[string]any{"id": "bob", "name": "Bob", "device_trackers": []any{}}
	ws.results["person/list"] = []any{personListResult([]any{livePersonAnna}, []any{yamlBob})}

	live, err := FetchLive(context.Background(), ws, []string{"person"}, false)
	if err != nil {
		t.Fatalf("FetchLive: %v", err)
	}

	if want := []map[string]any{livePersonAnna}; !reflect.DeepEqual(live["person"], want) {
		t.Errorf("person = %+v, want %+v", live["person"], want)
	}
	if want := []map[string]any{yamlBob}; !reflect.DeepEqual(live[registries.PersonYAMLBucket], want) {
		t.Errorf("%s = %+v, want %+v", registries.PersonYAMLBucket, live[registries.PersonYAMLBucket], want)
	}
}

func TestFetchLivePersonListWithoutAStorageListIsAnError(t *testing.T) {
	for name, result := range map[string]any{
		"a bare list":        []any{livePersonAnna},
		"no storage key":     map[string]any{"config": []any{}},
		"storage not a list": map[string]any{"storage": map[string]any{}, "config": []any{}},
	} {
		t.Run(name, func(t *testing.T) {
			ws := newFakeWS()
			ws.results["person/list"] = []any{result}

			if _, err := FetchLive(context.Background(), ws, []string{"person"}, false); err == nil || !strings.Contains(err.Error(), "person/list") {
				t.Errorf("err = %v, want a person/list shape error", err)
			}
		})
	}
}

func TestFetchLiveNamesZoneAndPersonWhenNotLoaded(t *testing.T) {
	for _, domain := range []string{"zone", "person"} {
		ws := newFakeWS()
		ws.raiseOn[domain+"/list"] = []error{&wsclient.Error{Code: "unknown_command", Message: "Unknown command."}}

		_, err := FetchLive(context.Background(), ws, []string{domain}, false)

		want := "helpers.yaml declares " + domain + " items, or this agent still manages some from an earlier one, " +
			"but Home Assistant has not loaded the " + domain + " integration - add default_config: or " + domain + ": to configuration.yaml"
		if err == nil || err.Error() != want {
			t.Errorf("err = %v, want %q", err, want)
		}
	}
}

// --- ApplyPlan: zone -------------------------------------------------------

func TestApplyPlanZoneCreateSendsTheDeclaredFields(t *testing.T) {
	params := map[string]any{"name": "Office", "latitude": 52.2297, "longitude": 21.0122, "radius": 150}
	plan := []registries.RegOp{regOp(registries.KindCreate, "zone", "office", params, "")}
	managed := map[string]string{}
	ws := newFakeWS()
	ws.results["zone/create"] = []any{map[string]any{
		"id": "office", "name": "Office", "latitude": 52.2297, "longitude": 21.0122, "radius": 150.0, "passive": false,
	}}

	if result := ApplyPlan(context.Background(), staticDialer(ws), plan, managed, t.TempDir()); !result.OK {
		t.Fatalf("result = %+v", result)
	}

	calls := ws.callsFor("zone/create")
	if len(calls) != 1 || !reflect.DeepEqual(calls[0].params, params) {
		t.Errorf("create calls = %+v, want one with %+v", calls, params)
	}
	if managed["zone:office"] != "office" {
		t.Errorf("managed = %+v", managed)
	}
}

// A zone update merges, so the declared fields alone would do; the full
// baseline is still sent, which a merge makes harmless.
func TestApplyPlanZoneUpdateCarriesTheFullBaseline(t *testing.T) {
	plan := []registries.RegOp{regOp(registries.KindUpdate, "zone", "office", map[string]any{"radius": 250}, "office")}
	managed := map[string]string{"zone:office": "office"}
	ws := newFakeWS()
	ws.results["zone/list"] = []any{[]any{liveZoneOffice}}

	if result := ApplyPlan(context.Background(), staticDialer(ws), plan, managed, t.TempDir()); !result.OK {
		t.Fatalf("result = %+v", result)
	}

	want := map[string]any{
		"zone_id": "office", "name": "Office", "latitude": 52.2297, "longitude": 21.0122,
		"radius": 250, "passive": false, "icon": "mdi:briefcase",
	}
	calls := ws.callsFor("zone/update")
	if len(calls) != 1 || !reflect.DeepEqual(calls[0].params, want) {
		t.Errorf("update calls = %+v, want one with %+v", calls, want)
	}
}

func TestApplyPlanZoneDelete(t *testing.T) {
	plan := []registries.RegOp{regOp(registries.KindDelete, "zone", "office", nil, "office")}
	managed := map[string]string{"zone:office": "office"}
	ws := newFakeWS()
	ws.results["zone/list"] = []any{[]any{liveZoneOffice}}

	if result := ApplyPlan(context.Background(), staticDialer(ws), plan, managed, t.TempDir()); !result.OK {
		t.Fatalf("result = %+v", result)
	}

	calls := ws.callsFor("zone/delete")
	if want := map[string]any{"zone_id": "office"}; len(calls) != 1 || !reflect.DeepEqual(calls[0].params, want) {
		t.Errorf("delete calls = %+v, want one with %+v", calls, want)
	}
	if _, still := managed["zone:office"]; still {
		t.Errorf("managed = %+v, want zone:office dropped", managed)
	}
}

// The inverse resends the prior zone. Under HA's merge that restores every
// field the zone had, but cannot remove one the update added: the icon
// stays, which is why the rollback sends no icon key at all rather than a
// null the schema would refuse.
func TestRollbackRegistryZoneUpdateResendsThePriorZone(t *testing.T) {
	stashDir := t.TempDir()
	prior := map[string]any{
		"id": "office", "name": "Office", "latitude": 52.2297, "longitude": 21.0122, "radius": 100.0, "passive": false,
	}
	plan := []registries.RegOp{regOp(registries.KindUpdate, "zone", "office",
		map[string]any{"radius": 250, "icon": "mdi:briefcase"}, "office")}
	managed := map[string]string{"zone:office": "office"}
	applyWS := newFakeWS()
	applyWS.results["zone/list"] = []any{[]any{prior}}

	if result := ApplyPlan(context.Background(), staticDialer(applyWS), plan, managed, stashDir); !result.OK {
		t.Fatalf("apply result = %+v", result)
	}

	rollbackWS := newFakeWS()
	if rb := RollbackRegistry(context.Background(), staticDialer(rollbackWS), stashDir, managed, nil, nil, nil); !rb.OK {
		t.Fatalf("rollback result = %+v", rb)
	}
	want := []wsCall{{msgType: "zone/update", params: map[string]any{
		"zone_id": "office", "name": "Office", "latitude": 52.2297, "longitude": 21.0122, "radius": 100.0, "passive": false,
	}}}
	if !reflect.DeepEqual(rollbackWS.calls, want) {
		t.Errorf("rollback calls = %+v, want %+v", rollbackWS.calls, want)
	}
	if managed["zone:office"] != "office" {
		t.Errorf("managed = %+v, want the managed zone kept", managed)
	}
}

// --- ApplyPlan: person -----------------------------------------------------

// HA's person update schema defaults a missing device_trackers to [], so an
// update of only the name would unlink every tracker; the baseline resends
// them, and the user and picture linked in the UI with them.
func TestApplyPlanPersonUpdateKeepsDeviceTrackersUserAndPicture(t *testing.T) {
	plan := []registries.RegOp{regOp(registries.KindUpdate, "person", "anna", map[string]any{"name": "Anna B"}, "anna")}
	managed := map[string]string{"person:anna": "anna"}
	ws := newFakeWS()
	ws.results["person/list"] = []any{personListResult([]any{livePersonAnna}, []any{})}

	if result := ApplyPlan(context.Background(), staticDialer(ws), plan, managed, t.TempDir()); !result.OK {
		t.Fatalf("result = %+v", result)
	}

	want := map[string]any{
		"person_id": "anna", "name": "Anna B", "user_id": "0d3f4a1b2c",
		"device_trackers": []any{"device_tracker.anna_phone", "device_tracker.anna_watch"},
		"picture":         "/api/image/serve/abc/512x512",
	}
	calls := ws.callsFor("person/update")
	if len(calls) != 1 || !reflect.DeepEqual(calls[0].params, want) {
		t.Errorf("update calls = %+v, want one with %+v", calls, want)
	}
}

// A person never linked to a user has user_id: null, which the baseline
// leaves out rather than sending.
func TestApplyPlanPersonAdoptLeavesNullUserIDOut(t *testing.T) {
	unlinked := map[string]any{"id": "guest", "name": "Guest", "user_id": nil, "device_trackers": []any{}, "picture": nil}
	plan := []registries.RegOp{regOp(registries.KindUpdate, "person", "guest",
		map[string]any{"name": "Guest", "device_trackers": []any{"device_tracker.guest_phone"}}, "guest")}
	ws := newFakeWS()
	ws.results["person/list"] = []any{personListResult([]any{unlinked}, []any{})}

	if result := ApplyPlan(context.Background(), staticDialer(ws), plan, map[string]string{}, t.TempDir()); !result.OK {
		t.Fatalf("result = %+v", result)
	}

	want := map[string]any{"person_id": "guest", "name": "Guest", "device_trackers": []any{"device_tracker.guest_phone"}}
	calls := ws.callsFor("person/update")
	if len(calls) != 1 || !reflect.DeepEqual(calls[0].params, want) {
		t.Errorf("update calls = %+v, want one with %+v", calls, want)
	}
}

func TestApplyPlanPersonCreateAndDelete(t *testing.T) {
	plan := []registries.RegOp{
		regOp(registries.KindCreate, "person", "guest", map[string]any{"name": "Guest"}, ""),
		regOp(registries.KindDelete, "person", "anna", nil, "anna"),
	}
	managed := map[string]string{"person:anna": "anna"}
	ws := newFakeWS()
	ws.results["person/list"] = []any{personListResult([]any{livePersonAnna}, []any{})}
	ws.results["person/create"] = []any{map[string]any{
		"id": "guest", "name": "Guest", "user_id": nil, "device_trackers": []any{}, "picture": nil,
	}}

	if result := ApplyPlan(context.Background(), staticDialer(ws), plan, managed, t.TempDir()); !result.OK {
		t.Fatalf("result = %+v", result)
	}

	if calls := ws.callsFor("person/create"); len(calls) != 1 || !reflect.DeepEqual(calls[0].params, map[string]any{"name": "Guest"}) {
		t.Errorf("create calls = %+v", calls)
	}
	if calls := ws.callsFor("person/delete"); len(calls) != 1 || !reflect.DeepEqual(calls[0].params, map[string]any{"person_id": "anna"}) {
		t.Errorf("delete calls = %+v", calls)
	}
	if want := map[string]string{"person:guest": "guest"}; !reflect.DeepEqual(managed, want) {
		t.Errorf("managed = %+v, want %+v", managed, want)
	}
}
