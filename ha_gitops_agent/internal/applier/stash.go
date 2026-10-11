package applier

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/ignorekeys"
)

// stashManifestFile is manifest.json's on-disk shape.
type stashManifestFile struct {
	Files       map[string]string `json:"files"`
	CreatedDirs []string          `json:"created_dirs"`
}

// makeStashDir allocates a fresh, empty per-apply directory under
// cfg.BackupRoot, named after the current UTC timestamp with a numeric
// suffix on collision.
func makeStashDir(cfg Config) (string, error) {
	ts := time.Now().UTC().Format("20060102T150405Z")
	stashDir := filepath.Join(cfg.BackupRoot, ts)
	candidate := stashDir
	// Bounded, and only a clean "does not exist" ends the probe: any other
	// stat failure (ENOTDIR from a file where the backup root should be,
	// EACCES) would recur for every suffix and spin forever.
	for suffix := 0; ; suffix++ {
		_, err := os.Stat(candidate)
		if os.IsNotExist(err) {
			break
		}
		if err != nil {
			return "", fmt.Errorf("probing %s: %w", candidate, err)
		}
		if suffix >= 1000 {
			return "", fmt.Errorf("no free stash directory name after %s", candidate)
		}
		candidate = fmt.Sprintf("%s-%d", stashDir, suffix+1)
	}
	if err := os.MkdirAll(candidate, 0o755); err != nil { // #nosec G301 -- 0755 deliberate: Home Assistant's own process must traverse/read these dirs
		return "", err
	}
	return candidate, nil
}

// MakeStashDir allocates a fresh per-apply stash directory for a caller
// that does not go through Apply. Registry-only applies (no file changes -
// see internal/regapply.ApplyPlan) still need somewhere to write
// registry_stash.json, and the empty manifest.json written here makes
// RollbackFrom answer "no file changes to roll back" with OK rather than an
// error about a missing manifest.
func MakeStashDir(cfg Config) (string, error) {
	stashDir, err := makeStashDir(cfg)
	if err != nil {
		return "", err
	}
	manifestPath := filepath.Join(stashDir, "manifest.json")
	if _, err := os.Stat(manifestPath); os.IsNotExist(err) {
		data, marshalErr := json.Marshal(stashManifestFile{Files: map[string]string{}, CreatedDirs: []string{}})
		if marshalErr != nil {
			return stashDir, marshalErr
		}
		if writeErr := os.WriteFile(manifestPath, data, 0o644); writeErr != nil { // #nosec G306 -- add-on-private state, not secret
			return stashDir, writeErr
		}
	}
	return stashDir, nil
}

// stashFiles copies every to-be-touched file that currently exists into
// stashDir, and writes a manifest.json recording each change as "existed"
// or "absent" plus the configRoot directories the apply is about to create,
// so RollbackFrom can remove them again if they end up empty.
func stashFiles(changes []Change, configRoot, stashDir string) error {
	manifest := map[string]string{}
	for _, change := range changes {
		src := filepath.Join(configRoot, change.Path)
		if _, err := os.Stat(src); err == nil {
			dest := filepath.Join(stashDir, change.Path)
			if parent := filepath.Dir(dest); parent != "." {
				if err := os.MkdirAll(parent, 0o755); err != nil { // #nosec G301 -- 0755 deliberate: Home Assistant's own process must traverse/read these dirs
					return err
				}
			}
			if err := copyFile(src, dest); err != nil {
				return err
			}
			manifest[change.Path] = "existed"
		} else {
			manifest[change.Path] = "absent"
		}
	}

	createdDirs := dirsToCreate(changes, configRoot)

	data, err := json.Marshal(stashManifestFile{Files: manifest, CreatedDirs: createdDirs})
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(stashDir, "manifest.json"), data, 0o644) // #nosec G306 -- add-on-private state, not secret
}

