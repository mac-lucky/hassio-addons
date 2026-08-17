package recon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/differ"
)

// usePauseFile points pausePath at a temp file and restores it after; the
// file is not created, because absent is the unpaused state.
func usePauseFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "paused")
	previous := pausePath
	pausePath = path
	t.Cleanup(func() { pausePath = previous })
	return path
}

func pauseFileExists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Stat(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat %s: %v", path, err)
	}
	return err == nil
}

// --- what a pause stops ------------------------------------------------

// Asserted against the git fake, not the resulting state: a tick that
// fetched and then declined to apply has already moved lastSHA.
func TestTickDoesNothingWhilePaused(t *testing.T) {
	usePauseFile(t)
	fakes := newReconcilerFakes()
	fakes.differ.changes = []differ.Change{{Path: "automations.yaml", Kind: "update", DiffText: "+x"}}
	opts := baseOpts()
	opts.DryRun = false
	r := fakes.reconciler(opts)
	if err := r.SetPaused(true); err != nil {
		t.Fatal(err)
	}

	r.tick(context.Background())

	if fakes.git.ensureCloneCalls != 0 || fakes.git.fetchCalls != 0 {
		t.Errorf("git calls = %d clone / %d fetch, want none: a paused tick must not reach the repository",
			fakes.git.ensureCloneCalls, fakes.git.fetchCalls)
	}
	if len(fakes.applier.applyCalls) != 0 {
		t.Errorf("apply_calls = %v, want none", fakes.applier.applyCalls)
	}
}

// The countdown defer sits at the top of tick, so this holds only while
// the pause check comes before it.
func TestPausedTickLeavesTheCountdownCleared(t *testing.T) {
	usePauseFile(t)
	fakes := newReconcilerFakes()
	r := fakes.reconciler(baseOpts())

	r.tick(context.Background())
	if r.Status().NextCheckUTC == "" {
		t.Fatal("next_check_utc is empty after an unpaused tick, want it armed")
	}

	if err := r.SetPaused(true); err != nil {
		t.Fatal(err)
	}
	if got := r.Status().NextCheckUTC; got != "" {
		t.Fatalf("next_check_utc = %q after pausing, want it cleared", got)
	}

	r.tick(context.Background())

	if got := r.Status().NextCheckUTC; got != "" {
		t.Errorf("next_check_utc = %q, want it still cleared: a paused tick must not re-arm the countdown", got)
	}
}

// Flux `suspend` semantics, not a kill switch: manual operations are what
// a pause does not stop.
func TestPauseLeavesManualReconcileAndApplyWorking(t *testing.T) {
	usePauseFile(t)
	fakes := newReconcilerFakes()
	fakes.differ.changes = []differ.Change{{Path: "automations.yaml", Kind: "update", DiffText: "+x"}}
	opts := baseOpts()
	opts.DryRun = false
	r := fakes.reconciler(opts)
	if err := r.SetPaused(true); err != nil {
		t.Fatal(err)
	}

	r.ReconcileNow(context.Background())
	r.ApplyNow(context.Background(), true)

	if fakes.git.fetchCalls != 1 {
		t.Errorf("fetch calls = %d, want 1: Check Now must still run while paused", fakes.git.fetchCalls)
	}
	if len(fakes.applier.applyCalls) != 1 {
		t.Errorf("apply_calls = %d, want 1: Apply must still run while paused", len(fakes.applier.applyCalls))
	}
}

// --- the flag itself ---------------------------------------------------

func TestSetPausedWritesAndRemovesTheFlagFile(t *testing.T) {
	path := usePauseFile(t)
	fakes := newReconcilerFakes()
	r := fakes.reconciler(baseOpts())

	if err := r.SetPaused(true); err != nil {
		t.Fatal(err)
	}
	if !pauseFileExists(t, path) {
		t.Error("the flag file was not written")
	}

	if err := r.SetPaused(false); err != nil {
		t.Fatal(err)
	}
	if pauseFileExists(t, path) {
		t.Error("the flag file was not removed")
	}
}

