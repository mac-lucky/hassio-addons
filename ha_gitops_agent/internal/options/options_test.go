package options

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// writeOptions writes data as JSON to <dir>/options.json, returning its
// path.
func writeOptions(t *testing.T, dir string, data map[string]any) string {
	t.Helper()
	path := filepath.Join(dir, "options.json")
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("marshal fixture data: %v", err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write fixture file: %v", err)
	}
	return path
}

// fakeOptionsFile writes a full, schema-shaped options.json.
func fakeOptionsFile(t *testing.T, dir string) string {
	t.Helper()
	return writeOptions(t, dir, map[string]any{
		"repo_url":                     "https://example.invalid/demo.git",
		"branch":                       "main",
		"git_username":                 "",
		"git_token":                    "",
		"interval_minutes":             5,
		"dry_run":                      true,
		"apply_after_pull":             "reload",
		"commit_back":                  false,
		"allow_import":                 false,
		"webhook_secret":               "",
		"age_key":                      "",
		"auto_update_addons":           []any{},
		"auto_update_interval_minutes": 360,
		"track_addon_versions":         false,
		"capture_live_changes":         false,
		"reconcile": map[string]any{
			"yaml_files":    true,
			"registries":    false,
			"dashboards":    false,
			"addon_options": false,
			"integrations":  false,
			"subentries":    false,
			"hacs":          false,
		},
	})
}

func TestLoadFullOptionsFile(t *testing.T) {
	dir := t.TempDir()
	path := fakeOptionsFile(t, dir)

	opts, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	want := Options{
		RepoURL:                   "https://example.invalid/demo.git",
		Branch:                    "main",
		GitUsername:               "",
		GitToken:                  "",
		IntervalMinutes:           5,
		DryRun:                    true,
		ApplyAfterPull:            "reload",
		CommitBack:                false,
		AllowImport:               false,
		WebhookSecret:             "",
		AgeKey:                    "",
		AutoUpdateAddons:          nil,
		AutoUpdateIntervalMinutes: 360,
		TrackAddonVersions:        false,
		CaptureLiveChanges:        false,
		ReconcileYAMLFiles:        true,
		ReconcileRegistries:       false,
		ReconcileDashboards:       false,
		ReconcileAddonOptions:     false,
		ReconcileIntegrations:     false,
		ReconcileSubentries:       false,
		ReconcileHacs:             false,
	}
	// DeepEqual rather than ==: Options has a slice field, so it is not
	// comparable.
	if !reflect.DeepEqual(opts, want) {
		t.Fatalf("Load() = %+v, want %+v", opts, want)
	}
}

func TestLoadMinimalOptionsFallsBackToDefaults(t *testing.T) {
	dir := t.TempDir()
	path := writeOptions(t, dir, map[string]any{})

	opts, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	want := Options{
		RepoURL:                   "",
		Branch:                    "main",
		GitUsername:               "",
		GitToken:                  "",
		IntervalMinutes:           5,
		DryRun:                    true,
		ApplyAfterPull:            "reload",
		CommitBack:                false,
		AllowImport:               false,
		WebhookSecret:             "",
		AgeKey:                    "",
		AutoUpdateAddons:          nil,
		AutoUpdateIntervalMinutes: 360,
		TrackAddonVersions:        false,
		CaptureLiveChanges:        false,
		ReconcileYAMLFiles:        true,
		ReconcileRegistries:       false,
		ReconcileDashboards:       false,
		ReconcileAddonOptions:     false,
		ReconcileIntegrations:     false,
		ReconcileSubentries:       false,
		ReconcileHacs:             false,
	}
	if !reflect.DeepEqual(opts, want) {
		t.Fatalf("Load() = %+v, want %+v", opts, want)
	}
}

