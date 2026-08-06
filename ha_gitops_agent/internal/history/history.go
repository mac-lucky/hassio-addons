// Package history appends one durable JSONL line per reconcile, apply,
// rollback or import to /data, outliving the in-memory activity log.
package history

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// Record kinds, one per operation internal/recon serializes behind its
// opLock. The add-on update check has its own timer and is not here.
const (
	KindReconcile = "reconcile"
	KindApply     = "apply"
	KindRollback  = "rollback"
	KindImport    = "import"
)

// Record outcomes: reconcile in_sync/drift/error, apply adds ok/partial/
// rolled_back, rollback and import ok/error. Partial: files landed anyway.
const (
	OutcomeOK         = "ok"
	OutcomeInSync     = "in_sync"
	OutcomeDrift      = "drift"
	OutcomePartial    = "partial"
	OutcomeRolledBack = "rolled_back"
	OutcomeError      = "error"
)

// ErrorMaxLen bounds Record.Error before it reaches disk: retention is a
// count of records, so an unbounded check_config dump would bloat it.
const ErrorMaxLen = 500

// shortSHALen matches the abbreviation internal/recon uses elsewhere.
const shortSHALen = 7

// Record is one completed run. The json tags are load-bearing (columns of
// /data/history.jsonl, keys in /status.json): add fields, never rename.
type Record struct {
	// Kind is one of the Kind* constants. An unrecognized value renders
	// as-is, so a newer binary's file degrades to an odd label, not a gap.
	Kind string `json:"kind"`

	// StartedUTC is when the operation began, RFC3339. Start not end, so
	// DurationMS describes the interval that follows it.
	StartedUTC string `json:"started_utc"`

	// DurationMS is how long the operation took. Milliseconds as an int:
	// time.Duration marshals as an unreadable nanosecond count.
	DurationMS int64 `json:"duration_ms"`

	// SHA is the full commit this run worked from, or "" when there is
	// none; a rollback stores none, or it would read as "rolled back to".
	SHA string `json:"sha"`

	// Outcome is one of the Outcome* constants above.
	Outcome string `json:"outcome"`

	// Files and RegOps count what the Kind did: pending, applied, restored
	// or committed. RegOps stays 0 for rollback and import.
	Files  int `json:"files"`
	RegOps int `json:"reg_ops"`

	// Error is why the run ended badly, truncated at ErrorMaxLen. Non-empty
	// does not imply OutcomeError: a partial apply carries counts and error.
	Error string `json:"error"`

	// StashDir is the per-apply backup directory this run wrote or restored
	// from. Pruned before the row is, so a stale path is a hint, not a link.
	StashDir string `json:"stash_dir"`
}

// ShortSHA abbreviates a commit SHA for display, or returns "" for the
// empty one. The agent's single answer to "how short is a short SHA".
func ShortSHA(sha string) string {
	if len(sha) <= shortSHALen {
		return sha
	}
	return sha[:shortSHALen]
}

// ShortSHA is Record.SHA abbreviated for display, or "" if there is none.
func (r Record) ShortSHA() string { return ShortSHA(r.SHA) }

// Duration is DurationMS as a time.Duration, for callers that format it.
func (r Record) Duration() time.Duration {
	return time.Duration(r.DurationMS) * time.Millisecond
}

// Counts renders the run's counts as "6 file(s), 3 reg op(s)", either
// half alone, or "-" - a zero count reads as a layer that did nothing.
func (r Record) Counts() string {
	switch {
	case r.Files > 0 && r.RegOps > 0:
		return fmt.Sprintf("%d file(s), %d reg op(s)", r.Files, r.RegOps)
	case r.Files > 0:
		return fmt.Sprintf("%d file(s)", r.Files)
	case r.RegOps > 0:
		return fmt.Sprintf("%d reg op(s)", r.RegOps)
	default:
		return "-"
	}
}

// Normalized returns r with this package's bounds applied, at creation
// time so the in-memory copy matches what lands on disk.
func (r Record) Normalized() Record {
	r.Error = truncateError(r.Error)
	return r
}

// truncateError bounds s at ErrorMaxLen, marking that it did so. Cuts on
// a rune boundary so non-ASCII bytes do not become a replacement char.
func truncateError(s string) string {
	if len(s) <= ErrorMaxLen {
		return s
	}
	const marker = " ... (truncated)"
	cut := ErrorMaxLen - len(marker)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return strings.TrimRight(s[:cut], " \t\n") + marker
}
