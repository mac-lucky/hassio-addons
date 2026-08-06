// Package registries computes a three-way reconciliation plan for Home
// Assistant's floor, area and label registries plus helper entities,
// against gitops/registries.yaml and gitops/helpers.yaml. Pure logic:
// nothing here opens a socket or touches live state, and a missing
// manifest is not an error, only an inactive feature.
//
// Planning is manifest x live state x registry_managed (see Plan). An
// area's floor/labels reference other manifest items by id; when the
// referenced item is only being created in this same plan, Params carries
// map[string]any{"$ref": "<rtype>:<id>"} for the applier to resolve.
package registries

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/difftext"
	yaml "go.yaml.in/yaml/v3"
)

// idPattern is the manifest id syntax, shared by every item type.
var idPattern = regexp.MustCompile(`^[a-z0-9_]+$`)

// SupportedHelperDomains are the helper domains gitops/helpers.yaml may
// declare; anything else at its top level is a validation error.
var SupportedHelperDomains = []string{
	"input_boolean",
	"input_number",
	"input_select",
	"input_text",
	"input_datetime",
	"counter",
	"timer",
}

var supportedHelperDomainSet = func() map[string]bool {
	m := make(map[string]bool, len(SupportedHelperDomains))
	for _, d := range SupportedHelperDomains {
		m[d] = true
	}
	return m
}()

// RegistryRTypes are the rtypes with their own config/<rtype>_registry/*
// commands, whose responses key each item by "<rtype>_id". Helper domains
// are DictStorageCollections: same "<domain>_id" in request params, but
// the literal "id" in every response. ResponseIDField and LiveIDOf are the
// only places that asymmetry should be encoded.
var RegistryRTypes = []string{"floor", "area", "label"}

// IsRegistryRType reports whether rtype has its own registry command
// family rather than being a helper domain. Ranges over RegistryRTypes so
// an rtype added there is recognized here too.
func IsRegistryRType(rtype string) bool {
	return slices.Contains(RegistryRTypes, rtype)
}

// ResponseIDField is what a response payload calls rtype's live id:
// "<rtype>_id" for floor/area/label, "id" for a helper domain.
func ResponseIDField(rtype string) string {
	if IsRegistryRType(rtype) {
		return rtype + "_id"
	}
	return "id"
}

// RequestIDField is what an update/delete request's params call the live
// id: always "<rtype>_id", helper domains included.
func RequestIDField(rtype string) string {
	return rtype + "_id"
}

// LiveIDOf is obj's live id for rtype, or "" (never a panic, and never a
// legitimate id) when it carries no ResponseIDField.
func LiveIDOf(rtype string, obj map[string]any) string {
	v, _ := obj[ResponseIDField(rtype)].(string)
	return v
}

// ManifestError is returned when a gitops manifest fails to parse or
// validate. Error() joins every problem found, not just the first.
type ManifestError struct {
	Problems []string
}

func (e *ManifestError) Error() string {
	return strings.Join(e.Problems, "; ")
}

// Desired is the parsed, validated contents of the gitops/ manifests: one
// map per item in manifest order, unknown fields included, which Plan
// forwards untouched into RegOp.Params. Helpers is keyed by domain.
type Desired struct {
	Floors  []map[string]any
	Areas   []map[string]any
	Labels  []map[string]any
	Helpers map[string][]map[string]any
}

// RegOp kinds.
const (
	KindCreate = "create"
	KindUpdate = "update"
	KindDelete = "delete"
	KindError  = "error"
)

// RegOp is one planned registry operation. KindError is a per-item problem
// that skips the item, not the plan. Key is the manifest id, never a live
// one; Params are WS params with names already translated (an area's floor
// -> floor_id) and may hold $ref placeholders.
//
// Secrets is what this op's secret:// references resolved to, so the
// applier can scrub them from live API errors; Declared is that same data
// as the manifest wrote it, which is what gets persisted so no credential
// lands in state.json or a rollback stash. Neither is rendered anywhere.
type RegOp struct {
	Kind     string
	RType    string
	Key      string
	Params   map[string]any
	LiveID   string
	DiffText string
	Error    string
	Secrets  []string
	Declared map[string]any
}

