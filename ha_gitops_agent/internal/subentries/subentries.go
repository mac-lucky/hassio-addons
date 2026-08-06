// Package subentries computes a reconciliation plan for Home Assistant
// config-entry SUBENTRIES - the child configurations a modern integration
// hangs off one parent config entry (a Google Calendar per calendar, a
// generic-thermostat per room) - against gitops/subentries.yaml, in the
// same registries.RegOp shape every other layer emits. It only ever plans;
// internal/regapply drives the flows.
//
// Hash-based like internal/flows, because the live data is unreadable: the
// config-entry WS API lists a subentry as "subentry_id"/"subentry_type"/
// "title"/"unique_id", never the "data" its flow submitted. So this layer
// stores a sha256 of the data it last applied (state.SubentryHashes) and
// treats a change to THAT as the signal to converge. Unlike flows, a
// subentry supports a reconfigure flow, which makes this a create/adopt/
// UPDATE layer rather than create/adopt/delete.
//
// Manage only what the manifest declares: create when no live subentry
// matches, reconfigure on adopt (so an adopted subentry converges rather
// than being trusted unseen) and on every later hash change, and never
// delete - removing an item is a bookkeeping-only UNMANAGE. There is no
// rollback either, since the prior data cannot be read back.
//
// The hash model's blind spot, shared with flows: a UI edit to a declared
// field is invisible, because the stored hash still matches the unchanged
// manifest. The declared value only reasserts itself the next time the
// manifest changes.
//
//	subentries:
//	  - id: calendar_family        # manifest key; required, [a-z0-9_]+, unique
//	    domain: google             # parent entry's integration domain; required
//	    entry_title: Home          # optional; disambiguates when the domain
//	                               # has more than one config entry
//	    subentry_type: calendar    # the flow to drive; required
//	    match:                     # how to recognize an existing subentry;
//	      unique_id: fam@group.calendar.google.com   # at least one of
//	      title: Family                              # unique_id / title
//	    data:                      # optional; flow input per step id
//	      user: {calendar_id: fam@group.calendar.google.com}
//
// match must carry at least one non-empty unique_id / title; unique_id is
// tried first (title is user-editable), and matching is always scoped to
// the resolved parent entry AND the declared subentry_type, so two
// subentries of different types sharing a title never collide. data, when
// present, maps each flow step id to that step's field mapping.
//
// Home Assistant names the first form of a CREATE flow "user" and of a
// RECONFIGURE flow "reconfigure" though both ask the same fields, so the
// driver aliases the pair: data declared under either name answers
// whichever the live flow presents. Fields the manifest does not declare
// keep the reconfigure form's pre-filled current values, so a partial data
// block edits only what it names.
//
// A declared value written as "secret://<name>" resolves at plan time
// against the live secrets.yaml (internal/secretref); the RESOLVED copy is
// what is hashed and sent, so rotating a secret changes the hash and rule
// 1 reconfigures. No op carries the unresolved original - this layer
// persists no declared data and writes no rollback stash - and the driver
// gets the values it must never echo back from the op's Secrets field. An
// unresolvable reference is a per-item error op.
package subentries

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/difftext"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/failmemory"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/registries"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/secretref"
	yaml "go.yaml.in/yaml/v3"
)

// idPattern is the manifest id syntax - the same [a-z0-9_]+ every other
// gitops/ manifest in this add-on uses.
var idPattern = regexp.MustCompile(`^[a-z0-9_]+$`)

// allowedFields are the only per-item fields gitops/subentries.yaml may
// declare besides id. Anything else is a validation error.
var allowedFields = map[string]bool{
	"domain": true, "entry_title": true, "subentry_type": true, "match": true, "data": true,
}

// allowedMatchKeys are the only keys a match block may carry. Both are
// optional individually; at least one must be a non-empty string.
var allowedMatchKeys = map[string]bool{"unique_id": true, "title": true}

// Kind values subentries.Plan's ops carry. Note the absence of KindDelete:
// this layer never deletes a subentry, so un-managing reuses KindUpdate
// with an "unmanage" param instead.
const (
	KindCreate = registries.KindCreate
	KindUpdate = registries.KindUpdate
	KindError  = registries.KindError
)

