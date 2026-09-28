package dashboards

// Lovelace resources: the resources: list of gitops/dashboards.yaml, the
// JS/CSS files the frontend loads for custom cards.
//
// # WS command shapes (verified against home-assistant/core)
//
// lovelace/info returns {resource_mode: "storage"|"yaml"}. Only in storage
// mode is there a DictStorageCollectionWebsocket at "lovelace/resources":
// .../list returns {id, type, url} per resource (id a uuid hex HA assigns),
// .../create takes {res_type, url}, .../update {resource_id, res_type?,
// url?} and merges them into the stored item, .../delete {resource_id}.
// res_type is stored and listed back as "type". HA enforces no uniqueness
// on url, so two resources can load the same file - which is why this
// package refuses to create one whose path is already live.
//
// # Identity
//
// A resource has no name, only a url, and HACS (27 of production's 30
// resources) appends ?hacstag=<n> to it and rewrites that query on every
// update of the card. So identity is the URL PATH - the url up to its
// query or fragment (resourceURLPath) - for adoption, for duplicate
// detection at load time, and for drift whenever the declared url itself
// carries no query.

import (
	"fmt"
	"sort"
	"strings"
	"unicode"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/difftext"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/registries"
)

// resourceTypes is Home Assistant's RESOURCE_TYPES
// (homeassistant/components/lovelace/const.py), in its order: the only
// res_type values lovelace/resources/create and .../update accept.
var resourceTypes = []string{"js", "css", "module", "html"}

// resourceFields are the only per-item fields a resources: entry may
// declare besides id.
var resourceFields = map[string]bool{"url": true, "type": true}

// resourceKeyPrefix marks a resource's entry in state.DashboardManaged,
// the map dashboards' "dashboard:" entries share.
const resourceKeyPrefix = "resource:"

// resourceModeStorage is the one lovelace/info resource_mode in which the
// resource commands exist at all.
const resourceModeStorage = "storage"

// Resource is one validated entry of the resources: list. URL is kept
// verbatim: a declared query is part of the desired state (see
// PlanResources for how it is compared).
type Resource struct {
	ID   string
	URL  string
	Type string
}

// parseResources validates the resources: list, returning the valid
// entries in manifest order plus every problem found. Absent or null is an
// empty list.
func parseResources(raw any) ([]Resource, []string) {
	result := []Resource{}
	if raw == nil {
		return result, nil
	}
	items, ok := raw.([]any)
	if !ok {
		return result, []string{"dashboards.yaml: resources must be a list"}
	}

	var errs []string
	seen := map[string]bool{}
	pathOwner := map[string]string{}

	for idx, rawItem := range items {
		itemMap, ok := rawItem.(map[string]any)
		if !ok {
			errs = append(errs, fmt.Sprintf("dashboards.yaml: resources[%d] is not a mapping", idx))
			continue
		}

		id, idIsString := itemMap["id"].(string)
		switch {
		case itemMap["id"] == nil:
			errs = append(errs, fmt.Sprintf("dashboards.yaml: resources[%d] has no 'id'", idx))
			continue
		case !idIsString || id == "":
			errs = append(errs, fmt.Sprintf("dashboards.yaml: resources[%d] has an invalid 'id': must be a non-empty string", idx))
			continue
		case !idPattern.MatchString(id):
			errs = append(errs, fmt.Sprintf(
				"dashboards.yaml: resources[%d] has an invalid 'id' '%s': must match [a-z0-9_-]+ "+
					"(lowercase letters, digits, underscore, hyphen)", idx, id))
			continue
		case seen[id]:
			errs = append(errs, fmt.Sprintf("dashboards.yaml: duplicate resource id '%s'", id))
			continue
		}

		res, itemErrs := validateResourceFields(id, itemMap)
		if len(itemErrs) > 0 {
			errs = append(errs, itemErrs...)
			continue
		}

		// Two entries on one path would both adopt the same live resource,
		// or else make the frontend load one file twice (a second define of
		// the same custom element throws).
		path := resourceURLPath(res.URL)
		if other, dup := pathOwner[path]; dup {
			errs = append(errs, fmt.Sprintf(
				"dashboards.yaml: resources '%s' and '%s' have the same URL path %s", other, id, difftext.PyRepr(path)))
			continue
		}

		seen[id] = true
		pathOwner[path] = id
		result = append(result, res)
	}
	return result, errs
}

