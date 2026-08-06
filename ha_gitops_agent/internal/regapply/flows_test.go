package regapply

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/flows"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/registries"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/secretref"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/secretref/secrettest"
)

// --- fakeIntegrationHTTP: records every Do() call and answers each
// "METHOD path" from a FIFO whose last entry repeats; unqueued keys get
// a bare 200 {}.

type integrationHTTPCall struct {
	method string
	path   string
	body   map[string]any
}

type integrationHTTPResp struct {
	status int
	data   any
}

type fakeIntegrationHTTP struct {
	mu    sync.Mutex
	calls []integrationHTTPCall
	queue map[string][]integrationHTTPResp
}

func newFakeIntegrationHTTP() *fakeIntegrationHTTP {
	return &fakeIntegrationHTTP{queue: map[string][]integrationHTTPResp{}}
}

func (f *fakeIntegrationHTTP) queueResponse(method, path string, status int, data any) {
	key := method + " " + path
	f.queue[key] = append(f.queue[key], integrationHTTPResp{status: status, data: data})
}

func (f *fakeIntegrationHTTP) Do(req *http.Request) (*http.Response, error) {
	var body map[string]any
	if req.Body != nil {
		data, _ := io.ReadAll(req.Body)
		if len(data) > 0 {
			_ = json.Unmarshal(data, &body)
		}
	}
	key := req.Method + " " + req.URL.Path

	f.mu.Lock()
	f.calls = append(f.calls, integrationHTTPCall{method: req.Method, path: req.URL.Path, body: body})
	resp := integrationHTTPResp{status: 200, data: map[string]any{}}
	if q := f.queue[key]; len(q) > 0 {
		resp = q[0]
		if len(q) > 1 {
			f.queue[key] = q[1:]
		}
	}
	f.mu.Unlock()

	var payload []byte
	if resp.status >= 400 {
		payload, _ = json.Marshal(map[string]any{"message": "boom"})
	} else {
		payload, _ = json.Marshal(resp.data)
	}
	return &http.Response{
		StatusCode: resp.status,
		Body:       io.NopCloser(bytes.NewReader(payload)),
		Header:     make(http.Header),
	}, nil
}

func (f *fakeIntegrationHTTP) callsFor(method, path string) []integrationHTTPCall {
	var out []integrationHTTPCall
	for _, c := range f.calls {
		if c.method == method && c.path == path {
			out = append(out, c)
		}
	}
	return out
}

// integrationOp builds an op as flows.Plan emits it, Declared included:
// the manifest's own data beside the resolved copy in Params, the same map
// when no secret reference is involved.
func integrationOp(kind, key string, params map[string]any, liveID string) registries.RegOp {
	if params == nil {
		params = map[string]any{}
	}
	declared, _ := params["data"].(map[string]any)
	return registries.RegOp{
		Kind: kind, RType: "integration", Key: key,
		Params: params, Declared: declared, LiveID: liveID, DiffText: "...",
	}
}

func readIntegrationStashFile(t *testing.T, stashDir string) integrationStashFileOnDisk {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(stashDir, "integration_stash.json"))
	if err != nil {
		t.Fatal(err)
	}
	var decoded integrationStashFileOnDisk
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	return decoded
}

// --- FetchIntegrationEntries --------------------------------------------

func TestFetchIntegrationEntriesReturnsBareArray(t *testing.T) {
	setSupervisorToken(t)
	client := newFakeIntegrationHTTP()
	client.queueResponse("GET", "/core/api/config/config_entries/entry", 200, []map[string]any{
		{"entry_id": "abc123", "domain": "workday", "title": "Workday"},
	})

	got, err := FetchIntegrationEntries(context.Background(), client)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(got) != 1 || got[0]["entry_id"] != "abc123" {
		t.Errorf("got = %+v", got)
	}
}

func TestFetchIntegrationEntriesMissingTokenErrors(t *testing.T) {
	t.Setenv("SUPERVISOR_TOKEN", "")
	_, err := FetchIntegrationEntries(context.Background(), newFakeIntegrationHTTP())
	if err == nil {
		t.Fatal("err = nil, want an error")
	}
}

func TestFetchIntegrationEntriesHTTPErrorPropagates(t *testing.T) {
	setSupervisorToken(t)
	client := newFakeIntegrationHTTP()
	client.queueResponse("GET", "/core/api/config/config_entries/entry", 500, nil)

	_, err := FetchIntegrationEntries(context.Background(), client)
	if err == nil {
		t.Fatal("err = nil, want an error")
	}
}

// --- driveFlow: happy paths ----------------------------------------------

func TestDriveFlowSingleStepHappyPath(t *testing.T) {
	setSupervisorToken(t)
	client := newFakeIntegrationHTTP()
	client.queueResponse("POST", "/core/api/config/config_entries/flow", 200, map[string]any{
		"flow_id": "flow1", "handler": "moon", "type": "form", "step_id": "user", "errors": nil,
	})
	client.queueResponse("POST", "/core/api/config/config_entries/flow/flow1", 200, map[string]any{
		"flow_id": "flow1", "handler": "moon", "type": "create_entry", "title": "Moon",
		"result": map[string]any{"entry_id": "entryABC"},
	})

	entryID, _, err := driveFlow(context.Background(), client, "test-token", "moon", map[string]any{"user": map[string]any{}})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if entryID != "entryABC" {
		t.Errorf("entry_id = %q, want entryABC", entryID)
	}
	if len(client.callsFor("DELETE", "/core/api/config/config_entries/flow/flow1")) != 0 {
		t.Errorf("flow was aborted on a successful completion")
	}
}

func TestDriveFlowMultiStepHappyPath(t *testing.T) {
	setSupervisorToken(t)
	client := newFakeIntegrationHTTP()
	client.queueResponse("POST", "/core/api/config/config_entries/flow", 200, map[string]any{
		"flow_id": "flow1", "handler": "workday", "type": "form", "step_id": "user",
	})
	client.queueResponse("POST", "/core/api/config/config_entries/flow/flow1", 200, map[string]any{
		"flow_id": "flow1", "handler": "workday", "type": "form", "step_id": "workdays",
	})
	client.queueResponse("POST", "/core/api/config/config_entries/flow/flow1", 200, map[string]any{
		"flow_id": "flow1", "handler": "workday", "type": "create_entry", "title": "Workday",
		"result": map[string]any{"entry_id": "entryXYZ"},
	})

	data := map[string]any{
		"user":     map[string]any{"name": "Workday", "country": "PL"},
		"workdays": map[string]any{"days": []any{"mon", "tue"}},
	}
	entryID, _, err := driveFlow(context.Background(), client, "test-token", "workday", data)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if entryID != "entryXYZ" {
		t.Errorf("entry_id = %q, want entryXYZ", entryID)
	}

	// The second POST must have carried the "workdays" step's data.
	calls := client.callsFor("POST", "/core/api/config/config_entries/flow/flow1")
	if len(calls) != 2 {
		t.Fatalf("advance calls = %d, want 2", len(calls))
	}
	if calls[0].body["name"] != "Workday" {
		t.Errorf("first advance body = %+v, want the 'user' step data", calls[0].body)
	}
	if !reflect_DeepEqualAny(calls[1].body["days"], []any{"mon", "tue"}) {
		t.Errorf("second advance body = %+v, want the 'workdays' step data", calls[1].body)
	}
}

func reflect_DeepEqualAny(a, b any) bool {
	aj, _ := json.Marshal(a)
	bj, _ := json.Marshal(b)
	return string(aj) == string(bj)
}

func TestDriveFlowNoInputFlowNeverAdvances(t *testing.T) {
	setSupervisorToken(t)
	client := newFakeIntegrationHTTP()
	client.queueResponse("POST", "/core/api/config/config_entries/flow", 200, map[string]any{
		"flow_id": "flow1", "handler": "moon", "type": "create_entry", "title": "Moon",
		"result": map[string]any{"entry_id": "entryZero"},
	})

	entryID, _, err := driveFlow(context.Background(), client, "test-token", "moon", nil)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if entryID != "entryZero" {
		t.Errorf("entry_id = %q", entryID)
	}
	if len(client.callsFor("POST", "/core/api/config/config_entries/flow/flow1")) != 0 {
		t.Errorf("advanced a flow that completed on start")
	}
}

// --- driveFlow: every failure path must abort the flow (never leave a
// half-open flow behind) ---------------------------------------------

func TestDriveFlowMissingStepDataAbortsFlow(t *testing.T) {
	setSupervisorToken(t)
	client := newFakeIntegrationHTTP()
	client.queueResponse("POST", "/core/api/config/config_entries/flow", 200, map[string]any{
		"flow_id": "flow1", "handler": "workday", "type": "form", "step_id": "user",
		"data_schema": []any{map[string]any{"name": "country", "required": true}},
	})
	client.queueResponse("DELETE", "/core/api/config/config_entries/flow/flow1", 200, map[string]any{"message": "Flow aborted"})

	_, _, err := driveFlow(context.Background(), client, "test-token", "workday", map[string]any{})
	if err == nil {
		t.Fatal("err = nil, want an error (no declared data for step 'user')")
	}
	if !strings.Contains(err.Error(), "user") {
		t.Errorf("err = %v, want it to name the step", err)
	}
	if len(client.callsFor("DELETE", "/core/api/config/config_entries/flow/flow1")) != 1 {
		t.Errorf("flow was not aborted: calls = %+v", client.calls)
	}
}

func TestDriveFlowFormErrorsAbortFlow(t *testing.T) {
	setSupervisorToken(t)
	client := newFakeIntegrationHTTP()
	client.queueResponse("POST", "/core/api/config/config_entries/flow", 200, map[string]any{
		"flow_id": "flow1", "handler": "workday", "type": "form", "step_id": "user",
		"errors": map[string]any{"country": "invalid_country"},
	})
	client.queueResponse("DELETE", "/core/api/config/config_entries/flow/flow1", 200, nil)

	_, _, err := driveFlow(context.Background(), client, "test-token", "workday", map[string]any{"user": map[string]any{}})
	if err == nil {
		t.Fatal("err = nil, want an error")
	}
	if !strings.Contains(err.Error(), "invalid_country") {
		t.Errorf("err = %v, want it to quote the field error", err)
	}
	if len(client.callsFor("DELETE", "/core/api/config/config_entries/flow/flow1")) != 1 {
		t.Errorf("flow was not aborted")
	}
}

