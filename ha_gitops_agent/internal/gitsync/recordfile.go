package gitsync

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// This file is the second, much narrower operation that writes to the
// tracked branch. Import seeds a whole config tree on a button press;
// RecordFile keeps ONE agent-owned file (today: the add-on versions
// internal/recon observes) up to date on every cycle that changes it. So it
// runs on the interval and must be silent when nothing changed - a commit
// per poll would bury the user's history - must survive losing a race with
// their push, and commits exactly one path via recordFileAt's "--only"
// rather than trusting the index.

// recordBranch is the local throwaway ref RecordFile commits on. Fixed
// rather than timestamped, because the refspec maps it onto the tracked
// branch and it is never pushed or seen; "checkout -B" force-creates it, so
// a leftover from a crashed run is replaced rather than colliding.
const recordBranch = "gitops/record"

// RecordFile commits content to relPath on the TRACKED branch and pushes
// it, reporting whether it had to commit anything. A false with a nil error
// is the ordinary outcome: the blob already matches, so nothing was
// committed or pushed and the working tree was not touched.
//
// Exactly one path is committed, enforced at the COMMIT ("--only --
// <path>") rather than trusted to the staging: a caller reaches this with a
// dirty index whenever an earlier operation's best-effort checkout restore
// did not land, and a bare commit would push that leftover onto the tracked
// branch. relPath goes through guardDriftPath like every other write into
// the worktree.
//
// relPath is NOT checked against Excluded, unlike stageDrift/stageImport:
// this file only ever exists repository-side, and being excluded is the
// point of it, exactly as for the managed .sops.yaml. A secret-shaped one
// is refused unconditionally - the strict matchesSecretPattern, since there
// is no encryption story for a file the agent renders itself - and a
// gitignored one fails loudly rather than recording nothing. The content is
// written verbatim, never through the encryption path.
//
// The tip is fetched inside this call and the push carries no --force, no
// --force-with-lease and no "+" prefix, so git's own fast-forward check is
// the guarantee, as in Import. Unlike Import a rejection IS retried, once,
// on the freshly fetched tip; a second gives up and the next cycle starts
// over. Workdir is left back in the detached checkout it was found in, and
// callers serialize this against every other GitSync method (recon's
// opLock).
func (g *GitSync) RecordFile(ctx context.Context, relPath string, content []byte, message string) (bool, error) {
	if err := g.guardWriteBranch(ctx, "record"); err != nil {
		return false, err
	}
	if matchesSecretPattern(relPath) {
		return false, fmt.Errorf("gitsync: record: refusing to record a secret-shaped path: %s", relPath)
	}

	tip, err := g.Fetch(ctx)
	if err != nil {
		return false, err
	}
	committed, err := g.recordFileAt(ctx, tip, relPath, content, message)
	if !errors.Is(err, errTrackedPushRejected) {
		return committed, err
	}

	newTip, fetchErr := g.Fetch(ctx)
	if fetchErr != nil {
		return false, fetchErr
	}
	if newTip == tip {
		// Refused without the branch moving, so it is not a lost race and
		// a second push would be refused the same way.
		return false, err
	}
	committed, err = g.recordFileAt(ctx, newTip, relPath, content, message)
	if errors.Is(err, errTrackedPushRejected) {
		return false, fmt.Errorf(
			"gitsync: record: %s moved on the remote twice while recording %s - giving up rather than racing it again",
			g.Opts.Branch, relPath)
	}
	return committed, err
}

// recordFileAt is one attempt at recording content on the commit tip.
// Returns (false, nil) when the tip already holds these bytes, and an error
// wrapping errTrackedPushRejected when the push lost a race.
func (g *GitSync) recordFileAt(ctx context.Context, tip, relPath string, content []byte, message string) (bool, error) {
	same, err := g.blobMatches(ctx, tip, relPath, content)
	if err != nil {
		return false, err
	}
	if same {
		return false, nil
	}

	restore, err := g.enterThrowawayBranch(ctx, recordBranch, tip)
	if err != nil {
		return false, err
	}
	defer restore()

	full, err := guardDriftPath(g.Workdir, relPath)
	if err != nil {
		return false, fmt.Errorf("gitsync: record: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
		return false, fmt.Errorf("gitsync: record: %w", err)
	}
	if err := os.WriteFile(full, content, 0o644); err != nil { // #nosec G306 -- guardDriftPath-confined under g.Workdir; agent-rendered bookkeeping, nothing secret
		return false, fmt.Errorf("gitsync: record: writing %s: %w", relPath, err)
	}
	if _, err := g.runGit(ctx, []string{"add", "--", relPath}, "", nil); err != nil {
		return false, err
	}

	// "--only -- <path>" commits that path and nothing else, whatever is in
	// the index. Not belt-and-braces: this can be reached with a dirty
	// index, and a bare commit would carry the leftover onto the tracked
	// branch. Enforced here rather than at the "git add" above, so the
	// one-path guarantee is true by construction.
	if _, err := g.runGit(ctx, []string{"commit", "--quiet", "-m", message, "--only", "--", relPath}, "", commitIdentityEnv()); err != nil {
		if isNothingToCommitError(err) {
			// blobMatches said the tip differs, yet the staged content does
			// not - a mode change, say. Reporting a commit that did not
			// happen would have the caller announce a change nobody made.
			return false, nil
		}
		return false, err
	}

	refspec := recordBranch + ":refs/heads/" + g.Opts.Branch
	if _, err := g.runGit(ctx, []string{"push", g.Opts.RepoURL, refspec}, "", g.credentialEnv()); err != nil {
		if isNonFastForwardError(err) {
			return false, fmt.Errorf("gitsync: record: %w: %s", errTrackedPushRejected, g.Opts.Branch)
		}
		return false, err
	}
	return true, nil
}

// blobMatches reports whether the blob tracked at path in sha is exactly
// content; a path the commit does not track is not a match, which is how a
// new file gets its first record. Read from the object database with "git
// show" rather than the working tree, which is what makes the no-op path
// safe every cycle: no checkout, so the detached tree differ and applier
// read between calls is never disturbed and need not sit at the fetched
// tip. A non-zero exit is read as "not tracked at sha", not a failure.
func (g *GitSync) blobMatches(ctx context.Context, sha, path string, content []byte) (bool, error) {
	result, err := g.runGitRaw(ctx, []string{"show", sha + ":" + path}, "", nil, 0)
	if err != nil {
		return false, err
	}
	if result.ExitCode != 0 {
		return false, nil
	}
	return result.Stdout == string(content), nil
}
