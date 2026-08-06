// Package entities plans reconciliation of Home Assistant's entity
// registry against gitops/entities.yaml.
//
// It is internal/registries' UPDATE-ONLY sibling, emitting the same
// registries.RegOp shape. The agent never creates or deletes an entity -
// only integrations do - so what it manages is six customization fields on
// entities that already exist: name, icon, area, labels, disabled, hidden.
//
// # Manifest format
//
// Items are keyed by entity_id, which already IS the live id:
//
//	entities:
//	  - entity_id: light.living_room_ceiling
//	    name: Ceiling Light
//	    icon: mdi:ceiling-light
//	    area: living_room   # a registries.yaml manifest area id, or a
//	                        # live area_id directly
//	    labels: [managed_by_gitops]  # same resolution rule as area
//	    disabled: false     # false -> disabled_by null; true -> "user"
//	    hidden: false       # same mapping for hidden_by
//
// Unlike registries.yaml's pass-through model this is an ALLOWLIST: only
// those six fields are accepted (aliases, categories, device_class and
// options are out of scope), and new_entity_id is rejected with its own
// message - renames are not supported in this phase.
//
// # Ownership
//
// The "managed" side is state["entity_originals"]: a per-field snapshot of
// what the entity looked like before the agent touched it.
//
//  1. A declared entity_id with no live entry -> KindError op ("entity not
//     found"). This layer NEVER creates an entity.
//  2. A live disabled_by that is neither null nor "user" means something
//     else owns disabling it -> KindError op, every cycle it stays so.
//  3. Otherwise only the manifest's OWN declared fields are compared or
//     sent. A KindUpdate op is emitted when a declared field differs from
//     live, or when it has no entry in entity_originals yet - so the
//     applier has something to execute that records the original.
//  4. Dropping an entity_id from the manifest, or declaring it with zero
//     fields, plans a KindRestore op putting every recorded original back.
//     This layer's "delete" restores; it never removes the entity.
//
// area/labels resolve first against registries.yaml's declared ids
// (through registry_managed to the live id), otherwise as a live
// area_id/label_id. One created in this SAME cycle does not resolve - the
// planner runs against pre-cycle registry state - see RefResolver.
package entities

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/difftext"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/registries"
	yaml "go.yaml.in/yaml/v3"
)

// entityIDPattern validates "domain.object_id". Simpler than HA's own
// cv.entity_id (which also forbids leading/trailing/doubled underscores),
// but enough for the mistakes a manifest author actually makes.
var entityIDPattern = regexp.MustCompile(`^[a-z0-9_]+\.[a-z0-9_]+$`)

// allowedFields are the only per-item fields besides entity_id; anything
// else is a validation error.
var allowedFields = map[string]bool{
	"name": true, "icon": true, "area": true, "labels": true, "disabled": true, "hidden": true,
}

// Kinds entities.Plan's ops carry. KindRestore is new to this layer, for
// an un-manage that restores prior values rather than deleting; the web UI
// derives its badge from RegOp.Kind, so it needs no change.
const (
	KindUpdate  = registries.KindUpdate
	KindError   = registries.KindError
	KindRestore = "restore"
)

// ManifestError is returned when gitops/entities.yaml fails to parse or
// validate. Error() aggregates every problem, not just the first.
type ManifestError struct {
	Problems []string
}

func (e *ManifestError) Error() string {
	return strings.Join(e.Problems, "; ")
}

// Desired is the parsed, validated gitops/entities.yaml: one map per item
// in manifest order, keyed by entity_id plus zero or more allowedFields.
type Desired struct {
	Entities []map[string]any
}

func emptyDesired() Desired { return Desired{Entities: []map[string]any{}} }

