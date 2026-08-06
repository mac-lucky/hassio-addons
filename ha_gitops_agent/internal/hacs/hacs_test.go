package hacs

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/registries"
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

// writeManifest writes content as <dir>/gitops/hacs.yaml.
func writeManifest(t *testing.T, dir, content string) {
	t.Helper()
	writeFile(t, filepath.Join(dir, "gitops", "hacs.yaml"), content)
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

// declared is one already-validated manifest item, as LoadManifest would
// have produced it.
func declared(id, repository, version string) map[string]any {
	item := map[string]any{"id": id, "repository": repository, "category": CategoryIntegration}
	if version != "" {
		item["version"] = version
	}
	return item
}

// liveRepo is one entry of hacs/repositories/list, carrying the fields
// this layer reads.
func liveRepo(id, fullName, domain string, installed bool) map[string]any {
	return map[string]any{
		"id": id, "full_name": fullName, "domain": domain,
		"installed": installed, "category": CategoryIntegration,
	}
}

func opKinds(ops []registries.RegOp) []string {
	out := make([]string, len(ops))
	for i, op := range ops {
		out[i] = op.Kind + " " + op.Key
	}
	return out
}

// --- LoadManifest ----------------------------------------------------

// The layer is simply inactive that cycle. It has no delete path, so an
// absent file cannot mean what an absent gitops/integrations.yaml means.
func TestMissingHacsFileIsNotAnError(t *testing.T) {
	dir := t.TempDir()
	desired, err := LoadManifest(dir)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(desired.Repos) != 0 {
		t.Errorf("repos = %+v, want empty", desired.Repos)
	}
}

func TestMissingGitopsDirIsNotAnError(t *testing.T) {
	dir := t.TempDir()
	desired, err := LoadManifest(filepath.Join(dir, "nonexistent"))
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(desired.Repos) != 0 {
		t.Errorf("repos = %+v, want empty", desired.Repos)
	}
}

func TestEmptyHacsKeyIsNotAnError(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "hacs:\n")
	desired, err := LoadManifest(dir)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(desired.Repos) != 0 {
		t.Errorf("repos = %+v, want empty", desired.Repos)
	}
}

func TestLoadManifestInvalidYAMLIsAnError(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "hacs: [\n")
	if _, err := LoadManifest(dir); err == nil {
		t.Fatal("err = nil, want an error")
	}
}

func TestLoadManifestReadsEveryField(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, `
hacs:
  - id: anker_solix
    repository: thomluther/ha-anker-solix
    category: integration
    version: "3.1.0"
`)
	desired, err := LoadManifest(dir)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	want := []map[string]any{{
		"id": "anker_solix", "repository": "thomluther/ha-anker-solix",
		"category": "integration", "version": "3.1.0",
	}}
	if !reflect.DeepEqual(desired.Repos, want) {
		t.Errorf("repos = %+v, want %+v", desired.Repos, want)
	}
}

// HACS itself accepts either spelling, and a user copies the URL out of
// the address bar more often than they type owner/name.
func TestLoadManifestNormalizesARepositoryURL(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, `
hacs:
  - id: anker_solix
    repository: https://github.com/thomluther/ha-anker-solix.git
    category: integration
`)
	desired, err := LoadManifest(dir)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got := desired.Repos[0]["repository"]; got != "thomluther/ha-anker-solix" {
		t.Errorf("repository = %v, want the normalized owner/name", got)
	}
}

func TestLoadManifestRejectsABadID(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "hacs:\n  - id: Anker Solix\n    repository: a/b\n    category: integration\n")
	if got := problems(t, mustErr(t, dir)); !strings.Contains(got, "invalid or missing 'id'") {
		t.Errorf("problems = %q", got)
	}
}

func TestLoadManifestRejectsADuplicateID(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, `
hacs:
  - id: solix
    repository: a/b
    category: integration
  - id: solix
    repository: c/d
    category: integration
`)
	if got := problems(t, mustErr(t, dir)); !strings.Contains(got, "duplicate hacs id 'solix'") {
		t.Errorf("problems = %q", got)
	}
}

func TestLoadManifestRejectsAnUnusableRepository(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "hacs:\n  - id: solix\n    repository: not-a-repo\n    category: integration\n")
	if got := problems(t, mustErr(t, dir)); !strings.Contains(got, "is not a github owner/name") {
		t.Errorf("problems = %q", got)
	}
}