func TestDriveFlowMenuStepAbortsFlow(t *testing.T) {
	setSupervisorToken(t)
	client := newFakeIntegrationHTTP()
	client.queueResponse("POST", "/core/api/config/config_entries/flow", 200, map[string]any{
		"flow_id": "flow1", "handler": "somedomain", "type": "menu", "step_id": "choose",
		"menu_options": []any{"a", "b"},
	})
	client.queueResponse("DELETE", "/core/api/config/config_entries/flow/flow1", 200, nil)

	_, _, err := driveFlow(context.Background(), client, "test-token", "somedomain", nil)
	if err == nil {
		t.Fatal("err = nil, want an error")
	}
	if !strings.Contains(err.Error(), "menu") {
		t.Errorf("err = %v, want it to name the unsupported type", err)
	}
	if len(client.callsFor("DELETE", "/core/api/config/config_entries/flow/flow1")) != 1 {
		t.Errorf("flow was not aborted")
	}
}

func TestDriveFlowExternalStepAbortsFlow(t *testing.T) {
	setSupervisorToken(t)
	client := newFakeIntegrationHTTP()
	client.queueResponse("POST", "/core/api/config/config_entries/flow", 200, map[string]any{
		"flow_id": "flow1", "handler": "oauthy", "type": "external", "step_id": "auth", "url": "https://example.com",
	})
	client.queueResponse("DELETE", "/core/api/config/config_entries/flow/flow1", 200, nil)

	_, _, err := driveFlow(context.Background(), client, "test-token", "oauthy", nil)
	if err == nil {
		t.Fatal("err = nil, want an error")
	}
	if len(client.callsFor("DELETE", "/core/api/config/config_entries/flow/flow1")) != 1 {
		t.Errorf("flow was not aborted")
	}
}

func TestDriveFlowShowProgressStepAbortsFlow(t *testing.T) {
	setSupervisorToken(t)
	client := newFakeIntegrationHTTP()
	client.queueResponse("POST", "/core/api/config/config_entries/flow", 200, map[string]any{
		"flow_id": "flow1", "handler": "slow", "type": "progress", "step_id": "wait", "progress_action": "downloading",
	})
	client.queueResponse("DELETE", "/core/api/config/config_entries/flow/flow1", 200, nil)

	_, _, err := driveFlow(context.Background(), client, "test-token", "slow", nil)
	if err == nil {
		t.Fatal("err = nil, want an error")
	}
	if len(client.callsFor("DELETE", "/core/api/config/config_entries/flow/flow1")) != 1 {
		t.Errorf("flow was not aborted")
	}
}

func TestDriveFlowExplicitAbortNeverCallsDelete(t *testing.T) {
	setSupervisorToken(t)
	client := newFakeIntegrationHTTP()
	client.queueResponse("POST", "/core/api/config/config_entries/flow", 200, map[string]any{
		"flow_id": "flow1", "handler": "moon", "type": "abort", "reason": "already_configured",
	})

	_, _, err := driveFlow(context.Background(), client, "test-token", "moon", nil)
	if err == nil {
		t.Fatal("err = nil, want an error")
	}
	if !strings.Contains(err.Error(), "already_configured") {
		t.Errorf("err = %v, want it to quote the abort reason", err)
	}
	// The flow already ended itself; DELETE is neither required nor
	// expected here.
	if len(client.callsFor("DELETE", "/core/api/config/config_entries/flow/flow1")) != 0 {
		t.Errorf("unnecessary abort call for an already-aborted flow")
	}
}

func TestDriveFlowMaxStepsBoundAbortsFlow(t *testing.T) {
	setSupervisorToken(t)
	client := newFakeIntegrationHTTP()
	client.queueResponse("POST", "/core/api/config/config_entries/flow", 200, map[string]any{
		"flow_id": "flow1", "handler": "loopy", "type": "form", "step_id": "step",
	})
	// Every advance re-asks for the same step, forever.
	client.queueResponse("POST", "/core/api/config/config_entries/flow/flow1", 200, map[string]any{
		"flow_id": "flow1", "handler": "loopy", "type": "form", "step_id": "step",
	})
	client.queueResponse("DELETE", "/core/api/config/config_entries/flow/flow1", 200, nil)

	data := map[string]any{"step": map[string]any{}}
	_, _, err := driveFlow(context.Background(), client, "test-token", "loopy", data)
	if err == nil {
		t.Fatal("err = nil, want an error (exceeded max steps)")
	}
	if !strings.Contains(err.Error(), "exceeded") {
		t.Errorf("err = %v, want it to mention exceeding the step bound", err)
	}
	if len(client.callsFor("DELETE", "/core/api/config/config_entries/flow/flow1")) != 1 {
		t.Errorf("flow was not aborted")
	}
	if len(client.callsFor("POST", "/core/api/config/config_entries/flow/flow1")) > maxFlowSteps {
		t.Errorf("advanced more than maxFlowSteps(%d) times", maxFlowSteps)
	}
}

func TestDriveFlowAdvanceHTTPErrorAbortsFlow(t *testing.T) {
	setSupervisorToken(t)
	client := newFakeIntegrationHTTP()
	client.queueResponse("POST", "/core/api/config/config_entries/flow", 200, map[string]any{
		"flow_id": "flow1", "handler": "workday", "type": "form", "step_id": "user",
	})
	client.queueResponse("POST", "/core/api/config/config_entries/flow/flow1", 500, nil)
	client.queueResponse("DELETE", "/core/api/config/config_entries/flow/flow1", 200, nil)

	_, _, err := driveFlow(context.Background(), client, "test-token", "workday", map[string]any{"user": map[string]any{}})
	if err == nil {
		t.Fatal("err = nil, want an error")
	}
	if len(client.callsFor("DELETE", "/core/api/config/config_entries/flow/flow1")) != 1 {
		t.Errorf("flow was not aborted")
	}
}

func TestDriveFlowCreateEntryWithNoEntryIDIsAnError(t *testing.T) {
	setSupervisorToken(t)
	client := newFakeIntegrationHTTP()
	client.queueResponse("POST", "/core/api/config/config_entries/flow", 200, map[string]any{
		"flow_id": "flow1", "handler": "moon", "type": "create_entry", "title": "Moon", "result": map[string]any{},
	})

	_, _, err := driveFlow(context.Background(), client, "test-token", "moon", nil)
	if err == nil {
		t.Fatal("err = nil, want an error")
	}
}

func TestAbortFlowTolerates404(t *testing.T) {
	setSupervisorToken(t)
	client := newFakeIntegrationHTTP()
	client.queueResponse("DELETE", "/core/api/config/config_entries/flow/gone", 404, nil)

	if err := abortFlow(context.Background(), client, "test-token", "gone"); err != nil {
		t.Errorf("err = %v, want nil (404 means already gone)", err)
	}
}

// --- ApplyFlowPlan: create ------------------------------------------------

func TestApplyFlowPlanCreateSuccess(t *testing.T) {
	setSupervisorToken(t)
	client := newFakeIntegrationHTTP()
	client.queueResponse("GET", "/core/api/config/config_entries/entry", 200, []map[string]any{})
	client.queueResponse("POST", "/core/api/config/config_entries/flow", 200, map[string]any{
		"flow_id": "flow1", "handler": "moon", "type": "create_entry", "title": "Moon",
		"result": map[string]any{"entry_id": "entryABC"},
	})

	op := integrationOp(flows.KindCreate, "moon_home", map[string]any{"domain": "moon", "title": "Moon", "data": map[string]any{}}, "")
	stashDir := t.TempDir()
	managed := map[string]string{}
	hashes := map[string]string{}
	dataSnaps := map[string]map[string]any{}

	result := ApplyFlowPlan(context.Background(), client, nil, []registries.RegOp{op}, managed, hashes, dataSnaps, map[string]map[string]any{}, stashDir)
	if !result.OK {
		t.Fatalf("result = %+v, want OK", result)
	}
	if managed["integration:moon_home"] != "entryABC" {
		t.Errorf("managed = %+v", managed)
	}
	if hashes["integration:moon_home"] == "" {
		t.Errorf("hashes not recorded: %+v", hashes)
	}
	if _, ok := dataSnaps["integration:moon_home"]; !ok {
		t.Errorf("data snapshot not recorded: %+v", dataSnaps)
	}

	stash := readIntegrationStashFile(t, stashDir)
	if len(stash.Ops) != 1 || stash.Ops[0].EntryID != "entryABC" || stash.Ops[0].Kind != flows.KindCreate {
		t.Errorf("stash = %+v", stash)
	}
}

func TestApplyFlowPlanCreateFailureNeverLeavesHalfOpenFlow(t *testing.T) {
	setSupervisorToken(t)
	client := newFakeIntegrationHTTP()
	client.queueResponse("GET", "/core/api/config/config_entries/entry", 200, []map[string]any{})
	client.queueResponse("POST", "/core/api/config/config_entries/flow", 200, map[string]any{
		"flow_id": "flow1", "handler": "workday", "type": "form", "step_id": "user",
		"data_schema": []any{map[string]any{"name": "country", "required": true}},
	})
	client.queueResponse("DELETE", "/core/api/config/config_entries/flow/flow1", 200, nil)

	op := integrationOp(flows.KindCreate, "workday_main", map[string]any{"domain": "workday", "title": "Workday", "data": map[string]any{}}, "")
	stashDir := t.TempDir()
	managed := map[string]string{}
	attempts := map[string]map[string]any{}

	result := ApplyFlowPlan(context.Background(), client, nil, []registries.RegOp{op}, managed, map[string]string{}, map[string]map[string]any{}, attempts, stashDir)
	if result.OK {
		t.Fatalf("result = %+v, want failure", result)
	}
	if len(managed) != 0 {
		t.Errorf("managed = %+v, want empty on failure", managed)
	}
	if len(client.callsFor("DELETE", "/core/api/config/config_entries/flow/flow1")) != 1 {
		t.Errorf("flow was not aborted on failure")
	}

	// Failure memory: the failed data's hash plus a reason, so the next
	// plan for the same data refuses to retry (see internal/flows).
	entry := attempts["integration:workday_main"]
	if entry == nil {
		t.Fatalf("attempts = %+v, want an entry recorded for the failed create", attempts)
	}
	if entry["hash"] != flows.HashData(map[string]any{}) {
		t.Errorf("attempts entry hash = %+v, want the failed data's hash", entry["hash"])
	}
	if errText, _ := entry["error"].(string); errText == "" || !strings.Contains(errText, "user") {
		t.Errorf("attempts entry error = %+v, want a short description naming the failing step", entry["error"])
	}
}

