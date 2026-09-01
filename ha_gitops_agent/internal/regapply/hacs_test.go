package regapply

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/hacs"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/registries"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/wsclient"
)

// hacsRepo is one entry of hacs/repositories/list as HACS returns it, cut
// down to the fields this layer reads.
func hacsRepo(id, fullName, domain string, installed bool) map[string]any {
	return map[string]any{
		"id": id, "full_name": fullName, "domain": domain,
		"installed": installed, "category": "integration",
	}
}

func hacsList(repos ...map[string]any) any {
	out := make([]any, len(repos))
	for i, repo := range repos {
		out[i] = repo
	}
	return out
}

func hacsConfig(components ...string) any {
	list := make([]any, len(components))
	for i, component := range components {
		list[i] = component
	}
	return map[string]any{"components": list, "version": "2026.8.0"}
}

// installOp is what hacs.Plan emits for a repository that is not installed:
// repositoryID empty means HACS has never heard of it.
func installOp(key, repository, repositoryID, version string) registries.RegOp {
	params := map[string]any{
		"repository": repository, "category": "integration",
		"repository_id": repositoryID, "hash": "hash-of-" + key,
	}
	if version != "" {
		params["version"] = version
	}
	return registries.RegOp{Kind: registries.KindCreate, RType: "hacs", Key: key, Params: params, DiffText: "..."}
}

func adoptOp(key, repository, repositoryID string) registries.RegOp {
	return registries.RegOp{
		Kind: registries.KindUpdate, RType: "hacs", Key: key, LiveID: repositoryID,
		Params:   map[string]any{"adopt": true, "repository": repository, "repository_id": repositoryID},
		DiffText: "...",
	}
}

// --- FetchHacsLive ----------------------------------------------------

// declaredHacs is one manifest item in the shape hacs.LoadManifest hands
// back, as the fetch request carries it.
func declaredHacs(id, repository string) map[string]any {
	return map[string]any{"id": id, "repository": repository, "category": "integration"}
}

// hacsRequest is the fetch request for a cycle declaring repos, with
// managed/pending filled in by the caller.
func hacsRequest(managed map[string]string, pending []string, repos ...map[string]any) HacsFetchRequest {
	return HacsFetchRequest{
		Desired:        hacs.Desired{Repos: repos},
		Managed:        managed,
		RestartPending: pending,
	}
}

// A cycle with anything unrecorded reads the whole store plus the loaded
// components, listing the integration category alone (v1's whole scope).
func TestFetchHacsLiveReadsRepositoriesAndComponents(t *testing.T) {
	ws := newFakeWS()
	ws.results[msgHacsRepositoriesList] = []any{hacsList(
		hacsRepo("1234", "thomluther/ha-anker-solix", "anker_solix", true),
		hacsRepo("5678", "basnijholt/adaptive-lighting", "adaptive_lighting", false),
	)}
	ws.results[msgCoreGetConfig] = []any{hacsConfig("hacs", "sensor", "anker_solix", "sensor.anker_solix")}

	live, err := FetchHacsLive(context.Background(), staticDialer(ws), hacsRequest(nil, []string{"anker_solix"},
		declaredHacs("anker_solix", "thomluther/ha-anker-solix")))
	if err != nil {
		t.Fatalf("FetchHacsLive: %v", err)
	}

	if len(live.Repositories) != 2 {
		t.Errorf("repositories = %+v, want both entries", live.Repositories)
	}
	// Platform pairs ("sensor.anker_solix") are not domains and would never
	// match one, so they are left out rather than compared against.
	if !reflect.DeepEqual(live.Components, []string{"hacs", "sensor", "anker_solix"}) {
		t.Errorf("components = %v, want the bare domains", live.Components)
	}
	if got := ws.callsFor(msgHacsRepositoriesList); len(got) != 1 {
		t.Fatalf("list calls = %d, want 1", len(got))
	}
	if got := ws.callsFor(msgHacsRepositoriesList)[0].params["categories"]; !reflect.DeepEqual(got, []any{"integration"}) {
		t.Errorf("categories = %v, want the integration category alone", got)
	}
	if !ws.closed {
		t.Error("the connection was left open")
	}
}

