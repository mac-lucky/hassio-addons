package recon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/differ"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/gitsync"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/options"
)

// importOpts is baseOpts with the import feature turned on.
func importOpts() options.Options {
	o := baseOpts()
	o.AllowImport = true
	return o
}

func TestImportLiveRefusedWhenOptionOff(t *testing.T) {
	f := newReconcilerFakes()
	r := f.reconciler(baseOpts())

	if _, err := r.ImportLive(context.Background()); !errors.Is(err, errImportDisabled) {
		t.Fatalf("ImportLive error = %v, want errImportDisabled", err)
	}
	if len(f.git.importCalls) != 0 {
		t.Errorf("git.Import called %d times with the option off, want 0 - the gate must live in the Reconciler, not just in the hidden button", len(f.git.importCalls))
	}
	if _, err := r.PreviewImport(context.Background()); !errors.Is(err, errImportDisabled) {
		t.Errorf("PreviewImport error = %v, want errImportDisabled", err)
	}
	// Both refusals re-render the same page, so each has to log or it
	// looks like it worked.
	events := r.Status().Events
	if !hasEventContaining(events, "import skipped: allow_import is disabled") {
		t.Errorf("events = %+v, want a skipped-import entry", events)
	}
	if !hasEventContaining(events, "import preview skipped: allow_import is disabled") {
		t.Errorf("events = %+v, want a skipped-preview entry", events)
	}
}

func TestImportLiveRefusesWhileBusy(t *testing.T) {
	f := newReconcilerFakes()
	r := f.reconciler(importOpts())

	if !r.opLock.TryLock() {
		t.Fatal("could not seize opLock")
	}
	defer r.opLock.Unlock()

	if _, err := r.ImportLive(context.Background()); !errors.Is(err, errBusy) {
		t.Fatalf("ImportLive error = %v, want errBusy", err)
	}
	// Read-only, but still locks: a preview built mid-apply would be wrong.
	if _, err := r.PreviewImport(context.Background()); !errors.Is(err, errBusy) {
		t.Errorf("PreviewImport error = %v, want errBusy", err)
	}
	if len(f.git.importCalls) != 0 {
		t.Errorf("git.Import called while busy, want 0 calls")
	}
	if len(f.git.scanLiveCalls) != 0 {
		t.Errorf("git.ScanLive called while busy, want 0 calls")
	}
	events := r.Status().Events
	if !hasEventContaining(events, "import skipped: another operation") {
		t.Errorf("events = %+v, want a skipped-import entry", events)
	}
	if !hasEventContaining(events, "import preview skipped: another operation") {
		t.Errorf("events = %+v, want a skipped-preview entry", events)
	}
}

// An import moves the tracked branch, so the pending list computed
// against the old tip is stale the moment the push lands.
func TestImportLiveReconcilesAfterSuccess(t *testing.T) {
	f := newReconcilerFakes()
	f.git.importResult = gitsync.ImportResult{CommitSHA: "abc1234def", Files: 3, Bytes: 4096}
	r := f.reconciler(importOpts())

	fetchesBefore := f.git.fetchCalls
	summary, err := r.ImportLive(context.Background())
	if err != nil {
		t.Fatalf("ImportLive: %v", err)
	}
	if summary.Files != 3 || summary.CommitSHA != "abc1234def" {
		t.Errorf("summary = %+v, want the git layer's own result carried through", summary)
	}
	if summary.Branch != "main" {
		t.Errorf("summary.Branch = %q, want the tracked branch", summary.Branch)
	}
	if f.git.fetchCalls <= fetchesBefore {
		t.Error("no reconcile ran after the import; the pending list would be stale")
	}
	// Nothing else here would catch importing from the checkout, not live.
	if call := f.git.importCalls[0]; call.configRoot != ConfigRoot {
		t.Errorf("config_root = %q, want %q", call.configRoot, ConfigRoot)
	}
	if call := f.git.importCalls[0]; call.limits != gitsync.DefaultImportLimits() {
		t.Errorf("limits = %+v, want the production defaults", call.limits)
	}
}