// Every other category HACS distributes - plugin, theme, python_script -
// is refused per item rather than silently ignored.
func TestLoadManifestRejectsANonIntegrationCategory(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "hacs:\n  - id: mushroom\n    repository: piitaya/lovelace-mushroom\n    category: plugin\n")
	got := problems(t, mustErr(t, dir))
	if !strings.Contains(got, "declares category 'plugin'") || !strings.Contains(got, "only 'integration' is supported") {
		t.Errorf("problems = %q", got)
	}
}

// YAML reads an unquoted 3.10 as the float 3.1, asking HACS for a release
// tag that does not exist; the error says to quote it.
func TestLoadManifestRejectsAnUnquotedVersion(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "hacs:\n  - id: solix\n    repository: a/b\n    category: integration\n    version: 3.10\n")
	if got := problems(t, mustErr(t, dir)); !strings.Contains(got, "must be a non-empty quoted string") {
		t.Errorf("problems = %q", got)
	}
}

func TestLoadManifestRejectsUnsupportedFields(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "hacs:\n  - id: solix\n    repository: a/b\n    category: integration\n    beta: true\n")
	if got := problems(t, mustErr(t, dir)); !strings.Contains(got, "unsupported field(s) beta") {
		t.Errorf("problems = %q", got)
	}
}

// Every problem in one report, not just the first: fixing a manifest one
// error per reconcile interval is how a five-minute loop becomes an hour.
func TestLoadManifestReportsEveryProblemAtOnce(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, `
hacs:
  - id: solix
    repository: bad
    category: integration
  - id: other
    repository: a/b
    category: theme
`)
	got := problems(t, mustErr(t, dir))
	if !strings.Contains(got, "is not a github owner/name") || !strings.Contains(got, "declares category 'theme'") {
		t.Errorf("problems = %q, want both items reported", got)
	}
}

func mustErr(t *testing.T, dir string) error {
	t.Helper()
	_, err := LoadManifest(dir)
	return err
}

// --- NormalizeRepository ---------------------------------------------

func TestNormalizeRepositoryAcceptsTheShapesAUserPastes(t *testing.T) {
	for _, declared := range []string{
		"thomluther/ha-anker-solix",
		"https://github.com/thomluther/ha-anker-solix",
		"https://github.com/thomluther/ha-anker-solix/",
		"https://github.com/thomluther/ha-anker-solix.git",
		"github.com/thomluther/ha-anker-solix",
		"  thomluther/ha-anker-solix  ",
	} {
		got, ok := NormalizeRepository(declared)
		if !ok || got != "thomluther/ha-anker-solix" {
			t.Errorf("NormalizeRepository(%q) = %q, %v", declared, got, ok)
		}
	}
}

func TestNormalizeRepositoryRefusesEverythingElse(t *testing.T) {
	for _, declared := range []string{"", "owner", "owner/name/extra", "owner name", "https://example.invalid/a/b"} {
		if got, ok := NormalizeRepository(declared); ok {
			t.Errorf("NormalizeRepository(%q) = %q, true - want a refusal", declared, got)
		}
	}
}

// --- Plan: install ----------------------------------------------------

// Not on the box, so it is downloaded, and the card names the repository,
// the version, and the restart that makes it load.
func TestPlanInstallsWhatIsMissing(t *testing.T) {
	desired := Desired{Repos: []map[string]any{declared("anker_solix", "thomluther/ha-anker-solix", "3.1.0")}}

	ops := Plan(desired, nil, nil, nil)

	if len(ops) != 1 {
		t.Fatalf("ops = %+v, want one install", ops)
	}
	op := ops[0]
	if op.Kind != KindCreate || op.RType != rtype || op.Key != "anker_solix" {
		t.Errorf("op = %+v, want a create of the hacs rtype", op)
	}
	if got := op.Params["repository"]; got != "thomluther/ha-anker-solix" {
		t.Errorf("params repository = %v", got)
	}
	if got := op.Params["version"]; got != "3.1.0" {
		t.Errorf("params version = %v", got)
	}
	if got, _ := op.Params["repository_id"].(string); got != "" {
		t.Errorf("params repository_id = %q, want empty - HACS has never heard of it", got)
	}
	for _, want := range []string{"thomluther/ha-anker-solix", "integration", "3.1.0", "restart"} {
		if !strings.Contains(op.DiffText, want) {
			t.Errorf("diff text = %q, want it to mention %q", op.DiffText, want)
		}
	}
}

