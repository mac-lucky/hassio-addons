package addonopts

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/registries"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/secretref/secrettest"
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

func TestMissingAddonsFileIsNotAnError(t *testing.T) {
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

func TestEmptyAddonsKeyIsNotAnError(t *testing.T) {
	workdir, gitops := mkGitops(t)
	writeFile(t, gitops, "addons.yaml", "addons:\n")
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
	writeFile(t, gitops, "addons.yaml", `
addons:
  - slug: core_configurator
    options:
      dirsfirst: true
    restart_on_change: false
`)
	desired, err := LoadManifest(workdir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []map[string]any{{
		"slug":              "core_configurator",
		"options":           map[string]any{"dirsfirst": true},
		"restart_on_change": false,
	}}
	if !reflect.DeepEqual(desired.Addons, want) {
		t.Errorf("addons = %+v, want %+v", desired.Addons, want)
	}
}

func TestLoadManifestDefaultsRestartOnChangeToTrue(t *testing.T) {
	workdir, gitops := mkGitops(t)
	writeFile(t, gitops, "addons.yaml", "addons:\n  - slug: core_configurator\n    options:\n      dirsfirst: true\n")
	desired, err := LoadManifest(workdir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if desired.Addons[0]["restart_on_change"] != true {
		t.Errorf("restart_on_change = %v, want true", desired.Addons[0]["restart_on_change"])
	}
}

func TestLoadManifestAcceptsHyphenatedSlug(t *testing.T) {
	workdir, gitops := mkGitops(t)
	writeFile(t, gitops, "addons.yaml", "addons:\n  - slug: a0d7b954_my-community-addon\n    options:\n      x: 1\n")
	desired, err := LoadManifest(workdir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if desired.Addons[0]["slug"] != "a0d7b954_my-community-addon" {
		t.Errorf("slug = %v", desired.Addons[0]["slug"])
	}
}

// --- LoadManifest(): validation ------------------------------------------

func TestLoadManifestInvalidSlug(t *testing.T) {
	cases := []string{
		"addons:\n  - slug: \"\"\n    options: {x: 1}\n",
		"addons:\n  - options: {x: 1}\n",
		"addons:\n  - slug: Has.Dots\n    options: {x: 1}\n",
		"addons:\n  - slug: UPPERCASE\n    options: {x: 1}\n",
	}
	for _, yamlContent := range cases {
		workdir, gitops := mkGitops(t)
		writeFile(t, gitops, "addons.yaml", yamlContent)
		_, err := LoadManifest(workdir)
		if err == nil {
			t.Fatalf("content %q: expected an error", yamlContent)
		}
		if !strings.Contains(err.Error(), "invalid or missing 'slug'") {
			t.Errorf("content %q: error = %q", yamlContent, err.Error())
		}
	}
}

func TestLoadManifestDuplicateSlug(t *testing.T) {
	workdir, gitops := mkGitops(t)
	writeFile(t, gitops, "addons.yaml", `
addons:
  - slug: core_configurator
    options: {x: 1}
  - slug: core_configurator
    options: {y: 2}
`)
	_, err := LoadManifest(workdir)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "duplicate slug 'core_configurator'") {
		t.Errorf("error = %q", err.Error())
	}
}

func TestLoadManifestMissingOptionsIsAnError(t *testing.T) {
	workdir, gitops := mkGitops(t)
	writeFile(t, gitops, "addons.yaml", "addons:\n  - slug: core_configurator\n")
	_, err := LoadManifest(workdir)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "missing or empty 'options'") {
		t.Errorf("error = %q", err.Error())
	}
}

func TestLoadManifestEmptyOptionsIsAnError(t *testing.T) {
	workdir, gitops := mkGitops(t)
	writeFile(t, gitops, "addons.yaml", "addons:\n  - slug: core_configurator\n    options: {}\n")
	_, err := LoadManifest(workdir)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "missing or empty 'options'") {
		t.Errorf("error = %q", err.Error())
	}
}

func TestLoadManifestNonBoolRestartOnChangeIsAnError(t *testing.T) {
	workdir, gitops := mkGitops(t)
	writeFile(t, gitops, "addons.yaml", "addons:\n  - slug: x\n    options: {a: 1}\n    restart_on_change: yes-please\n")
	_, err := LoadManifest(workdir)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "restart_on_change must be a boolean") {
		t.Errorf("error = %q", err.Error())
	}
}

