package recon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/options"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/regapply"
)

// trackVersionsOpts turns tracking on and leaves dry_run at its default
// (on), so these tests also prove the two are independent.
func trackVersionsOpts() options.Options {
	opts := baseOpts()
	opts.TrackAddonVersions = true
	return opts
}

func installed(slug, name, version string) regapply.InstalledAddon {
	return regapply.InstalledAddon{Slug: slug, Name: name, Version: version}
}

// --- rendering ------------------------------------------------------------

func TestRenderAddonVersions(t *testing.T) {
	tests := []struct {
		name   string
		addons []regapply.InstalledAddon
		want   string
	}{
		{
			name: "sorted by slug, whatever order supervisor answered in",
			addons: []regapply.InstalledAddon{
				installed("core_samba", "Samba share", "12.3.2"),
				installed("a0d7b954_esphome", "ESPHome Device Builder", "2025.8.0"),
			},
			want: "a0d7b954_esphome:\n" +
				"  name: ESPHome Device Builder\n" +
				"  version: 2025.8.0\n" +
				"core_samba:\n" +
				"  name: Samba share\n" +
				"  version: 12.3.2\n",
		},
		{
			name:   "a version that would read back as a number is quoted",
			addons: []regapply.InstalledAddon{installed("local_thing", "Thing", "1.2")},
			want:   "local_thing:\n  name: Thing\n  version: \"1.2\"\n",
		},
		{
			name:   "a name that would read back as a boolean is quoted",
			addons: []regapply.InstalledAddon{installed("local_thing", "true", "3.0.0")},
			want:   "local_thing:\n  name: \"true\"\n  version: 3.0.0\n",
		},
		{
			name:   "a version that would read back as null is quoted",
			addons: []regapply.InstalledAddon{installed("local_thing", "Thing", "~")},
			want:   "local_thing:\n  name: Thing\n  version: \"~\"\n",
		},
		{
			name:   "a name with a colon stays one scalar",
			addons: []regapply.InstalledAddon{installed("local_thing", "Thing: the sequel", "3.0.0")},
			want:   "local_thing:\n  name: 'Thing: the sequel'\n  version: 3.0.0\n",
		},
		{
			name:   "an add-on with no display name still records its version",
			addons: []regapply.InstalledAddon{installed("local_thing", "", "3.0.0")},
			want:   "local_thing:\n  name: \"\"\n  version: 3.0.0\n",
		},
		{
			name: "a duplicate slug keeps the first entry",
			addons: []regapply.InstalledAddon{
				installed("core_samba", "Samba share", "12.3.2"),
				installed("core_samba", "Samba share", "9.9.9"),
			},
			want: "core_samba:\n  name: Samba share\n  version: 12.3.2\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := renderAddonVersions(tt.addons)
			if err != nil {
				t.Fatalf("renderAddonVersions: %v", err)
			}
			if want := addonVersionsHeader + tt.want; string(got) != want {
				t.Errorf("renderAddonVersions() =\n%s\nwant\n%s", got, want)
			}
		})
	}
}

// gitsync.RecordFile's no-op check compares bytes, so a reshuffled
// Supervisor answer must not commit. Input order is all that can vary.
func TestRenderAddonVersionsIsByteStableWhateverTheInputOrder(t *testing.T) {
	addons := []regapply.InstalledAddon{
		installed("core_samba", "Samba share", "12.3.2"),
		installed("a0d7b954_esphome", "ESPHome Device Builder", "2025.8.0"),
		installed("core_configurator", "File editor", "6.5.1"),
		installed("local_thing", "Thing", "0.1.0"),
	}
	want, err := renderAddonVersions(addons)
	if err != nil {
		t.Fatalf("renderAddonVersions: %v", err)
	}

	orders := permutations(addons)
	if len(orders) != 24 {
		t.Fatalf("permutations = %d, want 24 - the test is not covering what it claims", len(orders))
	}
	for _, order := range orders {
		got, err := renderAddonVersions(order)
		if err != nil {
			t.Fatalf("renderAddonVersions: %v", err)
		}
		if string(got) != string(want) {
			t.Fatalf("order %v rendered differently:\n%s\nwant\n%s", slugsOf(order), got, want)
		}
	}
}

