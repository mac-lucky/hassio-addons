package gitsync

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/fsx"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/secretshape"
)

// commitAuthorName/commitAuthorEmail identify every CommitBack commit.
// Fixed rather than configurable: git_username/git_token already identify
// who PUSHES it, which is the credential that matters for audit.
const (
	commitAuthorName  = "GitOps Agent"
	commitAuthorEmail = "gitops-agent@localhost"
)

// driftBranchTimeFormat is the UTC timestamp CommitBack and ParkConflicts
// embed in their branch names: yyyymmddTHHMMSSZ, sortable and (to the
// second) unique.
const driftBranchTimeFormat = "20060102T150405Z"

// DriftCommitMessage is the fixed commit message CommitBack uses for
// every drift-capture commit.
const DriftCommitMessage = "drift: capture live changes from home assistant"

// ConflictCommitMessage is the fixed commit message ParkConflicts uses.
const ConflictCommitMessage = "conflict: preserve live copies the agent will not sync"

// commitIdentityEnv is the fixed identity every commit this package makes
// carries. A function rather than a package var because each caller hands
// it to a subprocess that may append to it.
func commitIdentityEnv() []string {
	return []string{
		"GIT_AUTHOR_NAME=" + commitAuthorName, "GIT_AUTHOR_EMAIL=" + commitAuthorEmail,
		"GIT_COMMITTER_NAME=" + commitAuthorName, "GIT_COMMITTER_EMAIL=" + commitAuthorEmail,
	}
}

// DriftFile is one path CommitBack should consider, plus differ's Kind for
// it. A local type because internal/differ already imports this package for
// Excluded, so importing differ back would cycle.
type DriftFile struct {
	Path string
	// Kind mirrors differ.Change.Kind ("add", "update", "delete").
	// Diagnostic only: CommitBack stages from the live filesystem instead,
	// and reports Kind in its nothing-to-stage error.
	Kind string
}

// CommitBack captures the CURRENT LIVE state of every path in files into a
// new "gitops/drift-<timestamp>" branch based on baseSHA and pushes it with
// Fetch's credential mechanism, returning the branch name and the files it
// held back (see HeldBack). opts.Branch is never touched.
//
// Kind is not consulted: each path is re-read under configRoot and staged
// with git add, or git rm when it is genuinely gone (fs.ErrNotExist alone -
// see liveFileIsGone) and the repo tracks it. A live deletion arrives here
// as differ's "add", so a repo-side file no apply has written out yet is
// captured as a deletion too, on the throwaway branch only. A branch whose
// every file was held back is not made; that is an error naming them.
//
// Workdir is left back at its detached baseSHA either way. Callers
// serialize this against every other GitSync method (recon's opLock).
func (g *GitSync) CommitBack(ctx context.Context, files []DriftFile, configRoot, baseSHA string, now time.Time) (string, []HeldBack, error) {
	branch := "gitops/drift-" + now.UTC().Format(driftBranchTimeFormat)
	staged, err := g.commitLiveOnto(ctx, "commit-back", branch, DriftCommitMessage, files, configRoot, baseSHA)
	if errors.Is(err, ErrAllHeldBack) {
		return "", staged.HeldBack, fmt.Errorf("gitsync: commit-back: nothing to commit: %w - %s: %s",
			err, HeldBackSummary(staged.HeldBack), HeldBackAdvice)
	}
	if err != nil {
		return "", staged.HeldBack, err
	}
	return branch, staged.HeldBack, nil
}

