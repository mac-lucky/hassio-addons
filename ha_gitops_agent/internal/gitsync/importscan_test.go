package gitsync

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeLive creates root/rel with size bytes of filler, making parents.
func writeLive(t *testing.T, root, rel string, size int) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
		t.Fatalf("MkdirAll %s: %v", full, err)
	}
	if err := os.WriteFile(full, []byte(strings.Repeat("x", size)), 0o600); err != nil {
		t.Fatalf("WriteFile %s: %v", full, err)
	}
}

// generousLimits are big enough never to be the thing under test.
func generousLimits() ImportLimits {
	return ImportLimits{MaxFiles: 1000, MaxTotalBytes: 1 << 30, MaxFileBytes: 1 << 30, MaxEntries: 100000}
}

func TestScanLiveCapturesOrdinaryFiles(t *testing.T) {
	root := t.TempDir()
	writeLive(t, root, "configuration.yaml", 10)
	writeLive(t, root, "packages/lights.yaml", 20)
	writeLive(t, root, "custom_components/foo/manifest.json", 30)

	plan, err := ScanLive(root, generousLimits())
	if err != nil {
		t.Fatalf("ScanLive: %v", err)
	}
	want := []string{"configuration.yaml", "custom_components/foo/manifest.json", "packages/lights.yaml"}
	if len(plan.Files) != len(want) {
		t.Fatalf("Files = %v, want %v", plan.Files, want)
	}
	for i, w := range want {
		if plan.Files[i] != w {
			t.Errorf("Files[%d] = %q, want %q (output must be sorted and slash-separated)", i, plan.Files[i], w)
		}
	}
	if plan.TotalBytes != 60 {
		t.Errorf("TotalBytes = %d, want 60", plan.TotalBytes)
	}
}

// Pruning, not per-file filtering: fs.SkipDir costs one skip for the whole
// tree. .storage/ and backups/ are the bulk of a real install.
func TestScanLivePrunesExcludedDirectoriesWithoutDescending(t *testing.T) {
	root := t.TempDir()
	writeLive(t, root, "configuration.yaml", 5)
	writeLive(t, root, ".storage/a/b/core.entity_registry", 5)
	writeLive(t, root, ".storage/a/b/core.device_registry", 5)
	writeLive(t, root, ".storage/onefile", 5)

	plan, err := ScanLive(root, generousLimits())
	if err != nil {
		t.Fatalf("ScanLive: %v", err)
	}
	if len(plan.Files) != 1 || plan.Files[0] != "configuration.yaml" {
		t.Fatalf("Files = %v, want just configuration.yaml", plan.Files)
	}
	if plan.SkippedExcluded != 1 {
		t.Errorf("SkippedExcluded = %d, want 1 (the pruned directory counted once, not once per file under it)", plan.SkippedExcluded)
	}
}

func TestScanLiveSkipsExcludedAndSecretShapedFiles(t *testing.T) {
	root := t.TempDir()
	writeLive(t, root, "configuration.yaml", 5)
	writeLive(t, root, "home-assistant_v2.db", 5)
	// Real install: a suffix after .db matched neither "*.db" nor "*.db-*".
	writeLive(t, root, "zigbee2mqtt/database.db.backup", 5)
	writeLive(t, root, "home-assistant_v2.db-wal", 5)
	writeLive(t, root, "home-assistant.log", 5)
	writeLive(t, root, "secrets.yaml", 5)
	writeLive(t, root, "certs/fullchain.pem", 5)
	writeLive(t, root, "sub/id_ed25519", 5)
	writeLive(t, root, ".env", 5)
	writeLive(t, root, "gitops/registries.yaml", 5)

	plan, err := ScanLive(root, generousLimits())
	if err != nil {
		t.Fatalf("ScanLive: %v", err)
	}
	if len(plan.Files) != 1 || plan.Files[0] != "configuration.yaml" {
		t.Fatalf("Files = %v, want just configuration.yaml", plan.Files)
	}
	if plan.SkippedSecret == 0 {
		t.Error("SkippedSecret = 0, want the secret-shaped files counted separately from the excluded ones")
	}
	if plan.SkippedExcluded == 0 {
		t.Error("SkippedExcluded = 0, want the database, log and gitops/ entries counted")
	}
}