func TestLoadManifestUnsupportedFieldIsAnError(t *testing.T) {
	workdir, gitops := mkGitops(t)
	writeFile(t, gitops, "addons.yaml", "addons:\n  - slug: x\n    options: {a: 1}\n    boot: auto\n")
	_, err := LoadManifest(workdir)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "unsupported field(s) boot") {
		t.Errorf("error = %q", err.Error())
	}
}

// --- DeclaredRestartOnChange ---------------------------------------------

func TestDeclaredRestartOnChange(t *testing.T) {
	desired := Desired{Addons: []map[string]any{
		{"slug": "a", "options": map[string]any{"x": 1}, "restart_on_change": true},
		{"slug": "b", "options": map[string]any{"x": 1}, "restart_on_change": false},
	}}
	got := DeclaredRestartOnChange(desired)
	want := map[string]bool{"a": true, "b": false}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// --- Plan(): ownership rules ----------------------------------------------

func desiredWith(slug string, options map[string]any) Desired {
	return Desired{Addons: []map[string]any{{"slug": slug, "options": options, "restart_on_change": true}}}
}

// installedInfo/notInstalledInfo build one slug's raw live entry the way
// internal/regapply's FetchAddonInfoAll shapes it.
func installedInfo(liveOptions map[string]any) map[string]any {
	return map[string]any{"options": liveOptions, "state": "started"}
}

func notInstalledInfo() map[string]any {
	return map[string]any{"installed": false}
}

func TestPlanNotInstalledIsAnErrorOp(t *testing.T) {
	desired := desiredWith("core_configurator", map[string]any{"dirsfirst": true})
	ops := Plan(desired, map[string]map[string]any{}, nil, "", nil)

	if len(ops) != 1 || ops[0].Kind != KindError {
		t.Fatalf("ops = %+v", ops)
	}
	if !strings.Contains(ops[0].Error, "add-on not installed: core_configurator") {
		t.Errorf("error = %q", ops[0].Error)
	}
}

func TestPlanKnownButNotInstalledIsAnErrorOp(t *testing.T) {
	desired := desiredWith("core_configurator", map[string]any{"dirsfirst": true})
	live := map[string]map[string]any{"core_configurator": notInstalledInfo()}
	ops := Plan(desired, live, nil, "", nil)

	if len(ops) != 1 || ops[0].Kind != KindError {
		t.Fatalf("ops = %+v", ops)
	}
}

func TestPlanSelfProtectionRefusesManagingOwnSlug(t *testing.T) {
	desired := desiredWith("ha_gitops_agent", map[string]any{"dry_run": false})
	live := map[string]map[string]any{"ha_gitops_agent": installedInfo(map[string]any{"dry_run": true})}
	ops := Plan(desired, live, nil, "ha_gitops_agent", nil)

	if len(ops) != 1 || ops[0].Kind != KindError {
		t.Fatalf("ops = %+v", ops)
	}
	if !strings.Contains(ops[0].Error, "self-protection") {
		t.Errorf("error = %q", ops[0].Error)
	}
}

func TestPlanFirstManagementEmitsUpdateEvenWithoutDrift(t *testing.T) {
	desired := desiredWith("core_configurator", map[string]any{"dirsfirst": true})
	live := map[string]map[string]any{"core_configurator": installedInfo(map[string]any{"dirsfirst": true})}
	ops := Plan(desired, live, nil, "", nil)

	if len(ops) != 1 || ops[0].Kind != KindUpdate {
		t.Fatalf("ops = %+v", ops)
	}
	if !reflect.DeepEqual(ops[0].Params, map[string]any{"dirsfirst": true}) {
		t.Errorf("params = %+v", ops[0].Params)
	}
}

func TestPlanNoOpWhenAlreadyManagedAndNoDrift(t *testing.T) {
	desired := desiredWith("core_configurator", map[string]any{"dirsfirst": true})
	live := map[string]map[string]any{"core_configurator": installedInfo(map[string]any{"dirsfirst": true})}
	originals := map[string]map[string]any{"addon:core_configurator": {"dirsfirst": false}}
	ops := Plan(desired, live, originals, "", nil)

	if len(ops) != 0 {
		t.Fatalf("ops = %+v, want none", ops)
	}
}

func TestPlanDriftEmitsUpdate(t *testing.T) {
	desired := desiredWith("core_configurator", map[string]any{"dirsfirst": true})
	live := map[string]map[string]any{"core_configurator": installedInfo(map[string]any{"dirsfirst": false})}
	originals := map[string]map[string]any{"addon:core_configurator": {"dirsfirst": false}}
	ops := Plan(desired, live, originals, "", nil)

	if len(ops) != 1 || ops[0].Kind != KindUpdate {
		t.Fatalf("ops = %+v", ops)
	}
	if !strings.Contains(ops[0].DiffText, "dirsfirst: False -> True") {
		t.Errorf("diff_text = %q", ops[0].DiffText)
	}
}

func TestPlanNewDeclaredKeyOnAlreadyManagedAddonStillEmitsUpdate(t *testing.T) {
	desired := desiredWith("core_configurator", map[string]any{"dirsfirst": true, "hide_dotfiles": true})
	live := map[string]map[string]any{
		"core_configurator": installedInfo(map[string]any{"dirsfirst": true, "hide_dotfiles": true}),
	}
	originals := map[string]map[string]any{"addon:core_configurator": {"dirsfirst": false}}
	ops := Plan(desired, live, originals, "", nil)

	if len(ops) != 1 || ops[0].Kind != KindUpdate {
		t.Fatalf("ops = %+v, want a single update op recording hide_dotfiles' original", ops)
	}
}

func TestPlanNumericTypeCrossingNeverRegistersAsDrift(t *testing.T) {
	desired := desiredWith("core_configurator", map[string]any{"port": 8080})
	live := map[string]map[string]any{"core_configurator": installedInfo(map[string]any{"port": float64(8080)})}
	originals := map[string]map[string]any{"addon:core_configurator": {"port": float64(8080)}}
	ops := Plan(desired, live, originals, "", nil)

	if len(ops) != 0 {
		t.Fatalf("ops = %+v, want none (int vs float64 must not register as drift)", ops)
	}
}

// TestPlanLargeNumericTypeCrossingReadsAsUnchanged is the same crossing at
// a magnitude Go's %v prints in exponent form ("timeout: 1e+06 ->
// 1000000"); difftext.ReprValue renders both sides as 1000000. The
// dirsfirst drift is only there to make the plan emit an op at all.
func TestPlanLargeNumericTypeCrossingReadsAsUnchanged(t *testing.T) {
	desired := desiredWith("core_configurator", map[string]any{"timeout": 1000000, "dirsfirst": true})
	live := map[string]map[string]any{
		"core_configurator": installedInfo(map[string]any{"timeout": float64(1000000), "dirsfirst": false}),
	}
	originals := map[string]map[string]any{"addon:core_configurator": {"dirsfirst": false}}

	ops := Plan(desired, live, originals, "", nil)

	if len(ops) != 1 || ops[0].Kind != KindUpdate {
		t.Fatalf("ops = %+v", ops)
	}
	if !strings.Contains(ops[0].DiffText, "timeout: 1000000 -> 1000000") {
		t.Errorf("diff_text = %q, want timeout unchanged on both sides", ops[0].DiffText)
	}
	if strings.Contains(ops[0].DiffText, "1e+06") {
		t.Errorf("diff_text = %q, want no exponent-form spelling", ops[0].DiffText)
	}
	if !strings.Contains(ops[0].DiffText, "dirsfirst: False -> True") {
		t.Errorf("diff_text = %q, want the real change still reported", ops[0].DiffText)
	}
}

func TestPlanRestoreOnUnmanage(t *testing.T) {
	live := map[string]map[string]any{"core_configurator": installedInfo(map[string]any{"dirsfirst": true})}
	originals := map[string]map[string]any{"addon:core_configurator": {"dirsfirst": false}}
	ops := Plan(Desired{}, live, originals, "", nil)

	if len(ops) != 1 || ops[0].Kind != KindRestore {
		t.Fatalf("ops = %+v", ops)
	}
	if !reflect.DeepEqual(ops[0].Params, map[string]any{"dirsfirst": false}) {
		t.Errorf("params = %+v", ops[0].Params)
	}
	if !strings.Contains(ops[0].DiffText, "dirsfirst: True -> False") {
		t.Errorf("diff_text = %q", ops[0].DiffText)
	}
}

func TestPlanRestoreNotInstalledIsAnErrorOp(t *testing.T) {
	originals := map[string]map[string]any{"addon:core_configurator": {"dirsfirst": false}}
	ops := Plan(Desired{}, map[string]map[string]any{}, originals, "", nil)

	if len(ops) != 1 || ops[0].Kind != KindError {
		t.Fatalf("ops = %+v", ops)
	}
	if !strings.Contains(ops[0].Error, "cannot restore") {
		t.Errorf("error = %q", ops[0].Error)
	}
}

func TestPlanRestoreSelfProtectionRefusesRestoringOwnSlug(t *testing.T) {
	live := map[string]map[string]any{"ha_gitops_agent": installedInfo(map[string]any{"dry_run": true})}
	originals := map[string]map[string]any{"addon:ha_gitops_agent": {"dry_run": false}}
	ops := Plan(Desired{}, live, originals, "ha_gitops_agent", nil)

	if len(ops) != 1 || ops[0].Kind != KindError {
		t.Fatalf("ops = %+v", ops)
	}
	if !strings.Contains(ops[0].Error, "self-protection") {
		t.Errorf("error = %q", ops[0].Error)
	}
}

func TestPlanReDeclaringAfterUnmanageDoesNotAlsoRestore(t *testing.T) {
	desired := desiredWith("core_configurator", map[string]any{"dirsfirst": true})
	live := map[string]map[string]any{"core_configurator": installedInfo(map[string]any{"dirsfirst": true})}
	originals := map[string]map[string]any{"addon:core_configurator": {"dirsfirst": false}}
	ops := Plan(desired, live, originals, "", nil)

	// Still declared - must plan as (a no-op) management, never a restore.
	for _, op := range ops {
		if op.Kind == KindRestore {
			t.Errorf("unexpected restore op while still declared: %+v", op)
		}
	}
}

func TestPlanUsesRegistriesRegOpShape(t *testing.T) {
	// Sanity check that this package genuinely reuses registries.RegOp
	// (Kind constants line up) rather than a lookalike type.
	op := registries.RegOp{Kind: KindUpdate}
	if op.Kind != KindUpdate {
		t.Fatalf("op.Kind = %q", op.Kind)
	}
}

// --- isInstalled/liveOptionsOf: raw live-entry interpretation -----------

func TestIsInstalledTrueForNormalInstalledShape(t *testing.T) {
	// The ordinary installed-add-on info response carries no "installed"
	// key at all - see Plan's own doc comment.
	if !isInstalled(map[string]any{"options": map[string]any{}, "state": "started"}) {
		t.Error("want installed")
	}
}

func TestIsInstalledFalseForExplicitInstalledFalse(t *testing.T) {
	if isInstalled(map[string]any{"installed": false, "options": map[string]any{}, "state": "unknown"}) {
		t.Error("want not installed")
	}
}

func TestIsInstalledFalseForAbsentEntry(t *testing.T) {
	if isInstalled(nil) {
		t.Error("want not installed")
	}
}

func TestLiveOptionsOfAbsentEntryIsNil(t *testing.T) {
	if got := liveOptionsOf(nil); got != nil {
		t.Errorf("got %+v, want nil", got)
	}
}

// --- VM e2e: an original that was ABSENT is not an original that was null

func TestIsAbsentOnlyMatchesTheMarker(t *testing.T) {
	tests := []struct {
		name string
		val  any
		want bool
	}{
		{name: "the marker itself", val: AbsentMarker(), want: true},
		{name: "the marker after a JSON round trip", val: roundTripJSON(t, AbsentMarker()), want: true},
		{name: "nil is a real null, not an absence", val: nil, want: false},
		{name: "an ordinary mapping", val: map[string]any{"a": 1}, want: false},
		{name: "an empty mapping", val: map[string]any{}, want: false},
		{name: "the marker key alongside another", val: map[string]any{absentMarkerKey: true, "a": 1}, want: false},
		{name: "the marker key set to false", val: map[string]any{absentMarkerKey: false}, want: false},
		{name: "the marker key set to a non-bool", val: map[string]any{absentMarkerKey: "yes"}, want: false},
		{name: "a scalar", val: "debug", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsAbsent(tt.val); got != tt.want {
				t.Errorf("IsAbsent(%#v) = %v, want %v", tt.val, got, tt.want)
			}
		})
	}
}

