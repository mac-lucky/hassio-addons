// Package hacs computes a reconciliation plan for HACS-distributed custom
// integrations against gitops/hacs.yaml, in the same registries.RegOp
// shape every other layer emits. It only ever plans; the HACS WebSocket
// commands are sent by internal/regapply's hacs.go.
//
// Install and adopt, nothing else. A declared repository HACS does not
// report as installed is downloaded, an installed one is adopted, and an
// item removed from the manifest is just stopped being followed. There is
// no uninstall path at all - removing a custom component takes its
// entities, config entries and history with it - so emptying or deleting
// gitops/hacs.yaml is safe here, unlike gitops/integrations.yaml. To
// remove one, use the HACS panel.
//
// v1 accepts only category "integration": it is the only category whose
// installed state this layer can verify the same way twice (an integration
// has a domain, and Home Assistant reports which domains it loaded).
// Anything else is a per-item validation error.
//
//	hacs:
//	  - id: anker_solix                       # manifest key; required, [a-z0-9_]+, unique
//	    repository: thomluther/ha-anker-solix # owner/name; a full https URL is accepted and normalized
//	    category: integration                 # required; v1 accepts only "integration"
//	    version: "3.1.0"                      # optional; used AT INSTALL TIME only
//
// version pins what a fresh download asks for and is deliberately NOT
// reconciled afterwards: nothing at all is planned for an already-
// installed repository, so editing it later neither upgrades nor
// downgrades. Upgrading is HACS's own job.
//
// Adopting an installed repository contacts nothing - it only records
// "this manifest id owns that HACS repository id" in state.HacsManaged -
// but is still emitted as a KindUpdate op, because state.json is only
// written by an apply and a plan-time write would record ownership on a
// dry-run cycle.
//
// A freshly downloaded integration is on disk but NOT loaded: Home
// Assistant imports custom_components at startup, so its domain appears
// only after a restart, or when its first config entry forces the import.
// Tracked in state.HacsRestartPending (see PruneRestartPending); nothing
// here ever restarts Home Assistant.
//
// No field of this manifest carries a data payload, so unlike
// internal/flows, subentries and addonopts there is no "secret://<name>"
// resolution here and no RegOp.Secrets to scrub.
package hacs

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/failmemory"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/registries"
	yaml "go.yaml.in/yaml/v3"
)

// idPattern is the manifest id syntax - the same [a-z0-9_]+ every other
// gitops/ manifest in this add-on uses.
var idPattern = regexp.MustCompile(`^[a-z0-9_]+$`)

// repoPattern is a GitHub "owner/name" after normalization, as strict as
// GitHub itself: an OWNER may carry only letters, digits and hyphens, a
// repository NAME also underscores and dots. Worth being strict - HACS's
// own add handler simply RETURNS on a string its regex refuses, with no
// reply and no error, costing a ten-second timeout and a remembered
// failure instead of a readable manifest error on the first cycle.
var repoPattern = regexp.MustCompile(`^[A-Za-z0-9-]+/[A-Za-z0-9_.-]+$`)

// CategoryIntegration is the one HACS category this layer accepts.
const CategoryIntegration = "integration"

// allowedFields are the only per-item fields gitops/hacs.yaml may declare
// besides id. Anything else is a validation error.
var allowedFields = map[string]bool{
	"repository": true, "category": true, "version": true,
}

// Kind values hacs.Plan's ops carry. Note the absence of KindDelete: this
// layer never uninstalls anything.
const (
	KindCreate = registries.KindCreate
	KindUpdate = registries.KindUpdate
	KindError  = registries.KindError
)

// KeyPrefix namespaces this layer's keys inside the shared state maps, as
// internal/flows' "integration:" does. Exported: regapply writes keys with it.
const KeyPrefix = "hacs:"

// rtype is the RegOp.RType every op this layer plans carries. Unexported:
// internal/recon keys its layer split off a constant of its own (rtypeHacs).
const rtype = "hacs"

// ManifestError is returned when gitops/hacs.yaml fails to parse or
// validate. Error() aggregates every problem found, not just the first.
type ManifestError struct {
	Problems []string
}

