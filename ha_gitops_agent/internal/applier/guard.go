package applier

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/fsx"
)

// guardChangePath is the path-traversal guard for APPLY-time changes: the
// containment checks of guardPathContained, plus the exclusion list.
//
// Kept separate from gitsync.guardDriftPath and
// dashboards.containDashboardConfigPath, which share only the containment
// arithmetic: this one alone consults Config.IsExcluded, and its refusals
// are phrased for the ingress dashboard.
func guardChangePath(cfg Config, path, configRootReal string) error {
	if cfg.IsExcluded(path) {
		return fmt.Errorf("refusing to touch excluded path: %s", path)
	}
	return guardPathContained(path, configRootReal)
}

// guardPathContained errors if path is absolute, empty, the root itself,
// contains ".." after normalization, or resolves (via fsx.Realpath,
// catching symlink tricks too) outside configRootReal. No exclusion
// check: RollbackFrom uses this alone, because a stash manifest records
// what the apply actually touched, and an exclusion pattern that changed
// since then (age_key cleared, an older binary's pattern set) must not
// strand the restore of a file the apply provably wrote.
func guardPathContained(path, configRootReal string) error {
	if filepath.IsAbs(path) {
		return fmt.Errorf("refusing to touch absolute path: %s", path)
	}

	normalized := filepath.Clean(path)
	// "." covers the empty path too; without this, a manifest entry of
	// "" or "." marked "absent" would reach os.Remove(configRoot).
	if normalized == "." || normalized == ".." || strings.HasPrefix(normalized, ".."+string(filepath.Separator)) {
		return fmt.Errorf("refusing to touch path outside config root: %s", path)
	}

	destReal := fsx.Realpath(filepath.Join(configRootReal, path))
	if destReal != configRootReal && !strings.HasPrefix(destReal, configRootReal+string(filepath.Separator)) {
		return fmt.Errorf("path escapes config root: %s", path)
	}
	return nil
}

// copyFile copies src to dst, preserving src's file mode. Used for both
// stashing (config -> stash) and writing (repo -> config).
//
// Both ends are Lstat-ed, because guardChangePath validates only the
// destination path STRING: a tracked symlink under repoRoot would otherwise
// copy an arbitrary readable file into live config (the repo-to-config
// counterpart of internal/differ.isRegularFile), and a symlinked dst would
// have os.WriteFile follow it and overwrite whatever it points at.
func copyFile(src, dst string) error {
	return copyFileTransformed(src, dst, "", nil)
}

// copyFileTransformed is copyFile with an optional content transform on
// what it read from src - decryption, in practice (see
// Config.TransformRepoFile). rel is the repository-relative path the
// transform needs to know how much of the file was encrypted.
//
// Only writeChanges' repository -> config direction passes one: stashing
// and rollback must stay byte-identical copies, or a rollback would restore
// something the user never had. The transform runs between the Lstat guards
// and the write, so a failure leaves dst untouched.
func copyFileTransformed(src, dst, rel string, transform TransformRepoFileFunc) error {
	srcInfo, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if !srcInfo.Mode().IsRegular() {
		return fmt.Errorf("refusing to copy non-regular source file: %s", src)
	}
	if dstInfo, statErr := os.Lstat(dst); statErr == nil && !dstInfo.Mode().IsRegular() {
		return fmt.Errorf("refusing to write through non-regular destination: %s", dst)
	}

	data, err := os.ReadFile(src) // #nosec G304 -- src is a change path resolved under a guarded root, and just Lstat-confirmed regular above
	if err != nil {
		return err
	}
	if transform != nil {
		data, err = transform(rel, data)
		if err != nil {
			return err
		}
	}
	// The Random variant, not WriteFileAtomic: dst sits in live config,
	// where a fixed "<name>.tmp" can be a real, unmanaged user file. The
	// atomicity keeps a crash mid-write from leaving dst half-written; the
	// stash still covers a crash between files.
	return fsx.WriteFileAtomicRandom(dst, data, srcInfo.Mode().Perm())
}
