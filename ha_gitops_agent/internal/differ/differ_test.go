package differ

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func write(t *testing.T, root, relPath string, data []byte) {
	t.Helper()
	full := filepath.Join(root, relPath)
	if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func dirs(t *testing.T) (repoRoot, configRoot string) {
	t.Helper()
	tmp := t.TempDir()
	repoRoot = filepath.Join(tmp, "repo")
	configRoot = filepath.Join(tmp, "config")
	if err := os.MkdirAll(repoRoot, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(configRoot, 0o750); err != nil {
		t.Fatal(err)
	}
	return repoRoot, configRoot
}

func TestAddDetected(t *testing.T) {
	repoRoot, configRoot := dirs(t)
	write(t, repoRoot, "new.yaml", []byte("key: value\n"))

	changes, _, _ := Compute(repoRoot, configRoot, []string{"new.yaml"}, nil, nil)

	if len(changes) != 1 {
		t.Fatalf("len(changes) = %d, want 1", len(changes))
	}
	c := changes[0]
	if c.Path != "new.yaml" || c.Kind != "add" {
		t.Errorf("change = %+v", c)
	}
	if !strings.Contains(c.DiffText, "+key: value") {
		t.Errorf("diff_text = %q, want it to contain %q", c.DiffText, "+key: value")
	}
	if !strings.Contains(c.DiffText, "config/new.yaml") || !strings.Contains(c.DiffText, "repo/new.yaml") {
		t.Errorf("diff_text = %q, want config/repo headers", c.DiffText)
	}
}

func TestUpdateDetected(t *testing.T) {
	repoRoot, configRoot := dirs(t)
	write(t, configRoot, "automations.yaml", []byte("- id: demo\n"))
	storageDir := filepath.Join(configRoot, ".storage")
	if err := os.MkdirAll(storageDir, 0o750); err != nil {
		t.Fatal(err)
	}
	write(t, configRoot, ".storage/core.entity_registry", []byte("{}"))
	write(t, repoRoot, "automations.yaml", []byte("- id: demo\n  alias: Demo\n"))

	changes, _, _ := Compute(repoRoot, configRoot, []string{"automations.yaml"}, nil, nil)

	if len(changes) != 1 {
		t.Fatalf("len(changes) = %d, want 1", len(changes))
	}
	c := changes[0]
	if c.Path != "automations.yaml" || c.Kind != "update" {
		t.Errorf("change = %+v", c)
	}
	if !strings.Contains(c.DiffText, "+  alias: Demo") {
		t.Errorf("diff_text = %q, want it to contain %q", c.DiffText, "+  alias: Demo")
	}
	if !strings.Contains(c.DiffText, "--- config/automations.yaml") {
		t.Errorf("diff_text = %q, want --- config header", c.DiffText)
	}
	if !strings.Contains(c.DiffText, "+++ repo/automations.yaml") {
		t.Errorf("diff_text = %q, want +++ repo header", c.DiffText)
	}
}

func TestNoChangeWhenIdentical(t *testing.T) {
	repoRoot, configRoot := dirs(t)
	write(t, repoRoot, "same.yaml", []byte("a: 1\n"))
	write(t, configRoot, "same.yaml", []byte("a: 1\n"))

	changes, _, _ := Compute(repoRoot, configRoot, []string{"same.yaml"}, nil, nil)

	if len(changes) != 0 {
		t.Fatalf("changes = %+v, want none", changes)
	}
}

func TestDeleteOnlyForPrevManifest(t *testing.T) {
	repoRoot, configRoot := dirs(t)
	// Previously applied, no longer tracked, still present -> delete.
	write(t, configRoot, "gone.yaml", []byte("old: true\n"))
	// In config but never applied by us and not tracked -> never deleted.
	write(t, configRoot, "untouched_by_us.yaml", []byte("user_owned: true\n"))

	changes, _, _ := Compute(repoRoot, configRoot, nil, []string{"gone.yaml"}, nil)

	if len(changes) != 1 {
		t.Fatalf("len(changes) = %d, want 1", len(changes))
	}
	if changes[0].Path != "gone.yaml" || changes[0].Kind != "delete" {
		t.Errorf("change = %+v", changes[0])
	}
}

func TestDeleteSkippedIfAlreadyGone(t *testing.T) {
	repoRoot, configRoot := dirs(t)

	changes, _, _ := Compute(repoRoot, configRoot, nil, []string{"already_removed.yaml"}, nil)

	if len(changes) != 0 {
		t.Fatalf("changes = %+v, want none", changes)
	}
}

func TestExcludedPathsNeverProduced(t *testing.T) {
	repoRoot, configRoot := dirs(t)

	// Would be an "add" if not excluded.
	write(t, repoRoot, ".storage/core.json", []byte("{}"))
	// Would be an "update" if not excluded.
	write(t, repoRoot, "notes.db", []byte("repo-version"))
	write(t, configRoot, "notes.db", []byte("config-version"))
	// Would be a "delete" if not excluded.
	write(t, configRoot, ".storage/old.json", []byte("{}"))

	changes, _, _ := Compute(
		repoRoot, configRoot,
		[]string{".storage/core.json", "notes.db"},
		[]string{".storage/old.json"}, nil,
	)

	if len(changes) != 0 {
		t.Fatalf("changes = %+v, want none", changes)
	}
}

func TestBinarySummary(t *testing.T) {
	repoRoot, configRoot := dirs(t)
	write(t, configRoot, "blob.bin", append([]byte("old"), append([]byte{0}, []byte("data")...)...))
	write(t, repoRoot, "blob.bin", append([]byte("new"), append([]byte{0}, []byte("data-longer")...)...))

	changes, _, _ := Compute(repoRoot, configRoot, []string{"blob.bin"}, nil, nil)

	if len(changes) != 1 {
		t.Fatalf("len(changes) = %d, want 1", len(changes))
	}
	c := changes[0]
	if c.Kind != "update" {
		t.Errorf("kind = %q, want update", c.Kind)
	}
	if !strings.HasPrefix(c.DiffText, "binary file changed,") {
		t.Errorf("diff_text = %q, want binary summary prefix", c.DiffText)
	}
	if !strings.Contains(c.DiffText, "->") {
		t.Errorf("diff_text = %q, want an arrow", c.DiffText)
	}
}

func TestTruncationMarkerOnHugeDiff(t *testing.T) {
	repoRoot, configRoot := dirs(t)

	var before, after strings.Builder
	for i := 0; i < 300; i++ {
		before.WriteString("before line ")
		before.WriteString(strconv.Itoa(i))
		before.WriteByte('\n')
		after.WriteString("after line ")
		after.WriteString(strconv.Itoa(i))
		after.WriteByte('\n')
	}
	write(t, configRoot, "huge.yaml", []byte(before.String()))
	write(t, repoRoot, "huge.yaml", []byte(after.String()))

	changes, _, _ := Compute(repoRoot, configRoot, []string{"huge.yaml"}, nil, nil)

	if len(changes) != 1 {
		t.Fatalf("len(changes) = %d, want 1", len(changes))
	}
	diffText := changes[0].DiffText
	if !strings.HasSuffix(diffText, "... diff truncated\n") {
		t.Errorf("diff_text does not end with truncation marker: %q", tail(diffText, 40))
	}
	if got := len(strings.Split(diffText, "\n")); got > 404 {
		// 400 diff lines + blank + marker line, plus one from the final split.
		t.Errorf("diff has %d lines, want <= 404", got)
	}
}

func TestDeterministicOrdering(t *testing.T) {
	repoRoot, configRoot := dirs(t)

	write(t, repoRoot, "z_add.yaml", []byte("new: true\n"))
	write(t, configRoot, "a_update.yaml", []byte("v: 1\n"))
	write(t, repoRoot, "a_update.yaml", []byte("v: 2\n"))
	write(t, configRoot, "m_delete.yaml", []byte("leftover: true\n"))

	changes, _, _ := Compute(
		repoRoot, configRoot,
		[]string{"z_add.yaml", "a_update.yaml"},
		[]string{"m_delete.yaml"}, nil,
	)

	want := []struct{ kind, path string }{
		{"update", "a_update.yaml"},
		{"add", "z_add.yaml"},
		{"delete", "m_delete.yaml"},
	}
	if len(changes) != len(want) {
		t.Fatalf("changes = %+v, want %d entries", changes, len(want))
	}
	for i, w := range want {
		if changes[i].Kind != w.kind || changes[i].Path != w.path {
			t.Errorf("changes[%d] = {%s, %s}, want {%s, %s}", i, changes[i].Kind, changes[i].Path, w.kind, w.path)
		}
	}
}

func TestMissingRepoFileDoesNotCrash(t *testing.T) {
	repoRoot, configRoot := dirs(t)
	// "ghost.yaml" is claimed tracked but was never actually written.

	changes, _, _ := Compute(repoRoot, configRoot, []string{"ghost.yaml"}, nil, nil)

	if len(changes) != 0 {
		t.Fatalf("changes = %+v, want none", changes)
	}
}

func TestUnreadableConfigFileDoesNotCrash(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("permission bits are meaningless as root/on Windows")
	}
	repoRoot, configRoot := dirs(t)
	write(t, repoRoot, "locked.yaml", []byte("v: 1\n"))
	locked := filepath.Join(configRoot, "locked.yaml")
	if err := os.WriteFile(locked, []byte("v: 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(locked, 0o600) }()

	changes, _, _ := Compute(repoRoot, configRoot, []string{"locked.yaml"}, nil, nil)

	if len(changes) != 0 {
		t.Fatalf("changes = %+v, want none", changes)
	}
}

// A tracked path that is a symlink outside the repo (git checks one out
// happily) must never be read: its target's plaintext would otherwise
// reach DiffText, the dashboard and /status.json.
func TestTrackedSymlinkNeverRead(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks behave differently on windows")
	}
	repoRoot, configRoot := dirs(t)
	outside := filepath.Join(t.TempDir(), "options.json")
	if err := os.WriteFile(outside, []byte(`{"git_token":"ghp_SUPERSECRETTOKENVALUE"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(repoRoot, "automations.yaml")); err != nil {
		t.Fatal(err)
	}

	changes, skipped, _ := Compute(repoRoot, configRoot, []string{"automations.yaml"}, nil, nil)

	if len(changes) != 0 {
		t.Fatalf("changes = %+v, want none (tracked symlink must be refused, never diffed)", changes)
	}
	if len(skipped) != 1 || skipped[0] != "automations.yaml" {
		t.Errorf("skippedContainment = %+v, want [\"automations.yaml\"]", skipped)
	}
}

// The same for the config side: configRoot's copy being a symlink must not
// be read into a diff either.
func TestConfigSideSymlinkNeverRead(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks behave differently on windows")
	}
	repoRoot, configRoot := dirs(t)
	write(t, repoRoot, "automations.yaml", []byte("- id: demo\n"))
	outside := filepath.Join(t.TempDir(), "options.json")
	if err := os.WriteFile(outside, []byte(`{"git_token":"ghp_SUPERSECRETTOKENVALUE"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(configRoot, "automations.yaml")); err != nil {
		t.Fatal(err)
	}

	changes, skipped, _ := Compute(repoRoot, configRoot, []string{"automations.yaml"}, nil, nil)

	if len(changes) != 0 {
		t.Fatalf("changes = %+v, want none (config-side symlink must be refused, never diffed)", changes)
	}
	if len(skipped) != 1 || skipped[0] != "automations.yaml" {
		t.Errorf("skippedContainment = %+v, want [\"automations.yaml\"]", skipped)
	}
}

// A manifest entry like "sub/../gitops/registries.yaml" must not evade the
// delete branch's gitsync.Excluded check and reach the real file.
func TestExcludedDotDotPathNeverProducesDelete(t *testing.T) {
	repoRoot, configRoot := dirs(t)
	write(t, configRoot, "gitops/registries.yaml", []byte("floors: []\n"))

	changes, _, _ := Compute(repoRoot, configRoot, nil, []string{"sub/../gitops/registries.yaml"}, nil)

	if len(changes) != 0 {
		t.Fatalf("changes = %+v, want none (dotdot path into gitops/ must stay excluded)", changes)
	}
}

// The symlinked-PARENT variant: the leaf is an ordinary file, so Lstat
// sees nothing wrong and only the realpath check catches the escape.
func TestTrackedSymlinkedParentDirNeverRead(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks behave differently on windows")
	}
	repoRoot, configRoot := dirs(t)
	outsideDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(outsideDir, "automations.yaml"), []byte(`{"git_token":"ghp_SUPERSECRETTOKENVALUE"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideDir, filepath.Join(repoRoot, "sub")); err != nil {
		t.Fatal(err)
	}

	changes, skipped, _ := Compute(repoRoot, configRoot, []string{"sub/automations.yaml"}, nil, nil)

	for _, c := range changes {
		if strings.Contains(c.DiffText, "SUPERSECRETTOKENVALUE") {
			t.Fatalf("DiffText leaked outside content via a symlinked parent directory: %+v", c)
		}
	}
	if len(changes) != 0 {
		t.Fatalf("changes = %+v, want none (symlinked parent directory must be refused, never diffed)", changes)
	}
	if len(skipped) != 1 || skipped[0] != "sub/automations.yaml" {
		t.Errorf("skippedContainment = %+v, want [\"sub/automations.yaml\"]", skipped)
	}
}

// The same on the config side, which is the realistic half: a symlinked
// subdirectory under /homeassistant is ordinary practice.
func TestConfigSideSymlinkedParentDirNeverRead(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks behave differently on windows")
	}
	repoRoot, configRoot := dirs(t)
	write(t, repoRoot, "sub/automations.yaml", []byte("- id: demo\n"))
	outsideDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(outsideDir, "automations.yaml"), []byte(`{"git_token":"ghp_SUPERSECRETTOKENVALUE"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideDir, filepath.Join(configRoot, "sub")); err != nil {
		t.Fatal(err)
	}

	changes, skipped, _ := Compute(repoRoot, configRoot, []string{"sub/automations.yaml"}, nil, nil)

	for _, c := range changes {
		if strings.Contains(c.DiffText, "SUPERSECRETTOKENVALUE") {
			t.Fatalf("DiffText leaked outside content via a symlinked parent directory: %+v", c)
		}
	}
	if len(changes) != 0 {
		t.Fatalf("changes = %+v, want none (symlinked parent directory must be refused, never diffed)", changes)
	}
	if len(skipped) != 1 || skipped[0] != "sub/automations.yaml" {
		t.Errorf("skippedContainment = %+v, want [\"sub/automations.yaml\"]", skipped)
	}
}

// A tracked path that simply does not exist is routine churn, not
// evidence: keeping it out of skippedContainment stops the event line
// crying wolf.
func TestOrdinaryMissingFileNeverAppearsInSkippedContainment(t *testing.T) {
	repoRoot, configRoot := dirs(t)

	_, skipped, _ := Compute(repoRoot, configRoot, []string{"ghost.yaml"}, nil, nil)

	if len(skipped) != 0 {
		t.Errorf("skippedContainment = %+v, want none for an ordinary missing file", skipped)
	}
}

// --- large files: size gate before any full read ------------------------

// forbidFullRead replaces readFile with one that fails the test if called,
// proving Compute never took the full-read path.
func forbidFullRead(t *testing.T) {
	t.Helper()
	orig := readFile
	readFile = func(path string) []byte {
		t.Fatalf("full read attempted on large file: %s", path)
		return nil
	}
	t.Cleanup(func() { readFile = orig })
}

func withLargeFileThreshold(t *testing.T, n int64) {
	t.Helper()
	orig := largeFileThresholdBytes
	largeFileThresholdBytes = n
	t.Cleanup(func() { largeFileThresholdBytes = orig })
}

func TestLargeFileUpdateReportsSummaryWithoutFullRead(t *testing.T) {
	withLargeFileThreshold(t, 16)
	forbidFullRead(t)

	repoRoot, configRoot := dirs(t)
	write(t, configRoot, "big.dat", bytesOf('a', 40))
	write(t, repoRoot, "big.dat", bytesOf('b', 55))

	changes, _, _ := Compute(repoRoot, configRoot, []string{"big.dat"}, nil, nil)

	if len(changes) != 1 {
		t.Fatalf("len(changes) = %d, want 1", len(changes))
	}
	c := changes[0]
	if c.Path != "big.dat" || c.Kind != "update" {
		t.Errorf("change = %+v", c)
	}
	if c.DiffText != "large file changed, 40 -> 55 bytes" {
		t.Errorf("diff_text = %q", c.DiffText)
	}
}

func TestLargeFileIdenticalProducesNoChangeWithoutFullRead(t *testing.T) {
	withLargeFileThreshold(t, 16)
	forbidFullRead(t)

	repoRoot, configRoot := dirs(t)
	data := bytesOf('x', 50)
	write(t, repoRoot, "same_big.dat", data)
	write(t, configRoot, "same_big.dat", data)

	changes, _, _ := Compute(repoRoot, configRoot, []string{"same_big.dat"}, nil, nil)

	if len(changes) != 0 {
		t.Fatalf("changes = %+v, want none", changes)
	}
}

func TestLargeFileAddReportsSummaryWithoutFullRead(t *testing.T) {
	withLargeFileThreshold(t, 16)
	forbidFullRead(t)

	repoRoot, configRoot := dirs(t)
	write(t, repoRoot, "new_big.dat", bytesOf('z', 30))

	changes, _, _ := Compute(repoRoot, configRoot, []string{"new_big.dat"}, nil, nil)

	if len(changes) != 1 {
		t.Fatalf("len(changes) = %d, want 1", len(changes))
	}
	c := changes[0]
	if c.Kind != "add" {
		t.Errorf("kind = %q, want add", c.Kind)
	}
	if c.DiffText != "large file changed, 0 -> 30 bytes" {
		t.Errorf("diff_text = %q", c.DiffText)
	}
}

func TestLargeFileDeleteReportsSummaryWithoutFullRead(t *testing.T) {
	withLargeFileThreshold(t, 16)
	forbidFullRead(t)

	repoRoot, configRoot := dirs(t)
	write(t, configRoot, "old_big.dat", bytesOf('y', 30))

	changes, _, _ := Compute(repoRoot, configRoot, nil, []string{"old_big.dat"}, nil)

	if len(changes) != 1 {
		t.Fatalf("len(changes) = %d, want 1", len(changes))
	}
	c := changes[0]
	if c.Kind != "delete" {
		t.Errorf("kind = %q, want delete", c.Kind)
	}
	if c.DiffText != "large file changed, 30 -> 0 bytes" {
		t.Errorf("diff_text = %q", c.DiffText)
	}
}

func TestSmallFileBelowThresholdStillUsesFullDiffPath(t *testing.T) {
	withLargeFileThreshold(t, 4*1024*1024)

	repoRoot, configRoot := dirs(t)
	write(t, configRoot, "small.yaml", []byte("a: 1\n"))
	write(t, repoRoot, "small.yaml", []byte("a: 2\n"))

	changes, _, _ := Compute(repoRoot, configRoot, []string{"small.yaml"}, nil, nil)

	if len(changes) != 1 {
		t.Fatalf("len(changes) = %d, want 1", len(changes))
	}
	c := changes[0]
	if c.Kind != "update" {
		t.Errorf("kind = %q, want update", c.Kind)
	}
	if strings.Contains(c.DiffText, "large file changed") {
		t.Errorf("diff_text = %q, want a real diff not a large-file summary", c.DiffText)
	}
	if !strings.Contains(c.DiffText, "-a: 1") || !strings.Contains(c.DiffText, "+a: 2") {
		t.Errorf("diff_text = %q, want -a: 1 / +a: 2", c.DiffText)
	}
}

func bytesOf(b byte, n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = b
	}
	return out
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
