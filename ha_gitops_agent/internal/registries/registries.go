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
	"log/slog"
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
// declare; anything else at its top level is a validation error. zone and
// person are not helpers to Home Assistant, but they are the same kind of
// storage collection (<domain>/list|create|update|delete) and go through
// the same path, with two differences: their update merges into the stored
// item rather than replacing it (see validateZones), and person/list is not
// a list (see PersonYAMLBucket).
var SupportedHelperDomains = []string{
	"input_boolean",
	"input_number",
	"input_select",
	"input_text",
	"input_datetime",
	"counter",
	"timer",
	"zone",
	"person",
}

// PersonYAMLBucket is the key regapply.FetchLive stores the persons defined in
// configuration.yaml under, next to live["person"]'s storage ones. Not an
// rtype: nothing plans against it except the name check in
// refuseYAMLPersonCreates, since those persons cannot be changed over the
// WS API.
const PersonYAMLBucket = "person_yaml"

var supportedHelperDomainSet = func() map[string]bool {
	m := make(map[string]bool, len(SupportedHelperDomains))
	for _, d := range SupportedHelperDomains {
		m[d] = true
	}
	return m
}()

// helperBoolFields are the fields each helper domain's storage schema reads
// with cv.boolean. yaml.v3 keeps an unquoted YAML 1.1 on/yes/off/no as a
// string, which cv.boolean accepts and stores as a bool - so the manifest
// value never equals the live one and the item is re-applied every cycle.
// Rejected at load time instead (validateHelpers).
var helperBoolFields = map[string][]string{
	"input_boolean":  {"initial"},
	"input_datetime": {"has_date", "has_time"},
	"counter":        {"restore"},
	"timer":          {"restore"},
	"zone":           {"passive"},
}

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

// RegOp kinds. KindForget is bookkeeping only: it drops a managed mapping
// and sends nothing to Home Assistant (see planDeletes for when).
const (
	KindCreate = "create"
	KindUpdate = "update"
	KindDelete = "delete"
	KindForget = "forget"
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
		validateHelperBools(domain, helpers[domain], errs)
		validateHelperNulls(domain, helpers[domain], errs)
		switch domain {
		case "zone":
			validateZones(helpers[domain], errs)
		case "person":
			validatePersons(helpers[domain], errs)
		}
	}
	return helpers
}

// helperDefaultedFields are the fields each helper domain's storage schema
// fills with a default when they are absent. A declared null removes a
// field from the update (see regapply's helper baseline), so for these HA
// stores the default instead, the next plan compares nil with it, and the
// item is re-applied - backup and all - on every cycle. person's update
// schema defaults device_trackers to [] even though its update merges, so
// a null there would unlink every tracker as well.
var helperDefaultedFields = map[string][]string{
	"counter":        {"initial", "restore", "step"},
	"timer":          {"duration", "restore"},
	"input_datetime": {"has_date", "has_time"},
	"input_number":   {"step", "mode"},
	"input_text":     {"min", "max", "mode"},
	"zone":           {"radius", "passive"},
	"person":         {"device_trackers"},
}

// validateZones checks what the zone schema needs and what would otherwise
// never converge: a real number for latitude, longitude and radius (a
// quoted one is coerced to float by HA and then never equals the
// manifest's text), and no null anywhere.
//
// Null is refused on every field, not just helperDefaultedFields, because
// a zone update is merged into the stored zone ({**item, **update_data})
// instead of replacing it: the null is left out of the update, the stored
// value stays, and the zone is re-applied every cycle. name is checked by
// validateItems and radius/passive by validateHelperNulls, so they are
// skipped here to report each problem once.
//
// Only storage zones can be managed: zone.home and the zones in
// configuration.yaml are not in zone/list, so they are never adopted and a
// declared zone with the same name is created next to them.
func validateZones(items []map[string]any, errs *[]string) {
	coordinates := []struct {
		field string
		limit float64
	}{{"latitude", 90}, {"longitude", 180}}

	for _, item := range items {
		itemID, _ := item["id"].(string)

		for _, field := range difftext.SortedKeys(item) {
			if item[field] != nil || field == "name" || slices.Contains(helperDefaultedFields["zone"], field) {
				continue
			}
			*errs = append(*errs, fmt.Sprintf(
				"helpers.yaml: zone '%s' field '%s' cannot be null - a zone update is merged into the stored zone, "+
					"so a null could never clear it and the zone would be re-applied every cycle; "+
					"omit the field (a set value can only be cleared in the Home Assistant UI)", itemID, field))
		}

		for _, c := range coordinates {
			v, present := item[c.field]
			if !present {
				*errs = append(*errs, fmt.Sprintf("helpers.yaml: zone '%s' is missing required field '%s'", itemID, c.field))
				continue
			}
			if v == nil {
				continue
			}
			if f, isNum := asFloat(v); !isNum || math.IsNaN(f) || f < -c.limit || f > c.limit {
				*errs = append(*errs, fmt.Sprintf(
					"helpers.yaml: zone '%s' field '%s' must be a number between %g and %g (a quoted number reads as text)",
					itemID, c.field, -c.limit, c.limit))
			}
		}

		if v, present := item["radius"]; present && v != nil {
			if f, isNum := asFloat(v); !isNum || math.IsNaN(f) || math.IsInf(f, 0) || f <= 0 {
				*errs = append(*errs, fmt.Sprintf(
					"helpers.yaml: zone '%s' field 'radius' must be a positive number of meters (a quoted number reads as text)", itemID))
			}
		}
	}
}

