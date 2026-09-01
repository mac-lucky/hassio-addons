// Package recon is the reconcile loop: it owns one reconcile cycle
// end-to-end (fetch, diff, plan registry changes, apply, roll back) and
// exposes it to the web UI and the background loop.
//
// Collaborators (see deps.go) are constructor dependencies defaulting to
// the real implementations, so tests can inject fakes.
package recon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/addonopts"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/applier"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/dashboards"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/differ"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/entities"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/gitsync"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/hacs"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/history"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/options"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/regapply"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/registries"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/secretref"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/subentries"
)

// ConfigRoot is the live Home Assistant config directory. An alias of
// applier.ConfigRoot, the only package allowed to write into it.
const ConfigRoot = applier.ConfigRoot

// secretsRoot is where internal/secretref reads secrets.yaml for
// "secret://<name>" values; a var only so tests can redirect it.
var secretsRoot = ConfigRoot

// eventLogMaxLen bounds the in-memory "recent activity" log Status
// returns.
const eventLogMaxLen = 200

// pruneKeep is how many old Supervisor backups / per-apply stash
// directories a successful apply keeps.
const pruneKeep = 5

// historyKeep is how many completed runs the durable history retains, in
// /data/history.jsonl and in the in-memory mirror of it.
const historyKeep = 200

// historyStatusMax is how many of those runs reach Status. Far below
// historyKeep: every row is re-rendered and re-hashed on every poll, and
// GET /history renders the rest without polling.
const historyStatusMax = 25

// Reconciler owns one reconcile cycle end-to-end and exposes it to the web
// UI and the background loop. A single instance, built by New, is shared
// by RunLoop and internal/web's handlers.
type Reconciler struct {
	opts options.Options

	git             Git
	differ          Differ
	applier         Applier
	snapshot        Snapshot
	status          StatusPusher
	registries      Registries
	registryApplier RegistryApplier
	entities        Entities
	dashboards      Dashboards
	addonOpts       AddonOpts
	flows           Flows
	subentries      Subentries
	hacs            Hacs
	history         History

	// wake asks RunLoop to run its next cycle now - the resume half of the
	// pause control. Buffered 1, only ever sent to non-blocking (see
	// SetPaused); assigned once in New, so it sits outside mu's remit.
	wake chan struct{}

	// mu guards every field below. Always held only for the short
	// duration of a read/write, never across a git/HTTP/websocket call.
	mu              sync.Mutex
	state           string
	lastError       string
	pending         []differ.Change
	pendingRegistry []registries.RegOp
	// pendingAddonRestartOnChange is slug -> restart_on_change from the
	// manifest ReconcileNow last planned against, snapshotted like
	// pendingRegistry so ApplyNow acts on exactly what was planned.
	pendingAddonRestartOnChange map[string]bool
	// pendingHacsRestartPending is the restart-reminder list as the HACS
	// layer left it (state.HacsRestartPending minus every domain now seen
	// loaded). nil means the layer did not run, so an apply leaves it be.
	pendingHacsRestartPending []string
	// lastCycleFailed marks a cycle that ended in failCycle with no plan
	// at all, which ApplyNow refuses on. Not the same as StateError, where
	// a partly-applied registry layer is worth pressing Apply again.
	lastCycleFailed bool
	lastSHA         string
	lastApplyUTC    string
	lastStashDir    string
	// lastStashSummary is the one sentence the Roll Back confirmation
	// quotes about what lastStashDir restores, or "" when none was
	// composed. Built at apply time; Status time would mean stat-ing it.
	lastStashSummary string
	// lastWarnings holds the most recent apply's check_config warnings
	// (applier.Result.Warnings), reset per ApplyNow so it describes
	// exactly one apply. ReconcileNow never touches it.
	lastWarnings string
	// lastDriftBranch mirrors applier.State.LastDriftBranch for display -
	// set forward from a successful commitDriftBack, never hydrated at
	// startup, like lastSHA/lastApplyUTC. lastStashDir IS hydrated (see
	// New), so the Roll Back button survives a restart.
	lastDriftBranch string
	// lastImportSHA/lastImportUTC mirror applier.State's fields of the
	// same names. Unlike lastDriftBranch these ARE hydrated in New, so the
	// repeat-import confirmation survives an add-on restart.
	lastImportSHA string
	lastImportUTC string
	// lastImportError is the last import or preview failure, apart from
	// lastError because it says nothing about whether live matches the
	// repository (see importLive). Rendered in its own callout.
	lastImportError string
	// lastImportPreview is the most recent PreviewImport result, kept for
	// htmx swaps and dropped once an import runs (it describes the before).
	lastImportPreview *ImportPreview
	// lastBackupError is the last pre-apply Supervisor backup failure, or
	// "". Reset by every apply. Apart from lastError because the apply
	// still succeeded: the safety net was missing, nothing is out of sync.
	lastBackupError string
	// addonUpdates is the last update check's per-slug results in
	// auto_update_addons' order - replaced wholesale by CheckAddonUpdates,
	// so a dropped slug stops being reported.
	addonUpdates []AddonUpdateStatus
	// lastAddonUpdate is "<slug> <version>" for the most recent add-on this
	// agent updated, or "". In-memory and forward-only like lastDriftBranch;
	// the per-add-on half lives in addonUpdates[i].LastUpdatedUTC.
	lastAddonUpdate string
	// addonUpdateSelfSlugFailed records that the previous check could not
	// resolve this agent's own slug, so the refusal is logged on the
	// TRANSITION rather than once every check interval.
	addonUpdateSelfSlugFailed bool
	// addonCheckFailed holds the slugs whose last check got no answer out
	// of Supervisor - the same transition guard, per slug so one
	// unreachable add-on cannot silence another's first failure.
	addonCheckFailed map[string]bool
	// lastVersionRecordUTC is when this agent last COMMITTED the add-on
	// version record (see recordAddonVersions), or "". A cycle that found
	// it already correct leaves this alone.
	lastVersionRecordUTC string
	// recordedVersions is slug -> version as of the last successful
	// record, the baseline the next one diffs against. nil until this
	// process has recorded once; see logVersionChanges.
	recordedVersions map[string]string
	// versionRecordFailed records that the previous record attempt failed,
	// so it is logged on the TRANSITION (see noteVersionRecordFailure).
	versionRecordFailed bool
	// captureFailed records that the previous capture could not push or
	// park, on the same transition guard and for a sharper version of its
	// reason: capture runs every cycle, so an unpushable repository would
	// otherwise fill the feed within hours (see noteCaptureFailure).
	captureFailed bool
	// lastCaptureUTC/lastCaptureSHA mirror applier.State's fields for
	// display, hydrated in New like lastImportSHA/lastImportUTC so the last
	// capture survives a restart.
	lastCaptureUTC string
	lastCaptureSHA string
	// conflicts mirrors applier.State's ConflictedPaths, sorted, with the
	// branch their live copies were parked on beside it - one branch per
	// parking, not per path. refreshStateMirrors is the only writer.
	conflicts          []string
	lastConflictBranch string
	lastConflictUTC    string
	// hacsUnavailable records that HACS is not installed - the one layer
	// failure that skips its layer instead of ending the cycle (see
	// planHacsLayer). Guarded on the transition, and a dashboard chip.
	hacsUnavailable bool
	events          []Event

	// blocked mirrors the failure memory in /data/state.json, sorted by
	// key. refreshStateMirrors is the only writer, and rebuilds it wherever
	// an operation already holds a fresh applier.State.
	blocked []BlockedItem
	// addonRestartOnChange mirrors state.AddonRestartOnChange with the
	// "addon:" prefix stripped - what a restore op restarts off once the
	// manifest entry that declared it is gone. refreshStateMirrors only.
	addonRestartOnChange map[string]bool
	// managed mirrors /data/state.json's ownership records - which files
	// and live objects this agent manages - as names and keys only. Written
	// only by refreshStateMirrors; see managedInventory.
	managed ManagedInventory
	// hacsRestartPending mirrors state.HacsRestartPending, sorted - HACS
	// domains Home Assistant has not loaded yet. Also written by the cycle
	// that prunes it, so a reminder need not wait for the next apply.
	hacsRestartPending []string
	// hacsLoaded holds domains seen loaded whose removal from the reminder
	// list is not on disk yet; refreshStateMirrors subtracts it so a
	// rebuild from disk cannot resurrect a retired reminder. Cleared by the
	// apply that persists the pruned list.
	hacsLoaded map[string]bool

	// nextTickUTC is when the unattended loop expects its next cycle, or ""
	// before the first has finished. Written at the end of tick, since it
	// is the cycle FINISHING that re-arms the interval. Display only.
	nextTickUTC string

	// paused is whether the unattended loop is switched off - see SetPaused
	// and pause.go. Hydrated from the flag file in New; the in-memory flag
	// is this process's authority even if the disk write failed.
	paused bool
	// pauseFileDirty records that paused and the flag file disagree because
	// the write failed. No button retries it (SetPaused is idempotent), so
	// the paused branch of tick does, every interval.
	pauseFileDirty bool

	// runs mirrors the durable run history, oldest-first, bounded by
	// historyKeep. Hydrated once from disk in New and extended in memory,
	// never re-read per Status call.
	runs []history.Record
	// historyWriteFailed records that the previous history append failed,
	// so it is logged on the TRANSITION rather than once per run. See
	// noteHistoryWriteFailure.
	historyWriteFailed bool
	// importRecordFailed records that an import was pushed but its record
	// could not be saved, which a restart would silently undo. Same
	// transition guard, but only a later import retires it - nothing
	// retries the write. See noteImportRecordFailure.
	importRecordFailed bool

	// selfAddonSlug/selfAddonSlugResolved cache the Supervisor slug this
	// agent runs as (see resolveSelfAddonSlug). A failed resolution is
	// never cached, so a hiccup cannot disable the self-protection guard.
	selfAddonSlug         string
	selfAddonSlugResolved bool

	// opLock guards ReconcileNow/ApplyNow/Rollback so at most one runs at a
	// time. Acquired non-blocking, so a busy operation never makes the web
	// UI hang - it just reports "busy".
	opLock sync.Mutex

	// checkLock is opLock's equivalent for CheckAddonUpdates alone - see
	// that method for why the check needs a lock of its own.
	checkLock sync.Mutex

	// addonCheckRunning mirrors checkLock for readers, set and cleared by
	// the one goroutine that holds it. Status is polled every few seconds
	// by every open tab, and probing the lock to answer would both report
	// false positives and steal presses - see checkRunning.
	addonCheckRunning atomic.Bool

	// pauseMu serializes SetPaused (and tick's retry) end to end, flag and
	// file together: not opLock, which pause must work underneath, and not
	// r.mu, which is never held across the I/O this section needs inside it.
	pauseMu sync.Mutex
}

