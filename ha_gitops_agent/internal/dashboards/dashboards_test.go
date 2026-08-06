package dashboards

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
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", name, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
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

func TestMissingDashboardsFileIsNotAnError(t *testing.T) {
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

func TestEmptyDashboardsKeyIsNotAnError(t *testing.T) {
	workdir, gitops := mkGitops(t)
	writeFile(t, gitops, "dashboards.yaml", "dashboards:\n")
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
	writeFile(t, gitops, "dashboards.yaml", `
dashboards:
  - id: gitops_home
    title: GitOps Home
    icon: mdi:view-dashboard
    config: dashboards/home.yaml
    show_in_sidebar: true
`)
	writeFile(t, workdir, "dashboards/home.yaml", "title: Home\nviews: []\n")

	desired, err := LoadManifest(workdir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []map[string]any{{
		"id": "gitops_home", "title": "GitOps Home", "icon": "mdi:view-dashboard",
		"config": "dashboards/home.yaml", "show_in_sidebar": true,
	}}
	if !reflect.DeepEqual(desired.Dashboards, want) {
		t.Errorf("dashboards = %+v, want %+v", desired.Dashboards, want)
	}
	content, ok := desired.Content["gitops_home"]
	if !ok || content.Err != "" {
		t.Fatalf("content = %+v", content)
	}
	wantContent := map[string]any{"title": "Home", "views": []any{}}
	if !reflect.DeepEqual(content.Data, wantContent) {
		t.Errorf("content.Data = %+v, want %+v", content.Data, wantContent)
	}
}

func TestLoadManifestOnlyRequiredFieldsIsValid(t *testing.T) {
	workdir, gitops := mkGitops(t)
	writeFile(t, gitops, "dashboards.yaml", "dashboards:\n  - id: home\n    title: Home\n    config: home.yaml\n")
	writeFile(t, workdir, "home.yaml", "views: []\n")

	desired, err := LoadManifest(workdir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []map[string]any{{"id": "home", "title": "Home", "config": "home.yaml"}}
	if !reflect.DeepEqual(desired.Dashboards, want) {
		t.Errorf("dashboards = %+v, want %+v", desired.Dashboards, want)
	}
}

// --- LoadManifest(): validation -------------------------------------------

func TestLoadManifestInvalidID(t *testing.T) {
	cases := []struct {
		name        string
		yamlContent string
		wantErr     string
	}{
		{
			"absent", "dashboards:\n  - title: Missing ID\n    config: x.yaml\n",
			"dashboards[0] has no 'id'",
		},
		{
			"null", "dashboards:\n  - id:\n    title: X\n    config: x.yaml\n",
			"dashboards[0] has no 'id'",
		},
		{
			"empty string", "dashboards:\n  - id: \"\"\n    title: X\n    config: x.yaml\n",
			"has an invalid 'id': must be a non-empty string",
		},
		{
			"not a string", "dashboards:\n  - id: 7\n    title: X\n    config: x.yaml\n",
			"has an invalid 'id': must be a non-empty string",
		},
		{
			"uppercase", "dashboards:\n  - id: NotValid\n    title: X\n    config: x.yaml\n",
			"has an invalid 'id' 'NotValid': a dashboard id is its url_path, and must match [a-z0-9_-]+",
		},
		{
			"dots", "dashboards:\n  - id: not.valid\n    title: X\n    config: x.yaml\n",
			"has an invalid 'id' 'not.valid': a dashboard id is its url_path, and must match [a-z0-9_-]+",
		},
		{
			"space", "dashboards:\n  - id: not valid\n    title: X\n    config: x.yaml\n",
			"has an invalid 'id' 'not valid': a dashboard id is its url_path, and must match [a-z0-9_-]+",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			workdir, gitops := mkGitops(t)
			writeFile(t, gitops, "dashboards.yaml", tc.yamlContent)
			_, err := LoadManifest(workdir)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestLoadManifestReservedIDsAreRejected(t *testing.T) {
	// Reserved ids are matched whole: "lovelace-home" is a different
	// url_path, not a way to spell around "lovelace".
	cases := []struct {
		id           string
		wantReserved bool
	}{
		{"default", true},
		{"lovelace", true},
		{"lovelace-home", false},
		{"default-view", false},
		{"my-lovelace", false},
	}
	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			workdir, gitops := mkGitops(t)
			writeFile(t, gitops, "dashboards.yaml", "dashboards:\n  - id: "+tc.id+"\n    title: X\n    config: x.yaml\n")
			writeFile(t, workdir, "x.yaml", "views: []\n")

			_, err := LoadManifest(workdir)
			if !tc.wantReserved {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), "reserved") {
				t.Errorf("error = %q", err.Error())
			}
		})
	}
}

func TestLoadManifestDuplicateID(t *testing.T) {
	workdir, gitops := mkGitops(t)
	writeFile(t, gitops, "dashboards.yaml", `
dashboards:
  - id: home
    title: A
    config: a.yaml
  - id: home
    title: B
    config: b.yaml
`)
	_, err := LoadManifest(workdir)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "duplicate dashboard id 'home'") {
		t.Errorf("error = %q", err.Error())
	}
}

func TestLoadManifestMissingTitle(t *testing.T) {
	workdir, gitops := mkGitops(t)
	writeFile(t, gitops, "dashboards.yaml", "dashboards:\n  - id: home\n    config: x.yaml\n")
	_, err := LoadManifest(workdir)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "invalid or missing 'title'") {
		t.Errorf("error = %q", err.Error())
	}
}

