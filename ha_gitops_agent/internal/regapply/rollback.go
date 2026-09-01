package regapply

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/registries"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/wsclient"
)

// RollbackRegistry undoes a previous ApplyPlan by replaying
// <stashDir>/registry_stash.json in reverse - the registry counterpart of
// applier.RollbackFrom, behind the same Rollback button.
//
// Not idempotent, unlike the file layer's rollback, so the stash is kept
// rewritten to hold only what still needs undoing. An entry with no live id
// is skipped and logged, never patched up from managed - that could point
// at an unrelated object the user recreated by hand.
//
// A delete's inverse recreates the object under whatever id Home Assistant
// assigns this time. Never panics; returns OK=false when the stash is
// missing, unreadable or corrupt.
//
// originals (state.EntityOriginals) and dashboardManaged
// (state.DashboardManaged) are mutated in place like managed, for the
// entity and dashboard entries the same stash can hold.
func RollbackRegistry(
	ctx context.Context, dialer Dialer, stashDir string,
	managed map[string]string, originals map[string]map[string]any, dashboardManaged map[string]string,
) (result RegistryApplyResult) {
	defer recoverToResult(&result, "regapply: rollback_registry")

	raw, err := readStashFileStrict[any](stashDir, registryStashFile)
	if err != nil {
		msg := fmt.Sprintf("cannot read registry rollback stash at %s: %v",
			filepath.Join(stashDir, registryStashFile), err)
		slog.Warn("regapply: rollback_registry", "error", msg)
		return RegistryApplyResult{OK: false, Error: msg}
	}

	executed := loadRegistryStashOps(raw)

	ws, err := dialer(ctx)
	if err != nil {
		msg := fmt.Sprintf("failed to connect: %v", err)
		slog.Warn("regapply: rollback_registry failed", "error", msg)
		return RegistryApplyResult{OK: false, Error: msg}
	}
	defer ws.Close()

	rolledBack, errMsg := inverseReplayAndPersist(ctx, ws, dialer, executed, managed, originals, dashboardManaged, stashDir, nil)
	if errMsg != "" {
		slog.Warn("regapply: rollback_registry", "error", errMsg)
	} else {
		slog.Info("regapply: rollback_registry undid ops", "count", len(executed))
	}
	return RegistryApplyResult{OK: rolledBack, RolledBack: rolledBack, Error: errMsg}
}

// loadRegistryStashOps decodes a registry_stash.json's contents into stash
// entries defensively: a malformed shape degrades to "no ops" or "skip this
// entry", so a corrupt-but-parseable stash replays whatever it safely can.
func loadRegistryStashOps(raw any) []stashEntry {
	m, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	opsRaw, ok := m["ops"].([]any)
	if !ok {
		return nil
	}

	var entries []stashEntry
	for _, item := range opsRaw {
		itemMap, ok := item.(map[string]any)
		if !ok {
			continue
		}
		kind, _ := itemMap["kind"].(string)
		rtype, _ := itemMap["rtype"].(string)
		key, _ := itemMap["key"].(string)
		liveID, _ := itemMap["live_id"].(string)
		if liveID == "" {
			slog.Warn("regapply: rollback_registry no known live id, skipping", "kind", kind, "rtype", rtype, "key", key)
			continue
		}
		priorObject, _ := itemMap["live_object"].(map[string]any)
		forwardParams, _ := itemMap["forward_params"].(map[string]any)
		originalsExisted, _ := itemMap["entity_originals_existed"].(bool)
		originalsSnapshot, _ := itemMap["entity_originals_snapshot"].(map[string]any)
		adopted, _ := itemMap["adopted"].(bool)
		entries = append(entries, stashEntry{
			Kind: kind, RType: rtype, Key: key, LiveID: liveID,
			PriorObject: priorObject, ForwardParams: forwardParams,
			OriginalsExisted: originalsExisted, OriginalsSnapshot: originalsSnapshot,
			Adopted: adopted,
		})
	}
	return entries
}

