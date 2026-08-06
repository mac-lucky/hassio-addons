package recon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/options"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/regapply"
)

// testSelfSlug is the agent's own slug in the shape Supervisor uses:
// repository hash, underscore, add-on slug.
const testSelfSlug = "a0d7b954_ha_gitops_agent"

func newAddonUpdateFakes() *reconcilerFakes {
	f := newReconcilerFakes()
	f.registryApplier.fetchSelfAddonSlugResult = testSelfSlug
	f.registryApplier.addonUpdateInfo = map[string]regapply.AddonUpdateInfo{}
	f.registryApplier.addonUpdateInfoErr = map[string]error{}
	f.registryApplier.updateAddonErr = map[string]error{}
	return f
}

// installedAddon registers one add-on with the fake Supervisor. Deriving
// update_available from the versions is a shorthand only this fake takes.
func installedAddon(f *reconcilerFakes, slug, name, version, latest string) {
	f.registryApplier.addonUpdateInfo[slug] = regapply.AddonUpdateInfo{
		Slug:            slug,
		Name:            name,
		Version:         version,
		VersionLatest:   latest,
		UpdateAvailable: version != latest,
	}
}

// autoUpdateOpts watches slugs with dry_run off, the configuration in
// which the loop installs things.
func autoUpdateOpts(slugs ...string) options.Options {
	opts := baseOpts()
	opts.DryRun = false
	opts.AutoUpdateAddons = slugs
	return opts
}

func dryRunAutoUpdateOpts(slugs ...string) options.Options {
	opts := autoUpdateOpts(slugs...)
	opts.DryRun = true
	return opts
}

func countEventsContaining(events []Event, sub string) int {
	n := 0
	for _, e := range events {
		if strings.Contains(e.Message, sub) {
			n++
		}
	}
	return n
}

// addonUpdateRow returns the row for slug, failing if there is none: every
// configured slug must be reported.
func addonUpdateRow(t *testing.T, st Status, slug string) AddonUpdateStatus {
	t.Helper()
	for _, u := range st.AddonUpdates {
		if u.Slug == slug {
			return u
		}
	}
	t.Fatalf("no result row for %q; rows = %+v", slug, st.AddonUpdates)
	return AddonUpdateStatus{}
}

// setAddonUpdateTimers shrinks the loop's two package-level timers for one
// test - the reason they are vars.
func setAddonUpdateTimers(t *testing.T, delay, interval time.Duration) {
	t.Helper()
	prevDelay, prevInterval := addonUpdateStartupDelay, addonUpdateCheckInterval
	addonUpdateStartupDelay, addonUpdateCheckInterval = delay, interval
	t.Cleanup(func() {
		addonUpdateStartupDelay, addonUpdateCheckInterval = prevDelay, prevInterval
	})
}

// --- one check cycle ---------------------------------------------------

// Updating the add-on this loop runs inside means Supervisor stopping the
// container mid-call, so the refusal comes before the fetch.
func TestCheckAddonUpdatesRefusesToUpdateItself(t *testing.T) {
	f := newAddonUpdateFakes()
	installedAddon(f, testSelfSlug, "GitOps Agent", "1.0.0", "1.1.0")
	r := f.reconciler(autoUpdateOpts(testSelfSlug))

	r.CheckAddonUpdates(context.Background())

	row := addonUpdateRow(t, r.Status(), testSelfSlug)
	if row.LastResult != "refused: will not update self" {
		t.Errorf("last_result = %q, want the self refusal", row.LastResult)
	}
	if calls := f.registryApplier.fetchAddonUpdateCalls; len(calls) != 0 {
		t.Errorf("the agent's own slug was fetched (%v); the refusal must come before any fetch", calls)
	}
	if calls := f.registryApplier.updateAddonCalls; len(calls) != 0 {
		t.Fatalf("the agent updated ITSELF (%v)", calls)
	}
}

func TestCheckAddonUpdatesDryRunRecordsWithoutInstalling(t *testing.T) {
	f := newAddonUpdateFakes()
	installedAddon(f, "esphome", "ESPHome Device Builder", "2025.6.0", "2025.7.1")
	r := f.reconciler(dryRunAutoUpdateOpts("esphome"))

	r.CheckAddonUpdates(context.Background())

	if calls := f.registryApplier.updateAddonCalls; len(calls) != 0 {
		t.Fatalf("dry_run installed an update (%v)", calls)
	}
	row := addonUpdateRow(t, r.Status(), "esphome")
	if row.LastResult != "update available (dry run, not installing)" {
		t.Errorf("last_result = %q, want the dry-run verdict", row.LastResult)
	}
	if !row.UpdateAvailable || row.Version != "2025.6.0" || row.LatestVersion != "2025.7.1" {
		t.Errorf("row = %+v, want the availability recorded with both versions", row)
	}
	if row.Name != "ESPHome Device Builder" {
		t.Errorf("name = %q, want the display name", row.Name)
	}
	if row.LastCheckedUTC == "" {
		t.Error("last_checked_utc is empty")
	}
	if row.LastUpdatedUTC != "" {
		t.Errorf("last_updated_utc = %q, want empty: nothing was installed", row.LastUpdatedUTC)
	}
}

