package recon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/addonopts"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/applier"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/applier/statetest"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/differ"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/regapply"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/registries"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/subentries"
)

// --- the blocked mirror ---------------------------------------------------

// blockedState is one failure per layer that keeps a memory, each carrying
// the "hash" sibling the mirror must never copy.
func blockedState(fakes *reconcilerFakes) {
	fakes.applier.state.IntegrationAttempts = map[string]map[string]any{
		"integration:workday_main": {"hash": "sha256-of-declared-data", "error": "invalid_auth"},
	}
	fakes.applier.state.SubentryAttempts = map[string]map[string]any{
		"subentry:widget_kitchen": {"hash": "sha256-of-declared-data", "error": "unexpected step"},
	}
}

// Hydrated in New, not on the first cycle, which after a restart can be
// minutes away.
func TestStatusCarriesTheBlockedItemsFromPersistedState(t *testing.T) {
	fakes := newReconcilerFakes()
	blockedState(fakes)

	r := fakes.reconciler(baseOpts())

	want := []BlockedItem{
		{Key: "integration:workday_main", RType: "integration", Name: "workday_main", Error: "invalid_auth"},
		{Key: "subentry:widget_kitchen", RType: "subentry", Name: "widget_kitchen", Error: "unexpected step"},
	}
	if got := r.Status().Blocked; !reflect.DeepEqual(got, want) {
		t.Errorf("blocked = %+v, want %+v", got, want)
	}
}

// The source is two maps, so an unsorted list would change the polled
// fragment's bytes every render and /fragment could never answer 204.
func TestStatusSortsTheBlockedItemsByKey(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.applier.state.IntegrationAttempts = map[string]map[string]any{
		"integration:zeta":  {"error": "z"},
		"integration:alpha": {"error": "a"},
	}
	fakes.applier.state.SubentryAttempts = map[string]map[string]any{
		"subentry:beta": {"error": "b"},
		"subentry:acme": {"error": "c"},
	}
	r := fakes.reconciler(baseOpts())

	var keys []string
	for _, item := range r.Status().Blocked {
		keys = append(keys, item.Key)
	}

	want := []string{"integration:alpha", "integration:zeta", "subentry:acme", "subentry:beta"}
	if !reflect.DeepEqual(keys, want) {
		t.Errorf("blocked keys = %v, want %v", keys, want)
	}
}

// The stored hash fingerprints the declared data, where a rejected
// password would be (see refreshStateMirrors' secret boundary).
func TestBlockedItemsCarryNoHash(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.applier.state.IntegrationAttempts = map[string]map[string]any{
		"integration:workday_main": {"hash": "sentinel-hash-value", "error": "invalid_auth"},
	}
	r := fakes.reconciler(baseOpts())

	for _, item := range r.Status().Blocked {
		if strings.Contains(item.Key+item.RType+item.Name+item.Error, "sentinel-hash-value") {
			t.Errorf("blocked item = %+v, want the declared-data hash left behind", item)
		}
	}
}

// state.json is user-writable, so a non-string error is reachable. The
// record still gets listed, with the planners' own fallback wording.
func TestBlockedItemSurvivesANonStringError(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.applier.state.IntegrationAttempts = map[string]map[string]any{
		"integration:workday_main": {"hash": "h", "error": 42},
		"integration:moon_home":    {"hash": "h"},
	}
	r := fakes.reconciler(baseOpts())

	blocked := r.Status().Blocked
	if len(blocked) != 2 {
		t.Fatalf("blocked = %+v, want both items listed anyway", blocked)
	}
	for _, item := range blocked {
		if item.Error != "unknown error" {
			t.Errorf("error = %q, want the planners' own fallback wording", item.Error)
		}
	}
}

// The mirror follows every operation holding a fresh state, not just the
// ones that write attempts.
func TestReconcileNowRefreshesTheBlockedMirror(t *testing.T) {
	fakes := newReconcilerFakes()
	r := fakes.reconciler(baseOpts())
	if got := r.Status().Blocked; len(got) != 0 {
		t.Fatalf("blocked = %+v, want none before the cycle", got)
	}
	blockedState(fakes)

	r.ReconcileNow(context.Background())

	if got := r.Status().Blocked; len(got) != 2 {
		t.Errorf("blocked = %+v, want the two records the cycle read", got)
	}
}

