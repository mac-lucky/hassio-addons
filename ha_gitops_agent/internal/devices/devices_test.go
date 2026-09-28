package devices

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/entities"
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

func loadErr(t *testing.T, content string) string {
	t.Helper()
	workdir, gitops := mkGitops(t)
	writeFile(t, gitops, "devices.yaml", content)
	_, err := LoadManifest(workdir)
	if err == nil {
		t.Fatalf("content %q: expected an error", content)
	}
	return err.Error()
}

// liveDevice is a config/device_registry/list entry with the fields this
// layer reads; extra ones merge over it.
func liveDevice(id, name string, extra map[string]any) map[string]any {
	obj := map[string]any{
		"id": id, "name": name, "name_by_user": nil, "area_id": nil, "labels": []any{},
		"disabled_by": nil, "identifiers": []any{}, "manufacturer": "Espressif", "model": "ESP32",
	}
	for k, v := range extra {
		obj[k] = v
	}
	return obj
}

func entry(id string, match Match, fields map[string]any) Device {
	if fields == nil {
		fields = map[string]any{}
	}
	return Device{ID: id, Match: match, Fields: fields}
}

func byName(name string) Match { return Match{Name: name} }

// --- LoadManifest(): missing/empty ------------------------------------

func TestMissingDevicesFileIsNotAnError(t *testing.T) {
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
	got, err := LoadManifest(t.TempDir())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !reflect.DeepEqual(got, emptyDesired()) {
		t.Errorf("got %+v, want empty Desired", got)
	}
}

