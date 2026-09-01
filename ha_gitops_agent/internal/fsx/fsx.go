// Package fsx resolves filesystem paths the way this add-on's containment
// guards need them resolved. A standard-library-only leaf, so applier,
// differ, gitsync and dashboards can all depend on it. Only Realpath is
// shared; the guards built on it each refuse a different set of paths and
// stay in their own packages.
package fsx

import (
	"os"
	"path/filepath"
	"strings"
)

// WriteFileAtomic writes data to path through a same-directory tmp file,
// fsyncing BEFORE the rename: on ext4/overlayfs the rename can be durable
// ahead of the data, so a power cut mid-write would otherwise leave a
// zero-length file where the previous good version was. For the small
// /data records whose loss silently resets the agent's memory
// (state.json, history.jsonl).
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm) // #nosec G304 -- callers pass fixed /data paths
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Realpath resolves p as far as symlinks allow, like Python's
// os.path.realpath: existing components are followed, a nonexistent tail
// is normalized, and it never errors. filepath.EvalSymlinks alone needs
// the whole path to exist, which the paths these guards check often do
// not - an add's destination is unwritten, a delete's source is gone.
func Realpath(p string) string {
	p = filepath.Clean(p)
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}
	dir, base := filepath.Split(p)
	dir = strings.TrimSuffix(dir, string(filepath.Separator))
	if dir == "" || dir == p {
		return p
	}
	return filepath.Join(Realpath(dir), base)
}
