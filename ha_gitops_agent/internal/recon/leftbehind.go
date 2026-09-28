package recon

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/applier"
)

// leftBehindEventPaths caps how many paths warnLeftBehind names in the feed
// event; removing an imported directory can leave hundreds behind, and the
// add-on log gets the full list separately.
const leftBehindEventPaths = 20

// liveRegularFile reports whether p, relative to ConfigRoot, is a regular
// file live. A variable so a test can answer without a real /homeassistant.
var liveRegularFile = func(p string) bool {
	info, err := os.Lstat(filepath.Join(ConfigRoot, p))
	return err == nil && info.Mode().IsRegular()
}

// leftBehindPaths returns, sorted, the paths prevTracked (files of a commit
// that described live - the last apply or the last import) has and tracked
// (the new commit's) does not, that are absent from manifest and still
// regular files live. Those are the removals an apply will NOT carry out:
// differ.Compute only ever plans a delete for a manifest path, and a file
// that arrived by Import or already matched live was never written by this
// agent, so it never entered the manifest.
func leftBehindPaths(prevTracked, tracked, manifest []string, live func(string) bool) []string {
	keep := make(map[string]bool, len(tracked)+len(manifest))
	for _, p := range tracked {
		keep[p] = true
	}
	for _, p := range manifest {
		keep[p] = true
	}
	var paths []string
	for _, p := range prevTracked {
		if !keep[p] && live(p) {
			keep[p] = true // a path both bases track is reported once
			paths = append(paths, p)
		}
	}
	slices.Sort(paths)
	return paths
}

// warnLeftBehind puts one event on the feed for each new set of
// leftBehindPaths, so a rename or removal in the repository that leaves the
// old file running live is not silent. The old files come from both the last
// applied and the last imported commit: an import never moves LastGoodSHA,
// and it usually plans nothing, so without LastImportSHA every file it
// brought in would be invisible here. A base that is gone from the clone or
// off this branch's history (a force-push) is skipped, as capture does.
//
// The set is remembered across cycles (a removal-only commit plans nothing,
// so no apply moves the bases past it and every cycle finds the same set),
// kept when a base cannot be read, and forgotten once it empties. In memory
// only, so an add-on restart reports a standing set once more.
// Informational; never fails the cycle.
func (r *Reconciler) warnLeftBehind(ctx context.Context, state applier.State, sha string, tracked []string) {
	var prev []string
	var bases []string
	for _, base := range []string{state.LastGoodSHA, state.LastImportSHA} {
		// The tip's own tree has nothing removed from it.
		if base == sha || slices.Contains(bases, base) || !r.usableBase(ctx, base, sha) {
			continue
		}
		bases = append(bases, base)
		files, err := r.git.TrackedFiles(ctx, base)
		if err != nil {
			// Keep the remembered set: a partial read would shrink it and
			// the next good read would report the same files again.
			slog.Debug("recon: left-behind check skipped", "base", base, "error", err)
			return
		}
		prev = append(prev, files...)
	}
	paths := leftBehindPaths(prev, tracked, state.Manifest, liveRegularFile)
	key := strings.Join(paths, "\x00")
	if key == r.leftBehindWarned {
		return
	}
	r.leftBehindWarned = key
	if len(paths) == 0 {
		return
	}
	listed := strings.Join(paths, ", ")
	if len(paths) > leftBehindEventPaths {
		listed = fmt.Sprintf("%s and %d more (full list in the add-on log)",
			strings.Join(paths[:leftBehindEventPaths], ", "), len(paths)-leftBehindEventPaths)
		slog.Info("recon: files left in place", "paths", paths)
	}
	r.logWarn(fmt.Sprintf(
		"left in place: %d file(s) removed from the repository stay live because this agent never wrote them - delete by hand if unwanted: %s",
		len(paths), listed))
}