// An uninstalled update stays available forever, so the dry-run branch is
// reached on every check: saying so every six hours would fill the log.
func TestCheckAddonUpdatesDryRunEventRepeatsOnlyWhenTheVersionMoves(t *testing.T) {
	f := newAddonUpdateFakes()
	installedAddon(f, "esphome", "ESPHome Device Builder", "2025.6.0", "2025.7.1")
	r := f.reconciler(dryRunAutoUpdateOpts("esphome"))

	r.CheckAddonUpdates(context.Background())
	r.CheckAddonUpdates(context.Background())

	const line = "dry run: add-on esphome update available"
	if n := countEventsContaining(r.Status().Events, line); n != 1 {
		t.Errorf("logged the same availability %d times, want 1", n)
	}
	if !hasEventContaining(r.Status().Events, "(2025.6.0 -> 2025.7.1), not installing") {
		t.Errorf("events = %+v, want both versions in the line", r.Status().Events)
	}

	// A newer release is news again.
	installedAddon(f, "esphome", "ESPHome Device Builder", "2025.6.0", "2025.8.0")
	r.CheckAddonUpdates(context.Background())

	if n := countEventsContaining(r.Status().Events, line); n != 2 {
		t.Errorf("availability events = %d, want 2: a newer version is worth saying again", n)
	}
}

func TestCheckAddonUpdatesInstallsAndConfirmsTheUpdate(t *testing.T) {
	f := newAddonUpdateFakes()
	installedAddon(f, "esphome", "ESPHome Device Builder", "2025.6.0", "2025.7.1")
	r := f.reconciler(autoUpdateOpts("esphome"))

	r.CheckAddonUpdates(context.Background())

	if got := f.registryApplier.updateAddonCalls; len(got) != 1 || got[0] != "esphome" {
		t.Fatalf("update_addon_calls = %v, want [esphome]", got)
	}
	// Fetched twice: once to decide, once to confirm what actually landed.
	if got := f.registryApplier.fetchAddonUpdateCalls; len(got) != 2 {
		t.Errorf("fetch calls = %v, want two (decide, then confirm)", got)
	}

	st := r.Status()
	row := addonUpdateRow(t, st, "esphome")
	if row.LastResult != "updated to 2025.7.1" {
		t.Errorf("last_result = %q, want the installed version", row.LastResult)
	}
	if row.Version != "2025.7.1" || row.UpdateAvailable {
		t.Errorf("row = %+v, want the confirming re-read's own numbers", row)
	}
	if row.LastUpdatedUTC == "" {
		t.Error("last_updated_utc is empty after a successful update")
	}
	if !eventLogged(st, "updating add-on esphome 2025.6.0 -> 2025.7.1 (with backup)") {
		t.Errorf("events = %+v, want the pre-update line naming the backup", st.Events)
	}
	if !eventLogged(st, "add-on esphome updated to 2025.7.1") {
		t.Errorf("events = %+v, want the completion line", st.Events)
	}
}

// The update must outlive a cancelled parent (as tick detaches before
// ApplyNow), or a restart mid-pull leaves the add-on between two versions.
func TestCheckAddonUpdatesDetachesTheUpdateFromACancelledContext(t *testing.T) {
	f := newAddonUpdateFakes()
	installedAddon(f, "esphome", "ESPHome Device Builder", "2025.6.0", "2025.7.1")
	r := f.reconciler(autoUpdateOpts("esphome"))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	r.CheckAddonUpdates(ctx)

	if len(f.registryApplier.updateAddonCtxs) != 1 {
		t.Fatalf("update calls = %d, want 1", len(f.registryApplier.updateAddonCtxs))
	}
	if err := f.registryApplier.updateAddonCtxs[0].Err(); err != nil {
		t.Errorf("UpdateAddon's context.Err() = %v, want nil: the install must be detached from the loop's context", err)
	}
}