// personFields are the only fields a person may declare besides id.
var personFields = map[string]bool{"name": true, "device_trackers": true}

// personUnportableFields are person fields that exist in HA but whose
// values only mean something on the install they came from. They are
// refused rather than passed through: set them in the UI, and the update's
// full baseline (see regapply's helper baseline) resends whatever is set
// there, so a managed person keeps it.
var personUnportableFields = map[string]string{
	"user_id": "a user id is a random id Home Assistant's auth system generates for each install; " +
		"link the user to the person in the Home Assistant UI",
	"picture": "a picture is the URL of an image uploaded to this install; set it in the Home Assistant UI",
}

// deviceTrackerIDPattern is a device_tracker entity id as cv.entity_id
// accepts it after lowercasing: a mixed-case one would be stored lowercased
// and never equal the manifest again.
var deviceTrackerIDPattern = regexp.MustCompile(`^device_tracker\.[a-z0-9_]+$`)

// validatePersons checks a person declares only name and device_trackers
// (see personFields and personUnportableFields), and that device_trackers
// is a list of device_tracker entity ids. A bare string is refused: HA's
// cv.ensure_list would store it as a one-element list that never equals
// the manifest's string. Nulls need no check here: name is validateItems',
// device_trackers validateHelperNulls', and anything else is refused by
// name already.
func validatePersons(items []map[string]any, errs *[]string) {
	for _, item := range items {
		itemID, _ := item["id"].(string)

		var unsupported []string
		for _, field := range difftext.SortedKeys(item) {
			if field == "id" || personFields[field] {
				continue
			}
			if why, unportable := personUnportableFields[field]; unportable {
				*errs = append(*errs, fmt.Sprintf(
					"helpers.yaml: person '%s' field '%s' cannot be managed here - %s; the agent keeps whatever is set there",
					itemID, field, why))
				continue
			}
			unsupported = append(unsupported, field)
		}
		if len(unsupported) > 0 {
			*errs = append(*errs, fmt.Sprintf(
				"helpers.yaml: person '%s' has unsupported field(s) %s (a person takes name and device_trackers)",
				itemID, strings.Join(unsupported, ", ")))
		}

		trackersRaw, present := item["device_trackers"]
		if !present || trackersRaw == nil {
			continue
		}
		trackers, isList := trackersRaw.([]any)
		if !isList {
			*errs = append(*errs, fmt.Sprintf(
				"helpers.yaml: person '%s' field 'device_trackers' must be a list of device_tracker entity ids", itemID))
			continue
		}
		for _, t := range trackers {
			if s, isString := t.(string); !isString || !deviceTrackerIDPattern.MatchString(s) {
				*errs = append(*errs, fmt.Sprintf(
					"helpers.yaml: person '%s' device_trackers entry %s is not a device_tracker entity id "+
						"(device_tracker.<object_id>, lowercase)", itemID, difftext.ReprValue(t)))
			}
		}
	}
}

// validateHelperNulls rejects null on a helperDefaultedFields field.
func validateHelperNulls(domain string, items []map[string]any, errs *[]string) {
	for _, item := range items {
		itemID, _ := item["id"].(string)
		for _, field := range helperDefaultedFields[domain] {
			if v, ok := item[field]; ok && v == nil {
				*errs = append(*errs, fmt.Sprintf(
					"helpers.yaml: %s '%s' field '%s' cannot be null - Home Assistant fills in a default for it; "+
						"omit the field to keep the live value, or give it one", domain, itemID, field))
			}
		}
	}
}

