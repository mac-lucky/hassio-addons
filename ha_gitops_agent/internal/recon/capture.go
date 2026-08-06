package recon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/applier"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/differ"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/gitsync"
)

// This file is the three-way classifier behind opts.CaptureLiveChanges. The
// file layer is one-way without it: the repository is the truth, and a live
// edit is drift to be overwritten. With it on, each drifting path is asked a
// second question - which SIDE moved - and only then routed.
//
// internal/differ has already established that the repository and the live
// config disagree about every path reaching here, so one further comparison
// settles it. The merge base is a COMMIT, not a stored hash: the tree the
// agent last wrote live is a real commit in the clone, so
// "git show <base>:<path>" is exactly the content it put there, and
// internal/gitsync's BlobEquivalent answers without any checkout.

// differKindAdd mirrors differ.Change.Kind's "add" literal. Named here for
// differKindDelete's reason, and read carefully: differ takes its "add"
// branch on ANY stat failure of the live file, deliberately matching
// Python's os.path.exists(), so "add" means "tracked in the repository and
// not readable live" - which is a DELETION only when the failure was
// fs.ErrNotExist. Confusing the two removes a file from the repository over
// one unreadable moment and then, next cycle, from the box.
const differKindAdd = "add"

// verdict is what the classifier decided for one drifting path.
type verdict int

const (
	// verdictApply writes the repository's version live, the one-way
	// behaviour and still the default whenever there is any doubt.
	verdictApply verdict = iota
	// verdictCapture pushes the live version onto the tracked branch.
	verdictCapture
	// verdictCaptureDelete records live's deletion of the path by removing
	// it from the tracked branch.
	verdictCaptureDelete
	// verdictConflict refuses both directions and parks a copy: the
	// repository and the live config both moved since the base, and nothing
	// here can tell which one was meant.
	verdictConflict
	// verdictDefer does nothing at all this cycle, with no event and no
	// conflict record. For the cases where the answer is genuinely not
	// knowable yet and waiting costs nothing, since the drift is still
	// there to be re-examined next time.
	verdictDefer
)

func (v verdict) String() string {
	switch v {
	case verdictApply:
		return "apply"
	case verdictCapture:
		return "capture"
	case verdictCaptureDelete:
		return "capture-delete"
	case verdictConflict:
		return "conflict"
	case verdictDefer:
		return "defer"
	}
	return "unknown"
}

// facts is everything classify needs about one drifting path, gathered by
// the caller so the decision itself stays pure and exhaustively testable.
type facts struct {
	// kind is differ.Change.Kind: "add", "update" or "delete".
	kind string
	// baseKnown is whether a merge base was resolved AND is still reachable
	// for this path. False whenever the agent has never applied or imported,
	// or the remote was rewritten out from under the recorded SHA.
	baseKnown bool
	// baseTracks is whether the base commit holds this path at all, which
	// tells a file ADDED since the base from one EDITED since it.
	baseTracks bool
	// repoMoved is whether the blob differs between the base and the fetched
	// tip. Always false when baseTracks is false and the path is not at the
	// tip either, which cannot happen for a path differ reported.
	repoMoved bool
	// liveIsBase is whether the live content still says what the base says.
	// False whenever baseTracks is false, since there is nothing to match.
	liveIsBase bool
	// liveGone is whether the live file is genuinely absent - fs.ErrNotExist
	// and nothing else. Only meaningful for kind "add"; see differKindAdd.
	liveGone bool
	// agentWroteIt is whether the path is in the manifest the last apply
	// wrote, which is the only evidence that it was EVER live. A repository
	// holds plenty that never belongs in /homeassistant - README.md, LICENSE,
	// .github/ - and none of it is excluded, so all of it reads as "tracked
	// and not live". Without this, the base tracking such a path would be
	// taken as proof the user deleted it and the capture would git rm it off
	// the branch. See differKindAdd.
	agentWroteIt bool
}

