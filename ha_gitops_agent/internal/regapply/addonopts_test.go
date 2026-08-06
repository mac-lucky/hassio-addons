package regapply

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/addonopts"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/registries"
)

// --- fakeAddonHTTP: records every Do() call and answers each "METHOD
// path" from a FIFO whose last entry repeats (so a poll loop needs no
// entry per iteration); an unqueued key returns a bare 200 {}.

type addonHTTPCall struct {
	method string
	path   string
	body   map[string]any
}

type addonHTTPResp struct {
	status int
	data   map[string]any
}

type fakeAddonHTTP struct {
	mu    sync.Mutex
	calls []addonHTTPCall
	queue map[string][]addonHTTPResp
}

func newFakeAddonHTTP() *fakeAddonHTTP {
	return &fakeAddonHTTP{queue: map[string][]addonHTTPResp{}}
}

func (f *fakeAddonHTTP) queueResponse(method, path string, status int, data map[string]any) {
	key := method + " " + path
	f.queue[key] = append(f.queue[key], addonHTTPResp{status: status, data: data})
}

func (f *fakeAddonHTTP) Do(req *http.Request) (*http.Response, error) {
	var body map[string]any
	if req.Body != nil {
		data, _ := io.ReadAll(req.Body)
		if len(data) > 0 {
			_ = json.Unmarshal(data, &body)
		}
	}
	key := req.Method + " " + req.URL.Path

	f.mu.Lock()
	f.calls = append(f.calls, addonHTTPCall{method: req.Method, path: req.URL.Path, body: body})
	resp := addonHTTPResp{status: 200, data: map[string]any{}}
	if q := f.queue[key]; len(q) > 0 {
		resp = q[0]
		if len(q) > 1 {
			f.queue[key] = q[1:]
		}
	}
	f.mu.Unlock()

	envelope := map[string]any{"result": "ok", "data": resp.data}
	if resp.status >= 400 {
		// "boom" unless the test queued its own "message": httperr quotes
		// the body verbatim, which the redaction tests need to exercise.
		message := "boom"
		if queued, ok := resp.data["message"].(string); ok && queued != "" {
			message = queued
		}
		envelope = map[string]any{"result": "error", "message": message}
	}
	payload, _ := json.Marshal(envelope)
	return &http.Response{
		StatusCode: resp.status,
		Body:       io.NopCloser(bytes.NewReader(payload)),
		Header:     make(http.Header),
	}, nil
}

func (f *fakeAddonHTTP) callsFor(method, path string) []addonHTTPCall {
	var out []addonHTTPCall
	for _, c := range f.calls {
		if c.method == method && c.path == path {
			out = append(out, c)
		}
	}
	return out
}

// A zero interval runs pollAddonStarted's real sleep loop instantly;
// per-test addonRestartPollTimeout overrides still apply on top.
func init() {
	addonRestartPollInterval = 0
}

func setSupervisorToken(t *testing.T) {
	t.Helper()
	t.Setenv("SUPERVISOR_TOKEN", "test-token")
}

func addonOp(kind, slug string, params map[string]any) registries.RegOp {
	if params == nil {
		params = map[string]any{}
	}
	return registries.RegOp{Kind: kind, RType: "addon", Key: slug, Params: params, LiveID: slug, DiffText: "..."}
}

func readAddonStashFile(t *testing.T, stashDir string) addonStashFileOnDisk {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(stashDir, "addon_stash.json"))
	if err != nil {
		t.Fatal(err)
	}
	var decoded addonStashFileOnDisk
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	return decoded
}

// --- FetchAddonInfoAll ----------------------------------------------------

