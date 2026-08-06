package recon

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// useAddonUpdatesFile points addonUpdatesPath at a fresh temp file for one
// test and puts it back afterwards - the same package-var swap
// usePauseFile does, under the same no-t.Parallel constraint (see its
// comment, which applies verbatim to this one).
//
// Needed by every test that runs a check, not only the ones asserting on
// the file: /data does not exist off the box, so without the swap a check
// persists into a warning nobody reads - and on a box where /data DOES
// exist, one test's rows would hydrate into the next test's reconciler.
//
// The file is deliberately NOT created. Absent is the first-run state, and
// readAddonUpdatesFile has to answer for it.
func useAddonUpdatesFile(t *testing.T) string {
	t.Helper()
	return useAddonUpdatesPath(t, filepath.Join(t.TempDir(), "addon_updates.json"))
}

// useAddonUpdatesPath is the same swap for a path the caller picks, which
// the unwritable-path test needs and nothing else should.
func useAddonUpdatesPath(t *testing.T, path string) string {
	t.Helper()
	previous := addonUpdatesPath
	addonUpdatesPath = path
	t.Cleanup(func() { addonUpdatesPath = previous })
	return path
}

// writeAddonUpdatesJSON puts raw bytes at the swapped path.
//
// Raw JSON rather than a marshalled addonUpdateFile, deliberately. The
// file's keys are AddonUpdateStatus's json tags, which are frozen by disk
// as well as by /status.json (see that type's doc comment); a fixture that
// marshalled the struct would rename itself right along with a renamed tag
// and assert nothing about the format it exists to pin.
func writeAddonUpdatesJSON(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// savedCheckUTC is the check time every hand-written fixture below carries:
// old enough that no clock this test could run on would produce it, so an
// assertion that it survived cannot pass by accident.
const savedCheckUTC = "2020-01-02T03:04:05Z"

// --- restoring across a restart ----------------------------------------

// The card shows nothing for the first two minutes of every process (see
// addonUpdateStartupDelay), so results that did not outlive a restart
// would leave a user who had just restarted the add-on - to fix the very
// thing they were looking at - staring at an empty card.
func TestACompletedCheckIsRestoredByTheNextProcess(t *testing.T) {
	path := useAddonUpdatesFile(t)
	f := newAddonUpdateFakes()
	installedAddon(f, "esphome", "ESPHome Device Builder", "2025.6.0", "2025.7.1")
	r := f.reconciler(dryRunAutoUpdateOpts("esphome"))

	r.CheckAddonUpdates(context.Background())
	recorded := addonUpdateRow(t, r.Status(), "esphome")

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the check recorded rows in memory but wrote no file: %v", err)
	}

	// A second Reconciler over the same path with the same option IS the
	// restart: same /data, same configuration, nothing in memory.
	f2 := newAddonUpdateFakes()
	r2 := f2.reconciler(dryRunAutoUpdateOpts("esphome"))

	if restored := addonUpdateRow(t, r2.Status(), "esphome"); restored != recorded {
		t.Errorf("restored row = %+v, want the recorded one verbatim (%+v)", restored, recorded)
	}
	// Hydration is a file read and nothing else. A start that quietly asked
	// Supervisor would be doing the two-minute delay's job at the worst
	// moment for it.
	if calls := f2.registryApplier.fetchAddonUpdateCalls; len(calls) != 0 {
		t.Errorf("startup fetched %v; hydration must never talk to Supervisor", calls)
	}
}