func TestLoadManifestMissingConfig(t *testing.T) {
	workdir, gitops := mkGitops(t)
	writeFile(t, gitops, "dashboards.yaml", "dashboards:\n  - id: home\n    title: Home\n")
	_, err := LoadManifest(workdir)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "invalid or missing 'config'") {
		t.Errorf("error = %q", err.Error())
	}
}

func TestLoadManifestUnsupportedFieldIsRejected(t *testing.T) {
	workdir, gitops := mkGitops(t)
	writeFile(t, gitops, "dashboards.yaml", "dashboards:\n  - id: home\n    title: Home\n    config: x.yaml\n    require_admin: true\n")
	_, err := LoadManifest(workdir)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "unsupported field(s) require_admin") {
		t.Errorf("error = %q", err.Error())
	}
}

func TestLoadManifestShowInSidebarMustBeBoolean(t *testing.T) {
	workdir, gitops := mkGitops(t)
	writeFile(t, gitops, "dashboards.yaml", "dashboards:\n  - id: home\n    title: Home\n    config: x.yaml\n    show_in_sidebar: yes-please\n")
	_, err := LoadManifest(workdir)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "show_in_sidebar must be a boolean") {
		t.Errorf("error = %q", err.Error())
	}
}

func TestLoadManifestIconMustBeAString(t *testing.T) {
	workdir, gitops := mkGitops(t)
	writeFile(t, gitops, "dashboards.yaml", "dashboards:\n  - id: home\n    title: Home\n    config: x.yaml\n    icon: [1, 2]\n")
	_, err := LoadManifest(workdir)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "icon must be a string") {
		t.Errorf("error = %q", err.Error())
	}
}

// --- LoadManifest(): per-item config file loading --------------------------

