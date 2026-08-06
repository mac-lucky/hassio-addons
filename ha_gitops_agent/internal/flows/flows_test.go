package flows

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/registries"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/secretref/secrettest"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// --- LoadManifest ----------------------------------------------------

func TestMissingIntegrationsFileIsNotAnError(t *testing.T) {
	dir := t.TempDir()
	desired, err := LoadManifest(dir)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(desired.Integrations) != 0 {
		t.Errorf("integrations = %+v, want empty", desired.Integrations)
	}
}

func TestMissingGitopsDirIsNotAnError(t *testing.T) {
	dir := t.TempDir()
	desired, err := LoadManifest(filepath.Join(dir, "nonexistent"))
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(desired.Integrations) != 0 {
		t.Errorf("integrations = %+v, want empty", desired.Integrations)
	}
}

func TestEmptyIntegrationsKeyIsNotAnError(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "gitops", "integrations.yaml"), "integrations:\n")
	desired, err := LoadManifest(dir)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(desired.Integrations) != 0 {
		t.Errorf("integrations = %+v, want empty", desired.Integrations)
	}
}

func TestLoadManifestInvalidYAMLIsAnError(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "gitops", "integrations.yaml"), "integrations: [\n")
	_, err := LoadManifest(dir)
	if err == nil {
		t.Fatal("err = nil, want an error")
	}
}

func TestLoadManifestTopLevelMustBeMapping(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "gitops", "integrations.yaml"), "- not\n- a\n- mapping\n")
	_, err := LoadManifest(dir)
	if err == nil {
		t.Fatal("err = nil, want an error")
	}
}

func TestLoadManifestIntegrationsMustBeAList(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "gitops", "integrations.yaml"), "integrations: not-a-list\n")
	_, err := LoadManifest(dir)
	if err == nil {
		t.Fatal("err = nil, want an error")
	}
}

func TestLoadManifestParsesAllKnownFields(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "gitops", "integrations.yaml"), `
integrations:
  - id: workday_main
    domain: workday
    title: Workday
    data:
      user:
        name: Workday
        country: PL
`)
	desired, err := LoadManifest(dir)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(desired.Integrations) != 1 {
		t.Fatalf("integrations = %+v, want 1 entry", desired.Integrations)
	}
	item := desired.Integrations[0]
	if item["id"] != "workday_main" || item["domain"] != "workday" || item["title"] != "Workday" {
		t.Errorf("item = %+v", item)
	}
	data, _ := item["data"].(map[string]any)
	user, _ := data["user"].(map[string]any)
	if user["name"] != "Workday" || user["country"] != "PL" {
		t.Errorf("data = %+v", data)
	}
}

func TestLoadManifestDefaultsDataToEmptyMap(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "gitops", "integrations.yaml"), `
integrations:
  - id: no_input
    domain: moon
    title: Moon
`)
	desired, err := LoadManifest(dir)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	data, ok := desired.Integrations[0]["data"].(map[string]any)
	if !ok || len(data) != 0 {
		t.Errorf("data = %+v, want empty non-nil map", desired.Integrations[0]["data"])
	}
}

func TestLoadManifestInvalidID(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "gitops", "integrations.yaml"), `
integrations:
  - id: "Not Valid!"
    domain: moon
    title: Moon
`)
	_, err := LoadManifest(dir)
	if err == nil {
		t.Fatal("err = nil, want an error")
	}
}

func TestLoadManifestDuplicateID(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "gitops", "integrations.yaml"), `
integrations:
  - id: dup
    domain: moon
    title: Moon
  - id: dup
    domain: workday
    title: Workday
`)
	_, err := LoadManifest(dir)
	if err == nil {
		t.Fatal("err = nil, want an error")
	}
	if !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("err = %v, want it to mention duplicate", err)
	}
}

func TestLoadManifestMissingDomainIsAnError(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "gitops", "integrations.yaml"), `
integrations:
  - id: x
    title: X
`)
	_, err := LoadManifest(dir)
	if err == nil {
		t.Fatal("err = nil, want an error")
	}
}

func TestLoadManifestMissingTitleIsAnError(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "gitops", "integrations.yaml"), `
integrations:
  - id: x
    domain: moon
`)
	_, err := LoadManifest(dir)
	if err == nil {
		t.Fatal("err = nil, want an error")
	}
}