// Supervisor can answer 200 to an update that installed nothing, and no
// later cycle corrects the activity log or last_addon_update.
func TestCheckAddonUpdatesReportsAnUpdateThatDidNotTake(t *testing.T) {
	f := newAddonUpdateFakes()
	installedAddon(f, "esphome", "ESPHome Device Builder", "2025.6.0", "2025.7.1")
	// Frozen: every read, the confirming one included, reports the version
	// the add-on started on.
	frozen := f.registryApplier.addonUpdateInfo["esphome"]
	f.registryApplier.onFetchAddonUpdateInfo = func(f *fakeRegistryApplier, slug string) {
		f.addonUpdateInfo[slug] = frozen
	}
	r := f.reconciler(autoUpdateOpts("esphome"))

	r.CheckAddonUpdates(context.Background())

	if got := f.registryApplier.updateAddonCalls; len(got) != 1 {
		t.Fatalf("update_addon_calls = %v, want the update to have been attempted", got)
	}

	st := r.Status()
	row := addonUpdateRow(t, st, "esphome")
	if row.LastResult != "update did not take: still on 2025.6.0" {
		t.Errorf("last_result = %q, want the did-not-take verdict", row.LastResult)
	}
	if row.LastUpdatedUTC != "" {
		t.Errorf("last_updated_utc = %q, want empty: nothing was installed", row.LastUpdatedUTC)
	}
	if !row.UpdateAvailable {
		t.Error("update_available = false; the add-on is still behind and the next cycle must retry")
	}
	if eventLogged(st, "add-on esphome updated to") {
		t.Errorf("events = %+v, want no completion line for an update that did not take", st.Events)
	}
	if got := f.status.pushes[len(f.status.pushes)-1].attrs["last_addon_update"]; got != nil {
		t.Errorf("last_addon_update = %v, want nil: nothing was installed", got)
	}
}

// Supervisor already reported the update finished, so only the
// confirmation is missing: the row falls back to the version asked for.
func TestCheckAddonUpdatesFallsBackWhenTheConfirmingReadFails(t *testing.T) {
	f := newAddonUpdateFakes()
	installedAddon(f, "esphome", "ESPHome Device Builder", "2025.6.0", "2025.7.1")
	f.registryApplier.onFetchAddonUpdateInfo = func(f *fakeRegistryApplier, slug string) {
		if len(f.fetchAddonUpdateCalls) > 1 {
			f.addonUpdateInfoErr[slug] = errors.New("add-on esphome: info request failed: connection reset by peer")
		}
	}
	r := f.reconciler(autoUpdateOpts("esphome"))

	r.CheckAddonUpdates(context.Background())

	st := r.Status()
	row := addonUpdateRow(t, st, "esphome")
	if row.LastResult != "updated to 2025.7.1" {
		t.Errorf("last_result = %q, want the update still reported as done", row.LastResult)
	}
	if row.Version != "2025.7.1" || row.UpdateAvailable {
		t.Errorf("row = %+v, want it fallen back to the version that was asked for", row)
	}
	if row.LastUpdatedUTC == "" {
		t.Error("last_updated_utc is empty; the update itself succeeded")
	}
	// Nothing actionable happened, so it belongs in slog.Warn only.
	if eventLogged(st, "connection reset by peer") {
		t.Errorf("events = %+v, want the unread confirmation kept out of the activity log", st.Events)
	}
	if st.State == StateError || st.LastError != "" {
		t.Errorf("state = %q / last_error = %q, want both untouched", st.State, st.LastError)
	}
}

// The sync state answers "does /homeassistant match the repository", and
// an add-on version is not in the repository at all.
func TestCheckAddonUpdatesFailureNeverTouchesSyncState(t *testing.T) {
	f := newAddonUpdateFakes()
	installedAddon(f, "esphome", "ESPHome Device Builder", "2025.6.0", "2025.7.1")
	installedAddon(f, "core_configurator", "File editor", "5.9.0", "5.9.0")
	f.registryApplier.updateAddonErr["esphome"] = errors.New(
		"add-on esphome: update returned HTTP 500 (Failed to pull image)")
	r := f.reconciler(autoUpdateOpts("esphome", "core_configurator"))
	before := r.Status().State

	r.CheckAddonUpdates(context.Background())

	st := r.Status()
	if st.State != before {
		t.Errorf("state = %q, want it unchanged at %q after an add-on update failure", st.State, before)
	}
	if st.State == StateError {
		t.Error("an add-on update failure flipped the sync state to error")
	}
	if st.LastError != "" {
		t.Errorf("last_error = %q, want empty: an add-on update failure is not a sync failure", st.LastError)
	}

	row := addonUpdateRow(t, st, "esphome")
	if !strings.HasPrefix(row.LastResult, "update failed: ") || !strings.Contains(row.LastResult, "Failed to pull image") {
		t.Errorf("last_result = %q, want the failure with Supervisor's own detail", row.LastResult)
	}
	if !eventLogged(st, "add-on update failed: esphome: ") {
		t.Errorf("events = %+v, want the failure named in the activity log", st.Events)
	}

	// One add-on failing must not strand the others behind it.
	if got := addonUpdateRow(t, st, "core_configurator").LastResult; got != "up to date" {
		t.Errorf("core_configurator last_result = %q, want it checked despite the earlier failure", got)
	}
}