// A repository HACS already lists but has not downloaded is installed by
// its known id, so the driver never has to add it as a custom repository.
func TestPlanInstallsAKnownButNotDownloadedRepositoryByID(t *testing.T) {
	desired := Desired{Repos: []map[string]any{declared("anker_solix", "thomluther/ha-anker-solix", "")}}
	live := []map[string]any{liveRepo("1234", "thomluther/ha-anker-solix", "anker_solix", false)}

	ops := Plan(desired, live, nil, nil)

	if len(ops) != 1 || ops[0].Kind != KindCreate {
		t.Fatalf("ops = %+v, want one install", ops)
	}
	if got := ops[0].Params["repository_id"]; got != "1234" {
		t.Errorf("params repository_id = %v, want the id HACS already has", got)
	}
	if got := ops[0].Params["domain"]; got != "anker_solix" {
		t.Errorf("params domain = %v, want the hint HACS already carries", got)
	}
	if _, present := ops[0].Params["version"]; present {
		t.Errorf("params = %+v, want no version key when none is declared", ops[0].Params)
	}
	if !strings.Contains(ops[0].DiffText, "(latest release)") {
		t.Errorf("diff text = %q, want it to name the default", ops[0].DiffText)
	}
}

// --- Plan: adopt ------------------------------------------------------

// Already on the box: nothing is downloaded, and the only op is the
// bookkeeping one recording who owns it.
func TestPlanAdoptsWhatIsAlreadyInstalled(t *testing.T) {
	desired := Desired{Repos: []map[string]any{declared("anker_solix", "thomluther/ha-anker-solix", "3.1.0")}}
	live := []map[string]any{liveRepo("1234", "thomluther/ha-anker-solix", "anker_solix", true)}

	ops := Plan(desired, live, nil, nil)

	if len(ops) != 1 {
		t.Fatalf("ops = %+v, want one adopt", ops)
	}
	op := ops[0]
	if op.Kind != KindUpdate || op.LiveID != "1234" {
		t.Errorf("op = %+v, want an update carrying the live id", op)
	}
	if adopt, _ := op.Params["adopt"].(bool); !adopt {
		t.Errorf("params = %+v, want the adopt marker the driver reads", op.Params)
	}
	if !strings.Contains(op.DiffText, "nothing is downloaded") {
		t.Errorf("diff text = %q", op.DiffText)
	}
}

// The second cycle plans nothing - not even when the manifest pins a
// version other than the installed one: version is install-time only.
func TestPlanIsSilentOnceOwnershipIsRecorded(t *testing.T) {
	desired := Desired{Repos: []map[string]any{declared("anker_solix", "thomluther/ha-anker-solix", "9.9.9")}}
	live := []map[string]any{liveRepo("1234", "thomluther/ha-anker-solix", "anker_solix", true)}
	managed := map[string]string{"hacs:anker_solix": "1234"}

	if ops := Plan(desired, live, managed, nil); len(ops) != 0 {
		t.Errorf("ops = %+v, want nothing at all", ops)
	}
}

// GitHub treats owner/name case-insensitively, so a manifest spelling it
// differently from HACS must adopt, not download a second copy.
func TestPlanMatchesFullNameCaseInsensitively(t *testing.T) {
	desired := Desired{Repos: []map[string]any{declared("anker_solix", "ThomLuther/HA-Anker-Solix", "")}}
	live := []map[string]any{liveRepo("1234", "thomluther/ha-anker-solix", "anker_solix", true)}

	ops := Plan(desired, live, nil, nil)

	if len(ops) != 1 || ops[0].Kind != KindUpdate {
		t.Fatalf("ops = %+v, want an adopt rather than a second download", ops)
	}
}

// Two HACS entries for one GitHub repository - a custom repository added
// for something already in the default store. The installed one wins.
func TestPlanPrefersTheInstalledDuplicate(t *testing.T) {
	desired := Desired{Repos: []map[string]any{declared("anker_solix", "thomluther/ha-anker-solix", "")}}
	live := []map[string]any{
		liveRepo("1234", "thomluther/ha-anker-solix", "anker_solix", false),
		liveRepo("5678", "thomluther/ha-anker-solix", "anker_solix", true),
	}

	ops := Plan(desired, live, nil, nil)

	if len(ops) != 1 || ops[0].Kind != KindUpdate || ops[0].LiveID != "5678" {
		t.Fatalf("ops = %+v, want an adopt of the installed entry", ops)
	}
}

// --- Plan: removal, failure memory, per-item errors --------------------

// Removing the item stops the agent following it and nothing else: no
// uninstall op, no unmanage op, ownership record left standing.
func TestPlanNeverUninstallsWhatTheManifestStoppedDeclaring(t *testing.T) {
	live := []map[string]any{liveRepo("1234", "thomluther/ha-anker-solix", "anker_solix", true)}
	managed := map[string]string{"hacs:anker_solix": "1234"}

	if ops := Plan(Desired{Repos: []map[string]any{}}, live, managed, nil); len(ops) != 0 {
		t.Errorf("ops = %+v, want nothing planned for an undeclared item", ops)
	}
	if managed["hacs:anker_solix"] != "1234" {
		t.Errorf("managed = %+v, want the ownership record left alone", managed)
	}
}

