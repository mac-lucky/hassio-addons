package applier

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/fsx"
)

// RollbackFrom restores configRoot from a stash directory written by Apply
// (see manifest.json inside it): a path that "existed" is copied back over
// whatever is there now, one that was "absent" is removed, and any
// directory the apply created that is now empty goes too. Safe to call
// twice.
//
// Returns OK=false with a descriptive Error - never a panic - when the
// manifest is missing, unreadable or corrupt, or when any file could not be
// restored or removed, so a rollback that restored nothing is
// distinguishable from one that worked. Changed lists what was actually
// restored or removed; RolledBack mirrors OK; StashDir echoes the input.
func RollbackFrom(cfg Config, stashDir, configRoot string) Result {
	manifestPath := filepath.Join(stashDir, "manifest.json")
	data, err := os.ReadFile(manifestPath) // #nosec G304 -- stashDir is agent-managed, under cfg.BackupRoot
	var raw any
	if err == nil {
		err = json.Unmarshal(data, &raw)
	}
	if err != nil {
		msg := fmt.Sprintf("cannot read rollback manifest at %s: %v", manifestPath, err)
		slog.Warn("applier: rollback_from failed", "error", msg)
		return Result{OK: false, Error: msg, StashDir: stashDir}
	}

	files, createdDirs, corruptErr := parseStashManifest(raw)
	if corruptErr != "" {
		msg := fmt.Sprintf("rollback manifest at %s is corrupt: %s", manifestPath, corruptErr)
		slog.Warn("applier: rollback_from failed", "error", msg)
		return Result{OK: false, Error: msg, StashDir: stashDir}
	}

	relPaths := make([]string, 0, len(files))
	for k := range files {
		relPaths = append(relPaths, k)
	}
	sort.Strings(relPaths)

	configRootReal := fsx.Realpath(configRoot)
	var restored []string
	var failures []string
	for _, relPath := range relPaths {
		status := files[relPath]
		// Re-run at rollback time because the manifest could be stale or
		// corrupt and a parent directory swapped for a symlink since the
		// apply; also keeps the backupSrc join below inside the stash.
		// Why containment only, not guardChangePath: see
		// guardPathContained's comment.
		if err := guardPathContained(relPath, configRootReal); err != nil {
			slog.Warn("applier: rollback_from refused path", "path", relPath, "error", err)
			failures = append(failures, fmt.Sprintf("%s: %v", relPath, err))
			continue
		}
		dest := filepath.Join(configRoot, relPath)
		switch status {
		case "existed":
			backupSrc := filepath.Join(stashDir, relPath)
			if _, err := os.Stat(backupSrc); err != nil {
				msg := fmt.Sprintf("missing stashed copy of %s", relPath)
				slog.Warn("applier: rollback_from", "detail", msg, "stash_dir", stashDir)
				failures = append(failures, fmt.Sprintf("%s: %s", relPath, msg))
				continue
			}
			if parent := filepath.Dir(dest); parent != "." {
				if err := os.MkdirAll(parent, 0o755); err != nil { // #nosec G301 -- 0755 deliberate: Home Assistant's own process must traverse/read these dirs
					slog.Warn("applier: rollback_from could not restore", "dest", dest, "error", err)
					failures = append(failures, fmt.Sprintf("%s: could not restore: %v", relPath, err))
					continue
				}
			}
			if err := copyFile(backupSrc, dest); err != nil {
				slog.Warn("applier: rollback_from could not restore", "dest", dest, "error", err)
				failures = append(failures, fmt.Sprintf("%s: could not restore: %v", relPath, err))
				continue
			}
			restored = append(restored, relPath)
		case "absent":
			if _, err := os.Stat(dest); err == nil {
				if err := os.Remove(dest); err != nil {
					slog.Warn("applier: rollback_from could not remove", "dest", dest, "error", err)
					failures = append(failures, fmt.Sprintf("%s: could not remove: %v", relPath, err))
					continue
				}
			}
			restored = append(restored, relPath)
		default:
			msg := fmt.Sprintf("unknown status %q for %s", status, relPath)
			slog.Warn("applier: rollback_from", "detail", msg)
			failures = append(failures, fmt.Sprintf("%s: %s", relPath, msg))
		}
	}

	removeEmptyCreatedDirs(createdDirs, configRoot)

	if len(failures) > 0 {
		return Result{
			OK: false, Changed: restored, Error: "rollback incomplete: " + strings.Join(failures, "; "),
			StashDir: stashDir,
		}
	}
	return Result{OK: true, Changed: restored, RolledBack: true, StashDir: stashDir}
}

// parseStashManifest decodes raw (a decoded manifest.json) into a files map
// and createdDirs list, accepting both the current {"files",
// "created_dirs"} shape and an older flat {path: status} one with no
// directories to clean up. corruptErr is non-empty only when "files" is
// present but is not itself an object.
func parseStashManifest(raw any) (files map[string]string, createdDirs []string, corruptErr string) {
	m, ok := raw.(map[string]any)
	if !ok {
		return map[string]string{}, nil, ""
	}

	filesRaw, hasFiles := m["files"]
	_, hasCreatedDirs := m["created_dirs"]
	if hasFiles && hasCreatedDirs {
		filesObj, ok := filesRaw.(map[string]any)
		if !ok {
			return nil, nil, "'files' is not an object"
		}
		return stringValueMap(filesObj), stringSlice(m["created_dirs"]), ""
	}

	// Older flat {path: status} stash, with no created_dirs to clean up.
	return stringValueMap(m), nil, ""
}

func stringValueMap(m map[string]any) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		s, _ := v.(string)
		out[k] = s
	}
	return out
}

func stringSlice(v any) []string {
	list, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, item := range list {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// removeEmptyCreatedDirs removes configRoot-relative directories the
// rolled-back apply created and that are now empty. Deepest-first, so a
// parent emptied by its own child goes with it. Never configRoot itself,
// never a directory that resolves outside it (a corrupt manifest entry, or
// a parent swapped for a symlink since the apply), and never a directory
// with anything left in it.
func removeEmptyCreatedDirs(createdDirs []string, configRoot string) {
	configRootReal := fsx.Realpath(configRoot)
	sorted := append([]string(nil), createdDirs...)
	sort.Slice(sorted, func(i, j int) bool {
		return strings.Count(sorted[i], string(filepath.Separator)) > strings.Count(sorted[j], string(filepath.Separator))
	})

	for _, relDir := range sorted {
		if guardPathContained(relDir, configRootReal) != nil {
			continue
		}
		absDir := filepath.Join(configRoot, relDir)
		info, err := os.Stat(absDir)
		if err != nil || !info.IsDir() {
			continue
		}
		entries, err := os.ReadDir(absDir)
		if err != nil {
			slog.Warn("applier: rollback_from could not remove created directory", "dir", absDir, "error", err)
			continue
		}
		if len(entries) == 0 {
			if err := os.Remove(absDir); err != nil {
				slog.Warn("applier: rollback_from could not remove created directory", "dir", absDir, "error", err)
			}
		}
	}
}
