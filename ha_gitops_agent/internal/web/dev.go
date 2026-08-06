//go:build dev

package web

// Dev-only preview fixtures: GET /?preview=<name> renders a canned
// Status. The `dev` tag compiles them in (dev_stub.go), DevEnvVar arms it.

import (
	"fmt"
	"net/http"
	"os"
	"sort"
	"time"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/history"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/recon"
)

// devNextCheckUTC is not on a fixed date because the client counts down
// from now. Resolved once at init, or no poll could ever answer 204.
var devNextCheckUTC = time.Now().Add(55 * time.Minute).UTC().Format(time.RFC3339)

// devAddonCheckedUTC is the second, rendered as an AGE and marked stale
// past data-stale-after. 24 minutes back, so one row previews fresh.
var devAddonCheckedUTC = time.Now().Add(-24 * time.Minute).UTC().Format(time.RFC3339)

// devPreviewStatus returns the status named by ?preview=, plus that name,
// which the fragment carries into its polling URL to keep it.
func devPreviewStatus(r *http.Request) (recon.Status, string, bool) {
	if os.Getenv(DevEnvVar) != "1" {
		return recon.Status{}, "", false
	}
	name := r.URL.Query().Get("preview")
	status, ok := devStatuses[name]
	if !ok {
		return recon.Status{}, "", false
	}
	return status, name, true
}

// devPreviewHistory is what GET /history renders. The "history" fixture
// answers with the longer list, the runs that do not fit on the card.
func devPreviewHistory(r *http.Request) ([]history.Record, string, bool) {
	status, name, ok := devPreviewStatus(r)
	if !ok {
		return nil, "", false
	}
	if name == "history" {
		return devRunLog(), name, true
	}
	return status.History, name, true
}

// devStatuses maps preview names to fully-populated fake statuses. All
// content is invented but shaped like real reconciler output.
var devStatuses = map[string]recon.Status{
	"firstrun": {
		State:      recon.StateDisabled,
		Configured: false,
	},
	"in_sync": {
		State:           recon.StateInSync,
		Configured:      true,
		RepoURL:         "https://github.com/example/ha-config.git",
		Branch:          "main",
		IntervalMinutes: 5,
		LastSHA:         "4f9c2a7e8b1d0c3f6a5e9d8c7b6a5f4e3d2c1b0a",
		LastSHAShort:    "4f9c2a7",
		LastApplyUTC:    "2026-08-01T21:14:22+00:00",
		LastStashDir:    "/data/backup/2026-08-01T21-14-09",
		NextCheckUTC:    devNextCheckUTC,
		Warnings: "Platform error sensor.templete - Integration 'templete' not found.\n" +
			"Package core.gitops: some referenced entities are not defined: sensor.outdoor_temp",
		Events:  devEvents(),
		History: devRuns(),
	},
	"drift":    devDriftStatus(false),
	"dry_run":  devDriftStatus(true),
	"applying": devApplyingStatus(),
	"error":    devErrorStatus(),
	"unseeded": devUnseededStatus(),

	"import_preview":   devImportPreviewStatus(),
	"import_done":      devImportDoneStatus(),
	"import_too_large": devImportTooLargeStatus(),

	"backup_failed":  devBackupFailedStatus(),
	"addon_updates":  devAddonUpdatesStatus(),
	"addon_checking": devAddonCheckRunningStatus(),
	"history":        devHistoryStatus(),
	"blocked":        devBlockedStatus(),
	"health":         devHealthStatus(),
	"managed":        devManagedStatus(),
	"paused":         devPausedStatus(),
}

// devUnseededStatus is a repository whose tracked branch does not exist
// yet: the seed banner and the blue pill, with import on so the banner
// names the button. No plan, no history - nothing has ever run against it.
func devUnseededStatus() recon.Status {
	return recon.Status{
		State:           recon.StateUnseeded,
		Configured:      true,
		RepoURL:         "https://github.com/example/ha-config.git",
		Branch:          "main",
		IntervalMinutes: 5,
		ImportEnabled:   true,
		NextCheckUTC:    devNextCheckUTC,
		Events: []recon.Event{{
			TS:      "2026-08-01T21:10:03+00:00",
			Message: "nothing to sync yet: branch main does not exist in the repository - import to seed it, or check the branch name",
		}},
	}
}