// Following links would capture a file under its innocuous name carrying
// the target's bytes.
func TestScanLiveSkipsSymlinks(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	writeLive(t, outside, "secret.txt", 5)
	writeLive(t, root, "configuration.yaml", 5)

	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(root, "notes.yaml")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "linkdir")); err != nil {
		t.Fatalf("Symlink dir: %v", err)
	}

	plan, err := ScanLive(root, generousLimits())
	if err != nil {
		t.Fatalf("ScanLive: %v", err)
	}
	for _, f := range plan.Files {
		if f == "notes.yaml" || strings.HasPrefix(f, "linkdir/") {
			t.Fatalf("Files = %v, want no symlinked entries", plan.Files)
		}
	}
	if plan.SkippedNonRegular < 2 {
		t.Errorf("SkippedNonRegular = %d, want at least 2 (the linked file and the linked directory)", plan.SkippedNonRegular)
	}
}

// Importing the rest would leave a repository that looks like a complete
// snapshot and is not.
func TestScanLivePerFileCapFailsRatherThanSkips(t *testing.T) {
	root := t.TempDir()
	writeLive(t, root, "configuration.yaml", 5)
	writeLive(t, root, "www/huge.bin", 4096)

	limits := generousLimits()
	limits.MaxFileBytes = 1024

	plan, err := ScanLive(root, limits)
	var tooLarge *ImportTooLargeError
	if !errors.As(err, &tooLarge) {
		t.Fatalf("ScanLive error = %v, want *ImportTooLargeError", err)
	}
	if tooLarge.Reason != "single file" {
		t.Errorf("Reason = %q, want %q", tooLarge.Reason, "single file")
	}
	if len(plan.Files) != 0 {
		t.Errorf("Files = %v, want the zero plan on a breach (never a partial import)", plan.Files)
	}
	if !strings.Contains(tooLarge.Error(), "www/huge.bin") {
		t.Errorf("error %q does not name the offending file", tooLarge.Error())
	}
	if !strings.Contains(tooLarge.Error(), "move it out of the config directory") {
		t.Errorf("error %q does not tell the user how to fix it", tooLarge.Error())
	}
	// Not .gitignore: these limits are measured before git is involved.
	if strings.Contains(tooLarge.Error(), "add it to the repository's .gitignore") {
		t.Errorf("error %q advises .gitignore, which cannot fix a size breach", tooLarge.Error())
	}
}

// Naming ten files when thousands tripped a count limit says nothing
// actionable.
func TestScanLiveFileCountCapNamesDirectories(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 12; i++ {
		writeLive(t, root, "www/tiles/t"+string(rune('a'+i))+".png", 4)
	}
	writeLive(t, root, "configuration.yaml", 4)

	limits := generousLimits()
	limits.MaxFiles = 5

	_, err := ScanLive(root, limits)
	var tooLarge *ImportTooLargeError
	if !errors.As(err, &tooLarge) {
		t.Fatalf("ScanLive error = %v, want *ImportTooLargeError", err)
	}
	if tooLarge.Reason != "file count" {
		t.Fatalf("Reason = %q, want %q", tooLarge.Reason, "file count")
	}
	if len(tooLarge.Offenders) == 0 || tooLarge.Offenders[0].Path != "www/" {
		t.Fatalf("Offenders = %+v, want www/ named first as a directory", tooLarge.Offenders)
	}
	if tooLarge.Offenders[0].Files != 12 {
		t.Errorf("Offenders[0].Files = %d, want 12", tooLarge.Offenders[0].Files)
	}
}

func TestScanLiveTotalBytesCapNamesLargestFirst(t *testing.T) {
	root := t.TempDir()
	writeLive(t, root, "media/big.bin", 900)
	writeLive(t, root, "www/small.bin", 100)

	limits := generousLimits()
	limits.MaxTotalBytes = 500

	_, err := ScanLive(root, limits)
	var tooLarge *ImportTooLargeError
	if !errors.As(err, &tooLarge) {
		t.Fatalf("ScanLive error = %v, want *ImportTooLargeError", err)
	}
	if tooLarge.Reason != "total size" {
		t.Fatalf("Reason = %q, want %q", tooLarge.Reason, "total size")
	}
	if len(tooLarge.Offenders) == 0 || tooLarge.Offenders[0].Path != "media/" {
		t.Fatalf("Offenders = %+v, want media/ first (largest by bytes)", tooLarge.Offenders)
	}
}

func TestScanLiveEntryCapAborts(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 20; i++ {
		writeLive(t, root, "d/f"+string(rune('a'+i))+".yaml", 1)
	}

	limits := generousLimits()
	limits.MaxEntries = 5

	_, err := ScanLive(root, limits)
	var tooLarge *ImportTooLargeError
	if !errors.As(err, &tooLarge) {
		t.Fatalf("ScanLive error = %v, want *ImportTooLargeError", err)
	}
	if tooLarge.Reason != "entry count" {
		t.Errorf("Reason = %q, want %q", tooLarge.Reason, "entry count")
	}
}