// permutations returns every ordering of items, each as its own slice.
func permutations(items []regapply.InstalledAddon) [][]regapply.InstalledAddon {
	if len(items) <= 1 {
		return [][]regapply.InstalledAddon{append([]regapply.InstalledAddon(nil), items...)}
	}
	var out [][]regapply.InstalledAddon
	for i := range items {
		rest := make([]regapply.InstalledAddon, 0, len(items)-1)
		rest = append(rest, items[:i]...)
		rest = append(rest, items[i+1:]...)
		for _, tail := range permutations(rest) {
			out = append(out, append([]regapply.InstalledAddon{items[i]}, tail...))
		}
	}
	return out
}

func slugsOf(addons []regapply.InstalledAddon) []string {
	out := make([]string, len(addons))
	for i, a := range addons {
		out[i] = a.Slug
	}
	return out
}

// --- the reconcile-cycle hook ---------------------------------------------

func TestReconcileRecordsAddonVersionsWhenTracking(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.registryApplier.installedAddons = []regapply.InstalledAddon{
		installed("core_samba", "Samba share", "12.3.2"),
		installed("a0d7b954_esphome", "ESPHome Device Builder", "2025.8.0"),
	}
	r := fakes.reconciler(trackVersionsOpts())

	r.ReconcileNow(context.Background())

	if len(fakes.git.recordFileCalls) != 1 {
		t.Fatalf("RecordFile calls = %d, want 1", len(fakes.git.recordFileCalls))
	}
	call := fakes.git.recordFileCalls[0]
	if call.relPath != "gitops/addon-versions.yaml" {
		t.Errorf("recorded path = %q, want gitops/addon-versions.yaml", call.relPath)
	}
	if call.message != addonVersionsCommitMessage {
		t.Errorf("commit message = %q, want %q", call.message, addonVersionsCommitMessage)
	}
	want, err := renderAddonVersions(fakes.registryApplier.installedAddons)
	if err != nil {
		t.Fatal(err)
	}
	if string(call.content) != string(want) {
		t.Errorf("recorded content =\n%s\nwant\n%s", call.content, want)
	}

	st := r.Status()
	if st.LastVersionRecordUTC == "" {
		t.Error("LastVersionRecordUTC is empty after a record that committed")
	}
	if !hasEventContaining(st.Events, "recorded add-on versions (2 add-on(s))") {
		t.Errorf("no summary event for the first record: %+v", st.Events)
	}
	// The record is not part of the sync verdict.
	if st.State != StateInSync || st.LastError != "" {
		t.Errorf("state = %q, last_error = %q, want in_sync with no error", st.State, st.LastError)
	}
}

func TestReconcileDoesNotRecordVersionsWhenTrackingIsOff(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.registryApplier.installedAddons = []regapply.InstalledAddon{installed("core_samba", "Samba share", "12.3.2")}
	r := fakes.reconciler(baseOpts())

	r.ReconcileNow(context.Background())

	if fakes.registryApplier.fetchInstalledCalls != 0 {
		t.Errorf("FetchInstalledAddons calls = %d, want 0 with track_addon_versions off", fakes.registryApplier.fetchInstalledCalls)
	}
	if len(fakes.git.recordFileCalls) != 0 {
		t.Errorf("RecordFile calls = %d, want 0", len(fakes.git.recordFileCalls))
	}
}

// A failed cycle has no usable repository to record into.
func TestReconcileDoesNotRecordVersionsWhenTheCycleFailed(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.git.fetchErr = errors.New("network is unreachable")
	fakes.registryApplier.installedAddons = []regapply.InstalledAddon{installed("core_samba", "Samba share", "12.3.2")}
	r := fakes.reconciler(trackVersionsOpts())

	r.ReconcileNow(context.Background())

	if fakes.registryApplier.fetchInstalledCalls != 0 {
		t.Errorf("FetchInstalledAddons calls = %d, want 0 after a failed cycle", fakes.registryApplier.fetchInstalledCalls)
	}
	if len(fakes.git.recordFileCalls) != 0 {
		t.Errorf("RecordFile calls = %d, want 0 after a failed cycle", len(fakes.git.recordFileCalls))
	}
}

