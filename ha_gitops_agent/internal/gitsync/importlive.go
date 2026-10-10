package gitsync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ImportCommitMessage is the fixed commit message Import uses.
const ImportCommitMessage = "import: seed repository from live home assistant config"

// importGitTimeout bounds the three Import steps whose cost scales with the
// config tree - see runGitWith on why DefaultGitTimeout is wrong for them.
var importGitTimeout = 15 * time.Minute

// ErrImportRejected reports a push refused because the tracked branch moved
// on the remote. Nothing was written: git's fast-forward check rejected it,
// so the remote still holds whatever moved it.
var ErrImportRejected = errors.New("push rejected: the tracked branch moved on the remote")

// ImportResult describes a completed import.
type ImportResult struct {
	// CommitSHA is the commit that now sits at the tip of opts.Branch.
	CommitSHA string
	// BaseSHA is the tip it was built on, or "" when the branch did not
	// exist on the remote yet.
	BaseSHA string
	Files   int
	Bytes   int64
	// Created reports whether this import brought opts.Branch into
	// existence rather than advancing it.
	Created bool
	// HeldBack is the scanned files left out because they hold a literal
	// value under a secret-shaped key. Not counted in Files or Bytes.
	HeldBack []HeldBack
}

// Import copies every importable file under configRoot (see ScanLive) into
// the repository and pushes them as one commit onto opts.Branch - the ONE
// operation here that writes to the tracked branch, which shapes it:
//
//   - The local ref is a throwaway "gitops/import-<timestamp>"; opts.Branch
//     is named only in the push refspec, so the "git branch -D" cleanup can
//     never be pointed at a local branch of the user's own name.
//   - The base is fetched inside this call rather than taken from a caller
//     whose tip may be a poll interval old.
//   - No --force, no --force-with-lease, no "+" prefix: git's own
//     server-side fast-forward check is what stops a concurrent push being
//     clobbered. A rejection is ErrImportRejected and is NOT retried, since
//     a retry would re-scan, re-commit and re-race unbounded.
//   - opts.Branch goes through guardWriteBranch first.
//
// Nothing is ever removed ("git add --ignore-removal"): a repository
// legitimately holds paths that never exist live - gitops/ manifests, a
// README, CI workflows, .gitignore itself - and excluded paths are
// invisible to the scan, so mirroring live would delete all of them.
//
// A file holding a literal value under a secret-shaped key is left out and
// reported (ImportResult.HeldBack): the rest of the import still lands,
// and the caller says which files are missing and why.
//
// A .gitignore match is silently left out, which is the supported way to
// shape an import; seeding an empty branch copies the config's own
// .gitignore files in first (ESPHome ships one), then DefaultGitignore if
// the root is still bare. This shapes only what is COMMITTED - ScanLive's
// limits are measured before git is involved.
//
// Workdir is left in a plain detached checkout either way; callers
// serialize Import as they do CommitBack (recon.Reconciler's opLock).
func (g *GitSync) Import(ctx context.Context, configRoot string, limits ImportLimits, now time.Time) (ImportResult, error) {
	if err := g.guardWriteBranch(ctx, "import"); err != nil {
		return ImportResult{}, err
	}

	// Scanning first, so an oversized or misconfigured config root fails
	// before any git command runs: no branch to clean up, no disturbed
	// checkout.
	plan, err := ScanLive(configRoot, limits)
	if err != nil {
		return ImportResult{}, err
	}
	if len(plan.Files) == 0 {
		return ImportResult{}, fmt.Errorf("gitsync: import: nothing to import: no importable files found under %s", configRoot)
	}

	// A probe failure has to propagate, or an ordinary auth failure takes
	// the orphan path below and builds a parentless commit the remote
	// correctly rejects.
	remoteHasBranch, err := g.RemoteHasBranch(ctx)
	if err != nil {
		return ImportResult{}, err
	}

	restoreSHA := g.CurrentSHA(ctx)
	tmpBranch := "gitops/import-" + now.UTC().Format(driftBranchTimeFormat)

	// Armed BEFORE the first command that can move HEAD: otherwise a
	// failure below returns with the workdir on the throwaway branch and
	// that ref never deleted. restoreTarget becomes the import's own commit
	// only when there was nothing checked out to go back to.
	restoreTarget := restoreSHA
	defer func() {
		if restoreTarget != "" {
			g.restoreDetachedCheckout(ctx, restoreTarget, tmpBranch)
			return
		}
		// Nothing was ever checked out, so there is no commit to detach at
		// and no branch deletable while HEAD is on it. Drop what was
		// written; the next EnsureClone/Checkout puts the rest right.
		// Uncancelled for restoreDetachedCheckout's reason.
		if _, err := g.runGit(context.WithoutCancel(ctx), []string{"clean", "-fdx"}, "", nil); err != nil {
			slog.Debug("gitsync: import: could not clean workdir after a failed seed", "branch", tmpBranch, "error", err)
		}
	}()

	var baseSHA string
	if remoteHasBranch {
		baseSHA, err = g.Fetch(ctx)
		if err != nil {
			return ImportResult{}, err
		}
		if _, err := g.runGit(ctx, []string{"checkout", "-B", tmpBranch, baseSHA}, "", nil); err != nil {
			return ImportResult{}, err
		}
	} else {
		// Branch not on the remote: seeding an empty repository. An orphan
		// checkout plus an emptied index starts deterministically whether
		// or not the clone has any commits.
		if _, err := g.runGit(ctx, []string{"checkout", "--orphan", tmpBranch}, "", nil); err != nil {
			return ImportResult{}, err
		}
		if _, err := g.runGit(ctx, []string{"read-tree", "--empty"}, "", nil); err != nil {
			return ImportResult{}, err
		}
	}
	if _, err := g.runGit(ctx, []string{"clean", "-fdx"}, "", nil); err != nil {
		return ImportResult{}, err
	}

	staged, err := g.stageImport(ctx, plan.Files, configRoot)
	if err != nil {
		return ImportResult{}, err
	}

	// "diff --cached --quiet" exits non-zero when something is staged, so
	// a zero exit here means the commit would be empty.
	clean, err := g.runGitStatus(ctx, []string{"diff", "--cached", "--quiet"}, "", nil)
	if err != nil {
		return ImportResult{}, err
	}
	if clean {
		if len(staged.heldBack) > 0 {
			return ImportResult{}, fmt.Errorf("gitsync: import: nothing to import: everything else already matches, and %d file(s) were held back - %s: %s",
				len(staged.heldBack), HeldBackSummary(staged.heldBack), HeldBackAdvice)
		}
		return ImportResult{}, fmt.Errorf("gitsync: import: nothing to import (the repository already matches the live config, or every scanned path is gitignored)")
	}

	commitEnv := []string{
		"GIT_AUTHOR_NAME=" + commitAuthorName, "GIT_AUTHOR_EMAIL=" + commitAuthorEmail,
		"GIT_COMMITTER_NAME=" + commitAuthorName, "GIT_COMMITTER_EMAIL=" + commitAuthorEmail,
	}
	if _, err := g.runGitWith(ctx, []string{"commit", "--quiet", "-m", ImportCommitMessage}, "", commitEnv, importGitTimeout); err != nil {
		return ImportResult{}, err
	}
	head, err := g.runGit(ctx, []string{"rev-parse", "HEAD"}, "", nil)
	if err != nil {
		return ImportResult{}, err
	}
	newSHA := strings.TrimSpace(head.Stdout)
	if restoreTarget == "" {
		restoreTarget = newSHA
	}

	refspec := tmpBranch + ":refs/heads/" + g.Opts.Branch
	if _, err := g.runGitWith(ctx, []string{"push", g.Opts.RepoURL, refspec}, "", g.credentialEnv(), importGitTimeout); err != nil {
		if isNonFastForwardError(err) {
			return ImportResult{}, fmt.Errorf("gitsync: import: %w: %s moved since this import started - run Check Now, then import again", ErrImportRejected, g.Opts.Branch)
		}
		return ImportResult{}, err
	}

	return ImportResult{
		CommitSHA: newSHA,
		BaseSHA:   baseSHA,
		// What was copied, not what the scan found: a live tree churns, so
		// a file can vanish between the two.
		Files:    staged.files,
		Bytes:    staged.bytes,
		Created:  !remoteHasBranch,
		HeldBack: staged.heldBack,
	}, nil
}