func TestScanLiveEmptyTreeIsNotAnError(t *testing.T) {
	plan, err := ScanLive(t.TempDir(), generousLimits())
	if err != nil {
		t.Fatalf("ScanLive: %v", err)
	}
	if len(plan.Files) != 0 {
		t.Errorf("Files = %v, want empty", plan.Files)
	}
}

// A typo'd or unmounted root must not read as a config whose every file is
// excluded; the UI renders the two very differently.
func TestScanLiveMissingConfigRootIsNotSilentlyEmpty(t *testing.T) {
	_, err := ScanLive(filepath.Join(t.TempDir(), "does-not-exist"), generousLimits())
	if err == nil {
		t.Fatal("ScanLive on a missing root returned no error")
	}
	if !strings.Contains(err.Error(), "does-not-exist") {
		t.Errorf("error %q does not name the root it could not read", err)
	}
}

func TestScanLiveConfigRootThatIsAFileIsRefused(t *testing.T) {
	root := t.TempDir()
	writeLive(t, root, "notadir", 4)

	if _, err := ScanLive(filepath.Join(root, "notadir"), generousLimits()); err == nil {
		t.Fatal("ScanLive on a file-as-root returned no error")
	}
}

// One unreadable directory must not cost the whole import, but it is
// counted: the result is a partial snapshot.
func TestScanLivePermissionDeniedSubtreeSkipsOnlyThatSubtree(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission bits do not apply")
	}
	root := t.TempDir()
	writeLive(t, root, "configuration.yaml", 4)
	writeLive(t, root, "locked/inside.yaml", 4)
	locked := filepath.Join(root, "locked")
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Skipf("chmod unavailable: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o750) })

	plan, err := ScanLive(root, generousLimits())
	if err != nil {
		t.Fatalf("ScanLive: %v", err)
	}
	if len(plan.Files) != 1 || plan.Files[0] != "configuration.yaml" {
		t.Fatalf("Files = %v, want just the readable file", plan.Files)
	}
	if plan.SkippedUnreadable == 0 {
		t.Error("SkippedUnreadable = 0, want the locked subtree counted - the snapshot is partial and nothing else would say so")
	}
}

// Every other test passes generousLimits(), so a 10 << 10 typo here would
// leave the suite green and break every real import.
func TestDefaultImportLimitsAreTheDocumentedValues(t *testing.T) {
	l := DefaultImportLimits()
	if l.MaxFiles != 25000 {
		t.Errorf("MaxFiles = %d, want 25000 (DOCS.md states this)", l.MaxFiles)
	}
	if l.MaxTotalBytes != 400<<20 {
		t.Errorf("MaxTotalBytes = %d, want 400 MiB", l.MaxTotalBytes)
	}
	if l.MaxFileBytes != 25<<20 {
		t.Errorf("MaxFileBytes = %d, want 25 MiB", l.MaxFileBytes)
	}
	// Calibrated against a real HACS install (6789 files, ~135 MB); below
	// that an ordinary config gets refused.
	if l.MaxFiles < 10000 || l.MaxTotalBytes < 200<<20 {
		t.Error("limits are below what a real HACS install needs; they would refuse an ordinary config")
	}
	if l.MaxEntries != 200000 {
		t.Errorf("MaxEntries = %d, want 200000", l.MaxEntries)
	}
}

// Renders every breach shape; the count-based ones are otherwise only
// checked structurally.
func TestImportTooLargeMessagesNameTheirUnits(t *testing.T) {
	cases := []struct {
		err  *ImportTooLargeError
		want []string
	}{
		{
			&ImportTooLargeError{
				Reason: "file count", Limit: 5000, Actual: 8123,
				Offenders: []ImportOffender{{Path: "www/", Files: 8000, Bytes: 1 << 20}},
			},
			[]string{"8123 files exceeds the 5000 limit", "largest directories: www/ (8000 files, 1.0 MB)"},
		},
		{
			&ImportTooLargeError{Reason: "entry count", Limit: 200000, Actual: 200001},
			[]string{"200001 directory entries exceeds the 200000 limit", "mount point"},
		},
		{
			&ImportTooLargeError{
				Reason: "total size", Limit: 100 << 20, Actual: 412 << 20,
				Offenders: []ImportOffender{{Path: "media/", Files: 3, Bytes: 400 << 20}},
			},
			[]string{"total size 412.0 MB exceeds the 100.0 MB limit"},
		},
	}
	for _, c := range cases {
		got := c.err.Error()
		for _, want := range c.want {
			if !strings.Contains(got, want) {
				t.Errorf("%s message = %q, want it to contain %q", c.err.Reason, got, want)
			}
		}
	}
}