// devPausedStatus is the loop switched off: chip, banner, Resume button.
// NextCheckUTC is cleared because recon.SetPaused clears it.
func devPausedStatus() recon.Status {
	status := devDriftStatus(false)
	status.State = recon.StateInSync
	status.PendingCount = 0
	status.Pending = nil
	status.PendingRegistry = nil
	status.PendingRestartSlugs = nil
	status.Paused = true
	status.NextCheckUTC = ""
	// Carries the add-on card because the banner's promise about it only
	// renders when that card does, and this preview exists to show the
	// whole paused contract. Rows rather than an empty card: a paused
	// agent that has checked before is the state the banner is read in.
	status.AutoUpdateEnabled = true
	status.AddonUpdates = devAddonUpdatesStatus().AddonUpdates
	return status
}

// devManagedStatus fills every inventory group, nothing pending. Names
// only, never values; the file group runs past inventoryMax.
func devManagedStatus() recon.Status {
	status := devDriftStatus(false)
	status.State = recon.StateInSync
	status.PendingCount = 0
	status.Pending = nil
	status.PendingRegistry = nil
	status.PendingRestartSlugs = nil
	status.Managed = recon.ManagedInventory{
		Files: devManagedFiles(),
		Registry: []string{
			"area:kitchen",
			"area:living_room",
			"area:office",
			"floor:ground",
			"floor:upstairs",
			"input_boolean:guest_mode",
			"input_number:vacuum_runs",
			"label:deprecated",
			"label:managed",
		},
		Entities: []string{
			"light.living_room_ceiling",
			"sensor.outdoor_temp",
			"switch.old_garage_heater",
		},
		Dashboards:   []string{"energy", "gitops_home"},
		Addons:       []string{"core_configurator", "core_ssh"},
		Integrations: []string{"moon_home", "workday_main"},
		Subentries:   []string{"widget_hall", "widget_kitchen"},
		Hacs:         []string{"adaptive_lighting", "anker_solix"},
	}
	// A restart reminder follows an apply that already happened, so it
	// rides this preview rather than the drift one.
	status.HacsRestartPending = []string{"anker_solix"}
	return status
}

// devManagedFiles is a config's own files plus enough generated ones to
// run past the render cap. Sorted, as the reconciler's own group is.
func devManagedFiles() []string {
	files := []string{
		"automations.yaml",
		"configuration.yaml",
		"packages/climate.yaml",
		"packages/lights.yaml",
		"packages/presence.yaml",
		"scenes.yaml",
		"scripts.yaml",
		"scripts/vacuum_kitchen.yaml",
		"themes/dark.yaml",
		"www/community/card-mod/card-mod.js",
		"zigbee2mqtt/configuration.yaml",
		"zigbee2mqtt/devices.yaml",
	}
	for i := 1; i <= 200; i++ {
		files = append(files, fmt.Sprintf("packages/room_%03d.yaml", i))
	}
	sort.Strings(files)
	return files
}

// devBlockedStatus carries both shapes: one still declared (the planner
// also emits an error op) and one orphaned, which only this card shows.
func devBlockedStatus() recon.Status {
	const workdayFailure = "flow step 'user' rejected the submitted data (invalid_auth)"

	status := devDriftStatus(false)
	status.Blocked = []recon.BlockedItem{
		{
			RType: "integration",
			Key:   "integration:workday_main",
			Name:  "workday_main",
			Error: workdayFailure,
		},
		{
			RType: "subentry",
			Key:   "subentry:widget_garage",
			Name:  "widget_garage",
			Error: "subentry flow ended in an unexpected step 'reauth_confirm'",
		},
	}
	for i, op := range status.PendingRegistry {
		if op.RType == "integration" && op.Key == "workday_main" {
			status.PendingRegistry[i] = recon.PendingRegOp{
				RType: "integration", Key: "workday_main", Kind: "error",
				Error: "previous attempt failed: " + workdayFailure +
					"; change its manifest entry or press Retry on the dashboard",
			}
		}
	}
	return status
}