func TestPlanRestoreOfAnAbsentOriginalPlansToUnsetTheKey(t *testing.T) {
	// log_level was optional and NOT set before the manifest declared it,
	// so the restore must read as "back to no value", never "back to
	// None" - Supervisor rejects the second with HTTP 400.
	live := map[string]map[string]any{
		"a0d7b954_chrony": installedInfo(map[string]any{"mode": "server", "log_level": "debug"}),
	}
	originals := map[string]map[string]any{"addon:a0d7b954_chrony": {"log_level": AbsentMarker()}}
	ops := Plan(Desired{}, live, originals, "", nil)

	if len(ops) != 1 || ops[0].Kind != KindRestore {
		t.Fatalf("ops = %+v", ops)
	}
	if !IsAbsent(ops[0].Params["log_level"]) {
		t.Errorf("params = %+v, want log_level carrying the absent marker", ops[0].Params)
	}
	if !strings.Contains(ops[0].DiffText, "log_level: 'debug' -> (unset)") {
		t.Errorf("diff_text = %q, want the restore to read as an unset, not as None", ops[0].DiffText)
	}
}

func TestPlanRestoreOfAnAbsentOriginalAlreadyGoneReportsNoValueChange(t *testing.T) {
	// The key is missing from live again, so the restore has nothing left
	// to ask for; comparing the marker by VALUE would read as drift forever.
	live := map[string]map[string]any{"a0d7b954_chrony": installedInfo(map[string]any{"mode": "server"})}
	originals := map[string]map[string]any{"addon:a0d7b954_chrony": {"log_level": AbsentMarker()}}
	ops := Plan(Desired{}, live, originals, "", nil)

	if len(ops) != 1 || ops[0].Kind != KindRestore {
		t.Fatalf("ops = %+v", ops)
	}
	if !strings.Contains(ops[0].DiffText, "live values already match") {
		t.Errorf("diff_text = %q, want the no-change wording", ops[0].DiffText)
	}
}

