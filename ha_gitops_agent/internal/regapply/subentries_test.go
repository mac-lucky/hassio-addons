package regapply

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/registries"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/subentries"
)

// subFlow is the REST path subentry flows are driven over, as
// fakeIntegrationHTTP keys it (coreAPI's /core/api prefix included).
const subFlow = "/core/api/config/config_entries/subentries/flow"

func subentryOp(kind, key string, params map[string]any, liveID string) registries.RegOp {
	if params == nil {
		params = map[string]any{}
	}
	return registries.RegOp{Kind: kind, RType: "subentry", Key: key, Params: params, LiveID: liveID, DiffText: "..."}
}

// field builds one serialized data_schema entry.
func field(name string, extra map[string]any) map[string]any {
	out := map[string]any{"name": name}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

// section builds one serialized form section (an "expandable" field).
func section(name string, required bool, inner ...any) map[string]any {
	return map[string]any{"name": name, "type": "expandable", "required": required, "schema": inner}
}

// --- schemaDefaults ------------------------------------------------------

func TestSchemaDefaultsPrefersSuggestedValueOverDefault(t *testing.T) {
	got := schemaDefaults([]any{
		field("host", map[string]any{
			"default":     "old.example",
			"description": map[string]any{"suggested_value": "live.example"},
		}),
		field("port", map[string]any{"default": float64(8080)}),
		field("nothing", nil),
		field("explicit_null", map[string]any{
			"default":     nil,
			"description": map[string]any{"suggested_value": nil},
		}),
		"not an object",
		field("", map[string]any{"default": "unnamed"}),
	})

	want := map[string]any{"host": "live.example", "port": float64(8080)}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("defaults = %#v, want %#v", got, want)
	}
}

func TestSchemaDefaultsKeepsColorsInTheShapeTheFormSuggests(t *testing.T) {
	// The form suggests a colour as the [R, G, B] its selector accepts even
	// though storage holds hex; resubmitting the suggestion round-trips.
	got := schemaDefaults([]any{
		field("color", map[string]any{
			"description": map[string]any{"suggested_value": []any{float64(255), float64(160), float64(0)}},
		}),
	})

	want := map[string]any{"color": []any{float64(255), float64(160), float64(0)}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("defaults = %#v, want %#v", got, want)
	}
}

func TestSchemaDefaultsNestsSectionsAndDropsEmptyOnes(t *testing.T) {
	got := schemaDefaults([]any{
		section("advanced", false,
			field("retries", map[string]any{"default": float64(3)}),
			field("verbose", map[string]any{"description": map[string]any{"suggested_value": true}}),
		),
		section("empty", false, field("nothing", nil)),
		map[string]any{"name": "bad_schema", "type": "expandable", "schema": "not a list"},
	})

	want := map[string]any{"advanced": map[string]any{"retries": float64(3), "verbose": true}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("defaults = %#v, want %#v", got, want)
	}
}

// --- buildStepSubmission -------------------------------------------------

