package entities

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/registries"
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

// --- LoadManifest(): missing/empty ------------------------------------

func TestMissingEntitiesFileIsNotAnError(t *testing.T) {
	workdir, _ := mkGitops(t)
	got, err := LoadManifest(workdir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !reflect.DeepEqual(got, emptyDesired()) {
		t.Errorf("got %+v, want empty Desired", got)
	}
}

func TestMissingGitopsDirIsNotAnError(t *testing.T) {
	workdir := t.TempDir()
	got, err := LoadManifest(workdir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !reflect.DeepEqual(got, emptyDesired()) {
		t.Errorf("got %+v, want empty Desired", got)
	}
}

func TestEmptyEntitiesKeyIsNotAnError(t *testing.T) {
	workdir, gitops := mkGitops(t)
	writeFile(t, gitops, "entities.yaml", "entities:\n")
	got, err := LoadManifest(workdir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !reflect.DeepEqual(got, emptyDesired()) {
		t.Errorf("got %+v, want empty Desired", got)
	}
}

// --- LoadManifest(): happy path -----------------------------------------

func TestLoadManifestParsesAllKnownFields(t *testing.T) {
	workdir, gitops := mkGitops(t)
	writeFile(t, gitops, "entities.yaml", `
entities:
  - entity_id: light.living_room_ceiling
    name: Ceiling Light
    icon: mdi:ceiling-light
    area: living_room
    labels: [managed_by_gitops]
    disabled: false
    hidden: false
`)
	desired, err := LoadManifest(workdir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []map[string]any{{
		"entity_id": "light.living_room_ceiling",
		"name":      "Ceiling Light",
		"icon":      "mdi:ceiling-light",
		"area":      "living_room",
		"labels":    []any{"managed_by_gitops"},
		"disabled":  false,
		"hidden":    false,
	}}
	if !reflect.DeepEqual(desired.Entities, want) {
		t.Errorf("entities = %+v, want %+v", desired.Entities, want)
	}
}

func TestLoadManifestEntityWithOnlyEntityIDIsValid(t *testing.T) {
	workdir, gitops := mkGitops(t)
	writeFile(t, gitops, "entities.yaml", "entities:\n  - entity_id: light.x\n")
	desired, err := LoadManifest(workdir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []map[string]any{{"entity_id": "light.x"}}
	if !reflect.DeepEqual(desired.Entities, want) {
		t.Errorf("entities = %+v, want %+v", desired.Entities, want)
	}
}

// --- LoadManifest(): validation ------------------------------------------

func TestLoadManifestInvalidEntityID(t *testing.T) {
	cases := []string{
		"entities:\n  - entity_id: not_an_entity_id\n",
		"entities:\n  - entity_id: \"\"\n",
		"entities:\n  - name: Missing ID\n",
		"entities:\n  - entity_id: Light.Foo\n", // uppercase not allowed
	}
	for _, yamlContent := range cases {
		workdir, gitops := mkGitops(t)
		writeFile(t, gitops, "entities.yaml", yamlContent)
		_, err := LoadManifest(workdir)
		if err == nil {
			t.Fatalf("content %q: expected an error", yamlContent)
		}
		if !strings.Contains(err.Error(), "invalid or missing 'entity_id'") {
			t.Errorf("content %q: error = %q", yamlContent, err.Error())
		}
	}
}

func TestLoadManifestDuplicateEntityID(t *testing.T) {
	workdir, gitops := mkGitops(t)
	writeFile(t, gitops, "entities.yaml", `
entities:
  - entity_id: light.x
    name: A
  - entity_id: light.x
    name: B
`)
	_, err := LoadManifest(workdir)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "duplicate entity_id 'light.x'") {
		t.Errorf("error = %q", err.Error())
	}
}

func TestLoadManifestNewEntityIDIsRejectedWithItsOwnMessage(t *testing.T) {
	workdir, gitops := mkGitops(t)
	writeFile(t, gitops, "entities.yaml", "entities:\n  - entity_id: light.x\n    new_entity_id: light.y\n")
	_, err := LoadManifest(workdir)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "renames are not supported") {
		t.Errorf("error = %q", err.Error())
	}
}

func TestLoadManifestUnsupportedFieldIsRejected(t *testing.T) {
	workdir, gitops := mkGitops(t)
	writeFile(t, gitops, "entities.yaml", "entities:\n  - entity_id: light.x\n    unique_id: abc123\n")
	_, err := LoadManifest(workdir)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "unsupported field(s) unique_id") {
		t.Errorf("error = %q", err.Error())
	}
}

func TestLoadManifestLabelsMustBeAList(t *testing.T) {
	workdir, gitops := mkGitops(t)
	writeFile(t, gitops, "entities.yaml", "entities:\n  - entity_id: light.x\n    labels: gitops\n")
	_, err := LoadManifest(workdir)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "labels must be a list") {
		t.Errorf("error = %q", err.Error())
	}
}

// An unquoted `name: 42` or `icon: true` must not reach the
// entity_registry/update params as the wrong Go type, where it surfaces
// only as an HA schema rejection at apply time.
func TestLoadManifestNameMustBeANonEmptyString(t *testing.T) {
	for _, tc := range []struct {
		name string
		yaml string
	}{
		{"wrong type", "entities:\n  - entity_id: light.kitchen\n    name: 42\n"},
		{"empty string", "entities:\n  - entity_id: light.kitchen\n    name: \"\"\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workdir, gitops := mkGitops(t)
			writeFile(t, gitops, "entities.yaml", tc.yaml)
			_, err := LoadManifest(workdir)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), "name must be a non-empty string") {
				t.Errorf("error = %q", err.Error())
			}
		})
	}
}

