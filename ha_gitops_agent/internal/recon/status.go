package recon

import (
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/history"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/registries"
)

// Reconciler states, mirroring app.main.Reconciler's state machine exactly
// (see also internal/statusd.States, the sensor-side copy of this set).
const (
	StateDisabled     = "disabled"
	StateInSync       = "in_sync"
	StateDriftPending = "drift_pending"
	StateApplying     = "applying"
	StateError        = "error"
	// StateUnseeded is a remote with no such branch yet: nothing to compare
	// against, so none of the states above applies and it is not a failure.
	StateUnseeded = "unseeded"
)

// RollbackPreviewNothing marks an apply whose layers kept no stash.
// Distinct from "" (no summary composed); also shown in /status.json.
const RollbackPreviewNothing = "nothing - this apply's layers keep no restore point"

// Event is one entry in the bounded recent-activity log Status returns.
type Event struct {
	TS      string `json:"ts"`
	Message string `json:"message"`
}

// PendingChange is the web/status.json view of one pending file change.
type PendingChange struct {
	Path     string `json:"path"`
	Kind     string `json:"kind"`
	DiffText string `json:"diff_text"`
}

// PendingRegOp is the web/status.json view of one pending registry
// operation.
type PendingRegOp struct {
	RType    string `json:"rtype"`
	Key      string `json:"key"`
	Kind     string `json:"kind"`
	DiffText string `json:"diff_text"`
	Error    string `json:"error"`
}

// ImportPreview is what an import WOULD capture, plus the per-reason
// account of what was passed over. Files and TotalBytes are what would be
// COMMITTED: the scan minus gitignored paths, which on a real config are
// most of it. Tagged because it is reachable from /status.json.
type ImportPreview struct {
	Files             []string `json:"files"`
	TotalBytes        int64    `json:"total_bytes"`
	SkippedExcluded   int      `json:"skipped_excluded"`
	SkippedGitignored int      `json:"skipped_gitignored"`
	SkippedSecret     int      `json:"skipped_secret"`
	SkippedNonRegular int      `json:"skipped_non_regular"`
	SkippedUnreadable int      `json:"skipped_unreadable"`
}

// The three AddonUpdateStatus.LastResult verdicts a second reader depends
// on, named like RollbackPreviewNothing so Actionable's folding cannot
// break the day somebody rewords one. Every other verdict stays inline.
const (
	// AddonUpdateRefusedSelf is the verdict for this agent's own slug -
	// see checkOneAddon, which refuses before fetching anything.
	AddonUpdateRefusedSelf = "refused: will not update self"
	// AddonUpdateNotInstalled is Supervisor saying the slug is not on this
	// box. An answer like any other, not a failure.
	AddonUpdateNotInstalled = "not installed"
	// AddonUpdateNotCheckedYet is the placeholder for a slug added since
	// the last check, written only by hydrateAddonUpdates. A check never
	// produces it.
	AddonUpdateNotCheckedYet = "not checked yet"
)

// AddonUpdateStatus is what the last check found for one slug in
// auto_update_addons, in that option's order. One entry per CONFIGURED
// slug, not per updatable add-on - a missing row is how a typo'd slug
// stays invisible.
//
// LastResult carries the outcome the other fields cannot: Version and
// LatestVersion are empty when no answer arrived, and UpdateAvailable is
// false both for a current add-on and an unreachable one.
//
// The json tags describe /status.json AND /data/addon_updates.json, which
// the check writes and the next start reads back (addonupdatestore.go), so
// both freeze them. Nothing here is a secret.
type AddonUpdateStatus struct {
	Slug            string `json:"slug"`
	Name            string `json:"name"`
	Version         string `json:"version"`
	LatestVersion   string `json:"latest_version"`
	UpdateAvailable bool   `json:"update_available"`
	LastResult      string `json:"last_result"`
	LastCheckedUTC  string `json:"last_checked_utc"`
	// LastUpdatedUTC is when THIS agent last updated this add-on, or "".
	// Unlike the fields above it survives later checks: it records what
	// this process did, not what Supervisor reports.
	LastUpdatedUTC string `json:"last_updated_utc"`
}

