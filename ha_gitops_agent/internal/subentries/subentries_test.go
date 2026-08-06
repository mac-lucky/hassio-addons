package subentries

import (
	"errors"
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

// writeManifest writes content as <dir>/gitops/subentries.yaml.
func writeManifest(t *testing.T, dir, content string) {
	t.Helper()
	writeFile(t, filepath.Join(dir, "gitops", "subentries.yaml"), content)
}

// problems returns a ManifestError's aggregated text, failing the test if
// err is not one.
func problems(t *testing.T, err error) string {
	t.Helper()
	if err == nil {
		t.Fatal("err = nil, want a ManifestError")
	}
	var manifestErr *ManifestError
	if !errors.As(err, &manifestErr) {
		t.Fatalf("err = %T (%v), want *ManifestError", err, err)
	}
	return manifestErr.Error()
}

// --- LoadManifest ----------------------------------------------------

func TestMissingSubentriesFileIsNotAnError(t *testing.T) {
	dir := t.TempDir()
	desired, err := LoadManifest(dir)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(desired.Subentries) != 0 {
		t.Errorf("subentries = %+v, want empty", desired.Subentries)
	}
}

func TestMissingGitopsDirIsNotAnError(t *testing.T) {
	dir := t.TempDir()
	desired, err := LoadManifest(filepath.Join(dir, "nonexistent"))
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(desired.Subentries) != 0 {
		t.Errorf("subentries = %+v, want empty", desired.Subentries)
	}
}

func TestEmptySubentriesKeyIsNotAnError(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "subentries:\n")
	desired, err := LoadManifest(dir)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(desired.Subentries) != 0 {
		t.Errorf("subentries = %+v, want empty", desired.Subentries)
	}
}

func TestLoadManifestInvalidYAMLIsAnError(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "subentries: [\n")
	if _, err := LoadManifest(dir); err == nil {
		t.Fatal("err = nil, want an error")
	}
}

func TestLoadManifestTopLevelMustBeMapping(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "- not\n- a\n- mapping\n")
	if got := problems(t, mustFail(t, dir)); !strings.Contains(got, "top level must be a mapping") {
		t.Errorf("problems = %q", got)
	}
}

func TestLoadManifestSubentriesMustBeAList(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "subentries:\n  not: a list\n")
	if got := problems(t, mustFail(t, dir)); !strings.Contains(got, "subentries must be a list") {
		t.Errorf("problems = %q", got)
	}
}

func TestLoadManifestItemMustBeMapping(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "subentries:\n  - just_a_string\n")
	if got := problems(t, mustFail(t, dir)); !strings.Contains(got, "subentries[0] is not a mapping") {
		t.Errorf("problems = %q", got)
	}
}

func TestLoadManifestParsesAllKnownFields(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, `subentries:
  - id: calendar_family
    domain: google
    entry_title: Home
    subentry_type: calendar
    match:
      unique_id: fam@group.calendar.google.com
      title: Family
    data:
      user:
        calendar_id: fam@group.calendar.google.com
        name: Family
`)
	desired, err := LoadManifest(dir)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(desired.Subentries) != 1 {
		t.Fatalf("subentries = %+v, want 1 item", desired.Subentries)
	}
	item := desired.Subentries[0]
	want := map[string]any{
		"id": "calendar_family", "domain": "google", "entry_title": "Home",
		"subentry_type": "calendar",
		"match": map[string]any{
			"unique_id": "fam@group.calendar.google.com", "title": "Family",
		},
		"data": map[string]any{
			"user": map[string]any{
				"calendar_id": "fam@group.calendar.google.com", "name": "Family",
			},
		},
	}
	if !reflect.DeepEqual(item, want) {
		t.Errorf("item = %+v, want %+v", item, want)
	}
}

func TestLoadManifestDefaultsDataToEmptyMap(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, `subentries:
  - id: zone_office
    domain: generic_thermostat
    subentry_type: zone
    match: {title: Office}
`)
	desired, err := LoadManifest(dir)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	data, ok := desired.Subentries[0]["data"].(map[string]any)
	if !ok || len(data) != 0 {
		t.Errorf("data = %+v, want an empty map", desired.Subentries[0]["data"])
	}
	// entry_title stays absent rather than defaulting to "", so Plan tells
	// "not declared" from "declared empty" without a sentinel.
	if _, present := desired.Subentries[0]["entry_title"]; present {
		t.Errorf("entry_title present, want absent when not declared")
	}
}

// mustFail runs LoadManifest expecting an error.
func mustFail(t *testing.T, dir string) error {
	t.Helper()
	_, err := LoadManifest(dir)
	if err == nil {
		t.Fatal("err = nil, want an error")
	}
	return err
}

// --- LoadManifest: validation --------------------------------------------

func TestLoadManifestInvalidID(t *testing.T) {
	for _, id := range []string{"Bad-ID", "with space", "UPPER", ""} {
		dir := t.TempDir()
		writeManifest(t, dir, "subentries:\n  - id: \""+id+"\"\n    domain: google\n    subentry_type: calendar\n    match: {title: X}\n")
		if got := problems(t, mustFail(t, dir)); !strings.Contains(got, "invalid or missing 'id'") {
			t.Errorf("id %q: problems = %q", id, got)
		}
	}
}

func TestLoadManifestDuplicateID(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, `subentries:
  - id: dup
    domain: google
    subentry_type: calendar
    match: {title: One}
  - id: dup
    domain: google
    subentry_type: calendar
    match: {title: Two}
`)
	if got := problems(t, mustFail(t, dir)); !strings.Contains(got, "duplicate subentry id 'dup'") {
		t.Errorf("problems = %q", got)
	}
}

func TestLoadManifestMissingDomainIsAnError(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "subentries:\n  - id: a\n    subentry_type: calendar\n    match: {title: X}\n")
	if got := problems(t, mustFail(t, dir)); !strings.Contains(got, "invalid or missing 'domain'") {
		t.Errorf("problems = %q", got)
	}
}

func TestLoadManifestMissingSubentryTypeIsAnError(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "subentries:\n  - id: a\n    domain: google\n    match: {title: X}\n")
	if got := problems(t, mustFail(t, dir)); !strings.Contains(got, "invalid or missing 'subentry_type'") {
		t.Errorf("problems = %q", got)
	}
}