// Rollback re-reads and re-saves the state, so its mirror follows what it
// saved - the same rule ApplyNow's refresh follows.
func TestRollbackRefreshesTheBlockedMirror(t *testing.T) {
	fakes := newReconcilerFakes()
	stashDir := t.TempDir()
	writeFile(t, filepath.Join(stashDir, "manifest.json"), `{"files": {}, "created_dirs": []}`)
	writeFile(t, filepath.Join(stashDir, "registry_stash.json"), `{"ops": []}`)
	fakes.differ.changes = []differ.Change{{Path: "automations.yaml", Kind: "update", DiffText: "+x"}}
	fakes.applier.applyResult = applier.Result{OK: true, Changed: []string{"automations.yaml"}, StashDir: stashDir}
	opts := baseOpts()
	opts.DryRun = false
	r := fakes.reconciler(opts)
	r.ReconcileNow(context.Background())
	r.ApplyNow(context.Background(), true)
	if got := r.Status().Blocked; len(got) != 0 {
		t.Fatalf("blocked = %+v, want none before the rollback", got)
	}
	// After the apply, so only the rollback's own refresh can surface it.
	blockedState(fakes)

	r.Rollback(context.Background())

	if got := r.Status().Blocked; len(got) != 2 {
		t.Errorf("blocked = %+v, want the two records the rollback saved", got)
	}
}

// A files-only rollback takes none of the registry branches, but its
// mirrors must still follow or the dashboard lags by a whole interval.
func TestFileOnlyRollbackRefreshesTheMirrors(t *testing.T) {
	fakes := newReconcilerFakes()
	stashDir := t.TempDir()
	// A file stash and nothing else, as a files-only install leaves.
	writeFile(t, filepath.Join(stashDir, "manifest.json"), `{"files": {}, "created_dirs": []}`)
	fakes.differ.changes = []differ.Change{{Path: "automations.yaml", Kind: "update", DiffText: "+x"}}
	fakes.applier.applyResult = applier.Result{OK: true, Changed: []string{"automations.yaml"}, StashDir: stashDir}
	opts := baseOpts()
	opts.DryRun = false
	r := fakes.reconciler(opts)
	r.ReconcileNow(context.Background())
	r.ApplyNow(context.Background(), true)
	// After the apply, so only the rollback's own refresh can surface it.
	fakes.applier.state = statetest.PoisonedState(t)

	r.Rollback(context.Background())

	if got := r.Status().Managed.Entities; !reflect.DeepEqual(got, []string{"light.kitchen", "sensor.outdoor_dew"}) {
		t.Errorf("entities = %v, want the mirror refreshed by a file-only rollback", got)
	}
	if got := r.Status().Blocked; len(got) != 3 {
		t.Errorf("blocked = %+v, want the records a file-only rollback read", got)
	}
}

// An apply is where an item first becomes blocked, so the mirror follows
// the state the layers wrote rather than waiting for the next cycle.
func TestApplyNowRefreshesTheBlockedMirrorFromWhatItSaved(t *testing.T) {
	fakes := newReconcilerFakes()
	// StateLoad always hands the layer a map to write into; the fake's zero
	// state does not, and the layer below writes in place.
	fakes.applier.state.SubentryAttempts = map[string]map[string]any{}
	fakes.registryApplier.onApplySubentryPlan = func(
		managed, hashes map[string]string, attempts map[string]map[string]any,
	) {
		attempts["subentry:kitchen"] = map[string]any{"hash": "h", "error": "flow rejected the submitted data"}
	}
	fakes.registryApplier.applyFlowResult = regapply.RegistryApplyResult{OK: true, Applied: []string{"create integration:pw"}}
	r := pendingSubentryReconciler(t, fakes)

	if got := r.Status().Blocked; len(got) != 0 {
		t.Fatalf("blocked = %+v, want none before the apply", got)
	}
	r.ApplyNow(context.Background(), true)

	want := []BlockedItem{{
		Key: "subentry:kitchen", RType: "subentry", Name: "kitchen",
		Error: "flow rejected the submitted data",
	}}
	if got := r.Status().Blocked; !reflect.DeepEqual(got, want) {
		t.Errorf("blocked = %+v, want %+v", got, want)
	}
}

// --- the managed inventory ------------------------------------------------