// keyPrefix namespaces this layer's keys inside the shared state maps,
// exactly as internal/flows' "integration:" does.
const keyPrefix = "subentry:"

// ManifestError is returned when gitops/subentries.yaml fails to parse or
// validate. Error() aggregates every problem found, not just the first.
type ManifestError struct {
	Problems []string
}

func (e *ManifestError) Error() string {
	return strings.Join(e.Problems, "; ")
}

// Desired is the parsed, validated contents of gitops/subentries.yaml: one
// map per declared item, in manifest order, keyed
// "id"/"domain"/"subentry_type"/"match", "data" (defaulted to an empty
// map) and "entry_title" when declared.
type Desired struct {
	Subentries []map[string]any
}

func emptyDesired() Desired { return Desired{Subentries: []map[string]any{}} }

// LoadManifest loads and validates <workdir>/gitops/subentries.yaml. A
// missing file - or a missing gitops/ directory, which fails the same stat
// - yields an empty Desired, not an error: the layer is simply inactive.
func LoadManifest(workdir string) (Desired, error) {
	path := filepath.Join(workdir, "gitops", "subentries.yaml")
	info, statErr := os.Stat(path)
	if statErr != nil || !info.Mode().IsRegular() {
		return emptyDesired(), nil
	}

	data, err := os.ReadFile(path) // #nosec G304 -- path is workdir-relative, constructed by this package only
	if err != nil {
		return Desired{}, &ManifestError{Problems: []string{fmt.Sprintf("subentries.yaml: could not read file: %v", err)}}
	}

	var parsed any
	if err := yaml.Unmarshal(data, &parsed); err != nil {
		return Desired{}, &ManifestError{Problems: []string{fmt.Sprintf("subentries.yaml: invalid YAML: %v", err)}}
	}
	if parsed == nil {
		return emptyDesired(), nil
	}
	obj, ok := parsed.(map[string]any)
	if !ok {
		return Desired{}, &ManifestError{Problems: []string{"subentries.yaml: top level must be a mapping"}}
	}

	itemsRaw, present := obj["subentries"]
	if !present || itemsRaw == nil {
		return emptyDesired(), nil
	}
	items, ok := itemsRaw.([]any)
	if !ok {
		return Desired{}, &ManifestError{Problems: []string{"subentries.yaml: subentries must be a list"}}
	}

	var errs []string
	seen := map[string]bool{}
	result := []map[string]any{}

	for idx, rawItem := range items {
		itemMap, ok := rawItem.(map[string]any)
		if !ok {
			errs = append(errs, fmt.Sprintf("subentries.yaml: subentries[%d] is not a mapping", idx))
			continue
		}

		id, idIsString := itemMap["id"].(string)
		if !idIsString || id == "" || !idPattern.MatchString(id) {
			errs = append(errs, fmt.Sprintf("subentries.yaml: subentries[%d] has an invalid or missing 'id'", idx))
			continue
		}
		if seen[id] {
			errs = append(errs, fmt.Sprintf("subentries.yaml: duplicate subentry id '%s'", id))
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
	return Desired{Subentries: result}, nil
}

// validateItemFields validates one item's fields besides id: domain and
// subentry_type are required non-empty strings; entry_title, when
// declared, must be non-empty (an empty one is a typo - omitting it
// already means "any entry"); match goes to validateMatch; data must be a
// mapping of mappings, one field map per flow step id, defaulted to an
// empty map so every item carries a usable "data" key. Anything outside
// allowedFields is an "unsupported field" error.
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
		errs = append(errs, fmt.Sprintf(
			"subentries.yaml: subentry '%s' has unsupported field(s) %s", id, strings.Join(unknown, ", ")))
	}

	if domain, ok := item["domain"].(string); !ok || domain == "" {
		errs = append(errs, fmt.Sprintf("subentries.yaml: subentry '%s' has an invalid or missing 'domain'", id))
	}
	if subentryType, ok := item["subentry_type"].(string); !ok || subentryType == "" {
		errs = append(errs, fmt.Sprintf("subentries.yaml: subentry '%s' has an invalid or missing 'subentry_type'", id))
	}
	if titleRaw, present := item["entry_title"]; present {
		if title, ok := titleRaw.(string); !ok || title == "" {
			errs = append(errs, fmt.Sprintf("subentries.yaml: subentry '%s' has an invalid 'entry_title'", id))
		}
	}

	errs = append(errs, validateMatch(id, item)...)

	if dataRaw, present := item["data"]; present {
		data, ok := dataRaw.(map[string]any)
		if !ok {
			errs = append(errs, fmt.Sprintf("subentries.yaml: subentry '%s' data must be a mapping", id))
		} else {
			for _, stepID := range sortedKeys(data) {
				if _, ok := data[stepID].(map[string]any); !ok {
					errs = append(errs, fmt.Sprintf(
						"subentries.yaml: subentry '%s' data step '%s' must be a mapping", id, stepID))
				}
			}
		}
	} else {
		item["data"] = map[string]any{}
	}

	if len(errs) > 0 {
		return nil, errs
	}
	return item, nil
}