// Actionable reports whether this row can still change on its own. Only
// two verdicts cannot: this agent's own slug and one Supervisor says is
// not installed, neither of which moves until the configuration does. A
// failed check is deliberately NOT in that set - it is the one unknown a
// user must act on. Folding is never dropping: both stay in
// Status().AddonUpdates, counted, one click away.
func (a AddonUpdateStatus) Actionable() bool {
	switch a.LastResult {
	case AddonUpdateRefusedSelf, AddonUpdateNotInstalled:
		return false
	default:
		return true
	}
}

// BlockedItem renders one entry of /data/state.json's failure memory: an
// attempt recorded so the layer does not re-drive the same failing flow
// every interval. Key is the state map's key ("integration:workday_main"),
// which POST /retry takes back; Name is it without the rtype prefix. The
// stored hash is NOT carried - it fingerprints data that may hold
// credentials.
type BlockedItem struct {
	Key   string `json:"key"`
	RType string `json:"rtype"`
	Name  string `json:"name"`
	Error string `json:"error"`
}

// ManagedInventory is everything this agent manages, read off the
// ownership records in /data/state.json. A name in here is one the agent
// may later delete or restore when the repository stops declaring it; a
// name not in here is one it will not.
//
// Two things it does NOT promise, since the card renders exactly this:
//
//   - not "what is in sync" - an imported repository syncs files this
//     agent never wrote, and those are not here (see Files).
//   - not "what will be acted on next cycle" - records outlive the
//     reconcile.* option that made them, so an object from a switched-off
//     layer stays listed and is planned against by nothing. Listed anyway:
//     the record is real and a later re-enable acts on it.
//
// NAMES AND KEYS ONLY, never the values beside them - see managedInventory
// in mirrors.go. Every slice is non-nil (so an empty group serializes as
// [], not null) and sorted, since six of the seven come from maps and an
// unsorted group would change the polled fragment's bytes every render.
type ManagedInventory struct {
	// Files is applier.State.Manifest: the paths this agent has WRITTEN,
	// and so the only ones it will delete or restore later. Not the paths
	// it syncs, which is larger - an import seeds the repository without
	// adding to the manifest, so a freshly imported config tracks hundreds
	// of files and owns none until an apply touches them.
	Files []string `json:"files"`
	// Registry is RegistryManaged's keys WITH their prefix ("floor:ground",
	// "input_boolean:guest_mode"): one map holds several object types and
	// the prefix is all that tells them apart, so stripping it would merge
	// names that are not the same object.
	Registry []string `json:"registry"`
	// The five below are each layer's map with the rtype prefix off, since
	// the group name already says which layer it is: EntityOriginals,
	// DashboardManaged, AddonOriginals, IntegrationManaged, SubentryManaged.
	Entities     []string `json:"entities"`
	Dashboards   []string `json:"dashboards"`
	Addons       []string `json:"addons"`
	Integrations []string `json:"integrations"`
	Subentries   []string `json:"subentries"`
	// Hacs is HacsManaged's keys, prefix off: the integrations this agent
	// downloaded or adopted through HACS. The one group granting no power
	// to remove anything - that layer never uninstalls - and listed for
	// exactly that reason: the record is the only trace that this agent,
	// not a person at the HACS panel, put the integration there.
	Hacs []string `json:"hacs"`
}

// Total is how many objects are managed across every group - the card
// badge's count, and what gates the template's empty state. Derived on
// read like AddonUpdatesAvailable and ApplyableCount, so a hand-built
// status cannot get it wrong.
func (m ManagedInventory) Total() int {
	return len(m.Files) + len(m.Registry) + len(m.Entities) + len(m.Dashboards) +
		len(m.Addons) + len(m.Integrations) + len(m.Subentries) + len(m.Hacs)
}

// clone is a deep copy for Status to hand out: the inventory behind it is
// a mirror shared by every caller, so a caller sorting or appending to a
// group would rewrite the agent's own view of what it manages.
func (m ManagedInventory) clone() ManagedInventory {
	return ManagedInventory{
		Files:        copyNames(m.Files),
		Registry:     copyNames(m.Registry),
		Entities:     copyNames(m.Entities),
		Dashboards:   copyNames(m.Dashboards),
		Addons:       copyNames(m.Addons),
		Integrations: copyNames(m.Integrations),
		Subentries:   copyNames(m.Subentries),
		Hacs:         copyNames(m.Hacs),
	}
}