func (e *ManifestError) Error() string {
	return strings.Join(e.Problems, "; ")
}

// Desired is the parsed, validated contents of gitops/hacs.yaml: one map
// per declared item, in manifest order, keyed "id", "repository"
// (normalized to owner/name) and "category", plus "version" when declared.
type Desired struct {
	Repos []map[string]any
}

func emptyDesired() Desired { return Desired{Repos: []map[string]any{}} }

// LoadManifest loads and validates <workdir>/gitops/hacs.yaml. A missing
// file yields an empty Desired, not an error, and plans nothing: unlike
// gitops/integrations.yaml, an absent file here never means "delete".
func LoadManifest(workdir string) (Desired, error) {
	path := filepath.Join(workdir, "gitops", "hacs.yaml")
	info, statErr := os.Stat(path)
	if statErr != nil || !info.Mode().IsRegular() {
		return emptyDesired(), nil
	}

	data, err := os.ReadFile(path) // #nosec G304 -- path is workdir-relative, constructed by this package only
	if err != nil {
		return Desired{}, &ManifestError{Problems: []string{fmt.Sprintf("hacs.yaml: could not read file: %v", err)}}
	}

	var parsed any
	if err := yaml.Unmarshal(data, &parsed); err != nil {
		return Desired{}, &ManifestError{Problems: []string{fmt.Sprintf("hacs.yaml: invalid YAML: %v", err)}}
	}
	if parsed == nil {
		return emptyDesired(), nil
	}
	obj, ok := parsed.(map[string]any)
	if !ok {
		return Desired{}, &ManifestError{Problems: []string{"hacs.yaml: top level must be a mapping"}}
	}

	itemsRaw, present := obj["hacs"]
	if !present || itemsRaw == nil {
		return emptyDesired(), nil
	}
	items, ok := itemsRaw.([]any)
	if !ok {
		return Desired{}, &ManifestError{Problems: []string{"hacs.yaml: hacs must be a list"}}
	}

	var errs []string
	seen := map[string]bool{}
	// Which id already declared each normalized repository. Two ids for one
	// repository would both plan an install and whichever ran LAST would
	// decide the version, so the manifest's meaning would depend on line
	// order. Refused, naming both.
	seenRepos := map[string]string{}
	result := []map[string]any{}

	for idx, rawItem := range items {
		itemMap, ok := rawItem.(map[string]any)
		if !ok {
			errs = append(errs, fmt.Sprintf("hacs.yaml: hacs[%d] is not a mapping", idx))
			continue
		}

		id, idIsString := itemMap["id"].(string)
		if !idIsString || id == "" || !idPattern.MatchString(id) {
			errs = append(errs, fmt.Sprintf("hacs.yaml: hacs[%d] has an invalid or missing 'id'", idx))
			continue
		}
		if seen[id] {
			errs = append(errs, fmt.Sprintf("hacs.yaml: duplicate hacs id '%s'", id))
			continue
		}

		item, itemErrs := validateItemFields(id, itemMap)
		if len(itemErrs) > 0 {
			errs = append(errs, itemErrs...)
			continue
		}

		// Lowercased like every other full_name comparison here: GitHub
		// treats owner/name case-insensitively, so two spellings are one repo.
		repository, _ := item["repository"].(string)
		repoKey := strings.ToLower(repository)
		if first, declared := seenRepos[repoKey]; declared {
			errs = append(errs, fmt.Sprintf(
				"hacs.yaml: hacs entries '%s' and '%s' both declare repository '%s'; "+
					"one repository can only be declared once", first, id, repository))
			continue
		}

		seen[id] = true
		seenRepos[repoKey] = id
		result = append(result, item)
	}

	if len(errs) > 0 {
		return Desired{}, &ManifestError{Problems: errs}
	}
	return Desired{Repos: result}, nil
}