func TestLoadPartialOptionsMixesSetAndDefaultFields(t *testing.T) {
	dir := t.TempDir()
	path := writeOptions(t, dir, map[string]any{
		"repo_url":         "https://example.invalid/only-this.git",
		"interval_minutes": 15,
		"reconcile": map[string]any{
			"yaml_files": false,
		},
	})

	opts, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if opts.RepoURL != "https://example.invalid/only-this.git" {
		t.Errorf("RepoURL = %q, want %q", opts.RepoURL, "https://example.invalid/only-this.git")
	}
	if opts.IntervalMinutes != 15 {
		t.Errorf("IntervalMinutes = %d, want 15", opts.IntervalMinutes)
	}
	if opts.Branch != "main" {
		t.Errorf("Branch = %q, want %q", opts.Branch, "main")
	}
	if opts.ApplyAfterPull != "reload" {
		t.Errorf("ApplyAfterPull = %q, want %q", opts.ApplyAfterPull, "reload")
	}
	if opts.ReconcileYAMLFiles {
		t.Errorf("ReconcileYAMLFiles = true, want false")
	}
	if opts.ReconcileRegistries {
		t.Errorf("ReconcileRegistries = true, want false")
	}
}

func TestLoadDashboardsReconcileToggle(t *testing.T) {
	dir := t.TempDir()
	path := writeOptions(t, dir, map[string]any{
		"reconcile": map[string]any{"dashboards": true},
	})

	opts, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !opts.ReconcileDashboards {
		t.Errorf("ReconcileDashboards = false, want true")
	}
	// Reconcile fields are read independently, not as a block: the rest
	// keep their defaults when only dashboards is set.
	if !opts.ReconcileYAMLFiles {
		t.Errorf("ReconcileYAMLFiles = false, want true (default)")
	}
	if opts.ReconcileRegistries {
		t.Errorf("ReconcileRegistries = true, want false (default)")
	}
}

func TestLoadSubentriesReconcileToggle(t *testing.T) {
	dir := t.TempDir()
	path := writeOptions(t, dir, map[string]any{
		"reconcile": map[string]any{"subentries": true},
	})

	opts, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !opts.ReconcileSubentries {
		t.Errorf("ReconcileSubentries = false, want true")
	}
	// Independent of the integrations toggle: a subentry's parent entry
	// need not be one this agent manages.
	if opts.ReconcileIntegrations {
		t.Errorf("ReconcileIntegrations = true, want false (default)")
	}
	if !opts.ReconcileYAMLFiles {
		t.Errorf("ReconcileYAMLFiles = false, want true (default)")
	}
}

func TestLoadSubentriesReconcileDefaultsToFalse(t *testing.T) {
	dir := t.TempDir()
	path := writeOptions(t, dir, map[string]any{})

	opts, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if opts.ReconcileSubentries {
		t.Error("ReconcileSubentries = true, want false (default)")
	}
}

func TestLoadHacsReconcileToggle(t *testing.T) {
	dir := t.TempDir()
	path := writeOptions(t, dir, map[string]any{
		"reconcile": map[string]any{"hacs": true},
	})

	opts, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !opts.ReconcileHacs {
		t.Errorf("ReconcileHacs = false, want true")
	}
	// Independent of the integrations toggle: HACS installs the code,
	// integrations.yaml sets an entry up with it, either is useful alone.
	if opts.ReconcileIntegrations {
		t.Errorf("ReconcileIntegrations = true, want false (default)")
	}
	if !opts.ReconcileYAMLFiles {
		t.Errorf("ReconcileYAMLFiles = false, want true (default)")
	}
}

func TestLoadHacsReconcileDefaultsToFalse(t *testing.T) {
	dir := t.TempDir()
	path := writeOptions(t, dir, map[string]any{})

	opts, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if opts.ReconcileHacs {
		t.Error("ReconcileHacs = true, want false (default)")
	}
}

func TestLoadCommitBackToggle(t *testing.T) {
	dir := t.TempDir()
	path := writeOptions(t, dir, map[string]any{"commit_back": true})

	opts, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !opts.CommitBack {
		t.Error("CommitBack = false, want true")
	}
}

func TestLoadCommitBackDefaultsToFalse(t *testing.T) {
	dir := t.TempDir()
	path := writeOptions(t, dir, map[string]any{})

	opts, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if opts.CommitBack {
		t.Error("CommitBack = true, want false (default)")
	}
}