// state.json holds declared flow data in the clear and /status.json is
// public, so the inventory carries keys and never the values beside them.
func TestStatusNeverCarriesASecretOutOfTheState(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.applier.state = statetest.PoisonedState(t)

	status := fakes.reconciler(baseOpts()).Status()

	encoded, err := json.Marshal(status)
	if err != nil {
		t.Fatalf("marshal status: %v", err)
	}
	if bytes.Contains(encoded, []byte(statetest.Sentinel)) {
		t.Errorf("status.json carries a value out of state.json:\n%s", encoded)
	}
	// The other half: an empty inventory would pass the check above.
	for _, want := range statetest.ManagedNames() {
		if !bytes.Contains(encoded, []byte(`"`+want+`"`)) {
			t.Errorf("status.json does not list %q, so the check above proves nothing", want)
		}
	}
}

// Each group drops the prefix its name already carries, except registry's,
// which tells a floor from an area. Sorted: the fragment is compared bytewise.
func TestStatusListsEveryManagedGroupSortedAndUnprefixed(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.applier.state = statetest.PoisonedState(t)

	managed := fakes.reconciler(baseOpts()).Status().Managed

	want := ManagedInventory{
		Files:        []string{"automations.yaml", "packages/lights.yaml"},
		Registry:     []string{"floor:ground"},
		Entities:     []string{"light.kitchen", "sensor.outdoor_dew"},
		Dashboards:   []string{"energy"},
		Addons:       []string{"core_ssh"},
		Integrations: []string{"workday_main"},
		Subentries:   []string{"widget_hall"},
		Hacs:         []string{"anker_solix"},
	}
	if !reflect.DeepEqual(managed, want) {
		t.Errorf("managed = %+v, want %+v", managed, want)
	}
	if got := managed.Total(); got != 10 {
		t.Errorf("total = %d, want 10 - every group counts", got)
	}
}

// Status hands out a copy: writing to one of its groups must not rewrite
// the agent's own view of what it manages.
func TestStatusManagedGroupsDoNotAliasTheMirror(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.applier.state = statetest.PoisonedState(t)
	r := fakes.reconciler(baseOpts())

	first := r.Status().Managed
	if len(first.Files) == 0 || len(first.Entities) == 0 {
		t.Fatal("fixture manages nothing, so aliasing cannot be observed")
	}
	first.Files[0] = "rewritten.yaml"
	first.Entities[0] = "rewritten.entity"

	second := r.Status().Managed
	if second.Files[0] != "automations.yaml" || second.Entities[0] != "light.kitchen" {
		t.Errorf("managed = %+v, want the mirror unchanged by a caller's writes", second)
	}
}

// The manifest goes back into state.json on every save, so the mirror
// sorts a copy rather than the caller's own slice.
func TestManagedInventoryLeavesTheManifestOrderAlone(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.applier.state.Manifest = []string{"packages/lights.yaml", "automations.yaml"}

	fakes.reconciler(baseOpts())

	want := []string{"packages/lights.yaml", "automations.yaml"}
	if !reflect.DeepEqual(fakes.applier.state.Manifest, want) {
		t.Errorf("manifest = %v, want %v - the mirror reordered the state's own slice", fakes.applier.state.Manifest, want)
	}
}

// Every group serializes as [] rather than null, the convention every
// other list on Status follows.
func TestManagedInventoryIsEmptyButNeverNull(t *testing.T) {
	fakes := newReconcilerFakes()

	status := fakes.reconciler(baseOpts()).Status()

	if got := status.Managed.Total(); got != 0 {
		t.Errorf("total = %d, want 0 on an agent that has applied nothing", got)
	}
	encoded, err := json.Marshal(status.Managed)
	if err != nil {
		t.Fatalf("marshal managed: %v", err)
	}
	want := `{"files":[],"registry":[],"entities":[],"dashboards":[],"addons":[],"integrations":[],"subentries":[],"hacs":[]}`
	if string(encoded) != want {
		t.Errorf("managed = %s, want %s", encoded, want)
	}
}