// LoadManifests loads and validates <workdir>/gitops/registries.yaml and
// helpers.yaml. A missing file is an empty Desired, not an error; one that
// exists but fails to parse or validate is a *ManifestError listing every
// problem found.
func LoadManifests(workdir string) (Desired, error) {
	gitopsDir := filepath.Join(workdir, "gitops")
	info, statErr := os.Stat(gitopsDir)
	if statErr != nil || !info.IsDir() {
		return emptyDesired(), nil
	}

	var errs []string

	registriesRaw := loadYAMLFile(filepath.Join(gitopsDir, "registries.yaml"), "registries.yaml", &errs)
	helpersRaw := loadYAMLFile(filepath.Join(gitopsDir, "helpers.yaml"), "helpers.yaml", &errs)

	floors := []map[string]any{}
	areas := []map[string]any{}
	labels := []map[string]any{}
	if registriesRaw != nil {
		floors = validateItems(registriesRaw, "floors", "floor", "registries.yaml", &errs)
		areas = validateItems(registriesRaw, "areas", "area", "registries.yaml", &errs)
		labels = validateItems(registriesRaw, "labels", "label", "registries.yaml", &errs)
		validateAreaRefs(areas, floors, labels, &errs)
	}

	helpers := map[string][]map[string]any{}
	if helpersRaw != nil {
		helpers = validateHelpers(helpersRaw, &errs)
	}

	if len(errs) > 0 {
		return Desired{}, &ManifestError{Problems: errs}
	}

	return Desired{Floors: floors, Areas: areas, Labels: labels, Helpers: helpers}, nil
}

func emptyDesired() Desired {
	return Desired{
		Floors:  []map[string]any{},
		Areas:   []map[string]any{},
		Labels:  []map[string]any{},
		Helpers: map[string][]map[string]any{},
	}
}

// loadYAMLFile reads and parses one manifest file. A missing file (or a
// directory) is nil with no error appended - the feature-inactive case; an
// empty file is an empty non-nil map; anything unreadable or unparseable
// appends to errs and returns nil.
func loadYAMLFile(path, label string, errs *[]string) map[string]any {
	info, statErr := os.Stat(path)
	if statErr != nil || !info.Mode().IsRegular() {
		return nil
	}

	data, err := os.ReadFile(path) // #nosec G304 -- path is workdir-relative, constructed by this package only
	if err != nil {
		*errs = append(*errs, fmt.Sprintf("%s: could not read file: %v", label, err))
		return nil
	}

	var parsed any
	if err := yaml.Unmarshal(data, &parsed); err != nil {
		*errs = append(*errs, fmt.Sprintf("%s: invalid YAML: %v", label, err))
		return nil
	}

	if parsed == nil {
		return map[string]any{}
	}
	obj, ok := parsed.(map[string]any)
	if !ok {
		*errs = append(*errs, fmt.Sprintf("%s: top level must be a mapping", label))
		return nil
	}
	return obj
}

// reservedExtraFields are names a pass-through field may never use: they
// collide with the WS envelope, with the id keyword internal/regapply
// injects itself, or (created_at/modified_at) with server-generated values
// the registry schemas reject at apply time.
var reservedExtraFields = []string{"type", "id", "msg_type", "created_at", "modified_at"}