func TestLoadManifestIconMustBeAString(t *testing.T) {
	workdir, gitops := mkGitops(t)
	writeFile(t, gitops, "entities.yaml", "entities:\n  - entity_id: light.kitchen\n    icon: true\n")
	_, err := LoadManifest(workdir)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "icon must be a string") {
		t.Errorf("error = %q", err.Error())
	}
}

func TestLoadManifestDisabledHiddenMustBeBooleans(t *testing.T) {
	for _, field := range []string{"disabled", "hidden"} {
		workdir, gitops := mkGitops(t)
		writeFile(t, gitops, "entities.yaml", "entities:\n  - entity_id: light.x\n    "+field+": yes-please\n")
		_, err := LoadManifest(workdir)
		if err == nil {
			t.Fatalf("field %s: expected an error", field)
		}
		if !strings.Contains(err.Error(), field+" must be a boolean") {
			t.Errorf("field %s: error = %q", field, err.Error())
		}
	}
}

func TestLoadManifestAggregatesEveryProblem(t *testing.T) {
	workdir, gitops := mkGitops(t)
	writeFile(t, gitops, "entities.yaml", `
entities:
  - entity_id: bad
  - entity_id: light.x
    new_entity_id: light.y
  - entity_id: light.z
    unique_id: abc
`)
	_, err := LoadManifest(workdir)
	if err == nil {
		t.Fatal("expected an error")
	}
	msg := err.Error()
	for _, want := range []string{"invalid or missing 'entity_id'", "renames are not supported", "unsupported field(s) unique_id"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q does not contain %q", msg, want)
		}
	}
}

// --- Plan(): entity not found --------------------------------------------

func TestPlanEntityNotFoundIsAnErrorOpNeverACreate(t *testing.T) {
	desired := Desired{Entities: []map[string]any{{"entity_id": "light.missing", "name": "X"}}}
	ops := Plan(desired, nil, nil, RefResolver{})

	if len(ops) != 1 {
		t.Fatalf("ops = %+v", ops)
	}
	op := ops[0]
	if op.Kind != registries.KindError || op.RType != "entity" || op.Key != "light.missing" {
		t.Errorf("op = %+v", op)
	}
	if !strings.Contains(op.Error, "entity not found") {
		t.Errorf("error = %q", op.Error)
	}
}