// classify routes one drifting path. The whole decision table, and the only
// place any of it is decided.
func classify(f facts) verdict {
	// No usable base: the agent has never written these files, so every live
	// byte predates it and none of it is evidence of an EDIT rather than
	// config that was always simply there. Capturing the lot is what Import
	// does, behind its own option. Apply is a strict no-regression - byte
	// for byte the behaviour before this feature existed - and it self-heals,
	// since the apply writes a fresh base.
	if !f.baseKnown {
		return verdictApply
	}

	switch f.kind {
	case differKindDelete:
		// The path left the repository and is still live. Carrying that out
		// is an apply, but only if live is still what the agent put there:
		// otherwise the user edited a file someone else deleted, and that is
		// nobody's call to make automatically. A base that does not track it
		// leaves liveIsBase false, which lands here too - correctly, since
		// there is then no evidence the live copy is the agent's at all.
		if f.liveIsBase {
			return verdictApply
		}
		return verdictConflict

	case differKindAdd:
		// Tracked in the repository, not readable live.
		if !f.liveGone {
			// Present but unreadable. Not a deletion, and not comparable
			// either, so nothing is decided and nothing is lost by waiting.
			return verdictDefer
		}
		if !f.baseTracks {
			// New in the repository since the base, and live never had it.
			// Nobody deleted anything; this is the ordinary create.
			return verdictApply
		}
		if !f.agentWroteIt {
			// The base tracks it, but the agent never put it live, so its
			// absence there is not a deletion - it is a repository file that
			// was never meant to be live in the first place, or one this agent
			// has not applied yet. Either way the write goes repository ->
			// live, which is the ordinary create again.
			return verdictApply
		}
		if f.repoMoved {
			// Deleted live, edited in the repository.
			return verdictConflict
		}
		return verdictCaptureDelete

	default:
		// "update": both sides hold the path and their contents differ.
		if !f.baseTracks {
			// Added on both sides since the base, with different content.
			// A real collision, and one nothing here can adjudicate.
			return verdictConflict
		}
		if f.repoMoved && f.liveIsBase {
			return verdictApply
		}
		if !f.repoMoved && !f.liveIsBase {
			return verdictCapture
		}
		if f.repoMoved && !f.liveIsBase {
			return verdictConflict
		}
		// Neither side moved, yet the differ says they disagree. The two
		// comparisons ran at different moments, so the honest reading is
		// that live changed and changed back while this cycle was working.
		// Doing nothing settles it: if the drift is real it is still there
		// next cycle, and if it was a race it is already gone.
		return verdictDefer
	}
}

// captureBases is the merge base each path is classified against, resolved
// once per cycle. Two sources rather than one, because a capture MOVES the
// tracked branch: from the next cycle on, the agent's own capture commit is
// what the live copy of those paths came from, and classifying them against
// the older base would read that commit as "the repository moved" and call
// the user's next edit to the same file a conflict.
type captureBases struct {
	// capture is applier.State's LastCaptureSHA, already checked reachable,
	// and captured is LastCapturePaths as a set. Empty when no capture is
	// outstanding. An apply prunes the paths it wrote (dropCapturedPaths), so
	// under an ordinary two-way setup the pair empties itself - but under
	// dry_run no apply ever runs, so it only grows, bounded by the tracked
	// file count. Each entry stays correct either way; a later capture commit
	// is a descendant of the earlier one and still carries its content.
	capture  string
	captured map[string]bool
	// fallback is the later of LastGoodSHA and LastImportSHA - both are
	// moments the repository and the live config are known to have agreed,
	// so either is a valid base, and "import once, then turn capture on"
	// works. Empty when neither is set or reachable, which classify reads as
	// "no base" and answers apply-only.
	fallback string
}

// forPath is the base to classify p against, or "" when there is none.
func (b captureBases) forPath(p string) string {
	if b.capture != "" && b.captured[p] {
		return b.capture
	}
	return b.fallback
}