// stageImport writes the live content of every scanned path into the
// repository tree and stages the lot. Every path is re-checked against
// Excluded/matchesSecretPattern and fails the whole call rather than being
// skipped, the same posture stageDrift takes; a file holding a literal
// secret is held back instead, every one of them gathered and reported.
//
// The staging is one bulk "git add" doing three jobs: --ignore-removal
// makes staging a deletion mechanically impossible (Import's never-remove
// promise), a directory pathspec skips .gitignore'd paths silently where
// naming one is fatal, and one subprocess instead of thousands is seconds
// instead of minutes. Safe on the whole worktree because Import has just
// forced a checkout and a clean, and a held-back file is never written
// into it.
func (g *GitSync) stageImport(ctx context.Context, files []string, configRoot string) (stagedTally, error) {
	// Ignore rules first, in copyIgnoresFromLive's order: the config's own
	// files, then the seed only if that left the root bare. PreviewIgnored
	// builds its throwaway tree the same way and has to stay in step.
	if err := g.copyIgnoresFromLive(files, configRoot); err != nil {
		return stagedTally{}, fmt.Errorf("gitsync: import: %w", err)
	}
	if _, err := g.ensureGitignore(); err != nil {
		return stagedTally{}, fmt.Errorf("gitsync: import: %w", err)
	}
	files, err := g.filterIgnored(ctx, "", files)
	if err != nil {
		return stagedTally{}, fmt.Errorf("gitsync: import: %w", err)
	}

	var tally stagedTally
	for _, p := range files {
		if err := refuseUnsyncablePath(p); err != nil {
			return stagedTally{}, fmt.Errorf("gitsync: import: %w", err)
		}
		copied, err := g.copyLiveIntoWorkdir(configRoot, p)
		if err != nil {
			var held *HeldBackError
			if errors.As(err, &held) {
				tally.heldBack = append(tally.heldBack, held.HeldBack)
				continue
			}
			return stagedTally{}, fmt.Errorf("gitsync: import: %w", err)
		}
		if !copied {
			// Gone (or no longer regular) between the scan and now. Not
			// counted, or the result overstates what landed.
			slog.Info("gitsync: import: live file vanished between scan and copy, skipping", "path", p)
			continue
		}
		tally.files++
		if info, err := os.Lstat(filepath.Join(configRoot, filepath.FromSlash(p))); err == nil {
			tally.bytes += info.Size()
		}
	}
	if _, err := g.runGitWith(ctx, []string{"add", "--ignore-removal", "--", "."}, "", nil, importGitTimeout); err != nil {
		return stagedTally{}, err
	}
	return tally, nil
}

// stagedTally is what stageImport actually copied, as opposed to what
// ScanLive found a moment earlier, and what it held back.
type stagedTally struct {
	files    int
	bytes    int64
	heldBack []HeldBack
}

// errTrackedPushRejected reports the one push failure the tracked-branch
// writers handle themselves: opts.Branch moved between a call's fetch and
// its push. Shared by RecordFile and CaptureFiles, which both retry once on
// a freshly fetched tip; it never leaves this package, since it only decides
// whether that retry is worth making. Import deliberately does not use it -
// re-scanning a whole config tree to race again is not worth it - and
// reports ErrImportRejected to the user instead.
var errTrackedPushRejected = errors.New("push rejected: the tracked branch moved on the remote")

// isNonFastForwardError matches a push the remote refused as not a
// fast-forward. Text matching, safe because gitEnv pins LC_ALL=C.
func isNonFastForwardError(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "non-fast-forward") ||
		strings.Contains(msg, "[rejected]") ||
		strings.Contains(msg, "fetch first") ||
		strings.Contains(msg, "Updates were rejected")
}
