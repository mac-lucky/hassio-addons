package regapply

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/registries"
)

// entityKindRestore mirrors entities.KindRestore. Copied rather than
// imported so internal/entities and internal/regapply share only
// internal/registries; internal/recon wires the two together.
const entityKindRestore = "restore"

// fetchLiveEntities fetches every entity Home Assistant's entity registry
// knows about via config/entity_registry/list.
func fetchLiveEntities(ctx context.Context, ws WSClient) ([]map[string]any, error) {
	result, err := ws.Cmd(ctx, "config/entity_registry/list", nil)
	if err != nil {
		return nil, err
	}
	return toObjectList(result), nil
}

// indexEntitiesByID indexes fetchLiveEntities' output by entity_id, not by
// registries.LiveIDOf's "id"/"<rtype>_id" convention (see indexLive).
func indexEntitiesByID(live []map[string]any) map[string]map[string]any {
	out := make(map[string]map[string]any, len(live))
	for _, obj := range live {
		if id, ok := obj["entity_id"].(string); ok && id != "" {
			out[id] = obj
		}
	}
	return out
}

// ApplyEntityPlan executes ops (from entities.Plan) against the entity
// registry, over one connection dialed from dialer. A sibling of ApplyPlan,
// not part of it: this layer runs only after that one succeeded, and a
// failure here must never undo its already-applied ops.
//
// Shares registry_stash.json with ApplyPlan without resetting it: whatever
// that layer wrote this cycle is read as a prefix every rewrite keeps, and
// a mid-plan failure inverts only this call's own entries.
//
// originals is state.EntityOriginals, mutated in place like managed is for
// ApplyPlan; the caller persists it.
func ApplyEntityPlan(
	ctx context.Context, dialer Dialer, ops []registries.RegOp, originals map[string]map[string]any, stashDir string,
) RegistryApplyResult {
	return applyLayerPlan(ops, func(executable []registries.RegOp) RegistryApplyResult {
		return applyEntityPlanInner(ctx, dialer, executable, originals, stashDir)
	})
}

func applyEntityPlanInner(
	ctx context.Context, dialer Dialer, executable []registries.RegOp, originals map[string]map[string]any, stashDir string,
) (result RegistryApplyResult) {
	defer recoverToResult(&result, "regapply: apply_entity_plan failed")

	preExisting, err := readRegistryStashTolerant(stashDir)
	if err != nil {
		msg := fmt.Sprintf("unexpected failure: %v", err)
		slog.Warn("regapply: apply_entity_plan failed", "error", msg)
		return RegistryApplyResult{OK: false, Error: msg}
	}

	ws, err := dialer(ctx)
	if err != nil {
		msg := fmt.Sprintf("failed to connect: %v", err)
		slog.Warn("regapply: apply_entity_plan failed", "error", msg)
		return RegistryApplyResult{OK: false, Error: msg}
	}
	defer ws.Close()

	liveEntities, err := fetchLiveEntities(ctx, ws)
	if err != nil {
		msg := fmt.Sprintf("unexpected failure: %v", err)
		slog.Warn("regapply: apply_entity_plan failed", "error", msg)
		return RegistryApplyResult{OK: false, Error: msg}
	}
	liveByID := indexEntitiesByID(liveEntities)

	var executed []stashEntry
	for _, op := range executable {
		entry, execErr := executeEntityOp(ctx, ws, op, liveByID, originals)
		if execErr != nil {
			replayConn := ws
			if isTransportOrTimeoutError(execErr) {
				replayConn = nil
			}
			rolledBack, undoErr := inverseReplayAndPersist(
				ctx, replayConn, dialer, executed, map[string]string{}, originals, nil, stashDir, preExisting)
			errMsg := fmt.Sprintf("%s entity:%s failed: %v", op.Kind, op.Key, execErr)
			if undoErr != "" {
				errMsg = fmt.Sprintf("%s; rollback also incomplete: %s", errMsg, undoErr)
			}
			slog.Warn("regapply: apply_entity_plan", "error", errMsg)
			return RegistryApplyResult{OK: false, Error: errMsg, RolledBack: rolledBack}
		}
		executed = append(executed, entry)

		toWrite := append(append([]stashEntry(nil), preExisting...), executed...)
		if err := writeRegistryStash(stashDir, toWrite); err != nil {
			msg := fmt.Sprintf(
				"%d entity op(s) applied successfully, but the rollback journal could not be written after %s entity:%s, "+
					"so no further ops were attempted and these cannot be rolled back from disk: %v",
				len(executed), op.Kind, op.Key, err)
			slog.Warn("regapply: apply_entity_plan", "error", msg)
			return RegistryApplyResult{OK: false, Applied: appliedLabels(executed), Error: msg}
		}
	}

	applied := appliedLabels(executed)
	slog.Info("regapply: apply_entity_plan executed", "applied", len(applied))
	return RegistryApplyResult{OK: true, Applied: applied}
}