// The inventory rides the same refresh as the blocked list, so a state
// changed underneath it - by another process, or by hand - still shows.
func TestReconcileNowRefreshesTheManagedInventory(t *testing.T) {
	fakes := newReconcilerFakes()
	r := fakes.reconciler(baseOpts())
	if got := r.Status().Managed.Total(); got != 0 {
		t.Fatalf("total = %d, want nothing before the cycle", got)
	}
	fakes.applier.state = statetest.PoisonedState(t)

	r.ReconcileNow(context.Background())

	if got := r.Status().Managed.Entities; !reflect.DeepEqual(got, []string{"light.kitchen", "sensor.outdoor_dew"}) {
		t.Errorf("entities = %v, want the two the cycle read", got)
	}
}

// --- RetryBlocked ---------------------------------------------------------

func TestRetryBlockedClearsTheAttemptAndPersistsIt(t *testing.T) {
	fakes := newReconcilerFakes()
	blockedState(fakes)
	r := fakes.reconciler(baseOpts())

	if err := r.RetryBlocked("integration:workday_main"); err != nil {
		t.Fatalf("RetryBlocked: %v", err)
	}

	if len(fakes.applier.stateSaveCalls) != 1 {
		t.Fatalf("state_save_calls = %d, want 1", len(fakes.applier.stateSaveCalls))
	}
	saved := fakes.applier.stateSaveCalls[0]
	if _, still := saved.IntegrationAttempts["integration:workday_main"]; still {
		t.Errorf("saved integration_attempts = %+v, want the retried item gone", saved.IntegrationAttempts)
	}
	// Each item is retried on its own; the other layer's memory stands.
	if len(saved.SubentryAttempts) != 1 {
		t.Errorf("saved subentry_attempts = %+v, want the other item left alone", saved.SubentryAttempts)
	}
	want := []BlockedItem{
		{Key: "subentry:widget_kitchen", RType: "subentry", Name: "widget_kitchen", Error: "unexpected step"},
	}
	if got := r.Status().Blocked; !reflect.DeepEqual(got, want) {
		t.Errorf("blocked = %+v, want %+v - the mirror must follow the save", got, want)
	}
	if events := r.Status().Events; !hasEventContaining(events, "retry: cleared failure memory for integration:workday_main") {
		t.Errorf("events = %+v, want the cleared-memory entry", events)
	}
}

func TestRetryBlockedClearsASubentryAttempt(t *testing.T) {
	fakes := newReconcilerFakes()
	blockedState(fakes)
	r := fakes.reconciler(baseOpts())

	if err := r.RetryBlocked("subentry:widget_kitchen"); err != nil {
		t.Fatalf("RetryBlocked: %v", err)
	}

	if len(fakes.applier.stateSaveCalls) != 1 {
		t.Fatalf("state_save_calls = %d, want 1", len(fakes.applier.stateSaveCalls))
	}
	if got := fakes.applier.stateSaveCalls[0].SubentryAttempts; len(got) != 0 {
		t.Errorf("saved subentry_attempts = %+v, want the retried item gone", got)
	}
}

// The Retry button answers with a re-render of the same page, so a silent
// refusal would look exactly like a retry that worked.
func TestRetryBlockedLogsWhenItRefusesBecauseAnotherOperationIsRunning(t *testing.T) {
	fakes := newReconcilerFakes()
	blockedState(fakes)
	r := fakes.reconciler(baseOpts())
	if !r.opLock.TryLock() {
		t.Fatal("could not seize opLock for the test")
	}
	defer r.opLock.Unlock()

	err := r.RetryBlocked("integration:workday_main")

	if err == nil || !strings.Contains(err.Error(), "another operation is already running") {
		t.Errorf("err = %v, want the busy refusal", err)
	}
	if len(fakes.applier.stateSaveCalls) != 0 {
		t.Errorf("state_save_calls = %+v, want none while busy", fakes.applier.stateSaveCalls)
	}
	if events := r.Status().Events; !hasEventContaining(events, "retry skipped: another operation is already running") {
		t.Errorf("events = %+v, want a skipped-retry entry", events)
	}
}

