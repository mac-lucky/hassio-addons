package applier

import (
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/gitsync"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/ignorekeys"
)

// Change is one file-level difference between the checked-out repo tree and
// the live config that Apply should act on. Path is relative to /config and
// equally to the repo root; Kind is ChangeAdd (tracked, missing from
// config), ChangeUpdate (tracked, content differs) or ChangeDelete
// (previously applied by this agent, no longer tracked); DiffText is
// human-readable and Apply never inspects it.
//
// Declared here rather than imported from internal/differ, which computes
// it, so this package stays independently buildable and testable.
type Change struct {
	Path     string
	Kind     string
	DiffText string
}

// Change kinds.
const (
	ChangeAdd    = "add"
	ChangeUpdate = "update"
	ChangeDelete = "delete"
)

// IsExcludedFunc reports whether p (a repo/config-relative path) must
// never be touched by Apply. Config.IsExcluded holds the check Apply
// actually uses; DefaultIsExcluded is the production implementation.
type IsExcludedFunc func(p string) bool

// IgnoredKeysFunc returns the ignore_keys key paths of p, a
// repo/config-relative path, nil when it has none. Config.IgnoredKeys holds
// the one Apply uses; gitsync.IgnoredKeys is the production one.
type IgnoredKeysFunc func(p string) []ignorekeys.KeyPath

// DefaultIsExcluded is gitsync.Excluded - the same root-anchored gitops/
// plus glob/segment algorithm internal/differ and internal/gitsync use for
// the identical purpose, rather than a second copy here.
func DefaultIsExcluded(p string) bool {
	return gitsync.Excluded(p)
}