func TestLoadWebhookSecretRoundTrips(t *testing.T) {
	dir := t.TempDir()
	path := writeOptions(t, dir, map[string]any{"webhook_secret": "s3cret-value"})

	opts, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if opts.WebhookSecret != "s3cret-value" {
		t.Errorf("WebhookSecret = %q, want %q", opts.WebhookSecret, "s3cret-value")
	}
}

func TestLoadWebhookSecretDefaultsToEmpty(t *testing.T) {
	dir := t.TempDir()
	path := writeOptions(t, dir, map[string]any{})

	opts, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if opts.WebhookSecret != "" {
		t.Errorf("WebhookSecret = %q, want empty (default)", opts.WebhookSecret)
	}
}

func TestLoadAgeKeyRoundTrips(t *testing.T) {
	// An age identity is checksummed, so any whitespace normalization or
	// case folding here turns a valid key into a startup failure.
	const key = "AGE-SECRET-KEY-1ZVEXNYJ990KE6NVCUYJE0YS5XHLTEY4ALPTT7P3AX3CM56ZDYNXQVEX4EM"
	dir := t.TempDir()
	path := writeOptions(t, dir, map[string]any{"age_key": key})

	opts, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if opts.AgeKey != key {
		t.Errorf("AgeKey = %q, want the configured key verbatim", opts.AgeKey)
	}
}

func TestLoadAgeKeyDefaultsToEmpty(t *testing.T) {
	dir := t.TempDir()
	path := writeOptions(t, dir, map[string]any{})

	opts, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if opts.AgeKey != "" {
		t.Errorf("AgeKey = %q, want empty (default) - no key means encryption stays off", opts.AgeKey)
	}
}

func TestLoadAutoUpdateAddons(t *testing.T) {
	// A missing key and an empty list both land on nil, the one way this
	// package spells "auto-updates are off". The rest is leniency: a
	// hand-edited options.json must never stop the add-on from booting.
	cases := []struct {
		name string
		raw  map[string]any
		want []string
	}{
		{
			name: "missing key",
			raw:  map[string]any{},
			want: nil,
		},
		{
			name: "empty list",
			raw:  map[string]any{"auto_update_addons": []any{}},
			want: nil,
		},
		{
			name: "ordinary list is kept in order",
			raw:  map[string]any{"auto_update_addons": []any{"a0d7b954_esphome", "core_samba"}},
			want: []string{"a0d7b954_esphome", "core_samba"},
		},
		{
			name: "whitespace trimmed, empties and duplicates dropped",
			raw: map[string]any{"auto_update_addons": []any{
				"  core_samba  ", "", "a0d7b954_esphome", "   ", "core_samba", "a0d7b954_esphome",
			}},
			want: []string{"core_samba", "a0d7b954_esphome"},
		},
		{
			name: "non-string elements dropped, strings around them kept",
			raw: map[string]any{"auto_update_addons": []any{
				"core_samba", 5, nil, true, []any{"a0d7b954_esphome"}, map[string]any{"slug": "core_ssh"}, "core_configurator",
			}},
			want: []string{"core_samba", "core_configurator"},
		},
		{
			name: "whole value is a string",
			raw:  map[string]any{"auto_update_addons": "core_samba"},
			want: nil,
		},
		{
			name: "whole value is a number",
			raw:  map[string]any{"auto_update_addons": 7},
			want: nil,
		},
		{
			name: "whole value is an object",
			raw:  map[string]any{"auto_update_addons": map[string]any{"core_samba": true}},
			want: nil,
		},
		{
			name: "whole value is null",
			raw:  map[string]any{"auto_update_addons": nil},
			want: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := writeOptions(t, dir, tc.raw)

			opts, err := Load(path)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if !reflect.DeepEqual(opts.AutoUpdateAddons, tc.want) {
				t.Errorf("AutoUpdateAddons = %#v, want %#v", opts.AutoUpdateAddons, tc.want)
			}
		})
	}
}