// Nothing registered the hacs/* commands, so HACS is not installed - the
// one failure the user can act on, so it must not read as an error code.
func TestFetchHacsLiveSaysWhenHacsIsNotInstalled(t *testing.T) {
	ws := newFakeWS()
	ws.raiseOn[msgHacsRepositoriesList] = []error{
		&wsclient.Error{Code: "unknown_command", Message: "unknown command"},
	}

	_, err := FetchHacsLive(context.Background(), staticDialer(ws), hacsRequest(nil, nil,
		declaredHacs("anker_solix", "thomluther/ha-anker-solix")))

	if err == nil {
		t.Fatal("err = nil, want the HACS-missing error")
	}
	for _, want := range []string{"does not look installed", "reconcile.hacs"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %q, want it to mention %q", err, want)
		}
	}
}

// Any other failure passes through as itself: a timeout is not a missing
// HACS, and would send the user to install what they already have.
func TestFetchHacsLiveKeepsAnOrdinaryFailureAsItIs(t *testing.T) {
	ws := newFakeWS()
	ws.raiseOn[msgHacsRepositoriesList] = []error{&wsclient.Error{Code: "timeout", Message: "no response"}}

	_, err := FetchHacsLive(context.Background(), staticDialer(ws), hacsRequest(nil, nil,
		declaredHacs("anker_solix", "thomluther/ha-anker-solix")))

	if err == nil || strings.Contains(err.Error(), "does not look installed") {
		t.Fatalf("err = %v, want the transport failure itself", err)
	}
}

// An unexpected get_config leaves the reminder list alone rather than
// failing the cycle: a stale restart reminder is the harmless error.
func TestFetchHacsLiveToleratesAConfigWithNoComponents(t *testing.T) {
	ws := newFakeWS()
	ws.results[msgHacsRepositoriesList] = []any{hacsList()}
	ws.results[msgCoreGetConfig] = []any{"not an object"}

	live, err := FetchHacsLive(context.Background(), staticDialer(ws), hacsRequest(nil, []string{"anker_solix"},
		declaredHacs("anker_solix", "thomluther/ha-anker-solix")))
	if err != nil {
		t.Fatalf("FetchHacsLive: %v", err)
	}
	if len(live.Components) != 0 {
		t.Errorf("components = %v, want none", live.Components)
	}
}

// --- FetchHacsLive: reading as little as the cycle allows ---------------

// The steady state: everything declared is installed and recorded, so the
// fetch reads just those repositories, never the thousands-entry store.
func TestFetchHacsLiveReadsOnlyTheDeclaredRepositoriesWhenAllAreRecorded(t *testing.T) {
	ws := newFakeWS()
	ws.results[msgHacsRepositoryInfo] = []any{
		hacsRepo("1234", "thomluther/ha-anker-solix", "anker_solix", true),
		hacsRepo("5678", "basnijholt/adaptive-lighting", "adaptive_lighting", true),
	}
	managed := map[string]string{"hacs:anker_solix": "1234", "hacs:adaptive_lighting": "5678"}

	live, err := FetchHacsLive(context.Background(), staticDialer(ws), hacsRequest(managed, nil,
		declaredHacs("anker_solix", "thomluther/ha-anker-solix"),
		declaredHacs("adaptive_lighting", "basnijholt/adaptive-lighting")))
	if err != nil {
		t.Fatalf("FetchHacsLive: %v", err)
	}

	// Two info reads and nothing else: no store listing, and no get_config
	// either, since no restart reminder stands to prune.
	want := []string{msgHacsRepositoryInfo, msgHacsRepositoryInfo}
	if got := ws.callTypes(); !reflect.DeepEqual(got, want) {
		t.Errorf("calls = %v, want %v", got, want)
	}
	if ids := []string{
		ws.callsFor(msgHacsRepositoryInfo)[0].params["repository_id"].(string),
		ws.callsFor(msgHacsRepositoryInfo)[1].params["repository_id"].(string),
	}; !reflect.DeepEqual(ids, []string{"1234", "5678"}) {
		t.Errorf("repository ids read = %v, want the recorded ones", ids)
	}
	// And the planner sees exactly what it would have found in the listing.
	if len(live.Repositories) != 2 {
		t.Fatalf("repositories = %+v, want one object per declared item", live.Repositories)
	}
	desired := hacs.Desired{Repos: []map[string]any{
		declaredHacs("anker_solix", "thomluther/ha-anker-solix"),
		declaredHacs("adaptive_lighting", "basnijholt/adaptive-lighting"),
	}}
	if ops := hacs.Plan(desired, live.Repositories, managed, nil); len(ops) != 0 {
		t.Errorf("ops = %+v, want the steady state to plan nothing", ops)
	}
}

