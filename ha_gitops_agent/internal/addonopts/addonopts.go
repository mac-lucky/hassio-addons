// Package addonopts computes a reconciliation plan for other Home
// Assistant add-ons' configured options, against gitops/addons.yaml.
//
// Update-only like internal/entities and emitting the same
// registries.RegOp shape, but driven over the Supervisor's REST API
// (execution side: internal/regapply's addonopts.go) - HA Core's
// WebSocket has no add-on-management surface.
//
//	addons:
//	  - slug: core_configurator   # the key; required
//	    options:                  # required, non-empty; ONLY these keys
//	      dirsfirst: true         # are ever read, compared, or written
//	    restart_on_change: true   # optional, default true
//
// Only the KEYS under options are touched; their values pass through
// untouched, in whatever shape the add-on's own schema takes.
//
// # Ownership
//
//  1. A declared slug that is not installed is a per-item KindError op -
//     this layer never installs an add-on.
//  2. The add-on this agent runs as (GET /addons/self/info) is refused in
//     both directions, so a reconcile can never reconfigure or restart
//     itself.
//  3. Otherwise KindUpdate on value drift, or when a declared key was
//     never recorded as managed (hasNewKey) so the applier has something
//     to execute that records that key's original value.
//  4. Dropping a still-managed slug from the manifest plans KindRestore
//     back to the recorded originals; a key that had no value is restored
//     by removing it (see AbsentMarker), never by writing null over it.
//
// Comparison goes through registries.ValuesEqual, so a value round-
// tripping through Supervisor's JSON never reads as drift.
//
// # Secret references
//
// A "secret://<name>" option value is resolved at plan time from the live
// secrets.yaml (internal/secretref): the resolved copy is compared and
// sent, the unresolved original rides on the op's Declared field and is
// what a plan line renders ("<key>: (hidden) -> secret://<name>"). An
// unresolvable reference is a per-item error op.
//
// state.AddonOriginals only ever holds the add-on's own prior live value,
// so no reference reaches it; the rollback stash is narrowed key by key
// instead (stashPriorOptions in internal/regapply). A rule-4 restore has
// no manifest entry left to say which keys came from a reference, so its
// plan line and stash entry both carry the resolved live value - one-shot,
// and closing it would cost a durable state.json field.
package addonopts

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/difftext"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/registries"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/secretref"
	yaml "go.yaml.in/yaml/v3"
)

// slugPattern validates a Supervisor add-on slug: lowercase
// alphanumerics, underscore and hyphen. Looser than other manifests'
// [a-z0-9_]+ ids because a slug is Supervisor's own external identifier,
// not a user-chosen manifest id.
var slugPattern = regexp.MustCompile(`^[a-z0-9_-]+$`)

// allowedFields are the only per-item fields gitops/addons.yaml may
// declare besides slug itself. Anything else is a validation error.
var allowedFields = map[string]bool{"slug": true, "options": true, "restart_on_change": true}

// Kind values addonopts.Plan's ops carry. KindRestore repeats
// entities.KindRestore's literal rather than importing it (layers stay
// decoupled at the source level) and means the same: an un-manage that
// restores prior values rather than deleting anything.
const (
	KindUpdate  = registries.KindUpdate
	KindError   = registries.KindError
	KindRestore = "restore"
)

// absentMarkerKey keys the sentinel object meaning "this option had no
// value before this agent touched it" in state.AddonOriginals and the
// applier's addon_stash.json. Un-typeable as a real option name.
const absentMarkerKey = "__ha_gitops_agent_option_absent__"

// absentRepr is how AbsentMarker renders in a plan's diff_text, next to
// reprValue's "None" for a value that genuinely is null.
const absentRepr = "(unset)"

// AbsentMarker returns the sentinel recorded as an option key's original
// value when that key had no value to begin with.
//
// Supervisor rejects an explicit null as "unset" (HTTP 400, "Missing
// required option") - a key is only put back by leaving it out of the
// merged object - so a restore must tell "had no value" from "had null"
// even after a JSON round trip through state.json, where both are the
// same three characters. Hence a value, not an absence. A fresh map per
// call, so a stored copy never aliases another entry's.
func AbsentMarker() map[string]any {
	return map[string]any{absentMarkerKey: true}
}