// Why the flag is a file and not a bool: a restart is exactly when an
// unattended apply would otherwise start again unsupervised.
func TestPauseSurvivesARestart(t *testing.T) {
	usePauseFile(t)
	first := newReconcilerFakes().reconciler(baseOpts())
	if err := first.SetPaused(true); err != nil {
		t.Fatal(err)
	}

	restarted := newReconcilerFakes().reconciler(baseOpts())

	if !restarted.Status().Paused {
		t.Fatal("a restarted agent is not paused, want the flag hydrated from disk")
	}
	if !hasEventContaining(restarted.Status().Events, "paused") {
		t.Error("the startup event does not say the agent came back paused")
	}

	if err := restarted.SetPaused(false); err != nil {
		t.Fatal(err)
	}
	resumed := newReconcilerFakes().reconciler(baseOpts())
	if resumed.Status().Paused {
		t.Error("a restarted agent is paused after a resume, want the flag gone")
	}
}

// An absent flag file is the state a resume asks for, not a failure -
// /data being wiped under a paused process does this.
func TestResumeWithNoFlagFileIsNotAnError(t *testing.T) {
	usePauseFile(t)
	fakes := newReconcilerFakes()
	r := fakes.reconciler(baseOpts())
	if err := r.SetPaused(true); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(pausePath); err != nil {
		t.Fatal(err)
	}

	if err := r.SetPaused(false); err != nil {
		t.Errorf("SetPaused(false) = %v, want nil when the flag file is already absent", err)
	}
}

// An unwritable /data must not leave the loop running; all that is lost is
// surviving a restart, which the error and the feed both say.
func TestSetPausedKeepsTheFlagWhenTheWriteFails(t *testing.T) {
	usePauseFile(t)
	// Under a directory that does not exist, so the write fails the way a
	// read-only /data would.
	pausePath = filepath.Join(pausePath, "unwritable", "paused")
	fakes := newReconcilerFakes()
	r := fakes.reconciler(baseOpts())

	err := r.SetPaused(true)

	if err == nil {
		t.Fatal("SetPaused = nil, want the write error reported")
	}
	status := r.Status()
	if !status.Paused {
		t.Error("the agent is not paused, want the in-memory flag to stand after a failed write")
	}
	if !hasEventContaining(status.Events, "will not survive a restart") {
		t.Error("the activity feed does not say the flag was not recorded")
	}
	// And really off, not just reported as off.
	r.tick(context.Background())
	if fakes.git.fetchCalls != 0 {
		t.Errorf("fetch calls = %d, want none: a failed flag write must still stop the loop", fakes.git.fetchCalls)
	}
}

// --- what a press reports ----------------------------------------------

func TestSetPausedLogsOneEventPerRealChange(t *testing.T) {
	usePauseFile(t)
	fakes := newReconcilerFakes()
	r := fakes.reconciler(baseOpts())

	if err := r.SetPaused(true); err != nil {
		t.Fatal(err)
	}
	if err := r.SetPaused(true); err != nil {
		t.Fatal(err)
	}

	events := r.Status().Events
	if got := countEventsContaining(events, "automatic checks paused"); got != 1 {
		t.Errorf("pause events = %d, want exactly 1: a second press changed nothing and must say nothing", got)
	}

	if err := r.SetPaused(false); err != nil {
		t.Fatal(err)
	}
	if got := countEventsContaining(r.Status().Events, "automatic checks resumed"); got != 1 {
		t.Errorf("resume events = %d, want exactly 1", got)
	}
}