func TestImportLiveDoesNotImportWhenTheCloneIsUnusable(t *testing.T) {
	f := newReconcilerFakes()
	f.git.ensureCloneErr = errors.New("clone failed: authentication required")
	r := f.reconciler(importOpts())

	if _, err := r.ImportLive(context.Background()); err == nil {
		t.Fatal("ImportLive succeeded despite an unusable clone")
	}
	if len(f.git.importCalls) != 0 {
		t.Errorf("git.Import called %d times after a failed clone, want 0", len(f.git.importCalls))
	}
	st := r.Status()
	if st.LastError != "" {
		t.Errorf("LastError = %q, want empty", st.LastError)
	}
	if !strings.Contains(st.LastImportError, "authentication required") {
		t.Errorf("LastImportError = %q, want the clone failure", st.LastImportError)
	}
}

// A cap breach or rejected push says nothing about whether live matches
// the repository, so it must not hijack the sync-state pill.
func TestImportLiveFailureLeavesSyncStateAlone(t *testing.T) {
	f := newReconcilerFakes()
	f.git.importErr = errors.New("refusing to import: total size 412.7 MB exceeds the 100.0 MB limit")
	r := f.reconciler(importOpts())

	stateBefore := r.Status().State
	if _, err := r.ImportLive(context.Background()); err == nil {
		t.Fatal("ImportLive succeeded, want the fake's error")
	}

	st := r.Status()
	if st.State != stateBefore {
		t.Errorf("State = %q, want it unchanged at %q", st.State, stateBefore)
	}
	if st.LastError != "" {
		t.Errorf("LastError = %q, want empty - an import failure is not a sync failure", st.LastError)
	}
	if !strings.Contains(st.LastImportError, "412.7 MB") {
		t.Errorf("LastImportError = %q, want the cap message surfaced in its own field", st.LastImportError)
	}
	if !eventLogged(st, "import failed:") {
		t.Error("no 'import failed:' entry in the activity log")
	}
}

// The anti-escalation rule: copying a file into the repository must never
// make the agent believe it owns that path on live.
func TestImportLiveDoesNotTouchManifest(t *testing.T) {
	f := newReconcilerFakes()
	f.applier.state.Manifest = []string{"automations.yaml", "scripts.yaml"}
	f.applier.state.LastGoodSHA = "deadbeef"
	r := f.reconciler(importOpts())

	if _, err := r.ImportLive(context.Background()); err != nil {
		t.Fatalf("ImportLive: %v", err)
	}

	got := f.applier.state.Manifest
	if len(got) != 2 || got[0] != "automations.yaml" || got[1] != "scripts.yaml" {
		t.Errorf("Manifest = %v, want it untouched by an import", got)
	}
	if f.applier.state.LastGoodSHA != "deadbeef" {
		t.Errorf("LastGoodSHA = %q, want it untouched - an import applies nothing to live", f.applier.state.LastGoodSHA)
	}
}

func TestImportLivePersistsAndSurfacesLastImport(t *testing.T) {
	f := newReconcilerFakes()
	f.git.importResult = gitsync.ImportResult{CommitSHA: "0123456789abcdef", Files: 2, Bytes: 100, Created: true}
	r := f.reconciler(importOpts())

	if _, err := r.ImportLive(context.Background()); err != nil {
		t.Fatalf("ImportLive: %v", err)
	}

	if f.applier.state.LastImportSHA != "0123456789abcdef" {
		t.Errorf("state.LastImportSHA = %q, want the import commit", f.applier.state.LastImportSHA)
	}
	if f.applier.state.LastImportUTC == "" {
		t.Error("state.LastImportUTC is empty, want a timestamp")
	}

	st := r.Status()
	if st.LastImportSHAShort != "0123456" {
		t.Errorf("LastImportSHAShort = %q, want 0123456", st.LastImportSHAShort)
	}
	if st.LastImportUTC == "" {
		t.Error("Status.LastImportUTC is empty")
	}
	if !st.ImportEnabled {
		t.Error("Status.ImportEnabled = false with allow_import on")
	}
	if !eventLogged(st, "onto new branch main") {
		t.Error("the activity log does not say the branch was created")
	}

	var sawAttr bool
	for _, push := range f.status.pushes {
		if v, ok := push.attrs["last_import_utc"]; ok && v != nil {
			sawAttr = true
		}
	}
	if !sawAttr {
		t.Error("last_import_utc never reached the sensor attributes")
	}
}