func TestLoadManifestUnsupportedFieldIsAnError(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "gitops", "integrations.yaml"), `
integrations:
  - id: x
    domain: moon
    title: Moon
    entry_id: abc123
`)
	_, err := LoadManifest(dir)
	if err == nil {
		t.Fatal("err = nil, want an error")
	}
	if !strings.Contains(err.Error(), "unsupported field") {
		t.Errorf("err = %v, want it to mention unsupported field", err)
	}
}

func TestLoadManifestDataMustBeAMapping(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "gitops", "integrations.yaml"), `
integrations:
  - id: x
    domain: moon
    title: Moon
    data: [1, 2, 3]
`)
	_, err := LoadManifest(dir)
	if err == nil {
		t.Fatal("err = nil, want an error")
	}
}

func TestLoadManifestDataStepMustBeAMapping(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "gitops", "integrations.yaml"), `
integrations:
  - id: x
    domain: moon
    title: Moon
    data:
      user: not-a-mapping
`)
	_, err := LoadManifest(dir)
	if err == nil {
		t.Fatal("err = nil, want an error")
	}
	if !strings.Contains(err.Error(), "must be a mapping") {
		t.Errorf("err = %v, want it to mention 'must be a mapping'", err)
	}
}

func TestLoadManifestAggregatesMultipleProblems(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "gitops", "integrations.yaml"), `
integrations:
  - id: bad one
    domain: moon
    title: Moon
  - id: x
    title: X
`)
	_, err := LoadManifest(dir)
	if err == nil {
		t.Fatal("err = nil, want an error")
	}
	merr, ok := err.(*ManifestError)
	if !ok {
		t.Fatalf("err type = %T, want *ManifestError", err)
	}
	if len(merr.Problems) < 2 {
		t.Errorf("problems = %+v, want at least 2", merr.Problems)
	}
}

// --- HashData ----------------------------------------------------------

func TestHashDataDeterministicRegardlessOfKeyOrder(t *testing.T) {
	a := map[string]any{"user": map[string]any{"name": "Workday", "country": "PL"}}
	b := map[string]any{"user": map[string]any{"country": "PL", "name": "Workday"}}
	if HashData(a) != HashData(b) {
		t.Errorf("hashes differ for the same data in a different key order")
	}
}

func TestHashDataDiffersForDifferentValues(t *testing.T) {
	a := map[string]any{"user": map[string]any{"name": "Workday", "country": "PL"}}
	b := map[string]any{"user": map[string]any{"name": "Workday", "country": "DE"}}
	if HashData(a) == HashData(b) {
		t.Errorf("hashes match for different data")
	}
}

func TestHashDataHandlesNil(t *testing.T) {
	if HashData(nil) == "" {
		t.Errorf("HashData(nil) = %q, want a non-empty hash", HashData(nil))
	}
	if HashData(nil) != HashData(map[string]any{}) {
		t.Errorf("HashData(nil) != HashData({}), want them equal")
	}
}

// --- Plan ----------------------------------------------------------------

func item(id, domain, title string, data map[string]any) map[string]any {
	if data == nil {
		data = map[string]any{}
	}
	return map[string]any{"id": id, "domain": domain, "title": title, "data": data}
}

func liveEntry(entryID, domain, title string) map[string]any {
	return map[string]any{"entry_id": entryID, "domain": domain, "title": title}
}

func findOp(ops []registries.RegOp, key string) *registries.RegOp {
	for i := range ops {
		if ops[i].Key == key {
			return &ops[i]
		}
	}
	return nil
}

func TestPlanCreatesWhenUnmanagedAndNoLiveMatch(t *testing.T) {
	desired := Desired{Integrations: []map[string]any{item("workday_main", "workday", "Workday", nil)}}
	ops := Plan(desired, nil, nil, nil, nil, nil)
	if len(ops) != 1 {
		t.Fatalf("ops = %+v, want 1", ops)
	}
	op := ops[0]
	if op.Kind != KindCreate || op.RType != "integration" || op.Key != "workday_main" {
		t.Errorf("op = %+v", op)
	}
	if op.Params["domain"] != "workday" || op.Params["title"] != "Workday" {
		t.Errorf("params = %+v", op.Params)
	}
}

// --- Plan(): failure memory ----------------------------------------------