func TestPlanRestoreOfAnExplicitNullOriginalStaysANullRestore(t *testing.T) {
	// The other half of the distinction: a recorded null is a value, and
	// keeps planning as one.
	live := map[string]map[string]any{"a0d7b954_chrony": installedInfo(map[string]any{"log_level": "debug"})}
	originals := map[string]map[string]any{"addon:a0d7b954_chrony": {"log_level": nil}}
	ops := Plan(Desired{}, live, originals, "", nil)

	if len(ops) != 1 || ops[0].Kind != KindRestore {
		t.Fatalf("ops = %+v", ops)
	}
	if IsAbsent(ops[0].Params["log_level"]) {
		t.Errorf("params = %+v, want a plain null, not the absent marker", ops[0].Params)
	}
	if !strings.Contains(ops[0].DiffText, "log_level: 'debug' -> None") {
		t.Errorf("diff_text = %q, want the restore to read as None", ops[0].DiffText)
	}
}

func TestAbsentMarkerSurvivesAJSONRoundTrip(t *testing.T) {
	// state.AddonOriginals is plain JSON, where a Go nil stops being
	// distinguishable from a stored null.
	originals := map[string]map[string]any{
		"addon:a0d7b954_chrony": {"log_level": AbsentMarker(), "mode": nil},
	}
	decoded, ok := roundTripJSON(t, originals).(map[string]any)
	if !ok {
		t.Fatalf("decoded = %#v, want a mapping", decoded)
	}
	entry, ok := decoded["addon:a0d7b954_chrony"].(map[string]any)
	if !ok {
		t.Fatalf("entry = %#v, want a mapping", decoded["addon:a0d7b954_chrony"])
	}
	if !IsAbsent(entry["log_level"]) {
		t.Errorf("log_level = %#v, want it still recognizable as absent", entry["log_level"])
	}
	if IsAbsent(entry["mode"]) || entry["mode"] != nil {
		t.Errorf("mode = %#v, want a plain null", entry["mode"])
	}
}

