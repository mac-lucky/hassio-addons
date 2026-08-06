// Package applier applies a computed diff to the live config, with
// validation and rollback. The only package allowed to write into a config
// root (normally /homeassistant): every write is preceded by a backup and
// followed by a Supervisor config check, and a failed check rolls back
// everything that apply touched.
package applier

import (
	"time"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/options"
)

// ConfigRoot is the live Home Assistant config directory, mounted
// read-write via config.yaml's homeassistant_config map entry. Declared
// here and used everywhere through this name (internal/recon's ConfigRoot
// aliases it) so the diffed path and the applied-into path cannot drift.
const ConfigRoot = "/homeassistant"

// Config bundles Apply's tunables: filesystem roots, HTTP timeouts, and the
// path-exclusion check. Passed by value to every call rather than kept in
// package variables, so tests pointing at their own t.TempDir() stay
// parallel-safe under -race.
type Config struct {
	// ConfigRoot is the live config directory this agent reconciles into by
	// default. A field so RollbackFrom has a default when called without an
	// explicit root (the web UI's Rollback button), and so StateLoad has a
	// stable root to sanitize manifest entries against.
	ConfigRoot string
	// BackupRoot is the per-apply stash location, under the add-on's
	// Supervisor-managed /data volume.
	BackupRoot string
	// StatePath is the persisted sync state file, under the same /data
	// volume.
	StatePath string
	// Supervisor is the Supervisor API base URL.
	Supervisor string

	// CheckConfigTimeout bounds the check_config call.
	CheckConfigTimeout time.Duration
	// ServiceCallTimeout bounds a homeassistant.reload_all/restart call.
	ServiceCallTimeout time.Duration
	// HealthProbeRequestTimeout bounds a single health-probe GET.
	HealthProbeRequestTimeout time.Duration
	// HealthProbeTimeoutRestart bounds the overall health-probe wait
	// after a restart.
	HealthProbeTimeoutRestart time.Duration
	// HealthProbeTimeoutReload bounds the overall health-probe wait
	// after a reload.
	HealthProbeTimeoutReload time.Duration
	// HealthProbeInterval is how long to sleep between health-probe
	// attempts.
	HealthProbeInterval time.Duration

	// IsExcluded is the path-exclusion check the guard consults before
	// touching any path. Overridable so tests can inject a trivial stub
	// instead of depending on internal/gitsync's actual algorithm.
	IsExcluded IsExcludedFunc

	// TransformRepoFile is applied to a repository file's bytes on the way
	// into the live config and nowhere else - not to the pre-apply stash
	// (live bytes, which must round-trip unchanged) and not to a rollback
	// restore, which replays that stash verbatim. nil means copy as-is.
	TransformRepoFile TransformRepoFileFunc
}

// DefaultConfig returns the production Config: /homeassistant,
// /data/backup, /data/state.json, and timeouts sized for
// Raspberry-Pi-class hardware (a restart can take minutes). internal/
// history owns /data/history.jsonl separately, since state decides what
// gets applied while run history governs nothing (see history.DefaultPath).
func DefaultConfig() Config {
	return Config{
		ConfigRoot: ConfigRoot,
		BackupRoot: "/data/backup",
		StatePath:  "/data/state.json",
		Supervisor: options.Supervisor,

		CheckConfigTimeout:        30 * time.Second,
		ServiceCallTimeout:        30 * time.Second,
		HealthProbeRequestTimeout: 5 * time.Second,
		HealthProbeTimeoutRestart: 300 * time.Second,
		HealthProbeTimeoutReload:  60 * time.Second,
		HealthProbeInterval:       3 * time.Second,

		IsExcluded: DefaultIsExcluded,
	}
}

// Result is the outcome of one Apply call, or a RollbackFrom call.
type Result struct {
	// OK is true if the apply completed and passed validation, whether or
	// not a later reload/restart also succeeded (see Error).
	OK bool
	// Changed lists paths (relative to configRoot) actually written or
	// removed. Empty if the apply was rolled back.
	Changed []string
	// Error holds check_config failure output, a failed reload/restart or
	// health-probe note, changes skipped by the path guard, or "".
	Error string
	// RolledBack is true if validation failed and the pre-apply state
	// was restored.
	RolledBack bool
	// StashDir is the per-file stash directory this call wrote to, usable
	// as RollbackFrom's stashDir argument even after a failure, since it is
	// what a manual rollback restores from. "" when no stash was created
	// (no changes, or every change rejected by the path guard).
	StashDir string
	// Warnings holds check_config's "warnings" field verbatim when the
	// config passed validation, "" when there were none. Never affects OK
	// or RolledBack: check_config already called the config valid.
	Warnings string
}