// validateMatch validates the required match block: only unique_id /
// title, each a string, at least one non-empty. A hard error rather than a
// create-only item, since without a usable match this layer would create a
// duplicate wherever it should have adopted.
func validateMatch(id string, item map[string]any) []string {
	matchRaw, present := item["match"]
	if !present {
		return []string{fmt.Sprintf("subentries.yaml: subentry '%s' is missing 'match'", id)}
	}
	match, ok := matchRaw.(map[string]any)
	if !ok {
		return []string{fmt.Sprintf("subentries.yaml: subentry '%s' match must be a mapping", id)}
	}

	var errs []string
	var unknown []string
	usable := false
	for _, k := range sortedKeys(match) {
		if !allowedMatchKeys[k] {
			unknown = append(unknown, k)
			continue
		}
		v, isString := match[k].(string)
		if !isString {
			errs = append(errs, fmt.Sprintf("subentries.yaml: subentry '%s' match %s must be a string", id, k))
			continue
		}
		if v != "" {
			usable = true
		}
	}
	if len(unknown) > 0 {
		errs = append(errs, fmt.Sprintf(
			"subentries.yaml: subentry '%s' match has unsupported field(s) %s", id, strings.Join(unknown, ", ")))
	}
	// Reported even alongside an unsupported-field error: "match:
	// {entity_id: x}" is both a wrong key and an unusable match.
	if !usable {
		errs = append(errs, fmt.Sprintf(
			"subentries.yaml: subentry '%s' match must declare a non-empty 'unique_id' and/or 'title'", id))
	}
	return errs
}

// HashData fingerprints a subentry flow's declared "data" (step id ->
// field map), so Plan's rule 1 detects a change without persisting the
// data itself - which may hold a credential - in state.SubentryHashes.
// Exported so internal/regapply computes the same fingerprint when it
// records a key under management.
//
// json.Marshal sorts map keys at every level, so manifest key order never
// changes the result. Both sides of every comparison here are
// YAML-parsed, so no int-vs-float64 normalization is needed: that risk
// needs a JSON-decoded live value, which this layer cannot even read.
func HashData(data map[string]any) string {
	return failmemory.Hash(data)
}

