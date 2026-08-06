// Package dashboards computes a reconciliation plan for Home Assistant's
// Lovelace dashboards - metadata (title, icon, sidebar visibility) and
// view content - against gitops/dashboards.yaml. Same registries.RegOp
// shape and the same create/adopt/update/delete-only-managed ownership
// registries.Plan implements; see Plan for the one difference (matching by
// url_path, which Home Assistant guarantees unique, not by name).
//
//	dashboards:
//	  - id: gitops-home            # required; becomes/matches url_path
//	    title: GitOps Home         # required
//	    icon: mdi:view-dashboard   # optional
//	    config: gitops/dashboards/home.yaml  # repo-relative view config; required
//	    show_in_sidebar: true      # optional, default true
//
// Both "gitops-home" and "gitops_home" are accepted ids - see idPattern
// for why the hyphenated spelling is the one adoption depends on.
//
// config points at a second, repo-relative YAML file: the Lovelace view
// config. LoadManifest parses it too, normalizing through a JSON round
// trip (loadContent) so it compares equal to HA's JSON-shaped WS responses
// whatever the YAML types (int vs float64) or key order. A config file
// that is missing or unparseable is a per-item problem (Desired.Content,
// surfacing as a KindError op), not a manifest-wide *ManifestError, so one
// broken file never blocks every other declared dashboard.
//
// The default dashboard (url_path nil) and the "lovelace" url_path HA's
// own migration creates are unmanageable in this phase: declaring either
// id is a validation error, not a runtime one.
//
// # WS command shapes (verified against home-assistant/core)
//
// Metadata is a plain DictStorageCollectionWebsocket at api_prefix
// "lovelace/dashboards": .../list (no params; every dashboard's metadata,
// storage- and YAML-mode, carrying id/url_path/title/icon/
// show_in_sidebar/require_admin/mode - "id" is a collection-assigned slug,
// NOT necessarily url_path verbatim, which is why adoption matches on
// url_path); .../create (url_path, title, icon?, show_in_sidebar?,
// allow_single_word? - the hyphen rule is the ONLY check HA makes on
// url_path, so internal/regapply always passes allow_single_word: true and
// an id spelled either way creates fine); .../update (dashboard_id - the
// "id" field from list/create, not url_path - plus any of
// title/icon/show_in_sidebar; icon: null clears it); .../delete
// (dashboard_id).
//
// CONTENT is an unrelated surface: lovelace/config (url_path, force)
// returns the raw config object, or a "config_not_found" error when
// nothing has been saved yet (a freshly created dashboard always starts
// that way); lovelace/config/save (config, url_path) overwrites it,
// admin-only, with no result payload. internal/regapply's dashboards.go
// combines the two into one RegOp per dashboard.
package dashboards

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/difftext"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/fsx"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/registries"
	"github.com/pmezard/go-difflib/difflib"
	yaml "go.yaml.in/yaml/v3"
)

// idPattern is the manifest id syntax: lowercase letters, digits,
// underscore and hyphen. Wider than other manifests' [a-z0-9_]+ because
// this id IS the dashboard's url_path and has to spell whatever Home
// Assistant already has. The hyphen is the load-bearing part: a dashboard
// made through HA's own UI always has one (the frontend never passes
// allow_single_word), so while this was [a-z0-9_]+ every hand-made
// dashboard was unadoptable.
var idPattern = regexp.MustCompile(`^[a-z0-9_-]+$`)

// reservedIDs are manifest ids that can never be declared: "default" is
// not a real url_path (the default dashboard's is nil), and "lovelace" is
// the url_path HA's own storage migration gives it. Matched on the whole
// id exactly - "lovelace-home" is a different dashboard.
var reservedIDs = map[string]bool{"default": true, "lovelace": true}

// allowedFields are the only per-item fields gitops/dashboards.yaml may
// declare besides id. Anything else is a validation error.
var allowedFields = map[string]bool{"title": true, "icon": true, "config": true, "show_in_sidebar": true}

// Kind values dashboards.Plan's ops carry - no new kind, since this
// layer's ownership model is registries.Plan's create/adopt/update/delete.
const (
	KindCreate = registries.KindCreate
	KindUpdate = registries.KindUpdate
	KindDelete = registries.KindDelete
	KindError  = registries.KindError
)