// New builds a Reconciler for opts. Any nil field of deps is filled in
// with the real, production implementation of that collaborator.
func New(opts options.Options, deps Deps) *Reconciler {
	r := &Reconciler{
		opts:            opts,
		git:             deps.Git,
		differ:          deps.Differ,
		applier:         deps.Applier,
		snapshot:        deps.Snapshot,
		status:          deps.Status,
		registries:      deps.Registries,
		registryApplier: deps.RegistryApplier,
		entities:        deps.Entities,
		dashboards:      deps.Dashboards,
		addonOpts:       deps.AddonOpts,
		flows:           deps.Flows,
		subentries:      deps.Subentries,
		hacs:            deps.Hacs,
		history:         deps.History,

		addonCheckFailed: map[string]bool{},
		hacsLoaded:       map[string]bool{},
		wake:             make(chan struct{}, 1),
	}

	if r.git == nil {
		g := gitsync.New(opts, gitsync.DefaultWorkdir)
		g.Crypter = deps.Crypter
		r.git = newRealGit(g)
	}
	// Both seams below decrypt through this, built from r.git's worktree so
	// the path handed to sops is the one those bytes came from. nil without
	// an age key, which keeps differ.Compute's refusal reachable.
	transform := repoDecryptTransform(deps.Crypter, r.git.Workdir())
	if r.differ == nil {
		r.differ = realDiffer{transform: transform}
	}
	if r.applier == nil {
		cfg := applier.DefaultConfig()
		cfg.TransformRepoFile = applierRepoTransform(transform)
		r.applier = &realApplier{cfg: cfg}
	}
	if r.snapshot == nil {
		r.snapshot = realSnapshot{}
	}
	if r.status == nil {
		r.status = realStatusPusher{}
	}
	if r.registries == nil {
		r.registries = realRegistries{}
	}
	if r.registryApplier == nil {
		r.registryApplier = newRealRegistryApplier(regapply.NewDialer())
	}
	if r.entities == nil {
		r.entities = realEntities{}
	}
	if r.dashboards == nil {
		r.dashboards = realDashboards{}
	}
	if r.addonOpts == nil {
		r.addonOpts = realAddonOpts{}
	}
	if r.flows == nil {
		r.flows = realFlows{}
	}
	if r.subentries == nil {
		r.subentries = realSubentries{}
	}
	if r.hacs == nil {
		r.hacs = realHacs{}
	}
	if r.history == nil {
		r.history = history.Open(history.DefaultPath, historyKeep)
	}

	if opts.RepoURL != "" {
		r.state = StateInSync
	} else {
		r.state = StateDisabled
	}

	// Hydrated from disk, unlike the other display mirrors: the web UI's
	// repeat-import confirmation is only useful if it outlives a restart.
	persisted := r.applier.StateLoad()
	r.lastImportSHA = persisted.LastImportSHA
	r.lastImportUTC = persisted.LastImportUTC
	// The rollback pointer too, but only while the directory it names still
	// exists: the stash directories survive a restart, and a restart is
	// exactly how people try to recover from a bad apply.
	if persisted.LastStashDir != "" {
		if info, err := os.Stat(persisted.LastStashDir); err == nil && info.IsDir() {
			r.lastStashDir = persisted.LastStashDir
			r.lastStashSummary = persisted.LastStashSummary
		}
	}
	// Likewise the blocked list: an item blocked by the previous process is
	// still blocked, and the first cycle is minutes away on a slow clone.
	r.refreshStateMirrors(persisted)

	// The run history's whole purpose is to outlive the process. This is
	// the only read of the file the agent ever does.
	r.runs = r.history.Load()

	// A pause that did not survive a restart would be one a user could not
	// trust: a restart is exactly when the loop would resume. See pause.go.
	r.paused = readPausedFile()

	// Gated the way RunAddonUpdateLoop gates itself, so a file left by an
	// old config cannot repopulate a card the dashboard no longer shows.
	// Without it the card says nothing until the first check, two minutes
	// in (see addonUpdateStartupDelay).
	if len(opts.AutoUpdateAddons) > 0 {
		r.addonUpdates = hydrateAddonUpdates(opts.AutoUpdateAddons, readAddonUpdatesFile())
	}

	// A paused agent logs nothing per interval on purpose (see tick), so
	// this line is all that separates it from one that looks broken.
	msg := "agent started"
	switch {
	case opts.RepoURL == "":
		// The stronger fact: with no repository the loop never starts, so
		// calling it paused would mislead.
		msg = "agent started (repo_url not configured)"
	case r.paused:
		msg = "agent started (paused - automatic checks are off)"
	}
	r.logEvent(msg)

	if r.paused {
		// The one startup that must push the sensor itself: this process's
		// first cycle returns without doing anything, so nothing else would
		// announce the agent until somebody pressed a button.
		r.pushStatus()
	}
	return r
}

// -- internal helpers -------------------------------------------------

// utcISO renders a timestamp the one way this package puts them on the
// wire: UTC, RFC3339. Every Status string field and event TS goes through
// here or utcNowISO.
func utcISO(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

func utcNowISO() string {
	return utcISO(time.Now())
}

// joinErrors joins the non-empty parts with "; ".
func joinErrors(parts ...string) string {
	var nonEmpty []string
	for _, p := range parts {
		if p != "" {
			nonEmpty = append(nonEmpty, p)
		}
	}
	out := ""
	for i, p := range nonEmpty {
		if i > 0 {
			out += "; "
		}
		out += p
	}
	return out
}

// regOpIdentity renders op as "<kind> <rtype>:<key>", the same label
// regapply.RegistryApplyResult.Applied uses, so a plan op is directly
// comparable against an Applied entry.
func regOpIdentity(op registries.RegOp) string {
	return fmt.Sprintf("%s %s:%s", op.Kind, op.RType, op.Key)
}

// nullable returns nil for "" so a status attribute serializes as JSON
// null. Only for the statusd.Push boundary, not Status()'s own "" convention.
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// countByRType counts ops whose RType matches rtype - one layer's share
// of pendingRegistry's merged list.
func countByRType(ops []registries.RegOp, rtype string) int {
	n := 0
	for _, op := range ops {
		if op.RType == rtype {
			n++
		}
	}
	return n
}

// countAddonUpdatesAvailable counts watched add-ons behind their store's
// newest version - the sensor's addon_updates_available. Callers hold r.mu.
func countAddonUpdatesAvailable(updates []AddonUpdateStatus) int {
	n := 0
	for _, u := range updates {
		if u.UpdateAvailable {
			n++
		}
	}
	return n
}

// withMu runs f with r.mu held, for sites that take the lock only to
// assign a field or two. Unlocks via defer, so a panic inside f cannot
// leave the mutex held - tick recovers from panics and keeps looping.
func (r *Reconciler) withMu(f func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f()
}

// eventMaxLen bounds one activity entry. Entries quote foreign text - a
// git error, a Supervisor body, a request parameter - and the ring is
// rendered and hashed by every 5-second fragment poll, so an unbounded
// entry is both a memory and a render cost.
const eventMaxLen = 2000

// logEvent appends one line to the bounded recent-activity log and the
// process log. Takes r.mu, so a caller must not hold it. Refusals log here
// too - the web UI re-renders identically either way.
func (r *Reconciler) logEvent(message string) {
	if len(message) > eventMaxLen {
		message = message[:eventMaxLen] + "... (truncated)"
	}
	entry := Event{TS: utcNowISO(), Message: message}
	r.mu.Lock()
	r.events = append(r.events, entry)
	if len(r.events) > eventLogMaxLen {
		r.events = r.events[len(r.events)-eventLogMaxLen:]
	}
	r.mu.Unlock()
	slog.Info(message)
}

// pushStatus pushes state/attrs to sensor.gitops_agent_status on every
// call, not just on a transition - what makes the sensor self-healing
// across a Home Assistant restart.
func (r *Reconciler) pushStatus() {
	r.mu.Lock()
	state := r.state
	attrs := map[string]any{
		"last_sha":                nullable(r.lastSHA),
		"last_apply_utc":          nullable(r.lastApplyUTC),
		"pending_changes":         len(r.pending) + len(r.pendingRegistry),
		"pending_registry_ops":    len(r.pendingRegistry),
		"pending_dashboard_ops":   countByRType(r.pendingRegistry, "dashboard"),
		"pending_addon_ops":       countByRType(r.pendingRegistry, "addon"),
		"pending_integration_ops": countByRType(r.pendingRegistry, "integration"),
		"pending_subentry_ops":    countByRType(r.pendingRegistry, "subentry"),
		"pending_hacs_ops":        countByRType(r.pendingRegistry, rtypeHacs),
		"error":                   nullable(r.lastError),
		"warnings":                nullable(r.lastWarnings),
		"last_drift_branch":       nullable(r.lastDriftBranch),
		// Conflicts are held out of pending_changes - they are in no plan -
		// so without their own count the one thing that actually needs a
		// person would publish drift_pending with pending_changes 0.
		"conflicts": len(r.conflicts),
		// The one durable, automatable fact about an import; the SHA is
		// redundant once the next reconcile moves last_sha onto it.
		"last_import_utc": nullable(r.lastImportUTC),
		// The two add-on-update facts an automation can act on. Per-slug
		// results stay off the sensor: a list attribute that reshuffles
		// every check would flap for nothing the dashboard lacks.
		"addon_updates_available": countAddonUpdatesAvailable(r.addonUpdates),
		"last_addon_update":       nullable(r.lastAddonUpdate),
		// An attribute, not a state: statusd.States is a closed vocabulary
		// and a paused agent is still in_sync or drift_pending. Read under
		// the same lock as the state so the two describe one moment.
		"paused": r.paused,
	}
	r.mu.Unlock()

	if _, err := r.status.Push(state, attrs); err != nil {
		slog.Warn("statusd.push failed", "state", state, "error", err)
	}
}

// Busy reports whether an operation holds opLock right now - the web
// layer's cheap early-out, where Status would clone every display mirror
// under mu just to read one flag.
func (r *Reconciler) Busy() bool { return r.busy() }

// busy reports whether opLock is currently held, without blocking on it:
// TryLock plus immediate Unlock, sync.Mutex having no inspector.
func (r *Reconciler) busy() bool {
	if r.opLock.TryLock() {
		r.opLock.Unlock()
		return false
	}
	return true
}

// checkRunning reports whether an add-on update check is in flight, which
// busy cannot see: the check runs outside opLock and takes it only while
// installing (see CheckAddonUpdates).
//
// Reads a flag rather than probing checkLock the way busy probes opLock,
// and the difference is deliberate. TryLock+Unlock answers "could I take
// it", not "is it held": two Status calls landing together make the
// second one's TryLock fail, so it reports a check that is not running -
// measured at 69 false positives in two seconds under eight concurrent
// callers. That would disable the Check button and render "checking now"
// for one poll, changing the fragment hash and re-swapping #app for
// nothing. Worse, and rarer, a Status call holding checkLock for its own
// inspection can make a real button press lose its TryLock and be
// refused. A flag contends with nobody and cannot do either.
//
// busy keeps the TryLock idiom because opLock is genuinely contended by
// the operations themselves; this one is not worth the same trade.
func (r *Reconciler) checkRunning() bool {
	return r.addonCheckRunning.Load()
}

// waitIdlePollInterval is how often WaitIdle re-checks opLock. A var so
// tests can shrink it.
var waitIdlePollInterval = 50 * time.Millisecond

// WaitIdle blocks until no ReconcileNow/ApplyNow/Rollback is in flight,
// or ctx is done - so main() can exit without cutting off a web- or
// webhook-triggered operation, which runs detached from its request (see
// web.opRoute) and is tracked by nothing else. Polls via TryLock: holding
// the lock would block the operation it is waiting on.
func (r *Reconciler) WaitIdle(ctx context.Context) error {
	if r.opLock.TryLock() {
		r.opLock.Unlock()
		return nil
	}

	ticker := time.NewTicker(waitIdlePollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if r.opLock.TryLock() {
				r.opLock.Unlock()
				return nil
			}
		}
	}
}

// isPaused reports whether the unattended loop is currently switched off.
// Takes r.mu itself, so a caller must NOT already hold it.
func (r *Reconciler) isPaused() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.paused
}

// SetPaused switches the unattended loop off or back on - Flux's
// `suspend` semantics, not a kill switch: it stops only tick's own cycle,
// while every hand-triggered route and the webhook keep working.
// Idempotent, so a second press writes no event. A failed disk write does
// not undo the flag; the loop stays off and tick retries the write.
func (r *Reconciler) SetPaused(paused bool) error {
	changed, err := r.commitPaused(paused)
	if !changed {
		return err
	}

	// Outside pauseMu: statusd.Push waits up to statusd.Timeout, and the
	// pause control must answer instantly. Pushed BEFORE the nudge, which
	// is load bearing - the woken cycle pushes a status of its own, and the
	// two racing could leave the sensor reporting a pause already lifted.
	r.pushStatus()

	if !paused {
		// Non-blocking, and correct when it does not send: a token already
		// in the buffer means a cycle is queued, which is all this asks.
		select {
		case r.wake <- struct{}{}:
		default:
		}
	}
	return err
}

// commitPaused is SetPaused's critical section: the in-memory flag, the
// flag file, and the feed lines that describe them, committed as one under
// pauseMu. Reports whether anything actually changed, so the caller knows
// whether there is anything to announce.
func (r *Reconciler) commitPaused(paused bool) (changed bool, err error) {
	r.pauseMu.Lock()
	defer r.pauseMu.Unlock()

	r.mu.Lock()
	if r.paused == paused {
		r.mu.Unlock()
		return false, nil
	}
	r.paused = paused
	if paused {
		// The countdown describes a cycle that is no longer coming, so it
		// goes with the flag. A cycle already in flight would otherwise put
		// it straight back on its way out - see tick's deferred re-arm,
		// which is conditional on this same flag for that reason.
		r.nextTickUTC = ""
	}
	r.mu.Unlock()

	err = writePausedFile(paused)
	r.withMu(func() { r.pauseFileDirty = err != nil })
	if err != nil {
		slog.Warn("could not record the pause flag", "path", pausePath, "paused", paused, "error", err)
		// In the feed as well as the process log, because it changes what
		// the user can rely on: the button did what it says for now, and a
		// restart would disagree - in whichever direction this press was
		// going.
		r.logEvent(fmt.Sprintf("%s: %v - %s", pauseWriteFailedPrefix(paused), err, pauseWriteFailedEffect(paused)))
	}

	// Inside the lock, unlike the status push: the feed is a sequence, and
	// two opposite presses that interleaved here would leave it claiming
	// the agent was resumed and then paused when the opposite is what
	// stuck. slog and an in-memory append, so nothing here can block.
	if paused {
		r.logEvent("automatic checks paused - manual actions still work")
	} else {
		r.logEvent("automatic checks resumed")
	}
	return true, err
}

