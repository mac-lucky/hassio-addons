// Package options loads the add-on's user-configured options, which
// Supervisor writes to /data/options.json per config.yaml's schema. The
// single place that knows that file's shape; everything else takes an
// Options value.
package options

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
)

// Supervisor is the base URL for the Supervisor API, reachable from any
// add-on container.
const Supervisor = "http://supervisor"

// Options is a typed view of /data/options.json, mirroring config.yaml's
// options/schema block with the nested "reconcile" object flattened into
// Reconcile* fields.
type Options struct {
	RepoURL         string
	Branch          string
	GitUsername     string
	GitToken        string
	IntervalMinutes int
	DryRun          bool
	ApplyAfterPull  string // "reload" | "restart" | "off"
	// CommitBack pushes detected live drift to a throwaway
	// "gitops/drift-<timestamp>" branch instead of discarding it on the
	// next apply (recon.CommitDriftBack). Never touches opts.Branch.
	CommitBack bool
	// AllowImport enables the web UI's import buttons, which snapshot the
	// live config tree onto opts.Branch as one commit (recon.ImportLive).
	// The only operation that writes to the tracked branch, hence off by
	// default and never run on the interval.
	AllowImport bool
	// WebhookSecret, when non-empty, starts internal/hook's listener on
	// :8098, reconciling on a matching POST /webhook.
	WebhookSecret string
	// AgeKey is the "AGE-SECRET-KEY-1..." identity values are encrypted to
	// on their way into git (internal/sopscrypt). Empty means encryption
	// is off and secrets.yaml stays out of the repository.
	//
	// The most sensitive value the add-on holds: never logged, never
	// rendered by the web UI, never passed to a subprocess as an argument.
	// cmd/ha-gitops-agent hands it to sopscrypt.New and keeps no copy.
	AgeKey string
	// AutoUpdateAddons lists the installed add-ons this agent may update
	// once Supervisor reports an update for them; empty turns the whole
	// capability off. Entries are Supervisor slugs, not display names. The
	// agent's own slug is refused at runtime rather than dropped here, so
	// the refusal is visible instead of silent.
	AutoUpdateAddons []string
	// AutoUpdateIntervalMinutes is how often that check runs. Its own
	// cadence, not IntervalMinutes: an add-on version is not in the
	// repository, so the check is not part of a reconcile. Startup-only,
	// like IntervalMinutes.
	AutoUpdateIntervalMinutes int
	// TrackAddonVersions records every installed add-on's version in
	// gitops/addon-versions.yaml, rewriting it whenever one changes -
	// whoever changed it (recon.recordAddonVersions). A repository write,
	// not a change to the box, so it ignores DryRun as AllowImport and
	// CommitBack do, but it needs GitToken to have push rights.
	TrackAddonVersions bool
	// CaptureLiveChanges turns the file layer two-way. Each cycle classifies
	// every drifting path three-way against a merge base: one the repository
	// did not move is pushed to opts.Branch as a "capture" commit and kept
	// out of the apply, one live did not move is applied as always, and one
	// both moved is refused in either direction and parked (recon's
	// capture.go). Off by default, since it is the only setting under which
	// an unattended cycle rewrites the tracked branch with config.
	//
	// A repository write, not a change to the box, so it ignores DryRun as
	// AllowImport, CommitBack and TrackAddonVersions do - with DryRun on it
	// is arguably the safest mode there is, live edits flowing to git and
	// nothing ever flowing back. It needs GitToken to have push rights on
	// opts.Branch; a protected branch makes every cycle fail the push.
	//
	// Supersedes CommitBack's automatic half, which would otherwise push a
	// throwaway branch for the same drift this commits.
	CaptureLiveChanges bool

	ReconcileYAMLFiles  bool
	ReconcileRegistries bool
	ReconcileDashboards bool
	// ReconcileAddonOptions syncs other add-ons' options from
	// gitops/addons.yaml (internal/addonopts).
	ReconcileAddonOptions bool
	// ReconcileIntegrations syncs config-entry integrations from
	// gitops/integrations.yaml (internal/flows).
	ReconcileIntegrations bool
	// ReconcileSubentries syncs config-entry subentries from
	// gitops/subentries.yaml (internal/subentries). Independent of
	// ReconcileIntegrations: a subentry's parent entry may well be one a
	// human set up in the UI.
	ReconcileSubentries bool
	// ReconcileHacs installs the HACS-distributed custom integrations in
	// gitops/hacs.yaml (internal/hacs). Installs and adopts only - nothing
	// is uninstalled when the manifest stops declaring it. A cycle that
	// finds HACS missing SKIPS this layer and raises a health flag rather
	// than failing (recon.planHacsLayer).
	ReconcileHacs bool
}

// Fallback values used for any option missing from, or malformed in,
// options.json. Mirrors the options: defaults block in config.yaml.
const (
	defaultIntervalMinutes = 5
	defaultApplyAfterPull  = "reload"
	// Six hours, paced to Supervisor's own store refresh - checking faster
	// only re-reads the same cached numbers.
	defaultAutoUpdateIntervalMinutes = 360
)

var validApplyAfterPull = map[string]bool{
	"reload":  true,
	"restart": true,
	"off":     true,
}

// ErrMissingSupervisorToken is returned by SupervisorToken when
// SUPERVISOR_TOKEN is not set in the environment.
var ErrMissingSupervisorToken = errors.New("options: SUPERVISOR_TOKEN is not set in the environment")

