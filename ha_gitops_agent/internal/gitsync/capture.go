package gitsync

import (
	"context"
	"errors"
	"fmt"
)

// This file is the third and last operation that writes to the tracked
// branch, and the only one that writes CONFIG there. Import seeds a whole
// tree on a button press; RecordFile keeps one agent-owned bookkeeping file
// current; CaptureFiles asserts that the live config is the truth for a set
// of paths and moves opts.Branch to say so. That is a much stronger claim
// than either of the others makes, which is why it is off by default and
// why the caller must have established, three-way against a merge base,
// that the repository did not also move for those paths.

// captureBranch is the local throwaway ref CaptureFiles commits on. Fixed
// rather than timestamped, for recordBranch's reason: the refspec maps it
// onto the tracked branch and it is never pushed or seen under this name,
// and "checkout -B" force-creates it, so a leftover from a crashed run is
// replaced rather than colliding.
const captureBranch = "gitops/capture"

// CaptureCommitMessage is the fixed message every capture commit carries.
// Fixed rather than describing the change, like DriftCommitMessage, since
// the diff already says what moved.
const CaptureCommitMessage = "capture: sync live home assistant changes"

// CaptureResult is what one capture actually landed. A zero CommitSHA with a
// nil error is the ordinary "nothing to do" outcome: everything asked for
// was gitignored, unreadable, or already matched the tip, so no commit was
// made and nothing was pushed.
type CaptureResult struct {
	// CommitSHA is the commit now at the tip of opts.Branch. It is the MERGE
	// BASE for every path in Paths from here on: a capture moves the tracked
	// branch, so classifying those paths against the older base would read
	// the agent's own commit as "the repository moved" and call the user's
	// next live edit a conflict.
	CommitSHA string
	// BaseSHA is the tip this was built on.
	BaseSHA string
	// Paths is what the commit actually carries, which is not necessarily
	// what was asked for: a gitignored path is skipped, and one that turned
	// out to be present-but-uncapturable stages nothing. A caller recording
	// where a path's base now lives must use THIS list, not its request.
	Paths []string
}

// CaptureFiles commits the CURRENT LIVE state of every path in files onto
// the TRACKED branch as one commit and pushes it, reporting that commit and
// the paths it carries. This is the write-back half of bidirectional sync:
// where CommitBack parks live drift on a throwaway branch for a human to
// review, this one moves opts.Branch itself.
//
// Staging is stageDrift's, unchanged and deliberately not reimplemented:
// each path is re-read under configRoot, encrypted in place when SOPS calls
// for it, refused if it is excluded or secret-shaped (refuseUnsyncablePath),
// resolved through guardDriftPath at both ends so a symlink cannot make it
// read or write outside its root, skipped rather than fatal when gitignored,
// and staged as a REMOVAL only when it is genuinely absent - fs.ErrNotExist
// alone, via liveFileIsGone, never a file that merely could not be read. The
// managed .sops.yaml rides along in the pathspec, so a commit carrying a
// newly encrypted secrets.yaml also carries the config to decrypt it.
//
// Exactly the staged paths are committed, enforced at the COMMIT ("--only --
// <paths...>") rather than trusted to the staging, for recordFileAt's
// reason: this can be reached with a dirty index left by an earlier
// operation whose best-effort checkout restore did not land, and a bare
// commit would push that leftover onto the tracked branch.
//
// The tip is fetched inside this call and the push carries no --force, no
// --force-with-lease and no "+" prefix, so git's own fast-forward check is
// what stops a concurrent push being clobbered, exactly as in Import and
// RecordFile. A rejection IS retried, once, on the freshly fetched tip - and
// the retry re-stages from live rather than replaying the tree it already
// built, since the whole claim being made is "live is the truth right now".
// A second rejection gives up and the next cycle starts over; nothing was
// written either way, so the caller's remaining duty is simply to keep those
// paths OUT of the apply that follows, or it will overwrite the very edits
// this failed to save.
//
// Workdir is left back in the detached checkout it was found in, with a
// pristine tree, so the applier running next reads the tip rather than this
// commit. Callers serialize this against every other GitSync method (recon's
// opLock). What protects the captured paths from the apply is not the lock
// but the caller REMOVING them from its plan before publishing it - recon's
// unattended cycle happens to hold one lock across both, its web and webhook
// paths do not, and both are safe for that structural reason.
func (g *GitSync) CaptureFiles(ctx context.Context, files []DriftFile, configRoot string) (CaptureResult, error) {
	if err := g.guardWriteBranch(ctx, "capture"); err != nil {
		return CaptureResult{}, err
	}
	if len(files) == 0 {
		return CaptureResult{}, fmt.Errorf("gitsync: capture: no files given")
	}

	tip, err := g.Fetch(ctx)
	if err != nil {
		return CaptureResult{}, err
	}
	result, err := g.captureFilesAt(ctx, tip, files, configRoot)
	if !errors.Is(err, errTrackedPushRejected) {
		return result, err
	}

	newTip, fetchErr := g.Fetch(ctx)
	if fetchErr != nil {
		return CaptureResult{}, fetchErr
	}
	if newTip == tip {
		// Refused without the branch moving, so it is not a lost race - a
		// protected branch or a token without push rights - and a second
		// attempt would be refused in exactly the same way.
		return CaptureResult{}, err
	}

	result, err = g.captureFilesAt(ctx, newTip, files, configRoot)
	if errors.Is(err, errTrackedPushRejected) {
		return CaptureResult{}, fmt.Errorf(
			"gitsync: capture: %s moved on the remote twice while capturing %d live change(s) - giving up rather than racing it again",
			g.Opts.Branch, len(files))
	}
	return result, err
}