// ParkConflicts captures the CURRENT LIVE state of every path in files into
// a new "gitops/conflict-<timestamp>" branch based on baseSHA and pushes it,
// returning the branch name and the files it held back. opts.Branch is
// never touched. When every file was held back nothing is pushed and the
// branch is "", with no error: the refusal stands, there is just no copy.
//
// CommitBack's machinery with a different prefix and a different meaning.
// Commit-back captures drift a human may want to merge; this preserves work
// the agent has decided it may not touch in EITHER direction, because the
// repository and the live config both moved since the merge base and there
// is no way to tell which one is meant. So the branch is a safety net rather
// than a proposal, and a failure to park does not change that verdict: the
// refusal is the protection, this is only the copy.
func (g *GitSync) ParkConflicts(ctx context.Context, files []DriftFile, configRoot, baseSHA string, now time.Time) (string, []HeldBack, error) {
	branch := "gitops/conflict-" + now.UTC().Format(driftBranchTimeFormat)
	staged, err := g.commitLiveOnto(ctx, "park-conflicts", branch, ConflictCommitMessage, files, configRoot, baseSHA)
	if errors.Is(err, ErrAllHeldBack) {
		return "", staged.HeldBack, nil
	}
	if err != nil {
		return "", staged.HeldBack, err
	}
	return branch, staged.HeldBack, nil
}

// ErrAllHeldBack reports that nothing was staged because every file was
// held back. CommitBack wraps it, so a caller can tell a drift set that
// will never commit as it stands from a push that failed; ParkConflicts
// absorbs it.
var ErrAllHeldBack = errors.New("every file was held back")

// commitLiveOnto builds branch at baseSHA holding the current live state of
// every path in files, commits it with message and pushes it under its own
// name, returning what was staged. The shared body of CommitBack and
// ParkConflicts, which differ only in what their branch MEANS. Both leave
// opts.Branch alone, and neither needs the fast-forward dance Import,
// RecordFile and CaptureFiles go through: the ref is new every time, so
// there is nothing on the remote to race.
//
// op names the operation in every error, so a failure says which button or
// which verdict produced it.
func (g *GitSync) commitLiveOnto(
	ctx context.Context, op, branch, message string, files []DriftFile, configRoot, baseSHA string,
) (stagedSet, error) {
	if len(files) == 0 {
		return stagedSet{}, fmt.Errorf("gitsync: %s: no files given", op)
	}
	if baseSHA == "" {
		return stagedSet{}, fmt.Errorf("gitsync: %s: no base commit to branch from", op)
	}

	if _, err := g.runGit(ctx, []string{"checkout", "-B", branch, baseSHA}, "", nil); err != nil {
		return stagedSet{}, err
	}
	defer g.restoreDetachedCheckout(ctx, baseSHA, branch)

	staged, err := g.stageDrift(ctx, op, files, configRoot)
	if err != nil {
		return stagedSet{}, err
	}
	if len(staged.Paths) == 0 {
		if len(staged.HeldBack) > 0 {
			return staged, ErrAllHeldBack
		}
		return staged, fmt.Errorf("gitsync: %s: nothing to stage among %v", op, files)
	}

	if _, err := g.runGit(ctx, []string{"commit", "--quiet", "-m", message}, "", commitIdentityEnv()); err != nil {
		if isNothingToCommitError(err) {
			// Everything staged is byte-identical to baseSHA. Named
			// explicitly rather than reported as success with an empty
			// branch name, which would set LastDriftBranch to "".
			return staged, fmt.Errorf("gitsync: %s: nothing to commit (live content already matches the repository)", op)
		}
		return staged, err
	}

	if _, err := g.runGit(ctx, []string{"push", g.Opts.RepoURL, branch}, "", g.credentialEnv()); err != nil {
		return staged, err
	}

	return staged, nil
}

// isNothingToCommitError matches git commit's "nothing to commit, working
// tree clean" - the one git failure explained on STDOUT, not STDERR.
func isNothingToCommitError(err error) bool {
	return strings.Contains(err.Error(), "nothing to commit")
}

// stagedSet is what one staging pass put in the index.
type stagedSet struct {
	// Paths is the content staged from live.
	Paths []string
	// HeldBack is the live files left out because they hold a literal
	// value under a secret-shaped key.
	HeldBack []HeldBack
}

