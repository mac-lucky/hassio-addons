package recon

import (
	"log/slog"
	"time"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/history"
)

// runRecorder is one in-flight operation's history entry: the clock that
// started when the operation did, the SHA it has learned so far, and the
// one place a finished record reaches the store.
//
// A handle rather than a bare start time because a reconcile learns its
// SHA partway through - after the fetch, and only if the fetch worked -
// and every failure after that point should name the commit it was
// working from while every failure before it should name none. Threading
// that through failCycle's twelve call sites as a second parameter would
// be twelve chances to pass the wrong one.
type runRecorder struct {
	r       *Reconciler
	kind    string
	started time.Time
	sha     string
	done    bool
}

// beginRun starts the clock for an operation of the given kind, only once
// it has decided to actually run. A REFUSAL never gets a recorder - dry
// run refuses on every tick, which would bury real runs. Refusals go to
// the activity feed instead (logEvent).
func (r *Reconciler) beginRun(kind string) *runRecorder {
	return &runRecorder{r: r, kind: kind, started: time.Now()}
}

// finish completes rec and hands it to the store, filling in kind, start,
// duration and the SHA when rec carries none - a caller-set rec.SHA wins.
// A second call is a no-op, so abandon can be deferred unconditionally.
// Normalizes here so the dashboard copy is bounded like the file's.
func (rr *runRecorder) finish(rec history.Record) {
	if rr.done {
		return
	}
	rr.done = true

	rec.Kind = rr.kind
	rec.StartedUTC = rr.started.UTC().Format(time.RFC3339)
	rec.DurationMS = time.Since(rr.started).Milliseconds()
	if rec.SHA == "" {
		rec.SHA = rr.sha
	}
	rr.r.recordRun(rec.Normalized())
}

// abandon records a run that never reached a finish, deferred right after
// every beginRun. It exists for panics: web.recoverOp and tick keep the
// process alive, so without this an apply that panicked mid-write would
// leave no row. A no-op once finish has set done.
func (rr *runRecorder) abandon() {
	rr.finish(history.Record{
		Outcome: history.OutcomeError,
		Error:   "the operation did not complete",
	})
}

// recordRun appends rec to the in-memory ring and then to disk, in
// separate critical sections: r.mu is never held across I/O, so a slow
// /data cannot block the web UI's next Status call. A failed write never
// fails the run - only its survival across a restart is lost.
func (r *Reconciler) recordRun(rec history.Record) {
	r.mu.Lock()
	r.runs = append(r.runs, rec)
	if len(r.runs) > historyKeep {
		r.runs = r.runs[len(r.runs)-historyKeep:]
	}
	r.mu.Unlock()

	if err := r.history.Append(rec); err != nil {
		r.noteHistoryWriteFailure(err)
		return
	}
	r.clearHistoryWriteFailure()
}

// noteHistoryWriteFailure logs a history write failure on the TRANSITION
// into that state, not per run - the usual cause is a read-only /data,
// which would otherwise flood the 200-line activity feed. Same guard as
// versionRecordFailed and addonCheckFailed.
func (r *Reconciler) noteHistoryWriteFailure(err error) {
	// slog gets every occurrence; noteVersionRecordFailure splits the two
	// the same way.
	slog.Warn("recon: could not record run history", "error", err)

	var first bool
	r.withMu(func() {
		first = !r.historyWriteFailed
		r.historyWriteFailed = true
	})

	if first {
		r.logEvent("could not record run history: " + err.Error())
	}
}

// clearHistoryWriteFailure is the other half of that guard: one line when
// writing works again, so the feed does not leave it looking broken.
func (r *Reconciler) clearHistoryWriteFailure() {
	var recovered bool
	r.withMu(func() {
		recovered = r.historyWriteFailed
		r.historyWriteFailed = false
	})

	if recovered {
		r.logEvent("run history is being recorded again")
	}
}