// validateItems checks raw[listKey] is a list of mappings, each with a
// unique well-formed id, a non-empty name and no reserved field. Invalid
// items are recorded in errs and dropped, so validation continues.
func validateItems(raw map[string]any, listKey, singular, label string, errs *[]string) []map[string]any {
	itemsRaw, present := raw[listKey]
	if !present || itemsRaw == nil {
		return []map[string]any{}
	}
	items, ok := itemsRaw.([]any)
	if !ok {
		*errs = append(*errs, fmt.Sprintf("%s: %s must be a list", label, listKey))
		return []map[string]any{}
	}

	reserved := make(map[string]bool, len(reservedExtraFields)+1)
	for _, f := range reservedExtraFields {
		reserved[f] = true
	}
	reserved[RequestIDField(singular)] = true

	seenIDs := map[string]bool{}
	result := []map[string]any{}

	for idx, rawItem := range items {
		itemMap, ok := rawItem.(map[string]any)
		if !ok {
			*errs = append(*errs, fmt.Sprintf("%s: %s[%d] is not a mapping", label, listKey, idx))
			continue
		}
		item := make(map[string]any, len(itemMap))
		for k, v := range itemMap {
			item[k] = v
		}

		itemID, idIsString := item["id"].(string)
		if !idIsString || itemID == "" || !idPattern.MatchString(itemID) {
			*errs = append(*errs, fmt.Sprintf("%s: %s[%d] has an invalid or missing 'id'", label, listKey, idx))
			continue
		}

		name, nameIsString := item["name"].(string)
		if !nameIsString || name == "" {
			*errs = append(*errs, fmt.Sprintf("%s: %s '%s' has an invalid or missing 'name'", label, singular, itemID))
		}

		// "id" itself is the manifest's own required key, already
		// consumed above - only a *second*, colliding field name is a
		// problem here.
		var collisions []string
		for k := range item {
			if k != "id" && reserved[k] {
				collisions = append(collisions, k)
			}
		}
		if len(collisions) > 0 {
			sort.Strings(collisions)
			*errs = append(*errs, fmt.Sprintf(
				"%s: %s '%s' uses reserved field name(s) %s", label, singular, itemID, strings.Join(collisions, ", ")))
			continue
		}

		if seenIDs[itemID] {
			*errs = append(*errs, fmt.Sprintf("%s: duplicate %s id '%s'", label, singular, itemID))
			continue
		}
		seenIDs[itemID] = true
		result = append(result, item)
	}

	return result
}

// validateAreaRefs checks every area's floor/labels reference points at an
// id declared and valid elsewhere in registries.yaml.
func validateAreaRefs(areas, floors, labels []map[string]any, errs *[]string) {
	floorIDs := map[string]bool{}
	for _, f := range floors {
		if id, ok := f["id"].(string); ok {
			floorIDs[id] = true
		}
	}
	labelIDs := map[string]bool{}
	for _, l := range labels {
		if id, ok := l["id"].(string); ok {
			labelIDs[id] = true
		}
	}

	for _, area := range areas {
		areaID, _ := area["id"].(string)

		if floorRefRaw, ok := area["floor"]; ok && floorRefRaw != nil {
			floorRefStr, _ := floorRefRaw.(string)
			if !floorIDs[floorRefStr] {
				*errs = append(*errs, fmt.Sprintf(
					"registries.yaml: area '%s' references unknown floor id '%v'", areaID, floorRefRaw))
			}
		}

		labelRefsRaw, hasLabels := area["labels"]
		if !hasLabels || labelRefsRaw == nil {
			continue
		}
		labelRefs, ok := labelRefsRaw.([]any)
		if !ok {
			*errs = append(*errs, fmt.Sprintf("registries.yaml: area '%s' labels must be a list", areaID))
			continue
		}
		for _, refRaw := range labelRefs {
			refStr, _ := refRaw.(string)
			if !labelIDs[refStr] {
				*errs = append(*errs, fmt.Sprintf(
					"registries.yaml: area '%s' references unknown label id '%v'", areaID, refRaw))
			}
		}
	}
}

// validateHelpers checks every top-level key of helpers.yaml is a
// supported domain and validates its items like registries.yaml's lists.
// Sorted order keeps ManifestError's message deterministic.
func validateHelpers(raw map[string]any, errs *[]string) map[string][]map[string]any {
	helpers := map[string][]map[string]any{}

	domains := make([]string, 0, len(raw))
	for k := range raw {
		domains = append(domains, k)
	}
	sort.Strings(domains)

	for _, domain := range domains {
		if !supportedHelperDomainSet[domain] {
			*errs = append(*errs, fmt.Sprintf("helpers.yaml: unknown helper domain '%s'", domain))
			continue
		}
		helpers[domain] = validateItems(raw, domain, domain, "helpers.yaml", errs)
	}
	return helpers
}

