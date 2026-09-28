package regapply

import (
	"context"
	"errors"
	"fmt"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/difftext"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/registries"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/wsclient"
)

// FetchLive lists every live floor, area and label, plus the helper
// domains named in helperDomains (registries.HelperDomainsFor: the ones the
// manifest declares or this agent manages, which covers every stale entry
// that could need deleting), keyed by rtype for registries.Plan.
// includeEntities adds the large entity registry.
//
// Only those domains, not every supported one: a helper integration that
// is not loaded - an install without default_config - answers its list
// command with unknown_command, and listing a domain nobody uses made every
// cycle fail on it.
//
// Listing person also fills live[registries.PersonYAMLBucket] with the
// persons defined in configuration.yaml (see listPersons).
func FetchLive(ctx context.Context, ws WSClient, helperDomains []string, includeEntities bool) (map[string][]map[string]any, error) {
	live := make(map[string][]map[string]any, len(registries.RegistryRTypes)+len(helperDomains)+2)
	for _, rtype := range registries.RegistryRTypes {
		items, err := listCmd(ctx, ws, rtype)
		if err != nil {
			return nil, err
		}
		live[rtype] = items
	}
	for _, domain := range helperDomains {
		var items []map[string]any
		var err error
		if domain == "person" {
			var yamlItems []map[string]any
			items, yamlItems, err = listPersons(ctx, ws)
			live[registries.PersonYAMLBucket] = yamlItems
		} else {
			items, err = listCmd(ctx, ws, domain)
		}
		if err != nil {
			var wsErr *wsclient.Error
			if errors.As(err, &wsErr) && wsErr.Code == wsCodeUnknownCommand {
				return nil, fmt.Errorf(
					"helpers.yaml declares %s items, or this agent still manages some from an earlier one, but Home Assistant "+
						"has not loaded the %s integration - add default_config: or %s: to configuration.yaml",
					domain, domain, domain)
			}
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

// listPersons unwraps person/list, which unlike every other list command
// answers {"storage": [...], "config": [...]}: storage is what the WS API
// can change, config the persons defined in configuration.yaml. Read
// through toObjectList it came back empty, and every declared person was
// created again on every cycle - so a shape without a storage list is an
// error, not an empty list. A nil result (no answer at all) is empty.
func listPersons(ctx context.Context, ws WSClient) (storage, yamlPersons []map[string]any, err error) {
	result, err := ws.Cmd(ctx, msgType("person", "list"), nil)
	if err != nil {
		return nil, nil, err
	}
	if result == nil {
		return []map[string]any{}, []map[string]any{}, nil
	}
	m, isMap := result.(map[string]any)
	if !isMap {
		return nil, nil, fmt.Errorf("person/list returned %T, want an object with storage and config lists", result)
	}
	if _, isList := m["storage"].([]any); !isList {
		return nil, nil, fmt.Errorf("person/list returned no storage list (keys %v)", difftext.SortedKeys(m))
	}
	return toObjectList(m["storage"]), toObjectList(m["config"]), nil
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

// helperDomainsOf is the helper domains ops touch, for an apply that needs
// only the live objects it is about to change.
func helperDomainsOf(ops []registries.RegOp) []string {
	seen := map[string]bool{}
	var domains []string
	for _, op := range ops {
		if registries.IsRegistryRType(op.RType) || seen[op.RType] {
			continue
		}
		if !registries.IsSupportedHelperDomain(op.RType) {
			continue
		}
		seen[op.RType] = true
		domains = append(domains, op.RType)
	}
	return domains
}
