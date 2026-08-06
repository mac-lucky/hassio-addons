package recon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/gitsync"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/history"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/humanize"
)

// errImportDisabled is ImportLive's and PreviewImport's refusal when
// opts.AllowImport is off. Gated here rather than only in the web UI, like
// errCommitBackDisabled: this is the one operation that writes to the
// tracked branch, so a direct POST /import must hit the same refusal.
var errImportDisabled = errors.New("allow_import is disabled")

// ImportSummary is what a completed import did, in recon's own terms - not
// gitsync.ImportResult, so internal/web stays unaware of internal/gitsync
// (the decoupling PendingChange gives differ.Change).
type ImportSummary struct {
	Files     int
	Bytes     int64
	CommitSHA string
	Branch    string
	Created   bool
}

// PreviewImport scans the live config tree and reports what an import
// would capture, changing no state. It runs git in one place - only git
// can answer what .gitignore matches - against a throwaway tree
// (gitsync.PreviewIgnored). Takes opLock despite being read-only, since a
// preview of a half-applied config would be quietly wrong.
func (r *Reconciler) PreviewImport(ctx context.Context) (ImportPreview, error) {
	if !r.opts.AllowImport {
		r.logEvent("import preview skipped: " + errImportDisabled.Error())
		return ImportPreview{}, errImportDisabled
	}
	if !r.opLock.TryLock() {
		r.logEvent("import preview skipped: " + errBusy.Error())
		return ImportPreview{}, errBusy
	}
	defer r.opLock.Unlock()

	plan, err := r.git.ScanLive(ConfigRoot, gitsync.DefaultImportLimits())
	if err != nil {
		r.setImportError(err)
		r.logEvent("import preview failed: " + err.Error())
		r.pushStatus()
		return ImportPreview{}, err
	}
	// Import filters the scan through .gitignore, where most of a real
	// tree goes: the measured install scans 5860 files and commits 191.
	kept, keptBytes, err := r.git.PreviewIgnored(ctx, ConfigRoot, plan.Files)
	if err != nil {
		r.setImportError(err)
		r.logEvent("import preview failed: " + err.Error())
		r.pushStatus()
		return ImportPreview{}, err
	}
	preview := ImportPreview{
		Files:             kept,
		TotalBytes:        keptBytes,
		SkippedExcluded:   plan.SkippedExcluded,
		SkippedGitignored: len(plan.Files) - len(kept),
		SkippedSecret:     plan.SkippedSecret,
		SkippedNonRegular: plan.SkippedNonRegular,
		SkippedUnreadable: plan.SkippedUnreadable,
	}
	r.withMu(func() {
		r.lastImportError = ""
		r.lastImportPreview = &preview
	})

	r.logEvent(fmt.Sprintf("import preview: %d file(s), %s", len(preview.Files), humanize.Bytes(preview.TotalBytes)))
	r.pushStatus()
	return preview, nil
}

// DismissImportPreview drops the last recorded preview, so its card leaves
// the page - the only other way out is running the import it previewed.
// Takes no opLock (it reads no live tree and runs no git) and logs no
// event, since the effect is already visible on the page.
//
// Returns without pushing when there was no preview to drop, and that
// guard is not an optimization: pushStatus is a SYNCHRONOUS Supervisor
// call waiting up to statusd.Timeout (10s) that writes the sensor, a
// recorder row and a state_changed event. Unguarded, every dismiss POST
// would do that for nothing - with allow_import off the field is always
// nil - and would block the handler for the full timeout during an HA
// restart. SetPaused takes the same guard for the same reason.
func (r *Reconciler) DismissImportPreview() {
	var changed bool
	r.withMu(func() {
		changed = r.lastImportPreview != nil
		r.lastImportPreview = nil
	})
	if !changed {
		return
	}
	r.pushStatus()
}