// Plan computes the ops that reconcile live state toward desired. live is
// keyed by rtype, holding plain maps exactly as HA's */list returns them;
// managed is state.json's "<rtype>:<manifest id>" -> live id mapping.
//
// Ownership rules per desired item:
//
//  1. managed and live -> update only if a declared field differs.
//  2. managed but gone -> create, as if never managed.
//  3. not managed -> adopt the one live object with the same name (always
//     an update, so the applier records the mapping), error on more than
//     one, create on none.
//  4. managed but no longer declared -> delete, if it still exists.
//     Objects never in managed are never touched.
//
// Creates and updates come back floors, labels, areas (which reference
// both), then helper domains alphabetically; deletes in reverse rtype
// order, so an area goes before the floor it referenced.
func Plan(desired Desired, live map[string][]map[string]any, managed map[string]string) []RegOp {
	if managed == nil {
		managed = map[string]string{}
	}
	var ops []RegOp
	resolved := map[string]string{}

	resolveRef := func(refType, key string) any {
		if liveID := resolved[refType+":"+key]; liveID != "" {
			return liveID
		}
		return map[string]any{"$ref": refType + ":" + key}
	}

	floorOps, floorResolved := planGroup("floor", desired.Floors, live["floor"], managed, nil, nil)
	ops = append(ops, floorOps...)
	mergeInto(resolved, floorResolved)

	labelOps, labelResolved := planGroup("label", desired.Labels, live["label"], managed, nil, nil)
	ops = append(ops, labelOps...)
	mergeInto(resolved, labelResolved)

	// A floor/label that came back as an error op can never resolve to a
	// live id this cycle, so an area referencing it is demoted to an error
	// below rather than carrying a $ref that blows up mid-apply.
	brokenRefs := map[string]string{}
	for _, op := range floorOps {
		if op.Kind == KindError {
			brokenRefs[op.RType+":"+op.Key] = op.Error
		}
	}
	for _, op := range labelOps {
		if op.Kind == KindError {
			brokenRefs[op.RType+":"+op.Key] = op.Error
		}
	}

	areaOps, areaResolved := planGroup("area", desired.Areas, live["area"], managed, resolveRef, brokenRefs)
	ops = append(ops, areaOps...)
	mergeInto(resolved, areaResolved)

	helperDomains := helperDomainsFor(desired, managed)
	for _, domain := range helperDomains {
		domainOps, domainResolved := planGroup(domain, desired.Helpers[domain], live[domain], managed, nil, nil)
		ops = append(ops, domainOps...)
		mergeInto(resolved, domainResolved)
	}

	var deleteOps []RegOp
	for _, domain := range helperDomains {
		deleteOps = append(deleteOps, planDeletes(domain, desired.Helpers[domain], live[domain], managed)...)
	}
	deleteOps = append(deleteOps, planDeletes("area", desired.Areas, live["area"], managed)...)
	deleteOps = append(deleteOps, planDeletes("label", desired.Labels, live["label"], managed)...)
	deleteOps = append(deleteOps, planDeletes("floor", desired.Floors, live["floor"], managed)...)
	ops = append(ops, deleteOps...)

	return ops
}

func mergeInto(dst, src map[string]string) {
	for k, v := range src {
		dst[k] = v
	}
}

// helperDomainsFor returns every helper domain needing planning: declared
// in the manifest, or still in managed so a dropped domain's stale entries
// are still deleted. Sorted for deterministic output.
func helperDomainsFor(desired Desired, managed map[string]string) []string {
	domains := map[string]bool{}
	for d := range desired.Helpers {
		domains[d] = true
	}
	for k := range managed {
		prefix := k
		if idx := strings.Index(k, ":"); idx >= 0 {
			prefix = k[:idx]
		}
		if !IsRegistryRType(prefix) {
			domains[prefix] = true
		}
	}
	result := make([]string, 0, len(domains))
	for d := range domains {
		result = append(result, d)
	}
	sort.Strings(result)
	return result
}