// --- Plan(): disabled_by guard --------------------------------------------

func TestPlanRefusesEntityDisabledByIntegration(t *testing.T) {
	live := []map[string]any{{"entity_id": "light.x", "name": "Old", "disabled_by": "integration"}}
	desired := Desired{Entities: []map[string]any{{"entity_id": "light.x", "name": "New"}}}
	ops := Plan(desired, live, nil, RefResolver{})

	if len(ops) != 1 || ops[0].Kind != registries.KindError {
		t.Fatalf("ops = %+v", ops)
	}
	if !strings.Contains(ops[0].Error, `disabled by "integration"`) {
		t.Errorf("error = %q", ops[0].Error)
	}
}

func TestPlanAllowsEntityDisabledByUser(t *testing.T) {
	live := []map[string]any{{"entity_id": "light.x", "name": "Old", "disabled_by": "user"}}
	desired := Desired{Entities: []map[string]any{{"entity_id": "light.x", "name": "New"}}}
	ops := Plan(desired, live, nil, RefResolver{})

	if len(ops) != 1 || ops[0].Kind != KindUpdate {
		t.Fatalf("ops = %+v", ops)
	}
}

func TestPlanRefusesRestoreWhenNowDisabledByIntegration(t *testing.T) {
	live := []map[string]any{{"entity_id": "light.x", "name": "Managed", "disabled_by": "config_entry"}}
	originals := map[string]map[string]any{"entity:light.x": {"name": "Original"}}
	ops := Plan(Desired{}, live, originals, RefResolver{})

	if len(ops) != 1 || ops[0].Kind != registries.KindError {
		t.Fatalf("ops = %+v", ops)
	}
	if !strings.Contains(ops[0].Error, "cannot restore") {
		t.Errorf("error = %q", ops[0].Error)
	}
}

// --- Plan(): hidden_by guard ----------------------------------------------

func TestPlanRefusesEntityHiddenByIntegration(t *testing.T) {
	live := []map[string]any{{"entity_id": "light.x", "name": "Old", "hidden_by": "integration"}}
	desired := Desired{Entities: []map[string]any{{"entity_id": "light.x", "name": "New"}}}
	ops := Plan(desired, live, nil, RefResolver{})

	if len(ops) != 1 || ops[0].Kind != registries.KindError {
		t.Fatalf("ops = %+v", ops)
	}
	if !strings.Contains(ops[0].Error, `hidden by "integration"`) {
		t.Errorf("error = %q", ops[0].Error)
	}
}

func TestPlanAllowsEntityHiddenByUser(t *testing.T) {
	live := []map[string]any{{"entity_id": "light.x", "name": "Old", "hidden_by": "user"}}
	desired := Desired{Entities: []map[string]any{{"entity_id": "light.x", "name": "New"}}}
	ops := Plan(desired, live, nil, RefResolver{})

	if len(ops) != 1 || ops[0].Kind != KindUpdate {
		t.Fatalf("ops = %+v", ops)
	}
}

func TestPlanRefusesRestoreWhenNowHiddenByIntegration(t *testing.T) {
	live := []map[string]any{{"entity_id": "light.x", "name": "Managed", "hidden_by": "integration"}}
	originals := map[string]map[string]any{"entity:light.x": {"name": "Original"}}
	ops := Plan(Desired{}, live, originals, RefResolver{})

	if len(ops) != 1 || ops[0].Kind != registries.KindError {
		t.Fatalf("ops = %+v", ops)
	}
	if !strings.Contains(ops[0].Error, "cannot restore") {
		t.Errorf("error = %q", ops[0].Error)
	}
}

