package applier

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/fsx"
)

// State is the agent's persisted sync state, loaded from and saved to
// cfg.StatePath. LastGoodSHA and LastApplyUTC use "" for "unset";
// RegistryManaged is the "<rtype>:<manifest id>" -> live id mapping for the
// registry layer (see internal/registries/internal/regapply).
type State struct {
	LastGoodSHA     string
	Manifest        []string
	LastApplyUTC    string
	RegistryManaged map[string]string
	// EntityOriginals is internal/entities' "entity:<entity_id>" -> {field:
	// original value} snapshot, restored when the entity leaves
	// gitops/entities.yaml. A manifest entity_id already IS the live id, so
	// presence of a key doubles as "currently managed".
	EntityOriginals map[string]map[string]any
	// DashboardManaged is internal/dashboards' "<rtype>:<manifest id>" ->
	// live id map, kept SEPARATE from RegistryManaged because
	// registries.Plan treats any other prefix there as a helper domain and
	// would plan config/<domain>/* deletes for a "dashboard:" entry, where
	// dashboards use the lovelace/dashboards/* commands instead.
	DashboardManaged map[string]string
	// AddonOriginals is internal/addonopts' "addon:<slug>" -> {option key:
	// original value} map, shaped and used exactly like EntityOriginals; a
	// manifest slug already IS the live Supervisor id.
	AddonOriginals map[string]map[string]any
	// AddonRestartOnChange is internal/addonopts' "addon:<slug>" ->
	// restart_on_change, kept separate because it is a per-management
	// setting rather than one of the option KEYS AddonOriginals snapshots.
	// Once a slug is undeclared it is the only record of whether an
	// un-manage restore should restart the add-on afterward.
	AddonRestartOnChange map[string]bool
	// IntegrationManaged is internal/flows' "integration:<manifest id>" ->
	// live entry_id map, separate for DashboardManaged's reason.
	IntegrationManaged map[string]string
	// IntegrationHashes is internal/flows' "integration:<manifest id>" ->
	// sha256 of the declared "data", taken when the integration comes under
	// management and compared every later cycle: config-entry data cannot
	// be updated once a flow completes, so a mismatch surfaces as a
	// per-item error op. A hash, not the values - flow data commonly
	// carries credentials and this file is what a user shares for support.
	IntegrationHashes map[string]string
	// IntegrationData is internal/flows' "integration:<manifest id>" -> the
	// declared "data" map itself, snapshotted alongside IntegrationHashes
	// so a rollback can replay a deleted integration's flow: a hash is
	// one-way, and by delete time the manifest no longer declares the item
	// (internal/regapply's flows.go is the only consumer).
	//
	// Stored as WRITTEN, never as a plan resolved it: a "secret://<name>"
	// value is persisted as that reference and resolved again at replay
	// (see internal/secretref, regapply's invertFlowOp), so only what the
	// manifest spells out in the clear lands here.
	IntegrationData map[string]map[string]any
	// IntegrationAttempts is internal/flows' "integration:<manifest id>" ->
	// {"hash": sha256 of the data that failed, "error": why}, written when
	// a CREATE fails (regapply's flows.go) and read by flows.Plan to avoid
	// retrying the identical failure every cycle. Mutually exclusive with
	// IntegrationManaged for the same id, since a success clears it. Shaped
	// like EntityOriginals only to reuse sanitizeFieldOriginals.
	IntegrationAttempts map[string]map[string]any
	// SubentryManaged is internal/subentries' "subentry:<manifest id>" ->
	// live SUBENTRY_ID map. Deliberately not the parent entry_id: a
	// subentry cannot move between config entries, so its parent is always
	// recoverable from where it lives (see subentries.Plan).
	SubentryManaged map[string]string
	// SubentryHashes is internal/subentries' "subentry:<manifest id>" ->
	// sha256 of the data last successfully applied. Unlike
	// IntegrationHashes a mismatch is actionable: a subentry supports a
	// reconfigure flow, so the layer re-submits the declared data. A hash
	// rather than the values, for IntegrationHashes' reason.
	SubentryHashes map[string]string
	// SubentryAttempts is internal/subentries' "subentry:<manifest id>" ->
	// {"hash": sha256 of the data that failed, "error": why}, written when
	// a create or reconfigure fails and read at plan time to stop the
	// identical retry every cycle (see subentries.Plan's "Failure memory").
	// There is deliberately no SubentryData counterpart: this layer never
	// deletes a subentry, so there is no flow to replay on rollback.
	SubentryAttempts map[string]map[string]any
	// HacsManaged is internal/hacs' "hacs:<manifest id>" -> HACS REPOSITORY
	// ID map (HACS's own numeric string, not the GitHub owner/name). Unlike
	// every other ownership map it grants no power to remove: that layer
	// never uninstalls, so an entry dropped from gitops/hacs.yaml leaves
	// its record standing.
	HacsManaged map[string]string
	// HacsAttempts is internal/hacs' "hacs:<manifest id>" -> {"hash":
	// sha256 of the declared entry that failed, "error": why}, written when
	// a DOWNLOAD fails and read at plan time (see hacs.Plan's "Failure
	// memory"). The hash covers the whole declared entry, which has no data
	// payload, and is kept only so an edited entry retries on its own.
	HacsAttempts map[string]map[string]any
	// HacsRestartPending is the downloaded integration DOMAINS the running
	// Home Assistant has not loaded yet - a custom component is imported at
	// startup, so a download does nothing until a restart. Sorted and
	// deduplicated, rendered verbatim on the dashboard. Entries leave only
	// when a cycle sees the domain loaded (hacs.PruneRestartPending).
	HacsRestartPending []string
	// LastDriftBranch is the branch internal/gitsync.CommitBack last pushed
	// (see internal/recon's commitDriftBack), shown in the web UI so a user
	// can open a PR; "" if commit-back has never run.
	LastDriftBranch string
	// LastDriftBackHash is driftSetHash's fingerprint of the change set the
	// last commit-back captured. internal/recon skips committing again
	// while a fresh hash matches, so standing drift does not push a new
	// throwaway branch on every poll interval.
	LastDriftBackHash string
	// LastImportSHA/LastImportUTC record the last successful import (see
	// internal/recon's importLive). Display only: an import deliberately
	// does NOT add its paths to Manifest, since ownership for deletion is
	// earned by an apply writing a file, never by copying one in.
	LastImportSHA string
	LastImportUTC string
}