// With no recorded id a declaration can only be found by full_name, so the
// whole store is read and the per-repository reads are not tried first.
func TestFetchHacsLiveFallsBackToTheListForAnUnrecordedDeclaration(t *testing.T) {
	ws := newFakeWS()
	ws.results[msgHacsRepositoriesList] = []any{
		hacsList(hacsRepo("5678", "basnijholt/adaptive-lighting", "adaptive_lighting", false)),
	}
	managed := map[string]string{"hacs:anker_solix": "1234"}

	live, err := FetchHacsLive(context.Background(), staticDialer(ws), hacsRequest(managed, nil,
		declaredHacs("adaptive_lighting", "basnijholt/adaptive-lighting")))
	if err != nil {
		t.Fatalf("FetchHacsLive: %v", err)
	}

	if got := ws.callTypes(); !reflect.DeepEqual(got, []string{msgHacsRepositoriesList}) {
		t.Errorf("calls = %v, want the store listing alone", got)
	}
	if len(live.Repositories) != 1 {
		t.Errorf("repositories = %+v, want the listing", live.Repositories)
	}
}

// The rebind check needs the newly declared repository's id (hacs.Plan
// rule 5), so a stale full_name falls back to the listing to find it.
func TestFetchHacsLiveFallsBackToTheListWhenARecordedIdNoLongerMatches(t *testing.T) {
	ws := newFakeWS()
	ws.results[msgHacsRepositoryInfo] = []any{
		hacsRepo("1234", "thomluther/ha-anker-solix", "anker_solix", true),
	}
	ws.results[msgHacsRepositoriesList] = []any{hacsList(
		hacsRepo("1234", "thomluther/ha-anker-solix", "anker_solix", true),
		hacsRepo("9999", "someone/else", "else", true),
	)}
	managed := map[string]string{"hacs:anker_solix": "1234"}
	desired := hacs.Desired{Repos: []map[string]any{declaredHacs("anker_solix", "someone/else")}}

	live, err := FetchHacsLive(context.Background(), staticDialer(ws), HacsFetchRequest{
		Desired: desired, Managed: managed,
	})
	if err != nil {
		t.Fatalf("FetchHacsLive: %v", err)
	}

	want := []string{msgHacsRepositoryInfo, msgHacsRepositoriesList}
	if got := ws.callTypes(); !reflect.DeepEqual(got, want) {
		t.Errorf("calls = %v, want %v", got, want)
	}
	ops := hacs.Plan(desired, live.Repositories, managed, nil)
	if len(ops) != 1 || ops[0].Kind != registries.KindError {
		t.Fatalf("ops = %+v, want the rebind refused", ops)
	}
}