// planGroup plans create/update/error ops for one rtype's manifest items,
// plus a "<rtype>:<id>" -> live id map ("" when not yet resolvable) for a
// later rtype's cross-references. resolveRef and brokenRefs are non-nil
// only for areas; an item pointing at a brokenRefs key becomes a KindError
// naming it rather than a $ref that can never resolve.
func planGroup(
	rtype string,
	manifestItems []map[string]any,
	liveItems []map[string]any,
	managed map[string]string,
	resolveRef func(rtype, key string) any,
	brokenRefs map[string]string,
) ([]RegOp, map[string]string) {
	liveByID := map[string]map[string]any{}
	for _, obj := range liveItems {
		if id := LiveIDOf(rtype, obj); id != "" {
			liveByID[id] = obj
		}
	}
	prefix := rtype + ":"
	claimed := map[string]bool{}
	for k, v := range managed {
		if strings.HasPrefix(k, prefix) {
			claimed[v] = true
		}
	}

	var ops []RegOp
	resolved := map[string]string{}

	for _, item := range manifestItems {
		key, _ := item["id"].(string)
		fullKey := rtype + ":" + key

		if refProblem, has := brokenRefMessage(rtype, item, brokenRefs); has {
			ops = append(ops, RegOp{Kind: KindError, RType: rtype, Key: key, Params: map[string]any{}, Error: refProblem})
			resolved[fullKey] = ""
			continue
		}

		params := paramsForItem(rtype, item, resolveRef)

		if liveID, isManaged := managed[fullKey]; isManaged {
			liveObj, exists := liveByID[liveID]
			if exists {
				diffText := fieldDiff(rtype, key, params, liveObj)
				if diffText != "" {
					ops = append(ops, RegOp{
						Kind: KindUpdate, RType: rtype, Key: key, Params: params, LiveID: liveID, DiffText: diffText,
					})
				}
				resolved[fullKey] = liveID
			} else {
				// Rule 2: managed but the live object is gone - recreate.
				ops = append(ops, RegOp{
					Kind: KindCreate, RType: rtype, Key: key, Params: params, DiffText: createDiffText(rtype, key, params),
				})
				resolved[fullKey] = ""
			}
			continue
		}

		// Rule 3: not managed yet.
		name, _ := item["name"].(string)
		var matches []map[string]any
		for _, obj := range liveItems {
			objName, _ := obj["name"].(string)
			if objName == name && !claimed[LiveIDOf(rtype, obj)] {
				matches = append(matches, obj)
			}
		}

		switch {
		case len(matches) == 1:
			liveObj := matches[0]
			liveID := LiveIDOf(rtype, liveObj)
			if liveID == "" {
				// Matched by name but carrying no id key this rtype uses -
				// too malformed to adopt, so surface it instead.
				ops = append(ops, RegOp{
					Kind: KindError, RType: rtype, Key: key, Params: map[string]any{},
					Error: fmt.Sprintf("live %s object matched by name %s has no usable id field", rtype, difftext.PyRepr(name)),
				})
				resolved[fullKey] = ""
				continue
			}
			claimed[liveID] = true
			diffText := fieldDiff(rtype, key, params, liveObj)
			if diffText == "" {
				diffText = adoptedNoChangeText(rtype, key, liveID)
			}
			ops = append(ops, RegOp{Kind: KindUpdate, RType: rtype, Key: key, Params: params, LiveID: liveID, DiffText: diffText})
			resolved[fullKey] = liveID
		case len(matches) > 1:
			ops = append(ops, RegOp{
				Kind: KindError, RType: rtype, Key: key, Params: map[string]any{},
				Error: fmt.Sprintf("ambiguous adopt: %d live %s objects named %s", len(matches), rtype, difftext.PyRepr(name)),
			})
			resolved[fullKey] = ""
		default:
			ops = append(ops, RegOp{
				Kind: KindCreate, RType: rtype, Key: key, Params: params, DiffText: createDiffText(rtype, key, params),
			})
			resolved[fullKey] = ""
		}
	}

	return ops, resolved
}

// brokenRefMessage names every one of an area's floor/labels references
// that points at a key in brokenRefs, or has=false when all resolve.
func brokenRefMessage(rtype string, item map[string]any, brokenRefs map[string]string) (string, bool) {
	if rtype != "area" || len(brokenRefs) == 0 {
		return "", false
	}

	var problems []string
	if floorRefRaw, ok := item["floor"]; ok && floorRefRaw != nil {
		floorRef, _ := floorRefRaw.(string)
		if msg, bad := brokenRefs["floor:"+floorRef]; bad {
			problems = append(problems, fmt.Sprintf("floor '%s' (%s)", floorRef, msg))
		}
	}
	if labelsRaw, ok := item["labels"]; ok && labelsRaw != nil {
		if list, ok := labelsRaw.([]any); ok {
			for _, lr := range list {
				labelRef, _ := lr.(string)
				if msg, bad := brokenRefs["label:"+labelRef]; bad {
					problems = append(problems, fmt.Sprintf("label '%s' (%s)", labelRef, msg))
				}
			}
		}
	}

	if len(problems) == 0 {
		return "", false
	}
	return "references broken: " + strings.Join(problems, "; "), true
}