// The honesty invariant the whole stale-age feature rests on. The
// dashboard renders how OLD each row is, so a restored row whose timestamp
// had been moved to now would be this agent claiming a check it never ran -
// and claiming it in the one place a user goes to find out whether checks
// are still happening at all.
func TestARestoredRowKeepsTheTimeItWasActuallyCheckedAt(t *testing.T) {
	path := useAddonUpdatesFile(t)
	writeAddonUpdatesJSON(t, path, `{"rows":[{"slug":"esphome","name":"ESPHome Device Builder",`+
		`"version":"2025.6.0","latest_version":"2025.7.1","update_available":true,`+
		`"last_result":"update available (dry run, not installing)",`+
		`"last_checked_utc":"`+savedCheckUTC+`","last_updated_utc":"2019-12-31T23:59:00Z"}]}`)

	f := newAddonUpdateFakes()
	installedAddon(f, "esphome", "ESPHome Device Builder", "2025.6.0", "2025.7.1")
	r := f.reconciler(dryRunAutoUpdateOpts("esphome"))

	row := addonUpdateRow(t, r.Status(), "esphome")
	if row.LastCheckedUTC != savedCheckUTC {
		t.Errorf("last_checked_utc = %q, want %q unchanged: nothing may re-stamp a check that did not run",
			row.LastCheckedUTC, savedCheckUTC)
	}
	// Every other field verbatim too, which is also what pins the on-disk
	// key names: a renamed json tag lands here as a zero-valued field.
	want := AddonUpdateStatus{
		Slug:            "esphome",
		Name:            "ESPHome Device Builder",
		Version:         "2025.6.0",
		LatestVersion:   "2025.7.1",
		UpdateAvailable: true,
		LastResult:      "update available (dry run, not installing)",
		LastCheckedUTC:  savedCheckUTC,
		LastUpdatedUTC:  "2019-12-31T23:59:00Z",
	}
	if row != want {
		t.Errorf("restored row = %+v, want %+v", row, want)
	}
	// And the row a restart restores is a real one: it counts on the sensor
	// the moment the process comes up, which is the documented behaviour
	// change (see Status.AddonUpdates).
	if got := r.Status().AddonUpdatesAvailable(); got != 1 {
		t.Errorf("addon_updates_available = %d, want 1: the update really is still waiting", got)
	}
}

// A slug added to the option since the last check has no saved row, and
// gets a placeholder rather than being left out - a watched slug silently
// missing from the list is exactly how a typo'd slug stays invisible.
//
// The order is the OPTION's, not the file's, and that is not cosmetic: the
// dashboard fragment is compared byte for byte to answer 204, so rows that
// came back in the file's order (or a map's) would re-swap the page under
// the reader on every poll.
func TestASlugAddedSinceTheLastCheckIsAPlaceholderInTheOptionsOwnOrder(t *testing.T) {
	path := useAddonUpdatesFile(t)
	// Saved in the opposite order to the one watched below, so a hydration
	// that walked the file could not pass this by luck.
	writeAddonUpdatesJSON(t, path, `{"rows":[`+
		`{"slug":"esphome","last_result":"up to date","last_checked_utc":"`+savedCheckUTC+`"},`+
		`{"slug":"core_configurator","last_result":"up to date","last_checked_utc":"`+savedCheckUTC+`"}]}`)

	f := newAddonUpdateFakes()
	r := f.reconciler(dryRunAutoUpdateOpts("core_configurator", "mosquitto", "esphome"))

	rows := r.Status().AddonUpdates
	gotSlugs := make([]string, len(rows))
	for i, row := range rows {
		gotSlugs[i] = row.Slug
	}
	wantSlugs := []string{"core_configurator", "mosquitto", "esphome"}
	if len(gotSlugs) != len(wantSlugs) {
		t.Fatalf("rows = %v, want one per watched slug in %v", gotSlugs, wantSlugs)
	}
	for i := range wantSlugs {
		if gotSlugs[i] != wantSlugs[i] {
			t.Fatalf("rows = %v, want the option's own order %v", gotSlugs, wantSlugs)
		}
	}

	placeholder := addonUpdateRow(t, r.Status(), "mosquitto")
	if placeholder.LastResult != AddonUpdateNotCheckedYet {
		t.Errorf("last_result = %q, want %q for a slug no check has ever seen",
			placeholder.LastResult, AddonUpdateNotCheckedYet)
	}
	// Empty rather than now: a placeholder has no check to report the age
	// of, and a timestamp here would render as one.
	if placeholder.LastCheckedUTC != "" {
		t.Errorf("last_checked_utc = %q on a placeholder, want empty", placeholder.LastCheckedUTC)
	}
	// The placeholder is not an update, so it must not reach the count the
	// sensor and the card's badge both carry.
	if got := r.Status().AddonUpdatesAvailable(); got != 0 {
		t.Errorf("addon_updates_available = %d, want 0: nothing here is known to be behind", got)
	}
}