func TestCheckAddonUpdatesReportsNotInstalledSlugs(t *testing.T) {
	f := newAddonUpdateFakes()
	r := f.reconciler(autoUpdateOpts("typo_addon"))

	r.CheckAddonUpdates(context.Background())

	row := addonUpdateRow(t, r.Status(), "typo_addon")
	if row.LastResult != "not installed" {
		t.Errorf("last_result = %q, want 'not installed' rather than an HTTP failure", row.LastResult)
	}
	if calls := f.registryApplier.updateAddonCalls; len(calls) != 0 {
		t.Errorf("update_addon_calls = %v, want none for a slug that is not installed", calls)
	}
}

func TestCheckAddonUpdatesReportsCheckFailures(t *testing.T) {
	f := newAddonUpdateFakes()
	f.registryApplier.addonUpdateInfoErr["esphome"] = errors.New(
		"add-on esphome: info returned HTTP 502 (Bad Gateway)")
	r := f.reconciler(autoUpdateOpts("esphome"))

	r.CheckAddonUpdates(context.Background())

	st := r.Status()
	row := addonUpdateRow(t, st, "esphome")
	if !strings.HasPrefix(row.LastResult, "check failed: ") || !strings.Contains(row.LastResult, "HTTP 502") {
		t.Errorf("last_result = %q, want the check failure with its detail", row.LastResult)
	}
	if row.UpdateAvailable {
		t.Error("a check that got no answer still claims an update is available")
	}
	if st.State == StateError || st.LastError != "" {
		t.Errorf("state = %q / last_error = %q, want a check failure kept out of the sync state", st.State, st.LastError)
	}
}

// A check failure reaches the activity log, but on the transition only: an
// unreachable Supervisor persists for days at one line every 6 hours.
func TestCheckAddonUpdatesLogsACheckFailureOncePerFailureRun(t *testing.T) {
	f := newAddonUpdateFakes()
	installedAddon(f, "esphome", "ESPHome Device Builder", "2025.7.1", "2025.7.1")
	f.registryApplier.addonUpdateInfoErr["esphome"] = errors.New(
		"add-on esphome: info returned HTTP 502 (Bad Gateway)")
	r := f.reconciler(autoUpdateOpts("esphome"))

	r.CheckAddonUpdates(context.Background())
	r.CheckAddonUpdates(context.Background())

	const failed = "add-on update check failed: esphome"
	if n := countEventsContaining(r.Status().Events, failed); n != 1 {
		t.Errorf("logged the same check failure %d times, want 1", n)
	}
	if !hasEventContaining(r.Status().Events, "HTTP 502") {
		t.Errorf("events = %+v, want the reason in the line", r.Status().Events)
	}

	// Recovery is news, and re-arms the guard: the next failure is a new
	// run, not a continuation.
	delete(f.registryApplier.addonUpdateInfoErr, "esphome")
	r.CheckAddonUpdates(context.Background())

	if n := countEventsContaining(r.Status().Events, "add-on update check recovered: esphome"); n != 1 {
		t.Errorf("recovery events = %d, want 1", n)
	}

	f.registryApplier.addonUpdateInfoErr["esphome"] = errors.New(
		"add-on esphome: info returned HTTP 502 (Bad Gateway)")
	r.CheckAddonUpdates(context.Background())

	if n := countEventsContaining(r.Status().Events, failed); n != 2 {
		t.Errorf("check failure events = %d, want 2: a fresh outage is worth saying again", n)
	}
}

// The guard is per slug, not per cycle.
func TestCheckAddonUpdatesLogsEachFailingSlugSeparately(t *testing.T) {
	f := newAddonUpdateFakes()
	f.registryApplier.addonUpdateInfoErr["esphome"] = errors.New("info returned HTTP 502 (Bad Gateway)")
	f.registryApplier.addonUpdateInfoErr["core_samba"] = errors.New("info request failed: connection reset by peer")
	r := f.reconciler(autoUpdateOpts("esphome", "core_samba"))

	r.CheckAddonUpdates(context.Background())

	events := r.Status().Events
	if n := countEventsContaining(events, "add-on update check failed: esphome"); n != 1 {
		t.Errorf("esphome failure events = %d, want 1", n)
	}
	if n := countEventsContaining(events, "add-on update check failed: core_samba"); n != 1 {
		t.Errorf("core_samba failure events = %d, want 1", n)
	}
}