func TestPlanBlockedByPriorFailureAtSameHashEmitsErrorOpNotCreate(t *testing.T) {
	data := map[string]any{}
	desired := Desired{Integrations: []map[string]any{item("esphome_main", "esphome", "ESPHome", data)}}
	attempts := map[string]map[string]any{
		"integration:esphome_main": {"hash": HashData(data), "error": "domain esphome: flow step 'user' has no declared data"},
	}

	ops := Plan(desired, nil, nil, nil, attempts, nil)
	if len(ops) != 1 || ops[0].Kind != KindError {
		t.Fatalf("ops = %+v, want a single error op, not a create", ops)
	}
	if !strings.Contains(ops[0].Error, "previous attempt failed") ||
		!strings.Contains(ops[0].Error, "flow step 'user' has no declared data") {
		t.Errorf("error = %q, want it to quote the stored failure", ops[0].Error)
	}
	if !strings.Contains(ops[0].Error, "change its manifest entry or press Retry on the dashboard") {
		t.Errorf("error = %q, want it to explain how to retry", ops[0].Error)
	}
}

func TestPlanNotBlockedWhenDeclaredDataHashChanged(t *testing.T) {
	desired := Desired{Integrations: []map[string]any{
		item("esphome_main", "esphome", "ESPHome", map[string]any{"user": map[string]any{"name": "fixed"}}),
	}}
	// The failure was recorded against the OLD data's hash; the manifest
	// has since been edited, so this must not be blocked.
	attempts := map[string]map[string]any{
		"integration:esphome_main": {"hash": HashData(map[string]any{}), "error": "flow step 'user' has no declared data"},
	}

	ops := Plan(desired, nil, nil, nil, attempts, nil)
	if len(ops) != 1 || ops[0].Kind != KindCreate {
		t.Fatalf("ops = %+v, want a create once the manifest entry changed", ops)
	}
}

func TestPlanIgnoresAttemptsForAKeyThatIsNowManaged(t *testing.T) {
	// attempts is only consulted on the create branch, so a stale entry
	// for a now-managed key must never resurface as a blocked create.
	data := map[string]any{}
	desired := Desired{Integrations: []map[string]any{item("workday_main", "workday", "Workday", data)}}
	live := []map[string]any{liveEntry("abc123", "workday", "Workday")}
	managed := map[string]string{"integration:workday_main": "abc123"}
	hashes := map[string]string{"integration:workday_main": HashData(data)}
	attempts := map[string]map[string]any{
		"integration:workday_main": {"hash": HashData(data), "error": "stale, must never be consulted while managed"},
	}

	ops := Plan(desired, live, managed, hashes, attempts, nil)
	if len(ops) != 0 {
		t.Errorf("ops = %+v, want none (already managed, unchanged)", ops)
	}
}

func TestPlanNeverMutatesAttempts(t *testing.T) {
	data := map[string]any{}
	desired := Desired{Integrations: []map[string]any{item("esphome_main", "esphome", "ESPHome", data)}}
	attempts := map[string]map[string]any{
		"integration:esphome_main": {"hash": HashData(data), "error": "boom"},
	}
	snapshot := map[string]map[string]any{
		"integration:esphome_main": {"hash": HashData(data), "error": "boom"},
	}

	Plan(desired, nil, nil, nil, attempts, nil)

	if !reflect.DeepEqual(attempts, snapshot) {
		t.Errorf("Plan mutated its attempts input: %+v, want %+v", attempts, snapshot)
	}
}

func TestPlanAdoptsExactSingleMatch(t *testing.T) {
	desired := Desired{Integrations: []map[string]any{item("workday_main", "workday", "Workday", nil)}}
	live := []map[string]any{liveEntry("abc123", "workday", "Workday")}
	ops := Plan(desired, live, nil, nil, nil, nil)
	if len(ops) != 1 {
		t.Fatalf("ops = %+v, want 1", ops)
	}
	op := ops[0]
	if op.Kind != KindUpdate || op.LiveID != "abc123" {
		t.Errorf("op = %+v, want an adopt (KindUpdate) with live_id abc123", op)
	}
}

func TestPlanAdoptRequiresExactTitleMatch(t *testing.T) {
	desired := Desired{Integrations: []map[string]any{item("workday_main", "workday", "Workday", nil)}}
	live := []map[string]any{liveEntry("abc123", "workday", "Workday (old)")}
	ops := Plan(desired, live, nil, nil, nil, nil)
	if len(ops) != 1 || ops[0].Kind != KindCreate {
		t.Fatalf("ops = %+v, want a create (title did not match exactly)", ops)
	}
}