// The user took the slug out of auto_update_addons, so nothing is going to
// check it again. Keeping the row would report on an add-on this agent has
// stopped watching, in a card whose whole claim is that it lists what is
// being watched.
func TestASlugNoLongerWatchedIsNotRestored(t *testing.T) {
	path := useAddonUpdatesFile(t)
	writeAddonUpdatesJSON(t, path, `{"rows":[`+
		`{"slug":"esphome","last_result":"up to date","last_checked_utc":"`+savedCheckUTC+`"},`+
		`{"slug":"core_deconz","last_result":"update available","update_available":true,`+
		`"last_checked_utc":"`+savedCheckUTC+`"}]}`)

	f := newAddonUpdateFakes()
	r := f.reconciler(dryRunAutoUpdateOpts("esphome"))

	rows := r.Status().AddonUpdates
	if len(rows) != 1 || rows[0].Slug != "esphome" {
		t.Fatalf("rows = %+v, want only the still-watched slug", rows)
	}
	// The dropped row was an available update, so a hydration that kept it
	// would also inflate the sensor's count for an add-on nobody watches.
	if got := r.Status().AddonUpdatesAvailable(); got != 0 {
		t.Errorf("addon_updates_available = %d, want 0", got)
	}
}

// --- degradation -------------------------------------------------------

// Read never errors, matching applier.StateLoad: a missing, torn or
// hand-edited file costs one blank card and nothing else. Never a panic,
// never a refusal to start, and never a half-decoded row - which would
// render as an add-on with no name and no verdict, the one outcome worse
// than an empty card.
func TestADamagedAddonUpdatesFileRestoresNothingAndStillChecks(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		write bool
	}{
		{name: "missing entirely"},
		{name: "empty file", body: "", write: true},
		{name: "truncated mid-row", body: `{"rows":[{"slug":"esphome","last_res`, write: true},
		{name: "not JSON at all", body: "hand-edited by somebody with a text editor\n", write: true},
		{name: "rows is null", body: `{"rows":null}`, write: true},
		{name: "rows is a string", body: `{"rows":"esphome"}`, write: true},
		{name: "rows is an object", body: `{"rows":{"esphome":{"slug":"esphome"}}}`, write: true},
		{name: "rows holds numbers", body: `{"rows":[1,2,3]}`, write: true},
		{name: "a row is a string", body: `{"rows":["esphome"]}`, write: true},
		{name: "a row field has the wrong type", body: `{"rows":[{"slug":"esphome","update_available":"yes"}]}`, write: true},
		{name: "top level is an array", body: `[{"slug":"esphome"}]`, write: true},
		{name: "top level is a number", body: `42`, write: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := useAddonUpdatesFile(t)
			if tc.write {
				writeAddonUpdatesJSON(t, path, tc.body)
			}

			f := newAddonUpdateFakes()
			installedAddon(f, "esphome", "ESPHome Device Builder", "2025.6.0", "2025.7.1")
			r := f.reconciler(dryRunAutoUpdateOpts("esphome"))

			if rows := r.Status().AddonUpdates; len(rows) != 0 {
				t.Fatalf("rows = %+v, want nothing restored from a damaged file", rows)
			}

			// And the damage is not sticky: the next check writes over it,
			// so one bad file costs one card, not the feature.
			r.CheckAddonUpdates(context.Background())
			if row := addonUpdateRow(t, r.Status(), "esphome"); row.LastResult == "" {
				t.Errorf("row = %+v, want a real verdict after a check", row)
			}
			if saved := readAddonUpdatesFile(); len(saved) != 1 || saved[0].Slug != "esphome" {
				t.Errorf("file now holds %+v, want the check's own row", saved)
			}
		})
	}
}

