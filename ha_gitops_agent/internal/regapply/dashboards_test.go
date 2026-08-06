package regapply

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/registries"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/wsclient"
)

func dashboardOp(kind, id string, params map[string]any, liveID string) registries.RegOp {
	if params == nil {
		params = map[string]any{}
	}
	return registries.RegOp{Kind: kind, RType: "dashboard", Key: id, Params: params, LiveID: liveID, DiffText: "..."}
}

// --- FetchLiveDashboards() -------------------------------------------------

func TestFetchLiveDashboardsListsMetadataAndFetchesContentPerID(t *testing.T) {
	ws := newFakeWS()
	ws.results["lovelace/dashboards/list"] = []any{[]any{
		map[string]any{"id": "home", "url_path": "home", "title": "Home"},
	}}
	ws.results["lovelace/config"] = []any{map[string]any{"views": []any{}}}

	dashboards, content, err := FetchLiveDashboards(context.Background(), ws, []string{"home"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !reflect.DeepEqual(dashboards, []map[string]any{{"id": "home", "url_path": "home", "title": "Home"}}) {
		t.Errorf("dashboards = %+v", dashboards)
	}
	if !reflect.DeepEqual(content["home"], map[string]any{"views": []any{}}) {
		t.Errorf("content = %+v", content)
	}
	configCalls := ws.callsFor("lovelace/config")
	if len(configCalls) != 1 || configCalls[0].params["url_path"] != "home" {
		t.Errorf("config calls = %+v", configCalls)
	}
}

func TestFetchLiveDashboardsConfigNotFoundIsNilNotAnError(t *testing.T) {
	ws := newFakeWS()
	ws.results["lovelace/dashboards/list"] = []any{[]any{}}
	ws.raiseOn["lovelace/config"] = []error{&wsclient.Error{Code: "config_not_found", Message: "No config found."}}

	_, content, err := FetchLiveDashboards(context.Background(), ws, []string{"fresh"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v, ok := content["fresh"]; !ok || v != nil {
		t.Errorf("content[fresh] = %+v, want a present nil entry", content["fresh"])
	}
}

func TestFetchLiveDashboardsOtherErrorPropagates(t *testing.T) {
	ws := newFakeWS()
	ws.results["lovelace/dashboards/list"] = []any{[]any{}}
	ws.raiseOn["lovelace/config"] = []error{&wsclient.Error{Code: "unknown_error", Message: "boom"}}

	_, _, err := FetchLiveDashboards(context.Background(), ws, []string{"home"})
	if err == nil {
		t.Fatal("expected an error")
	}
}

// --- ApplyDashboardPlan(): create --------------------------------------

func TestApplyDashboardPlanCreateSendsMetadataThenContent(t *testing.T) {
	stashDir := t.TempDir()
	params := map[string]any{
		"metadata": map[string]any{"title": "Home", "show_in_sidebar": true},
		"content":  map[string]any{"views": []any{}},
	}
	ops := []registries.RegOp{dashboardOp(registries.KindCreate, "home", params, "")}
	ws := newFakeWS()
	ws.results["lovelace/dashboards/create"] = []any{map[string]any{"id": "home", "url_path": "home", "title": "Home"}}
	managed := map[string]string{}

	result := ApplyDashboardPlan(context.Background(), staticDialer(ws), ops, managed, stashDir)

	if !result.OK {
		t.Fatalf("result = %+v", result)
	}
	if managed["dashboard:home"] != "home" {
		t.Errorf("managed = %+v", managed)
	}
	createCalls := ws.callsFor("lovelace/dashboards/create")
	wantCreate := map[string]any{"url_path": "home", "allow_single_word": true, "title": "Home", "show_in_sidebar": true}
	if len(createCalls) != 1 || !reflect.DeepEqual(createCalls[0].params, wantCreate) {
		t.Errorf("create call = %+v, want %+v", createCalls, wantCreate)
	}
	saveCalls := ws.callsFor("lovelace/config/save")
	wantSave := map[string]any{"url_path": "home", "config": map[string]any{"views": []any{}}}
	if len(saveCalls) != 1 || !reflect.DeepEqual(saveCalls[0].params, wantSave) {
		t.Errorf("save call = %+v, want %+v", saveCalls, wantSave)
	}

	stash := readStash(t, stashDir)
	if kinds := stashOpKinds(t, stash); !reflect.DeepEqual(kinds, []string{"create"}) {
		t.Errorf("stash kinds = %+v", kinds)
	}
}

func TestApplyDashboardPlanCreateIncludesIconWhenPresent(t *testing.T) {
	stashDir := t.TempDir()
	params := map[string]any{
		"metadata": map[string]any{"title": "Home", "show_in_sidebar": true, "icon": "mdi:home"},
	}
	ops := []registries.RegOp{dashboardOp(registries.KindCreate, "home", params, "")}
	ws := newFakeWS()
	ws.results["lovelace/dashboards/create"] = []any{map[string]any{"id": "home"}}

	result := ApplyDashboardPlan(context.Background(), staticDialer(ws), ops, map[string]string{}, stashDir)
	if !result.OK {
		t.Fatalf("result = %+v", result)
	}
	createCalls := ws.callsFor("lovelace/dashboards/create")
	if createCalls[0].params["icon"] != "mdi:home" {
		t.Errorf("create call = %+v, want icon mdi:home", createCalls[0])
	}
}

// The create is stashed before the content call runs, so a content
// failure leaves inverseReplayAndPersist a real entry to invert.
func TestApplyDashboardPlanCreateContentFailureIsStashedThenInverted(t *testing.T) {
	stashDir := t.TempDir()
	params := map[string]any{
		"metadata": map[string]any{"title": "Home", "show_in_sidebar": true},
		"content":  map[string]any{"views": []any{}},
	}
	ops := []registries.RegOp{dashboardOp(registries.KindCreate, "home", params, "")}
	ws := newFakeWS()
	ws.results["lovelace/dashboards/create"] = []any{map[string]any{"id": "home"}}
	ws.raiseOn["lovelace/config/save"] = []error{&wsclient.Error{Code: "unknown_error", Message: "save boom"}}
	managed := map[string]string{}

	result := ApplyDashboardPlan(context.Background(), staticDialer(ws), ops, managed, stashDir)

	if result.OK {
		t.Fatalf("result = %+v, want failure", result)
	}
	if !strings.Contains(result.Error, "save boom") {
		t.Errorf("error = %q", result.Error)
	}
	if !result.RolledBack {
		t.Errorf("result.RolledBack = false, want true - the create was recorded, then successfully inverted")
	}
	if len(managed) != 0 {
		t.Errorf("managed = %+v, want empty - the create was inverted", managed)
	}
	deleteCalls := ws.callsFor("lovelace/dashboards/delete")
	want := map[string]any{"dashboard_id": "home"}
	if len(deleteCalls) != 1 || !reflect.DeepEqual(deleteCalls[0].params, want) {
		t.Errorf("inverse delete calls = %+v, want %+v", deleteCalls, want)
	}
	// Stashed mid-op then inverted, so the file exists but is empty -
	// nothing left to roll back a second time.
	stash := readStash(t, stashDir)
	if ops2, _ := stash["ops"].([]any); len(ops2) != 0 {
		t.Errorf("stash ops = %+v, want empty after successful inversion", ops2)
	}
}

// When the inverse delete also fails RolledBack is false, but the stash
// entry is still dropped, matching inverseReplayAndPersist's
// write-before-invert polarity: dashboardManaged keeps it tracked for the
// next reconcile rather than leaving a phantom retry target.
func TestApplyDashboardPlanCreateContentFailureInverseAlsoFailsStaysTracked(t *testing.T) {
	stashDir := t.TempDir()
	params := map[string]any{
		"metadata": map[string]any{"title": "Home", "show_in_sidebar": true},
		"content":  map[string]any{"views": []any{}},
	}
	ops := []registries.RegOp{dashboardOp(registries.KindCreate, "home", params, "")}
	ws := newFakeWS()
	ws.results["lovelace/dashboards/create"] = []any{map[string]any{"id": "home"}}
	ws.raiseOn["lovelace/config/save"] = []error{&wsclient.Error{Code: "unknown_error", Message: "save boom"}}
	ws.raiseOn["lovelace/dashboards/delete"] = []error{&wsclient.Error{Code: "unknown_error", Message: "cleanup boom"}}
	managed := map[string]string{}

	result := ApplyDashboardPlan(context.Background(), staticDialer(ws), ops, managed, stashDir)

	if result.OK || result.RolledBack {
		t.Fatalf("result = %+v, want failure and RolledBack=false", result)
	}
	if !strings.Contains(result.Error, "save boom") || !strings.Contains(result.Error, "cleanup boom") {
		t.Errorf("error = %q, want both failures mentioned", result.Error)
	}
	if managed["dashboard:home"] != "home" {
		t.Errorf("managed = %+v, want the still-live create still tracked for ordinary reconciliation", managed)
	}
	stash := readStash(t, stashDir)
	if ops2, _ := stash["ops"].([]any); len(ops2) != 0 {
		t.Errorf("stash ops = %+v, want empty - dropped per inverseReplayAndPersist's own polarity", ops2)
	}
}

// The connection dies between the create and content calls: the stashed
// entry must be inverted on a fresh dial, never on the dead connection.
func TestApplyDashboardPlanCreateContentFailureRedialsForInverse(t *testing.T) {
	stashDir := t.TempDir()
	params := map[string]any{
		"metadata": map[string]any{"title": "Home", "show_in_sidebar": true},
		"content":  map[string]any{"views": []any{}},
	}
	ops := []registries.RegOp{dashboardOp(registries.KindCreate, "home", params, "")}

	ws1 := newFakeWS()
	ws1.results["lovelace/dashboards/create"] = []any{map[string]any{"id": "home"}}
	ws1.raiseOn["lovelace/config/save"] = []error{&wsclient.Error{Code: "transport", Message: "socket is already closed."}}
	ws2 := newFakeWS()

	dialCount := 0
	dialer := func(context.Context) (WSClient, error) {
		dialCount++
		if dialCount == 1 {
			return ws1, nil
		}
		return ws2, nil
	}

	managed := map[string]string{}
	result := ApplyDashboardPlan(context.Background(), dialer, ops, managed, stashDir)

	if result.OK || !result.RolledBack {
		t.Fatalf("result = %+v, want failure with a successful rollback", result)
	}
	if dialCount != 2 {
		t.Errorf("dialCount = %d, want 2 (initial dial + redial for the inverse)", dialCount)
	}
	if len(ws1.callsFor("lovelace/dashboards/delete")) != 0 {
		t.Errorf("ws1 (the dead connection) must never be asked to compensate, calls = %+v", ws1.calls)
	}
	deleteCalls := ws2.callsFor("lovelace/dashboards/delete")
	want := map[string]any{"dashboard_id": "home"}
	if len(deleteCalls) != 1 || !reflect.DeepEqual(deleteCalls[0].params, want) {
		t.Errorf("ws2 (redialed) delete calls = %+v, want %+v", deleteCalls, want)
	}
	if len(managed) != 0 {
		t.Errorf("managed = %+v, want empty - the create was inverted", managed)
	}
	stash := readStash(t, stashDir)
	if ops2, _ := stash["ops"].([]any); len(ops2) != 0 {
		t.Errorf("stash ops = %+v, want empty after successful inversion", ops2)
	}
}

// --- ApplyDashboardPlan(): update ---------------------------------------

func TestApplyDashboardPlanUpdateMetadataOnly(t *testing.T) {
	stashDir := t.TempDir()
	params := map[string]any{"metadata": map[string]any{"title": "New Title", "show_in_sidebar": true}}
	ops := []registries.RegOp{dashboardOp(registries.KindUpdate, "home", params, "abc123")}
	ws := newFakeWS()
	ws.results["lovelace/dashboards/list"] = []any{[]any{
		map[string]any{"id": "abc123", "url_path": "home", "title": "Old Title", "show_in_sidebar": true},
	}}
	managed := map[string]string{"dashboard:home": "abc123"}

	result := ApplyDashboardPlan(context.Background(), staticDialer(ws), ops, managed, stashDir)

	if !result.OK {
		t.Fatalf("result = %+v", result)
	}
	updateCalls := ws.callsFor("lovelace/dashboards/update")
	want := map[string]any{"dashboard_id": "abc123", "title": "New Title", "show_in_sidebar": true}
	if len(updateCalls) != 1 || !reflect.DeepEqual(updateCalls[0].params, want) {
		t.Errorf("update call = %+v, want %+v", updateCalls, want)
	}
	if len(ws.callsFor("lovelace/config/save")) != 0 {
		t.Errorf("config/save should not have been called")
	}
	if managed["dashboard:home"] != "abc123" {
		t.Errorf("managed = %+v", managed)
	}
}

func TestApplyDashboardPlanUpdateContentOnly(t *testing.T) {
	stashDir := t.TempDir()
	params := map[string]any{"content": map[string]any{"views": []any{"new"}}}
	ops := []registries.RegOp{dashboardOp(registries.KindUpdate, "home", params, "abc123")}
	ws := newFakeWS()
	ws.results["lovelace/dashboards/list"] = []any{[]any{
		map[string]any{"id": "abc123", "url_path": "home", "title": "Home"},
	}}
	ws.results["lovelace/config"] = []any{map[string]any{"views": []any{}}}

	result := ApplyDashboardPlan(context.Background(), staticDialer(ws), ops, map[string]string{"dashboard:home": "abc123"}, stashDir)

	if !result.OK {
		t.Fatalf("result = %+v", result)
	}
	if len(ws.callsFor("lovelace/dashboards/update")) != 0 {
		t.Errorf("dashboards/update should not have been called")
	}
	saveCalls := ws.callsFor("lovelace/config/save")
	want := map[string]any{"url_path": "home", "config": map[string]any{"views": []any{"new"}}}
	if len(saveCalls) != 1 || !reflect.DeepEqual(saveCalls[0].params, want) {
		t.Errorf("save call = %+v, want %+v", saveCalls, want)
	}
}

// The metadata call is stashed before the content call, so the revert
// seen here is inverseReplayAndPersist, not an inline same-connection one.
func TestApplyDashboardPlanUpdateContentFailureRevertsMetadata(t *testing.T) {
	stashDir := t.TempDir()
	params := map[string]any{
		"metadata": map[string]any{"title": "New Title", "show_in_sidebar": true},
		"content":  map[string]any{"views": []any{"new"}},
	}
	ops := []registries.RegOp{dashboardOp(registries.KindUpdate, "home", params, "abc123")}
	ws := newFakeWS()
	ws.results["lovelace/dashboards/list"] = []any{[]any{
		map[string]any{"id": "abc123", "url_path": "home", "title": "Old Title", "show_in_sidebar": true},
	}}
	ws.raiseOn["lovelace/config/save"] = []error{&wsclient.Error{Code: "unknown_error", Message: "save boom"}}
	managed := map[string]string{"dashboard:home": "abc123"}

	result := ApplyDashboardPlan(context.Background(), staticDialer(ws), ops, managed, stashDir)

	if result.OK {
		t.Fatalf("result = %+v, want failure", result)
	}
	if !result.RolledBack {
		t.Errorf("result.RolledBack = false, want true - the metadata half was recorded, then successfully inverted")
	}
	updateCalls := ws.callsFor("lovelace/dashboards/update")
	if len(updateCalls) != 2 {
		t.Fatalf("update calls = %+v, want forward + revert", updateCalls)
	}
	revert := updateCalls[1]
	want := map[string]any{"dashboard_id": "abc123", "title": "Old Title", "show_in_sidebar": true}
	if !reflect.DeepEqual(revert.params, want) {
		t.Errorf("revert params = %+v, want %+v", revert.params, want)
	}
	stash := readStash(t, stashDir)
	if ops2, _ := stash["ops"].([]any); len(ops2) != 0 {
		t.Errorf("stash ops = %+v, want empty after successful inversion", ops2)
	}
}

// Update-side counterpart to the create redial test: the stashed metadata
// entry must be inverted on a fresh dial, not the connection that died.
func TestApplyDashboardPlanUpdateContentFailureRedialsForInverse(t *testing.T) {
	stashDir := t.TempDir()
	params := map[string]any{
		"metadata": map[string]any{"title": "New Title", "show_in_sidebar": true},
		"content":  map[string]any{"views": []any{"new"}},
	}
	ops := []registries.RegOp{dashboardOp(registries.KindUpdate, "home", params, "abc123")}

	ws1 := newFakeWS()
	ws1.results["lovelace/dashboards/list"] = []any{[]any{
		map[string]any{"id": "abc123", "url_path": "home", "title": "Old Title", "show_in_sidebar": true},
	}}
	ws1.raiseOn["lovelace/config/save"] = []error{&wsclient.Error{Code: "transport", Message: "socket is already closed."}}
	ws2 := newFakeWS()

	dialCount := 0
	dialer := func(context.Context) (WSClient, error) {
		dialCount++
		if dialCount == 1 {
			return ws1, nil
		}
		return ws2, nil
	}

	managed := map[string]string{"dashboard:home": "abc123"}
	result := ApplyDashboardPlan(context.Background(), dialer, ops, managed, stashDir)

	if result.OK || !result.RolledBack {
		t.Fatalf("result = %+v, want failure with a successful rollback", result)
	}
	if dialCount != 2 {
		t.Errorf("dialCount = %d, want 2 (initial dial + redial for the inverse)", dialCount)
	}
	if len(ws1.callsFor("lovelace/dashboards/update")) != 1 {
		t.Errorf("ws1 (the dead connection) must only have the forward update, calls = %+v", ws1.calls)
	}
	revertCalls := ws2.callsFor("lovelace/dashboards/update")
	want := map[string]any{"dashboard_id": "abc123", "title": "Old Title", "show_in_sidebar": true}
	if len(revertCalls) != 1 || !reflect.DeepEqual(revertCalls[0].params, want) {
		t.Errorf("ws2 (redialed) revert calls = %+v, want %+v", revertCalls, want)
	}
	stash := readStash(t, stashDir)
	if ops2, _ := stash["ops"].([]any); len(ops2) != 0 {
		t.Errorf("stash ops = %+v, want empty after successful inversion", ops2)
	}
}

func TestApplyDashboardPlanAdoptForcesMetadataCallEvenWithNoDrift(t *testing.T) {
	stashDir := t.TempDir()
	params := map[string]any{"metadata": map[string]any{"title": "Home", "show_in_sidebar": true}}
	ops := []registries.RegOp{dashboardOp(registries.KindUpdate, "home", params, "abc123")}
	ws := newFakeWS()
	ws.results["lovelace/dashboards/list"] = []any{[]any{
		map[string]any{"id": "abc123", "url_path": "home", "title": "Home", "show_in_sidebar": true},
	}}
	managed := map[string]string{}

	result := ApplyDashboardPlan(context.Background(), staticDialer(ws), ops, managed, stashDir)

	if !result.OK {
		t.Fatalf("result = %+v", result)
	}
	if len(ws.callsFor("lovelace/dashboards/update")) != 1 {
		t.Errorf("update calls = %+v, want exactly 1 (records the adopt)", ws.callsFor("lovelace/dashboards/update"))
	}
	if managed["dashboard:home"] != "abc123" {
		t.Errorf("managed = %+v, want the adopt recorded", managed)
	}
}

// --- ApplyDashboardPlan(): delete ---------------------------------------

func TestApplyDashboardPlanDeleteStashesFullPriorMetadataAndContent(t *testing.T) {
	stashDir := t.TempDir()
	ops := []registries.RegOp{dashboardOp(registries.KindDelete, "home", nil, "abc123")}
	ws := newFakeWS()
	ws.results["lovelace/dashboards/list"] = []any{[]any{
		map[string]any{"id": "abc123", "url_path": "home", "title": "Home", "icon": "mdi:home", "show_in_sidebar": true},
	}}
	ws.results["lovelace/config"] = []any{map[string]any{"views": []any{"a"}}}
	managed := map[string]string{"dashboard:home": "abc123"}

	result := ApplyDashboardPlan(context.Background(), staticDialer(ws), ops, managed, stashDir)

	if !result.OK {
		t.Fatalf("result = %+v", result)
	}
	if len(managed) != 0 {
		t.Errorf("managed = %+v, want empty", managed)
	}
	deleteCalls := ws.callsFor("lovelace/dashboards/delete")
	if len(deleteCalls) != 1 || deleteCalls[0].params["dashboard_id"] != "abc123" {
		t.Errorf("delete calls = %+v", deleteCalls)
	}

	stash := readStash(t, stashDir)
	ops2, _ := stash["ops"].([]any)
	if len(ops2) != 1 {
		t.Fatalf("stash ops = %+v", ops2)
	}
	entry, _ := ops2[0].(map[string]any)
	liveObject, _ := entry["live_object"].(map[string]any)
	metadata, _ := liveObject["metadata"].(map[string]any)
	if metadata["title"] != "Home" || metadata["icon"] != "mdi:home" {
		t.Errorf("stashed metadata = %+v", metadata)
	}
	content, _ := liveObject["content"].(map[string]any)
	if !reflect.DeepEqual(content, map[string]any{"views": []any{"a"}}) {
		t.Errorf("stashed content = %+v", content)
	}
}

// --- ApplyDashboardPlan(): error ops skipped -----------------------------

func TestApplyDashboardPlanSkipsErrorOps(t *testing.T) {
	errOp := registries.RegOp{Kind: registries.KindError, RType: "dashboard", Key: "broken", Error: "dashboard config file could not be loaded"}
	createOp := dashboardOp(registries.KindCreate, "home", map[string]any{"metadata": map[string]any{"title": "Home", "show_in_sidebar": true}}, "")
	ws := newFakeWS()
	ws.results["lovelace/dashboards/create"] = []any{map[string]any{"id": "home"}}

	result := ApplyDashboardPlan(
		context.Background(), staticDialer(ws), []registries.RegOp{errOp, createOp}, map[string]string{}, t.TempDir())

	if !result.OK {
		t.Fatalf("result = %+v", result)
	}
	if len(result.SkippedErrors) != 1 || result.SkippedErrors[0].Key != "broken" {
		t.Errorf("skipped = %+v", result.SkippedErrors)
	}
}

// --- ApplyDashboardPlan(): shares registry_stash.json --------------------

func TestApplyDashboardPlanAppendsToStashOtherLayersAlreadyWrote(t *testing.T) {
	stashDir := t.TempDir()

	regPlan := []registries.RegOp{regOp(registries.KindCreate, "floor", "ground", map[string]any{"name": "Ground"}, "")}
	regWS := newFakeWS()
	regWS.results["config/floor_registry/create"] = []any{map[string]any{"floor_id": "F1", "name": "Ground"}}
	if !ApplyPlan(context.Background(), staticDialer(regWS), regPlan, map[string]string{}, stashDir).OK {
		t.Fatal("registries apply setup failed")
	}

	dashOps := []registries.RegOp{dashboardOp(registries.KindCreate, "home", map[string]any{"metadata": map[string]any{"title": "Home", "show_in_sidebar": true}}, "")}
	dashWS := newFakeWS()
	dashWS.results["lovelace/dashboards/create"] = []any{map[string]any{"id": "home"}}
	dashResult := ApplyDashboardPlan(context.Background(), staticDialer(dashWS), dashOps, map[string]string{}, stashDir)
	if !dashResult.OK {
		t.Fatalf("dashboard apply result = %+v", dashResult)
	}

	stash := readStash(t, stashDir)
	kinds := stashOpKinds(t, stash)
	if !reflect.DeepEqual(kinds, []string{"create", "create"}) {
		t.Errorf("stash kinds = %+v, want the floor create preserved ahead of the dashboard create", kinds)
	}
}

func TestApplyDashboardPlanMidPlanFailurePreservesEarlierLayerPrefix(t *testing.T) {
	stashDir := t.TempDir()

	regPlan := []registries.RegOp{regOp(registries.KindCreate, "floor", "ground", map[string]any{"name": "Ground"}, "")}
	regWS := newFakeWS()
	regWS.results["config/floor_registry/create"] = []any{map[string]any{"floor_id": "F1", "name": "Ground"}}
	if !ApplyPlan(context.Background(), staticDialer(regWS), regPlan, map[string]string{}, stashDir).OK {
		t.Fatal("registries apply setup failed")
	}

	dashOps := []registries.RegOp{
		dashboardOp(registries.KindCreate, "a", map[string]any{"metadata": map[string]any{"title": "A", "show_in_sidebar": true}}, ""),
		dashboardOp(registries.KindCreate, "b", map[string]any{"metadata": map[string]any{"title": "B", "show_in_sidebar": true}}, ""),
	}
	dashWS := newFakeWS()
	// b's create comes back with no id, which counts as a failure. fakeWS
	// cannot raise on only the second call of one msgType: its raiseOn
	// queue applies once non-empty and would blank a's result too.
	dashWS.results["lovelace/dashboards/create"] = []any{
		map[string]any{"id": "a"},
		map[string]any{},
	}
	managed := map[string]string{}

	result := ApplyDashboardPlan(context.Background(), staticDialer(dashWS), dashOps, managed, stashDir)

	if result.OK || !result.RolledBack {
		t.Fatalf("result = %+v", result)
	}
	if len(managed) != 0 {
		t.Errorf("managed = %+v, want empty (a's create was inverted)", managed)
	}
	stash := readStash(t, stashDir)
	kinds := stashOpKinds(t, stash)
	if !reflect.DeepEqual(kinds, []string{"create"}) {
		t.Errorf("stash kinds = %+v, want only the preserved floor create", kinds)
	}
	deleteCalls := dashWS.callsFor("lovelace/dashboards/delete")
	if len(deleteCalls) != 1 || deleteCalls[0].params["dashboard_id"] != "a" {
		t.Errorf("delete calls = %+v, want a's create inverted", deleteCalls)
	}
}

// --- RollbackRegistry(): inverts dashboard entries ------------------------

func TestRollbackRegistryInvertsDashboardCreate(t *testing.T) {
	stashDir := t.TempDir()
	ops := []registries.RegOp{dashboardOp(registries.KindCreate, "home", map[string]any{"metadata": map[string]any{"title": "Home", "show_in_sidebar": true}}, "")}
	ws := newFakeWS()
	ws.results["lovelace/dashboards/create"] = []any{map[string]any{"id": "abc123"}}
	managed := map[string]string{}
	if !ApplyDashboardPlan(context.Background(), staticDialer(ws), ops, managed, stashDir).OK {
		t.Fatal("apply setup failed")
	}

	rollbackWS := newFakeWS()
	result := RollbackRegistry(context.Background(), staticDialer(rollbackWS), stashDir, map[string]string{}, nil, managed)

	if !result.OK || !result.RolledBack {
		t.Fatalf("result = %+v", result)
	}
	if len(managed) != 0 {
		t.Errorf("managed = %+v, want empty", managed)
	}
	deleteCalls := rollbackWS.callsFor("lovelace/dashboards/delete")
	if len(deleteCalls) != 1 || deleteCalls[0].params["dashboard_id"] != "abc123" {
		t.Errorf("delete calls = %+v", deleteCalls)
	}
}

func TestRollbackRegistryInvertsDashboardUpdateRestoringOnlyTouchedAxes(t *testing.T) {
	stashDir := t.TempDir()
	params := map[string]any{"metadata": map[string]any{"title": "New Title", "show_in_sidebar": true}}
	ops := []registries.RegOp{dashboardOp(registries.KindUpdate, "home", params, "abc123")}
	ws := newFakeWS()
	ws.results["lovelace/dashboards/list"] = []any{[]any{
		map[string]any{"id": "abc123", "url_path": "home", "title": "Old Title", "show_in_sidebar": true},
	}}
	managed := map[string]string{"dashboard:home": "abc123"}
	if !ApplyDashboardPlan(context.Background(), staticDialer(ws), ops, managed, stashDir).OK {
		t.Fatal("apply setup failed")
	}

	rollbackWS := newFakeWS()
	result := RollbackRegistry(context.Background(), staticDialer(rollbackWS), stashDir, map[string]string{}, nil, managed)

	if !result.OK {
		t.Fatalf("result = %+v", result)
	}
	updateCalls := rollbackWS.callsFor("lovelace/dashboards/update")
	want := map[string]any{"dashboard_id": "abc123", "title": "Old Title", "show_in_sidebar": true}
	if len(updateCalls) != 1 || !reflect.DeepEqual(updateCalls[0].params, want) {
		t.Errorf("update call = %+v, want %+v", updateCalls, want)
	}
	if len(rollbackWS.callsFor("lovelace/config/save")) != 0 {
		t.Errorf("config/save should not have been called - content was never touched by the forward op")
	}
}

func TestRollbackRegistryInvertsDashboardUpdateRestoringPriorContent(t *testing.T) {
	stashDir := t.TempDir()
	params := map[string]any{"content": map[string]any{"views": []any{"new"}}}
	ops := []registries.RegOp{dashboardOp(registries.KindUpdate, "home", params, "abc123")}
	ws := newFakeWS()
	ws.results["lovelace/dashboards/list"] = []any{[]any{
		map[string]any{"id": "abc123", "url_path": "home", "title": "Home"},
	}}
	ws.results["lovelace/config"] = []any{map[string]any{"views": []any{"old"}}}
	managed := map[string]string{"dashboard:home": "abc123"}
	if !ApplyDashboardPlan(context.Background(), staticDialer(ws), ops, managed, stashDir).OK {
		t.Fatal("apply setup failed")
	}

	rollbackWS := newFakeWS()
	result := RollbackRegistry(context.Background(), staticDialer(rollbackWS), stashDir, map[string]string{}, nil, managed)

	if !result.OK {
		t.Fatalf("result = %+v", result)
	}
	saveCalls := rollbackWS.callsFor("lovelace/config/save")
	want := map[string]any{"url_path": "home", "config": map[string]any{"views": []any{"old"}}}
	if len(saveCalls) != 1 || !reflect.DeepEqual(saveCalls[0].params, want) {
		t.Errorf("save call = %+v, want %+v", saveCalls, want)
	}
}

func TestRollbackRegistryInvertsDashboardUpdateContentNeverExistedLeavesItAlone(t *testing.T) {
	stashDir := t.TempDir()
	params := map[string]any{"content": map[string]any{"views": []any{"new"}}}
	ops := []registries.RegOp{dashboardOp(registries.KindUpdate, "home", params, "abc123")}
	ws := newFakeWS()
	ws.results["lovelace/dashboards/list"] = []any{[]any{
		map[string]any{"id": "abc123", "url_path": "home", "title": "Home"},
	}}
	// lovelace/config returns nothing configured (default fakeWS nil result
	// mimics config_not_found already having been normalized to nil).
	managed := map[string]string{"dashboard:home": "abc123"}
	if !ApplyDashboardPlan(context.Background(), staticDialer(ws), ops, managed, stashDir).OK {
		t.Fatal("apply setup failed")
	}

	rollbackWS := newFakeWS()
	result := RollbackRegistry(context.Background(), staticDialer(rollbackWS), stashDir, map[string]string{}, nil, managed)

	if !result.OK {
		t.Fatalf("result = %+v", result)
	}
	if len(rollbackWS.callsFor("lovelace/config/save")) != 0 {
		t.Errorf("config/save should not have been called - there was no prior content to restore")
	}
}

func TestRollbackRegistryInvertsDashboardDeleteRecreatingWithContent(t *testing.T) {
	stashDir := t.TempDir()
	ops := []registries.RegOp{dashboardOp(registries.KindDelete, "home", nil, "abc123")}
	ws := newFakeWS()
	ws.results["lovelace/dashboards/list"] = []any{[]any{
		map[string]any{"id": "abc123", "url_path": "home", "title": "Home", "icon": "mdi:home", "show_in_sidebar": true},
	}}
	ws.results["lovelace/config"] = []any{map[string]any{"views": []any{"a"}}}
	managed := map[string]string{"dashboard:home": "abc123"}
	if !ApplyDashboardPlan(context.Background(), staticDialer(ws), ops, managed, stashDir).OK {
		t.Fatal("apply setup failed")
	}
	if len(managed) != 0 {
		t.Fatalf("managed after delete = %+v, want empty", managed)
	}

	rollbackWS := newFakeWS()
	rollbackWS.results["lovelace/dashboards/create"] = []any{map[string]any{"id": "new-id"}}
	result := RollbackRegistry(context.Background(), staticDialer(rollbackWS), stashDir, map[string]string{}, nil, managed)

	if !result.OK {
		t.Fatalf("result = %+v", result)
	}
	if managed["dashboard:home"] != "new-id" {
		t.Errorf("managed = %+v, want dashboard:home remapped to new-id", managed)
	}
	createCalls := rollbackWS.callsFor("lovelace/dashboards/create")
	want := map[string]any{"url_path": "home", "allow_single_word": true, "title": "Home", "icon": "mdi:home", "show_in_sidebar": true}
	if len(createCalls) != 1 || !reflect.DeepEqual(createCalls[0].params, want) {
		t.Errorf("create call = %+v, want %+v", createCalls, want)
	}
	saveCalls := rollbackWS.callsFor("lovelace/config/save")
	wantSave := map[string]any{"url_path": "home", "config": map[string]any{"views": []any{"a"}}}
	if len(saveCalls) != 1 || !reflect.DeepEqual(saveCalls[0].params, wantSave) {
		t.Errorf("save call = %+v, want %+v", saveCalls, wantSave)
	}
}

// Rolling back an admin-only dashboard's delete must recreate it
// admin-only, not widen it by dropping require_admin.
func TestRollbackRegistryInvertsDashboardDeleteRecreatingWithRequireAdmin(t *testing.T) {
	stashDir := t.TempDir()
	ops := []registries.RegOp{dashboardOp(registries.KindDelete, "home", nil, "abc123")}
	ws := newFakeWS()
	ws.results["lovelace/dashboards/list"] = []any{[]any{
		map[string]any{"id": "abc123", "url_path": "home", "title": "Home", "show_in_sidebar": true, "require_admin": true},
	}}
	managed := map[string]string{"dashboard:home": "abc123"}
	if !ApplyDashboardPlan(context.Background(), staticDialer(ws), ops, managed, stashDir).OK {
		t.Fatal("apply setup failed")
	}

	rollbackWS := newFakeWS()
	rollbackWS.results["lovelace/dashboards/create"] = []any{map[string]any{"id": "new-id"}}
	result := RollbackRegistry(context.Background(), staticDialer(rollbackWS), stashDir, map[string]string{}, nil, managed)

	if !result.OK {
		t.Fatalf("result = %+v", result)
	}
	createCalls := rollbackWS.callsFor("lovelace/dashboards/create")
	if len(createCalls) != 1 || createCalls[0].params["require_admin"] != true {
		t.Errorf("create call = %+v, want require_admin: true carried through", createCalls)
	}
}

func TestRollbackRegistryInvertsCombinedStashIncludingDashboardAndEntity(t *testing.T) {
	stashDir := t.TempDir()

	entOps := []registries.RegOp{entityOp(registries.KindUpdate, "light.x", map[string]any{"name": "New"})}
	entWS := newFakeWS()
	entWS.results["config/entity_registry/list"] = []any{[]any{map[string]any{"entity_id": "light.x", "name": "Old"}}}
	originals := map[string]map[string]any{}
	if !ApplyEntityPlan(context.Background(), staticDialer(entWS), entOps, originals, stashDir).OK {
		t.Fatal("entity apply setup failed")
	}

	dashOps := []registries.RegOp{dashboardOp(registries.KindCreate, "home", map[string]any{"metadata": map[string]any{"title": "Home", "show_in_sidebar": true}}, "")}
	dashWS := newFakeWS()
	dashWS.results["lovelace/dashboards/create"] = []any{map[string]any{"id": "abc123"}}
	dashboardManaged := map[string]string{}
	if !ApplyDashboardPlan(context.Background(), staticDialer(dashWS), dashOps, dashboardManaged, stashDir).OK {
		t.Fatal("dashboard apply setup failed")
	}

	rollbackWS := newFakeWS()
	result := RollbackRegistry(context.Background(), staticDialer(rollbackWS), stashDir, map[string]string{}, originals, dashboardManaged)

	if !result.OK || !result.RolledBack {
		t.Fatalf("result = %+v", result)
	}
	if len(originals) != 0 {
		t.Errorf("originals = %+v, want empty", originals)
	}
	if len(dashboardManaged) != 0 {
		t.Errorf("dashboardManaged = %+v, want empty", dashboardManaged)
	}

	// Reverse order: the dashboard create (applied last) must invert
	// before the entity update (applied first).
	dashInvertIdx, entityInvertIdx := -1, -1
	for i, c := range rollbackWS.calls {
		if c.msgType == "lovelace/dashboards/delete" {
			dashInvertIdx = i
		}
		if c.msgType == "config/entity_registry/update" {
			entityInvertIdx = i
		}
	}
	if dashInvertIdx == -1 || entityInvertIdx == -1 || dashInvertIdx > entityInvertIdx {
		t.Errorf("calls = %+v, want dashboard invert before entity invert", rollbackWS.calls)
	}
}
