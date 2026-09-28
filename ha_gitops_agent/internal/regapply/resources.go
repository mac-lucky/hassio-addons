package regapply

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/registries"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/wsclient"
)

// Lovelace resources live in a DictStorageCollectionWebsocket at
// "lovelace/resources", registered only in storage mode: update/delete
// name the item "resource_id", and create/update take the type as
// "res_type" while list returns it as "type". The full verified shape is
// in internal/dashboards' resources.go. list is .../list, not the bare
// "lovelace/resources" HA keeps only for backwards compatibility and has
// marked for removal.
const (
	msgLovelaceInfo            = "lovelace/info"
	msgLovelaceResourcesList   = "lovelace/resources/list"
	msgLovelaceResourcesCreate = "lovelace/resources/create"
	msgLovelaceResourcesUpdate = "lovelace/resources/update"
	msgLovelaceResourcesDelete = "lovelace/resources/delete"
)

// resourceModeStorage is the resource_mode in which the commands above
// exist; dashboards.PlanResources refuses every declared resource in any
// other.
const resourceModeStorage = "storage"

// resourceModeYAML is the mode reported for a Home Assistant too old for
// lovelace/info whose list command is missing too (see FetchLiveResources).
const resourceModeYAML = "yaml"

// isUnknownCommand reports whether err is Core's unknown_command answer.
func isUnknownCommand(err error) bool {
	var wsErr *wsclient.Error
	return errors.As(err, &wsErr) && wsErr.Code == wsCodeUnknownCommand
}

// FetchLiveResources reports Home Assistant's Lovelace resource_mode
// (lovelace/info) and, in storage mode only, every live resource, over one
// connection dialed from dialer. In any other mode resources is nil: the
// list command may not exist there, and dashboards.PlanResources plans
// nothing but refusals.
//
// lovelace/info only exists since Home Assistant 2026.2. Older versions
// answer it unknown_command, and are asked for the list directly instead:
// the resource collection's commands, list included, are registered only
// in storage mode, so the list answering at all IS the mode.
func FetchLiveResources(ctx context.Context, dialer Dialer) (resources []map[string]any, resourceMode string, err error) {
	ws, err := dialer(ctx)
	if err != nil {
		return nil, "", err
	}
	defer ws.Close()

	result, err := ws.Cmd(ctx, msgLovelaceInfo, nil)
	if isUnknownCommand(err) {
		resources, err = fetchLiveResourceList(ctx, ws)
		if isUnknownCommand(err) {
			return nil, resourceModeYAML, nil
		}
		if err != nil {
			return nil, "", err
		}
		return resources, resourceModeStorage, nil
	}
	if err != nil {
		return nil, "", err
	}
	info, _ := result.(map[string]any)
	resourceMode, _ = info["resource_mode"].(string)
	if resourceMode == "" {
		return nil, "", errors.New("lovelace/info returned no resource_mode")
	}
	if resourceMode != resourceModeStorage {
		return nil, resourceMode, nil
	}

	resources, err = fetchLiveResourceList(ctx, ws)
	if err != nil {
		return nil, "", err
	}
	return resources, resourceMode, nil
}

func fetchLiveResourceList(ctx context.Context, ws WSClient) ([]map[string]any, error) {
	result, err := ws.Cmd(ctx, msgLovelaceResourcesList, nil)
	if err != nil {
		return nil, err
	}
	return toObjectList(result), nil
}

// fetchLiveResourcesForOps indexes live resources by id for
// ApplyDashboardPlan, purely to stash an accurate prior: only a resource
// update or delete needs one, so a plan without either skips the call.
func fetchLiveResourcesForOps(ctx context.Context, ws WSClient, ops []registries.RegOp) (map[string]map[string]any, error) {
	needed := false
	for _, op := range ops {
		if op.RType == "resource" && (op.Kind == registries.KindUpdate || op.Kind == registries.KindDelete) {
			needed = true
			break
		}
	}
	if !needed {
		return nil, nil
	}
	live, err := fetchLiveResourceList(ctx, ws)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]map[string]any, len(live))
	for _, obj := range live {
		if id, ok := obj["id"].(string); ok && id != "" {
			byID[id] = obj
		}
	}
	return byID, nil
}

// resourceWSFields translates {url, type} - the live list's naming, which
// ops and stash entries use - into create/update params, where the type is
// res_type. Nothing else is ever passed through, a hand-edited stash
// included.
func resourceWSFields(fields map[string]any) map[string]any {
	out := map[string]any{}
	if v, ok := fields["url"]; ok {
		out["url"] = v
	}
	if v, ok := fields["type"]; ok {
		out["res_type"] = v
	}
	return out
}

