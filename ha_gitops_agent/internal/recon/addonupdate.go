package recon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/regapply"
)

// This file is the auto_update_addons option end to end: a background
// loop asking Supervisor whether any watched add-on is behind, and
// (unless dry_run is on) installing what it finds.
//
// Deliberately NOT part of the reconcile cycle: a reconcile answers "does
// live match the repository", and an add-on version is not in the
// repository at all, so an update can neither be planned as drift nor
// rolled back. Own loop, own cadence, own status fields, and it never
// moves the reconciler's state/lastError - see CheckAddonUpdates.

// addonUpdateCheckInterval is how often RunAddonUpdateLoop asks Supervisor
// what is available. A var so tests can shrink it. Six hours is paced to
// Supervisor's own store refresh, which /addons/<slug>/info answers out
// of - polling faster would only re-read the same cached numbers.
var addonUpdateCheckInterval = 6 * time.Hour

// addonUpdateStartupDelay is how long RunAddonUpdateLoop waits before the
// first check, a var for the same test reason. Startup is the worst moment
// to ask: the first reconcile is already competing for Supervisor while
// the host starts everything else.
var addonUpdateStartupDelay = 2 * time.Minute

// errCheckRunning is CheckAddonUpdates' single-flight refusal. Worded like
// errBusy but deliberately separate: this is the narrower checkLock, and
// waiting on it is a different fact from waiting on the reconcile lock.
var errCheckRunning = errors.New("an add-on update check is already running")

// RunAddonUpdateLoop checks for add-on updates until ctx is done: once
// after addonUpdateStartupDelay, then every addonUpdateCheckInterval.
// Returns immediately when auto_update_addons is empty, so main can start
// it unconditionally. Waits first, unlike RunLoop, which ticks at once.
func (r *Reconciler) RunAddonUpdateLoop(ctx context.Context) {
	if len(r.opts.AutoUpdateAddons) == 0 {
		return
	}

	timer := time.NewTimer(addonUpdateStartupDelay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		r.addonUpdateCycle(ctx)
		timer.Reset(addonUpdateCheckInterval)
	}
}

// addonUpdateCycle runs one CheckAddonUpdates, recovering from a panic so
// one bad cycle cannot kill the loop goroutine - nothing else would
// restart it. A panic still releases opLock: updateOneAddon unlocks via
// defer, which runs as the panic unwinds.
func (r *Reconciler) addonUpdateCycle(ctx context.Context) {
	defer func() {
		if rec := recover(); rec != nil {
			slog.Error("add-on update check failed", "panic", rec)
		}
	}()

	// Paused means no unattended activity, and this loop is exactly that:
	// left running it would back up, pull and restart an add-on every six
	// hours under a banner promising the agent will not act on its own.
	// Silent like tick's paused return; the recorded results stay up.
	if r.isPaused() {
		return
	}

	r.CheckAddonUpdates(ctx)
}

// CheckAddonUpdates runs one check over every slug in auto_update_addons,
// in the option's order, records it in Status().AddonUpdates and persists
// it (addonupdatestore.go) so the next process starts with rows rather
// than a blank card. Exported because POST /addons/check calls it too -
// and that button reaches here WHILE PAUSED, deliberately: a person
// pressing a button is not the unattended activity a pause switches off.
//
// Single-flight on checkLock, refused rather than queued. Two overlapping
// checks would each write a whole new result set over the other's, and
// both would install the same available update. opLock cannot do this job:
// it is released between add-ons (see updateOneAddon), which is exactly
// the window a second check would slip through.
//
// NOTHING here touches state, lastError or lastCycleFailed, on any path
// including a failed update - those answer whether /homeassistant matches
// the repository, and an add-on version is not in the repository. Same
// isolation lastBackupError has.
func (r *Reconciler) CheckAddonUpdates(ctx context.Context) {
	slugs := r.opts.AutoUpdateAddons
	if len(slugs) == 0 {
		// Logged rather than silent: the button re-renders the same page,
		// so a check that declined looks like one that found nothing.
		r.logEvent("add-on update check skipped: auto_update_addons is empty")
		return
	}

	if !r.checkLock.TryLock() {
		r.logEvent("add-on update check skipped: " + errCheckRunning.Error())
		return
	}
	defer r.checkLock.Unlock()

	// Set inside the lock and cleared on the way out, including through a
	// panic recovered by addonUpdateCycle - a flag left set would disable
	// the Check button for the life of the process. This is what
	// checkRunning reads; see it for why Status does not probe the lock.
	r.addonCheckRunning.Store(true)
	defer r.addonCheckRunning.Store(false)

	// Resolved FIRST, and fatal to the cycle when it fails: the self-slug
	// is all that stops this loop updating the add-on it runs inside,
	// which Supervisor does by stopping the container mid-call. No fetch,
	// no update, and the previous results are left untouched.
	selfSlug, err := r.resolveSelfAddonSlug(ctx)
	if err != nil {
		r.reportSelfSlugFailure(err)
		return
	}
	r.withMu(func() { r.addonUpdateSelfSlugFailed = false })

	previous := r.previousAddonUpdates()
	results := make([]AddonUpdateStatus, 0, len(slugs))
	for _, slug := range slugs {
		results = append(results, r.checkOneAddon(ctx, slug, selfSlug, previous[slug]))
	}

	r.withMu(func() { r.addonUpdates = results })

	// Best effort, after the results are live, like importLive's state
	// save: a read-only /data costs the persistence, not the check. Warned
	// rather than logged as an event, since this is a display cache.
	// Marshalled without r.mu: a results slice is never mutated after
	// publication, and checkLock excludes the only builder of another.
	if err := writeAddonUpdatesFile(results); err != nil {
		slog.Warn("recon: add-on update check finished but failed to persist its results",
			"path", addonUpdatesPath, "error", err)
	}

	r.pushStatus()
}