// pauseWriteFailedPrefix and pauseWriteFailedEffect word a failed flag
// write for the direction it was going. One message for both would be
// wrong half the time: failing to CREATE the file loses a pause, failing
// to REMOVE it keeps one the user just cleared - opposite surprises, and
// the second is the one that would look like the add-on ignoring a button.
func pauseWriteFailedPrefix(paused bool) string {
	if paused {
		return "could not record the pause in " + pausePath
	}
	return "could not clear the pause flag in " + pausePath
}

func pauseWriteFailedEffect(paused bool) string {
	if paused {
		return "the pause will not survive a restart"
	}
	return "the agent will come back paused after a restart"
}

// retryPauseFile re-attempts a flag write that failed. Called only from
// tick's paused branch, the one slot where it is free and reachable - no
// press retries it. Silent on continued failure; the transition already
// logged one event.
func (r *Reconciler) retryPauseFile() {
	r.pauseMu.Lock()
	defer r.pauseMu.Unlock()

	r.mu.Lock()
	dirty, paused := r.pauseFileDirty, r.paused
	r.mu.Unlock()
	if !dirty {
		return
	}

	if err := writePausedFile(paused); err != nil {
		return
	}
	r.withMu(func() { r.pauseFileDirty = false })
	// Retracts what the failure logged; dirty is now clear, so this cannot
	// repeat until the next failure.
	r.logEvent("the pause flag was recorded after all - it will survive a restart")
}

func (r *Reconciler) snapshotPending() []differ.Change {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]differ.Change(nil), r.pending...)
}

// resolveSelfAddonSlug returns the cached selfAddonSlug, resolving it via
// RegistryApplier.FetchSelfAddonSlug on first use - so Supervisor is
// called once per process, and a failed resolution is retried next time.
func (r *Reconciler) resolveSelfAddonSlug(ctx context.Context) (string, error) {
	r.mu.Lock()
	if r.selfAddonSlugResolved {
		slug := r.selfAddonSlug
		r.mu.Unlock()
		return slug, nil
	}
	r.mu.Unlock()

	slug, err := r.registryApplier.FetchSelfAddonSlug(ctx)
	if err != nil {
		return "", err
	}

	r.withMu(func() {
		r.selfAddonSlug = slug
		r.selfAddonSlugResolved = true
	})
	return slug, nil
}

// -- public API ---------------------------------------------------------

// Status returns the current status dict for the web UI and
// StatusPusher.Push.
func (r *Reconciler) Status() Status {
	r.mu.Lock()
	state := r.state
	lastError := r.lastError
	pending := append([]differ.Change(nil), r.pending...)
	pendingRegistry := append([]registries.RegOp(nil), r.pendingRegistry...)
	lastSHA := r.lastSHA
	lastApplyUTC := r.lastApplyUTC
	lastStashDir := r.lastStashDir
	lastStashSummary := r.lastStashSummary
	lastWarnings := r.lastWarnings
	lastDriftBranch := r.lastDriftBranch
	lastImportSHA := r.lastImportSHA
	lastImportUTC := r.lastImportUTC
	lastImportError := r.lastImportError
	lastImportPreview := r.lastImportPreview
	lastBackupError := r.lastBackupError
	lastVersionRecordUTC := r.lastVersionRecordUTC
	nextTickUTC := r.nextTickUTC
	// Read beside nextTickUTC, which SetPaused clears under the same lock:
	// "paused" plus a countdown would promise a cycle that is not coming.
	paused := r.paused
	// Copied out so a caller cannot reach the live results. make+copy
	// rather than append-to-nil: a nil slice serializes as null while every
	// sibling list emits [], which the UI would have to special-case.
	addonUpdates := make([]AddonUpdateStatus, len(r.addonUpdates))
	copy(addonUpdates, r.addonUpdates)
	blocked := make([]BlockedItem, len(r.blocked))
	copy(blocked, r.blocked)
	conflicts := make([]string, len(r.conflicts))
	copy(conflicts, r.conflicts)
	lastConflictBranch := r.lastConflictBranch
	lastConflictUTC := r.lastConflictUTC
	lastCaptureSHA := r.lastCaptureSHA
	lastCaptureUTC := r.lastCaptureUTC
	captureFailing := r.captureFailed
	managed := r.managed.clone()
	hacsRestartPending := make([]string, len(r.hacsRestartPending))
	copy(hacsRestartPending, r.hacsRestartPending)
	// Read under the same lock as pendingRegistry, so the slugs describe
	// the plan this Status reports rather than a later one.
	pendingRestarts := pendingRestartSlugs(pendingRegistry, r.pendingAddonRestartOnChange, r.addonRestartOnChange)
	historyWriteFailing := r.historyWriteFailed
	versionRecordFailing := r.versionRecordFailed
	importRecordFailing := r.importRecordFailed
	hacsUnavailable := r.hacsUnavailable
	addonCheckFailing := sortedTrueKeys(r.addonCheckFailed)
	addonUpdateSelfSlugFailing := r.addonUpdateSelfSlugFailed
	events := append([]Event(nil), r.events...)
	runs := newestRuns(r.runs, historyStatusMax)
	// Read under the same lock as runs, so the count and the rows come
	// from one ring.
	historyTotal := len(r.runs)
	r.mu.Unlock()

	lastSHAShort := history.ShortSHA(lastSHA)

	pendingChanges := make([]PendingChange, len(pending))
	for i, c := range pending {
		pendingChanges[i] = PendingChange{Path: c.Path, Kind: c.Kind, DiffText: c.DiffText}
	}
	pendingRegOps := make([]PendingRegOp, len(pendingRegistry))
	for i, op := range pendingRegistry {
		pendingRegOps[i] = PendingRegOp{RType: op.RType, Key: op.Key, Kind: op.Kind, DiffText: op.DiffText, Error: op.Error}
	}

	return Status{
		State:               state,
		Busy:                r.busy(),
		Configured:          r.opts.RepoURL != "",
		DryRun:              r.opts.DryRun,
		RepoURL:             r.opts.RepoURL,
		Branch:              r.opts.Branch,
		IntervalMinutes:     r.opts.IntervalMinutes,
		LastSHA:             lastSHA,
		LastSHAShort:        lastSHAShort,
		LastApplyUTC:        lastApplyUTC,
		LastStashDir:        lastStashDir,
		RollbackPreview:     lastStashSummary,
		LastError:           lastError,
		LastBackupError:     lastBackupError,
		Warnings:            lastWarnings,
		CommitBackEnabled:   r.opts.CommitBack,
		LastDriftBranch:     lastDriftBranch,
		ImportEnabled:       r.opts.AllowImport,
		CaptureEnabled:      r.opts.CaptureLiveChanges,
		LastCaptureUTC:      lastCaptureUTC,
		LastCaptureSHA:      lastCaptureSHA,
		LastCaptureSHAShort: history.ShortSHA(lastCaptureSHA),
		Conflicts:           conflicts,
		ConflictBranch:      lastConflictBranch,
		ConflictUTC:         lastConflictUTC,
		LastImportSHA:       lastImportSHA,
		LastImportSHAShort:  history.ShortSHA(lastImportSHA),
		LastImportUTC:       lastImportUTC,
		LastImportError:     lastImportError,
		ImportPreview:       lastImportPreview,
		AutoUpdateEnabled:   len(r.opts.AutoUpdateAddons) > 0,
		AddonUpdates:        addonUpdates,
		// Outside the locked block, beside Busy: both are lock inspectors,
		// and TryLock-ing another mutex under r.mu would order the two.
		AddonCheckRunning: r.checkRunning(),
		// No lock: addonUpdateCheckInterval is fixed for the life of the
		// process. Seconds because the client compares it against an age in
		// milliseconds; a test-shrunk interval truncating to 0 means "no
		// stale marker".
		AddonCheckIntervalSeconds: int(r.addonCheckInterval().Seconds()),

		LastVersionRecordUTC: lastVersionRecordUTC,
		NextCheckUTC:         nextTickUTC,
		Paused:               paused,

		PendingCount:        len(pending) + len(pendingRegistry),
		Pending:             pendingChanges,
		PendingRegistry:     pendingRegOps,
		Blocked:             blocked,
		Managed:             managed,
		HacsRestartPending:  hacsRestartPending,
		PendingRestartSlugs: pendingRestarts,

		HistoryWriteFailing:        historyWriteFailing,
		VersionRecordFailing:       versionRecordFailing,
		CaptureFailing:             captureFailing,
		ImportRecordFailing:        importRecordFailing,
		HacsUnavailable:            hacsUnavailable,
		AddonCheckFailing:          addonCheckFailing,
		AddonUpdateSelfSlugFailing: addonUpdateSelfSlugFailing,

		Events:       events,
		History:      runs,
		HistoryTotal: historyTotal,
	}
}

// HistoryAll is every run this process knows about, newest-first - the
// in-memory mirror of /data/history.jsonl, what GET /history renders. A
// copy, since r.runs is the live ring and outlives every caller.
func (r *Reconciler) HistoryAll() []history.Record {
	r.mu.Lock()
	defer r.mu.Unlock()
	return newestRuns(r.runs, len(r.runs))
}

// pendingRestartSlugs is which add-ons an apply of ops would restart,
// reading intent from the same two sources regapply's executeAddonOp
// does: declared for a normal op, managed for a RESTORE, whose manifest
// entry is gone by then. Error-kind ops never execute. Callers hold r.mu.
func pendingRestartSlugs(ops []registries.RegOp, declared, managed map[string]bool) []string {
	slugs := map[string]bool{}
	for _, op := range ops {
		if op.RType != "addon" || op.Kind == registries.KindError {
			continue
		}
		intent := declared
		if op.Kind == addonopts.KindRestore {
			intent = managed
		}
		if intent[op.Key] {
			slugs[op.Key] = true
		}
	}
	return sortedStringKeys(slugs)
}

// sortedTrueKeys is the keys of m whose value is true, sorted - every
// map-derived Status slice must be, since the polled fragment is compared
// byte for byte. Callers hold r.mu.
func sortedTrueKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for key, set := range m {
		if set {
			out = append(out, key)
		}
	}
	sort.Strings(out)
	return out
}

// newestRuns copies at most n records off the end of an oldest-first
// history and returns them newest-first, the order Status promises.
// make+copy so an empty history serializes as [] rather than null.
func newestRuns(runs []history.Record, n int) []history.Record {
	if n > len(runs) {
		n = len(runs)
	}
	out := make([]history.Record, n)
	copy(out, runs[len(runs)-n:])
	slices.Reverse(out)
	return out
}

// desiredNonEmpty is true if desired declares anything at all. A bare
// "input_boolean:" key with no items counts: Helpers is non-empty as a
// map even though it maps to zero items.
func desiredNonEmpty(d registries.Desired) bool {
	return len(d.Floors) > 0 || len(d.Areas) > 0 || len(d.Labels) > 0 || len(d.Helpers) > 0
}

// registryLayerHasWork decides whether ReconcileNow opens a websocket at
// all. Not just desiredNonEmpty: emptying the manifest must still plan
// rule-4 deletes for whatever this agent created or adopted.
func registryLayerHasWork(desired registries.Desired, state applier.State) bool {
	return desiredNonEmpty(desired) || len(state.RegistryManaged) > 0
}

// dashboardContentIDsToFetch returns every declared dashboard id whose
// config file loaded - the set dashboards.Plan needs live content for. A
// failed config is already destined to be a KindError op, so it skips the
// round trip.
func dashboardContentIDsToFetch(desired dashboards.Desired) []string {
	var ids []string
	for _, item := range desired.Dashboards {
		id, _ := item["id"].(string)
		if content, ok := desired.Content[id]; ok && content.Err == "" {
			ids = append(ids, id)
		}
	}
	return ids
}

// addonSlugsToFetch returns every slug addonopts.Plan needs live
// Supervisor info for: every declared slug plus every slug in originals,
// so a slug the manifest just dropped can still be planned as a restore.
// Deduplicated and sorted for a deterministic fetch order.
func addonSlugsToFetch(desired addonopts.Desired, originals map[string]map[string]any) []string {
	seen := map[string]bool{}
	for _, item := range desired.Addons {
		if slug, _ := item["slug"].(string); slug != "" {
			seen[slug] = true
		}
	}
	for key := range originals {
		if slug := strings.TrimPrefix(key, "addon:"); slug != "" {
			seen[slug] = true
		}
	}
	slugs := make([]string, 0, len(seen))
	for slug := range seen {
		slugs = append(slugs, slug)
	}
	sort.Strings(slugs)
	return slugs
}

