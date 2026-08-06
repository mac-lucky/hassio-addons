package gitsync

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
)

// The encrypted tests here flip the package-wide encryption switch through
// enableEncryption, so nothing in this file may call t.Parallel.

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

// Without this the ciphertext's own nondeterminism reads as drift, and an
// encrypted file is captured on every single cycle forever.
func TestBlobEquivalentComparesEncryptedContentSemantically(t *testing.T) {
	tmp := t.TempDir()
	bare, work := makeRemote(t, tmp, "remote")
	const live = "http_password: \"abc#123\"\nunused:\n"
	commitFile(t, work, "secrets.yaml", fakeEncrypt(live), "seed encrypted secrets")

	gs := New(makeOpts("file://"+bare), filepath.Join(tmp, "clone"))
	fake := enableEncryption(t, gs)
	ctx := context.Background()
	if err := gs.EnsureClone(ctx); err != nil {
		t.Fatalf("EnsureClone: %v", err)
	}
	sha, err := gs.Fetch(ctx)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	// Stands in for sops re-emitting from its own parse: quotes dropped,
	// an empty value written as null. Byte-different, same document.
	fake.decryptRewrite = func(plaintext string) string {
		return strings.ReplaceAll(strings.ReplaceAll(plaintext, `"abc#123"`, "abc#123"), "unused:\n", "unused: null\n")
	}

	equiv, tracked, err := gs.BlobEquivalent(ctx, sha, "secrets.yaml", []byte(live))
	if err != nil {
		t.Fatalf("BlobEquivalent: %v", err)
	}
	if !tracked {
		t.Fatal("tracked = false, want true")
	}
	if !equiv {
		t.Error("equivalent = false, want true: sops formatting is not a live edit")
	}

	// And a real edit still reads as one, or the comparison is worthless.
	equiv, _, err = gs.BlobEquivalent(ctx, sha, "secrets.yaml", []byte("http_password: \"different\"\nunused:\n"))
	if err != nil {
		t.Fatalf("BlobEquivalent on changed content: %v", err)
	}
	if equiv {
		t.Error("equivalent = true for genuinely different content, want false")
	}
}

// The temp file exists so sops has a path to open. It must hold ciphertext,
// keep its basename so sops picks the right store, live outside the worktree
// where "git clean -fdx" and a capture's staging cannot see it, and be gone
// afterwards.
func TestBlobEquivalentMaterializesTheBaseBlobSafely(t *testing.T) {
	tmp := t.TempDir()
	bare, work := makeRemote(t, tmp, "remote")
	const live = "http_password: secret\n"
	commitFile(t, work, "secrets.yaml", fakeEncrypt(live), "seed encrypted secrets")

	gs := New(makeOpts("file://"+bare), filepath.Join(tmp, "clone"))
	fake := enableEncryption(t, gs)
	ctx := context.Background()
	if err := gs.EnsureClone(ctx); err != nil {
		t.Fatalf("EnsureClone: %v", err)
	}
	sha, err := gs.Fetch(ctx)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	if _, _, err := gs.BlobEquivalent(ctx, sha, "secrets.yaml", []byte(live)); err != nil {
		t.Fatalf("BlobEquivalent: %v", err)
	}

	if len(fake.calls) == 0 {
		t.Fatal("sops was never invoked, so nothing about the temp file is proven")
	}
	last := fake.calls[len(fake.calls)-1]
	handed := last[len(last)-1]

	if got := filepath.Base(handed); got != "secrets.yaml" {
		t.Errorf("sops was handed %q, want the basename kept as secrets.yaml: it picks its store from the extension", got)
	}
	if strings.HasPrefix(handed, gs.Workdir+string(filepath.Separator)) {
		t.Errorf("base blob was materialized inside the worktree at %q, where clean -fdx and a capture's staging would see it", handed)
	}
	if _, err := os.Stat(handed); !os.IsNotExist(err) {
		t.Errorf("os.Stat(%q) err = %v, want the temp directory removed", handed, err)
	}
}

// Fail closed: the caller turns an error into a conflict, which refuses both
// directions. Answering "not equivalent" would call it a live edit and push
// the live copy over whatever the repository holds.
func TestBlobEquivalentFailsClosedWhenTheBaseBlobCannotBeDecrypted(t *testing.T) {
	tmp := t.TempDir()
	bare, work := makeRemote(t, tmp, "remote")
	commitFile(t, work, "secrets.yaml", fakeEncrypt("http_password: secret\n"), "seed encrypted secrets")

	gs := New(makeOpts("file://"+bare), filepath.Join(tmp, "clone"))
	ctx := context.Background()
	if err := gs.EnsureClone(ctx); err != nil {
		t.Fatalf("EnsureClone: %v", err)
	}
	sha, err := gs.Fetch(ctx)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	// No Crypter configured at all, which is what a missing age_key looks
	// like from here.
	_, tracked, err := gs.BlobEquivalent(ctx, sha, "secrets.yaml", []byte("http_password: secret\n"))
	if err == nil {
		t.Fatal("BlobEquivalent() error = nil, want a refusal it cannot decrypt")
	}
	if !tracked {
		t.Error("tracked = false, want true: the commit does track the path, it just could not be read")
	}
	if !strings.Contains(err.Error(), "age key") {
		t.Errorf("error = %v, want it to name the missing age key", err)
	}
}