func TestPlanRefusesManageWhenDisabledByIntegrationEvenIfOnlyHiddenDeclared(t *testing.T) {
	// The guard runs per-entity, not per-field: declaring only "hidden"
	// does not exempt an entity whose disabled_by is non-user/non-null.
	live := []map[string]any{{"entity_id": "light.x", "disabled_by": "integration"}}
	desired := Desired{Entities: []map[string]any{{"entity_id": "light.x", "hidden": true}}}
	ops := Plan(desired, live, nil, RefResolver{})

	if len(ops) != 1 || ops[0].Kind != registries.KindError {
		t.Fatalf("ops = %+v", ops)
	}
}

// --- Plan(): restore tolerates a poisoned entity_originals value ----------

func TestPlanRestoreSkipsPoisonedDisabledByFieldButRestoresOthers(t *testing.T) {
	// A disabled_by of "integration" recorded before the clamp existed
	// would fail HA's update schema (only null/"user" are valid), so the
	// field must be dropped from what is sent, not the whole restore op.
	live := []map[string]any{{"entity_id": "light.x", "name": "Managed", "disabled_by": nil}}
	originals := map[string]map[string]any{"entity:light.x": {"name": "Original", "disabled_by": "integration"}}
	ops := Plan(Desired{}, live, originals, RefResolver{})

	if len(ops) != 1 || ops[0].Kind != KindRestore {
		t.Fatalf("ops = %+v", ops)
	}
	if _, present := ops[0].Params["disabled_by"]; present {
		t.Errorf("params = %+v, want disabled_by dropped", ops[0].Params)
	}
	if ops[0].Params["name"] != "Original" {
		t.Errorf("params = %+v, want name still restored", ops[0].Params)
	}
}

func TestPlanRestoreSkipsPoisonedHiddenByField(t *testing.T) {
	live := []map[string]any{{"entity_id": "light.x", "hidden_by": nil}}
	originals := map[string]map[string]any{"entity:light.x": {"hidden_by": "config_entry"}}
	ops := Plan(Desired{}, live, originals, RefResolver{})

	if len(ops) != 1 || ops[0].Kind != KindRestore {
		t.Fatalf("ops = %+v", ops)
	}
	if len(ops[0].Params) != 0 {
		t.Errorf("params = %+v, want empty - the only recorded field was poisoned", ops[0].Params)
	}
}

func TestPlanRestoreLeavesLegitimateUserAndNullByValuesUntouched(t *testing.T) {
	live := []map[string]any{{"entity_id": "light.x", "disabled_by": nil, "hidden_by": nil}}
	originals := map[string]map[string]any{"entity:light.x": {"disabled_by": "user", "hidden_by": nil}}
	ops := Plan(Desired{}, live, originals, RefResolver{})

	if len(ops) != 1 || ops[0].Kind != KindRestore {
		t.Fatalf("ops = %+v", ops)
	}
	want := map[string]any{"disabled_by": "user", "hidden_by": nil}
	if !reflect.DeepEqual(ops[0].Params, want) {
		t.Errorf("params = %+v, want %+v", ops[0].Params, want)
	}
}

// --- Plan(): first management / drift / no-drift --------------------------

func TestPlanFirstManagementAlwaysEmitsUpdateEvenWithNoDrift(t *testing.T) {
	live := []map[string]any{{"entity_id": "light.x", "name": "Same"}}
	desired := Desired{Entities: []map[string]any{{"entity_id": "light.x", "name": "Same"}}}
	ops := Plan(desired, live, nil, RefResolver{})

	if len(ops) != 1 || ops[0].Kind != KindUpdate {
		t.Fatalf("ops = %+v", ops)
	}
	if !strings.Contains(ops[0].DiffText, "no field changes needed") {
		t.Errorf("diff = %q", ops[0].DiffText)
	}
}

func TestPlanAlreadyManagedNoDriftNoNewFieldEmitsNoOp(t *testing.T) {
	live := []map[string]any{{"entity_id": "light.x", "name": "Same"}}
	desired := Desired{Entities: []map[string]any{{"entity_id": "light.x", "name": "Same"}}}
	originals := map[string]map[string]any{"entity:light.x": {"name": "OriginalName"}}
	ops := Plan(desired, live, originals, RefResolver{})

	if len(ops) != 0 {
		t.Errorf("ops = %+v, want none", ops)
	}
}