func TestLoadManifestEmptyEntryTitleIsAnError(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "subentries:\n  - id: a\n    domain: google\n    entry_title: \"\"\n    subentry_type: calendar\n    match: {title: X}\n")
	if got := problems(t, mustFail(t, dir)); !strings.Contains(got, "invalid 'entry_title'") {
		t.Errorf("problems = %q", got)
	}
}

func TestLoadManifestUnsupportedFieldIsAnError(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, `subentries:
  - id: a
    domain: google
    subentry_type: calendar
    match: {title: X}
    titel: typo
    extra: nope
`)
	got := problems(t, mustFail(t, dir))
	// Unknown fields are reported sorted and together, so two typos still
	// produce one readable line.
	if !strings.Contains(got, "unsupported field(s) extra, titel") {
		t.Errorf("problems = %q", got)
	}
}

func TestLoadManifestMissingMatchIsAnError(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "subentries:\n  - id: a\n    domain: google\n    subentry_type: calendar\n")
	if got := problems(t, mustFail(t, dir)); !strings.Contains(got, "is missing 'match'") {
		t.Errorf("problems = %q", got)
	}
}

func TestLoadManifestMatchMustBeAMapping(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "subentries:\n  - id: a\n    domain: google\n    subentry_type: calendar\n    match: nope\n")
	if got := problems(t, mustFail(t, dir)); !strings.Contains(got, "match must be a mapping") {
		t.Errorf("problems = %q", got)
	}
}

func TestLoadManifestMatchUnsupportedKeyIsAnError(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "subentries:\n  - id: a\n    domain: google\n    subentry_type: calendar\n    match: {entity_id: sensor.x}\n")
	if got := problems(t, mustFail(t, dir)); !strings.Contains(got, "match has unsupported field(s) entity_id") {
		t.Errorf("problems = %q", got)
	}
}

func TestLoadManifestMatchValueMustBeAString(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "subentries:\n  - id: a\n    domain: google\n    subentry_type: calendar\n    match: {title: 42}\n")
	if got := problems(t, mustFail(t, dir)); !strings.Contains(got, "match title must be a string") {
		t.Errorf("problems = %q", got)
	}
}

func TestLoadManifestMatchNeedsOneNonEmptyValue(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "subentries:\n  - id: a\n    domain: google\n    subentry_type: calendar\n    match: {unique_id: \"\", title: \"\"}\n")
	if got := problems(t, mustFail(t, dir)); !strings.Contains(got, "must declare a non-empty 'unique_id' and/or 'title'") {
		t.Errorf("problems = %q", got)
	}
}

func TestLoadManifestMatchAcceptsEitherKeyAlone(t *testing.T) {
	for _, match := range []string{"{unique_id: abc}", "{title: Family}", "{unique_id: abc, title: Family}"} {
		dir := t.TempDir()
		writeManifest(t, dir, "subentries:\n  - id: a\n    domain: google\n    subentry_type: calendar\n    match: "+match+"\n")
		if _, err := LoadManifest(dir); err != nil {
			t.Errorf("match %s: err = %v, want nil", match, err)
		}
	}
}

func TestLoadManifestDataMustBeAMapping(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "subentries:\n  - id: a\n    domain: google\n    subentry_type: calendar\n    match: {title: X}\n    data: nope\n")
	if got := problems(t, mustFail(t, dir)); !strings.Contains(got, "data must be a mapping") {
		t.Errorf("problems = %q", got)
	}
}

func TestLoadManifestDataStepMustBeAMapping(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, `subentries:
  - id: a
    domain: google
    subentry_type: calendar
    match: {title: X}
    data:
      user: not_a_mapping
`)
	if got := problems(t, mustFail(t, dir)); !strings.Contains(got, "data step 'user' must be a mapping") {
		t.Errorf("problems = %q", got)
	}
}

func TestLoadManifestAggregatesMultipleProblems(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, `subentries:
  - id: first
    subentry_type: calendar
    match: {title: X}
  - id: second
    domain: google
    match: {}
  - id: third
    domain: google
    subentry_type: calendar
    match: {title: X}
    bogus: 1
`)
	got := problems(t, mustFail(t, dir))
	for _, want := range []string{
		"'first' has an invalid or missing 'domain'",
		"'second' has an invalid or missing 'subentry_type'",
		"'second' match must declare a non-empty",
		"'third' has unsupported field(s) bogus",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("problems = %q, missing %q", got, want)
		}
	}
}

func TestLoadManifestValidationFailureReturnsNoItems(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, `subentries:
  - id: good
    domain: google
    subentry_type: calendar
    match: {title: X}
  - id: bad
    domain: google
`)
	desired, err := LoadManifest(dir)
	if err == nil {
		t.Fatal("err = nil, want an error")
	}
	// All-or-nothing: one bad item must not leave a half-applied manifest
	// that silently un-manages the rest.
	if len(desired.Subentries) != 0 {
		t.Errorf("subentries = %+v, want empty on error", desired.Subentries)
	}
}

// --- HashData ------------------------------------------------------------

func TestHashDataDeterministicRegardlessOfKeyOrder(t *testing.T) {
	a := map[string]any{"user": map[string]any{"host": "h", "port": 80}}
	b := map[string]any{"user": map[string]any{"port": 80, "host": "h"}}
	if HashData(a) != HashData(b) {
		t.Errorf("HashData differs across key order: %q vs %q", HashData(a), HashData(b))
	}
}

func TestHashDataDiffersForDifferentValues(t *testing.T) {
	a := map[string]any{"user": map[string]any{"host": "h"}}
	b := map[string]any{"user": map[string]any{"host": "other"}}
	if HashData(a) == HashData(b) {
		t.Errorf("HashData collided for different values: %q", HashData(a))
	}
}

func TestHashDataDiffersAcrossStepIDs(t *testing.T) {
	// The step id is part of the fingerprint: moving the same fields to
	// another flow step is a real change.
	a := map[string]any{"user": map[string]any{"host": "h"}}
	b := map[string]any{"reconfigure": map[string]any{"host": "h"}}
	if HashData(a) == HashData(b) {
		t.Errorf("HashData collided across step ids: %q", HashData(a))
	}
}

func TestHashDataHandlesNilAndEmptyIdentically(t *testing.T) {
	if HashData(nil) != HashData(map[string]any{}) {
		t.Errorf("HashData(nil) = %q, want the empty-map hash %q", HashData(nil), HashData(map[string]any{}))
	}
	if HashData(nil) == "" {
		t.Error("HashData(nil) = empty string, want a hash")
	}
}