// Plan computes the subentry operations needed to reconcile live Home
// Assistant subentries toward desired, given the managed ownership
// mapping, the per-key applied-data hashes, and the per-key memory of the
// last failed attempt.
//
// liveEntries is every config entry as regapply.FetchIntegrationEntries
// returns them ("entry_id", "domain", "title"), used only to resolve a
// declared domain (+entry_title) to the parent entry_id.
// liveSubentriesByEntryID maps a parent entry_id to its live subentry
// objects: "subentry_id", "subentry_type", "title", "unique_id", never
// "data". managed is state.SubentryManaged ("subentry:<id>" -> live
// subentry_id, NOT entry_id: the parent is recovered from where the
// subentry lives); hashes is state.SubentryHashes (-> HashData of the data
// last applied); attempts is state.SubentryAttempts (-> {"hash", "error"}
// for the last attempt that FAILED at this exact data).
//
// # Rules
//
//  1. key managed and its live subentry exists -> compare
//     HashData(declared) against hashes[key]. Equal -> no op. Different ->
//     a reconfigure flow re-submits the data (KindUpdate). A MISSING
//     stored hash counts as different: only a hand-edited state file
//     reaches that, and a reconfigure is idempotent, so re-applying
//     self-heals where treating it as converged would strand the item.
//     Deliberately unlike internal/flows, which has no update to offer.
//  2. key managed but its live subentry is gone (deleted in the UI) ->
//     handled as rule 3, as if never managed; a stale subentry_id can
//     never legitimately come back.
//  3. key unmanaged -> resolve the parent entry, then look for a subentry
//     to adopt within it and of the declared subentry_type: by
//     match.unique_id, falling back to match.title only if unique_id
//     matched nothing. Exactly one -> adopt (KindUpdate), which also
//     reconfigures it. More than one -> a per-item error op. None ->
//     create (KindCreate, driving a subentry flow at apply time).
//  4. key managed but no longer declared -> UNMANAGE (KindUpdate with
//     Params {"unmanage": true}): drop the bookkeeping, leave the live
//     subentry untouched. Deleting one destroys the devices, entities and
//     history hanging off it, with no rollback path - the data needed to
//     re-create it is exactly what HA will not show. Emitted even when
//     the live subentry is already gone, since the key still has to go.
//
// Parent resolution is rule 3 only: rule 1 recovers the parent from live
// placement, so renaming a parent entry never disturbs a converged item.
// Zero or several matching entries is a per-item error op, never a
// failure of the whole cycle.
//
// Failure memory: hashes is only written on SUCCESS, so it cannot remember
// a failure. internal/regapply writes attempts when one fails and clears
// it on success; while attempts holds this key at the CURRENT data's hash,
// rules 1 and 3 emit an error op instead. Updates are blocked too, unlike
// in flows, because a rejected reconfigure would otherwise re-drive a
// doomed flow against a live subentry every few minutes.
//
// secrets resolves "secret://<name>" values; a nil resolver refuses every
// reference with an error op rather than passing one through unresolved.
func Plan(
	desired Desired, liveEntries []map[string]any,
	liveSubentriesByEntryID map[string][]map[string]any,
	managed map[string]string, hashes map[string]string, attempts map[string]map[string]any,
	secrets *secretref.Resolver,
) []registries.RegOp {
	if managed == nil {
		managed = map[string]string{}
	}
	if hashes == nil {
		hashes = map[string]string{}
	}
	if attempts == nil {
		attempts = map[string]map[string]any{}
	}
	if liveSubentriesByEntryID == nil {
		liveSubentriesByEntryID = map[string][]map[string]any{}
	}

	declaredIDs := map[string]bool{}
	for _, item := range desired.Subentries {
		if id, _ := item["id"].(string); id != "" {
			declaredIDs[id] = true
		}
	}

	// claimed holds every subentry_id another manifest key already owns,
	// plus every one adopted as this loop runs, so two items can never
	// adopt the same live subentry.
	//
	// Only keys the manifest STILL declares claim: a key being un-managed
	// this same call is releasing its subentry, which is exactly the shape
	// of renaming a manifest id, and pre-claiming for it would hide the
	// live subentry from its successor - and since this layer leaves the
	// live object in place, that would mean a duplicate it can never clean
	// up. Foreign keys are filtered because this map is shared.
	claimed := map[string]bool{}
	for fullKey, subentryID := range managed {
		if !strings.HasPrefix(fullKey, keyPrefix) {
			continue
		}
		if declaredIDs[strings.TrimPrefix(fullKey, keyPrefix)] {
			claimed[subentryID] = true
		}
	}

	var ops []registries.RegOp

	for _, item := range desired.Subentries {
		id, _ := item["id"].(string)
		subentryType, _ := item["subentry_type"].(string)
		declared, _ := item["data"].(map[string]any)
		key := keyPrefix + id

		// Before the hash: rule 1 compares a hash of the RESOLVED data, so a
		// rotated secret reads as a data change. An unresolvable reference
		// stops this item alone.
		data, secretValues, resolveErr := secrets.ResolveMap(declared)
		if resolveErr != nil {
			ops = append(ops, errorOp(id, secretref.UnresolvedMessage("subentry", id, resolveErr)))
			continue
		}
		currentHash := HashData(data)

		if subentryID, isManaged := managed[key]; isManaged {
			if parentID, liveType, exists := locateSubentry(liveSubentriesByEntryID, subentryID); exists {
				if liveType != subentryType {
					// HashData covers only the declared data, so a
					// subentry_type edit is invisible until the data changes
					// and then drives a NEW-type flow against an old-type
					// subentry. A type is not reconfigurable.
					ops = append(ops, errorOp(id, fmt.Sprintf(
						"declared subentry_type '%s' does not match the managed live subentry's type '%s'; "+
							"a subentry cannot change type - remove it in Home Assistant and let this entry create a new one",
						subentryType, liveType)))
					continue
				}
				if hashes[key] == currentHash {
					continue
				}
				if refusal, blocked := failmemory.Refusal(attempts, key, currentHash); blocked {
					ops = append(ops, errorOp(id, refusal))
					continue
				}
				ops = append(ops, registries.RegOp{
					Kind: KindUpdate, RType: "subentry", Key: id,
					Params: map[string]any{
						"entry_id": parentID, "subentry_id": subentryID,
						"subentry_type": subentryType, "data": data,
					},
					LiveID:   subentryID,
					DiffText: reconfigureText(id, subentryType, declared),
					Secrets:  secretValues,
				})
				continue
			}
			// Rule 2: managed but the live subentry is gone - fall through
			// to adopt-or-create below, exactly as if never managed.
		}

		domain, _ := item["domain"].(string)
		entryTitle, _ := item["entry_title"].(string)
		parentID, problem := resolveParent(liveEntries, domain, entryTitle)
		if problem != "" {
			ops = append(ops, errorOp(id, problem))
			continue
		}

		match, _ := item["match"].(map[string]any)
		matchUniqueID, _ := match["unique_id"].(string)
		matchTitle, _ := match["title"].(string)
		candidates, matchedBy := matchSubentries(
			liveSubentriesByEntryID[parentID], subentryType, matchUniqueID, matchTitle, claimed)

		switch {
		case matchedBy == "unique_id_claimed":
			// The declared unique_id names a subentry another key owns;
			// falling back to title would reconfigure some OTHER object
			// with data meant for this one.
			ops = append(ops, errorOp(id, fmt.Sprintf(
				"the live subentry of type '%s' with unique_id %s is already managed by another manifest entry",
				subentryType, difftext.PyRepr(matchUniqueID))))
		case len(candidates) == 1:
			subentryID, _ := candidates[0]["subentry_id"].(string)
			if subentryID == "" {
				ops = append(ops, errorOp(id, fmt.Sprintf(
					"live subentry of type '%s' matched by %s has no usable subentry_id field", subentryType, matchedBy)))
				continue
			}
			if refusal, blocked := failmemory.Refusal(attempts, key, currentHash); blocked {
				// An adopt drives a real reconfigure flow, so a doomed one
				// would re-drive every cycle exactly like a doomed update.
				ops = append(ops, errorOp(id, refusal))
				continue
			}
			claimed[subentryID] = true
			ops = append(ops, registries.RegOp{
				Kind: KindUpdate, RType: "subentry", Key: id,
				Params: map[string]any{
					"entry_id": parentID, "subentry_id": subentryID,
					"subentry_type": subentryType, "data": data,
				},
				LiveID:   subentryID,
				DiffText: adoptedText(id, subentryType, subentryID, matchedBy, declared),
				Secrets:  secretValues,
			})
		case len(candidates) > 1:
			ops = append(ops, errorOp(id, fmt.Sprintf(
				"ambiguous adopt: %d live subentries of type '%s' under entry %s match %s %s",
				len(candidates), subentryType, parentID, matchedBy,
				difftext.PyRepr(matchedValue(matchedBy, matchUniqueID, matchTitle)))))
		default:
			if refusal, blocked := failmemory.Refusal(attempts, key, currentHash); blocked {
				ops = append(ops, errorOp(id, refusal))
				continue
			}
			ops = append(ops, registries.RegOp{
				Kind: KindCreate, RType: "subentry", Key: id,
				Params: map[string]any{
					"entry_id": parentID, "subentry_type": subentryType, "data": data,
					"match_unique_id": matchUniqueID, "match_title": matchTitle,
				},
				DiffText: createText(subentryType, parentID, declared),
				Secrets:  secretValues,
			})
		}
	}

	var unmanageKeys []string
	for fullKey := range managed {
		if strings.HasPrefix(fullKey, keyPrefix) {
			unmanageKeys = append(unmanageKeys, fullKey)
		}
	}
	sort.Strings(unmanageKeys)
	for _, fullKey := range unmanageKeys {
		id := strings.TrimPrefix(fullKey, keyPrefix)
		if declaredIDs[id] {
			continue
		}
		ops = append(ops, registries.RegOp{
			Kind: KindUpdate, RType: "subentry", Key: id,
			Params:   map[string]any{"unmanage": true},
			LiveID:   managed[fullKey],
			DiffText: unmanageText(id),
		})
	}

	return ops
}