// A recorded id HACS no longer answers for is a case only the listing can
// decide, so it falls back rather than reporting a failure.
func TestFetchHacsLiveFallsBackToTheListWhenARecordedRepositoryIsGoneOrUninstalled(t *testing.T) {
	cases := []struct {
		name  string
		setup func(ws *fakeWS)
	}{
		{"HACS refuses the id", func(ws *fakeWS) {
			ws.raiseOn[msgHacsRepositoryInfo] = []error{
				&wsclient.Error{Code: "unknown_error", Message: "unknown repository"},
			}
		}},
		{"the repository is no longer installed", func(ws *fakeWS) {
			ws.results[msgHacsRepositoryInfo] = []any{
				hacsRepo("1234", "thomluther/ha-anker-solix", "anker_solix", false),
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ws := newFakeWS()
			tc.setup(ws)
			ws.results[msgHacsRepositoriesList] = []any{
				hacsList(hacsRepo("1234", "thomluther/ha-anker-solix", "anker_solix", false)),
			}

			live, err := FetchHacsLive(context.Background(), staticDialer(ws),
				hacsRequest(map[string]string{"hacs:anker_solix": "1234"}, nil,
					declaredHacs("anker_solix", "thomluther/ha-anker-solix")))
			if err != nil {
				t.Fatalf("FetchHacsLive: %v", err)
			}
			if got := ws.callTypes(); !reflect.DeepEqual(got, []string{msgHacsRepositoryInfo, msgHacsRepositoriesList}) {
				t.Errorf("calls = %v, want the info read then the listing", got)
			}
			if len(live.Repositories) != 1 {
				t.Errorf("repositories = %+v, want the listing", live.Repositories)
			}
		})
	}
}

// A dead connection would fail the listing too, saying nothing about HACS,
// so it is reported as itself rather than costing a second doomed call.
func TestFetchHacsLiveDoesNotFallBackAfterATransportFailure(t *testing.T) {
	ws := newFakeWS()
	ws.raiseOn[msgHacsRepositoryInfo] = []error{&wsclient.Error{Code: "transport", Message: "connection closed"}}

	_, err := FetchHacsLive(context.Background(), staticDialer(ws),
		hacsRequest(map[string]string{"hacs:anker_solix": "1234"}, nil,
			declaredHacs("anker_solix", "thomluther/ha-anker-solix")))

	if err == nil || !strings.Contains(err.Error(), "connection closed") {
		t.Fatalf("err = %v, want the transport failure itself", err)
	}
	if got := ws.callTypes(); !reflect.DeepEqual(got, []string{msgHacsRepositoryInfo}) {
		t.Errorf("calls = %v, want no second attempt on a dead connection", got)
	}
}

// Nothing declared and a reminder standing: the only work is pruning it
// against Core's get_config, so no HACS command is sent at all.
func TestFetchHacsLiveReadsOnlyTheComponentsForAStandingReminder(t *testing.T) {
	ws := newFakeWS()
	ws.results[msgCoreGetConfig] = []any{hacsConfig("hacs", "anker_solix")}

	live, err := FetchHacsLive(context.Background(), staticDialer(ws), hacsRequest(nil, []string{"anker_solix"}))
	if err != nil {
		t.Fatalf("FetchHacsLive: %v", err)
	}

	if got := ws.callTypes(); !reflect.DeepEqual(got, []string{msgCoreGetConfig}) {
		t.Errorf("calls = %v, want get_config alone", got)
	}
	if len(live.Repositories) != 0 {
		t.Errorf("repositories = %+v, want none read", live.Repositories)
	}
	if got := hacs.PruneRestartPending([]string{"anker_solix"}, live.Components); len(got) != 0 {
		t.Errorf("pending = %v, want the loaded domain to clear the reminder", got)
	}
}

// --- ApplyHacsPlan: installing ----------------------------------------

// The full path for a repository HACS has never seen: add it as a custom
// repository, read back the id it was given, download it, and confirm.
func TestApplyHacsPlanAddsThenDownloadsAnUnknownRepository(t *testing.T) {
	ws := newFakeWS()
	ws.results[msgHacsRepositoriesList] = []any{
		hacsList(hacsRepo("1234", "thomluther/ha-anker-solix", "anker_solix", false)),
	}
	ws.results[msgHacsRepositoryInfo] = []any{
		hacsRepo("1234", "thomluther/ha-anker-solix", "anker_solix", true),
	}
	managed := map[string]string{}
	attempts := map[string]map[string]any{}
	var restartPending []string

	result := ApplyHacsPlan(context.Background(), staticDialer(ws),
		[]registries.RegOp{installOp("anker_solix", "thomluther/ha-anker-solix", "", "3.1.0")},
		managed, attempts, &restartPending)

	if !result.OK {
		t.Fatalf("result = %+v, want ok", result)
	}
	// One store listing, because only an add changes what the store holds;
	// the confirmation reads the single repository (see the package doc).
	want := []string{
		msgHacsRepositoriesAdd, msgHacsRepositoriesList,
		msgHacsRepositoryDownload, msgHacsRepositoryInfo,
	}
	if got := ws.callTypes(); !reflect.DeepEqual(got, want) {
		t.Errorf("calls = %v, want %v", got, want)
	}
	add := ws.callsFor(msgHacsRepositoriesAdd)[0].params
	if add["repository"] != "thomluther/ha-anker-solix" || add["category"] != "integration" {
		t.Errorf("add params = %+v", add)
	}
	download := ws.callsFor(msgHacsRepositoryDownload)[0].params
	if download["repository"] != "1234" || download["version"] != "3.1.0" {
		t.Errorf("download params = %+v, want the id HACS assigned and the pinned version", download)
	}
	if managed["hacs:anker_solix"] != "1234" {
		t.Errorf("managed = %+v, want the repository id recorded", managed)
	}
	// Downloaded, therefore on disk and not imported: a custom component
	// only loads at startup.
	if !reflect.DeepEqual(restartPending, []string{"anker_solix"}) {
		t.Errorf("restart pending = %v, want the downloaded domain", restartPending)
	}
}

// An already-listed repository downloads by its known id, with no
// custom-repository step and no version key - which asks for the latest.
func TestApplyHacsPlanDownloadsAKnownRepositoryByID(t *testing.T) {
	ws := newFakeWS()
	ws.results[msgHacsRepositoryInfo] = []any{
		hacsRepo("1234", "thomluther/ha-anker-solix", "anker_solix", true),
	}
	managed := map[string]string{}
	var restartPending []string

	result := ApplyHacsPlan(context.Background(), staticDialer(ws),
		[]registries.RegOp{installOp("anker_solix", "thomluther/ha-anker-solix", "1234", "")},
		managed, map[string]map[string]any{}, &restartPending)

	if !result.OK {
		t.Fatalf("result = %+v, want ok", result)
	}
	// No listing at all: the plan already carried the id, so nothing here
	// needs the whole store.
	if got := ws.callTypes(); !reflect.DeepEqual(got, []string{msgHacsRepositoryDownload, msgHacsRepositoryInfo}) {
		t.Errorf("calls = %v, want a download and its confirmation", got)
	}
	if _, present := ws.callsFor(msgHacsRepositoryDownload)[0].params["version"]; present {
		t.Error("download params carry a version the manifest never declared")
	}
}

// A download unpacks GitHub archives before answering - minutes on a Pi.
// The ten-second default would remember a working install as failed.
func TestApplyHacsPlanGivesOnlyTheDownloadALongBudget(t *testing.T) {
	ws := newFakeWS()
	ws.results[msgHacsRepositoryInfo] = []any{
		hacsRepo("1234", "thomluther/ha-anker-solix", "anker_solix", true),
	}

	ApplyHacsPlan(context.Background(), staticDialer(ws),
		[]registries.RegOp{installOp("anker_solix", "thomluther/ha-anker-solix", "", "")},
		map[string]string{}, map[string]map[string]any{}, new([]string))

	for i, call := range ws.calls {
		budget := ws.timeouts[i]
		switch call.msgType {
		case msgHacsRepositoryDownload:
			if budget != hacsDownloadTimeout {
				t.Errorf("download budget = %v, want %v", budget, hacsDownloadTimeout)
			}
		case msgHacsRepositoriesList:
			// The whole store, serialized on the fly - its own budget too,
			// or a slow box records a transport failure and retries the
			// same listing every cycle.
			if budget != hacsListTimeout {
				t.Errorf("list budget = %v, want %v", budget, hacsListTimeout)
			}
		default:
			if budget != 0 {
				t.Errorf("%s budget = %v, want the client default", call.msgType, budget)
			}
		}
	}
}

// An accepted download that left the repository unmarked is a failure:
// reporting it applied would re-download it every cycle.
func TestApplyHacsPlanFailsWhenTheRepositoryIsNotMarkedInstalled(t *testing.T) {
	ws := newFakeWS()
	ws.results[msgHacsRepositoryInfo] = []any{
		hacsRepo("1234", "thomluther/ha-anker-solix", "anker_solix", false),
	}
	managed := map[string]string{}
	attempts := map[string]map[string]any{}
	var restartPending []string

	result := ApplyHacsPlan(context.Background(), staticDialer(ws),
		[]registries.RegOp{installOp("anker_solix", "thomluther/ha-anker-solix", "1234", "")},
		managed, attempts, &restartPending)

	if result.OK {
		t.Fatalf("result = %+v, want a failure", result)
	}
	if len(managed) != 0 || len(restartPending) != 0 {
		t.Errorf("managed = %+v, restart pending = %v, want neither recorded", managed, restartPending)
	}
	entry, found := attempts["hacs:anker_solix"]
	if !found {
		t.Fatalf("attempts = %+v, want the failure remembered", attempts)
	}
	if entry["hash"] != "hash-of-anker_solix" {
		t.Errorf("attempts hash = %v, want the planned entry's own fingerprint", entry["hash"])
	}
	if !strings.Contains(entry["error"].(string), "does not mark it installed") {
		t.Errorf("attempts error = %v", entry["error"])
	}
}

// A failing download records why, so the next plan refuses to repeat it -
// and says so in the words HACS itself used.
func TestApplyHacsPlanRecordsADownloadFailure(t *testing.T) {
	ws := newFakeWS()
	ws.raiseOn[msgHacsRepositoryDownload] = []error{
		&wsclient.Error{Code: "unknown_error", Message: "no release tagged 9.9.9"},
	}
	attempts := map[string]map[string]any{}
	var restartPending []string

	result := ApplyHacsPlan(context.Background(), staticDialer(ws),
		[]registries.RegOp{installOp("anker_solix", "thomluther/ha-anker-solix", "1234", "9.9.9")},
		map[string]string{}, attempts, &restartPending)

	if result.OK {
		t.Fatalf("result = %+v, want a failure", result)
	}
	if !strings.Contains(result.Error, "no release tagged 9.9.9") {
		t.Errorf("error = %q, want HACS's own words", result.Error)
	}
	if _, found := attempts["hacs:anker_solix"]; !found {
		t.Errorf("attempts = %+v, want the failure remembered", attempts)
	}
}

// The HACS-missing wording reaches applies too: a box that lost HACS
// mid-cycle must not report an "unknown_command" nobody can act on.
func TestApplyHacsPlanSaysWhenHacsIsNotInstalled(t *testing.T) {
	ws := newFakeWS()
	ws.raiseOn[msgHacsRepositoriesAdd] = []error{
		&wsclient.Error{Code: "unknown_command", Message: "unknown command"},
	}

	result := ApplyHacsPlan(context.Background(), staticDialer(ws),
		[]registries.RegOp{installOp("anker_solix", "thomluther/ha-anker-solix", "", "")},
		map[string]string{}, map[string]map[string]any{}, new([]string))

	if result.OK || !strings.Contains(result.Error, "does not look installed") {
		t.Errorf("result = %+v, want the HACS-missing wording", result)
	}
}

// Each repository applies independently, as in the integration and
// subentry layers: one failed download must not stop a sibling.
func TestApplyHacsPlanIsolatesOneFailedItem(t *testing.T) {
	ws := newFakeWS()
	ws.raiseOn[msgHacsRepositoryDownload] = []error{
		&wsclient.Error{Code: "unknown_error", Message: "repository not found"},
	}
	ws.results[msgHacsRepositoryInfo] = []any{
		hacsRepo("5678", "basnijholt/adaptive-lighting", "adaptive_lighting", true),
	}
	managed := map[string]string{}
	var restartPending []string

	result := ApplyHacsPlan(context.Background(), staticDialer(ws), []registries.RegOp{
		installOp("gone", "nobody/no-such-repo", "1111", ""),
		installOp("adaptive_lighting", "basnijholt/adaptive-lighting", "5678", ""),
	}, managed, map[string]map[string]any{}, &restartPending)

	if result.OK {
		t.Fatalf("result = %+v, want the overall call to fail", result)
	}
	if !reflect.DeepEqual(result.Applied, []string{"create hacs:adaptive_lighting"}) {
		t.Errorf("applied = %v, want the sibling that worked", result.Applied)
	}
	if managed["hacs:adaptive_lighting"] != "5678" {
		t.Errorf("managed = %+v, want the successful sibling recorded", managed)
	}
}

// Error ops never execute and never block the rest of the plan - the same
// contract every other Apply*Plan in this package keeps.
func TestApplyHacsPlanSkipsErrorOps(t *testing.T) {
	ws := newFakeWS()

	result := ApplyHacsPlan(context.Background(), staticDialer(ws), []registries.RegOp{
		{Kind: registries.KindError, RType: "hacs", Key: "broken", Error: "previous attempt failed"},
	}, map[string]string{}, map[string]map[string]any{}, new([]string))

	if !result.OK || len(result.SkippedErrors) != 1 {
		t.Fatalf("result = %+v, want the error op skipped and reported", result)
	}
	if len(ws.calls) != 0 {
		t.Errorf("calls = %v, want nothing sent", ws.callTypes())
	}
}

// --- ApplyHacsPlan: adopting ------------------------------------------

// An adopt is bookkeeping only and opens no connection, so an adopt-only
// plan applies on a box the agent cannot reach over the WebSocket.
func TestApplyHacsPlanAdoptsWithoutDialing(t *testing.T) {
	dialed := false
	dialer := func(context.Context) (WSClient, error) {
		dialed = true
		return nil, errors.New("should not be dialed")
	}
	managed := map[string]string{}
	attempts := map[string]map[string]any{"hacs:adaptive_lighting": {"hash": "old", "error": "github rate limit"}}
	var restartPending []string

	result := ApplyHacsPlan(context.Background(), dialer,
		[]registries.RegOp{adoptOp("adaptive_lighting", "basnijholt/adaptive-lighting", "5678")},
		managed, attempts, &restartPending)

	if !result.OK {
		t.Fatalf("result = %+v, want ok", result)
	}
	if dialed {
		t.Error("an adopt-only plan opened a websocket")
	}
	if managed["hacs:adaptive_lighting"] != "5678" {
		t.Errorf("managed = %+v, want the ownership recorded", managed)
	}
	if len(attempts) != 0 {
		t.Errorf("attempts = %+v, want the stale record cleared", attempts)
	}
	// Adopting says nothing about loading: the integration was already
	// installed, so it is either loaded or it is somebody else's restart.
	if len(restartPending) != 0 {
		t.Errorf("restart pending = %v, want an adopt to raise no reminder", restartPending)
	}
}

// --- idempotence -------------------------------------------------------

// HACS marks a repository installed the moment the download finishes, so
// the next plan sends nothing instead of downloading it again.
func TestASecondPlanAfterADownloadSendsNothing(t *testing.T) {
	ws := newFakeWS()
	ws.results[msgHacsRepositoryInfo] = []any{
		hacsRepo("1234", "thomluther/ha-anker-solix", "anker_solix", true),
	}
	// The next cycle's own fetch, which reads the repository the apply just
	// recorded - one object, by id.
	ws.results[msgHacsRepositoryInfo] = append(ws.results[msgHacsRepositoryInfo],
		hacsRepo("1234", "thomluther/ha-anker-solix", "anker_solix", true))
	ws.results[msgCoreGetConfig] = []any{hacsConfig("hacs", "sensor")}
	managed := map[string]string{}
	var restartPending []string

	first := ApplyHacsPlan(context.Background(), staticDialer(ws),
		[]registries.RegOp{installOp("anker_solix", "thomluther/ha-anker-solix", "1234", "")},
		managed, map[string]map[string]any{}, &restartPending)
	if !first.OK {
		t.Fatalf("first apply = %+v, want ok", first)
	}

	desired := hacs.Desired{Repos: []map[string]any{{
		"id": "anker_solix", "repository": "thomluther/ha-anker-solix", "category": "integration",
	}}}
	live, err := FetchHacsLive(context.Background(), staticDialer(ws), HacsFetchRequest{
		Desired: desired, Managed: managed, RestartPending: restartPending,
	})
	if err != nil {
		t.Fatalf("FetchHacsLive: %v", err)
	}

	if ops := hacs.Plan(desired, live.Repositories, managed, nil); len(ops) != 0 {
		t.Errorf("ops = %+v, want nothing left to do", ops)
	}
	// And the reminder is still standing, because the domain is not in the
	// components this fetch read.
	if got := hacs.PruneRestartPending(restartPending, live.Components); !reflect.DeepEqual(got, []string{"anker_solix"}) {
		t.Errorf("pending = %v, want the reminder to survive until the domain loads", got)
	}
}

// --- the connection under the layer ------------------------------------

// countingDialer hands out a fresh fakeWS per dial, recording each one, so
// a test can tell a redial from a reused connection.
type countingDialer struct {
	conns []*fakeWS
	build func(n int) *fakeWS
}

func (d *countingDialer) dial(context.Context) (WSClient, error) {
	ws := d.build(len(d.conns))
	d.conns = append(d.conns, ws)
	return ws, nil
}

// coder/websocket closes the connection on any error, so a cached dead
// client failed every later op and got each remembered as an item failure
// (see lazyConn). The isolation test above leaves the connection usable.
func TestApplyHacsPlanRedialsAfterATransportFailureInsteadOfCascading(t *testing.T) {
	dialer := &countingDialer{build: func(n int) *fakeWS {
		ws := newFakeWS()
		if n == 0 {
			// The first connection dies on the first download, the way a
			// real one does when a command times out.
			ws.raiseOn[msgHacsRepositoryDownload] = []error{
				&wsclient.Error{Code: "timeout", Message: "no response for id=1 within 10s"},
			}
			return ws
		}
		ws.results[msgHacsRepositoryInfo] = []any{
			hacsRepo("5678", "basnijholt/adaptive-lighting", "adaptive_lighting", true),
		}
		return ws
	}}
	managed := map[string]string{}
	attempts := map[string]map[string]any{}
	var restartPending []string

	result := ApplyHacsPlan(context.Background(), dialer.dial, []registries.RegOp{
		installOp("slow", "thomluther/ha-anker-solix", "1234", ""),
		installOp("adaptive_lighting", "basnijholt/adaptive-lighting", "5678", ""),
	}, managed, attempts, &restartPending)

	if len(dialer.conns) != 2 {
		t.Fatalf("dials = %d, want the dead connection replaced", len(dialer.conns))
	}
	if !dialer.conns[0].closed {
		t.Error("the dead connection was not closed")
	}
	// The sibling behind the failure ran on the fresh connection and
	// genuinely applied.
	if !reflect.DeepEqual(result.Applied, []string{"create hacs:adaptive_lighting"}) {
		t.Errorf("applied = %v, want the op behind the failure to have run", result.Applied)
	}
	if managed["hacs:adaptive_lighting"] != "5678" {
		t.Errorf("managed = %+v, want the sibling recorded", managed)
	}
	// Nothing is remembered: the connection failed, not the declaration,
	// and a record would block a repo whose hash never changes on its own.
	if len(attempts) != 0 {
		t.Errorf("attempts = %+v, want no failure memory from a transport failure", attempts)
	}
	// It is still reported, though - this cycle really did not install it.
	if !strings.Contains(result.Error, "create hacs:slow failed") {
		t.Errorf("error = %q, want the failed op named", result.Error)
	}
}

// A failure that does not close the connection must not cost a redial, or
// a busy box turns into a connection storm.
func TestApplyHacsPlanKeepsTheConnectionAfterARefusedCommand(t *testing.T) {
	dialer := &countingDialer{build: func(int) *fakeWS {
		ws := newFakeWS()
		ws.raiseOn[msgHacsRepositoryDownload] = []error{
			&wsclient.Error{Code: "unknown_error", Message: "repository not found"},
			&wsclient.Error{Code: "unknown_error", Message: "repository not found"},
		}
		return ws
	}}

	ApplyHacsPlan(context.Background(), dialer.dial, []registries.RegOp{
		installOp("one", "owner/one", "1", ""),
		installOp("two", "owner/two", "2", ""),
	}, map[string]string{}, map[string]map[string]any{}, new([]string))

	if len(dialer.conns) != 1 {
		t.Errorf("dials = %d, want the connection reused", len(dialer.conns))
	}
}