// A failed download is not retried on its own, and the error op says how
// to retry it - the cause may be outside the manifest entirely.
func TestPlanRefusesToRetryTheSameFailedEntry(t *testing.T) {
	item := declared("anker_solix", "thomluther/ha-anker-solix", "9.9.9")
	desired := Desired{Repos: []map[string]any{item}}
	attempts := map[string]map[string]any{
		"hacs:anker_solix": {"hash": hashEntry(item), "error": "no release tagged 9.9.9"},
	}

	ops := Plan(desired, nil, nil, attempts)

	if len(ops) != 1 || ops[0].Kind != KindError {
		t.Fatalf("ops = %+v, want one error op", ops)
	}
	for _, want := range []string{"no release tagged 9.9.9", "press Retry"} {
		if !strings.Contains(ops[0].Error, want) {
			t.Errorf("error = %q, want it to mention %q", ops[0].Error, want)
		}
	}
}

// Editing the entry is the other way out of a blocked item: a different
// version is a different declaration, so the old memory no longer fits.
func TestPlanRetriesOnceTheEntryChanges(t *testing.T) {
	failed := declared("anker_solix", "thomluther/ha-anker-solix", "9.9.9")
	attempts := map[string]map[string]any{
		"hacs:anker_solix": {"hash": hashEntry(failed), "error": "no release tagged 9.9.9"},
	}
	desired := Desired{Repos: []map[string]any{declared("anker_solix", "thomluther/ha-anker-solix", "3.1.0")}}

	ops := Plan(desired, nil, nil, attempts)

	if len(ops) != 1 || ops[0].Kind != KindCreate {
		t.Fatalf("ops = %+v, want the corrected entry attempted again", ops)
	}
}

// An adopt sends nothing, so it can never be what failed - blocking one
// would strand an item on a record that cannot describe it.
func TestPlanAdoptIsNotBlockedByAFailureMemory(t *testing.T) {
	item := declared("anker_solix", "thomluther/ha-anker-solix", "")
	desired := Desired{Repos: []map[string]any{item}}
	live := []map[string]any{liveRepo("1234", "thomluther/ha-anker-solix", "anker_solix", true)}
	attempts := map[string]map[string]any{
		"hacs:anker_solix": {"hash": hashEntry(item), "error": "github rate limit"},
	}

	ops := Plan(desired, live, nil, attempts)

	if len(ops) != 1 || ops[0].Kind != KindUpdate {
		t.Fatalf("ops = %+v, want the adopt planned anyway", ops)
	}
}

// One unusable live object stops that item alone; every other declared
// repository is still planned.
func TestPlanReportsAnInstalledRepositoryWithNoIDPerItem(t *testing.T) {
	desired := Desired{Repos: []map[string]any{
		declared("broken", "owner/broken", ""),
		declared("anker_solix", "thomluther/ha-anker-solix", ""),
	}}
	live := []map[string]any{
		{"full_name": "owner/broken", "installed": true, "category": CategoryIntegration},
	}

	ops := Plan(desired, live, nil, nil)

	if got := opKinds(ops); !reflect.DeepEqual(got, []string{"error broken", "create anker_solix"}) {
		t.Fatalf("ops = %v, want the broken item alone refused", got)
	}
	if !strings.Contains(ops[0].Error, "no usable repository id") {
		t.Errorf("error = %q", ops[0].Error)
	}
}

// --- PruneRestartPending ----------------------------------------------

// A domain Home Assistant now carries is loaded, whether that came from a
// restart or from setting the integration up.
func TestPruneRestartPendingDropsLoadedDomains(t *testing.T) {
	got := PruneRestartPending(
		[]string{"anker_solix", "adaptive_lighting"},
		[]string{"sensor", "hacs", "adaptive_lighting"})

	if !reflect.DeepEqual(got, []string{"anker_solix"}) {
		t.Errorf("pending = %v, want only the domain that is still missing", got)
	}
}

// Sorted, deduplicated and never nil: this is persisted in state.json and
// rendered into a polled fragment compared byte for byte.
func TestPruneRestartPendingIsSortedAndDeduplicated(t *testing.T) {
	got := PruneRestartPending([]string{"zeta", "alpha", "zeta", ""}, nil)

	if !reflect.DeepEqual(got, []string{"alpha", "zeta"}) {
		t.Errorf("pending = %v, want a sorted, deduplicated list", got)
	}
	if empty := PruneRestartPending(nil, nil); empty == nil || len(empty) != 0 {
		t.Errorf("pending = %v, want an empty non-nil list", empty)
	}
}

