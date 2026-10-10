package gitsync

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
)

// This file is the READ side of bidirectional sync: the three questions
// internal/recon's classifier asks of a commit before it decides whether a
// drifting path moved in the repository, moved live, or moved on both sides.
// Everything here reads through the object database ("git cat-file", "git
// diff", "git show") rather than the working tree, which is the property
// that makes classification free to run every cycle: no checkout, so the
// detached tree internal/differ and internal/applier read between calls is
// never disturbed and need not sit at any particular commit. RecordFile's
// blobMatches earns its no-op path the same way.

// CommitReachable reports whether sha names a commit object in the local
// clone. The classifier's merge base is a SHA persisted across restarts
// (applier.State's LastGoodSHA and friends), so by the time it is read the
// remote may have been force-pushed, rewritten or garbage-collected out
// from under it. A missing base is not an error - it is the answer that
// sends the cycle down its apply-only path - so this returns a bool and
// keeps errors for a git that would not run at all.
func (g *GitSync) CommitReachable(ctx context.Context, sha string) (bool, error) {
	if sha == "" {
		return false, nil
	}
	// "^{commit}" peels, so a SHA naming a blob or a tree answers false
	// rather than true-but-unusable. Non-zero exit is the ANSWER here, which
	// is what runGitStatus is for.
	return g.runGitStatus(ctx, []string{"cat-file", "-e", sha + "^{commit}"}, "", nil)
}

// IsAncestor reports whether ancestor is reachable from descendant, which is
// how the classifier tells two candidate merge bases apart. The agent records
// LastGoodSHA and LastImportSHA independently, and an import deliberately does
// not advance LastGoodSHA, so on any install that applied before it imported
// the two disagree. Picking the older one reads every path the import moved as
// "the repository moved", which turns the user's next live edit to one of
// those files into a conflict that is not real.
//
// Like CommitReachable, non-zero exit is the answer rather than a failure.
func (g *GitSync) IsAncestor(ctx context.Context, ancestor, descendant string) (bool, error) {
	if ancestor == "" || descendant == "" {
		return false, nil
	}
	return g.runGitStatus(ctx, []string{"merge-base", "--is-ancestor", ancestor, descendant}, "", nil)
}

// ChangedBetween names every path whose blob differs between base and tip.
// One subprocess for the whole tree rather than a query per path: the caller
// intersects the result with its own much smaller drift set, and a pathspec
// per path would be both slower and a footgun, since pathspecs carry glob
// magic that a repo-relative path must not be subjected to.
//
// Names only - no blob is read - so a file that is large or binary costs
// exactly what a one-line YAML costs. base == tip short
// circuits to nothing changed without launching git, which is the common
// case for an agent whose repository nobody else pushes to.
func (g *GitSync) ChangedBetween(ctx context.Context, base, tip string) ([]string, error) {
	if base == "" || tip == "" {
		return nil, fmt.Errorf("gitsync: changed-between: needs two commits, got %q and %q", base, tip)
	}
	if base == tip {
		return nil, nil
	}

	// -z for TrackedFilesRaw's reason: without it git C-quotes any non-ASCII
	// path and the caller's intersection against differ's paths stops
	// matching. --no-renames because rename detection (on by default) lists
	// only a renamed file's NEW name, so its old path would read as "the
	// repository did not move" and a live copy of it get captured back
	// beside the renamed one. The trailing "--" settles revision-versus-
	// path ambiguity rather than trusting both arguments to look like SHAs.
	result, err := g.runGit(ctx, []string{"diff", "--name-only", "--no-renames", "-z", base, tip, "--"}, "", nil)
	if err != nil {
		return nil, err
	}

	var files []string
	for _, p := range strings.Split(result.Stdout, "\x00") {
		if p != "" {
			files = append(files, p)
		}
	}
	return files, nil
}

// BlobEquivalent reports whether the blob tracked at path in sha says the
// same thing as the live bytes, and separately whether sha tracks the path
// at all. Those are the two facts the classifier needs from a merge base:
// equivalent decides "did live move", and tracked tells a file ADDED since
// the base from one EDITED since it, which get opposite verdicts.
//
// A commit that does not track the path is (false, false, nil) - not an
// error - matching blobMatches, which reads a non-zero "git show" as "not
// tracked at sha" for the same reason. Otherwise this is a byte compare.
func (g *GitSync) BlobEquivalent(ctx context.Context, sha, path string, live []byte) (equivalent, tracked bool, err error) {
	if sha == "" {
		return false, false, fmt.Errorf("gitsync: blob-equivalent: no commit given for %s", path)
	}

	blob, tracked, err := g.blobAt(ctx, sha, path)
	if err != nil || !tracked {
		return false, false, err
	}
	// Compared as strings so the blob is never copied into a second buffer:
	// the compiler turns this into an allocation-free comparison.
	return blob == string(live), true, nil
}