func TestPreviewImportReportsThePlanWithoutImporting(t *testing.T) {
	f := newReconcilerFakes()
	f.git.scanLivePlan = gitsync.ImportPlan{
		Files:           []string{"configuration.yaml", "packages/lights.yaml"},
		TotalBytes:      2048,
		SkippedExcluded: 4,
		SkippedSecret:   1,
	}
	r := f.reconciler(importOpts())

	preview, err := r.PreviewImport(context.Background())
	if err != nil {
		t.Fatalf("PreviewImport: %v", err)
	}
	if len(preview.Files) != 2 || preview.TotalBytes != 2048 {
		t.Errorf("preview = %+v, want the scan's own plan", preview)
	}
	if preview.SkippedSecret != 1 {
		t.Errorf("SkippedSecret = %d, want 1", preview.SkippedSecret)
	}
	if len(f.git.importCalls) != 0 {
		t.Error("a preview ran a real import; it may read through git but must never write")
	}
	if len(f.git.scanLiveCalls) != 1 {
		t.Errorf("ScanLive called %d times, want 1", len(f.git.scanLiveCalls))
	}
	if call := f.git.scanLiveCalls[0]; call.configRoot != ConfigRoot {
		t.Errorf("config_root = %q, want %q", call.configRoot, ConfigRoot)
	}
}

func TestPreviewImportSurfacesScanFailure(t *testing.T) {
	f := newReconcilerFakes()
	// A real error value, not an invented string, so the message here is
	// the one users actually see.
	f.git.scanLiveErr = &gitsync.ImportTooLargeError{
		Reason: "file count", Limit: 5000, Actual: 8123,
		Offenders: []gitsync.ImportOffender{{Path: "www/", Files: 8000, Bytes: 1 << 20}},
	}
	r := f.reconciler(importOpts())

	if _, err := r.PreviewImport(context.Background()); err == nil {
		t.Fatal("PreviewImport succeeded, want the fake's error")
	}
	st := r.Status()
	if !strings.Contains(st.LastImportError, "8123 files exceeds the 5000 limit") {
		t.Errorf("LastImportError = %q, want the cap message", st.LastImportError)
	}
	if st.LastError != "" {
		t.Errorf("LastError = %q, want empty", st.LastError)
	}
}

// Otherwise the only way off the page is running the import the card is
// previewing, which is what somebody who decided against it will not do.
func TestDismissImportPreviewClearsThePreviewAndNothingElse(t *testing.T) {
	f := newReconcilerFakes()
	f.git.scanLivePlan = gitsync.ImportPlan{Files: []string{"configuration.yaml"}, TotalBytes: 512}
	f.git.importResult = gitsync.ImportResult{CommitSHA: "0123456789abcdef", Files: 1, Bytes: 512}
	r := f.reconciler(importOpts())

	// An import first, so the fields it must leave alone are non-empty.
	if _, err := r.ImportLive(context.Background()); err != nil {
		t.Fatalf("ImportLive: %v", err)
	}
	if _, err := r.PreviewImport(context.Background()); err != nil {
		t.Fatalf("PreviewImport: %v", err)
	}

	before := r.Status()
	if before.ImportPreview == nil {
		t.Fatal("no preview recorded to dismiss")
	}
	pushesBefore := len(f.status.pushes)

	r.DismissImportPreview()

	after := r.Status()
	if after.ImportPreview != nil {
		t.Errorf("import_preview = %+v, want nil after a dismissal", after.ImportPreview)
	}
	// Clearing these would leave the repeat-import confirmation unable to
	// say when this repository was last seeded.
	if after.LastImportUTC != before.LastImportUTC || after.LastImportSHA != before.LastImportSHA {
		t.Errorf("last import = %q/%q, want it untouched (%q/%q)",
			after.LastImportUTC, after.LastImportSHA, before.LastImportUTC, before.LastImportSHA)
	}
	if after.LastImportError != before.LastImportError {
		t.Errorf("last_import_error = %q, want it untouched (%q)", after.LastImportError, before.LastImportError)
	}
	// No event: the card leaving the page is already visible, and the log
	// is 200 entries shared with the reconcile history.
	if len(after.Events) != len(before.Events) {
		t.Errorf("events went %d -> %d, want a dismissal to log nothing", len(before.Events), len(after.Events))
	}
	// It does push: sensor and page both render from Status, so an
	// unannounced change waits for the next poll to notice it.
	if len(f.status.pushes) <= pushesBefore {
		t.Error("no status push after the dismissal")
	}

	// Idempotent: the button stays on screen until the next render.
	r.DismissImportPreview()
	if r.Status().ImportPreview != nil {
		t.Error("a second dismissal brought the preview back")
	}
}