// IsAbsent reports whether v is the AbsentMarker sentinel, before or
// after a JSON round trip. The length check keeps a real option mapping
// that carries that key alongside others an ordinary value.
func IsAbsent(v any) bool {
	m, ok := v.(map[string]any)
	if !ok || len(m) != 1 {
		return false
	}
	marked, ok := m[absentMarkerKey].(bool)
	return ok && marked
}

// ManifestError is returned when gitops/addons.yaml fails to parse or
// validate. Error() aggregates every problem found, not just the first.
type ManifestError struct {
	Problems []string
}

func (e *ManifestError) Error() string {
	return strings.Join(e.Problems, "; ")
}

// Desired is the parsed, validated contents of gitops/addons.yaml: one
// map per declared item, in manifest order, keyed "slug", "options" (a
// non-empty map) and "restart_on_change" (always present, default true).
type Desired struct {
	Addons []map[string]any
}

func emptyDesired() Desired { return Desired{Addons: []map[string]any{}} }

// LoadManifest loads and validates <workdir>/gitops/addons.yaml. A
// missing file yields an empty Desired, not an error: the layer is simply
// inactive for that cycle.
func LoadManifest(workdir string) (Desired, error) {
	path := filepath.Join(workdir, "gitops", "addons.yaml")
	info, statErr := os.Stat(path)
	if statErr != nil || !info.Mode().IsRegular() {
		return emptyDesired(), nil
	}

	data, err := os.ReadFile(path) // #nosec G304 -- path is workdir-relative, constructed by this package only
	if err != nil {
		return Desired{}, &ManifestError{Problems: []string{fmt.Sprintf("addons.yaml: could not read file: %v", err)}}
	}

	var parsed any
	if err := yaml.Unmarshal(data, &parsed); err != nil {
		return Desired{}, &ManifestError{Problems: []string{fmt.Sprintf("addons.yaml: invalid YAML: %v", err)}}
	}
	if parsed == nil {
		return emptyDesired(), nil
	}
	obj, ok := parsed.(map[string]any)
	if !ok {
		return Desired{}, &ManifestError{Problems: []string{"addons.yaml: top level must be a mapping"}}
	}

	itemsRaw, present := obj["addons"]
	if !present || itemsRaw == nil {
		return emptyDesired(), nil
	}
	items, ok := itemsRaw.([]any)
	if !ok {
		return Desired{}, &ManifestError{Problems: []string{"addons.yaml: addons must be a list"}}
	}

	var errs []string
	seen := map[string]bool{}
	result := []map[string]any{}

	for idx, rawItem := range items {
		itemMap, ok := rawItem.(map[string]any)
		if !ok {
			errs = append(errs, fmt.Sprintf("addons.yaml: addons[%d] is not a mapping", idx))
			continue
		}

		slug, slugIsString := itemMap["slug"].(string)
		if !slugIsString || slug == "" || !slugPattern.MatchString(slug) {
			errs = append(errs, fmt.Sprintf("addons.yaml: addons[%d] has an invalid or missing 'slug'", idx))
			continue
		}
		if seen[slug] {
			errs = append(errs, fmt.Sprintf("addons.yaml: duplicate slug '%s'", slug))
			continue
		}

		item, itemErrs := validateItemFields(slug, itemMap)
		if len(itemErrs) > 0 {
			errs = append(errs, itemErrs...)
			continue
		}

		seen[slug] = true
		result = append(result, item)
	}

	if len(errs) > 0 {
		return Desired{}, &ManifestError{Problems: errs}
	}
	return Desired{Addons: result}, nil
}

