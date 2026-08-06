package gitsync

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// checkedOutGitSync is importGitSync plus the checkout Import does for
// itself; EnsureClone uses --no-checkout, so the worktree is empty until then.
func checkedOutGitSync(t *testing.T, bare, workdir string) *GitSync {
	t.Helper()
	gs := importGitSync(t, bare, workdir)
	sha, err := gs.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if err := gs.Checkout(context.Background(), sha); err != nil {
		t.Fatalf("Checkout: %v", err)
	}
	return gs
}

// Machine-written paths, every one of them found tracked in a real
// imported config.
func TestExcludedCoversMachineWrittenArtifacts(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		// Python bytecode: rewritten on every interpreter reload.
		{"custom_components/foo/__pycache__/sensor.cpython-313.pyc", true},
		{"__pycache__", true},
		{"pyscript/helpers.pyc", true},
		{"pyscript/helpers.pyo", true},
		{"pyscript/helpers.py", false},
		// Instance identity and run state.
		{".HA_VERSION", true},
		{".uuid", true},
		{".ha_run.lock", true},
		{".cache/pip/wheel", true},
		// Written by Home Assistant, not by the user.
		{"ip_bans.yaml", true},
		{"known_devices.yaml", true},
		{"known_devices.yaml.bak", true},
		// ESPHome build cache and Device Builder state. The peer link key is
		// binary key material, which values-only encryption cannot cover.
		{"esphome/.esphome/build/x.o", true},
		{"esphome/.device-builder-peer-link-key.bin", true},
		{"esphome/.device-builder.json", true},
		{"esphome/.device-builder.json.lock", true},
		// ...but an ordinary device config is what the agent manages.
		{"esphome/plant-care.yaml", false},
	}
	for _, c := range cases {
		if got := Excluded(c.path); got != c.want {
			t.Errorf("Excluded(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}

// /config/image is HA's uploaded-image store, but "image" is an ordinary
// word: excluding it at any depth would stop a user's www/image/ syncing.
func TestExcludedImageIsRootAnchored(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"image", true},
		{"image/67c86524cfecc456610966229a5d8016/original", true},
		{"www/image/logo.png", false},
		{"packages/image/lights.yaml", false},
	}
	for _, c := range cases {
		if got := Excluded(c.path); got != c.want {
			t.Errorf("Excluded(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}

func TestUnquoteGitPath(t *testing.T) {
	cases := []struct{ in, want string }{
		{"packages/lights.yaml", "packages/lights.yaml"},
		// git prints the escaped form; want uses a Go escape to stay ASCII.
		{`"caf\303\251/secrets.yaml"`, "caf\u00e9/secrets.yaml"},
		{`"with\nnewline.yaml"`, "with\nnewline.yaml"},
		{`"with\"quote.yaml"`, `with"quote.yaml`},
		// Unparseable is returned as-is: the worst case is one extra file
		// in the commit, not a silently excluded one.
		{`"unterminated`, `"unterminated`},
	}
	for _, c := range cases {
		if got := unquoteGitPath(c.in); got != c.want {
			t.Errorf("unquoteGitPath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// An empty branch is the one import with no .gitignore to honor, so
// without the seed it captures every HACS-installed file in the config.
func TestImportSeedsGitignoreOnFirstImport(t *testing.T) {
	tmp := t.TempDir()
	bare := filepath.Join(tmp, "empty.git")
	runGitHelper(t, tmp, "init", "--bare", "-b", "main", bare)

	configRoot := filepath.Join(tmp, "config")
	writeLive(t, configRoot, "configuration.yaml", 10)
	writeLive(t, configRoot, "custom_components/pushward/sensor.py", 10)
	writeLive(t, configRoot, "custom_components/pushward/translations/pl.json", 10)
	writeLive(t, configRoot, "www/community/button-card/button-card.js", 10)
	writeLive(t, configRoot, "zigbee2mqtt/coordinator_backup.json", 10)

	gs := importGitSync(t, bare, filepath.Join(tmp, "workdir"))
	res, err := gs.Import(context.Background(), configRoot, generousLimits(), fixedImportTime)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}

	if got, ok := showAtRef(t, bare, "main", GitignoreFile); !ok || got != DefaultGitignore {
		t.Errorf("main:%s = %q (ok=%v), want the seeded default", GitignoreFile, got, ok)
	}
	if _, ok := showAtRef(t, bare, "main", "configuration.yaml"); !ok {
		t.Error("configuration.yaml missing: the seed must not shut out ordinary config")
	}
	for _, p := range []string{
		"custom_components/pushward/sensor.py",
		"custom_components/pushward/translations/pl.json",
		"www/community/button-card/button-card.js",
		"zigbee2mqtt/coordinator_backup.json",
	} {
		if _, ok := showAtRef(t, bare, "main", p); ok {
			t.Errorf("%s was imported, want it left to whatever installed it", p)
		}
	}
	// The tally counts what was committed, not what was scanned.
	if res.Files != 1 {
		t.Errorf("Files = %d, want 1 (configuration.yaml alone)", res.Files)
	}
}

// The seed is a starting point the user edits. Unlike ensureSopsConfig,
// regenerating it would re-ignore what they un-ignored on purpose.
func TestEnsureGitignoreNeverOverwrites(t *testing.T) {
	tmp := t.TempDir()
	bare, work := makeRemote(t, tmp, "repo")
	const mine = "# mine\ncustom_components/foo/*.log\n"
	commitFile(t, work, GitignoreFile, mine, "add gitignore")

	gs := checkedOutGitSync(t, bare, filepath.Join(tmp, "workdir"))
	written, err := gs.ensureGitignore()
	if err != nil {
		t.Fatalf("ensureGitignore: %v", err)
	}
	if written {
		t.Error("ensureGitignore reported a write over an existing .gitignore")
	}
	got, err := os.ReadFile(filepath.Join(gs.Workdir, GitignoreFile))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != mine {
		t.Errorf("%s = %q, want the user's file untouched", GitignoreFile, string(got))
	}
}

// .gitignore does not apply to already-tracked files and "git add" honors
// that, so the filter must agree or a managed file stops being managed.
func TestFilterIgnoredKeepsTrackedPaths(t *testing.T) {
	tmp := t.TempDir()
	bare, work := makeRemote(t, tmp, "repo")
	// Tracked first: "git add" refuses a path .gitignore already covers.
	commitFile(t, work, "custom_components/mine/sensor.py", "tracked\n", "track one anyway")
	commitFile(t, work, GitignoreFile, "custom_components/\n", "add gitignore")

	gs := checkedOutGitSync(t, bare, filepath.Join(tmp, "workdir"))
	in := []string{
		"configuration.yaml",
		"custom_components/mine/sensor.py",   // tracked: stays
		"custom_components/hacs/__init__.py", // untracked and ignored: dropped
	}
	got, err := gs.filterIgnored(context.Background(), "", in)
	if err != nil {
		t.Fatalf("filterIgnored: %v", err)
	}
	want := []string{"configuration.yaml", "custom_components/mine/sensor.py"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("filterIgnored = %v, want %v", got, want)
	}
}

// Batching keeps thousands of paths out of one argv; crossing a boundary
// must not change the answer. The count tracks the budget, so retuning holds.
func TestFilterIgnoredBatches(t *testing.T) {
	tmp := t.TempDir()
	bare, work := makeRemote(t, tmp, "repo")
	commitFile(t, work, GitignoreFile, "*.pyc\n", "add gitignore")

	gs := checkedOutGitSync(t, bare, filepath.Join(tmp, "workdir"))
	var in []string
	wantKept := 0
	// ~14 bytes per generated path, so this is a little over two budgets.
	for i := range checkIgnoreArgvBudget/7 + 7 {
		if i%3 == 0 {
			in = append(in, filepath.ToSlash(filepath.Join("pkg", "f"+strconv.Itoa(i)+".pyc")))
			continue
		}
		in = append(in, filepath.ToSlash(filepath.Join("pkg", "f"+strconv.Itoa(i)+".yaml")))
		wantKept++
	}
	got, err := gs.filterIgnored(context.Background(), "", in)
	if err != nil {
		t.Fatalf("filterIgnored: %v", err)
	}
	if len(got) != wantKept {
		t.Errorf("filterIgnored kept %d, want %d", len(got), wantKept)
	}
	for _, p := range got {
		if strings.HasSuffix(p, ".pyc") {
			t.Errorf("filterIgnored kept %q", p)
		}
	}
}

// ESPHome ships a .gitignore under /config/esphome for its build output.
// Unless the live ones are copied in first, an empty branch tracks it all.
func TestImportHonorsNestedLiveGitignoreOnFirstImport(t *testing.T) {
	tmp := t.TempDir()
	bare := filepath.Join(tmp, "empty.git")
	runGitHelper(t, tmp, "init", "--bare", "-b", "main", bare)

	configRoot := filepath.Join(tmp, "config")
	writeLive(t, configRoot, "configuration.yaml", 10)
	writeLiveText(t, configRoot, "esphome/.gitignore", "**/src/\n**/platformio.ini\n")
	writeLive(t, configRoot, "esphome/plant-care.yaml", 10)
	writeLive(t, configRoot, "esphome/plant-care/src/main.cpp", 10)
	writeLive(t, configRoot, "esphome/plant-care/platformio.ini", 10)

	gs := importGitSync(t, bare, filepath.Join(tmp, "workdir"))
	if _, err := gs.Import(context.Background(), configRoot, generousLimits(), fixedImportTime); err != nil {
		t.Fatalf("Import: %v", err)
	}

	for _, p := range []string{"configuration.yaml", "esphome/plant-care.yaml", "esphome/.gitignore"} {
		if _, ok := showAtRef(t, bare, "main", p); !ok {
			t.Errorf("%s missing from the import", p)
		}
	}
	for _, p := range []string{"esphome/plant-care/src/main.cpp", "esphome/plant-care/platformio.ini"} {
		if _, ok := showAtRef(t, bare, "main", p); ok {
			t.Errorf("%s was imported, want esphome/.gitignore honored", p)
		}
	}
}

// A live .gitignore overwrites the repository's copy, so the bulk "git add"
// applies its content; the filter must read the same file, not both.
func TestImportAppliesTheGitignoreItIsAboutToCommit(t *testing.T) {
	tmp := t.TempDir()
	bare, work := makeRemote(t, tmp, "repo")
	commitFile(t, work, GitignoreFile, "*.ignored-by-repo\n", "repo rules")

	configRoot := filepath.Join(tmp, "config")
	writeLiveText(t, configRoot, GitignoreFile, "*.ignored-by-live\n")
	writeLive(t, configRoot, "a.ignored-by-repo", 10)
	writeLive(t, configRoot, "b.ignored-by-live", 10)

	gs := importGitSync(t, bare, filepath.Join(tmp, "workdir"))
	if _, err := gs.Import(context.Background(), configRoot, generousLimits(), fixedImportTime); err != nil {
		t.Fatalf("Import: %v", err)
	}
	// The repository's old rule dies with this commit, so it must not
	// shape what the same import keeps.
	if _, ok := showAtRef(t, bare, "main", "a.ignored-by-repo"); !ok {
		t.Error("a.ignored-by-repo was skipped by a rule this import replaces")
	}
	if _, ok := showAtRef(t, bare, "main", "b.ignored-by-live"); ok {
		t.Error("b.ignored-by-live was imported, want the rule it is committed with applied")
	}
	if got, ok := showAtRef(t, bare, "main", GitignoreFile); !ok || got != "*.ignored-by-live\n" {
		t.Errorf("main:%s = %q (ok=%v), want the live content imported", GitignoreFile, got, ok)
	}
}

// The preview tree is built from git and scan paths, repo-relative today;
// this is what confines the write if either ever is not.
func TestWriteUnderRootRefusesEscapes(t *testing.T) {
	root := t.TempDir()
	for _, rel := range []string{"../escape", "../../etc/passwd", "/etc/passwd", "sub/../../out"} {
		if err := writeUnderRoot(root, rel, []byte("x")); err == nil {
			t.Errorf("writeUnderRoot(%q) succeeded, want a refusal", rel)
		}
	}
	if err := writeUnderRoot(root, "esphome/.gitignore", []byte("**/src/\n")); err != nil {
		t.Errorf("writeUnderRoot on an ordinary nested path: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(root, "esphome", ".gitignore"))
	if err != nil || string(got) != "**/src/\n" {
		t.Errorf("nested write = %q (err %v), want the content", string(got), err)
	}
}

func TestPreviewIgnoredMatchesWhatAnImportCommits(t *testing.T) {
	tmp := t.TempDir()
	bare := filepath.Join(tmp, "empty.git")
	runGitHelper(t, tmp, "init", "--bare", "-b", "main", bare)

	configRoot := filepath.Join(tmp, "config")
	writeLive(t, configRoot, "configuration.yaml", 10)
	writeLive(t, configRoot, "custom_components/hacs/__init__.py", 10)
	writeLiveText(t, configRoot, "esphome/.gitignore", "**/src/\n")
	writeLive(t, configRoot, "esphome/plant-care.yaml", 10)
	writeLive(t, configRoot, "esphome/plant-care/src/main.cpp", 10)

	plan, err := ScanLive(configRoot, generousLimits())
	if err != nil {
		t.Fatalf("ScanLive: %v", err)
	}
	gs := importGitSync(t, bare, filepath.Join(tmp, "workdir"))
	kept, _, err := gs.PreviewIgnored(context.Background(), configRoot, plan.Files)
	if err != nil {
		t.Fatalf("PreviewIgnored: %v", err)
	}

	res, err := gs.Import(context.Background(), configRoot, generousLimits(), fixedImportTime)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if len(kept) != res.Files {
		t.Errorf("preview said %d file(s), import committed %d", len(kept), res.Files)
	}
	for _, p := range kept {
		if _, ok := showAtRef(t, bare, "main", p); !ok {
			t.Errorf("preview listed %s but the import did not commit it", p)
		}
	}
}

// A populated repo has .gitignore files of its own that the preview cannot
// read from the worktree, so it reconstructs them from HEAD - preview-only
// code with no counterpart in the import to keep it honest.
func TestPreviewIgnoredMatchesAnImportIntoAPopulatedRepo(t *testing.T) {
	tmp := t.TempDir()
	bare, work := makeRemote(t, tmp, "repo")
	commitFile(t, work, GitignoreFile, "*.repo-rule\n", "root rules")
	commitFile(t, work, "packages/"+GitignoreFile, "nested.repo-rule\n", "nested rules")

	configRoot := filepath.Join(tmp, "config")
	writeLive(t, configRoot, "configuration.yaml", 10)
	writeLive(t, configRoot, "a.repo-rule", 10)
	writeLive(t, configRoot, "packages/lights.yaml", 10)
	writeLive(t, configRoot, "packages/nested.repo-rule", 10)

	plan, err := ScanLive(configRoot, generousLimits())
	if err != nil {
		t.Fatalf("ScanLive: %v", err)
	}
	gs := importGitSync(t, bare, filepath.Join(tmp, "workdir"))
	kept, keptBytes, err := gs.PreviewIgnored(context.Background(), configRoot, plan.Files)
	if err != nil {
		t.Fatalf("PreviewIgnored: %v", err)
	}
	for _, p := range []string{"a.repo-rule", "packages/nested.repo-rule"} {
		if slices.Contains(kept, p) {
			t.Errorf("preview kept %s, but a repository rule ignores it", p)
		}
	}

	res, err := gs.Import(context.Background(), configRoot, generousLimits(), fixedImportTime)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if len(kept) != res.Files {
		t.Errorf("preview said %d file(s), import committed %d", len(kept), res.Files)
	}
	if keptBytes != res.Bytes {
		t.Errorf("preview said %d byte(s), import committed %d", keptBytes, res.Bytes)
	}
	for _, p := range kept {
		if _, ok := showAtRef(t, bare, "main", p); !ok {
			t.Errorf("preview listed %s but the import did not commit it", p)
		}
	}
}

// The differ reads the worktree, so writing the live .gitignore files in
// (what the real import does) would report genuine drift as in sync.
func TestPreviewIgnoredLeavesTheWorktreeAlone(t *testing.T) {
	tmp := t.TempDir()
	bare, work := makeRemote(t, tmp, "repo")
	commitFile(t, work, GitignoreFile, "*.repo-rule\n", "repo rules")

	configRoot := filepath.Join(tmp, "config")
	writeLiveText(t, configRoot, GitignoreFile, "*.live-rule\n")
	writeLive(t, configRoot, "configuration.yaml", 10)

	gs := checkedOutGitSync(t, bare, filepath.Join(tmp, "workdir"))
	plan, err := ScanLive(configRoot, generousLimits())
	if err != nil {
		t.Fatalf("ScanLive: %v", err)
	}
	if _, _, err := gs.PreviewIgnored(context.Background(), configRoot, plan.Files); err != nil {
		t.Fatalf("PreviewIgnored: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(gs.Workdir, GitignoreFile))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "*.repo-rule\n" {
		t.Errorf("worktree %s = %q, want the repository's copy untouched", GitignoreFile, string(got))
	}
}