func TestApplyFlowPlanSuccessAfterEarlierFailureClearsAttempts(t *testing.T) {
	// A previously failed key's entry clears as soon as its create
	// succeeds, so a stale failure never blocks a now-managed key.
	setSupervisorToken(t)
	client := newFakeIntegrationHTTP()
	client.queueResponse("GET", "/core/api/config/config_entries/entry", 200, []map[string]any{})
	client.queueResponse("POST", "/core/api/config/config_entries/flow", 200, map[string]any{
		"flow_id": "flow1", "handler": "moon", "type": "create_entry", "title": "Moon",
		"result": map[string]any{"entry_id": "entryABC"},
	})

	op := integrationOp(flows.KindCreate, "moon_home", map[string]any{"domain": "moon", "title": "Moon", "data": map[string]any{}}, "")
	stashDir := t.TempDir()
	managed := map[string]string{}
	attempts := map[string]map[string]any{
		"integration:moon_home": {"hash": flows.HashData(map[string]any{}), "error": "a stale earlier failure"},
	}

	result := ApplyFlowPlan(context.Background(), client, nil, []registries.RegOp{op}, managed, map[string]string{}, map[string]map[string]any{}, attempts, stashDir)
	if !result.OK {
		t.Fatalf("result = %+v, want OK", result)
	}
	if managed["integration:moon_home"] != "entryABC" {
		t.Errorf("managed = %+v", managed)
	}
	if _, ok := attempts["integration:moon_home"]; ok {
		t.Errorf("attempts = %+v, want the stale entry cleared on success", attempts)
	}
}

// --- ApplyFlowPlan: adopt (KindUpdate) -----------------------------------

func TestApplyFlowPlanAdoptNeverContactsHomeAssistant(t *testing.T) {
	setSupervisorToken(t)
	client := newFakeIntegrationHTTP()
	client.queueResponse("GET", "/core/api/config/config_entries/entry", 200, []map[string]any{
		{"entry_id": "abc123", "domain": "workday", "title": "Workday"},
	})

	op := integrationOp(flows.KindUpdate, "workday_main",
		map[string]any{"domain": "workday", "title": "Workday", "data": map[string]any{}}, "abc123")
	stashDir := t.TempDir()
	managed := map[string]string{}
	hashes := map[string]string{}
	dataSnaps := map[string]map[string]any{}

	result := ApplyFlowPlan(context.Background(), client, nil, []registries.RegOp{op}, managed, hashes, dataSnaps, map[string]map[string]any{}, stashDir)
	if !result.OK {
		t.Fatalf("result = %+v", result)
	}
	if managed["integration:workday_main"] != "abc123" {
		t.Errorf("managed = %+v", managed)
	}
	// Only the up-front live-entry-list fetch should have happened - no
	// POST/DELETE to the flow or entry endpoints at all.
	for _, c := range client.calls {
		if c.method != "GET" {
			t.Errorf("unexpected call during adopt: %+v", c)
		}
	}
}

// --- ApplyFlowPlan: delete ------------------------------------------------

func TestApplyFlowPlanDeleteSuccess(t *testing.T) {
	setSupervisorToken(t)
	client := newFakeIntegrationHTTP()
	client.queueResponse("GET", "/core/api/config/config_entries/entry", 200, []map[string]any{
		{"entry_id": "abc123", "domain": "workday", "title": "Workday"},
	})
	client.queueResponse("DELETE", "/core/api/config/config_entries/entry/abc123", 200, nil)

	op := integrationOp(registries.KindDelete, "workday_main", nil, "abc123")
	stashDir := t.TempDir()
	managed := map[string]string{"integration:workday_main": "abc123"}
	hashes := map[string]string{"integration:workday_main": "somehash"}
	dataSnaps := map[string]map[string]any{"integration:workday_main": {"user": map[string]any{"name": "Workday"}}}

	result := ApplyFlowPlan(context.Background(), client, nil, []registries.RegOp{op}, managed, hashes, dataSnaps, map[string]map[string]any{}, stashDir)
	if !result.OK {
		t.Fatalf("result = %+v", result)
	}
	if _, ok := managed["integration:workday_main"]; ok {
		t.Errorf("managed still has the key: %+v", managed)
	}
	if _, ok := hashes["integration:workday_main"]; ok {
		t.Errorf("hashes still has the key: %+v", hashes)
	}
	if _, ok := dataSnaps["integration:workday_main"]; ok {
		t.Errorf("data snapshot still has the key: %+v", dataSnaps)
	}

	stash := readIntegrationStashFile(t, stashDir)
	if len(stash.Ops) != 1 || stash.Ops[0].Domain != "workday" || stash.Ops[0].Title != "Workday" {
		t.Errorf("stash = %+v, want domain/title recovered from the live entry", stash)
	}
	if stash.Ops[0].Data["user"] == nil {
		t.Errorf("stash data = %+v, want the persisted snapshot carried through for replay", stash.Ops[0].Data)
	}
}

func TestApplyFlowPlanDeleteTolerates404(t *testing.T) {
	setSupervisorToken(t)
	client := newFakeIntegrationHTTP()
	client.queueResponse("GET", "/core/api/config/config_entries/entry", 200, []map[string]any{
		{"entry_id": "abc123", "domain": "workday", "title": "Workday"},
	})
	client.queueResponse("DELETE", "/core/api/config/config_entries/entry/abc123", 404, nil)

	op := integrationOp(registries.KindDelete, "workday_main", nil, "abc123")
	managed := map[string]string{"integration:workday_main": "abc123"}

	result := ApplyFlowPlan(context.Background(), client, nil, []registries.RegOp{op}, managed, map[string]string{}, map[string]map[string]any{}, map[string]map[string]any{}, t.TempDir())
	if !result.OK {
		t.Fatalf("result = %+v, want OK (404 means already gone)", result)
	}
}

// --- ApplyFlowPlan: skipped errors, dry-run-like network shape ----------

func TestApplyFlowPlanSkipsErrorOpsWithoutExecuting(t *testing.T) {
	setSupervisorToken(t)
	client := newFakeIntegrationHTTP()
	client.queueResponse("GET", "/core/api/config/config_entries/entry", 200, []map[string]any{})

	errOp := registries.RegOp{Kind: registries.KindError, RType: "integration", Key: "broken", Error: "ambiguous"}
	result := ApplyFlowPlan(context.Background(), client, nil, []registries.RegOp{errOp}, map[string]string{}, map[string]string{}, map[string]map[string]any{}, map[string]map[string]any{}, t.TempDir())
	if !result.OK {
		t.Fatalf("result = %+v", result)
	}
	if len(result.SkippedErrors) != 1 {
		t.Errorf("skipped_errors = %+v, want 1", result.SkippedErrors)
	}
	if len(client.callsFor("POST", "/core/api/config/config_entries/flow")) != 0 {
		t.Errorf("an error op was somehow executed")
	}
}

// --- Rollback: create's inverse deletes the entry ------------------------

func TestRollbackFlowPlanInvertsCreateByDeleting(t *testing.T) {
	setSupervisorToken(t)
	client := newFakeIntegrationHTTP()
	client.queueResponse("DELETE", "/core/api/config/config_entries/entry/entryABC", 200, nil)

	stashDir := t.TempDir()
	if err := writeIntegrationStash(stashDir, []integrationStashEntry{
		{Kind: flows.KindCreate, Key: "moon_home", Domain: "moon", Title: "Moon", EntryID: "entryABC", Data: map[string]any{}},
	}); err != nil {
		t.Fatal(err)
	}

	managed := map[string]string{"integration:moon_home": "entryABC"}
	hashes := map[string]string{"integration:moon_home": "h"}
	dataSnaps := map[string]map[string]any{"integration:moon_home": {}}

	result := RollbackFlowPlan(context.Background(), client, nil, stashDir, managed, hashes, dataSnaps, secrettest.From(t, ""))
	if !result.OK || !result.RolledBack {
		t.Fatalf("result = %+v", result)
	}
	if len(client.callsFor("DELETE", "/core/api/config/config_entries/entry/entryABC")) != 1 {
		t.Errorf("did not delete the created entry")
	}
	if len(managed) != 0 || len(hashes) != 0 || len(dataSnaps) != 0 {
		t.Errorf("bookkeeping not dropped: managed=%+v hashes=%+v data=%+v", managed, hashes, dataSnaps)
	}
}

// --- Rollback: adopt's inverse never contacts Home Assistant ------------

func TestRollbackFlowPlanInvertsAdoptWithoutAnyHTTPCall(t *testing.T) {
	setSupervisorToken(t)
	client := newFakeIntegrationHTTP()

	stashDir := t.TempDir()
	if err := writeIntegrationStash(stashDir, []integrationStashEntry{
		{Kind: flows.KindUpdate, Key: "workday_main", Domain: "workday", Title: "Workday", EntryID: "abc123", Data: map[string]any{}},
	}); err != nil {
		t.Fatal(err)
	}

	managed := map[string]string{"integration:workday_main": "abc123"}
	hashes := map[string]string{"integration:workday_main": "h"}
	dataSnaps := map[string]map[string]any{"integration:workday_main": {}}

	result := RollbackFlowPlan(context.Background(), client, nil, stashDir, managed, hashes, dataSnaps, secrettest.From(t, ""))
	if !result.OK {
		t.Fatalf("result = %+v", result)
	}
	if len(client.calls) != 0 {
		t.Errorf("adopt rollback made an HTTP call: %+v", client.calls)
	}
	if len(managed) != 0 {
		t.Errorf("managed = %+v, want dropped", managed)
	}
}