// stageDrift stages the live version of each path (git add), or its removal
// (git rm) when genuinely gone, and reports what it staged so a caller can
// refuse an empty commit and so one committing by pathspec knows the names.
// Every path is re-checked against Excluded/matchesSecretPattern and
// guardDriftPath here, whatever the caller already filtered, and a live
// file holding a literal secret is held back rather than staged. op names
// the operation in the errors and the skip logs.
func (g *GitSync) stageDrift(ctx context.Context, op string, files []DriftFile, configRoot string) (stagedSet, error) {
	var set stagedSet

	for _, f := range files {
		p := f.Path
		if err := refuseUnsyncablePath(p); err != nil {
			return stagedSet{}, fmt.Errorf("gitsync: %s: %w", op, err)
		}

		copied, err := g.copyLiveIntoWorkdir(configRoot, p)
		if err != nil {
			var held *HeldBackError
			if errors.As(err, &held) {
				slog.Info("gitsync: "+op+": holding back a file with a literal secret", "path", p, "keys", strings.Join(held.Keys, ", "))
				set.HeldBack = append(set.HeldBack, held.HeldBack)
				continue
			}
			return stagedSet{}, fmt.Errorf("gitsync: %s: %w", op, err)
		}
		if copied {
			added, err := g.gitAddSkippingIgnored(ctx, op, p)
			if err != nil {
				return stagedSet{}, err
			}
			if added {
				set.Paths = append(set.Paths, p)
			}
			continue
		}

		gone, err := liveFileIsGone(configRoot, p)
		if err != nil {
			return stagedSet{}, fmt.Errorf("gitsync: %s: %w", op, err)
		}
		if !gone {
			// Still there, just not capturable (unreadable, or no longer a
			// regular file). A removal would delete what nobody deleted.
			slog.Info("gitsync: "+op+": live path still exists but could not be captured, not staging a removal", "path", p)
			continue
		}

		// Only stage a removal if the repo tracks the path: "git rm" on an
		// untracked path is an error.
		repoPath, err := guardDriftPath(g.Workdir, p)
		if err != nil {
			return stagedSet{}, fmt.Errorf("gitsync: %s: %w", op, err)
		}
		if _, err := os.Stat(repoPath); err != nil {
			slog.Info("gitsync: "+op+": live path is gone but the repository does not track it, nothing to remove", "path", p)
			continue
		}
		if _, err := g.runGit(ctx, []string{"rm", "--quiet", "--", p}, "", nil); err != nil {
			return stagedSet{}, err
		}
		set.Paths = append(set.Paths, p)
	}
	return set, nil
}

// liveFileIsGone reports whether p is genuinely absent from configRoot;
// only fs.ErrNotExist counts, because internal/differ reports EVERY stat
// failure as "add" and committing a removal for a momentarily unreadable
// file would delete one nobody deleted. Stats through symlinks, as
// copyLiveIntoWorkdir does, so the two cannot disagree about a path.
func liveFileIsGone(configRoot, p string) (bool, error) {
	livePath, err := guardDriftPath(configRoot, p)
	if err != nil {
		return false, err
	}
	if _, err := os.Stat(livePath); err != nil {
		return errors.Is(err, fs.ErrNotExist), nil
	}
	return false, nil
}

// copyLiveIntoWorkdir writes p's live content from configRoot into the same
// relative place under Workdir and reports whether it did; false with a nil
// error means absent or not a regular file, and the caller decides what
// that means. Both ends go through guardDriftPath.
//
// A live file holding a literal value under a secret-shaped key is never
// written: that is a *HeldBackError, and whatever the worktree held at that
// path stays as the checked-out commit has it, so nothing of it is staged.
func (g *GitSync) copyLiveIntoWorkdir(configRoot, p string) (bool, error) {
	livePath, err := guardDriftPath(configRoot, p)
	if err != nil {
		return false, err
	}
	repoPath, err := guardDriftPath(g.Workdir, p)
	if err != nil {
		return false, err
	}

	info, statErr := os.Stat(livePath)
	if statErr != nil || !info.Mode().IsRegular() {
		return false, nil
	}

	content, err := os.ReadFile(livePath) // #nosec G304 -- livePath is guardDriftPath-confined (symlink-resolved) under configRoot, see above
	if err != nil {
		return false, fmt.Errorf("reading live %s: %w", p, err)
	}
	if keys := secretshape.Literals(p, content); len(keys) > 0 {
		return false, &HeldBackError{HeldBack: HeldBack{Path: p, Keys: keys}}
	}
	if err := os.MkdirAll(filepath.Dir(repoPath), 0o750); err != nil {
		return false, err
	}
	// 0600: the worktree sits inside every Supervisor backup of /data. Git
	// records only the executable bit, so the tighter mode never reaches
	// the repository.
	if err := os.WriteFile(repoPath, content, 0o600); err != nil { // #nosec G304,G703 -- repoPath is guardDriftPath-confined (symlink-resolved) under g.Workdir
		return false, err
	}
	return true, nil
}