// This runs every cycle, so an event per cycle would be the whole log.
func TestSecondRecordOfUnchangedVersionsIsSilent(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.registryApplier.installedAddons = []regapply.InstalledAddon{installed("core_samba", "Samba share", "12.3.2")}
	r := fakes.reconciler(trackVersionsOpts())

	r.ReconcileNow(context.Background())
	recordedAt := r.Status().LastVersionRecordUTC
	r.ReconcileNow(context.Background())

	if len(fakes.git.recordFileCalls) != 2 {
		t.Fatalf("RecordFile calls = %d, want 2 (it is asked every cycle)", len(fakes.git.recordFileCalls))
	}
	st := r.Status()
	if got := countEventsContaining(st.Events, "recorded add-on versions"); got != 1 {
		t.Errorf("summary events = %d, want 1 - the second cycle changed nothing", got)
	}
	if st.LastVersionRecordUTC != recordedAt {
		t.Errorf("LastVersionRecordUTC = %q, want it left at %q when nothing was committed",
			st.LastVersionRecordUTC, recordedAt)
	}
}

func TestRecordLogsWhatChangedBetweenCycles(t *testing.T) {
	tests := []struct {
		name      string
		second    []regapply.InstalledAddon
		wantEvent string
	}{
		{
			name:      "a version that moved",
			second:    []regapply.InstalledAddon{installed("core_samba", "Samba share", "12.4.0")},
			wantEvent: "recorded version change: core_samba 12.3.2 -> 12.4.0",
		},
		{
			name: "an add-on that appeared",
			second: []regapply.InstalledAddon{
				installed("core_samba", "Samba share", "12.3.2"),
				installed("core_configurator", "File editor", "6.5.1"),
			},
			wantEvent: "recorded version change: core_configurator added at 6.5.1",
		},
		{
			name:      "an add-on that went away",
			second:    []regapply.InstalledAddon{installed("core_configurator", "File editor", "6.5.1")},
			wantEvent: "recorded version change: core_samba removed (was 12.3.2)",
		},
		{
			name: "a display name that changed but no version did",
			second: []regapply.InstalledAddon{
				installed("core_samba", "Samba share (renamed)", "12.3.2"),
			},
			wantEvent: "recorded add-on versions (1 add-on(s))",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakes := newReconcilerFakes()
			fakes.registryApplier.installedAddons = []regapply.InstalledAddon{installed("core_samba", "Samba share", "12.3.2")}
			r := fakes.reconciler(trackVersionsOpts())
			r.ReconcileNow(context.Background())

			fakes.registryApplier.installedAddons = tt.second
			r.ReconcileNow(context.Background())

			st := r.Status()
			if !hasEventContaining(st.Events, tt.wantEvent) {
				t.Errorf("no event %q among %+v", tt.wantEvent, st.Events)
			}
		})
	}
}

// A core update pulls a dozen add-ons with it, which must not push the
// reconcile history out of the log.
func TestRecordCapsTheNumberOfChangeEvents(t *testing.T) {
	fakes := newReconcilerFakes()
	var before, after []regapply.InstalledAddon
	for i := 0; i < versionChangeEventCap+2; i++ {
		slug := fmt.Sprintf("core_addon%02d", i)
		before = append(before, installed(slug, "Add-on", "1.0.0"))
		after = append(after, installed(slug, "Add-on", "2.0.0"))
	}
	fakes.registryApplier.installedAddons = before
	r := fakes.reconciler(trackVersionsOpts())
	r.ReconcileNow(context.Background())

	fakes.registryApplier.installedAddons = after
	r.ReconcileNow(context.Background())

	st := r.Status()
	if got := countEventsContaining(st.Events, "recorded version change:"); got != versionChangeEventCap {
		t.Errorf("change events = %d, want %d", got, versionChangeEventCap)
	}
	if !hasEventContaining(st.Events, "... and 2 more add-on version change(s)") {
		t.Errorf("no overflow event among %+v", st.Events)
	}
	// The kept ones are the sorted first ones, not map order.
	if !hasEventContaining(st.Events, "recorded version change: core_addon00 1.0.0 -> 2.0.0") {
		t.Errorf("the capped list does not start at the first slug: %+v", st.Events)
	}
}