// toApplierChanges maps differ.Change to applier's field-identical Change
// - the two packages are decoupled on purpose, and this is the seam.
func toApplierChanges(changes []differ.Change) []applier.Change {
	out := make([]applier.Change, len(changes))
	for i, c := range changes {
		out[i] = applier.Change{Path: c.Path, Kind: c.Kind, DiffText: c.DiffText}
	}
	return out
}

// failCycle ends a cycle that could not run to completion: err becomes
// lastError, state becomes StateError, and the pending plan is discarded
// as a set - a stale plan describes a tree that is no longer checked out,
// and applying it would report in_sync on an error nothing resolved.
// Taking run as a parameter keeps reconcileNow to one record per path out.
func (r *Reconciler) failCycle(run *runRecorder, err error) []differ.Change {
	r.withMu(func() {
		r.lastError = err.Error()
		r.state = StateError
		r.pending = nil
		r.pendingRegistry = nil
		r.pendingAddonRestartOnChange = nil
		r.pendingHacsRestartPending = nil
		r.lastCycleFailed = true
	})
	r.logEvent("error: " + err.Error())
	run.finish(history.Record{Outcome: history.OutcomeError, Error: err.Error()})
	r.pushStatus()
	return nil
}

// unseededCycle ends a cycle that found no such branch on the remote.
// Clears the stale plan like failCycle but also clears lastError, because
// nothing is wrong: the state carries the condition instead.
//
// The event fires only on the transition in, and no history row is written
// at all (run.discard), because this repeats every interval until somebody
// seeds the repository - a row per tick is how a card stops being read.
//
// lastCycleFailed is still set: there is no plan, so an apply must refuse
// rather than apply nothing and end by declaring in_sync.
func (r *Reconciler) unseededCycle(run *runRecorder) []differ.Change {
	var first bool
	r.withMu(func() {
		first = r.state != StateUnseeded
		r.state = StateUnseeded
		r.lastError = ""
		r.pending = nil
		r.pendingRegistry = nil
		r.pendingAddonRestartOnChange = nil
		r.pendingHacsRestartPending = nil
		r.lastCycleFailed = true
	})
	if first {
		r.logEvent(fmt.Sprintf(
			"nothing to sync yet: branch %s does not exist in the repository - import to seed it, or check the branch name",
			r.opts.Branch))
	}
	run.discard()
	r.pushStatus()
	return nil
}

// ReconcileNow runs one fetch + diff cycle immediately: fetch, guard
// against tracked secrets (on the RAW list, never TrackedFiles), check
// out, diff against ConfigRoot, plan each enabled registry layer, then
// update the pending lists and push status. An early exit empties those
// lists (see failCycle). Never lets a bad cycle propagate to the caller.
func (r *Reconciler) ReconcileNow(ctx context.Context) []differ.Change {
	if !r.opLock.TryLock() {
		// The one busy refusal that writes no event: the tick and the
		// webhook can both fire repeatedly under a long apply, and neither
		// is a user waiting on a page. Hands back the current plan, so
		// nothing is lost.
		return r.snapshotPending()
	}
	defer r.opLock.Unlock()

	return r.reconcileNow(ctx)
}

// reconcileNow is ReconcileNow's body, minus the lock. Callers must
// already hold opLock. Split out for ImportLive, which needs the import
// and the refresh after it to be one operation - the public entry point
// would quietly hand back the pre-import snapshot instead.
func (r *Reconciler) reconcileNow(ctx context.Context) []differ.Change {
	run := r.beginRun(history.KindReconcile)
	defer run.abandon()

	if err := r.git.EnsureClone(ctx); err != nil {
		return r.failCycle(run, err)
	}
	sha, err := r.git.Fetch(ctx)
	if err != nil {
		if errors.Is(err, gitsync.ErrRemoteBranchMissing) {
			return r.unseededCycle(run)
		}
		return r.failCycle(run, err)
	}
	// From here on every exit knows its commit. The two failures above
	// have none to name, and naming the previous cycle's would claim this
	// one got further than it did.
	run.sha = sha

	// The guard MUST see the unfiltered tree: TrackedFiles drops anything
	// matching gitsync.Excluded, which includes secrets.yaml and .ssh/ -
	// exactly the accidents this exists to catch.
	rawTracked, err := r.git.TrackedFilesRaw(ctx, sha)
	if err != nil {
		return r.failCycle(run, err)
	}
	if err := r.git.GuardSecretsAt(ctx, sha, rawTracked); err != nil {
		// Through failCycle like every other cycle-ending failure: this one
		// stops before checkout, and nothing about the stale plan should
		// stay actionable while a hard stop stands.
		return r.failCycle(run, fmt.Errorf("refusing to sync: secrets tracked in repository: %w", err))
	}

	tracked, err := r.git.TrackedFiles(ctx, sha)
	if err != nil {
		return r.failCycle(run, err)
	}
	if err := r.git.Checkout(ctx, sha); err != nil {
		return r.failCycle(run, err)
	}

	state := r.applier.StateLoad()
	// Refreshed here - from the copy this cycle plans against - rather than
	// at the end, where a failCycle return would skip them. The capture
	// phase is the one thing in this cycle that DOES write state.json, and
	// it refreshes them again from what it wrote.
	r.refreshStateMirrors(state)
	changes, skippedContainment, decryptFailures := r.differ.Compute(r.git.Workdir(), ConfigRoot, tracked, state.Manifest)
	if len(decryptFailures) > 0 {
		// A file that could not be decrypted ends the cycle: writing
		// ciphertext into the config or skipping the file silently are both
		// worse than saying which file and why.
		return r.failCycle(run, fmt.Errorf("refusing to sync: %s", strings.Join(decryptFailures, "; ")))
	}
	if len(skippedContainment) > 0 {
		// Invisible otherwise - differ.Compute only slog.Warns per path. A
		// path that is non-regular or escapes its root is plausible abuse
		// of the containment guard rather than churn, so it gets a visible
		// event regardless of dry_run. Informational; changes no state.
		r.logEvent(fmt.Sprintf(
			"skipped %d non-regular/escaping path(s): %s", len(skippedContainment), strings.Join(skippedContainment, ", ")))
	}

	var registryOps []registries.RegOp
	if r.opts.ReconcileRegistries {
		planned, err := r.planRegistryLayer(ctx, state)
		if err != nil {
			return r.failCycle(run, err)
		}
		registryOps = planned
	}

	// Its own toggle: unlike internal/entities, dashboards has no data
	// dependency on floor/area/label state. Placed after that block so it
	// runs only once the earlier layer succeeded.
	if r.opts.ReconcileDashboards {
		planned, err := r.planDashboardLayer(ctx, state)
		if err != nil {
			return r.failCycle(run, err)
		}
		registryOps = append(registryOps, planned...)
	}

	// One resolver for the whole cycle, shared by the three layers that may
	// declare "secret://<name>": secrets.yaml is read at most once, so two
	// references in one plan cannot resolve against two generations of it.
	secrets := secretref.NewResolver(secretsRoot)

	// Its own toggle too, after dashboards for the same "runs only once
	// the earlier layer succeeded" reason.
	var addonRestartOnChange map[string]bool
	if r.opts.ReconcileAddonOptions {
		planned, restartOnChange, err := r.planAddonLayer(ctx, state, secrets)
		if err != nil {
			return r.failCycle(run, err)
		}
		registryOps = append(registryOps, planned...)
		addonRestartOnChange = restartOnChange
	}

	// BEFORE integrations: this layer downloads the CODE, and internal/
	// flows can only create an entry for a domain HA already has. Not
	// sufficient on its own - the component is not importable until HA
	// restarts, so the entry retries per cycle until then.
	var hacsRestartPending []string
	if r.opts.ReconcileHacs {
		planned, pending, err := r.planHacsLayer(ctx, state)
		if err != nil {
			return r.failCycle(run, err)
		}
		registryOps = append(registryOps, planned...)
		hacsRestartPending = pending
	}

	// Its own toggle too, after HACS for the same "runs only once the
	// earlier layer succeeded" reason.
	entryCache := &integrationEntriesCache{fetch: r.registryApplier.FetchIntegrationEntries}
	if r.opts.ReconcileIntegrations {
		planned, err := r.planIntegrationLayer(ctx, state, entryCache, secrets)
		if err != nil {
			return r.failCycle(run, err)
		}
		registryOps = append(registryOps, planned...)
	}

	// Its own toggle too - a box can declare subentries of an integration
	// it set up by hand. After integrations because a subentry's parent may
	// be an entry that block just planned, found live only next cycle. It
	// shares that block's entry fetch (see integrationEntriesCache).
	if r.opts.ReconcileSubentries {
		planned, err := r.planSubentryLayer(ctx, state, entryCache, secrets)
		if err != nil {
			return r.failCycle(run, err)
		}
		registryOps = append(registryOps, planned...)
	}

	// The capture phase, after every layer that can failCycle and before the
	// plan is published. After, so a cycle cannot push a capture and then
	// discard the plan describing it; before, so what lands in r.pending is
	// already the apply's plan by construction - a captured or conflicted
	// path was never published, and ApplyNow's snapshot needs no filter of
	// its own. With capture_live_changes off this returns changes unchanged.
	changes, unresolved := r.captureLiveChanges(ctx, sha, changes, state)

	// Decided once and used for the state, the event line and the history
	// row: recomputing it is three chances to disagree with itself. Computed
	// from the RESIDUAL, so a cycle that captured everything reports in sync
	// rather than drift it has already resolved - but counting what capture
	// held back without resolving (a conflict, a defer, a push that failed),
	// or a failed capture would leave the page saying "in sync" over an edit
	// still waiting to be saved.
	drifted := len(changes) > 0 || len(registryOps) > 0 || unresolved > 0

	r.mu.Lock()
	r.pending = changes
	r.pendingRegistry = registryOps
	r.pendingAddonRestartOnChange = addonRestartOnChange
	r.pendingHacsRestartPending = hacsRestartPending
	if hacsRestartPending != nil {
		// Onto the display without waiting for an apply: a reminder is
		// cleared by an OBSERVATION, and holding it back would tell a user
		// to restart a Home Assistant they already restarted. What this
		// cycle retired is remembered separately, since disk still has it
		// and every later mirror refresh reads disk (see hacsLoaded).
		for _, domain := range state.HacsRestartPending {
			r.hacsLoaded[domain] = true
		}
		for _, domain := range hacsRestartPending {
			delete(r.hacsLoaded, domain)
		}
		r.hacsRestartPending = hacsRestartPending
	}
	r.lastSHA = sha
	r.lastError = ""
	// This plan is against the tree checked out right now, so whatever the
	// previous cycle left behind no longer applies.
	r.lastCycleFailed = false
	if drifted {
		r.state = StateDriftPending
	} else {
		r.state = StateInSync
	}
	r.mu.Unlock()

	outcome := history.OutcomeInSync
	if drifted {
		outcome = history.OutcomeDrift
		held := ""
		if unresolved > 0 {
			// Named separately from the pending counts, which are the APPLY
			// plan: these are paths capture will not let an apply touch.
			held = fmt.Sprintf(", %d held back by capture", unresolved)
		}
		r.logEvent(fmt.Sprintf(
			"drift detected: %d file change(s), %d registry change(s) pending%s", len(changes), len(registryOps), held))
	} else {
		r.logEvent("in sync: no changes detected")
	}
	// After the event line, before pushStatus: a slow /data must not delay
	// the feed a user is watching.
	run.finish(history.Record{
		Outcome: outcome,
		Files:   len(changes),
		RegOps:  len(registryOps),
	})
	r.pushStatus()

	// Automatic half of commit_back: only under dry_run, since a real apply
	// already writes the drift live, and only on file drift. See
	// maybeAutoCommitDriftBack for the once-per-drift-set dedup.
	//
	// Superseded by capture_live_changes, which has just written the same
	// live content to the tracked branch: both firing would push a throwaway
	// branch proposing a change that is already merged. The manual Commit
	// Drift Back button is untouched - an explicit request to park a set for
	// review is still worth having.
	if r.opts.CommitBack && r.opts.DryRun && !r.opts.CaptureLiveChanges && len(changes) > 0 {
		r.maybeAutoCommitDriftBack(ctx, changes)
	}

	// track_addon_versions, last and only on a cycle that got this far.
	// Gated on neither dry_run nor drift: it writes to the repository, not
	// the box, and add-on versions are not drift (see versionrecord.go).
	r.maybeRecordAddonVersions(ctx)

	return changes
}