// copyNames copies one group. Presized rather than append-to-nil, since a
// nil slice serializes as null and every group here is empty on an agent
// that has not applied anything yet.
func copyNames(names []string) []string {
	out := make([]string, len(names))
	copy(out, names)
	return out
}

// Status is the current status for display: sync state, last SHA, pending
// changes, last apply time, last error, and recent activity.
//
// Every string field uses "" for unset, like applier.State: never a
// legitimate value once set, so no presence flag is needed. ImportPreview
// is the one pointer, and has to be - "a preview ran and found nothing"
// renders differently from "no preview has run".
type Status struct {
	State           string `json:"state"`
	Busy            bool   `json:"busy"`
	Configured      bool   `json:"configured"`
	DryRun          bool   `json:"dry_run"`
	RepoURL         string `json:"repo_url"`
	Branch          string `json:"branch"`
	IntervalMinutes int    `json:"interval_minutes"`
	LastSHA         string `json:"last_sha"`
	LastSHAShort    string `json:"last_sha_short"`
	LastApplyUTC    string `json:"last_apply_utc"`
	LastStashDir    string `json:"last_stash_dir"`
	// RollbackPreview is what a rollback from LastStashDir would restore -
	// "3 file(s), registry objects and integrations" - composed by the
	// apply that wrote the stash rather than derived here, since answering
	// it means stat-ing the directory and Status is built several times a
	// minute (see Reconciler.lastStashSummary). Three values, one dialog
	// wording each: a summary; RollbackPreviewNothing, where rolling back
	// changes nothing; and "", where nothing was ever composed (a file-layer
	// failure, an older binary's state, a hand-built status).
	RollbackPreview string `json:"rollback_preview"`
	LastError       string `json:"last_error"`
	// CommitBackEnabled mirrors opts.CommitBack, so the web UI can decide
	// whether to show the "Commit Drift Back" button at all.
	CommitBackEnabled bool `json:"commit_back_enabled"`
	// LastDriftBranch is the most recent commit-back branch name, or "" if
	// commit-back has not run this process (Reconciler.lastDriftBranch).
	LastDriftBranch string `json:"last_drift_branch"`
	// ImportEnabled mirrors opts.AllowImport, so the web UI can decide
	// whether to show the import buttons at all.
	ImportEnabled bool `json:"import_enabled"`
	// LastImportUTC/LastImportSHA describe the most recent successful
	// import, or "" if this repository was never seeded from live. Unlike
	// LastDriftBranch these survive a restart, since the repeat-import
	// confirmation is only useful if it can say when the last one was.
	LastImportUTC      string `json:"last_import_utc"`
	LastImportSHA      string `json:"last_import_sha"`
	LastImportSHAShort string `json:"last_import_sha_short"`
	// LastImportError is the most recent import or preview failure. Apart
	// from LastError because an import failure says nothing about whether
	// live matches the repository (see recon.importLive).
	LastImportError string `json:"last_import_error"`
	// ImportPreview is the most recent preview's result, or nil if none has
	// run or an import has since consumed it. On Status rather than
	// returned alone because the web UI re-renders from Status after every
	// action, so a result not here cannot survive the swap.
	ImportPreview *ImportPreview `json:"import_preview"`
	// LastBackupError is the most recent apply's pre-apply Supervisor
	// backup failure, or "". Apart from LastError and Warnings because it
	// says nothing about the config - the apply went through and Rollback
	// still works off the local stash. It says the second safety net
	// DOCS.md promises was not taken.
	LastBackupError string `json:"last_backup_error"`
	// Warnings holds the last apply's check_config warnings verbatim,
	// possibly multi-line. Unlike LastError, non-empty never implies
	// StateError: check_config already treated the config as valid.
	Warnings string `json:"warnings"`
	// AutoUpdateEnabled is len(opts.AutoUpdateAddons) > 0, so the web UI
	// can decide whether to show the add-on update card - the job
	// CommitBackEnabled/ImportEnabled do for their buttons.
	AutoUpdateEnabled bool `json:"auto_update_enabled"`
	// LastVersionRecordUTC is when the agent last committed the add-on
	// version record (track_addon_versions), or "" if it committed none
	// this process - including the ordinary case where every cycle found
	// the record already correct.
	LastVersionRecordUTC string `json:"last_version_record_utc"`
	// NextCheckUTC is when the unattended loop expects its next cycle, or
	// "" before this process's first cycle finished. An estimate for
	// display, not a schedule anything reads back (Reconciler.nextTickUTC).
	NextCheckUTC string `json:"next_check_utc"`
	// Paused is whether the unattended loop is switched off - see
	// Reconciler.SetPaused for what that stops (the interval cycle) and
	// does not (manual actions, and the webhook).
	//
	// A flag beside the state rather than a state of its own, which is the
	// load-bearing part: statusd.States is a closed vocabulary automations
	// key on, and pause is orthogonal to it. A paused agent that found
	// drift is still drift_pending. Folding pause into State would make
	// every existing "is it in sync" condition silently false.
	//
	// NextCheckUTC is "" whenever this is set.
	Paused bool `json:"paused"`
	// AddonUpdates is the most recent check's per-slug results. Empty is
	// not "nothing to update": with AutoUpdateEnabled set it means no check
	// has ever recorded anything, on this run or an earlier persisted one
	// (addonupdatestore.go).
	//
	// Restored rows let a fresh process report a waiting update within
	// seconds, before its own first check - honest, but it does mean
	// AddonUpdatesAvailable is non-zero before the startup delay elapses.
	// The rows say when they were actually checked.
	AddonUpdates []AddonUpdateStatus `json:"addon_updates"`
	// AddonCheckRunning is whether a check is in flight. Busy cannot
	// answer this, which is why the field exists: Busy is opLock, and a
	// check runs under checkLock, taking opLock only while installing. A
	// spinner keyed off Busy would show nothing for the minutes a check
	// spends talking to Supervisor.
	AddonCheckRunning bool `json:"addon_check_running"`
	// AddonCheckIntervalSeconds is addonUpdateCheckInterval in seconds,
	// published so the dashboard can mark a row stale once it is older
	// than one whole interval. Constant for the process, so it changes no
	// fragment bytes between renders; the comparison is the CLIENT's, since
	// anything computed from "now" server-side would re-swap every poll.
	// 0 means no stale marker, which the interval-shrinking tests want.
	AddonCheckIntervalSeconds int `json:"addon_check_interval_seconds"`
	// PendingCount is files plus registry ops, error-kind included: an
	// unresolvable item is still something the user must see, and Apply's
	// dialog should be honest about the full scope.
	PendingCount    int             `json:"pending_count"`
	Pending         []PendingChange `json:"pending"`
	PendingRegistry []PendingRegOp  `json:"pending_registry"`
	// Blocked is the failure memory in /data/state.json, sorted by Key.
	// Deliberately NOT the same set as PendingRegistry's error-kind ops: a
	// record whose item is still declared appears in both, while one whose
	// item left the manifest (or whose layer was switched off) is planned
	// as nothing and visible only here. Which is which cannot be told at
	// Status time, so all are listed and the card explains what a record
	// means.
	Blocked []BlockedItem `json:"blocked"`
	// Managed is what the agent owns, as of the last operation with a fresh
	// state in hand (refreshStateMirrors). Read-only on the page:
	// un-managing happens by removing the item from the repository.
	Managed ManagedInventory `json:"managed"`
	// HacsRestartPending is the HACS-downloaded integration domains the
	// running Home Assistant has not loaded yet - a custom component is
	// imported at startup, so a fresh download does nothing until a
	// restart. Sorted.
	//
	// A reminder, not a warning: nothing is retried and this agent never
	// restarts Home Assistant. An entry goes on the first cycle that finds
	// its domain loaded.
	HacsRestartPending []string `json:"hacs_restart_pending"`
	// PendingRestartSlugs is the add-ons an apply would restart: every
	// executable add-on op whose slug declares restart_on_change. Sorted
	// and deduplicated, so the confirmation names them stably.
	PendingRestartSlugs []string `json:"pending_restart_slugs"`
	// The standing health flags, mirroring the reconciler's transition
	// guards. Each guard logs once entering failure and once recovering, so
	// 200 events later the condition is true and invisible - these put it
	// back on the page for as long as it lasts.
	HistoryWriteFailing  bool `json:"history_write_failing"`
	VersionRecordFailing bool `json:"version_record_failing"`
	// ImportRecordFailing is an import that was pushed but not recorded, so
	// a restart would show the previous import. Unlike its siblings nothing
	// retries it; only a later successful import clears it.
	ImportRecordFailing bool `json:"import_record_failing"`
	// HacsUnavailable is whether the HACS layer is on but HACS is not
	// installed. The only standing flag raised by a reconcile layer: every
	// other layer failure ends the cycle and this one does not (see
	// planHacsLayer), so without it a user would have reconcile.hacs on and
	// nothing happening.
	HacsUnavailable bool `json:"hacs_unavailable"`
	// AddonCheckFailing is sorted, and has to be: its source is a map, and
	// an unsorted slice would change the fragment's bytes on every poll.
	AddonCheckFailing          []string `json:"addon_check_failing"`
	AddonUpdateSelfSlugFailing bool     `json:"addon_update_self_slug_failing"`
	Events                     []Event  `json:"events"`
	// History is the most recent completed runs, NEWEST-FIRST, at most
	// historyStatusMax. The order differs from Events (oldest-first,
	// reversed by the template): a run history is looked up, where "most
	// recent" is every consumer's first question, and reversing once here
	// makes truncation a plain head of the slice.
	//
	// history.Record is used directly rather than mirrored like
	// PendingChange mirrors differ.Change: a mirror exists when the source
	// serves a different master in a package internal/web must not know.
	// internal/history is a stdlib-only leaf, like internal/humanize. The
	// cost: its json tags now freeze /status.json as well as
	// /data/history.jsonl.
	History []history.Record `json:"history"`
	// HistoryTotal is how many runs are held in all, which the dashboard
	// compares against len(History) to decide whether a longer list is
	// worth linking to (GET /history). At most historyKeep, and it only
	// moves when a run lands, which already rewrites the rows above it.
	HistoryTotal int `json:"history_total"`
}