// The key is whatever was posted, and only the two layers with a failure
// memory have anything to clear.
func TestRetryBlockedRejectsAKeyThatNamesNoLayer(t *testing.T) {
	fakes := newReconcilerFakes()
	blockedState(fakes)
	r := fakes.reconciler(baseOpts())

	for _, key := range []string{"", "workday_main", "addon:core_ssh", "entity:light.kitchen"} {
		err := r.RetryBlocked(key)
		if err == nil {
			t.Errorf("RetryBlocked(%q) = nil, want a refusal", key)
		}
	}
	if len(fakes.applier.stateSaveCalls) != 0 {
		t.Errorf("state_save_calls = %+v, want none for a key that names no layer", fakes.applier.stateSaveCalls)
	}
	// The page comes back identical either way, so the refusal must log.
	if events := r.Status().Events; !hasEventContaining(events, "retry skipped: cannot retry") {
		t.Errorf("events = %+v, want a skipped-retry entry per refusal", events)
	}
}

// Routine, not exceptional: a cycle between the render and the press can
// have cleared the entry the button was drawn from.
func TestRetryBlockedRejectsAnItemThatIsNotRecorded(t *testing.T) {
	fakes := newReconcilerFakes()
	blockedState(fakes)
	r := fakes.reconciler(baseOpts())

	err := r.RetryBlocked("integration:never_declared")

	if err == nil || !strings.Contains(err.Error(), "nothing to retry") {
		t.Errorf("err = %v, want the not-recorded refusal", err)
	}
	if len(fakes.applier.stateSaveCalls) != 0 {
		t.Errorf("state_save_calls = %+v, want none for an item that is not recorded", fakes.applier.stateSaveCalls)
	}
	if events := r.Status().Events; !hasEventContaining(events, "retry skipped: nothing to retry") {
		t.Errorf("events = %+v, want a skipped-retry entry", events)
	}
}

// The refusal path refreshes the mirror off what it just read, or a row
// for a record already gone from disk survives every press.
func TestRetryBlockedClearsAPhantomRowItCannotFind(t *testing.T) {
	fakes := newReconcilerFakes()
	blockedState(fakes)
	r := fakes.reconciler(baseOpts())
	if got := r.Status().Blocked; len(got) != 2 {
		t.Fatalf("blocked = %+v, want the two records at startup", got)
	}
	// Cleared underneath the mirror, the way a successful apply does.
	fakes.applier.state.IntegrationAttempts = map[string]map[string]any{}
	fakes.applier.state.SubentryAttempts = map[string]map[string]any{}

	if err := r.RetryBlocked("integration:workday_main"); err == nil {
		t.Fatal("RetryBlocked = nil, want the not-recorded refusal")
	}

	if got := r.Status().Blocked; len(got) != 0 {
		t.Errorf("blocked = %+v, want the phantom rows gone", got)
	}
}

// A failed save leaves the record on disk, so the row comes back and the
// feed has to say why.
func TestRetryBlockedLogsWhenTheStateCannotBeSaved(t *testing.T) {
	fakes := newReconcilerFakes()
	blockedState(fakes)
	fakes.applier.stateSaveErr = errors.New("read-only file system")
	r := fakes.reconciler(baseOpts())

	err := r.RetryBlocked("integration:workday_main")

	if err == nil || !strings.Contains(err.Error(), "read-only file system") {
		t.Errorf("err = %v, want the save failure", err)
	}
	if events := r.Status().Events; !hasEventContaining(events, "retry failed for integration:workday_main: read-only file system") {
		t.Errorf("events = %+v, want the failed-retry entry", events)
	}
}

// --- Status.PendingRestartSlugs -------------------------------------------

// restartingAddonPlan plans one add-on op per shape the restart warning
// cares about: executable with and without the flag, plus an error op.
func restartingAddonPlan(t *testing.T, fakes *reconcilerFakes) *Reconciler {
	t.Helper()
	fakes.addonOpts.desired = addonopts.Desired{Addons: []map[string]any{
		{"slug": "core_configurator", "restart_on_change": true, "options": map[string]any{}},
		{"slug": "core_ssh", "restart_on_change": false, "options": map[string]any{}},
		{"slug": "core_letsencrypt", "restart_on_change": true, "options": map[string]any{}},
	}}
	fakes.addonOpts.planOps = []registries.RegOp{
		{Kind: registries.KindUpdate, RType: "addon", Key: "core_configurator", DiffText: "x"},
		{Kind: registries.KindUpdate, RType: "addon", Key: "core_ssh", DiffText: "y"},
		{Kind: registries.KindError, RType: "addon", Key: "core_letsencrypt", Error: "add-on not installed"},
	}
	opts := baseOpts()
	opts.ReconcileAddonOptions = true
	r := fakes.reconciler(opts)
	r.ReconcileNow(context.Background())
	return r
}