// executeResourceOp executes one "resource" op from
// dashboards.PlanResources, persisting through the same stash helpers and
// into the same dashboardManaged map as executeDashboardOp. Every kind is
// a single WS call at most, so each persists once, after its call.
func executeResourceOp(
	ctx context.Context, ws WSClient, op registries.RegOp,
	liveByID map[string]map[string]any, dashboardManaged map[string]string,
	stashDir string, preExisting []stashEntry, executed *[]stashEntry,
) error {
	fullKey := "resource:" + op.Key

	switch op.Kind {
	case registries.KindCreate:
		result, err := ws.Cmd(ctx, msgLovelaceResourcesCreate, resourceWSFields(op.Params))
		if err != nil {
			return err
		}
		resultMap, _ := result.(map[string]any)
		newID, _ := resultMap["id"].(string)
		if newID == "" {
			return fmt.Errorf("create of resource:%s did not return a live id", op.Key)
		}
		dashboardManaged[fullKey] = newID
		entry := stashEntry{Kind: registries.KindCreate, RType: "resource", Key: op.Key, LiveID: newID}
		return appendDashboardStashEntry(stashDir, preExisting, executed, entry)

	case registries.KindUpdate:
		liveObj := liveByID[op.LiveID]
		if liveObj == nil {
			// Gone between plan and apply: an adopt would record a mapping
			// to nothing, an update would fail anyway.
			return fmt.Errorf("resource %s no longer exists; re-check to plan against current state", op.LiveID)
		}
		// Before the write into dashboardManaged, which IS the adoption - a
		// re-adoption too, when the key's old resource is gone and this is
		// another one on the same path: undoing it must release the key,
		// or the key would stay on a resource (HACS's, say) this apply
		// merely found.
		priorID, wasManaged := dashboardManaged[fullKey]
		prior := map[string]any{}
		forward := map[string]any{}
		for f, v := range op.Params {
			prior[f] = liveObj[f]
			forward[f] = v
		}
		// An adopt with nothing drifted has no fields to send, and is
		// bookkeeping only: the mapping and its stash entry.
		if len(forward) > 0 {
			params := resourceWSFields(forward)
			params["resource_id"] = op.LiveID
			if _, err := ws.Cmd(ctx, msgLovelaceResourcesUpdate, params); err != nil {
				return err
			}
		}
		dashboardManaged[fullKey] = op.LiveID
		entry := stashEntry{
			Kind: registries.KindUpdate, RType: "resource", Key: op.Key, LiveID: op.LiveID,
			PriorObject: prior, ForwardParams: forward, Adopted: !wasManaged || priorID != op.LiveID,
		}
		return appendDashboardStashEntry(stashDir, preExisting, executed, entry)

	case registries.KindDelete:
		liveObj := liveByID[op.LiveID]
		if liveObj == nil {
			// Already gone live, so nothing changed and nothing is stashed;
			// an empty prior would leave a rollback nothing to recreate.
			slog.Info("regapply: apply_dashboard_plan: resource already absent, nothing to delete", "key", op.Key)
			delete(dashboardManaged, fullKey)
			return nil
		}
		if _, err := ws.Cmd(ctx, msgLovelaceResourcesDelete, map[string]any{"resource_id": op.LiveID}); err != nil {
			return err
		}
		prior := map[string]any{}
		for _, f := range []string{"url", "type"} {
			if v, ok := liveObj[f]; ok {
				prior[f] = v
			}
		}
		delete(dashboardManaged, fullKey)
		entry := stashEntry{Kind: registries.KindDelete, RType: "resource", Key: op.Key, LiveID: op.LiveID, PriorObject: prior}
		return appendDashboardStashEntry(stashDir, preExisting, executed, entry)

	case registries.KindForget:
		// Bookkeeping only, recorded before the mapping is dropped, as
		// executeDashboardOp does for a dashboard's forget.
		entry := stashEntry{Kind: registries.KindForget, RType: "resource", Key: op.Key, LiveID: op.LiveID}
		if err := appendDashboardStashEntry(stashDir, preExisting, executed, entry); err != nil {
			return err
		}
		delete(dashboardManaged, fullKey)
		return nil
	}
	return fmt.Errorf("unreachable: unknown op kind %q", op.Kind)
}

// invertResourceOp inverts one executed "resource" stash entry: create ->
// delete; update -> restore the prior value of each field ForwardParams
// touched (none for a no-drift adopt, which only releases its mapping);
// delete -> recreate from the stashed url and type, remapping
// dashboardManaged to the id HA assigns this time; forget -> restore the
// mapping only.
//
// A nil prior field is skipped rather than sent: both create's and
// update's schemas reject null for url and res_type.
func invertResourceOp(ctx context.Context, ws WSClient, entry stashEntry, dashboardManaged map[string]string) error {
	fullKey := "resource:" + entry.Key

	switch entry.Kind {
	case registries.KindCreate:
		if _, err := ws.Cmd(ctx, msgLovelaceResourcesDelete, map[string]any{"resource_id": entry.LiveID}); err != nil {
			return err
		}
		delete(dashboardManaged, fullKey)
		return nil

	case registries.KindUpdate:
		restore := map[string]any{}
		for f := range entry.ForwardParams {
			if v := entry.PriorObject[f]; v != nil {
				restore[f] = v
			}
		}
		if len(restore) > 0 {
			params := resourceWSFields(restore)
			params["resource_id"] = entry.LiveID
			if _, err := ws.Cmd(ctx, msgLovelaceResourcesUpdate, params); err != nil {
				return err
			}
		}
		if entry.Adopted {
			// The update was the adoption; releasing the key keeps a later
			// manifest removal from deleting a resource HACS or the user made.
			delete(dashboardManaged, fullKey)
		}
		return nil

	case registries.KindDelete:
		url, _ := entry.PriorObject["url"].(string)
		typ, _ := entry.PriorObject["type"].(string)
		if url == "" || typ == "" {
			return fmt.Errorf("cannot recreate resource:%s: the stashed prior url or type is missing", entry.Key)
		}
		result, err := ws.Cmd(ctx, msgLovelaceResourcesCreate, map[string]any{"url": url, "res_type": typ})
		if err != nil {
			return err
		}
		resultMap, _ := result.(map[string]any)
		if newID, _ := resultMap["id"].(string); newID != "" {
			dashboardManaged[fullKey] = newID
		}
		return nil

	case registries.KindForget:
		dashboardManaged[fullKey] = entry.LiveID
		return nil
	}

	return fmt.Errorf("unreachable: unknown op kind %q", entry.Kind)
}
