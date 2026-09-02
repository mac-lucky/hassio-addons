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
// (state.json, history.jsonl). The tmp name is the fixed "<path>.tmp" -
// fine for the agent's own /data, where no real file carries that name;
// destinations in user territory need WriteFileAtomicRandom.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	f, err := os.OpenFile(path+".tmp", os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm) // #nosec G304 -- callers pass fixed /data paths
	if err != nil {
		return err
	}
	return commitTemp(f, path, data, perm)
}

// WriteFileAtomicRandom is WriteFileAtomic through a randomly named temp
// file, for destinations in user territory (live HA config), where a
// fixed "<name>.tmp" could clobber a real, unmanaged user file.
func WriteFileAtomicRandom(path string, data []byte, perm os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".gitops-agent-*.tmp")
	if err != nil {
		return err
	}
	return commitTemp(f, path, data, perm)
}

// commitTemp finishes an atomic write over the open temp file f: exact
// perm (chmod, immune to umask), write, fsync, close, rename over path.
// The temp is cleaned up on every failure path here rather than by
// callers - a stale one is litter in a volume Supervisor backs up, and
// can hold credential-bearing state. Both deferred calls are no-ops on
// success: the rename moved the temp away and Close already ran.
func commitTemp(f *os.File, path string, data []byte, perm os.FileMode) error {
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()
	defer func() { _ = f.Close() }()
	if err := f.Chmod(perm); err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
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