// sortedKeys returns m's keys in a stable order, so validation problems
// and diff text never depend on Go's randomized map iteration.
func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// locateSubentry finds which parent entry currently holds subentryID, and
// returns that subentry's own subentry_type so the caller can refuse a
// manifest that changed the declared type (a change the data hash cannot
// see). Read from live placement rather than re-resolved from the
// manifest, so renaming a parent entry never breaks a converged item.
// Entry ids are visited sorted, keeping the result deterministic even in
// the shape this cannot legitimately have - one subentry under two
// parents.
func locateSubentry(liveSubentriesByEntryID map[string][]map[string]any, subentryID string) (entryID, subentryType string, found bool) {
	if subentryID == "" {
		return "", "", false
	}
	entryIDs := make([]string, 0, len(liveSubentriesByEntryID))
	for k := range liveSubentriesByEntryID {
		entryIDs = append(entryIDs, k)
	}
	sort.Strings(entryIDs)
	for _, eid := range entryIDs {
		for _, sub := range liveSubentriesByEntryID[eid] {
			if sid, _ := sub["subentry_id"].(string); sid == subentryID {
				liveType, _ := sub["subentry_type"].(string)
				return eid, liveType, true
			}
		}
	}
	return "", "", false
}

// resolveParent resolves the declared domain, narrowed by entryTitle, to
// exactly one live config entry's entry_id. A non-empty problem means the
// caller emits an error op instead: guessing which entry a subentry
// belongs under would attach devices to the wrong integration.
func resolveParent(liveEntries []map[string]any, domain, entryTitle string) (entryID, problem string) {
	var matches []map[string]any
	for _, e := range liveEntries {
		if d, _ := e["domain"].(string); d != domain {
			continue
		}
		if entryTitle != "" {
			if t, _ := e["title"].(string); t != entryTitle {
				continue
			}
		}
		matches = append(matches, e)
	}

	switch {
	case len(matches) == 0:
		return "", fmt.Sprintf(
			"no live integration entry for domain '%s'%s to hold this subentry; set the integration up first",
			domain, titleQualifier(entryTitle))
	case len(matches) > 1:
		return "", fmt.Sprintf(
			"ambiguous parent: %d live integration entries for domain '%s'%s; set 'entry_title' to pick one",
			len(matches), domain, titleQualifier(entryTitle))
	}

	entryID, _ = matches[0]["entry_id"].(string)
	if entryID == "" {
		return "", fmt.Sprintf(
			"live integration entry for domain '%s'%s has no usable entry_id field", domain, titleQualifier(entryTitle))
	}
	return entryID, ""
}