func TestPlanAmbiguousAdoptIsAnErrorOp(t *testing.T) {
	desired := Desired{Integrations: []map[string]any{item("workday_main", "workday", "Workday", nil)}}
	live := []map[string]any{
		liveEntry("abc123", "workday", "Workday"),
		liveEntry("def456", "workday", "Workday"),
	}
	ops := Plan(desired, live, nil, nil, nil, nil)
	if len(ops) != 1 || ops[0].Kind != KindError {
		t.Fatalf("ops = %+v, want a single error op", ops)
	}
	if !strings.Contains(ops[0].Error, "ambiguous") {
		t.Errorf("error = %q, want it to mention ambiguous", ops[0].Error)
	}
}

func TestPlanAdoptDoesNotClaimAnEntryAlreadyManagedByAnotherKey(t *testing.T) {
	desired := Desired{Integrations: []map[string]any{
		item("first", "workday", "Workday", nil),
		item("second", "workday", "Workday", nil),
	}}
	live := []map[string]any{liveEntry("abc123", "workday", "Workday")}
	managed := map[string]string{"integration:first": "abc123"}
	hashes := map[string]string{"integration:first": HashData(map[string]any{})}
	ops := Plan(desired, live, managed, hashes, nil, nil)

	first := findOp(ops, "first")
	second := findOp(ops, "second")
	if first != nil {
		t.Errorf("first = %+v, want no op (already reconciled, hash unchanged)", first)
	}
	if second == nil || second.Kind != KindCreate {
		t.Fatalf("second = %+v, want a create (the only live match is already claimed by 'first')", second)
	}
}

func TestPlanManagedUnchangedHashEmitsNoOp(t *testing.T) {
	data := map[string]any{"user": map[string]any{"name": "Workday"}}
	desired := Desired{Integrations: []map[string]any{item("workday_main", "workday", "Workday", data)}}
	live := []map[string]any{liveEntry("abc123", "workday", "Workday")}
	managed := map[string]string{"integration:workday_main": "abc123"}
	hashes := map[string]string{"integration:workday_main": HashData(data)}

	ops := Plan(desired, live, managed, hashes, nil, nil)
	if len(ops) != 0 {
		t.Fatalf("ops = %+v, want none (already reconciled)", ops)
	}
}

func TestPlanManagedChangedHashIsAnErrorOp(t *testing.T) {
	desired := Desired{Integrations: []map[string]any{
		item("workday_main", "workday", "Workday", map[string]any{"user": map[string]any{"name": "Changed"}}),
	}}
	live := []map[string]any{liveEntry("abc123", "workday", "Workday")}
	managed := map[string]string{"integration:workday_main": "abc123"}
	hashes := map[string]string{"integration:workday_main": HashData(map[string]any{"user": map[string]any{"name": "Original"}})}

	ops := Plan(desired, live, managed, hashes, nil, nil)
	if len(ops) != 1 || ops[0].Kind != KindError {
		t.Fatalf("ops = %+v, want a single error op", ops)
	}
	if !strings.Contains(ops[0].Error, "changed after it was created") {
		t.Errorf("error = %q, want it to explain the data changed", ops[0].Error)
	}
	// Plan is pure: the error op must not touch managed/hashes.
	if managed["integration:workday_main"] != "abc123" {
		t.Errorf("managed mutated: %+v", managed)
	}
}

func TestPlanManagedButLiveEntryGoneFallsThroughToAdopt(t *testing.T) {
	desired := Desired{Integrations: []map[string]any{item("workday_main", "workday", "Workday", nil)}}
	// The managed entry_id "stale" is gone; a fresh entry with the same
	// domain+title showed up under a new id.
	live := []map[string]any{liveEntry("fresh789", "workday", "Workday")}
	managed := map[string]string{"integration:workday_main": "stale"}

	ops := Plan(desired, live, managed, nil, nil, nil)
	if len(ops) != 1 {
		t.Fatalf("ops = %+v, want 1", ops)
	}
	if ops[0].Kind != KindUpdate || ops[0].LiveID != "fresh789" {
		t.Errorf("op = %+v, want an adopt of the fresh entry", ops[0])
	}
}