// Nothing about the preview belongs to the config, so the card clears
// while an apply or reconcile holds opLock.
func TestDismissImportPreviewWorksWhileAnOperationIsRunning(t *testing.T) {
	f := newReconcilerFakes()
	f.git.scanLivePlan = gitsync.ImportPlan{Files: []string{"configuration.yaml"}, TotalBytes: 512}
	r := f.reconciler(importOpts())

	if _, err := r.PreviewImport(context.Background()); err != nil {
		t.Fatalf("PreviewImport: %v", err)
	}
	if !r.opLock.TryLock() {
		t.Fatal("could not seize opLock for the test")
	}
	defer r.opLock.Unlock()

	r.DismissImportPreview()

	if st := r.Status(); st.ImportPreview != nil {
		t.Errorf("import_preview = %+v, want it cleared while opLock is held", st.ImportPreview)
	}
}

// Only ImportLive may call the unlocked worker.
func TestReconcileNowStillTakesOpLock(t *testing.T) {
	f := newReconcilerFakes()
	r := f.reconciler(baseOpts())

	if !r.opLock.TryLock() {
		t.Fatal("could not seize opLock")
	}
	defer r.opLock.Unlock()

	fetchesBefore := f.git.fetchCalls
	r.ReconcileNow(context.Background())
	if f.git.fetchCalls != fetchesBefore {
		t.Error("ReconcileNow ran while the op lock was held")
	}
}

// eventLogged reports whether any activity entry contains substr.
func eventLogged(st Status, substr string) bool {
	for _, e := range st.Events {
		if strings.Contains(e.Message, substr) {
			return true
		}
	}
	return false
}

// Import filters through .gitignore before copying, so the scan's own
// numbers are not what would be committed.
func TestPreviewImportReportsWhatWouldBeCommitted(t *testing.T) {
	f := newReconcilerFakes()
	r := f.reconciler(importOpts())
	f.git.scanLivePlan = gitsync.ImportPlan{
		Files:      []string{"configuration.yaml", "custom_components/hacs/__init__.py", "packages/lights.yaml"},
		TotalBytes: 3000,
	}
	f.git.previewIgnored = map[string]bool{"custom_components/hacs/__init__.py": true}
	f.git.previewIgnoredKeptBytes = 1000

	preview, err := r.PreviewImport(context.Background())
	if err != nil {
		t.Fatalf("PreviewImport: %v", err)
	}
	want := []string{"configuration.yaml", "packages/lights.yaml"}
	if strings.Join(preview.Files, ",") != strings.Join(want, ",") {
		t.Errorf("Files = %v, want %v", preview.Files, want)
	}
	if preview.TotalBytes != 1000 {
		t.Errorf("TotalBytes = %d, want 1000", preview.TotalBytes)
	}
	if preview.SkippedGitignored != 1 {
		t.Errorf("SkippedGitignored = %d, want 1", preview.SkippedGitignored)
	}
}