// validateItemFields validates one item's fields besides slug: anything
// outside allowedFields errors, options must be a non-empty mapping, and
// restart_on_change must be a bool - defaulted to true when not declared.
func validateItemFields(slug string, itemMap map[string]any) (map[string]any, []string) {
	item := map[string]any{"slug": slug}
	var errs []string
	var unknown []string

	for k, v := range itemMap {
		switch {
		case k == "slug":
			continue
		case !allowedFields[k]:
			unknown = append(unknown, k)
		default:
			item[k] = v
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		errs = append(errs, fmt.Sprintf("addons.yaml: addon '%s' has unsupported field(s) %s", slug, strings.Join(unknown, ", ")))
	}
	if len(errs) > 0 {
		return nil, errs
	}

	optionsRaw, hasOptions := item["options"]
	options, optionsIsMap := optionsRaw.(map[string]any)
	if !hasOptions || !optionsIsMap || len(options) == 0 {
		errs = append(errs, fmt.Sprintf("addons.yaml: addon '%s' has a missing or empty 'options'", slug))
	}

	if v, ok := item["restart_on_change"]; ok {
		if _, ok := v.(bool); !ok {
			errs = append(errs, fmt.Sprintf("addons.yaml: addon '%s' restart_on_change must be a boolean", slug))
		}
	} else {
		item["restart_on_change"] = true
	}

	if len(errs) > 0 {
		return nil, errs
	}
	return item, nil
}

// DeclaredRestartOnChange maps each declared slug to its
// restart_on_change, for the applier to consult on a KindUpdate. A slug
// absent from the result was not declared this cycle; a restore reads the
// persisted state.AddonRestartOnChange instead.
func DeclaredRestartOnChange(desired Desired) map[string]bool {
	out := make(map[string]bool, len(desired.Addons))
	for _, item := range desired.Addons {
		slug, _ := item["slug"].(string)
		restart, _ := item["restart_on_change"].(bool)
		out[slug] = restart
	}
	return out
}

// Plan computes the add-on option operations needed to reconcile live
// Supervisor state toward desired, given the per-key original-value
// snapshots (state.AddonOriginals, holding AbsentMarker for a key that
// had no value) and the slug this agent runs as. Ownership rules: see the
// package doc comment.
//
// live is keyed by slug and shaped like Supervisor's own GET
// /addons/<slug>/info "data" object: "options", plus "installed" present
// and false only for a known-but-never-installed add-on (an installed one
// carries no such key). A slug missing from live counts as not installed;
// internal/regapply's FetchAddonInfoAll synthesizes an entry for every
// slug it was asked to fetch.
//
// secrets resolves "secret://<name>" option values; a nil resolver
// refuses every reference with an error op rather than passing one
// through unresolved.
func Plan(
	desired Desired, live map[string]map[string]any, originals map[string]map[string]any, selfSlug string,
	secrets *secretref.Resolver,
) []registries.RegOp {
	if originals == nil {
		originals = map[string]map[string]any{}
	}
	declaredSlugs := map[string]bool{}
	var ops []registries.RegOp

	for _, item := range desired.Addons {
		slug, _ := item["slug"].(string)
		declaredSlugs[slug] = true
		key := "addon:" + slug

		if selfSlug != "" && slug == selfSlug {
			ops = append(ops, errorOp(slug, "refusing to manage this add-on's own options (self-protection)"))
			continue
		}

		if !isInstalled(live[slug]) {
			ops = append(ops, errorOp(slug, "add-on not installed: "+slug))
			continue
		}

		declared, _ := item["options"].(map[string]any)
		options, secretValues, resolveErr := secrets.ResolveMap(declared)
		if resolveErr != nil {
			ops = append(ops, errorOp(slug, fmt.Sprintf(
				"declared options for add-on '%s' reference a secret that could not be resolved: %v", slug, resolveErr)))
			continue
		}

		existingOriginals, hasOriginals := originals[key]
		firstRecording := !hasOriginals || hasNewKey(options, existingOriginals)
		diffText := optionsDiff(options, liveOptionsOf(live[slug]), declared)
		if diffText == "" && !firstRecording {
			continue
		}
		if diffText == "" {
			diffText = adoptedNoChangeText(slug)
		}
		ops = append(ops, registries.RegOp{
			Kind: KindUpdate, RType: "addon", Key: slug, Params: options, LiveID: slug, DiffText: diffText,
			Secrets: secretValues, Declared: declared,
		})
	}

	var restoreSlugs []string
	for key := range originals {
		slug := strings.TrimPrefix(key, "addon:")
		if declaredSlugs[slug] {
			continue
		}
		restoreSlugs = append(restoreSlugs, slug)
	}
	sort.Strings(restoreSlugs)

	for _, slug := range restoreSlugs {
		key := "addon:" + slug
		if selfSlug != "" && slug == selfSlug {
			ops = append(ops, errorOp(slug, "refusing to restore this add-on's own options (self-protection)"))
			continue
		}
		if !isInstalled(live[slug]) {
			ops = append(ops, errorOp(slug, fmt.Sprintf(
				"add-on not installed: %s (was managed; cannot restore its original options)", slug)))
			continue
		}
		restoreParams := originals[key]
		// nil declared: a restore's params are recorded live values, which
		// cannot contain a manifest reference.
		diffText := optionsDiff(restoreParams, liveOptionsOf(live[slug]), nil)
		if diffText == "" {
			diffText = restoreNoChangeText(slug)
		}
		ops = append(ops, registries.RegOp{
			Kind: KindRestore, RType: "addon", Key: slug, Params: restoreParams, LiveID: slug, DiffText: diffText,
		})
	}

	return ops
}

// isInstalled reads one slug's raw live info entry: an explicit
// "installed": false, or a nil entry, means not installed; anything else
// - including the normal response, which carries no such key - means
// installed.
func isInstalled(raw map[string]any) bool {
	if raw == nil {
		return false
	}
	if v, ok := raw["installed"]; ok {
		b, _ := v.(bool)
		return b
	}
	return true
}

// liveOptionsOf extracts "options" - the add-on's current effective
// options - from one slug's raw live info entry. A nil entry, or one
// without the key, yields a nil map, which optionsDiff reads as "no live
// value for any key".
func liveOptionsOf(raw map[string]any) map[string]any {
	if raw == nil {
		return nil
	}
	m, _ := raw["options"].(map[string]any)
	return m
}

// hasNewKey reports whether params declares a key not yet in
// existingOriginals - the trigger for an update op with no value drift,
// so the applier records that key's original. Mirrors
// entities.hasNewField.
func hasNewKey(params, existingOriginals map[string]any) bool {
	for k := range params {
		if _, already := existingOriginals[k]; !already {
			return true
		}
	}
	return false
}

func errorOp(slug, msg string) registries.RegOp {
	return registries.RegOp{Kind: registries.KindError, RType: "addon", Key: slug, Params: map[string]any{}, Error: msg}
}

func adoptedNoChangeText(slug string) string {
	return fmt.Sprintf("now managing add-on '%s' options; no value changes needed", slug)
}

func restoreNoChangeText(slug string) string {
	return fmt.Sprintf("restoring original options for add-on '%s'; live values already match", slug)
}

// optionsDiff is a per-key "old -> new" comparison of params (the
// declared option keys, or a restore's recorded originals) against the
// matching keys of liveOptions, returning "" if nothing differs. Flat
// per-key rather than the unified diff other layers use: an option value
// is usually a bare scalar, not prose-like config text.
//
// An AbsentMarker param - reachable only from a restore, since a manifest
// cannot express "no value" - is compared by PRESENCE, because the change
// it describes is the key's removal, which ValuesEqual cannot see.
//
// declared is the manifest's still-unresolved options (nil for a
// restore); a key whose declared value holds a "secret://<name>" renders
// from that value with the live side masked.
func optionsDiff(params, liveOptions, declared map[string]any) string {
	changed := false
	keys := difftext.SortedKeys(params)
	lines := make([]string, 0, len(keys))
	for _, k := range keys {
		oldVal, liveHas := liveOptions[k]
		newVal := params[k]
		switch {
		case IsAbsent(newVal):
			if liveHas {
				changed = true
			}
		case !registries.ValuesEqual(oldVal, newVal):
			changed = true
		}
		// A value declared as a reference must not print what it resolved to.
		if declaredVal, isRef := secretref.RefAt(declared, k); isRef {
			lines = append(lines, fmt.Sprintf("%s: %s -> %s", k, secretHiddenRepr, reprValue(declaredVal)))
			continue
		}
		lines = append(lines, fmt.Sprintf("%s: %s -> %s", k, reprValue(oldVal), reprValue(newVal)))
	}
	if !changed {
		return ""
	}
	return strings.Join(lines, "\n")
}

// secretHiddenRepr stands in a plan line for a value that must not be
// shown, next to reprValue's "None" and absentRepr's "(unset)".
const secretHiddenRepr = "(hidden)"

// reprValue renders an option value for a plan line in internal/difftext's
// Python-repr style, with AbsentMarker as "(unset)" and deliberately not
// "None": Supervisor treats a missing key and a null as different requests
// and rejects the second.
func reprValue(v any) string {
	return difftext.ReprValueWithSentinel(v, absentSentinel)
}

// absentSentinel is reprValue's AbsentMarker hook, package-level rather
// than a per-call closure so a many-key plan allocates none.
func absentSentinel(v any) (string, bool) {
	if IsAbsent(v) {
		return absentRepr, true
	}
	return "", false
}