// LoadManifest loads and validates <workdir>/gitops/entities.yaml. A
// missing file returns an empty Desired, not an error: the layer is idle.
func LoadManifest(workdir string) (Desired, error) {
	path := filepath.Join(workdir, "gitops", "entities.yaml")
	info, statErr := os.Stat(path)
	if statErr != nil || !info.Mode().IsRegular() {
		return emptyDesired(), nil
	}

	data, err := os.ReadFile(path) // #nosec G304 -- path is workdir-relative, constructed by this package only
	if err != nil {
		return Desired{}, &ManifestError{Problems: []string{fmt.Sprintf("entities.yaml: could not read file: %v", err)}}
	}

	var parsed any
	if err := yaml.Unmarshal(data, &parsed); err != nil {
		return Desired{}, &ManifestError{Problems: []string{fmt.Sprintf("entities.yaml: invalid YAML: %v", err)}}
	}
	if parsed == nil {
		return emptyDesired(), nil
	}
	obj, ok := parsed.(map[string]any)
	if !ok {
		return Desired{}, &ManifestError{Problems: []string{"entities.yaml: top level must be a mapping"}}
	}

	itemsRaw, present := obj["entities"]
	if !present || itemsRaw == nil {
		return emptyDesired(), nil
	}
	items, ok := itemsRaw.([]any)
	if !ok {
		return Desired{}, &ManifestError{Problems: []string{"entities.yaml: entities must be a list"}}
	}

	var errs []string
	seen := map[string]bool{}
	result := []map[string]any{}

	for idx, rawItem := range items {
		itemMap, ok := rawItem.(map[string]any)
		if !ok {
			errs = append(errs, fmt.Sprintf("entities.yaml: entities[%d] is not a mapping", idx))
			continue
		}

		entityID, idIsString := itemMap["entity_id"].(string)
		if !idIsString || entityID == "" || !entityIDPattern.MatchString(entityID) {
			errs = append(errs, fmt.Sprintf("entities.yaml: entities[%d] has an invalid or missing 'entity_id'", idx))
			continue
		}
		if seen[entityID] {
			errs = append(errs, fmt.Sprintf("entities.yaml: duplicate entity_id '%s'", entityID))
			continue
		}

		item, itemErrs := validateItemFields(entityID, itemMap)
		if len(itemErrs) > 0 {
			errs = append(errs, itemErrs...)
			continue
		}

		seen[entityID] = true
		result = append(result, item)
	}

	if len(errs) > 0 {
		return Desired{}, &ManifestError{Problems: errs}
	}
	return Desired{Entities: result}, nil
}

