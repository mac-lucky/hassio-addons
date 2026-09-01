package regapply

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/registries"
)

// ApplyPlan executes plan (from registries.Plan) in order, over one
// connection dialed from dialer. It fetches live state first (so
// update/delete ops can stash prior objects), resets
// <stashDir>/registry_stash.json, then rewrites that file after each
// confirmed op, so it never claims an op happened before its WS call did.
//
// {"$ref": ...} params resolve against creates already executed here,
// falling back to managed; managed is mutated in memory as ops run and
// persisted by the caller. KindError ops come back in SkippedErrors.
//
// The first failing op, for any reason, stops the plan and best-effort
// inverts everything already executed. Never panics.
func ApplyPlan(ctx context.Context, dialer Dialer, plan []registries.RegOp, managed map[string]string, stashDir string) RegistryApplyResult {
	return applyLayerPlan(plan, func(executable []registries.RegOp) RegistryApplyResult {
		return applyPlanInner(ctx, dialer, executable, managed, stashDir)
	})
}

func applyPlanInner(
	ctx context.Context, dialer Dialer, executable []registries.RegOp, managed map[string]string, stashDir string,
) (result RegistryApplyResult) {
	defer recoverToResult(&result, "regapply: apply_plan failed")

	ws, err := dialer(ctx)
	if err != nil {
		msg := fmt.Sprintf("failed to connect: %v", err)
		slog.Warn("regapply: apply_plan failed", "error", msg)
		return RegistryApplyResult{OK: false, Error: msg}
	}
	defer ws.Close()

	live, err := FetchLive(ctx, ws, false)
	if err != nil {
		msg := fmt.Sprintf("unexpected failure: %v", err)
		slog.Warn("regapply: apply_plan failed", "error", msg)
		return RegistryApplyResult{OK: false, Error: msg}
	}
	liveIndex := indexLive(live)

	if err := writeRegistryStash(stashDir, nil); err != nil {
		msg := fmt.Sprintf("unexpected failure: %v", err)
		slog.Warn("regapply: apply_plan failed", "error", msg)
		return RegistryApplyResult{OK: false, Error: msg}
	}

	resolvedIDs := map[string]string{}
	var executed []stashEntry
	for _, op := range executable {
		entry, execErr := executeOne(ctx, ws, op, liveIndex, resolvedIDs, managed)
		if execErr != nil {
			replayConn := ws
			if isTransportOrTimeoutError(execErr) {
				// That connection is dead (see the package doc comment), so
				// let inverseReplayAndPersist redial instead of reusing it.
				replayConn = nil
			}
			rolledBack, undoErr := inverseReplayAndPersist(ctx, replayConn, dialer, executed, managed, nil, nil, stashDir, nil)
			errMsg := fmt.Sprintf("%s %s:%s failed: %v", op.Kind, op.RType, op.Key, execErr)
			if undoErr != "" {
				errMsg = fmt.Sprintf("%s; rollback also incomplete: %s", errMsg, undoErr)
			}
			slog.Warn("regapply: apply_plan", "error", errMsg)
			return RegistryApplyResult{OK: false, Error: errMsg, RolledBack: rolledBack}
		}
		executed = append(executed, entry)

		// Deliberately outside the failure handling above: a failure here
		// must NOT inverse-replay, since the op genuinely succeeded and the
		// replay's own stash writes would fail for the same reason anyway.
		// managed already reflects the op, so all that is lost is rolling
		// back from disk. Execution stops, but the result says they applied.
		if err := writeRegistryStash(stashDir, executed); err != nil {
			msg := fmt.Sprintf(
				"%d op(s) applied successfully, but the rollback journal could not be written after %s %s:%s, "+
					"so no further ops were attempted and these cannot be rolled back from disk: %v",
				len(executed), op.Kind, op.RType, op.Key, err)
			slog.Warn("regapply: apply_plan", "error", msg)
			return RegistryApplyResult{OK: false, Applied: appliedLabels(executed), Error: msg}
		}
	}

	applied := appliedLabels(executed)
	slog.Info("regapply: apply_plan executed", "applied", len(applied))
	return RegistryApplyResult{OK: true, Applied: applied}
}

// appliedLabels renders executed as the "<kind> <rtype>:<key>" strings
// RegistryApplyResult.Applied reports.
func appliedLabels(executed []stashEntry) []string {
	out := make([]string, len(executed))
	for i, e := range executed {
		out[i] = fmt.Sprintf("%s %s:%s", e.Kind, e.RType, e.Key)
	}
	return out
}