// --- Rollback: delete's inverse re-runs the declared flow with a fresh id

func TestRollbackFlowPlanInvertsDeleteByReRunningFlow(t *testing.T) {
	setSupervisorToken(t)
	client := newFakeIntegrationHTTP()
	client.queueResponse("POST", "/core/api/config/config_entries/flow", 200, map[string]any{
		"flow_id": "flow1", "handler": "workday", "type": "form", "step_id": "user",
	})
	client.queueResponse("POST", "/core/api/config/config_entries/flow/flow1", 200, map[string]any{
		"flow_id": "flow1", "handler": "workday", "type": "create_entry", "title": "Workday",
		"result": map[string]any{"entry_id": "freshNEW"},
	})

	stashDir := t.TempDir()
	declaredData := map[string]any{"user": map[string]any{"name": "Workday", "country": "PL"}}
	if err := writeIntegrationStash(stashDir, []integrationStashEntry{
		{Kind: registries.KindDelete, Key: "workday_main", Domain: "workday", Title: "Workday", EntryID: "oldID", Data: declaredData},
	}); err != nil {
		t.Fatal(err)
	}

	managed := map[string]string{}
	hashes := map[string]string{}
	dataSnaps := map[string]map[string]any{}

	result := RollbackFlowPlan(context.Background(), client, nil, stashDir, managed, hashes, dataSnaps, secrettest.From(t, ""))
	if !result.OK {
		t.Fatalf("result = %+v", result)
	}
	if managed["integration:workday_main"] != "freshNEW" {
		t.Errorf("managed = %+v, want the fresh entry_id (never the deleted one)", managed)
	}
	if hashes["integration:workday_main"] == "" {
		t.Errorf("hash not re-recorded")
	}
	calls := client.callsFor("POST", "/core/api/config/config_entries/flow/flow1")
	if len(calls) != 1 || calls[0].body["name"] != "Workday" {
		t.Errorf("flow was not replayed with the stashed declared data: calls = %+v", calls)
	}
}

func TestRollbackFlowPlanDeleteInverseFailureLeavesBookkeepingUntouched(t *testing.T) {
	setSupervisorToken(t)
	client := newFakeIntegrationHTTP()
	client.queueResponse("POST", "/core/api/config/config_entries/flow", 500, nil)

	stashDir := t.TempDir()
	if err := writeIntegrationStash(stashDir, []integrationStashEntry{
		{Kind: registries.KindDelete, Key: "workday_main", Domain: "workday", Title: "Workday", EntryID: "oldID", Data: map[string]any{}},
	}); err != nil {
		t.Fatal(err)
	}

	managed := map[string]string{}
	result := RollbackFlowPlan(
		context.Background(), client, nil, stashDir,
		managed, map[string]string{}, map[string]map[string]any{}, secrettest.From(t, ""))
	if result.OK {
		t.Fatalf("result = %+v, want failure", result)
	}
	if len(managed) != 0 {
		t.Errorf("managed = %+v, want empty (the replay never succeeded)", managed)
	}
}

// --- Missing stash is not an error --------------------------------------

func TestRollbackFlowPlanMissingStashIsNotAnError(t *testing.T) {
	setSupervisorToken(t)
	client := newFakeIntegrationHTTP()
	result := RollbackFlowPlan(
		context.Background(), client, nil, t.TempDir(),
		map[string]string{}, map[string]string{}, map[string]map[string]any{}, secrettest.From(t, ""))
	if !result.OK || !result.RolledBack {
		t.Errorf("result = %+v, want a trivial success", result)
	}
}

func TestIntegrationStashExists(t *testing.T) {
	dir := t.TempDir()
	if IntegrationStashExists(dir) {
		t.Error("want false before any stash is written")
	}
	if err := writeIntegrationStash(dir, nil); err != nil {
		t.Fatal(err)
	}
	if !IntegrationStashExists(dir) {
		t.Error("want true after writeIntegrationStash")
	}
}

// --- Per-op isolation: a sibling's failure never undoes an already-
// succeeded op (a broken integration used to delete a good sibling back
// out every cycle; see ApplyFlowPlan's doc comment)

func TestApplyFlowPlanSiblingFailureNeverUndoesAnAlreadySucceededOp(t *testing.T) {
	setSupervisorToken(t)
	client := newFakeIntegrationHTTP()
	client.queueResponse("GET", "/core/api/config/config_entries/entry", 200, []map[string]any{})
	// First op: create "a" (e.g. moon) succeeds outright.
	client.queueResponse("POST", "/core/api/config/config_entries/flow", 200, map[string]any{
		"flow_id": "flowA", "handler": "moon", "type": "create_entry", "title": "A",
		"result": map[string]any{"entry_id": "entryA"},
	})
	// Second op: create "b" (e.g. a misconfigured esphome) fails outright
	// - its 'user' step wants fields and has no declared data.
	client.queueResponse("POST", "/core/api/config/config_entries/flow", 200, map[string]any{
		"flow_id": "flowB", "handler": "workday", "type": "form", "step_id": "user",
		"data_schema": []any{map[string]any{"name": "country", "required": true}},
	})
	client.queueResponse("DELETE", "/core/api/config/config_entries/flow/flowB", 200, nil)

	opA := integrationOp(flows.KindCreate, "a", map[string]any{"domain": "moon", "title": "A", "data": map[string]any{}}, "")
	opB := integrationOp(flows.KindCreate, "b", map[string]any{"domain": "workday", "title": "B", "data": map[string]any{}}, "")
	stashDir := t.TempDir()
	managed := map[string]string{}
	hashes := map[string]string{}
	dataSnaps := map[string]map[string]any{}
	attempts := map[string]map[string]any{}

	result := ApplyFlowPlan(context.Background(), client, nil, []registries.RegOp{opA, opB}, managed, hashes, dataSnaps, attempts, stashDir)

	if result.OK {
		t.Fatalf("result = %+v, want failure (op B failed)", result)
	}
	if result.RolledBack {
		t.Errorf("result.RolledBack = true, want false - per-op isolation never rolls anything back")
	}
	if !strings.Contains(result.Error, "integration:b") {
		t.Errorf("error = %q, want it to name the failed op", result.Error)
	}

	// Op A's create must never be undone: no DELETE against its entry,
	// and it stays genuinely tracked.
	if len(client.callsFor("DELETE", "/core/api/config/config_entries/entry/entryA")) != 0 {
		t.Errorf("op A's successful create was rolled back - it never should be under per-op isolation")
	}
	if managed["integration:a"] != "entryA" {
		t.Errorf("managed = %+v, want a's create to stay tracked", managed)
	}
	if hashes["integration:a"] == "" {
		t.Errorf("hashes = %+v, want a's hash recorded", hashes)
	}
	if len(result.Applied) != 1 || result.Applied[0] != "create integration:a" {
		t.Errorf("Applied = %+v, want a's create reported as genuinely applied even though OK is false", result.Applied)
	}

	// Op B's half-open flow was still cleaned up, and it was never
	// recorded as managed - a clean failure, not a leak.
	if len(client.callsFor("DELETE", "/core/api/config/config_entries/flow/flowB")) != 1 {
		t.Errorf("op B's half-open flow was not aborted")
	}
	if _, ok := managed["integration:b"]; ok {
		t.Errorf("managed = %+v, want b never recorded", managed)
	}
	if attempts["integration:b"] == nil {
		t.Errorf("attempts = %+v, want b's failure recorded for failure memory", attempts)
	}

	stash := readIntegrationStashFile(t, stashDir)
	if len(stash.Ops) != 1 || stash.Ops[0].Key != "a" {
		t.Errorf("stash = %+v, want only a's create recorded", stash)
	}
}

// --- Home Assistant's own rejection reason reaches the error --------------

func TestDriveFlowSurfacesCoresRejectionMessage(t *testing.T) {
	// The live failure: a 400 said what Core disliked about the declared
	// data, and none of it reached the user, the dashboard or the log.
	setSupervisorToken(t)
	client := newFakeIntegrationHTTP()
	client.queueResponse("POST", "/core/api/config/config_entries/flow", 200, map[string]any{
		"flow_id": "flow1", "handler": "time_date", "type": "form", "step_id": "user",
	})
	client.queueResponse("POST", "/core/api/config/config_entries/flow/flow1", 400, nil)
	client.queueResponse("DELETE", "/core/api/config/config_entries/flow/flow1", 200, nil)

	_, _, err := driveFlow(context.Background(), client, "test-token", "time_date",
		map[string]any{"user": map[string]any{"display_options": []any{"date"}}})

	if err == nil {
		t.Fatal("err = nil, want an error")
	}
	if !strings.Contains(err.Error(), "returned HTTP 400") {
		t.Errorf("err = %q, want the status", err)
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("err = %q, want home assistant's own message alongside the status", err)
	}
}

func TestFetchIntegrationEntriesSurfacesCoresRejectionMessage(t *testing.T) {
	setSupervisorToken(t)
	client := newFakeIntegrationHTTP()
	client.queueResponse("GET", "/core/api/config/config_entries/entry", 401, nil)

	_, err := FetchIntegrationEntries(context.Background(), client)

	if err == nil {
		t.Fatal("err = nil, want an error")
	}
	if !strings.Contains(err.Error(), "returned HTTP 401") || !strings.Contains(err.Error(), "boom") {
		t.Errorf("err = %q, want the status and core's own message", err)
	}
}

// --- a rejected step's error never carries the secret it submitted -------

