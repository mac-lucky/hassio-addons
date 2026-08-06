package applier

import (
	"log/slog"
	"os"
	"path/filepath"
	"sort"
)

// PruneStashDirs deletes old per-apply stash directories under
// cfg.BackupRoot, keeping the keep most recent plus exclude, and is called
// after every successful apply (see recon's pruneKeep) since nothing else
// bounds their growth on a small /data volume. Directory names sort
// chronologically (makeStashDir's timestamp format), so reverse sorting the
// listing finds the newest.
//
// exclude is the stash a pending rollback still points to: never removed
// even once aged out, so the Rollback button cannot point at a directory
// that no longer exists. Best-effort - list and remove failures are logged.
func PruneStashDirs(cfg Config, keep int, exclude string) {
	info, err := os.Stat(cfg.BackupRoot)
	if err != nil || !info.IsDir() {
		return
	}

	entries, err := os.ReadDir(cfg.BackupRoot)
	if err != nil {
		slog.Warn("applier: prune_stash_dirs could not list", "dir", cfg.BackupRoot, "error", err)
		return
	}

	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))

	keepNames := map[string]bool{}
	for i, n := range names {
		if i < keep {
			keepNames[n] = true
		}
	}
	if exclude != "" {
		keepNames[filepath.Base(filepath.Clean(exclude))] = true
	}

	for _, name := range names {
		if keepNames[name] {
			continue
		}
		target := filepath.Join(cfg.BackupRoot, name)
		if err := os.RemoveAll(target); err != nil {
			slog.Warn("applier: prune_stash_dirs could not remove", "dir", target, "error", err)
		}
	}
}
