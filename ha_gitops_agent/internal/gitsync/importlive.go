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

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/sopscrypt"
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
		if _, err := g.runGit(ctx, []string{"clean", "-fdx"}, "", nil); err != nil {
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
		Files:   staged.files,
		Bytes:   staged.bytes,
		Created: !remoteHasBranch,
	}, nil
}

// stageImport writes the live content of every scanned path into the
// repository tree and stages the lot. Every path is re-checked against
// Excluded/secretShapedDisallowed and fails the whole call rather than
// being skipped, the same posture stageDrift takes.
//
// The staging is one bulk "git add" doing three jobs: --ignore-removal
// makes staging a deletion mechanically impossible (Import's never-remove
// promise), a directory pathspec skips .gitignore'd paths silently where
// naming one is fatal, and one subprocess instead of thousands is seconds
// instead of minutes. Safe on the whole worktree because Import has just
// forced a checkout and a clean.
func (g *GitSync) stageImport(ctx context.Context, files []string, configRoot string) (stagedTally, error) {
	if _, err := g.ensureSopsConfig(); err != nil {
		return stagedTally{}, fmt.Errorf("gitsync: import: %w", err)
	}
	// Ignore rules first, in copyIgnoresFromLive's order: the config's own
	// files, then the seed only if that left the root bare. PreviewIgnored
	// builds its throwaway tree the same way and has to stay in step.
	if err := g.copyIgnoresFromLive(ctx, files, configRoot); err != nil {
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
	// Refusals are collected, not returned on the first: files SOPS cannot
	// encrypt safely come in groups, and one per full scan would be
	// thirteen slow rounds for the user.
	var refusals []string
	for _, p := range files {
		if err := refuseUnsyncablePath(p); err != nil {
			return stagedTally{}, fmt.Errorf("gitsync: import: %w", err)
		}
		unchanged, err := g.encryptedCopyIsCurrent(ctx, configRoot, p)
		if err != nil {
			return stagedTally{}, fmt.Errorf("gitsync: import: %w", err)
		}
		if unchanged {
			// Counted, not skipped: the file IS in the snapshot this
			// import produces, it just did not have to be rewritten.
			tally.files++
			if info, err := os.Lstat(filepath.Join(configRoot, filepath.FromSlash(p))); err == nil {
				tally.bytes += info.Size()
			}
			continue
		}
		copied, err := g.copyLiveIntoWorkdir(ctx, configRoot, p)
		if err != nil {
			var refusal *EncryptRefusedError
			if errors.As(err, &refusal) {
				refusals = append(refusals, refusal.Error())
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
	if len(refusals) > 0 {
		// Nothing is staged when any file was refused: a partial import
		// looks complete while missing exactly the files holding secrets.
		return stagedTally{}, fmt.Errorf("gitsync: import: %d file(s) cannot be encrypted safely:\n  %s",
			len(refusals), strings.Join(refusals, "\n  "))
	}
	if _, err := g.runGitWith(ctx, []string{"add", "--ignore-removal", "--", "."}, "", nil, importGitTimeout); err != nil {
		return stagedTally{}, err
	}
	return tally, nil
}

// stagedTally is what stageImport actually copied, as opposed to what
// ScanLive found a moment earlier.
type stagedTally struct {
	files int
	bytes int64
}

// encryptedCopyIsCurrent reports whether the worktree already holds an
// encrypted copy of p whose plaintext is exactly what is live now. Not an
// optimization: sops ciphertext is nondeterministic, so without this every
// import would rewrite every encrypted file and commit pure noise. Import
// runs after a forced checkout at the base commit, so "in the worktree"
// means "in the repository at the tip being built on".
//
// Fails open in every uncertain case - a wrong "no" costs one rewrite, a
// wrong "yes" keeps a stale secret. The comparison is
// sopscrypt.SemanticallyEqual, as the differ uses, because sops re-emits
// from its own parse (quotes dropped, empty values written as null).
func (g *GitSync) encryptedCopyIsCurrent(ctx context.Context, configRoot, p string) (bool, error) {
	// The path test comes first because it is the only free check here: an
	// import walks thousands of files this agent never encrypts, and each
	// would otherwise be read twice. A superset of what gets encrypted.
	if !g.Crypter.Enabled() || !sopscrypt.EncryptablePath(p) {
		return false, nil
	}
	repoPath, err := guardDriftPath(g.Workdir, p)
	if err != nil {
		return false, err
	}
	existing, err := os.ReadFile(repoPath) // #nosec G304 -- repoPath is guardDriftPath-confined (symlink-resolved) under g.Workdir
	if err != nil || !sopscrypt.IsEncrypted(existing) {
		return false, nil
	}
	livePath, err := guardDriftPath(configRoot, p)
	if err != nil {
		return false, err
	}
	info, statErr := os.Stat(livePath)
	if statErr != nil || !info.Mode().IsRegular() {
		return false, nil
	}
	live, err := os.ReadFile(livePath) // #nosec G304 -- livePath is guardDriftPath-confined (symlink-resolved) under configRoot
	if err != nil {
		return false, nil
	}
	plaintext, err := g.Crypter.DecryptFile(ctx, repoPath)
	if err != nil {
		// Most likely encrypted to a different age key than the one
		// configured now; re-encrypting from live is how a rotation
		// completes.
		slog.Debug("gitsync: import: could not decrypt the repository copy, re-encrypting from live", "path", p, "error", err)
		return false, nil
	}
	return sopscrypt.SemanticallyEqual(plaintext, live), nil
}

// isNonFastForwardError matches a push the remote refused as not a
// fast-forward. Text matching, safe because gitEnv pins LC_ALL=C.
func isNonFastForwardError(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "non-fast-forward") ||
		strings.Contains(msg, "[rejected]") ||
		strings.Contains(msg, "fetch first") ||
		strings.Contains(msg, "Updates were rejected")
}