func TestPlanAlreadyManagedWithDriftEmitsUpdate(t *testing.T) {
	live := []map[string]any{{"entity_id": "light.x", "name": "LiveName"}}
	desired := Desired{Entities: []map[string]any{{"entity_id": "light.x", "name": "ManifestName"}}}
	originals := map[string]map[string]any{"entity:light.x": {"name": "OriginalName"}}
	ops := Plan(desired, live, originals, RefResolver{})

	if len(ops) != 1 || ops[0].Kind != KindUpdate {
		t.Fatalf("ops = %+v", ops)
	}
	if !reflect.DeepEqual(ops[0].Params, map[string]any{"name": "ManifestName"}) {
		t.Errorf("params = %+v", ops[0].Params)
	}
}

func TestPlanNewlyDeclaredFieldOnAlreadyManagedEntityEmitsUpdateEvenWithNoValueDrift(t *testing.T) {
	// Managed for "name" only; the newly declared "icon" already matches
	// live, but an op must still fire so its original gets recorded.
	live := []map[string]any{{"entity_id": "light.x", "name": "Same", "icon": "mdi:lightbulb"}}
	desired := Desired{Entities: []map[string]any{{"entity_id": "light.x", "name": "Same", "icon": "mdi:lightbulb"}}}
	originals := map[string]map[string]any{"entity:light.x": {"name": "Same"}}
	ops := Plan(desired, live, originals, RefResolver{})

	if len(ops) != 1 || ops[0].Kind != KindUpdate {
		t.Fatalf("ops = %+v", ops)
	}
}

func TestPlanForwardParamsOnlyDeclaredFieldsAreCompared(t *testing.T) {
	live := []map[string]any{{"entity_id": "light.x", "name": "Same", "icon": "mdi:something-else"}}
	desired := Desired{Entities: []map[string]any{{"entity_id": "light.x", "name": "Same"}}}
	originals := map[string]map[string]any{"entity:light.x": {"name": "Same"}}
	ops := Plan(desired, live, originals, RefResolver{})

	if len(ops) != 0 {
		t.Errorf("ops = %+v, want none - icon was never declared, so it must never be compared", ops)
	}
}

func TestPlanZeroDeclaredFieldsIsANoOp(t *testing.T) {
	live := []map[string]any{{"entity_id": "light.x", "name": "Same"}}
	desired := Desired{Entities: []map[string]any{{"entity_id": "light.x"}}}
	ops := Plan(desired, live, nil, RefResolver{})

	if len(ops) != 0 {
		t.Errorf("ops = %+v, want none", ops)
	}
}

// --- Plan(): disabled/hidden mapping ---------------------------------------

func TestPlanDisabledHiddenMapToDisabledByHiddenBy(t *testing.T) {
	live := []map[string]any{{"entity_id": "light.x", "disabled_by": nil, "hidden_by": nil}}
	desired := Desired{Entities: []map[string]any{{"entity_id": "light.x", "disabled": true, "hidden": true}}}
	ops := Plan(desired, live, nil, RefResolver{})

	if len(ops) != 1 {
		t.Fatalf("ops = %+v", ops)
	}
	want := map[string]any{"disabled_by": "user", "hidden_by": "user"}
	if !reflect.DeepEqual(ops[0].Params, want) {
		t.Errorf("params = %+v, want %+v", ops[0].Params, want)
	}
}

func TestPlanDisabledFalseMapsToNilDisabledBy(t *testing.T) {
	live := []map[string]any{{"entity_id": "light.x", "disabled_by": "user"}}
	desired := Desired{Entities: []map[string]any{{"entity_id": "light.x", "disabled": false}}}
	ops := Plan(desired, live, nil, RefResolver{})

	if len(ops) != 1 {
		t.Fatalf("ops = %+v", ops)
	}
	if v, ok := ops[0].Params["disabled_by"]; !ok || v != nil {
		t.Errorf("params[disabled_by] = %+v, want nil", ops[0].Params["disabled_by"])
	}
}