func TestHashDataIsStableAcrossCalls(t *testing.T) {
	data := map[string]any{"user": map[string]any{"a": 1, "b": "two", "c": []any{1, 2}}}
	first := HashData(data)
	for i := 0; i < 20; i++ {
		if got := HashData(data); got != first {
			t.Fatalf("HashData call %d = %q, want %q", i, got, first)
		}
	}
}

// --- Plan: fixtures ------------------------------------------------------

// item builds one validated manifest item the way LoadManifest would.
func item(id, domain, entryTitle, subentryType string, match, data map[string]any) map[string]any {
	out := map[string]any{
		"id": id, "domain": domain, "subentry_type": subentryType,
		"match": match, "data": data,
	}
	if entryTitle != "" {
		out["entry_title"] = entryTitle
	}
	if data == nil {
		out["data"] = map[string]any{}
	}
	return out
}

func liveEntry(entryID, domain, title string) map[string]any {
	return map[string]any{"entry_id": entryID, "domain": domain, "title": title}
}

func liveSub(subentryID, subentryType, title, uniqueID string) map[string]any {
	return map[string]any{
		"subentry_id": subentryID, "subentry_type": subentryType,
		"title": title, "unique_id": uniqueID,
	}
}

func findOp(ops []registries.RegOp, key string) *registries.RegOp {
	for i := range ops {
		if ops[i].Key == key {
			return &ops[i]
		}
	}
	return nil
}

// stdData is the declared data most Plan tests use, with its hash.
func stdData() map[string]any {
	return map[string]any{"user": map[string]any{"calendar_id": "fam@example.invalid"}}
}

// --- Plan: create and adopt ----------------------------------------------

func TestPlanCreatesWhenUnmanagedAndNoLiveMatch(t *testing.T) {
	desired := Desired{Subentries: []map[string]any{
		item("cal_family", "google", "", "calendar", map[string]any{"title": "Family"}, stdData()),
	}}
	ops := Plan(desired, []map[string]any{liveEntry("e1", "google", "Home")}, nil, nil, nil, nil, nil)

	if len(ops) != 1 {
		t.Fatalf("ops = %+v, want 1", ops)
	}
	op := ops[0]
	if op.Kind != KindCreate || op.RType != "subentry" || op.Key != "cal_family" {
		t.Errorf("op = %+v", op)
	}
	want := map[string]any{
		"entry_id": "e1", "subentry_type": "calendar", "data": stdData(),
		"match_unique_id": "", "match_title": "Family",
	}
	if !reflect.DeepEqual(op.Params, want) {
		t.Errorf("params = %+v, want %+v", op.Params, want)
	}
	if op.LiveID != "" {
		t.Errorf("LiveID = %q, want empty on a create", op.LiveID)
	}
}

func TestPlanCreateDiffTextNamesFieldsButNotValues(t *testing.T) {
	desired := Desired{Subentries: []map[string]any{
		item("cal_family", "google", "", "calendar",
			map[string]any{"title": "Family"},
			map[string]any{"user": map[string]any{"api_key": "s3cret", "calendar_id": "fam@example.invalid"}}),
	}}
	ops := Plan(desired, []map[string]any{liveEntry("e1", "google", "Home")}, nil, nil, nil, nil, nil)

	diff := ops[0].DiffText
	if !strings.Contains(diff, "step 'user': api_key, calendar_id") {
		t.Errorf("DiffText = %q, want the declared field names", diff)
	}
	if strings.Contains(diff, "s3cret") {
		t.Errorf("DiffText leaked a declared value: %q", diff)
	}
}

func TestPlanAdoptsByUniqueID(t *testing.T) {
	desired := Desired{Subentries: []map[string]any{
		item("cal_family", "google", "", "calendar",
			map[string]any{"unique_id": "fam@example.invalid"}, stdData()),
	}}
	live := map[string][]map[string]any{
		"e1": {liveSub("sub-1", "calendar", "Whatever The UI Says", "fam@example.invalid")},
	}
	ops := Plan(desired, []map[string]any{liveEntry("e1", "google", "Home")}, live, nil, nil, nil, nil)

	if len(ops) != 1 {
		t.Fatalf("ops = %+v, want 1", ops)
	}
	op := ops[0]
	if op.Kind != KindUpdate || op.LiveID != "sub-1" {
		t.Errorf("op = %+v, want an adopt update of sub-1", op)
	}
	want := map[string]any{
		"entry_id": "e1", "subentry_id": "sub-1", "subentry_type": "calendar", "data": stdData(),
	}
	if !reflect.DeepEqual(op.Params, want) {
		t.Errorf("params = %+v, want %+v", op.Params, want)
	}
	// Adoption reconfigures rather than trusting the live subentry unseen.
	if !strings.Contains(op.DiffText, "reconfigure flow will apply the declared data") {
		t.Errorf("DiffText = %q", op.DiffText)
	}
}

func TestPlanAdoptsByTitleWhenUniqueIDMatchesNothing(t *testing.T) {
	desired := Desired{Subentries: []map[string]any{
		item("cal_family", "google", "", "calendar",
			map[string]any{"unique_id": "not-present", "title": "Family"}, stdData()),
	}}
	live := map[string][]map[string]any{
		"e1": {liveSub("sub-1", "calendar", "Family", "some-other-uid")},
	}
	ops := Plan(desired, []map[string]any{liveEntry("e1", "google", "Home")}, live, nil, nil, nil, nil)

	if len(ops) != 1 || ops[0].Kind != KindUpdate || ops[0].LiveID != "sub-1" {
		t.Fatalf("ops = %+v, want a title-matched adopt", ops)
	}
	if !strings.Contains(ops[0].DiffText, "matched by title") {
		t.Errorf("DiffText = %q, want it to name the match field", ops[0].DiffText)
	}
}