// planRegistryLayer plans the floor/area/label/helper layer and the
// entity layer, which share one live fetch: entities.NewRefResolver
// resolves area/label references out of that same live state. Returns
// their ops, none when neither has work, or the error that ends the cycle.
func (r *Reconciler) planRegistryLayer(ctx context.Context, state applier.State) ([]registries.RegOp, error) {
	desired, err := r.registries.LoadManifests(r.git.Workdir())
	if err != nil {
		// A *registries.ManifestError aggregates every problem across both
		// manifest files, and lands in last_error verbatim via failCycle.
		return nil, err
	}
	entityDesired, err := r.entities.LoadManifest(r.git.Workdir())
	if err != nil {
		return nil, err
	}

	// Like registryLayerHasWork: emptying entities.yaml must still plan
	// restore-on-unmanage for whatever this agent started managing.
	entityLayerHasWork := len(entityDesired.Entities) > 0 || len(state.EntityOriginals) > 0
	var planned []registries.RegOp
	if registryLayerHasWork(desired, state) || entityLayerHasWork {
		// The entity list fetch is gated separately (a real registry can be
		// large); floor/area/label/helper state is needed either way, for
		// its own plan and for entities.NewRefResolver.
		live, err := r.registryApplier.FetchLive(ctx, entityLayerHasWork)
		if err != nil {
			return nil, err
		}
		if registryLayerHasWork(desired, state) {
			planned = append(planned, r.registries.Plan(desired, live, state.RegistryManaged)...)
		}
		if entityLayerHasWork {
			refs := entities.NewRefResolver(desired, state.RegistryManaged, live["area"], live["label"])
			planned = append(planned, r.entities.Plan(entityDesired, live["entity"], state.EntityOriginals, refs)...)
		}
	}
	return planned, nil
}

// planDashboardLayer plans the dashboard layer: returns the ops to append
// to this cycle's plan - none when the layer has no work - or the error
// that must end the cycle.
func (r *Reconciler) planDashboardLayer(ctx context.Context, state applier.State) ([]registries.RegOp, error) {
	dashboardDesired, err := r.dashboards.LoadManifest(r.git.Workdir())
	if err != nil {
		return nil, err
	}

	// Like registryLayerHasWork: emptying the manifest must still plan
	// deletes for whatever this agent created or adopted.
	dashboardLayerHasWork := len(dashboardDesired.Dashboards) > 0 || len(state.DashboardManaged) > 0
	var planned []registries.RegOp
	if dashboardLayerHasWork {
		ids := dashboardContentIDsToFetch(dashboardDesired)
		liveDashboards, liveContent, err := r.registryApplier.FetchLiveDashboards(ctx, ids)
		if err != nil {
			return nil, err
		}
		planned = append(planned,
			r.dashboards.Plan(dashboardDesired, liveDashboards, liveContent, state.DashboardManaged)...)
	}
	return planned, nil
}

// planAddonLayer plans the add-on options layer: the ops to append plus
// the manifest's restart_on_change map ApplyNow later consults (see
// pendingAddonRestartOnChange), or the error that ends the cycle.
func (r *Reconciler) planAddonLayer(
	ctx context.Context, state applier.State, secrets *secretref.Resolver,
) ([]registries.RegOp, map[string]bool, error) {
	addonDesired, err := r.addonOpts.LoadManifest(r.git.Workdir())
	if err != nil {
		return nil, nil, err
	}

	// Like the layers above: emptying the manifest must still plan
	// restore-on-unmanage for whatever this agent started managing.
	addonLayerHasWork := len(addonDesired.Addons) > 0 || len(state.AddonOriginals) > 0
	var planned []registries.RegOp
	var addonRestartOnChange map[string]bool
	if addonLayerHasWork {
		selfSlug, err := r.resolveSelfAddonSlug(ctx)
		if err != nil {
			return nil, nil, err
		}
		slugs := addonSlugsToFetch(addonDesired, state.AddonOriginals)
		liveAddons, err := r.registryApplier.FetchAddonInfoAll(ctx, slugs)
		if err != nil {
			return nil, nil, err
		}
		planned = append(planned, r.addonOpts.Plan(addonDesired, liveAddons, state.AddonOriginals, selfSlug, secrets)...)
		addonRestartOnChange = addonopts.DeclaredRestartOnChange(addonDesired)
	}
	return planned, addonRestartOnChange, nil
}

// planHacsLayer plans the HACS layer: ops to append, plus the reminder
// list as this cycle saw it - nil only when the layer had no work, which
// tells ApplyNow not to persist reminders. Work means a declared manifest
// or a standing reminder, not managed items: internal/hacs never
// uninstalls. HACS not being installed skips the layer rather than ending
// the cycle, raising a health flag instead.
func (r *Reconciler) planHacsLayer(ctx context.Context, state applier.State) ([]registries.RegOp, []string, error) {
	desired, err := r.hacs.LoadManifest(r.git.Workdir())
	if err != nil {
		return nil, nil, err
	}

	if len(desired.Repos) == 0 && len(state.HacsRestartPending) == 0 {
		// Nothing declared and no reminder standing, which includes not
		// leaving a "HACS is not installed" chip up from when there was.
		r.clearHacsUnavailable()
		return nil, nil, nil
	}

	live, err := r.registryApplier.FetchHacsLive(ctx, regapply.HacsFetchRequest{
		Desired:        desired,
		Managed:        state.HacsManaged,
		RestartPending: state.HacsRestartPending,
	})
	if err != nil {
		if errors.Is(err, regapply.ErrHacsNotInstalled) {
			r.noteHacsUnavailable(err)
			return nil, nil, nil
		}
		return nil, nil, err
	}
	r.clearHacsUnavailable()
	planned := r.hacs.Plan(desired, live.Repositories, state.HacsManaged, state.HacsAttempts)
	// Always non-nil, so the caller can tell "the layer ran and nothing is
	// pending" from "the layer did not run".
	pending := hacs.PruneRestartPending(state.HacsRestartPending, live.Components)
	if pending == nil {
		pending = []string{}
	}
	return planned, pending, nil
}

// noteHacsUnavailable records that HACS is not installed, logging only on
// the way INTO that state - it never clears by itself, so a line per cycle
// would fill the log. Status.HacsUnavailable carries the standing half.
func (r *Reconciler) noteHacsUnavailable(err error) {
	var first bool
	r.withMu(func() {
		first = !r.hacsUnavailable
		r.hacsUnavailable = true
	})
	slog.Warn("recon: hacs layer skipped", "error", err)
	if first {
		r.logEvent("hacs layer skipped: " + err.Error())
	}
}

// clearHacsUnavailable records that HACS answered, logging the recovery
// only if the previous cycle could not reach it - the other half of
// noteHacsUnavailable's guard.
func (r *Reconciler) clearHacsUnavailable() {
	var recovered bool
	r.withMu(func() {
		recovered = r.hacsUnavailable
		r.hacsUnavailable = false
	})
	if recovered {
		r.logEvent("hacs layer recovered: HACS answered again")
	}
}

// integrationEntriesCache memoizes one cycle's FetchIntegrationEntries
// across the integrations and subentries layers: independent toggles, so
// either may pay for the fetch, but the box is asked once. Per cycle, not
// per process - the entries are the live state being planned against - and
// a failed fetch is not cached.
type integrationEntriesCache struct {
	fetch   func(context.Context) ([]map[string]any, error)
	entries []map[string]any
	fetched bool
}

func (c *integrationEntriesCache) get(ctx context.Context) ([]map[string]any, error) {
	if c.fetched {
		return c.entries, nil
	}
	entries, err := c.fetch(ctx)
	if err != nil {
		return nil, err
	}
	c.entries = entries
	c.fetched = true
	return entries, nil
}

// planIntegrationLayer plans the integrations layer: returns the ops to
// append to this cycle's plan - none when the layer has no work - or the
// error that must end the cycle.
func (r *Reconciler) planIntegrationLayer(
	ctx context.Context, state applier.State, entries *integrationEntriesCache, secrets *secretref.Resolver,
) ([]registries.RegOp, error) {
	flowsDesired, err := r.flows.LoadManifest(r.git.Workdir())
	if err != nil {
		return nil, err
	}

	// Like the layers above: emptying the manifest must still plan deletes
	// for whatever this agent created or adopted.
	flowsLayerHasWork := len(flowsDesired.Integrations) > 0 || len(state.IntegrationManaged) > 0
	var planned []registries.RegOp
	if flowsLayerHasWork {
		liveEntries, err := entries.get(ctx)
		if err != nil {
			return nil, err
		}
		planned = append(planned, r.flows.Plan(
			flowsDesired, liveEntries,
			state.IntegrationManaged, state.IntegrationHashes, state.IntegrationAttempts, secrets)...)
	}
	return planned, nil
}

// planSubentryLayer plans the subentries layer, mirroring
// planIntegrationLayer with one extra fetch: the entry list says which
// parents exist, and a second call reads the subentries hanging off them.
func (r *Reconciler) planSubentryLayer(
	ctx context.Context, state applier.State, entries *integrationEntriesCache, secrets *secretref.Resolver,
) ([]registries.RegOp, error) {
	subDesired, err := r.subentries.LoadManifest(r.git.Workdir())
	if err != nil {
		return nil, err
	}

	// Like flowsLayerHasWork: emptying the manifest must still plan rule-4
	// unmanages for whatever this agent created or adopted.
	subentryLayerHasWork := len(subDesired.Subentries) > 0 || len(state.SubentryManaged) > 0
	var planned []registries.RegOp
	if subentryLayerHasWork {
		liveEntries, err := entries.get(ctx)
		if err != nil {
			return nil, err
		}
		liveSubentries, err := r.registryApplier.FetchSubentries(ctx, subentryParentEntryIDs(subDesired, liveEntries))
		if err != nil {
			return nil, err
		}
		planned = append(planned, r.subentries.Plan(
			subDesired, liveEntries, liveSubentries,
			state.SubentryManaged, state.SubentryHashes, state.SubentryAttempts, secrets)...)
	}
	return planned, nil
}