func TestEmptyDevicesKeyIsNotAnError(t *testing.T) {
	workdir, gitops := mkGitops(t)
	writeFile(t, gitops, "devices.yaml", "devices:\n")
	got, err := LoadManifest(workdir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !reflect.DeepEqual(got, emptyDesired()) {
		t.Errorf("got %+v, want empty Desired", got)
	}
}

func TestLoadManifestTopLevelShapes(t *testing.T) {
	for _, tc := range []struct{ yaml, want string }{
		{"- a\n- b\n", "top level must be a mapping"},
		{"devices: kitchen\n", "devices must be a list"},
		{"devices: [\n", "invalid YAML"},
		{"devices:\n  - just_a_string\n", "devices[0] is not a mapping"},
	} {
		if msg := loadErr(t, tc.yaml); !strings.Contains(msg, tc.want) {
			t.Errorf("content %q: error = %q, want %q", tc.yaml, msg, tc.want)
		}
	}
}

// --- LoadManifest(): happy path -----------------------------------------

func TestLoadManifestParsesAllKnownFields(t *testing.T) {
	workdir, gitops := mkGitops(t)
	writeFile(t, gitops, "devices.yaml", `
devices:
  - id: kitchen_strip
    match:
      name: LED Kitchen
      identifier: "esphome:aa:bb:cc:dd:ee:ff"
      device_id: f66eece92f36df1e909d27f0d866b1b7
    name: Kitchen LED strip
    area: kitchen
    labels: [lighting]
    disabled: false
`)
	desired, err := LoadManifest(workdir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []Device{{
		ID: "kitchen_strip",
		Match: Match{
			DeviceID: "f66eece92f36df1e909d27f0d866b1b7", Name: "LED Kitchen", Identifier: "esphome:aa:bb:cc:dd:ee:ff",
		},
		Fields: map[string]any{
			"name": "Kitchen LED strip", "area": "kitchen", "labels": []any{"lighting"}, "disabled": false,
		},
	}}
	if !reflect.DeepEqual(desired.Devices, want) {
		t.Errorf("devices = %+v, want %+v", desired.Devices, want)
	}
}

func TestLoadManifestEntryWithOnlyIDAndMatchIsValid(t *testing.T) {
	workdir, gitops := mkGitops(t)
	writeFile(t, gitops, "devices.yaml", "devices:\n  - id: x\n    match: {name: X}\n")
	desired, err := LoadManifest(workdir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []Device{{ID: "x", Match: Match{Name: "X"}, Fields: map[string]any{}}}
	if !reflect.DeepEqual(desired.Devices, want) {
		t.Errorf("devices = %+v, want %+v", desired.Devices, want)
	}
}

func TestLoadManifestNullsAreKeptAsExplicitClears(t *testing.T) {
	workdir, gitops := mkGitops(t)
	writeFile(t, gitops, "devices.yaml", "devices:\n  - id: x\n    match: {name: X}\n    name: null\n    area: null\n")
	desired, err := LoadManifest(workdir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := map[string]any{"name": nil, "area": nil}
	if !reflect.DeepEqual(desired.Devices[0].Fields, want) {
		t.Errorf("fields = %+v, want %+v", desired.Devices[0].Fields, want)
	}
}

// --- LoadManifest(): validation ------------------------------------------

func TestLoadManifestInvalidID(t *testing.T) {
	for _, content := range []string{
		"devices:\n  - match: {name: X}\n",
		"devices:\n  - id: \"\"\n    match: {name: X}\n",
		"devices:\n  - id: Kitchen\n    match: {name: X}\n",
		"devices:\n  - id: kitchen-strip\n    match: {name: X}\n",
		"devices:\n  - id: 42\n    match: {name: X}\n",
	} {
		if msg := loadErr(t, content); !strings.Contains(msg, "invalid or missing 'id'") {
			t.Errorf("content %q: error = %q", content, msg)
		}
	}
}

func TestLoadManifestDuplicateID(t *testing.T) {
	msg := loadErr(t, "devices:\n  - id: x\n    match: {name: A}\n  - id: x\n    match: {name: B}\n")
	if !strings.Contains(msg, "duplicate id 'x'") {
		t.Errorf("error = %q", msg)
	}
}

func TestLoadManifestMatchValidation(t *testing.T) {
	for _, tc := range []struct{ name, yaml, want string }{
		{"missing", "devices:\n  - id: x\n    name: X\n", "needs a 'match' mapping"},
		{"not a mapping", "devices:\n  - id: x\n    match: LED Kitchen\n", "needs a 'match' mapping"},
		{"empty", "devices:\n  - id: x\n    match: {}\n", "match needs at least one of device_id, name, identifier"},
		{"unknown key", "devices:\n  - id: x\n    match: {name: X, model: ESP32}\n", "match has unsupported key(s) model"},
		{"number", "devices:\n  - id: x\n    match: {device_id: 1234}\n", "match.device_id must be a non-empty string"},
		{"empty string", "devices:\n  - id: x\n    match: {name: \"\"}\n", "match.name must be a non-empty string"},
		{"identifier without colon", "devices:\n  - id: x\n    match: {identifier: esphome}\n", "match.identifier must be \"domain:value\""},
		{"identifier without value", "devices:\n  - id: x\n    match: {identifier: \"esphome:\"}\n", "match.identifier must be \"domain:value\""},
		{"identifier without domain", "devices:\n  - id: x\n    match: {identifier: \":aa:bb\"}\n", "match.identifier must be \"domain:value\""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if msg := loadErr(t, tc.yaml); !strings.Contains(msg, tc.want) {
				t.Errorf("error = %q, want %q", msg, tc.want)
			}
		})
	}
}

func TestLoadManifestUnsupportedFieldIsRejected(t *testing.T) {
	msg := loadErr(t, "devices:\n  - id: x\n    match: {name: X}\n    name_by_user: Y\n    icon: mdi:x\n")
	if !strings.Contains(msg, "unsupported field(s) icon, name_by_user") {
		t.Errorf("error = %q", msg)
	}
}

func TestLoadManifestFieldTypes(t *testing.T) {
	for _, tc := range []struct{ name, field, want string }{
		{"name number", "name: 42", "name must be a non-empty string or null"},
		{"name empty", "name: \"\"", "name must be a non-empty string or null"},
		{"area list", "area: [kitchen]", "area must be a non-empty string or null"},
		{"labels scalar", "labels: lighting", "labels must be a list of non-empty strings"},
		{"labels number element", "labels: [1]", "labels must be a list of non-empty strings"},
		{"disabled string", "disabled: yes-please", "disabled must be a boolean"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msg := loadErr(t, "devices:\n  - id: x\n    match: {name: X}\n    "+tc.field+"\n")
			if !strings.Contains(msg, tc.want) {
				t.Errorf("error = %q, want %q", msg, tc.want)
			}
		})
	}
}

func TestLoadManifestAggregatesEveryProblem(t *testing.T) {
	msg := loadErr(t, `
devices:
  - id: Bad
    match: {name: X}
  - id: y
    match: {}
    icon: mdi:x
  - id: z
    match: {name: Z}
    disabled: maybe
`)
	for _, want := range []string{
		"invalid or missing 'id'", "device 'y' has unsupported field(s) icon", "device 'y' match needs at least one",
		"device 'z' disabled must be a boolean",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q does not contain %q", msg, want)
		}
	}
}

func TestLoadManifestErrorIsAManifestError(t *testing.T) {
	workdir, gitops := mkGitops(t)
	writeFile(t, gitops, "devices.yaml", "devices:\n  - id: x\n")
	_, err := LoadManifest(workdir)
	var me *ManifestError
	if !errors.As(err, &me) || len(me.Problems) != 1 {
		t.Errorf("err = %#v, want a *ManifestError with one problem", err)
	}
}

// --- Plan(): matching --------------------------------------------------------

func TestPlanNoMatchIsAnErrorOpNeverACreate(t *testing.T) {
	live := []map[string]any{liveDevice("D1", "Other", nil)}
	desired := Desired{Devices: []Device{entry("kitchen_strip", byName("LED Kitchen"), map[string]any{"name": "Strip"})}}
	ops := Plan(desired, live, nil, entities.RefResolver{})

	if len(ops) != 1 {
		t.Fatalf("ops = %+v", ops)
	}
	op := ops[0]
	if op.Kind != registries.KindError || op.RType != "device" || op.Key != "kitchen_strip" {
		t.Errorf("op = %+v", op)
	}
	if op.Error != `no device matches name "LED Kitchen"` {
		t.Errorf("error = %q", op.Error)
	}
}

func TestPlanAmbiguousMatchNamesEveryCandidate(t *testing.T) {
	live := []map[string]any{
		liveDevice("D3", "Hue bulb", nil), liveDevice("D1", "Hue bulb", nil),
		liveDevice("D2", "hue BULB", nil), liveDevice("D4", "Hue lamp", nil),
	}
	desired := Desired{Devices: []Device{entry("bulb", byName("Hue bulb"), map[string]any{"name": "Desk"})}}
	ops := Plan(desired, live, nil, entities.RefResolver{})

	if len(ops) != 1 || ops[0].Kind != registries.KindError {
		t.Fatalf("ops = %+v", ops)
	}
	if !strings.HasPrefix(ops[0].Error, `3 devices match name "Hue bulb": D1, D2, D3;`) {
		t.Errorf("error = %q", ops[0].Error)
	}
}

func TestPlanMatchByNameIsCaseInsensitiveAgainstTheIntegrationName(t *testing.T) {
	live := []map[string]any{liveDevice("D1", "LED Kitchen", nil)}
	desired := Desired{Devices: []Device{entry("strip", byName("led kitchen"), map[string]any{"name": "Strip"})}}
	ops := Plan(desired, live, nil, entities.RefResolver{})

	if len(ops) != 1 || ops[0].Kind != KindUpdate || ops[0].LiveID != "D1" || ops[0].Key != "strip" {
		t.Fatalf("ops = %+v", ops)
	}
}

// The entry's own rename must not break its match on the next cycle.
func TestPlanMatchByNameIgnoresNameByUser(t *testing.T) {
	live := []map[string]any{liveDevice("D1", "LED Kitchen", map[string]any{"name_by_user": "Strip"})}

	byOriginal := Desired{Devices: []Device{entry("strip", byName("LED Kitchen"), map[string]any{"name": "Strip"})}}
	ops := Plan(byOriginal, live, map[string]map[string]any{"device:D1": {"name_by_user": nil}}, entities.RefResolver{})
	if len(ops) != 0 {
		t.Errorf("ops = %+v, want none - matched by the integration name, already renamed", ops)
	}

	byUserName := Desired{Devices: []Device{entry("strip", byName("Strip"), map[string]any{"name": "Strip"})}}
	ops = Plan(byUserName, live, nil, entities.RefResolver{})
	if len(ops) != 1 || ops[0].Kind != registries.KindError || !strings.Contains(ops[0].Error, "no device matches") {
		t.Errorf("ops = %+v, want a no-match error - name_by_user is not what match.name compares", ops)
	}
}

func TestPlanMatchByIdentifier(t *testing.T) {
	live := []map[string]any{
		liveDevice("D1", "Plug", map[string]any{"identifiers": []any{[]any{"esphome", "aa:bb:cc:dd:ee:01"}}}),
		liveDevice("D2", "Plug", map[string]any{"identifiers": []any{
			[]any{"mqtt", "plug2"}, []any{"esphome", "aa:bb:cc:dd:ee:02"},
		}}),
	}
	desired := Desired{Devices: []Device{
		entry("plug", Match{Identifier: "esphome:aa:bb:cc:dd:ee:02"}, map[string]any{"name": "Desk plug"}),
	}}
	ops := Plan(desired, live, nil, entities.RefResolver{})

	if len(ops) != 1 || ops[0].Kind != KindUpdate || ops[0].LiveID != "D2" {
		t.Fatalf("ops = %+v", ops)
	}
}

func TestPlanMatchByIdentifierIsExact(t *testing.T) {
	live := []map[string]any{
		liveDevice("D1", "Plug", map[string]any{"identifiers": []any{[]any{"esphome", "AA:BB:CC:DD:EE:01"}}}),
	}
	desired := Desired{Devices: []Device{
		entry("plug", Match{Identifier: "esphome:aa:bb:cc:dd:ee:01"}, map[string]any{"name": "Desk plug"}),
	}}
	ops := Plan(desired, live, nil, entities.RefResolver{})

	if len(ops) != 1 || ops[0].Kind != registries.KindError {
		t.Fatalf("ops = %+v, want a no-match error", ops)
	}
}

// HomeKit Controller's domain half carries a colon of its own, and its
// legacy ones a hyphen; splitting the manifest value at any one colon, or
// holding the domain to an integration-domain pattern, misses both.
func TestPlanMatchByIdentifierWithColonsAndHyphensInTheDomain(t *testing.T) {
	live := []map[string]any{
		liveDevice("D1", "Bridge", map[string]any{"identifiers": []any{
			[]any{"homekit_controller:accessory-id", "00:11:22:33:44:55:aid:1"},
		}}),
		liveDevice("D2", "Sensor", map[string]any{"identifiers": []any{
			[]any{"accessory-id", "00:11:22:33:44:55:aid:7"},
		}}),
	}
	for _, tc := range []struct{ identifier, want string }{
		{"homekit_controller:accessory-id:00:11:22:33:44:55:aid:1", "D1"},
		{"accessory-id:00:11:22:33:44:55:aid:7", "D2"},
	} {
		desired := Desired{Devices: []Device{entry("hk", Match{Identifier: tc.identifier}, map[string]any{"name": "X"})}}
		ops := Plan(desired, live, nil, entities.RefResolver{})

		if len(ops) != 1 || ops[0].Kind != KindUpdate || ops[0].LiveID != tc.want {
			t.Errorf("identifier %q: ops = %+v, want an update of %s", tc.identifier, ops, tc.want)
		}
	}
}

func TestLoadManifestAcceptsHomeKitIdentifiers(t *testing.T) {
	workdir, gitops := mkGitops(t)
	writeFile(t, gitops, "devices.yaml", `
devices:
  - id: bridge
    match: {identifier: "homekit_controller:accessory-id:00:11:22:33:44:55:aid:1"}
  - id: sensor
    match: {identifier: "serial-number:ABC123"}
`)
	desired, err := LoadManifest(workdir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(desired.Devices) != 2 || desired.Devices[1].Match.Identifier != "serial-number:ABC123" {
		t.Errorf("devices = %+v", desired.Devices)
	}
}

func TestPlanMatchByDeviceID(t *testing.T) {
	live := []map[string]any{liveDevice("D1", "Plug", nil), liveDevice("D2", "Plug", nil)}
	desired := Desired{Devices: []Device{entry("plug", Match{DeviceID: "D2"}, map[string]any{"name": "Desk plug"})}}
	ops := Plan(desired, live, nil, entities.RefResolver{})

	if len(ops) != 1 || ops[0].Kind != KindUpdate || ops[0].LiveID != "D2" {
		t.Fatalf("ops = %+v", ops)
	}
}

func TestPlanEveryMatchKeyGivenMustHold(t *testing.T) {
	live := []map[string]any{liveDevice("D1", "Plug", nil)}
	desired := Desired{Devices: []Device{entry("plug", Match{DeviceID: "D1", Name: "Lamp"}, map[string]any{"name": "X"})}}
	ops := Plan(desired, live, nil, entities.RefResolver{})

	if len(ops) != 1 || ops[0].Kind != registries.KindError {
		t.Fatalf("ops = %+v", ops)
	}
	if ops[0].Error != `no device matches device_id "D1" and name "Lamp"` {
		t.Errorf("error = %q", ops[0].Error)
	}
}

func TestPlanTwoEntriesOnOneDeviceAreBothErrors(t *testing.T) {
	live := []map[string]any{liveDevice("D1", "Plug", map[string]any{"identifiers": []any{[]any{"zha", "00:11"}}})}
	desired := Desired{Devices: []Device{
		entry("a", byName("Plug"), map[string]any{"name": "A"}),
		entry("b", Match{Identifier: "zha:00:11"}, map[string]any{"name": "B"}),
	}}
	ops := Plan(desired, live, nil, entities.RefResolver{})

	if len(ops) != 2 {
		t.Fatalf("ops = %+v", ops)
	}
	for i, key := range []string{"a", "b"} {
		if ops[i].Kind != registries.KindError || ops[i].Key != key {
			t.Errorf("ops[%d] = %+v", i, ops[i])
		}
		if !strings.Contains(ops[i].Error, `device "Plug" (D1) is matched by entries 'a', 'b'`) {
			t.Errorf("ops[%d].Error = %q", i, ops[i].Error)
		}
	}
}

func TestPlanTwoEntriesOnOneManagedDeviceNeverRestoreIt(t *testing.T) {
	live := []map[string]any{liveDevice("D1", "Plug", map[string]any{"name_by_user": "A"})}
	desired := Desired{Devices: []Device{
		entry("a", byName("Plug"), map[string]any{"name": "A"}),
		entry("b", Match{DeviceID: "D1"}, map[string]any{"name": "B"}),
	}}
	originals := map[string]map[string]any{"device:D1": {"name_by_user": nil}}
	ops := Plan(desired, live, originals, entities.RefResolver{})

	for _, op := range ops {
		if op.Kind != registries.KindError {
			t.Errorf("op = %+v, want only the two conflict errors", op)
		}
	}
}

// --- Plan(): disabled_by guard -----------------------------------------------

func TestPlanRefusesDisabledOnDeviceDisabledByIntegration(t *testing.T) {
	for _, by := range []string{"integration", "config_entry"} {
		live := []map[string]any{liveDevice("D1", "Plug", map[string]any{"disabled_by": by})}
		desired := Desired{Devices: []Device{entry("plug", byName("Plug"), map[string]any{"disabled": false, "name": "X"})}}
		ops := Plan(desired, live, nil, entities.RefResolver{})

		if len(ops) != 1 || ops[0].Kind != registries.KindError {
			t.Fatalf("%s: ops = %+v", by, ops)
		}
		if !strings.Contains(ops[0].Error, `disabled by "`+by+`", not by a user`) ||
			!strings.Contains(ops[0].Error, "drop 'disabled'") {
			t.Errorf("%s: error = %q", by, ops[0].Error)
		}
	}
}

func TestPlanAllowsDisabledOnDeviceDisabledByUser(t *testing.T) {
	live := []map[string]any{liveDevice("D1", "Plug", map[string]any{"disabled_by": "user"})}
	desired := Desired{Devices: []Device{entry("plug", byName("Plug"), map[string]any{"disabled": false})}}
	ops := Plan(desired, live, nil, entities.RefResolver{})

	if len(ops) != 1 || ops[0].Kind != KindUpdate {
		t.Fatalf("ops = %+v", ops)
	}
	if v, ok := ops[0].Params["disabled_by"]; !ok || v != nil {
		t.Errorf("params = %+v, want disabled_by nil", ops[0].Params)
	}
}

// Only disabled fights the integration; renaming a device it disabled does
// not, unlike entities' per-entity guard.
func TestPlanDeviceDisabledByIntegrationCanStillBeRenamed(t *testing.T) {
	live := []map[string]any{liveDevice("D1", "Plug", map[string]any{"disabled_by": "integration"})}
	desired := Desired{Devices: []Device{entry("plug", byName("Plug"), map[string]any{"name": "Desk plug"})}}
	ops := Plan(desired, live, nil, entities.RefResolver{})

	if len(ops) != 1 || ops[0].Kind != KindUpdate {
		t.Fatalf("ops = %+v", ops)
	}
}

func TestPlanRefusesRestoreOfDisabledByWhenNowDisabledByIntegration(t *testing.T) {
	live := []map[string]any{liveDevice("D1", "Plug", map[string]any{"disabled_by": "config_entry"})}
	originals := map[string]map[string]any{"device:D1": {"disabled_by": "user", "name_by_user": nil}}
	ops := Plan(Desired{}, live, originals, entities.RefResolver{})

	if len(ops) != 1 || ops[0].Kind != registries.KindError {
		t.Fatalf("ops = %+v", ops)
	}
	if !strings.Contains(ops[0].Error, "cannot restore device") || !strings.Contains(ops[0].Error, `"config_entry"`) {
		t.Errorf("error = %q", ops[0].Error)
	}
}

func TestPlanRestoreWithoutDisabledByIgnoresTheGuard(t *testing.T) {
	live := []map[string]any{liveDevice("D1", "Plug", map[string]any{"disabled_by": "integration", "name_by_user": "X"})}
	originals := map[string]map[string]any{"device:D1": {"name_by_user": nil}}
	ops := Plan(Desired{}, live, originals, entities.RefResolver{})

	if len(ops) != 1 || ops[0].Kind != KindRestore {
		t.Fatalf("ops = %+v", ops)
	}
}

// --- Plan(): first management / drift / no-drift --------------------------

func TestPlanFirstManagementAlwaysEmitsUpdateEvenWithNoDrift(t *testing.T) {
	live := []map[string]any{liveDevice("D1", "Plug", map[string]any{"name_by_user": "Desk plug"})}
	desired := Desired{Devices: []Device{entry("plug", byName("Plug"), map[string]any{"name": "Desk plug"})}}
	ops := Plan(desired, live, nil, entities.RefResolver{})

	if len(ops) != 1 || ops[0].Kind != KindUpdate {
		t.Fatalf("ops = %+v", ops)
	}
	if ops[0].DiffText != `now managing plug as device "Desk plug" (D1); no field changes needed` {
		t.Errorf("diff = %q", ops[0].DiffText)
	}
}

func TestPlanAlreadyManagedNoDriftNoNewFieldEmitsNoOp(t *testing.T) {
	live := []map[string]any{liveDevice("D1", "Plug", map[string]any{"name_by_user": "Desk plug"})}
	desired := Desired{Devices: []Device{entry("plug", byName("Plug"), map[string]any{"name": "Desk plug"})}}
	originals := map[string]map[string]any{"device:D1": {"name_by_user": nil}}
	ops := Plan(desired, live, originals, entities.RefResolver{})

	if len(ops) != 0 {
		t.Errorf("ops = %+v, want none", ops)
	}
}

func TestPlanAlreadyManagedWithDriftEmitsUpdate(t *testing.T) {
	live := []map[string]any{liveDevice("D1", "Plug", map[string]any{"name_by_user": "Changed in UI"})}
	desired := Desired{Devices: []Device{entry("plug", byName("Plug"), map[string]any{"name": "Desk plug"})}}
	originals := map[string]map[string]any{"device:D1": {"name_by_user": nil}}
	ops := Plan(desired, live, originals, entities.RefResolver{})

	if len(ops) != 1 || ops[0].Kind != KindUpdate {
		t.Fatalf("ops = %+v", ops)
	}
	if !reflect.DeepEqual(ops[0].Params, map[string]any{"name_by_user": "Desk plug"}) {
		t.Errorf("params = %+v", ops[0].Params)
	}
	for _, want := range []string{
		"--- live/device/D1", "+++ manifest/device/plug", "-name_by_user: 'Changed in UI'", "+name_by_user: 'Desk plug'",
	} {
		if !strings.Contains(ops[0].DiffText, want) {
			t.Errorf("diff = %q, missing %q", ops[0].DiffText, want)
		}
	}
}

func TestPlanNewlyDeclaredFieldOnManagedDeviceEmitsUpdateEvenWithNoValueDrift(t *testing.T) {
	live := []map[string]any{liveDevice("D1", "Plug", map[string]any{"name_by_user": "Desk plug", "labels": []any{"L1"}})}
	liveLabels := []map[string]any{{"label_id": "L1"}}
	refs := entities.NewRefResolver(registries.Desired{}, nil, nil, liveLabels)
	desired := Desired{Devices: []Device{
		entry("plug", byName("Plug"), map[string]any{"name": "Desk plug", "labels": []any{"L1"}}),
	}}
	originals := map[string]map[string]any{"device:D1": {"name_by_user": nil}}
	ops := Plan(desired, live, originals, refs)

	if len(ops) != 1 || ops[0].Kind != KindUpdate {
		t.Fatalf("ops = %+v", ops)
	}
}

func TestPlanOnlyDeclaredFieldsAreCompared(t *testing.T) {
	live := []map[string]any{liveDevice("D1", "Plug", map[string]any{"name_by_user": "Desk plug", "area_id": "elsewhere"})}
	desired := Desired{Devices: []Device{entry("plug", byName("Plug"), map[string]any{"name": "Desk plug"})}}
	originals := map[string]map[string]any{"device:D1": {"name_by_user": nil}}
	ops := Plan(desired, live, originals, entities.RefResolver{})

	if len(ops) != 0 {
		t.Errorf("ops = %+v, want none - area was never declared, so it must never be compared", ops)
	}
}

func TestPlanLabelsCompareOrderInsensitively(t *testing.T) {
	live := []map[string]any{liveDevice("D1", "Plug", map[string]any{"labels": []any{"L2", "L1"}})}
	refs := entities.NewRefResolver(registries.Desired{}, nil, nil, []map[string]any{{"label_id": "L1"}, {"label_id": "L2"}})
	desired := Desired{Devices: []Device{entry("plug", byName("Plug"), map[string]any{"labels": []any{"L1", "L2"}})}}
	originals := map[string]map[string]any{"device:D1": {"labels": []any{}}}
	ops := Plan(desired, live, originals, refs)

	if len(ops) != 0 {
		t.Errorf("ops = %+v, want none", ops)
	}
}

func TestPlanZeroDeclaredFieldsIsANoOp(t *testing.T) {
	live := []map[string]any{liveDevice("D1", "Plug", nil)}
	desired := Desired{Devices: []Device{entry("plug", byName("Plug"), nil)}}
	ops := Plan(desired, live, nil, entities.RefResolver{})

	if len(ops) != 0 {
		t.Errorf("ops = %+v, want none", ops)
	}
}

// --- Plan(): field mapping ------------------------------------------------

func TestPlanFieldsMapOntoDeviceRegistryParams(t *testing.T) {
	live := []map[string]any{liveDevice("D1", "Plug", nil)}
	refs := entities.NewRefResolver(registries.Desired{}, nil,
		[]map[string]any{{"area_id": "kitchen"}}, []map[string]any{{"label_id": "lighting"}})
	desired := Desired{Devices: []Device{entry("plug", byName("Plug"), map[string]any{
		"name": "Desk plug", "area": "kitchen", "labels": []any{"lighting"}, "disabled": true,
	})}}
	ops := Plan(desired, live, nil, refs)

	if len(ops) != 1 {
		t.Fatalf("ops = %+v", ops)
	}
	want := map[string]any{
		"name_by_user": "Desk plug", "area_id": "kitchen", "labels": []any{"lighting"}, "disabled_by": "user",
	}
	if !reflect.DeepEqual(ops[0].Params, want) {
		t.Errorf("params = %+v, want %+v", ops[0].Params, want)
	}
}

// HA keeps device labels as a set, so a repeat sent as-is would come back
// once and read as drift forever.
func TestPlanDeduplicatesResolvedLabels(t *testing.T) {
	registriesDesired := registries.Desired{Labels: []map[string]any{{"id": "lighting", "name": "Lighting"}}}
	refs := entities.NewRefResolver(registriesDesired, map[string]string{"label:lighting": "L1"}, nil,
		[]map[string]any{{"label_id": "L1"}, {"label_id": "L2"}})
	desired := Desired{Devices: []Device{entry("plug", byName("Plug"), map[string]any{
		"labels": []any{"lighting", "L2", "lighting", "L1"},
	})}}

	live := []map[string]any{liveDevice("D1", "Plug", nil)}
	ops := Plan(desired, live, nil, refs)
	if len(ops) != 1 || !reflect.DeepEqual(ops[0].Params["labels"], []any{"L1", "L2"}) {
		t.Fatalf("ops = %+v, want labels [L1 L2] in first-seen order", ops)
	}

	settled := []map[string]any{liveDevice("D1", "Plug", map[string]any{"labels": []any{"L2", "L1"}})}
	originals := map[string]map[string]any{"device:D1": {"labels": []any{}}}
	if ops := Plan(desired, settled, originals, refs); len(ops) != 0 {
		t.Errorf("ops = %+v, want none once HA holds the set", ops)
	}
}

func TestPlanNullsClearWithoutResolution(t *testing.T) {
	live := []map[string]any{liveDevice("D1", "Plug", map[string]any{
		"name_by_user": "Desk plug", "area_id": "kitchen", "labels": []any{"lighting"},
	})}
	desired := Desired{Devices: []Device{entry("plug", byName("Plug"), map[string]any{
		"name": nil, "area": nil, "labels": nil,
	})}}
	ops := Plan(desired, live, nil, entities.RefResolver{})

	if len(ops) != 1 || ops[0].Kind != KindUpdate {
		t.Fatalf("ops = %+v", ops)
	}
	want := map[string]any{"name_by_user": nil, "area_id": nil, "labels": []any{}}
	if !reflect.DeepEqual(ops[0].Params, want) {
		t.Errorf("params = %+v, want %+v", ops[0].Params, want)
	}
}

func TestPlanAreaResolvesThroughRegistriesManifest(t *testing.T) {
	live := []map[string]any{liveDevice("D1", "Plug", nil)}
	registriesDesired := registries.Desired{Areas: []map[string]any{{"id": "kitchen", "name": "Kitchen"}}}
	refs := entities.NewRefResolver(registriesDesired, map[string]string{"area:kitchen": "A1"}, nil, nil)
	desired := Desired{Devices: []Device{entry("plug", byName("Plug"), map[string]any{"area": "kitchen"})}}
	ops := Plan(desired, live, nil, refs)

	if len(ops) != 1 || ops[0].Params["area_id"] != "A1" {
		t.Fatalf("ops = %+v, want area_id A1", ops)
	}
}

func TestPlanUnresolvedRefIsAnErrorOpAndKeepsTheDeviceManaged(t *testing.T) {
	live := []map[string]any{liveDevice("D1", "Plug", map[string]any{"name_by_user": "X"})}
	desired := Desired{Devices: []Device{entry("plug", byName("Plug"), map[string]any{"area": "nope"})}}
	originals := map[string]map[string]any{"device:D1": {"name_by_user": nil}}
	ops := Plan(desired, live, originals, entities.RefResolver{})

	if len(ops) != 1 || ops[0].Kind != registries.KindError || ops[0].Key != "plug" {
		t.Fatalf("ops = %+v, want a single error op, not a restore", ops)
	}
	if !strings.Contains(ops[0].Error, "area 'nope' not found") {
		t.Errorf("error = %q", ops[0].Error)
	}
}

// --- Plan(): restore-on-unmanage --------------------------------------------

func TestPlanRestoresOnRemovalFromManifest(t *testing.T) {
	live := []map[string]any{liveDevice("D1", "Plug", map[string]any{"name_by_user": "Desk plug", "area_id": "kitchen"})}
	originals := map[string]map[string]any{"device:D1": {"name_by_user": nil, "area_id": "hall"}}
	ops := Plan(Desired{}, live, originals, entities.RefResolver{})

	if len(ops) != 1 {
		t.Fatalf("ops = %+v", ops)
	}
	op := ops[0]
	if op.Kind != KindRestore || op.RType != "device" || op.Key != "D1" || op.LiveID != "D1" {
		t.Errorf("op = %+v", op)
	}
	want := map[string]any{"name_by_user": nil, "area_id": "hall"}
	if !reflect.DeepEqual(op.Params, want) {
		t.Errorf("params = %+v, want %+v", op.Params, want)
	}
}

func TestPlanRestoreForAnEntryWithNoFieldsIsKeyedByTheManifestID(t *testing.T) {
	live := []map[string]any{liveDevice("D1", "Plug", map[string]any{"name_by_user": "Desk plug"})}
	desired := Desired{Devices: []Device{entry("plug", byName("Plug"), nil)}}
	originals := map[string]map[string]any{"device:D1": {"name_by_user": nil}}
	ops := Plan(desired, live, originals, entities.RefResolver{})

	if len(ops) != 1 || ops[0].Kind != KindRestore || ops[0].Key != "plug" || ops[0].LiveID != "D1" {
		t.Fatalf("ops = %+v, want a single restore keyed 'plug'", ops)
	}
}

func TestPlanRepointedEntryRestoresTheOldDeviceAndManagesTheNewOne(t *testing.T) {
	live := []map[string]any{
		liveDevice("D1", "Old plug", map[string]any{"name_by_user": "Desk plug"}),
		liveDevice("D2", "New plug", nil),
	}
	desired := Desired{Devices: []Device{entry("plug", byName("New plug"), map[string]any{"name": "Desk plug"})}}
	originals := map[string]map[string]any{"device:D1": {"name_by_user": nil}}
	ops := Plan(desired, live, originals, entities.RefResolver{})

	if len(ops) != 2 {
		t.Fatalf("ops = %+v", ops)
	}
	if ops[0].Kind != KindUpdate || ops[0].LiveID != "D2" {
		t.Errorf("ops[0] = %+v, want the update of D2", ops[0])
	}
	if ops[1].Kind != KindRestore || ops[1].LiveID != "D1" {
		t.Errorf("ops[1] = %+v, want the restore of D1", ops[1])
	}
}

func TestPlanRestoreNoOpWhenNeverManaged(t *testing.T) {
	live := []map[string]any{liveDevice("D1", "Plug", nil)}
	if ops := Plan(Desired{}, live, nil, entities.RefResolver{}); len(ops) != 0 {
		t.Errorf("ops = %+v, want none", ops)
	}
}

// An error op here would repeat every cycle with no way out but editing
// state.json; a vanished device has nothing left to restore.
func TestPlanManagedDeviceGoneIsForgotten(t *testing.T) {
	originals := map[string]map[string]any{"device:gone": {"name_by_user": nil}}
	ops := Plan(Desired{}, nil, originals, entities.RefResolver{})

	if len(ops) != 1 {
		t.Fatalf("ops = %+v", ops)
	}
	op := ops[0]
	if op.Kind != KindForget || op.RType != "device" || op.Key != "gone" || op.LiveID != "gone" || len(op.Params) != 0 {
		t.Errorf("op = %+v", op)
	}
	if op.DiffText != "stop tracking device gone: it no longer exists in Home Assistant; nothing to restore" {
		t.Errorf("diff = %q", op.DiffText)
	}
}

// A device re-added under a new id: the old id is forgotten, the new one
// managed from scratch.
func TestPlanDeviceReaddedUnderANewIDForgetsTheOldOne(t *testing.T) {
	live := []map[string]any{liveDevice("NEW", "Plug", nil)}
	desired := Desired{Devices: []Device{entry("plug", byName("Plug"), map[string]any{"name": "Desk plug"})}}
	originals := map[string]map[string]any{"device:OLD": {"name_by_user": nil}}
	ops := Plan(desired, live, originals, entities.RefResolver{})

	if len(ops) != 2 {
		t.Fatalf("ops = %+v", ops)
	}
	if ops[0].Kind != KindUpdate || ops[0].LiveID != "NEW" {
		t.Errorf("ops[0] = %+v, want the first management of NEW", ops[0])
	}
	if ops[1].Kind != KindForget || ops[1].LiveID != "OLD" {
		t.Errorf("ops[1] = %+v, want OLD forgotten", ops[1])
	}
}

// The hold protects live values from being undone; a forget undoes nothing.
func TestPlanUnmatchedEntryDoesNotHoldAForget(t *testing.T) {
	desired := Desired{Devices: []Device{entry("plug", byName("Plug"), map[string]any{"name": "Desk plug"})}}
	originals := map[string]map[string]any{"device:gone": {"name_by_user": nil}}
	ops := Plan(desired, nil, originals, entities.RefResolver{})

	if len(ops) != 2 || ops[0].Kind != registries.KindError || ops[1].Kind != KindForget {
		t.Fatalf("ops = %+v, want the no-match error and the forget", ops)
	}
}

func TestPlanRestoreNoDriftStillEmitsOpToDropBookkeeping(t *testing.T) {
	live := []map[string]any{liveDevice("D1", "Plug", nil)}
	originals := map[string]map[string]any{"device:D1": {"name_by_user": nil}}
	ops := Plan(Desired{}, live, originals, entities.RefResolver{})

	if len(ops) != 1 || ops[0].Kind != KindRestore {
		t.Fatalf("ops = %+v", ops)
	}
	if ops[0].DiffText != `restoring original values for device "Plug" (D1); live values already match` {
		t.Errorf("diff = %q", ops[0].DiffText)
	}
}

func TestPlanRestoreSanitizesPoisonedOriginals(t *testing.T) {
	live := []map[string]any{liveDevice("D1", "Plug", map[string]any{"name_by_user": "X"})}
	originals := map[string]map[string]any{"device:D1": {
		"name_by_user": nil, "disabled_by": "integration", "icon": "mdi:x",
	}}
	ops := Plan(Desired{}, live, originals, entities.RefResolver{})

	if len(ops) != 1 || ops[0].Kind != KindRestore {
		t.Fatalf("ops = %+v", ops)
	}
	if !reflect.DeepEqual(ops[0].Params, map[string]any{"name_by_user": nil}) {
		t.Errorf("params = %+v, want only name_by_user", ops[0].Params)
	}
}

func TestPlanIgnoresOriginalsKeysWithoutTheDevicePrefix(t *testing.T) {
	live := []map[string]any{liveDevice("D1", "Plug", nil)}
	originals := map[string]map[string]any{"D1": {"name_by_user": nil}}
	if ops := Plan(Desired{}, live, originals, entities.RefResolver{}); len(ops) != 0 {
		t.Errorf("ops = %+v, want none", ops)
	}
}

// --- Plan(): an unresolved entry holds restores ----------------------------

// An integration renaming its device breaks a name-only match; that must
// not read as "the entry is gone" and undo everything it set.
func TestPlanEntryMatchingNothingHoldsEveryRestore(t *testing.T) {
	live := []map[string]any{liveDevice("D1", "Renamed by firmware", map[string]any{
		"name_by_user": "Desk plug", "disabled_by": "user",
	})}
	desired := Desired{Devices: []Device{entry("plug", byName("Plug"), map[string]any{"name": "Desk plug", "disabled": true})}}
	originals := map[string]map[string]any{"device:D1": {"name_by_user": nil, "disabled_by": nil}}
	ops := Plan(desired, live, originals, entities.RefResolver{})

	if len(ops) != 2 {
		t.Fatalf("ops = %+v", ops)
	}
	for _, op := range ops {
		if op.Kind != registries.KindError {
			t.Errorf("op = %+v, want only errors", op)
		}
	}
	want := `not restoring device "Desk plug" (D1) yet: manifest entry 'plug' matches no device right now`
	if ops[1].Key != "D1" || !strings.HasPrefix(ops[1].Error, want) {
		t.Errorf("held restore = %+v, want error starting %q", ops[1], want)
	}
}

func TestPlanHeldRestoreNamesEveryUnmatchedEntry(t *testing.T) {
	live := []map[string]any{liveDevice("D1", "Plug", map[string]any{"name_by_user": "X"})}
	desired := Desired{Devices: []Device{
		entry("a", byName("Nope A"), map[string]any{"name": "A"}),
		entry("b", byName("Nope B"), map[string]any{"name": "B"}),
	}}
	originals := map[string]map[string]any{"device:D1": {"name_by_user": nil}}
	ops := Plan(desired, live, originals, entities.RefResolver{})

	if len(ops) != 3 || !strings.Contains(ops[2].Error, "manifest entries 'a', 'b' match no device") {
		t.Fatalf("ops = %+v", ops)
	}
}

// An entry with no fields wants its device restored anyway, so its failing
// to match cannot mean "hold".
func TestPlanUnmatchedEntryWithNoFieldsDoesNotHoldRestores(t *testing.T) {
	live := []map[string]any{liveDevice("D1", "Plug", map[string]any{"name_by_user": "X"})}
	desired := Desired{Devices: []Device{entry("gone", byName("Nope"), nil)}}
	originals := map[string]map[string]any{"device:D1": {"name_by_user": nil}}
	ops := Plan(desired, live, originals, entities.RefResolver{})

	if len(ops) != 2 || ops[0].Kind != registries.KindError || ops[1].Kind != KindRestore {
		t.Fatalf("ops = %+v, want the no-match error and the restore", ops)
	}
}

func TestPlanAmbiguousEntryHoldsOnlyItsCandidates(t *testing.T) {
	live := []map[string]any{
		liveDevice("D1", "Hue bulb", map[string]any{"name_by_user": "Desk"}),
		liveDevice("D2", "Hue bulb", nil),
		liveDevice("D3", "Plug", map[string]any{"name_by_user": "Old name"}),
	}
	desired := Desired{Devices: []Device{entry("bulb", byName("Hue bulb"), map[string]any{"name": "Desk"})}}
	originals := map[string]map[string]any{
		"device:D1": {"name_by_user": nil},
		"device:D3": {"name_by_user": nil},
	}
	ops := Plan(desired, live, originals, entities.RefResolver{})

	if len(ops) != 2 {
		t.Fatalf("ops = %+v", ops)
	}
	if ops[0].Kind != registries.KindError || ops[0].Key != "bulb" {
		t.Errorf("ops[0] = %+v, want the ambiguity error", ops[0])
	}
	if ops[1].Kind != KindRestore || ops[1].LiveID != "D3" {
		t.Errorf("ops[1] = %+v, want D3 restored and D1 held", ops[1])
	}
}

func TestPlanDoesNotMutateOriginals(t *testing.T) {
	live := []map[string]any{liveDevice("D1", "Plug", nil)}
	desired := Desired{Devices: []Device{entry("plug", byName("Plug"), map[string]any{"name": "X"})}}
	originals := map[string]map[string]any{"device:D9": {"name_by_user": nil}}
	Plan(desired, live, originals, entities.RefResolver{})

	if !reflect.DeepEqual(originals, map[string]map[string]any{"device:D9": {"name_by_user": nil}}) {
		t.Errorf("originals = %+v, want untouched", originals)
	}
}