func TestRedactStepSecretsScrubsOnlySecretNamedFields(t *testing.T) {
	tests := []struct {
		name     string
		text     string
		stepData map[string]any
		want     string
	}{
		{
			name:     "a password field's value is scrubbed",
			text:     "User input malformed: invalid value hunter2",
			stepData: map[string]any{"password": "hunter2"},
			want:     "User input malformed: invalid value ***REDACTED***",
		},
		{
			name:     "api_key, token and secret spellings are all covered",
			text:     "bad k1, bad t1, bad s1",
			stepData: map[string]any{"api_key": "k1", "access_token": "t1", "client_secret": "s1"},
			want:     "bad ***REDACTED***, bad ***REDACTED***, bad ***REDACTED***",
		},
		{
			name:     "an ordinary field's value is left alone",
			text:     "invalid value for country: PL",
			stepData: map[string]any{"country": "PL"},
			want:     "invalid value for country: PL",
		},
		{
			// A form section submits as a nested mapping (see
			// subentries.go); its secret is still a secret one level down.
			name:     "a secret inside a section is scrubbed",
			text:     "invalid value 'hunter2' for creds.password",
			stepData: map[string]any{"creds": map[string]any{"password": "hunter2"}},
			want:     "invalid value '***REDACTED***' for creds.password",
		},
		{
			name:     "an ordinary field inside a section is left alone",
			text:     "invalid value 'PL' for locale.country",
			stepData: map[string]any{"locale": map[string]any{"country": "PL"}},
			want:     "invalid value 'PL' for locale.country",
		},
		{
			// The SECTION's own name is not a field name: a section called
			// "secrets" full of ordinary fields must not scrub them.
			name:     "a section named like a secret does not scrub its plain fields",
			text:     "invalid value 'PL' for secrets.country",
			stepData: map[string]any{"secrets": map[string]any{"country": "PL"}},
			want:     "invalid value 'PL' for secrets.country",
		},
		{
			name:     "a non-string secret value is ignored rather than stringified",
			text:     "expected str @ data['password']",
			stepData: map[string]any{"password": 1234},
			want:     "expected str @ data['password']",
		},
		{
			name:     "an empty secret value never blanks the whole message",
			text:     "expected str @ data['password']",
			stepData: map[string]any{"password": ""},
			want:     "expected str @ data['password']",
		},
		{
			name:     "no declared data is a no-op",
			text:     "flow rejected",
			stepData: nil,
			want:     "flow rejected",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := redactStepSecrets(tt.text, tt.stepData); got != tt.want {
				t.Errorf("redactStepSecrets() = %q, want %q", got, tt.want)
			}
		})
	}
}

// --- VM e2e: a step that asks for nothing needs nothing declared -------
//
// `domain: moon` with no `data:` used to fail, though moon's only step has
// an empty data_schema and wanted {}. DOCS.md promises `data` is optional.

func TestDriveFlowEmptySchemaStepNeedsNoDeclaredData(t *testing.T) {
	tests := []struct {
		name string
		form map[string]any
	}{
		{
			name: "data_schema is an empty list",
			form: map[string]any{
				"flow_id": "flow1", "handler": "moon", "type": "form", "step_id": "user",
				"data_schema": []any{},
			},
		},
		{
			name: "data_schema is absent entirely",
			form: map[string]any{"flow_id": "flow1", "handler": "moon", "type": "form", "step_id": "user"},
		},
		{
			name: "data_schema is null",
			form: map[string]any{
				"flow_id": "flow1", "handler": "moon", "type": "form", "step_id": "user", "data_schema": nil,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setSupervisorToken(t)
			client := newFakeIntegrationHTTP()
			client.queueResponse("POST", "/core/api/config/config_entries/flow", 200, tt.form)
			client.queueResponse("POST", "/core/api/config/config_entries/flow/flow1", 200, map[string]any{
				"flow_id": "flow1", "handler": "moon", "type": "create_entry", "title": "Moon",
				"result": map[string]any{"entry_id": "entryABC"},
			})

			entryID, _, err := driveFlow(context.Background(), client, "test-token", "moon", nil)
			if err != nil {
				t.Fatalf("err = %v, want the step answered with {}", err)
			}
			if entryID != "entryABC" {
				t.Errorf("entry_id = %q, want entryABC", entryID)
			}

			calls := client.callsFor("POST", "/core/api/config/config_entries/flow/flow1")
			if len(calls) != 1 {
				t.Fatalf("advance calls = %d, want 1", len(calls))
			}
			if len(calls[0].body) != 0 {
				t.Errorf("advance body = %+v, want {}", calls[0].body)
			}
			if len(client.callsFor("DELETE", "/core/api/config/config_entries/flow/flow1")) != 0 {
				t.Errorf("the flow was aborted even though it completed")
			}
		})
	}
}

func TestDriveFlowEmptySchemaStepStillSubmitsDeclaredData(t *testing.T) {
	// A manifest that does declare data for an empty-schema step still
	// submits it: the empty-schema path is a fallback, not an override.
	setSupervisorToken(t)
	client := newFakeIntegrationHTTP()
	client.queueResponse("POST", "/core/api/config/config_entries/flow", 200, map[string]any{
		"flow_id": "flow1", "handler": "moon", "type": "form", "step_id": "user", "data_schema": []any{},
	})
	client.queueResponse("POST", "/core/api/config/config_entries/flow/flow1", 200, map[string]any{
		"flow_id": "flow1", "handler": "moon", "type": "create_entry", "title": "Moon",
		"result": map[string]any{"entry_id": "entryABC"},
	})

	data := map[string]any{"user": map[string]any{"name": "Moon"}}
	if _, _, err := driveFlow(context.Background(), client, "test-token", "moon", data); err != nil {
		t.Fatalf("err = %v", err)
	}

	calls := client.callsFor("POST", "/core/api/config/config_entries/flow/flow1")
	if len(calls) != 1 || calls[0].body["name"] != "Moon" {
		t.Errorf("advance calls = %+v, want the declared data submitted verbatim", calls)
	}
}

// --- VM e2e: a step that wants fields says which ones --------------------
//
// The "no declared data" error named the step but not its fields, which
// Home Assistant sends along with the step anyway.

func TestDriveFlowMissingDataErrorNamesTheStepsFields(t *testing.T) {
	setSupervisorToken(t)
	client := newFakeIntegrationHTTP()
	client.queueResponse("POST", "/core/api/config/config_entries/flow", 200, map[string]any{
		"flow_id": "flow1", "handler": "time_date", "type": "form", "step_id": "user",
		"data_schema": []any{
			map[string]any{
				"name":     "display_options",
				"required": true,
				"selector": map[string]any{"select": map[string]any{
					"options": []any{
						"time", "date", "date_time", "date_time_utc", "date_time_iso", "time_date", "time_utc",
					},
					"multiple": false,
				}},
			},
		},
	})
	client.queueResponse("DELETE", "/core/api/config/config_entries/flow/flow1", 200, nil)

	_, _, err := driveFlow(context.Background(), client, "test-token", "time_date", nil)
	if err == nil {
		t.Fatal("err = nil, want an error")
	}

	want := "domain time_date: flow step 'user' has no declared data in the manifest (add a data.user mapping). " +
		"Step 'user' accepts: display_options (required, select: time, date, date_time, date_time_utc, " +
		"date_time_iso, time_date, time_utc)"
	if err.Error() != want {
		t.Errorf("err  = %q\nwant = %q", err.Error(), want)
	}
}