func TestPendingRestartSlugsNamesOnlyExecutableOpsThatDeclareARestart(t *testing.T) {
	fakes := newReconcilerFakes()
	r := restartingAddonPlan(t, fakes)

	got := r.Status().PendingRestartSlugs

	// core_ssh declares no restart; core_letsencrypt declares one but is an
	// error op, which no apply ever executes.
	if want := []string{"core_configurator"}; !reflect.DeepEqual(got, want) {
		t.Errorf("pending_restart_slugs = %v, want %v", got, want)
	}
}

// Un-managing restarts too, off the recorded intent: the manifest entry is
// gone by then (see regapply's executeAddonOp).
func TestPendingRestartSlugsNamesARestoreFromTheRecordedIntent(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.applier.state.AddonRestartOnChange = map[string]bool{
		"addon:core_ssh":   true,
		"addon:core_samba": false,
	}
	fakes.addonOpts.planOps = []registries.RegOp{
		{Kind: addonopts.KindRestore, RType: "addon", Key: "core_ssh", DiffText: "x"},
		{Kind: addonopts.KindRestore, RType: "addon", Key: "core_samba", DiffText: "y"},
	}
	// Declared is empty on purpose: reading the intent from there would
	// name neither slug.
	fakes.applier.state.AddonOriginals = map[string]map[string]any{
		"addon:core_ssh": {"authorized_keys": []any{}}, "addon:core_samba": {"logins": []any{}},
	}
	opts := baseOpts()
	opts.ReconcileAddonOptions = true
	r := fakes.reconciler(opts)

	r.ReconcileNow(context.Background())

	if want := []string{"core_ssh"}; !reflect.DeepEqual(r.Status().PendingRestartSlugs, want) {
		t.Errorf("pending_restart_slugs = %v, want %v", r.Status().PendingRestartSlugs, want)
	}
}

func TestPendingRestartSlugsIsEmptyWithoutAnAddonPlan(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.subentries.desired = subentries.Desired{Subentries: []map[string]any{declaredSubentry("kitchen", "pushward")}}
	fakes.subentries.planOps = []registries.RegOp{
		{Kind: subentries.KindCreate, RType: "subentry", Key: "kitchen", DiffText: "+k"},
	}
	opts := baseOpts()
	opts.ReconcileSubentries = true
	r := fakes.reconciler(opts)

	r.ReconcileNow(context.Background())

	if got := r.Status().PendingRestartSlugs; len(got) != 0 {
		t.Errorf("pending_restart_slugs = %v, want none - no add-on op is pending", got)
	}
}

// --- standing health flags ------------------------------------------------

// Each is logged once on failure and once on recovery, so a few hundred
// events later only Status still shows the condition.
func TestStatusExposesTheStandingHealthFlags(t *testing.T) {
	fakes := newReconcilerFakes()
	r := fakes.reconciler(baseOpts())
	r.withMu(func() {
		r.historyWriteFailed = true
		r.versionRecordFailed = true
		r.addonUpdateSelfSlugFailed = true
		r.addonCheckFailed = map[string]bool{"core_samba": true, "a0d7b954_esphome": true}
	})

	status := r.Status()

	if !status.HistoryWriteFailing || !status.VersionRecordFailing || !status.AddonUpdateSelfSlugFailing {
		t.Errorf("status = %+v, want the three boolean flags raised", status)
	}
	// Sorted, like every map-derived list: the fragment is compared bytewise.
	if want := []string{"a0d7b954_esphome", "core_samba"}; !reflect.DeepEqual(status.AddonCheckFailing, want) {
		t.Errorf("addon_check_failing = %v, want %v", status.AddonCheckFailing, want)
	}
	if !status.HasHealthWarnings() {
		t.Error("has_health_warnings = false with four flags raised")
	}
}

func TestStatusReportsNoHealthWarningsOnAHealthyAgent(t *testing.T) {
	fakes := newReconcilerFakes()

	status := fakes.reconciler(baseOpts()).Status()

	if status.HasHealthWarnings() {
		t.Errorf("status = %+v, want no health warnings", status)
	}
	if len(status.AddonCheckFailing) != 0 {
		t.Errorf("addon_check_failing = %v, want none", status.AddonCheckFailing)
	}
}