// "not installed" is a complete answer, not a failure: a typo'd slug never
// stops being a typo, so it never logs and announces no recovery either.
func TestCheckAddonUpdatesNeverLogsANotInstalledSlug(t *testing.T) {
	f := newAddonUpdateFakes()
	f.registryApplier.addonUpdateInfoErr["typo_addon"] = fmt.Errorf(
		"add-on typo_addon: %w", regapply.ErrAddonNotInstalled)
	r := f.reconciler(autoUpdateOpts("typo_addon"))

	r.CheckAddonUpdates(context.Background())
	r.CheckAddonUpdates(context.Background())

	events := r.Status().Events
	if n := countEventsContaining(events, "typo_addon"); n != 0 {
		t.Errorf("logged a not-installed slug %d times, want 0; events = %+v", n, events)
	}
	if row := addonUpdateRow(t, r.Status(), "typo_addon"); row.LastResult != "not installed" {
		t.Errorf("last_result = %q, want the row to carry it instead", row.LastResult)
	}
}

// A check that got no answer has no verdict to repeat, while the display
// name and "this agent updated it at T" stay true.
func TestCheckAddonUpdatesCarriesForwardOnlyTheFactsAFailedCheckCannotChange(t *testing.T) {
	f := newAddonUpdateFakes()
	installedAddon(f, "esphome", "ESPHome Device Builder", "2025.6.0", "2025.7.1")
	r := f.reconciler(autoUpdateOpts("esphome"))

	r.CheckAddonUpdates(context.Background())
	updated := addonUpdateRow(t, r.Status(), "esphome")
	if updated.LastUpdatedUTC == "" {
		t.Fatalf("setup: first cycle did not update the add-on; row = %+v", updated)
	}

	f.registryApplier.addonUpdateInfoErr["esphome"] = errors.New(
		"add-on esphome: info returned HTTP 502 (Bad Gateway)")
	r.CheckAddonUpdates(context.Background())

	row := addonUpdateRow(t, r.Status(), "esphome")
	if row.Name != "ESPHome Device Builder" {
		t.Errorf("name = %q, want the last one seen kept rather than falling back to a raw slug", row.Name)
	}
	if row.LastUpdatedUTC != updated.LastUpdatedUTC {
		t.Errorf("last_updated_utc = %q, want %q: a failed check cannot un-do an update this agent made",
			row.LastUpdatedUTC, updated.LastUpdatedUTC)
	}
	if row.Version != "" || row.LatestVersion != "" || row.UpdateAvailable {
		t.Errorf("row = %+v, want Supervisor's verdict cleared: this check got no answer", row)
	}
}

// An update takes opLock, so one already in flight defers it rather than
// queueing behind a 30-minute image pull.
func TestCheckAddonUpdatesDefersWhileAnotherOperationRuns(t *testing.T) {
	f := newAddonUpdateFakes()
	installedAddon(f, "esphome", "ESPHome Device Builder", "2025.6.0", "2025.7.1")
	r := f.reconciler(autoUpdateOpts("esphome"))

	if !r.opLock.TryLock() {
		t.Fatal("could not seize opLock for the test")
	}
	defer r.opLock.Unlock()

	r.CheckAddonUpdates(context.Background())

	row := addonUpdateRow(t, r.Status(), "esphome")
	if row.LastResult != "update available, deferred: another operation is running" {
		t.Errorf("last_result = %q, want the deferral", row.LastResult)
	}
	if calls := f.registryApplier.updateAddonCalls; len(calls) != 0 {
		t.Fatalf("update ran while another operation held opLock (%v)", calls)
	}
	if !row.UpdateAvailable {
		t.Error("update_available = false; a deferred update is still available")
	}
}

// Without a confirmed self-slug the refusal above cannot be enforced, so
// the cycle does nothing rather than work from an unverified list.
func TestCheckAddonUpdatesRecordsNothingWhenTheSelfSlugIsUnknown(t *testing.T) {
	f := newAddonUpdateFakes()
	installedAddon(f, "esphome", "ESPHome Device Builder", "2025.6.0", "2025.7.1")
	f.registryApplier.fetchSelfAddonSlugErr = errors.New("supervisor unreachable")
	r := f.reconciler(autoUpdateOpts("esphome"))

	r.CheckAddonUpdates(context.Background())
	r.CheckAddonUpdates(context.Background())

	st := r.Status()
	if len(st.AddonUpdates) != 0 {
		t.Errorf("addon_updates = %+v, want nothing recorded from an unverified list", st.AddonUpdates)
	}
	if calls := f.registryApplier.fetchAddonUpdateCalls; len(calls) != 0 {
		t.Errorf("fetched %v without a confirmed self-slug", calls)
	}
	if calls := f.registryApplier.updateAddonCalls; len(calls) != 0 {
		t.Fatalf("updated %v without a confirmed self-slug", calls)
	}
	// Said once, not once per check.
	if n := countEventsContaining(st.Events, "cannot confirm this agent's own slug"); n != 1 {
		t.Errorf("logged the self-slug failure %d times over two checks, want 1", n)
	}
}

