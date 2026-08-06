package regapply

import (
	"context"
	"fmt"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/registries"
)

// FetchLive lists every live registry and helper object keyed by rtype for
// registries.Plan. Lists all supported domains regardless of what the
// manifest declares, so undeclared ones still get their stale entries
// deleted; includeEntities adds the large entity registry.
func FetchLive(ctx context.Context, ws WSClient, includeEntities bool) (map[string][]map[string]any, error) {
	live := make(map[string][]map[string]any, len(registries.RegistryRTypes)+len(registries.SupportedHelperDomains)+1)
	for _, rtype := range registries.RegistryRTypes {
		items, err := listCmd(ctx, ws, rtype)
		if err != nil {
			return nil, err
		}
		live[rtype] = items
	}
	for _, domain := range registries.SupportedHelperDomains {
		items, err := listCmd(ctx, ws, domain)
		if err != nil {
			return nil, err
		}
		live[domain] = items
	}
	if includeEntities {
		items, err := fetchLiveEntities(ctx, ws)
		if err != nil {
			return nil, err
		}
		live["entity"] = items
	}
	return live, nil
}

func listCmd(ctx context.Context, ws WSClient, rtype string) ([]map[string]any, error) {
	result, err := ws.Cmd(ctx, msgType(rtype, "list"), nil)
	if err != nil {
		return nil, err
	}
	return toObjectList(result), nil
}

func toObjectList(result any) []map[string]any {
	list, ok := result.([]any)
	if !ok {
		return []map[string]any{}
	}
	out := make([]map[string]any, 0, len(list))
	for _, item := range list {
		if m, ok := item.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// indexLive indexes FetchLive's output as {rtype: {liveID: object}}. The
// "entity" bucket keys on entity_id, not registries.LiveIDOf: entity
// responses carry both "id" and "entity_id", and only the latter is the
// live id here.
func indexLive(live map[string][]map[string]any) map[string]map[string]map[string]any {
	index := make(map[string]map[string]map[string]any, len(live))
	for rtype, objs := range live {
		byID := make(map[string]map[string]any, len(objs))
		for _, obj := range objs {
			id := registries.LiveIDOf(rtype, obj)
			if rtype == "entity" {
				id, _ = obj["entity_id"].(string)
			}
			if id != "" {
				byID[id] = obj
			}
		}
		index[rtype] = byID
	}
	return index
}

// msgType is config/<rtype>_registry/<action> for floor/area/label,
// <rtype>/<action> for a helper domain.
func msgType(rtype, action string) string {
	if registries.IsRegistryRType(rtype) {
		return fmt.Sprintf("config/%s_registry/%s", rtype, action)
	}
	return fmt.Sprintf("%s/%s", rtype, action)
}