func TestPlanPrefersUniqueIDOverTitle(t *testing.T) {
	desired := Desired{Subentries: []map[string]any{
		item("cal_family", "google", "", "calendar",
			map[string]any{"unique_id": "uid-1", "title": "Family"}, stdData()),
	}}
	live := map[string][]map[string]any{"e1": {
		liveSub("sub-title", "calendar", "Family", "other-uid"),
		liveSub("sub-uid", "calendar", "Renamed In The UI", "uid-1"),
	}}
	ops := Plan(desired, []map[string]any{liveEntry("e1", "google", "Home")}, live, nil, nil, nil, nil)

	if len(ops) != 1 || ops[0].LiveID != "sub-uid" {
		t.Fatalf("ops = %+v, want the unique_id match adopted", ops)
	}
	if !strings.Contains(ops[0].DiffText, "matched by unique_id") {
		t.Errorf("DiffText = %q", ops[0].DiffText)
	}
}

func TestPlanAmbiguousAdoptIsAnErrorOp(t *testing.T) {
	desired := Desired{Subentries: []map[string]any{
		item("cal_family", "google", "", "calendar", map[string]any{"title": "Family"}, stdData()),
	}}
	live := map[string][]map[string]any{"e1": {
		liveSub("sub-1", "calendar", "Family", "uid-1"),
		liveSub("sub-2", "calendar", "Family", "uid-2"),
	}}
	ops := Plan(desired, []map[string]any{liveEntry("e1", "google", "Home")}, live, nil, nil, nil, nil)

	if len(ops) != 1 || ops[0].Kind != KindError {
		t.Fatalf("ops = %+v, want one error op", ops)
	}
	if !strings.Contains(ops[0].Error, "ambiguous adopt: 2 live subentries") {
		t.Errorf("Error = %q", ops[0].Error)
	}
}

func TestPlanAdoptIgnoresSubentriesOfAnotherType(t *testing.T) {
	desired := Desired{Subentries: []map[string]any{
		item("cal_family", "google", "", "calendar", map[string]any{"title": "Family"}, stdData()),
	}}
	// Same title, different subentry_type: not a candidate, so this creates
	// rather than adopting the wrong thing.
	live := map[string][]map[string]any{"e1": {liveSub("sub-1", "task_list", "Family", "uid-1")}}
	ops := Plan(desired, []map[string]any{liveEntry("e1", "google", "Home")}, live, nil, nil, nil, nil)

	if len(ops) != 1 || ops[0].Kind != KindCreate {
		t.Fatalf("ops = %+v, want a create", ops)
	}
}

func TestPlanAdoptDoesNotClaimASubentryManagedByAnotherKey(t *testing.T) {
	// cal_family is STILL declared and owns sub-1, so cal_other - matching
	// the same live subentry by title - must create its own.
	desired := Desired{Subentries: []map[string]any{
		item("cal_family", "google", "", "calendar", map[string]any{"title": "Family"}, stdData()),
		item("cal_other", "google", "", "calendar", map[string]any{"title": "Family"}, stdData()),
	}}
	live := map[string][]map[string]any{"e1": {liveSub("sub-1", "calendar", "Family", "uid-1")}}
	managed := map[string]string{"subentry:cal_family": "sub-1"}
	hashes := map[string]string{"subentry:cal_family": HashData(stdData())}

	ops := Plan(desired, []map[string]any{liveEntry("e1", "google", "Home")}, live, managed, hashes, nil, nil)

	create := findOp(ops, "cal_other")
	if create == nil || create.Kind != KindCreate {
		t.Fatalf("ops = %+v, want cal_other to create rather than steal sub-1", ops)
	}
	if findOp(ops, "cal_family") != nil {
		t.Errorf("ops = %+v, want the converged owner to plan nothing", ops)
	}
}

func TestPlanRenamingAManifestIDAdoptsRatherThanDuplicating(t *testing.T) {
	// The rename shape: the old key is gone (releasing sub-1 this same
	// call) and a new key declares the same match. Adopting is the only
	// outcome that does not strand a duplicate this layer cannot delete.
	desired := Desired{Subentries: []map[string]any{
		item("cal_renamed", "google", "", "calendar", map[string]any{"unique_id": "uid-1"}, stdData()),
	}}
	live := map[string][]map[string]any{"e1": {liveSub("sub-1", "calendar", "Family", "uid-1")}}
	managed := map[string]string{"subentry:cal_family": "sub-1"}
	hashes := map[string]string{"subentry:cal_family": HashData(stdData())}

	ops := Plan(desired, []map[string]any{liveEntry("e1", "google", "Home")}, live, managed, hashes, nil, nil)

	adopt := findOp(ops, "cal_renamed")
	if adopt == nil || adopt.Kind != KindUpdate || adopt.LiveID != "sub-1" {
		t.Fatalf("ops = %+v, want cal_renamed to adopt sub-1", ops)
	}
	if adopt.Params["unmanage"] != nil {
		t.Errorf("adopt op carries unmanage: %+v", adopt.Params)
	}
	unmanage := findOp(ops, "cal_family")
	if unmanage == nil || unmanage.Params["unmanage"] != true {
		t.Fatalf("ops = %+v, want the old key un-managed", ops)
	}
}

func TestPlanRefusesToAdoptWhenTheDeclaredUniqueIDIsOwnedByAnotherKey(t *testing.T) {
	// uid-1 belongs to a still-declared key, and this item's title matches
	// a DIFFERENT live subentry: falling back would write its data onto
	// the wrong object.
	desired := Desired{Subentries: []map[string]any{
		item("cal_family", "google", "", "calendar", map[string]any{"unique_id": "uid-1"}, stdData()),
		item("cal_other", "google", "", "calendar",
			map[string]any{"unique_id": "uid-1", "title": "Shared"}, stdData()),
	}}
	live := map[string][]map[string]any{"e1": {
		liveSub("sub-1", "calendar", "Family", "uid-1"),
		liveSub("sub-2", "calendar", "Shared", "uid-2"),
	}}
	managed := map[string]string{"subentry:cal_family": "sub-1"}
	hashes := map[string]string{"subentry:cal_family": HashData(stdData())}

	ops := Plan(desired, []map[string]any{liveEntry("e1", "google", "Home")}, live, managed, hashes, nil, nil)

	op := findOp(ops, "cal_other")
	if op == nil || op.Kind != KindError {
		t.Fatalf("ops = %+v, want cal_other refused rather than adopting sub-2", ops)
	}
	if !strings.Contains(op.Error, "already managed by another manifest entry") {
		t.Errorf("Error = %q", op.Error)
	}
}

