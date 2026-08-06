// Package flows plans reconciliation of Home Assistant config-entry
// integrations against gitops/integrations.yaml. It emits registries.RegOp
// values, so it reuses the same pending-ops list, web UI card and status
// counts as internal/registries.
//
// Home Assistant has no "edit a config entry's data" call, and data a flow
// submitted is not readable back, so this layer only creates (drives a
// flow), adopts (bookkeeping only - its KindUpdate op never contacts HA)
// and deletes. Only plain multi-step FORM flows are supported: OAuth/
// external-auth, discovery, options and reauth flows abort into a per-item
// error op. Planning only; the driving lives in internal/regapply's
// flows.go.
//
// # Manifest format
//
//	integrations:
//	  - id: workday_main         # manifest key; required
//	    domain: workday          # the integration's domain; required
//	    title: Workday           # required; adopt matches by domain + exact title
//	    data:                    # optional; flow input per step id
//	      user: {name: Workday, country: PL}
//
// data maps each step id to that step's field mapping - exactly the shape
// internal/regapply POSTs back into each "form" step.
//
// # Ownership
//
//  1. key not in managed: exactly one unclaimed live entry with this domain
//     and this exact title -> adopt (KindUpdate). Several -> error op.
//     None -> create (KindCreate).
//  2. key in managed and the entry still exists: hash of the declared data
//     vs. the hash snapshotted at adoption (state.IntegrationHashes).
//     Different -> error op telling the user to delete and re-declare.
//  3. key in managed but the live entry is gone -> rule 1 again; a stale
//     managed entry_id can never legitimately be reused.
//  4. key in managed but no longer declared -> delete the live entry. An
//     unmanaged entry sharing the same domain+title is never touched.
//
// # Failure memory
//
// A create that fails at apply time would otherwise be retried every
// cycle forever, driving a fresh flow each time. attempts
// (state.IntegrationAttempts, written by internal/regapply) records the
// data hash that failed; while it still matches, rule 1/3 emits an error
// op instead of creating, until the manifest entry changes.
//
// # Secret references
//
// "secret://<name>" values are resolved at plan time from the live
// secrets.yaml (see internal/secretref). The RESOLVED copy is hashed (so a
// rotation reads as a data change under rule 2) and travels as Params'
// "data" for internal/regapply to submit; the UNRESOLVED original travels
// as the op's Declared field, which is all that reaches state and the
// stash - so neither file ever holds the credential. An unresolvable
// reference is a per-item error op, so one broken item cannot stop the
// rest.
//
// Params carry "domain", "title" and "data" on create/adopt, and are empty
// on delete - internal/regapply reads those off the live entry instead.
package flows

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

// idPattern is the manifest id syntax, shared with every other gitops/
// manifest in this add-on.
var idPattern = regexp.MustCompile(`^[a-z0-9_]+$`)

// allowedFields are the only per-item fields besides id; anything else is
// a validation error.
var allowedFields = map[string]bool{"domain": true, "title": true, "data": true}

// Kinds flows.Plan's ops carry - registries' own constants, no new one.
const (
	KindCreate = registries.KindCreate
	KindUpdate = registries.KindUpdate
	KindDelete = registries.KindDelete
	KindError  = registries.KindError
)

// ManifestError is returned when gitops/integrations.yaml fails to parse
// or validate. Error() aggregates every problem, not just the first.
type ManifestError struct {
	Problems []string
}

func (e *ManifestError) Error() string {
	return strings.Join(e.Problems, "; ")
}

// Desired is the parsed, validated gitops/integrations.yaml: one map per
// item in manifest order, always carrying id/domain/title/data ("data"
// defaults to an empty map).
type Desired struct {
	Integrations []map[string]any
}

func emptyDesired() Desired { return Desired{Integrations: []map[string]any{}} }