// matchSubentries returns the live subentries of the parent that could be
// the declared one, and which match field found them. Candidates are
// narrowed to the declared subentry_type first, and already-claimed
// subentries are invisible. unique_id is tried before title (a flow
// assigns it; a title is user-editable), and title only when unique_id
// matched nothing at all.
//
// The one case that must NOT fall through to title is a unique_id
// matching a subentry another key already claimed - trying the title
// would write this item's data onto a different object - so it is
// reported as matchedBy "unique_id_claimed" and the caller refuses.
func matchSubentries(
	liveSubentries []map[string]any, subentryType, matchUniqueID, matchTitle string, claimed map[string]bool,
) (candidates []map[string]any, matchedBy string) {
	byField := func(field, want string) (found, claimedOut []map[string]any) {
		if want == "" {
			return nil, nil
		}
		for _, sub := range liveSubentries {
			if t, _ := sub["subentry_type"].(string); t != subentryType {
				continue
			}
			if v, _ := sub[field].(string); v != want {
				continue
			}
			if sid, _ := sub["subentry_id"].(string); claimed[sid] {
				claimedOut = append(claimedOut, sub)
				continue
			}
			found = append(found, sub)
		}
		return found, claimedOut
	}

	found, claimedByUniqueID := byField("unique_id", matchUniqueID)
	if len(found) > 0 {
		return found, "unique_id"
	}
	if len(claimedByUniqueID) > 0 {
		return nil, "unique_id_claimed"
	}
	if found, _ := byField("title", matchTitle); len(found) > 0 {
		return found, "title"
	}
	return nil, ""
}