// pushStatus is a real call out to Home Assistant, and two tabs disagreeing
// for one poll interval is the ordinary way this gets pressed twice.
func TestSetPausedPushesTheSensorOnlyOnARealChange(t *testing.T) {
	usePauseFile(t)
	fakes := newReconcilerFakes()
	r := fakes.reconciler(baseOpts())

	if err := r.SetPaused(true); err != nil {
		t.Fatal(err)
	}
	pushes := len(fakes.status.pushes)
	if pushes == 0 {
		t.Fatal("pausing pushed no status at all")
	}
	if got := fakes.status.pushes[pushes-1].attrs["paused"]; got != true {
		t.Errorf("paused attr = %v, want true", got)
	}

	if err := r.SetPaused(true); err != nil {
		t.Fatal(err)
	}
	if len(fakes.status.pushes) != pushes {
		t.Errorf("pushes = %d, want %d: a no-op press must not push", len(fakes.status.pushes), pushes)
	}

	if err := r.SetPaused(false); err != nil {
		t.Fatal(err)
	}
	last := fakes.status.pushes[len(fakes.status.pushes)-1]
	if got := last.attrs["paused"]; got != false {
		t.Errorf("paused attr after resume = %v, want false", got)
	}
}

// On every push, not only the two the button makes, or an automation
// reading it sees it disappear on the next ordinary cycle.
func TestEveryStatusPushCarriesThePausedAttribute(t *testing.T) {
	usePauseFile(t)
	fakes := newReconcilerFakes()
	r := fakes.reconciler(baseOpts())

	r.tick(context.Background())

	if len(fakes.status.pushes) == 0 {
		t.Fatal("a cycle pushed no status at all")
	}
	for i, push := range fakes.status.pushes {
		if _, ok := push.attrs["paused"]; !ok {
			t.Errorf("push %d carries no paused attribute", i)
		}
	}
}

// --- resuming wakes the loop -------------------------------------------

// Without this the agent sits idle for up to a whole interval after the
// user turns it back on, under a dashboard that says it is running.
func TestResumeNudgesTheWakeChannel(t *testing.T) {
	usePauseFile(t)
	fakes := newReconcilerFakes()
	r := fakes.reconciler(baseOpts())

	if err := r.SetPaused(true); err != nil {
		t.Fatal(err)
	}
	select {
	case <-r.wake:
		t.Fatal("pausing nudged the loop, want only a resume to")
	default:
	}

	if err := r.SetPaused(false); err != nil {
		t.Fatal(err)
	}
	select {
	case <-r.wake:
	default:
		t.Fatal("resuming did not nudge the loop")
	}
}

// The other end of the wire. Waiting out the first cycle puts the loop in
// the select, and the hour interval leaves wake the only way to a second.
func TestRunLoopRunsACycleOnResumeWithoutWaitingOutTheInterval(t *testing.T) {
	usePauseFile(t)
	fakes := newReconcilerFakes()
	opts := baseOpts()
	opts.IntervalMinutes = 60
	r := fakes.reconciler(opts)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.RunLoop(ctx)
	}()
	defer func() {
		cancel()
		<-done
	}()

	// One "in sync" line per cycle, the only per-cycle fact published under
	// a lock - the fakes belong to the loop goroutine.
	awaitCycles(t, r, 1)

	if err := r.SetPaused(true); err != nil {
		t.Fatal(err)
	}
	if err := r.SetPaused(false); err != nil {
		t.Fatal(err)
	}

	awaitCycles(t, r, 2)
}

// awaitCycles blocks until n reconcile cycles have completed, or fails.
func awaitCycles(t *testing.T, r *Reconciler, n int) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		if countEventsContaining(r.Status().Events, "in sync: no changes detected") >= n {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("only %d cycle(s) completed within 5s, want %d",
				countEventsContaining(r.Status().Events, "in sync: no changes detected"), n)
		case <-time.After(time.Millisecond):
		}
	}
}

// --- a pause that lands mid-cycle --------------------------------------