// Each check rewrites the whole row set, so two overlapping would discard
// one and could install the same update twice.
func TestCheckAddonUpdatesRefusesToRunTwiceAtOnce(t *testing.T) {
	f := newAddonUpdateFakes()
	installedAddon(f, "esphome", "ESPHome Device Builder", "2025.6.0", "2025.7.1")
	r := f.reconciler(autoUpdateOpts("esphome"))

	// A first check that recorded its results and is still in flight.
	r.CheckAddonUpdates(context.Background())
	if !r.checkLock.TryLock() {
		t.Fatal("could not seize checkLock for the test")
	}
	defer r.checkLock.Unlock()
	before := addonUpdateRow(t, r.Status(), "esphome")
	fetchesBefore := len(f.registryApplier.fetchAddonUpdateCalls)

	r.CheckAddonUpdates(context.Background())

	if got := len(f.registryApplier.fetchAddonUpdateCalls); got != fetchesBefore {
		t.Errorf("fetch calls went %d -> %d; the second check must not run at all", fetchesBefore, got)
	}
	if got := len(f.registryApplier.updateAddonCalls); got != 1 {
		t.Fatalf("update_addon_calls = %d, want the update NOT repeated by the overlapping check", got)
	}
	if got := addonUpdateRow(t, r.Status(), "esphome"); got != before {
		t.Errorf("row = %+v, want the in-flight check's results left alone (%+v)", got, before)
	}
	if !hasEventContaining(r.Status().Events, "add-on update check skipped: an add-on update check is already running") {
		t.Errorf("events = %+v, want the refusal to leave a trace", r.Status().Events)
	}
}

func TestCheckAddonUpdatesLogsWhenNoAddonsAreConfigured(t *testing.T) {
	f := newAddonUpdateFakes()
	r := f.reconciler(baseOpts())

	r.CheckAddonUpdates(context.Background())

	if !hasEventContaining(r.Status().Events, "add-on update check skipped: auto_update_addons is empty") {
		t.Errorf("events = %+v, want the refusal to leave a trace", r.Status().Events)
	}
	if f.registryApplier.fetchSelfAddonSlugCalls != 0 {
		t.Error("resolved the self-slug with nothing to check")
	}
}

// --- status ------------------------------------------------------------

func TestStatusCarriesAddonUpdatesAsACopy(t *testing.T) {
	f := newAddonUpdateFakes()
	installedAddon(f, "core_configurator", "File editor", "5.9.0", "5.9.0")
	r := f.reconciler(autoUpdateOpts("core_configurator"))

	r.CheckAddonUpdates(context.Background())

	st := r.Status()
	if !st.AutoUpdateEnabled {
		t.Error("auto_update_enabled = false with a non-empty auto_update_addons")
	}
	if len(st.AddonUpdates) != 1 || st.AddonUpdates[0].LastResult != "up to date" {
		t.Fatalf("addon_updates = %+v, want one up-to-date row", st.AddonUpdates)
	}

	st.AddonUpdates[0].LastResult = "tampered"
	st.AddonUpdates[0].Slug = "somethingelse"

	if got := addonUpdateRow(t, r.Status(), "core_configurator").LastResult; got != "up to date" {
		t.Errorf("last_result = %q after a caller wrote to the slice it was handed, want it unaffected", got)
	}
}

// Empty is normal for a process's first two minutes, and a nil slice
// serializes as null while every sibling list emits [].
func TestStatusAddonUpdatesIsEmptyNotNilBeforeTheFirstCheck(t *testing.T) {
	f := newAddonUpdateFakes()
	r := f.reconciler(autoUpdateOpts("esphome"))

	st := r.Status()
	if st.AddonUpdates == nil {
		t.Fatal("addon_updates is nil before the first check; it must marshal as [] , not null")
	}
	encoded, err := json.Marshal(st.AddonUpdates)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if string(encoded) != "[]" {
		t.Errorf("addon_updates serialized as %s, want []", encoded)
	}
}

// The dashboard fold keys on Actionable. A failed check renders like the
// two folded verdicts but must stay visible: the next one may succeed.
func TestOnlyTheTwoVerdictsThatCannotMoveAreFoldedAway(t *testing.T) {
	cases := []struct {
		result string
		want   bool
	}{
		// The two that move only when the configuration does.
		{AddonUpdateRefusedSelf, false},
		{AddonUpdateNotInstalled, false},

		// Everything else, including every shape of "we do not know".
		{AddonUpdateNotCheckedYet, true},
		{"up to date", true},
		{"update available (dry run, not installing)", true},
		{"update available, deferred: another operation is running", true},
		{"updated to 2025.7.1", true},
		{"update failed: 502 Bad Gateway", true},
		{"update did not take: still on 2025.6.0", true},
		{"check failed: connection refused", true},
		{"check failed: " + regapply.ErrAddonNotInstalled.Error(), true},
		// An older binary's row, or a hand-edited file: unknown text is no
		// evidence that nothing can change.
		{"", true},
		{"something no version of this agent ever wrote", true},
	}

	for _, tc := range cases {
		row := AddonUpdateStatus{Slug: "esphome", LastResult: tc.result}
		if got := row.Actionable(); got != tc.want {
			t.Errorf("Actionable() = %v for last_result %q, want %v", got, tc.result, tc.want)
		}
	}
}