// resolveBases picks this cycle's merge bases and checks each is still a
// commit the tip descends from. A recorded SHA can go unreachable - a
// force-push, a rewritten history, a re-clone - and classifying against a
// commit git cannot read would fail every path; answering "no base" instead
// sends the cycle down the apply-only path it took before this feature
// existed, which the next successful apply then repairs.
//
// Reachability alone is not enough, which a force-push of the tracked branch
// shows on real hardware: the old commit is still in the object database, so
// it reads as reachable, but it now sits on an ABANDONED line. Diffing it
// against the tip then reports every path that differs across the divergence
// as "the repository moved", and any of those the user also edited live
// becomes a conflict that never happened. An orphaned base is no base.
func (r *Reconciler) resolveBases(ctx context.Context, tip string, state applier.State) captureBases {
	var bases captureBases

	if len(state.LastCapturePaths) > 0 && r.usableBase(ctx, state.LastCaptureSHA, tip) {
		bases.capture = state.LastCaptureSHA
		bases.captured = pathSet(state.LastCapturePaths)
	}

	// Both an apply and an import make the repository and the live config
	// agree at a known instant, so either commit is a valid base - but they
	// are recorded independently, and importLive deliberately never advances
	// LastGoodSHA. Whichever is LATER is the one that still describes live,
	// so prefer the descendant rather than a fixed order: on an install that
	// applied before it imported, the older LastGoodSHA would read every path
	// the import moved as "the repository moved" and call the next live edit
	// to those files a conflict.
	good := r.usableBase(ctx, state.LastGoodSHA, tip)
	imported := r.usableBase(ctx, state.LastImportSHA, tip)
	switch {
	case good && imported:
		bases.fallback = state.LastGoodSHA
		if ok, err := r.git.IsAncestor(ctx, state.LastGoodSHA, state.LastImportSHA); err == nil && ok {
			bases.fallback = state.LastImportSHA
		}
	case good:
		bases.fallback = state.LastGoodSHA
	case imported:
		bases.fallback = state.LastImportSHA
	}
	return bases
}

// usableBase reports whether sha can serve as a merge base for a cycle at
// tip: present in the clone, and an ancestor of tip so that diffing the two
// describes what the repository DID rather than how two lines of history
// differ. The empty SHA and every error fold into "no", which all mean the
// same thing to a caller choosing a base.
func (r *Reconciler) usableBase(ctx context.Context, sha, tip string) bool {
	if sha == "" {
		return false
	}
	if ok, err := r.git.CommitReachable(ctx, sha); err != nil || !ok {
		return false
	}
	// A commit is its own ancestor, so base == tip needs no special case.
	ok, err := r.git.IsAncestor(ctx, sha, tip)
	return err == nil && ok
}

// captureRouting is one cycle's classification, split by what happens next.
type captureRouting struct {
	// apply is the residual plan: the only changes that reach the applier.
	apply []differ.Change
	// capture is what gets pushed to the tracked branch, deletions included
	// (gitsync.CommitBack's staging reads Kind for diagnostics only and
	// decides removal from the live filesystem itself).
	capture []differ.Change
	// conflicts is refused in both directions and parked.
	conflicts []differ.Change
	// deferred is neither, and deliberately silent.
	deferred []differ.Change
}