// executeOne executes a single create/update/delete op and returns a record
// of it for the stash file and a later invertOne. A failure leaves earlier
// ops' effects on managed unchanged.
func executeOne(
	ctx context.Context, ws WSClient, op registries.RegOp,
	liveIndex map[string]map[string]map[string]any, resolvedIDs map[string]string, managed map[string]string,
) (stashEntry, error) {
	fullKey := op.RType + ":" + op.Key
	reqIDField := registries.RequestIDField(op.RType)

	switch op.Kind {
	case registries.KindCreate:
		params, err := resolveParams(op.Params, resolvedIDs, managed)
		if err != nil {
			return stashEntry{}, err
		}
		result, err := ws.Cmd(ctx, msgType(op.RType, "create"), params)
		if err != nil {
			return stashEntry{}, err
		}
		resultMap, _ := result.(map[string]any)
		newID := registries.LiveIDOf(op.RType, resultMap)
		if newID == "" {
			return stashEntry{}, fmt.Errorf("create of %s did not return a live id", fullKey)
		}
		resolvedIDs[fullKey] = newID
		managed[fullKey] = newID
		return stashEntry{Kind: registries.KindCreate, RType: op.RType, Key: op.Key, LiveID: newID}, nil

	case registries.KindUpdate:
		params, err := resolveParams(op.Params, resolvedIDs, managed)
		if err != nil {
			return stashEntry{}, err
		}
		prior := liveIndex[op.RType][op.LiveID]
		if prior == nil {
			// The object left between plan and apply. Updating it anyway
			// would stash a nil prior, whose inverse sends "field: null" for
			// every touched field and jams the rollback.
			return stashEntry{}, fmt.Errorf("live object %s no longer exists; re-check to plan against current state", op.LiveID)
		}
		_, wasManaged := managed[fullKey]
		reqParams := make(map[string]any, len(params)+1)
		reqParams[reqIDField] = op.LiveID
		for k, v := range params {
			reqParams[k] = v
		}
		if _, err := ws.Cmd(ctx, msgType(op.RType, "update"), reqParams); err != nil {
			return stashEntry{}, err
		}
		resolvedIDs[fullKey] = op.LiveID
		managed[fullKey] = op.LiveID
		// forward_params is what invertOne restores from prior: the fields
		// THIS op touched, $ref-resolved, never the full prior object.
		return stashEntry{
			Kind: registries.KindUpdate, RType: op.RType, Key: op.Key, LiveID: op.LiveID,
			PriorObject: prior, ForwardParams: params, Adopted: !wasManaged,
		}, nil

	case registries.KindDelete:
		prior := liveIndex[op.RType][op.LiveID]
		reqParams := map[string]any{reqIDField: op.LiveID}
		if _, err := ws.Cmd(ctx, msgType(op.RType, "delete"), reqParams); err != nil {
			return stashEntry{}, err
		}
		delete(managed, fullKey)
		return stashEntry{Kind: registries.KindDelete, RType: op.RType, Key: op.Key, LiveID: op.LiveID, PriorObject: prior}, nil
	}

	return stashEntry{}, fmt.Errorf("unreachable: unknown op kind %q", op.Kind)
}

// resolveParams copies params, replacing every {"$ref": "<rtype>:<key>"}
// placeholder - bare or inside a list (an area's labels) - with its live
// id. Returns an *UnresolvedRefError when neither resolvedIDs nor managed
// knows the key.
func resolveParams(params map[string]any, resolvedIDs, managed map[string]string) (map[string]any, error) {
	resolved := make(map[string]any, len(params))
	for fieldName, value := range params {
		if ref, ok := refKey(value); ok {
			liveID, err := resolveRef(ref, resolvedIDs, managed)
			if err != nil {
				return nil, err
			}
			resolved[fieldName] = liveID
			continue
		}
		if list, ok := value.([]any); ok {
			out := make([]any, len(list))
			for i, item := range list {
				if ref, ok := refKey(item); ok {
					liveID, err := resolveRef(ref, resolvedIDs, managed)
					if err != nil {
						return nil, err
					}
					out[i] = liveID
				} else {
					out[i] = item
				}
			}
			resolved[fieldName] = out
			continue
		}
		resolved[fieldName] = value
	}
	return resolved, nil
}

func refKey(value any) (string, bool) {
	m, ok := value.(map[string]any)
	if !ok || len(m) != 1 {
		return "", false
	}
	ref, ok := m["$ref"].(string)
	return ref, ok
}

func resolveRef(ref string, resolvedIDs, managed map[string]string) (string, error) {
	if id, ok := resolvedIDs[ref]; ok {
		return id, nil
	}
	if id, ok := managed[ref]; ok {
		return id, nil
	}
	return "", &UnresolvedRefError{Ref: ref}
}