func TestAbsentMarkerIsAFreshMapEveryCall(t *testing.T) {
	first, second := AbsentMarker(), AbsentMarker()
	first["extra"] = true
	if !IsAbsent(second) {
		t.Errorf("second marker = %#v, want it unaffected by mutating the first", second)
	}
}

func roundTripJSON(t *testing.T, v any) any {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return decoded
}

// --- Plan(): secret references --------------------------------------------

func TestPlanResolvesASecretReferenceIntoTheOptionsItSends(t *testing.T) {
	desired := desiredWith("core_mqtt", map[string]any{"password": "secret://mqtt_password"})
	live := map[string]map[string]any{"core_mqtt": installedInfo(map[string]any{"password": "old"})}
	ops := Plan(desired, live, nil, "", secrettest.From(t, "mqtt_password: "+secrettest.Resolved+"\n"))

	if len(ops) != 1 || ops[0].Kind != KindUpdate {
		t.Fatalf("ops = %+v, want one update", ops)
	}
	if !reflect.DeepEqual(ops[0].Params, map[string]any{"password": secrettest.Resolved}) {
		t.Errorf("params = %+v, want the resolved value on the wire", ops[0].Params)
	}
	if !reflect.DeepEqual(ops[0].Secrets, []string{secrettest.Resolved}) {
		t.Errorf("Secrets = %+v, want the resolved value for the applier to redact with", ops[0].Secrets)
	}
}