// validateItemFields validates one item's fields besides id: repository is
// a required owner/name (or a GitHub URL normalizing to one), category
// must be CategoryIntegration, version is optional but a QUOTED string,
// and anything outside allowedFields is an "unsupported field" error.
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
			"hacs.yaml: hacs entry '%s' has unsupported field(s) %s", id, strings.Join(unknown, ", ")))
	}

	repository, isString := item["repository"].(string)
	switch {
	case !isString || repository == "":
		errs = append(errs, fmt.Sprintf("hacs.yaml: hacs entry '%s' has an invalid or missing 'repository'", id))
	default:
		normalized, ok := NormalizeRepository(repository)
		if !ok {
			errs = append(errs, fmt.Sprintf(
				"hacs.yaml: hacs entry '%s' repository '%s' is not a github owner/name (or a https://github.com/owner/name URL)",
				id, repository))
		} else {
			item["repository"] = normalized
		}
	}

	category, isString := item["category"].(string)
	switch {
	case !isString || category == "":
		errs = append(errs, fmt.Sprintf("hacs.yaml: hacs entry '%s' has an invalid or missing 'category'", id))
	case category != CategoryIntegration:
		errs = append(errs, fmt.Sprintf(
			"hacs.yaml: hacs entry '%s' declares category '%s'; only '%s' is supported",
			id, category, CategoryIntegration))
	}

	// A quoted string, and the error says so: YAML reads an unquoted 3.10 as
	// the float 3.1, asking HACS for a release tag that does not exist.
	if versionRaw, present := item["version"]; present {
		if version, ok := versionRaw.(string); !ok || version == "" {
			errs = append(errs, fmt.Sprintf(
				"hacs.yaml: hacs entry '%s' version must be a non-empty quoted string (write \"3.1.0\", not 3.1.0)", id))
		}
	}

	if len(errs) > 0 {
		return nil, errs
	}
	return item, nil
}

// NormalizeRepository reduces a declared repository to the "owner/name"
// HACS stores in full_name, also accepting a full GitHub URL, and reports
// false for anything else. Done locally as well as by HACS's own add
// command because this string is what every LOCAL comparison (live
// full_name, ownership record) is made against.
func NormalizeRepository(declared string) (string, bool) {
	repo := strings.TrimSpace(declared)
	for _, prefix := range []string{"https://github.com/", "http://github.com/", "github.com/"} {
		if rest, found := strings.CutPrefix(repo, prefix); found {
			repo = rest
			break
		}
	}
	repo = strings.TrimSuffix(strings.Trim(repo, "/"), ".git")
	if !repoPattern.MatchString(repo) {
		return "", false
	}
	// A dot is legal in a repository NAME, but a segment of "." or ".." is
	// the shape that walks out of a directory, and resolves nowhere.
	for _, segment := range strings.Split(repo, "/") {
		if segment == "." || segment == ".." {
			return "", false
		}
	}
	return repo, true
}

// hashEntry fingerprints one declared manifest entry, to decide whether a
// recorded failure still describes the CURRENT declaration (see Plan's
// failure memory). Every field goes in, version included: bumping a pin is
// exactly the edit that should unblock a download that failed at the
// previous one. Unexported - regapply reads the hash off Params["hash"].
func hashEntry(item map[string]any) string {
	return failmemory.Hash(fingerprint(item))
}

// fingerprint is the map hashEntry digests, kept separate so Refusal can
// compare the stored hash against the same fields in either hash form.
func fingerprint(item map[string]any) map[string]any {
	return map[string]any{
		"id":         asString(item["id"]),
		"repository": asString(item["repository"]),
		"category":   asString(item["category"]),
		"version":    asString(item["version"]),
	}
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}