// reportSelfSlugFailure records that this cycle could not confirm the
// agent's own slug, logging only on the transition into that state - see
// addonUpdateSelfSlugFailed.
func (r *Reconciler) reportSelfSlugFailure(err error) {
	var first bool
	r.withMu(func() {
		first = !r.addonUpdateSelfSlugFailed
		r.addonUpdateSelfSlugFailed = true
	})
	if first {
		r.logEvent("add-on update check skipped: cannot confirm this agent's own slug: " + err.Error())
	}
}

// noteAddonCheckFailure records that this cycle got no answer about slug,
// reporting whether that is new. The per-slug half of
// reportSelfSlugFailure's guard: a check failing for a week is still worth
// exactly one event in a 200-entry log.
func (r *Reconciler) noteAddonCheckFailure(slug string) (first bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	first = !r.addonCheckFailed[slug]
	r.addonCheckFailed[slug] = true
	return first
}

// clearAddonCheckFailure records that Supervisor answered about slug,
// reporting whether the previous cycle was failing on it. Called for "not
// installed" too, which is an answer like any other.
func (r *Reconciler) clearAddonCheckFailure(slug string) (recovered bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	recovered = r.addonCheckFailed[slug]
	delete(r.addonCheckFailed, slug)
	return recovered
}

// previousAddonUpdates keys the last cycle's results by slug, for the two
// things a check needs from the one before: facts that outlive a single
// check (AddonUpdateStatus.LastUpdatedUTC), and the versions that decide
// whether an "update available" event would repeat itself.
func (r *Reconciler) previousAddonUpdates() map[string]AddonUpdateStatus {
	r.mu.Lock()
	defer r.mu.Unlock()

	return addonUpdatesBySlug(r.addonUpdates)
}

// checkOneAddon checks (and, when it may, updates) one add-on, returning
// the row that describes what happened. prev is this slug's row from the
// last cycle, or the zero value the first time it is seen.
func (r *Reconciler) checkOneAddon(ctx context.Context, slug, selfSlug string, prev AddonUpdateStatus) AddonUpdateStatus {
	res := AddonUpdateStatus{
		Slug: slug,
		// Only these two carry forward: LastUpdatedUTC records something
		// this agent did, and Name is a label worth keeping so a failed
		// check does not fall back to a raw slug. Version/LatestVersion/
		// UpdateAvailable are Supervisor's current verdict, and a check
		// with no answer has none.
		Name:           prev.Name,
		LastUpdatedUTC: prev.LastUpdatedUTC,
		LastCheckedUTC: utcNowISO(),
	}

	// The selfSlug != "" half mirrors addonopts.Plan's guard: an empty
	// resolved slug must never match anything. Defence in depth against a
	// seam returning ("", nil), which a stub can produce and Supervisor
	// cannot.
	if selfSlug != "" && slug == selfSlug {
		// Refused before the fetch: updating the add-on this loop runs
		// inside means Supervisor stopping the container mid-call, most
		// likely leaving a partially written state.json and nothing left
		// to report it. That update belongs in Supervisor's own UI.
		res.LastResult = AddonUpdateRefusedSelf
		if res.Name == "" {
			// Nothing ever fetches this add-on, so no later cycle fills
			// the display name in.
			res.Name = slug
		}
		return res
	}

	// Lock-free: a fetch mutates nothing, so it need not contend with a
	// reconcile or an apply. Only the update below takes opLock.
	info, err := r.registryApplier.FetchAddonUpdateInfo(ctx, slug)
	if err != nil {
		if errors.Is(err, regapply.ErrAddonNotInstalled) {
			// Its own result, not a failure: an uninstalled slug is a typo
			// or a removed add-on, and an HTTP error would send people
			// hunting a Supervisor problem that is not there. Row-only,
			// since a fact that never changes need not log every 6 hours.
			r.clearAddonCheckFailure(slug)
			res.LastResult = AddonUpdateNotInstalled
			return res
		}
		// Logged on the way INTO failure only (noteAddonCheckFailure).
		// Worth an event unlike the row-only cases: the agent has stopped
		// being able to answer for that add-on at all.
		if r.noteAddonCheckFailure(slug) {
			r.logEvent(fmt.Sprintf("add-on update check failed: %s: %s", slug, err.Error()))
		}
		// An inline sentence rather than a constant, since it carries the
		// error text and nothing reads it back. AddonUpdateStatus.Actionable
		// leaves it OUT of the fold that hides the two permanent verdicts:
		// this is the one unknown a user must act on.
		res.LastResult = "check failed: " + err.Error()
		return res
	}
	if r.clearAddonCheckFailure(slug) {
		r.logEvent("add-on update check recovered: " + slug)
	}

	res.Name = info.Name
	res.Version = info.Version
	res.LatestVersion = info.VersionLatest
	res.UpdateAvailable = info.UpdateAvailable
	if !info.UpdateAvailable {
		res.LastResult = "up to date"
		return res
	}

	if r.opts.DryRun {
		res.LastResult = "update available (dry run, not installing)"
		// Logged only when it says something new: an uninstalled update
		// stays available forever, so this branch is reached every six
		// hours. Keyed on both versions, since a manual partial update
		// ("1.0 -> 2.0" becoming "1.1 -> 2.0") is worth a fresh entry.
		if prev.Version != info.Version || prev.LatestVersion != info.VersionLatest || !prev.UpdateAvailable {
			r.logEvent(fmt.Sprintf("dry run: add-on %s update available (%s -> %s), not installing",
				slug, info.Version, info.VersionLatest))
		}
		return res
	}

	r.updateOneAddon(ctx, info, &res)
	return res
}