func TestPlanBlockedByPriorFailureBlocksAnAdoptToo(t *testing.T) {
	desired := Desired{Subentries: []map[string]any{
		item("cal_family", "google", "", "calendar", map[string]any{"title": "Family"}, stdData()),
	}}
	live := map[string][]map[string]any{"e1": {liveSub("sub-1", "calendar", "Family", "uid-1")}}
	attempts := map[string]map[string]any{
		"subentry:cal_family": {"hash": HashData(stdData()), "error": "step 'user' rejected calendar_id"},
	}

	ops := Plan(desired, []map[string]any{liveEntry("e1", "google", "Home")}, live, nil, nil, attempts, nil)

	if len(ops) != 1 || ops[0].Kind != KindError {
		t.Fatalf("ops = %+v, want the adopt blocked by the recorded failure", ops)
	}
	if !strings.Contains(ops[0].Error, "previous attempt failed") {
		t.Errorf("Error = %q", ops[0].Error)
	}
}

func TestPlanAdoptRejectsSubentryWithoutUsableID(t *testing.T) {
	desired := Desired{Subentries: []map[string]any{
		item("cal_family", "google", "", "calendar", map[string]any{"title": "Family"}, stdData()),
	}}
	live := map[string][]map[string]any{"e1": {{"subentry_type": "calendar", "title": "Family"}}}
	ops := Plan(desired, []map[string]any{liveEntry("e1", "google", "Home")}, live, nil, nil, nil, nil)

	if len(ops) != 1 || ops[0].Kind != KindError {
		t.Fatalf("ops = %+v, want one error op", ops)
	}
	if !strings.Contains(ops[0].Error, "no usable subentry_id") {
		t.Errorf("Error = %q", ops[0].Error)
	}
}

// --- Plan: managed keys --------------------------------------------------

// managedFixture is the common "already managed and live" starting point:
// one declared item, one parent entry, one live subentry under it.
func managedFixture(data map[string]any) (Desired, []map[string]any, map[string][]map[string]any, map[string]string) {
	desired := Desired{Subentries: []map[string]any{
		item("cal_family", "google", "", "calendar", map[string]any{"unique_id": "uid-1"}, data),
	}}
	entries := []map[string]any{liveEntry("e1", "google", "Home")}
	live := map[string][]map[string]any{"e1": {liveSub("sub-1", "calendar", "Family", "uid-1")}}
	managed := map[string]string{"subentry:cal_family": "sub-1"}
	return desired, entries, live, managed
}

func TestPlanManagedUnchangedHashEmitsNoOp(t *testing.T) {
	desired, entries, live, managed := managedFixture(stdData())
	hashes := map[string]string{"subentry:cal_family": HashData(stdData())}

	ops := Plan(desired, entries, live, managed, hashes, nil, nil)
	if len(ops) != 0 {
		t.Errorf("ops = %+v, want none", ops)
	}
}

func TestPlanManagedChangedHashEmitsReconfigureUpdate(t *testing.T) {
	newData := map[string]any{"user": map[string]any{"calendar_id": "changed@example.invalid"}}
	desired, entries, live, managed := managedFixture(newData)
	hashes := map[string]string{"subentry:cal_family": HashData(stdData())}

	ops := Plan(desired, entries, live, managed, hashes, nil, nil)
	if len(ops) != 1 {
		t.Fatalf("ops = %+v, want 1", ops)
	}
	op := ops[0]
	if op.Kind != KindUpdate || op.RType != "subentry" || op.LiveID != "sub-1" {
		t.Errorf("op = %+v", op)
	}
	want := map[string]any{
		"entry_id": "e1", "subentry_id": "sub-1", "subentry_type": "calendar", "data": newData,
	}
	if !reflect.DeepEqual(op.Params, want) {
		t.Errorf("params = %+v, want %+v", op.Params, want)
	}
	if !strings.Contains(op.DiffText, "changed; a reconfigure flow will re-submit it") {
		t.Errorf("DiffText = %q", op.DiffText)
	}
	if strings.Contains(op.DiffText, "changed@example.invalid") {
		t.Errorf("DiffText leaked a declared value: %q", op.DiffText)
	}
}

func TestPlanManagedMissingStoredHashReconfigures(t *testing.T) {
	// Deliberately unlike internal/flows, which treats a missing hash as
	// converged: re-applying is idempotent, so a truncated state file
	// recovers instead of stranding the item.
	desired, entries, live, managed := managedFixture(stdData())

	ops := Plan(desired, entries, live, managed, map[string]string{}, nil, nil)
	if len(ops) != 1 || ops[0].Kind != KindUpdate {
		t.Fatalf("ops = %+v, want one reconfigure update", ops)
	}
}

func TestPlanManagedRecoversParentFromLivePlacement(t *testing.T) {
	// entry_title no longer matches the live entry's title, and a managed,
	// still-live subentry must not care: its parent comes from where it
	// actually lives.
	desired := Desired{Subentries: []map[string]any{
		item("cal_family", "google", "Renamed Since", "calendar", map[string]any{"unique_id": "uid-1"},
			map[string]any{"user": map[string]any{"calendar_id": "new@example.invalid"}}),
	}}
	entries := []map[string]any{liveEntry("e1", "google", "Home")}
	live := map[string][]map[string]any{"e1": {liveSub("sub-1", "calendar", "Family", "uid-1")}}
	managed := map[string]string{"subentry:cal_family": "sub-1"}
	hashes := map[string]string{"subentry:cal_family": HashData(stdData())}

	ops := Plan(desired, entries, live, managed, hashes, nil, nil)
	if len(ops) != 1 || ops[0].Kind != KindUpdate {
		t.Fatalf("ops = %+v, want one reconfigure update", ops)
	}
	if ops[0].Params["entry_id"] != "e1" {
		t.Errorf("entry_id = %v, want e1 from live placement", ops[0].Params["entry_id"])
	}
}

func TestPlanManagedUnchangedHashSurvivesParentRename(t *testing.T) {
	desired := Desired{Subentries: []map[string]any{
		item("cal_family", "google", "Renamed Since", "calendar", map[string]any{"unique_id": "uid-1"}, stdData()),
	}}
	entries := []map[string]any{liveEntry("e1", "google", "Home")}
	live := map[string][]map[string]any{"e1": {liveSub("sub-1", "calendar", "Family", "uid-1")}}
	managed := map[string]string{"subentry:cal_family": "sub-1"}
	hashes := map[string]string{"subentry:cal_family": HashData(stdData())}

	if ops := Plan(desired, entries, live, managed, hashes, nil, nil); len(ops) != 0 {
		t.Errorf("ops = %+v, want none", ops)
	}
}

