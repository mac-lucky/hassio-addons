package regapply

import (
	"fmt"
	"log/slog"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/registries"
)

// RegistryApplyResult is the outcome of one ApplyPlan or RollbackRegistry
// call.
type RegistryApplyResult struct {
	// OK is true if every executable op in the plan ran successfully.
	OK bool
	// Applied holds "<kind> <rtype>:<key>" for ops executed and still in
	// effect, in execution order. Normally empty when OK is false, since a
	// failed apply is inverse-replayed; the exception is a rollback-journal
	// write failure after a successful op, which does not replay.
	Applied []string
	// Error is "" on full success; otherwise the failing op plus whatever
	// the best-effort inverse-replay could not undo.
	Error string
	// RolledBack is true only if a failure was fully undone - false on full
	// success and on an incomplete inverse-replay.
	RolledBack bool
	// SkippedErrors holds the plan's KindError ops (e.g. an ambiguous
	// adopt-by-name). Never executed, never blocking; the caller surfaces
	// them.
	SkippedErrors []registries.RegOp
}

// applyLayerPlan is the prologue every Apply*Plan shares: split the plan
// into executable ops and KindError refusals, and re-attach the refusals to
// the layer's own result as SkippedErrors.
//
// Returning before inner runs when nothing is executable is load-bearing:
// under recon.needsStashDir's exemption stashDir can be "", and writing a
// stash header then fails with "mkdir : no such file or directory".
func applyLayerPlan(
	ops []registries.RegOp, inner func(executable []registries.RegOp) RegistryApplyResult,
) RegistryApplyResult {
	var executable, skippedErrors []registries.RegOp
	for _, op := range ops {
		if op.Kind == registries.KindError {
			skippedErrors = append(skippedErrors, op)
		} else {
			executable = append(executable, op)
		}
	}

	if len(executable) == 0 {
		return RegistryApplyResult{OK: true, SkippedErrors: skippedErrors}
	}
	result := inner(executable)
	result.SkippedErrors = skippedErrors
	return result
}

// recoverToResult converts a panic into a reported failure on *result,
// logged under logMsg, so a bad type assertion surfaces as a failed apply
// rather than taking the reconcile loop down. Every Apply*/Rollback* entry
// point defers it first; the named return is what carries the substitution
// out. It only reports - unwinding loses the executed-ops slice, so what
// landed is recovered from the stash on the next explicit Rollback.
func recoverToResult(result *RegistryApplyResult, logMsg string) {
	if r := recover(); r != nil {
		msg := fmt.Sprintf("unexpected failure: %v", r)
		slog.Warn(logMsg, "error", msg)
		*result = RegistryApplyResult{OK: false, Error: msg}
	}
}

// UnresolvedRefError is returned when a {"$ref": "<rtype>:<key>"}
// placeholder resolves against neither an earlier create in this plan nor
// managed. Treated exactly like a wsclient.Error: fails the op, replays.
type UnresolvedRefError struct {
	Ref string
}

func (e *UnresolvedRefError) Error() string {
	return fmt.Sprintf("unresolved reference %q", e.Ref)
}

// stashEntry is the in-memory (and, via toStashOnDisk, on-disk) record of
// one executed op - enough to invert it later.
type stashEntry struct {
	Kind          string
	RType         string
	Key           string
	LiveID        string
	PriorObject   map[string]any // nil for a fresh create
	ForwardParams map[string]any // non-nil only for "update"

	// OriginalsExisted/OriginalsSnapshot are entity-only: state.EntityOriginals'
	// entry for this entity_id from immediately BEFORE this op, so
	// invertEntityOp restores the bookkeeping as well as the live fields.
	// False means no entry existed yet, so invert deletes the key.
	OriginalsExisted  bool
	OriginalsSnapshot map[string]any
}
