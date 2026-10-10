package gitsync

import (
	"context"
	"slices"
	"sort"
	"strings"
	"testing"
)

// zeroSHA is a well-formed object name nothing in a fresh repository can
// resolve - a stand-in for a base commit the remote has since rewritten
// away.
const zeroSHA = "0000000000000000000000000000000000000000"

// --- CommitReachable ------------------------------------------------------

// The classifier's merge base is a SHA read back out of state.json, so
// "still there?" has to be answerable without treating "no" as a failure.
func TestCommitReachableAnswersRatherThanFailing(t *testing.T) {
	f := newRecordFixture(t)
	ctx := context.Background()

	cases := []struct {
		name string
		sha  string
		want bool
	}{
		{"the fetched tip", f.sha, true},
		{"a commit the clone does not have", zeroSHA, false},
		{"no base recorded yet", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := f.gs.CommitReachable(ctx, tc.sha)
			if err != nil {
				t.Fatalf("CommitReachable(%q) error = %v, want nil: an absent commit is an ANSWER", tc.sha, err)
			}
			if got != tc.want {
				t.Errorf("CommitReachable(%q) = %v, want %v", tc.sha, got, tc.want)
			}
		})
	}
}

// A SHA that resolves to a blob is not a usable base, and "^{commit}" is
// what makes the difference visible.
func TestCommitReachableRefusesANonCommitObject(t *testing.T) {
	f := newRecordFixture(t)
	ctx := context.Background()

	result, err := f.gs.runGit(ctx, []string{"rev-parse", f.sha + ":automations.yaml"}, "", nil)
	if err != nil {
		t.Fatalf("rev-parse blob: %v", err)
	}
	blobSHA := strings.TrimSpace(result.Stdout)

	got, err := f.gs.CommitReachable(ctx, blobSHA)
	if err != nil {
		t.Fatalf("CommitReachable: %v", err)
	}
	if got {
		t.Errorf("CommitReachable(<blob>) = true, want false: a blob cannot be a merge base")
	}
}

// --- ChangedBetween -------------------------------------------------------

func TestChangedBetweenNamesOnlyThePathsWhoseBlobMoved(t *testing.T) {
	f := newRecordFixture(t)
	ctx := context.Background()
	base := f.sha

	// Moved and stayed moved.
	commitFile(t, f.work, "automations.yaml", "- id: edited\n", "edit automations")
	// Added outright.
	commitFile(t, f.work, "packages/demo.yaml", "demo: 1\n", "add package")
	// Moved and moved back: git compares the two ENDPOINTS, so this must
	// not appear however many commits it took to get there.
	commitFile(t, f.work, "scripts.yaml", "a: 1\n", "add scripts")
	commitFile(t, f.work, "scripts.yaml", "a: 2\n", "change scripts")
	commitFile(t, f.work, "scripts.yaml", "a: 1\n", "change scripts back")

	tip, err := f.gs.Fetch(ctx)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	changed, err := f.gs.ChangedBetween(ctx, base, tip)
	if err != nil {
		t.Fatalf("ChangedBetween: %v", err)
	}

	// scripts.yaml is in the wanted set because it did not exist at base and
	// does at tip, whatever it did in between; the reverted-path property is
	// the next case, on a path that exists at both ends.
	want := []string{"automations.yaml", "packages/demo.yaml", "scripts.yaml"}
	sort.Strings(changed)
	if !slices.Equal(changed, want) {
		t.Errorf("ChangedBetween() = %v, want exactly %v", changed, want)
	}
}

// The endpoints are all that matter: a file edited and reverted between two
// commits did not change, and reporting it would make the classifier ask a
// question about a path with no answer.
func TestChangedBetweenIgnoresAPathEditedAndReverted(t *testing.T) {
	f := newRecordFixture(t)
	ctx := context.Background()
	base := f.sha

	commitFile(t, f.work, "automations.yaml", "- id: temporary\n", "edit")
	commitFile(t, f.work, "automations.yaml", "- id: demo\n", "revert")

	tip, err := f.gs.Fetch(ctx)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if tip == base {
		t.Fatal("tip did not move; the fixture is not exercising anything")
	}

	changed, err := f.gs.ChangedBetween(ctx, base, tip)
	if err != nil {
		t.Fatalf("ChangedBetween: %v", err)
	}
	if len(changed) != 0 {
		t.Errorf("ChangedBetween() = %v, want nothing: the endpoints hold identical content", changed)
	}
}

// The short circuit is what keeps a quiet repository free of subprocesses.
func TestChangedBetweenShortCircuitsOnIdenticalCommits(t *testing.T) {
	f := newRecordFixture(t)

	changed, err := f.gs.ChangedBetween(context.Background(), f.sha, f.sha)
	if err != nil {
		t.Fatalf("ChangedBetween: %v", err)
	}
	if changed != nil {
		t.Errorf("ChangedBetween(sha, sha) = %v, want nil", changed)
	}
}

func TestChangedBetweenRefusesAnEmptyCommit(t *testing.T) {
	f := newRecordFixture(t)

	if _, err := f.gs.ChangedBetween(context.Background(), "", f.sha); err == nil {
		t.Error("ChangedBetween(\"\", tip) error = nil, want a refusal")
	}
}

// --- BlobEquivalent -------------------------------------------------------

func TestBlobEquivalentReadsThroughGitShowWithoutTouchingTheCheckout(t *testing.T) {
	f := newRecordFixture(t)
	ctx := context.Background()

	cases := []struct {
		name        string
		path        string
		live        string
		wantEquiv   bool
		wantTracked bool
	}{
		{"live matches the blob", "automations.yaml", "- id: demo\n", true, true},
		{"live moved", "automations.yaml", "- id: edited\n", false, true},
		{"the commit does not track it", "packages/new.yaml", "anything\n", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			equiv, tracked, err := f.gs.BlobEquivalent(ctx, f.sha, tc.path, []byte(tc.live))
			if err != nil {
				t.Fatalf("BlobEquivalent: %v", err)
			}
			if equiv != tc.wantEquiv || tracked != tc.wantTracked {
				t.Errorf("BlobEquivalent() = (%v, %v), want (%v, %v)", equiv, tracked, tc.wantEquiv, tc.wantTracked)
			}
		})
	}

	// The whole point of reading through the object database: the detached
	// checkout the differ and applier are looking at is untouched.
	if got := f.gs.CurrentSHA(ctx); got != f.sha {
		t.Errorf("CurrentSHA() = %q, want %q: BlobEquivalent must not move HEAD", got, f.sha)
	}
}

// Rename detection lists only the new name, so a renamed file's old path
// read as untouched and a live copy of it was captured back beside the
// renamed one.
func TestChangedBetweenNamesBothSidesOfARename(t *testing.T) {
	f := newRecordFixture(t)
	ctx := context.Background()
	commitFile(t, f.work, "automations.yaml", "- id: a\n  alias: long enough to be detected as a rename\n", "add")
	base, err := f.gs.Fetch(ctx)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	runGitHelper(t, f.work, "mv", "automations.yaml", "automations-renamed.yaml")
	runGitHelper(t, f.work, "commit", "-m", "rename")
	runGitHelper(t, f.work, "push", "origin", "main")
	tip, err := f.gs.Fetch(ctx)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	changed, err := f.gs.ChangedBetween(ctx, base, tip)
	if err != nil {
		t.Fatalf("ChangedBetween: %v", err)
	}

	sort.Strings(changed)
	if want := []string{"automations-renamed.yaml", "automations.yaml"}; !slices.Equal(changed, want) {
		t.Errorf("ChangedBetween() = %v, want %v", changed, want)
	}
}