// subentryParentEntryIDs returns the entry ids subentries.Plan needs live
// subentries for: every live config entry of a declared domain, sorted.
// The CANDIDATE set, not the resolved parents. entry_title must NOT
// narrow it - Plan finds a managed subentry by scanning every entry for
// its subentry_id (see subentries.locateSubentry), so narrowing would hide
// it and create a duplicate. Editing an item's DOMAIN has that gap.
func subentryParentEntryIDs(desired subentries.Desired, liveEntries []map[string]any) []string {
	domains := map[string]bool{}
	for _, item := range desired.Subentries {
		if domain, _ := item["domain"].(string); domain != "" {
			domains[domain] = true
		}
	}

	seen := map[string]bool{}
	for _, entry := range liveEntries {
		d, _ := entry["domain"].(string)
		if !domains[d] {
			continue
		}
		if id, _ := entry["entry_id"].(string); id != "" {
			seen[id] = true
		}
	}
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// ApplyNow applies the pending diff, files then registries: a best-effort
// Supervisor backup, internal/applier for the write+validate+reload, then
// the registry plan reusing that apply's stash dir. A registry failure
// does not undo applied files, registry ops never run if the file apply
// failed, and skipped error ops stay pending. force=false (the loop)
// refuses under dry_run; it does not override the no-plan refusal.
func (r *Reconciler) ApplyNow(ctx context.Context, force bool) applier.Result {
	// Not logged, unlike the refusal below: the Apply button always forces,
	// so only the tick reaches this, every interval for as long as dry_run
	// is on. A configured mode the banner already states, not an event.
	if !force && r.opts.DryRun {
		return applier.Result{OK: false, Error: "dry_run is enabled; use the Apply button to override"}
	}

	// Logged, because this one is TRANSIENT: an attempt lost a race rather
	// than the add-on being configured a certain way. Usually the tick,
	// firing while a web-triggered operation still runs.
	if !r.opLock.TryLock() {
		r.logEvent("apply skipped: " + errBusy.Error())
		return applier.Result{OK: false, Error: errBusy.Error()}
	}
	defer r.opLock.Unlock()

	return r.applyNow(ctx)
}

// applyNow is ApplyNow's body without the lock or the dry_run refusal, for
// ReconcileNow/reconcileNow's reason: runCycle composes a cycle's capture
// and its apply under ONE opLock hold, and an inner method that took the
// lock again would deadlock. Callers hold opLock.
func (r *Reconciler) applyNow(ctx context.Context) applier.Result {
	// Read under opLock, not just mu: no reconcile can be running, so this
	// cannot catch a cycle halfway through deciding its outcome.
	r.mu.Lock()
	cycleFailed := r.lastCycleFailed
	unseeded := r.state == StateUnseeded
	r.mu.Unlock()
	if cycleFailed {
		// The plan is empty, so this would apply nothing and then clear
		// last_error on its way out, announcing an error it never looked at
		// as resolved. Rollback is not gated this way - it needs no plan,
		// and is the recovery path from exactly this state.
		msg := "the last reconcile failed, so there is nothing planned to apply; fix the reported error and check again"
		if unseeded {
			msg = "the tracked branch does not exist on the remote yet, so there is nothing planned to apply; import to seed the repository first"
		}
		r.logEvent("apply skipped: " + msg)
		return applier.Result{OK: false, Error: msg}
	}

	// After the three refusals, before any work: a refusal is not a run and
	// gets no row (see beginRun).
	run := r.beginRun(history.KindApply)
	defer run.abandon()

	r.mu.Lock()
	// Second line of defence for the conflict verdict: the capture phase
	// already kept these out of r.pending, and this makes a conflicted path
	// unapplyable even from a plan built before it was one - a hand-seeded
	// status, a future caller - the same posture stageDrift takes by
	// re-checking every path whatever its caller already filtered.
	//
	// From the mirror rather than a StateLoad: it is refreshed from every
	// write of the record and hydrated at startup, so it is as current as
	// the file, and a second load here would re-run guardChangePath -
	// filepath.EvalSymlinks per manifest entry - over ~191 paths for a
	// result that is empty on every install with the option off.
	pending := dropConflicted(append([]differ.Change(nil), r.pending...), r.conflicts)
	registryOps := append([]registries.RegOp(nil), r.pendingRegistry...)
	addonRestartOnChange := r.pendingAddonRestartOnChange
	hacsRestartPending := r.pendingHacsRestartPending
	// Read under the same lock as the plan, so the row names the commit
	// whose plan this apply executes and lines up with its reconcile row.
	run.sha = r.lastSHA
	r.state = StateApplying
	// Cleared per apply, then set below from this call's own result.
	r.lastWarnings = ""
	r.mu.Unlock()
	r.pushStatus()
	r.logEvent(fmt.Sprintf("applying %d change(s)", len(pending)))

	// Best-effort: Rollback uses applier's per-file stash, taken below,
	// whose failure IS fatal. Reported rather than swallowed - on a large
	// install this can fail every apply (see snapshot.BackupTimeout), and
	// the dashboard would stay green without the safety net.
	if _, backupErr := r.snapshot.PreApplyBackup(ctx); backupErr != nil {
		r.withMu(func() { r.lastBackupError = backupErr.Error() })
		r.logEvent("pre-apply supervisor backup failed: " + backupErr.Error())
	} else {
		r.withMu(func() { r.lastBackupError = "" })
	}

	result, failed := r.applyFileLayer(ctx, pending)
	if failed {
		// The single funnel for every file-layer failure, so one record
		// covers both of applyFileLayer's exits. Only claim a rollback that
		// actually happened.
		outcome := history.OutcomeError
		if result.RolledBack {
			outcome = history.OutcomeRolledBack
		}
		run.finish(history.Record{
			Outcome:  outcome,
			Files:    len(result.Changed),
			Error:    result.Error,
			StashDir: result.StashDir,
		})
		if result.StashDir != "" {
			// The stash must survive a restart even off a failed apply:
			// Rollback is the manual retry when the automatic one was
			// incomplete. The summary stays empty for applyFileLayer's reason.
			st := r.applier.StateLoad()
			st.LastStashDir = result.StashDir
			st.LastStashSummary = ""
			if saveErr := r.applier.StateSave(st); saveErr != nil {
				slog.Warn("recon: could not persist the rollback pointer", "error", saveErr)
			}
		}
		// A permanently failing change would otherwise allocate one stash
		// directory per interval until an apply succeeds; the success path
		// prunes, so this path has to as well.
		r.applier.PruneStashDirs(pruneKeep, result.StashDir)
		return result
	}

	state := r.applier.StateLoad()
	// Merge into the existing manifest rather than replacing it with just
	// this apply's paths: the manifest is the full set of paths this
	// agent currently manages, and internal/differ.Compute only ever
	// proposes a "delete" for a path still listed here.
	manifest := make(map[string]bool, len(state.Manifest)+len(pending))
	for _, p := range state.Manifest {
		manifest[p] = true
	}
	for _, change := range pending {
		if change.Kind == differKindDelete {
			delete(manifest, change.Path)
		} else {
			manifest[change.Path] = true
		}
	}
	state.LastGoodSHA = r.git.CurrentSHA(ctx)
	state.Manifest = sortedStringKeys(manifest)
	state.LastApplyUTC = utcNowISO()
	// A path this apply just wrote is the repository's again, so LastGoodSHA
	// describes it better than an older capture commit does. Deliberately
	// NOT the reverse - LastGoodSHA is never set to a capture commit, since
	// that would claim the agent wrote the whole of that tree live when a
	// conflicted or deferred path in the same cycle was written nowhere.
	dropCapturedPaths(&state, changePaths(pending))

	// The one piece of state a PLAN can change (see
	// pendingHacsRestartPending): this is where the cycle's observation of
	// loaded domains becomes durable. Before the layers, so the HACS layer
	// appends to the pruned list; skipped when that cycle never looked, so
	// an apply cannot wipe reminders it knows nothing about.
	if hacsRestartPending != nil {
		state.HacsRestartPending = hacsRestartPending
	}

	// Registry ops run only now that the file layer has succeeded (or had
	// nothing to do). state.RegistryManaged is mutated in place by
	// ApplyPlan; it is persisted below in the same StateSave() as the
	// manifest update so both land together.
	var acc registryApplyOutcome
	finalStashDir := result.StashDir
	if len(registryOps) > 0 {
		// registryOps is unified for display but executed as seven
		// independent layers, each running only once the one before it
		// succeeded: registries, entities, dashboards, addon options, HACS,
		// integrations, subentries. A later failure never undoes an earlier
		// layer. The order matches ReconcileNow's planning order and is
		// load bearing for one pair - HACS downloads the code an
		// integration's config entry needs.
		layers := splitRegistryOpsByLayer(registryOps)

		if finalStashDir == "" && needsStashDir(registryOps) {
			newDir, mkErr := r.applier.MakeStashDir()
			if mkErr != nil {
				acc.registryError = mkErr.Error()
				// Its own value because nothing has run: the event-log
				// switch must blame neither "registries" nor a rollback
				// that never happened. The gate stops a subentry-only plan
				// too - an unwritable /data fails the StateSave below, and
				// an unrecorded subentry CREATE returns as a duplicate this
				// agent can never delete.
				acc.failedLayer = "stash allocation"
			} else {
				finalStashDir = newDir
			}
		}

		if acc.registryError == "" && len(layers.registry) > 0 {
			r.applyRegistryLayer(ctx, layers.registry, &state, finalStashDir, &acc)
		}

		if acc.registryError == "" && len(layers.entity) > 0 {
			r.applyEntityLayer(ctx, layers.entity, &state, finalStashDir, &acc)
		}

		if acc.registryError == "" && len(layers.dashboard) > 0 {
			r.applyDashboardLayer(ctx, layers.dashboard, &state, finalStashDir, &acc)
		}

		if acc.registryError == "" && len(layers.addon) > 0 {
			r.applyAddonLayer(ctx, layers.addon, addonRestartOnChange, &state, finalStashDir, &acc)
		}

		if acc.registryError == "" && len(layers.hacs) > 0 {
			r.applyHacsLayer(ctx, layers.hacs, &state, &acc)
		}

		if acc.registryError == "" && len(layers.integration) > 0 {
			r.applyIntegrationLayer(ctx, layers.integration, &state, finalStashDir, &acc)
		}

		if acc.registryError == "" && len(layers.subentry) > 0 {
			r.applySubentryLayer(ctx, layers.subentry, &state, &acc)
		}
	}

	// Unpacked into locals so everything below reads the accumulated
	// outcome the same way, registry plan or not.
	registryError := acc.registryError
	failedLayer := acc.failedLayer
	registryAppliedCount := acc.registryAppliedCount
	registryRolledBack := acc.registryRolledBack
	registrySkipped := acc.registrySkipped
	appliedIdentities := acc.appliedIdentities

	// BEFORE the save below, which can fail and return: the stash is on
	// disk either way, and a bookkeeping failure is exactly when someone
	// reaches for Roll Back. The only site that composes a summary, so the
	// dialog cannot describe one apply at two fidelities.
	if finalStashDir != "" {
		summary := stashSummary(len(result.Changed), finalStashDir)
		// Into the state saved below too, so the rollback point survives a
		// restart. When this apply allocated no stash, the previous apply's
		// persisted pointer rides through StateLoad/StateSave untouched.
		state.LastStashDir = finalStashDir
		state.LastStashSummary = summary
		r.withMu(func() {
			r.lastStashDir = finalStashDir
			r.lastStashSummary = summary
		})
	}

	if err := r.applier.StateSave(state); err != nil {
		// Everything computed above is discarded even though the files (and
		// any registry ops) already landed for real.
		r.withMu(func() {
			r.lastError = err.Error()
			r.state = StateError
		})
		r.logEvent("error applying: " + err.Error())
		// OutcomePartial, not OutcomeError: the files landed and only the
		// bookkeeping failed, which is exactly when someone needs to know
		// what did land.
		run.finish(history.Record{
			Outcome:  history.OutcomePartial,
			Files:    len(result.Changed),
			RegOps:   registryAppliedCount,
			Error:    err.Error(),
			StashDir: finalStashDir,
		})
		// The failure path leaks stash directories exactly as the file-layer
		// one does; prune here too, protecting this apply's own stash.
		r.applier.PruneStashDirs(pruneKeep, finalStashDir)
		r.pushStatus()
		return applier.Result{OK: false, Error: err.Error()}
	}

	// The pruned list is on disk, so hacsLoaded has done its job. Only when
	// this apply actually persisted one - an apply planned with the layer
	// off just wrote back whatever was there.
	if hacsRestartPending != nil {
		r.withMu(func() { r.hacsLoaded = map[string]bool{} })
	}

	// The integration and subentry layers record failures into the state
	// they were handed, so this is where a newly blocked item first exists.
	// After the save: a mirror displays what is on disk.
	r.refreshStateMirrors(state)

	if registryError != "" {
		// The file layer stays applied - a registry failure never undoes
		// files - but the overall call failed, so the Result says so.
		result = applier.Result{
			OK:         false,
			Changed:    result.Changed,
			Error:      joinErrors(result.Error, registryError),
			RolledBack: registryRolledBack,
			StashDir:   finalStashDir,
		}
	}

	r.mu.Lock()
	r.pending = nil
	r.lastApplyUTC = state.LastApplyUTC
	r.lastSHA = state.LastGoodSHA
	if registryError == "" {
		// Skipped error ops (a name conflict, say) stay pending and
		// visible rather than clearing with the ops that ran.
		r.pendingRegistry = registrySkipped
		if len(registrySkipped) > 0 {
			r.state = StateDriftPending
		} else {
			r.state = StateInSync
		}
		r.lastError = ""
	} else {
		// Some ops already landed for real, so pendingRegistry must hold
		// only what still needs to happen - the original plan would let a
		// retry re-submit them and create duplicates. Keeping the ops whose
		// identity never appeared in any layer's Applied covers layers that
		// never ran, ops undone by inverse-replay, and skipped error ops,
		// in one pass.
		applied := make(map[string]bool, len(appliedIdentities))
		for _, a := range appliedIdentities {
			applied[a] = true
		}
		stillPending := make([]registries.RegOp, 0, len(registryOps))
		for _, op := range registryOps {
			if !applied[regOpIdentity(op)] {
				stillPending = append(stillPending, op)
			}
		}
		r.pendingRegistry = stillPending
		r.state = StateError
		r.lastError = registryError
	}
	r.mu.Unlock()

	switch {
	case registryError != "":
		// Phrased for the layer that failed, keyed by the literal each
		// apply*Layer writes into failedLayer. The default arm claims
		// nothing a layer cannot do, so a new layer without an arm lands
		// there rather than reporting a rollback that never happened.
		switch failedLayer {
		case "integrations", "subentries", "hacs":
			// The three per-op-isolated layers: each item applies
			// independently, and none has a rollback to report -
			// integrations and subentries cannot read back what they would
			// need, HACS has no uninstall. Only what failed and what stayed.
			applied := ""
			if registryAppliedCount > 0 {
				applied = fmt.Sprintf("; %d registry change(s) stayed applied", registryAppliedCount)
			}
			r.logEvent(fmt.Sprintf(
				"files applied (%d change(s)); %s: %s%s",
				len(result.Changed), failedLayer, registryError, applied))

		case "stash allocation":
			// Nothing ran at all, so the default arm's "registries failed
			// ... rolled back" would blame the wrong layer and report a
			// rollback of ops that were never attempted.
			r.logEvent(fmt.Sprintf(
				"files applied (%d change(s)); could not allocate a stash directory for %d pending registry op(s): %s",
				len(result.Changed), len(registryOps), registryError))

		default:
			// Only claim a rollback that happened: a registry apply can fail
			// with RolledBack=false and leave real ops in effect (see
			// regapply.ApplyPlan), and saying "rolled back" would report
			// live registries as untouched when they are not.
			outcome := "and were rolled back"
			if !registryRolledBack {
				outcome = "and could NOT be fully rolled back"
			}
			layer := failedLayer
			if layer == "" {
				layer = "registries"
			}
			// Earlier layers are NOT undone by the failing layer's
			// inverse-replay; they stay applied, in their stash files for
			// the Rollback button.
			applied := ""
			if registryAppliedCount > 0 {
				applied = fmt.Sprintf("; %d earlier registry change(s) stayed applied", registryAppliedCount)
			}
			r.logEvent(fmt.Sprintf(
				"files applied (%d change(s)); %s failed %s: %s%s",
				len(result.Changed), layer, outcome, registryError, applied))
		}
	case len(registryOps) > 0:
		msg := fmt.Sprintf("applied %d file change(s) and %d registry change(s)", len(result.Changed), registryAppliedCount)
		if len(registrySkipped) > 0 {
			msg += fmt.Sprintf("; %d registry item(s) still need attention", len(registrySkipped))
		}
		r.logEvent(msg)
	default:
		r.logEvent(fmt.Sprintf("applied %d change(s)", len(result.Changed)))
	}

	// One record for the whole apply, off the same locals the switch above
	// reads. "Rolled back" only when NOTHING is left applied; anything else
	// that failed is OutcomePartial, because something is live.
	var outcome string
	switch {
	case registryError == "":
		outcome = history.OutcomeOK
	case registryRolledBack && registryAppliedCount == 0 && len(result.Changed) == 0:
		outcome = history.OutcomeRolledBack
	default:
		outcome = history.OutcomePartial
	}
	run.finish(history.Record{
		Outcome:  outcome,
		Files:    len(result.Changed),
		RegOps:   registryAppliedCount,
		Error:    registryError,
		StashDir: finalStashDir,
	})

	// Best-effort cleanup; neither call returns an error, and both log
	// their own failures.
	r.snapshot.Prune(pruneKeep)
	r.applier.PruneStashDirs(pruneKeep, finalStashDir)

	r.pushStatus()
	return result
}

// applyFileLayer applies the pending file changes through internal/
// applier and records the stash directory, this apply's check_config
// warnings and any failure. A true second return means ApplyNow must stop
// and return the Result alongside it, already logged and pushed.
func (r *Reconciler) applyFileLayer(ctx context.Context, pending []differ.Change) (applier.Result, bool) {
	result, err := r.applier.Apply(ctx, toApplierChanges(pending), r.git.Workdir(), ConfigRoot, r.opts)
	if err != nil {
		// applier.Apply returns a non-nil error for only two failures rather
		// than folding them into Result: writing the stash directory, and a
		// missing Supervisor token.
		r.withMu(func() {
			r.lastError = err.Error()
			r.state = StateError
		})
		r.logEvent("error applying: " + err.Error())
		r.pushStatus()
		return applier.Result{OK: false, Error: err.Error()}, true
	}

	// Recorded regardless of ok/rolled_back: a failed apply leaves a stash
	// too, and Rollback is the manual retry when its own rollback was
	// incomplete. The preview is CLEARED rather than composed - Changed is
	// unset on every failing return - and moves with the stash so the
	// previous apply's summary cannot stand over this one.
	if result.StashDir != "" {
		r.withMu(func() {
			r.lastStashDir = result.StashDir
			r.lastStashSummary = ""
		})
	}

	// Warnings inform rather than block, so they are recorded
	// unconditionally, and a later registry failure does not clear them.
	if result.Warnings != "" {
		r.withMu(func() { r.lastWarnings = result.Warnings })
		r.logEvent("config warnings after apply: " + result.Warnings)
	}

	if !result.OK {
		r.withMu(func() {
			r.lastError = result.Error
			r.state = StateError
		})
		r.logEvent("apply failed: " + result.Error)
		r.pushStatus()
		return result, true
	}

	return result, false
}

// stashSummary names what a rollback from stashDir would put back: the
// stashed files, and each registry layer that left a stash beside them.
// Never "" - an apply that stashed nothing nameable gets
// RollbackPreviewNothing, since "" means no summary was composed at all.
//
// The three checks are disk stats, hence once per apply and never from
// Status (see lastStashSummary). Subentries and HACS are absent because
// neither writes a stash: a subentry's prior data is unreadable once
// submitted, and a download's only inverse is an uninstall nothing plans.
func stashSummary(files int, stashDir string) string {
	parts := make([]string, 0, 4)
	if files > 0 {
		parts = append(parts, fmt.Sprintf("%d file(s)", files))
	}
	if regapply.RegistryStashExists(stashDir) {
		parts = append(parts, "registry objects")
	}
	if regapply.AddonStashExists(stashDir) {
		parts = append(parts, "add-on options")
	}
	if regapply.IntegrationStashExists(stashDir) {
		parts = append(parts, "integrations")
	}
	if len(parts) == 0 {
		return RollbackPreviewNothing
	}
	return joinNaturally(parts)
}

// joinNaturally joins parts the way the sentence around them reads: "a",
// "a and b", "a, b and c". Commas alone read as a truncated list.
func joinNaturally(parts []string) string {
	switch len(parts) {
	case 0:
		return ""
	case 1:
		return parts[0]
	default:
		return strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1]
	}
}