// classifyChanges routes every drifting path. Cheap where it matters: with
// nothing drifting the caller never reaches here at all, and a base equal to
// the tip settles "did the repository move" for every path without launching
// git. It still asks LiveFactsAt per DRIFTING path, so the cost scales with
// how much drifted rather than with the tracked-file count.
// written is the previous manifest as a set: the paths the last apply put
// live, and so the only ones whose absence live can mean a deletion.
func (r *Reconciler) classifyChanges(
	ctx context.Context, tip string, changes []differ.Change, bases captureBases, written map[string]bool,
) captureRouting {
	var routing captureRouting

	// Which paths the repository moved, PER BASE - not merged into one set.
	// At most two bases are ever in play, and they disagree on exactly the
	// paths that matter: a captured path has moved since LastGoodSHA
	// precisely because the agent's own capture moved it, and reading that
	// as "the repository moved" is the false conflict the override exists to
	// prevent. A base equal to the tip answers without launching git.
	movedByBase := map[string]map[string]bool{}
	for _, base := range []string{bases.capture, bases.fallback} {
		if base == "" {
			continue
		}
		if _, done := movedByBase[base]; done {
			continue
		}
		if base == tip {
			movedByBase[base] = map[string]bool{}
			continue
		}
		moved, err := r.git.ChangedBetween(ctx, base, tip)
		if err != nil {
			// Cannot tell which side moved, so nothing may be captured this
			// cycle. Apply-only is the pre-feature behaviour, not a loss.
			slog.Warn("recon: capture: could not compare the merge base with the tip, applying only", "base", base, "error", err)
			routing.apply = changes
			return routing
		}
		movedByBase[base] = pathSet(moved)
	}

	for _, change := range changes {
		base := bases.forPath(change.Path)
		f := facts{
			kind:         change.Kind,
			baseKnown:    base != "",
			repoMoved:    movedByBase[base][change.Path],
			agentWroteIt: written[change.Path],
		}

		if f.baseKnown {
			live, err := r.git.LiveFactsAt(ctx, base, ConfigRoot, change.Path)
			switch {
			case errors.Is(err, gitsync.ErrNotComparable):
				// Too large to hold beside the base blob. Neither side is
				// claimed and the drift stays visible.
				routing.deferred = append(routing.deferred, change)
				continue
			case err != nil:
				// The BASE could not be read - most often an encrypted blob
				// this agent has no key for. Fail closed: overwriting content
				// nobody could check is the one outcome worth refusing.
				slog.Warn("recon: capture: could not read the merge base, treating as a conflict", "path", change.Path, "error", err)
				routing.conflicts = append(routing.conflicts, change)
				continue
			}
			f.baseTracks = live.BaseTracks
			f.liveIsBase = live.MatchesBase
			f.liveGone = live.Gone
		}

		switch classify(f) {
		case verdictCapture, verdictCaptureDelete:
			routing.capture = append(routing.capture, change)
		case verdictConflict:
			routing.conflicts = append(routing.conflicts, change)
		case verdictDefer:
			routing.deferred = append(routing.deferred, change)
		default:
			routing.apply = append(routing.apply, change)
		}
	}
	return routing
}