// executeEntityOp executes a single entity update/restore op and returns a
// record of it for the stash file and a later invertEntityOp.
//
// Both kinds are the same config/entity_registry/update call; only the
// bookkeeping differs. "update" records the pre-op live value of each
// op.Params field NOT already in originals, so a recorded original is never
// overwritten (mirrors entities.Plan's hasNewField - both sides must agree).
// "restore" sends the recorded originals back and drops the mapping.
func executeEntityOp(
	ctx context.Context, ws WSClient, op registries.RegOp, liveByID map[string]map[string]any, originals map[string]map[string]any,
) (stashEntry, error) {
	key := "entity:" + op.Key
	liveObj := liveByID[op.Key]

	// Re-check ownership against this call's fresh live fetch. Plan ran the
	// same guard, but the plan is cached and executed later - the whole
	// dry-run review window - and an integration that claimed disabled_by/
	// hidden_by in between would otherwise be silently overwritten, with a
	// clamped null recorded as the "original" in place of its actual state.
	if liveObj != nil {
		if msg := entityByFieldGuard(liveObj); msg != "" {
			return stashEntry{}, fmt.Errorf("no longer user-owned since the plan was built: %s", msg)
		}
	}

	existingOriginals, hasOriginals := originals[key]
	var priorSnapshot map[string]any
	if hasOriginals {
		priorSnapshot = make(map[string]any, len(existingOriginals))
		for k, v := range existingOriginals {
			priorSnapshot[k] = v
		}
	}

	reqParams := make(map[string]any, len(op.Params)+1)
	reqParams["entity_id"] = op.LiveID
	for k, v := range op.Params {
		reqParams[k] = v
	}
	if _, err := ws.Cmd(ctx, "config/entity_registry/update", reqParams); err != nil {
		return stashEntry{}, err
	}

	switch op.Kind {
	case registries.KindUpdate:
		updated := existingOriginals
		if updated == nil {
			updated = map[string]any{}
		} else {
			// Copy: priorSnapshot above already captured the caller's map,
			// which must not be mutated beyond what this op adds.
			copied := make(map[string]any, len(updated))
			for k, v := range updated {
				copied[k] = v
			}
			updated = copied
		}
		for field := range op.Params {
			if _, already := updated[field]; !already {
				var liveVal any
				if liveObj != nil {
					liveVal = liveObj[field]
				}
				updated[field] = clampByFieldOriginal(op.Key, field, liveVal)
			}
		}
		originals[key] = updated

	case entityKindRestore:
		delete(originals, key)
	}

	return stashEntry{
		Kind: op.Kind, RType: "entity", Key: op.Key, LiveID: op.LiveID,
		PriorObject: liveObj, ForwardParams: op.Params,
		OriginalsExisted: hasOriginals, OriginalsSnapshot: priorSnapshot,
	}, nil
}

// entityByFieldGuard mirrors entities.byFieldGuard - copied, not imported,
// for the same reason as entityKindRestore. executeEntityOp runs it against
// the fresh live fetch so a plan built before an integration claimed the
// entity refuses to fire instead of overwriting that ownership.
func entityByFieldGuard(liveObj map[string]any) string {
	var msgs []string
	for _, field := range [...]string{"disabled_by", "hidden_by"} {
		v, ok := liveObj[field]
		if !ok || v == nil {
			continue
		}
		s, _ := v.(string)
		if s == "" || s == "user" {
			continue
		}
		verb := "disabled"
		if field == "hidden_by" {
			verb = "hidden"
		}
		msgs = append(msgs, fmt.Sprintf("%s by %q, not by a user; refusing to touch it", verb, s))
	}
	return strings.Join(msgs, "; ")
}

// byFieldsClamped are the two entity_registry fields whose only valid
// outgoing values are null/"user". clampByFieldOriginal is a last net
// behind the plan-time and apply-time guards: both pass a non-string live
// value through (mirroring HA's schema, which only ever holds strings), so
// anything left that is neither null nor "user" is recorded as null rather
// than a value a later restore could never send back.
var byFieldsClamped = map[string]bool{"disabled_by": true, "hidden_by": true}

func clampByFieldOriginal(entityID, field string, liveVal any) any {
	if !byFieldsClamped[field] {
		return liveVal
	}
	if liveVal == nil {
		return nil
	}
	if s, ok := liveVal.(string); ok && s == "user" {
		return liveVal
	}
	slog.Warn(
		"regapply: apply_entity_plan: live value for a *_by field is neither null nor \"user\"; recording null instead of a value a later restore could never send back",
		"entity_id", entityID, "field", field, "live_value", liveVal)
	return nil
}

// invertEntityOp inverts one executed entity stash entry: the live fields
// entry.ForwardParams touched go back to entry.PriorObject's values, then
// originals' bookkeeping goes back to OriginalsExisted/OriginalsSnapshot.
// Unchanged for both an "update" and a "restore" entry.
func invertEntityOp(ctx context.Context, ws WSClient, entry stashEntry, originals map[string]map[string]any) error {
	key := "entity:" + entry.Key
	reqParams := make(map[string]any, len(entry.ForwardParams)+1)
	reqParams["entity_id"] = entry.LiveID
	for field := range entry.ForwardParams {
		var val any
		if entry.PriorObject != nil {
			val = entry.PriorObject[field]
		}
		reqParams[field] = val
	}
	if _, err := ws.Cmd(ctx, "config/entity_registry/update", reqParams); err != nil {
		return err
	}

	if entry.OriginalsExisted {
		originals[key] = entry.OriginalsSnapshot
	} else {
		delete(originals, key)
	}
	return nil
}

// readRegistryStashTolerant reads registry_stash.json like RollbackRegistry
// does, except a missing file reports no entries rather than failing: this
// cycle may have had no ApplyPlan ops at all, so no prefix to preserve.
func readRegistryStashTolerant(stashDir string) ([]stashEntry, error) {
	raw, found, err := readStashFile[any](stashDir, registryStashFile)
	if err != nil || !found {
		return nil, err
	}
	return loadRegistryStashOps(raw), nil
}