// devHealthStatus raises every standing health flag at once, which no
// real agent would manage: a catalogue, so the chip row can be seen wrap.
func devHealthStatus() recon.Status {
	status := devDriftStatus(false)
	status.State = recon.StateInSync
	status.PendingCount = 0
	status.Pending = nil
	status.PendingRegistry = nil
	status.PendingRestartSlugs = nil
	status.HistoryWriteFailing = true
	status.VersionRecordFailing = true
	status.ImportRecordFailing = true
	status.CaptureFailing = true
	status.AddonUpdateSelfSlugFailing = true
	status.AddonCheckFailing = []string{"a0d7b954_esphome", "core_samba"}
	// The only flag a reconcile layer raises: HACS is on and cannot run.
	status.HacsUnavailable = true
	return status
}

// devHistoryStatus is the run-history card alone, other cards quieted.
// The card's half of the preview (the standalone page renders devRunLog).
func devHistoryStatus() recon.Status {
	status := devDriftStatus(false)
	status.State = recon.StateInSync
	status.PendingCount = 0
	status.Pending = nil
	status.PendingRegistry = nil
	status.PendingRestartSlugs = nil
	status.Warnings = ""
	status.History = devRunCatalogue()
	return status
}

// devAddonUpdatesStatus is the auto_update_addons card with one row of
// every shape, which no real check produces at once.
//
// The last two fold, decided only by AddonUpdateStatus.Actionable, so
// both are written from the consts it switches on. One row carries
// devAddonCheckedUTC, so a fresh age renders beside the stale ones.
func devAddonUpdatesStatus() recon.Status {
	status := devDriftStatus(true)
	status.State = recon.StateInSync
	status.PendingCount = 0
	status.Pending = nil
	status.PendingRegistry = nil
	status.PendingRestartSlugs = nil
	status.AutoUpdateEnabled = true
	// Six hours, RunAddonUpdateLoop's own interval, handed to the client
	// as data-stale-after. 0 would mean no marker at all.
	status.AddonCheckIntervalSeconds = 21600
	status.AddonUpdates = []recon.AddonUpdateStatus{
		{
			Slug:           "core_configurator",
			Name:           "File editor",
			Version:        "5.9.0",
			LatestVersion:  "5.9.0",
			LastResult:     "up to date",
			LastCheckedUTC: "2026-08-03T14:12:07+00:00",
		},
		{
			Slug:            "a0d7b954_esphome",
			Name:            "ESPHome Device Builder",
			Version:         "2026.7.3",
			LatestVersion:   "2026.8.0",
			UpdateAvailable: true,
			LastResult:      "update available (dry run, not installing)",
			// Checked minutes ago, installed on the fixed date: an old
			// CHECK is a problem, an old install is not.
			LastCheckedUTC: devAddonCheckedUTC,
			LastUpdatedUTC: "2026-07-11T05:03:44+00:00",
		},
		{
			Slug:            "core_samba",
			Name:            "Samba share",
			Version:         "12.3.2",
			LatestVersion:   "12.4.0",
			UpdateAvailable: true,
			LastResult: "update failed: supervisor request failed with 400: " +
				"Can't install ghcr.io/home-assistant/amd64-addon-samba:12.4.0: no space left on device",
			LastCheckedUTC: "2026-08-03T14:12:07+00:00",
		},
		{
			// Above the fold with an "unknown" badge: Supervisor was
			// unreachable and may succeed next cycle, unlike the two below.
			Slug:           "core_mariadb",
			Name:           "MariaDB",
			LastResult:     "check failed: supervisor request failed with 502: Bad Gateway",
			LastCheckedUTC: "2026-08-03T14:12:07+00:00",
		},
		{
			// Folded. No display name either - a slug typed wrong reaches
			// exactly this row, which is why it is never dropped.
			Slug:           "core_typo",
			LastResult:     recon.AddonUpdateNotInstalled,
			LastCheckedUTC: "2026-08-03T14:12:07+00:00",
		},
		{
			// Folded. No versions, slug as the name: the self row is
			// refused before anything is fetched from Supervisor.
			Slug:           "local_ha_gitops_agent",
			Name:           "local_ha_gitops_agent",
			LastResult:     recon.AddonUpdateRefusedSelf,
			LastCheckedUTC: "2026-08-03T14:12:07+00:00",
		},
	}
	status.Events = append(status.Events, recon.Event{
		TS:      "2026-08-03T14:12:07+00:00",
		Message: "dry run: add-on a0d7b954_esphome update available (2026.7.3 -> 2026.8.0), not installing",
	})
	return status
}

