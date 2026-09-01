package regapply

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/registries"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/wsclient"
)

// --- fakeWS: records every Cmd() call, and answers each msgType from a
// FIFO of results and/or errors (missing or exhausted queues give nil).

type wsCall struct {
	msgType string
	params  map[string]any
}

type fakeWS struct {
	calls   []wsCall
	results map[string][]any
	raiseOn map[string][]error
	closed  bool
	// timeouts records the budget each call was given (hacsDownloadTimeout);
	// 0 is the client's own default, which ordinary calls pass.
	timeouts []time.Duration
}

func newFakeWS() *fakeWS {
	return &fakeWS{results: map[string][]any{}, raiseOn: map[string][]error{}}
}

func (f *fakeWS) Cmd(ctx context.Context, msgType string, params map[string]any) (any, error) {
	return f.CmdTimeout(ctx, msgType, params, 0)
}

func (f *fakeWS) CmdTimeout(
	_ context.Context, msgType string, params map[string]any, timeout time.Duration,
) (any, error) {
	f.calls = append(f.calls, wsCall{msgType: msgType, params: params})
	f.timeouts = append(f.timeouts, timeout)
	if errs := f.raiseOn[msgType]; len(errs) > 0 {
		err := errs[0]
		f.raiseOn[msgType] = errs[1:]
		return nil, err
	}
	if results := f.results[msgType]; len(results) > 0 {
		result := results[0]
		f.results[msgType] = results[1:]
		return result, nil
	}
	return nil, nil
}

func (f *fakeWS) Close() { f.closed = true }

func (f *fakeWS) callTypes() []string {
	out := make([]string, len(f.calls))
	for i, c := range f.calls {
		out[i] = c.msgType
	}
	return out
}

func (f *fakeWS) callsFor(msgType string) []wsCall {
	var out []wsCall
	for _, c := range f.calls {
		if c.msgType == msgType {
			out = append(out, c)
		}
	}
	return out
}

func staticDialer(ws WSClient) Dialer {
	return func(context.Context) (WSClient, error) { return ws, nil }
}

// snapshotEachWriteWS is fakeWS plus a stash snapshot before every
// non-*/list call, so a test can pin what was confirmed executed so far.
type snapshotEachWriteWS struct {
	*fakeWS
	stashPath            string
	snapshotsBeforeWrite []map[string]any
}

func (f *snapshotEachWriteWS) Cmd(ctx context.Context, msgType string, params map[string]any) (any, error) {
	return f.CmdTimeout(ctx, msgType, params, 0)
}

func (f *snapshotEachWriteWS) CmdTimeout(
	ctx context.Context, msgType string, params map[string]any, timeout time.Duration,
) (any, error) {
	if !strings.HasSuffix(msgType, "/list") {
		if data, err := os.ReadFile(f.stashPath); err == nil {
			var snap map[string]any
			if json.Unmarshal(data, &snap) == nil {
				f.snapshotsBeforeWrite = append(f.snapshotsBeforeWrite, snap)
			}
		}
	}
	return f.fakeWS.CmdTimeout(ctx, msgType, params, timeout)
}

func regOp(kind, rtype, key string, params map[string]any, liveID string) registries.RegOp {
	if params == nil {
		params = map[string]any{}
	}
	return registries.RegOp{Kind: kind, RType: rtype, Key: key, Params: params, LiveID: liveID, DiffText: "..."}
}

// List-response shapes matching core's _entry_dict(): every field the
// update schema accepts, plus created_at/modified_at, which it rejects.
var realisticFloor = map[string]any{
	"aliases": []any{}, "created_at": 1700000000.0, "floor_id": "F-OLD", "icon": "mdi:home",
	"level": 0, "name": "Old floor", "modified_at": 1700000500.0,
}

var realisticArea = map[string]any{
	"aliases": []any{}, "area_id": "A-OLD", "floor_id": nil, "humidity_entity_id": nil,
	"icon": "mdi:sofa", "labels": []any{}, "name": "Old room", "picture": nil,
	"temperature_entity_id": nil, "created_at": 1700000000.0, "modified_at": 1700000500.0,
}

var realisticLabel = map[string]any{
	"color": "indigo", "created_at": 1700000000.0, "description": nil,
	"icon": "mdi:source-branch", "label_id": "L-OLD", "name": "Old label", "modified_at": 1700000500.0,
}

func readStash(t *testing.T, stashDir string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(stashDir, "registry_stash.json"))
	if err != nil {
		t.Fatal(err)
	}
	var stash map[string]any
	if err := json.Unmarshal(data, &stash); err != nil {
		t.Fatal(err)
	}
	return stash
}

func stashOpKinds(t *testing.T, stash map[string]any) []string {
	t.Helper()
	ops, _ := stash["ops"].([]any)
	kinds := make([]string, len(ops))
	for i, op := range ops {
		m, _ := op.(map[string]any)
		kinds[i], _ = m["kind"].(string)
	}
	return kinds
}

// --- FetchLive ---------------------------------------------------------