// --- hashEntry --------------------------------------------------------

// The fingerprint has to move when any declared field does, or a corrected
// manifest entry would stay blocked by the failure of the old one.
func TestHashEntryCoversEveryDeclaredField(t *testing.T) {
	base := declared("anker_solix", "thomluther/ha-anker-solix", "3.1.0")
	for _, other := range []map[string]any{
		declared("anker_solix_2", "thomluther/ha-anker-solix", "3.1.0"),
		declared("anker_solix", "thomluther/other", "3.1.0"),
		declared("anker_solix", "thomluther/ha-anker-solix", "3.2.0"),
		declared("anker_solix", "thomluther/ha-anker-solix", ""),
	} {
		if hashEntry(base) == hashEntry(other) {
			t.Errorf("hashEntry(%+v) collides with the base entry", other)
		}
	}
	if hashEntry(base) != hashEntry(declared("anker_solix", "thomluther/ha-anker-solix", "3.1.0")) {
		t.Error("hashEntry is not stable across two identical entries")
	}
}

// Two ids for one repository would both plan an install and whichever ran
// last would decide the version - the manifest would mean different things
// depending on its line order.
func TestLoadManifestRejectsTwoIDsDeclaringOneRepository(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, `
hacs:
  - id: solix
    repository: thomluther/ha-anker-solix
    category: integration
    version: "3.1.0"
  - id: solix_pinned
    repository: ThomLuther/HA-Anker-Solix
    category: integration
    version: "2.0.0"
`)
	got := problems(t, mustErr(t, dir))
	if !strings.Contains(got, "'solix' and 'solix_pinned' both declare repository") {
		t.Errorf("problems = %q, want both ids named", got)
	}
}

// GitHub's own rule, worth enforcing: HACS's add handler returns without
// replying for a shape its regex rejects, so a typo that reaches the box
// costs a hung command and a remembered failure.
func TestNormalizeRepositoryRefusesShapesGitHubItselfWouldNot(t *testing.T) {
	for _, declared := range []string{
		"own_er/name", // underscore in the owner half
		"own.er/name", // dot in the owner half
		"owner/..",    // a path segment that walks up
		"owner/.",     // and one that goes nowhere
		"../owner/name",
	} {
		if got, ok := NormalizeRepository(declared); ok {
			t.Errorf("NormalizeRepository(%q) = %q, true - want a refusal", declared, got)
		}
	}
	// A dot inside a NAME is legal and common (home-assistant.io-style
	// repositories), so the strictness must not go further than GitHub's.
	if got, ok := NormalizeRepository("owner/some.repo_name-2"); !ok || got != "owner/some.repo_name-2" {
		t.Errorf("NormalizeRepository = %q, %v - want a legal name accepted", got, ok)
	}
}

// Pointing an id at a different repository would install the new one and
// drop the record of the old, leaving the first on the box unowned.
func TestPlanRefusesToRebindAnIDToADifferentRepository(t *testing.T) {
	desired := Desired{Repos: []map[string]any{declared("solix", "someone/other-solix", "")}}
	live := []map[string]any{
		liveRepo("1234", "thomluther/ha-anker-solix", "anker_solix", true),
		liveRepo("5678", "someone/other-solix", "other_solix", false),
	}
	managed := map[string]string{"hacs:solix": "1234"}

	ops := Plan(desired, live, managed, nil)

	if len(ops) != 1 || ops[0].Kind != KindError {
		t.Fatalf("ops = %+v, want one refusal", ops)
	}
	for _, want := range []string{"already manages HACS repository 1234", "new id", "no owner"} {
		if !strings.Contains(ops[0].Error, want) {
			t.Errorf("error = %q, want it to mention %q", ops[0].Error, want)
		}
	}
}

// The same id and repository is not a rebind, whatever else changed -
// otherwise every managed item would refuse itself.
func TestPlanDoesNotMistakeAnUnchangedEntryForARebind(t *testing.T) {
	desired := Desired{Repos: []map[string]any{declared("solix", "thomluther/ha-anker-solix", "9.9.9")}}
	live := []map[string]any{liveRepo("1234", "thomluther/ha-anker-solix", "anker_solix", true)}
	managed := map[string]string{"hacs:solix": "1234"}

	if ops := Plan(desired, live, managed, nil); len(ops) != 0 {
		t.Errorf("ops = %+v, want nothing at all", ops)
	}
}