// Why AddonCheckRunning exists: Busy is opLock, which the check takes only
// while installing, so a spinner keyed off Busy would show nothing.
func TestAddonCheckRunningIsTrueDuringACheckWhileBusyStaysFalse(t *testing.T) {
	useAddonUpdatesFile(t)
	f := newAddonUpdateFakes()
	installedAddon(f, "esphome", "ESPHome Device Builder", "2025.6.0", "2025.7.1")
	r := f.reconciler(dryRunAutoUpdateOpts("esphome"))

	// Read from inside the fetch, where a real check spends its time.
	// dry_run keeps the run there, so opLock is genuinely untouched.
	var during Status
	f.registryApplier.onFetchAddonUpdateInfo = func(_ *fakeRegistryApplier, _ string) {
		during = r.Status()
	}

	if st := r.Status(); st.AddonCheckRunning {
		t.Fatal("addon_check_running = true before any check ran")
	}

	r.CheckAddonUpdates(context.Background())

	if !during.AddonCheckRunning {
		t.Error("addon_check_running = false during the fetch; the button can never show a spinner")
	}
	if during.Busy {
		t.Error("busy = true during the fetch; the check must not look like an apply to anything gating on opLock")
	}
	if st := r.Status(); st.AddonCheckRunning {
		t.Error("addon_check_running stayed true after the check finished")
	}
}

// Published so the client can mark a row older than one interval stale.
// Comparing on the server would change the fragment's bytes every render.
func TestAddonCheckIntervalSecondsMirrorsTheLoopsOwnInterval(t *testing.T) {
	f := newAddonUpdateFakes()

	setAddonUpdateTimers(t, time.Minute, 90*time.Minute)
	if got := f.reconciler(dryRunAutoUpdateOpts("esphome")).Status().AddonCheckIntervalSeconds; got != 5400 {
		t.Errorf("addon_check_interval_seconds = %d, want 5400", got)
	}

	// A test-shrunk interval truncates to 0, which means "no stale marker"
	// rather than "everything is stale".
	setAddonUpdateTimers(t, time.Millisecond, 5*time.Millisecond)
	if got := f.reconciler(dryRunAutoUpdateOpts("esphome")).Status().AddonCheckIntervalSeconds; got != 0 {
		t.Errorf("addon_check_interval_seconds = %d, want 0 for a sub-second interval", got)
	}
}

func TestStatusAutoUpdateDisabledWithoutConfiguredAddons(t *testing.T) {
	f := newAddonUpdateFakes()
	r := f.reconciler(baseOpts())

	if st := r.Status(); st.AutoUpdateEnabled {
		t.Error("auto_update_enabled = true with the option empty")
	}
}

func TestCheckAddonUpdatesPushesSensorAttributes(t *testing.T) {
	f := newAddonUpdateFakes()
	installedAddon(f, "esphome", "ESPHome Device Builder", "2025.6.0", "2025.7.1")
	r := f.reconciler(dryRunAutoUpdateOpts("esphome"))

	r.CheckAddonUpdates(context.Background())

	push := f.status.pushes[len(f.status.pushes)-1]
	if got := push.attrs["addon_updates_available"]; got != 1 {
		t.Errorf("addon_updates_available = %v, want 1 while a dry run holds the update back", got)
	}
	if got := push.attrs["last_addon_update"]; got != nil {
		t.Errorf("last_addon_update = %v, want nil (JSON null) before anything is installed", got)
	}

	// Same add-on, now actually installed.
	f2 := newAddonUpdateFakes()
	installedAddon(f2, "esphome", "ESPHome Device Builder", "2025.6.0", "2025.7.1")
	r2 := f2.reconciler(autoUpdateOpts("esphome"))

	r2.CheckAddonUpdates(context.Background())

	push = f2.status.pushes[len(f2.status.pushes)-1]
	if got := push.attrs["addon_updates_available"]; got != 0 {
		t.Errorf("addon_updates_available = %v, want 0 once the update landed", got)
	}
	if got := push.attrs["last_addon_update"]; got != "esphome 2025.7.1" {
		t.Errorf("last_addon_update = %v, want 'esphome 2025.7.1'", got)
	}
}

// --- the loop ----------------------------------------------------------

