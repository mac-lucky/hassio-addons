// Package fsx resolves filesystem paths the way this add-on's containment
// guards need them resolved. A standard-library-only leaf, so applier,
// differ, gitsync and dashboards can all depend on it. Only Realpath is
// shared; the guards built on it each refuse a different set of paths and
// stay in their own packages.
package fsx

import (
	"path/filepath"
	"strings"
)

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