// AddonUpdatesAvailable is how many watched add-ons are known to be
// behind. Deliberately not len(AddonUpdates), which counts add-ons being
// WATCHED - a current fleet would read "Add-on updates 4" beside four
// rows saying "up to date". Derived on read like ApplyableCount, and the
// sensor's addon_updates_available goes through the same helper.
func (s Status) AddonUpdatesAvailable() int {
	return countAddonUpdatesAvailable(s.AddonUpdates)
}

// RollbackRestoresNothing is whether the last apply's preview names
// nothing to restore - the one case where rolling back is a no-op, and the
// template's third confirm wording. A method rather than a comparison in
// the template, so the marker string lives in exactly one place.
func (s Status) RollbackRestoresNothing() bool {
	return s.RollbackPreview == RollbackPreviewNothing
}

// HasHealthWarnings is whether any standing health flag is raised - what
// gates the header's chip row, so it is absent rather than empty on a
// healthy agent. Derived on read like AddonUpdatesAvailable.
func (s Status) HasHealthWarnings() bool {
	return s.HistoryWriteFailing || s.VersionRecordFailing || s.ImportRecordFailing ||
		s.HacsUnavailable || s.AddonUpdateSelfSlugFailing || len(s.AddonCheckFailing) > 0
}

// ApplyableCount is how many pending items an apply would attempt: the
// files, plus the registry ops that can execute. Deliberately not
// PendingCount, which includes error-kind ops the user must see but no
// apply executes (ApplyPlan skips them, as does tick's
// hasExecutableRegistryOps) - promising "apply 4 change(s)" and applying 3
// is how a dialog stops being believed. Derived on read so PendingCount
// keeps its meaning and the two cannot drift apart.
func (s Status) ApplyableCount() int {
	n := len(s.Pending)
	for _, op := range s.PendingRegistry {
		if op.Kind != registries.KindError {
			n++
		}
	}
	return n
}