func TestDescribeStepSchemaRendersEveryShapeWithoutPanicking(t *testing.T) {
	longName := strings.Repeat("n", 500)

	manyOptions := make([]any, 30)
	for i := range manyOptions {
		manyOptions[i] = fmt.Sprintf("opt-%02d", i+1)
	}
	manyFields := make([]any, 20)
	for i := range manyFields {
		manyFields[i] = map[string]any{"name": fmt.Sprintf("f-%02d", i+1)}
	}

	tests := []struct {
		name   string
		schema any
		want   string
	}{
		{
			name:   "not a list at all says nothing",
			schema: map[string]any{"name": "x"},
			want:   "",
		},
		{
			name:   "an empty list says nothing",
			schema: []any{},
			want:   "",
		},
		{
			name: "several fields are comma-separated",
			schema: []any{
				map[string]any{"name": "host", "required": true, "selector": map[string]any{"text": map[string]any{}}},
				map[string]any{"name": "verify_ssl", "optional": true, "selector": map[string]any{"boolean": map[string]any{}}},
			},
			want: "host (required, text), verify_ssl (optional, boolean)",
		},
		{
			name:   "a legacy type string stands in for a missing selector",
			schema: []any{map[string]any{"name": "port", "required": true, "type": "integer"}},
			want:   "port (required, integer)",
		},
		{
			name: "select options given as value/label objects render their value",
			schema: []any{map[string]any{
				"name":     "mode",
				"required": true,
				"selector": map[string]any{"select": map[string]any{"options": []any{
					map[string]any{"value": "auto", "label": "Automatic"},
					map[string]any{"value": "manual", "label": "By hand"},
				}}},
			}},
			want: "mode (required, select: auto, manual)",
		},
		{
			name: "a selector taking a list says so",
			schema: []any{map[string]any{
				"name":     "conditions",
				"required": true,
				"selector": map[string]any{"select": map[string]any{
					"options": []any{"rain", "snow"}, "multiple": true,
				}},
			}},
			want: "conditions (required, select (multiple): rain, snow)",
		},
		{
			name: "multiple is only claimed where the selector says so",
			schema: []any{
				map[string]any{"name": "one", "selector": map[string]any{"select": map[string]any{
					"options": []any{"a"}, "multiple": false,
				}}},
				map[string]any{"name": "unstated", "selector": map[string]any{"select": map[string]any{
					"options": []any{"a"},
				}}},
				map[string]any{"name": "not_a_bool", "selector": map[string]any{"select": map[string]any{
					"options": []any{"a"}, "multiple": "yes",
				}}},
			},
			want: "one (select: a), unstated (select: a), not_a_bool (select: a)",
		},
		{
			name: "a list selector with no options at all still says multiple",
			schema: []any{map[string]any{
				"name": "entities", "selector": map[string]any{"entity": map[string]any{"multiple": true}},
			}},
			want: "entities (entity (multiple))",
		},
		{
			name: "an option object carrying only a label is dropped",
			schema: []any{map[string]any{
				"name": "mode",
				"selector": map[string]any{"select": map[string]any{"options": []any{
					map[string]any{"label": "Automatic"},
					map[string]any{"value": "manual"},
				}}},
			}},
			want: "mode (select: manual)",
		},
		{
			name: "non-string scalar options still render",
			schema: []any{map[string]any{
				"name":     "level",
				"selector": map[string]any{"select": map[string]any{"options": []any{float64(1), true, "high"}}},
			}},
			want: "level (select: 1, true, high)",
		},
		{
			name: "an oversized options list is truncated, not flooded",
			schema: []any{map[string]any{
				"name":     "entity",
				"required": true,
				"selector": map[string]any{"select": map[string]any{"options": manyOptions}},
			}},
			want: "entity (required, select: opt-01, opt-02, opt-03, opt-04, opt-05, opt-06, opt-07, opt-08, " +
				"opt-09, opt-10, opt-11, opt-12, ...)",
		},
		{
			name:   "an oversized field list is truncated too",
			schema: manyFields,
			want:   "f-01, f-02, f-03, f-04, f-05, f-06, f-07, f-08, f-09, f-10, f-11, f-12" + schemaTruncationMark,
		},
		{
			name:   "an absurdly long rendering is cut to one readable row",
			schema: []any{map[string]any{"name": longName}},
			want:   longName[:maxSchemaChars] + schemaTruncationMark,
		},
		{
			name: "malformed entries degrade to the name, or are dropped",
			schema: []any{
				"not an object",
				42,
				nil,
				map[string]any{"required": true},
				map[string]any{"name": 7},
				map[string]any{"name": "plain"},
				map[string]any{"name": "weird_selector", "selector": "not a map"},
				map[string]any{"name": "empty_selector", "selector": map[string]any{}, "type": "string"},
				map[string]any{"name": "unreadable_config", "selector": map[string]any{"select": "not a map"}},
				map[string]any{"name": "unreadable_options", "selector": map[string]any{"select": map[string]any{"options": "nope"}}},
				map[string]any{"name": "nested_option", "selector": map[string]any{"select": map[string]any{
					"options": []any{map[string]any{"value": []any{"nested"}}, "ok"},
				}}},
			},
			want: "plain, weird_selector, empty_selector (string), unreadable_config (select), " +
				"unreadable_options (select), nested_option (select: ok)",
		},
		{
			name: "a multi-key selector renders deterministically",
			schema: []any{map[string]any{
				"name":     "odd",
				"selector": map[string]any{"select": map[string]any{}, "boolean": map[string]any{}},
			}},
			want: "odd (boolean)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := describeStepSchema(tt.schema); got != tt.want {
				t.Errorf("describeStepSchema() = %q\nwant                  = %q", got, tt.want)
			}
		})
	}
}

func TestDriveFlowUnreadableSchemaStillNamesTheStep(t *testing.T) {
	// A schema this add-on cannot read is not evidence the step wants
	// nothing: the flow still fails, and the error still says which step.
	setSupervisorToken(t)
	client := newFakeIntegrationHTTP()
	client.queueResponse("POST", "/core/api/config/config_entries/flow", 200, map[string]any{
		"flow_id": "flow1", "handler": "weird", "type": "form", "step_id": "user",
		"data_schema": map[string]any{"not": "a list"},
	})
	client.queueResponse("DELETE", "/core/api/config/config_entries/flow/flow1", 200, nil)

	_, _, err := driveFlow(context.Background(), client, "test-token", "weird", nil)
	if err == nil {
		t.Fatal("err = nil, want an error")
	}
	if !strings.Contains(err.Error(), "add a data.user mapping") {
		t.Errorf("err = %q, want it to name the step", err)
	}
	if strings.Contains(err.Error(), "accepts") {
		t.Errorf("err = %q, want no field list for a schema that could not be read", err)
	}
	if len(client.callsFor("DELETE", "/core/api/config/config_entries/flow/flow1")) != 1 {
		t.Errorf("flow was not aborted")
	}
}

// --- VM e2e: the declared title is what ends up on the entry -------------
//
// Adoption matches by domain plus the exact live title, so an entry left
// with the flow's own title could never be adopted back - and losing
// tracking meant the next reconcile created a duplicate beside it.

func flowCreatesEntry(client *fakeIntegrationHTTP, entryID, liveTitle string) {
	client.queueResponse("GET", "/core/api/config/config_entries/entry", 200, []map[string]any{})
	client.queueResponse("POST", "/core/api/config/config_entries/flow", 200, map[string]any{
		"flow_id": "flow1", "handler": "time_date", "type": "create_entry", "title": liveTitle,
		"result": map[string]any{"entry_id": entryID, "title": liveTitle},
	})
}

func TestApplyFlowPlanCreateRenamesTheEntryToTheDeclaredTitle(t *testing.T) {
	setSupervisorToken(t)
	client := newFakeIntegrationHTTP()
	flowCreatesEntry(client, "entryABC", "Time & Date time")
	ws := newFakeWS()

	op := integrationOp(flows.KindCreate, "time_now",
		map[string]any{"domain": "time_date", "title": "GitOps E2E Time", "data": map[string]any{}}, "")
	managed := map[string]string{}

	result := ApplyFlowPlan(context.Background(), client, staticDialer(ws), []registries.RegOp{op},
		managed, map[string]string{}, map[string]map[string]any{}, map[string]map[string]any{}, t.TempDir())
	if !result.OK {
		t.Fatalf("result = %+v, want OK", result)
	}

	calls := ws.callsFor(msgConfigEntriesUpdate)
	if len(calls) != 1 {
		t.Fatalf("%s calls = %+v, want exactly 1", msgConfigEntriesUpdate, ws.callTypes())
	}
	if calls[0].params["entry_id"] != "entryABC" {
		t.Errorf("rename entry_id = %v, want entryABC", calls[0].params["entry_id"])
	}
	if calls[0].params["title"] != "GitOps E2E Time" {
		t.Errorf("rename title = %v, want the declared one", calls[0].params["title"])
	}
	if managed["integration:time_now"] != "entryABC" {
		t.Errorf("managed = %+v", managed)
	}
}

func TestApplyFlowPlanCreateSkipsTheRenameWhenThereIsNothingToChange(t *testing.T) {
	tests := []struct {
		name      string
		declared  string
		liveTitle string
	}{
		{name: "the flow already produced the declared title", declared: "Moon", liveTitle: "Moon"},
		{name: "the manifest declares no title at all", declared: "", liveTitle: "Time & Date time"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setSupervisorToken(t)
			client := newFakeIntegrationHTTP()
			flowCreatesEntry(client, "entryABC", tt.liveTitle)
			ws := newFakeWS()

			op := integrationOp(flows.KindCreate, "time_now",
				map[string]any{"domain": "time_date", "title": tt.declared, "data": map[string]any{}}, "")

			result := ApplyFlowPlan(context.Background(), client, staticDialer(ws), []registries.RegOp{op},
				map[string]string{}, map[string]string{}, map[string]map[string]any{}, map[string]map[string]any{}, t.TempDir())
			if !result.OK {
				t.Fatalf("result = %+v, want OK", result)
			}
			if len(ws.calls) != 0 {
				t.Errorf("websocket calls = %+v, want none", ws.calls)
			}
		})
	}
}

func TestApplyFlowPlanCreateSurvivesAFailedRenameWithTheEntryStillTracked(t *testing.T) {
	setSupervisorToken(t)
	client := newFakeIntegrationHTTP()
	flowCreatesEntry(client, "entryABC", "Time & Date time")
	ws := newFakeWS()
	ws.raiseOn[msgConfigEntriesUpdate] = []error{errors.New("not authorized")}

	op := integrationOp(flows.KindCreate, "time_now",
		map[string]any{"domain": "time_date", "title": "GitOps E2E Time", "data": map[string]any{}}, "")
	stashDir := t.TempDir()
	managed := map[string]string{}
	hashes := map[string]string{}
	dataSnaps := map[string]map[string]any{}
	attempts := map[string]map[string]any{}

	result := ApplyFlowPlan(context.Background(), client, staticDialer(ws), []registries.RegOp{op},
		managed, hashes, dataSnaps, attempts, stashDir)

	// The entry Home Assistant genuinely created is never thrown away over
	// a title: it stays tracked, stashed and reported as applied.
	if managed["integration:time_now"] != "entryABC" {
		t.Errorf("managed = %+v, want the created entry still tracked", managed)
	}
	if hashes["integration:time_now"] == "" {
		t.Errorf("hashes = %+v, want the create's hash recorded", hashes)
	}
	if _, blocked := attempts["integration:time_now"]; blocked {
		t.Errorf("attempts = %+v, want no failure memory for an entry that was created", attempts)
	}
	if len(result.Applied) != 1 || result.Applied[0] != "create integration:time_now" {
		t.Errorf("Applied = %+v, want the create reported as applied", result.Applied)
	}
	stash := readIntegrationStashFile(t, stashDir)
	if len(stash.Ops) != 1 || stash.Ops[0].EntryID != "entryABC" {
		t.Errorf("stash = %+v, want the create journalled so it can still be rolled back", stash)
	}
	if len(client.callsFor("DELETE", "/core/api/config/config_entries/entry/entryABC")) != 0 {
		t.Errorf("the created entry was deleted over a failed rename")
	}

	// It is still reported, distinctly: what is live no longer matches the
	// manifest, and that is exactly the duplicate hazard.
	if result.OK {
		t.Fatalf("result = %+v, want the failed rename reported", result)
	}
	for _, want := range []string{"entryABC", "Time & Date time", "GitOps E2E Time", "adopt", "not authorized"} {
		if !strings.Contains(result.Error, want) {
			t.Errorf("error = %q, want it to mention %q", result.Error, want)
		}
	}
}