// captureLiveChanges is the capture phase, run at the tail of a cycle that
// got far enough to have a plan, and returns the changes the apply may still
// act on. With the option off it is the identity function.
//
// A captured path leaves the plan whether or not its push succeeded. That is
// the whole safety property: the classifier decided the live copy is the
// truth there, so applying the repository's version would destroy exactly
// the edit a failed capture just failed to save. The drift stays visible and
// the next cycle tries again.
func (r *Reconciler) captureLiveChanges(
	ctx context.Context, tip string, changes []differ.Change, state applier.State,
) (apply []differ.Change, unresolved int) {
	// Nothing drifts, so nothing conflicts - and with the option off there is
	// no writer left that could ever clear a record made while it was on, so
	// turning it off has to clear one too. Otherwise backing out of the
	// feature would exclude those paths from every future apply forever,
	// while the card kept promising they clear themselves.
	if !r.opts.CaptureLiveChanges || len(changes) == 0 {
		r.clearStandingConflicts(state)
		return changes, 0
	}

	routing := r.classifyChanges(ctx, tip, changes, r.resolveBases(ctx, tip, state), pathSet(state.Manifest))
	if len(routing.deferred) > 0 {
		// Named, not counted: a deferred path is in no card and no plan, so
		// this line is the only place it appears at all.
		slog.Info("recon: capture: deferring path(s) that could not be classified",
			"paths", strings.Join(changePaths(routing.deferred), ", "))
	}

	// One state write for the whole phase: the next cycle's classifier reads
	// the capture commit and the conflict list together, and two saves would
	// let a restart land between them.
	next := state
	var failures []string
	// Everything held back from the apply without being resolved. The cycle
	// reports drift on the strength of it, so a failed capture cannot leave
	// the dashboard saying "in sync" over an edit still waiting to be saved.
	unresolved = len(routing.conflicts) + len(routing.deferred)

	if len(routing.capture) > 0 {
		result, err := r.git.CaptureFiles(ctx, driftFiles(routing.capture), ConfigRoot)
		switch {
		case err != nil:
			unresolved += len(routing.capture)
			failures = append(failures, fmt.Sprintf(
				"could not push %d captured live change(s) to %s: %v - those files will not be applied over until it succeeds",
				len(routing.capture), r.opts.Branch, err))
		case result.CommitSHA != "":
			next.LastCaptureSHA = result.CommitSHA
			next.LastCaptureUTC = utcNowISO()
			// UNION, not replacement: a later capture commit is a descendant
			// of the earlier one and still carries every path it captured, so
			// it is a valid base for all of them. Replacing the list would
			// drop an older path back to LastGoodSHA, where the agent's own
			// earlier capture reads as "the repository moved" - the very
			// false conflict this override exists to prevent. The list is
			// pruned by applies instead (see dropCapturedPaths).
			next.LastCapturePaths = unionPaths(state.LastCapturePaths, result.Paths)
			r.logEvent(fmt.Sprintf("captured %d live change(s) to %s: %s",
				len(result.Paths), r.opts.Branch, strings.Join(result.Paths, ", ")))
		}
	}

	next.ConflictedPaths = changePaths(routing.conflicts)
	// Only when the SET changed. A conflict is by definition something only a
	// person clears, so it is still here next cycle and every cycle after -
	// re-parking each time would push a new gitops/conflict-<second> branch
	// to the user's remote every interval forever, and repeat the same line
	// until it had evicted the whole 200-entry feed. maybeAutoCommitDriftBack
	// makes exactly this check against LastDriftBackHash, for exactly this
	// reason. Comparing the set rather than hashing it: it is already sorted,
	// already in hand, and unlike a drift set its CONTENT does not matter -
	// the parked branch is a copy, not a proposal.
	if len(routing.conflicts) > 0 && !slices.Equal(next.ConflictedPaths, state.ConflictedPaths) {
		branch, err := r.git.ParkConflicts(ctx, driftFiles(routing.conflicts), ConfigRoot, tip, time.Now())
		if err != nil {
			// The verdict stands regardless: refusing to touch the path in
			// either direction is the protection, the branch is only the copy.
			failures = append(failures, fmt.Sprintf("could not park %d conflicted live copy/copies: %v", len(routing.conflicts), err))
		} else {
			next.LastConflictBranch = branch
			next.LastConflictUTC = utcNowISO()
		}
		r.logEvent(fmt.Sprintf("conflict on %d path(s), left untouched in both directions: %s",
			len(routing.conflicts), strings.Join(next.ConflictedPaths, ", ")))
	}

	if len(failures) > 0 {
		r.noteCaptureFailure(strings.Join(failures, "; "))
	} else {
		r.clearCaptureFailure()
	}

	// Only when something actually moved. The ordinary "the repository moved,
	// apply it" cycle routes everything to verdictApply and leaves next
	// identical to state, and rewriting the largest file the agent owns on
	// flash-backed hardware every drifting cycle buys nothing.
	if !sameCaptureState(state, next) {
		if err := r.applier.StateSave(next); err != nil {
			// The capture is already pushed; losing the record of it only
			// costs the next cycle its merge-base override, which reads as a
			// conflict rather than as data loss.
			slog.Warn("recon: capture: could not persist capture state", "error", err)
		}
		r.refreshStateMirrors(next)
	}
	return routing.apply, unresolved
}

// sameCaptureState reports whether a phase changed anything worth persisting.
// Only the fields this phase writes: everything else in the two values came
// from the same StateLoad.
func sameCaptureState(a, b applier.State) bool {
	return a.LastCaptureSHA == b.LastCaptureSHA &&
		a.LastConflictBranch == b.LastConflictBranch &&
		slices.Equal(a.ConflictedPaths, b.ConflictedPaths) &&
		slices.Equal(a.LastCapturePaths, b.LastCapturePaths)
}

