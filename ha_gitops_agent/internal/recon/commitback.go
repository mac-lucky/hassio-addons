package recon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"time"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/differ"
)

// errBusy is the single "another operation is already running" refusal
// every op-lock guarded entry point hands back, so the error return and
// applier.Result.Error cannot word it differently.
var errBusy = errors.New("another operation is already running")

// errNoFileDrift is CommitDriftBack's refusal when no pending FILE change
// is there to capture - registry/entity/dashboard/addon/integration drift
// is out of scope (gitsync.CommitBack only touches paths under configRoot).
var errNoFileDrift = errors.New("no pending file drift to commit back")

// errNoFetchedTip is CommitDriftBack's refusal when no successful
// ReconcileNow has run yet, so there is no known SHA to branch from.
var errNoFetchedTip = errors.New("no fetched tip to branch from yet - run a reconcile first")

// errCommitBackDisabled is CommitDriftBack's refusal when opts.CommitBack
// is off. Gated here rather than only in the web UI so every caller,
// including a direct POST /commitback, inherits the same refusal.
var errCommitBackDisabled = errors.New("commit_back is disabled")

// driftSetHash fingerprints a pending change set's paths so
// maybeAutoCommitDriftBack can spot unchanged drift across cycles. changes
// is differ.Compute output, already sorted, so no sort is needed here.
func driftSetHash(changes []differ.Change) string {
	h := sha256.New()
	for _, c := range changes {
		h.Write([]byte(c.Kind))
		h.Write([]byte{0})
		h.Write([]byte(c.Path))
		h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// CommitDriftBack commits the currently pending file drift to a new
// "gitops/drift-<timestamp>" branch (see gitsync.CommitBack). Refuses when
// opts.CommitBack is off, and refuses rather than blocks while another
// op-lock guarded operation runs.
func (r *Reconciler) CommitDriftBack(ctx context.Context) (string, error) {
	if !r.opts.CommitBack {
		r.logEvent("commit-back skipped: " + errCommitBackDisabled.Error())
		return "", errCommitBackDisabled
	}
	if !r.opLock.TryLock() {
		r.logEvent("commit-back skipped: " + errBusy.Error())
		return "", errBusy
	}
	defer r.opLock.Unlock()

	return r.commitDriftBack(ctx, r.snapshotPending())
}

// commitDriftBack pushes the branch via Git.CommitBack and persists
// last_drift_branch plus the drift hash, so the automatic policy does not
// re-push standing drift after a restart. Callers (the button and
// maybeAutoCommitDriftBack) already hold opLock. A failure only logs to
// the activity feed - it never sets lastError or flips state to
// StateError, like internal/snapshot's best-effort backups.
func (r *Reconciler) commitDriftBack(ctx context.Context, changes []differ.Change) (string, error) {
	// Both refusals log: only the manual button can reach them, and a
	// button press that logs nothing looks like one that worked.
	if len(changes) == 0 {
		r.logEvent("commit-back skipped: " + errNoFileDrift.Error())
		return "", errNoFileDrift
	}
	r.mu.Lock()
	lastSHA := r.lastSHA
	r.mu.Unlock()
	if lastSHA == "" {
		r.logEvent("commit-back skipped: " + errNoFetchedTip.Error())
		return "", errNoFetchedTip
	}

	branch, err := r.git.CommitBack(ctx, driftFiles(changes), ConfigRoot, lastSHA, time.Now())
	if err != nil {
		r.logEvent("commit-back failed: " + err.Error())
		return "", err
	}

	state := r.applier.StateLoad()
	state.LastDriftBranch = branch
	state.LastDriftBackHash = driftSetHash(changes)
	if saveErr := r.applier.StateSave(state); saveErr != nil {
		slog.Warn("recon: commit-back succeeded but failed to persist state", "error", saveErr)
	}

	r.withMu(func() { r.lastDriftBranch = branch })
	r.logEvent("committed drift back to branch " + branch)
	r.pushStatus()
	return branch, nil
}

// maybeAutoCommitDriftBack is the automatic half of commit_back, called
// from the tail of a successful ReconcileNow. Skips a drift set already
// captured (driftSetHash) so standing drift does not push a branch per
// poll; the hash comes from persisted state, so the dedup survives restart.
func (r *Reconciler) maybeAutoCommitDriftBack(ctx context.Context, changes []differ.Change) {
	state := r.applier.StateLoad()
	if driftSetHash(changes) == state.LastDriftBackHash {
		return
	}
	_, _ = r.commitDriftBack(ctx, changes)
}