// validateResourceFields validates one entry's fields besides id: url a
// non-empty string starting "/", "http://" or "https://" with no
// whitespace or control characters, type one of resourceTypes, and nothing
// outside resourceFields.
func validateResourceFields(id string, itemMap map[string]any) (Resource, []string) {
	var errs []string

	var unknown []string
	for k := range itemMap {
		if k != "id" && !resourceFields[k] {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		errs = append(errs, fmt.Sprintf("dashboards.yaml: resource '%s' has unsupported field(s) %s", id, strings.Join(unknown, ", ")))
	}

	url, ok := itemMap["url"].(string)
	switch {
	case !ok || url == "":
		errs = append(errs, fmt.Sprintf("dashboards.yaml: resource '%s' has an invalid or missing 'url'", id))
	case strings.IndexFunc(url, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0:
		errs = append(errs, fmt.Sprintf("dashboards.yaml: resource '%s' url must not contain whitespace or control characters", id))
	case !strings.HasPrefix(url, "/") && !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://"):
		errs = append(errs, fmt.Sprintf("dashboards.yaml: resource '%s' url must start with '/', 'http://' or 'https://'", id))
	// A resource is script every browser session runs, so a URL that reads
	// as local in review must be local: "//host/x.js" is scheme-relative,
	// and browsers read "/\host/x.js" the same way. An off-site URL has to
	// say so with its scheme.
	case strings.HasPrefix(url, "//") || strings.Contains(url, "\\"):
		errs = append(errs, fmt.Sprintf(
			"dashboards.yaml: resource '%s' url must not start with '//' or contain '\\' - write an off-site URL with its https:// scheme", id))
	}

	allowed := strings.Join(resourceTypes, ", ")
	typ, ok := itemMap["type"].(string)
	switch {
	case !ok || typ == "":
		errs = append(errs, fmt.Sprintf("dashboards.yaml: resource '%s' has an invalid or missing 'type': must be one of %s", id, allowed))
	case !isResourceType(typ):
		errs = append(errs, fmt.Sprintf("dashboards.yaml: resource '%s' has an unknown type '%s': must be one of %s", id, typ, allowed))
	}

	if len(errs) > 0 {
		return Resource{}, errs
	}
	return Resource{ID: id, URL: url, Type: typ}, nil
}

func isResourceType(typ string) bool {
	for _, t := range resourceTypes {
		if t == typ {
			return true
		}
	}
	return false
}

// resourceURLPath is url without its query string or fragment: the part
// that names the file, and so a resource's identity (see the file comment).
func resourceURLPath(url string) string {
	if i := strings.IndexAny(url, "?#"); i >= 0 {
		return url[:i]
	}
	return url
}

// resourceURLMatches reports whether live already satisfies declared. A
// declared url with no query or fragment pins only the path, so a HACS
// hacstag bump on the live side is never drift and never overwritten; one
// that carries a query pins the whole url.
func resourceURLMatches(declared, live string) bool {
	if resourceURLPath(declared) == declared {
		return resourceURLPath(live) == declared
	}
	return live == declared
}

// PlanResources computes the Lovelace resource operations needed to
// reconcile live resources toward desired.Resources, as RType "resource"
// ops that regapply.ApplyDashboardPlan executes alongside dashboards'.
//
// liveResources is lovelace/resources/list's output and resourceMode
// lovelace/info's resource_mode (regapply.FetchLiveResources returns both).
// managed is state.DashboardManaged: "resource:<id>" -> live resource id;
// its "dashboard:" entries belong to Plan and are never read here.
//
// Outside storage mode the resource commands do not exist, so every
// declared entry is a KindError op and nothing else is planned - no delete
// and no forget, since a managed id may come back once the mode does.
//
// Otherwise ownership follows Plan's rules with the URL path standing in
// for url_path, which unlike url_path HA does not keep unique:
//
//  1. key managed and the live resource exists -> update op only if the
//     type differs or the url does (resourceURLMatches); params carry only
//     what changed.
//  2. key unmanaged, or managed but its resource gone: exactly one live
//     resource on the declared URL path -> adopt it (an update op, with
//     empty params when nothing drifted, which the applier executes as
//     bookkeeping only); more than one -> an ambiguous-adopt KindError;
//     none -> create. Re-adopting a vanished managed resource's path rather
//     than creating straight away keeps a resource HACS or the user
//     re-added from being loaded twice.
//  3. key managed but no longer declared -> delete the live resource, or
//     forget the mapping when it is gone or a declared key holds it this
//     plan (a renamed manifest id). One an ambiguous adopt left contested
//     is left alone until that error clears.
//
// A live resource neither declared nor managed - every HACS one, unless
// the manifest adopts it - is never touched.
func PlanResources(desired Desired, liveResources []map[string]any, resourceMode string, managed map[string]string) []registries.RegOp {
	if resourceMode != resourceModeStorage {
		return resourceModeErrorOps(desired.Resources, resourceMode)
	}
	if managed == nil {
		managed = map[string]string{}
	}

	liveByID := map[string]map[string]any{}
	for _, obj := range liveResources {
		if id, ok := obj["id"].(string); ok && id != "" {
			liveByID[id] = obj
		}
	}

	declared := map[string]bool{}
	for _, r := range desired.Resources {
		declared[r.ID] = true
	}
	// Only keys still declared claim their live resource; a key dropped
	// this same plan is releasing it, which is what renaming a manifest id
	// looks like - the new id must be free to adopt it.
	claimed := map[string]bool{}
	for fullKey, liveID := range managed {
		if strings.HasPrefix(fullKey, resourceKeyPrefix) && declared[strings.TrimPrefix(fullKey, resourceKeyPrefix)] {
			claimed[liveID] = true
		}
	}

	owned := map[string]string{}
	contested := map[string]bool{}
	var ops []registries.RegOp

	for _, r := range desired.Resources {
		key := resourceKeyPrefix + r.ID

		if liveID, isManaged := managed[key]; isManaged {
			if liveObj, exists := liveByID[liveID]; exists {
				owned[liveID] = key
				if op := planResourceUpdate(r, liveID, liveObj, false); op != nil {
					ops = append(ops, *op)
				}
				continue
			}
		}

		var matches []map[string]any
		for _, obj := range liveResources {
			liveURL, _ := obj["url"].(string)
			liveID, _ := obj["id"].(string)
			if resourceURLPath(liveURL) == resourceURLPath(r.URL) && !claimed[liveID] {
				matches = append(matches, obj)
			}
		}

		switch {
		case len(matches) == 1:
			liveID, _ := matches[0]["id"].(string)
			if liveID == "" {
				ops = append(ops, resourceErrorOp(r.ID, fmt.Sprintf(
					"live resource matched by URL path %s has no usable id field", difftext.PyRepr(resourceURLPath(r.URL)))))
				continue
			}
			owned[liveID] = key
			ops = append(ops, *planResourceUpdate(r, liveID, matches[0], true))
		case len(matches) > 1:
			ops = append(ops, resourceErrorOp(r.ID, fmt.Sprintf(
				"ambiguous adopt: %d live resources have the URL path %s", len(matches), difftext.PyRepr(resourceURLPath(r.URL)))))
			for _, obj := range matches {
				if liveID, _ := obj["id"].(string); liveID != "" {
					contested[liveID] = true
				}
			}
		default:
			ops = append(ops, planResourceCreate(r))
		}
	}

	var deleteKeys []string
	for fullKey := range managed {
		if strings.HasPrefix(fullKey, resourceKeyPrefix) && !declared[strings.TrimPrefix(fullKey, resourceKeyPrefix)] {
			deleteKeys = append(deleteKeys, fullKey)
		}
	}
	sort.Strings(deleteKeys)
	for _, fullKey := range deleteKeys {
		id := strings.TrimPrefix(fullKey, resourceKeyPrefix)
		liveID := managed[fullKey]
		liveObj, exists := liveByID[liveID]
		switch {
		case !exists:
			// Forgotten, not skipped: nothing else would ever drop the
			// mapping, and while it stays the layer keeps fetching for it.
			ops = append(ops, registries.RegOp{
				Kind: KindForget, RType: "resource", Key: id, Params: map[string]any{}, LiveID: liveID,
				DiffText: fmt.Sprintf("stop tracking %s: live resource %s is gone", fullKey, liveID),
			})
		case owned[liveID] != "":
			ops = append(ops, registries.RegOp{
				Kind: KindForget, RType: "resource", Key: id, Params: map[string]any{}, LiveID: liveID,
				DiffText: fmt.Sprintf("stop tracking %s: live resource %s is now managed as %s", fullKey, liveID, owned[liveID]),
			})
		case contested[liveID]:
			continue
		default:
			ops = append(ops, registries.RegOp{
				Kind: KindDelete, RType: "resource", Key: id, Params: map[string]any{}, LiveID: liveID,
				DiffText: resourceDeleteDiffText(id, liveObj),
			})
		}
	}

	return ops
}

// resourceModeErrorOps refuses every declared resource when resourceMode
// is not storage, naming why.
func resourceModeErrorOps(resources []Resource, resourceMode string) []registries.RegOp {
	msg := fmt.Sprintf(
		"Home Assistant reports Lovelace resource_mode %s; resources can only be managed in storage mode",
		difftext.PyRepr(resourceMode))
	if resourceMode == "yaml" {
		msg = "Lovelace resources are in YAML mode: Home Assistant reads them from configuration.yaml " +
			"under lovelace: resources:, so they cannot be managed from dashboards.yaml"
	}
	ops := make([]registries.RegOp, 0, len(resources))
	for _, r := range resources {
		ops = append(ops, resourceErrorOp(r.ID, msg))
	}
	return ops
}

// planResourceCreate builds the KindCreate op for a resource with no live
// match. Params are {url, type} in the live list's naming; regapply sends
// type as res_type.
func planResourceCreate(r Resource) registries.RegOp {
	params := map[string]any{"url": r.URL, "type": r.Type}
	lines := []string{
		fmt.Sprintf("type: %s\n", difftext.ReprValue(r.Type)),
		fmt.Sprintf("url: %s\n", difftext.ReprValue(r.URL)),
	}
	return registries.RegOp{
		Kind: KindCreate, RType: "resource", Key: r.ID, Params: params,
		DiffText: difftext.UnifiedDiff(nil, lines, "live/resource/"+r.ID, "manifest/resource/"+r.ID),
	}
}

// planResourceUpdate builds the KindUpdate op for a resource with a live
// match (managed, or adopted by path this plan). Params hold only the
// fields that drifted. Returns nil when nothing did and this is not a
// forced adopt.
func planResourceUpdate(r Resource, liveID string, liveObj map[string]any, forceAdopt bool) *registries.RegOp {
	params := map[string]any{}
	if liveType, _ := liveObj["type"].(string); liveType != r.Type {
		params["type"] = r.Type
	}
	if liveURL, _ := liveObj["url"].(string); !resourceURLMatches(r.URL, liveURL) {
		params["url"] = r.URL
	}
	if len(params) == 0 && !forceAdopt {
		return nil
	}

	diffText := resourceUpdateDiffText(r.ID, params, liveObj)
	if diffText == "" {
		diffText = fmt.Sprintf("adopted existing resource '%s' (live id %s); no changes needed", r.ID, liveID)
	}
	return &registries.RegOp{
		Kind: KindUpdate, RType: "resource", Key: r.ID, Params: params, LiveID: liveID, DiffText: diffText,
	}
}

// resourceUpdateDiffText diffs the live resource against what it will be
// once params are applied - so a url left alone keeps its live query on
// both sides rather than showing a change that is never sent.
func resourceUpdateDiffText(id string, params, liveObj map[string]any) string {
	if len(params) == 0 {
		return ""
	}
	var before, after []string
	for _, f := range []string{"type", "url"} {
		liveVal := liveObj[f]
		newVal := liveVal
		if v, ok := params[f]; ok {
			newVal = v
		}
		before = append(before, fmt.Sprintf("%s: %s\n", f, difftext.ReprValue(liveVal)))
		after = append(after, fmt.Sprintf("%s: %s\n", f, difftext.ReprValue(newVal)))
	}
	return difftext.UnifiedDiff(before, after, "live/resource/"+id, "manifest/resource/"+id)
}

func resourceDeleteDiffText(id string, liveObj map[string]any) string {
	var lines []string
	for _, f := range []string{"type", "url"} {
		if v, ok := liveObj[f]; ok {
			lines = append(lines, fmt.Sprintf("%s: %s\n", f, difftext.ReprValue(v)))
		}
	}
	return difftext.UnifiedDiff(lines, nil, "live/resource/"+id, "manifest/resource/"+id)
}

func resourceErrorOp(id, msg string) registries.RegOp {
	return registries.RegOp{Kind: KindError, RType: "resource", Key: id, Params: map[string]any{}, Error: msg}
}
