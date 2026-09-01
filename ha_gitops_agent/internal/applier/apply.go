package applier

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/fsx"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/options"
)

// Apply applies changes to configRoot, validating before committing:
//
//  1. Check every path against guardChangePath. A rejected change is
//     skipped rather than fatal, named in Result.Error; if every change is
//     rejected the call returns OK=false with no stash.
//  2. Stash a copy of every file about to be touched (add, update, delete)
//     under cfg.BackupRoot/<utc-ts>/, preserving relative paths.
//  3. Copy adds/updates from repoRoot through cfg.TransformRepoFile
//     (decryption) and remove deletes. A transform failure rolls back the
//     stash exactly as a failed write does.
//  4. POST <cfg.Supervisor>/core/api/config/core/check_config with the
//     Supervisor token.
//  5. Invalid config: restore the stash, remove newly added files, return
//     RolledBack=true and OK=false. A valid config's "warnings" go to
//     Result.Warnings and never affect OK or RolledBack.
//  6. Per opts.ApplyAfterPull, call homeassistant.reload_all ("reload"),
//     homeassistant.restart ("restart"), or nothing ("off").
//  7. Health-probe GET <cfg.Supervisor>/core/api/ until 200 or timeout.
//     After a restart a timeout IS evidence the change broke Home
//     Assistant: roll back, retry the restart to bring it up, return
//     RolledBack=true. After a reload it is not - Home Assistant never
//     stopped and check_config already passed - so the files stay applied
//     and Result.Error carries a warning.
//
// client nil means http.DefaultClient; tests inject a fake. Apply returns a
// non-nil error only for a stash directory it could not create or write and
// a missing Supervisor token; every other failure is carried in Result.
func Apply(
	ctx context.Context, cfg Config, changes []Change, repoRoot, configRoot string, opts options.Options, client HTTPClient,
) (Result, error) {
	if len(changes) == 0 {
		return Result{OK: true}, nil
	}
	if client == nil {
		client = http.DefaultClient
	}

	configRootReal := fsx.Realpath(configRoot)
	var goodChanges []Change
	var skipNotes []string
	for _, change := range changes {
		if err := guardChangePath(cfg, change.Path, configRootReal); err != nil {
			skipNotes = append(skipNotes, err.Error())
		} else if note := nonRegularLiveNote(configRoot, change.Path); note != "" {
			// Skipped like a guard rejection rather than failing the batch:
			// stashing a symlinked live file would refuse the copy and
			// abort every sibling change with it.
			skipNotes = append(skipNotes, note)
		} else {
			goodChanges = append(goodChanges, change)
		}
	}

	skipNote := ""
	if len(skipNotes) > 0 {
		skipNote = fmt.Sprintf("skipped %d change(s) rejected by the path guard: %s", len(skipNotes), strings.Join(skipNotes, "; "))
	}

	if len(goodChanges) == 0 {
		return Result{OK: false, Error: skipNote}, nil
	}
	changes = goodChanges

	// Before any write: a missing token means check_config could never run,
	// and finding that out after writeChanges would leave the config
	// overwritten with no validation and no stash pointer to roll back from.
	token, err := options.SupervisorToken()
	if err != nil {
		return Result{}, err
	}

	stashDir, err := makeStashDir(cfg)
	if err != nil {
		return Result{}, fmt.Errorf("applier: creating stash directory: %w", err)
	}
	if err := stashFiles(changes, configRoot, stashDir); err != nil {
		return Result{}, fmt.Errorf("applier: stashing files: %w", err)
	}

	changedPaths, writeErr := writeChanges(cfg, changes, repoRoot, configRoot)
	if writeErr != nil {
		errMsg, rbOK := rollbackAfterFailure(cfg, stashDir, configRoot, fmt.Sprintf("failed writing changes: %v", writeErr))
		return Result{OK: false, Error: joinNotes(skipNote, errMsg), RolledBack: rbOK, StashDir: stashDir}, nil
	}

	valid, checkErr, warnings := checkConfig(ctx, client, cfg, token)
	if !valid {
		errMsg, rbOK := rollbackAfterFailure(cfg, stashDir, configRoot, checkErr)
		return Result{OK: false, Error: joinNotes(skipNote, errMsg), RolledBack: rbOK, StashDir: stashDir}, nil
	}

	errMsg := ""
	if service := serviceFor(opts.ApplyAfterPull); service != "" {
		if callOK, callErr := callService(ctx, client, cfg, token, service); !callOK {
			errMsg = callErr
		}

		timeout := cfg.HealthProbeTimeoutReload
		if opts.ApplyAfterPull == "restart" {
			timeout = cfg.HealthProbeTimeoutRestart
		}
		if !healthProbe(ctx, client, cfg, token, timeout) {
			if opts.ApplyAfterPull == "restart" {
				// Home Assistant genuinely went down; not coming back in
				// time is evidence the change broke it.
				rbErr, rbOK := rollbackAfterFailure(cfg, stashDir, configRoot, "health probe timed out after applying changes")
				// Best-effort: failure here does not change the verdict.
				_, _ = callService(ctx, client, cfg, token, service)
				return Result{OK: false, Error: joinNotes(skipNote, rbErr), RolledBack: rbOK, StashDir: stashDir}, nil
			}
			// reload_all runs in-process and check_config already
			// passed, so a slow probe is not evidence the change is bad.
			errMsg = fmt.Sprintf(
				"Home Assistant applied your changes, but did not confirm it was healthy within %d seconds. Please check Home Assistant.",
				int(timeout.Seconds()))
		}
	}

	return Result{OK: true, Changed: changedPaths, Error: joinNotes(skipNote, errMsg), StashDir: stashDir, Warnings: warnings}, nil
}

