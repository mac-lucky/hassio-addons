package regapply

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/registries"
)

// The device registry is a plain registry at "config/device_registry", but
// not one registries.Plan manages: no create or delete exists over WS, and
// an update names its device with "device_id" while list answers with "id".
const (
	msgDeviceRegistryList   = "config/device_registry/list"
	msgDeviceRegistryUpdate = "config/device_registry/update"
)

// deviceKindRestore mirrors devices.KindRestore (which is
// entities.KindRestore). Copied rather than imported for the same reason as
// entityKindRestore.
const deviceKindRestore = "restore"

// fetchLiveDevices fetches every device Home Assistant's device registry
// knows about via config/device_registry/list.
func fetchLiveDevices(ctx context.Context, ws WSClient) ([]map[string]any, error) {
	result, err := ws.Cmd(ctx, msgDeviceRegistryList, nil)
	if err != nil {
		return nil, err
	}
	return toObjectList(result), nil
}

// FetchLiveDevices lists the device registry for devices.Plan over one
// dialed connection. The area and label lists devices.Plan also needs (for
// entities.NewRefResolver) come from FetchLive, like the entity layer's.
func FetchLiveDevices(ctx context.Context, dialer Dialer) ([]map[string]any, error) {
	if dialer == nil {
		return nil, errors.New("no websocket dialer was configured for this call")
	}
	ws, err := dialer(ctx)
	if err != nil {
		return nil, err
	}
	defer ws.Close()
	return fetchLiveDevices(ctx, ws)
}

// indexDevicesByID indexes fetchLiveDevices' output by its "id" field.
func indexDevicesByID(live []map[string]any) map[string]map[string]any {
	out := make(map[string]map[string]any, len(live))
	for _, obj := range live {
		if id, ok := obj["id"].(string); ok && id != "" {
			out[id] = obj
		}
	}
	return out
}

// ApplyDevicePlan executes ops (from devices.Plan) against the device
// registry, over one connection dialed from dialer - ApplyEntityPlan's
// sibling in every respect: its own dial and fetch, no undoing an earlier
// layer, and registry_stash.json shared without a reset, whatever earlier
// layers wrote this apply kept as the prefix of every rewrite while a
// mid-plan failure inverts only this call's own entries. The one
// difference: a forget sends nothing, so a plan holding only forgets never
// dials at all.
//
// deviceOriginals is state.DeviceOriginals, mutated in place; the caller
// persists it.
func ApplyDevicePlan(
	ctx context.Context, dialer Dialer, ops []registries.RegOp, deviceOriginals map[string]map[string]any, stashDir string,
) RegistryApplyResult {
	return applyLayerPlan(ops, func(executable []registries.RegOp) RegistryApplyResult {
		return applyDevicePlanInner(ctx, dialer, executable, deviceOriginals, stashDir)
	})
}