// devAddonCheckRunningStatus is the same card mid-check. Its own scenario
// because the rows shown are still the PREVIOUS check's results.
func devAddonCheckRunningStatus() recon.Status {
	status := devAddonUpdatesStatus()
	status.AddonCheckRunning = true
	return status
}

// devBackupFailedStatus is an apply that succeeded while the Supervisor
// backup timed out. No Warnings, so the callout stands alone.
func devBackupFailedStatus() recon.Status {
	return recon.Status{
		State:           recon.StateInSync,
		Configured:      true,
		RepoURL:         "https://github.com/example/ha-config.git",
		Branch:          "main",
		IntervalMinutes: 5,
		LastSHA:         "4f9c2a7e8b1d0c3f6a5e9d8c7b6a5f4e3d2c1b0a",
		LastSHAShort:    "4f9c2a7",
		LastApplyUTC:    "2026-08-01T21:14:22+00:00",
		LastStashDir:    "/data/backup/2026-08-01T21-14-09",
		NextCheckUTC:    devNextCheckUTC,
		LastBackupError: "supervisor request failed after 15m0s: " +
			"Post \"http://supervisor/backups/new/partial\": context deadline exceeded",
		Events: devEvents(),
	}
}

func devDriftStatus(dryRun bool) recon.Status {
	pending := devPendingFiles()
	registry := devPendingRegistry()
	return recon.Status{
		State:             recon.StateDriftPending,
		Configured:        true,
		DryRun:            dryRun,
		RepoURL:           "https://github.com/example/ha-config.git",
		Branch:            "main",
		IntervalMinutes:   5,
		LastSHA:           "4f9c2a7e8b1d0c3f6a5e9d8c7b6a5f4e3d2c1b0a",
		LastSHAShort:      "4f9c2a7",
		LastApplyUTC:      "2026-08-01T21:14:22+00:00",
		LastStashDir:      "/data/backup/2026-08-01T21-14-09",
		NextCheckUTC:      devNextCheckUTC,
		CommitBackEnabled: true,
		LastDriftBranch:   "gitops/drift-20260802T063000Z",
		ImportEnabled:     true,
		PendingCount:      len(pending) + len(registry),
		Pending:           pending,
		PendingRegistry:   registry,
		// The add-on op declares restart_on_change, so the Apply confirm
		// names it. Previews that empty the plan must clear this too.
		PendingRestartSlugs: []string{"core_configurator"},
		// What the last apply stashed, as the Roll Back confirm quotes it.
		RollbackPreview: "3 file(s) and registry objects",
		Events:          devEvents(),
		History:         devRuns(),
		// More runs held than the card shows, which is what puts the "all
		// N" link there. Off devRunLog, so it matches GET /history.
		HistoryTotal: len(devRunLog()),
	}
}

// devImportPreviewStatus is a repository that has not been seeded yet,
// with a preview on screen waiting to be acted on.
func devImportPreviewStatus() recon.Status {
	status := devDriftStatus(false)
	status.State = recon.StateInSync
	status.PendingCount = 0
	status.Pending = nil
	status.PendingRegistry = nil
	status.PendingRestartSlugs = nil
	status.ImportEnabled = true
	status.ImportPreview = &recon.ImportPreview{
		Files: devImportPreviewFiles(),
		// A real config tree's size, since that is what the summary asks
		// the reader to agree to pushing.
		TotalBytes:        17_931_872,
		SkippedExcluded:   6,
		SkippedGitignored: 4518,
		SkippedSecret:     2,
		SkippedNonRegular: 1,
	}
	return status
}

// devImportPreviewFiles runs past inventoryMax so the shared partial
// shows its cap line. Sorted, as gitsync walks the tree lexically.
func devImportPreviewFiles() []string {
	files := []string{
		"automations.yaml",
		"configuration.yaml",
		"custom_components/hacs/manifest.json",
		"packages/climate.yaml",
		"packages/lights.yaml",
		"scripts.yaml",
		"themes/dark.yaml",
	}
	for i := 1; i <= 200; i++ {
		files = append(files, fmt.Sprintf("packages/room_%03d.yaml", i))
	}
	sort.Strings(files)
	return files
}