// ImportLive seeds the repository from the live config tree (see
// gitsync.Import). The import and the reconcile after it share one opLock
// acquisition: the import moves the tracked branch, so the pending list is
// stale the instant the push lands and no caller may observe that.
func (r *Reconciler) ImportLive(ctx context.Context) (ImportSummary, error) {
	if !r.opts.AllowImport {
		r.logEvent("import skipped: " + errImportDisabled.Error())
		return ImportSummary{}, errImportDisabled
	}
	if !r.opLock.TryLock() {
		r.logEvent("import skipped: " + errBusy.Error())
		return ImportSummary{}, errBusy
	}
	defer r.opLock.Unlock()

	summary, err := r.importLive(ctx)
	if err != nil {
		return ImportSummary{}, err
	}
	r.reconcileNow(ctx)
	return summary, nil
}

// importLive drives one import; callers must already hold opLock. A
// failure surfaces in lastImportError and records history.OutcomeError,
// but does NOT set lastError or StateError (like commitDriftBack) - the
// record describes the run, the sync state describes live versus the
// repository. Its own run, so an import writes two history rows: import
// and reconcile, which is what actually happened.
func (r *Reconciler) importLive(ctx context.Context) (ImportSummary, error) {
	run := r.beginRun(history.KindImport)
	defer run.abandon()

	if err := r.git.EnsureClone(ctx); err != nil {
		r.setImportError(err)
		r.logEvent("import failed: " + err.Error())
		run.finish(history.Record{Outcome: history.OutcomeError, Error: err.Error()})
		r.pushStatus()
		return ImportSummary{}, err
	}

	res, err := r.git.Import(ctx, ConfigRoot, gitsync.DefaultImportLimits(), time.Now())
	if err != nil {
		r.setImportError(err)
		r.logEvent("import failed: " + err.Error())
		run.finish(history.Record{Outcome: history.OutcomeError, Error: err.Error()})
		r.pushStatus()
		return ImportSummary{}, err
	}

	importedUTC := utcNowISO()

	// Records that an import happened, for display and the repeat-import
	// confirmation. Never touches Manifest or LastGoodSHA: ownership is
	// earned by an apply writing a file, and an import applied nothing to
	// live - adding these paths would let a later repo reorganization
	// propose deleting config the agent never placed there.
	state := r.applier.StateLoad()
	state.LastImportSHA = res.CommitSHA
	state.LastImportUTC = importedUTC
	if saveErr := r.applier.StateSave(state); saveErr != nil {
		slog.Warn("recon: import succeeded but failed to persist state", "error", saveErr)
	}

	r.withMu(func() {
		r.lastImportSHA = res.CommitSHA
		r.lastImportUTC = importedUTC
		r.lastImportError = ""
		// The preview described the pre-import tree; beside a completed
		// import it would read as the import's own result.
		r.lastImportPreview = nil
	})

	onto := "onto " + r.opts.Branch
	if res.Created {
		onto = "onto new branch " + r.opts.Branch
	}
	r.logEvent(fmt.Sprintf("imported %d file(s) (%s) from %s %s at %s",
		res.Files, humanize.Bytes(res.Bytes), ConfigRoot, onto, history.ShortSHA(res.CommitSHA)))
	// Files counts what was COMMITTED, and the SHA is a commit this import
	// created rather than read - the only kind where that is true. RegOps
	// stays zero: an import touches no registries.
	run.finish(history.Record{
		Outcome: history.OutcomeOK,
		SHA:     res.CommitSHA,
		Files:   res.Files,
	})
	r.pushStatus()

	return ImportSummary{
		Files:     res.Files,
		Bytes:     res.Bytes,
		CommitSHA: res.CommitSHA,
		Branch:    r.opts.Branch,
		Created:   res.Created,
	}, nil
}

// setImportError records the message the web UI's import callout shows.
// Clearing happens inline at the two success sites, under their own lock.
func (r *Reconciler) setImportError(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lastImportError = err.Error()
}
