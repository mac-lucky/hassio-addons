package gitsync

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/options"
)

// --- pure logic: Excluded / SecretPatterns -------------------------------
// No git binary, no filesystem, no network. Redaction is covered elsewhere:
// execx.Redact in internal/execx, its inputs in credentials_test.go.

func TestExcluded(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{".storage/foo", true},
		{"sub/.storage/foo", true},
		{"home-assistant_v2.db", true},
		{"home-assistant.log.1", true},
		{"secrets.yaml", true},
		{"gitops/registries.yaml", true},
		{"gitops/helpers.yaml", true},
		{"automations.yaml", false},
		{"packages/demo.yaml", false},
	}
	for _, c := range cases {
		if got := Excluded(c.path); got != c.want {
			t.Errorf("Excluded(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}

// A copy step treats an existing directory as its destination and copies
// into it, so an unfiltered file named ".storage" writes inside the registry.
func TestExcludedMatchesBareFileNamedLikeDirectoryEntry(t *testing.T) {
	var dirStyleEntries []string
	for _, e := range ExcludedPatterns {
		// Leading "/" is the root-anchored form, covered by
		// TestGitopsExclusionRootAnchoredOnly and TestExcludedImageIsRootAnchored.
		if len(e) > 0 && e[len(e)-1] == '/' && e[0] != '/' {
			dirStyleEntries = append(dirStyleEntries, e[:len(e)-1])
		}
	}
	// Derived from ExcludedPatterns, so a new directory entry is covered
	// the moment it is added.
	if len(dirStyleEntries) == 0 {
		t.Fatal("no directory-style entries in ExcludedPatterns, so this test checks nothing")
	}
	for _, name := range dirStyleEntries {
		if !Excluded(name) {
			t.Errorf("Excluded(%q) = false, want true (bare file)", name)
		}
		if !Excluded("sub/" + name) {
			t.Errorf("Excluded(%q) = false, want true (nested file)", "sub/"+name)
		}
	}
}

// Only the repository-root gitops/ is agent input; a nested one (bundled
// in a custom component, say) is ordinary config and must sync.
func TestGitopsExclusionRootAnchoredOnly(t *testing.T) {
	for _, p := range []string{"gitops", "gitops/registries.yaml", "gitops/helpers.yaml", "gitops/nested/deep.yaml"} {
		if !Excluded(p) {
			t.Errorf("Excluded(%q) = false, want true", p)
		}
	}
	for _, p := range []string{"sub/gitops", "sub/gitops/registries.yaml", "custom_components/foo/gitops/bar.yaml"} {
		if Excluded(p) {
			t.Errorf("Excluded(%q) = true, want false", p)
		}
	}
}

// A ".."-laden path must resolve the way callers eventually clean it, or
// it dodges the root-anchored gitops/ guard on its first segment.
func TestExcludedNormalizesDotDotBeforeSegmentCheck(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"sub/../gitops/registries.yaml", true},
		{"sub/../gitops/foo.yaml", true},
		{"a/b/../../gitops/x.yaml", true},
		{"sub/gitops/../../gitops/y.yaml", true},
		// Still legitimately outside gitops/ after cleaning.
		{"sub/../automations.yaml", false},
	}
	for _, c := range cases {
		if got := Excluded(c.path); got != c.want {
			t.Errorf("Excluded(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}

// Still ".."-prefixed after path.Clean means it is not repo-relative at
// all, so it is excluded rather than fed to rooted segment matching.
func TestExcludedFailsClosedOnPathsThatClimbAboveRoot(t *testing.T) {
	for _, p := range []string{"..", "../etc/passwd", "../../gitops/registries.yaml", "../automations.yaml"} {
		if !Excluded(p) {
			t.Errorf("Excluded(%q) = false, want true (fail closed)", p)
		}
	}
}

// Encryption switches off the secrets.yaml entry, making the config-root
// file syncable; it does not make .storage/ or .ssh/ syncable.
func TestExcludedDirectoryEntriesOutrankTheConditionalSecretsEntry(t *testing.T) {
	for _, on := range []bool{false, true} {
		SetEncryptionEnabled(on)
		for _, p := range []string{".storage/secrets.yaml", ".ssh/secrets.yaml", "sub/.cloud/secrets.yaml"} {
			if !Excluded(p) {
				t.Errorf("Excluded(%q) = false with encryption=%v, want true", p, on)
			}
		}
		if got := Excluded("secrets.yaml"); got == on {
			t.Errorf("Excluded(\"secrets.yaml\") = %v with encryption=%v, want %v", got, on, !on)
		}
	}
	SetEncryptionEnabled(false)
}

func TestExcludedAndSecretPatternsAreNonempty(t *testing.T) {
	if !contains(ExcludedPatterns, ".storage/") {
		t.Error("ExcludedPatterns missing \".storage/\"")
	}
	if !contains(ExcludedPatterns, "secrets.yaml") {
		t.Error("ExcludedPatterns missing \"secrets.yaml\"")
	}
	if !contains(SecretPatterns, "secrets.yaml") {
		t.Error("SecretPatterns missing \"secrets.yaml\"")
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func TestGuardSecretsRaisesOnTrackedSecretFiles(t *testing.T) {
	gs := New(makeOpts("file:///unused"), t.TempDir())

	cases := [][]string{
		{"secrets.yaml"},
		{"config/secrets.yaml"},
		{".ssh/id_rsa"},
		{"id_rsa"},
		{".env"},
		{"SECRETS.YAML"}, // case-insensitive
	}
	// With encryption off, GuardSecretsAt answers from the path list alone
	// and never resolves the sha.
	for _, files := range cases {
		err := gs.GuardSecretsAt(context.Background(), "unused-sha", files)
		var target *SecretsTrackedError
		if !errors.As(err, &target) {
			t.Errorf("GuardSecretsAt(%v) error = %v, want *SecretsTrackedError", files, err)
		}
	}
}

// recon.ReconcileNow supplies its own "refusing to sync: secrets tracked
// in repository: " prefix, so anything more here doubles it.
func TestSecretsTrackedErrorMessageIsJustTheFileList(t *testing.T) {
	err := &SecretsTrackedError{Files: []string{"secrets.yaml"}}
	if got, want := err.Error(), "secrets.yaml"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}

	multi := &SecretsTrackedError{Files: []string{"secrets.yaml", ".env"}}
	if got, want := multi.Error(), "secrets.yaml, .env"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestGuardSecretsDoesNotRaiseOnNormalFiles(t *testing.T) {
	gs := New(makeOpts("file:///unused"), t.TempDir())
	err := gs.GuardSecretsAt(context.Background(), "unused-sha", []string{"automations.yaml", "packages/demo.yaml"})
	if err != nil {
		t.Errorf("GuardSecretsAt() error = %v, want nil", err)
	}
}

// --- real-git integration -------------------------------------------------
// Real local repositories under t.TempDir() rather than a mocked
// subprocess, so git itself is exercised end to end.

func makeOpts(repoURL string) options.Options {
	return options.Options{
		RepoURL:            repoURL,
		Branch:             "main",
		IntervalMinutes:    5,
		DryRun:             true,
		ApplyAfterPull:     "reload",
		ReconcileYAMLFiles: true,
	}
}

func runGitHelper(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...) // #nosec G204 -- fixed "git" binary; args are test-controlled fixture setup, never external input
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.invalid",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.invalid",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, out)
	}
}

// makeRemote creates a bare "remote" plus a checkout that pushes into it.
func makeRemote(t *testing.T, tmp, name string) (bare, work string) {
	t.Helper()
	bare = filepath.Join(tmp, name+".git")
	work = filepath.Join(tmp, name+"-work")
	runGitHelper(t, tmp, "init", "--bare", "-b", "main", bare)
	runGitHelper(t, tmp, "init", "-b", "main", work)
	runGitHelper(t, work, "remote", "add", "origin", "file://"+bare)
	return bare, work
}

func commitFile(t *testing.T, work, relPath, content, message string) {
	t.Helper()
	full := filepath.Join(work, relPath)
	if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitHelper(t, work, "add", relPath)
	runGitHelper(t, work, "commit", "-m", message)
	runGitHelper(t, work, "push", "origin", "main")
}

func TestEnsureCloneThenFetchThenCheckout(t *testing.T) {
	tmp := t.TempDir()
	bare, work := makeRemote(t, tmp, "remote")
	commitFile(t, work, "automations.yaml", "- id: demo\n", "commit")

	workdir := filepath.Join(tmp, "clone")
	gs := New(makeOpts("file://"+bare), workdir)
	ctx := context.Background()

	if err := gs.EnsureClone(ctx); err != nil {
		t.Fatalf("EnsureClone: %v", err)
	}
	if info, err := os.Stat(filepath.Join(workdir, ".git")); err != nil || !info.IsDir() {
		t.Fatal(".git dir missing after EnsureClone")
	}
	// --no-checkout: working tree stays empty until Checkout runs.
	if _, err := os.Stat(filepath.Join(workdir, "automations.yaml")); !os.IsNotExist(err) {
		t.Fatal("automations.yaml should not exist before Checkout")
	}

	sha, err := gs.Fetch(ctx)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(sha) != 40 {
		t.Fatalf("len(sha) = %d, want 40", len(sha))
	}

	if err := gs.Checkout(ctx, sha); err != nil {
		t.Fatalf("Checkout: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(workdir, "automations.yaml")) // #nosec G304 -- workdir is a t.TempDir() fixture path this test created
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "- id: demo\n" {
		t.Errorf("content = %q", data)
	}
	if got := gs.CurrentSHA(ctx); got != sha {
		t.Errorf("CurrentSHA() = %q, want %q", got, sha)
	}
}

func TestFetchReturnsNewSHAAndCheckoutSwitchesContent(t *testing.T) {
	tmp := t.TempDir()
	bare, work := makeRemote(t, tmp, "remote")
	commitFile(t, work, "automations.yaml", "- id: demo\n", "commit")

	workdir := filepath.Join(tmp, "clone")
	gs := New(makeOpts("file://"+bare), workdir)
	ctx := context.Background()

	if err := gs.EnsureClone(ctx); err != nil {
		t.Fatalf("EnsureClone: %v", err)
	}
	sha1, err := gs.Fetch(ctx)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if err := gs.Checkout(ctx, sha1); err != nil {
		t.Fatalf("Checkout: %v", err)
	}

	commitFile(t, work, "automations.yaml", "- id: demo\n  alias: Demo\n", "update")
	sha2, err := gs.Fetch(ctx)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if sha2 == sha1 {
		t.Fatal("sha2 == sha1, want a new sha")
	}

	if err := gs.Checkout(ctx, sha2); err != nil {
		t.Fatalf("Checkout: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(workdir, "automations.yaml")) // #nosec G304 -- workdir is a t.TempDir() fixture path this test created
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "alias: Demo") {
		t.Errorf("content = %q, want it to contain alias: Demo", data)
	}
	if got := gs.CurrentSHA(ctx); got != sha2 {
		t.Errorf("CurrentSHA() = %q, want %q", got, sha2)
	}
}

func TestCurrentSHANoneBeforeClone(t *testing.T) {
	gs := New(makeOpts("file:///does-not-matter"), filepath.Join(t.TempDir(), "clone"))
	if got := gs.CurrentSHA(context.Background()); got != "" {
		t.Errorf("CurrentSHA() = %q, want empty", got)
	}
}

func TestEnsureCloneIdempotentAndReclonesOnOriginChange(t *testing.T) {
	tmp := t.TempDir()
	bare1, work1 := makeRemote(t, tmp, "remote1")
	commitFile(t, work1, "automations.yaml", "- id: one\n", "commit")
	bare2, work2 := makeRemote(t, tmp, "remote2")
	commitFile(t, work2, "automations.yaml", "- id: two\n", "commit")

	workdir := filepath.Join(tmp, "clone")
	gs := New(makeOpts("file://"+bare1), workdir)
	ctx := context.Background()

	if err := gs.EnsureClone(ctx); err != nil {
		t.Fatalf("EnsureClone: %v", err)
	}
	headPath := filepath.Join(workdir, ".git", "HEAD")
	firstInfo, err := os.Stat(headPath)
	if err != nil {
		t.Fatal(err)
	}

	if err := gs.EnsureClone(ctx); err != nil { // same origin: no-op
		t.Fatalf("EnsureClone (no-op): %v", err)
	}
	secondInfo, err := os.Stat(headPath)
	if err != nil {
		t.Fatal(err)
	}
	if !secondInfo.ModTime().Equal(firstInfo.ModTime()) {
		t.Error("HEAD mtime changed on a no-op EnsureClone")
	}

	gs.Opts = makeOpts("file://" + bare2)
	if err := gs.EnsureClone(ctx); err != nil { // origin changed: wipe + re-clone
		t.Fatalf("EnsureClone (re-clone): %v", err)
	}
	sha, err := gs.Fetch(ctx)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if err := gs.Checkout(ctx, sha); err != nil {
		t.Fatalf("Checkout: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(workdir, "automations.yaml")) // #nosec G304 -- workdir is a t.TempDir() fixture path this test created
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "- id: two\n" {
		t.Errorf("content = %q, want the remote2 content", data)
	}
}

func TestGuardSecretsCatchesRealTrackedSecretFiles(t *testing.T) {
	// TrackedFiles() filters out ExcludedPatterns - secrets.yaml and .ssh/
	// included - so the guard needs the unfiltered TrackedFilesRaw().
	tmp := t.TempDir()
	bare, work := makeRemote(t, tmp, "remote")
	commitFile(t, work, "automations.yaml", "- id: demo\n", "commit")
	commitFile(t, work, "secrets.yaml", "api_key: hunter2\n", "commit")
	commitFile(t, work, ".ssh/id_rsa", "-----BEGIN OPENSSH PRIVATE KEY-----\n", "commit")

	workdir := filepath.Join(tmp, "clone")
	gs := New(makeOpts("file://"+bare), workdir)
	ctx := context.Background()

	if err := gs.EnsureClone(ctx); err != nil {
		t.Fatalf("EnsureClone: %v", err)
	}
	sha, err := gs.Fetch(ctx)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	raw, err := gs.TrackedFilesRaw(ctx, sha)
	if err != nil {
		t.Fatalf("TrackedFilesRaw: %v", err)
	}
	if !contains(raw, "secrets.yaml") {
		t.Error("raw tracked files missing secrets.yaml")
	}
	if !contains(raw, ".ssh/id_rsa") {
		t.Error("raw tracked files missing .ssh/id_rsa")
	}

	var target *SecretsTrackedError
	if err := gs.GuardSecretsAt(ctx, sha, raw); !errors.As(err, &target) {
		t.Errorf("GuardSecretsAt(raw) error = %v, want *SecretsTrackedError", err)
	}

	// The trap: the filtered view has already dropped the offending paths,
	// so guarding with it lets a tracked secret through.
	filtered, err := gs.TrackedFiles(ctx, sha)
	if err != nil {
		t.Fatalf("TrackedFiles: %v", err)
	}
	if contains(filtered, "secrets.yaml") || contains(filtered, ".ssh/id_rsa") {
		t.Error("filtered tracked files still contain secret paths")
	}
	if err := gs.GuardSecretsAt(ctx, sha, filtered); err != nil {
		t.Errorf("GuardSecretsAt(filtered) error = %v, want nil (proves the trap is real)", err)
	}
}

func TestTrackedFilesOmitsExcluded(t *testing.T) {
	tmp := t.TempDir()
	bare, work := makeRemote(t, tmp, "remote")
	commitFile(t, work, "automations.yaml", "- id: demo\n", "commit")
	commitFile(t, work, ".storage/core.entity_registry", "{}", "commit")
	commitFile(t, work, "home-assistant.log", "boot ok\n", "commit")

	workdir := filepath.Join(tmp, "clone")
	gs := New(makeOpts("file://"+bare), workdir)
	ctx := context.Background()

	if err := gs.EnsureClone(ctx); err != nil {
		t.Fatalf("EnsureClone: %v", err)
	}
	sha, err := gs.Fetch(ctx)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	tracked, err := gs.TrackedFiles(ctx, sha)
	if err != nil {
		t.Fatalf("TrackedFiles: %v", err)
	}

	if !contains(tracked, "automations.yaml") {
		t.Error("tracked missing automations.yaml")
	}
	if contains(tracked, ".storage/core.entity_registry") {
		t.Error("tracked should omit .storage/core.entity_registry")
	}
	if contains(tracked, "home-assistant.log") {
		t.Error("tracked should omit home-assistant.log")
	}
}

func TestTrackedFilesOmitsGitopsManifests(t *testing.T) {
	// gitops/ is registry-layer input, never copied into /homeassistant.
	tmp := t.TempDir()
	bare, work := makeRemote(t, tmp, "remote")
	commitFile(t, work, "automations.yaml", "- id: demo\n", "commit")
	commitFile(t, work, "gitops/registries.yaml", "floors: []\n", "commit")
	commitFile(t, work, "gitops/helpers.yaml", "input_boolean: []\n", "commit")

	workdir := filepath.Join(tmp, "clone")
	gs := New(makeOpts("file://"+bare), workdir)
	ctx := context.Background()

	if err := gs.EnsureClone(ctx); err != nil {
		t.Fatalf("EnsureClone: %v", err)
	}
	sha, err := gs.Fetch(ctx)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	tracked, err := gs.TrackedFiles(ctx, sha)
	if err != nil {
		t.Fatalf("TrackedFiles: %v", err)
	}

	if !contains(tracked, "automations.yaml") {
		t.Error("tracked missing automations.yaml")
	}
	if contains(tracked, "gitops/registries.yaml") || contains(tracked, "gitops/helpers.yaml") {
		t.Error("tracked should omit gitops/ manifests")
	}
}

func TestTokenNeverPersistedUnderWorkdir(t *testing.T) {
	tmp := t.TempDir()
	bare, work := makeRemote(t, tmp, "remote")
	commitFile(t, work, "automations.yaml", "- id: demo\n", "commit")

	workdir := filepath.Join(tmp, "clone")
	token := "ghp_TESTTOKEN123"
	opts := makeOpts("file://" + bare)
	opts.GitUsername = "agent"
	opts.GitToken = token
	gs := New(opts, workdir)
	ctx := context.Background()

	if err := gs.EnsureClone(ctx); err != nil {
		t.Fatalf("EnsureClone: %v", err)
	}
	sha, err := gs.Fetch(ctx)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if err := gs.Checkout(ctx, sha); err != nil {
		t.Fatalf("Checkout: %v", err)
	}

	err = filepath.Walk(workdir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		content, readErr := os.ReadFile(path) // #nosec G304 G122 -- path comes from filepath.Walk over a t.TempDir() fixture this test created and fully controls; no untrusted concurrent writer exists in this test
		if readErr != nil {
			return readErr
		}
		if strings.Contains(string(content), token) {
			t.Errorf("token leaked into %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	if content, err := os.ReadFile(gs.gitConfigGlobal); err == nil { // #nosec G304 -- gitConfigGlobal is derived from a t.TempDir() fixture path
		if strings.Contains(string(content), token) {
			t.Errorf("token leaked into isolated git config %s", gs.gitConfigGlobal)
		}
	}
}

func TestTokenRedactedInForcedFetchError(t *testing.T) {
	tmp := t.TempDir()
	bare, work := makeRemote(t, tmp, "remote")
	commitFile(t, work, "automations.yaml", "- id: demo\n", "commit")

	workdir := filepath.Join(tmp, "clone")
	gs := New(makeOpts("file://"+bare), workdir)
	ctx := context.Background()
	if err := gs.EnsureClone(ctx); err != nil {
		t.Fatalf("EnsureClone: %v", err)
	}

	token := "ghp_TESTTOKEN123"
	// A repo_url that fails fast (loopback, connection refused) but with
	// credentials configured, so the redaction path really runs.
	opts := makeOpts("https://127.0.0.1:1/nope/nope.git")
	opts.GitUsername = "agent"
	opts.GitToken = token
	gs.Opts = opts
	gs.Timeout = 5 * time.Second

	_, err := gs.Fetch(ctx)
	if err == nil {
		t.Fatal("Fetch() error = nil, want a connection failure")
	}
	if strings.Contains(err.Error(), token) {
		t.Errorf("Fetch() error = %v, leaks token", err)
	}
}

func TestEnsureCloneDoesNotPersistTokenInGitConfig(t *testing.T) {
	tmp := t.TempDir()
	bare, work := makeRemote(t, tmp, "remote")
	commitFile(t, work, "automations.yaml", "- id: demo\n", "commit")

	workdir := filepath.Join(tmp, "clone")
	token := "ghp_TESTTOKEN123"
	opts := makeOpts("file://" + bare)
	opts.GitUsername = "agent"
	opts.GitToken = token
	gs := New(opts, workdir)
	ctx := context.Background()

	if err := gs.EnsureClone(ctx); err != nil {
		t.Fatalf("EnsureClone: %v", err)
	}
	if _, err := gs.Fetch(ctx); err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	gitConfig, err := os.ReadFile(filepath.Join(workdir, ".git", "config")) // #nosec G304 -- workdir is a t.TempDir() fixture path this test created
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(gitConfig), token) {
		t.Error("git config contains the token")
	}
	if strings.Contains(string(gitConfig), "x-access-token") {
		t.Error("git config contains the x-access-token placeholder")
	}
}