// devImportDoneStatus is the state just after a successful import: the
// repository now matches live, so there is nothing pending.
func devImportDoneStatus() recon.Status {
	status := devDriftStatus(false)
	status.State = recon.StateInSync
	status.PendingCount = 0
	status.Pending = nil
	status.PendingRegistry = nil
	status.PendingRestartSlugs = nil
	status.ImportEnabled = true
	status.LastImportUTC = "2026-08-03T14:12:07+00:00"
	status.LastImportSHA = "9d3b7c1a5e2f8046b3c9d7e1a2f5084b6c3d9e17"
	status.LastImportSHAShort = "9d3b7c1"
	return status
}

// devImportTooLargeStatus is a config tree past the size cap, for the
// long actionable message this has to render legibly.
func devImportTooLargeStatus() recon.Status {
	status := devDriftStatus(false)
	status.State = recon.StateInSync
	status.PendingCount = 0
	status.Pending = nil
	status.PendingRegistry = nil
	status.PendingRestartSlugs = nil
	status.ImportEnabled = true
	status.LastImportError = "gitsync: import: refusing to import: total size 412.7 MB exceeds the 100.0 MB limit; " +
		"largest: media/cam/2026-07-31.mp4 (188.2 MB), media/cam/2026-07-30.mp4 (154.9 MB), www/backup.tar (61.1 MB); " +
		"move it out of the config directory or add it to the repository's .gitignore, then try again"
	return status
}

func devApplyingStatus() recon.Status {
	status := devDriftStatus(false)
	status.State = recon.StateApplying
	status.Busy = true
	return status
}

func devErrorStatus() recon.Status {
	status := devDriftStatus(false)
	status.State = recon.StateError
	status.LastError = "check_config failed:\n" +
		"Invalid config for [automation]: required key not provided @ data['trigger']. " +
		"Got None. (See /config/automations.yaml, line 42).\n" +
		"Configuration check exited with code 1; nothing was written."
	status.Events = append(status.Events, recon.Event{
		TS:      "2026-08-02T07:31:44+00:00",
		Message: "apply failed: check_config rejected the pending configuration; files restored from stash",
	})
	return status
}

func devPendingFiles() []recon.PendingChange {
	return []recon.PendingChange{
		{
			Path: "automations.yaml",
			Kind: "update",
			DiffText: "--- a/automations.yaml\n" +
				"+++ b/automations.yaml\n" +
				"@@ -12,9 +12,11 @@\n" +
				" - alias: Morning lights\n" +
				"   trigger:\n" +
				"     - platform: sun\n" +
				"       event: sunrise\n" +
				"+      offset: \"-00:30:00\"\n" +
				"   action:\n" +
				"     - service: light.turn_on\n" +
				"       data:\n" +
				"-        brightness: 180\n" +
				"+        brightness_pct: 70\n" +
				"+        transition: 5\n",
		},
		{
			Path: "scripts/vacuum_kitchen.yaml",
			Kind: "add",
			DiffText: "--- /dev/null\n" +
				"+++ b/scripts/vacuum_kitchen.yaml\n" +
				"@@ -0,0 +1,6 @@\n" +
				"+vacuum_kitchen:\n" +
				"+  alias: Vacuum the kitchen\n" +
				"+  sequence:\n" +
				"+    - service: vacuum.send_command\n" +
				"+      data:\n" +
				"+        command: app_segment_clean\n",
		},
		{
			Path: "scenes_old.yaml",
			Kind: "delete",
			DiffText: "--- a/scenes_old.yaml\n" +
				"+++ /dev/null\n" +
				"@@ -1,5 +0,0 @@\n" +
				"-- name: Movie night\n" +
				"-  entities:\n" +
				"-    light.living_room:\n" +
				"-      state: on\n" +
				"-      brightness: 40\n",
		},
	}
}