// --- Plan(): area/labels ref resolution -------------------------------------

func TestRefResolverManifestIDViaManaged(t *testing.T) {
	registriesDesired := registries.Desired{Areas: []map[string]any{{"id": "living_room", "name": "Living room"}}}
	managed := map[string]string{"area:living_room": "A1"}
	refs := NewRefResolver(registriesDesired, managed, nil, nil)

	id, err := refs.Resolve("area", "living_room")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "A1" {
		t.Errorf("id = %q, want A1", id)
	}
}

func TestRefResolverManifestIDNotYetLiveIsAnError(t *testing.T) {
	registriesDesired := registries.Desired{Areas: []map[string]any{{"id": "living_room", "name": "Living room"}}}
	refs := NewRefResolver(registriesDesired, map[string]string{}, nil, nil)

	_, err := refs.Resolve("area", "living_room")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "no live id yet") {
		t.Errorf("error = %q", err.Error())
	}
	// A same-cycle area/label create resolves on the next reconcile, so
	// the message must read as transient, not as a permanent failure.
	if !strings.Contains(err.Error(), "resolves automatically on the next cycle") {
		t.Errorf("error = %q, want it to explain this is transient, not a permanent failure", err.Error())
	}
}

func TestRefResolverFallsBackToLiveIDDirectly(t *testing.T) {
	liveAreas := []map[string]any{{"area_id": "A1", "name": "Living room"}}
	refs := NewRefResolver(registries.Desired{}, nil, liveAreas, nil)

	id, err := refs.Resolve("area", "A1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "A1" {
		t.Errorf("id = %q, want A1", id)
	}
}

func TestRefResolverUnresolvedIsAnError(t *testing.T) {
	refs := NewRefResolver(registries.Desired{}, nil, nil, nil)
	_, err := refs.Resolve("label", "nope")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error = %q", err.Error())
	}
}

func TestPlanAreaRefUnresolvedIsAnErrorOp(t *testing.T) {
	live := []map[string]any{{"entity_id": "light.x", "name": "X"}}
	desired := Desired{Entities: []map[string]any{{"entity_id": "light.x", "area": "nope"}}}
	ops := Plan(desired, live, nil, RefResolver{})

	if len(ops) != 1 || ops[0].Kind != registries.KindError {
		t.Fatalf("ops = %+v", ops)
	}
	if !strings.Contains(ops[0].Error, "not found") {
		t.Errorf("error = %q", ops[0].Error)
	}
}

func TestPlanAreaAndLabelsResolveEndToEnd(t *testing.T) {
	live := []map[string]any{{"entity_id": "light.x", "name": "X"}}
	liveAreas := []map[string]any{{"area_id": "A1", "name": "Living room"}}
	liveLabels := []map[string]any{{"label_id": "L1", "name": "GitOps"}}
	refs := NewRefResolver(registries.Desired{}, nil, liveAreas, liveLabels)
	desired := Desired{Entities: []map[string]any{{"entity_id": "light.x", "area": "A1", "labels": []any{"L1"}}}}

	ops := Plan(desired, live, nil, refs)
	if len(ops) != 1 || ops[0].Kind != KindUpdate {
		t.Fatalf("ops = %+v", ops)
	}
	want := map[string]any{"area_id": "A1", "labels": []any{"L1"}}
	if !reflect.DeepEqual(ops[0].Params, want) {
		t.Errorf("params = %+v, want %+v", ops[0].Params, want)
	}
}

func TestPlanAreaNullClearsAreaIDWithoutResolution(t *testing.T) {
	live := []map[string]any{{"entity_id": "light.x", "name": "X", "area_id": "A1"}}
	desired := Desired{Entities: []map[string]any{{"entity_id": "light.x", "area": nil}}}
	ops := Plan(desired, live, nil, RefResolver{})

	if len(ops) != 1 || ops[0].Kind != KindUpdate {
		t.Fatalf("ops = %+v", ops)
	}
	if v, ok := ops[0].Params["area_id"]; !ok || v != nil {
		t.Errorf("params[area_id] = %+v, want nil", ops[0].Params["area_id"])
	}
}