// tick checks the flag once at the top and then plans for minutes, so a
// pause in that window stops the apply but lets the reconcile finish.
func TestPausePressedMidCycleStopsThatCyclesApply(t *testing.T) {
	usePauseFile(t)
	fakes := newReconcilerFakes()
	fakes.differ.changes = []differ.Change{{Path: "automations.yaml", Kind: "update", DiffText: "+x"}}
	opts := baseOpts()
	opts.DryRun = false
	r := fakes.reconciler(opts)
	// The last seam before tick decides whether to apply.
	fakes.differ.beforeCompute = func() {
		if err := r.SetPaused(true); err != nil {
			t.Error(err)
		}
	}

	r.tick(context.Background())

	if len(fakes.applier.applyCalls) != 0 {
		t.Errorf("apply_calls = %v, want none: a pause during the cycle must stop its apply", fakes.applier.applyCalls)
	}
	if got := r.Status().PendingCount; got != 1 {
		t.Errorf("pending_count = %d, want 1: the check that already ran still reports what it found", got)
	}
}

// The deferred re-arm runs on every path out of tick, so an unconditional
// one would restore the countdown and later ticks return before the defer.
func TestPausePressedMidCycleLeavesTheCountdownCleared(t *testing.T) {
	usePauseFile(t)
	fakes := newReconcilerFakes()
	r := fakes.reconciler(baseOpts())
	fakes.differ.beforeCompute = func() {
		if err := r.SetPaused(true); err != nil {
			t.Error(err)
		}
	}

	r.tick(context.Background())

	if got := r.Status().NextCheckUTC; got != "" {
		t.Errorf("next_check_utc = %q, want it cleared: the cycle in flight re-armed a countdown the pause removed", got)
	}
	// And stays that way, since every later tick returns early.
	r.tick(context.Background())
	if got := r.Status().NextCheckUTC; got != "" {
		t.Errorf("next_check_utc = %q on the next tick, want it still cleared", got)
	}
}

// --- what still has to happen while paused ------------------------------

// Every other push site sits inside an operation a paused agent skips, and
// the States API drops entities across a Core restart.
func TestPausedTickKeepsPushingTheSensor(t *testing.T) {
	usePauseFile(t)
	fakes := newReconcilerFakes()
	r := fakes.reconciler(baseOpts())
	if err := r.SetPaused(true); err != nil {
		t.Fatal(err)
	}
	before := len(fakes.status.pushes)

	r.tick(context.Background())

	if len(fakes.status.pushes) != before+1 {
		t.Fatalf("pushes = %d, want %d: a paused tick must still announce the agent",
			len(fakes.status.pushes), before+1)
	}
	last := fakes.status.pushes[len(fakes.status.pushes)-1]
	if got := last.attrs["paused"]; got != true {
		t.Errorf("paused attr = %v, want true", got)
	}
	// Silent otherwise: a line per interval for the length of the pause.
	if got := countEventsContaining(r.Status().Events, "paused"); got != 1 {
		t.Errorf("events mentioning the pause = %d, want only the one SetPaused wrote", got)
	}
}