// Load reads path into an Options. This is the add-on's boot path, so a
// missing or malformed key falls back to its config.yaml default rather
// than erroring; only path not existing is reported.
func Load(path string) (Options, error) {
	raw, err := readJSONObject(path)
	if err != nil {
		return Options{}, fmt.Errorf("options: %w", err)
	}

	reconcile, ok := raw["reconcile"].(map[string]any)
	if !ok {
		reconcile = map[string]any{}
	}

	applyAfterPull := asString(raw["apply_after_pull"], defaultApplyAfterPull)
	if !validApplyAfterPull[applyAfterPull] {
		applyAfterPull = defaultApplyAfterPull
	}

	intervalMinutes, ok := safeInt(raw["interval_minutes"])
	if !ok || intervalMinutes < 1 || intervalMinutes > 1440 {
		intervalMinutes = defaultIntervalMinutes
	}

	autoUpdateIntervalMinutes, ok := safeInt(raw["auto_update_interval_minutes"])
	if !ok || autoUpdateIntervalMinutes < 15 || autoUpdateIntervalMinutes > 10080 {
		autoUpdateIntervalMinutes = defaultAutoUpdateIntervalMinutes
	}

	branch := asString(raw["branch"], "main")
	if branch == "" {
		branch = "main"
	}

	return Options{
		RepoURL:                   asString(raw["repo_url"], ""),
		Branch:                    branch,
		GitUsername:               asString(raw["git_username"], ""),
		GitToken:                  asString(raw["git_token"], ""),
		IntervalMinutes:           intervalMinutes,
		DryRun:                    truthy(raw["dry_run"], true),
		ApplyAfterPull:            applyAfterPull,
		CommitBack:                truthy(raw["commit_back"], false),
		AllowImport:               truthy(raw["allow_import"], false),
		WebhookSecret:             asString(raw["webhook_secret"], ""),
		AgeKey:                    asString(raw["age_key"], ""),
		AutoUpdateAddons:          asStringSlice(raw["auto_update_addons"]),
		AutoUpdateIntervalMinutes: autoUpdateIntervalMinutes,
		TrackAddonVersions:        truthy(raw["track_addon_versions"], false),
		CaptureLiveChanges:        truthy(raw["capture_live_changes"], false),

		ReconcileYAMLFiles:    truthy(reconcile["yaml_files"], true),
		ReconcileRegistries:   truthy(reconcile["registries"], false),
		ReconcileDashboards:   truthy(reconcile["dashboards"], false),
		ReconcileAddonOptions: truthy(reconcile["addon_options"], false),
		ReconcileIntegrations: truthy(reconcile["integrations"], false),
		ReconcileSubentries:   truthy(reconcile["subentries"], false),
		ReconcileHacs:         truthy(reconcile["hacs"], false),
	}, nil
}

// SupervisorToken returns the Supervisor API bearer token from
// SUPERVISOR_TOKEN, which Supervisor injects when config.yaml sets
// hassio_api or homeassistant_api.
func SupervisorToken() (string, error) {
	token := os.Getenv("SUPERVISOR_TOKEN")
	if token == "" {
		return "", ErrMissingSupervisorToken
	}
	return token, nil
}

// readJSONObject reads and parses path as a JSON object. A missing file is
// an error wrapping os.ErrNotExist; invalid JSON, or JSON that is not an
// object, falls back to an empty object rather than crashing boot.
func readJSONObject(path string) (map[string]any, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- path is the fixed Supervisor options path in production; callers (chiefly tests) overriding it is by design, see Load's doc comment
	if err != nil {
		return nil, err
	}

	var parsed any
	if err := json.Unmarshal(data, &parsed); err != nil {
		return map[string]any{}, nil
	}

	obj, ok := parsed.(map[string]any)
	if !ok {
		return map[string]any{}, nil
	}
	return obj, nil
}

// asString converts value to a string, returning def for nil. Anything
// else non-string is rendered with Go's default formatting.
func asString(value any, def string) string {
	if value == nil {
		return def
	}
	if s, ok := value.(string); ok {
		return s
	}
	return fmt.Sprint(value)
}

// asStringSlice converts value to a list of strings; anything that is not
// a JSON array, and an array left with nothing usable, both yield nil.
//
// A non-string element is dropped rather than formatted, because these
// strings are identifiers matched against something real and "5" would
// only name something that cannot exist. Survivors are trimmed, empties
// dropped and duplicates removed, first occurrence kept.
func asStringSlice(value any) []string {
	items, ok := value.([]any)
	if !ok {
		return nil
	}

	var out []string
	seen := make(map[string]bool, len(items))
	for _, item := range items {
		s, ok := item.(string)
		if !ok {
			continue
		}
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// truthy converts value to a bool, returning def for nil. Present but
// malformed fields degrade rather than crash startup.
func truthy(value any, def bool) bool {
	switch v := value.(type) {
	case nil:
		return def
	case bool:
		return v
	case float64:
		return v != 0
	case string:
		return v != ""
	case map[string]any:
		return len(v) != 0
	case []any:
		return len(v) != 0
	default:
		return def
	}
}

// safeInt converts value to an int, reporting false for anything missing,
// not a number, or not int-shaped - a JSON bool included.
func safeInt(value any) (int, bool) {
	switch v := value.(type) {
	case bool:
		return 0, false
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return 0, false
		}
		return int(math.Trunc(v)), true
	case string:
		s := strings.TrimSpace(v)
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return 0, false
		}
		return int(n), true
	default:
		return 0, false
	}
}