// A /data this agent cannot write is a broken volume, not a broken check.
// Everything the check found is in memory, on the dashboard and on the
// sensor either way; the only thing lost is that the NEXT process starts
// with a blank card for two minutes.
func TestAnUnwritableFileCostsThePersistenceAndNothingElse(t *testing.T) {
	// A path whose parent does not exist. writeAddonUpdatesFile
	// deliberately does not MkdirAll (see its comment), so this is the
	// shape a missing or read-only /data takes - and it fails the same way
	// everywhere, unlike a chmod trick, which does nothing when the tests
	// run as root.
	path := useAddonUpdatesPath(t, filepath.Join(t.TempDir(), "no-such-directory", "addon_updates.json"))
	f := newAddonUpdateFakes()
	installedAddon(f, "esphome", "ESPHome Device Builder", "2025.6.0", "2025.7.1")
	r := f.reconciler(dryRunAutoUpdateOpts("esphome"))
	pushesBefore := len(f.status.pushes)

	r.CheckAddonUpdates(context.Background())

	if row := addonUpdateRow(t, r.Status(), "esphome"); !row.UpdateAvailable {
		t.Errorf("row = %+v, want the check's own finding recorded in memory", row)
	}
	if len(f.status.pushes) <= pushesBefore {
		t.Error("no sensor push after the check; a failed persist must not swallow the result")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("os.Stat(%q) = %v, want the write to have failed", path, err)
	}
	// No half-written .tmp left behind either, in the volume a user browses
	// over Samba.
	if _, err := os.Stat(path + ".tmp"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a .tmp file survived the failed write: %v", err)
	}
	// Warned through slog, not logged as an activity event: the feed is for
	// what the agent did to the box, and this is a display cache.
	if hasEventContaining(r.Status().Events, "persist") {
		t.Errorf("events = %+v, want no feed line about a failed display-cache write", r.Status().Events)
	}
}

// --- the gate ----------------------------------------------------------

// With auto_update_addons empty the card is not rendered at all, so a file
// left behind by a configuration that used to watch add-ons must not put
// rows back into a status nothing displays them from. Empty must also stay
// empty rather than nil, which is a separate promise: it serializes into
// /status.json as [] beside every other list.
func TestNoWatchedAddonsMeansNoRestoredRows(t *testing.T) {
	path := useAddonUpdatesFile(t)
	writeAddonUpdatesJSON(t, path, `{"rows":[{"slug":"esphome","last_result":"up to date",`+
		`"last_checked_utc":"`+savedCheckUTC+`"}]}`)

	f := newAddonUpdateFakes()
	r := f.reconciler(baseOpts())

	st := r.Status()
	if len(st.AddonUpdates) != 0 {
		t.Fatalf("rows = %+v, want none with the option empty", st.AddonUpdates)
	}
	if st.AutoUpdateEnabled {
		t.Error("auto_update_enabled = true with the option empty")
	}
	encoded, err := json.Marshal(st.AddonUpdates)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if string(encoded) != "[]" {
		t.Errorf("addon_updates serialized as %s, want []", encoded)
	}
}