// registryApplyOutcome is the running result of the registry layers
// ApplyNow sequences after the file layer. Each layer folds its own
// RegistryApplyResult in, and every layer after the first runs only while
// registryError is still empty.
type registryApplyOutcome struct {
	// registryError is the first layer failure, the one that stopped every
	// layer after it from running.
	registryError string
	// failedLayer names which of the seven layers produced registryError,
	// so the event log need not blame "registries" for all of them.
	failedLayer string
	// registryAppliedCount counts the ops that actually ran, across every
	// layer that got as far as running any.
	registryAppliedCount int
	// registryRolledBack mirrors the failing layer's own
	// RegistryApplyResult.RolledBack - never assumed.
	registryRolledBack bool
	// registrySkipped collects kind == "error" ops from a *successful*
	// apply: never executed, but must stay visible to the user.
	registrySkipped []registries.RegOp
	// appliedIdentities accumulates the "<kind> <rtype>:<key>" labels of
	// every op reported as executed and still in effect, OK layer or not
	// (see RegistryApplyResult.Applied). ApplyNow rebuilds pendingRegistry
	// from the ops missing here.
	appliedIdentities []string
}

// needsStashDir is whether this plan is worth a stash directory, which
// also decides whether an unwritable /data stops it (see ApplyNow's
// MakeStashDir gate). True for every plan but one shape: all-HACS-adopt,
// which touches nothing live and is idempotent, and where an empty stash
// would evict a real rollback point from PruneStashDirs' five.
func needsStashDir(registryOps []registries.RegOp) bool {
	for _, op := range registryOps {
		if op.Kind == registries.KindError {
			continue
		}
		if op.RType == rtypeHacs && hacs.IsAdopt(op) {
			continue
		}
		return true
	}
	return false
}

// layerOps is one pending registry plan split by RType into the layers
// ApplyNow executes it as. A struct because seven same-typed slices in one
// signature is where a caller silently swaps two.
type layerOps struct {
	registry    []registries.RegOp
	entity      []registries.RegOp
	dashboard   []registries.RegOp
	addon       []registries.RegOp
	hacs        []registries.RegOp
	integration []registries.RegOp
	subentry    []registries.RegOp
}

// splitRegistryOpsByLayer groups one pending registry plan by RType. The
// registry layer is the default arm because its RType is not one fixed
// string: floor/area/label and every helper domain belong to it.
func splitRegistryOpsByLayer(registryOps []registries.RegOp) layerOps {
	var layers layerOps
	for _, op := range registryOps {
		switch op.RType {
		case "entity":
			layers.entity = append(layers.entity, op)
		case "dashboard":
			layers.dashboard = append(layers.dashboard, op)
		case "addon":
			layers.addon = append(layers.addon, op)
		case rtypeHacs:
			layers.hacs = append(layers.hacs, op)
		case "integration":
			layers.integration = append(layers.integration, op)
		case "subentry":
			layers.subentry = append(layers.subentry, op)
		default:
			layers.registry = append(layers.registry, op)
		}
	}
	return layers
}

// applyRegistryLayer executes the floor/area/label/helper ops of the
// pending plan and folds what happened into acc. state.RegistryManaged is
// mutated in place by ApplyPlan; ApplyNow persists it.
func (r *Reconciler) applyRegistryLayer(
	ctx context.Context, registryOnlyOps []registries.RegOp,
	state *applier.State, finalStashDir string, acc *registryApplyOutcome,
) {
	regResult := r.registryApplier.ApplyPlan(ctx, registryOnlyOps, state.RegistryManaged, finalStashDir)
	acc.appliedIdentities = append(acc.appliedIdentities, regResult.Applied...)
	if regResult.OK {
		acc.registryAppliedCount += len(regResult.Applied)
		acc.registrySkipped = append(acc.registrySkipped, regResult.SkippedErrors...)
	} else {
		acc.registryError = regResult.Error
		acc.registryRolledBack = regResult.RolledBack
		acc.failedLayer = "registries"
	}
}

// applyEntityLayer executes the entity ops of the pending plan and folds
// what happened into acc. state.EntityOriginals is mutated in place by
// ApplyEntityPlan; ApplyNow persists it.
func (r *Reconciler) applyEntityLayer(
	ctx context.Context, entityOnlyOps []registries.RegOp,
	state *applier.State, finalStashDir string, acc *registryApplyOutcome,
) {
	entResult := r.registryApplier.ApplyEntityPlan(ctx, entityOnlyOps, state.EntityOriginals, finalStashDir)
	acc.appliedIdentities = append(acc.appliedIdentities, entResult.Applied...)
	if entResult.OK {
		acc.registryAppliedCount += len(entResult.Applied)
		acc.registrySkipped = append(acc.registrySkipped, entResult.SkippedErrors...)
	} else {
		acc.registryError = entResult.Error
		acc.registryRolledBack = entResult.RolledBack
		acc.failedLayer = "entities"
	}
}

// applyDashboardLayer executes the dashboard ops of the pending plan and
// folds what happened into acc. state.DashboardManaged is mutated in
// place by ApplyDashboardPlan; ApplyNow persists it.
func (r *Reconciler) applyDashboardLayer(
	ctx context.Context, dashboardOnlyOps []registries.RegOp,
	state *applier.State, finalStashDir string, acc *registryApplyOutcome,
) {
	dashResult := r.registryApplier.ApplyDashboardPlan(ctx, dashboardOnlyOps, state.DashboardManaged, finalStashDir)
	acc.appliedIdentities = append(acc.appliedIdentities, dashResult.Applied...)
	if dashResult.OK {
		acc.registryAppliedCount += len(dashResult.Applied)
		acc.registrySkipped = append(acc.registrySkipped, dashResult.SkippedErrors...)
	} else {
		acc.registryError = dashResult.Error
		acc.registryRolledBack = dashResult.RolledBack
		acc.failedLayer = "dashboards"
	}
}

// applyAddonLayer executes the add-on options ops and folds what happened
// into acc. addonRestartOnChange is the manifest setting these ops were
// planned against. state.AddonOriginals and state.AddonRestartOnChange are
// mutated in place by ApplyAddonPlan; ApplyNow persists them.
func (r *Reconciler) applyAddonLayer(
	ctx context.Context, addonOnlyOps []registries.RegOp, addonRestartOnChange map[string]bool,
	state *applier.State, finalStashDir string, acc *registryApplyOutcome,
) {
	addonResult := r.registryApplier.ApplyAddonPlan(
		ctx, addonOnlyOps, addonRestartOnChange, state.AddonOriginals, state.AddonRestartOnChange, finalStashDir)
	acc.appliedIdentities = append(acc.appliedIdentities, addonResult.Applied...)
	if addonResult.OK {
		acc.registryAppliedCount += len(addonResult.Applied)
		acc.registrySkipped = append(acc.registrySkipped, addonResult.SkippedErrors...)
	} else {
		acc.registryError = addonResult.Error
		acc.registryRolledBack = addonResult.RolledBack
		acc.failedLayer = "add-on options"
	}
}

// applyIntegrationLayer executes the integration ops of the pending plan
// and folds what happened into acc. state.IntegrationManaged/Hashes/Data
// are mutated in place by ApplyFlowPlan; ApplyNow persists them.
func (r *Reconciler) applyIntegrationLayer(
	ctx context.Context, integrationOnlyOps []registries.RegOp,
	state *applier.State, finalStashDir string, acc *registryApplyOutcome,
) {
	flowResult := r.registryApplier.ApplyFlowPlan(
		ctx, integrationOnlyOps, state.IntegrationManaged, state.IntegrationHashes,
		state.IntegrationData, state.IntegrationAttempts, finalStashDir)
	// Per-op isolation (see ApplyFlowPlan): Applied/SkippedErrors count
	// unconditionally, not just when OK, because a partial failure leaves
	// this layer's own successful siblings applied.
	acc.registryAppliedCount += len(flowResult.Applied)
	acc.registrySkipped = append(acc.registrySkipped, flowResult.SkippedErrors...)
	acc.appliedIdentities = append(acc.appliedIdentities, flowResult.Applied...)
	if !flowResult.OK {
		acc.registryError = flowResult.Error
		acc.registryRolledBack = flowResult.RolledBack
		acc.failedLayer = "integrations"
	}
}