// validateHelperBools rejects a helperBoolFields value that is set but not
// a real boolean. null stays allowed: it means "leave unset".
func validateHelperBools(domain string, items []map[string]any, errs *[]string) {
	for _, item := range items {
		itemID, _ := item["id"].(string)
		for _, field := range helperBoolFields[domain] {
			v, ok := item[field]
			if !ok || v == nil {
				continue
			}
			if _, isBool := v.(bool); !isBool {
				*errs = append(*errs, fmt.Sprintf(
					"helpers.yaml: %s '%s' field '%s' must be true or false (unquoted on/yes read as text)", domain, itemID, field))
			}
		}
	}
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
//     one, create on none. Only keys the manifest still declares hold a
//     live object back from adoption, so a renamed key adopts the object
//     its old key managed.
//  4. managed but no longer declared -> delete, if it still exists. If it
//     is gone, or a declared key now holds it (the rename in rule 3), the
//     mapping is forgotten instead: nothing is sent to Home Assistant.
//     Objects never in managed are never touched.
//
// A person create where configuration.yaml already defines a person of
// that name is an error op instead (refuseYAMLPersonCreates).
//
// Creates and updates come back floors, labels, areas (which reference
// both), then helper domains alphabetically; deletes and forgets after
// them in reverse rtype order, so an area goes before the floor it
// referenced and a rename's adopt runs before its old key is forgotten.
func Plan(desired Desired, live map[string][]map[string]any, managed map[string]string) []RegOp {
	if managed == nil {
		managed = map[string]string{}
	}
	var ops []RegOp
	resolved := map[string]string{}
	// holds is, per rtype, what planDeletes must not delete: see planGroup.
	holds := map[string]liveHolds{}

	resolveRef := func(refType, key string) any {
		if liveID := resolved[refType+":"+key]; liveID != "" {
			return liveID
		}
		return map[string]any{"$ref": refType + ":" + key}
	}

	floorOps, floorResolved, floorHolds := planGroup("floor", desired.Floors, live["floor"], managed, nil, nil)
	ops = append(ops, floorOps...)
	mergeInto(resolved, floorResolved)
	holds["floor"] = floorHolds

	labelOps, labelResolved, labelHolds := planGroup("label", desired.Labels, live["label"], managed, nil, nil)
	ops = append(ops, labelOps...)
	mergeInto(resolved, labelResolved)
	holds["label"] = labelHolds

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

	areaOps, areaResolved, areaHolds := planGroup("area", desired.Areas, live["area"], managed, resolveRef, brokenRefs)
	ops = append(ops, areaOps...)
	mergeInto(resolved, areaResolved)
	holds["area"] = areaHolds

	helperDomains := helperDomainsFor(desired, managed)
	for _, domain := range helperDomains {
		domainOps, domainResolved, domainHolds := planGroup(domain, desired.Helpers[domain], live[domain], managed, nil, nil)
		if domain == "person" {
			domainOps = refuseYAMLPersonCreates(domainOps, live[PersonYAMLBucket])
		}
		ops = append(ops, domainOps...)
		mergeInto(resolved, domainResolved)
		holds[domain] = domainHolds
	}

	var deleteOps []RegOp
	for _, domain := range helperDomains {
		deleteOps = append(deleteOps, planDeletes(domain, desired.Helpers[domain], live[domain], managed, holds[domain])...)
	}
	deleteOps = append(deleteOps, planDeletes("area", desired.Areas, live["area"], managed, holds["area"])...)
	deleteOps = append(deleteOps, planDeletes("label", desired.Labels, live["label"], managed, holds["label"])...)
	deleteOps = append(deleteOps, planDeletes("floor", desired.Floors, live["floor"], managed, holds["floor"])...)
	ops = append(ops, deleteOps...)

	return ops
}

// liveHolds is what one rtype's declared keys hold on live objects after
// planGroup: owned maps a live id to the "<rtype>:<key>" that manages or is
// adopting it this plan, and contested marks every live id a declared key
// might have adopted but could not decide on (an ambiguous match, or an
// area with a broken reference). planDeletes deletes neither: an undeclared
// key on an owned id is a rename and is forgotten, and one on a contested
// id is left alone until the error clears - it may well be the object the
// rename was meant to keep.
type liveHolds struct {
	owned     map[string]string
	contested map[string]bool
}

func mergeInto(dst, src map[string]string) {
	for k, v := range src {
		dst[k] = v
	}
}

// HelperDomainsFor is the helper domains Plan looks at, for the caller that
// fetches live state for it (regapply.FetchLive): nothing else is listed.
func HelperDomainsFor(desired Desired, managed map[string]string) []string {
	return helperDomainsFor(desired, managed)
}

// IsSupportedHelperDomain reports whether domain is a helper domain this
// package manages.
func IsSupportedHelperDomain(domain string) bool {
	return supportedHelperDomainSet[domain]
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
		if IsRegistryRType(prefix) {
			continue
		}
		if !supportedHelperDomainSet[prefix] {
			// A migrated or hand-edited state.json: treating an unknown
			// prefix as a helper domain would plan "<prefix>/list" and
			// "<prefix>/delete" against commands Home Assistant may not
			// have - or, worse, against ones it does.
			slog.Warn("registries: ignoring registry_managed entry with unknown prefix", "key", k)
			continue
		}
		domains[prefix] = true
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
// later rtype's cross-references, and the liveHolds planDeletes needs.
// resolveRef and brokenRefs are non-nil only for areas; an item pointing
// at a brokenRefs key becomes a KindError naming it rather than a $ref
// that can never resolve.
func planGroup(
	rtype string,
	manifestItems []map[string]any,
	liveItems []map[string]any,
	managed map[string]string,
	resolveRef func(rtype, key string) any,
	brokenRefs map[string]string,
) ([]RegOp, map[string]string, liveHolds) {
	liveByID := map[string]map[string]any{}
	for _, obj := range liveItems {
		if id := LiveIDOf(rtype, obj); id != "" {
			liveByID[id] = obj
		}
	}
	prefix := rtype + ":"
	declared := map[string]bool{}
	for _, item := range manifestItems {
		if id, ok := item["id"].(string); ok {
			declared[id] = true
		}
	}
	// Only keys the manifest still declares claim their live object. A key
	// being dropped this same plan is releasing it, which is exactly what
	// renaming a manifest id looks like: claiming it for the old key hid it
	// from the new one, whose create then collided with it by name (and a
	// helper came back as <slug>_2) on every cycle.
	claimed := map[string]bool{}
	for k, v := range managed {
		if strings.HasPrefix(k, prefix) && declared[strings.TrimPrefix(k, prefix)] {
			claimed[v] = true
		}
	}

	var ops []RegOp
	resolved := map[string]string{}
	contested := map[string]bool{}
	candidates := func(name string) []map[string]any {
		var matches []map[string]any
		for _, obj := range liveItems {
			objName, _ := obj["name"].(string)
			if nameMatches(rtype, objName, name) && !claimed[LiveIDOf(rtype, obj)] {
				matches = append(matches, obj)
			}
		}
		return matches
	}
	contest := func(matches []map[string]any) {
		for _, obj := range matches {
			if id := LiveIDOf(rtype, obj); id != "" {
				contested[id] = true
			}
		}
	}

	for _, item := range manifestItems {
		key, _ := item["id"].(string)
		fullKey := rtype + ":" + key
		name, _ := item["name"].(string)

		if refProblem, has := brokenRefMessage(rtype, item, brokenRefs); has {
			ops = append(ops, RegOp{Kind: KindError, RType: rtype, Key: key, Params: map[string]any{}, Error: refProblem})
			resolved[fullKey] = ""
			// Never got as far as adopting, so whatever it would have
			// adopted is off limits to planDeletes this plan.
			contest(candidates(name))
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
				ops = append(ops, createOp(rtype, key, params))
				resolved[fullKey] = ""
			}
			continue
		}

		// Rule 3: not managed yet.
		matches := candidates(name)

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
			contest(matches)
		default:
			ops = append(ops, createOp(rtype, key, params))
			resolved[fullKey] = ""
		}
	}

	// Sorted so a (corrupt) state with two declared keys on one live id
	// names the same owner in every plan.
	owned := map[string]string{}
	for _, fullKey := range difftext.SortedKeys(resolved) {
		if liveID := resolved[fullKey]; liveID != "" {
			if _, taken := owned[liveID]; !taken {
				owned[liveID] = fullKey
			}
		}
	}
	return ops, resolved, liveHolds{owned: owned, contested: contested}
}