// blobAt reads path's blob at sha. tracked is false, with no error, when
// sha does not hold the path: a non-zero "git show" is that answer.
func (g *GitSync) blobAt(ctx context.Context, sha, path string) (blob string, tracked bool, err error) {
	result, err := g.runGitRaw(ctx, []string{"show", sha + ":" + path}, "", nil, 0)
	if err != nil {
		return "", false, err
	}
	if result.ExitCode != 0 {
		return "", false, nil
	}
	return result.Stdout, true, nil
}

// maxCompareBytes caps what LiveFactsAt reads into memory. internal/differ
// compares anything above its own threshold in lockstep chunks precisely so
// a pair of large files is never held at once, and a classifier that read
// both whole would undo that on the same files. Above the cap the answer is
// ErrNotComparable rather than a guess.
const maxCompareBytes = 4 * 1024 * 1024

// ErrNotComparable reports a live file that exists but was deliberately not
// read. The caller defers the path rather than treating it as either side
// having moved - distinct from every other error out of LiveFactsAt, which
// means the base could not be read and must fail closed to a conflict.
var ErrNotComparable = errors.New("gitsync: file is too large to compare in memory")

// LiveFacts is what one path's live copy says, relative to a base commit.
type LiveFacts struct {
	// MatchesBase is whether the live content still says what the base blob
	// says. False whenever the base does not track the path, and false for a
	// path that could not be read live, where the caller uses Gone instead.
	MatchesBase bool
	// BaseTracks is whether the base commit holds the path at all, which is
	// what tells a file ADDED since the base from one EDITED since it.
	BaseTracks bool
	// Gone is whether the live file is genuinely absent - fs.ErrNotExist and
	// nothing else, liveFileIsGone's rule, because internal/differ reports
	// every stat failure as "add" and a removal staged over an unreadable
	// moment deletes a file nobody deleted.
	Gone bool
	// BaseIsSops is whether the base blob is a SOPS document: ciphertext a
	// version before 0.9.0 committed, while live holds what that version
	// decrypted out of it. Comparing the two says nothing about which side
	// moved, so the caller treats such a base as no base at all.
	BaseIsSops bool
}

// LiveFactsAt answers all three questions the three-way classifier asks
// about one path, in one call, so the live read happens HERE - beside
// guardDriftPath, which resolves symlinks all the way down and refuses a
// path escaping its root or landing on an excluded or secret-shaped one -
// rather than in a caller joining paths by hand.
//
// An unreadable-but-present file is (false, tracked, false): not gone, not
// matching, which the classifier defers. A file over maxCompareBytes is
// ErrNotComparable, also a defer. Any other error means the BASE could not
// be read, and the classifier turns that into a conflict rather than
// overwriting content it could not check.
func (g *GitSync) LiveFactsAt(ctx context.Context, sha, configRoot, path string) (LiveFacts, error) {
	livePath, err := guardDriftPath(configRoot, path)
	if err != nil {
		return LiveFacts{}, err
	}

	info, statErr := os.Stat(livePath)
	switch {
	case statErr != nil || !info.Mode().IsRegular():
		// Absent, unreadable, or no longer a regular file. Only the first is
		// a deletion, and only fs.ErrNotExist proves it.
		gone, err := liveFileIsGone(configRoot, path)
		if err != nil {
			return LiveFacts{}, err
		}
		blob, tracked, err := g.blobAt(ctx, sha, path)
		if err != nil {
			return LiveFacts{}, err
		}
		return LiveFacts{BaseTracks: tracked, Gone: gone, BaseIsSops: tracked && isSopsDocument([]byte(blob))}, nil

	case info.Size() > maxCompareBytes:
		return LiveFacts{}, fmt.Errorf("%w: %s", ErrNotComparable, path)
	}

	blob, tracked, err := g.blobAt(ctx, sha, path)
	if err != nil {
		return LiveFacts{}, err
	}
	baseIsSops := tracked && isSopsDocument([]byte(blob))

	live, err := os.ReadFile(livePath) // #nosec G304 -- livePath is guardDriftPath-confined (symlink-resolved) under configRoot
	if err != nil {
		// Raced between the stat and the read. Not gone as far as anything
		// here can prove, so it defers.
		return LiveFacts{BaseTracks: tracked, BaseIsSops: baseIsSops}, nil
	}
	// Compared as strings, BlobEquivalent's reason.
	return LiveFacts{MatchesBase: tracked && blob == string(live), BaseTracks: tracked, BaseIsSops: baseIsSops}, nil
}