func TestLoadManifestMissingConfigFileIsAPerItemProblemNotAManifestError(t *testing.T) {
	workdir, gitops := mkGitops(t)
	writeFile(t, gitops, "dashboards.yaml", "dashboards:\n  - id: home\n    title: Home\n    config: does_not_exist.yaml\n")

	desired, err := LoadManifest(workdir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(desired.Dashboards) != 1 {
		t.Fatalf("dashboards = %+v, want the item still recorded", desired.Dashboards)
	}
	content := desired.Content["home"]
	if content.Err == "" {
		t.Fatalf("content = %+v, want an error", content)
	}
	if content.Data != nil {
		t.Errorf("content.Data = %+v, want nil", content.Data)
	}
}

func TestLoadManifestInvalidYAMLConfigFileIsAPerItemProblem(t *testing.T) {
	workdir, gitops := mkGitops(t)
	writeFile(t, gitops, "dashboards.yaml", "dashboards:\n  - id: home\n    title: Home\n    config: bad.yaml\n")
	writeFile(t, workdir, "bad.yaml", "not: [valid\n")

	desired, err := LoadManifest(workdir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if desired.Content["home"].Err == "" {
		t.Fatalf("content = %+v, want an error", desired.Content["home"])
	}
}

func TestLoadManifestConfigFileNormalizesIntsToFloat64(t *testing.T) {
	workdir, gitops := mkGitops(t)
	writeFile(t, gitops, "dashboards.yaml", "dashboards:\n  - id: home\n    title: Home\n    config: x.yaml\n")
	writeFile(t, workdir, "x.yaml", "count: 5\n")

	desired, err := LoadManifest(workdir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := map[string]any{"count": float64(5)}
	if !reflect.DeepEqual(desired.Content["home"].Data, want) {
		t.Errorf("content.Data = %+v, want %+v", desired.Content["home"].Data, want)
	}
}

func TestLoadManifestEmptyConfigFileIsAnEmptyMapping(t *testing.T) {
	workdir, gitops := mkGitops(t)
	writeFile(t, gitops, "dashboards.yaml", "dashboards:\n  - id: home\n    title: Home\n    config: x.yaml\n")
	writeFile(t, workdir, "x.yaml", "")

	desired, err := LoadManifest(workdir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if desired.Content["home"].Err != "" {
		t.Fatalf("content = %+v", desired.Content["home"])
	}
	if !reflect.DeepEqual(desired.Content["home"].Data, map[string]any{}) {
		t.Errorf("content.Data = %+v, want empty map", desired.Content["home"].Data)
	}
}

func TestLoadManifestConfigFileMustBeAMapping(t *testing.T) {
	workdir, gitops := mkGitops(t)
	writeFile(t, gitops, "dashboards.yaml", "dashboards:\n  - id: home\n    title: Home\n    config: x.yaml\n")
	writeFile(t, workdir, "x.yaml", "- 1\n- 2\n")

	desired, err := LoadManifest(workdir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if desired.Content["home"].Err == "" {
		t.Fatalf("content = %+v, want an error", desired.Content["home"])
	}
}

// --- LoadManifest(): config path containment (B4) -----------------------

func TestLoadManifestConfigPathTraversalIsAPerItemProblemNotAManifestError(t *testing.T) {
	workdir, gitops := mkGitops(t)
	// A sibling of workdir, standing in for anything else reachable - e.g.
	// HA's own secrets.yaml one level up from the checkout.
	writeFile(t, filepath.Dir(workdir), "secrets.yaml", "api_key: super-secret\n")
	writeFile(t, gitops, "dashboards.yaml", "dashboards:\n  - id: home\n    title: Home\n    config: ../secrets.yaml\n")

	desired, err := LoadManifest(workdir)
	if err != nil {
		t.Fatalf("unexpected manifest-wide error: %v, want a per-item problem instead", err)
	}
	if len(desired.Dashboards) != 1 {
		t.Fatalf("dashboards = %+v, want the item still recorded", desired.Dashboards)
	}
	content := desired.Content["home"]
	if content.Err == "" {
		t.Fatal("content.Err = \"\", want the traversal rejected")
	}
	if content.Data != nil {
		t.Errorf("content.Data = %+v, want nil - secrets.yaml must never be read", content.Data)
	}

	ops := Plan(desired, nil, nil, nil)
	if len(ops) != 1 || ops[0].Kind != KindError || ops[0].Key != "home" {
		t.Fatalf("Plan() = %+v, want a single KindError op for 'home'", ops)
	}
}

func TestLoadManifestConfigPathSymlinkEscapingWorkdirIsRejected(t *testing.T) {
	workdir, gitops := mkGitops(t)
	writeFile(t, filepath.Dir(workdir), "secrets.yaml", "api_key: super-secret\n")
	writeFile(t, gitops, "dashboards.yaml", "dashboards:\n  - id: home\n    title: Home\n    config: linked.yaml\n")
	if err := os.Symlink(filepath.Join(filepath.Dir(workdir), "secrets.yaml"), filepath.Join(workdir, "linked.yaml")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	desired, err := LoadManifest(workdir)
	if err != nil {
		t.Fatalf("unexpected manifest-wide error: %v", err)
	}
	content := desired.Content["home"]
	if content.Err == "" {
		t.Fatal("content.Err = \"\", want the symlink escape rejected")
	}
	if content.Data != nil {
		t.Errorf("content.Data = %+v, want nil", content.Data)
	}
}

// --- Plan(): config load failure -------------------------------------------

func TestPlanConfigLoadFailureIsAnErrorOpNeverExecuted(t *testing.T) {
	desired := Desired{
		Dashboards: []map[string]any{{"id": "home", "title": "Home", "config": "x.yaml"}},
		Content:    map[string]DashboardContent{"home": {Err: "could not read config file 'x.yaml'"}},
	}
	ops := Plan(desired, nil, nil, nil)

	if len(ops) != 1 || ops[0].Kind != registries.KindError || ops[0].RType != "dashboard" || ops[0].Key != "home" {
		t.Fatalf("ops = %+v", ops)
	}
	if !strings.Contains(ops[0].Error, "could not read config file") {
		t.Errorf("error = %q", ops[0].Error)
	}
}

// --- Plan(): create -----------------------------------------------------

func desiredWith(id, title, configPath string, extra map[string]any) Desired {
	item := map[string]any{"id": id, "title": title, "config": configPath}
	for k, v := range extra {
		item[k] = v
	}
	return Desired{
		Dashboards: []map[string]any{item},
		Content:    map[string]DashboardContent{id: {Data: map[string]any{"views": []any{}}}},
	}
}

func TestPlanNoLiveMatchIsACreate(t *testing.T) {
	desired := desiredWith("home", "Home", "x.yaml", nil)
	ops := Plan(desired, nil, nil, nil)

	if len(ops) != 1 || ops[0].Kind != registries.KindCreate || ops[0].RType != "dashboard" || ops[0].Key != "home" {
		t.Fatalf("ops = %+v", ops)
	}
	metadata, _ := ops[0].Params["metadata"].(map[string]any)
	want := map[string]any{"title": "Home", "show_in_sidebar": true}
	if !reflect.DeepEqual(metadata, want) {
		t.Errorf("metadata = %+v, want %+v", metadata, want)
	}
	content, _ := ops[0].Params["content"].(map[string]any)
	if !reflect.DeepEqual(content, map[string]any{"views": []any{}}) {
		t.Errorf("content = %+v", content)
	}
}

func TestPlanCreateIncludesIconWhenDeclared(t *testing.T) {
	desired := desiredWith("home", "Home", "x.yaml", map[string]any{"icon": "mdi:home", "show_in_sidebar": false})
	ops := Plan(desired, nil, nil, nil)

	metadata, _ := ops[0].Params["metadata"].(map[string]any)
	want := map[string]any{"title": "Home", "show_in_sidebar": false, "icon": "mdi:home"}
	if !reflect.DeepEqual(metadata, want) {
		t.Errorf("metadata = %+v, want %+v", metadata, want)
	}
}

func TestPlanManagedButLiveGoneRecreates(t *testing.T) {
	desired := desiredWith("home", "Home", "x.yaml", nil)
	managed := map[string]string{"dashboard:home": "abc123"}
	ops := Plan(desired, nil, nil, managed)

	if len(ops) != 1 || ops[0].Kind != registries.KindCreate {
		t.Fatalf("ops = %+v", ops)
	}
}

// --- Plan(): adopt --------------------------------------------------------

func TestPlanAdoptsByExactURLPathMatch(t *testing.T) {
	desired := desiredWith("home", "Home", "x.yaml", nil)
	live := []map[string]any{{"id": "home", "url_path": "home", "title": "Home", "show_in_sidebar": true}}
	liveContent := map[string]map[string]any{"home": {"views": []any{}}}

	ops := Plan(desired, live, liveContent, nil)

	if len(ops) != 1 || ops[0].Kind != registries.KindUpdate {
		t.Fatalf("ops = %+v", ops)
	}
	if ops[0].LiveID != "home" {
		t.Errorf("live_id = %q, want home", ops[0].LiveID)
	}
	if !strings.Contains(ops[0].DiffText, "adopted existing dashboard") {
		t.Errorf("diff = %q", ops[0].DiffText)
	}
}

func TestPlanAdoptWithMetadataDriftIncludesIt(t *testing.T) {
	desired := desiredWith("home", "New Title", "x.yaml", nil)
	live := []map[string]any{{"id": "home", "url_path": "home", "title": "Old Title", "show_in_sidebar": true}}
	liveContent := map[string]map[string]any{"home": {"views": []any{}}}

	ops := Plan(desired, live, liveContent, nil)

	if len(ops) != 1 || ops[0].Kind != registries.KindUpdate {
		t.Fatalf("ops = %+v", ops)
	}
	if !strings.Contains(ops[0].DiffText, "Old Title") || !strings.Contains(ops[0].DiffText, "New Title") {
		t.Errorf("diff = %q", ops[0].DiffText)
	}
}

func TestPlanNeverAmbiguousMatchesUniqueURLPath(t *testing.T) {
	// Home Assistant enforces a unique url_path, so only the live dashboard
	// whose url_path matches the manifest id is ever considered.
	desired := desiredWith("home", "Home", "x.yaml", nil)
	live := []map[string]any{
		{"id": "other", "url_path": "other", "title": "Other"},
		{"id": "home", "url_path": "home", "title": "Home", "show_in_sidebar": true},
	}
	liveContent := map[string]map[string]any{"home": {"views": []any{}}}

	ops := Plan(desired, live, liveContent, nil)

	if len(ops) != 1 || ops[0].LiveID != "home" {
		t.Fatalf("ops = %+v", ops)
	}
}

// --- Plan(): already managed, drift / no-drift ------------------------------

func TestPlanAlreadyManagedNoDriftEmitsNoOp(t *testing.T) {
	desired := desiredWith("home", "Home", "x.yaml", nil)
	managed := map[string]string{"dashboard:home": "abc123"}
	live := []map[string]any{{"id": "abc123", "url_path": "home", "title": "Home", "show_in_sidebar": true}}
	liveContent := map[string]map[string]any{"home": {"views": []any{}}}

	ops := Plan(desired, live, liveContent, managed)

	if len(ops) != 0 {
		t.Errorf("ops = %+v, want none", ops)
	}
}

func TestPlanAlreadyManagedMetadataDriftOnlyOmitsContentParam(t *testing.T) {
	desired := desiredWith("home", "New Title", "x.yaml", nil)
	managed := map[string]string{"dashboard:home": "abc123"}
	live := []map[string]any{{"id": "abc123", "url_path": "home", "title": "Old Title", "show_in_sidebar": true}}
	liveContent := map[string]map[string]any{"home": {"views": []any{}}}

	ops := Plan(desired, live, liveContent, managed)

	if len(ops) != 1 || ops[0].Kind != registries.KindUpdate {
		t.Fatalf("ops = %+v", ops)
	}
	if _, hasContent := ops[0].Params["content"]; hasContent {
		t.Errorf("params = %+v, want no content key (content did not drift)", ops[0].Params)
	}
	if _, hasMetadata := ops[0].Params["metadata"]; !hasMetadata {
		t.Errorf("params = %+v, want a metadata key", ops[0].Params)
	}
}

func TestPlanAlreadyManagedContentDriftOnlyOmitsMetadataParam(t *testing.T) {
	desired := desiredWith("home", "Home", "x.yaml", nil)
	desired.Content["home"] = DashboardContent{Data: map[string]any{"views": []any{"new"}}}
	managed := map[string]string{"dashboard:home": "abc123"}
	live := []map[string]any{{"id": "abc123", "url_path": "home", "title": "Home", "show_in_sidebar": true}}
	liveContent := map[string]map[string]any{"home": {"views": []any{}}}

	ops := Plan(desired, live, liveContent, managed)

	if len(ops) != 1 || ops[0].Kind != registries.KindUpdate {
		t.Fatalf("ops = %+v", ops)
	}
	if _, hasMetadata := ops[0].Params["metadata"]; hasMetadata {
		t.Errorf("params = %+v, want no metadata key (metadata did not drift)", ops[0].Params)
	}
	content, _ := ops[0].Params["content"].(map[string]any)
	if !reflect.DeepEqual(content, map[string]any{"views": []any{"new"}}) {
		t.Errorf("content = %+v", content)
	}
}

func TestPlanAlreadyManagedContentNeverSavedIsDrift(t *testing.T) {
	desired := desiredWith("home", "Home", "x.yaml", nil)
	managed := map[string]string{"dashboard:home": "abc123"}
	live := []map[string]any{{"id": "abc123", "url_path": "home", "title": "Home", "show_in_sidebar": true}}
	// liveContent has no entry for "home" at all - config_not_found.

	ops := Plan(desired, live, nil, managed)

	if len(ops) != 1 || ops[0].Kind != registries.KindUpdate {
		t.Fatalf("ops = %+v", ops)
	}
	if _, hasContent := ops[0].Params["content"]; !hasContent {
		t.Errorf("params = %+v, want a content key", ops[0].Params)
	}
}

// --- Plan(): delete-only-managed --------------------------------------------

func TestPlanDeletesManagedDashboardRemovedFromManifest(t *testing.T) {
	managed := map[string]string{"dashboard:home": "abc123"}
	live := []map[string]any{{"id": "abc123", "url_path": "home", "title": "Home"}}

	ops := Plan(Desired{}, live, nil, managed)

	if len(ops) != 1 || ops[0].Kind != registries.KindDelete || ops[0].Key != "home" || ops[0].LiveID != "abc123" {
		t.Fatalf("ops = %+v", ops)
	}
}

func TestPlanDeleteNoOpWhenLiveAlreadyGone(t *testing.T) {
	managed := map[string]string{"dashboard:home": "abc123"}
	ops := Plan(Desired{}, nil, nil, managed)

	if len(ops) != 0 {
		t.Errorf("ops = %+v, want none", ops)
	}
}

func TestPlanNeverTouchesUnmanagedLiveDashboards(t *testing.T) {
	live := []map[string]any{{"id": "abc123", "url_path": "someone_elses", "title": "Not managed"}}
	ops := Plan(Desired{}, live, nil, nil)

	if len(ops) != 0 {
		t.Errorf("ops = %+v, want none", ops)
	}
}

// --- VM e2e: a dashboard made in the HA UI is always hyphenated ------------

func TestHyphenatedIDAdoptsAUICreatedDashboard(t *testing.T) {
	// HA's own UI cannot create a hyphenless url_path, so every hand-made
	// dashboard had a url_path this manifest refused to load, putting the
	// adopt half of create/adopt/update/delete out of reach.
	workdir, gitops := mkGitops(t)
	writeFile(t, gitops, "dashboards.yaml",
		"dashboards:\n  - id: gitops-e2e-adopt\n    title: Adopted\n    config: gitops/adopt.yaml\n")
	writeFile(t, gitops, "adopt.yaml", "views: []\n")

	desired, err := LoadManifest(workdir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(desired.Dashboards) != 1 || desired.Dashboards[0]["id"] != "gitops-e2e-adopt" {
		t.Fatalf("dashboards = %+v", desired.Dashboards)
	}

	live := []map[string]any{
		{"id": "8f2c1a", "url_path": "gitops-e2e-adopt", "title": "Adopted", "show_in_sidebar": true},
	}
	liveContent := map[string]map[string]any{"gitops-e2e-adopt": {"views": []any{}}}

	ops := Plan(desired, live, liveContent, nil)

	if len(ops) != 1 {
		t.Fatalf("ops = %+v, want exactly one", ops)
	}
	if ops[0].Kind != registries.KindUpdate || ops[0].Key != "gitops-e2e-adopt" || ops[0].LiveID != "8f2c1a" {
		t.Fatalf("op = %+v", ops[0])
	}
	if !strings.Contains(ops[0].DiffText, "adopted existing dashboard") {
		t.Errorf("diff = %q", ops[0].DiffText)
	}
}

func TestHyphenatedIDWithNoLiveMatchIsASingleCreate(t *testing.T) {
	workdir, gitops := mkGitops(t)
	writeFile(t, gitops, "dashboards.yaml",
		"dashboards:\n  - id: gitops-e2e\n    title: E2E\n    config: gitops/e2e.yaml\n")
	writeFile(t, gitops, "e2e.yaml", "views: []\n")

	desired, err := LoadManifest(workdir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	ops := Plan(desired, nil, nil, nil)

	if len(ops) != 1 {
		t.Fatalf("ops = %+v, want exactly one", ops)
	}
	// Key is what internal/regapply sends as url_path, so it must carry the
	// hyphen through untouched.
	if ops[0].Kind != registries.KindCreate || ops[0].Key != "gitops-e2e" {
		t.Fatalf("op = %+v", ops[0])
	}
	content, _ := ops[0].Params["content"].(map[string]any)
	if !reflect.DeepEqual(content, map[string]any{"views": []any{}}) {
		t.Errorf("content = %+v", content)
	}
}

// --- LoadManifest() + Plan(): a missing manifest is not "layer off" ---------

func TestMissingOrEmptiedManifestStillDeletesManagedDashboards(t *testing.T) {
	// DOCS.md promises this: a missing dashboards.yaml leaves the layer
	// idle while nothing is managed, but once a dashboard is managed,
	// deleting or emptying the file reads as "delete what I manage".
	cases := []struct {
		name        string
		yamlContent string // "" means write no dashboards.yaml at all
	}{
		{"file deleted", ""},
		{"dashboards key emptied", "dashboards:\n"},
		{"empty list", "dashboards: []\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			workdir, gitops := mkGitops(t)
			if tc.yamlContent != "" {
				writeFile(t, gitops, "dashboards.yaml", tc.yamlContent)
			}

			desired, err := LoadManifest(workdir)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(desired.Dashboards) != 0 {
				t.Fatalf("dashboards = %+v, want none declared", desired.Dashboards)
			}

			managed := map[string]string{"dashboard:gitops-home": "abc123"}
			live := []map[string]any{{"id": "abc123", "url_path": "gitops-home", "title": "Home"}}

			ops := Plan(desired, live, nil, managed)

			if len(ops) != 1 {
				t.Fatalf("ops = %+v, want exactly one", ops)
			}
			if ops[0].Kind != registries.KindDelete || ops[0].Key != "gitops-home" || ops[0].LiveID != "abc123" {
				t.Fatalf("op = %+v", ops[0])
			}
		})
	}
}

// --- Plan(): managed map is untouched by declared dashboards.yaml alone -----

func TestPlanRegistriesRTypeDoesNotCollideWithRegistriesPlan(t *testing.T) {
	// A "dashboard:" managed key must never be mistaken for a
	// registries.Plan helper-domain key; Plan must never emit an op for
	// any rtype other than "dashboard".
	desired := desiredWith("home", "Home", "x.yaml", nil)
	ops := Plan(desired, nil, nil, nil)
	for _, op := range ops {
		if op.RType != "dashboard" {
			t.Errorf("op = %+v, want rtype dashboard", op)
		}
	}
}