func TestApplyFlowPlanCreateReportsARenameItCouldNotEvenAttempt(t *testing.T) {
	// No dialer at all (a caller that never wired one up) is a rename
	// failure like any other - never a silent skip.
	setSupervisorToken(t)
	client := newFakeIntegrationHTTP()
	flowCreatesEntry(client, "entryABC", "Time & Date time")

	op := integrationOp(flows.KindCreate, "time_now",
		map[string]any{"domain": "time_date", "title": "GitOps E2E Time", "data": map[string]any{}}, "")
	managed := map[string]string{}

	result := ApplyFlowPlan(context.Background(), client, nil, []registries.RegOp{op},
		managed, map[string]string{}, map[string]map[string]any{}, map[string]map[string]any{}, t.TempDir())
	if result.OK {
		t.Fatalf("result = %+v, want the rename reported", result)
	}
	if managed["integration:time_now"] != "entryABC" {
		t.Errorf("managed = %+v, want the created entry still tracked", managed)
	}
}

func TestRollbackFlowPlanDeleteInverseRestoresTheDeclaredTitle(t *testing.T) {
	// Rolling back a deletion re-runs the flow, so its entry carries the
	// flow's title and the same adopt-matching hazard.
	setSupervisorToken(t)
	client := newFakeIntegrationHTTP()
	client.queueResponse("POST", "/core/api/config/config_entries/flow", 200, map[string]any{
		"flow_id": "flow1", "handler": "time_date", "type": "create_entry",
		"result": map[string]any{"entry_id": "freshNEW", "title": "Time & Date time"},
	})
	ws := newFakeWS()

	stashDir := t.TempDir()
	if err := writeIntegrationStash(stashDir, []integrationStashEntry{{
		Kind: registries.KindDelete, Key: "time_now", Domain: "time_date",
		Title: "GitOps E2E Time", EntryID: "oldID", Data: map[string]any{},
	}}); err != nil {
		t.Fatal(err)
	}

	managed := map[string]string{}
	result := RollbackFlowPlan(context.Background(), client, staticDialer(ws), stashDir,
		managed, map[string]string{}, map[string]map[string]any{}, secrettest.From(t, ""))
	if !result.OK {
		t.Fatalf("result = %+v", result)
	}
	if managed["integration:time_now"] != "freshNEW" {
		t.Errorf("managed = %+v, want the fresh entry_id", managed)
	}

	calls := ws.callsFor(msgConfigEntriesUpdate)
	if len(calls) != 1 || calls[0].params["entry_id"] != "freshNEW" || calls[0].params["title"] != "GitOps E2E Time" {
		t.Errorf("rename calls = %+v, want the re-created entry retitled to the declared name", calls)
	}
}

func TestRollbackFlowPlanDeleteInverseKeepsBookkeepingWhenTheRenameFails(t *testing.T) {
	setSupervisorToken(t)
	client := newFakeIntegrationHTTP()
	client.queueResponse("POST", "/core/api/config/config_entries/flow", 200, map[string]any{
		"flow_id": "flow1", "handler": "time_date", "type": "create_entry",
		"result": map[string]any{"entry_id": "freshNEW", "title": "Time & Date time"},
	})
	ws := newFakeWS()
	ws.raiseOn[msgConfigEntriesUpdate] = []error{errors.New("not authorized")}

	stashDir := t.TempDir()
	if err := writeIntegrationStash(stashDir, []integrationStashEntry{{
		Kind: registries.KindDelete, Key: "time_now", Domain: "time_date",
		Title: "GitOps E2E Time", EntryID: "oldID", Data: map[string]any{},
	}}); err != nil {
		t.Fatal(err)
	}

	managed := map[string]string{}
	result := RollbackFlowPlan(context.Background(), client, staticDialer(ws), stashDir,
		managed, map[string]string{}, map[string]map[string]any{}, secrettest.From(t, ""))
	if result.OK {
		t.Fatalf("result = %+v, want the failed rename reported", result)
	}
	// The entry exists, so the agent must keep owning it - dropping the
	// bookkeeping here would strand a live config entry nothing tracks.
	if managed["integration:time_now"] != "freshNEW" {
		t.Errorf("managed = %+v, want the re-created entry tracked despite the rename failure", managed)
	}
	if !strings.Contains(result.Error, "GitOps E2E Time") {
		t.Errorf("error = %q, want it to name the title that could not be restored", result.Error)
	}
}

// --- Secret references: what is sent, what is stored, what is shown ------

// secretFlowOp is a create op as flows.Plan builds it for
// "secret://anker_password": Params resolved, Declared the reference,
// Secrets the value the applier must never echo.
func secretFlowOp() registries.RegOp {
	op := integrationOp(flows.KindCreate, "anker", map[string]any{
		"domain": "anker", "title": "Anker",
		"data": map[string]any{"user": map[string]any{"password": secrettest.Resolved}},
	}, "")
	op.Secrets = []string{secrettest.Resolved}
	op.Declared = map[string]any{"user": map[string]any{"password": "secret://anker_password"}}
	return op
}

func TestApplyFlowPlanSubmitsTheResolvedDataButStoresTheReference(t *testing.T) {
	setSupervisorToken(t)
	client := newFakeIntegrationHTTP()
	client.queueResponse("GET", "/core/api/config/config_entries/entry", 200, []map[string]any{})
	client.queueResponse("POST", "/core/api/config/config_entries/flow", 200, map[string]any{
		"flow_id": "flow1", "handler": "anker", "type": "form", "step_id": "user",
	})
	client.queueResponse("POST", "/core/api/config/config_entries/flow/flow1", 200, map[string]any{
		"flow_id": "flow1", "handler": "anker", "type": "create_entry", "title": "Anker",
		"result": map[string]any{"entry_id": "entryNEW", "title": "Anker"},
	})

	managed := map[string]string{}
	hashes := map[string]string{}
	dataSnaps := map[string]map[string]any{}
	stashDir := t.TempDir()

	result := ApplyFlowPlan(context.Background(), client, nil, []registries.RegOp{secretFlowOp()},
		managed, hashes, dataSnaps, map[string]map[string]any{}, stashDir)
	if !result.OK {
		t.Fatalf("result = %+v", result)
	}

	// The flow got the real credential.
	calls := client.callsFor("POST", "/core/api/config/config_entries/flow/flow1")
	if len(calls) != 1 || calls[0].body["password"] != secrettest.Resolved {
		t.Fatalf("flow body = %+v, want the resolved password", calls)
	}
	// state.IntegrationData got the reference.
	snap := dataSnaps["integration:anker"]
	step, _ := snap["user"].(map[string]any)
	if step["password"] != "secret://anker_password" {
		t.Errorf("state data = %+v, want the unresolved reference", snap)
	}
	// The hash is of the RESOLVED data, so the next plan sees a rotation.
	want := flows.HashData(map[string]any{"user": map[string]any{"password": secrettest.Resolved}})
	if hashes["integration:anker"] != want {
		t.Errorf("hash = %q, want the hash of the resolved data", hashes["integration:anker"])
	}
	// And the on-disk rollback journal holds the reference too.
	stash := readIntegrationStashFile(t, stashDir)
	stashStep, _ := stash.Ops[0].Data["user"].(map[string]any)
	if stashStep["password"] != "secret://anker_password" {
		t.Errorf("stash data = %+v, want the unresolved reference", stash.Ops[0].Data)
	}
}

// Core's rejection text is quoted verbatim into the activity feed and
// state.IntegrationAttempts, and a validator may name the rejected value.
func TestApplyFlowPlanScrubsAResolvedSecretOutOfAFailure(t *testing.T) {
	setSupervisorToken(t)
	client := newFakeIntegrationHTTP()
	client.queueResponse("GET", "/core/api/config/config_entries/entry", 200, []map[string]any{})
	client.queueResponse("POST", "/core/api/config/config_entries/flow", 200, map[string]any{
		"flow_id": "flow1", "handler": "anker", "type": "form", "step_id": "user",
	})
	// A field name that says nothing ("password" would already be scrubbed
	// by redactStepSecrets), so only the Secrets list can save this.
	client.queueResponse("POST", "/core/api/config/config_entries/flow/flow1", 200, map[string]any{
		"flow_id": "flow1", "handler": "anker", "type": "form", "step_id": "user",
		"errors": map[string]any{"base": "rejected value " + secrettest.Resolved},
	})
	client.queueResponse("DELETE", "/core/api/config/config_entries/flow/flow1", 200, nil)

	attempts := map[string]map[string]any{}
	result := ApplyFlowPlan(context.Background(), client, nil, []registries.RegOp{secretFlowOp()},
		map[string]string{}, map[string]string{}, map[string]map[string]any{}, attempts, t.TempDir())

	if result.OK {
		t.Fatalf("result = %+v, want the rejection reported", result)
	}
	if strings.Contains(result.Error, secrettest.Resolved) {
		t.Errorf("result error carries the resolved secret: %q", result.Error)
	}
	if !strings.Contains(result.Error, "***REDACTED***") {
		t.Errorf("result error = %q, want the marker where the value was", result.Error)
	}
	remembered, _ := attempts["integration:anker"]["error"].(string)
	if strings.Contains(remembered, secrettest.Resolved) {
		t.Errorf("attempts entry carries the resolved secret onto disk: %q", remembered)
	}
}