func applyDevicePlanInner(
	ctx context.Context, dialer Dialer, executable []registries.RegOp, deviceOriginals map[string]map[string]any, stashDir string,
) (result RegistryApplyResult) {
	defer recoverToResult(&result, "regapply: apply_device_plan failed")

	preExisting, err := readRegistryStashTolerant(stashDir)
	if err != nil {
		msg := fmt.Sprintf("unexpected failure: %v", err)
		slog.Warn("regapply: apply_device_plan failed", "error", msg)
		return RegistryApplyResult{OK: false, Error: msg}
	}

	// A plan of forgets alone sends nothing, so it neither dials nor lists:
	// dropping a vanished device's bookkeeping must not wait on a box that
	// is briefly unreachable. ws then stays a nil interface, which
	// executeDeviceOp never reads for a forget and inverseReplayAndPersist
	// treats as "redial when an inverse needs one".
	var ws WSClient
	liveByID := map[string]map[string]any{}
	if devicePlanSendsAnything(executable) {
		conn, err := dialer(ctx)
		if err != nil {
			msg := fmt.Sprintf("failed to connect: %v", err)
			slog.Warn("regapply: apply_device_plan failed", "error", msg)
			return RegistryApplyResult{OK: false, Error: msg}
		}
		defer conn.Close()
		ws = conn

		liveDevices, err := fetchLiveDevices(ctx, ws)
		if err != nil {
			msg := fmt.Sprintf("unexpected failure: %v", err)
			slog.Warn("regapply: apply_device_plan failed", "error", msg)
			return RegistryApplyResult{OK: false, Error: msg}
		}
		liveByID = indexDevicesByID(liveDevices)
	}

	var executed []stashEntry
	for _, op := range executable {
		entry, execErr := executeDeviceOp(ctx, ws, op, liveByID, deviceOriginals)
		if execErr != nil {
			replayConn := ws
			if isTransportOrTimeoutError(execErr) {
				replayConn = nil
			}
			rolledBack, undoErr := inverseReplayAndPersist(
				ctx, replayConn, dialer, executed, map[string]string{}, nil, nil, deviceOriginals, stashDir, preExisting)
			errMsg := fmt.Sprintf("%s device:%s failed: %v", op.Kind, op.Key, execErr)
			if undoErr != "" {
				errMsg = fmt.Sprintf("%s; rollback also incomplete: %s", errMsg, undoErr)
			}
			slog.Warn("regapply: apply_device_plan", "error", errMsg)
			return RegistryApplyResult{OK: false, Error: errMsg, RolledBack: rolledBack}
		}
		executed = append(executed, entry)

		toWrite := append(append([]stashEntry(nil), preExisting...), executed...)
		if err := writeRegistryStash(stashDir, toWrite); err != nil {
			msg := fmt.Sprintf(
				"%d device op(s) applied successfully, but the rollback journal could not be written after %s device:%s, "+
					"so no further ops were attempted and these cannot be rolled back from disk: %v",
				len(executed), op.Kind, op.Key, err)
			slog.Warn("regapply: apply_device_plan", "error", msg)
			return RegistryApplyResult{OK: false, Applied: appliedLabels(executed), Error: msg}
		}
	}

	applied := appliedLabels(executed)
	slog.Info("regapply: apply_device_plan executed", "applied", len(applied))
	return RegistryApplyResult{OK: true, Applied: applied}
}

// devicePlanSendsAnything reports whether any op needs Home Assistant,
// which is every kind but a forget.
func devicePlanSendsAnything(ops []registries.RegOp) bool {
	for _, op := range ops {
		if op.Kind != registries.KindForget {
			return true
		}
	}
	return false
}

// executeDeviceOp executes a single device update/restore/forget op and
// returns a record of it for the stash file and a later invertDeviceOp.
//
// update and restore are the same config/device_registry/update call, and
// the bookkeeping is executeEntityOp's: "update" records the pre-op live
// value of each op.Params field NOT already in deviceOriginals (mirroring
// devices.Plan's hasNewField), "restore" drops the mapping. A forget drops
// it too but sends nothing - the device is gone. The mapping is keyed by
// op.LiveID; op.Key is the manifest id, or the device id for an op on a
// device no entry names any more, and only labels the op.
func executeDeviceOp(
	ctx context.Context, ws WSClient, op registries.RegOp,
	liveByID map[string]map[string]any, deviceOriginals map[string]map[string]any,
) (stashEntry, error) {
	key := "device:" + op.LiveID
	if op.Kind == registries.KindForget {
		// Checked before the live lookup, which a forget-only plan never
		// fetched. The dropped originals ride along in the entry, so
		// invertDeviceOp can put them back.
		existingOriginals, hasOriginals := deviceOriginals[key]
		delete(deviceOriginals, key)
		return stashEntry{
			Kind: registries.KindForget, RType: "device", Key: op.Key, LiveID: op.LiveID,
			OriginalsExisted: hasOriginals, OriginalsSnapshot: existingOriginals,
		}, nil
	}

	liveObj := liveByID[op.LiveID]
	if liveObj == nil {
		// The device left between plan and apply. HA would refuse the
		// update anyway; this names why, and never stashes a nil prior whose
		// inverse would send null for every field.
		return stashEntry{}, fmt.Errorf("device %s no longer exists; re-check to plan against current state", op.LiveID)
	}

	// Re-check ownership against this call's fresh live fetch, as
	// executeEntityOp does: the plan is cached through the whole dry-run
	// review window, and an integration or config entry that disabled the
	// device since would otherwise be overridden. Only when this op sends
	// disabled_by, the same scope devices.Plan's guard has.
	if _, sendsDisabled := op.Params["disabled_by"]; sendsDisabled {
		if msg := deviceDisabledByGuard(liveObj); msg != "" {
			return stashEntry{}, fmt.Errorf("no longer user-owned since the plan was built: %s", msg)
		}
	}

	existingOriginals, hasOriginals := deviceOriginals[key]
	var priorSnapshot map[string]any
	if hasOriginals {
		priorSnapshot = make(map[string]any, len(existingOriginals))
		for k, v := range existingOriginals {
			priorSnapshot[k] = v
		}
	}

	reqParams := make(map[string]any, len(op.Params)+1)
	reqParams["device_id"] = op.LiveID
	for k, v := range op.Params {
		reqParams[k] = v
	}
	if _, err := ws.Cmd(ctx, msgDeviceRegistryUpdate, reqParams); err != nil {
		return stashEntry{}, err
	}

	switch op.Kind {
	case registries.KindUpdate:
		// A copy, for the same reason as executeEntityOp's.
		updated := make(map[string]any, len(existingOriginals)+len(op.Params))
		for k, v := range existingOriginals {
			updated[k] = v
		}
		for field := range op.Params {
			if _, already := updated[field]; !already {
				updated[field] = clampDeviceDisabledByOriginal(op.LiveID, field, liveObj[field])
			}
		}
		deviceOriginals[key] = updated

	case deviceKindRestore:
		delete(deviceOriginals, key)
	}

	return stashEntry{
		Kind: op.Kind, RType: "device", Key: op.Key, LiveID: op.LiveID,
		PriorObject: liveObj, ForwardParams: op.Params,
		OriginalsExisted: hasOriginals, OriginalsSnapshot: priorSnapshot,
	}, nil
}