// captureFilesAt is one attempt at capturing files onto the commit tip.
// Returns a zero CommitSHA with a nil error when there was nothing to
// commit, and an error wrapping errTrackedPushRejected when the push lost a
// race.
func (g *GitSync) captureFilesAt(ctx context.Context, tip string, files []DriftFile, configRoot string) (CaptureResult, error) {
	// Restores to wherever the worktree already was - the tip this cycle
	// checked out and is about to apply from - not to the tip just fetched.
	restore, err := g.enterThrowawayBranch(ctx, captureBranch, tip)
	if err != nil {
		return CaptureResult{}, err
	}
	defer restore()

	staged, err := g.stageDrift(ctx, "capture", files, configRoot)
	if err != nil {
		return CaptureResult{}, err
	}
	if len(staged.Paths) == 0 {
		// Nothing capturable after the per-path guards had their say. Not an
		// error: the caller has nothing to record, and the paths it asked
		// about stay drift it will be shown again next cycle.
		return CaptureResult{BaseSHA: tip}, nil
	}

	args := append([]string{"commit", "--quiet", "-m", CaptureCommitMessage, "--only", "--"}, staged.pathspec()...)
	if _, err := g.runGit(ctx, args, "", commitIdentityEnv()); err != nil {
		if isNothingToCommitError(err) {
			// Staged content turned out identical to the tip - a mode change,
			// say. Reporting a commit that did not happen would have the
			// caller record a merge base that does not exist.
			return CaptureResult{BaseSHA: tip}, nil
		}
		return CaptureResult{}, err
	}

	// Read before the push, while HEAD is still the commit just made.
	commitSHA := g.CurrentSHA(ctx)
	if commitSHA == "" {
		return CaptureResult{}, fmt.Errorf("gitsync: capture: committed but could not resolve the new commit")
	}

	refspec := captureBranch + ":refs/heads/" + g.Opts.Branch
	if _, err := g.runGit(ctx, []string{"push", g.Opts.RepoURL, refspec}, "", g.credentialEnv()); err != nil {
		if isNonFastForwardError(err) {
			return CaptureResult{}, fmt.Errorf("gitsync: capture: %w: %s", errTrackedPushRejected, g.Opts.Branch)
		}
		return CaptureResult{}, err
	}

	return CaptureResult{CommitSHA: commitSHA, BaseSHA: tip, Paths: staged.Paths}, nil
}