func TestPlanManagedButLiveSubentryGoneFallsThroughToAdopt(t *testing.T) {
	desired := Desired{Subentries: []map[string]any{
		item("cal_family", "google", "", "calendar", map[string]any{"unique_id": "uid-1"}, stdData()),
	}}
	entries := []map[string]any{liveEntry("e1", "google", "Home")}
	// The managed subentry_id is gone and a different live subentry now
	// carries the declared unique_id: deleted and re-added in the UI.
	live := map[string][]map[string]any{"e1": {liveSub("sub-new", "calendar", "Family", "uid-1")}}
	managed := map[string]string{"subentry:cal_family": "sub-old"}
	hashes := map[string]string{"subentry:cal_family": HashData(stdData())}

	ops := Plan(desired, entries, live, managed, hashes, nil, nil)
	if len(ops) != 1 || ops[0].Kind != KindUpdate || ops[0].LiveID != "sub-new" {
		t.Fatalf("ops = %+v, want an adopt of sub-new", ops)
	}
	if !strings.Contains(ops[0].DiffText, "adopted existing subentry") {
		t.Errorf("DiffText = %q", ops[0].DiffText)
	}
}

func TestPlanManagedButLiveSubentryGoneAndNoMatchCreates(t *testing.T) {
	desired := Desired{Subentries: []map[string]any{
		item("cal_family", "google", "", "calendar", map[string]any{"unique_id": "uid-1"}, stdData()),
	}}
	entries := []map[string]any{liveEntry("e1", "google", "Home")}
	managed := map[string]string{"subentry:cal_family": "sub-old"}
	hashes := map[string]string{"subentry:cal_family": HashData(stdData())}

	ops := Plan(desired, entries, nil, managed, hashes, nil, nil)
	if len(ops) != 1 || ops[0].Kind != KindCreate {
		t.Fatalf("ops = %+v, want a create", ops)
	}
}

// --- Plan: failure memory ------------------------------------------------

func TestPlanBlockedByPriorFailureAtSameHashEmitsErrorOpNotCreate(t *testing.T) {
	desired := Desired{Subentries: []map[string]any{
		item("cal_family", "google", "", "calendar", map[string]any{"title": "Family"}, stdData()),
	}}
	attempts := map[string]map[string]any{
		"subentry:cal_family": {"hash": HashData(stdData()), "error": "step 'user' rejected field 'calendar_id'"},
	}

	ops := Plan(desired, []map[string]any{liveEntry("e1", "google", "Home")}, nil, nil, nil, attempts, nil)
	if len(ops) != 1 || ops[0].Kind != KindError {
		t.Fatalf("ops = %+v, want one error op", ops)
	}
	if !strings.Contains(ops[0].Error, "previous attempt failed: step 'user' rejected field 'calendar_id'") {
		t.Errorf("Error = %q", ops[0].Error)
	}
}

func TestPlanBlockedByPriorFailureBlocksAnUpdateToo(t *testing.T) {
	// Why this layer blocks updates where internal/flows blocks only
	// creates: a doomed reconfigure would re-drive a flow every cycle.
	newData := map[string]any{"user": map[string]any{"calendar_id": "bad@example.invalid"}}
	desired, entries, live, managed := managedFixture(newData)
	hashes := map[string]string{"subentry:cal_family": HashData(stdData())}
	attempts := map[string]map[string]any{
		"subentry:cal_family": {"hash": HashData(newData), "error": "reconfigure rejected"},
	}

	ops := Plan(desired, entries, live, managed, hashes, attempts, nil)
	if len(ops) != 1 || ops[0].Kind != KindError {
		t.Fatalf("ops = %+v, want one error op", ops)
	}
	if !strings.Contains(ops[0].Error, "previous attempt failed: reconfigure rejected") {
		t.Errorf("Error = %q", ops[0].Error)
	}
}

func TestPlanNotBlockedWhenDeclaredDataHashChanged(t *testing.T) {
	desired := Desired{Subentries: []map[string]any{
		item("cal_family", "google", "", "calendar", map[string]any{"title": "Family"},
			map[string]any{"user": map[string]any{"calendar_id": "fixed@example.invalid"}}),
	}}
	attempts := map[string]map[string]any{
		"subentry:cal_family": {"hash": HashData(stdData()), "error": "old failure"},
	}

	ops := Plan(desired, []map[string]any{liveEntry("e1", "google", "Home")}, nil, nil, nil, attempts, nil)
	if len(ops) != 1 || ops[0].Kind != KindCreate {
		t.Fatalf("ops = %+v, want a create (the manifest was edited since the failure)", ops)
	}
}

func TestPlanBlockedFailureWithoutStoredErrorTextStillReports(t *testing.T) {
	desired := Desired{Subentries: []map[string]any{
		item("cal_family", "google", "", "calendar", map[string]any{"title": "Family"}, stdData()),
	}}
	attempts := map[string]map[string]any{"subentry:cal_family": {"hash": HashData(stdData())}}

	ops := Plan(desired, []map[string]any{liveEntry("e1", "google", "Home")}, nil, nil, nil, attempts, nil)
	if len(ops) != 1 || ops[0].Kind != KindError {
		t.Fatalf("ops = %+v, want one error op", ops)
	}
	if !strings.Contains(ops[0].Error, "unknown error") {
		t.Errorf("Error = %q", ops[0].Error)
	}
}

func TestPlanIgnoresAttemptsForAKeyThatIsNowManaged(t *testing.T) {
	// A stale attempts entry must not block an already-converged key:
	// hashes wins, and internal/regapply cleans the entry up.
	desired, entries, live, managed := managedFixture(stdData())
	hashes := map[string]string{"subentry:cal_family": HashData(stdData())}
	attempts := map[string]map[string]any{
		"subentry:cal_family": {"hash": HashData(stdData()), "error": "stale"},
	}

	if ops := Plan(desired, entries, live, managed, hashes, attempts, nil); len(ops) != 0 {
		t.Errorf("ops = %+v, want none", ops)
	}
}

// --- Plan: unmanage, parent resolution, type changes ---------------------