func devPendingRegistry() []recon.PendingRegOp {
	return []recon.PendingRegOp{
		{
			RType: "floor",
			Key:   "ground",
			Kind:  "create",
			DiffText: "+name: Ground floor\n" +
				"+level: 0\n" +
				"+icon: mdi:home-floor-g\n",
		},
		{
			RType: "area",
			Key:   "living_room",
			Kind:  "update",
			DiffText: "-icon: mdi:sofa-outline\n" +
				"+icon: mdi:sofa\n" +
				"+floor_id: ground\n",
		},
		{
			RType:    "label",
			Key:      "deprecated",
			Kind:     "delete",
			DiffText: "-name: Deprecated\n-color: red\n",
		},
		{
			RType: "area",
			Key:   "office",
			Kind:  "error",
			Error: "ambiguous adopt: 2 live area objects named 'Office'; rename one in Home Assistant or set a unique id in the repo",
		},
		{
			RType: "entity",
			Key:   "light.living_room_ceiling",
			Kind:  "update",
			DiffText: "-icon: mdi:ceiling-light-outline\n" +
				"+icon: mdi:ceiling-light\n" +
				"+area_id: living_room\n",
		},
		{
			RType: "entity",
			Key:   "switch.old_garage_heater",
			Kind:  "restore",
			DiffText: "-name: Garage Heater\n" +
				"+name: null\n",
		},
		{
			RType: "dashboard",
			Key:   "gitops_home",
			Kind:  "create",
			DiffText: "+title: GitOps Home\n" +
				"+show_in_sidebar: True\n" +
				"--- live/dashboard/gitops_home/config.yaml\n" +
				"+++ manifest/dashboard/gitops_home/config.yaml\n" +
				"@@ -0,0 +1,3 @@\n" +
				"+title: Home\n" +
				"+views:\n" +
				"+- title: Overview\n",
		},
		{
			RType: "dashboard",
			Key:   "energy",
			Kind:  "update",
			DiffText: "--- live/dashboard/energy\n" +
				"+++ manifest/dashboard/energy\n" +
				"@@ -1,2 +1,2 @@\n" +
				"-icon: mdi:flash-outline\n" +
				"+icon: mdi:lightning-bolt\n" +
				" title: Energy\n",
		},
		{
			RType:    "dashboard",
			Key:      "old_kiosk",
			Kind:     "delete",
			DiffText: "-title: Old Kiosk\n-show_in_sidebar: True\n",
		},
		{
			RType: "dashboard",
			Key:   "broken",
			Kind:  "error",
			Error: "could not read config file 'dashboards/broken.yaml': open dashboards/broken.yaml: no such file or directory",
		},
		{
			RType:    "addon",
			Key:      "core_configurator",
			Kind:     "update",
			DiffText: "dirsfirst: False -> True",
		},
		{
			RType:    "addon",
			Key:      "core_ssh",
			Kind:     "restore",
			DiffText: "authorized_keys: ['ssh-ed25519 AAAA...'] -> []",
		},
		{
			RType: "addon",
			Key:   "core_letsencrypt",
			Kind:  "error",
			Error: "add-on not installed: core_letsencrypt",
		},
		{
			RType: "integration",
			Key:   "workday_main",
			Kind:  "create",
			DiffText: "+domain: workday\n" +
				"+title: Workday\n" +
				"(a new config-entry flow will be driven to create this integration)\n",
		},
		{
			RType: "integration",
			Key:   "moon_home",
			Kind:  "update",
			DiffText: "adopted existing integration 'moon_home' (domain 'moon', live entry_id 7f3a9c); " +
				"no flow will run",
		},
		{
			RType:    "integration",
			Key:      "old_weather",
			Kind:     "delete",
			DiffText: "-domain: openweathermap\n-title: Old Weather\n-entry_id: 4b1e6d\n",
		},
		{
			RType: "integration",
			Key:   "workday_holidays",
			Kind:  "error",
			Error: "declared data for integration 'workday_holidays' (domain 'workday') changed after it was created; " +
				"this layer cannot update an existing config entry's data - delete it " +
				"(remove it from the manifest, let this apply, then re-declare it) to apply the new configuration",
		},
		{
			RType: "hacs",
			Key:   "anker_solix",
			Kind:  "create",
			DiffText: "+repository: thomluther/ha-anker-solix\n" +
				"+category: integration\n" +
				"+version: 3.1.0\n" +
				"(HACS will download this integration; Home Assistant has to restart before it loads)\n",
		},
		{
			RType: "hacs",
			Key:   "adaptive_lighting",
			Kind:  "update",
			DiffText: "adopted installed HACS integration 'adaptive_lighting' " +
				"(basnijholt/adaptive-lighting, repository id 208324598, domain 'adaptive_lighting'); " +
				"nothing is downloaded and no version is changed\n",
		},
		{
			RType: "hacs",
			Key:   "missing_card",
			Kind:  "error",
			Error: "previous attempt failed: could not download nobody/no-such-repo: " +
				"HACS accepted the custom repository nobody/no-such-repo but does not list it afterwards; " +
				"change its manifest entry or press Retry on the dashboard",
		},
		{
			RType: "subentry",
			Key:   "widget_kitchen",
			Kind:  "create",
			DiffText: "+subentry_type: tracked_widget\n" +
				"+parent entry_id: 7f3a9c\n" +
				"+declared fields (values hidden): user.slug, user.stat_rows\n" +
				"(a new subentry flow will be driven to create this subentry)\n",
		},
		{
			RType: "subentry",
			Key:   "widget_hall",
			Kind:  "update",
			DiffText: "declared data for subentry 'widget_hall' (type 'tracked_widget') changed; " +
				"a reconfigure flow will re-submit it\n" +
				"declared fields (values hidden): user.slug, user.stat_rows\n",
		},
		{
			RType:    "subentry",
			Key:      "widget_old_office",
			Kind:     "update",
			DiffText: "stopped managing subentry 'widget_old_office'; the live subentry is left untouched",
		},
		{
			RType: "subentry",
			Key:   "calendar_family",
			Kind:  "error",
			Error: "no live integration entry for domain 'google' to hold this subentry; set the integration up first",
		},
	}
}