func TestLoadTrackAddonVersions(t *testing.T) {
	// Off unless the option says otherwise, and off for anything that
	// cannot be read as a yes: it writes to the user's repository, so the
	// ambiguous cases resolve to "do not".
	cases := []struct {
		name string
		raw  map[string]any
		want bool
	}{
		{name: "missing key", raw: map[string]any{}, want: false},
		{name: "explicit false", raw: map[string]any{"track_addon_versions": false}, want: false},
		{name: "explicit true", raw: map[string]any{"track_addon_versions": true}, want: true},
		{name: "null reads as the default", raw: map[string]any{"track_addon_versions": nil}, want: false},
		{name: "a non-empty string is truthy", raw: map[string]any{"track_addon_versions": "true"}, want: true},
		{name: "an empty string is not", raw: map[string]any{"track_addon_versions": ""}, want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts, err := Load(writeOptions(t, t.TempDir(), tc.raw))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if opts.TrackAddonVersions != tc.want {
				t.Errorf("TrackAddonVersions = %v, want %v", opts.TrackAddonVersions, tc.want)
			}
		})
	}
}

func TestLoadCaptureLiveChanges(t *testing.T) {
	// Off unless the option says otherwise, for TrackAddonVersions' reason
	// and more so: this is the one setting under which an unattended cycle
	// rewrites the tracked branch with config, so every ambiguous value has
	// to resolve to "do not".
	cases := []struct {
		name string
		raw  map[string]any
		want bool
	}{
		{name: "missing key", raw: map[string]any{}, want: false},
		{name: "explicit false", raw: map[string]any{"capture_live_changes": false}, want: false},
		{name: "explicit true", raw: map[string]any{"capture_live_changes": true}, want: true},
		{name: "null reads as the default", raw: map[string]any{"capture_live_changes": nil}, want: false},
		{name: "a non-empty string is truthy", raw: map[string]any{"capture_live_changes": "true"}, want: true},
		{name: "an empty string is not", raw: map[string]any{"capture_live_changes": ""}, want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts, err := Load(writeOptions(t, t.TempDir(), tc.raw))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if opts.CaptureLiveChanges != tc.want {
				t.Errorf("CaptureLiveChanges = %v, want %v", opts.CaptureLiveChanges, tc.want)
			}
		})
	}
}

func TestLoadRejectsBadApplyAfterPull(t *testing.T) {
	dir := t.TempDir()
	path := writeOptions(t, dir, map[string]any{"apply_after_pull": "delete-everything"})

	opts, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if opts.ApplyAfterPull != "reload" {
		t.Errorf("ApplyAfterPull = %q, want %q", opts.ApplyAfterPull, "reload")
	}
}

func TestLoadRejectsOutOfRangeInterval(t *testing.T) {
	cases := []int{0, 99999}
	for _, interval := range cases {
		dir := t.TempDir()
		path := writeOptions(t, dir, map[string]any{"interval_minutes": interval})

		opts, err := Load(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if opts.IntervalMinutes != defaultIntervalMinutes {
			t.Errorf("interval_minutes=%d: IntervalMinutes = %d, want %d", interval, opts.IntervalMinutes, defaultIntervalMinutes)
		}
	}
}

func TestLoadRejectsNonNumericInterval(t *testing.T) {
	dir := t.TempDir()
	path := writeOptions(t, dir, map[string]any{"interval_minutes": "not-a-number"})

	opts, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if opts.IntervalMinutes != defaultIntervalMinutes {
		t.Errorf("IntervalMinutes = %d, want %d", opts.IntervalMinutes, defaultIntervalMinutes)
	}
}

func TestLoadMissingFileRaises(t *testing.T) {
	dir := t.TempDir()
	_, err := Load(filepath.Join(dir, "does-not-exist.json"))
	if err == nil {
		t.Fatal("Load() error = nil, want a not-exist error")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Load() error = %v, want errors.Is(err, os.ErrNotExist)", err)
	}
}

func TestLoadMalformedTopLevelJSONFallsBackToDefaults(t *testing.T) {
	// Defensive about the whole file, not just individual keys: invalid
	// JSON syntax degrades to the documented defaults, not a boot failure.
	dir := t.TempDir()
	path := filepath.Join(dir, "options.json")
	if err := os.WriteFile(path, []byte("not json at all"), 0o600); err != nil {
		t.Fatalf("write fixture file: %v", err)
	}

	opts, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if opts.IntervalMinutes != defaultIntervalMinutes {
		t.Errorf("IntervalMinutes = %d, want %d", opts.IntervalMinutes, defaultIntervalMinutes)
	}
	if opts.ApplyAfterPull != defaultApplyAfterPull {
		t.Errorf("ApplyAfterPull = %q, want %q", opts.ApplyAfterPull, defaultApplyAfterPull)
	}
}

func TestLoadNonObjectTopLevelJSONFallsBackToDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "options.json")
	if err := os.WriteFile(path, []byte("[1, 2, 3]"), 0o600); err != nil {
		t.Fatalf("write fixture file: %v", err)
	}

	opts, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if opts.Branch != "main" {
		t.Errorf("Branch = %q, want %q", opts.Branch, "main")
	}
}