// Falling back to the scan's numbers would look right and be wrong.
func TestPreviewImportFailsLoudlyWhenTheFilterFails(t *testing.T) {
	f := newReconcilerFakes()
	r := f.reconciler(importOpts())
	f.git.scanLivePlan = gitsync.ImportPlan{Files: []string{"configuration.yaml"}, TotalBytes: 10}
	f.git.previewIgnoredErr = errors.New("check-ignore exploded")

	if _, err := r.PreviewImport(context.Background()); err == nil {
		t.Fatal("PreviewImport succeeded, want the filter failure surfaced")
	}
}

// Bug fix: importLive's own pushStatus used to republish the verdict the
// import had just invalidated - on an unseeded repository, the very error
// the seed resolved - for the whole of the chained reconcile.
func TestASuccessfulImportClearsTheSyncVerdictItSuperseded(t *testing.T) {
	f := newReconcilerFakes()
	f.git.fetchErr = fmt.Errorf("%w: main", gitsync.ErrRemoteBranchMissing)
	r := f.reconciler(importOpts())

	r.ReconcileNow(context.Background())
	if got := r.Status().State; got != StateUnseeded {
		t.Fatalf("State = %q, want %q before the import", got, StateUnseeded)
	}

	// What the import does to the world: the branch now exists.
	f.git.fetchErr = nil
	if _, err := r.ImportLive(context.Background()); err != nil {
		t.Fatalf("ImportLive: %v", err)
	}

	// The push importLive makes itself, before the chained reconcile's -
	// the one the sensor and the next poll actually sat on.
	if len(f.status.pushes) < 2 {
		t.Fatalf("pushes = %d, want at least 2", len(f.status.pushes))
	}
	own := f.status.pushes[len(f.status.pushes)-2]
	if own.state == StateError || own.state == StateUnseeded {
		t.Errorf("importLive published state %q, want the superseded verdict retired", own.state)
	}
	if own.attrs["error"] != nil {
		t.Errorf("importLive published error %v, want nil", own.attrs["error"])
	}
}

// A drift_pending plan may still hold registry ops, which an import cannot
// have changed: gitops/ is excluded from what it commits.
func TestASuccessfulImportDoesNotOverwriteADriftPendingState(t *testing.T) {
	f := newReconcilerFakes()
	f.differ.changes = []differ.Change{{Path: "automations.yaml", Kind: "modified"}}
	r := f.reconciler(importOpts())

	r.ReconcileNow(context.Background())
	if got := r.Status().State; got != StateDriftPending {
		t.Fatalf("State = %q, want %q", got, StateDriftPending)
	}

	if _, err := r.ImportLive(context.Background()); err != nil {
		t.Fatalf("ImportLive: %v", err)
	}

	if got := r.Status().State; got != StateDriftPending {
		t.Errorf("State = %q, want %q left alone", got, StateDriftPending)
	}
}

// Bug fix: the save failure was only logged, so a restart silently showed
// the previous import and nothing on the page said why.
func TestAnImportWhoseRecordCannotBeSavedRaisesAHealthFlag(t *testing.T) {
	f := newReconcilerFakes()
	f.applier.stateSaveErr = errors.New("/data is read-only")
	r := f.reconciler(importOpts())

	if _, err := r.ImportLive(context.Background()); err != nil {
		t.Fatalf("ImportLive = %v, want success: the commit was pushed", err)
	}

	st := r.Status()
	if !st.ImportRecordFailing {
		t.Error("ImportRecordFailing = false, want true")
	}
	if !st.HasHealthWarnings() {
		t.Error("HasHealthWarnings = false, want the chip row shown")
	}
	if st.LastImportUTC == "" {
		t.Error("LastImportUTC is empty, want the in-memory value still correct")
	}
	if !eventLogged(st, "could not be saved") {
		t.Error("no event explaining the unsaved record")
	}
}

func TestTheImportRecordFlagStaysDownWhenTheSaveWorks(t *testing.T) {
	f := newReconcilerFakes()
	r := f.reconciler(importOpts())

	if _, err := r.ImportLive(context.Background()); err != nil {
		t.Fatalf("ImportLive: %v", err)
	}

	if r.Status().ImportRecordFailing {
		t.Error("ImportRecordFailing = true after a save that worked")
	}
}