func TestBuildStepSubmissionMergesSectionsOneLevelAndReplacesEverythingElse(t *testing.T) {
	schema := []any{
		field("host", map[string]any{"default": "live.example"}),
		field("modes", map[string]any{"default": []any{"a", "b", "c"}}),
		section("advanced", false,
			field("retries", map[string]any{"default": float64(3)}),
			field("verbose", map[string]any{"default": false}),
		),
	}
	declared := map[string]any{
		"modes":    []any{"z"},
		"advanced": map[string]any{"verbose": true},
	}

	got, err := buildStepSubmission(schema, declared)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := map[string]any{
		"host":     "live.example",
		"modes":    []any{"z"},
		"advanced": map[string]any{"retries": float64(3), "verbose": true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("submission = %#v, want %#v", got, want)
	}
	if declared["advanced"].(map[string]any)["retries"] != nil {
		t.Fatal("the declared section map was mutated by the merge")
	}
}

func TestBuildStepSubmissionNamesAMissingRequiredFieldAndListsTheSchema(t *testing.T) {
	schema := []any{
		field("host", map[string]any{"required": true, "selector": map[string]any{"text": map[string]any{}}}),
		field("port", map[string]any{"required": true, "default": float64(80)}),
	}

	_, err := buildStepSubmission(schema, nil)
	if err == nil {
		t.Fatal("expected an error for the required field with no default")
	}
	if !strings.Contains(err.Error(), "'host'") {
		t.Fatalf("error does not name the missing field: %v", err)
	}
	if !strings.Contains(err.Error(), "the step accepts: host (required, text)") {
		t.Fatalf("error does not list the step's schema: %v", err)
	}
}

func TestBuildStepSubmissionNamesAMissingRequiredFieldInsideASection(t *testing.T) {
	schema := []any{
		section("advanced", false,
			field("retries", map[string]any{"default": float64(3)}),
			field("api_key", map[string]any{"required": true}),
		),
	}

	if _, err := buildStepSubmission(schema, nil); err == nil {
		t.Fatal("expected an error for the required field inside the section")
	} else if !strings.Contains(err.Error(), "'advanced.api_key'") {
		t.Fatalf("error does not name the section field: %v", err)
	}
}

// --- driveSubentryFlow ---------------------------------------------------

func TestDriveSubentryFlowCreateSendsTheHandlerPairAndSubmitsDefaults(t *testing.T) {
	client := newFakeIntegrationHTTP()
	client.queueResponse("POST", subFlow, 200, map[string]any{
		"type": "form", "flow_id": "f1", "step_id": "user",
		"data_schema": []any{
			field("calendar_id", map[string]any{"required": true}),
			field("track_all", map[string]any{"default": true}),
		},
	})
	client.queueResponse("POST", subFlow+"/f1", 200, map[string]any{"type": "create_entry"})

	err := driveSubentryFlow(context.Background(), client, "tok", "e1", "calendar", "",
		map[string]any{"user": map[string]any{"calendar_id": "fam@group"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	start := client.callsFor("POST", subFlow)[0]
	if !reflect.DeepEqual(start.body["handler"], []any{"e1", "calendar"}) {
		t.Fatalf("handler = %#v, want the [entry_id, subentry_type] pair", start.body["handler"])
	}
	if _, present := start.body["subentry_id"]; present {
		t.Fatal("a create must not send subentry_id - its presence is what selects a reconfigure")
	}

	submitted := client.callsFor("POST", subFlow+"/f1")[0].body
	want := map[string]any{"calendar_id": "fam@group", "track_all": true}
	if !reflect.DeepEqual(submitted, want) {
		t.Fatalf("submitted = %#v, want %#v", submitted, want)
	}
}

func TestDriveSubentryFlowReconfigureSendsSubentryIDAndTreatsItsAbortAsSuccess(t *testing.T) {
	client := newFakeIntegrationHTTP()
	client.queueResponse("POST", subFlow, 200, map[string]any{
		"type": "form", "flow_id": "f1", "step_id": "reconfigure",
		"data_schema": []any{field("calendar_id", map[string]any{"required": true})},
	})
	client.queueResponse("POST", subFlow+"/f1", 200, map[string]any{
		"type": "abort", "reason": "reconfigure_successful",
	})

	err := driveSubentryFlow(context.Background(), client, "tok", "e1", "calendar", "sub1",
		map[string]any{"reconfigure": map[string]any{"calendar_id": "fam@group"}})
	if err != nil {
		t.Fatalf("reconfigure_successful must be success, got: %v", err)
	}
	if got := client.callsFor("POST", subFlow)[0].body["subentry_id"]; got != "sub1" {
		t.Fatalf("subentry_id = %v, want sub1", got)
	}
}

func TestDriveSubentryFlowFailsOnAnyOtherAbortReason(t *testing.T) {
	client := newFakeIntegrationHTTP()
	client.queueResponse("POST", subFlow, 200, map[string]any{"type": "abort", "reason": "already_configured"})

	err := driveSubentryFlow(context.Background(), client, "tok", "e1", "calendar", "sub1", nil)
	if err == nil || !strings.Contains(err.Error(), "already_configured") {
		t.Fatalf("error = %v, want the abort reason reported as a failure", err)
	}
}

func TestDriveSubentryFlowFailsWhenAReconfigureCreatesInstead(t *testing.T) {
	client := newFakeIntegrationHTTP()
	client.queueResponse("POST", subFlow, 200, map[string]any{"type": "create_entry"})

	err := driveSubentryFlow(context.Background(), client, "tok", "e1", "calendar", "sub1", nil)
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("error = %v, want a create_entry on a reconfigure reported as a possible duplicate", err)
	}
}

func TestDriveSubentryFlowAliasesTheTwoStepOneNames(t *testing.T) {
	cases := []struct {
		name     string
		liveStep string
		data     map[string]any
		want     string
	}{
		{"user data answers a reconfigure step", "reconfigure", map[string]any{
			"user": map[string]any{"calendar_id": "from_user"},
		}, "from_user"},
		{"reconfigure data answers a user step", "user", map[string]any{
			"reconfigure": map[string]any{"calendar_id": "from_reconfigure"},
		}, "from_reconfigure"},
		{"the exact step id wins over the alias", "reconfigure", map[string]any{
			"user":        map[string]any{"calendar_id": "from_user"},
			"reconfigure": map[string]any{"calendar_id": "from_reconfigure"},
		}, "from_reconfigure"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := newFakeIntegrationHTTP()
			client.queueResponse("POST", subFlow, 200, map[string]any{
				"type": "form", "flow_id": "f1", "step_id": tc.liveStep,
				"data_schema": []any{field("calendar_id", map[string]any{"required": true})},
			})
			client.queueResponse("POST", subFlow+"/f1", 200, map[string]any{"type": "create_entry"})

			if err := driveSubentryFlow(
				context.Background(), client, "tok", "e1", "calendar", "", tc.data); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got := client.callsFor("POST", subFlow+"/f1")[0].body["calendar_id"]; got != tc.want {
				t.Fatalf("calendar_id = %v, want %s", got, tc.want)
			}
		})
	}
}

func TestDriveSubentryFlowAbortsTheFlowWhenAStepCannotBeAnswered(t *testing.T) {
	client := newFakeIntegrationHTTP()
	client.queueResponse("POST", subFlow, 200, map[string]any{
		"type": "form", "flow_id": "f1", "step_id": "user",
		"data_schema": []any{field("calendar_id", map[string]any{"required": true})},
	})

	err := driveSubentryFlow(context.Background(), client, "tok", "e1", "calendar", "", nil)
	if err == nil || !strings.Contains(err.Error(), "'calendar_id'") {
		t.Fatalf("error = %v, want the unanswerable field named", err)
	}
	if len(client.callsFor("DELETE", subFlow+"/f1")) != 1 {
		t.Fatal("the started flow was left open instead of being aborted")
	}
}

func TestDriveSubentryFlowStopsAtMaxFlowSteps(t *testing.T) {
	client := newFakeIntegrationHTTP()
	endlessForm := map[string]any{"type": "form", "flow_id": "f1", "step_id": "user", "data_schema": []any{}}
	client.queueResponse("POST", subFlow, 200, endlessForm)
	client.queueResponse("POST", subFlow+"/f1", 200, endlessForm)

	err := driveSubentryFlow(context.Background(), client, "tok", "e1", "calendar", "", nil)
	if err == nil || !strings.Contains(err.Error(), "exceeded 5 steps") {
		t.Fatalf("error = %v, want the step bound reported", err)
	}
	if got := len(client.callsFor("POST", subFlow+"/f1")); got != maxFlowSteps {
		t.Fatalf("advanced %d times, want %d", got, maxFlowSteps)
	}
	if len(client.callsFor("DELETE", subFlow+"/f1")) != 1 {
		t.Fatal("the runaway flow was left open instead of being aborted")
	}
}

func TestDriveSubentryFlowRejectsStepTypesItCannotDrive(t *testing.T) {
	client := newFakeIntegrationHTTP()
	client.queueResponse("POST", subFlow, 200, map[string]any{
		"type": "menu", "flow_id": "f1", "step_id": "pick",
	})

	err := driveSubentryFlow(context.Background(), client, "tok", "e1", "calendar", "", nil)
	if err == nil || !strings.Contains(err.Error(), `"menu"`) {
		t.Fatalf("error = %v, want the unsupported step type reported", err)
	}
	if len(client.callsFor("DELETE", subFlow+"/f1")) != 1 {
		t.Fatal("the unsupported flow was left open instead of being aborted")
	}
}

func TestDriveSubentryFlowFailsWhenHomeAssistantRejectsTheStep(t *testing.T) {
	client := newFakeIntegrationHTTP()
	client.queueResponse("POST", subFlow, 200, map[string]any{
		"type": "form", "flow_id": "f1", "step_id": "user", "data_schema": []any{},
		"errors": map[string]any{"calendar_id": "invalid_calendar"},
	})

	err := driveSubentryFlow(context.Background(), client, "tok", "e1", "calendar", "", nil)
	if err == nil || !strings.Contains(err.Error(), "invalid_calendar") {
		t.Fatalf("error = %v, want home assistant's own rejection reported", err)
	}
}

// --- FetchSubentries -----------------------------------------------------

// liveSub builds one live subentry object as
// config_entries/subentries/list returns it.
func liveSub(id, subentryType, title, uniqueID string) map[string]any {
	return map[string]any{"subentry_id": id, "subentry_type": subentryType, "title": title, "unique_id": uniqueID}
}

func wsList(subs ...map[string]any) []any {
	out := make([]any, len(subs))
	for i, s := range subs {
		out[i] = s
	}
	return out
}

func TestFetchSubentriesListsEachEntryOverOneConnection(t *testing.T) {
	ws := newFakeWS()
	ws.results[msgSubentriesList] = []any{
		wsList(liveSub("s1", "calendar", "Family", "fam@group")),
		wsList(),
	}

	got, err := FetchSubentries(context.Background(), staticDialer(ws), []string{"e1", "e2"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got["e1"]) != 1 || len(got["e2"]) != 0 {
		t.Fatalf("subentries = %#v, want one under e1 and none under e2", got)
	}
	calls := ws.callsFor(msgSubentriesList)
	if len(calls) != 2 || calls[0].params["entry_id"] != "e1" || calls[1].params["entry_id"] != "e2" {
		t.Fatalf("calls = %#v, want one list per entry id", calls)
	}
	if !ws.closed {
		t.Fatal("the connection was not closed")
	}
}

func TestFetchSubentriesDialsNothingForAnEmptyList(t *testing.T) {
	got, err := FetchSubentries(context.Background(), nil, nil)
	if err != nil || len(got) != 0 {
		t.Fatalf("got (%#v, %v), want an empty map and no error", got, err)
	}
}

// --- ApplySubentryPlan ---------------------------------------------------

// queueCreateFlow scripts a one-step create flow that completes.
func queueCreateFlow(client *fakeIntegrationHTTP) {
	client.queueResponse("POST", subFlow, 200, map[string]any{
		"type": "form", "flow_id": "f1", "step_id": "user", "data_schema": []any{},
	})
	client.queueResponse("POST", subFlow+"/f1", 200, map[string]any{"type": "create_entry"})
}

func createOp(key string, params map[string]any) registries.RegOp {
	base := map[string]any{"entry_id": "e1", "subentry_type": "calendar", "data": map[string]any{}}
	for k, v := range params {
		base[k] = v
	}
	return subentryOp(subentries.KindCreate, key, base, "")
}

func TestApplySubentryPlanCreateDiscoversTheNewSubentryID(t *testing.T) {
	cases := []struct {
		name   string
		params map[string]any
		after  []any
		want   string
	}{
		{
			"by declared unique_id",
			map[string]any{"match_unique_id": "fam@group"},
			wsList(liveSub("s1", "calendar", "Other", "other@group"), liveSub("s2", "calendar", "Family", "fam@group")), "s2",
		},
		{
			"by declared title",
			map[string]any{"match_title": "Family"},
			wsList(liveSub("s1", "calendar", "Other", ""), liveSub("s2", "calendar", "Family", "")), "s2",
		},
		{"by being the only new one", nil, wsList(liveSub("s9", "calendar", "Whatever", "")), "s9"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setSupervisorToken(t)
			client := newFakeIntegrationHTTP()
			queueCreateFlow(client)
			ws := newFakeWS()
			ws.results[msgSubentriesList] = []any{wsList(), tc.after}

			managed, hashes := map[string]string{}, map[string]string{}
			// Pre-seeded so the attempts clear is covered on create too.
			attempts := map[string]map[string]any{
				"subentry:cal": {"hash": subentries.HashData(map[string]any{}), "error": "an earlier create failed"},
			}
			result := ApplySubentryPlan(context.Background(), client, staticDialer(ws),
				[]registries.RegOp{createOp("cal", tc.params)}, managed, hashes, attempts)

			if !result.OK {
				t.Fatalf("apply failed: %s", result.Error)
			}
			if _, stale := attempts["subentry:cal"]; stale {
				t.Errorf("attempts = %#v, want the entry cleared after a successful create", attempts)
			}
			if managed["subentry:cal"] != tc.want {
				t.Fatalf("managed = %#v, want subentry:cal -> %s", managed, tc.want)
			}
			if hashes["subentry:cal"] != subentries.HashData(map[string]any{}) {
				t.Fatalf("hashes = %#v, want the applied data's hash", hashes)
			}
		})
	}
}

func TestApplySubentryPlanFailsACreateItCannotTrack(t *testing.T) {
	setSupervisorToken(t)
	client := newFakeIntegrationHTTP()
	queueCreateFlow(client)
	ws := newFakeWS()
	// Two indistinguishable new subentries, nothing declared to tell apart.
	ws.results[msgSubentriesList] = []any{
		wsList(),
		wsList(liveSub("s1", "calendar", "A", ""), liveSub("s2", "calendar", "B", "")),
	}

	managed, hashes := map[string]string{}, map[string]string{}
	attempts := map[string]map[string]any{}
	result := ApplySubentryPlan(context.Background(), client, staticDialer(ws),
		[]registries.RegOp{createOp("cal", nil)}, managed, hashes, attempts)

	if result.OK || !strings.Contains(result.Error, "could not be identified") {
		t.Fatalf("result = %#v, want an untrackable create reported as a failure", result)
	}
	if len(managed) != 0 || len(hashes) != 0 {
		t.Fatalf("managed=%#v hashes=%#v, want nothing recorded for an untrackable create", managed, hashes)
	}
	if attempts["subentry:cal"]["hash"] != subentries.HashData(map[string]any{}) {
		t.Fatalf("attempts = %#v, want the failed data's hash remembered", attempts)
	}
}

func TestApplySubentryPlanCreateDoesNotRememberAFailedListAsABadDeclaration(t *testing.T) {
	setSupervisorToken(t)
	client := newFakeIntegrationHTTP()
	ws := newFakeWS()
	ws.raiseOn[msgSubentriesList] = []error{errors.New("connection reset")}

	attempts := map[string]map[string]any{}
	result := ApplySubentryPlan(context.Background(), client, staticDialer(ws),
		[]registries.RegOp{createOp("cal", nil)}, map[string]string{}, map[string]string{}, attempts)

	if result.OK {
		t.Fatal("expected the read failure to fail the op")
	}
	if len(attempts) != 0 {
		t.Fatalf("attempts = %#v, want a transient read failure not to block the next retry", attempts)
	}
	if len(client.calls) != 0 {
		t.Fatal("no flow should have been started after the pre-create listing failed")
	}
}

// queueReconfigureFlow scripts a one-step reconfigure flow that ends the
// way core's async_update_and_abort does.
func queueReconfigureFlow(client *fakeIntegrationHTTP) {
	client.queueResponse("POST", subFlow, 200, map[string]any{
		"type": "form", "flow_id": "f1", "step_id": "reconfigure", "data_schema": []any{},
	})
	client.queueResponse("POST", subFlow+"/f1", 200, map[string]any{
		"type": "abort", "reason": "reconfigure_successful",
	})
}

func updateOp(key, subentryID string, data map[string]any) registries.RegOp {
	return subentryOp(subentries.KindUpdate, key, map[string]any{
		"entry_id": "e1", "subentry_id": subentryID, "subentry_type": "calendar", "data": data,
	}, subentryID)
}

func TestApplySubentryPlanUpdateRecordsTheHashAndClearsAPriorAttempt(t *testing.T) {
	setSupervisorToken(t)
	client := newFakeIntegrationHTTP()
	queueReconfigureFlow(client)

	data := map[string]any{"calendar_id": "fam@group"}
	managed := map[string]string{"subentry:cal": "s1"}
	hashes := map[string]string{"subentry:cal": "stale"}
	attempts := map[string]map[string]any{"subentry:cal": {"hash": "stale", "error": "boom"}}

	result := ApplySubentryPlan(context.Background(), client, staticDialer(newFakeWS()),
		[]registries.RegOp{updateOp("cal", "s1", data)}, managed, hashes, attempts)

	if !result.OK {
		t.Fatalf("apply failed: %s", result.Error)
	}
	if hashes["subentry:cal"] != subentries.HashData(data) {
		t.Fatalf("hashes = %#v, want the applied data's hash", hashes)
	}
	if len(attempts) != 0 {
		t.Fatalf("attempts = %#v, want a success to clear the failure memory", attempts)
	}
	if got := result.Applied; len(got) != 1 || got[0] != "update subentry:cal" {
		t.Fatalf("applied = %#v", got)
	}
}

func TestApplySubentryPlanRemembersAFailedUpdate(t *testing.T) {
	setSupervisorToken(t)
	client := newFakeIntegrationHTTP()
	client.queueResponse("POST", subFlow, 200, map[string]any{"type": "abort", "reason": "unknown_subentry"})

	data := map[string]any{"calendar_id": "fam@group"}
	hashes := map[string]string{"subentry:cal": "previous"}
	attempts := map[string]map[string]any{}

	result := ApplySubentryPlan(context.Background(), client, staticDialer(newFakeWS()),
		[]registries.RegOp{updateOp("cal", "s1", data)}, map[string]string{"subentry:cal": "s1"}, hashes, attempts)

	if result.OK || !strings.Contains(result.Error, "unknown_subentry") {
		t.Fatalf("result = %#v, want the failed reconfigure reported", result)
	}
	if attempts["subentry:cal"]["hash"] != subentries.HashData(data) {
		t.Fatalf("attempts = %#v, want the failed data's hash remembered", attempts)
	}
	if hashes["subentry:cal"] != "previous" {
		t.Fatal("a failed reconfigure must not record its data as applied")
	}
}

func TestApplySubentryPlanUnmanageTouchesNeitherRESTNorWebSocket(t *testing.T) {
	setSupervisorToken(t)
	client := newFakeIntegrationHTTP()
	ws := newFakeWS()

	managed := map[string]string{"subentry:cal": "s1", "integration:other": "e9"}
	hashes := map[string]string{"subentry:cal": "h", "integration:other": "h"}
	attempts := map[string]map[string]any{"subentry:cal": {"hash": "h"}}

	result := ApplySubentryPlan(context.Background(), client, staticDialer(ws),
		[]registries.RegOp{subentryOp(subentries.KindUpdate, "cal", map[string]any{"unmanage": true}, "s1")},
		managed, hashes, attempts)

	if !result.OK {
		t.Fatalf("apply failed: %s", result.Error)
	}
	if len(client.calls) != 0 || len(ws.calls) != 0 {
		t.Fatal("unmanage must be bookkeeping only - it left the live subentry alone")
	}
	if _, still := managed["subentry:cal"]; still {
		t.Fatalf("managed = %#v, want the key dropped", managed)
	}
	if len(hashes) != 1 || len(attempts) != 0 {
		t.Fatalf("hashes=%#v attempts=%#v, want only this layer's own keys dropped", hashes, attempts)
	}
}

func TestApplySubentryPlanKeepsGoingAfterAFailedOp(t *testing.T) {
	setSupervisorToken(t)
	client := newFakeIntegrationHTTP()
	// First start aborts (the failing op), the second opens a real form.
	client.queueResponse("POST", subFlow, 200, map[string]any{"type": "abort", "reason": "not_supported"})
	client.queueResponse("POST", subFlow, 200, map[string]any{
		"type": "form", "flow_id": "f1", "step_id": "reconfigure", "data_schema": []any{},
	})
	client.queueResponse("POST", subFlow+"/f1", 200, map[string]any{
		"type": "abort", "reason": "reconfigure_successful",
	})

	managed := map[string]string{"subentry:broken": "s1", "subentry:fine": "s2"}
	hashes := map[string]string{}
	result := ApplySubentryPlan(context.Background(), client, staticDialer(newFakeWS()),
		[]registries.RegOp{
			updateOp("broken", "s1", map[string]any{"a": 1}),
			updateOp("fine", "s2", map[string]any{"b": 2}),
		}, managed, hashes, map[string]map[string]any{})

	if result.OK || !strings.Contains(result.Error, "update subentry:broken failed") {
		t.Fatalf("result = %#v, want the broken op reported", result)
	}
	if got := result.Applied; len(got) != 1 || got[0] != "update subentry:fine" {
		t.Fatalf("applied = %#v, want the healthy sibling still applied", got)
	}
	if _, ok := hashes["subentry:fine"]; !ok {
		t.Fatal("the healthy sibling was not recorded")
	}
	if _, ok := hashes["subentry:broken"]; ok {
		t.Fatal("the failed op recorded a hash")
	}
}

func TestApplySubentryPlanReportsErrorOpsWithoutExecutingThem(t *testing.T) {
	setSupervisorToken(t)
	client := newFakeIntegrationHTTP()
	errorOp := subentryOp(registries.KindError, "cal", map[string]any{"error": "ambiguous adopt"}, "")

	result := ApplySubentryPlan(context.Background(), client, staticDialer(newFakeWS()),
		[]registries.RegOp{errorOp}, map[string]string{}, map[string]string{}, map[string]map[string]any{})

	if !result.OK || result.RolledBack {
		t.Fatalf("result = %#v, want an OK result carrying the skipped error", result)
	}
	if len(result.SkippedErrors) != 1 || len(client.calls) != 0 {
		t.Fatalf("skipped = %#v, calls = %d", result.SkippedErrors, len(client.calls))
	}
}

func TestApplySubentryPlanRefusesAnUpdateWithNoSubentryID(t *testing.T) {
	// driveSubentryFlow reads an empty subentry_id as "create", so an update
	// missing its id would quietly create a second subentry.
	setSupervisorToken(t)
	client := newFakeIntegrationHTTP()
	ws := newFakeWS()

	managed, hashes := map[string]string{}, map[string]string{}
	attempts := map[string]map[string]any{}
	op := registries.RegOp{
		Kind: subentries.KindUpdate, RType: "subentry", Key: "cal",
		Params: map[string]any{
			"entry_id": "e1", "subentry_type": "calendar", "data": map[string]any{},
		},
	}
	result := ApplySubentryPlan(context.Background(), client, staticDialer(ws),
		[]registries.RegOp{op}, managed, hashes, attempts)

	if result.OK {
		t.Fatalf("apply reported OK, want the op refused")
	}
	if !strings.Contains(result.Error, "no subentry_id") {
		t.Errorf("Error = %q, want it to name the missing subentry_id", result.Error)
	}
	if len(client.calls) != 0 {
		t.Errorf("client made %d call(s), want none", len(client.calls))
	}
	if len(managed) != 0 || len(hashes) != 0 {
		t.Errorf("managed = %#v hashes = %#v, want neither written", managed, hashes)
	}
	if _, recorded := attempts["subentry:cal"]; !recorded {
		t.Errorf("attempts = %#v, want the refusal remembered", attempts)
	}
}

// A rejection is quoted into the activity feed and into
// state.SubentryAttempts on disk, so resolved secrets must be scrubbed.
func TestApplySubentryPlanScrubsAResolvedSecretOutOfAFailure(t *testing.T) {
	setSupervisorToken(t)
	client := newFakeIntegrationHTTP()
	client.queueResponse("POST", subFlow, 200, map[string]any{
		"type": "abort", "reason": "rejected value S3CRET-resolved",
	})

	data := map[string]any{"api_key": "S3CRET-resolved"}
	op := updateOp("widget", "s1", data)
	op.Secrets = []string{"S3CRET-resolved"}

	attempts := map[string]map[string]any{}
	result := ApplySubentryPlan(context.Background(), client, staticDialer(newFakeWS()),
		[]registries.RegOp{op}, map[string]string{"subentry:widget": "s1"}, map[string]string{}, attempts)

	if result.OK {
		t.Fatalf("result = %#v, want the rejection reported", result)
	}
	if strings.Contains(result.Error, "S3CRET-resolved") {
		t.Errorf("result error carries the resolved secret: %q", result.Error)
	}
	remembered, _ := attempts["subentry:widget"]["error"].(string)
	if strings.Contains(remembered, "S3CRET-resolved") {
		t.Errorf("attempts entry carries the resolved secret onto disk: %q", remembered)
	}
	if !strings.Contains(remembered, "***REDACTED***") {
		t.Errorf("attempts entry = %q, want the marker where the value was", remembered)
	}
}