// Comparison happens against the resolved value, so an add-on already
// holding what the secrets file says is converged - no write, no restart.
func TestPlanComparesTheResolvedValueAgainstLive(t *testing.T) {
	desired := desiredWith("core_mqtt", map[string]any{"password": "secret://mqtt_password"})
	live := map[string]map[string]any{"core_mqtt": installedInfo(map[string]any{"password": secrettest.Resolved})}
	originals := map[string]map[string]any{"addon:core_mqtt": {"password": "before"}}

	ops := Plan(desired, live, originals, "", secrettest.From(t, "mqtt_password: "+secrettest.Resolved+"\n"))
	if len(ops) != 0 {
		t.Fatalf("ops = %+v, want none: live already holds what the reference resolves to", ops)
	}
}

// The one layer whose plan line renders VALUES, so both sides must be
// masked: each would otherwise print the credential itself.
func TestPlanMasksBothSidesOfASecretOptionsDiffLine(t *testing.T) {
	desired := desiredWith("core_mqtt", map[string]any{
		"password": "secret://mqtt_password", "logins": []any{"a"},
	})
	live := map[string]map[string]any{"core_mqtt": installedInfo(map[string]any{
		"password": secrettest.Resolved, "logins": []any{"b"},
	})}
	originals := map[string]map[string]any{"addon:core_mqtt": {"password": "before", "logins": []any{"b"}}}

	ops := Plan(desired, live, originals, "", secrettest.From(t, "mqtt_password: "+secrettest.Resolved+"\n"))
	if len(ops) != 1 {
		t.Fatalf("ops = %+v, want one update for the changed logins", ops)
	}
	diff := ops[0].DiffText
	if strings.Contains(diff, secrettest.Resolved) {
		t.Errorf("DiffText = %q, want no resolved value in it", diff)
	}
	if !strings.Contains(diff, "password: (hidden) -> 'secret://mqtt_password'") {
		t.Errorf("DiffText = %q, want the masked line naming the reference", diff)
	}
	// The keys that are not references still render normally.
	if !strings.Contains(diff, "logins:") {
		t.Errorf("DiffText = %q, want the ordinary key still shown", diff)
	}
}