// createOp is the KindCreate op for params, minus any field declared null:
// on a create, null can only mean "leave unset", which omitting it already
// does, and the create schemas reject it for most fields (an area's
// floor_id is a plain str there, unlike on update).
func createOp(rtype, key string, params map[string]any) RegOp {
	createParams := make(map[string]any, len(params))
	for k, v := range params {
		if v != nil {
			createParams[k] = v
		}
	}
	return RegOp{
		Kind: KindCreate, RType: rtype, Key: key, Params: createParams, DiffText: createDiffText(rtype, key, createParams),
	}
}

// refuseYAMLPersonCreates turns every person create whose name matches a
// person defined in configuration.yaml (yamlPersons, live[PersonYAMLBucket])
// into a KindError. The two share one id space, so HA would not refuse the
// create: it would make a second person with the same name, and a YAML
// person can never be adopted instead since the WS API cannot change it.
// Covers the managed-but-gone recreate as well as a first create; adopting
// a storage person of that name is left alone.
func refuseYAMLPersonCreates(ops []RegOp, yamlPersons []map[string]any) []RegOp {
	if len(yamlPersons) == 0 {
		return ops
	}
	for i, op := range ops {
		if op.Kind != KindCreate {
			continue
		}
		name, _ := op.Params["name"].(string)
		for _, yamlPerson := range yamlPersons {
			yamlName, _ := yamlPerson["name"].(string)
			if !nameMatches("person", yamlName, name) {
				continue
			}
			yamlID, _ := yamlPerson["id"].(string)
			ops[i] = RegOp{
				Kind: KindError, RType: op.RType, Key: op.Key, Params: map[string]any{},
				Error: fmt.Sprintf(
					"a person named %s (id %s) is defined in configuration.yaml; creating this one would give Home Assistant "+
						"two persons with that name - remove it from configuration.yaml or from helpers.yaml",
					difftext.PyRepr(name), difftext.PyRepr(yamlID)),
			}
			break
		}
	}
	return ops
}