func TestPlanUnmanagesUndeclaredKeysInSortedOrder(t *testing.T) {
	managed := map[string]string{
		"subentry:zzz": "sub-z", "subentry:aaa": "sub-a", "subentry:mmm": "sub-m",
	}
	ops := Plan(Desired{}, []map[string]any{liveEntry("e1", "google", "Home")}, nil, managed, nil, nil, nil)

	if len(ops) != 3 {
		t.Fatalf("ops = %+v, want three unmanage ops", ops)
	}
	var keys []string
	for _, op := range ops {
		keys = append(keys, op.Key)
		if op.Kind != KindUpdate || op.Params["unmanage"] != true {
			t.Errorf("op %+v is not an unmanage", op)
		}
		if !strings.Contains(op.DiffText, "left untouched") {
			t.Errorf("DiffText = %q, want it to say the live subentry is untouched", op.DiffText)
		}
	}
	if got := strings.Join(keys, ","); got != "aaa,mmm,zzz" {
		t.Errorf("unmanage order = %s, want sorted", got)
	}
	if ops[0].LiveID != "sub-a" {
		t.Errorf("LiveID = %q, want the live subentry_id carried through", ops[0].LiveID)
	}
}

func TestPlanUnmanagesEvenWhenTheLiveSubentryIsAlreadyGone(t *testing.T) {
	// The state key still has to go, or the layer keeps claiming a
	// subentry that no longer exists.
	managed := map[string]string{"subentry:cal_family": "sub-gone"}
	ops := Plan(Desired{}, []map[string]any{liveEntry("e1", "google", "Home")},
		map[string][]map[string]any{"e1": {}}, managed, nil, nil, nil)

	if len(ops) != 1 || ops[0].Params["unmanage"] != true {
		t.Fatalf("ops = %+v, want one unmanage op", ops)
	}
}

func TestPlanDoesNotUnmanageAKeyThatIsStillDeclared(t *testing.T) {
	desired := Desired{Subentries: []map[string]any{
		item("cal_family", "google", "", "calendar", map[string]any{"unique_id": "uid-1"}, stdData()),
	}}
	live := map[string][]map[string]any{"e1": {liveSub("sub-1", "calendar", "Family", "uid-1")}}
	managed := map[string]string{"subentry:cal_family": "sub-1"}
	hashes := map[string]string{"subentry:cal_family": HashData(stdData())}

	if ops := Plan(desired, []map[string]any{liveEntry("e1", "google", "Home")}, live, managed, hashes, nil, nil); len(ops) != 0 {
		t.Fatalf("ops = %+v, want a converged managed key to plan nothing", ops)
	}
}

func TestPlanParentResolutionFailures(t *testing.T) {
	cases := []struct {
		name    string
		entries []map[string]any
		item    map[string]any
		want    string
	}{
		{
			name:    "no entry for the domain",
			entries: []map[string]any{liveEntry("e1", "hue", "Hue")},
			item:    item("cal_family", "google", "", "calendar", map[string]any{"title": "Family"}, stdData()),
			want:    "no live integration entry for domain 'google'",
		},
		{
			name: "two entries and no entry_title to pick one",
			entries: []map[string]any{
				liveEntry("e1", "google", "Home"), liveEntry("e2", "google", "Work"),
			},
			item: item("cal_family", "google", "", "calendar", map[string]any{"title": "Family"}, stdData()),
			want: "ambiguous parent: 2 live integration entries",
		},
		{
			name:    "entry_title matches nothing",
			entries: []map[string]any{liveEntry("e1", "google", "Home")},
			item:    item("cal_family", "google", "Work", "calendar", map[string]any{"title": "Family"}, stdData()),
			want:    "no live integration entry for domain 'google' titled 'Work'",
		},
		{
			name:    "entry carries no usable entry_id",
			entries: []map[string]any{{"domain": "google", "title": "Home"}},
			item:    item("cal_family", "google", "", "calendar", map[string]any{"title": "Family"}, stdData()),
			want:    "has no usable entry_id field",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ops := Plan(Desired{Subentries: []map[string]any{tc.item}}, tc.entries, nil, nil, nil, nil, nil)
			if len(ops) != 1 || ops[0].Kind != KindError {
				t.Fatalf("ops = %+v, want one error op", ops)
			}
			if !strings.Contains(ops[0].Error, tc.want) {
				t.Errorf("Error = %q, want it to contain %q", ops[0].Error, tc.want)
			}
		})
	}
}

func TestPlanEntryTitleNarrowsToTheRightParent(t *testing.T) {
	desired := Desired{Subentries: []map[string]any{
		item("cal_work", "google", "Work", "calendar", map[string]any{"title": "Family"}, stdData()),
	}}
	entries := []map[string]any{liveEntry("e1", "google", "Home"), liveEntry("e2", "google", "Work")}

	ops := Plan(desired, entries, nil, nil, nil, nil, nil)

	if len(ops) != 1 || ops[0].Kind != KindCreate {
		t.Fatalf("ops = %+v, want a create under the narrowed parent", ops)
	}
	if ops[0].Params["entry_id"] != "e2" {
		t.Errorf("entry_id = %v, want e2", ops[0].Params["entry_id"])
	}
}

func TestPlanRefusesAManagedSubentryWhoseDeclaredTypeChanged(t *testing.T) {
	// The hash covers only the declared data, so without this guard a type
	// edit stays invisible until the data changes, and the reconfigure
	// then drives the new type against the old subentry.
	desired := Desired{Subentries: []map[string]any{
		item("cal_family", "google", "", "task_list", map[string]any{"unique_id": "uid-1"}, stdData()),
	}}
	live := map[string][]map[string]any{"e1": {liveSub("sub-1", "calendar", "Family", "uid-1")}}
	managed := map[string]string{"subentry:cal_family": "sub-1"}
	hashes := map[string]string{"subentry:cal_family": HashData(stdData())}

	ops := Plan(desired, []map[string]any{liveEntry("e1", "google", "Home")}, live, managed, hashes, nil, nil)

	if len(ops) != 1 || ops[0].Kind != KindError {
		t.Fatalf("ops = %+v, want the type change refused", ops)
	}
	if !strings.Contains(ops[0].Error, "cannot change type") {
		t.Errorf("Error = %q", ops[0].Error)
	}
}

