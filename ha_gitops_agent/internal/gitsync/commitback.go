package gitsync

import (
	"bytes"
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
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/sopscrypt"
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
// Fetch's credential mechanism, returning the branch name. opts.Branch is
// never touched.
//
// Kind is not consulted: each path is re-read under configRoot and staged
// with git add, or git rm when it is genuinely gone (fs.ErrNotExist alone -
// see liveFileIsGone) and the repo tracks it. A live deletion arrives here
// as differ's "add", so a repo-side file no apply has written out yet is
// captured as a deletion too, on the throwaway branch only.
//
// Workdir is left back at its detached baseSHA either way. Callers
// serialize this against every other GitSync method (recon's opLock).
func (g *GitSync) CommitBack(ctx context.Context, files []DriftFile, configRoot, baseSHA string, now time.Time) (string, error) {
	branch := "gitops/drift-" + now.UTC().Format(driftBranchTimeFormat)
	if _, err := g.commitLiveOnto(ctx, "commit-back", branch, DriftCommitMessage, files, configRoot, baseSHA); err != nil {
		return "", err
	}
	return branch, nil
}

// ParkConflicts captures the CURRENT LIVE state of every path in files into
// a new "gitops/conflict-<timestamp>" branch based on baseSHA and pushes it,
// returning the branch name. opts.Branch is never touched.
//
// CommitBack's machinery with a different prefix and a different meaning.
// Commit-back captures drift a human may want to merge; this preserves work
// the agent has decided it may not touch in EITHER direction, because the
// repository and the live config both moved since the merge base and there
// is no way to tell which one is meant. So the branch is a safety net rather
// than a proposal, and a failure to park does not change that verdict: the
// refusal is the protection, this is only the copy.
func (g *GitSync) ParkConflicts(ctx context.Context, files []DriftFile, configRoot, baseSHA string, now time.Time) (string, error) {
	branch := "gitops/conflict-" + now.UTC().Format(driftBranchTimeFormat)
	if _, err := g.commitLiveOnto(ctx, "park-conflicts", branch, ConflictCommitMessage, files, configRoot, baseSHA); err != nil {
		return "", err
	}
	return branch, nil
}

// commitLiveOnto builds branch at baseSHA holding the current live state of
// every path in files, commits it with message and pushes it under its own
// name, returning the paths the commit carries. The shared body of
// CommitBack and ParkConflicts, which differ only in what their branch
// MEANS. Both leave opts.Branch alone, and neither needs the fast-forward
// dance Import, RecordFile and CaptureFiles go through: the ref is new every
// time, so there is nothing on the remote to race.
//
// op names the operation in every error, so a failure says which button or
// which verdict produced it.
func (g *GitSync) commitLiveOnto(
	ctx context.Context, op, branch, message string, files []DriftFile, configRoot, baseSHA string,
) ([]string, error) {
	if len(files) == 0 {
		return nil, fmt.Errorf("gitsync: %s: no files given", op)
	}
	if baseSHA == "" {
		return nil, fmt.Errorf("gitsync: %s: no base commit to branch from", op)
	}

	if _, err := g.runGit(ctx, []string{"checkout", "-B", branch, baseSHA}, "", nil); err != nil {
		return nil, err
	}
	defer g.restoreDetachedCheckout(ctx, baseSHA, branch)

	staged, err := g.stageDrift(ctx, op, files, configRoot)
	if err != nil {
		return nil, err
	}
	if len(staged.Paths) == 0 {
		return nil, fmt.Errorf("gitsync: %s: nothing to stage among %v", op, files)
	}

	if _, err := g.runGit(ctx, []string{"commit", "--quiet", "-m", message}, "", commitIdentityEnv()); err != nil {
		if isNothingToCommitError(err) {
			// Everything staged is byte-identical to baseSHA. Named
			// explicitly rather than reported as success with an empty
			// branch name, which would set LastDriftBranch to "".
			return nil, fmt.Errorf("gitsync: %s: nothing to commit (live content already matches the repository)", op)
		}
		return nil, err
	}

	if _, err := g.runGit(ctx, []string{"push", g.Opts.RepoURL, branch}, "", g.credentialEnv()); err != nil {
		return nil, err
	}

	return staged.Paths, nil
}

// isNothingToCommitError matches git commit's "nothing to commit, working
// tree clean" - the one git failure explained on STDOUT, not STDERR.
func isNothingToCommitError(err error) bool {
	return strings.Contains(err.Error(), "nothing to commit")
}

// stagedSet is what one staging pass put in the index.
type stagedSet struct {
	// Paths is the content staged from live. A .sops.yaml refresh is
	// deliberately NOT in here: it is not drift, and an operation whose only
	// change is that config must still refuse as "nothing to stage".
	Paths []string
	// SopsConfig records that this pass also refreshed the managed
	// .sops.yaml. Kept separate for the reason above, but a "commit --only"
	// pathspec has to name it as well, or a commit carrying a newly
	// encrypted secrets.yaml would leave behind the config to decrypt it.
	SopsConfig bool
}

// pathspec is every path this pass staged, for a commit that names them
// rather than trusting the index.
func (s stagedSet) pathspec() []string {
	if s.SopsConfig {
		return append(append([]string{}, s.Paths...), sopscrypt.ConfigFile)
	}
	return s.Paths
}

// stageDrift stages the live version of each path (git add), or its removal
// (git rm) when genuinely gone, and reports what it staged so a caller can
// refuse an empty commit and so one committing by pathspec knows the names.
// Every path is re-checked against Excluded/secretShapedDisallowed and
// guardDriftPath here, whatever the caller already filtered. op names the
// operation in the errors and the skip logs.
func (g *GitSync) stageDrift(ctx context.Context, op string, files []DriftFile, configRoot string) (stagedSet, error) {
	var set stagedSet

	// Written first, so a branch carrying a newly encrypted secrets.yaml
	// also carries the config to decrypt it.
	wroteConfig, err := g.stageSopsConfig(ctx, op)
	if err != nil {
		return stagedSet{}, fmt.Errorf("gitsync: %s: %w", op, err)
	}
	set.SopsConfig = wroteConfig

	for _, f := range files {
		p := f.Path
		if err := refuseUnsyncablePath(p); err != nil {
			return stagedSet{}, fmt.Errorf("gitsync: %s: %w", op, err)
		}

		copied, err := g.copyLiveIntoWorkdir(ctx, configRoot, p)
		if err != nil {
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
// that means. Both ends go through guardDriftPath, and a configured Crypter
// encrypts in place before this returns, so plaintext is never staged.
func (g *GitSync) copyLiveIntoWorkdir(ctx context.Context, configRoot, p string) (bool, error) {
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
	if err := os.MkdirAll(filepath.Dir(repoPath), 0o750); err != nil {
		return false, err
	}
	if err := os.WriteFile(repoPath, content, 0o644); err != nil { // #nosec G304,G306,G703 -- repoPath is guardDriftPath-confined (symlink-resolved) under g.Workdir; config content, not secret
		return false, err
	}
	if err := g.encryptInWorkdir(ctx, repoPath, p, content); err != nil {
		return false, err
	}
	return true, nil
}

// encryptInWorkdir encrypts the just-written worktree copy of p when its
// content calls for it; a no-op with no age key. content is passed in
// rather than re-read, so the answer cannot differ from what was written.
// A NeedsEncryption refusal fails the whole operation: skipping would push
// a plaintext secret, encrypting anyway would corrupt the file.
func (g *GitSync) encryptInWorkdir(ctx context.Context, repoPath, p string, content []byte) error {
	if !g.Crypter.Enabled() {
		if EncryptionEnabled() {
			// Fail closed on a half-wired agent: the switch alone has
			// already let secrets.yaml through Excluded and
			// secretShapedDisallowed, with nothing left to encrypt it.
			return fmt.Errorf(
				"encryption is enabled but no age key is loaded - refusing to write %s to the repository in the clear", p)
		}
		return nil
	}
	need, refusal := sopscrypt.NeedsEncryption(p, content)
	if refusal != "" {
		return &EncryptRefusedError{Path: p, Reason: refusal}
	}
	if !need {
		return nil
	}
	if err := g.Crypter.EncryptFileInPlace(ctx, repoPath, p); err != nil {
		return fmt.Errorf("encrypting %s: %w", p, err)
	}
	return nil
}

// EncryptRefusedError reports one file holding a secret SOPS cannot encrypt
// without breaking it (see sopscrypt.NeedsEncryption). Typed so a caller
// walking a whole tree can gather every one and report them together.
type EncryptRefusedError struct {
	Path   string
	Reason string
}

func (e *EncryptRefusedError) Error() string {
	return e.Path + " " + e.Reason
}

// ensureSopsConfig writes the .sops.yaml the agent manages at the worktree
// root and reports whether it had to; a no-op with no age key or when the
// file is already right, which keeps it out of every commit after the
// first. It lets a human run plain "sops <file>" in their own clone, and is
// in ExcludedPatterns so it never reaches /homeassistant.
func (g *GitSync) ensureSopsConfig() (bool, error) {
	if !g.Crypter.Enabled() {
		return false, nil
	}
	want := g.Crypter.SopsConfig()
	// Guarded and unlinked first: a tracked .sops.yaml that is a SYMLINK
	// survives clone and checkout, and an unguarded write would follow it
	// (pointed at /homeassistant/configuration.yaml, say).
	full, err := guardDriftPath(g.Workdir, sopscrypt.ConfigFile)
	if err != nil {
		return false, err
	}
	if existing, err := os.ReadFile(full); err == nil && bytes.Equal(existing, want) { // #nosec G304 -- guardDriftPath-confined under g.Workdir
		return false, nil
	}
	if err := os.Remove(full); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, fmt.Errorf("replacing %s: %w", sopscrypt.ConfigFile, err)
	}
	if err := os.WriteFile(full, want, 0o644); err != nil { // #nosec G306 -- public recipient and path rules, nothing secret
		return false, fmt.Errorf("writing %s: %w", sopscrypt.ConfigFile, err)
	}
	return true, nil
}

// stageSopsConfig is ensureSopsConfig plus the explicit "git add" stageDrift
// needs, since it stages path by path rather than in one bulk call, and
// reports whether the config was actually staged.
func (g *GitSync) stageSopsConfig(ctx context.Context, op string) (bool, error) {
	written, err := g.ensureSopsConfig()
	if err != nil || !written {
		return false, err
	}
	return g.gitAddSkippingIgnored(ctx, op, sopscrypt.ConfigFile)
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
		// matchesSecretPattern, NOT the encryption-gated version: the gate
		// excuses a file DECLARED secrets.yaml, but the declared name picks
		// the encryption rules, so "automations.yaml -> secrets.yaml" would
		// be encrypted key-by-key and commit most values in the clear.
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
func (g *GitSync) restoreDetachedCheckout(ctx context.Context, sha, branch string) {
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
	if _, err := g.runGit(ctx, []string{"checkout", "--detach", "--force", sha}, "", nil); err != nil {
		slog.Warn("gitsync: could not restore detached checkout", "sha", sha, "error", err)
		return false
	}
	if _, err := g.runGit(ctx, []string{"clean", "-fdx"}, "", nil); err != nil {
		slog.Warn("gitsync: could not clean workdir", "error", err)
	}
	return true
}