// matchedValue returns the declared value behind whichever match field
// produced a set of candidates, for the ambiguity message.
func matchedValue(matchedBy, matchUniqueID, matchTitle string) string {
	if matchedBy == "unique_id" {
		return matchUniqueID
	}
	return matchTitle
}

func errorOp(id, msg string) registries.RegOp {
	return registries.RegOp{Kind: KindError, RType: "subentry", Key: id, Params: map[string]any{}, Error: msg}
}

// titleQualifier renders an optional entry_title for an error message.
func titleQualifier(entryTitle string) string {
	if entryTitle == "" {
		return ""
	}
	return fmt.Sprintf(" titled %s", difftext.PyRepr(entryTitle))
}

// dataFieldsText renders the declared data as step id -> field NAMES,
// never values: a flow's fields routinely include a credential, and this
// text reaches the web UI's pending-op card and the log.
func dataFieldsText(data map[string]any) string {
	if len(data) == 0 {
		return "(no data declared)"
	}
	var steps []string
	for _, stepID := range sortedKeys(data) {
		fields, ok := data[stepID].(map[string]any)
		if !ok || len(fields) == 0 {
			steps = append(steps, fmt.Sprintf("step '%s': (no fields)", stepID))
			continue
		}
		steps = append(steps, fmt.Sprintf("step '%s': %s", stepID, strings.Join(sortedKeys(fields), ", ")))
	}
	return strings.Join(steps, "; ")
}

func createText(subentryType, parentEntryID string, data map[string]any) string {
	return fmt.Sprintf(
		"+subentry_type: %s\n+parent entry_id: %s\n+declared fields (values hidden): %s\n"+
			"(a new subentry flow will be driven to create this subentry)\n",
		subentryType, parentEntryID, dataFieldsText(data))
}

func reconfigureText(id, subentryType string, data map[string]any) string {
	return fmt.Sprintf(
		"declared data for subentry '%s' (type '%s') changed; a reconfigure flow will re-submit it\n"+
			"declared fields (values hidden): %s\n",
		id, subentryType, dataFieldsText(data))
}

func adoptedText(id, subentryType, subentryID, matchedBy string, data map[string]any) string {
	return fmt.Sprintf(
		"adopted existing subentry '%s' (type '%s', live subentry_id %s, matched by %s); "+
			"a reconfigure flow will apply the declared data\n"+
			"declared fields (values hidden): %s\n",
		id, subentryType, subentryID, matchedBy, dataFieldsText(data))
}

func unmanageText(id string) string {
	return fmt.Sprintf("stopped managing subentry '%s'; the live subentry is left untouched", id)
}