// StateLoad loads the agent's persisted sync state from cfg.StatePath,
// returning defaults - never an error - when the file is missing or does
// not parse. Entries guardChangePath would reject, or of the wrong shape,
// are dropped one by one and logged rather than resetting the whole field,
// so a poisoned state.json cannot become an unsafe delete or a Plan panic.
func StateLoad(cfg Config) State {
	defaults := State{
		Manifest: []string{}, RegistryManaged: map[string]string{},
		EntityOriginals: map[string]map[string]any{}, DashboardManaged: map[string]string{},
		AddonOriginals: map[string]map[string]any{}, AddonRestartOnChange: map[string]bool{},
		IntegrationManaged: map[string]string{}, IntegrationHashes: map[string]string{},
		IntegrationData: map[string]map[string]any{}, IntegrationAttempts: map[string]map[string]any{},
		SubentryManaged: map[string]string{}, SubentryHashes: map[string]string{},
		SubentryAttempts: map[string]map[string]any{},
		HacsManaged:      map[string]string{}, HacsAttempts: map[string]map[string]any{},
		HacsRestartPending: []string{},
	}

	data, err := os.ReadFile(cfg.StatePath) // #nosec G304 -- cfg.StatePath is the fixed Supervisor-managed state path in production
	if err != nil {
		return defaults
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return defaults
	}

	state := defaults
	if v, ok := raw["last_good_sha"]; ok {
		state.LastGoodSHA = asString(v)
	}
	if v, ok := raw["last_apply_utc"]; ok {
		state.LastApplyUTC = asString(v)
	}
	state.Manifest = sanitizeManifest(cfg, raw["manifest"])
	state.RegistryManaged = sanitizeManagedMap(raw["registry_managed"], "registry_managed")
	state.EntityOriginals = sanitizeFieldOriginals(raw["entity_originals"], "entity_originals")
	state.DashboardManaged = sanitizeManagedMap(raw["dashboard_managed"], "dashboard_managed")
	state.AddonOriginals = sanitizeFieldOriginals(raw["addon_originals"], "addon_originals")
	state.AddonRestartOnChange = sanitizeBoolMap(raw["addon_restart_on_change"], "addon_restart_on_change")
	state.IntegrationManaged = sanitizeManagedMap(raw["integration_managed"], "integration_managed")
	state.IntegrationHashes = sanitizeManagedMap(raw["integration_hashes"], "integration_hashes")
	state.IntegrationData = sanitizeFieldOriginals(raw["integration_data"], "integration_data")
	state.IntegrationAttempts = sanitizeFieldOriginals(raw["integration_attempts"], "integration_attempts")
	state.SubentryManaged = sanitizeManagedMap(raw["subentry_managed"], "subentry_managed")
	state.SubentryHashes = sanitizeManagedMap(raw["subentry_hashes"], "subentry_hashes")
	state.SubentryAttempts = sanitizeFieldOriginals(raw["subentry_attempts"], "subentry_attempts")
	state.HacsManaged = sanitizeManagedMap(raw["hacs_managed"], "hacs_managed")
	state.HacsAttempts = sanitizeFieldOriginals(raw["hacs_attempts"], "hacs_attempts")
	state.HacsRestartPending = sanitizeStringList(raw["hacs_restart_pending"], "hacs_restart_pending")
	if v, ok := raw["last_drift_branch"]; ok {
		state.LastDriftBranch = asString(v)
	}
	if v, ok := raw["last_drift_back_hash"]; ok {
		state.LastDriftBackHash = asString(v)
	}
	if v, ok := raw["last_import_sha"]; ok {
		state.LastImportSHA = asString(v)
	}
	if v, ok := raw["last_import_utc"]; ok {
		state.LastImportUTC = asString(v)
	}
	return state
}