// ManifestError is returned when gitops/dashboards.yaml fails to parse or
// fails STRUCTURAL validation, aggregating every problem found. A config
// FILE that fails to load is a per-item problem instead (Desired.Content).
type ManifestError struct {
	Problems []string
}

func (e *ManifestError) Error() string {
	return strings.Join(e.Problems, "; ")
}

// DashboardContent is the outcome of loading one dashboard's config file:
// Data (normalized view config) or Err naming why - never both.
type DashboardContent struct {
	Data map[string]any
	Err  string
}

// Desired is the parsed, validated contents of gitops/dashboards.yaml
// plus every declared dashboard's loaded config file.
//
// Dashboards holds one map per item, in manifest order, keyed
// "id"/"title"/"config" plus "icon"/"show_in_sidebar" if declared - only
// allowedFields' fields, never registries.yaml's pass-through model, since
// lovelace/dashboards' schemas are fixed. Content maps each id to its
// config file's result, present even for a failed one.
type Desired struct {
	Dashboards []map[string]any
	Content    map[string]DashboardContent
}

func emptyDesired() Desired {
	return Desired{Dashboards: []map[string]any{}, Content: map[string]DashboardContent{}}
}

// LoadManifest loads and validates <workdir>/gitops/dashboards.yaml, then
// loads every declared dashboard's (also workdir-relative) config file. A
// missing manifest yields an empty Desired, not an error: the layer is
// simply inactive for that cycle.
func LoadManifest(workdir string) (Desired, error) {
	path := filepath.Join(workdir, "gitops", "dashboards.yaml")
	info, statErr := os.Stat(path)
	if statErr != nil || !info.Mode().IsRegular() {
		return emptyDesired(), nil
	}

	data, err := os.ReadFile(path) // #nosec G304 -- path is workdir-relative, constructed by this package only
	if err != nil {
		return Desired{}, &ManifestError{Problems: []string{fmt.Sprintf("dashboards.yaml: could not read file: %v", err)}}
	}

	var parsed any
	if err := yaml.Unmarshal(data, &parsed); err != nil {
		return Desired{}, &ManifestError{Problems: []string{fmt.Sprintf("dashboards.yaml: invalid YAML: %v", err)}}
	}
	if parsed == nil {
		return emptyDesired(), nil
	}
	obj, ok := parsed.(map[string]any)
	if !ok {
		return Desired{}, &ManifestError{Problems: []string{"dashboards.yaml: top level must be a mapping"}}
	}

	itemsRaw, present := obj["dashboards"]
	if !present || itemsRaw == nil {
		return emptyDesired(), nil
	}
	items, ok := itemsRaw.([]any)
	if !ok {
		return Desired{}, &ManifestError{Problems: []string{"dashboards.yaml: dashboards must be a list"}}
	}

	var errs []string
	seen := map[string]bool{}
	result := []map[string]any{}

	for idx, rawItem := range items {
		itemMap, ok := rawItem.(map[string]any)
		if !ok {
			errs = append(errs, fmt.Sprintf("dashboards.yaml: dashboards[%d] is not a mapping", idx))
			continue
		}

		id, idIsString := itemMap["id"].(string)
		switch {
		case itemMap["id"] == nil:
			errs = append(errs, fmt.Sprintf("dashboards.yaml: dashboards[%d] has no 'id'", idx))
			continue
		case !idIsString || id == "":
			errs = append(errs, fmt.Sprintf("dashboards.yaml: dashboards[%d] has an invalid 'id': must be a non-empty string", idx))
			continue
		case !idPattern.MatchString(id):
			errs = append(errs, fmt.Sprintf(
				"dashboards.yaml: dashboards[%d] has an invalid 'id' '%s': a dashboard id is its url_path, and must match "+
					"[a-z0-9_-]+ (lowercase letters, digits, underscore, hyphen)", idx, id))
			continue
		case reservedIDs[id]:
			errs = append(errs, fmt.Sprintf("dashboards.yaml: dashboard id '%s' is reserved and cannot be managed", id))
			continue
		case seen[id]:
			errs = append(errs, fmt.Sprintf("dashboards.yaml: duplicate dashboard id '%s'", id))
			continue
		}

		item, itemErrs := validateItemFields(id, itemMap)
		if len(itemErrs) > 0 {
			errs = append(errs, itemErrs...)
			continue
		}

		seen[id] = true
		result = append(result, item)
	}

	if len(errs) > 0 {
		return Desired{}, &ManifestError{Problems: errs}
	}

	content := make(map[string]DashboardContent, len(result))
	for _, item := range result {
		id, _ := item["id"].(string)
		configPath, _ := item["config"].(string)
		data, loadErr := loadContent(workdir, configPath)
		if loadErr != nil {
			content[id] = DashboardContent{Err: loadErr.Error()}
			continue
		}
		content[id] = DashboardContent{Data: data}
	}

	return Desired{Dashboards: result, Content: content}, nil
}