// dirsToCreate returns configRoot-relative directories writeChanges is
// about to create for add/update changes. Must be called before any writes
// happen. Ordered shallowest-first.
func dirsToCreate(changes []Change, configRoot string) []string {
	toCreate := map[string]bool{}
	for _, change := range changes {
		if change.Kind != ChangeAdd && change.Kind != ChangeUpdate {
			continue
		}
		relDir := filepath.Dir(change.Path)
		for relDir != "" && relDir != "." && relDir != string(filepath.Separator) {
			if info, err := os.Stat(filepath.Join(configRoot, relDir)); err == nil && info.IsDir() {
				break
			}
			toCreate[relDir] = true
			next := filepath.Dir(relDir)
			if next == relDir {
				break
			}
			relDir = next
		}
	}

	result := make([]string, 0, len(toCreate))
	for d := range toCreate {
		result = append(result, d)
	}
	sort.Slice(result, func(i, j int) bool {
		return strings.Count(result[i], string(filepath.Separator)) < strings.Count(result[j], string(filepath.Separator))
	})
	return result
}

// writeChanges copies add/update changes from repoRoot into configRoot and
// removes files for delete changes, returning the paths touched in order.
// A failed copy errors rather than skipping the file, so Apply rolls the
// whole batch back instead of leaving the config half-updated.
//
// A path with ignore_keys entries (cfg.IgnoredKeys) is written as the
// repository's copy with the live value at each ignored key, or without
// the key where live has none - a file not live yet has none
// (keepLiveValues): those keys belong to the app that writes the file, and
// applying git content must not strip them.
// The stash taken before this holds the live file as it was, ignored keys
// included, so a rollback restores them byte for byte.
func writeChanges(cfg Config, changes []Change, repoRoot, configRoot string) ([]string, error) {
	var changedPaths []string
	for _, change := range changes {
		dest := filepath.Join(configRoot, change.Path)
		switch change.Kind {
		case ChangeAdd, ChangeUpdate:
			src := filepath.Join(repoRoot, change.Path)
			if parent := filepath.Dir(dest); parent != "." {
				if err := os.MkdirAll(parent, 0o755); err != nil { // #nosec G301 -- 0755 deliberate: Home Assistant's own process must traverse/read these dirs
					return nil, err
				}
			}
			var keys []ignorekeys.KeyPath
			if cfg.IgnoredKeys != nil {
				keys = cfg.IgnoredKeys(change.Path)
			}
			if err := copyFileWith(src, dest, keepLiveValues(keys, change.Path, dest)); err != nil {
				return nil, err
			}
			changedPaths = append(changedPaths, change.Path)
		case ChangeDelete:
			if _, err := os.Stat(dest); err == nil {
				if err := os.Remove(dest); err != nil {
					return nil, err
				}
			}
			changedPaths = append(changedPaths, change.Path)
		default:
			// Unrecognized change kind: skipped, never fatal to the batch.
		}
	}
	return changedPaths, nil
}

// keepLiveValues is writeChanges' transform for a path with ignore_keys
// entries, nil (a plain copy) for one without. It reads the live file at
// dst as the copy is about to replace it and overlays its values onto the
// repository's content (ignorekeys.Overlay). A live file that does not
// exist yet holds none, so an add leaves every ignored key out - what its
// published diff shows.
//
// A live file that exists but cannot be read, or cannot be read as YAML
// that way, has no values to keep: the repository's copy is written as it
// is, as without the option, with a warning naming the file and never its
// content. That strips whatever the app had written until it writes it
// again; a repository copy that cannot be read that way is written as it
// is too.
func keepLiveValues(keys []ignorekeys.KeyPath, relPath, dst string) func([]byte) []byte {
	if len(keys) == 0 {
		return nil
	}
	return func(repo []byte) []byte {
		live, err := os.ReadFile(dst) // #nosec G304 -- dst is a change path under the guarded config root, Lstat-checked by copyFileWith before this runs
		switch {
		case errors.Is(err, fs.ErrNotExist):
			live = nil
		case err != nil:
			slog.Warn("applier: ignore_keys not applied, the live file cannot be read; writing the repository copy as it is", "path", relPath, "error", err)
			return repo
		}
		out, err := ignorekeys.Overlay(keys, repo, live)
		if err != nil {
			slog.Warn("applier: ignore_keys not applied, writing the repository copy as it is", "path", relPath, "reason", err)
			return repo
		}
		return out
	}
}