// asString best-effort converts value to a string, "" for nil. Mirrors
// options.asString: a non-string, non-nil value renders with Go's default
// formatting rather than being dropped.
func asString(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}

// sanitizeManifest drops any entry guardChangePath would reject, so a
// hand-edited state.json (say "../outside.txt") can never resurface as a
// delete change reaching outside cfg.ConfigRoot.
func sanitizeManifest(cfg Config, raw any) []string {
	list, ok := raw.([]any)
	if !ok {
		return []string{}
	}

	configRootReal := fsx.Realpath(cfg.ConfigRoot)
	clean := []string{}
	for _, entryRaw := range list {
		entry, ok := entryRaw.(string)
		if !ok || entry == "" {
			continue
		}
		if err := guardChangePath(cfg, entry, configRootReal); err != nil {
			slog.Warn("applier: state_load dropping unsafe manifest entry", "entry", entry)
			continue
		}
		clean = append(clean, entry)
	}
	return clean
}

// sanitizeManagedMap reduces a corrupt "<rtype>:<manifest id>" -> live id
// mapping to a well-shaped map[string]string, so a malformed value is a
// dropped entry rather than a panic out of a Plan expecting string->string.
// field names the log messages after the raw JSON key.
func sanitizeManagedMap(raw any, field string) map[string]string {
	m, ok := raw.(map[string]any)
	if !ok {
		if raw != nil {
			slog.Warn("applier: state_load "+field+" is not a mapping, resetting", "value", raw)
		}
		return map[string]string{}
	}
	clean := map[string]string{}
	for k, v := range m {
		if s, ok := v.(string); ok {
			clean[k] = s
		} else {
			slog.Warn("applier: state_load dropping malformed "+field+" entry", "key", k, "value", v)
		}
	}
	return clean
}

// sanitizeFieldOriginals is sanitizeManagedMap for a per-field originals
// mapping, reducing it to map[string]map[string]any so a malformed value is
// a dropped entry rather than a panic out of entities.Plan, addonopts.Plan
// or their regapply appliers, all of which expect this shape.
func sanitizeFieldOriginals(raw any, field string) map[string]map[string]any {
	m, ok := raw.(map[string]any)
	if !ok {
		if raw != nil {
			slog.Warn("applier: state_load "+field+" is not a mapping, resetting", "value", raw)
		}
		return map[string]map[string]any{}
	}
	clean := map[string]map[string]any{}
	for k, v := range m {
		fields, ok := v.(map[string]any)
		if !ok {
			slog.Warn("applier: state_load dropping malformed "+field+" entry", "key", k, "value", v)
			continue
		}
		clean[k] = fields
	}
	return clean
}

// sanitizeBoolMap mirrors sanitizeManagedMap for the one bool-shaped field
// (see AddonRestartOnChange).
func sanitizeBoolMap(raw any, field string) map[string]bool {
	m, ok := raw.(map[string]any)
	if !ok {
		if raw != nil {
			slog.Warn("applier: state_load "+field+" is not a mapping, resetting", "value", raw)
		}
		return map[string]bool{}
	}
	clean := map[string]bool{}
	for k, v := range m {
		if b, ok := v.(bool); ok {
			clean[k] = b
		} else {
			slog.Warn("applier: state_load dropping malformed "+field+" entry", "key", k, "value", v)
		}
	}
	return clean
}