// validateItemFields validates one item's fields besides id: title and
// config are required non-empty strings, icon must be a string or null,
// show_in_sidebar a boolean, and anything outside allowedFields is an
// "unsupported field" error.
//
// These checks also keep rendered metadata to strings, booleans and nulls
// - the whole value domain the diff-text builders hand to
// difftext.ReprValue - so widening this needs a diff-text assertion too.
func validateItemFields(id string, itemMap map[string]any) (map[string]any, []string) {
	item := map[string]any{"id": id}
	var errs []string
	var unknown []string

	for k, v := range itemMap {
		switch {
		case k == "id":
			continue
		case !allowedFields[k]:
			unknown = append(unknown, k)
		default:
			item[k] = v
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		errs = append(errs, fmt.Sprintf("dashboards.yaml: dashboard '%s' has unsupported field(s) %s", id, strings.Join(unknown, ", ")))
	}

	if title, ok := item["title"].(string); !ok || title == "" {
		errs = append(errs, fmt.Sprintf("dashboards.yaml: dashboard '%s' has an invalid or missing 'title'", id))
	}
	if configPath, ok := item["config"].(string); !ok || configPath == "" {
		errs = append(errs, fmt.Sprintf("dashboards.yaml: dashboard '%s' has an invalid or missing 'config'", id))
	}
	if v, ok := item["icon"]; ok && v != nil {
		if _, ok := v.(string); !ok {
			errs = append(errs, fmt.Sprintf("dashboards.yaml: dashboard '%s' icon must be a string", id))
		}
	}
	if v, ok := item["show_in_sidebar"]; ok {
		if _, ok := v.(bool); !ok {
			errs = append(errs, fmt.Sprintf("dashboards.yaml: dashboard '%s' show_in_sidebar must be a boolean", id))
		}
	}

	if len(errs) > 0 {
		return nil, errs
	}
	return item, nil
}

// loadContent reads and parses <workdir>/<configPath>, the dashboard's
// Lovelace view config, normalizing through a JSON round trip so a
// YAML-authored int compares equal to HA's float64 and key order is
// canonical - neither should ever register as drift.
//
// configPath is manifest-declared and not fully trusted: unguarded, a
// value like "../secrets.yaml" would read anything the add-on can reach
// and publish it as a dashboard's content, so containDashboardConfigPath
// applies the same realpath containment as gitsync and applier.
func loadContent(workdir, configPath string) (map[string]any, error) {
	fullPath, err := containDashboardConfigPath(workdir, configPath)
	if err != nil {
		return nil, fmt.Errorf("config file '%s': %w", configPath, err)
	}
	data, err := os.ReadFile(fullPath) // #nosec G304 -- fullPath is containDashboardConfigPath-confined (symlink-resolved) under workdir
	if err != nil {
		return nil, fmt.Errorf("could not read config file '%s': %w", configPath, err)
	}

	var parsed any
	if err := yaml.Unmarshal(data, &parsed); err != nil {
		return nil, fmt.Errorf("config file '%s': invalid YAML: %w", configPath, err)
	}
	if parsed == nil {
		parsed = map[string]any{}
	}

	normalized, err := normalizeViaJSON(parsed)
	if err != nil {
		return nil, fmt.Errorf("config file '%s': %w", configPath, err)
	}
	m, ok := normalized.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("config file '%s': top level must be a mapping", configPath)
	}
	return m, nil
}