// maxHeldBackKeys bounds how many key paths HeldBack.String names per file.
const maxHeldBackKeys = 5

// HeldBackAdvice is the fix every held-back report ends with.
const HeldBackAdvice = "move each value into secrets.yaml and reference it with !secret, or list the file in exclude_paths"

// HeldBack is one live file a write to git left out because it holds a
// literal value under a secret-shaped key (internal/secretshape). Keys are
// key paths, never values, so it is safe to log and to display.
type HeldBack struct {
	Path string
	Keys []string
}

// String renders "path (key, key)".
func (h HeldBack) String() string {
	keys := h.Keys
	more := ""
	if len(keys) > maxHeldBackKeys {
		more = fmt.Sprintf(" and %d more", len(keys)-maxHeldBackKeys)
		keys = keys[:maxHeldBackKeys]
	}
	return h.Path + " (" + strings.Join(keys, ", ") + more + ")"
}

// HeldBackSummary renders a list of them for an event or an error.
func HeldBackSummary(items []HeldBack) string {
	parts := make([]string, len(items))
	for i, h := range items {
		parts[i] = h.String()
	}
	return strings.Join(parts, ", ")
}

// HeldBackError is copyLiveIntoWorkdir's refusal of one file. Typed so a
// caller walking many files can gather every one and carry on.
type HeldBackError struct {
	HeldBack
}

func (e *HeldBackError) Error() string {
	return e.String() + " holds a literal value under a secret-shaped key"
}

// gitAddSkippingIgnored stages p, tolerating the one failure a gitignored
// path produces: "git add" without "-f" is fatal, which would abort the
// whole operation over one ignored path. Any other add failure still
// propagates. Returns whether p was staged, so a skip never counts toward
// "something was staged".
func (g *GitSync) gitAddSkippingIgnored(ctx context.Context, op, p string) (bool, error) {
	_, err := g.runGit(ctx, []string{"add", "--", p}, "", nil)
	if err == nil {
		return true, nil
	}
	if isIgnoredPathError(err) {
		slog.Info("gitsync: "+op+": skipping gitignored path", "path", p)
		return false, nil
	}
	return false, err
}

// isIgnoredPathError matches "git add"'s refusal of a .gitignore'd path
// ("ignored by one of your .gitignore files" / "Use -f if you really want").
func isIgnoredPathError(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "ignored by one of your .gitignore files") || strings.Contains(msg, "Use -f if you really want")
}

// guardDriftPath resolves path under root, rejecting it if absolute, if it
// leaves root after normalization, or if it resolves (fsx.Realpath, so
// symlinks are followed all the way down) outside root or onto an in-root
// excluded/secret-shaped path. Not internal/applier's guardChangePath: only
// this one returns the resolved path and re-checks the symlink target, and
// importing applier here would cycle.
func guardDriftPath(root, path string) (string, error) {
	if filepath.IsAbs(path) {
		return "", fmt.Errorf("refusing to touch absolute path: %s", path)
	}
	normalized := filepath.Clean(path)
	if normalized == ".." || strings.HasPrefix(normalized, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("refusing to touch path outside root: %s", path)
	}
	rootClean := filepath.Clean(root)
	full := filepath.Join(rootClean, normalized)
	if full != rootClean && !strings.HasPrefix(full, rootClean+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes root: %s", path)
	}

	rootReal := fsx.Realpath(rootClean)
	destReal := fsx.Realpath(full)
	if destReal != rootReal && !strings.HasPrefix(destReal, rootReal+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes root via symlink: %s", path)
	}
	if rel, err := filepath.Rel(rootReal, destReal); err == nil && rel != "." {
		relSlash := filepath.ToSlash(rel)
		// The TARGET is what gets read or written, so "automations.yaml ->
		// secrets.yaml" is refused like secrets.yaml itself.
		if relSlash != normalized && (Excluded(relSlash) || matchesSecretPattern(relSlash)) {
			return "", fmt.Errorf("path resolves (via symlink) to an excluded/secret-shaped path: %s -> %s", path, relSlash)
		}
	}
	return full, nil
}