// --- Plan(): restore-on-unmanage --------------------------------------------

func TestPlanRestoresOnRemovalFromManifest(t *testing.T) {
	live := []map[string]any{{"entity_id": "light.x", "name": "Managed", "icon": "mdi:new"}}
	originals := map[string]map[string]any{"entity:light.x": {"name": "Original", "icon": "mdi:old"}}
	ops := Plan(Desired{}, live, originals, RefResolver{})

	if len(ops) != 1 {
		t.Fatalf("ops = %+v", ops)
	}
	op := ops[0]
	if op.Kind != KindRestore || op.RType != "entity" || op.Key != "light.x" {
		t.Errorf("op = %+v", op)
	}
	want := map[string]any{"name": "Original", "icon": "mdi:old"}
	if !reflect.DeepEqual(op.Params, want) {
		t.Errorf("params = %+v, want %+v", op.Params, want)
	}
}

func TestPlanRestoreWithZeroDeclaredFieldsButStillDeclaredEntityID(t *testing.T) {
	live := []map[string]any{{"entity_id": "light.x", "name": "Managed"}}
	desired := Desired{Entities: []map[string]any{{"entity_id": "light.x"}}}
	originals := map[string]map[string]any{"entity:light.x": {"name": "Original"}}
	ops := Plan(desired, live, originals, RefResolver{})

	if len(ops) != 1 || ops[0].Kind != KindRestore {
		t.Fatalf("ops = %+v, want a single restore op", ops)
	}
}

func TestPlanRestoreNoOpWhenNoLongerManaged(t *testing.T) {
	// Removed from the manifest and never in originals: nothing to do.
	live := []map[string]any{{"entity_id": "light.x", "name": "Untouched"}}
	ops := Plan(Desired{}, live, nil, RefResolver{})

	if len(ops) != 0 {
		t.Errorf("ops = %+v, want none", ops)
	}
}

func TestPlanRestoreEntityGoneIsAnErrorOp(t *testing.T) {
	originals := map[string]map[string]any{"entity:light.gone": {"name": "Original"}}
	ops := Plan(Desired{}, nil, originals, RefResolver{})

	if len(ops) != 1 || ops[0].Kind != registries.KindError {
		t.Fatalf("ops = %+v", ops)
	}
	if !strings.Contains(ops[0].Error, "entity not found") {
		t.Errorf("error = %q", ops[0].Error)
	}
}

func TestPlanKeepsManagingWhenStillDeclaredEvenWithAnError(t *testing.T) {
	// Still declared with real fields but hit a resolution error: not
	// eligible for restore.
	live := []map[string]any{{"entity_id": "light.x", "name": "X"}}
	desired := Desired{Entities: []map[string]any{{"entity_id": "light.x", "area": "nope"}}}
	originals := map[string]map[string]any{"entity:light.x": {"name": "Original"}}
	ops := Plan(desired, live, originals, RefResolver{})

	if len(ops) != 1 || ops[0].Kind != registries.KindError {
		t.Fatalf("ops = %+v, want a single error op, not a restore", ops)
	}
}

func TestPlanRestoreNoDriftStillEmitsOpToDropBookkeeping(t *testing.T) {
	live := []map[string]any{{"entity_id": "light.x", "name": "Original"}}
	originals := map[string]map[string]any{"entity:light.x": {"name": "Original"}}
	ops := Plan(Desired{}, live, originals, RefResolver{})

	if len(ops) != 1 || ops[0].Kind != KindRestore {
		t.Fatalf("ops = %+v", ops)
	}
	if !strings.Contains(ops[0].DiffText, "already match") {
		t.Errorf("diff = %q", ops[0].DiffText)
	}
}