func TestValidateMatchReportsUnusableEvenAlongsideAnUnknownKey(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, `
subentries:
  - id: cal_family
    domain: google
    subentry_type: calendar
    match:
      entity_id: calendar.family
`)
	_, err := LoadManifest(dir)
	if err == nil {
		t.Fatal("LoadManifest succeeded, want a validation error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "unsupported field(s) entity_id") {
		t.Errorf("error = %q, want the unknown key reported", msg)
	}
	if !strings.Contains(msg, "must declare a non-empty 'unique_id' and/or 'title'") {
		t.Errorf("error = %q, want the unusable match reported too", msg)
	}
}

// --- Plan: secret references ---------------------------------------------

// secretItem is the manifest item every test below plans, declaring one
// field as a reference.
func secretItem() map[string]any {
	return item("widget_hall", "pushward", "", "widget",
		map[string]any{"title": "Hall"},
		map[string]any{"user": map[string]any{"name": "Hall", "api_key": "secret://pushward_key"}})
}

func TestPlanResolvesASecretReferenceForThePayloadAndTheHash(t *testing.T) {
	desired := Desired{Subentries: []map[string]any{secretItem()}}
	entries := []map[string]any{liveEntry("e1", "pushward", "PushWard")}
	secrets := secrettest.From(t, "pushward_key: "+secrettest.Resolved+"\n")

	ops := Plan(desired, entries, nil, nil, nil, nil, secrets)
	if len(ops) != 1 || ops[0].Kind != KindCreate {
		t.Fatalf("ops = %+v, want one create", ops)
	}
	op := ops[0]

	data, _ := op.Params["data"].(map[string]any)
	step, _ := data["user"].(map[string]any)
	if step["api_key"] != secrettest.Resolved {
		t.Errorf("params data = %+v, want the resolved value on the wire", data)
	}
	if !reflect.DeepEqual(op.Secrets, []string{secrettest.Resolved}) {
		t.Errorf("Secrets = %+v, want the resolved value for the driver to redact with", op.Secrets)
	}
	// No Declared here, unlike internal/flows: this layer persists and
	// stashes nothing, so an unresolved copy would be dead weight.
	if op.Declared != nil {
		t.Errorf("Declared = %+v, want nil: nothing in this layer reads it", op.Declared)
	}
}

// The point of hashing the resolved data: rotate the value in secrets.yaml
// and the same, unedited manifest converges the live subentry onto it.
func TestPlanReconfiguresWhenTheSecretRotates(t *testing.T) {
	desired := Desired{Subentries: []map[string]any{secretItem()}}
	entries := []map[string]any{liveEntry("e1", "pushward", "PushWard")}
	live := map[string][]map[string]any{"e1": {liveSub("sub-1", "widget", "Hall", "hall")}}
	managed := map[string]string{"subentry:widget_hall": "sub-1"}
	appliedHash := HashData(map[string]any{"user": map[string]any{"name": "Hall", "api_key": secrettest.Resolved}})
	hashes := map[string]string{"subentry:widget_hall": appliedHash}

	// Unchanged secret, unchanged manifest: converged.
	if ops := Plan(desired, entries, live, managed, hashes, nil,
		secrettest.From(t, "pushward_key: "+secrettest.Resolved+"\n")); len(ops) != 0 {
		t.Fatalf("ops = %+v, want none while nothing has changed", ops)
	}

	// Same manifest, rotated secret: a reconfigure re-submits it.
	ops := Plan(desired, entries, live, managed, hashes, nil, secrettest.From(t, "pushward_key: rotated-value\n"))
	if len(ops) != 1 || ops[0].Kind != KindUpdate || ops[0].LiveID != "sub-1" {
		t.Fatalf("ops = %+v, want a reconfigure of sub-1", ops)
	}
	data, _ := ops[0].Params["data"].(map[string]any)
	step, _ := data["user"].(map[string]any)
	if step["api_key"] != "rotated-value" {
		t.Errorf("params data = %+v, want the rotated value", data)
	}
}

func TestPlanEmitsAPerItemErrorOpForAnUnresolvableSecret(t *testing.T) {
	healthy := item("cal_family", "google", "", "calendar",
		map[string]any{"title": "Family"}, stdData())
	desired := Desired{Subentries: []map[string]any{secretItem(), healthy}}
	entries := []map[string]any{
		liveEntry("e1", "pushward", "PushWard"),
		liveEntry("e2", "google", "Home"),
	}

	ops := Plan(desired, entries, nil, nil, nil, nil, secrettest.From(t, "other_key: "+secrettest.Resolved+"\n"))
	broken := findOp(ops, "widget_hall")
	if broken == nil || broken.Kind != KindError {
		t.Fatalf("ops = %+v, want an error op for the item whose secret is missing", ops)
	}
	if !strings.Contains(broken.Error, "no key 'pushward_key'") {
		t.Errorf("Error = %q, want it to name the missing key", broken.Error)
	}
	if ok := findOp(ops, "cal_family"); ok == nil || ok.Kind != KindCreate {
		t.Errorf("ops = %+v, want the other subentry still planned", ops)
	}
}

func TestPlanNeverRendersAResolvedSecret(t *testing.T) {
	secrets := secrettest.From(t, "pushward_key: "+secrettest.Resolved+"\n")
	entries := []map[string]any{liveEntry("e1", "pushward", "PushWard")}
	live := map[string][]map[string]any{"e1": {liveSub("sub-1", "widget", "Hall", "hall")}}

	for _, tc := range []struct {
		what    string
		live    map[string][]map[string]any
		managed map[string]string
		hashes  map[string]string
	}{
		{"create", nil, nil, nil},
		{"adopt", live, nil, nil},
		{"reconfigure", live, map[string]string{"subentry:widget_hall": "sub-1"}, map[string]string{"subentry:widget_hall": "stale"}},
	} {
		desired := Desired{Subentries: []map[string]any{secretItem()}}
		ops := Plan(desired, entries, tc.live, tc.managed, tc.hashes, nil, secrets)
		if len(ops) == 0 {
			t.Fatalf("%s: no op planned, so this check proves nothing", tc.what)
		}
		for _, op := range ops {
			if strings.Contains(op.DiffText, secrettest.Resolved) || strings.Contains(op.Error, secrettest.Resolved) {
				t.Errorf("%s: op renders the resolved secret: %+v", tc.what, op)
			}
			// The field NAME is still shown: it is the whole value of the
			// diff line, and it is not a secret.
			if op.DiffText != "" && !strings.Contains(op.DiffText, "api_key") {
				t.Errorf("%s: DiffText = %q, want the declared field names", tc.what, op.DiffText)
			}
		}
	}
}