func TestPlanManagedButLiveEntryGoneAndNoMatchCreates(t *testing.T) {
	desired := Desired{Integrations: []map[string]any{item("workday_main", "workday", "Workday", nil)}}
	managed := map[string]string{"integration:workday_main": "stale"}

	ops := Plan(desired, nil, managed, nil, nil, nil)
	if len(ops) != 1 || ops[0].Kind != KindCreate {
		t.Fatalf("ops = %+v, want a create", ops)
	}
}

func TestPlanDeletesOnlyManagedRemovedFromManifest(t *testing.T) {
	live := []map[string]any{liveEntry("abc123", "workday", "Workday")}
	managed := map[string]string{"integration:workday_main": "abc123"}

	ops := Plan(Desired{}, live, managed, nil, nil, nil)
	if len(ops) != 1 {
		t.Fatalf("ops = %+v, want 1", ops)
	}
	op := ops[0]
	if op.Kind != KindDelete || op.LiveID != "abc123" || op.Key != "workday_main" {
		t.Errorf("op = %+v", op)
	}
	if len(op.Params) != 0 {
		t.Errorf("delete params = %+v, want empty", op.Params)
	}
}

func TestPlanDeleteSkippedWhenAlreadyGone(t *testing.T) {
	managed := map[string]string{"integration:workday_main": "abc123"}
	ops := Plan(Desired{}, nil, managed, nil, nil, nil)
	if len(ops) != 0 {
		t.Fatalf("ops = %+v, want none (already gone, nothing to delete)", ops)
	}
}

func TestPlanNeverTouchesUnmanagedLiveEntries(t *testing.T) {
	live := []map[string]any{liveEntry("abc123", "workday", "Workday")}
	ops := Plan(Desired{}, live, nil, nil, nil, nil)
	if len(ops) != 0 {
		t.Fatalf("ops = %+v, want none (this agent never managed this entry)", ops)
	}
}

func TestPlanUsesRegistriesRegOpShape(t *testing.T) {
	// Plan's ops must be the shared registries.RegOp type so they slot
	// into pendingRegistry.
	ops := Plan(Desired{}, nil, nil, nil, nil, nil)
	acceptsRegOps(ops)
}

func acceptsRegOps(_ []registries.RegOp) {}

func TestPlanIsPureNeverMutatesInputs(t *testing.T) {
	desired := Desired{Integrations: []map[string]any{item("a", "moon", "Moon", nil)}}
	managed := map[string]string{}
	hashes := map[string]string{}
	before := reflect.DeepEqual(managed, map[string]string{}) && reflect.DeepEqual(hashes, map[string]string{})
	if !before {
		t.Fatal("setup invariant broken")
	}
	Plan(desired, nil, managed, hashes, nil, nil)
	if len(managed) != 0 || len(hashes) != 0 {
		t.Errorf("Plan mutated its managed/hashes inputs: managed=%+v hashes=%+v", managed, hashes)
	}
}

// --- Plan(): secret references ------------------------------------------

func TestPlanResolvesASecretReferenceForThePayloadAndKeepsTheReferenceForState(t *testing.T) {
	declared := map[string]any{"user": map[string]any{"host": "nas.local", "password": "secret://anker_password"}}
	desired := Desired{Integrations: []map[string]any{item("anker", "anker", "Anker", declared)}}
	secrets := secrettest.From(t, "anker_password: "+secrettest.Resolved+"\n")

	ops := Plan(desired, nil, nil, nil, nil, secrets)
	if len(ops) != 1 || ops[0].Kind != KindCreate {
		t.Fatalf("ops = %+v, want one create", ops)
	}
	op := ops[0]

	// The flow payload carries the value...
	data, _ := op.Params["data"].(map[string]any)
	step, _ := data["user"].(map[string]any)
	if step["password"] != secrettest.Resolved {
		t.Errorf("params data = %+v, want the resolved value", data)
	}
	// ...and what regapply snapshots into state.json carries the reference.
	declaredStep, _ := op.Declared["user"].(map[string]any)
	if declaredStep["password"] != "secret://anker_password" {
		t.Errorf("Declared = %+v, want the unresolved reference", op.Declared)
	}
	if !reflect.DeepEqual(op.Secrets, []string{secrettest.Resolved}) {
		t.Errorf("Secrets = %+v, want the resolved value for the applier to redact with", op.Secrets)
	}
}