func TestFetchAddonInfoAllInstalledAddon(t *testing.T) {
	setSupervisorToken(t)
	client := newFakeAddonHTTP()
	client.queueResponse("GET", "/addons/core_configurator/info", 200,
		map[string]any{"options": map[string]any{"dirsfirst": true}, "state": "started"})

	got, err := FetchAddonInfoAll(context.Background(), client, []string{"core_configurator"})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	want := map[string]map[string]any{
		"core_configurator": {"options": map[string]any{"dirsfirst": true}, "state": "started"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestFetchAddonInfoAllUnknownSlugIs404(t *testing.T) {
	setSupervisorToken(t)
	client := newFakeAddonHTTP()
	client.queueResponse("GET", "/addons/ghost/info", 404, nil)

	got, err := FetchAddonInfoAll(context.Background(), client, []string{"ghost"})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if installed, _ := got["ghost"]["installed"].(bool); installed {
		t.Errorf("got %+v, want installed: false", got)
	}
}

func TestFetchAddonInfoAllKnownButNeverInstalled(t *testing.T) {
	setSupervisorToken(t)
	client := newFakeAddonHTTP()
	client.queueResponse("GET", "/addons/store_only/info", 200,
		map[string]any{"installed": false, "options": map[string]any{}, "state": "unknown"})

	got, err := FetchAddonInfoAll(context.Background(), client, []string{"store_only"})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if installed, _ := got["store_only"]["installed"].(bool); installed {
		t.Errorf("got %+v, want installed: false", got)
	}
}

func TestFetchAddonInfoAllMissingTokenErrors(t *testing.T) {
	t.Setenv("SUPERVISOR_TOKEN", "")
	_, err := FetchAddonInfoAll(context.Background(), newFakeAddonHTTP(), []string{"x"})
	if err == nil {
		t.Fatal("expected an error")
	}
}

// --- FetchSelfAddonSlug ----------------------------------------------------

func TestFetchSelfAddonSlug(t *testing.T) {
	setSupervisorToken(t)
	client := newFakeAddonHTTP()
	client.queueResponse("GET", "/addons/self/info", 200, map[string]any{"slug": "ha_gitops_agent"})

	slug, err := FetchSelfAddonSlug(context.Background(), client)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if slug != "ha_gitops_agent" {
		t.Errorf("slug = %q", slug)
	}
}

func TestFetchSelfAddonSlugMissingSlugIsAnError(t *testing.T) {
	setSupervisorToken(t)
	client := newFakeAddonHTTP()
	client.queueResponse("GET", "/addons/self/info", 200, map[string]any{})

	_, err := FetchSelfAddonSlug(context.Background(), client)
	if err == nil {
		t.Fatal("expected an error")
	}
}

// --- ApplyAddonPlan(): read-merge-write ------------------------------------

func TestApplyAddonPlanReadMergeWriteNeverDropsUndeclaredKeys(t *testing.T) {
	setSupervisorToken(t)
	stashDir := t.TempDir()
	client := newFakeAddonHTTP()
	client.queueResponse("GET", "/addons/core_configurator/info", 200, map[string]any{
		"options": map[string]any{"dirsfirst": false, "hide_dotfiles": true, "untouched": "keepme"},
		"state":   "started",
	})
	ops := []registries.RegOp{addonOp(addonopts.KindUpdate, "core_configurator", map[string]any{"dirsfirst": true})}
	originals := map[string]map[string]any{}
	restartState := map[string]bool{}

	result := ApplyAddonPlan(
		context.Background(), client, ops, map[string]bool{"core_configurator": false}, originals, restartState, stashDir)

	if !result.OK {
		t.Fatalf("result = %+v", result)
	}
	calls := client.callsFor("POST", "/addons/core_configurator/options")
	if len(calls) != 1 {
		t.Fatalf("options calls = %+v", calls)
	}
	sentOptions, _ := calls[0].body["options"].(map[string]any)
	want := map[string]any{"dirsfirst": true, "hide_dotfiles": true, "untouched": "keepme"}
	if !reflect.DeepEqual(sentOptions, want) {
		t.Errorf("sent options = %+v, want %+v (undeclared keys must survive)", sentOptions, want)
	}
}

// --- ApplyAddonPlan(): default-pinning fix (B1) ----------------------------

func TestApplyAddonPlanNoOpDeclarationPostsNothing(t *testing.T) {
	setSupervisorToken(t)
	stashDir := t.TempDir()
	client := newFakeAddonHTTP()
	client.queueResponse("GET", "/addons/core_configurator/info", 200, map[string]any{
		"options": map[string]any{"dirsfirst": true}, "state": "started",
	})
	ops := []registries.RegOp{addonOp(addonopts.KindUpdate, "core_configurator", map[string]any{"dirsfirst": true})}

	result := ApplyAddonPlan(context.Background(), client, ops,
		map[string]bool{"core_configurator": true}, map[string]map[string]any{}, map[string]bool{}, stashDir)

	if !result.OK {
		t.Fatalf("result = %+v", result)
	}
	if calls := client.callsFor("POST", "/addons/core_configurator/options"); len(calls) != 0 {
		t.Errorf("options POST calls = %+v, want none (declared value already matches live)", calls)
	}
}

func TestApplyAddonPlanReconstructsPersistedOnlyFromStoreDefaults(t *testing.T) {
	setSupervisorToken(t)
	stashDir := t.TempDir()
	client := newFakeAddonHTTP()
	client.queueResponse("GET", "/store/addons/core_configurator", 200, map[string]any{
		"options": map[string]any{"dirsfirst": false, "hide_dotfiles": false},
	})
	client.queueResponse("GET", "/addons/core_configurator/info", 200, map[string]any{
		"options": map[string]any{
			"dirsfirst": false, "hide_dotfiles": true, "extra_real_override": "keepme",
		},
		"state": "started",
	})
	ops := []registries.RegOp{addonOp(addonopts.KindUpdate, "core_configurator", map[string]any{"theme": "dark"})}

	result := ApplyAddonPlan(context.Background(), client, ops,
		map[string]bool{"core_configurator": false}, map[string]map[string]any{}, map[string]bool{}, stashDir)

	if !result.OK {
		t.Fatalf("result = %+v", result)
	}
	calls := client.callsFor("POST", "/addons/core_configurator/options")
	if len(calls) != 1 {
		t.Fatalf("options calls = %+v", calls)
	}
	sentOptions, _ := calls[0].body["options"].(map[string]any)
	want := map[string]any{
		// "dirsfirst" is absent: it matched its schema default, so it was
		// never a real persisted override and must not be pinned as one.
		"hide_dotfiles": true, "extra_real_override": "keepme", "theme": "dark",
	}
	if !reflect.DeepEqual(sentOptions, want) {
		t.Errorf("sent options = %+v, want %+v", sentOptions, want)
	}
}

func TestApplyAddonPlanFallsBackWhenStoreDefaultsUnavailable(t *testing.T) {
	// On today's Supervisor GET /store/addons/<slug> carries no "options"
	// (see fetchAddonStoreDefaultsRaw), so this degrades to the full view.
	setSupervisorToken(t)
	stashDir := t.TempDir()
	client := newFakeAddonHTTP()
	// No GET /store/addons/core_configurator queued at all - fakeAddonHTTP
	// returns a bare 200 {} for it, exactly like real Supervisor today.
	client.queueResponse("GET", "/addons/core_configurator/info", 200, map[string]any{
		"options": map[string]any{"dirsfirst": false, "untouched": "keepme"}, "state": "started",
	})
	ops := []registries.RegOp{addonOp(addonopts.KindUpdate, "core_configurator", map[string]any{"dirsfirst": true})}

	result := ApplyAddonPlan(context.Background(), client, ops,
		map[string]bool{"core_configurator": false}, map[string]map[string]any{}, map[string]bool{}, stashDir)

	if !result.OK {
		t.Fatalf("result = %+v", result)
	}
	calls := client.callsFor("POST", "/addons/core_configurator/options")
	if len(calls) != 1 {
		t.Fatalf("options calls = %+v", calls)
	}
	sentOptions, _ := calls[0].body["options"].(map[string]any)
	want := map[string]any{"dirsfirst": true, "untouched": "keepme"}
	if !reflect.DeepEqual(sentOptions, want) {
		t.Errorf("sent options = %+v, want %+v", sentOptions, want)
	}
}

func TestApplyAddonPlanFirstManagementRecordsOriginals(t *testing.T) {
	setSupervisorToken(t)
	stashDir := t.TempDir()
	client := newFakeAddonHTTP()
	client.queueResponse("GET", "/addons/core_configurator/info", 200, map[string]any{
		"options": map[string]any{"dirsfirst": false}, "state": "started",
	})
	ops := []registries.RegOp{addonOp(addonopts.KindUpdate, "core_configurator", map[string]any{"dirsfirst": true})}
	originals := map[string]map[string]any{}
	restartState := map[string]bool{}

	result := ApplyAddonPlan(
		context.Background(), client, ops, map[string]bool{"core_configurator": false}, originals, restartState, stashDir)

	if !result.OK {
		t.Fatalf("result = %+v", result)
	}
	want := map[string]map[string]any{"addon:core_configurator": {"dirsfirst": false}}
	if !reflect.DeepEqual(originals, want) {
		t.Errorf("originals = %+v, want %+v", originals, want)
	}
	if restartState["addon:core_configurator"] != false {
		t.Errorf("restart_on_change state = %v, want false", restartState["addon:core_configurator"])
	}
}

func TestApplyAddonPlanNewDeclaredKeyOnAlreadyManagedOnlyRecordsNewField(t *testing.T) {
	setSupervisorToken(t)
	stashDir := t.TempDir()
	client := newFakeAddonHTTP()
	client.queueResponse("GET", "/addons/core_configurator/info", 200, map[string]any{
		"options": map[string]any{"dirsfirst": true, "hide_dotfiles": true}, "state": "started",
	})
	ops := []registries.RegOp{addonOp(addonopts.KindUpdate, "core_configurator",
		map[string]any{"dirsfirst": true, "hide_dotfiles": true})}
	originals := map[string]map[string]any{"addon:core_configurator": {"dirsfirst": false}}
	restartState := map[string]bool{"addon:core_configurator": false}

	result := ApplyAddonPlan(
		context.Background(), client, ops, map[string]bool{"core_configurator": true}, originals, restartState, stashDir)

	if !result.OK {
		t.Fatalf("result = %+v", result)
	}
	// "dirsfirst"'s true original must survive this op's pre-write live
	// value; only the newly declared "hide_dotfiles" is recorded.
	want := map[string]map[string]any{"addon:core_configurator": {"dirsfirst": false, "hide_dotfiles": true}}
	if !reflect.DeepEqual(originals, want) {
		t.Errorf("originals = %+v, want %+v", originals, want)
	}
}

// --- ApplyAddonPlan(): restart behavior -------------------------------------

func TestApplyAddonPlanRestartsWhenChangedAndRestartOnChange(t *testing.T) {
	setSupervisorToken(t)
	stashDir := t.TempDir()
	client := newFakeAddonHTTP()
	client.queueResponse("GET", "/addons/core_configurator/info", 200, map[string]any{
		"options": map[string]any{"dirsfirst": false}, "state": "started",
	})
	ops := []registries.RegOp{addonOp(addonopts.KindUpdate, "core_configurator", map[string]any{"dirsfirst": true})}

	result := ApplyAddonPlan(context.Background(), client, ops,
		map[string]bool{"core_configurator": true}, map[string]map[string]any{}, map[string]bool{}, stashDir)

	if !result.OK {
		t.Fatalf("result = %+v", result)
	}
	if len(client.callsFor("POST", "/addons/core_configurator/restart")) != 1 {
		t.Errorf("restart calls = %+v, want 1", client.callsFor("POST", "/addons/core_configurator/restart"))
	}
}

func TestApplyAddonPlanNeverRestartsWhenUnchanged(t *testing.T) {
	setSupervisorToken(t)
	stashDir := t.TempDir()
	client := newFakeAddonHTTP()
	client.queueResponse("GET", "/addons/core_configurator/info", 200, map[string]any{
		"options": map[string]any{"dirsfirst": true}, "state": "started",
	})
	ops := []registries.RegOp{addonOp(addonopts.KindUpdate, "core_configurator", map[string]any{"dirsfirst": true})}

	result := ApplyAddonPlan(context.Background(), client, ops,
		map[string]bool{"core_configurator": true}, map[string]map[string]any{}, map[string]bool{}, stashDir)

	if !result.OK {
		t.Fatalf("result = %+v", result)
	}
	if len(client.callsFor("POST", "/addons/core_configurator/restart")) != 0 {
		t.Errorf("restart calls = %+v, want none (no value drift)", client.callsFor("POST", "/addons/core_configurator/restart"))
	}
}

func TestApplyAddonPlanNeverRestartsWhenRestartOnChangeFalse(t *testing.T) {
	setSupervisorToken(t)
	stashDir := t.TempDir()
	client := newFakeAddonHTTP()
	client.queueResponse("GET", "/addons/core_configurator/info", 200, map[string]any{
		"options": map[string]any{"dirsfirst": false}, "state": "started",
	})
	ops := []registries.RegOp{addonOp(addonopts.KindUpdate, "core_configurator", map[string]any{"dirsfirst": true})}

	result := ApplyAddonPlan(context.Background(), client, ops,
		map[string]bool{"core_configurator": false}, map[string]map[string]any{}, map[string]bool{}, stashDir)

	if !result.OK {
		t.Fatalf("result = %+v", result)
	}
	if len(client.callsFor("POST", "/addons/core_configurator/restart")) != 0 {
		t.Errorf("restart calls = %+v, want none (restart_on_change false)", client.callsFor("POST", "/addons/core_configurator/restart"))
	}
}

func TestApplyAddonPlanPollsUntilStarted(t *testing.T) {
	setSupervisorToken(t)
	addonRestartPollInterval = 0
	stashDir := t.TempDir()
	client := newFakeAddonHTTP()
	// First info call (pre-write fetch) sees the drifted value; the two
	// post-restart poll calls see "startup" then "started".
	client.queue["GET /addons/core_configurator/info"] = []addonHTTPResp{
		{status: 200, data: map[string]any{"options": map[string]any{"dirsfirst": false}, "state": "started"}},
		{status: 200, data: map[string]any{"options": map[string]any{"dirsfirst": true}, "state": "startup"}},
		{status: 200, data: map[string]any{"options": map[string]any{"dirsfirst": true}, "state": "started"}},
	}
	ops := []registries.RegOp{addonOp(addonopts.KindUpdate, "core_configurator", map[string]any{"dirsfirst": true})}

	result := ApplyAddonPlan(context.Background(), client, ops,
		map[string]bool{"core_configurator": true}, map[string]map[string]any{}, map[string]bool{}, stashDir)

	if !result.OK {
		t.Fatalf("result = %+v", result)
	}
	if got := len(client.callsFor("GET", "/addons/core_configurator/info")); got != 3 {
		t.Errorf("info calls = %d, want 3 (1 pre-write fetch + 2 poll iterations)", got)
	}
}

func TestApplyAddonPlanRestartPollTimeoutFailsOpAndInverts(t *testing.T) {
	setSupervisorToken(t)
	prevTimeout := addonRestartPollTimeout
	addonRestartPollInterval = 0
	addonRestartPollTimeout = 0 // first poll check already past deadline
	defer func() { addonRestartPollTimeout = prevTimeout }()
	stashDir := t.TempDir()
	client := newFakeAddonHTTP()
	client.queueResponse("GET", "/addons/core_configurator/info", 200, map[string]any{
		"options": map[string]any{"dirsfirst": false}, "state": "startup", // never reaches "started"
	})
	ops := []registries.RegOp{addonOp(addonopts.KindUpdate, "core_configurator", map[string]any{"dirsfirst": true})}

	result := ApplyAddonPlan(context.Background(), client, ops,
		map[string]bool{"core_configurator": true}, map[string]map[string]any{}, map[string]bool{}, stashDir)

	if result.OK {
		t.Fatalf("result = %+v, want ok=false", result)
	}
	if !strings.Contains(result.Error, "did not report state") {
		t.Errorf("error = %q", result.Error)
	}
	// The inverse restores the pre-op options - a second options POST
	// carrying the original (unchanged) value.
	calls := client.callsFor("POST", "/addons/core_configurator/options")
	if len(calls) < 2 {
		t.Fatalf("options calls = %+v, want at least 2 (forward + inverse)", calls)
	}
	restored, _ := calls[len(calls)-1].body["options"].(map[string]any)
	if restored["dirsfirst"] != false {
		t.Errorf("inverse options = %+v, want dirsfirst restored to false", restored)
	}
}

// Double fault: the options POST succeeds, the restart poll times out,
// and the inline self-revert POST fails too. Originals must still be
// recorded, or the add-on's pre-management value is lost for good.
func TestApplyAddonPlanDoubleFaultPreservesOriginalsAndStash(t *testing.T) {
	setSupervisorToken(t)
	prevTimeout := addonRestartPollTimeout
	addonRestartPollInterval = 0
	addonRestartPollTimeout = 0 // first poll check already past deadline
	defer func() { addonRestartPollTimeout = prevTimeout }()
	stashDir := t.TempDir()
	client := newFakeAddonHTTP()
	client.queueResponse("GET", "/addons/core_configurator/info", 200, map[string]any{
		"options": map[string]any{"dirsfirst": false}, "state": "startup", // never reaches "started"
	})
	// Forward write succeeds; the self-revert after the poll timeout is
	// queued to fail too - the double fault.
	client.queueResponse("POST", "/addons/core_configurator/options", 200, map[string]any{})
	client.queueResponse("POST", "/addons/core_configurator/options", 500, map[string]any{})
	ops := []registries.RegOp{addonOp(addonopts.KindUpdate, "core_configurator", map[string]any{"dirsfirst": true})}
	originals := map[string]map[string]any{}
	restartState := map[string]bool{}

	result := ApplyAddonPlan(context.Background(), client, ops,
		map[string]bool{"core_configurator": true}, originals, restartState, stashDir)

	if result.OK {
		t.Fatalf("result = %+v, want ok=false", result)
	}
	if result.RolledBack {
		t.Error("result.RolledBack = true, want false: the self-revert genuinely failed, nothing was undone")
	}
	if !strings.Contains(result.Error, "could not restore its prior options") {
		t.Errorf("error = %q, want it to mention the failed revert", result.Error)
	}
	if len(result.Applied) != 1 || result.Applied[0] != "update addon:core_configurator" {
		t.Errorf("applied = %+v, want the stuck op reported as applied", result.Applied)
	}

	wantOriginals := map[string]any{"dirsfirst": false}
	if !reflect.DeepEqual(originals["addon:core_configurator"], wantOriginals) {
		t.Errorf("originals = %+v, want %+v (the true pre-management value must survive the double fault)", originals, wantOriginals)
	}
	if !restartState["addon:core_configurator"] {
		t.Errorf("restart_on_change state = %+v, want core_configurator recorded as managed", restartState)
	}

	stash := readAddonStashFile(t, stashDir)
	if len(stash.Ops) != 1 || stash.Ops[0].Slug != "core_configurator" {
		t.Errorf("addon_stash.json ops = %+v, want the stuck op recorded for a future manual Rollback", stash.Ops)
	}
}

// aaa applies, then bbb double-faults: aaa was genuinely reverted, so it
// must not be re-listed in Applied or addon_stash.json alongside bbb -
// recon would skip its retry and a Rollback would replay a stale entry.
func TestApplyAddonPlanDoubleFaultDoesNotRelistRevertedSiblingAsApplied(t *testing.T) {
	setSupervisorToken(t)
	prevTimeout := addonRestartPollTimeout
	addonRestartPollInterval = 0
	addonRestartPollTimeout = 0 // first poll check already past deadline
	defer func() { addonRestartPollTimeout = prevTimeout }()
	stashDir := t.TempDir()
	client := newFakeAddonHTTP()
	// Two GETs for aaa: the forward write reads the pre-op state, the
	// invert's fresh fetch reads the state after it - fakeAddonHTTP has
	// no backing store to update itself.
	client.queueResponse("GET", "/addons/aaa/info", 200, map[string]any{
		"options": map[string]any{"enabled": false}, "state": "started",
	})
	client.queueResponse("GET", "/addons/aaa/info", 200, map[string]any{
		"options": map[string]any{"enabled": true}, "state": "started",
	})
	client.queueResponse("GET", "/addons/bbb/info", 200, map[string]any{
		"options": map[string]any{"dirsfirst": false}, "state": "startup", // never reaches "started"
	})
	// bbb: forward write succeeds; the inline self-revert attempted after
	// the restart+poll timeout is queued to fail too - the double fault.
	client.queueResponse("POST", "/addons/bbb/options", 200, map[string]any{})
	client.queueResponse("POST", "/addons/bbb/options", 500, map[string]any{})

	ops := []registries.RegOp{
		addonOp(addonopts.KindUpdate, "aaa", map[string]any{"enabled": true}),
		addonOp(addonopts.KindUpdate, "bbb", map[string]any{"dirsfirst": true}),
	}
	declared := map[string]bool{"aaa": false, "bbb": true}
	originals := map[string]map[string]any{}
	restartState := map[string]bool{}

	result := ApplyAddonPlan(context.Background(), client, ops, declared, originals, restartState, stashDir)

	if result.OK {
		t.Fatalf("result = %+v, want ok=false", result)
	}
	if result.RolledBack {
		t.Error("result.RolledBack = true, want false")
	}
	if len(result.Applied) != 1 || result.Applied[0] != "update addon:bbb" {
		t.Errorf("applied = %+v, want only [\"update addon:bbb\"] - aaa was genuinely reverted and must not be re-listed", result.Applied)
	}

	stash := readAddonStashFile(t, stashDir)
	if len(stash.Ops) != 1 || stash.Ops[0].Slug != "bbb" {
		t.Errorf("addon_stash.json ops = %+v, want only bbb - aaa was reverted, not stuck", stash.Ops)
	}

	// aaa must genuinely have been reverted: a second options POST to aaa
	// carrying its pre-op ("enabled": false) value.
	aaaCalls := client.callsFor("POST", "/addons/aaa/options")
	if len(aaaCalls) < 2 {
		t.Fatalf("aaa options calls = %+v, want at least 2 (forward + inverse)", aaaCalls)
	}
	restored, _ := aaaCalls[len(aaaCalls)-1].body["options"].(map[string]any)
	if restored["enabled"] != false {
		t.Errorf("aaa inverse options = %+v, want enabled restored to false", restored)
	}
	if _, stillManaged := originals["addon:aaa"]; stillManaged {
		t.Errorf("originals = %+v, want addon:aaa removed - the revert fully undid its first-time management", originals)
	}
}

// --- ApplyAddonPlan(): restore on unmanage ----------------------------------

func TestApplyAddonPlanRestoreSendsMergedOriginalsAndDropsMapping(t *testing.T) {
	setSupervisorToken(t)
	stashDir := t.TempDir()
	client := newFakeAddonHTTP()
	client.queueResponse("GET", "/addons/core_configurator/info", 200, map[string]any{
		"options": map[string]any{"dirsfirst": true, "untouched": "keepme"}, "state": "started",
	})
	ops := []registries.RegOp{addonOp(addonopts.KindRestore, "core_configurator", map[string]any{"dirsfirst": false})}
	originals := map[string]map[string]any{"addon:core_configurator": {"dirsfirst": false}}
	restartState := map[string]bool{"addon:core_configurator": true}

	result := ApplyAddonPlan(context.Background(), client, ops, map[string]bool{}, originals, restartState, stashDir)

	if !result.OK {
		t.Fatalf("result = %+v", result)
	}
	if len(originals) != 0 {
		t.Errorf("originals = %+v, want empty (dropped on restore)", originals)
	}
	if len(restartState) != 0 {
		t.Errorf("restart_on_change state = %+v, want empty (dropped on restore)", restartState)
	}
	calls := client.callsFor("POST", "/addons/core_configurator/options")
	sent, _ := calls[0].body["options"].(map[string]any)
	want := map[string]any{"dirsfirst": false, "untouched": "keepme"}
	if !reflect.DeepEqual(sent, want) {
		t.Errorf("sent options = %+v, want %+v", sent, want)
	}
	if len(client.callsFor("POST", "/addons/core_configurator/restart")) != 1 {
		t.Errorf("restart calls = %+v, want 1 (restart_on_change was true when managed)", client.callsFor("POST", "/addons/core_configurator/restart"))
	}
}

// --- ApplyAddonPlan(): not installed at execution time ----------------------

func TestApplyAddonPlanNotInstalledAtExecutionTimeFailsOp(t *testing.T) {
	setSupervisorToken(t)
	stashDir := t.TempDir()
	client := newFakeAddonHTTP()
	client.queueResponse("GET", "/addons/core_configurator/info", 404, nil)
	ops := []registries.RegOp{addonOp(addonopts.KindUpdate, "core_configurator", map[string]any{"dirsfirst": true})}

	result := ApplyAddonPlan(context.Background(), client, ops,
		map[string]bool{"core_configurator": true}, map[string]map[string]any{}, map[string]bool{}, stashDir)

	if result.OK {
		t.Fatalf("result = %+v, want ok=false", result)
	}
	if !strings.Contains(result.Error, "not installed") {
		t.Errorf("error = %q", result.Error)
	}
}

// --- ApplyAddonPlan(): error ops are skipped --------------------------------

func TestApplyAddonPlanSkipsErrorOps(t *testing.T) {
	setSupervisorToken(t)
	stashDir := t.TempDir()
	client := newFakeAddonHTTP()
	errOp := registries.RegOp{Kind: registries.KindError, RType: "addon", Key: "missing", Error: "add-on not installed"}

	result := ApplyAddonPlan(context.Background(), client, []registries.RegOp{errOp},
		map[string]bool{}, map[string]map[string]any{}, map[string]bool{}, stashDir)

	if !result.OK {
		t.Fatalf("result = %+v", result)
	}
	if len(result.SkippedErrors) != 1 || result.SkippedErrors[0].Key != "missing" {
		t.Errorf("skipped_errors = %+v", result.SkippedErrors)
	}
	if len(client.calls) != 0 {
		t.Errorf("calls = %+v, want none - the error op must never be executed", client.calls)
	}
}

// --- addon_stash.json shape --------------------------------------------------

func TestApplyAddonPlanWritesAddonStash(t *testing.T) {
	setSupervisorToken(t)
	stashDir := t.TempDir()
	client := newFakeAddonHTTP()
	client.queueResponse("GET", "/addons/core_configurator/info", 200, map[string]any{
		"options": map[string]any{"dirsfirst": false}, "state": "started",
	})
	ops := []registries.RegOp{addonOp(addonopts.KindUpdate, "core_configurator", map[string]any{"dirsfirst": true})}

	result := ApplyAddonPlan(context.Background(), client, ops,
		map[string]bool{"core_configurator": false}, map[string]map[string]any{}, map[string]bool{}, stashDir)
	if !result.OK {
		t.Fatalf("result = %+v", result)
	}

	stash := readAddonStashFile(t, stashDir)
	if len(stash.Ops) != 1 || stash.Ops[0].Slug != "core_configurator" || stash.Ops[0].Kind != addonopts.KindUpdate {
		t.Fatalf("stash = %+v", stash)
	}
}

func TestAddonStashExists(t *testing.T) {
	stashDir := t.TempDir()
	if AddonStashExists(stashDir) {
		t.Error("want false before any apply")
	}
	if err := writeAddonStash(stashDir, nil); err != nil {
		t.Fatal(err)
	}
	if !AddonStashExists(stashDir) {
		t.Error("want true after writeAddonStash")
	}
}

// --- RollbackAddonPlan ------------------------------------------------------

func TestRollbackAddonPlanRestoresOptionsAndBookkeeping(t *testing.T) {
	setSupervisorToken(t)
	stashDir := t.TempDir()
	applyClient := newFakeAddonHTTP()
	applyClient.queueResponse("GET", "/addons/core_configurator/info", 200, map[string]any{
		"options": map[string]any{"dirsfirst": false}, "state": "started",
	})
	ops := []registries.RegOp{addonOp(addonopts.KindUpdate, "core_configurator", map[string]any{"dirsfirst": true})}
	originals := map[string]map[string]any{}
	restartState := map[string]bool{}
	applyResult := ApplyAddonPlan(
		context.Background(), applyClient, ops, map[string]bool{"core_configurator": false}, originals, restartState, stashDir)
	if !applyResult.OK {
		t.Fatalf("apply result = %+v", applyResult)
	}

	rollbackClient := newFakeAddonHTTP()
	rollbackClient.queueResponse("GET", "/addons/core_configurator/info", 200, map[string]any{
		"options": map[string]any{"dirsfirst": true}, "state": "started",
	})
	result := RollbackAddonPlan(context.Background(), rollbackClient, stashDir, originals, restartState)

	if !result.OK || !result.RolledBack {
		t.Fatalf("rollback result = %+v", result)
	}
	if len(originals) != 0 {
		t.Errorf("originals = %+v, want empty (first management undone)", originals)
	}
	calls := rollbackClient.callsFor("POST", "/addons/core_configurator/options")
	sent, _ := calls[0].body["options"].(map[string]any)
	if sent["dirsfirst"] != false {
		t.Errorf("restored options = %+v, want dirsfirst back to false", sent)
	}
}

func TestRollbackAddonPlanMissingStashIsANoop(t *testing.T) {
	setSupervisorToken(t)
	stashDir := t.TempDir()
	result := RollbackAddonPlan(context.Background(), newFakeAddonHTTP(), stashDir, map[string]map[string]any{}, map[string]bool{})

	if !result.OK {
		t.Fatalf("result = %+v, want ok=true (nothing to roll back)", result)
	}
}

// --- VM e2e: restoring an option that was never set at all ------------------

// chronyLiveOptions is the real add-on's effective options: four keys the
// agent never declared, plus log_level, optional and unset before it did.
func chronyLiveOptions(logLevel any, withLogLevel bool) map[string]any {
	opts := map[string]any{
		"set_system_clock": true,
		"mode":             "server",
		"ntp_pool":         "pool.ntp.org",
		"ntp_server":       []any{"time.google.com"},
	}
	if withLogLevel {
		opts["log_level"] = logLevel
	}
	return opts
}

// assertSentOptions fails on any key want does not list: "log_level is not
// in here" is the point, and a subset comparison would pass regardless.
func assertSentOptions(t *testing.T, sent, want map[string]any) {
	t.Helper()
	for key, wantVal := range want {
		gotVal, present := sent[key]
		if !present {
			t.Errorf("sent options are missing %q; got %+v", key, sent)
			continue
		}
		if !reflect.DeepEqual(gotVal, wantVal) {
			t.Errorf("sent options[%q] = %#v, want %#v", key, gotVal, wantVal)
		}
	}
	for key := range sent {
		if _, wanted := want[key]; !wanted {
			t.Errorf("sent options carry an unwanted key %q = %#v; got %+v", key, sent[key], sent)
		}
	}
}

func TestApplyAddonPlanRestoreOfAnAbsentOriginalOmitsTheKey(t *testing.T) {
	// The live failure: {"log_level": null} was rejected 400 "Missing
	// required option" every cycle; leaving the key out was accepted.
	setSupervisorToken(t)
	stashDir := t.TempDir()
	client := newFakeAddonHTTP()
	client.queueResponse("GET", "/addons/a0d7b954_chrony/info", 200, map[string]any{
		"options": chronyLiveOptions("debug", true), "state": "started",
	})
	ops := []registries.RegOp{addonOp(addonopts.KindRestore, "a0d7b954_chrony",
		map[string]any{"log_level": addonopts.AbsentMarker()})}
	originals := map[string]map[string]any{
		"addon:a0d7b954_chrony": {"log_level": addonopts.AbsentMarker()},
	}
	restartState := map[string]bool{"addon:a0d7b954_chrony": true}

	result := ApplyAddonPlan(context.Background(), client, ops, map[string]bool{}, originals, restartState, stashDir)

	if !result.OK {
		t.Fatalf("result = %+v", result)
	}
	calls := client.callsFor("POST", "/addons/a0d7b954_chrony/options")
	if len(calls) != 1 {
		t.Fatalf("options calls = %d, want exactly 1", len(calls))
	}
	sent, _ := calls[0].body["options"].(map[string]any)
	if _, present := sent["log_level"]; present {
		t.Errorf("sent options = %+v, want log_level dropped entirely, never sent as null", sent)
	}
	assertSentOptions(t, sent, chronyLiveOptions(nil, false))
	if len(originals) != 0 {
		t.Errorf("originals = %+v, want empty (dropped on restore)", originals)
	}
	if len(client.callsFor("POST", "/addons/a0d7b954_chrony/restart")) != 1 {
		t.Errorf("restart calls = %d, want 1 (removing the key really changed the options)",
			len(client.callsFor("POST", "/addons/a0d7b954_chrony/restart")))
	}
}

func TestApplyAddonPlanRestoreOfAnAbsentOriginalAlreadyGoneWritesNothing(t *testing.T) {
	// The key is already missing from the live options, and the agent kept
	// sending the rejected null anyway. There is nothing left to write.
	setSupervisorToken(t)
	stashDir := t.TempDir()
	client := newFakeAddonHTTP()
	client.queueResponse("GET", "/addons/a0d7b954_chrony/info", 200, map[string]any{
		"options": chronyLiveOptions(nil, false), "state": "started",
	})
	ops := []registries.RegOp{addonOp(addonopts.KindRestore, "a0d7b954_chrony",
		map[string]any{"log_level": addonopts.AbsentMarker()})}
	originals := map[string]map[string]any{
		"addon:a0d7b954_chrony": {"log_level": addonopts.AbsentMarker()},
	}

	result := ApplyAddonPlan(context.Background(), client, ops, map[string]bool{}, originals,
		map[string]bool{"addon:a0d7b954_chrony": true}, stashDir)

	if !result.OK {
		t.Fatalf("result = %+v", result)
	}
	if calls := client.callsFor("POST", "/addons/a0d7b954_chrony/options"); len(calls) != 0 {
		t.Errorf("options calls = %+v, want none", calls)
	}
	if calls := client.callsFor("POST", "/addons/a0d7b954_chrony/restart"); len(calls) != 0 {
		t.Errorf("restart calls = %+v, want none", calls)
	}
}

func TestApplyAddonPlanRestoreOfAnExplicitNullOriginalStillSendsNull(t *testing.T) {
	// The other side of the marker's distinction: an original recorded as
	// a real null is a value, restored by writing it, not dropping the key.
	setSupervisorToken(t)
	stashDir := t.TempDir()
	client := newFakeAddonHTTP()
	client.queueResponse("GET", "/addons/a0d7b954_chrony/info", 200, map[string]any{
		"options": chronyLiveOptions("debug", true), "state": "started",
	})
	ops := []registries.RegOp{addonOp(addonopts.KindRestore, "a0d7b954_chrony", map[string]any{"log_level": nil})}
	originals := map[string]map[string]any{"addon:a0d7b954_chrony": {"log_level": nil}}

	result := ApplyAddonPlan(context.Background(), client, ops, map[string]bool{}, originals,
		map[string]bool{"addon:a0d7b954_chrony": false}, stashDir)

	if !result.OK {
		t.Fatalf("result = %+v", result)
	}
	calls := client.callsFor("POST", "/addons/a0d7b954_chrony/options")
	if len(calls) != 1 {
		t.Fatalf("options calls = %d, want exactly 1", len(calls))
	}
	sent, _ := calls[0].body["options"].(map[string]any)
	value, present := sent["log_level"]
	if !present || value != nil {
		t.Errorf("sent options = %+v, want log_level present and null", sent)
	}
	assertSentOptions(t, sent, chronyLiveOptions(nil, true))
}

func TestApplyAddonPlanRecordsAnAbsentOriginalForAKeyTheAddonNeverHad(t *testing.T) {
	// Managing a key with no live value records that fact as a value of
	// its own, or a later restore cannot tell it from a null.
	setSupervisorToken(t)
	stashDir := t.TempDir()
	client := newFakeAddonHTTP()
	client.queueResponse("GET", "/addons/a0d7b954_chrony/info", 200, map[string]any{
		"options": chronyLiveOptions(nil, false), "state": "started",
	})
	ops := []registries.RegOp{addonOp(addonopts.KindUpdate, "a0d7b954_chrony", map[string]any{"log_level": "debug"})}
	originals := map[string]map[string]any{}

	result := ApplyAddonPlan(context.Background(), client, ops,
		map[string]bool{"a0d7b954_chrony": true}, originals, map[string]bool{}, stashDir)

	if !result.OK {
		t.Fatalf("result = %+v", result)
	}
	recorded := originals["addon:a0d7b954_chrony"]
	if !addonopts.IsAbsent(recorded["log_level"]) {
		t.Errorf("recorded originals = %+v, want log_level recorded as absent, not as null", recorded)
	}
	if len(recorded) != 1 {
		t.Errorf("recorded originals = %+v, want only the one declared key", recorded)
	}

	stash := readAddonStashFile(t, stashDir)
	if len(stash.Ops) != 1 {
		t.Fatalf("stash = %+v, want one entry", stash.Ops)
	}
	if !addonopts.IsAbsent(stash.Ops[0].PriorOptions["log_level"]) {
		t.Errorf("stashed prior_options = %+v, want log_level still recognizable as absent after the JSON round trip",
			stash.Ops[0].PriorOptions)
	}
}

func TestRollbackAddonPlanUnsetsAKeyThatHadNoOriginalValue(t *testing.T) {
	// Rolling back the first management of a key that had no value has to
	// take the key back out again, for the same reason a restore does.
	setSupervisorToken(t)
	stashDir := t.TempDir()
	applyClient := newFakeAddonHTTP()
	applyClient.queueResponse("GET", "/addons/a0d7b954_chrony/info", 200, map[string]any{
		"options": chronyLiveOptions(nil, false), "state": "started",
	})
	ops := []registries.RegOp{addonOp(addonopts.KindUpdate, "a0d7b954_chrony", map[string]any{"log_level": "debug"})}
	originals := map[string]map[string]any{}
	restartState := map[string]bool{}
	if applyResult := ApplyAddonPlan(context.Background(), applyClient, ops,
		map[string]bool{"a0d7b954_chrony": false}, originals, restartState, stashDir); !applyResult.OK {
		t.Fatalf("apply result = %+v", applyResult)
	}

	rollbackClient := newFakeAddonHTTP()
	rollbackClient.queueResponse("GET", "/addons/a0d7b954_chrony/info", 200, map[string]any{
		"options": chronyLiveOptions("debug", true), "state": "started",
	})
	result := RollbackAddonPlan(context.Background(), rollbackClient, stashDir, originals, restartState)

	if !result.OK || !result.RolledBack {
		t.Fatalf("rollback result = %+v", result)
	}
	calls := rollbackClient.callsFor("POST", "/addons/a0d7b954_chrony/options")
	if len(calls) != 1 {
		t.Fatalf("options calls = %d, want exactly 1", len(calls))
	}
	sent, _ := calls[0].body["options"].(map[string]any)
	if _, present := sent["log_level"]; present {
		t.Errorf("restored options = %+v, want log_level dropped entirely, never sent as null", sent)
	}
	assertSentOptions(t, sent, chronyLiveOptions(nil, false))
	if len(originals) != 0 {
		t.Errorf("originals = %+v, want empty (first management undone)", originals)
	}
}

// --- Supervisor's own rejection reason reaches the error --------------------

func TestApplyAddonPlanSurfacesSupervisorsRejectionMessage(t *testing.T) {
	// A bare "options returned HTTP 400" is unactionable; Supervisor says
	// what was wrong in the body, which the fake echoes as "boom".
	setSupervisorToken(t)
	stashDir := t.TempDir()
	client := newFakeAddonHTTP()
	client.queueResponse("GET", "/addons/core_configurator/info", 200, map[string]any{
		"options": map[string]any{"dirsfirst": false}, "state": "started",
	})
	client.queueResponse("POST", "/addons/core_configurator/options", 400, nil)
	ops := []registries.RegOp{addonOp(addonopts.KindUpdate, "core_configurator", map[string]any{"dirsfirst": true})}

	result := ApplyAddonPlan(context.Background(), client, ops,
		map[string]bool{"core_configurator": true}, map[string]map[string]any{}, map[string]bool{}, stashDir)

	if result.OK {
		t.Fatalf("result = %+v, want ok=false", result)
	}
	if !strings.Contains(result.Error, "options returned HTTP 400") {
		t.Errorf("error = %q, want the status", result.Error)
	}
	if !strings.Contains(result.Error, "boom") {
		t.Errorf("error = %q, want supervisor's own message alongside the status", result.Error)
	}
}

func TestFetchAddonInfoAllSurfacesSupervisorsRejectionMessage(t *testing.T) {
	setSupervisorToken(t)
	client := newFakeAddonHTTP()
	client.queueResponse("GET", "/addons/core_configurator/info", 500, nil)

	_, err := FetchAddonInfoAll(context.Background(), client, []string{"core_configurator"})

	if err == nil {
		t.Fatal("err = nil, want a failure")
	}
	if !strings.Contains(err.Error(), "info returned HTTP 500") || !strings.Contains(err.Error(), "boom") {
		t.Errorf("err = %q, want the status and supervisor's own message", err)
	}
}

// --- Secret references: what reaches addon_stash.json, and what does not --

// addonResolvedSecret is what "secret://mqtt_password" has resolved to by
// the time an op arrives; the "appears nowhere" assertions hunt for it.
const addonResolvedSecret = "S3CRET-resolved"

// secretAddonOp is an update op as addonopts.Plan builds it for a
// reference: Params resolved, Declared the reference, Secrets the value.
func secretAddonOp(params map[string]any) registries.RegOp {
	op := addonOp(addonopts.KindUpdate, "core_mqtt", params)
	op.Declared = map[string]any{"password": "secret://mqtt_password"}
	op.Secrets = []string{addonResolvedSecret}
	return op
}

// The forward value is the resolved credential, and addon_stash.json sits
// under /data/backup for five applies, inside any Supervisor backup.
func TestApplyAddonPlanKeepsTheResolvedSecretOutOfTheStash(t *testing.T) {
	setSupervisorToken(t)
	stashDir := t.TempDir()
	client := newFakeAddonHTTP()
	client.queueResponse("GET", "/addons/core_mqtt/info", 200, map[string]any{
		"options": map[string]any{"password": "the-users-own-old-password"}, "state": "started",
	})

	ops := []registries.RegOp{secretAddonOp(map[string]any{"password": addonResolvedSecret})}
	originals := map[string]map[string]any{}
	result := ApplyAddonPlan(context.Background(), client, ops,
		map[string]bool{"core_mqtt": false}, originals, map[string]bool{}, stashDir)
	if !result.OK {
		t.Fatalf("result = %+v", result)
	}

	// Supervisor got the real credential.
	posts := client.callsFor("POST", "/addons/core_mqtt/options")
	if len(posts) != 1 {
		t.Fatalf("options POSTs = %+v, want 1", posts)
	}
	sent, _ := posts[0].body["options"].(map[string]any)
	if sent["password"] != addonResolvedSecret {
		t.Errorf("posted options = %+v, want the resolved value", sent)
	}

	// The stash file did not.
	raw, err := os.ReadFile(filepath.Join(stashDir, "addon_stash.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), addonResolvedSecret) {
		t.Errorf("addon_stash.json carries the resolved secret:\n%s", raw)
	}
	if !strings.Contains(string(raw), "secret://mqtt_password") {
		t.Errorf("addon_stash.json does not carry the reference either, so nothing marks the key:\n%s", raw)
	}

	stash := readAddonStashFile(t, stashDir)
	if stash.Ops[0].ForwardOptions["password"] != "secret://mqtt_password" {
		t.Errorf("stash forward = %+v, want the reference", stash.Ops[0].ForwardOptions)
	}
	// First management, so the prior value is the user's own - recorded in
	// the clear in state.AddonOriginals, which a faithful rollback needs.
	if stash.Ops[0].PriorOptions["password"] != "the-users-own-old-password" {
		t.Errorf("stash prior = %+v, want the pre-management value kept", stash.Ops[0].PriorOptions)
	}
}

// The rotation case: the key is already managed, so its prior LIVE value
// is the credential this agent wrote last time, and it lives nowhere else.
func TestApplyAddonPlanKeepsAnAlreadyManagedSecretOutOfThePriorOptions(t *testing.T) {
	setSupervisorToken(t)
	stashDir := t.TempDir()
	client := newFakeAddonHTTP()
	client.queueResponse("GET", "/addons/core_mqtt/info", 200, map[string]any{
		"options": map[string]any{"password": "PREVIOUS-resolved-secret"}, "state": "started",
	})

	ops := []registries.RegOp{secretAddonOp(map[string]any{"password": addonResolvedSecret})}
	originals := map[string]map[string]any{"addon:core_mqtt": {"password": "the-users-own-old-password"}}
	result := ApplyAddonPlan(context.Background(), client, ops,
		map[string]bool{"core_mqtt": false}, originals, map[string]bool{"addon:core_mqtt": false}, stashDir)
	if !result.OK {
		t.Fatalf("result = %+v", result)
	}

	raw, err := os.ReadFile(filepath.Join(stashDir, "addon_stash.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{addonResolvedSecret, "PREVIOUS-resolved-secret"} {
		if strings.Contains(string(raw), forbidden) {
			t.Errorf("addon_stash.json carries %q:\n%s", forbidden, raw)
		}
	}
	stash := readAddonStashFile(t, stashDir)
	if stash.Ops[0].PriorOptions["password"] != "secret://mqtt_password" {
		t.Errorf("stash prior = %+v, want the reference where the old credential would have gone", stash.Ops[0].PriorOptions)
	}
}

// What that costs: a rollback puts every ordinary key back and leaves the
// referenced one where the apply put it.
func TestRollbackAddonPlanLeavesAReferencedKeyAloneAndRestoresTheRest(t *testing.T) {
	setSupervisorToken(t)
	stashDir := t.TempDir()
	client := newFakeAddonHTTP()
	client.queueResponse("GET", "/addons/core_mqtt/info", 200, map[string]any{
		"options": map[string]any{"password": addonResolvedSecret, "logins": []any{"new"}}, "state": "started",
	})

	if err := writeAddonStash(stashDir, []addonStashEntry{{
		Kind: addonopts.KindUpdate, Slug: "core_mqtt",
		PriorOptions:   map[string]any{"password": "secret://mqtt_password", "logins": []any{"old"}},
		ForwardOptions: map[string]any{"password": "secret://mqtt_password", "logins": []any{"new"}},
	}}); err != nil {
		t.Fatal(err)
	}

	result := RollbackAddonPlan(context.Background(), client, stashDir,
		map[string]map[string]any{}, map[string]bool{})
	if !result.OK {
		t.Fatalf("result = %+v", result)
	}

	posts := client.callsFor("POST", "/addons/core_mqtt/options")
	if len(posts) != 1 {
		t.Fatalf("options POSTs = %+v, want 1", posts)
	}
	sent, _ := posts[0].body["options"].(map[string]any)
	if !reflect.DeepEqual(sent["logins"], []any{"old"}) {
		t.Errorf("posted options = %+v, want the ordinary key restored", sent)
	}
	// Left where the apply put it - never overwritten with the reference
	// text, and never re-resolved into whatever secrets.yaml says now.
	if sent["password"] != addonResolvedSecret {
		t.Errorf("posted options = %+v, want the referenced key untouched", sent)
	}
}

// httperr quotes Supervisor's rejection body verbatim, and the two undo
// paths reach the feed, /data/history.jsonl and the log without the op.
func TestRollbackAddonPlanScrubsAResolvedSecretOutOfAFailure(t *testing.T) {
	setSupervisorToken(t)
	stashDir := t.TempDir()
	client := newFakeAddonHTTP()
	client.queueResponse("GET", "/addons/core_mqtt/info", 200, map[string]any{
		"options": map[string]any{"password": addonResolvedSecret, "logins": []any{"new"}}, "state": "started",
	})
	client.queueResponse("POST", "/addons/core_mqtt/options", 400, map[string]any{
		"message": "Invalid option: password=" + addonResolvedSecret,
	})

	if err := writeAddonStash(stashDir, []addonStashEntry{{
		Kind: addonopts.KindUpdate, Slug: "core_mqtt",
		PriorOptions:   map[string]any{"password": "secret://mqtt_password", "logins": []any{"old"}},
		ForwardOptions: map[string]any{"password": "secret://mqtt_password", "logins": []any{"new"}},
	}}); err != nil {
		t.Fatal(err)
	}

	result := RollbackAddonPlan(context.Background(), client, stashDir,
		map[string]map[string]any{}, map[string]bool{})
	if result.OK {
		t.Fatalf("result = %+v, want the rejection reported", result)
	}
	if strings.Contains(result.Error, addonResolvedSecret) {
		t.Errorf("rollback error carries the resolved secret: %q", result.Error)
	}
	if !strings.Contains(result.Error, "***REDACTED***") {
		t.Errorf("rollback error = %q, want the marker where the value was", result.Error)
	}
}

// The other undo path: a failing op inverts its earlier siblings, and that
// inverse's failure joins the forward error past where op.Secrets applies.
func TestApplyAddonPlanScrubsASecretOutOfAnIncompleteRollbackNote(t *testing.T) {
	setSupervisorToken(t)
	stashDir := t.TempDir()
	client := newFakeAddonHTTP()
	// First op: succeeds against the referenced key.
	client.queueResponse("GET", "/addons/core_mqtt/info", 200, map[string]any{
		"options": map[string]any{"password": "the-users-own-old-password"}, "state": "started",
	})
	client.queueResponse("GET", "/addons/core_mqtt/info", 200, map[string]any{
		"options": map[string]any{"password": addonResolvedSecret}, "state": "started",
	})
	// Second op: fails, which triggers the inverse replay of the first.
	client.queueResponse("GET", "/addons/core_other/info", 500, nil)
	// The forward write lands; the INVERSE's write is what fails, quoting
	// the live credential back.
	client.queueResponse("POST", "/addons/core_mqtt/options", 200, nil)
	client.queueResponse("POST", "/addons/core_mqtt/options", 400, map[string]any{
		"message": "Invalid option: password=" + addonResolvedSecret,
	})

	ops := []registries.RegOp{
		secretAddonOp(map[string]any{"password": addonResolvedSecret}),
		addonOp(addonopts.KindUpdate, "core_other", map[string]any{"x": 1}),
	}
	result := ApplyAddonPlan(context.Background(), client, ops,
		map[string]bool{"core_mqtt": false, "core_other": false},
		map[string]map[string]any{}, map[string]bool{}, stashDir)

	if result.OK {
		t.Fatalf("result = %+v, want the second op's failure reported", result)
	}
	if !strings.Contains(result.Error, "rollback also incomplete") {
		t.Fatalf("result error = %q, want the inverse failure folded in - otherwise this proves nothing", result.Error)
	}
	if strings.Contains(result.Error, addonResolvedSecret) {
		t.Errorf("apply error carries the resolved secret through the rollback note: %q", result.Error)
	}
}