// The one path replaying data from storage rather than a fresh op: the
// stash holds a reference, resolved against secrets.yaml as it stands now.
func TestRollbackFlowPlanResolvesAStoredReferenceBeforeReplayingIt(t *testing.T) {
	setSupervisorToken(t)
	client := newFakeIntegrationHTTP()
	client.queueResponse("POST", "/core/api/config/config_entries/flow", 200, map[string]any{
		"flow_id": "flow1", "handler": "anker", "type": "form", "step_id": "user",
	})
	client.queueResponse("POST", "/core/api/config/config_entries/flow/flow1", 200, map[string]any{
		"flow_id": "flow1", "handler": "anker", "type": "create_entry", "title": "Anker",
		"result": map[string]any{"entry_id": "freshNEW", "title": "Anker"},
	})

	stashDir := t.TempDir()
	stored := map[string]any{"user": map[string]any{"password": "secret://anker_password"}}
	if err := writeIntegrationStash(stashDir, []integrationStashEntry{
		{Kind: registries.KindDelete, Key: "anker", Domain: "anker", Title: "Anker", EntryID: "oldID", Data: stored},
	}); err != nil {
		t.Fatal(err)
	}

	managed := map[string]string{}
	hashes := map[string]string{}
	dataSnaps := map[string]map[string]any{}
	result := RollbackFlowPlan(context.Background(), client, nil, stashDir, managed, hashes, dataSnaps,
		secrettest.From(t, "anker_password: "+secrettest.Resolved+"\n"))
	if !result.OK {
		t.Fatalf("result = %+v", result)
	}

	calls := client.callsFor("POST", "/core/api/config/config_entries/flow/flow1")
	if len(calls) != 1 || calls[0].body["password"] != secrettest.Resolved {
		t.Fatalf("replayed body = %+v, want the reference resolved before it went on the wire", calls)
	}
	if hashes["integration:anker"] != flows.HashData(map[string]any{"user": map[string]any{"password": secrettest.Resolved}}) {
		t.Errorf("hash = %q, want the hash of the resolved data", hashes["integration:anker"])
	}
	step, _ := dataSnaps["integration:anker"]["user"].(map[string]any)
	if step["password"] != "secret://anker_password" {
		t.Errorf("re-recorded state data = %+v, want the reference kept", dataSnaps)
	}
}

func TestRollbackFlowPlanReportsAnUnresolvableStoredReference(t *testing.T) {
	setSupervisorToken(t)
	client := newFakeIntegrationHTTP()

	stashDir := t.TempDir()
	stored := map[string]any{"user": map[string]any{"password": "secret://anker_password"}}
	if err := writeIntegrationStash(stashDir, []integrationStashEntry{
		{Kind: registries.KindDelete, Key: "anker", Domain: "anker", Title: "Anker", EntryID: "oldID", Data: stored},
	}); err != nil {
		t.Fatal(err)
	}

	result := RollbackFlowPlan(context.Background(), client, nil, stashDir,
		map[string]string{}, map[string]string{}, map[string]map[string]any{}, secrettest.From(t, "other: x\n"))
	if result.OK {
		t.Fatalf("result = %+v, want the unresolvable reference reported", result)
	}
	if !strings.Contains(result.Error, "no key 'anker_password'") {
		t.Errorf("error = %q, want it to name the missing key", result.Error)
	}
	// No flow was started, so nothing half-created is left behind.
	if len(client.callsFor("POST", "/core/api/config/config_entries/flow")) != 0 {
		t.Errorf("a flow was driven despite the unresolvable reference")
	}
}

// A pre-secret-references stash holds plaintext, so rolling it back on a
// box with no secrets.yaml works: the lazy resolver never opens the file,
// which is what LoadCount proves.
func TestRollbackFlowPlanReplaysAPreSecretStashWithoutTouchingTheSecretsFile(t *testing.T) {
	setSupervisorToken(t)
	client := newFakeIntegrationHTTP()
	client.queueResponse("POST", "/core/api/config/config_entries/flow", 200, map[string]any{
		"flow_id": "flow1", "handler": "workday", "type": "form", "step_id": "user",
	})
	client.queueResponse("POST", "/core/api/config/config_entries/flow/flow1", 200, map[string]any{
		"flow_id": "flow1", "handler": "workday", "type": "create_entry", "title": "Workday",
		"result": map[string]any{"entry_id": "freshNEW", "title": "Workday"},
	})

	stashDir := t.TempDir()
	plaintext := map[string]any{"user": map[string]any{"name": "Workday", "password": "written-before-references-existed"}}
	if err := writeIntegrationStash(stashDir, []integrationStashEntry{
		{Kind: registries.KindDelete, Key: "workday_main", Domain: "workday", Title: "Workday", EntryID: "oldID", Data: plaintext},
	}); err != nil {
		t.Fatal(err)
	}

	// A config root with no secrets.yaml in it at all.
	secrets := secretref.NewResolver(t.TempDir())
	managed := map[string]string{}
	result := RollbackFlowPlan(context.Background(), client, nil, stashDir,
		managed, map[string]string{}, map[string]map[string]any{}, secrets)

	if !result.OK {
		t.Fatalf("result = %+v, want a pre-secret stash to replay unchanged", result)
	}
	calls := client.callsFor("POST", "/core/api/config/config_entries/flow/flow1")
	if len(calls) != 1 || calls[0].body["password"] != "written-before-references-existed" {
		t.Errorf("replayed body = %+v, want the stashed plaintext submitted as it stands", calls)
	}
	if managed["integration:workday_main"] != "freshNEW" {
		t.Errorf("managed = %+v, want the re-created entry recorded", managed)
	}
	if got := secrets.LoadCount(); got != 0 {
		t.Errorf("secrets.yaml was read %d times, want 0: nothing in this stash references it", got)
	}
}

// One commit declares both the HACS download and the config entry: the
// download lands but the domain is not importable until a restart, so the
// flow gets 404 "Invalid handler specified". Remembering that would block
// the entry forever on a hash that never changes.
func TestApplyFlowPlanDoesNotRememberADomainThatIsNotLoadedYet(t *testing.T) {
	setSupervisorToken(t)
	client := newFakeIntegrationHTTP()
	client.queueResponse("GET", "/core/api/config/config_entries/entry", 200, []map[string]any{})
	client.queueResponse("POST", "/core/api/config/config_entries/flow", 404,
		map[string]any{"message": "Invalid handler specified"})

	op := integrationOp(flows.KindCreate, "anker",
		map[string]any{"domain": "anker_solix", "title": "Anker Solix", "data": map[string]any{}}, "")
	attempts := map[string]map[string]any{}

	result := ApplyFlowPlan(context.Background(), client, nil, []registries.RegOp{op},
		map[string]string{}, map[string]string{}, map[string]map[string]any{}, attempts, t.TempDir())

	if result.OK {
		t.Fatalf("result = %+v, want the create reported as failed this cycle", result)
	}
	if len(attempts) != 0 {
		t.Errorf("attempts = %+v, want nothing remembered for a transient condition", attempts)
	}
	// And the message has to say what to do, since the agent will not do it:
	// this add-on never restarts Home Assistant.
	for _, want := range []string{"restart home assistant", "on its own"} {
		if !strings.Contains(strings.ToLower(result.Error), want) {
			t.Errorf("error = %q, want it to mention %q", result.Error, want)
		}
	}
}

// Same journey with a declared secret: the failure is redacted before the
// sentinel is checked, so a flattened wrap chain made errors.Is false and
// the item was remembered as failed. Redaction keeps the cause now.
func TestApplyFlowPlanDoesNotRememberANotLoadedDomainForAnOpWithSecrets(t *testing.T) {
	setSupervisorToken(t)
	client := newFakeIntegrationHTTP()
	client.queueResponse("GET", "/core/api/config/config_entries/entry", 200, []map[string]any{})
	client.queueResponse("POST", "/core/api/config/config_entries/flow", 404,
		map[string]any{"message": "Invalid handler specified"})

	op := secretFlowOp()
	attempts := map[string]map[string]any{}

	result := ApplyFlowPlan(context.Background(), client, nil, []registries.RegOp{op},
		map[string]string{}, map[string]string{}, map[string]map[string]any{}, attempts, t.TempDir())

	if result.OK {
		t.Fatalf("result = %+v, want the create reported as failed this cycle", result)
	}
	if len(attempts) != 0 {
		t.Errorf("attempts = %+v, want nothing remembered for a transient condition", attempts)
	}
	// And the redaction itself is unaffected: the resolved value stays out
	// of everything this reports.
	if strings.Contains(result.Error, secrettest.Resolved) {
		t.Errorf("error = %q, want the resolved secret scrubbed", result.Error)
	}
}

// Every other 404 keeps the old behaviour - the sentinel is about a handler
// Home Assistant does not have, not about 404 as a status.
func TestApplyFlowPlanStillRemembersAnOrdinaryFlowFailure(t *testing.T) {
	setSupervisorToken(t)
	client := newFakeIntegrationHTTP()
	client.queueResponse("GET", "/core/api/config/config_entries/entry", 200, []map[string]any{})
	client.queueResponse("POST", "/core/api/config/config_entries/flow", 200, map[string]any{
		"flow_id": "flow1", "handler": "workday", "type": "abort", "reason": "single_instance_allowed",
	})

	op := integrationOp(flows.KindCreate, "workday_main",
		map[string]any{"domain": "workday", "title": "Workday", "data": map[string]any{}}, "")
	attempts := map[string]map[string]any{}

	ApplyFlowPlan(context.Background(), client, nil, []registries.RegOp{op},
		map[string]string{}, map[string]string{}, map[string]map[string]any{}, attempts, t.TempDir())

	if _, found := attempts["integration:workday_main"]; !found {
		t.Errorf("attempts = %+v, want an ordinary refusal remembered", attempts)
	}
}

// needsStashDir exempts an all-adopt plan, so stashDir arrives as "" while
// the plan may be nothing but error ops. The nil client and dialer are the
// assertion: if the zero-executable guard stops firing, they deref.
func TestApplyFlowPlanWithOnlyErrorOpsDoesNoIOAndKeepsThemPending(t *testing.T) {
	ops := []registries.RegOp{{
		Kind: registries.KindError, RType: "integration", Key: "anker_solix_main",
		Error: "references a secret that could not be resolved",
	}}

	result := ApplyFlowPlan(context.Background(), nil, nil, ops, nil, nil, nil, nil, "")

	if !result.OK {
		t.Fatalf("result = %+v, want OK for a plan with nothing to execute", result)
	}
	if len(result.SkippedErrors) != 1 || result.SkippedErrors[0].Key != "anker_solix_main" {
		t.Errorf("skipped_errors = %+v, want the error op passed through to stay pending", result.SkippedErrors)
	}
}