// Its first cycle returns doing nothing, so without this nothing pushes
// until somebody presses a button.
func TestNewPushesTheSensorWhenItStartsPaused(t *testing.T) {
	usePauseFile(t)
	if err := os.WriteFile(pausePath, []byte("1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fakes := newReconcilerFakes()

	r := fakes.reconciler(baseOpts())

	if len(fakes.status.pushes) != 1 {
		t.Fatalf("pushes at startup = %d, want 1", len(fakes.status.pushes))
	}
	if got := fakes.status.pushes[0].attrs["paused"]; got != true {
		t.Errorf("paused attr = %v, want true", got)
	}
	if !r.Status().Paused {
		t.Error("the agent did not come back paused")
	}
}

// An unpaused startup keeps its one-push-per-cycle shape.
func TestNewDoesNotPushWhenItStartsRunning(t *testing.T) {
	usePauseFile(t)
	fakes := newReconcilerFakes()

	fakes.reconciler(baseOpts())

	if len(fakes.status.pushes) != 0 {
		t.Errorf("pushes at startup = %d, want none on an agent that is not paused", len(fakes.status.pushes))
	}
}

// The add-on update loop is unattended activity too: a backup, an image
// pull and a restart every six hours is what the button exists to stop.
func TestPausedAddonUpdateCycleChecksNothing(t *testing.T) {
	usePauseFile(t)
	f := newAddonUpdateFakes()
	installedAddon(f, "esphome", "ESPHome Device Builder", "2025.6.0", "2025.7.1")
	r := f.reconciler(autoUpdateOpts("esphome"))
	if err := r.SetPaused(true); err != nil {
		t.Fatal(err)
	}

	r.addonUpdateCycle(context.Background())

	if calls := f.registryApplier.fetchAddonUpdateCalls; len(calls) != 0 {
		t.Errorf("supervisor was asked about %v, want nothing while paused", calls)
	}
	if calls := f.registryApplier.updateAddonCalls; len(calls) != 0 {
		t.Errorf("a paused agent installed %v", calls)
	}
	if got := r.Status().AddonUpdates; len(got) != 0 {
		t.Errorf("addon_updates = %+v, want nothing recorded while paused", got)
	}
}

// And it comes back with the loop rather than needing a resume of its own.
func TestResumedAddonUpdateCycleChecksAgain(t *testing.T) {
	usePauseFile(t)
	f := newAddonUpdateFakes()
	installedAddon(f, "esphome", "ESPHome Device Builder", "2025.6.0", "2025.7.1")
	r := f.reconciler(autoUpdateOpts("esphome"))
	if err := r.SetPaused(true); err != nil {
		t.Fatal(err)
	}
	r.addonUpdateCycle(context.Background())
	if err := r.SetPaused(false); err != nil {
		t.Fatal(err)
	}

	r.addonUpdateCycle(context.Background())

	if len(f.registryApplier.fetchAddonUpdateCalls) == 0 {
		t.Error("a resumed update cycle asked Supervisor nothing")
	}
}

// --- retrying a flag write that failed ----------------------------------

// The dashboard only offers the opposite action, so no press would retry
// the write; the paused tick is the free slot that comes round anyway.
func TestPausedTickRetriesAFailedFlagWrite(t *testing.T) {
	good := usePauseFile(t)
	unwritable := filepath.Join(good, "unwritable", "paused")
	pausePath = unwritable
	fakes := newReconcilerFakes()
	r := fakes.reconciler(baseOpts())

	if err := r.SetPaused(true); err == nil {
		t.Fatal("SetPaused = nil, want the write to have failed")
	}
	if pauseFileExists(t, good) {
		t.Fatal("the flag file exists before the retry")
	}

	// /data comes back.
	pausePath = good
	r.tick(context.Background())

	if !pauseFileExists(t, good) {
		t.Fatal("the paused tick did not retry the write")
	}
	if !hasEventContaining(r.Status().Events, "recorded after all") {
		t.Error("the feed does not retract the earlier 'will not survive a restart'")
	}

	// Once written, not rewritten and not re-announced.
	before := countEventsContaining(r.Status().Events, "recorded after all")
	r.tick(context.Background())
	if got := countEventsContaining(r.Status().Events, "recorded after all"); got != before {
		t.Errorf("recovery events = %d, want %d: the retry must not repeat once it has succeeded", got, before)
	}
}

// The transition already said it once.
func TestPausedTickRetriesSilentlyWhileTheWriteKeepsFailing(t *testing.T) {
	dir := usePauseFile(t)
	pausePath = filepath.Join(dir, "unwritable", "paused")
	fakes := newReconcilerFakes()
	r := fakes.reconciler(baseOpts())
	if err := r.SetPaused(true); err == nil {
		t.Fatal("SetPaused = nil, want the write to have failed")
	}
	before := len(r.Status().Events)

	r.tick(context.Background())
	r.tick(context.Background())

	if got := len(r.Status().Events); got != before {
		t.Errorf("events = %d, want %d: a still-failing retry must stay silent", got, before)
	}
}

// --- locking ------------------------------------------------------------

// SetPaused takes pauseMu and never opLock, so pressing Pause during an
// apply returns instead of blocking.
func TestSetPausedWorksWhileAnOperationHoldsTheLock(t *testing.T) {
	usePauseFile(t)
	fakes := newReconcilerFakes()
	r := fakes.reconciler(baseOpts())

	r.opLock.Lock()
	defer r.opLock.Unlock()

	done := make(chan error, 1)
	go func() { done <- r.SetPaused(true) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("SetPaused did not return while an operation held opLock - it must not take that lock")
	}
	if !r.Status().Paused {
		t.Error("the agent is not paused")
	}
}

// --- reading the flag ---------------------------------------------------

// Treating a directory as the flag would pause the agent permanently:
// neither os.Remove nor os.WriteFile can clear one, so Resume would fail.
func TestADirectoryAtThePausePathIsNotAPause(t *testing.T) {
	path := usePauseFile(t)
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}

	if readPausedFile() {
		t.Error("a directory at the flag path reads as paused, want it ignored")
	}
}

// Fail open on purpose: an agent that stopped checking over a failed stat
// looks like a broken add-on, on a /data nobody could clear the flag from.
func TestAnUnstattableFlagPathReadsAsRunning(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the directory permissions this relies on")
	}
	dir := t.TempDir()
	sealed := filepath.Join(dir, "sealed")
	if err := os.Mkdir(sealed, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(sealed, "paused")
	if err := os.WriteFile(path, []byte("1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sealed, 0); err != nil {
		t.Fatal(err)
	}
	// Restored so t.TempDir's own cleanup can remove the tree.
	t.Cleanup(func() { _ = os.Chmod(sealed, 0o700) })

	previous := pausePath
	pausePath = path
	t.Cleanup(func() { pausePath = previous })

	if readPausedFile() {
		t.Error("an unstattable flag path reads as paused, want it to fail open")
	}
}

// What pauseMu buys: writing the file after unlocking r.mu lets memory and
// disk commit in opposite orders, which reverses on the next restart.
func TestConcurrentOppositePressesLeaveTheFlagAndTheFileAgreeing(t *testing.T) {
	usePauseFile(t)
	fakes := newReconcilerFakes()
	r := fakes.reconciler(baseOpts())

	for round := 0; round < 200; round++ {
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); _ = r.SetPaused(true) }()
		go func() { defer wg.Done(); _ = r.SetPaused(false) }()
		wg.Wait()

		if got, want := pauseFileExists(t, pausePath), r.Status().Paused; got != want {
			t.Fatalf("round %d: flag file present = %v, in-memory paused = %v - they must commit together",
				round, got, want)
		}
	}
}