// sanitizeStringList reduces a corrupt list of names to a well-shaped
// []string, sorted and deduplicated. Sorted HERE rather than at each
// reader: it renders into a polled dashboard fragment compared byte for
// byte, which a hand-edited state.json must not change on every render.
func sanitizeStringList(raw any, field string) []string {
	list, ok := raw.([]any)
	if !ok {
		if raw != nil {
			slog.Warn("applier: state_load "+field+" is not a list, resetting", "value", raw)
		}
		return []string{}
	}
	seen := map[string]bool{}
	clean := []string{}
	for _, entryRaw := range list {
		entry, ok := entryRaw.(string)
		if !ok || entry == "" {
			slog.Warn("applier: state_load dropping malformed "+field+" entry", "value", entryRaw)
			continue
		}
		if seen[entry] {
			continue
		}
		seen[entry] = true
		clean = append(clean, entry)
	}
	sort.Strings(clean)
	return clean
}

// StateSave persists state to cfg.StatePath, overwriting it, after every
// successful apply. Writes a .tmp file then renames, so a crash mid-write
// cannot leave a truncated state.json.
func StateSave(cfg Config, state State) error {
	if parent := filepath.Dir(cfg.StatePath); parent != "." {
		if err := os.MkdirAll(parent, 0o755); err != nil { // #nosec G301 -- 0755 deliberate: Home Assistant's own process must traverse/read these dirs
			return err
		}
	}

	manifest := state.Manifest
	if manifest == nil {
		manifest = []string{}
	}
	registryManaged := state.RegistryManaged
	if registryManaged == nil {
		registryManaged = map[string]string{}
	}
	entityOriginals := state.EntityOriginals
	if entityOriginals == nil {
		entityOriginals = map[string]map[string]any{}
	}
	dashboardManaged := state.DashboardManaged
	if dashboardManaged == nil {
		dashboardManaged = map[string]string{}
	}
	addonOriginals := state.AddonOriginals
	if addonOriginals == nil {
		addonOriginals = map[string]map[string]any{}
	}
	addonRestartOnChange := state.AddonRestartOnChange
	if addonRestartOnChange == nil {
		addonRestartOnChange = map[string]bool{}
	}
	integrationManaged := state.IntegrationManaged
	if integrationManaged == nil {
		integrationManaged = map[string]string{}
	}
	integrationHashes := state.IntegrationHashes
	if integrationHashes == nil {
		integrationHashes = map[string]string{}
	}
	integrationData := state.IntegrationData
	if integrationData == nil {
		integrationData = map[string]map[string]any{}
	}
	integrationAttempts := state.IntegrationAttempts
	if integrationAttempts == nil {
		integrationAttempts = map[string]map[string]any{}
	}
	subentryManaged := state.SubentryManaged
	if subentryManaged == nil {
		subentryManaged = map[string]string{}
	}
	subentryHashes := state.SubentryHashes
	if subentryHashes == nil {
		subentryHashes = map[string]string{}
	}
	subentryAttempts := state.SubentryAttempts
	if subentryAttempts == nil {
		subentryAttempts = map[string]map[string]any{}
	}
	hacsManaged := state.HacsManaged
	if hacsManaged == nil {
		hacsManaged = map[string]string{}
	}
	hacsAttempts := state.HacsAttempts
	if hacsAttempts == nil {
		hacsAttempts = map[string]map[string]any{}
	}
	// Sorted on the way out as well as in, so the file never depends on a
	// writer having remembered to keep it sorted.
	hacsRestartPending := append([]string{}, state.HacsRestartPending...)
	sort.Strings(hacsRestartPending)

	payload := map[string]any{
		"last_good_sha":           nullableString(state.LastGoodSHA),
		"manifest":                manifest,
		"last_apply_utc":          nullableString(state.LastApplyUTC),
		"registry_managed":        registryManaged,
		"entity_originals":        entityOriginals,
		"dashboard_managed":       dashboardManaged,
		"addon_originals":         addonOriginals,
		"addon_restart_on_change": addonRestartOnChange,
		"integration_managed":     integrationManaged,
		"integration_hashes":      integrationHashes,
		"integration_data":        integrationData,
		"integration_attempts":    integrationAttempts,
		"subentry_managed":        subentryManaged,
		"subentry_hashes":         subentryHashes,
		"subentry_attempts":       subentryAttempts,
		"hacs_managed":            hacsManaged,
		"hacs_attempts":           hacsAttempts,
		"hacs_restart_pending":    hacsRestartPending,
		"last_drift_branch":       nullableString(state.LastDriftBranch),
		"last_drift_back_hash":    nullableString(state.LastDriftBackHash),
		"last_import_sha":         nullableString(state.LastImportSHA),
		"last_import_utc":         nullableString(state.LastImportUTC),
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	tmpPath := cfg.StatePath + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o644); err != nil { // #nosec G306 -- add-on-private state, not secret
		return err
	}
	return os.Rename(tmpPath, cfg.StatePath)
}

func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