// enterThrowawayBranch force-creates branch at tip, cleans the tree, and
// returns the restore the caller must defer. Shared by the two tracked-branch
// writers that build a commit on top of a freshly fetched tip (RecordFile and
// CaptureFiles); CommitBack and ParkConflicts do not use it, because they
// restore to the base they branched from rather than to wherever the worktree
// happened to be.
//
// That distinction is the whole point of the helper: these two run INSIDE a
// reconcile cycle, whose detached checkout the differ and applier are reading
// between calls, so the worktree has to go back exactly where it was and not
// to the tip just fetched. A workdir with nothing checked out has nowhere to
// go back to, so it lands on that tip. The restore is returned rather than
// deferred here so it covers the clean below as well.
func (g *GitSync) enterThrowawayBranch(ctx context.Context, branch, tip string) (restore func(), err error) {
	restoreSHA := g.CurrentSHA(ctx)
	if restoreSHA == "" {
		restoreSHA = tip
	}
	if _, err := g.runGit(ctx, []string{"checkout", "-B", branch, tip}, "", nil); err != nil {
		return nil, err
	}
	restore = func() { g.restoreDetachedCheckout(ctx, restoreSHA, branch) }
	if _, err := g.runGit(ctx, []string{"clean", "-fdx"}, "", nil); err != nil {
		restore()
		return nil, err
	}
	return restore, nil
}

// restoreDetachedCheckout puts Workdir back into the detached checkout at
// sha every other GitSync method assumes, and drops the throwaway branch
// ref. Best-effort and logged, never returned: it runs from a defer, and
// the next ReconcileNow's Checkout forces Workdir back into shape anyway.
//
// Detached from ctx's cancellation (runGit's own timeout still bounds it):
// the caller's ctx is the cycle's, and SIGTERM cancels it mid-capture. A
// cancelled ctx cannot start a process at all, so the restore used to be a
// no-op exactly then, leaving Workdir on the throwaway branch for the apply
// that runCycle deliberately lets finish.
func (g *GitSync) restoreDetachedCheckout(ctx context.Context, sha, branch string) {
	ctx = context.WithoutCancel(ctx)
	if !g.restoreDetached(ctx, sha) {
		return
	}
	// Already pushed, or not worth keeping after a failed push. Quiet: it
	// may not exist at all if CommitBack failed before the checkout -B.
	if _, err := g.runGit(ctx, []string{"branch", "-D", branch}, "", nil); err != nil {
		slog.Debug("gitsync: could not delete local throwaway branch", "branch", branch, "error", err)
	}
}

// restoreDetached puts Workdir back into a detached checkout at sha with a
// pristine tree, reporting whether it got there. Split out from
// restoreDetachedCheckout because Import's empty-clone path needs the clean
// with no sha to detach at.
func (g *GitSync) restoreDetached(ctx context.Context, sha string) bool {
	ctx = context.WithoutCancel(ctx) // see restoreDetachedCheckout
	if _, err := g.runGit(ctx, []string{"checkout", "--detach", "--force", sha}, "", nil); err != nil {
		slog.Warn("gitsync: could not restore detached checkout", "sha", sha, "error", err)
		return false
	}
	if _, err := g.runGit(ctx, []string{"clean", "-fdx"}, "", nil); err != nil {
		slog.Warn("gitsync: could not clean workdir", "error", err)
	}
	return true
}
