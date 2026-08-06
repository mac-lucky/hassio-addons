package registries

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("writing %s: %v", name, err)
	}
}

func mkGitops(t *testing.T) (workdir, gitops string) {
	t.Helper()
	workdir = t.TempDir()
	gitops = filepath.Join(workdir, "gitops")
	if err := os.Mkdir(gitops, 0o755); err != nil {
		t.Fatalf("mkdir gitops: %v", err)
	}
	return workdir, gitops
}

// --- LoadManifests(): missing/empty gitops/ --------------------------------

func TestMissingGitopsDirIsNotAnError(t *testing.T) {
	workdir := t.TempDir()
	got, err := LoadManifests(workdir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !reflect.DeepEqual(got, emptyDesired()) {
		t.Errorf("got %+v, want empty Desired", got)
	}
}

func TestEmptyGitopsDirIsNotAnError(t *testing.T) {
	workdir, _ := mkGitops(t)
	got, err := LoadManifests(workdir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !reflect.DeepEqual(got, emptyDesired()) {
		t.Errorf("got %+v, want empty Desired", got)
	}
}

func TestRegistriesYAMLWithoutHelpersYAML(t *testing.T) {
	workdir, gitops := mkGitops(t)
	writeFile(t, gitops, "registries.yaml", "floors:\n  - id: ground\n    name: Ground floor\n")

	desired, err := LoadManifests(workdir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := []map[string]any{{"id": "ground", "name": "Ground floor"}}
	if !reflect.DeepEqual(desired.Floors, want) {
		t.Errorf("floors = %+v, want %+v", desired.Floors, want)
	}
	if len(desired.Areas) != 0 || len(desired.Labels) != 0 || len(desired.Helpers) != 0 {
		t.Errorf("expected empty areas/labels/helpers, got %+v", desired)
	}
}

func TestUnknownPerItemFieldsPassThroughUntouched(t *testing.T) {
	workdir, gitops := mkGitops(t)
	writeFile(t, gitops, "registries.yaml", `
floors:
  - id: ground
    name: Ground floor
    icon: mdi:home
    level: 0
`)

	desired, err := LoadManifests(workdir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []map[string]any{{"id": "ground", "name": "Ground floor", "icon": "mdi:home", "level": 0}}
	if !reflect.DeepEqual(desired.Floors, want) {
		t.Errorf("floors = %+v, want %+v", desired.Floors, want)
	}
}

// --- LoadManifests(): validation --------------------------------------------

func TestInvalidYAMLSyntaxReturnsManifestError(t *testing.T) {
	workdir, gitops := mkGitops(t)
	writeFile(t, gitops, "registries.yaml", "floors: [this is not: valid: yaml")

	_, err := LoadManifests(workdir)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "invalid YAML") {
		t.Errorf("error = %q, want it to contain %q", err.Error(), "invalid YAML")
	}
}

func TestManifestErrorAggregatesEveryProblemNotJustTheFirst(t *testing.T) {
	workdir, gitops := mkGitops(t)
	writeFile(t, gitops, "registries.yaml", `
floors:
  - id: Ground
    name: Ground floor
areas:
  - id: living_room
    name: Living room
    floor: nonexistent
labels:
  - id: gitops
    name: GitOps
  - id: gitops
    name: GitOps Dup
`)
	writeFile(t, gitops, "helpers.yaml", `
input_foo:
  - id: x
    name: X
`)

	_, err := LoadManifests(workdir)
	if err == nil {
		t.Fatal("expected an error")
	}
	message := err.Error()
	for _, want := range []string{
		"invalid or missing 'id'",
		"unknown floor id 'nonexistent'",
		"duplicate label id 'gitops'",
		"unknown helper domain 'input_foo'",
	} {
		if !strings.Contains(message, want) {
			t.Errorf("message %q does not contain %q", message, want)
		}
	}
}

// --- Plan(): create ----------------------------------------------------

func TestCreateNew(t *testing.T) {
	desired := Desired{Floors: []map[string]any{{"id": "ground", "name": "Ground floor", "icon": "mdi:home"}}}

	ops := Plan(desired, nil, nil)

	if len(ops) != 1 {
		t.Fatalf("got %d ops, want 1", len(ops))
	}
	op := ops[0]
	if op.Kind != KindCreate || op.RType != "floor" || op.Key != "ground" || op.LiveID != "" || op.Error != "" {
		t.Errorf("unexpected op: %+v", op)
	}
	want := map[string]any{"name": "Ground floor", "icon": "mdi:home"}
	if !reflect.DeepEqual(op.Params, want) {
		t.Errorf("params = %+v, want %+v", op.Params, want)
	}
	if !strings.Contains(op.DiffText, "+name: 'Ground floor'") {
		t.Errorf("diff_text = %q, missing expected line", op.DiffText)
	}
}

// --- Plan(): adopt-by-name ----------------------------------------------

func TestAdoptByUniqueName(t *testing.T) {
	desired := Desired{Floors: []map[string]any{{"id": "ground", "name": "Ground floor"}}}
	live := map[string][]map[string]any{"floor": {{"floor_id": "abc123", "name": "Ground floor"}}}

	ops := Plan(desired, live, nil)

	if len(ops) != 1 {
		t.Fatalf("got %d ops, want 1", len(ops))
	}
	op := ops[0]
	if op.Kind != KindUpdate || op.RType != "floor" || op.Key != "ground" || op.LiveID != "abc123" || op.Error != "" {
		t.Errorf("unexpected op: %+v", op)
	}
	if !strings.Contains(op.DiffText, "adopted existing floor 'ground'") {
		t.Errorf("diff_text = %q", op.DiffText)
	}
}

func TestAdoptByUniqueNameAlsoReportsDrift(t *testing.T) {
	desired := Desired{Floors: []map[string]any{{"id": "ground", "name": "Ground floor", "level": 1}}}
	live := map[string][]map[string]any{"floor": {{"floor_id": "abc123", "name": "Ground floor", "level": 0}}}

	ops := Plan(desired, live, nil)

	if len(ops) != 1 {
		t.Fatalf("got %d ops, want 1", len(ops))
	}
	op := ops[0]
	if op.Kind != KindUpdate || op.LiveID != "abc123" {
		t.Errorf("unexpected op: %+v", op)
	}
	if !strings.Contains(op.DiffText, "-level: 0") || !strings.Contains(op.DiffText, "+level: 1") {
		t.Errorf("diff_text = %q", op.DiffText)
	}
}

func TestAmbiguousAdoptBecomesPerItemError(t *testing.T) {
	desired := Desired{Labels: []map[string]any{{"id": "gitops", "name": "GitOps"}}}
	live := map[string][]map[string]any{"label": {
		{"label_id": "l1", "name": "GitOps"},
		{"label_id": "l2", "name": "GitOps"},
	}}

	ops := Plan(desired, live, nil)

	if len(ops) != 1 {
		t.Fatalf("got %d ops, want 1", len(ops))
	}
	op := ops[0]
	if op.Kind != KindError || op.RType != "label" || op.Key != "gitops" || op.LiveID != "" {
		t.Errorf("unexpected op: %+v", op)
	}
	if len(op.Params) != 0 {
		t.Errorf("params = %+v, want empty", op.Params)
	}
	if op.DiffText != "" {
		t.Errorf("diff_text = %q, want empty", op.DiffText)
	}
	if !strings.Contains(op.Error, "ambiguous adopt") {
		t.Errorf("error = %q", op.Error)
	}
}

// --- Plan(): already-managed items ---------------------------------------

func TestRecreateAfterUserDeletedLive(t *testing.T) {
	desired := Desired{Floors: []map[string]any{{"id": "ground", "name": "Ground floor"}}}
	live := map[string][]map[string]any{"floor": {}}
	managed := map[string]string{"floor:ground": "old-live-id"}

	ops := Plan(desired, live, managed)

	if len(ops) != 1 {
		t.Fatalf("got %d ops, want 1", len(ops))
	}
	op := ops[0]
	if op.Kind != KindCreate || op.RType != "floor" || op.Key != "ground" || op.LiveID != "" {
		t.Errorf("unexpected op: %+v", op)
	}
	want := map[string]any{"name": "Ground floor"}
	if !reflect.DeepEqual(op.Params, want) {
		t.Errorf("params = %+v, want %+v", op.Params, want)
	}
}

func TestUpdateOnlyOnDriftNoOpWhenIdentical(t *testing.T) {
	desired := Desired{Floors: []map[string]any{{"id": "ground", "name": "Ground floor"}}}
	live := map[string][]map[string]any{"floor": {{"floor_id": "abc", "name": "Ground floor"}}}
	managed := map[string]string{"floor:ground": "abc"}

	ops := Plan(desired, live, managed)
	if len(ops) != 0 {
		t.Errorf("got %d ops, want 0: %+v", len(ops), ops)
	}
}

func TestUpdateOnlyOnDriftEmitsUpdateWhenChanged(t *testing.T) {
	desired := Desired{Floors: []map[string]any{{"id": "ground", "name": "Ground floor", "level": 2}}}
	live := map[string][]map[string]any{"floor": {{"floor_id": "abc", "name": "Ground floor", "level": 1}}}
	managed := map[string]string{"floor:ground": "abc"}

	ops := Plan(desired, live, managed)

	if len(ops) != 1 {
		t.Fatalf("got %d ops, want 1", len(ops))
	}
	op := ops[0]
	if op.Kind != KindUpdate || op.LiveID != "abc" {
		t.Errorf("unexpected op: %+v", op)
	}
	if !strings.Contains(op.DiffText, "-level: 1") || !strings.Contains(op.DiffText, "+level: 2") {
		t.Errorf("diff_text = %q", op.DiffText)
	}
}

func TestOmittedFieldNeverDrifts(t *testing.T) {
	// The manifest never mentions "icon", so a user-set one is not drift.
	desired := Desired{Floors: []map[string]any{{"id": "ground", "name": "Ground floor"}}}
	live := map[string][]map[string]any{"floor": {{"floor_id": "abc", "name": "Ground floor", "icon": "mdi:home-outline"}}}
	managed := map[string]string{"floor:ground": "abc"}

	ops := Plan(desired, live, managed)
	if len(ops) != 0 {
		t.Errorf("got %d ops, want 0: %+v", len(ops), ops)
	}
}

func TestListFieldOrderNeverDrifts(t *testing.T) {
	desired := Desired{Floors: []map[string]any{{"id": "ground", "name": "Ground floor", "aliases": []any{"a", "b"}}}}
	live := map[string][]map[string]any{"floor": {{"floor_id": "abc", "name": "Ground floor", "aliases": []any{"b", "a"}}}}
	managed := map[string]string{"floor:ground": "abc"}

	ops := Plan(desired, live, managed)
	if len(ops) != 0 {
		t.Errorf("got %d ops, want 0: %+v", len(ops), ops)
	}
}

// --- ValuesEqual(): numeric normalization at every depth --------------------

// ValuesEqual promises int-vs-float64 normalization at ANY depth, but a
// number nested inside a map once fell through to Go's `a == b`.
// addonopts.optionsDiffer needs it for `{network: {port: 8080}}`, which
// would otherwise drift - and restart the add-on - every cycle.
func TestValuesEqualNormalizesNestedNumerics(t *testing.T) {
	cases := []struct {
		name   string
		before any
		after  any
	}{
		{"bare scalar", int(8080), float64(8080)},
		{"top-level list of scalars", []any{int(8080), int(443)}, []any{float64(8080), float64(443)}},
		{
			"nested map (the port-8080 repro)",
			map[string]any{"network": map[string]any{"port": int(8080)}},
			map[string]any{"network": map[string]any{"port": float64(8080)}},
		},
		{
			"list of maps",
			[]any{map[string]any{"port": int(8080)}, map[string]any{"port": int(443)}},
			[]any{map[string]any{"port": float64(443)}, map[string]any{"port": float64(8080)}},
		},
		{
			"mixed depths",
			map[string]any{"count": int(3), "network": map[string]any{"ports": []any{int(80), int(443)}}},
			map[string]any{"count": float64(3), "network": map[string]any{"ports": []any{float64(80), float64(443)}}},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if !ValuesEqual(c.before, c.after) {
				t.Errorf("ValuesEqual(%#v, %#v) = false, want true", c.before, c.after)
			}
		})
	}
}

// The other half: only same-value-different-numeric-type compares equal,
// never a genuine value change.
func TestValuesEqualStillDetectsNestedNonNumericDrift(t *testing.T) {
	before := map[string]any{"network": map[string]any{"port": int(8080)}}
	after := map[string]any{"network": map[string]any{"port": int(9090)}}
	if ValuesEqual(before, after) {
		t.Errorf("ValuesEqual(%#v, %#v) = true, want false: the nested value genuinely differs", before, after)
	}
}

func TestRenameViaStableID(t *testing.T) {
	// The manifest id (and therefore the registry_managed mapping and the
	// live object) never change on a rename - only the "name" field does.
	desired := Desired{Floors: []map[string]any{{"id": "ground", "name": "Ground Level"}}}
	live := map[string][]map[string]any{"floor": {{"floor_id": "abc", "name": "Ground floor"}}}
	managed := map[string]string{"floor:ground": "abc"}

	ops := Plan(desired, live, managed)

	if len(ops) != 1 {
		t.Fatalf("got %d ops, want 1", len(ops))
	}
	op := ops[0]
	if op.Kind != KindUpdate || op.Key != "ground" || op.LiveID != "abc" {
		t.Errorf("unexpected op: %+v", op)
	}
	if !strings.Contains(op.DiffText, "-name: 'Ground floor'") || !strings.Contains(op.DiffText, "+name: 'Ground Level'") {
		t.Errorf("diff_text = %q", op.DiffText)
	}
}

// --- Plan(): deletes -----------------------------------------------------

func TestDeleteOnlyManagedUndeclaredLiveObjectsNeverTouched(t *testing.T) {
	desired := Desired{}
	live := map[string][]map[string]any{"floor": {
		{"floor_id": "abc", "name": "Ground floor"}, // managed, undeclared -> delete
		{"floor_id": "xyz", "name": "User's floor"}, // never managed -> untouched
	}}
	managed := map[string]string{"floor:ground": "abc"}

	ops := Plan(desired, live, managed)

	if len(ops) != 1 {
		t.Fatalf("got %d ops, want 1", len(ops))
	}
	op := ops[0]
	if op.Kind != KindDelete || op.RType != "floor" || op.Key != "ground" || op.LiveID != "abc" {
		t.Errorf("unexpected op: %+v", op)
	}
	if len(op.Params) != 0 {
		t.Errorf("params = %+v, want empty", op.Params)
	}
	if !strings.Contains(op.DiffText, "-name: 'Ground floor'") {
		t.Errorf("diff_text = %q", op.DiffText)
	}
}

func TestDeleteSkippedWhenLiveObjectAlreadyGone(t *testing.T) {
	desired := Desired{}
	live := map[string][]map[string]any{"floor": {}}
	managed := map[string]string{"floor:ground": "abc"}

	ops := Plan(desired, live, managed)
	if len(ops) != 0 {
		t.Errorf("got %d ops, want 0: %+v", len(ops), ops)
	}
}

// --- Plan(): cross-references -----------------------------------------------

func TestCrossRefToSamePlanCreateUsesRefPlaceholder(t *testing.T) {
	desired := Desired{
		Floors: []map[string]any{{"id": "ground", "name": "Ground floor"}},
		Areas: []map[string]any{
			{"id": "living_room", "name": "Living room", "floor": "ground", "labels": []any{"gitops"}},
		},
		Labels: []map[string]any{{"id": "gitops", "name": "GitOps"}},
	}

	ops := Plan(desired, nil, nil)

	areaOp := findOp(t, ops, "area")
	if areaOp.Kind != KindCreate {
		t.Fatalf("area op kind = %q, want create", areaOp.Kind)
	}
	wantFloor := map[string]any{"$ref": "floor:ground"}
	if !reflect.DeepEqual(areaOp.Params["floor_id"], wantFloor) {
		t.Errorf("floor_id = %+v, want %+v", areaOp.Params["floor_id"], wantFloor)
	}
	wantLabels := []any{map[string]any{"$ref": "label:gitops"}}
	if !reflect.DeepEqual(areaOp.Params["labels"], wantLabels) {
		t.Errorf("labels = %+v, want %+v", areaOp.Params["labels"], wantLabels)
	}
}

func TestCrossRefResolvesToLiveIDWhenAlreadyManaged(t *testing.T) {
	desired := Desired{
		Floors: []map[string]any{{"id": "ground", "name": "Ground floor"}},
		Areas:  []map[string]any{{"id": "living_room", "name": "Living room", "floor": "ground"}},
	}
	live := map[string][]map[string]any{"floor": {{"floor_id": "live-floor-1", "name": "Ground floor"}}}
	managed := map[string]string{"floor:ground": "live-floor-1"}

	ops := Plan(desired, live, managed)

	areaOp := findOp(t, ops, "area")
	if areaOp.Kind != KindCreate {
		t.Fatalf("area op kind = %q, want create", areaOp.Kind)
	}
	if areaOp.Params["floor_id"] != "live-floor-1" {
		t.Errorf("floor_id = %+v, want live-floor-1", areaOp.Params["floor_id"])
	}
}

// --- Plan(): ordering ------------------------------------------------------

func TestOrderingCreatesThenReversedDeletes(t *testing.T) {
	desired := Desired{
		Floors: []map[string]any{{"id": "ground", "name": "Ground floor"}},
		Labels: []map[string]any{{"id": "gitops", "name": "GitOps"}},
		Areas:  []map[string]any{{"id": "living_room", "name": "Living room"}},
		Helpers: map[string][]map[string]any{
			"input_boolean": {{"id": "demo_flag", "name": "Demo flag"}},
			"counter":       {{"id": "demo_counter", "name": "Demo counter"}},
		},
	}
	live := map[string][]map[string]any{
		"floor": {{"floor_id": "old_floor", "name": "Old floor"}},
		"label": {{"label_id": "old_label", "name": "Old label"}},
		"area":  {{"area_id": "old_area", "name": "Old area"}},
		// Helper domains key every response item "id", never
		// "<domain>_id" - see RegistryRTypes.
		"input_number": {{"id": "old_helper", "name": "Old helper"}},
	}
	managed := map[string]string{
		"floor:old_floor_key":         "old_floor",
		"label:old_label_key":         "old_label",
		"area:old_area_key":           "old_area",
		"input_number:old_helper_key": "old_helper",
	}

	ops := Plan(desired, live, managed)

	type kindRType struct{ kind, rtype string }
	got := make([]kindRType, len(ops))
	for i, op := range ops {
		got[i] = kindRType{op.Kind, op.RType}
	}
	want := []kindRType{
		{"create", "floor"},
		{"create", "label"},
		{"create", "area"},
		{"create", "counter"},
		{"create", "input_boolean"},
		{"delete", "input_number"},
		{"delete", "area"},
		{"delete", "label"},
		{"delete", "floor"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("order = %+v, want %+v", got, want)
	}
}

// --- helper id-field asymmetry (floor/area/label use "<rtype>_id" in
// responses; helper domains use "id") ---------------------------------------

func TestResponseIDFieldAndLiveIDOfSplitRegistriesFromHelpers(t *testing.T) {
	if got := RequestIDField("floor"); got != "floor_id" {
		t.Errorf("RequestIDField(floor) = %q", got)
	}
	if got := RequestIDField("input_boolean"); got != "input_boolean_id" {
		t.Errorf("RequestIDField(input_boolean) = %q", got)
	}

	if got := LiveIDOf("floor", map[string]any{"floor_id": "F1", "name": "Ground"}); got != "F1" {
		t.Errorf("LiveIDOf(floor) = %q", got)
	}
	if got := LiveIDOf("area", map[string]any{"area_id": "A1", "name": "Living"}); got != "A1" {
		t.Errorf("LiveIDOf(area) = %q", got)
	}
	if got := LiveIDOf("label", map[string]any{"label_id": "L1", "name": "GitOps"}); got != "L1" {
		t.Errorf("LiveIDOf(label) = %q", got)
	}
	// Helper domains key every response item "id", never "<domain>_id"
	// (home-assistant/core, homeassistant/helpers/collection.py).
	if got := LiveIDOf("input_boolean", map[string]any{"id": "demo_flag", "name": "Demo flag"}); got != "demo_flag" {
		t.Errorf("LiveIDOf(input_boolean, id-keyed) = %q", got)
	}
	if got := LiveIDOf("input_boolean", map[string]any{"input_boolean_id": "demo_flag"}); got != "" {
		t.Errorf("LiveIDOf(input_boolean, wrongly-keyed) = %q, want \"\"", got)
	}
}

func TestHelperNoDriftWhenAlreadyManagedAndLiveObjectPresent(t *testing.T) {
	// A managed helper with no drift against its "id"-keyed live object
	// must plan nothing, not a spurious re-create.
	desired := Desired{Helpers: map[string][]map[string]any{
		"input_boolean": {{"id": "demo_flag", "name": "Demo flag", "icon": "mdi:flag"}},
	}}
	live := map[string][]map[string]any{"input_boolean": {{"id": "demo_flag", "name": "Demo flag", "icon": "mdi:flag"}}}
	managed := map[string]string{"input_boolean:demo_flag": "demo_flag"}

	ops := Plan(desired, live, managed)
	if len(ops) != 0 {
		t.Errorf("got %d ops, want 0: %+v", len(ops), ops)
	}
}

func TestHelperAdoptByNameUsesRealisticIDShape(t *testing.T) {
	desired := Desired{Helpers: map[string][]map[string]any{"input_boolean": {{"id": "demo_flag", "name": "Demo flag"}}}}
	live := map[string][]map[string]any{"input_boolean": {{"id": "demo_flag", "name": "Demo flag"}}}

	ops := Plan(desired, live, nil)

	if len(ops) != 1 {
		t.Fatalf("got %d ops, want 1", len(ops))
	}
	op := ops[0]
	if op.Kind != KindUpdate || op.RType != "input_boolean" || op.LiveID != "demo_flag" {
		t.Errorf("unexpected op: %+v", op)
	}
	if !strings.Contains(op.DiffText, "adopted existing input_boolean") {
		t.Errorf("diff_text = %q", op.DiffText)
	}
}

func TestHelperDeleteWhenRemovedFromManifest(t *testing.T) {
	// Rule 4 for a helper: still in registry_managed with an id-keyed live
	// object, no longer declared, so it plans a delete.
	desired := Desired{}
	live := map[string][]map[string]any{"input_boolean": {{"id": "demo_flag", "name": "Demo flag"}}}
	managed := map[string]string{"input_boolean:demo_flag": "demo_flag"}

	ops := Plan(desired, live, managed)

	if len(ops) != 1 {
		t.Fatalf("got %d ops, want 1", len(ops))
	}
	op := ops[0]
	if op.Kind != KindDelete || op.RType != "input_boolean" || op.Key != "demo_flag" || op.LiveID != "demo_flag" {
		t.Errorf("unexpected op: %+v", op)
	}
}

// --- VM e2e: timer.duration spelling drift ------------------------------

// From a live run: duration "00:05:00" never reached in_sync, because HA
// echoes str(timedelta) back as "0:05:00" and the string compare planned
// the same no-op update forever.
func TestTimerDurationHAZeroPaddedSpellingIsNotDrift(t *testing.T) {
	desired := Desired{Helpers: map[string][]map[string]any{
		"timer": {{"id": "gitops_e2e_timer", "name": "GitOps E2E Timer", "duration": "00:05:00"}},
	}}
	live := map[string][]map[string]any{
		"timer": {{"id": "gitops_e2e_timer", "name": "GitOps E2E Timer", "duration": "0:05:00"}},
	}
	managed := map[string]string{"timer:gitops_e2e_timer": "gitops_e2e_timer"}

	ops := Plan(desired, live, managed)
	if len(ops) != 0 {
		t.Errorf("got %d ops, want 0: %+v", len(ops), ops)
	}
}

// The other spelling HA accepts: a plain integer count of seconds, which
// must equal the live H:MM:SS string it normalizes to.
func TestTimerDurationIntSecondsEqualsHAStringSpelling(t *testing.T) {
	desired := Desired{Helpers: map[string][]map[string]any{
		"timer": {{"id": "gitops_e2e_timer", "name": "GitOps E2E Timer", "duration": 300}},
	}}
	live := map[string][]map[string]any{
		"timer": {{"id": "gitops_e2e_timer", "name": "GitOps E2E Timer", "duration": "0:05:00"}},
	}
	managed := map[string]string{"timer:gitops_e2e_timer": "gitops_e2e_timer"}

	ops := Plan(desired, live, managed)
	if len(ops) != 0 {
		t.Errorf("got %d ops, want 0: %+v", len(ops), ops)
	}
}

// A manifest may spell a ".000000" fractional-seconds suffix, which
// cv.time_period_str accepts. The live side never emits one.
func TestTimerDurationFractionalSecondsSpellingIsNotDrift(t *testing.T) {
	desired := Desired{Helpers: map[string][]map[string]any{
		"timer": {{"id": "gitops_e2e_timer", "name": "GitOps E2E Timer", "duration": "0:05:00.000000"}},
	}}
	live := map[string][]map[string]any{
		"timer": {{"id": "gitops_e2e_timer", "name": "GitOps E2E Timer", "duration": "0:05:00"}},
	}
	managed := map[string]string{"timer:gitops_e2e_timer": "gitops_e2e_timer"}

	ops := Plan(desired, live, managed)
	if len(ops) != 0 {
		t.Errorf("got %d ops, want 0: %+v", len(ops), ops)
	}
}

// A real duration change must still plan an update, not just a respelling.
func TestTimerDurationGenuineChangeStillProducesUpdate(t *testing.T) {
	desired := Desired{Helpers: map[string][]map[string]any{
		"timer": {{"id": "gitops_e2e_timer", "name": "GitOps E2E Timer", "duration": "0:10:00"}},
	}}
	live := map[string][]map[string]any{
		"timer": {{"id": "gitops_e2e_timer", "name": "GitOps E2E Timer", "duration": "0:05:00"}},
	}
	managed := map[string]string{"timer:gitops_e2e_timer": "gitops_e2e_timer"}

	ops := Plan(desired, live, managed)
	if len(ops) != 1 {
		t.Fatalf("got %d ops, want 1: %+v", len(ops), ops)
	}
	op := ops[0]
	if op.Kind != KindUpdate || op.RType != "timer" {
		t.Errorf("unexpected op: %+v", op)
	}
	if !strings.Contains(op.DiffText, "-duration: '0:05:00'") || !strings.Contains(op.DiffText, "+duration: '0:10:00'") {
		t.Errorf("diff_text = %q", op.DiffText)
	}
}

// The special case is scoped to timer's "duration" field: a floor "name"
// holding a duration-shaped string still compares as plain text.
func TestNonTimerDurationShapedFieldComparedAsPlainString(t *testing.T) {
	desired := Desired{Floors: []map[string]any{{"id": "ground", "name": "00:05:00"}}}
	live := map[string][]map[string]any{"floor": {{"floor_id": "abc", "name": "0:05:00"}}}
	managed := map[string]string{"floor:ground": "abc"}

	ops := Plan(desired, live, managed)
	if len(ops) != 1 {
		t.Fatalf("got %d ops, want 1: %+v", len(ops), ops)
	}
	if ops[0].Kind != KindUpdate {
		t.Errorf("unexpected op: %+v", ops[0])
	}
}

// The other half: even on a timer the case is scoped to "duration". Both
// durations are identical here, so the update can only come from "name".
func TestTimerNameFieldDurationShapedRespellingStillProducesUpdate(t *testing.T) {
	desired := Desired{Helpers: map[string][]map[string]any{
		"timer": {{"id": "gitops_e2e_timer", "name": "00:05:00", "duration": "0:05:00"}},
	}}
	live := map[string][]map[string]any{
		"timer": {{"id": "gitops_e2e_timer", "name": "0:05:00", "duration": "0:05:00"}},
	}
	managed := map[string]string{"timer:gitops_e2e_timer": "gitops_e2e_timer"}

	ops := Plan(desired, live, managed)
	if len(ops) != 1 {
		t.Fatalf("got %d ops, want 1: %+v", len(ops), ops)
	}
	if ops[0].Kind != KindUpdate || ops[0].RType != "timer" {
		t.Errorf("unexpected op: %+v", ops[0])
	}
}

// timerDurationEqual's ok=false path: a value that is no duration at all
// falls back to the ordinary comparison rather than being called equal.
func TestTimerDurationUnparseableValueFallsBackToPlainStringCompare(t *testing.T) {
	desired := Desired{Helpers: map[string][]map[string]any{
		"timer": {{"id": "gitops_e2e_timer", "name": "GitOps E2E Timer", "duration": "not a duration"}},
	}}
	live := map[string][]map[string]any{
		"timer": {{"id": "gitops_e2e_timer", "name": "GitOps E2E Timer", "duration": "0:05:00"}},
	}
	managed := map[string]string{"timer:gitops_e2e_timer": "gitops_e2e_timer"}

	ops := Plan(desired, live, managed)
	if len(ops) != 1 {
		t.Fatalf("got %d ops, want 1: %+v", len(ops), ops)
	}
	if ops[0].Kind != KindUpdate || ops[0].RType != "timer" {
		t.Errorf("unexpected op: %+v", ops[0])
	}
}

// cv.time_period_str has no zero-padding requirement, so an unpadded
// minute must still parse rather than fall back and drift forever.
func TestTimerDurationSingleDigitMinuteMatchesZeroPadded(t *testing.T) {
	desired := Desired{Helpers: map[string][]map[string]any{
		"timer": {{"id": "gitops_e2e_timer", "name": "GitOps E2E Timer", "duration": "0:5:00"}},
	}}
	live := map[string][]map[string]any{
		"timer": {{"id": "gitops_e2e_timer", "name": "GitOps E2E Timer", "duration": "0:05:00"}},
	}
	managed := map[string]string{"timer:gitops_e2e_timer": "gitops_e2e_timer"}

	ops := Plan(desired, live, managed)
	if len(ops) != 0 {
		t.Errorf("got %d ops, want 0: %+v", len(ops), ops)
	}
}

// cv.time_period_str's 2-part branch is H:MM, not M:SS, so "05:00" is five
// hours and matches a live "5:00:00".
func TestTimerDurationTwoPartFormIsHoursMinutes(t *testing.T) {
	desired := Desired{Helpers: map[string][]map[string]any{
		"timer": {{"id": "gitops_e2e_timer", "name": "GitOps E2E Timer", "duration": "05:00"}},
	}}
	live := map[string][]map[string]any{
		"timer": {{"id": "gitops_e2e_timer", "name": "GitOps E2E Timer", "duration": "5:00:00"}},
	}
	managed := map[string]string{"timer:gitops_e2e_timer": "gitops_e2e_timer"}

	ops := Plan(desired, live, managed)
	if len(ops) != 0 {
		t.Errorf("got %d ops, want 0: %+v", len(ops), ops)
	}
}

// cv.time_period_str range-checks nothing, so "1:70:00" is a legal
// spelling of a live "2:10:00", not an error.
func TestTimerDurationMinutesOverSixtyNotRangeChecked(t *testing.T) {
	desired := Desired{Helpers: map[string][]map[string]any{
		"timer": {{"id": "gitops_e2e_timer", "name": "GitOps E2E Timer", "duration": "1:70:00"}},
	}}
	live := map[string][]map[string]any{
		"timer": {{"id": "gitops_e2e_timer", "name": "GitOps E2E Timer", "duration": "2:10:00"}},
	}
	managed := map[string]string{"timer:gitops_e2e_timer": "gitops_e2e_timer"}

	ops := Plan(desired, live, managed)
	if len(ops) != 0 {
		t.Errorf("got %d ops, want 0: %+v", len(ops), ops)
	}
}

// The same, for seconds: "0:00:90" matches a live "0:01:30".
func TestTimerDurationSecondsOverSixtyNotRangeChecked(t *testing.T) {
	desired := Desired{Helpers: map[string][]map[string]any{
		"timer": {{"id": "gitops_e2e_timer", "name": "GitOps E2E Timer", "duration": "0:00:90"}},
	}}
	live := map[string][]map[string]any{
		"timer": {{"id": "gitops_e2e_timer", "name": "GitOps E2E Timer", "duration": "0:01:30"}},
	}
	managed := map[string]string{"timer:gitops_e2e_timer": "gitops_e2e_timer"}

	ops := Plan(desired, live, managed)
	if len(ops) != 0 {
		t.Errorf("got %d ops, want 0: %+v", len(ops), ops)
	}
}

// _format_timedelta truncates rather than rounds, so a manifest 300.5
// seconds is the same value as a live "0:05:00".
func TestTimerDurationFractionalSecondsCountTruncatesToMatch(t *testing.T) {
	desired := Desired{Helpers: map[string][]map[string]any{
		"timer": {{"id": "gitops_e2e_timer", "name": "GitOps E2E Timer", "duration": 300.5}},
	}}
	live := map[string][]map[string]any{
		"timer": {{"id": "gitops_e2e_timer", "name": "GitOps E2E Timer", "duration": "0:05:00"}},
	}
	managed := map[string]string{"timer:gitops_e2e_timer": "gitops_e2e_timer"}

	ops := Plan(desired, live, managed)
	if len(ops) != 0 {
		t.Errorf("got %d ops, want 0: %+v", len(ops), ops)
	}
}

// The same truncation, for a string with a non-zero fractional suffix.
func TestTimerDurationFractionalSecondsStringTruncatesToMatch(t *testing.T) {
	desired := Desired{Helpers: map[string][]map[string]any{
		"timer": {{"id": "gitops_e2e_timer", "name": "GitOps E2E Timer", "duration": "0:05:00.5"}},
	}}
	live := map[string][]map[string]any{
		"timer": {{"id": "gitops_e2e_timer", "name": "GitOps E2E Timer", "duration": "0:05:00"}},
	}
	managed := map[string]string{"timer:gitops_e2e_timer": "gitops_e2e_timer"}

	ops := Plan(desired, live, managed)
	if len(ops) != 0 {
		t.Errorf("got %d ops, want 0: %+v", len(ops), ops)
	}
}

// cv.time_period's third input shape: a mapping like {minutes: 5}, echoed
// back as the same _format_timedelta string as any other spelling.
func TestTimerDurationDictFormMatchesEquivalentString(t *testing.T) {
	desired := Desired{Helpers: map[string][]map[string]any{
		"timer": {{"id": "gitops_e2e_timer", "name": "GitOps E2E Timer", "duration": map[string]any{"minutes": 5}}},
	}}
	live := map[string][]map[string]any{
		"timer": {{"id": "gitops_e2e_timer", "name": "GitOps E2E Timer", "duration": "0:05:00"}},
	}
	managed := map[string]string{"timer:gitops_e2e_timer": "gitops_e2e_timer"}

	ops := Plan(desired, live, managed)
	if len(ops) != 0 {
		t.Errorf("got %d ops, want 0: %+v", len(ops), ops)
	}
}

// --- input_select.options: order-sensitive, spelling-normalized compare ---

// input_select.options is order-preserving on live HA, so a reorder must
// plan an update - ValuesEqual's order-insensitive compare planned none.
func TestInputSelectOptionsReorderProducesUpdateNotNoOp(t *testing.T) {
	desired := Desired{Helpers: map[string][]map[string]any{
		"input_select": {{"id": "mode_select", "name": "Mode", "options": []any{"a", "b", "c"}}},
	}}
	live := map[string][]map[string]any{
		"input_select": {{"id": "mode_select", "name": "Mode", "options": []any{"c", "b", "a"}}},
	}
	managed := map[string]string{"input_select:mode_select": "mode_select"}

	ops := Plan(desired, live, managed)

	if len(ops) != 1 {
		t.Fatalf("got %d ops, want 1: %+v", len(ops), ops)
	}
	op := ops[0]
	if op.Kind != KindUpdate || op.RType != "input_select" || op.LiveID != "mode_select" {
		t.Errorf("unexpected op: %+v", op)
	}
	if !strings.Contains(op.DiffText, "-options: ['c', 'b', 'a']") || !strings.Contains(op.DiffText, "+options: ['a', 'b', 'c']") {
		t.Errorf("diff_text = %q", op.DiffText)
	}
}

// The control for the reorder case: identical order plans nothing.
func TestInputSelectOptionsSameOrderIsNoOp(t *testing.T) {
	desired := Desired{Helpers: map[string][]map[string]any{
		"input_select": {{"id": "mode_select", "name": "Mode", "options": []any{"a", "b", "c"}}},
	}}
	live := map[string][]map[string]any{
		"input_select": {{"id": "mode_select", "name": "Mode", "options": []any{"a", "b", "c"}}},
	}
	managed := map[string]string{"input_select:mode_select": "mode_select"}

	ops := Plan(desired, live, managed)
	if len(ops) != 0 {
		t.Errorf("got %d ops, want 0: %+v", len(ops), ops)
	}
}

// HA validates options with cv.string and echoes strings back, so a
// manifest `options: [1, 2]` must not drift against live ["1", "2"].
func TestInputSelectOptionsIntScalarsMatchHAStringSpelling(t *testing.T) {
	desired := Desired{Helpers: map[string][]map[string]any{
		"input_select": {{"id": "level_select", "name": "Level", "options": []any{1, 2}}},
	}}
	live := map[string][]map[string]any{
		"input_select": {{"id": "level_select", "name": "Level", "options": []any{"1", "2"}}},
	}
	managed := map[string]string{"input_select:level_select": "level_select"}

	ops := Plan(desired, live, managed)
	if len(ops) != 0 {
		t.Errorf("got %d ops, want 0: %+v", len(ops), ops)
	}
}

// The same for bool: str(True) is capitalized, so coercing with Go's
// fmt.Sprint would swap one forever-drift loop for another.
func TestInputSelectOptionsBoolScalarMatchesHAStringSpelling(t *testing.T) {
	desired := Desired{Helpers: map[string][]map[string]any{
		"input_select": {{"id": "flag_select", "name": "Flag", "options": []any{true}}},
	}}
	live := map[string][]map[string]any{
		"input_select": {{"id": "flag_select", "name": "Flag", "options": []any{"True"}}},
	}
	managed := map[string]string{"input_select:flag_select": "flag_select"}

	ops := Plan(desired, live, managed)
	if len(ops) != 0 {
		t.Errorf("got %d ops, want 0: %+v", len(ops), ops)
	}
}

// A documented limitation, not a bug: JSON turns `2.0` into an int on the
// way out but keeps `1.5` a float, so no single str() model fits and a
// float option is left uncoerced. DOCS.md says to quote it.
func TestInputSelectOptionsFloatScalarIsNotCoerced(t *testing.T) {
	desired := Desired{Helpers: map[string][]map[string]any{
		"input_select": {{"id": "ratio_select", "name": "Ratio", "options": []any{1.5}}},
	}}
	live := map[string][]map[string]any{
		"input_select": {{"id": "ratio_select", "name": "Ratio", "options": []any{"1.5"}}},
	}
	managed := map[string]string{"input_select:ratio_select": "ratio_select"}

	ops := Plan(desired, live, managed)

	if len(ops) != 1 {
		t.Fatalf("got %d ops, want 1: %+v", len(ops), ops)
	}
	if op := ops[0]; op.Kind != KindUpdate || op.RType != "input_select" {
		t.Errorf("unexpected op: %+v", op)
	}
}

// A content change, not a reorder or respelling, must still plan an update.
func TestInputSelectOptionsGenuineChangeStillProducesUpdate(t *testing.T) {
	desired := Desired{Helpers: map[string][]map[string]any{
		"input_select": {{"id": "mode_select", "name": "Mode", "options": []any{"a", "b"}}},
	}}
	live := map[string][]map[string]any{
		"input_select": {{"id": "mode_select", "name": "Mode", "options": []any{"a", "c"}}},
	}
	managed := map[string]string{"input_select:mode_select": "mode_select"}

	ops := Plan(desired, live, managed)

	if len(ops) != 1 {
		t.Fatalf("got %d ops, want 1: %+v", len(ops), ops)
	}
	if op := ops[0]; op.Kind != KindUpdate || op.RType != "input_select" {
		t.Errorf("unexpected op: %+v", op)
	}
}

// A length mismatch alone must plan an update, whatever the elements are.
func TestInputSelectOptionsLengthMismatchProducesUpdate(t *testing.T) {
	desired := Desired{Helpers: map[string][]map[string]any{
		"input_select": {{"id": "mode_select", "name": "Mode", "options": []any{"a", "b"}}},
	}}
	live := map[string][]map[string]any{
		"input_select": {{"id": "mode_select", "name": "Mode", "options": []any{"a"}}},
	}
	managed := map[string]string{"input_select:mode_select": "mode_select"}

	ops := Plan(desired, live, managed)

	if len(ops) != 1 {
		t.Fatalf("got %d ops, want 1: %+v", len(ops), ops)
	}
	if op := ops[0]; op.Kind != KindUpdate || op.RType != "input_select" {
		t.Errorf("unexpected op: %+v", op)
	}
}

// The options fix must not reach the genuinely set-like fields: a floor's
// aliases and an area's labels still compare order-insensitively.
func TestListFieldOrderStillInsensitiveForAreaLabelsAndFloorAliases(t *testing.T) {
	desired := Desired{
		Floors: []map[string]any{{"id": "ground", "name": "Ground floor", "aliases": []any{"a", "b"}}},
		Labels: []map[string]any{{"id": "label_a", "name": "Label A"}, {"id": "label_b", "name": "Label B"}},
		Areas:  []map[string]any{{"id": "living_room", "name": "Living room", "labels": []any{"label_a", "label_b"}}},
	}
	live := map[string][]map[string]any{
		"floor": {{"floor_id": "F1", "name": "Ground floor", "aliases": []any{"b", "a"}}},
		"label": {{"label_id": "LA_LIVE", "name": "Label A"}, {"label_id": "LB_LIVE", "name": "Label B"}},
		"area":  {{"area_id": "A1", "name": "Living room", "labels": []any{"LB_LIVE", "LA_LIVE"}}},
	}
	managed := map[string]string{
		"floor:ground":     "F1",
		"label:label_a":    "LA_LIVE",
		"label:label_b":    "LB_LIVE",
		"area:living_room": "A1",
	}

	ops := Plan(desired, live, managed)
	if len(ops) != 0 {
		t.Errorf("got %d ops, want 0: %+v", len(ops), ops)
	}
}

func TestFullFloorAreaHelperPlanNeverPanicsOnRealisticShapes(t *testing.T) {
	// Every ownership rule at once, against realistic response shapes
	// (registries keyed "<rtype>_id", helpers keyed "id").
	desired := Desired{
		Floors: []map[string]any{{"id": "ground", "name": "Ground floor"}},
		Areas:  []map[string]any{{"id": "living_room", "name": "Living room", "floor": "ground"}},
		Helpers: map[string][]map[string]any{
			"input_boolean": {{"id": "demo_flag", "name": "Demo flag"}},
		},
	}
	live := map[string][]map[string]any{
		"floor":         {{"floor_id": "F1", "name": "Ground floor"}},
		"area":          {{"area_id": "A1", "name": "Living room", "floor_id": "F1"}},
		"label":         {},
		"input_boolean": {{"id": "demo_flag", "name": "Demo flag"}},
	}
	managed := map[string]string{"floor:ground": "F1", "area:living_room": "A1", "input_boolean:demo_flag": "demo_flag"}

	ops := Plan(desired, live, managed)
	if len(ops) != 0 {
		t.Errorf("got %d ops, want 0 (fully in sync): %+v", len(ops), ops)
	}
}

func TestAmbiguousLiveObjectMissingIDFieldBecomesErrorNotPanic(t *testing.T) {
	// A name match carrying no usable id key is too malformed to adopt: a
	// per-item error, never a panic out of Plan.
	desired := Desired{Labels: []map[string]any{{"id": "gitops", "name": "GitOps"}}}
	live := map[string][]map[string]any{"label": {{"name": "GitOps"}}} // no "label_id" at all

	ops := Plan(desired, live, nil)

	if len(ops) != 1 {
		t.Fatalf("got %d ops, want 1", len(ops))
	}
	if ops[0].Kind != KindError {
		t.Errorf("kind = %q, want error", ops[0].Kind)
	}
	if !strings.Contains(ops[0].Error, "no usable id field") {
		t.Errorf("error = %q", ops[0].Error)
	}
}

// --- an ambiguous/broken cross-ref demotes the referencing item to an
// error op instead of emitting a $ref that later explodes ------------------

func TestAreaReferencingAmbiguousFloorIsDemotedToError(t *testing.T) {
	desired := Desired{
		Floors: []map[string]any{{"id": "ground", "name": "Ground floor"}},
		Areas:  []map[string]any{{"id": "living_room", "name": "Living room", "floor": "ground"}},
	}
	live := map[string][]map[string]any{
		"floor": {{"floor_id": "F1", "name": "Ground floor"}, {"floor_id": "F2", "name": "Ground floor"}},
		"area":  {},
		"label": {},
	}

	ops := Plan(desired, live, nil)

	if len(ops) != 2 {
		t.Fatalf("got %d ops, want 2: %+v", len(ops), ops)
	}
	for _, op := range ops {
		if op.Kind != KindError {
			t.Errorf("op %+v kind != error", op)
		}
	}
	floorOp := findOp(t, ops, "floor")
	areaOp := findOp(t, ops, "area")
	if !strings.Contains(floorOp.Error, "ambiguous adopt") {
		t.Errorf("floor error = %q", floorOp.Error)
	}
	if !strings.Contains(areaOp.Error, "floor 'ground'") {
		t.Errorf("area error = %q", areaOp.Error)
	}
	if len(areaOp.Params) != 0 {
		t.Errorf("area params = %+v, want empty", areaOp.Params)
	}
	if strings.Contains(areaOp.Error, "$ref") {
		t.Errorf("area error leaked $ref: %q", areaOp.Error)
	}
}

func TestBrokenRefDoesNotBlockTheRestOfThePlan(t *testing.T) {
	// An area with an unresolvable floor ref becomes an error op, but an
	// unrelated, independently-planned item still plans normally.
	desired := Desired{
		Floors: []map[string]any{{"id": "ground", "name": "Ground floor"}},
		Labels: []map[string]any{{"id": "gitops", "name": "GitOps"}},
		Areas:  []map[string]any{{"id": "living_room", "name": "Living room", "floor": "ground"}},
	}
	live := map[string][]map[string]any{
		"floor": {{"floor_id": "F1", "name": "Ground floor"}, {"floor_id": "F2", "name": "Ground floor"}},
		"area":  {},
		"label": {},
	}

	ops := Plan(desired, live, nil)

	labelOp := findOp(t, ops, "label")
	if labelOp.Kind != KindCreate {
		t.Errorf("label op kind = %q, want create", labelOp.Kind)
	}
	areaOp := findOp(t, ops, "area")
	if areaOp.Kind != KindError {
		t.Errorf("area op kind = %q, want error", areaOp.Kind)
	}
}

func TestDiffTextNeverRendersARawRefPlaceholder(t *testing.T) {
	desired := Desired{
		Floors: []map[string]any{{"id": "ground", "name": "Ground floor"}},
		Areas:  []map[string]any{{"id": "living_room", "name": "Living room", "floor": "ground"}},
	}

	ops := Plan(desired, nil, nil)

	areaOp := findOp(t, ops, "area")
	if areaOp.Kind != KindCreate {
		t.Fatalf("area op kind = %q, want create", areaOp.Kind)
	}
	wantFloor := map[string]any{"$ref": "floor:ground"}
	if !reflect.DeepEqual(areaOp.Params["floor_id"], wantFloor) {
		t.Errorf("floor_id = %+v, want %+v", areaOp.Params["floor_id"], wantFloor)
	}
	if strings.Contains(areaOp.DiffText, "$ref") {
		t.Errorf("diff_text leaked $ref: %q", areaOp.DiffText)
	}
	if !strings.Contains(areaOp.DiffText, "pending: floor:ground") {
		t.Errorf("diff_text = %q, missing pending marker", areaOp.DiffText)
	}
}

// --- manifest items can't declare fields that collide with reserved WS
// envelope/request-id keys --------------------------------------------------

func TestReservedFieldNameTypeIsRejected(t *testing.T) {
	workdir, gitops := mkGitops(t)
	writeFile(t, gitops, "registries.yaml", `
floors:
  - id: ground
    name: Ground floor
    type: whoops
`)

	_, err := LoadManifests(workdir)
	if err == nil || !strings.Contains(err.Error(), "reserved field name") {
		t.Fatalf("err = %v, want it to contain 'reserved field name'", err)
	}
}

func TestReservedFieldNameMsgTypeIsRejected(t *testing.T) {
	// "msg_type" is wsclient.Client.Cmd's first parameter, so a field of
	// that name is caught here rather than during a live apply.
	workdir, gitops := mkGitops(t)
	writeFile(t, gitops, "helpers.yaml", `
input_boolean:
  - id: demo_flag
    name: Demo flag
    msg_type: whoops
`)

	_, err := LoadManifests(workdir)
	if err == nil || !strings.Contains(err.Error(), "reserved field name") {
		t.Fatalf("err = %v, want it to contain 'reserved field name'", err)
	}
}

func TestReservedFieldNamesCreatedAtModifiedAtAreRejected(t *testing.T) {
	workdir, gitops := mkGitops(t)
	writeFile(t, gitops, "registries.yaml", `
labels:
  - id: gitops
    name: GitOps
    created_at: "2026-01-01T00:00:00+00:00"
areas:
  - id: living_room
    name: Living room
    modified_at: "2026-01-01T00:00:00+00:00"
`)

	_, err := LoadManifests(workdir)
	if err == nil || !strings.Contains(err.Error(), "reserved field name") {
		t.Fatalf("err = %v, want it to contain 'reserved field name'", err)
	}
	if !strings.Contains(err.Error(), "created_at") || !strings.Contains(err.Error(), "modified_at") {
		t.Errorf("err = %v, want both created_at and modified_at", err)
	}
}

func TestReservedFieldNameMatchingRequestIDFieldIsRejected(t *testing.T) {
	workdir, gitops := mkGitops(t)
	writeFile(t, gitops, "helpers.yaml", `
input_boolean:
  - id: demo_flag
    name: Demo flag
    input_boolean_id: collide
`)

	_, err := LoadManifests(workdir)
	if err == nil || !strings.Contains(err.Error(), "reserved field name") {
		t.Fatalf("err = %v, want it to contain 'reserved field name'", err)
	}
}

func TestAreaOwnRTypeIDFieldNameIsRejected(t *testing.T) {
	workdir, gitops := mkGitops(t)
	writeFile(t, gitops, "registries.yaml", `
areas:
  - id: living_room
    name: Living room
    area_id: collide
`)

	_, err := LoadManifests(workdir)
	if err == nil || !strings.Contains(err.Error(), "reserved field name") {
		t.Fatalf("err = %v, want it to contain 'reserved field name'", err)
	}
}

func TestOrdinaryExtraFieldsAreUnaffectedByReservedFieldCheck(t *testing.T) {
	workdir, gitops := mkGitops(t)
	writeFile(t, gitops, "registries.yaml", `
floors:
  - id: ground
    name: Ground floor
    icon: mdi:home
    level: 0
    aliases: [g, downstairs]
`)

	desired, err := LoadManifests(workdir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if desired.Floors[0]["icon"] != "mdi:home" {
		t.Errorf("icon = %+v, want mdi:home", desired.Floors[0]["icon"])
	}
}

// --- test helpers ------------------------------------------------------

func findOp(t *testing.T, ops []RegOp, rtype string) RegOp {
	t.Helper()
	for _, op := range ops {
		if op.RType == rtype {
			return op
		}
	}
	t.Fatalf("no op found for rtype %q in %+v", rtype, ops)
	return RegOp{}
}