// clearStandingConflicts drops a conflict record whose two sides have since
// agreed. A no-op when there is nothing recorded, so a quiet agent does not
// rewrite state.json every interval.
func (r *Reconciler) clearStandingConflicts(state applier.State) {
	if len(state.ConflictedPaths) == 0 {
		return
	}
	next := state
	next.ConflictedPaths = nil
	if err := r.applier.StateSave(next); err != nil {
		slog.Warn("recon: capture: could not clear the conflict record", "error", err)
		return
	}
	r.logEvent(fmt.Sprintf("conflict cleared on %d path(s): the repository and live agree again", len(state.ConflictedPaths)))
	r.refreshStateMirrors(next)
}

// driftFiles adapts a change set for internal/gitsync, which cannot import
// internal/differ (differ imports gitsync for Excluded).
func driftFiles(changes []differ.Change) []gitsync.DriftFile {
	files := make([]gitsync.DriftFile, len(changes))
	for i, c := range changes {
		files[i] = gitsync.DriftFile{Path: c.Path, Kind: c.Kind}
	}
	return files
}

// pathSet is the inverse of sortedStringKeys: a lookup table over a path
// list, which four call sites in this file all wanted.
func pathSet(paths []string) map[string]bool {
	set := make(map[string]bool, len(paths))
	for _, p := range paths {
		set[p] = true
	}
	return set
}

func changePaths(changes []differ.Change) []string {
	paths := make([]string, 0, len(changes))
	for _, c := range changes {
		paths = append(paths, c.Path)
	}
	sort.Strings(paths)
	return paths
}

// unionPaths merges two path lists, sorted and deduplicated.
func unionPaths(a, b []string) []string {
	seen := make(map[string]bool, len(a)+len(b))
	for _, list := range [][]string{a, b} {
		for _, p := range list {
			seen[p] = true
		}
	}
	return sortedStringKeys(seen)
}

// dropCapturedPaths removes paths an apply has just written from the
// merge-base override, since LastGoodSHA now describes them better than the
// capture commit does, and clears the commit once nothing is left pointing
// at it. Called from the apply, which is the only thing that can make a
// captured path the repository's again.
func dropCapturedPaths(state *applier.State, applied []string) {
	if state.LastCaptureSHA == "" || len(state.LastCapturePaths) == 0 {
		return
	}
	written := pathSet(applied)
	kept := state.LastCapturePaths[:0:0]
	for _, p := range state.LastCapturePaths {
		if !written[p] {
			kept = append(kept, p)
		}
	}
	state.LastCapturePaths = kept
	if len(kept) == 0 {
		state.LastCaptureSHA = ""
		state.LastCaptureUTC = ""
	}
}

// dropConflicted removes every conflicted path from a plan. Silent: the
// capture phase already logged the conflict and the card names it, so a
// second line per apply would say nothing new.
func dropConflicted(changes []differ.Change, conflicted []string) []differ.Change {
	if len(conflicted) == 0 || len(changes) == 0 {
		return changes
	}
	refused := pathSet(conflicted)
	kept := changes[:0:0]
	for _, c := range changes {
		if !refused[c.Path] {
			kept = append(kept, c)
		}
	}
	return kept
}

// noteCaptureFailure logs one event on ENTERING failure and nothing on the
// ones after, versionrecord.go's rule and for its reason doubled: capture
// runs every cycle, so a genuinely unpushable repository - a bad token, a
// protected branch - would fill the 200-entry feed within hours.
//
// It never sets lastError or flips the state to StateError. A repository
// write failing says nothing about whether live matches the repository,
// which is what those two mean. It does have a consequence the version
// record does not, so the message states it.
func (r *Reconciler) noteCaptureFailure(reason string) {
	slog.Warn("recon: capture: " + reason)
	first := false
	r.withMu(func() {
		first = !r.captureFailed
		r.captureFailed = true
	})
	if first {
		r.logEvent("warning: " + reason)
	}
}

// clearCaptureFailure logs the recovery once, for noteCaptureFailure's
// reason in reverse.
func (r *Reconciler) clearCaptureFailure() {
	recovered := false
	r.withMu(func() {
		recovered = r.captureFailed
		r.captureFailed = false
	})
	if recovered {
		r.logEvent("capture is working again")
	}
}