// containDashboardConfigPath resolves configPath under workdir, rejecting
// it if absolute, or if it lands outside workdir after normalization or
// after following symlinks (fsx.Realpath). Kept local rather than shared
// with gitsync/applier: only the resolution they had in common moved to
// internal/fsx, and this applies none of their write-side checks.
func containDashboardConfigPath(workdir, configPath string) (string, error) {
	if filepath.IsAbs(configPath) {
		return "", fmt.Errorf("refusing to read absolute path: %s", configPath)
	}
	normalized := filepath.Clean(configPath)
	if normalized == ".." || strings.HasPrefix(normalized, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("refusing to read path outside the checkout: %s", configPath)
	}

	rootClean := filepath.Clean(workdir)
	full := filepath.Join(rootClean, normalized)
	if full != rootClean && !strings.HasPrefix(full, rootClean+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes the checkout: %s", configPath)
	}

	rootReal := fsx.Realpath(rootClean)
	destReal := fsx.Realpath(full)
	if destReal != rootReal && !strings.HasPrefix(destReal, rootReal+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes the checkout via symlink: %s", configPath)
	}
	return full, nil
}

func normalizeViaJSON(v any) (any, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var out any
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Plan computes the dashboard operations needed to reconcile live Lovelace
// dashboards toward desired, given the managed ownership mapping.
//
// liveDashboards is every dashboard's metadata as
// lovelace/dashboards/list returns it. liveContent maps a manifest id to
// its saved config; nil, whether as an absent key or a present nil value,
// means nothing has ever been saved at that url_path. managed is
// state.DashboardManaged: "dashboard:<id>" -> live id (the collection's
// own "id" field, NOT necessarily url_path verbatim).
//
// Ownership mirrors registries.Plan's rules with url_path standing in for
// "name"; HA enforces a unique url_path per dashboard, so unlike by-name
// adoption this match can never be ambiguous.
//
//  1. key managed and the live object exists -> update op only if
//     metadata or content differs.
//  2. key managed but the live object is gone -> create, as if unmanaged.
//  3. key unmanaged: a live dashboard has this url_path -> adopt it (an
//     update op is emitted even with no drift, so the applier has
//     something to execute that records the mapping); none -> create.
//  4. key managed but no longer declared -> delete the live dashboard, if
//     it still exists.
//
// A dashboard whose config file failed to load is a KindError op instead
// of any of the above, and never touches managed.
func Plan(desired Desired, liveDashboards []map[string]any, liveContent map[string]map[string]any, managed map[string]string) []registries.RegOp {
	if managed == nil {
		managed = map[string]string{}
	}

	liveByURLPath := map[string]map[string]any{}
	liveByLiveID := map[string]map[string]any{}
	for _, obj := range liveDashboards {
		if urlPath, ok := obj["url_path"].(string); ok && urlPath != "" {
			liveByURLPath[urlPath] = obj
		}
		if liveID, ok := obj["id"].(string); ok && liveID != "" {
			liveByLiveID[liveID] = obj
		}
	}

	declaredIDs := map[string]bool{}
	var ops []registries.RegOp

	for _, item := range desired.Dashboards {
		id, _ := item["id"].(string)
		declaredIDs[id] = true
		key := "dashboard:" + id

		content, hasContent := desired.Content[id]
		if !hasContent || content.Err != "" {
			msg := "dashboard config file could not be loaded"
			if hasContent {
				msg = content.Err
			}
			ops = append(ops, errorOp(id, msg))
			continue
		}

		if liveID, isManaged := managed[key]; isManaged {
			if liveObj, exists := liveByLiveID[liveID]; exists {
				if op := planUpdate(id, liveID, item, content.Data, liveObj, liveContent[id], false); op != nil {
					ops = append(ops, *op)
				}
			} else {
				ops = append(ops, planCreate(id, item, content.Data))
			}
			continue
		}

		if liveObj, found := liveByURLPath[id]; found {
			liveID, _ := liveObj["id"].(string)
			if liveID == "" {
				ops = append(ops, errorOp(id, fmt.Sprintf("live dashboard matched by url_path %s has no usable id field", difftext.PyRepr(id))))
				continue
			}
			if op := planUpdate(id, liveID, item, content.Data, liveObj, liveContent[id], true); op != nil {
				ops = append(ops, *op)
			}
			continue
		}

		ops = append(ops, planCreate(id, item, content.Data))
	}

	var deleteKeys []string
	for fullKey := range managed {
		if strings.HasPrefix(fullKey, "dashboard:") {
			deleteKeys = append(deleteKeys, fullKey)
		}
	}
	sort.Strings(deleteKeys)
	for _, fullKey := range deleteKeys {
		id := strings.TrimPrefix(fullKey, "dashboard:")
		if declaredIDs[id] {
			continue
		}
		liveID := managed[fullKey]
		liveObj, exists := liveByLiveID[liveID]
		if !exists {
			continue
		}
		ops = append(ops, registries.RegOp{
			Kind: KindDelete, RType: "dashboard", Key: id, Params: map[string]any{}, LiveID: liveID,
			DiffText: deleteDiffText(id, liveObj),
		})
	}

	return ops
}

// planCreate builds the KindCreate op for a dashboard with no live match:
// metadata and content are both sent, since neither can exist yet.
func planCreate(id string, item map[string]any, content map[string]any) registries.RegOp {
	metadata := desiredMetadata(item)
	params := map[string]any{"metadata": metadata, "content": content}
	return registries.RegOp{
		Kind: KindCreate, RType: "dashboard", Key: id, Params: params,
		DiffText: createDiffText(id, metadata, content),
	}
}

// planUpdate builds the KindUpdate op for a dashboard with a live match
// (managed, or adopted by url_path this cycle). Metadata and content are
// independent axes: params carries "metadata" when it differs or
// forceAdopt needs SOMETHING to execute so the applier records the
// mapping, and "content" when content differs or was never saved. Returns
// nil when neither axis needs anything and this is not a forced adopt.
func planUpdate(
	id, liveID string, item map[string]any, desiredContent map[string]any,
	liveObj map[string]any, liveContentForID map[string]any, forceAdopt bool,
) *registries.RegOp {
	desiredMd := desiredMetadata(item)
	metaDiff := metadataDiff(id, desiredMd, liveObj)

	params := map[string]any{}
	var diffParts []string

	if metaDiff != "" || forceAdopt {
		params["metadata"] = desiredMd
		if metaDiff != "" {
			diffParts = append(diffParts, metaDiff)
		}
	}

	if !contentEqual(desiredContent, liveContentForID) {
		params["content"] = desiredContent
		diffParts = append(diffParts, contentDiffText(id, desiredContent, liveContentForID))
	}

	if len(params) == 0 {
		return nil
	}

	diffText := strings.Join(diffParts, "\n")
	if diffText == "" {
		diffText = adoptedNoChangeText(id, liveID)
	}
	return &registries.RegOp{
		Kind: KindUpdate, RType: "dashboard", Key: id, Params: params, LiveID: liveID, DiffText: diffText,
	}
}

// desiredMetadata resolves item's metadata into the shape regapply sends
// to lovelace/dashboards/create|update: title and show_in_sidebar always
// present (defaulting to true, a manifest default rather than "leave
// untouched"), icon only if declared - possibly null, to clear it.
func desiredMetadata(item map[string]any) map[string]any {
	md := map[string]any{
		"title":           item["title"],
		"show_in_sidebar": showInSidebar(item),
	}
	if v, ok := item["icon"]; ok {
		md["icon"] = v
	}
	return md
}

func showInSidebar(item map[string]any) bool {
	if v, ok := item["show_in_sidebar"]; ok {
		b, _ := v.(bool)
		return b
	}
	return true
}

// contentEqual reports whether desired and live are the same normalized
// Lovelace config. Both sides are already JSON-shaped, so this wants
// difftext.DeepEqual's exact walk rather than the numeric/list-order
// normalization registries.ValuesEqual needs for hand-authored fields:
// content is compared whole-document, not field by field.
func contentEqual(desired, live map[string]any) bool {
	if desired == nil && live == nil {
		return true
	}
	if desired == nil || live == nil {
		return false
	}
	return difftext.DeepEqual(desired, live)
}

func errorOp(id, msg string) registries.RegOp {
	return registries.RegOp{Kind: KindError, RType: "dashboard", Key: id, Params: map[string]any{}, Error: msg}
}

func adoptedNoChangeText(id, liveID string) string {
	return fmt.Sprintf("adopted existing dashboard '%s' (live id %s); no changes needed", id, liveID)
}

// metadataDiff is a unified-diff-style comparison of desired's fields
// against the matching fields of liveObj. Returns "" if nothing differs.
func metadataDiff(id string, desired map[string]any, liveObj map[string]any) string {
	changed := false
	fields := difftext.SortedKeys(desired)
	before := make([]string, 0, len(fields))
	after := make([]string, 0, len(fields))
	for _, f := range fields {
		liveVal := liveObj[f]
		desiredVal := desired[f]
		if liveVal != desiredVal {
			changed = true
		}
		before = append(before, fmt.Sprintf("%s: %s\n", f, difftext.ReprValue(liveVal)))
		after = append(after, fmt.Sprintf("%s: %s\n", f, difftext.ReprValue(desiredVal)))
	}
	if !changed {
		return ""
	}
	return difftext.UnifiedDiff(before, after, "live/dashboard/"+id, "manifest/dashboard/"+id)
}

// createDiffText is metadataDiff's style for a fresh create: every
// metadata field, plus the whole content document as YAML, are "new".
func createDiffText(id string, metadata map[string]any, content map[string]any) string {
	fields := difftext.SortedKeys(metadata)
	lines := make([]string, 0, len(fields))
	for _, f := range fields {
		lines = append(lines, fmt.Sprintf("%s: %s\n", f, difftext.ReprValue(metadata[f])))
	}
	metaText := difftext.UnifiedDiff(nil, lines, "live/dashboard/"+id, "manifest/dashboard/"+id)
	return strings.Join(nonEmpty(metaText, contentDiffText(id, content, nil)), "\n")
}

// deleteDiffText is metadataDiff's style for a delete: every live
// metadata field goes away. Content is not shown - it dies with the
// dashboard, with no separate WS call, so there is nothing to diff.
func deleteDiffText(id string, liveObj map[string]any) string {
	fields := []string{"title", "icon", "show_in_sidebar"}
	lines := make([]string, 0, len(fields))
	for _, f := range fields {
		if v, ok := liveObj[f]; ok {
			lines = append(lines, fmt.Sprintf("%s: %s\n", f, difftext.ReprValue(v)))
		}
	}
	return difftext.UnifiedDiff(lines, nil, "live/dashboard/"+id, "manifest/dashboard/"+id)
}

// contentDiffText is a unified diff of live and desired content, each
// rendered back to YAML, capped the way internal/differ caps a file diff
// (maxContentDiffLines/maxContentDiffBytes) - a shared constraint, not
// shared code, since differ's cap helpers are unexported to it.
func contentDiffText(id string, desired, live map[string]any) string {
	beforeLines := difflib.SplitLines(renderYAML(live))
	afterLines := difflib.SplitLines(renderYAML(desired))
	text := difftext.UnifiedDiff(beforeLines, afterLines,
		"live/dashboard/"+id+"/config.yaml", "manifest/dashboard/"+id+"/config.yaml")
	if text == "" {
		// Identical content, and UnifiedDiff's failure path. Returning
		// early matters: SplitLines("") is []string{"\n"}, which would
		// survive nonEmpty and print a blank line under a create's diff.
		return ""
	}
	return truncateContentDiff(difflib.SplitLines(text))
}

// renderYAML renders v back to YAML text for a human-readable diff. A nil
// map renders as an empty document, matching "no content saved yet".
func renderYAML(v map[string]any) string {
	if v == nil {
		return ""
	}
	data, err := yaml.Marshal(v)
	if err != nil {
		return ""
	}
	return string(data)
}

// Truncation caps for a content diff, matching internal/differ's own
// maxDiffLines/maxDiffBytes/truncationMarker exactly.
const (
	maxContentDiffLines     = 400
	maxContentDiffBytes     = 40 * 1024
	contentTruncationMarker = "\n... diff truncated\n"
)

func truncateContentDiff(lines []string) string {
	truncated := false
	if len(lines) > maxContentDiffLines {
		lines = lines[:maxContentDiffLines]
		truncated = true
	}
	text := strings.Join(lines, "")
	if len(text) > maxContentDiffBytes {
		text = text[:maxContentDiffBytes]
		truncated = true
	}
	if truncated {
		text += contentTruncationMarker
	}
	return text
}

func nonEmpty(parts ...string) []string {
	var out []string
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