// Plan computes the HACS operations needed to reconcile the live HACS
// repository list toward desired, given the ownership mapping and the
// memory of the last failed attempt per key.
//
// liveRepos is every repository HACS knows in the "integration" category,
// as regapply.FetchHacsLive returns them: "id" (HACS's own repository id,
// a string), "full_name", "installed", "category" and, for an integration,
// "domain". managed is state.HacsManaged ("hacs:<manifest id>" ->
// repository id); attempts is state.HacsAttempts ("hacs:<manifest id>" ->
// {"hash", "error"}).
//
// # Rules
//
//  1. Declared repository installed (matched by full_name, case-
//     insensitively - a manifest in the wrong case must not install a
//     second copy) -> nothing if managed already records that exact
//     repository id, otherwise ADOPT it (KindUpdate, bookkeeping only).
//     An installed repository is never touched again, whatever version.
//  2. Known to HACS but not installed -> INSTALL (KindCreate) by its id.
//  3. Unknown to HACS -> INSTALL with no id; the driver adds it as a
//     custom repository first, then downloads it.
//  4. A managed key the manifest no longer declares -> nothing, not even
//     an unmanage op: the record is the only trace that this agent
//     installed it, and clearing it would discard that.
//  5. A managed key whose declared repository now resolves to a DIFFERENT
//     live repository -> a per-item error op, checked before every rule
//     above. Rebinding an id installs the new repository and drops the
//     record of the old, which on a layer that never uninstalls leaves
//     the first one on the box unowned.
//
// Failure memory: while attempts holds an entry for this key at the
// CURRENT hash, rules 2 and 3 emit an error op instead of retrying the
// download every cycle forever. internal/regapply writes it on a failed
// install and clears it on success; editing the entry moves the hash and
// retries on its own, and the dashboard's Retry button clears the record.
// Adoption is never blocked - it sends nothing, so it cannot have failed.
func Plan(
	desired Desired, liveRepos []map[string]any,
	managed map[string]string, attempts map[string]map[string]any,
) []registries.RegOp {
	if managed == nil {
		managed = map[string]string{}
	}
	if attempts == nil {
		attempts = map[string]map[string]any{}
	}

	byFullName := indexByFullName(liveRepos)

	var ops []registries.RegOp
	for _, item := range desired.Repos {
		id, _ := item["id"].(string)
		repository, _ := item["repository"].(string)
		category, _ := item["category"].(string)
		version, _ := item["version"].(string)
		key := KeyPrefix + id

		live := byFullName[strings.ToLower(repository)]
		liveID, _ := live["id"].(string)
		installed, _ := live["installed"].(bool)
		domain, _ := live["domain"].(string)

		// Rule 5, before anything else acts on this item: the id already
		// owns a DIFFERENT repository, and rebinding it would leave that
		// one on the box unowned.
		if ownedID, isManaged := managed[key]; isManaged && ownedID != "" && liveID != "" && ownedID != liveID {
			ops = append(ops, errorOp(id, fmt.Sprintf(
				"hacs entry '%s' already manages HACS repository %s, but now declares '%s' (repository %s); "+
					"declare the new repository under a new id, or remove this entry first - "+
					"rebinding an id would leave the repository it already installed with no owner",
				id, ownedID, repository, liveID)))
			continue
		}

		if installed {
			if managed[key] == liveID && liveID != "" {
				continue
			}
			if liveID == "" {
				ops = append(ops, errorOp(id, fmt.Sprintf(
					"HACS reports '%s' as installed but gave it no usable repository id", repository)))
				continue
			}
			ops = append(ops, registries.RegOp{
				Kind: KindUpdate, RType: rtype, Key: id,
				Params: map[string]any{
					"adopt": true, "repository": repository, "repository_id": liveID,
				},
				LiveID:   liveID,
				DiffText: adoptText(id, repository, liveID, domain),
			})
			continue
		}

		// Below the branches above, not before them: an adopt is never
		// blocked by failure memory and an installed repository plans
		// nothing, so neither has any use for a fingerprint.
		fp := fingerprint(item)
		if refusal, blocked := failmemory.Refusal(attempts, key, fp); blocked {
			ops = append(ops, errorOp(id, refusal))
			continue
		}

		params := map[string]any{
			"repository": repository, "category": category, "hash": failmemory.Hash(fp),
			// Empty when HACS has never heard of this repository - the driver
			// reads that as "add it as a custom repository first".
			"repository_id": liveID,
		}
		if version != "" {
			params["version"] = version
		}
		if domain != "" {
			// Only a hint: HACS knows a domain for a repository it already
			// fetched, and the driver re-reads it after the download anyway.
			params["domain"] = domain
		}
		ops = append(ops, registries.RegOp{
			Kind: KindCreate, RType: rtype, Key: id,
			Params:   params,
			DiffText: installText(repository, category, version, liveID),
		})
	}

	return ops
}