// Two shapes a real configuration cannot produce, pinned so the behaviour
// is at least known rather than discovered: options.asStringSlice trims
// every entry of auto_update_addons, drops the empty ones and deduplicates
// what is left, so neither a repeated slug nor an empty one survives the
// option loader. Both are reachable only through an Options built in
// process - which is to say, here.
//
// What the function does with them is the least surprising thing available:
// one row per option entry, so a repeat is restored twice (a check would
// also produce two rows for it), and a saved row keyed on "" matches no
// watched slug and is dropped like any other row nobody watches.
func TestARepeatedSlugRestoresTwiceAndASavedEmptySlugIsDropped(t *testing.T) {
	path := useAddonUpdatesFile(t)
	writeAddonUpdatesJSON(t, path, `{"rows":[`+
		`{"slug":"esphome","last_result":"up to date","last_checked_utc":"`+savedCheckUTC+`"},`+
		`{"slug":"","last_result":"up to date","last_checked_utc":"`+savedCheckUTC+`"}]}`)

	f := newAddonUpdateFakes()
	r := f.reconciler(dryRunAutoUpdateOpts("esphome", "esphome"))

	rows := r.Status().AddonUpdates
	if len(rows) != 2 {
		t.Fatalf("rows = %+v, want one per option entry", rows)
	}
	if rows[0] != rows[1] {
		t.Errorf("rows = %+v, want the same saved row restored for both entries", rows)
	}
	for _, row := range rows {
		if row.Slug == "" {
			t.Errorf("rows = %+v, want the saved empty-slug row dropped", rows)
		}
	}
}

// --- the file itself ---------------------------------------------------

// The round trip through disk, asserted at the store rather than through a
// Reconciler: the write is atomic (tmp then rename), so what a reader sees
// is either the previous content or the whole new one, never a prefix.
func TestWritingTheAddonUpdatesFileReplacesItAtomically(t *testing.T) {
	path := useAddonUpdatesFile(t)
	first := []AddonUpdateStatus{{Slug: "esphome", LastResult: "up to date", LastCheckedUTC: savedCheckUTC}}
	if err := writeAddonUpdatesFile(first); err != nil {
		t.Fatalf("writeAddonUpdatesFile: %v", err)
	}
	if got := readAddonUpdatesFile(); len(got) != 1 || got[0] != first[0] {
		t.Fatalf("read back %+v, want %+v", got, first)
	}

	// A shorter second write. Nothing of the longer first one may survive
	// it - a truncating writer that reused the same file would leave the
	// tail of the old JSON behind and the next read would discard the lot.
	second := []AddonUpdateStatus{{Slug: "a", LastResult: "up to date"}}
	if err := writeAddonUpdatesFile(second); err != nil {
		t.Fatalf("writeAddonUpdatesFile: %v", err)
	}
	if got := readAddonUpdatesFile(); len(got) != 1 || got[0] != second[0] {
		t.Errorf("read back %+v, want %+v", got, second)
	}
	if _, err := os.Stat(path + ".tmp"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the .tmp file outlived the rename: %v", err)
	}

	// An empty result set is a real answer (every watched slug was dropped
	// from the option), and must not read back as "no file".
	if err := writeAddonUpdatesFile(nil); err != nil {
		t.Fatalf("writeAddonUpdatesFile(nil): %v", err)
	}
	if got := readAddonUpdatesFile(); len(got) != 0 {
		t.Errorf("read back %+v after writing nothing, want empty", got)
	}
}

// The loop's own persistence, through the path a real process takes: the
// timer fires, the cycle runs, the file is there for the next start. Kept
// separate from the direct CheckAddonUpdates tests above because it is the
// only one that proves the unattended caller persists too.
func TestTheUnattendedCycleAlsoPersistsWhatItFound(t *testing.T) {
	usePauseFile(t)
	useAddonUpdatesFile(t)
	setAddonUpdateTimers(t, time.Millisecond, time.Hour)
	f := newAddonUpdateFakes()
	installedAddon(f, "esphome", "ESPHome Device Builder", "2025.6.0", "2025.7.1")
	r := f.reconciler(dryRunAutoUpdateOpts("esphome"))

	r.addonUpdateCycle(context.Background())

	saved := readAddonUpdatesFile()
	if len(saved) != 1 || saved[0].Slug != "esphome" || !saved[0].UpdateAvailable {
		t.Fatalf("file holds %+v, want the cycle's own row", saved)
	}
	if saved[0].LastCheckedUTC == "" {
		t.Error("the persisted row carries no check time, so its age can never be rendered")
	}
}
