package recon

import (
	"fmt"
	"slices"
	"strings"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/gitsync"
)

// warnIgnoredKeysUntracked puts one event on the feed when the set of
// ignore_keys files the repository does not track changes: such an entry
// does nothing at all, which is silent otherwise - a .yml written for a
// .yaml, or a file since removed from the repository. Files only, never
// keys. In memory like warnLeftBehind's set, so an add-on restart reports
// a standing set once more; a set that empties is forgotten quietly.
func (r *Reconciler) warnIgnoredKeysUntracked(tracked []string) {
	files := gitsync.IgnoredKeyFiles()
	if len(files) == 0 && len(r.ignoredUntrackedWarned) == 0 {
		return
	}
	trackedSet := pathSet(tracked)
	var untracked []string
	for _, f := range files {
		if !trackedSet[f] {
			untracked = append(untracked, f)
		}
	}
	if slices.Equal(untracked, r.ignoredUntrackedWarned) {
		return
	}
	r.ignoredUntrackedWarned = untracked
	if len(untracked) == 0 {
		return
	}
	r.logWarn(fmt.Sprintf("ignore_keys names %d file(s) the repository does not track, so those entries do nothing: %s",
		len(untracked), strings.Join(untracked, ", ")))
}