func devEvents() []recon.Event {
	return []recon.Event{
		{TS: "2026-08-02T06:58:12+00:00", Message: "agent started"},
		{TS: "2026-08-02T07:00:03+00:00", Message: "reconcile: fetched origin/main at 4f9c2a7"},
		{TS: "2026-08-02T07:00:04+00:00", Message: "reconcile: 3 file changes, 4 registry ops pending"},
		{TS: "2026-08-02T07:05:03+00:00", Message: "reconcile: no new commits on origin/main"},
		{TS: "2026-08-02T07:10:03+00:00", Message: "reconcile: no new commits on origin/main"},
		{TS: "2026-08-02T07:15:04+00:00", Message: "reconcile: 3 file changes, 4 registry ops pending"},
	}
}

// devRuns is the everyday case: reconciles plus the apply that followed
// the drift they found.
func devRuns() []history.Record {
	return []history.Record{
		{
			Kind: history.KindApply, StartedUTC: "2026-08-02T07:15:09+00:00", DurationMS: 4200,
			SHA: "4f9c2a7e8b1d0c3f6a5e9d8c7b6a5f4e3d2c1b0a", Outcome: history.OutcomeOK,
			Files: 3, RegOps: 4, StashDir: "/data/backup/20260802T071509Z",
		},
		{
			Kind: history.KindReconcile, StartedUTC: "2026-08-02T07:15:04+00:00", DurationMS: 1100,
			SHA: "4f9c2a7e8b1d0c3f6a5e9d8c7b6a5f4e3d2c1b0a", Outcome: history.OutcomeDrift,
			Files: 3, RegOps: 4,
		},
		{
			Kind: history.KindReconcile, StartedUTC: "2026-08-02T07:10:03+00:00", DurationMS: 900,
			SHA: "91be004c1a2b3d4e5f60718293a4b5c6d7e8f900", Outcome: history.OutcomeInSync,
		},
	}
}