// statusd.Push waits up to ten seconds on Supervisor, so holding pauseMu
// across it would make the second of two quick presses wait that out.
func TestASlowSensorPushDoesNotBlockTheNextPress(t *testing.T) {
	usePauseFile(t)
	fakes := newReconcilerFakes()
	blocked := make(chan struct{})
	fakes.status.block = blocked
	r := fakes.reconciler(baseOpts())

	// Both presses have to be joined before the test returns: awaitPaused only
	// waits for the in-memory flag, so a press is still inside writePausedFile -
	// reading the package-level pausePath - while usePauseFile's Cleanup restores
	// it. Deferred first so it runs last; close(blocked) has to release the push
	// they are stuck in before Wait can finish.
	var presses sync.WaitGroup
	defer presses.Wait()
	defer close(blocked)

	presses.Add(1)
	go func() { defer presses.Done(); _ = r.SetPaused(true) }()
	awaitPaused(t, r, true)

	// The first press is still stuck inside its push.
	presses.Add(1)
	go func() { defer presses.Done(); _ = r.SetPaused(false) }()
	awaitPaused(t, r, false)
}

// awaitPaused blocks until the agent reports want, or fails.
func awaitPaused(t *testing.T, r *Reconciler, want bool) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for r.Status().Paused != want {
		select {
		case <-deadline:
			t.Fatalf("paused did not become %v within 2s - a press is waiting on a lock it should not need", want)
		case <-time.After(time.Millisecond):
		}
	}
}