// applyHacsLayer executes the HACS ops and folds what happened into acc.
// state.HacsManaged/HacsAttempts/HacsRestartPending are mutated in place
// by ApplyHacsPlan; ApplyNow persists them. No stash directory: the only
// inverse of a download is an uninstall, which internal/hacs never plans.
func (r *Reconciler) applyHacsLayer(
	ctx context.Context, hacsOnlyOps []registries.RegOp,
	state *applier.State, acc *registryApplyOutcome,
) {
	hacsResult := r.registryApplier.ApplyHacsPlan(
		ctx, hacsOnlyOps, state.HacsManaged, state.HacsAttempts, &state.HacsRestartPending)
	// Per-op isolation, as in applyIntegrationLayer: a downloaded
	// integration stays downloaded when a sibling op fails.
	acc.registryAppliedCount += len(hacsResult.Applied)
	acc.registrySkipped = append(acc.registrySkipped, hacsResult.SkippedErrors...)
	acc.appliedIdentities = append(acc.appliedIdentities, hacsResult.Applied...)
	if !hacsResult.OK {
		acc.registryError = hacsResult.Error
		acc.registryRolledBack = hacsResult.RolledBack
		acc.failedLayer = "hacs"
	}
}

// applySubentryLayer executes the subentry ops and folds what happened
// into acc. state.SubentryManaged/Hashes/Attempts are mutated in place by
// ApplySubentryPlan; ApplyNow persists them. No stash directory: a
// subentry's prior data is unreadable, so there is no rollback to offer.
func (r *Reconciler) applySubentryLayer(
	ctx context.Context, subentryOnlyOps []registries.RegOp,
	state *applier.State, acc *registryApplyOutcome,
) {
	subResult := r.registryApplier.ApplySubentryPlan(
		ctx, subentryOnlyOps, state.SubentryManaged, state.SubentryHashes, state.SubentryAttempts)
	// Per-op isolation, as in applyIntegrationLayer: a partial failure
	// leaves this layer's own successful siblings applied.
	acc.registryAppliedCount += len(subResult.Applied)
	acc.registrySkipped = append(acc.registrySkipped, subResult.SkippedErrors...)
	acc.appliedIdentities = append(acc.appliedIdentities, subResult.Applied...)
	if !subResult.OK {
		acc.registryError = subResult.Error
		acc.registryRolledBack = subResult.RolledBack
		acc.failedLayer = "subentries"
	}
}

// differKindDelete mirrors differ.Change.Kind's "delete" literal, which
// internal/differ does not export as a constant.
const differKindDelete = "delete"

func sortedStringKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Rollback restores the last known-good state from the stash directory the
// most recent ApplyNow recorded, successful or not: files via
// Applier.RollbackFrom, then registries if that directory holds their
// stashes. Files first because restoring a copy is idempotent and a replay
// of registry inverses is not, so it runs only after files succeeded.
// Returns a failing Result rather than panicking.
func (r *Reconciler) Rollback(ctx context.Context) applier.Result {
	if !r.opLock.TryLock() {
		// Logged, like the refusal below: the Roll Back button is the only
		// caller, and it answers with a re-render of the same page.
		r.logEvent("rollback skipped: " + errBusy.Error())
		return applier.Result{OK: false, Error: errBusy.Error()}
	}
	defer r.opLock.Unlock()

	r.mu.Lock()
	stashDir := r.lastStashDir
	r.mu.Unlock()

	if stashDir == "" {
		msg := "no previous apply to roll back to"
		r.logEvent("rollback skipped: " + msg)
		return applier.Result{OK: false, Error: msg}
	}

	// After both refusals, like ApplyNow: neither of them is a run.
	run := r.beginRun(history.KindRollback)
	defer run.abandon()

	r.withMu(func() { r.state = StateApplying })
	r.pushStatus()
	r.logEvent("rolling back from " + stashDir)

	result := r.applier.RollbackFrom(stashDir, ConfigRoot)

	registryError := ""
	registryRolledBack := true
	addonError := ""
	addonRolledBack := true
	integrationError := ""
	integrationRolledBack := true
	if result.OK {
		hasRegistryStash := regapply.RegistryStashExists(stashDir)
		// addon_stash.json and integration_stash.json are their own files,
		// independent of registry_stash.json and each other, so each is
		// checked and attempted regardless of how the others went.
		hasAddonStash := regapply.AddonStashExists(stashDir)
		hasIntegrationStash := regapply.IntegrationStashExists(stashDir)

		if hasRegistryStash || hasAddonStash || hasIntegrationStash {
			regState := r.applier.StateLoad()
			if hasRegistryStash {
				regResult := r.registryApplier.RollbackRegistry(
					ctx, stashDir, regState.RegistryManaged, regState.EntityOriginals, regState.DashboardManaged)
				registryRolledBack = regResult.OK
				registryError = regResult.Error
			}
			if hasAddonStash {
				addonResult := r.registryApplier.RollbackAddonPlan(ctx, stashDir, regState.AddonOriginals, regState.AddonRestartOnChange)
				addonRolledBack = addonResult.OK
				addonError = addonResult.Error
			}
			if hasIntegrationStash {
				// A stashed delete replays the declared data as WRITTEN, so
				// a "secret://" reference in it resolves against the secrets
				// file as it stands NOW, not as it stood when the
				// integration was deleted.
				flowResult := r.registryApplier.RollbackFlowPlan(
					ctx, stashDir, regState.IntegrationManaged, regState.IntegrationHashes, regState.IntegrationData,
					secretref.NewResolver(secretsRoot))
				integrationRolledBack = flowResult.OK
				integrationError = flowResult.Error
			}
			if saveErr := r.applier.StateSave(regState); saveErr != nil {
				// Folded into the three rolled-back flags rather than
				// propagated as a harder failure.
				registryRolledBack = false
				addonRolledBack = false
				integrationRolledBack = false
				registryError = joinErrors(registryError, saveErr.Error())
			}
		}
	}

	// Out here because the mirrors follow EVERY rollback: an install with
	// no registry layer takes none of the branches above and would show
	// pre-rollback ownership for up to a whole interval. Re-read rather
	// than reusing regState, which exists only on that branch. The file
	// manifest does not shrink: the repository still asks for those files.
	r.refreshStateMirrors(r.applier.StateLoad())

	combined := applier.Result{
		OK:         result.OK && registryRolledBack && addonRolledBack && integrationRolledBack,
		Changed:    result.Changed,
		Error:      joinErrors(result.Error, registryError, addonError, integrationError),
		RolledBack: result.RolledBack && registryRolledBack && addonRolledBack && integrationRolledBack,
		StashDir:   result.StashDir,
	}

	if combined.OK {
		// The persisted pointer clears with the in-memory one, or a restart
		// would resurrect a rollback point this rollback just consumed.
		st := r.applier.StateLoad()
		if st.LastStashDir != "" || st.LastStashSummary != "" {
			st.LastStashDir = ""
			st.LastStashSummary = ""
			if saveErr := r.applier.StateSave(st); saveErr != nil {
				slog.Warn("recon: could not clear the persisted rollback pointer", "error", saveErr)
			}
		}
		r.withMu(func() {
			r.state = StateInSync
			r.lastError = ""
			r.lastStashDir = ""
			// Cleared with the directory it describes, or it would come
			// back attached to the NEXT apply's stash.
			r.lastStashSummary = ""
		})
		r.logEvent("rollback complete")
	} else {
		r.withMu(func() {
			r.lastError = combined.Error
			r.state = StateError
		})
		r.logEvent("rollback failed: " + combined.Error)
	}

	// The one kind that never carries a SHA: a rollback moves live AWAY
	// from the commit the last apply put there, so one here would read as
	// "rolled back to a3f9c21". Files counts what was RESTORED; RegOps
	// stays zero, since the registry rollbacks report no op count.
	rollbackOutcome := history.OutcomeOK
	if !combined.OK {
		rollbackOutcome = history.OutcomeError
	}
	run.finish(history.Record{
		Outcome:  rollbackOutcome,
		Files:    len(combined.Changed),
		Error:    combined.Error,
		StashDir: stashDir,
	})

	r.pushStatus()
	return combined
}

// tick is one reconcile-loop iteration: check, then apply if needed -
// registry-only drift is enough when dry_run is off, but kind == "error"
// ops never execute, so their presence alone must not trigger an apply,
// or an unresolved conflict would take a backup and open a websocket
// every interval forever. Recovers from a collaborator's panic so one bad
// cycle cannot kill the loop goroutine.
func (r *Reconciler) tick(ctx context.Context) {
	// Silent, unlike every other refusal here: this one fires on a timer
	// for as long as the pause lasts, and the pause is already on the page,
	// in the sensor, and once in the feed. The two things it does before
	// returning are the two that must keep happening.
	if r.isPaused() {
		r.retryPauseFile()
		// Otherwise the sensor is never written again while paused - every
		// other push sits inside an operation a paused agent skips - and
		// HA's States API does not persist entities across a Core restart,
		// so it would vanish. state_attr(..., 'paused') on a missing entity
		// is None, which is falsy: exactly backwards. Changes no bytes.
		r.pushStatus()
		return
	}

	// Registered before the recover below, so it runs after it: a cycle
	// that failed is still a cycle, and RunLoop re-arms either way.
	// Conditional on the flag because a pause can land during the minutes
	// this cycle takes - an unconditional re-arm would restore the
	// countdown SetPaused just cleared, and nothing would clear it again.
	defer func() {
		next := utcISO(time.Now().Add(time.Duration(r.opts.IntervalMinutes) * time.Minute))
		r.withMu(func() {
			if !r.paused {
				r.nextTickUTC = next
			}
		})
	}()
	defer func() {
		if rec := recover(); rec != nil {
			slog.Error("reconcile cycle failed", "panic", rec)
		}
	}()

	r.runCycle(ctx)
}

// runCycle is the unattended loop's whole cycle under ONE opLock hold:
// reconcile, which captures at its tail, then apply. One hold rather than
// tick's former two, because with opts.CaptureLiveChanges on those are two
// halves of a single decision - between them the agent has concluded that a
// path's live copy is the truth, pushed it, and taken it out of the plan,
// and an operation slipping into that gap would act on half of it. The same
// composition ImportLive makes over its import and the reconcile after it.
//
// The refusal is silent, as ReconcileNow's is: the caller is a timer, not
// somebody waiting on a page.
func (r *Reconciler) runCycle(ctx context.Context) {
	if !r.opLock.TryLock() {
		return
	}
	defer r.opLock.Unlock()

	changes := r.reconcileNow(ctx)

	r.mu.Lock()
	pendingRegistry := append([]registries.RegOp(nil), r.pendingRegistry...)
	r.mu.Unlock()

	hasExecutableRegistryOps := false
	for _, op := range pendingRegistry {
		if op.Kind != registries.KindError {
			hasExecutableRegistryOps = true
			break
		}
	}

	// Re-read, not reused from the top: the fetch and plan take minutes,
	// and a pause pressed in that window must stop the apply - which writes
	// files and restarts add-ons - even though the reconcile finishes.
	if (len(changes) > 0 || hasExecutableRegistryOps) && !r.opts.DryRun && !r.isPaused() {
		// Detached like web.opContext: RunLoop's ctx cancels on SIGTERM, and
		// a restart mid-apply must not make applier.Apply read a validated
		// change as a failed check_config, nor cut short regapply's redial.
		// The reconcile above keeps the cancelable ctx - only the apply,
		// which mutates live state, needs a clean stopping point.
		r.applyNow(context.WithoutCancel(ctx))
	}
}

// RunLoop runs tick every opts.IntervalMinutes until ctx is done. Ticks
// first, then waits, re-arming the ticker after each cycle so a slow one
// never makes the next fire early. Keeps running while paused - it is tick
// that returns immediately, which is what lets a resume be a nudge on
// r.wake rather than a goroutine to start and stop.
func (r *Reconciler) RunLoop(ctx context.Context) {
	interval := time.Duration(r.opts.IntervalMinutes) * time.Minute
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		r.tick(ctx)
		ticker.Reset(interval)
		select {
		case <-ctx.Done():
			return
		case <-r.wake:
			// A resume, so the next cycle runs now. Costs at most one
			// redundant cycle when a resume lands mid-cycle; draining the
			// channel first would instead drop a resume that arrived
			// microseconds too early to be seen.
		case <-ticker.C:
		}
	}
}