// planDeletes is rule 4: a managed entry no longer declared, whose live
// object still exists, becomes a delete. Sorted by manifest id.
func planDeletes(rtype string, manifestItems []map[string]any, liveItems []map[string]any, managed map[string]string) []RegOp {
	liveByID := map[string]map[string]any{}
	for _, obj := range liveItems {
		if id := LiveIDOf(rtype, obj); id != "" {
			liveByID[id] = obj
		}
	}
	manifestIDs := map[string]bool{}
	for _, item := range manifestItems {
		if id, ok := item["id"].(string); ok {
			manifestIDs[id] = true
		}
	}
	prefix := rtype + ":"

	keys := make([]string, 0, len(managed))
	for k := range managed {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var ops []RegOp
	for _, fullKey := range keys {
		if !strings.HasPrefix(fullKey, prefix) {
			continue
		}
		key := strings.TrimPrefix(fullKey, prefix)
		if manifestIDs[key] {
			continue
		}
		liveID := managed[fullKey]
		liveObj, exists := liveByID[liveID]
		if !exists {
			// Already gone; the applier drops the stale mapping next time
			// it writes state.
			continue
		}
		ops = append(ops, RegOp{
			Kind: KindDelete, RType: rtype, Key: key, Params: map[string]any{}, LiveID: liveID,
			DiffText: deleteDiffText(rtype, liveObj),
		})
	}
	return ops
}

// paramsForItem translates one manifest item into WS params: every field
// but id, untouched, except an area's floor/labels, which resolveRef
// resolves and which are renamed to what the WS API expects.
func paramsForItem(rtype string, item map[string]any, resolveRef func(rtype, key string) any) map[string]any {
	params := make(map[string]any, len(item))
	for k, v := range item {
		if k != "id" {
			params[k] = v
		}
	}
	if rtype != "area" {
		return params
	}

	if floorRaw, ok := params["floor"]; ok {
		delete(params, "floor")
		floorKey, _ := floorRaw.(string)
		params["floor_id"] = resolveRef("floor", floorKey)
	}
	if labelsRaw, ok := params["labels"]; ok {
		delete(params, "labels")
		var labelKeys []any
		if list, ok := labelsRaw.([]any); ok {
			labelKeys = list
		}
		resolvedLabels := make([]any, len(labelKeys))
		for i, lk := range labelKeys {
			lkStr, _ := lk.(string)
			resolvedLabels[i] = resolveRef("label", lkStr)
		}
		params["labels"] = resolvedLabels
	}
	return params
}

// ValuesEqual is field-value equality for drift detection: lists compare
// order-insensitively (HA need not echo manifest order) and numbers by
// value across Go types (YAML int vs JSON float64). Exported so
// internal/addonopts can compare add-on options the same way.
func ValuesEqual(before, after any) bool {
	beforeList, beforeIsList := before.([]any)
	afterList, afterIsList := after.([]any)
	if beforeIsList && afterIsList {
		if len(beforeList) != len(afterList) {
			return false
		}
		bSorted := append([]any(nil), beforeList...)
		aSorted := append([]any(nil), afterList...)
		sort.Slice(bSorted, func(i, j int) bool { return difftext.ReprValue(bSorted[i]) < difftext.ReprValue(bSorted[j]) })
		sort.Slice(aSorted, func(i, j int) bool { return difftext.ReprValue(aSorted[i]) < difftext.ReprValue(aSorted[j]) })
		for i := range bSorted {
			if !difftext.DeepEqualNumbersByValue(bSorted[i], aSorted[i]) {
				return false
			}
		}
		return true
	}
	return difftext.DeepEqualNumbersByValue(before, after)
}

// asFloat reports v's numeric value for the types a YAML or JSON decoder
// can produce, so timer.duration accepts a bare seconds count in any.
func asFloat(v any) (float64, bool) {
	switch vv := v.(type) {
	case int:
		return float64(vv), true
	case int64:
		return float64(vv), true
	case float64:
		return vv, true
	case float32:
		return float64(vv), true
	default:
		return 0, false
	}
}

// renderDiffValue renders a params value for diff_text, turning a $ref
// placeholder into a readable pending marker so no Go-internal shape
// reaches the web UI. Lists are rendered element-wise.
func renderDiffValue(value any) any {
	if m, ok := value.(map[string]any); ok {
		if ref, hasRef := m["$ref"]; hasRef && len(m) == 1 {
			refStr, _ := ref.(string)
			return fmt.Sprintf("<pending: %s>", refStr)
		}
		return value
	}
	if list, ok := value.([]any); ok {
		out := make([]any, len(list))
		for i, item := range list {
			out[i] = renderDiffValue(item)
		}
		return out
	}
	return value
}

// valuesEqualForField is ValuesEqual plus two field-scoped cases:
// timer.duration (timerDurationEqual) and input_select.options
// (inputSelectOptionsEqual). Not folded into ValuesEqual, which has no
// rtype or field name in scope; keep both guards this narrow.
func valuesEqualForField(rtype, fieldName string, before, after any) bool {
	if rtype == "timer" && fieldName == "duration" {
		if equal, ok := timerDurationEqual(before, after); ok {
			return equal
		}
	}
	if rtype == "input_select" && fieldName == "options" {
		if equal, ok := inputSelectOptionsEqual(before, after); ok {
			return equal
		}
	}
	return ValuesEqual(before, after)
}

// timerDurationSeconds reduces a timer.duration to total seconds, so two
// spellings of the same duration compare equal. Accepts what cv.time_period
// does: a bare number, a 2- or 3-part string (H:MM, never M:SS, with no
// range check - "1:70:00" is legal), or a cv.time_period_dict map. HA only
// ever echoes the H:MM:SS form back, which the 3-part branch parses.
func timerDurationSeconds(v any) (seconds float64, ok bool) {
	if f, isNum := asFloat(v); isNum {
		return f, true
	}
	if m, isMap := v.(map[string]any); isMap {
		return timerDurationDictSeconds(m)
	}
	s, isStr := v.(string)
	if !isStr {
		return 0, false
	}
	parts := strings.Split(s, ":")
	if len(parts) != 2 && len(parts) != 3 {
		return 0, false
	}
	hours, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil {
		return 0, false
	}
	minutes, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil {
		return 0, false
	}
	secs := 0.0
	if len(parts) == 3 {
		if secs, err = strconv.ParseFloat(strings.TrimSpace(parts[2]), 64); err != nil {
			return 0, false
		}
	}
	return float64(hours)*3600 + float64(minutes)*60 + secs, true
}

// timerDurationDictSeconds sums the five cv.time_period_dict keys into
// total seconds. An unrecognized key means the map is not a duration at
// all, so ok is false rather than a partial sum.
func timerDurationDictSeconds(m map[string]any) (seconds float64, ok bool) {
	if len(m) == 0 {
		return 0, false
	}
	factors := map[string]float64{
		"days":         86400,
		"hours":        3600,
		"minutes":      60,
		"seconds":      1,
		"milliseconds": 0.001,
	}
	total := 0.0
	for key, val := range m {
		factor, known := factors[key]
		if !known {
			return 0, false
		}
		f, isNum := asFloat(val)
		if !isNum {
			return 0, false
		}
		total += f * factor
	}
	return total, true
}

// timerDurationEqual compares two durations by value, since HA accepts
// several spellings and echoes only one back - comparing strings drifts
// forever. Both sides truncate toward zero, as _format_timedelta does;
// ok is false when either side is not a duration, so the caller falls back.
func timerDurationEqual(a, b any) (equal, ok bool) {
	as, aok := timerDurationSeconds(a)
	bs, bok := timerDurationSeconds(b)
	if !aok || !bok {
		return false, false
	}
	return math.Trunc(as) == math.Trunc(bs), true
}

// cvStringScalar coerces one input_select.options element the way HA's
// cv.string does - str(value), so `1` and `true` come back as "1" and
// "True" and drift forever without this. ok is false for every other type:
// cv.string rejects nil, lists and maps outright, and floats are excluded
// because JSON turns a whole-valued one back into an int (DOCS.md says to
// quote them).
func cvStringScalar(v any) (s string, ok bool) {
	switch vv := v.(type) {
	case string:
		return vv, true
	case bool:
		// Python's str(bool) is capitalized, unlike Go's. Getting it
		// backwards fails silently, as a different permanent drift loop.
		if vv {
			return "True", true
		}
		return "False", true
	case int:
		return strconv.Itoa(vv), true
	case int64:
		return strconv.FormatInt(vv, 10), true
	default:
		return "", false
	}
}

// inputSelectOptionsEqual covers two bugs at once. Order: HA preserves
// options in declared order (that is the dropdown the user sees), so
// ValuesEqual's order-insensitive compare would treat a reorder as no
// drift and never apply it. Spelling: every option goes through cv.string
// (see cvStringScalar), so a non-string scalar drifts forever uncoerced.
//
// ok is false when either side is not a list or holds an element
// cvStringScalar cannot coerce, so the caller falls back. Not closed:
// `options: [1, "1"]` passes HA's _unique before cv.string, then dedupes
// to one stored value that can never match the manifest's two.
func inputSelectOptionsEqual(a, b any) (equal, ok bool) {
	aList, aIsList := a.([]any)
	bList, bIsList := b.([]any)
	if !aIsList || !bIsList {
		return false, false
	}
	if len(aList) != len(bList) {
		return false, true
	}
	for i := range aList {
		as, aok := cvStringScalar(aList[i])
		bs, bok := cvStringScalar(bList[i])
		if !aok || !bok {
			return false, false
		}
		if as != bs {
			return false, true
		}
	}
	return true, true
}

// fieldDiff is a unified-diff-style comparison of the manifest's declared
// fields against liveObj's matching ones, or "" when nothing differs.
func fieldDiff(rtype, key string, params map[string]any, liveObj map[string]any) string {
	changed := false
	fieldNames := difftext.SortedKeys(params)
	beforeLines := make([]string, 0, len(fieldNames))
	afterLines := make([]string, 0, len(fieldNames))
	for _, fieldName := range fieldNames {
		afterVal := params[fieldName]
		beforeVal := liveObj[fieldName]
		beforeRepr := difftext.ReprValue(beforeVal)
		fieldChanged := !valuesEqualForField(rtype, fieldName, beforeVal, afterVal)
		if fieldChanged {
			changed = true
		}
		beforeLines = append(beforeLines, fmt.Sprintf("%s: %s\n", fieldName, beforeRepr))
		if fieldChanged {
			afterLines = append(afterLines, fmt.Sprintf("%s: %s\n", fieldName, difftext.ReprValue(renderDiffValue(afterVal))))
		} else {
			// Same value, different spelling: render it identically on
			// both sides so the diff does not manufacture a -/+ pair.
			afterLines = append(afterLines, fmt.Sprintf("%s: %s\n", fieldName, beforeRepr))
		}
	}

	if !changed {
		return ""
	}
	return difftext.UnifiedDiff(beforeLines, afterLines, fmt.Sprintf("live/%s/%s", rtype, key), fmt.Sprintf("manifest/%s/%s", rtype, key))
}

// createDiffText is the same unified-diff style as fieldDiff, but for a
// fresh create: every declared field is "new".
func createDiffText(rtype, key string, params map[string]any) string {
	fieldNames := difftext.SortedKeys(params)
	lines := make([]string, 0, len(fieldNames))
	for _, fieldName := range fieldNames {
		lines = append(lines, fmt.Sprintf("%s: %s\n", fieldName, difftext.ReprValue(renderDiffValue(params[fieldName]))))
	}
	return difftext.UnifiedDiff(nil, lines, fmt.Sprintf("live/%s/%s", rtype, key), fmt.Sprintf("manifest/%s/%s", rtype, key))
}

// deleteDiffText is the same unified-diff style as fieldDiff, but for a
// delete: every live field goes away.
func deleteDiffText(rtype string, liveObj map[string]any) string {
	idField := ResponseIDField(rtype)
	keys := make([]string, 0, len(liveObj))
	for k := range liveObj {
		if k != idField {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	lines := make([]string, 0, len(keys))
	for _, k := range keys {
		lines = append(lines, fmt.Sprintf("%s: %s\n", k, difftext.ReprValue(liveObj[k])))
	}
	return difftext.UnifiedDiff(lines, nil, fmt.Sprintf("live/%s", rtype), fmt.Sprintf("manifest/%s", rtype))
}

func adoptedNoChangeText(rtype, key, liveID string) string {
	return fmt.Sprintf("adopted existing %s '%s' (live id %s); no field changes needed", rtype, key, liveID)
}