// validateItemFields validates one manifest item apart from entity_id (the
// caller did that): new_entity_id gets its own rejection message, anything
// outside allowedFields is unsupported, and name/icon/labels/disabled/
// hidden are type-checked when present.
//
// Those type checks are also what keeps rendered fields to strings, bools,
// nulls and string lists - the whole value domain fieldDiff hands to
// difftext.ReprValue. Widening this (and allowedFields) changes plan text,
// so add a diff-text assertion, not just a validation one.
func validateItemFields(entityID string, itemMap map[string]any) (map[string]any, []string) {
	item := map[string]any{"entity_id": entityID}
	var errs []string
	var unknown []string

	for k, v := range itemMap {
		switch {
		case k == "entity_id":
			continue
		case k == "new_entity_id":
			errs = append(errs, fmt.Sprintf(
				"entities.yaml: entity '%s' declares 'new_entity_id'; entity renames are not supported in this phase", entityID))
		case !allowedFields[k]:
			unknown = append(unknown, k)
		default:
			item[k] = v
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		errs = append(errs, fmt.Sprintf(
			"entities.yaml: entity '%s' has unsupported field(s) %s", entityID, strings.Join(unknown, ", ")))
	}
	if len(errs) > 0 {
		return nil, errs
	}

	if v, ok := item["name"]; ok && v != nil {
		if s, ok := v.(string); !ok || s == "" {
			return nil, []string{fmt.Sprintf("entities.yaml: entity '%s' name must be a non-empty string", entityID)}
		}
	}
	if v, ok := item["icon"]; ok && v != nil {
		if _, ok := v.(string); !ok {
			return nil, []string{fmt.Sprintf("entities.yaml: entity '%s' icon must be a string", entityID)}
		}
	}
	if v, ok := item["labels"]; ok && v != nil {
		if _, ok := v.([]any); !ok {
			return nil, []string{fmt.Sprintf("entities.yaml: entity '%s' labels must be a list", entityID)}
		}
	}
	if v, ok := item["disabled"]; ok && v != nil {
		if _, ok := v.(bool); !ok {
			return nil, []string{fmt.Sprintf("entities.yaml: entity '%s' disabled must be a boolean", entityID)}
		}
	}
	if v, ok := item["hidden"]; ok && v != nil {
		if _, ok := v.(bool); !ok {
			return nil, []string{fmt.Sprintf("entities.yaml: entity '%s' hidden must be a boolean", entityID)}
		}
	}
	return item, nil
}

// RefResolver resolves a manifest area/labels value: a registries.yaml
// manifest id first (through registry_managed), a live id otherwise.
type RefResolver struct {
	manifestIDs map[string]map[string]bool
	managed     map[string]string
	liveIDs     map[string]map[string]bool
}

// NewRefResolver builds a RefResolver from what a cycle already has: the
// registries.yaml manifest, registry_managed, and the live area/label
// lists FetchLive retrieved this cycle.
func NewRefResolver(registriesDesired registries.Desired, managed map[string]string, liveAreas, liveLabels []map[string]any) RefResolver {
	areaIDs := map[string]bool{}
	for _, a := range registriesDesired.Areas {
		if id, ok := a["id"].(string); ok {
			areaIDs[id] = true
		}
	}
	labelIDs := map[string]bool{}
	for _, l := range registriesDesired.Labels {
		if id, ok := l["id"].(string); ok {
			labelIDs[id] = true
		}
	}

	liveAreaIDs := map[string]bool{}
	for _, a := range liveAreas {
		if id, ok := a["area_id"].(string); ok {
			liveAreaIDs[id] = true
		}
	}
	liveLabelIDs := map[string]bool{}
	for _, l := range liveLabels {
		if id, ok := l["label_id"].(string); ok {
			liveLabelIDs[id] = true
		}
	}

	return RefResolver{
		manifestIDs: map[string]map[string]bool{"area": areaIDs, "label": labelIDs},
		managed:     managed,
		liveIDs:     map[string]map[string]bool{"area": liveAreaIDs, "label": liveLabelIDs},
	}
}

// Resolve returns the live id ref should translate to for rtype ("area"
// or "label"), or an error naming why it could not.
func (r RefResolver) Resolve(rtype, ref string) (string, error) {
	if ref == "" {
		return "", fmt.Errorf("%s reference is empty", rtype)
	}
	if r.manifestIDs[rtype][ref] {
		if liveID, ok := r.managed[rtype+":"+ref]; ok && liveID != "" {
			return liveID, nil
		}
		return "", fmt.Errorf(
			"%s '%s' is declared in registries.yaml but has no live id yet (not created or adopted) - "+
				"if it is also being created this same reconcile, this resolves automatically on the next cycle, once it exists live; "+
				"not a permanent failure", rtype, ref)
	}
	if r.liveIDs[rtype][ref] {
		return ref, nil
	}
	return "", fmt.Errorf("%s '%s' not found in registries.yaml or live %ss", rtype, ref, rtype)
}

// Plan computes the entity registry ops reconciling live entities toward
// desired, given state.EntityOriginals and the ownership rules in the
// package doc comment.
func Plan(desired Desired, liveEntities []map[string]any, originals map[string]map[string]any, refs RefResolver) []registries.RegOp {
	if originals == nil {
		originals = map[string]map[string]any{}
	}
	liveByID := map[string]map[string]any{}
	for _, obj := range liveEntities {
		if id, ok := obj["entity_id"].(string); ok && id != "" {
			liveByID[id] = obj
		}
	}

	// declaredIDs: every entity_id in the manifest in any form, errors
	// included - never restored. emptyDeclared: declared with entity_id
	// only, which is restore-eligible like "not declared" at all.
	declaredIDs := map[string]bool{}
	emptyDeclared := map[string]bool{}
	var ops []registries.RegOp

	for _, item := range desired.Entities {
		entityID, _ := item["entity_id"].(string)
		declaredIDs[entityID] = true
		key := "entity:" + entityID

		liveObj, exists := liveByID[entityID]
		if !exists {
			ops = append(ops, errorOp(entityID, "entity not found: "+entityID))
			continue
		}
		if msg := byFieldGuard(liveObj); msg != "" {
			ops = append(ops, errorOp(entityID, "cannot manage "+entityID+": "+msg))
			continue
		}

		params, err := buildParams(item, refs)
		if err != nil {
			ops = append(ops, errorOp(entityID, err.Error()))
			continue
		}
		if len(params) == 0 {
			emptyDeclared[entityID] = true
			continue
		}

		existingOriginals, hasOriginals := originals[key]
		firstRecording := !hasOriginals || hasNewField(params, existingOriginals)
		diffText := fieldDiff(entityID, params, liveObj)
		if diffText == "" && !firstRecording {
			continue
		}
		if diffText == "" {
			diffText = adoptedNoChangeText(entityID)
		}
		ops = append(ops, registries.RegOp{
			Kind: KindUpdate, RType: "entity", Key: entityID, Params: params, LiveID: entityID, DiffText: diffText,
		})
	}

	var restoreIDs []string
	for key := range originals {
		entityID := strings.TrimPrefix(key, "entity:")
		if declaredIDs[entityID] && !emptyDeclared[entityID] {
			continue
		}
		restoreIDs = append(restoreIDs, entityID)
	}
	sort.Strings(restoreIDs)

	for _, entityID := range restoreIDs {
		key := "entity:" + entityID
		liveObj, exists := liveByID[entityID]
		if !exists {
			ops = append(ops, errorOp(entityID,
				fmt.Sprintf("entity not found: %s (was managed; cannot restore its original values)", entityID)))
			continue
		}
		if msg := byFieldGuard(liveObj); msg != "" {
			ops = append(ops, errorOp(entityID, "cannot restore "+entityID+": "+msg))
			continue
		}

		restoreParams := sanitizeRestoreParams(entityID, originals[key])
		diffText := fieldDiff(entityID, restoreParams, liveObj)
		if diffText == "" {
			diffText = restoreNoChangeText(entityID)
		}
		ops = append(ops, registries.RegOp{
			Kind: KindRestore, RType: "entity", Key: entityID, Params: restoreParams, LiveID: entityID, DiffText: diffText,
		})
	}

	return ops
}

// hasNewField reports whether params declares a field missing from
// existingOriginals - rule 3's trigger for an update op with no drift, so
// the applier records that field's original value.
func hasNewField(params, existingOriginals map[string]any) bool {
	for field := range params {
		if _, already := existingOriginals[field]; !already {
			return true
		}
	}
	return false
}

// disabledByGuard returns "" if disabled_by is unset or "user" (safe to
// touch), otherwise a message naming the value refusing it - rule 2.
func disabledByGuard(liveObj map[string]any) string {
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

// hiddenByGuard mirrors disabledByGuard for hidden_by: HA's update schema
// accepts the same two values (null/"user"), and anything else means an
// integration owns hiding this entity.
func hiddenByGuard(liveObj map[string]any) string {
	v, ok := liveObj["hidden_by"]
	if !ok || v == nil {
		return ""
	}
	s, _ := v.(string)
	if s == "" || s == "user" {
		return ""
	}
	return fmt.Sprintf("hidden by %q, not by a user; refusing to touch it", s)
}

// byFieldGuard runs both guards, joining messages if both fire, and
// returns "" only if both pass. Used by Plan's manage and restore loops.
func byFieldGuard(liveObj map[string]any) string {
	var msgs []string
	if msg := disabledByGuard(liveObj); msg != "" {
		msgs = append(msgs, msg)
	}
	if msg := hiddenByGuard(liveObj); msg != "" {
		msgs = append(msgs, msg)
	}
	return strings.Join(msgs, "; ")
}

// restoreByFields are the two fields whose only valid outgoing values are
// null/"user". regapply's executeEntityOp clamps what it records for them,
// but a state.json written before that clamp may hold something else, and
// sending it back would fail HA's update schema and jam that restore every
// cycle. sanitizeRestoreParams drops the field instead of the whole op;
// the restore deletes the entity's entity_originals entry either way.
var restoreByFields = map[string]bool{"disabled_by": true, "hidden_by": true}

func sanitizeRestoreParams(entityID string, params map[string]any) map[string]any {
	out := make(map[string]any, len(params))
	for f, v := range params {
		if restoreByFields[f] {
			if s, ok := v.(string); ok && s != "" && s != "user" {
				slog.Warn(
					"entities: restore: recorded original for a *_by field is neither null nor \"user\"; skipping this field rather than sending a value Home Assistant would reject",
					"entity_id", entityID, "field", f, "value", s)
				continue
			}
		}
		out[f] = v
	}
	return out
}

// buildParams translates item's declared fields into
// config/entity_registry/update params: name/icon pass through, area/
// labels resolve through refs into area_id/labels, disabled/hidden become
// disabled_by/hidden_by ("user"/null).
func buildParams(item map[string]any, refs RefResolver) (map[string]any, error) {
	params := map[string]any{}

	if v, ok := item["name"]; ok {
		params["name"] = v
	}
	if v, ok := item["icon"]; ok {
		params["icon"] = v
	}
	if v, ok := item["area"]; ok {
		if v == nil {
			params["area_id"] = nil
		} else {
			ref, _ := v.(string)
			liveID, err := refs.Resolve("area", ref)
			if err != nil {
				return nil, err
			}
			params["area_id"] = liveID
		}
	}
	if v, ok := item["labels"]; ok {
		list, _ := v.([]any)
		resolved := make([]any, len(list))
		for i, lv := range list {
			ref, _ := lv.(string)
			liveID, err := refs.Resolve("label", ref)
			if err != nil {
				return nil, err
			}
			resolved[i] = liveID
		}
		params["labels"] = resolved
	}
	if v, ok := item["disabled"]; ok {
		b, _ := v.(bool)
		if b {
			params["disabled_by"] = "user"
		} else {
			params["disabled_by"] = nil
		}
	}
	if v, ok := item["hidden"]; ok {
		b, _ := v.(bool)
		if b {
			params["hidden_by"] = "user"
		} else {
			params["hidden_by"] = nil
		}
	}
	return params, nil
}

func errorOp(entityID, msg string) registries.RegOp {
	return registries.RegOp{Kind: registries.KindError, RType: "entity", Key: entityID, Params: map[string]any{}, Error: msg}
}

func adoptedNoChangeText(entityID string) string {
	return fmt.Sprintf("now managing %s; no field changes needed", entityID)
}

func restoreNoChangeText(entityID string) string {
	return fmt.Sprintf("restoring original values for %s; live values already match", entityID)
}

// fieldsEqual is drift-detection equality: a []any (only ever "labels")
// compares order-insensitively, since HA need not echo labels back in
// manifest order; everything else compares as a plain scalar.
func fieldsEqual(a, b any) bool {
	aList, aIsList := a.([]any)
	bList, bIsList := b.([]any)
	if aIsList || bIsList {
		if len(aList) != len(bList) {
			return false
		}
		as, bs := stringsOf(aList), stringsOf(bList)
		sort.Strings(as)
		sort.Strings(bs)
		for i := range as {
			if as[i] != bs[i] {
				return false
			}
		}
		return true
	}
	return a == b
}

func stringsOf(list []any) []string {
	out := make([]string, len(list))
	for i, v := range list {
		s, _ := v.(string)
		out[i] = s
	}
	return out
}

// fieldDiff renders params (declared fields, or a restore's recorded
// originals) against liveObj's matching fields as a unified diff, or ""
// when nothing differs.
func fieldDiff(entityID string, params, liveObj map[string]any) string {
	changed := false
	fields := difftext.SortedKeys(params)
	before := make([]string, 0, len(fields))
	after := make([]string, 0, len(fields))
	for _, f := range fields {
		beforeVal := liveObj[f]
		afterVal := params[f]
		if !fieldsEqual(beforeVal, afterVal) {
			changed = true
		}
		before = append(before, fmt.Sprintf("%s: %s\n", f, difftext.ReprValue(beforeVal)))
		after = append(after, fmt.Sprintf("%s: %s\n", f, difftext.ReprValue(afterVal)))
	}
	if !changed {
		return ""
	}
	return difftext.UnifiedDiff(before, after, "live/entity/"+entityID, "manifest/entity/"+entityID)
}