// LoadManifest loads and validates <workdir>/gitops/integrations.yaml. A
// missing file returns an empty Desired, not an error: the layer is idle.
func LoadManifest(workdir string) (Desired, error) {
	path := filepath.Join(workdir, "gitops", "integrations.yaml")
	info, statErr := os.Stat(path)
	if statErr != nil || !info.Mode().IsRegular() {
		return emptyDesired(), nil
	}

	data, err := os.ReadFile(path) // #nosec G304 -- path is workdir-relative, constructed by this package only
	if err != nil {
		return Desired{}, &ManifestError{Problems: []string{fmt.Sprintf("integrations.yaml: could not read file: %v", err)}}
	}

	var parsed any
	if err := yaml.Unmarshal(data, &parsed); err != nil {
		return Desired{}, &ManifestError{Problems: []string{fmt.Sprintf("integrations.yaml: invalid YAML: %v", err)}}
	}
	if parsed == nil {
		return emptyDesired(), nil
	}
	obj, ok := parsed.(map[string]any)
	if !ok {
		return Desired{}, &ManifestError{Problems: []string{"integrations.yaml: top level must be a mapping"}}
	}

	itemsRaw, present := obj["integrations"]
	if !present || itemsRaw == nil {
		return emptyDesired(), nil
	}
	items, ok := itemsRaw.([]any)
	if !ok {
		return Desired{}, &ManifestError{Problems: []string{"integrations.yaml: integrations must be a list"}}
	}

	var errs []string
	seen := map[string]bool{}
	result := []map[string]any{}

	for idx, rawItem := range items {
		itemMap, ok := rawItem.(map[string]any)
		if !ok {
			errs = append(errs, fmt.Sprintf("integrations.yaml: integrations[%d] is not a mapping", idx))
			continue
		}

		id, idIsString := itemMap["id"].(string)
		if !idIsString || id == "" || !idPattern.MatchString(id) {
			errs = append(errs, fmt.Sprintf("integrations.yaml: integrations[%d] has an invalid or missing 'id'", idx))
			continue
		}
		if seen[id] {
			errs = append(errs, fmt.Sprintf("integrations.yaml: duplicate integration id '%s'", id))
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
	return Desired{Integrations: result}, nil
}

// validateItemFields validates one manifest item apart from id (the caller
// did that): domain and title are required non-empty strings; data must be
// a mapping of mappings, defaulted to empty so it is always present.
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
			"integrations.yaml: integration '%s' has unsupported field(s) %s", id, strings.Join(unknown, ", ")))
	}

	if domain, ok := item["domain"].(string); !ok || domain == "" {
		errs = append(errs, fmt.Sprintf("integrations.yaml: integration '%s' has an invalid or missing 'domain'", id))
	}
	if title, ok := item["title"].(string); !ok || title == "" {
		errs = append(errs, fmt.Sprintf("integrations.yaml: integration '%s' has an invalid or missing 'title'", id))
	}

	if dataRaw, present := item["data"]; present {
		data, ok := dataRaw.(map[string]any)
		if !ok {
			errs = append(errs, fmt.Sprintf("integrations.yaml: integration '%s' data must be a mapping", id))
		} else {
			for stepID, fieldsRaw := range data {
				if _, ok := fieldsRaw.(map[string]any); !ok {
					errs = append(errs, fmt.Sprintf(
						"integrations.yaml: integration '%s' data step '%s' must be a mapping", id, stepID))
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

// HashData fingerprints a flow's declared data (step id -> field map) so
// rule 2 can detect a change without persisting the data itself, which may
// hold a credential. Key-order insensitive (json.Marshal sorts keys at
// every level). Exported so internal/regapply hashes identically.
func HashData(data map[string]any) string {
	return failmemory.Hash(data)
}

// Plan computes the ops reconciling live config entries toward desired,
// following the ownership rules in the package doc comment.
//
// liveEntries are config entries as GET /api/config/config_entries/entry
// returns them (at least entry_id, domain, title). managed, hashes and
// attempts are the state maps keyed "integration:<manifest id>". secrets
// resolves "secret://" values; a nil resolver refuses every reference
// rather than letting one through unresolved.
func Plan(
	desired Desired, liveEntries []map[string]any,
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

	liveByEntryID := map[string]map[string]any{}
	for _, e := range liveEntries {
		if id, ok := e["entry_id"].(string); ok && id != "" {
			liveByEntryID[id] = e
		}
	}
	claimed := map[string]bool{}
	for _, liveID := range managed {
		claimed[liveID] = true
	}

	declaredIDs := map[string]bool{}
	var ops []registries.RegOp

	for _, item := range desired.Integrations {
		id, _ := item["id"].(string)
		domain, _ := item["domain"].(string)
		title, _ := item["title"].(string)
		declared, _ := item["data"].(map[string]any)
		declaredIDs[id] = true
		key := "integration:" + id

		// Resolve first: the hash rule, the adopt payload and the create
		// payload must all see the same resolved copy.
		data, secretValues, resolveErr := secrets.ResolveMap(declared)
		if resolveErr != nil {
			ops = append(ops, errorOp(id, secretref.UnresolvedMessage("integration", id, resolveErr)))
			continue
		}

		if liveID, isManaged := managed[key]; isManaged {
			if _, exists := liveByEntryID[liveID]; exists {
				currentHash := HashData(data)
				if storedHash := hashes[key]; storedHash != "" && storedHash != currentHash {
					// The hash covers RESOLVED data, so "changed" can also mean a
					// referenced secret was rotated, with no repository diff.
					rotated := ""
					if len(secretValues) > 0 {
						rotated = " (which includes a referenced secret being rotated in secrets.yaml)"
					}
					ops = append(ops, errorOp(id, fmt.Sprintf(
						"declared data for integration '%s' (domain '%s') changed after it was created%s; "+
							"this layer cannot update an existing config entry's data - delete it "+
							"(remove it from the manifest, let this apply, then re-declare it) to apply the new configuration",
						id, domain, rotated)))
				}
				continue
			}
			// Rule 3: managed but the live entry is gone - fall through to
			// adopt-or-create as if never managed.
		}

		var matches []map[string]any
		for _, e := range liveEntries {
			eDomain, _ := e["domain"].(string)
			eTitle, _ := e["title"].(string)
			eID, _ := e["entry_id"].(string)
			if eDomain == domain && eTitle == title && !claimed[eID] {
				matches = append(matches, e)
			}
		}

		switch {
		case len(matches) == 1:
			entryID, _ := matches[0]["entry_id"].(string)
			if entryID == "" {
				ops = append(ops, errorOp(id, fmt.Sprintf(
					"live integration entry matched by domain '%s' title %s has no usable entry_id field", domain, difftext.PyRepr(title))))
				continue
			}
			claimed[entryID] = true
			ops = append(ops, registries.RegOp{
				Kind: KindUpdate, RType: "integration", Key: id,
				Params:   map[string]any{"domain": domain, "title": title, "data": data},
				LiveID:   entryID,
				DiffText: adoptedText(id, domain, entryID),
				Secrets:  secretValues,
				Declared: declared,
			})
		case len(matches) > 1:
			ops = append(ops, errorOp(id, fmt.Sprintf(
				"ambiguous adopt: %d live integration entries for domain '%s' titled %s", len(matches), domain, difftext.PyRepr(title))))
		default:
			if refusal, blocked := failmemory.Refusal(attempts, key, HashData(data)); blocked {
				ops = append(ops, errorOp(id, refusal))
				continue
			}
			ops = append(ops, registries.RegOp{
				Kind: KindCreate, RType: "integration", Key: id,
				Params:   map[string]any{"domain": domain, "title": title, "data": data},
				DiffText: createText(id, domain, title),
				Secrets:  secretValues,
				Declared: declared,
			})
		}
	}

	var deleteKeys []string
	for fullKey := range managed {
		if strings.HasPrefix(fullKey, "integration:") {
			deleteKeys = append(deleteKeys, fullKey)
		}
	}
	sort.Strings(deleteKeys)
	for _, fullKey := range deleteKeys {
		id := strings.TrimPrefix(fullKey, "integration:")
		if declaredIDs[id] {
			continue
		}
		liveID := managed[fullKey]
		liveEntry, exists := liveByEntryID[liveID]
		if !exists {
			continue
		}
		domain, _ := liveEntry["domain"].(string)
		title, _ := liveEntry["title"].(string)
		ops = append(ops, registries.RegOp{
			Kind: KindDelete, RType: "integration", Key: id, Params: map[string]any{}, LiveID: liveID,
			DiffText: deleteText(id, domain, title, liveID),
		})
	}

	return ops
}

func errorOp(id, msg string) registries.RegOp {
	return registries.RegOp{Kind: KindError, RType: "integration", Key: id, Params: map[string]any{}, Error: msg}
}

func adoptedText(id, domain, entryID string) string {
	return fmt.Sprintf("adopted existing integration '%s' (domain '%s', live entry_id %s); no flow will run", id, domain, entryID)
}

func createText(id, domain, title string) string {
	return fmt.Sprintf("+domain: %s\n+title: %s\n(a new config-entry flow will be driven to create this integration)\n", domain, title)
}

func deleteText(id, domain, title, entryID string) string {
	return fmt.Sprintf("-domain: %s\n-title: %s\n-entry_id: %s\n", domain, title, entryID)
}