func TestLoadEmptyBranchFallsBackToMain(t *testing.T) {
	dir := t.TempDir()
	path := writeOptions(t, dir, map[string]any{"branch": ""})

	opts, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if opts.Branch != "main" {
		t.Errorf("Branch = %q, want %q", opts.Branch, "main")
	}
}

func TestSupervisorTokenMissingRaises(t *testing.T) {
	t.Setenv("SUPERVISOR_TOKEN", "")

	_, err := SupervisorToken()
	if !errors.Is(err, ErrMissingSupervisorToken) {
		t.Errorf("SupervisorToken() error = %v, want ErrMissingSupervisorToken", err)
	}
}

func TestSupervisorTokenPresent(t *testing.T) {
	t.Setenv("SUPERVISOR_TOKEN", "abc123")

	token, err := SupervisorToken()
	if err != nil {
		t.Fatalf("SupervisorToken: %v", err)
	}
	if token != "abc123" {
		t.Errorf("SupervisorToken() = %q, want %q", token, "abc123")
	}
}

func TestLoadAllowImportToggle(t *testing.T) {
	dir := t.TempDir()
	path := writeOptions(t, dir, map[string]any{"allow_import": true})

	opts, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !opts.AllowImport {
		t.Error("AllowImport = false, want true")
	}
}

func TestLoadAllowImportDefaultsToFalse(t *testing.T) {
	dir := t.TempDir()
	path := writeOptions(t, dir, map[string]any{})

	opts, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if opts.AllowImport {
		t.Error("AllowImport = true, want false (default) - import writes to the tracked branch, so it must be opt-in")
	}
}

func TestLoadAutoUpdateIntervalMinutes(t *testing.T) {
	dir := t.TempDir()
	path := writeOptions(t, dir, map[string]any{"auto_update_interval_minutes": 60})

	opts, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if opts.AutoUpdateIntervalMinutes != 60 {
		t.Errorf("AutoUpdateIntervalMinutes = %d, want 60", opts.AutoUpdateIntervalMinutes)
	}
}

// Out of range falls back rather than erroring: this is the boot path, and
// a bad number must not stop the agent starting.
func TestLoadRejectsOutOfRangeAutoUpdateInterval(t *testing.T) {
	for _, v := range []any{0, 14, 10081, -1} {
		dir := t.TempDir()
		path := writeOptions(t, dir, map[string]any{"auto_update_interval_minutes": v})

		opts, err := Load(path)
		if err != nil {
			t.Fatalf("Load(%v): %v", v, err)
		}
		if opts.AutoUpdateIntervalMinutes != defaultAutoUpdateIntervalMinutes {
			t.Errorf("AutoUpdateIntervalMinutes for %v = %d, want %d",
				v, opts.AutoUpdateIntervalMinutes, defaultAutoUpdateIntervalMinutes)
		}
	}
}

func TestLoadRejectsNonNumericAutoUpdateInterval(t *testing.T) {
	for _, v := range []any{"soon", true, nil} {
		dir := t.TempDir()
		path := writeOptions(t, dir, map[string]any{"auto_update_interval_minutes": v})

		opts, err := Load(path)
		if err != nil {
			t.Fatalf("Load(%v): %v", v, err)
		}
		if opts.AutoUpdateIntervalMinutes != defaultAutoUpdateIntervalMinutes {
			t.Errorf("AutoUpdateIntervalMinutes for %v = %d, want %d",
				v, opts.AutoUpdateIntervalMinutes, defaultAutoUpdateIntervalMinutes)
		}
	}
}