// Nothing installed is impossible - this agent is itself an add-on - so
// an empty answer must not blank the record out.
func TestRecordSkipsAnEmptyInstalledList(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.registryApplier.installedAddons = nil
	r := fakes.reconciler(trackVersionsOpts())

	r.ReconcileNow(context.Background())

	if len(fakes.git.recordFileCalls) != 0 {
		t.Errorf("RecordFile calls = %d, want 0 for an empty installed list", len(fakes.git.recordFileCalls))
	}
	st := r.Status()
	if st.State != StateInSync || st.LastError != "" {
		t.Errorf("state = %q, last_error = %q, want the cycle unaffected", st.State, st.LastError)
	}
}

func TestRecordFailuresAreWarningsAndNeverTheSyncState(t *testing.T) {
	tests := []struct {
		name      string
		arrange   func(f *reconcilerFakes)
		wantEvent string
	}{
		{
			name:      "supervisor cannot be reached",
			arrange:   func(f *reconcilerFakes) { f.registryApplier.installedAddonsErr = errors.New("connection refused") },
			wantEvent: "could not read the installed add-on list: connection refused",
		},
		{
			name: "the push is refused",
			arrange: func(f *reconcilerFakes) {
				f.registryApplier.installedAddons = []regapply.InstalledAddon{installed("core_samba", "Samba share", "12.3.2")}
				f.git.recordFileErr = errors.New("remote: write access denied")
			},
			wantEvent: "could not record add-on versions: remote: write access denied",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakes := newReconcilerFakes()
			tt.arrange(fakes)
			r := fakes.reconciler(trackVersionsOpts())

			r.ReconcileNow(context.Background())

			st := r.Status()
			if st.State != StateInSync {
				t.Errorf("state = %q, want in_sync - a failed record says nothing about the sync", st.State)
			}
			if st.LastError != "" {
				t.Errorf("last_error = %q, want it left empty", st.LastError)
			}
			if st.LastVersionRecordUTC != "" {
				t.Errorf("LastVersionRecordUTC = %q, want it unset after a failure", st.LastVersionRecordUTC)
			}
			if !hasEventContaining(st.Events, tt.wantEvent) {
				t.Errorf("no warning event %q among %+v", tt.wantEvent, st.Events)
			}
			if !strings.HasPrefix(eventContaining(t, st.Events, tt.wantEvent), "warning: ") {
				t.Error("the failure event does not read as a warning")
			}
		})
	}
}

// A standing failure must log once, not four times an hour forever.
func TestRepeatedRecordFailureLogsOnceAndReportsRecovery(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.registryApplier.installedAddonsErr = errors.New("connection refused")
	r := fakes.reconciler(trackVersionsOpts())

	r.ReconcileNow(context.Background())
	r.ReconcileNow(context.Background())
	r.ReconcileNow(context.Background())

	if got := countEventsContaining(r.Status().Events, "could not read the installed add-on list"); got != 1 {
		t.Errorf("failure events = %d, want 1 across three failing cycles", got)
	}

	fakes.registryApplier.installedAddonsErr = nil
	fakes.registryApplier.installedAddons = []regapply.InstalledAddon{installed("core_samba", "Samba share", "12.3.2")}
	r.ReconcileNow(context.Background())

	st := r.Status()
	if !hasEventContaining(st.Events, "add-on version record recovered") {
		t.Errorf("no recovery event among %+v", st.Events)
	}
	if st.LastVersionRecordUTC == "" {
		t.Error("LastVersionRecordUTC is empty after the recovering record committed")
	}
}

// The record is a repository write, not a change to the box, and dry_run
// governs only the latter - the same line import and commit_back draw.
func TestRecordRunsWithDryRunOff(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.registryApplier.installedAddons = []regapply.InstalledAddon{installed("core_samba", "Samba share", "12.3.2")}
	opts := trackVersionsOpts()
	opts.DryRun = false
	r := fakes.reconciler(opts)

	r.ReconcileNow(context.Background())

	if len(fakes.git.recordFileCalls) != 1 {
		t.Errorf("RecordFile calls = %d, want 1 with dry_run off too", len(fakes.git.recordFileCalls))
	}
}

// eventContaining returns the first event message holding sub, or fails.
func eventContaining(t *testing.T, events []Event, sub string) string {
	t.Helper()
	for _, e := range events {
		if strings.Contains(e.Message, sub) {
			return e.Message
		}
	}
	t.Fatalf("no event containing %q among %+v", sub, events)
	return ""
}