// deviceDisabledByGuard mirrors devices.disabledByGuard - copied, not
// imported, for the same reason as deviceKindRestore.
func deviceDisabledByGuard(liveObj map[string]any) string {
	v, ok := liveObj["disabled_by"]
	if !ok || v == nil {
		return ""
	}
	s, _ := v.(string)
	if s == "" || s == "user" {
		return ""
	}
	return fmt.Sprintf("disabled by %q, not by a user; refusing to touch it", s)
}

// clampDeviceDisabledByOriginal is clampByFieldOriginal for the one device
// field whose only valid outgoing values are null/"user": a last net behind
// both guards, which pass a non-string live value through, so what is
// recorded is always something a later restore can send back.
func clampDeviceDisabledByOriginal(deviceID, field string, liveVal any) any {
	if field != "disabled_by" || liveVal == nil {
		return liveVal
	}
	if s, ok := liveVal.(string); ok && s == "user" {
		return liveVal
	}
	slog.Warn(
		"regapply: apply_device_plan: live disabled_by is neither null nor \"user\"; recording null instead of a value a later restore could never send back",
		"device_id", deviceID, "live_value", liveVal)
	return nil
}

// invertDeviceOp inverts one executed device stash entry exactly as
// invertEntityOp does an entity one: the fields entry.ForwardParams touched
// go back to entry.PriorObject's values, then deviceOriginals goes back to
// OriginalsExisted/OriginalsSnapshot under "device:<LiveID>". Unchanged for
// both an "update" and a "restore" entry; a "forget" changed nothing live,
// so its inverse is the bookkeeping alone.
func invertDeviceOp(ctx context.Context, ws WSClient, entry stashEntry, deviceOriginals map[string]map[string]any) error {
	key := "device:" + entry.LiveID
	if entry.Kind != registries.KindForget {
		reqParams := make(map[string]any, len(entry.ForwardParams)+1)
		reqParams["device_id"] = entry.LiveID
		for field := range entry.ForwardParams {
			var val any
			if entry.PriorObject != nil {
				val = entry.PriorObject[field]
			}
			reqParams[field] = val
		}
		if _, err := ws.Cmd(ctx, msgDeviceRegistryUpdate, reqParams); err != nil {
			return err
		}
	}

	if entry.OriginalsExisted {
		deviceOriginals[key] = entry.OriginalsSnapshot
	} else {
		delete(deviceOriginals, key)
	}
	return nil
}