// devRunCatalogue carries one row of every shape, in an order no real
// install produces (two applies cannot both be the most recent).
func devRunCatalogue() []history.Record {
	return []history.Record{
		{
			// The minutes branch of humanDuration: a real apply can wait
			// five minutes on the health probe.
			Kind: history.KindApply, StartedUTC: "2026-08-02T09:40:00+00:00", DurationMS: 192_000,
			SHA: "4f9c2a7e8b1d0c3f6a5e9d8c7b6a5f4e3d2c1b0a", Outcome: history.OutcomeOK,
			Files: 18, RegOps: 2, StashDir: "/data/backup/20260802T094000Z",
		},
		{
			// What "partial" exists for: the files are live, and calling
			// this an error would say otherwise.
			Kind: history.KindApply, StartedUTC: "2026-08-02T09:20:11+00:00", DurationMS: 6400,
			SHA: "4f9c2a7e8b1d0c3f6a5e9d8c7b6a5f4e3d2c1b0a", Outcome: history.OutcomePartial,
			Files: 3, RegOps: 1, StashDir: "/data/backup/20260802T092011Z",
			Error: "integrations: create pushward: flow step 'user' rejected the submitted data (invalid_auth); " +
				"1 earlier registry change(s) stayed applied",
		},
		{
			// A long error near the ErrorMaxLen cut, wrapping onto its own
			// full-width row.
			Kind: history.KindApply, StartedUTC: "2026-08-02T08:55:02+00:00", DurationMS: 12_800,
			SHA: "91be004c1a2b3d4e5f60718293a4b5c6d7e8f900", Outcome: history.OutcomeRolledBack,
			StashDir: "/data/backup/20260802T085502Z",
			Error: "check_config failed: Platform error sensor.templete - Integration 'templete' not found. " +
				"Package core.gitops: some referenced entities are not defined: sensor.outdoor_temp, " +
				"sensor.indoor_temp, binary_sensor.front_door. Invalid config for [automation]: " +
				"required key not provided @ data['action']. Got None. (See /config/automations.yaml, line 41).",
		},
		{
			// No SHA: a rollback moves live away from a commit, so the
			// column renders "-".
			Kind: history.KindRollback, StartedUTC: "2026-08-02T08:54:40+00:00", DurationMS: 2100,
			Outcome: history.OutcomeOK, Files: 3, StashDir: "/data/backup/20260802T085502Z",
		},
		{
			Kind: history.KindReconcile, StartedUTC: "2026-08-02T08:50:03+00:00", DurationMS: 1400,
			SHA: "91be004c1a2b3d4e5f60718293a4b5c6d7e8f900", Outcome: history.OutcomeDrift,
			Files: 6, RegOps: 3,
		},
		{
			Kind: history.KindReconcile, StartedUTC: "2026-08-02T08:45:03+00:00", DurationMS: 870,
			SHA: "91be004c1a2b3d4e5f60718293a4b5c6d7e8f900", Outcome: history.OutcomeInSync,
		},
		{
			// Stopped before it could fetch, so it names no commit either.
			Kind: history.KindReconcile, StartedUTC: "2026-08-02T08:40:03+00:00", DurationMS: 320,
			Outcome: history.OutcomeError,
			Error:   "refusing to sync: secrets tracked in repository: secrets.yaml",
		},
		{
			Kind: history.KindImport, StartedUTC: "2026-08-02T08:30:12+00:00", DurationMS: 3300,
			SHA: "0c1d2e3f4a5b6c7d8e9f0a1b2c3d4e5f60718293", Outcome: history.OutcomeOK,
			Files: 191,
		},
	}
}

// devRunsBeforeCatalogue is how many ordinary cycles devRunLog appends,
// enough to exceed the card's cut (recon.historyStatusMax is 25).
const devRunsBeforeCatalogue = 32

// devRunLog is the history page's fixture: devRunCatalogue plus ordinary
// cycles, on a fixed date since the tests compare bytes.
func devRunLog() []history.Record {
	records := devRunCatalogue()
	// Walking backwards from the oldest catalogue row, so the whole list
	// stays newest-first.
	started := time.Date(2026, 8, 2, 8, 30, 12, 0, time.UTC)
	for i := 1; i <= devRunsBeforeCatalogue; i++ {
		started = started.Add(-5 * time.Minute)
		record := history.Record{
			Kind:       history.KindReconcile,
			StartedUTC: started.Format(time.RFC3339),
			DurationMS: int64(700 + (i*137)%900),
			SHA:        "0c1d2e3f4a5b6c7d8e9f0a1b2c3d4e5f60718293",
			Outcome:    history.OutcomeInSync,
		}
		// Every seventh cycle found something, so the page is not one
		// outcome repeated.
		if i%7 == 0 {
			record.Outcome = history.OutcomeDrift
			record.Files = 1 + i%4
			record.RegOps = i % 3
		}
		records = append(records, record)
	}
	return records
}