func TestRunAddonUpdateLoopReturnsImmediatelyWhenDisabled(t *testing.T) {
	setAddonUpdateTimers(t, time.Hour, time.Hour)
	f := newAddonUpdateFakes()
	r := f.reconciler(baseOpts())

	done := make(chan struct{})
	go func() {
		r.RunAddonUpdateLoop(context.Background())
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunAddonUpdateLoop parked on its timer with auto_update_addons empty")
	}
}

func TestRunAddonUpdateLoopWaitsOutTheStartupDelay(t *testing.T) {
	setAddonUpdateTimers(t, time.Hour, time.Millisecond)
	f := newAddonUpdateFakes()
	installedAddon(f, "esphome", "ESPHome Device Builder", "2025.6.0", "2025.7.1")
	r := f.reconciler(dryRunAutoUpdateOpts("esphome"))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		r.RunAddonUpdateLoop(ctx)
		close(done)
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunAddonUpdateLoop did not return promptly after context cancellation")
	}

	// Read after the goroutine exits, so this does not race the loop.
	if calls := f.registryApplier.fetchAddonUpdateCalls; len(calls) != 0 {
		t.Errorf("checked %v before the startup delay elapsed", calls)
	}
}

func TestRunAddonUpdateLoopChecksThenStopsOnContextCancel(t *testing.T) {
	setAddonUpdateTimers(t, time.Millisecond, 5*time.Millisecond)
	f := newAddonUpdateFakes()
	installedAddon(f, "core_configurator", "File editor", "5.9.0", "5.9.0")
	r := f.reconciler(dryRunAutoUpdateOpts("core_configurator"))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		r.RunAddonUpdateLoop(ctx)
		close(done)
	}()

	waitForAddonUpdateRows(t, r)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunAddonUpdateLoop did not return promptly after context cancellation")
	}
}

// A panic costs one cycle, not the loop: nothing else in the process would
// ever notice an add-on update again.
func TestRunAddonUpdateLoopSurvivesAPanickingCycle(t *testing.T) {
	setAddonUpdateTimers(t, time.Millisecond, 5*time.Millisecond)
	f := newAddonUpdateFakes()
	installedAddon(f, "core_configurator", "File editor", "5.9.0", "5.9.0")
	f.registryApplier.onFetchAddonUpdateInfo = func(f *fakeRegistryApplier, slug string) {
		if len(f.fetchAddonUpdateCalls) == 1 {
			panic("supervisor client blew up")
		}
	}
	r := f.reconciler(dryRunAutoUpdateOpts("core_configurator"))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		r.RunAddonUpdateLoop(ctx)
		close(done)
	}()

	// Only a completed cycle records rows, so the panicking one cannot
	// satisfy this.
	waitForAddonUpdateRows(t, r)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunAddonUpdateLoop did not return after the panicking cycle")
	}

	if calls := f.registryApplier.fetchAddonUpdateCalls; len(calls) < 2 {
		t.Errorf("fetch calls = %v, want the loop to have gone on to a second cycle", calls)
	}
}

// waitForAddonUpdateRows blocks until a cycle has recorded results, going
// through Status so the loop's own state is never read unsynchronised.
func waitForAddonUpdateRows(t *testing.T, r *Reconciler) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(r.Status().AddonUpdates) > 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("no add-on update check completed within the deadline")
}

func TestAddonCheckIntervalComesFromTheOption(t *testing.T) {
	// The package var shrunk the way the loop tests shrink it, so a test
	// that reads the option cannot pass by accident.
	setAddonUpdateTimers(t, time.Millisecond, 5*time.Millisecond)

	f := newReconcilerFakes()
	opts := baseOpts()
	opts.AutoUpdateAddons = []string{"core_mosquitto"}
	opts.AutoUpdateIntervalMinutes = 60
	r := f.reconciler(opts)

	if got := r.addonCheckInterval(); got != time.Hour {
		t.Errorf("addonCheckInterval = %v, want 1h from the option", got)
	}
	// The card's staleness marker has to follow the option too, or a slower
	// cadence would mark every row stale.
	if got := r.Status().AddonCheckIntervalSeconds; got != 3600 {
		t.Errorf("AddonCheckIntervalSeconds = %d, want 3600", got)
	}
}

// A hand-built Options carries 0, which is what lets setAddonUpdateTimers
// still govern the loop tests.
func TestAddonCheckIntervalFallsBackToTheCompiledDefault(t *testing.T) {
	f := newReconcilerFakes()
	opts := baseOpts()
	opts.AutoUpdateAddons = []string{"core_mosquitto"}
	r := f.reconciler(opts)

	if got := r.addonCheckInterval(); got != addonUpdateCheckInterval {
		t.Errorf("addonCheckInterval = %v, want the package default %v", got, addonUpdateCheckInterval)
	}
}