// ReloadAfterRollback asks Home Assistant to re-read restored files, with
// the same service the rolled-back apply issued (opts.ApplyAfterPull).
// Without it a rollback leaves /config holding the old bytes while HA
// keeps RUNNING the applied config - matching neither side - until some
// later apply or manual restart. Best-effort: the files are already
// restored either way, so the return is "" or a warning to surface.
func ReloadAfterRollback(ctx context.Context, cfg Config, opts options.Options, client HTTPClient) string {
	const prefix = "could not ask home assistant to reload the restored files: "
	service := serviceFor(opts.ApplyAfterPull)
	if service == "" {
		return ""
	}
	if client == nil {
		client = http.DefaultClient
	}
	token, err := options.SupervisorToken()
	if err != nil {
		return prefix + err.Error()
	}
	if ok, errMsg := callService(ctx, client, cfg, token, service); !ok {
		return prefix + errMsg
	}
	return ""
}

// serviceFor maps opts.ApplyAfterPull to the homeassistant service an
// apply issues, or "" for "off". One mapping for Apply and
// ReloadAfterRollback, so a rollback can never reload with a different
// service than the apply it undoes.
func serviceFor(applyAfterPull string) string {
	switch applyAfterPull {
	case "reload":
		return "reload_all"
	case "restart":
		return "restart"
	}
	return ""
}

// nonRegularLiveNote reports a change whose live path exists as something
// other than a regular file - a user-made symlink into a shared location,
// typically. "" means the path is regular or absent and the change may
// proceed.
func nonRegularLiveNote(configRoot, relPath string) string {
	info, err := os.Lstat(filepath.Join(configRoot, relPath))
	if err != nil || info.Mode().IsRegular() {
		return ""
	}
	return fmt.Sprintf("skipped %s: the live path is not a regular file, so this agent leaves it alone", relPath)
}

func joinNotes(parts ...string) string {
	var nonEmpty []string
	for _, p := range parts {
		if p != "" {
			nonEmpty = append(nonEmpty, p)
		}
	}
	return strings.Join(nonEmpty, "; ")
}

// rollbackAfterFailure rolls back to stashDir after baseError, folding any
// rollback failure into the returned message rather than swallowing it.
// Returns (error, rolledBackOK).
func rollbackAfterFailure(cfg Config, stashDir, configRoot, baseError string) (string, bool) {
	rb := RollbackFrom(cfg, stashDir, configRoot)
	if rb.OK {
		return baseError, true
	}
	return fmt.Sprintf("%s; rollback also incomplete: %s", baseError, rb.Error), false
}