// Hashing the RESOLVED data is what makes a rotation visible here at all.
func TestPlanHashesTheResolvedDataNotTheReference(t *testing.T) {
	declared := map[string]any{"user": map[string]any{"password": "secret://anker_password"}}
	desired := Desired{Integrations: []map[string]any{item("anker", "anker", "Anker", declared)}}
	live := []map[string]any{liveEntry("abc123", "anker", "Anker")}
	managed := map[string]string{"integration:anker": "abc123"}
	resolvedHash := HashData(map[string]any{"user": map[string]any{"password": secrettest.Resolved}})

	if ops := Plan(desired, live, managed, map[string]string{"integration:anker": resolvedHash},
		nil, secrettest.From(t, "anker_password: "+secrettest.Resolved+"\n")); len(ops) != 0 {
		t.Errorf("ops = %+v, want none: the stored hash is of the resolved data", ops)
	}

	// Rotate the value: the same manifest now hashes differently, and this
	// layer cannot update a live config entry.
	ops := Plan(desired, live, managed, map[string]string{"integration:anker": resolvedHash},
		nil, secrettest.From(t, "anker_password: rotated-value\n"))
	if len(ops) != 1 || ops[0].Kind != KindError {
		t.Fatalf("ops = %+v, want the changed-data refusal", ops)
	}
	if !strings.Contains(ops[0].Error, "changed after it was created") {
		t.Errorf("Error = %q", ops[0].Error)
	}
	// secrets.yaml changed, not the manifest, so "changed" alone would
	// point at a repository diff that is not there.
	if !strings.Contains(ops[0].Error, "referenced secret being rotated in secrets.yaml") {
		t.Errorf("Error = %q, want the rotation named as a possible cause", ops[0].Error)
	}
}

func TestPlanEmitsAPerItemErrorOpForAnUnresolvableSecret(t *testing.T) {
	declared := map[string]any{"user": map[string]any{"password": "secret://anker_password"}}
	desired := Desired{Integrations: []map[string]any{
		item("anker", "anker", "Anker", declared),
		item("workday_main", "workday", "Workday", nil),
	}}
	secrets := secrettest.From(t, "other_key: "+secrettest.Resolved+"\n")

	ops := Plan(desired, nil, nil, nil, nil, secrets)
	if len(ops) != 2 {
		t.Fatalf("ops = %+v, want the broken item plus the healthy one", ops)
	}
	broken := findOp(ops, "anker")
	if broken == nil || broken.Kind != KindError {
		t.Fatalf("ops = %+v, want an error op for the item whose secret is missing", ops)
	}
	if !strings.Contains(broken.Error, "no key 'anker_password'") {
		t.Errorf("Error = %q, want it to name the missing key", broken.Error)
	}
	// One broken declaration must not take the rest of the manifest with it.
	if healthy := findOp(ops, "workday_main"); healthy == nil || healthy.Kind != KindCreate {
		t.Errorf("ops = %+v, want the other integration still planned", ops)
	}
}

// DiffText and Error are everything this layer puts on the dashboard, in
// the activity feed and in the log: a resolved secret must never reach them.
func TestPlanNeverRendersAResolvedSecret(t *testing.T) {
	declared := map[string]any{"user": map[string]any{"password": "secret://anker_password"}}
	secrets := secrettest.From(t, "anker_password: "+secrettest.Resolved+"\n")
	live := []map[string]any{liveEntry("abc123", "anker", "Anker")}

	for _, tc := range []struct {
		what    string
		live    []map[string]any
		managed map[string]string
		hashes  map[string]string
	}{
		{"create", nil, nil, nil},
		{"adopt", live, nil, nil},
		{"changed data", live, map[string]string{"integration:anker": "abc123"}, map[string]string{"integration:anker": "stale"}},
	} {
		desired := Desired{Integrations: []map[string]any{item("anker", "anker", "Anker", declared)}}
		for _, op := range Plan(desired, tc.live, tc.managed, tc.hashes, nil, secrets) {
			if strings.Contains(op.DiffText, secrettest.Resolved) || strings.Contains(op.Error, secrettest.Resolved) {
				t.Errorf("%s: op renders the resolved secret: %+v", tc.what, op)
			}
		}
	}
}