// PruneRestartPending drops every domain in pending that Home Assistant
// now reports as a loaded component, sorted and deduplicated because the
// result is persisted in state.json and rendered into a dashboard fragment
// compared byte for byte.
//
// internal/regapply ADDS a domain through this same function, so the
// sort-and-dedupe invariant has one owner; that caller passes a nil
// components and only wants the normalization.
func PruneRestartPending(pending, components []string) []string {
	// No reminder stands, so nothing to compare: returning early is what
	// lets the caller skip READING the components at all. Always
	// []string{}, never nil, since it is persisted and rendered.
	if len(pending) == 0 {
		return []string{}
	}

	loaded := make(map[string]bool, len(components))
	for _, component := range components {
		loaded[component] = true
	}

	seen := map[string]bool{}
	out := make([]string, 0, len(pending))
	for _, domain := range pending {
		if domain == "" || loaded[domain] || seen[domain] {
			continue
		}
		seen[domain] = true
		out = append(out, domain)
	}
	sort.Strings(out)
	return out
}

// FindRepository returns the live HACS repository object carrying
// fullName, or nil - Plan's own rule 1 matching, exported so
// internal/regapply resolves a just-added repository the same way the
// planner would, never to a different HACS repository id.
func FindRepository(liveRepos []map[string]any, fullName string) map[string]any {
	return indexByFullName(liveRepos)[strings.ToLower(fullName)]
}

// IsAdopt reports whether op is this layer's bookkeeping-only op, which
// contacts nothing. Kept beside the planner that writes the key, since
// internal/recon reads it to decide a plan needs no stash directory.
// Per-layer on purpose: internal/flows' own adopt contacts nothing either,
// yet MUST be stashed, because it writes ownership a rollback takes back.
func IsAdopt(op registries.RegOp) bool {
	adopt, _ := op.Params["adopt"].(bool)
	return adopt
}

// indexByFullName indexes the live HACS repository list by LOWERCASED
// full_name (Plan's rule 1). An object with no full_name is skipped rather
// than indexed under "", which a failed-to-normalize entry would match. A
// duplicate full_name - a custom repository added alongside a default one
// - keeps the INSTALLED one, so nothing on disk is ever re-installed.
func indexByFullName(liveRepos []map[string]any) map[string]map[string]any {
	index := make(map[string]map[string]any, len(liveRepos))
	for _, repo := range liveRepos {
		fullName, _ := repo["full_name"].(string)
		if fullName == "" {
			continue
		}
		lower := strings.ToLower(fullName)
		if existing, found := index[lower]; found {
			if installed, _ := existing["installed"].(bool); installed {
				continue
			}
		}
		index[lower] = repo
	}
	return index
}

func errorOp(id, msg string) registries.RegOp {
	return registries.RegOp{Kind: KindError, RType: rtype, Key: id, Params: map[string]any{}, Error: msg}
}

// installText describes a download for the pending-ops card. The version
// line always renders, naming the default when nothing is pinned.
func installText(repository, category, version, liveID string) string {
	if version == "" {
		version = "(latest release)"
	}
	known := "\n(HACS does not know this repository yet; it is added as a custom repository first)"
	if liveID != "" {
		known = ""
	}
	return fmt.Sprintf(
		"+repository: %s\n+category: %s\n+version: %s%s\n"+
			"(HACS will download this integration; Home Assistant has to restart before it loads)\n",
		repository, category, version, known)
}

// adoptText describes a bookkeeping-only adopt.
func adoptText(id, repository, liveID, domain string) string {
	suffix := ""
	if domain != "" {
		suffix = fmt.Sprintf(", domain '%s'", domain)
	}
	return fmt.Sprintf(
		"adopted installed HACS integration '%s' (%s, repository id %s%s); "+
			"nothing is downloaded and no version is changed\n",
		id, repository, liveID, suffix)
}