// nameMatches is the adopt-by-name comparison. Floors, areas and labels
// match the way Home Assistant enforces their uniqueness, normalize_name's
// casefold().replace(" ", ""), so a manifest "living room" adopts a live
// "Living Room" instead of colliding with it on create; fieldDiff then
// shows the spelling difference and the adopt writes the manifest's.
// strings.EqualFold is Unicode simple folding, not Python's full casefold:
// a name differing only where full folding expands a character (German
// sharp s against "ss") is not adopted, and its create is refused as a
// duplicate, exactly as before this matched case.
// Helpers have no unique-name rule, so they match exactly.
func nameMatches(rtype, live, declared string) bool {
	if !IsRegistryRType(rtype) {
		return live == declared
	}
	return strings.EqualFold(strings.ReplaceAll(live, " ", ""), strings.ReplaceAll(declared, " ", ""))
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

// planDeletes is rule 4: a managed entry no longer declared becomes a
// delete if its live object still exists, or a forget when the object is
// gone or holds.owned says a declared key has taken it over. An entry on a
// holds.contested id plans nothing. Sorted by manifest id.
func planDeletes(
	rtype string, manifestItems []map[string]any, liveItems []map[string]any, managed map[string]string, holds liveHolds,
) []RegOp {
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
		switch {
		case !exists:
			// Forgotten, not skipped: a floor/area/label/helper id is a slug
			// of its name, so a stale mapping would claim - and delete - the
			// next object anyone makes with that name.
			ops = append(ops, RegOp{
				Kind: KindForget, RType: rtype, Key: key, Params: map[string]any{}, LiveID: liveID,
				DiffText: fmt.Sprintf("stop tracking %s: live object %s is gone", fullKey, liveID),
			})
		case holds.owned[liveID] != "":
			ops = append(ops, RegOp{
				Kind: KindForget, RType: rtype, Key: key, Params: map[string]any{}, LiveID: liveID,
				DiffText: fmt.Sprintf("stop tracking %s: live object %s is now managed as %s", fullKey, liveID, holds.owned[liveID]),
			})
		case holds.contested[liveID]:
			continue
		default:
			ops = append(ops, RegOp{
				Kind: KindDelete, RType: rtype, Key: key, Params: map[string]any{}, LiveID: liveID,
				DiffText: deleteDiffText(rtype, liveObj),
			})
		}
	}
	return ops
}

// paramsForItem translates one manifest item into WS params: every field
// but id, untouched, except an area's floor/labels, which resolveRef
// resolves and which are renamed to what the WS API expects. A floor
// declared null stays null, which clears it on update.
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
		if floorRaw == nil {
			// Resolving it would make a {"$ref": "floor:"} no plan can
			// ever satisfy, failing the whole layer every cycle.
			params["floor_id"] = nil
		} else {
			floorKey, _ := floorRaw.(string)
			params["floor_id"] = resolveRef("floor", floorKey)
		}
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