// inverseReplayAndPersist best-effort inverts every entry in executed, in
// reverse order.
//
// The stash is rewritten to the SHORTENED list BEFORE each inverse, the
// opposite polarity to the forward path (see the package doc comment). A
// failed write leaves the entry untouched and unattempted - under-reverting
// is the safe outcome. A failed inverse still drops it: managed was left
// unchanged, so ordinary reconciliation still tracks the object.
//
// ws is the connection for the first inverse, or nil when the failure that
// triggered the replay already killed it. Whenever no live connection is on
// hand, dialer redials; if dialer fails or is nil the replay stops, leaving
// every entry it never reached outstanding on disk for a later retry.
//
// prefix holds entries written ahead of executed's shrinking tail on every
// rewrite but never inverted here - ApplyEntityPlan passes the entries an
// earlier, already-committed ApplyPlan wrote this cycle. nil elsewhere.
//
// rolledBack is true only if every write and every inverse succeeded.
func inverseReplayAndPersist(
	ctx context.Context, ws WSClient, dialer Dialer, executed []stashEntry,
	managed map[string]string, originals map[string]map[string]any, dashboardManaged map[string]string,
	stashDir string, prefix []stashEntry,
) (rolledBack bool, errMsg string) {
	outstanding := make([]int, len(executed))
	for i := range executed {
		outstanding[i] = i
	}

	var failures []string
	conn := ws
	ownsConn := false
	defer func() {
		if ownsConn && conn != nil {
			conn.Close()
		}
	}()

	for pos := len(executed) - 1; pos >= 0; pos-- {
		entry := executed[pos]
		label := fmt.Sprintf("%s %s:%s", entry.Kind, entry.RType, entry.Key)

		shortenedIdx := removeInt(outstanding, pos)
		toWrite := append(append([]stashEntry(nil), prefix...), entriesFor(executed, shortenedIdx)...)
		if err := writeRegistryStash(stashDir, toWrite); err != nil {
			failures = append(failures, fmt.Sprintf("%s: stash write failed: %v", label, err))
			continue
		}
		outstanding = shortenedIdx

		if conn == nil {
			if dialer == nil {
				failures = append(failures, fmt.Sprintf("%s: no connection available", label))
				return false, strings.Join(failures, "; ")
			}
			newConn, dialErr := dialer(ctx)
			if dialErr != nil {
				failures = append(failures, fmt.Sprintf("%s: redial after transport error failed: %v", label, dialErr))
				return false, strings.Join(failures, "; ")
			}
			conn = newConn
			ownsConn = true
		}

		invertErr := invertOne(ctx, conn, entry, managed, originals, dashboardManaged)
		if invertErr == nil {
			continue
		}
		failures = append(failures, fmt.Sprintf("%s: %v", label, invertErr))

		if isTransportOrTimeoutError(invertErr) {
			// coder/websocket closes the connection on ANY error, timeouts
			// included, so force a redial before the next entry.
			if ownsConn {
				conn.Close()
			}
			conn = nil
			ownsConn = false
		}
	}

	if len(failures) > 0 {
		return false, strings.Join(failures, "; ")
	}
	return true, ""
}

// removeInt returns a copy of s with the first occurrence of v removed,
// preserving order.
func removeInt(s []int, v int) []int {
	out := make([]int, 0, len(s))
	removed := false
	for _, x := range s {
		if !removed && x == v {
			removed = true
			continue
		}
		out = append(out, x)
	}
	return out
}

// entriesFor picks the entries at idx, in idx's order. Generic so the
// addon and integration replays share it for their own entry types.
func entriesFor[T any](executed []T, idx []int) []T {
	out := make([]T, len(idx))
	for i, x := range idx {
		out[i] = executed[x]
	}
	return out
}

// isTransportOrTimeoutError reports whether err is a *wsclient.Error whose
// Code means the connection is now dead (see the package doc comment).
func isTransportOrTimeoutError(err error) bool {
	var wsErr *wsclient.Error
	if errors.As(err, &wsErr) {
		return wsErr.Code == "transport" || wsErr.Code == "timeout"
	}
	return false
}

// serverGeneratedFields are the timestamps every floor/area/label response
// carries but the create/update request schema rejects - Home Assistant's
// WS schemas refuse unknown keys outright. Together with the id field
// (registries.ResponseIDField) they are the whole difference between the
// two shapes. Never present on a helper storage-collection item.
var serverGeneratedFields = map[string]bool{"created_at": true, "modified_at": true}