// A reference nested inside a structured option value is masked the same
// way - reprValue would otherwise print the whole mapping, secret and all.
func TestPlanMasksANestedSecretOptionValue(t *testing.T) {
	desired := desiredWith("core_mqtt", map[string]any{
		"broker": map[string]any{"host": "mqtt.local", "password": "secret://mqtt_password"},
	})
	live := map[string]map[string]any{"core_mqtt": installedInfo(map[string]any{"broker": map[string]any{"host": "old"}})}

	ops := Plan(desired, live, nil, "", secrettest.From(t, "mqtt_password: "+secrettest.Resolved+"\n"))
	if len(ops) != 1 {
		t.Fatalf("ops = %+v, want one update", ops)
	}
	if strings.Contains(ops[0].DiffText, secrettest.Resolved) {
		t.Errorf("DiffText = %q, want no resolved value in it", ops[0].DiffText)
	}
	if !strings.Contains(ops[0].DiffText, "secret://mqtt_password") {
		t.Errorf("DiffText = %q, want the reference itself shown", ops[0].DiffText)
	}
}

func TestPlanEmitsAPerItemErrorOpForAnUnresolvableSecret(t *testing.T) {
	desired := Desired{Addons: []map[string]any{
		{"slug": "core_mqtt", "options": map[string]any{"password": "secret://mqtt_password"}, "restart_on_change": true},
		{"slug": "core_configurator", "options": map[string]any{"dirsfirst": true}, "restart_on_change": true},
	}}
	live := map[string]map[string]any{
		"core_mqtt":         installedInfo(map[string]any{"password": "old"}),
		"core_configurator": installedInfo(map[string]any{"dirsfirst": false}),
	}

	ops := Plan(desired, live, nil, "", secrettest.From(t, "other_key: "+secrettest.Resolved+"\n"))
	if len(ops) != 2 {
		t.Fatalf("ops = %+v, want the broken add-on plus the healthy one", ops)
	}
	if ops[0].Kind != KindError || !strings.Contains(ops[0].Error, "no key 'mqtt_password'") {
		t.Errorf("ops[0] = %+v, want an error op naming the missing key", ops[0])
	}
	// One broken declaration must not take the rest of the manifest with it.
	if ops[1].Kind != KindUpdate || ops[1].Key != "core_configurator" {
		t.Errorf("ops[1] = %+v, want the other add-on still planned", ops[1])
	}
}