func TestFetchLiveCoversRegistriesAndEveryHelperDomain(t *testing.T) {
	ws := newFakeWS()
	ws.results["config/floor_registry/list"] = []any{[]any{realisticFloor}}
	ws.results["config/area_registry/list"] = []any{[]any{realisticArea}}
	ws.results["config/label_registry/list"] = []any{[]any{realisticLabel}}
	ws.results["input_boolean/list"] = []any{[]any{map[string]any{"id": "IB1", "name": "Flag"}}}

	live, err := FetchLive(context.Background(), ws, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !reflect.DeepEqual(live["floor"], []map[string]any{realisticFloor}) {
		t.Errorf("floor = %+v", live["floor"])
	}
	if !reflect.DeepEqual(live["area"], []map[string]any{realisticArea}) {
		t.Errorf("area = %+v", live["area"])
	}
	if !reflect.DeepEqual(live["label"], []map[string]any{realisticLabel}) {
		t.Errorf("label = %+v", live["label"])
	}
	want := []map[string]any{{"id": "IB1", "name": "Flag"}}
	if !reflect.DeepEqual(live["input_boolean"], want) {
		t.Errorf("input_boolean = %+v", live["input_boolean"])
	}
	for _, domain := range registries.SupportedHelperDomains {
		if _, ok := live[domain]; !ok {
			t.Errorf("missing domain %q in live", domain)
		}
	}
	calledTypes := ws.callTypes()
	count := 0
	for _, c := range calledTypes {
		if c == "config/floor_registry/list" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("config/floor_registry/list called %d times, want 1", count)
	}
	if !contains(calledTypes, "counter/list") || !contains(calledTypes, "timer/list") {
		t.Errorf("calls = %+v, missing counter/list or timer/list", calledTypes)
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// --- ApplyPlan(): happy path / incremental stash -----------------------

func TestApplyPlanHappyPathWritesOnlyConfirmedExecutedOpsIncrementally(t *testing.T) {
	// The stash must never claim an op happened before its WS result
	// confirmed it.
	stashDir := t.TempDir()
	plan := []registries.RegOp{
		regOp(registries.KindCreate, "floor", "ground", map[string]any{"name": "Ground floor"}, ""),
		regOp(registries.KindUpdate, "area", "living_room", map[string]any{"name": "Living room", "icon": "mdi:sofa"}, "A1"),
		regOp(registries.KindDelete, "label", "old", nil, "L1"),
	}
	managed := map[string]string{"area:living_room": "A1", "label:old": "L1"}
	inner := newFakeWS()
	inner.results["config/area_registry/list"] = []any{[]any{map[string]any{"area_id": "A1", "name": "Living room", "icon": "mdi:old"}}}
	inner.results["config/label_registry/list"] = []any{[]any{map[string]any{"label_id": "L1", "name": "Old label"}}}
	inner.results["config/floor_registry/create"] = []any{map[string]any{"floor_id": "F1", "name": "Ground floor"}}
	ws := &snapshotEachWriteWS{fakeWS: inner, stashPath: filepath.Join(stashDir, "registry_stash.json")}

	result := ApplyPlan(context.Background(), staticDialer(ws), plan, managed, stashDir)

	if !result.OK || result.RolledBack || len(result.SkippedErrors) != 0 {
		t.Fatalf("result = %+v", result)
	}
	want := []string{"create floor:ground", "update area:living_room", "delete label:old"}
	if !reflect.DeepEqual(result.Applied, want) {
		t.Errorf("applied = %+v, want %+v", result.Applied, want)
	}

	if len(ws.snapshotsBeforeWrite) != 3 {
		t.Fatalf("snapshots = %d, want 3", len(ws.snapshotsBeforeWrite))
	}
	// Before the first op (create floor) ran: nothing confirmed yet.
	if ops, _ := ws.snapshotsBeforeWrite[0]["ops"].([]any); len(ops) != 0 {
		t.Errorf("snapshot[0] ops = %+v, want empty", ops)
	}
	// Before the second op: only the floor create, already carrying its
	// real live id rather than a placeholder.
	ops1, _ := ws.snapshotsBeforeWrite[1]["ops"].([]any)
	if len(ops1) != 1 {
		t.Fatalf("snapshot[1] ops = %+v, want 1 entry", ops1)
	}
	entry0, _ := ops1[0].(map[string]any)
	if entry0["kind"] != "create" || entry0["rtype"] != "floor" || entry0["key"] != "ground" || entry0["live_id"] != "F1" {
		t.Errorf("snapshot[1][0] = %+v", entry0)
	}
	if entry0["live_object"] != nil || entry0["forward_params"] != nil {
		t.Errorf("snapshot[1][0] = %+v, want live_object and forward_params both null", entry0)
	}
	// Before the third op (delete label) ran: floor create + area update.
	ops2, _ := ws.snapshotsBeforeWrite[2]["ops"].([]any)
	if len(ops2) != 2 {
		t.Fatalf("snapshot[2] ops = %+v, want 2 entries", ops2)
	}
	entry1, _ := ops2[1].(map[string]any)
	fp, _ := entry1["forward_params"].(map[string]any)
	wantFP := map[string]any{"name": "Living room", "icon": "mdi:sofa"}
	if !reflect.DeepEqual(fp, wantFP) {
		t.Errorf("forward_params = %+v, want %+v", fp, wantFP)
	}

	if !reflect.DeepEqual(managed, map[string]string{"area:living_room": "A1", "floor:ground": "F1"}) {
		t.Errorf("managed = %+v", managed)
	}

	finalStash := readStash(t, stashDir)
	if kinds := stashOpKinds(t, finalStash); !reflect.DeepEqual(kinds, []string{"create", "update", "delete"}) {
		t.Errorf("final stash kinds = %+v", kinds)
	}
}

// --- ApplyPlan(): $ref resolution ---------------------------------------

func TestApplyPlanResolvesRefToSamePlanCreate(t *testing.T) {
	plan := []registries.RegOp{
		regOp(registries.KindCreate, "floor", "ground", map[string]any{"name": "Ground floor"}, ""),
		regOp(registries.KindCreate, "area", "living_room", map[string]any{
			"name": "Living room", "floor_id": map[string]any{"$ref": "floor:ground"},
		}, ""),
	}
	managed := map[string]string{}
	ws := newFakeWS()
	ws.results["config/floor_registry/create"] = []any{map[string]any{"floor_id": "F1", "name": "Ground floor"}}
	ws.results["config/area_registry/create"] = []any{map[string]any{"area_id": "A1", "name": "Living room", "floor_id": "F1"}}

	result := ApplyPlan(context.Background(), staticDialer(ws), plan, managed, t.TempDir())

	if !result.OK {
		t.Fatalf("result = %+v", result)
	}
	calls := ws.callsFor("config/area_registry/create")
	if len(calls) != 1 {
		t.Fatalf("area create calls = %d", len(calls))
	}
	want := map[string]any{"name": "Living room", "floor_id": "F1"}
	if !reflect.DeepEqual(calls[0].params, want) {
		t.Errorf("params = %+v, want %+v", calls[0].params, want)
	}
	if !reflect.DeepEqual(managed, map[string]string{"floor:ground": "F1", "area:living_room": "A1"}) {
		t.Errorf("managed = %+v", managed)
	}
}

func TestApplyPlanRefResolvesFromLabelsListToo(t *testing.T) {
	plan := []registries.RegOp{
		regOp(registries.KindCreate, "label", "gitops", map[string]any{"name": "GitOps"}, ""),
		regOp(registries.KindCreate, "area", "living_room", map[string]any{
			"name": "Living room", "labels": []any{map[string]any{"$ref": "label:gitops"}},
		}, ""),
	}
	managed := map[string]string{}
	ws := newFakeWS()
	ws.results["config/label_registry/create"] = []any{map[string]any{"label_id": "L1", "name": "GitOps"}}
	ws.results["config/area_registry/create"] = []any{map[string]any{"area_id": "A1", "name": "Living room", "labels": []any{"L1"}}}

	result := ApplyPlan(context.Background(), staticDialer(ws), plan, managed, t.TempDir())

	if !result.OK {
		t.Fatalf("result = %+v", result)
	}
	calls := ws.callsFor("config/area_registry/create")
	want := map[string]any{"name": "Living room", "labels": []any{"L1"}}
	if !reflect.DeepEqual(calls[0].params, want) {
		t.Errorf("params = %+v, want %+v", calls[0].params, want)
	}
}

func TestApplyPlanUnresolvableRefFailsAndInverseReplays(t *testing.T) {
	plan := []registries.RegOp{
		regOp(registries.KindCreate, "floor", "ground", map[string]any{"name": "Ground floor"}, ""),
		regOp(registries.KindCreate, "area", "living_room", map[string]any{
			"name": "Living room", "floor_id": map[string]any{"$ref": "floor:other"},
		}, ""),
	}
	managed := map[string]string{}
	ws := newFakeWS()
	ws.results["config/floor_registry/create"] = []any{map[string]any{"floor_id": "F1", "name": "Ground floor"}}

	result := ApplyPlan(context.Background(), staticDialer(ws), plan, managed, t.TempDir())

	if result.OK || !result.RolledBack {
		t.Fatalf("result = %+v", result)
	}
	if !strings.Contains(result.Error, "floor:other") {
		t.Errorf("error = %q", result.Error)
	}
	if len(result.Applied) != 0 {
		t.Errorf("applied = %+v, want empty", result.Applied)
	}
	if len(managed) != 0 {
		t.Errorf("managed = %+v, want empty", managed)
	}

	callTypes := ws.callTypes()
	if contains(callTypes, "config/area_registry/create") {
		t.Errorf("area create should never have been attempted")
	}
	if n := len(ws.callsFor("config/floor_registry/create")); n != 1 {
		t.Errorf("floor create called %d times, want 1", n)
	}
	deleteCalls := ws.callsFor("config/floor_registry/delete")
	if len(deleteCalls) != 1 || !reflect.DeepEqual(deleteCalls[0].params, map[string]any{"floor_id": "F1"}) {
		t.Errorf("delete calls = %+v", deleteCalls)
	}
}

// --- ApplyPlan(): mid-plan failure + reverse-order inverse --------------

func TestApplyPlanMidPlanErrorInverseReplayReverseOrder(t *testing.T) {
	plan := []registries.RegOp{
		regOp(registries.KindCreate, "floor", "a", map[string]any{"name": "A"}, ""),
		regOp(registries.KindCreate, "label", "b", map[string]any{"name": "B"}, ""),
		regOp(registries.KindUpdate, "area", "c", map[string]any{"name": "C", "icon": "mdi:new"}, "A-C"),
	}
	managed := map[string]string{"area:c": "A-C"}
	ws := newFakeWS()
	ws.results["config/area_registry/list"] = []any{[]any{map[string]any{"area_id": "A-C", "name": "C", "icon": "mdi:old"}}}
	ws.results["config/floor_registry/create"] = []any{map[string]any{"floor_id": "F-A", "name": "A"}}
	ws.results["config/label_registry/create"] = []any{map[string]any{"label_id": "L-B", "name": "B"}}
	ws.raiseOn["config/area_registry/update"] = []error{&wsclient.Error{Code: "invalid_format", Message: "boom"}}

	result := ApplyPlan(context.Background(), staticDialer(ws), plan, managed, t.TempDir())

	if result.OK || !result.RolledBack {
		t.Fatalf("result = %+v", result)
	}
	if !strings.Contains(result.Error, "update area:c failed") {
		t.Errorf("error = %q", result.Error)
	}
	if !reflect.DeepEqual(managed, map[string]string{"area:c": "A-C"}) {
		t.Errorf("managed = %+v, want untouched", managed)
	}

	n := len(ws.calls)
	if n < 2 {
		t.Fatalf("too few calls: %+v", ws.calls)
	}
	tail := ws.calls[n-2:]
	if tail[0].msgType != "config/label_registry/delete" || !reflect.DeepEqual(tail[0].params, map[string]any{"label_id": "L-B"}) {
		t.Errorf("tail[0] = %+v", tail[0])
	}
	if tail[1].msgType != "config/floor_registry/delete" || !reflect.DeepEqual(tail[1].params, map[string]any{"floor_id": "F-A"}) {
		t.Errorf("tail[1] = %+v", tail[1])
	}
}

func TestApplyPlanDeleteInverseRecreatesAndRemapsRegistryManaged(t *testing.T) {
	// The recreate must strip created_at/modified_at: a real floor list
	// carries both, and floor_registry/create rejects unknown keys.
	plan := []registries.RegOp{
		regOp(registries.KindDelete, "floor", "old", nil, "F-OLD"),
		regOp(registries.KindUpdate, "area", "c", map[string]any{"name": "C"}, "A-C"),
	}
	managed := map[string]string{"floor:old": "F-OLD", "area:c": "A-C"}
	ws := newFakeWS()
	ws.results["config/floor_registry/list"] = []any{[]any{realisticFloor}}
	ws.results["config/area_registry/list"] = []any{[]any{map[string]any{"area_id": "A-C", "name": "C"}}}
	ws.results["config/floor_registry/create"] = []any{map[string]any{"floor_id": "F-NEW", "name": "Old floor", "level": 0}}
	ws.raiseOn["config/area_registry/update"] = []error{&wsclient.Error{Code: "unknown_error", Message: "boom"}}

	result := ApplyPlan(context.Background(), staticDialer(ws), plan, managed, t.TempDir())

	if result.OK || !result.RolledBack {
		t.Fatalf("result = %+v", result)
	}
	if managed["floor:old"] != "F-NEW" {
		t.Errorf("managed[floor:old] = %q, want F-NEW", managed["floor:old"])
	}

	recreateCalls := ws.callsFor("config/floor_registry/create")
	if len(recreateCalls) == 0 {
		t.Fatal("no recreate call")
	}
	want := map[string]any{"aliases": []any{}, "icon": "mdi:home", "level": 0, "name": "Old floor"}
	if !reflect.DeepEqual(recreateCalls[0].params, want) {
		t.Errorf("recreate params = %+v, want %+v", recreateCalls[0].params, want)
	}
}

// --- ApplyPlan(): error ops are skipped, not fatal -----------------------

func TestApplyPlanSkipsErrorOpsButExecutesRest(t *testing.T) {
	errorOp := registries.RegOp{Kind: registries.KindError, RType: "area", Key: "x", Params: map[string]any{}, Error: "ambiguous adopt: 2 live area objects named 'X'"}
	createOp := regOp(registries.KindCreate, "floor", "y", map[string]any{"name": "Y"}, "")
	managed := map[string]string{}
	ws := newFakeWS()
	ws.results["config/floor_registry/create"] = []any{map[string]any{"floor_id": "F-Y", "name": "Y"}}

	result := ApplyPlan(context.Background(), staticDialer(ws), []registries.RegOp{errorOp, createOp}, managed, t.TempDir())

	if !result.OK {
		t.Fatalf("result = %+v", result)
	}
	if len(result.SkippedErrors) != 1 || result.SkippedErrors[0].Key != "x" {
		t.Errorf("skipped_errors = %+v", result.SkippedErrors)
	}
	if !reflect.DeepEqual(result.Applied, []string{"create floor:y"}) {
		t.Errorf("applied = %+v", result.Applied)
	}
	for _, t2 := range []string{"config/area_registry/create", "config/area_registry/update", "config/area_registry/delete"} {
		if contains(ws.callTypes(), t2) {
			t.Errorf("unexpected call %q for a skipped error op", t2)
		}
	}
}

func TestApplyPlanUpdatesRegistryManagedOnNoDriftAdoption(t *testing.T) {
	op := registries.RegOp{
		Kind: registries.KindUpdate, RType: "label", Key: "gitops",
		Params: map[string]any{"name": "GitOps", "color": "indigo"}, LiveID: "L1",
		DiffText: "adopted existing label 'gitops' (live id L1); no field changes needed",
	}
	managed := map[string]string{}
	ws := newFakeWS()
	ws.results["config/label_registry/list"] = []any{[]any{map[string]any{"label_id": "L1", "name": "GitOps", "color": "indigo"}}}

	result := ApplyPlan(context.Background(), staticDialer(ws), []registries.RegOp{op}, managed, t.TempDir())

	if !result.OK {
		t.Fatalf("result = %+v", result)
	}
	if !reflect.DeepEqual(managed, map[string]string{"label:gitops": "L1"}) {
		t.Errorf("managed = %+v", managed)
	}
	calls := ws.callsFor("config/label_registry/update")
	want := map[string]any{"label_id": "L1", "name": "GitOps", "color": "indigo"}
	if len(calls) != 1 || !reflect.DeepEqual(calls[0].params, want) {
		t.Errorf("update call = %+v, want %+v", calls, want)
	}
}

func TestApplyPlanPartialInverseFailureDropsEntryFromStashButKeepsRegistryManaged(t *testing.T) {
	// The entry is dropped from the stash before its inverse is attempted,
	// so a failed inverse is not retryable, but managed still tracks it.
	stashDir := t.TempDir()
	plan := []registries.RegOp{
		regOp(registries.KindCreate, "floor", "a", map[string]any{"name": "A"}, ""),
		regOp(registries.KindCreate, "label", "b", map[string]any{"name": "B"}, ""),
		regOp(registries.KindUpdate, "area", "c", map[string]any{"name": "C"}, "A-C"),
	}
	managed := map[string]string{"area:c": "A-C"}
	ws := newFakeWS()
	ws.results["config/area_registry/list"] = []any{[]any{map[string]any{"area_id": "A-C", "name": "C"}}}
	ws.results["config/floor_registry/create"] = []any{map[string]any{"floor_id": "F-A", "name": "A"}}
	ws.results["config/label_registry/create"] = []any{map[string]any{"label_id": "L-B", "name": "B"}}
	ws.raiseOn["config/area_registry/update"] = []error{&wsclient.Error{Code: "unknown_error", Message: "area boom"}}
	ws.raiseOn["config/label_registry/delete"] = []error{&wsclient.Error{Code: "unknown_error", Message: "label delete boom"}}

	result := ApplyPlan(context.Background(), staticDialer(ws), plan, managed, stashDir)

	if result.OK || result.RolledBack {
		t.Fatalf("result = %+v", result)
	}
	if !strings.Contains(result.Error, "label:b") {
		t.Errorf("error = %q", result.Error)
	}
	// The label's delete failed so it still exists and keeps its mapping;
	// the floor's inverse succeeded, so its mapping is gone.
	if !reflect.DeepEqual(managed, map[string]string{"area:c": "A-C", "label:b": "L-B"}) {
		t.Errorf("managed = %+v", managed)
	}

	stash := readStash(t, stashDir)
	if ops, _ := stash["ops"].([]any); len(ops) != 0 {
		t.Errorf("stash ops = %+v, want empty", ops)
	}
}

// --- RollbackRegistry(, nil) -----------------------------------------------

func TestRollbackRegistryHappyPath(t *testing.T) {
	stashDir := t.TempDir()
	plan := []registries.RegOp{regOp(registries.KindCreate, "floor", "ground", map[string]any{"name": "Ground floor"}, "")}
	managed := map[string]string{}
	applyWS := newFakeWS()
	applyWS.results["config/floor_registry/create"] = []any{map[string]any{"floor_id": "F1", "name": "Ground floor"}}

	applyResult := ApplyPlan(context.Background(), staticDialer(applyWS), plan, managed, stashDir)
	if !applyResult.OK {
		t.Fatalf("apply result = %+v", applyResult)
	}
	if !reflect.DeepEqual(managed, map[string]string{"floor:ground": "F1"}) {
		t.Fatalf("managed after apply = %+v", managed)
	}

	rollbackWS := newFakeWS()
	result := RollbackRegistry(context.Background(), staticDialer(rollbackWS), stashDir, managed, nil, nil)

	if !result.OK || !result.RolledBack {
		t.Fatalf("rollback result = %+v", result)
	}
	if len(managed) != 0 {
		t.Errorf("managed = %+v, want empty", managed)
	}
	want := []wsCall{{msgType: "config/floor_registry/delete", params: map[string]any{"floor_id": "F1"}}}
	if !reflect.DeepEqual(rollbackWS.calls, want) {
		t.Errorf("calls = %+v, want %+v", rollbackWS.calls, want)
	}
}

func TestRollbackRegistryRecreateRemapsRegistryManaged(t *testing.T) {
	stashDir := t.TempDir()
	plan := []registries.RegOp{regOp(registries.KindDelete, "floor", "old", nil, "F-OLD")}
	managed := map[string]string{"floor:old": "F-OLD"}
	applyWS := newFakeWS()
	applyWS.results["config/floor_registry/list"] = []any{[]any{realisticFloor}}

	applyResult := ApplyPlan(context.Background(), staticDialer(applyWS), plan, managed, stashDir)
	if !applyResult.OK {
		t.Fatalf("apply result = %+v", applyResult)
	}
	if len(managed) != 0 {
		t.Fatalf("managed after apply = %+v", managed)
	}

	rollbackWS := newFakeWS()
	rollbackWS.results["config/floor_registry/create"] = []any{map[string]any{"floor_id": "F-NEW", "name": "Old floor", "level": 0}}
	result := RollbackRegistry(context.Background(), staticDialer(rollbackWS), stashDir, managed, nil, nil)

	if !result.OK {
		t.Fatalf("result = %+v", result)
	}
	if managed["floor:old"] != "F-NEW" {
		t.Errorf("managed = %+v", managed)
	}
	// RollbackRegistry reads the stash fresh from JSON, so numeric fields
	// decode as float64 where ApplyPlan's in-call inverse keeps Go ints.
	want := []wsCall{{
		msgType: "config/floor_registry/create",
		params:  map[string]any{"aliases": []any{}, "icon": "mdi:home", "level": float64(0), "name": "Old floor"},
	}}
	if !reflect.DeepEqual(rollbackWS.calls, want) {
		t.Errorf("calls = %+v, want %+v", rollbackWS.calls, want)
	}
}

func TestRollbackRegistryMissingStashReturnsErrorResult(t *testing.T) {
	ws := newFakeWS()
	result := RollbackRegistry(context.Background(), staticDialer(ws), filepath.Join(t.TempDir(), "does-not-exist"), map[string]string{}, nil, nil)

	if result.OK || result.RolledBack {
		t.Fatalf("result = %+v", result)
	}
	if !strings.Contains(result.Error, "registry") {
		t.Errorf("error = %q", result.Error)
	}
}

// --- helper id-field asymmetry (execution level) ------------------------

func TestApplyPlanHelperCreateUsesRealisticIDResponseShape(t *testing.T) {
	plan := []registries.RegOp{regOp(registries.KindCreate, "input_boolean", "demo_flag", map[string]any{"name": "Demo flag"}, "")}
	managed := map[string]string{}
	ws := newFakeWS()
	ws.results["input_boolean/create"] = []any{map[string]any{"id": "demo_flag", "name": "Demo flag"}}

	result := ApplyPlan(context.Background(), staticDialer(ws), plan, managed, t.TempDir())

	if !result.OK {
		t.Fatalf("result = %+v", result)
	}
	if !reflect.DeepEqual(managed, map[string]string{"input_boolean:demo_flag": "demo_flag"}) {
		t.Errorf("managed = %+v", managed)
	}
}

func TestApplyPlanHelperDeleteUsesRealisticIDResponseShape(t *testing.T) {
	plan := []registries.RegOp{regOp(registries.KindDelete, "input_boolean", "demo_flag", nil, "demo_flag")}
	managed := map[string]string{"input_boolean:demo_flag": "demo_flag"}
	ws := newFakeWS()
	ws.results["input_boolean/list"] = []any{[]any{map[string]any{"id": "demo_flag", "name": "Demo flag"}}}

	result := ApplyPlan(context.Background(), staticDialer(ws), plan, managed, t.TempDir())

	if !result.OK {
		t.Fatalf("result = %+v", result)
	}
	if len(managed) != 0 {
		t.Errorf("managed = %+v, want empty", managed)
	}
	deleteCalls := ws.callsFor("input_boolean/delete")
	if len(deleteCalls) != 1 || !reflect.DeepEqual(deleteCalls[0].params, map[string]any{"input_boolean_id": "demo_flag"}) {
		t.Errorf("delete calls = %+v", deleteCalls)
	}
}

func TestApplyPlanFullFloorAreaHelperEndToEndRealisticShapes(t *testing.T) {
	plan := []registries.RegOp{
		regOp(registries.KindCreate, "floor", "ground", map[string]any{"name": "Ground floor"}, ""),
		regOp(registries.KindCreate, "area", "living_room", map[string]any{
			"name": "Living room", "floor_id": map[string]any{"$ref": "floor:ground"},
		}, ""),
		regOp(registries.KindCreate, "input_boolean", "demo_flag", map[string]any{"name": "Demo flag"}, ""),
	}
	managed := map[string]string{}
	ws := newFakeWS()
	ws.results["config/floor_registry/create"] = []any{map[string]any{"floor_id": "F1", "name": "Ground floor"}}
	ws.results["config/area_registry/create"] = []any{map[string]any{"area_id": "A1", "name": "Living room", "floor_id": "F1"}}
	ws.results["input_boolean/create"] = []any{map[string]any{"id": "demo_flag", "name": "Demo flag"}}

	result := ApplyPlan(context.Background(), staticDialer(ws), plan, managed, t.TempDir())

	if !result.OK {
		t.Fatalf("result = %+v", result)
	}
	want := map[string]string{"floor:ground": "F1", "area:living_room": "A1", "input_boolean:demo_flag": "demo_flag"}
	if !reflect.DeepEqual(managed, want) {
		t.Errorf("managed = %+v, want %+v", managed, want)
	}
}

// --- confirmed-executed-only stash discipline ---------------------------

func TestApplyPlanTransportFailureNeverStashesAnOpThatNeverRan(t *testing.T) {
	// A transport failure on the second op leaves only the confirmed floor
	// create stashed, never a phantom entry for the delete that never ran.
	stashDir := t.TempDir()
	plan := []registries.RegOp{
		regOp(registries.KindCreate, "floor", "ground", map[string]any{"name": "Ground"}, ""),
		regOp(registries.KindDelete, "area", "old", nil, "A-OLD"),
	}
	managed := map[string]string{}
	ws := newFakeWS()
	ws.results["config/area_registry/list"] = []any{[]any{map[string]any{"area_id": "A-OLD", "name": "Old room", "icon": "mdi:x"}}}
	ws.results["config/floor_registry/create"] = []any{map[string]any{"floor_id": "F1", "name": "Ground"}}
	ws.raiseOn["config/area_registry/delete"] = []error{&wsclient.Error{Code: "transport", Message: "socket is already closed."}}
	ws.results["config/floor_registry/delete"] = []any{nil} // served by the redial

	dialCount := 0
	dialer := func(context.Context) (WSClient, error) {
		dialCount++
		return ws, nil
	}

	result := ApplyPlan(context.Background(), dialer, plan, managed, stashDir)

	if result.OK || !result.RolledBack {
		t.Fatalf("result = %+v", result)
	}
	if dialCount != 2 {
		t.Errorf("dialCount = %d, want 2 (initial dial + redial after the transport error)", dialCount)
	}

	stash := readStash(t, stashDir)
	if ops, _ := stash["ops"].([]any); len(ops) != 0 {
		t.Errorf("stash ops = %+v, want empty", ops)
	}

	// A later Rollback click must do nothing, above all never recreate
	// "Old room" a second time.
	rollbackWS := newFakeWS()
	rbResult := RollbackRegistry(context.Background(), staticDialer(rollbackWS), stashDir, managed, nil, nil)
	if !rbResult.OK {
		t.Fatalf("rollback result = %+v", rbResult)
	}
	if len(rollbackWS.calls) != 0 {
		t.Errorf("rollback calls = %+v, want none", rollbackWS.calls)
	}
}

func TestApplyPlanGenericExceptionDuringExecutionStillTriggersInverseReplay(t *testing.T) {
	// Not just *wsclient.Error/*UnresolvedRefError: any failure triggers
	// inverse-replay.
	plan := []registries.RegOp{
		regOp(registries.KindCreate, "floor", "a", map[string]any{"name": "A"}, ""),
		regOp(registries.KindUpdate, "area", "c", map[string]any{"name": "C"}, "A-C"),
	}
	managed := map[string]string{"area:c": "A-C"}
	ws := newFakeWS()
	ws.results["config/area_registry/list"] = []any{[]any{map[string]any{"area_id": "A-C", "name": "C"}}}
	ws.results["config/floor_registry/create"] = []any{map[string]any{"floor_id": "F-A", "name": "A"}}
	ws.raiseOn["config/area_registry/update"] = []error{errors.New("not a wsclient error")}

	result := ApplyPlan(context.Background(), staticDialer(ws), plan, managed, t.TempDir())

	if result.OK || !result.RolledBack {
		t.Fatalf("result = %+v", result)
	}
	if !strings.Contains(result.Error, "not a wsclient error") {
		t.Errorf("error = %q", result.Error)
	}
	deleteCalls := ws.callsFor("config/floor_registry/delete")
	if len(deleteCalls) != 1 || !reflect.DeepEqual(deleteCalls[0].params, map[string]any{"floor_id": "F-A"}) {
		t.Errorf("delete calls = %+v", deleteCalls)
	}
}

func TestApplyPlanNeverPanicsEvenOnUnexpectedFetchLiveFailure(t *testing.T) {
	ws := newFakeWS()
	ws.raiseOn["config/floor_registry/list"] = []error{errors.New("boom")}

	result := ApplyPlan(
		context.Background(), staticDialer(ws),
		[]registries.RegOp{regOp(registries.KindCreate, "floor", "ground", map[string]any{"name": "Ground"}, "")},
		map[string]string{}, t.TempDir(),
	)

	if result.OK {
		t.Fatalf("result = %+v", result)
	}
	if !strings.Contains(result.Error, "boom") {
		t.Errorf("error = %q", result.Error)
	}
}

func TestRollbackRegistryAfterPartialFailureSecondAttemptMakesNoFurtherCalls(t *testing.T) {
	// Entries are dropped from the stash before their inverse is attempted,
	// so one whose inverse fails is not retryable on a second click.
	stashDir := t.TempDir()
	if err := writeRegistryStash(stashDir, []stashEntry{
		{Kind: registries.KindCreate, RType: "floor", Key: "ground", LiveID: "F1"},
		{Kind: registries.KindDelete, RType: "area", Key: "old", LiveID: "A-OLD", PriorObject: realisticArea},
	}); err != nil {
		t.Fatal(err)
	}
	managed := map[string]string{"floor:ground": "F1"}

	// Inversion runs in reverse: the area recreate succeeds first, then
	// the floor delete fails.
	ws1 := newFakeWS()
	ws1.results["config/area_registry/create"] = []any{map[string]any{"area_id": "A-NEW", "name": "Old room"}}
	ws1.raiseOn["config/floor_registry/delete"] = []error{&wsclient.Error{Code: "unknown_error", Message: "boom"}}
	result1 := RollbackRegistry(context.Background(), staticDialer(ws1), stashDir, managed, nil, nil)

	if result1.OK {
		t.Fatalf("result1 = %+v", result1)
	}
	wantCalls := []wsCall{
		{msgType: "config/area_registry/create", params: map[string]any{
			"aliases": []any{}, "floor_id": nil, "humidity_entity_id": nil, "icon": "mdi:sofa",
			"labels": []any{}, "name": "Old room", "picture": nil, "temperature_entity_id": nil,
		}},
		{msgType: "config/floor_registry/delete", params: map[string]any{"floor_id": "F1"}},
	}
	if !reflect.DeepEqual(ws1.calls, wantCalls) {
		t.Errorf("calls = %+v, want %+v", ws1.calls, wantCalls)
	}
	afterFirst := readStash(t, stashDir)
	if ops, _ := afterFirst["ops"].([]any); len(ops) != 0 {
		t.Errorf("stash after first attempt = %+v, want empty", ops)
	}
	if !reflect.DeepEqual(managed, map[string]string{"floor:ground": "F1", "area:old": "A-NEW"}) {
		t.Errorf("managed = %+v", managed)
	}

	// Second attempt: nothing left in the stash, so nothing is retried.
	ws2 := newFakeWS()
	result2 := RollbackRegistry(context.Background(), staticDialer(ws2), stashDir, managed, nil, nil)
	if !result2.OK {
		t.Fatalf("result2 = %+v", result2)
	}
	if len(ws2.calls) != 0 {
		t.Errorf("ws2.calls = %+v, want none", ws2.calls)
	}
}

func TestRollbackRegistryStashWriteFailureLeavesEntryRetryableAndSkipsItsInvert(t *testing.T) {
	// The one retryable failure mode: a stash write failure skips the
	// inverse entirely, so the entry stays outstanding for a clean retry.
	// Inverting first would leave an entry a retry inverts a second time.
	stashDir := t.TempDir()
	if err := writeRegistryStashReal(stashDir, []stashEntry{
		{Kind: registries.KindDelete, RType: "area", Key: "old", LiveID: "A-OLD", PriorObject: realisticArea},
	}); err != nil {
		t.Fatal(err)
	}
	managed := map[string]string{}

	orig := writeRegistryStash
	writeRegistryStash = func(string, []stashEntry) error {
		return errors.New("[Errno 28] No space left on device")
	}
	ws1 := newFakeWS()
	result1 := RollbackRegistry(context.Background(), staticDialer(ws1), stashDir, managed, nil, nil)
	writeRegistryStash = orig

	if result1.OK {
		t.Fatalf("result1 = %+v", result1)
	}
	if len(ws1.calls) != 0 {
		t.Errorf("ws1.calls = %+v, want none - the invert was never attempted", ws1.calls)
	}
	afterFirst := readStash(t, stashDir)
	if ops, _ := afterFirst["ops"].([]any); len(ops) != 1 {
		t.Errorf("stash after first attempt = %+v, want the entry untouched (write never landed)", ops)
	}

	// Healthy filesystem: inverts once, stripped of the id field and
	// created_at/modified_at.
	ws2 := newFakeWS()
	ws2.results["config/area_registry/create"] = []any{map[string]any{"area_id": "A-NEW", "name": "Old room"}}
	result2 := RollbackRegistry(context.Background(), staticDialer(ws2), stashDir, managed, nil, nil)

	if !result2.OK {
		t.Fatalf("result2 = %+v", result2)
	}
	wantCall := map[string]any{
		"aliases": []any{}, "floor_id": nil, "humidity_entity_id": nil, "icon": "mdi:sofa",
		"labels": []any{}, "name": "Old room", "picture": nil, "temperature_entity_id": nil,
	}
	if len(ws2.calls) != 1 || !reflect.DeepEqual(ws2.calls[0].params, wantCall) {
		t.Errorf("ws2.calls = %+v, want one create with %+v", ws2.calls, wantCall)
	}
}

func TestRollbackRegistrySkipsCreateEntryWithNoLiveIDNeverFallsBackToManaged(t *testing.T) {
	stashDir := t.TempDir()
	// Written directly (not via writeRegistryStash, which never omits
	// live_id) to simulate a corrupt/hand-edited stash entry.
	raw := `{"ops": [{"kind": "create", "rtype": "floor", "key": "ground", "live_id": null, "live_object": null}]}`
	if err := os.WriteFile(filepath.Join(stashDir, "registry_stash.json"), []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	managed := map[string]string{"floor:ground": "F-USER-RECREATED"}

	ws := newFakeWS()
	result := RollbackRegistry(context.Background(), staticDialer(ws), stashDir, managed, nil, nil)

	if !result.OK {
		t.Fatalf("result = %+v", result)
	}
	if len(ws.calls) != 0 {
		t.Errorf("calls = %+v, want none", ws.calls)
	}
	if !reflect.DeepEqual(managed, map[string]string{"floor:ground": "F-USER-RECREATED"}) {
		t.Errorf("managed = %+v", managed)
	}
}

func TestRollbackRegistryNeverPanicsOnUnexpectedInternalFailure(t *testing.T) {
	stashDir := t.TempDir()
	if err := writeRegistryStash(stashDir, []stashEntry{{Kind: registries.KindCreate, RType: "floor", Key: "ground", LiveID: "F1"}}); err != nil {
		t.Fatal(err)
	}

	// A panicking Cmd stands in for any internal failure: it must be
	// recovered into a failing result, not crash the caller.
	ws := panicWS{}
	result := RollbackRegistry(context.Background(), staticDialer(ws), stashDir, map[string]string{}, nil, nil)

	if result.OK {
		t.Fatalf("result = %+v", result)
	}
	if !strings.Contains(result.Error, "unexpected bug") {
		t.Errorf("error = %q", result.Error)
	}
}

type panicWS struct{}

func (panicWS) Cmd(context.Context, string, map[string]any) (any, error) { panic("unexpected bug") }
func (panicWS) CmdTimeout(context.Context, string, map[string]any, time.Duration) (any, error) {
	panic("unexpected bug")
}
func (panicWS) Close() {}

// --- VM e2e: real-hardware update/delete-inverse schema rejection ------

func TestUpdateInverseRestoresOnlyForwardTouchedFieldsNotTheFullObject(t *testing.T) {
	// Resending the full stashed object fails on real HA: the inverse must
	// restore only the fields the forward update touched.
	plan := []registries.RegOp{
		regOp(registries.KindUpdate, "area", "c", map[string]any{"icon": "mdi:new"}, "A-C"),
		regOp(registries.KindCreate, "floor", "a", map[string]any{"name": "A"}, ""), // fails, triggers inverse-replay
	}
	priorArea := map[string]any{}
	for k, v := range realisticArea {
		priorArea[k] = v
	}
	priorArea["area_id"] = "A-C"
	priorArea["icon"] = "mdi:old"
	priorArea["labels"] = []any{"gitops"}
	priorArea["floor_id"] = "F-OTHER"

	managed := map[string]string{"area:c": "A-C"}
	ws := newFakeWS()
	ws.results["config/area_registry/list"] = []any{[]any{priorArea}}
	ws.raiseOn["config/floor_registry/create"] = []error{&wsclient.Error{Code: "unknown_error", Message: "boom"}}

	result := ApplyPlan(context.Background(), staticDialer(ws), plan, managed, t.TempDir())

	if result.OK || !result.RolledBack {
		t.Fatalf("result = %+v", result)
	}

	updateCalls := ws.callsFor("config/area_registry/update")
	if len(updateCalls) != 2 {
		t.Fatalf("update calls = %d, want 2", len(updateCalls))
	}
	forward, inverse := updateCalls[0], updateCalls[1]
	if !reflect.DeepEqual(forward.params, map[string]any{"area_id": "A-C", "icon": "mdi:new"}) {
		t.Errorf("forward params = %+v", forward.params)
	}
	if !reflect.DeepEqual(inverse.params, map[string]any{"area_id": "A-C", "icon": "mdi:old"}) {
		t.Errorf("inverse params = %+v", inverse.params)
	}
}

func TestUpdateInverseSendsNilForFieldAbsentFromStashedObject(t *testing.T) {
	plan := []registries.RegOp{
		regOp(registries.KindUpdate, "area", "c", map[string]any{"icon": "mdi:new"}, "A-C"),
		regOp(registries.KindCreate, "floor", "a", map[string]any{"name": "A"}, ""),
	}
	sparsePriorArea := map[string]any{"area_id": "A-C", "name": "C"} // no "icon" key at all
	managed := map[string]string{"area:c": "A-C"}
	ws := newFakeWS()
	ws.results["config/area_registry/list"] = []any{[]any{sparsePriorArea}}
	ws.raiseOn["config/floor_registry/create"] = []error{&wsclient.Error{Code: "unknown_error", Message: "boom"}}

	result := ApplyPlan(context.Background(), staticDialer(ws), plan, managed, t.TempDir())

	if result.OK || !result.RolledBack {
		t.Fatalf("result = %+v", result)
	}
	updateCalls := ws.callsFor("config/area_registry/update")
	inverse := updateCalls[len(updateCalls)-1]
	if !reflect.DeepEqual(inverse.params, map[string]any{"area_id": "A-C", "icon": nil}) {
		t.Errorf("inverse params = %+v", inverse.params)
	}
}

func TestDeleteInverseStripsServerGeneratedFieldsForEveryRegistryRType(t *testing.T) {
	cases := []struct {
		rtype    string
		obj      map[string]any
		idField  string
		wantCall map[string]any
	}{
		// The stash is read fresh from JSON, so "level" decodes as float64
		// even though it started as a Go int; the bytes sent are the same.
		{"floor", realisticFloor, "floor_id", map[string]any{"aliases": []any{}, "icon": "mdi:home", "level": float64(0), "name": "Old floor"}},
		{"label", realisticLabel, "label_id", map[string]any{"color": "indigo", "description": nil, "icon": "mdi:source-branch", "name": "Old label"}},
		{"area", realisticArea, "area_id", map[string]any{
			"aliases": []any{}, "floor_id": nil, "humidity_entity_id": nil, "icon": "mdi:sofa",
			"labels": []any{}, "name": "Old room", "picture": nil, "temperature_entity_id": nil,
		}},
	}
	for _, tc := range cases {
		t.Run(tc.rtype, func(t *testing.T) {
			stashDir := t.TempDir()
			liveID, _ := tc.obj[tc.idField].(string)
			if err := writeRegistryStash(stashDir, []stashEntry{
				{Kind: registries.KindDelete, RType: tc.rtype, Key: "old", LiveID: liveID, PriorObject: tc.obj},
			}); err != nil {
				t.Fatal(err)
			}
			ws := newFakeWS()
			ws.results["config/"+tc.rtype+"_registry/create"] = []any{map[string]any{tc.idField: "NEW-ID"}}

			result := RollbackRegistry(context.Background(), staticDialer(ws), stashDir, map[string]string{}, nil, nil)

			if !result.OK {
				t.Fatalf("result = %+v", result)
			}
			calls := ws.callsFor("config/" + tc.rtype + "_registry/create")
			if len(calls) != 1 || !reflect.DeepEqual(calls[0].params, tc.wantCall) {
				t.Errorf("create params = %+v, want %+v", calls, tc.wantCall)
			}
		})
	}
}

// --- Go-specific: dialer redial adaptation -------------------------------

func TestInverseReplayRedialsAfterTimeoutOpFailure(t *testing.T) {
	// A "timeout" wsclient error means the connection is dead (per
	// coder/websocket), so the inverse-replay must run on a redial.
	stashDir := t.TempDir()
	firstWS := newFakeWS()
	firstWS.results["config/floor_registry/create"] = []any{map[string]any{"floor_id": "F1", "name": "Ground floor"}}
	firstWS.results["config/area_registry/list"] = []any{[]any{map[string]any{"area_id": "A-OLD", "name": "Old room"}}}
	firstWS.raiseOn["config/area_registry/delete"] = []error{&wsclient.Error{Code: "timeout", Message: "no response for id"}}

	secondWS := newFakeWS()
	secondWS.results["config/floor_registry/delete"] = []any{nil}

	dialCount := 0
	dialer := func(context.Context) (WSClient, error) {
		dialCount++
		if dialCount == 1 {
			return firstWS, nil
		}
		return secondWS, nil
	}

	plan := []registries.RegOp{
		regOp(registries.KindCreate, "floor", "ground", map[string]any{"name": "Ground floor"}, ""),
		regOp(registries.KindDelete, "area", "old", nil, "A-OLD"),
	}
	managed := map[string]string{}

	result := ApplyPlan(context.Background(), dialer, plan, managed, stashDir)

	if result.OK || !result.RolledBack {
		t.Fatalf("result = %+v", result)
	}
	if dialCount != 2 {
		t.Errorf("dialCount = %d, want 2 (initial dial + redial for the inverse-replay)", dialCount)
	}
	if len(firstWS.callsFor("config/floor_registry/delete")) != 0 {
		t.Errorf("the dead (first) connection must not have been reused for the invert")
	}
	if len(secondWS.callsFor("config/floor_registry/delete")) != 1 {
		t.Errorf("expected the invert (floor delete) on the redialed connection, got %+v", secondWS.calls)
	}
	if !secondWS.closed {
		t.Errorf("the redialed connection should be closed once the replay finishes")
	}
}

func TestInverseReplayRedialFailureLeavesStashOutstanding(t *testing.T) {
	// The op fails with "timeout", so the replay needs a connection at
	// once; when the redial fails it must stop and leave the stash intact.
	stashDir := t.TempDir()
	ws := newFakeWS()
	ws.results["config/floor_registry/create"] = []any{map[string]any{"floor_id": "F-A", "name": "A"}}
	ws.results["config/label_registry/create"] = []any{map[string]any{"label_id": "L-B", "name": "B"}}
	ws.results["config/area_registry/list"] = []any{[]any{map[string]any{"area_id": "A-C", "name": "C"}}}
	ws.raiseOn["config/area_registry/update"] = []error{&wsclient.Error{Code: "timeout", Message: "no response"}}

	dialCount := 0
	dialer := func(context.Context) (WSClient, error) {
		dialCount++
		if dialCount == 1 {
			return ws, nil
		}
		return nil, errors.New("network unreachable")
	}

	plan := []registries.RegOp{
		regOp(registries.KindCreate, "floor", "a", map[string]any{"name": "A"}, ""),
		regOp(registries.KindCreate, "label", "b", map[string]any{"name": "B"}, ""),
		regOp(registries.KindUpdate, "area", "c", map[string]any{"name": "C"}, "A-C"),
	}
	managed := map[string]string{"area:c": "A-C"}

	result := ApplyPlan(context.Background(), dialer, plan, managed, stashDir)

	if result.OK || result.RolledBack {
		t.Fatalf("result = %+v, want ok=false rolled_back=false", result)
	}
	if !strings.Contains(result.Error, "redial") {
		t.Errorf("error = %q, want it to mention the failed redial", result.Error)
	}
	if dialCount != 2 {
		t.Errorf("dialCount = %d, want 2 (initial dial + one failed redial attempt)", dialCount)
	}

	// Nothing was inverted: the label's stash-drop landed just before the
	// redial failed, and the floor was never reached. Both still exist and
	// stay in managed, so reconciliation still cleans them up.
	want := map[string]string{"area:c": "A-C", "label:b": "L-B", "floor:a": "F-A"}
	if !reflect.DeepEqual(managed, want) {
		t.Errorf("managed = %+v, want %+v", managed, want)
	}

	stash := readStash(t, stashDir)
	kinds := stashOpKinds(t, stash)
	if !reflect.DeepEqual(kinds, []string{"create"}) {
		t.Errorf("stash kinds = %+v, want [create] (just the floor, never reached by the replay)", kinds)
	}
}

// A stash write failing after an op succeeded must not inverse-replay:
// the op is real, only the journal is degraded, so say the ops applied.
func TestApplyPlanStashWriteFailureAfterSuccessfulOpKeepsItAppliedAndDoesNotInvert(t *testing.T) {
	stashDir := t.TempDir()
	ws := newFakeWS()
	ws.results["config/floor_registry/create"] = []any{map[string]any{"floor_id": "F1", "name": "Ground"}}
	dialer := func(context.Context) (WSClient, error) { return ws, nil }
	plan := []registries.RegOp{
		{Kind: registries.KindCreate, RType: "floor", Key: "ground", Params: map[string]any{"name": "Ground"}},
		{Kind: registries.KindCreate, RType: "label", Key: "gitops", Params: map[string]any{"name": "GitOps"}},
	}
	managed := map[string]string{}

	realWrite := writeRegistryStash
	writes := 0
	writeRegistryStash = func(dir string, entries []stashEntry) error {
		writes++
		if writes >= 2 { // the initial reset succeeds; the post-op-1 rewrite fails
			return errors.New("no space left on device")
		}
		return realWrite(dir, entries)
	}
	result := ApplyPlan(context.Background(), dialer, plan, managed, stashDir)
	writeRegistryStash = realWrite

	if result.OK {
		t.Errorf("result = %+v, want ok=false", result)
	}
	if want := []string{"create floor:ground"}; !reflect.DeepEqual(result.Applied, want) {
		t.Errorf("applied = %+v, want %+v (the op really did execute)", result.Applied, want)
	}
	if !strings.Contains(result.Error, "1 op(s) applied successfully") {
		t.Errorf("error = %q, want it to say the op applied", result.Error)
	}
	if result.RolledBack {
		t.Error("rolled_back = true, want false: nothing was inverted")
	}
	if managed["floor:ground"] != "F1" {
		t.Errorf("managed = %+v, want the executed create still recorded", managed)
	}
	// No inverse may have been attempted, and the second op must not run.
	for _, c := range ws.callTypes() {
		if c == "config/floor_registry/delete" {
			t.Error("an inverse-replay ran; a journal write failure must never undo a successful op")
		}
		if c == "config/label_registry/create" {
			t.Error("execution continued past the journal failure")
		}
	}
}

// Every Apply*Plan carries this guard; the registry layer's stash write
// would otherwise be the first thing to touch an empty stashDir.
func TestApplyPlanWithOnlyErrorOpsDoesNoIOAndKeepsThemPending(t *testing.T) {
	ops := []registries.RegOp{{Kind: registries.KindError, RType: "area", Key: "office", Error: "ambiguous adopt"}}

	result := ApplyPlan(context.Background(), nil, ops, nil, "")

	if !result.OK {
		t.Fatalf("result = %+v, want OK for a plan with nothing to execute", result)
	}
	if len(result.SkippedErrors) != 1 {
		t.Errorf("skipped_errors = %+v, want the error op passed through", result.SkippedErrors)
	}
}

// Rolling back an adopt must release ownership: the no-drift update that
// records a pre-existing object into managed IS the adoption, and leaving
// the key after its inverse would let a later manifest removal delete a
// user-made object.
func TestRollbackRegistryReleasesAnAdoptedObject(t *testing.T) {
	stashDir := t.TempDir()
	plan := []registries.RegOp{regOp(registries.KindUpdate, "floor", "old", map[string]any{"name": "Old floor"}, "F-OLD")}
	managed := map[string]string{}
	applyWS := newFakeWS()
	applyWS.results["config/floor_registry/list"] = []any{[]any{realisticFloor}}

	applyResult := ApplyPlan(context.Background(), staticDialer(applyWS), plan, managed, stashDir)
	if !applyResult.OK {
		t.Fatalf("apply result = %+v", applyResult)
	}
	if managed["floor:old"] != "F-OLD" {
		t.Fatalf("managed after adopt = %+v", managed)
	}

	rollbackWS := newFakeWS()
	result := RollbackRegistry(context.Background(), staticDialer(rollbackWS), stashDir, managed, nil, nil)
	if !result.OK {
		t.Fatalf("rollback result = %+v", result)
	}
	if _, still := managed["floor:old"]; still {
		t.Errorf("managed = %+v, want the adopted key released", managed)
	}
}

// The counterpart: an update of an ALREADY-managed object keeps its key on
// rollback - ownership predates the op, so its inverse must not touch it.
func TestRollbackRegistryKeepsAPreManagedKey(t *testing.T) {
	stashDir := t.TempDir()
	plan := []registries.RegOp{regOp(registries.KindUpdate, "floor", "old", map[string]any{"name": "New name"}, "F-OLD")}
	managed := map[string]string{"floor:old": "F-OLD"}
	applyWS := newFakeWS()
	applyWS.results["config/floor_registry/list"] = []any{[]any{realisticFloor}}

	if applyResult := ApplyPlan(context.Background(), staticDialer(applyWS), plan, managed, stashDir); !applyResult.OK {
		t.Fatalf("apply result = %+v", applyResult)
	}

	rollbackWS := newFakeWS()
	if result := RollbackRegistry(context.Background(), staticDialer(rollbackWS), stashDir, managed, nil, nil); !result.OK {
		t.Fatalf("rollback result = %+v", result)
	}
	if managed["floor:old"] != "F-OLD" {
		t.Errorf("managed = %+v, want the pre-managed key kept", managed)
	}
}

// An update whose live object vanished between plan and apply must refuse
// rather than stash a nil prior whose inverse can only send nulls.
func TestApplyPlanRefusesAnUpdateOfAVanishedObject(t *testing.T) {
	stashDir := t.TempDir()
	plan := []registries.RegOp{regOp(registries.KindUpdate, "floor", "old", map[string]any{"name": "New name"}, "F-GONE")}
	managed := map[string]string{"floor:old": "F-GONE"}
	applyWS := newFakeWS()

	result := ApplyPlan(context.Background(), staticDialer(applyWS), plan, managed, stashDir)

	if result.OK {
		t.Fatalf("result = %+v, want a refusal", result)
	}
	if !strings.Contains(result.Error, "no longer exists") {
		t.Errorf("error = %q, want it to say the object is gone", result.Error)
	}
	for _, call := range applyWS.calls {
		if call.msgType == "config/floor_registry/update" {
			t.Errorf("update was sent despite the missing prior: %+v", call)
		}
	}
}