// updateOneAddon installs one add-on's available update and fills in the
// outcome on res. Takes opLock per ADD-ON, not around the batch: one
// update blocks for a backup, an image pull and a restart
// (regapply.UpdateAddon's 30-minute budget), so a batch-wide lock would
// make a queued Apply wait out every add-on rather than one. A busy lock
// is not an error - the next interval's check tries again.
func (r *Reconciler) updateOneAddon(ctx context.Context, info regapply.AddonUpdateInfo, res *AddonUpdateStatus) {
	if !r.opLock.TryLock() {
		res.LastResult = "update available, deferred: another operation is running"
		return
	}
	defer r.opLock.Unlock()

	r.logEvent(fmt.Sprintf("updating add-on %s %s -> %s (with backup)", info.Slug, info.Version, info.VersionLatest))

	// Detached like tick's apply step: ctx cancels on SIGTERM, and a
	// restart mid-pull must not abandon an update Supervisor is already
	// executing, leaving the add-on stopped between two versions. The
	// re-fetch shares it so the confirmation cannot be what gets cut off.
	updateCtx := context.WithoutCancel(ctx)
	if err := r.registryApplier.UpdateAddon(updateCtx, info.Slug); err != nil {
		r.logEvent(fmt.Sprintf("add-on update failed: %s: %s", info.Slug, err.Error()))
		res.LastResult = "update failed: " + err.Error()
		return
	}

	// Re-read rather than assume: a 200 says the call finished, not which
	// version is installed. When the re-read works, its numbers win.
	installed := info.VersionLatest
	confirmed, err := r.registryApplier.FetchAddonUpdateInfo(updateCtx, info.Slug)
	if err != nil {
		// The update succeeded; only the confirmation is missing. Left at
		// the version asked for, which is what Supervisor said it installed.
		slog.Warn("recon: add-on updated but the confirming re-read failed", "slug", info.Slug, "error", err)
		res.Version = info.VersionLatest
		res.UpdateAvailable = false
	} else {
		res.Name = confirmed.Name
		res.Version = confirmed.Version
		res.LatestVersion = confirmed.VersionLatest
		res.UpdateAvailable = confirmed.UpdateAvailable
		installed = confirmed.Version

		// An unmoved version means nothing was installed, however cheerful
		// the call - Supervisor reports a no-op update as a success. Its
		// own outcome rather than "updated to <the version it was on>",
		// which would claim in the activity log and last_addon_update
		// something no later cycle corrects. Landing on a version OTHER
		// than the one aimed for is still an update and reported as one.
		if confirmed.Version == info.Version {
			slog.Warn("recon: add-on update reported success but the version did not move",
				"slug", info.Slug, "version", confirmed.Version, "version_latest", confirmed.VersionLatest)
			res.LastResult = "update did not take: still on " + confirmed.Version
			return
		}
	}

	res.LastUpdatedUTC = utcNowISO()
	res.LastResult = "updated to " + installed
	r.withMu(func() { r.lastAddonUpdate = info.Slug + " " + installed })
	r.logEvent(fmt.Sprintf("add-on %s updated to %s", info.Slug, installed))
}
