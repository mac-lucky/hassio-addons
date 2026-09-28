package dashboards

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// --- LoadManifest(): resources -------------------------------------------

func TestLoadManifestParsesResources(t *testing.T) {
	workdir, gitops := mkGitops(t)
	writeFile(t, gitops, "dashboards.yaml", `
resources:
  - id: bubble_card
    url: /hacsfiles/Bubble-Card/bubble-card.js
    type: module
  - id: cdn-font
    url: https://fonts.example.com/font.css?v=2
    type: css
`)

	desired, err := LoadManifest(workdir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []Resource{
		{ID: "bubble_card", URL: "/hacsfiles/Bubble-Card/bubble-card.js", Type: "module"},
		{ID: "cdn-font", URL: "https://fonts.example.com/font.css?v=2", Type: "css"},
	}
	if !reflect.DeepEqual(desired.Resources, want) {
		t.Errorf("resources = %+v, want %+v", desired.Resources, want)
	}
	if len(desired.Dashboards) != 0 {
		t.Errorf("dashboards = %+v, want none - a resources-only manifest is valid", desired.Dashboards)
	}
}

func TestLoadManifestParsesResourcesNextToDashboards(t *testing.T) {
	workdir, gitops := mkGitops(t)
	writeFile(t, gitops, "dashboards.yaml", `
dashboards:
  - id: home
    title: Home
    config: home.yaml
resources:
  - id: card
    url: /local/card.js
    type: js
`)
	writeFile(t, workdir, "home.yaml", "views: []\n")

	desired, err := LoadManifest(workdir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(desired.Dashboards) != 1 || desired.Content["home"].Err != "" {
		t.Errorf("dashboards = %+v, content = %+v", desired.Dashboards, desired.Content)
	}
	if !reflect.DeepEqual(desired.Resources, []Resource{{ID: "card", URL: "/local/card.js", Type: "js"}}) {
		t.Errorf("resources = %+v", desired.Resources)
	}
}

func TestLoadManifestEmptyResourcesKeyIsNotAnError(t *testing.T) {
	workdir, gitops := mkGitops(t)
	writeFile(t, gitops, "dashboards.yaml", "resources:\n")
	got, err := LoadManifest(workdir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !reflect.DeepEqual(got, emptyDesired()) {
		t.Errorf("got %+v, want empty Desired", got)
	}
}

func TestLoadManifestResourceValidation(t *testing.T) {
	cases := []struct {
		name        string
		yamlContent string
		wantErr     string
	}{
		{"not a list", "resources: {a: b}\n", "resources must be a list"},
		{"item not a mapping", "resources:\n  - just-a-string\n", "resources[0] is not a mapping"},
		{"no id", "resources:\n  - url: /local/a.js\n    type: js\n", "resources[0] has no 'id'"},
		{"id not a string", "resources:\n  - id: 5\n    url: /local/a.js\n    type: js\n", "resources[0] has an invalid 'id': must be a non-empty string"},
		{"id bad pattern", "resources:\n  - id: Bad Id\n    url: /local/a.js\n    type: js\n", "resources[0] has an invalid 'id' 'Bad Id'"},
		{
			"duplicate id",
			"resources:\n  - id: a\n    url: /local/a.js\n    type: js\n  - id: a\n    url: /local/b.js\n    type: js\n",
			"duplicate resource id 'a'",
		},
		{"unsupported field", "resources:\n  - id: a\n    url: /local/a.js\n    type: js\n    zzz: 1\n    aaa: 2\n", "resource 'a' has unsupported field(s) aaa, zzz"},
		{"url missing", "resources:\n  - id: a\n    type: js\n", "resource 'a' has an invalid or missing 'url'"},
		{"url empty", "resources:\n  - id: a\n    url: ''\n    type: js\n", "resource 'a' has an invalid or missing 'url'"},
		{"url not a string", "resources:\n  - id: a\n    url: [x]\n    type: js\n", "resource 'a' has an invalid or missing 'url'"},
		{"url relative", "resources:\n  - id: a\n    url: local/a.js\n    type: js\n", "resource 'a' url must start with '/', 'http://' or 'https://'"},
		{"url other scheme", "resources:\n  - id: a\n    url: ftp://host/a.js\n    type: js\n", "resource 'a' url must start with"},
		{"url scheme-relative", "resources:\n  - id: a\n    url: //evil.example/a.js\n    type: module\n", "resource 'a' url must not start with '//'"},
		{"url backslash", "resources:\n  - id: a\n    url: '/\\evil.example/a.js'\n    type: module\n", "resource 'a' url must not start with '//' or contain"},
		{"url with a space", "resources:\n  - id: a\n    url: /local/a b.js\n    type: js\n", "resource 'a' url must not contain whitespace"},
		{"url with a newline", "resources:\n  - id: a\n    url: \"/local/a.js\\n\"\n    type: js\n", "resource 'a' url must not contain whitespace"},
		{"type missing", "resources:\n  - id: a\n    url: /local/a.js\n", "resource 'a' has an invalid or missing 'type': must be one of js, css, module, html"},
		{"type unknown", "resources:\n  - id: a\n    url: /local/a.js\n    type: javascript\n", "resource 'a' has an unknown type 'javascript': must be one of js, css, module, html"},
		{
			"same path, different query",
			"resources:\n  - id: a\n    url: /hacsfiles/x/x.js?hacstag=1\n    type: module\n  - id: b\n    url: /hacsfiles/x/x.js\n    type: module\n",
			"resources 'a' and 'b' have the same URL path '/hacsfiles/x/x.js'",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			workdir, gitops := mkGitops(t)
			writeFile(t, gitops, "dashboards.yaml", tc.yamlContent)
			_, err := LoadManifest(workdir)
			var mErr *ManifestError
			if !errors.As(err, &mErr) {
				t.Fatalf("err = %v, want a *ManifestError", err)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("err = %q, want it to contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// Problems in both lists come back together, so one bad resource never
// hides a bad dashboard.
func TestLoadManifestAggregatesDashboardAndResourceProblems(t *testing.T) {
	workdir, gitops := mkGitops(t)
	writeFile(t, gitops, "dashboards.yaml", "dashboards: nope\nresources:\n  - id: a\n    url: /local/a.js\n")

	_, err := LoadManifest(workdir)
	var mErr *ManifestError
	if !errors.As(err, &mErr) {
		t.Fatalf("err = %v, want a *ManifestError", err)
	}
	if len(mErr.Problems) != 2 ||
		!strings.Contains(mErr.Problems[0], "dashboards must be a list") ||
		!strings.Contains(mErr.Problems[1], "resource 'a' has an invalid or missing 'type'") {
		t.Errorf("problems = %+v", mErr.Problems)
	}
}

// --- PlanResources() ---------------------------------------------------------

func resourcesDesired(resources ...Resource) Desired {
	return Desired{Resources: resources}
}

func TestPlanResourcesNoLiveMatchIsACreate(t *testing.T) {
	desired := resourcesDesired(Resource{ID: "card", URL: "/local/card.js", Type: "module"})
	live := []map[string]any{{"id": "other", "url": "/local/other.js", "type": "module"}}

	ops := PlanResources(desired, live, "storage", nil)

	if len(ops) != 1 || ops[0].Kind != KindCreate || ops[0].RType != "resource" || ops[0].Key != "card" {
		t.Fatalf("ops = %+v", ops)
	}
	if want := map[string]any{"url": "/local/card.js", "type": "module"}; !reflect.DeepEqual(ops[0].Params, want) {
		t.Errorf("params = %+v, want %+v", ops[0].Params, want)
	}
	if !strings.Contains(ops[0].DiffText, "+url: '/local/card.js'") || !strings.Contains(ops[0].DiffText, "+type: 'module'") {
		t.Errorf("diff = %q", ops[0].DiffText)
	}
}

// The declared url carries no query, so HACS's hacstag on the live side is
// not drift: the adopt sends nothing and records the mapping.
func TestPlanResourcesAdoptsByPathIgnoringLiveQuery(t *testing.T) {
	desired := resourcesDesired(Resource{ID: "bubble_card", URL: "/hacsfiles/Bubble-Card/bubble-card.js", Type: "module"})
	live := []map[string]any{
		{"id": "abc", "url": "/hacsfiles/Bubble-Card/bubble-card.js?hacstag=680112919341", "type": "module"},
		{"id": "def", "url": "/hacsfiles/other/other.js?hacstag=1", "type": "module"},
	}

	ops := PlanResources(desired, live, "storage", map[string]string{})

	if len(ops) != 1 || ops[0].Kind != KindUpdate || ops[0].Key != "bubble_card" || ops[0].LiveID != "abc" {
		t.Fatalf("ops = %+v", ops)
	}
	if len(ops[0].Params) != 0 {
		t.Errorf("params = %+v, want none - nothing drifted", ops[0].Params)
	}
	if !strings.Contains(ops[0].DiffText, "adopted existing resource 'bubble_card' (live id abc)") {
		t.Errorf("diff = %q", ops[0].DiffText)
	}
}

// A declared query pins the whole url: adoption is still by path, but the
// differing query is drift, and only the url is sent.
func TestPlanResourcesAdoptWithDeclaredQuerySendsTheURL(t *testing.T) {
	desired := resourcesDesired(Resource{ID: "card", URL: "/local/card.js?v=2", Type: "module"})
	live := []map[string]any{{"id": "abc", "url": "/local/card.js?v=1", "type": "module"}}

	ops := PlanResources(desired, live, "storage", nil)

	if len(ops) != 1 || ops[0].Kind != KindUpdate || ops[0].LiveID != "abc" {
		t.Fatalf("ops = %+v", ops)
	}
	if want := map[string]any{"url": "/local/card.js?v=2"}; !reflect.DeepEqual(ops[0].Params, want) {
		t.Errorf("params = %+v, want %+v", ops[0].Params, want)
	}
}

func TestPlanResourcesManagedHacstagBumpIsNotDrift(t *testing.T) {
	desired := resourcesDesired(Resource{ID: "bubble_card", URL: "/hacsfiles/Bubble-Card/bubble-card.js", Type: "module"})
	live := []map[string]any{{"id": "abc", "url": "/hacsfiles/Bubble-Card/bubble-card.js?hacstag=999", "type": "module"}}
	managed := map[string]string{"resource:bubble_card": "abc"}

	if ops := PlanResources(desired, live, "storage", managed); len(ops) != 0 {
		t.Errorf("ops = %+v, want none", ops)
	}
}

func TestPlanResourcesManagedDeclaredQueryIsComparedInFull(t *testing.T) {
	desired := resourcesDesired(Resource{ID: "card", URL: "/local/card.js?v=2", Type: "module"})
	managed := map[string]string{"resource:card": "abc"}

	same := []map[string]any{{"id": "abc", "url": "/local/card.js?v=2", "type": "module"}}
	if ops := PlanResources(desired, same, "storage", managed); len(ops) != 0 {
		t.Errorf("identical url: ops = %+v, want none", ops)
	}

	differs := []map[string]any{{"id": "abc", "url": "/local/card.js?v=1", "type": "module"}}
	ops := PlanResources(desired, differs, "storage", managed)
	if len(ops) != 1 || ops[0].Kind != KindUpdate || !reflect.DeepEqual(ops[0].Params, map[string]any{"url": "/local/card.js?v=2"}) {
		t.Fatalf("ops = %+v", ops)
	}
	if !strings.Contains(ops[0].DiffText, "-url: '/local/card.js?v=1'") || !strings.Contains(ops[0].DiffText, "+url: '/local/card.js?v=2'") {
		t.Errorf("diff = %q", ops[0].DiffText)
	}
}

func TestPlanResourcesManagedPathChangeIsDrift(t *testing.T) {
	desired := resourcesDesired(Resource{ID: "card", URL: "/local/card-v2.js", Type: "module"})
	live := []map[string]any{{"id": "abc", "url": "/local/card.js", "type": "module"}}

	ops := PlanResources(desired, live, "storage", map[string]string{"resource:card": "abc"})

	if len(ops) != 1 || !reflect.DeepEqual(ops[0].Params, map[string]any{"url": "/local/card-v2.js"}) {
		t.Fatalf("ops = %+v", ops)
	}
}

// Only the type is sent, and the diff keeps the live url (query included)
// on both sides, since it is not being changed.
func TestPlanResourcesTypeDriftSendsOnlyTheType(t *testing.T) {
	desired := resourcesDesired(Resource{ID: "card", URL: "/hacsfiles/card/card.js", Type: "module"})
	live := []map[string]any{{"id": "abc", "url": "/hacsfiles/card/card.js?hacstag=5", "type": "js"}}

	ops := PlanResources(desired, live, "storage", map[string]string{"resource:card": "abc"})

	if len(ops) != 1 || ops[0].Kind != KindUpdate || !reflect.DeepEqual(ops[0].Params, map[string]any{"type": "module"}) {
		t.Fatalf("ops = %+v", ops)
	}
	if !strings.Contains(ops[0].DiffText, "-type: 'js'") || !strings.Contains(ops[0].DiffText, "+type: 'module'") {
		t.Errorf("diff = %q", ops[0].DiffText)
	}
	if strings.Contains(ops[0].DiffText, "-url") || strings.Contains(ops[0].DiffText, "+url") {
		t.Errorf("diff = %q, want the url shown only as context", ops[0].DiffText)
	}
}

func TestPlanResourcesAmbiguousAdoptIsAnError(t *testing.T) {
	desired := resourcesDesired(Resource{ID: "card", URL: "/local/card.js", Type: "module"})
	live := []map[string]any{
		{"id": "abc", "url": "/local/card.js?v=1", "type": "module"},
		{"id": "def", "url": "/local/card.js", "type": "module"},
	}

	ops := PlanResources(desired, live, "storage", nil)

	if len(ops) != 1 || ops[0].Kind != KindError || ops[0].RType != "resource" || ops[0].Key != "card" {
		t.Fatalf("ops = %+v", ops)
	}
	if want := "ambiguous adopt: 2 live resources have the URL path '/local/card.js'"; ops[0].Error != want {
		t.Errorf("error = %q, want %q", ops[0].Error, want)
	}
}

func TestPlanResourcesDeletesManagedResourceRemovedFromManifest(t *testing.T) {
	live := []map[string]any{{"id": "abc", "url": "/local/card.js", "type": "module"}}

	ops := PlanResources(Desired{}, live, "storage", map[string]string{"resource:card": "abc"})

	if len(ops) != 1 || ops[0].Kind != KindDelete || ops[0].RType != "resource" || ops[0].Key != "card" || ops[0].LiveID != "abc" {
		t.Fatalf("ops = %+v", ops)
	}
	if !strings.Contains(ops[0].DiffText, "-url: '/local/card.js'") {
		t.Errorf("diff = %q", ops[0].DiffText)
	}
}

func TestPlanResourcesForgetWhenLiveAlreadyGone(t *testing.T) {
	ops := PlanResources(Desired{}, nil, "storage", map[string]string{"resource:card": "abc"})

	if len(ops) != 1 || ops[0].Kind != KindForget || ops[0].RType != "resource" || ops[0].Key != "card" || ops[0].LiveID != "abc" {
		t.Fatalf("ops = %+v", ops)
	}
	if want := "stop tracking resource:card: live resource abc is gone"; ops[0].DiffText != want {
		t.Errorf("diff = %q, want %q", ops[0].DiffText, want)
	}
}

// Renaming a manifest id: the new id adopts the old id's resource, and the
// old id is forgotten rather than deleting what was just adopted.
func TestPlanResourcesRenamedIDAdoptsAndForgetsInsteadOfDeleting(t *testing.T) {
	desired := resourcesDesired(Resource{ID: "new_name", URL: "/local/card.js", Type: "module"})
	live := []map[string]any{{"id": "abc", "url": "/local/card.js", "type": "module"}}
	managed := map[string]string{"resource:old_name": "abc"}

	ops := PlanResources(desired, live, "storage", managed)

	if len(ops) != 2 {
		t.Fatalf("ops = %+v, want an adopt and a forget", ops)
	}
	if ops[0].Kind != KindUpdate || ops[0].Key != "new_name" || ops[0].LiveID != "abc" {
		t.Errorf("ops[0] = %+v, want new_name adopting abc", ops[0])
	}
	if ops[1].Kind != KindForget || ops[1].Key != "old_name" ||
		ops[1].DiffText != "stop tracking resource:old_name: live resource abc is now managed as resource:new_name" {
		t.Errorf("ops[1] = %+v, want old_name forgotten", ops[1])
	}
}

// A resource one declared key manages is never adopted by another, even
// when that key's declared url is about to move it off the shared path.
func TestPlanResourcesManagedResourceIsNotAdoptedByAnotherID(t *testing.T) {
	desired := resourcesDesired(
		Resource{ID: "a", URL: "/local/b.js", Type: "module"},
		Resource{ID: "b", URL: "/local/a.js", Type: "module"},
	)
	live := []map[string]any{{"id": "abc", "url": "/local/a.js", "type": "module"}}

	ops := PlanResources(desired, live, "storage", map[string]string{"resource:a": "abc"})

	if len(ops) != 2 {
		t.Fatalf("ops = %+v", ops)
	}
	if ops[0].Kind != KindUpdate || ops[0].Key != "a" || ops[0].Params["url"] != "/local/b.js" {
		t.Errorf("ops[0] = %+v, want a's url updated", ops[0])
	}
	if ops[1].Kind != KindCreate || ops[1].Key != "b" {
		t.Errorf("ops[1] = %+v, want b created", ops[1])
	}
}

// HA keeps no url unique, so a managed resource that vanished and came
// back under a new id (HACS, or re-added by hand) is re-adopted rather
// than duplicated.
func TestPlanResourcesManagedButGoneReadoptsByPath(t *testing.T) {
	desired := resourcesDesired(Resource{ID: "card", URL: "/hacsfiles/card/card.js", Type: "module"})
	live := []map[string]any{{"id": "new-id", "url": "/hacsfiles/card/card.js?hacstag=2", "type": "module"}}

	ops := PlanResources(desired, live, "storage", map[string]string{"resource:card": "old-id"})

	if len(ops) != 1 || ops[0].Kind != KindUpdate || ops[0].LiveID != "new-id" || len(ops[0].Params) != 0 {
		t.Fatalf("ops = %+v, want a bookkeeping adopt of new-id", ops)
	}
}

func TestPlanResourcesManagedButGoneWithNoMatchIsACreate(t *testing.T) {
	desired := resourcesDesired(Resource{ID: "card", URL: "/local/card.js", Type: "module"})

	ops := PlanResources(desired, nil, "storage", map[string]string{"resource:card": "old-id"})

	if len(ops) != 1 || ops[0].Kind != KindCreate || ops[0].Key != "card" {
		t.Fatalf("ops = %+v", ops)
	}
}

// A managed id dropped from the manifest while its resource is contested
// by an ambiguous adopt is left alone: it may be the one being kept.
func TestPlanResourcesContestedResourceIsNotDeleted(t *testing.T) {
	desired := resourcesDesired(Resource{ID: "new_name", URL: "/local/card.js", Type: "module"})
	live := []map[string]any{
		{"id": "abc", "url": "/local/card.js", "type": "module"},
		{"id": "def", "url": "/local/card.js?v=1", "type": "module"},
	}

	ops := PlanResources(desired, live, "storage", map[string]string{"resource:old_name": "abc"})

	if len(ops) != 1 || ops[0].Kind != KindError || ops[0].Key != "new_name" {
		t.Fatalf("ops = %+v, want only the ambiguous-adopt error", ops)
	}
}

func TestPlanResourcesNeverTouchesUnmanagedLiveResources(t *testing.T) {
	live := []map[string]any{
		{"id": "abc", "url": "/hacsfiles/a/a.js?hacstag=1", "type": "module"},
		{"id": "def", "url": "/hacsfiles/b/b.js?hacstag=2", "type": "module"},
	}
	if ops := PlanResources(Desired{}, live, "storage", nil); len(ops) != 0 {
		t.Errorf("ops = %+v, want none", ops)
	}
}

func TestPlanResourcesYAMLModeRefusesEveryDeclaredEntryAndDeletesNothing(t *testing.T) {
	desired := resourcesDesired(
		Resource{ID: "a", URL: "/local/a.js", Type: "module"},
		Resource{ID: "b", URL: "/local/b.js", Type: "js"},
	)
	managed := map[string]string{"resource:a": "abc", "resource:gone": "def"}

	ops := PlanResources(desired, nil, "yaml", managed)

	if len(ops) != 2 {
		t.Fatalf("ops = %+v, want one error per declared entry and nothing else", ops)
	}
	for i, id := range []string{"a", "b"} {
		op := ops[i]
		if op.Kind != KindError || op.RType != "resource" || op.Key != id {
			t.Errorf("ops[%d] = %+v", i, op)
		}
		if !strings.Contains(op.Error, "YAML mode") || !strings.Contains(op.Error, "configuration.yaml under lovelace: resources:") {
			t.Errorf("ops[%d].Error = %q", i, op.Error)
		}
	}
	if len(managed) != 2 {
		t.Errorf("managed = %+v, want it untouched", managed)
	}
}

func TestPlanResourcesUnknownModeIsRefusedToo(t *testing.T) {
	ops := PlanResources(resourcesDesired(Resource{ID: "a", URL: "/local/a.js", Type: "js"}), nil, "", nil)
	if len(ops) != 1 || ops[0].Kind != KindError || !strings.Contains(ops[0].Error, "only be managed in storage mode") {
		t.Fatalf("ops = %+v", ops)
	}
}

// --- the shared DashboardManaged map -------------------------------------

// Plan must never read a "resource:" entry, even one whose live id
// happens to equal a live dashboard's.
func TestPlanIgnoresResourceKeysInManaged(t *testing.T) {
	live := []map[string]any{{"id": "abc", "url_path": "card", "title": "Card"}}
	managed := map[string]string{"resource:card": "abc"}

	if ops := Plan(Desired{}, live, nil, managed); len(ops) != 0 {
		t.Errorf("ops = %+v, want none", ops)
	}
}

func TestPlanResourcesIgnoresDashboardKeysInManaged(t *testing.T) {
	live := []map[string]any{{"id": "abc", "url": "/local/home.js", "type": "module"}}
	managed := map[string]string{"dashboard:home": "abc"}

	if ops := PlanResources(Desired{}, live, "storage", managed); len(ops) != 0 {
		t.Errorf("ops = %+v, want none", ops)
	}
}

// One manifest id used for both a dashboard and a resource stays two
// independent keys.
func TestSameIDForDashboardAndResourceDoesNotCross(t *testing.T) {
	desired := desiredWith("home", "Home", "x.yaml", nil)
	desired.Resources = []Resource{{ID: "home", URL: "/local/home.js", Type: "module"}}
	liveDashboards := []map[string]any{{"id": "d1", "url_path": "home", "title": "Home", "show_in_sidebar": true}}
	liveContent := map[string]map[string]any{"home": {"views": []any{}}}
	liveResources := []map[string]any{{"id": "r1", "url": "/local/home.js", "type": "module"}}
	managed := map[string]string{"dashboard:home": "d1", "resource:home": "r1"}

	if ops := Plan(desired, liveDashboards, liveContent, managed); len(ops) != 0 {
		t.Errorf("Plan ops = %+v, want none", ops)
	}
	if ops := PlanResources(desired, liveResources, "storage", managed); len(ops) != 0 {
		t.Errorf("PlanResources ops = %+v, want none", ops)
	}
}