// invertOne inverts one executed entry: create -> delete; update -> set
// only entry.ForwardParams' fields back to their stashed prior values (nil
// clears one absent there); delete -> recreate from the stashed prior
// object minus its read-only fields, remapping managed to whatever id Home
// Assistant assigns this time.
//
// Restoring only what the op changed, rather than the full snapshot, both
// avoids resending fields the update schema rejects and keeps the inverse
// from clobbering a field a concurrent process touched in between.
func invertOne(
	ctx context.Context, ws WSClient, entry stashEntry,
	managed map[string]string, originals map[string]map[string]any, dashboardManaged map[string]string,
) error {
	if entry.RType == "entity" {
		return invertEntityOp(ctx, ws, entry, originals)
	}
	if entry.RType == "dashboard" {
		return invertDashboardOp(ctx, ws, entry, dashboardManaged)
	}

	fullKey := entry.RType + ":" + entry.Key
	reqIDField := registries.RequestIDField(entry.RType)

	switch entry.Kind {
	case registries.KindCreate:
		if _, err := ws.Cmd(ctx, msgType(entry.RType, "delete"), map[string]any{reqIDField: entry.LiveID}); err != nil {
			return err
		}
		delete(managed, fullKey)
		return nil

	case registries.KindUpdate:
		restoreParams := make(map[string]any, len(entry.ForwardParams)+1)
		restoreParams[reqIDField] = entry.LiveID
		for fieldName := range entry.ForwardParams {
			var val any
			if entry.PriorObject != nil {
				val = entry.PriorObject[fieldName]
			}
			restoreParams[fieldName] = val
		}
		if _, err := ws.Cmd(ctx, msgType(entry.RType, "update"), restoreParams); err != nil {
			return err
		}
		if entry.Adopted {
			// The update was the adoption, so the inverse releases the
			// object: leaving the key would let a later manifest removal
			// delete a user-made object the agent no longer manages.
			delete(managed, fullKey)
		}
		return nil

	case registries.KindDelete:
		createParams := stripReadonlyFields(entry.RType, entry.PriorObject)
		result, err := ws.Cmd(ctx, msgType(entry.RType, "create"), createParams)
		if err != nil {
			return err
		}
		resultMap, _ := result.(map[string]any)
		if newID := registries.LiveIDOf(entry.RType, resultMap); newID != "" {
			managed[fullKey] = newID
		}
		return nil
	}

	return fmt.Errorf("unreachable: unknown op kind %q", entry.Kind)
}

// stripReadonlyFields returns a stashed live object without its response id
// field (registries.ResponseIDField) or any serverGeneratedFields entry,
// ready to pass back as create params - none are accepted create inputs.
func stripReadonlyFields(rtype string, obj map[string]any) map[string]any {
	if obj == nil {
		return map[string]any{}
	}
	idField := registries.ResponseIDField(rtype)
	out := make(map[string]any, len(obj))
	for k, v := range obj {
		if k == idField || serverGeneratedFields[k] {
			continue
		}
		out[k] = v
	}
	return out
}

// stashOpOnDisk is one entry's on-disk shape inside registry_stash.json,
// used for writing only - reading goes through the duck-typed
// loadRegistryStashOps, which tolerates a hand-edited or corrupted file.
type stashOpOnDisk struct {
	Kind          string         `json:"kind"`
	RType         string         `json:"rtype"`
	Key           string         `json:"key"`
	LiveID        string         `json:"live_id"`
	LiveObject    map[string]any `json:"live_object"`
	ForwardParams map[string]any `json:"forward_params"`
	// entity-only; zero-valued for every other rtype, none of which touch
	// state.EntityOriginals.
	EntityOriginalsExisted  bool           `json:"entity_originals_existed,omitempty"`
	EntityOriginalsSnapshot map[string]any `json:"entity_originals_snapshot,omitempty"`
	// See stashEntry.Adopted; omitted when false so an old stash and a
	// non-adopting update read identically.
	Adopted bool `json:"adopted,omitempty"`
}

type stashFileOnDisk struct {
	Ops []stashOpOnDisk `json:"ops"`
}

func toStashOnDisk(executed []stashEntry) []stashOpOnDisk {
	out := make([]stashOpOnDisk, len(executed))
	for i, e := range executed {
		out[i] = stashOpOnDisk{
			Kind: e.Kind, RType: e.RType, Key: e.Key, LiveID: e.LiveID,
			LiveObject: e.PriorObject, ForwardParams: e.ForwardParams,
			EntityOriginalsExisted: e.OriginalsExisted, EntityOriginalsSnapshot: e.OriginalsSnapshot,
			Adopted: e.Adopted,
		}
	}
	return out
}

// writeRegistryStash atomically rewrites <stashDir>/registry_stash.json to
// hold exactly entries. A variable, not a func, so tests can substitute a
// failing implementation for inverseReplayAndPersist's write-failure path.
var writeRegistryStash = writeRegistryStashReal

func writeRegistryStashReal(stashDir string, entries []stashEntry) error {
	return writeStashFile(stashDir, registryStashFile, stashFileOnDisk{Ops: toStashOnDisk(entries)})
}
