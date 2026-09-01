package applier

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/fsx"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/options"
)

// --- test fixtures -----------------------------------------------------

func testConfig(t *testing.T) Config {
	t.Helper()
	dir := t.TempDir()
	cfg := DefaultConfig()
	cfg.BackupRoot = filepath.Join(dir, "backup")
	cfg.StatePath = filepath.Join(dir, "state.json")
	// Keep timeouts/poll interval tiny so tests run instantly; individual
	// tests override these further where the exact value matters.
	cfg.HealthProbeInterval = 0
	cfg.HealthProbeTimeoutReload = 50 * time.Millisecond
	cfg.HealthProbeTimeoutRestart = 50 * time.Millisecond
	return cfg
}

func testOptions(applyAfterPull string) options.Options {
	return options.Options{
		RepoURL:            "https://example.invalid/demo.git",
		Branch:             "main",
		IntervalMinutes:    5,
		DryRun:             false,
		ApplyAfterPull:     applyAfterPull,
		ReconcileYAMLFiles: true,
	}
}

type jsonResponse struct {
	status int
	body   map[string]any
}

func (r jsonResponse) httpResponse() *http.Response {
	data, _ := json.Marshal(r.body)
	return &http.Response{StatusCode: r.status, Body: io.NopCloser(bytes.NewReader(data)), Header: make(http.Header)}
}

type call struct {
	method string
	url    string
	auth   string
}

// fakeClient records every request and dispatches by the URL suffixes
// registered in postResponses/getResponses. authToken, if set, also gates
// GET .../core/api/ (the health probe) on the Authorization header.
type fakeClient struct {
	postResponses map[string]jsonResponse
	getResponses  map[string]jsonResponse
	raiseOn       map[string]error
	authToken     string
	calls         []call
}

func (f *fakeClient) Do(req *http.Request) (*http.Response, error) {
	url := req.URL.String()
	f.calls = append(f.calls, call{method: req.Method, url: url, auth: req.Header.Get("Authorization")})

	if err, ok := f.raiseOn[url]; ok {
		return nil, err
	}

	if f.authToken != "" && strings.HasSuffix(url, "/core/api/") {
		if req.Header.Get("Authorization") != "Bearer "+f.authToken {
			return jsonResponse{status: 401, body: map[string]any{}}.httpResponse(), nil
		}
		return jsonResponse{status: 200, body: map[string]any{"message": "API running."}}.httpResponse(), nil
	}

	table := f.getResponses
	if req.Method == http.MethodPost {
		table = f.postResponses
	}
	for suffix, resp := range table {
		if strings.HasSuffix(url, suffix) {
			return resp.httpResponse(), nil
		}
	}
	return jsonResponse{status: 200, body: map[string]any{}}.httpResponse(), nil
}

func validCheckConfigClient(extraPost map[string]jsonResponse) *fakeClient {
	post := map[string]jsonResponse{
		"/core/api/config/core/check_config": {status: 200, body: map[string]any{"result": "valid", "errors": nil}},
	}
	for k, v := range extraPost {
		post[k] = v
	}
	return &fakeClient{
		postResponses: post,
		getResponses:  map[string]jsonResponse{"/core/api/": {status: 200, body: map[string]any{}}},
	}
}

func writeText(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readText(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func hasCall(calls []call, method, url string) bool {
	for _, c := range calls {
		if c.method == method && c.url == url {
			return true
		}
	}
	return false
}

// --- Apply(): basics -----------------------------------------------------

func TestEmptyChangesIsNoop(t *testing.T) {
	t.Setenv("SUPERVISOR_TOKEN", "test-token")
	cfg := testConfig(t)
	configRoot := t.TempDir()
	before, _ := os.ReadDir(configRoot)

	result, err := Apply(context.Background(), cfg, nil, filepath.Join(t.TempDir(), "repo"), configRoot, testOptions("off"), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.OK || len(result.Changed) != 0 || result.Error != "" || result.RolledBack || result.StashDir != "" {
		t.Errorf("result = %+v, want an all-zero OK result", result)
	}
	after, _ := os.ReadDir(configRoot)
	if len(before) != len(after) {
		t.Errorf("config root contents changed")
	}
	if _, err := os.Stat(cfg.BackupRoot); !os.IsNotExist(err) {
		t.Errorf("backup root should not exist")
	}
}

// The token is read before anything is written: finding it missing after
// writeChanges would leave the config overwritten with no check_config and
// no stash pointer to roll back from.
func TestMissingSupervisorTokenWritesNothing(t *testing.T) {
	t.Setenv("SUPERVISOR_TOKEN", "")
	cfg := testConfig(t)
	repoRoot := t.TempDir()
	configRoot := t.TempDir()
	writeText(t, filepath.Join(repoRoot, "automations.yaml"), "new\n")
	writeText(t, filepath.Join(configRoot, "automations.yaml"), "old\n")

	changes := []Change{{Path: "automations.yaml", Kind: ChangeUpdate}}
	_, err := Apply(context.Background(), cfg, changes, repoRoot, configRoot, testOptions("off"), validCheckConfigClient(nil))
	if err == nil {
		t.Fatal("expected an error for the missing token")
	}
	if got := readText(t, filepath.Join(configRoot, "automations.yaml")); got != "old\n" {
		t.Errorf("automations.yaml = %q, want untouched", got)
	}
	if _, statErr := os.Stat(cfg.BackupRoot); !os.IsNotExist(statErr) {
		t.Errorf("backup root should not exist: nothing was stashed")
	}
}

// A backup root that is not a directory must be an error, not a spin:
// os.IsNotExist is false for ENOTDIR, and the probe loop used to retry the
// next suffix forever with the operation lock held.
func TestBackupRootAsAFileFailsInsteadOfLooping(t *testing.T) {
	t.Setenv("SUPERVISOR_TOKEN", "test-token")
	cfg := testConfig(t)
	writeText(t, cfg.BackupRoot, "not a directory\n")
	repoRoot := t.TempDir()
	configRoot := t.TempDir()
	writeText(t, filepath.Join(repoRoot, "automations.yaml"), "new\n")
	writeText(t, filepath.Join(configRoot, "automations.yaml"), "old\n")

	changes := []Change{{Path: "automations.yaml", Kind: ChangeUpdate}}
	_, err := Apply(context.Background(), cfg, changes, repoRoot, configRoot, testOptions("off"), validCheckConfigClient(nil))
	if err == nil {
		t.Fatal("expected an error for the unusable backup root")
	}
	if got := readText(t, filepath.Join(configRoot, "automations.yaml")); got != "old\n" {
		t.Errorf("automations.yaml = %q, want untouched", got)
	}
}

func TestHappyPathWritesFilesAndReturnsOK(t *testing.T) {
	t.Setenv("SUPERVISOR_TOKEN", "test-token")
	cfg := testConfig(t)
	repoRoot := t.TempDir()
	configRoot := t.TempDir()
	writeText(t, filepath.Join(repoRoot, "automations.yaml"), "- id: demo\n  alias: Demo\n")
	writeText(t, filepath.Join(configRoot, "automations.yaml"), "- id: demo\n")

	changes := []Change{{Path: "automations.yaml", Kind: ChangeUpdate}}
	client := validCheckConfigClient(nil)

	result, err := Apply(context.Background(), cfg, changes, repoRoot, configRoot, testOptions("off"), client)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !result.OK || result.RolledBack {
		t.Fatalf("result = %+v", result)
	}
	if len(result.Changed) != 1 || result.Changed[0] != "automations.yaml" {
		t.Errorf("changed = %+v", result.Changed)
	}
	if result.StashDir == "" {
		t.Fatal("expected a stash dir")
	}
	if info, err := os.Stat(result.StashDir); err != nil || !info.IsDir() {
		t.Errorf("stash dir not created: %v", err)
	}
	if got := readText(t, filepath.Join(configRoot, "automations.yaml")); got != "- id: demo\n  alias: Demo\n" {
		t.Errorf("automations.yaml = %q", got)
	}
}

func TestAddAndDeleteWrites(t *testing.T) {
	t.Setenv("SUPERVISOR_TOKEN", "test-token")
	cfg := testConfig(t)
	repoRoot := t.TempDir()
	configRoot := t.TempDir()
	writeText(t, filepath.Join(repoRoot, "new.yaml"), "k: v\n")
	writeText(t, filepath.Join(configRoot, "old.yaml"), "gone: true\n")

	changes := []Change{
		{Path: "new.yaml", Kind: ChangeAdd},
		{Path: "old.yaml", Kind: ChangeDelete},
	}
	client := validCheckConfigClient(nil)

	result, err := Apply(context.Background(), cfg, changes, repoRoot, configRoot, testOptions("off"), client)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !result.OK {
		t.Fatalf("result = %+v", result)
	}
	if len(result.Changed) != 2 {
		t.Errorf("changed = %+v", result.Changed)
	}
	if _, err := os.Stat(filepath.Join(configRoot, "new.yaml")); err != nil {
		t.Errorf("new.yaml should exist")
	}
	if _, err := os.Stat(filepath.Join(configRoot, "old.yaml")); !os.IsNotExist(err) {
		t.Errorf("old.yaml should be removed")
	}
}

func TestInvalidCheckConfigRestoresBytesAndRemovesAddedFiles(t *testing.T) {
	t.Setenv("SUPERVISOR_TOKEN", "test-token")
	cfg := testConfig(t)
	repoRoot := t.TempDir()
	configRoot := t.TempDir()

	originalBytes := "- id: demo\n"
	writeText(t, filepath.Join(repoRoot, "automations.yaml"), "- id: demo\n  alias: Demo\n")
	writeText(t, filepath.Join(configRoot, "automations.yaml"), originalBytes)
	writeText(t, filepath.Join(repoRoot, "new.yaml"), "k: v\n")

	changes := []Change{
		{Path: "automations.yaml", Kind: ChangeUpdate},
		{Path: "new.yaml", Kind: ChangeAdd},
	}
	client := &fakeClient{postResponses: map[string]jsonResponse{
		"/core/api/config/core/check_config": {
			status: 200, body: map[string]any{"result": "invalid", "errors": "automations.yaml: bad alias"},
		},
	}}

	result, err := Apply(context.Background(), cfg, changes, repoRoot, configRoot, testOptions("off"), client)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.OK || !result.RolledBack {
		t.Fatalf("result = %+v", result)
	}
	if len(result.Changed) != 0 {
		t.Errorf("changed = %+v, want empty", result.Changed)
	}
	if result.Error != "automations.yaml: bad alias" {
		t.Errorf("error = %q", result.Error)
	}
	// stash_dir stays populated even on a rolled-back failure, so the UI
	// can offer a manual retry when the automatic one was incomplete.
	if result.StashDir == "" {
		t.Error("expected a stash dir")
	}
	if got := readText(t, filepath.Join(configRoot, "automations.yaml")); got != originalBytes {
		t.Errorf("automations.yaml = %q, want %q", got, originalBytes)
	}
	if _, err := os.Stat(filepath.Join(configRoot, "new.yaml")); !os.IsNotExist(err) {
		t.Errorf("new.yaml should have been removed")
	}
}

func TestConnectionErrorOnCheckConfigRollsBack(t *testing.T) {
	t.Setenv("SUPERVISOR_TOKEN", "test-token")
	cfg := testConfig(t)
	repoRoot := t.TempDir()
	configRoot := t.TempDir()
	writeText(t, filepath.Join(repoRoot, "automations.yaml"), "changed\n")
	writeText(t, filepath.Join(configRoot, "automations.yaml"), "original\n")

	changes := []Change{{Path: "automations.yaml", Kind: ChangeUpdate}}
	client := &fakeClient{raiseOn: map[string]error{
		cfg.Supervisor + "/core/api/config/core/check_config": errors.New("refused"),
	}}

	result, err := Apply(context.Background(), cfg, changes, repoRoot, configRoot, testOptions("off"), client)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.OK || !result.RolledBack {
		t.Fatalf("result = %+v", result)
	}
	if got := readText(t, filepath.Join(configRoot, "automations.yaml")); got != "original\n" {
		t.Errorf("automations.yaml = %q", got)
	}
	if !strings.Contains(result.Error, "check_config request failed") {
		t.Errorf("error = %q", result.Error)
	}
}

func TestNon2xxCheckConfigReportsCoresOwnReason(t *testing.T) {
	// A bare status code says nothing a user can act on; Core already
	// explains itself in the response body.
	t.Setenv("SUPERVISOR_TOKEN", "test-token")
	cfg := testConfig(t)
	repoRoot := t.TempDir()
	configRoot := t.TempDir()
	writeText(t, filepath.Join(repoRoot, "automations.yaml"), "changed\n")
	writeText(t, filepath.Join(configRoot, "automations.yaml"), "original\n")

	changes := []Change{{Path: "automations.yaml", Kind: ChangeUpdate}}
	client := &fakeClient{postResponses: map[string]jsonResponse{
		"/core/api/config/core/check_config": {status: 500, body: map[string]any{"message": "Config check failed to start"}},
	}}

	result, err := Apply(context.Background(), cfg, changes, repoRoot, configRoot, testOptions("off"), client)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.OK || !result.RolledBack {
		t.Fatalf("result = %+v", result)
	}
	if !strings.Contains(result.Error, "check_config returned HTTP 500") {
		t.Errorf("error = %q, want the status", result.Error)
	}
	if !strings.Contains(result.Error, "Config check failed to start") {
		t.Errorf("error = %q, want core's own reason alongside the status", result.Error)
	}
}

// --- Apply(): check_config warnings ---------------------------------------

func TestValidCheckConfigWarningsAreCapturedInResult(t *testing.T) {
	t.Setenv("SUPERVISOR_TOKEN", "test-token")
	cfg := testConfig(t)
	repoRoot := t.TempDir()
	configRoot := t.TempDir()
	writeText(t, filepath.Join(repoRoot, "automations.yaml"), "- id: demo\n  alias: Demo\n")
	writeText(t, filepath.Join(configRoot, "automations.yaml"), "- id: demo\n")

	changes := []Change{{Path: "automations.yaml", Kind: ChangeUpdate}}
	client := &fakeClient{postResponses: map[string]jsonResponse{
		"/core/api/config/core/check_config": {
			status: 200,
			body: map[string]any{
				"result":   "valid",
				"errors":   nil,
				"warnings": "Integration 'templete' not found.\nPlease check your config.",
			},
		},
	}}

	result, err := Apply(context.Background(), cfg, changes, repoRoot, configRoot, testOptions("off"), client)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !result.OK || result.RolledBack {
		t.Fatalf("result = %+v", result)
	}
	if want := "Integration 'templete' not found.\nPlease check your config."; result.Warnings != want {
		t.Errorf("warnings = %q, want %q", result.Warnings, want)
	}
}

func TestAbsentOrNullCheckConfigWarningsReturnEmptyString(t *testing.T) {
	for _, tc := range []struct {
		name string
		body map[string]any
	}{
		{"absent", map[string]any{"result": "valid", "errors": nil}},
		{"null", map[string]any{"result": "valid", "errors": nil, "warnings": nil}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SUPERVISOR_TOKEN", "test-token")
			cfg := testConfig(t)
			repoRoot := t.TempDir()
			configRoot := t.TempDir()
			writeText(t, filepath.Join(repoRoot, "automations.yaml"), "changed\n")
			writeText(t, filepath.Join(configRoot, "automations.yaml"), "original\n")

			changes := []Change{{Path: "automations.yaml", Kind: ChangeUpdate}}
			client := &fakeClient{postResponses: map[string]jsonResponse{
				"/core/api/config/core/check_config": {status: 200, body: tc.body},
			}}

			result, err := Apply(context.Background(), cfg, changes, repoRoot, configRoot, testOptions("off"), client)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if !result.OK {
				t.Fatalf("result = %+v", result)
			}
			if result.Warnings != "" {
				t.Errorf("warnings = %q, want empty", result.Warnings)
			}
		})
	}
}

func TestInvalidCheckConfigNeverSurfacesWarnings(t *testing.T) {
	// An invalid config already rolls back; any warnings alongside the
	// errors are not meaningful and must not leak into Result.Warnings.
	t.Setenv("SUPERVISOR_TOKEN", "test-token")
	cfg := testConfig(t)
	repoRoot := t.TempDir()
	configRoot := t.TempDir()
	writeText(t, filepath.Join(repoRoot, "automations.yaml"), "changed\n")
	writeText(t, filepath.Join(configRoot, "automations.yaml"), "original\n")

	changes := []Change{{Path: "automations.yaml", Kind: ChangeUpdate}}
	client := &fakeClient{postResponses: map[string]jsonResponse{
		"/core/api/config/core/check_config": {
			status: 200,
			body: map[string]any{
				"result": "invalid", "errors": "bad alias", "warnings": "unrelated warning",
			},
		},
	}}

	result, err := Apply(context.Background(), cfg, changes, repoRoot, configRoot, testOptions("off"), client)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.OK || !result.RolledBack {
		t.Fatalf("result = %+v", result)
	}
	if result.Warnings != "" {
		t.Errorf("warnings = %q, want empty", result.Warnings)
	}
}

// --- Apply(): path guard -------------------------------------------------

func TestPathTraversalChangeIsSkippedNotFatal(t *testing.T) {
	// A single offending change must not abort the whole apply: it is
	// skipped and reported, and here it was the only change in the batch.
	for _, badPath := range []string{"../../etc/passwd", "/etc/passwd", "sub/../../x"} {
		t.Run(badPath, func(t *testing.T) {
			t.Setenv("SUPERVISOR_TOKEN", "test-token")
			cfg := testConfig(t)
			repoRoot := t.TempDir()
			configRoot := t.TempDir()
			writeText(t, filepath.Join(configRoot, "safe.yaml"), "keep: me\n")

			changes := []Change{{Path: badPath, Kind: ChangeAdd}}

			result, err := Apply(context.Background(), cfg, changes, repoRoot, configRoot, testOptions("off"), nil)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if result.OK {
				t.Fatalf("result = %+v", result)
			}
			if result.StashDir != "" {
				t.Errorf("stash dir = %q, want empty", result.StashDir)
			}
			if !strings.Contains(result.Error, badPath) {
				t.Errorf("error = %q, want it to contain %q", result.Error, badPath)
			}

			if _, err := os.Stat(cfg.BackupRoot); !os.IsNotExist(err) {
				t.Errorf("backup root should not exist")
			}
			entries, _ := os.ReadDir(configRoot)
			if len(entries) != 1 || entries[0].Name() != "safe.yaml" {
				t.Errorf("config root entries = %+v", entries)
			}
			if got := readText(t, filepath.Join(configRoot, "safe.yaml")); got != "keep: me\n" {
				t.Errorf("safe.yaml = %q", got)
			}
		})
	}
}

func TestExcludedPathIsSkippedNotFatal(t *testing.T) {
	t.Setenv("SUPERVISOR_TOKEN", "test-token")
	cfg := testConfig(t)
	repoRoot := t.TempDir()
	configRoot := t.TempDir()

	changes := []Change{{Path: "secrets.yaml", Kind: ChangeAdd}}

	result, err := Apply(context.Background(), cfg, changes, repoRoot, configRoot, testOptions("off"), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.OK {
		t.Fatalf("result = %+v", result)
	}
	if !strings.Contains(result.Error, "secrets.yaml") {
		t.Errorf("error = %q", result.Error)
	}
	if _, err := os.Stat(cfg.BackupRoot); !os.IsNotExist(err) {
		t.Errorf("backup root should not exist")
	}
}

func TestOneBadPathAmongGoodChangesAppliesTheGoodOnes(t *testing.T) {
	// Two realistic triggers for a bad path mid-batch: a stale manifest
	// entry, or a symlinked subdirectory landing outside config_root.
	t.Setenv("SUPERVISOR_TOKEN", "test-token")
	cfg := testConfig(t)
	repoRoot := t.TempDir()
	configRoot := t.TempDir()
	writeText(t, filepath.Join(repoRoot, "automations.yaml"), "- id: demo\n  alias: Demo\n")
	writeText(t, filepath.Join(configRoot, "automations.yaml"), "- id: demo\n")

	changes := []Change{
		{Path: "../outside.yaml", Kind: ChangeDelete},
		{Path: "automations.yaml", Kind: ChangeUpdate},
	}
	client := validCheckConfigClient(nil)

	result, err := Apply(context.Background(), cfg, changes, repoRoot, configRoot, testOptions("off"), client)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !result.OK {
		t.Fatalf("result = %+v", result)
	}
	if len(result.Changed) != 1 || result.Changed[0] != "automations.yaml" {
		t.Errorf("changed = %+v", result.Changed)
	}
	if !strings.Contains(result.Error, "outside.yaml") {
		t.Errorf("error = %q", result.Error)
	}
	if got := readText(t, filepath.Join(configRoot, "automations.yaml")); got != "- id: demo\n  alias: Demo\n" {
		t.Errorf("automations.yaml = %q", got)
	}
}

// TestSymlinkSourceRefused is the repo-to-config counterpart of the differ
// package's tracked-symlink refusal: guardChangePath validates only the
// destination path string, so a repoRoot source that is itself a symlink
// could copy whatever it points to into live config. copyFile now Lstats
// the source, and that failure takes the writeChanges-error path.
func TestSymlinkSourceRefused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks behave differently on windows")
	}
	t.Setenv("SUPERVISOR_TOKEN", "test-token")
	cfg := testConfig(t)
	repoRoot := t.TempDir()
	configRoot := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.json")
	if err := os.WriteFile(outside, []byte(`{"git_token":"leaked"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(repoRoot, "automations.yaml")); err != nil {
		t.Fatal(err)
	}
	writeText(t, filepath.Join(configRoot, "automations.yaml"), "keep: me\n")

	changes := []Change{{Path: "automations.yaml", Kind: ChangeUpdate}}
	client := validCheckConfigClient(nil)

	result, err := Apply(context.Background(), cfg, changes, repoRoot, configRoot, testOptions("off"), client)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.OK || !result.RolledBack {
		t.Fatalf("result = %+v, want a rolled-back failure", result)
	}
	if !strings.Contains(result.Error, "non-regular") {
		t.Errorf("error = %q, want it to mention the refusal", result.Error)
	}
	got := readText(t, filepath.Join(configRoot, "automations.yaml"))
	if got != "keep: me\n" {
		t.Errorf("automations.yaml = %q, want original content preserved", got)
	}
	if strings.Contains(got, "leaked") {
		t.Fatal("secret content leaked into config")
	}
}

// TestSymlinkDestinationRefused covers the write-through-symlink case: a
// dangling destination symlink resolves to nothing, so stashFiles' os.Stat
// reads it as "absent" and guardChangePath's realpath lets it through
// (EvalSymlinks fails open on a nonexistent target). Without copyFile's own
// destination guard, os.WriteFile follows it and plants a brand-new file
// outside configRoot.
func TestSymlinkDestinationRefused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks behave differently on windows")
	}
	t.Setenv("SUPERVISOR_TOKEN", "test-token")
	cfg := testConfig(t)
	repoRoot := t.TempDir()
	configRoot := t.TempDir()
	writeText(t, filepath.Join(repoRoot, "automations.yaml"), "- id: demo\n  alias: Demo\n")
	outsideTarget := filepath.Join(t.TempDir(), "planted.yaml")
	if err := os.Symlink(outsideTarget, filepath.Join(configRoot, "automations.yaml")); err != nil {
		t.Fatal(err)
	}

	changes := []Change{{Path: "automations.yaml", Kind: ChangeUpdate}}
	client := validCheckConfigClient(nil)

	result, err := Apply(context.Background(), cfg, changes, repoRoot, configRoot, testOptions("off"), client)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.OK {
		t.Fatalf("result = %+v, want failure", result)
	}
	if !strings.Contains(result.Error, "non-regular") {
		t.Errorf("error = %q, want it to mention the refusal", result.Error)
	}
	if _, statErr := os.Lstat(outsideTarget); !os.IsNotExist(statErr) {
		t.Fatal("outside target must not have been created through the dangling symlink")
	}
}

// TestGuardChangePathRejectsDotDotIntoGitopsRoot is the applier-level leg
// of the gitsync.Excluded normalization fix: guardChangePath calls
// cfg.IsExcluded before its own traversal check, so a raw
// "sub/../gitops/foo.yaml" used to sail through with segments[0] == "sub"
// and reach a real gitops/ file inside configRoot.
func TestGuardChangePathRejectsDotDotIntoGitopsRoot(t *testing.T) {
	cfg := DefaultConfig()
	configRoot := t.TempDir()
	writeText(t, filepath.Join(configRoot, "gitops", "registries.yaml"), "floors: []\n")
	configRootReal := fsx.Realpath(configRoot)

	if err := guardChangePath(cfg, "sub/../gitops/registries.yaml", configRootReal); err == nil {
		t.Fatal("guardChangePath = nil, want a rejection for a path that cleans into gitops/")
	}
}

// --- RollbackFrom() --------------------------------------------------------

func TestRollbackFromIsIdempotent(t *testing.T) {
	configRoot := t.TempDir()
	writeText(t, filepath.Join(configRoot, "kept.yaml"), "live\n")

	stashDir := filepath.Join(t.TempDir(), "backup", "20260101T000000Z")
	if err := os.MkdirAll(stashDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeText(t, filepath.Join(stashDir, "kept.yaml"), "backed-up\n")
	writeText(t, filepath.Join(stashDir, "manifest.json"), `{"kept.yaml": "existed", "added.yaml": "absent"}`)
	writeText(t, filepath.Join(configRoot, "added.yaml"), "should be removed\n")

	cfg := DefaultConfig()
	first := RollbackFrom(cfg, stashDir, configRoot)
	second := RollbackFrom(cfg, stashDir, configRoot) // must not misbehave

	if !first.OK || !first.RolledBack {
		t.Fatalf("first = %+v", first)
	}
	if first.StashDir != stashDir {
		t.Errorf("stash dir = %q, want %q", first.StashDir, stashDir)
	}
	if !second.OK {
		t.Errorf("second = %+v", second)
	}
	if got := readText(t, filepath.Join(configRoot, "kept.yaml")); got != "backed-up\n" {
		t.Errorf("kept.yaml = %q", got)
	}
	if _, err := os.Stat(filepath.Join(configRoot, "added.yaml")); !os.IsNotExist(err) {
		t.Errorf("added.yaml should be removed")
	}
}

func TestRollbackFromMissingManifestReturnsErrorResult(t *testing.T) {
	stashDir := filepath.Join(t.TempDir(), "backup", "does-not-exist")

	result := RollbackFrom(DefaultConfig(), stashDir, t.TempDir())

	if result.OK || result.RolledBack {
		t.Fatalf("result = %+v", result)
	}
	if result.StashDir != stashDir {
		t.Errorf("stash dir = %q", result.StashDir)
	}
	if !strings.Contains(result.Error, "manifest") {
		t.Errorf("error = %q", result.Error)
	}
}

func TestRollbackFromCorruptManifestReturnsErrorResult(t *testing.T) {
	stashDir := filepath.Join(t.TempDir(), "backup", "20260101T000000Z")
	if err := os.MkdirAll(stashDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeText(t, filepath.Join(stashDir, "manifest.json"), "not valid json{")

	result := RollbackFrom(DefaultConfig(), stashDir, t.TempDir())

	if result.OK {
		t.Fatalf("result = %+v", result)
	}
	if !strings.Contains(result.Error, "manifest") {
		t.Errorf("error = %q", result.Error)
	}
}

func TestRollbackFromReportsIncompleteWhenStashedCopyMissing(t *testing.T) {
	configRoot := t.TempDir()
	stashDir := filepath.Join(t.TempDir(), "backup", "20260101T000000Z")
	if err := os.MkdirAll(stashDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// The manifest claims a stashed copy of kept.yaml exists but none was
	// written - a stash partially cleaned up or corrupted on disk.
	writeText(t, filepath.Join(stashDir, "manifest.json"), `{"files": {"kept.yaml": "existed"}, "created_dirs": []}`)

	result := RollbackFrom(DefaultConfig(), stashDir, configRoot)

	if result.OK || result.RolledBack {
		t.Fatalf("result = %+v", result)
	}
	if !strings.Contains(result.Error, "kept.yaml") {
		t.Errorf("error = %q", result.Error)
	}
}

// --- MakeStashDir() / PruneStashDirs() ------------------------------------

func TestMakeStashDirIsImmediatelyValidForRollbackFrom(t *testing.T) {
	// MakeStashDir is used for a registry-only apply, where no file gets
	// stashed: RollbackFrom must read that as "nothing to roll back"
	// (OK=true) rather than the missing-manifest error.
	cfg := testConfig(t)
	stashDir, err := MakeStashDir(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if info, statErr := os.Stat(stashDir); statErr != nil || !info.IsDir() {
		t.Fatalf("stash dir not created: %v", statErr)
	}
	if !strings.HasPrefix(stashDir, cfg.BackupRoot) {
		t.Errorf("stash dir %q not under backup root %q", stashDir, cfg.BackupRoot)
	}

	result := RollbackFrom(cfg, stashDir, t.TempDir())
	if !result.OK {
		t.Errorf("result = %+v", result)
	}
	if len(result.Changed) != 0 {
		t.Errorf("changed = %+v, want empty", result.Changed)
	}
}

func TestMakeStashDirAllocatesAFreshDirectoryEachCall(t *testing.T) {
	cfg := testConfig(t)
	first, err := MakeStashDir(cfg)
	if err != nil {
		t.Fatal(err)
	}
	second, err := MakeStashDir(cfg)
	if err != nil {
		t.Fatal(err)
	}

	if first == second {
		t.Errorf("both calls returned %q", first)
	}
	for _, d := range []string{first, second} {
		if info, err := os.Stat(d); err != nil || !info.IsDir() {
			t.Errorf("%q not a directory: %v", d, err)
		}
	}
}

func TestPruneStashDirsKeepsRecentAndNeverRemovesExcluded(t *testing.T) {
	cfg := testConfig(t)
	names := []string{
		"20260101T000000Z", "20260102T000000Z", "20260103T000000Z", "20260104T000000Z",
		"20260105T000000Z", "20260106T000000Z", "20260107T000000Z",
	}
	for _, name := range names {
		if err := os.MkdirAll(filepath.Join(cfg.BackupRoot, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// oldest of all, but still referenced by a pending rollback: must
	// survive.
	excluded := names[0]

	PruneStashDirs(cfg, 3, filepath.Join(cfg.BackupRoot, excluded))

	entries, err := os.ReadDir(cfg.BackupRoot)
	if err != nil {
		t.Fatal(err)
	}
	remaining := map[string]bool{}
	for _, e := range entries {
		remaining[e.Name()] = true
	}
	want := map[string]bool{excluded: true, "20260105T000000Z": true, "20260106T000000Z": true, "20260107T000000Z": true}
	if len(remaining) != len(want) {
		t.Fatalf("remaining = %+v, want %+v", remaining, want)
	}
	for k := range want {
		if !remaining[k] {
			t.Errorf("expected %q to remain", k)
		}
	}
}

func TestPruneStashDirsNoopWhenBackupRootMissing(t *testing.T) {
	cfg := testConfig(t)
	cfg.BackupRoot = filepath.Join(t.TempDir(), "does-not-exist")
	PruneStashDirs(cfg, 5, "") // must not panic
}

// --- StateLoad() / StateSave() --------------------------------------------

func TestStateLoadDropsUnsafeManifestEntries(t *testing.T) {
	cfg := testConfig(t)
	cfg.ConfigRoot = filepath.Join(t.TempDir(), "config")
	writeText(t, cfg.StatePath, `{
		"last_good_sha": "abc123",
		"manifest": ["good.yaml", "../outside.yaml", "/etc/passwd", "secrets.yaml", 42, ""],
		"last_apply_utc": null
	}`)

	state := StateLoad(cfg)

	if len(state.Manifest) != 1 || state.Manifest[0] != "good.yaml" {
		t.Errorf("manifest = %+v, want [good.yaml]", state.Manifest)
	}
}

func TestStateLoadSanitizesNonDictRegistryManaged(t *testing.T) {
	// A registry_managed that is a list, or any other non-mapping, must
	// reduce to {} rather than panic out of registries.Plan.
	cfg := testConfig(t)
	writeText(t, cfg.StatePath, `{"manifest": [], "registry_managed": ["not", "a", "dict"]}`)

	state := StateLoad(cfg)
	if len(state.RegistryManaged) != 0 {
		t.Errorf("registry_managed = %+v, want empty", state.RegistryManaged)
	}
}

func TestStateLoadDropsMalformedRegistryManagedEntries(t *testing.T) {
	// A non-string value in an otherwise-dict registry_managed is dropped
	// individually rather than resetting the whole mapping.
	cfg := testConfig(t)
	writeText(t, cfg.StatePath, `{"manifest": [], "registry_managed": {"floor:ground": "F1", "area:living_room": 42}}`)

	state := StateLoad(cfg)
	want := map[string]string{"floor:ground": "F1"}
	if len(state.RegistryManaged) != len(want) || state.RegistryManaged["floor:ground"] != "F1" {
		t.Errorf("registry_managed = %+v, want %+v", state.RegistryManaged, want)
	}
}

func TestStateLoadMissingRegistryManagedDefaultsToEmptyDict(t *testing.T) {
	// A state.json written before the registry layer existed must still
	// load with the field present and empty, not nil.
	cfg := testConfig(t)
	writeText(t, cfg.StatePath, `{"last_good_sha": "abc123", "manifest": [], "last_apply_utc": null}`)

	state := StateLoad(cfg)
	if state.RegistryManaged == nil || len(state.RegistryManaged) != 0 {
		t.Errorf("registry_managed = %+v, want empty non-nil map", state.RegistryManaged)
	}
}

func TestStateLoadSanitizesNonDictEntityOriginals(t *testing.T) {
	cfg := testConfig(t)
	writeText(t, cfg.StatePath, `{"manifest": [], "entity_originals": ["not", "a", "dict"]}`)

	state := StateLoad(cfg)
	if len(state.EntityOriginals) != 0 {
		t.Errorf("entity_originals = %+v, want empty", state.EntityOriginals)
	}
}

func TestStateLoadDropsMalformedEntityOriginalsEntries(t *testing.T) {
	// A per-entity value that is not itself a mapping is dropped
	// individually rather than resetting the whole mapping.
	cfg := testConfig(t)
	writeText(t, cfg.StatePath, `{"manifest": [], "entity_originals": {"entity:light.x": {"name": "Old"}, "entity:light.y": "not a dict"}}`)

	state := StateLoad(cfg)
	want := map[string]map[string]any{"entity:light.x": {"name": "Old"}}
	if !reflect.DeepEqual(state.EntityOriginals, want) {
		t.Errorf("entity_originals = %+v, want %+v", state.EntityOriginals, want)
	}
}

func TestStateLoadMissingEntityOriginalsDefaultsToEmptyDict(t *testing.T) {
	cfg := testConfig(t)
	writeText(t, cfg.StatePath, `{"last_good_sha": "abc123", "manifest": [], "last_apply_utc": null}`)

	state := StateLoad(cfg)
	if state.EntityOriginals == nil || len(state.EntityOriginals) != 0 {
		t.Errorf("entity_originals = %+v, want empty non-nil map", state.EntityOriginals)
	}
}

func TestStateLoadSanitizesNonDictDashboardManaged(t *testing.T) {
	cfg := testConfig(t)
	writeText(t, cfg.StatePath, `{"manifest": [], "dashboard_managed": ["not", "a", "dict"]}`)

	state := StateLoad(cfg)
	if len(state.DashboardManaged) != 0 {
		t.Errorf("dashboard_managed = %+v, want empty", state.DashboardManaged)
	}
}

func TestStateLoadDropsMalformedDashboardManagedEntries(t *testing.T) {
	cfg := testConfig(t)
	writeText(t, cfg.StatePath, `{"manifest": [], "dashboard_managed": {"dashboard:home": "abc123", "dashboard:other": 42}}`)

	state := StateLoad(cfg)
	want := map[string]string{"dashboard:home": "abc123"}
	if len(state.DashboardManaged) != len(want) || state.DashboardManaged["dashboard:home"] != "abc123" {
		t.Errorf("dashboard_managed = %+v, want %+v", state.DashboardManaged, want)
	}
}

func TestStateLoadMissingDashboardManagedDefaultsToEmptyDict(t *testing.T) {
	cfg := testConfig(t)
	writeText(t, cfg.StatePath, `{"last_good_sha": "abc123", "manifest": [], "last_apply_utc": null}`)

	state := StateLoad(cfg)
	if state.DashboardManaged == nil || len(state.DashboardManaged) != 0 {
		t.Errorf("dashboard_managed = %+v, want empty non-nil map", state.DashboardManaged)
	}
}

func TestStateLoadSanitizesNonDictAddonOriginals(t *testing.T) {
	cfg := testConfig(t)
	writeText(t, cfg.StatePath, `{"manifest": [], "addon_originals": ["not", "a", "dict"]}`)

	state := StateLoad(cfg)
	if len(state.AddonOriginals) != 0 {
		t.Errorf("addon_originals = %+v, want empty", state.AddonOriginals)
	}
}

func TestStateLoadDropsMalformedAddonOriginalsEntries(t *testing.T) {
	cfg := testConfig(t)
	writeText(t, cfg.StatePath,
		`{"manifest": [], "addon_originals": {"addon:core_configurator": {"dirsfirst": false}, "addon:other": "not a dict"}}`)

	state := StateLoad(cfg)
	want := map[string]map[string]any{"addon:core_configurator": {"dirsfirst": false}}
	if !reflect.DeepEqual(state.AddonOriginals, want) {
		t.Errorf("addon_originals = %+v, want %+v", state.AddonOriginals, want)
	}
}

func TestStateLoadMissingAddonOriginalsDefaultsToEmptyDict(t *testing.T) {
	cfg := testConfig(t)
	writeText(t, cfg.StatePath, `{"last_good_sha": "abc123", "manifest": [], "last_apply_utc": null}`)

	state := StateLoad(cfg)
	if state.AddonOriginals == nil || len(state.AddonOriginals) != 0 {
		t.Errorf("addon_originals = %+v, want empty non-nil map", state.AddonOriginals)
	}
}

func TestStateLoadSanitizesNonDictAddonRestartOnChange(t *testing.T) {
	cfg := testConfig(t)
	writeText(t, cfg.StatePath, `{"manifest": [], "addon_restart_on_change": ["not", "a", "dict"]}`)

	state := StateLoad(cfg)
	if len(state.AddonRestartOnChange) != 0 {
		t.Errorf("addon_restart_on_change = %+v, want empty", state.AddonRestartOnChange)
	}
}

func TestStateLoadDropsMalformedAddonRestartOnChangeEntries(t *testing.T) {
	cfg := testConfig(t)
	writeText(t, cfg.StatePath,
		`{"manifest": [], "addon_restart_on_change": {"addon:core_configurator": true, "addon:other": "not a bool"}}`)

	state := StateLoad(cfg)
	want := map[string]bool{"addon:core_configurator": true}
	if !reflect.DeepEqual(state.AddonRestartOnChange, want) {
		t.Errorf("addon_restart_on_change = %+v, want %+v", state.AddonRestartOnChange, want)
	}
}

func TestStateLoadMissingAddonRestartOnChangeDefaultsToEmptyDict(t *testing.T) {
	cfg := testConfig(t)
	writeText(t, cfg.StatePath, `{"last_good_sha": "abc123", "manifest": [], "last_apply_utc": null}`)

	state := StateLoad(cfg)
	if state.AddonRestartOnChange == nil || len(state.AddonRestartOnChange) != 0 {
		t.Errorf("addon_restart_on_change = %+v, want empty non-nil map", state.AddonRestartOnChange)
	}
}

func TestStateLoadSanitizesNonDictIntegrationManaged(t *testing.T) {
	cfg := testConfig(t)
	writeText(t, cfg.StatePath, `{"manifest": [], "integration_managed": ["not", "a", "dict"]}`)

	state := StateLoad(cfg)
	if len(state.IntegrationManaged) != 0 {
		t.Errorf("integration_managed = %+v, want empty", state.IntegrationManaged)
	}
}

func TestStateLoadDropsMalformedIntegrationManagedEntries(t *testing.T) {
	cfg := testConfig(t)
	writeText(t, cfg.StatePath, `{"manifest": [], "integration_managed": {"integration:workday_main": "abc123", "integration:other": 42}}`)

	state := StateLoad(cfg)
	want := map[string]string{"integration:workday_main": "abc123"}
	if !reflect.DeepEqual(state.IntegrationManaged, want) {
		t.Errorf("integration_managed = %+v, want %+v", state.IntegrationManaged, want)
	}
}

func TestStateLoadMissingIntegrationManagedDefaultsToEmptyDict(t *testing.T) {
	cfg := testConfig(t)
	writeText(t, cfg.StatePath, `{"last_good_sha": "abc123", "manifest": [], "last_apply_utc": null}`)

	state := StateLoad(cfg)
	if state.IntegrationManaged == nil || len(state.IntegrationManaged) != 0 {
		t.Errorf("integration_managed = %+v, want empty non-nil map", state.IntegrationManaged)
	}
}

func TestStateLoadSanitizesNonDictIntegrationHashes(t *testing.T) {
	cfg := testConfig(t)
	writeText(t, cfg.StatePath, `{"manifest": [], "integration_hashes": ["not", "a", "dict"]}`)

	state := StateLoad(cfg)
	if len(state.IntegrationHashes) != 0 {
		t.Errorf("integration_hashes = %+v, want empty", state.IntegrationHashes)
	}
}

func TestStateLoadDropsMalformedIntegrationHashesEntries(t *testing.T) {
	cfg := testConfig(t)
	writeText(t, cfg.StatePath, `{"manifest": [], "integration_hashes": {"integration:workday_main": "deadbeef", "integration:other": 42}}`)

	state := StateLoad(cfg)
	want := map[string]string{"integration:workday_main": "deadbeef"}
	if !reflect.DeepEqual(state.IntegrationHashes, want) {
		t.Errorf("integration_hashes = %+v, want %+v", state.IntegrationHashes, want)
	}
}

func TestStateLoadMissingIntegrationHashesDefaultsToEmptyDict(t *testing.T) {
	cfg := testConfig(t)
	writeText(t, cfg.StatePath, `{"last_good_sha": "abc123", "manifest": [], "last_apply_utc": null}`)

	state := StateLoad(cfg)
	if state.IntegrationHashes == nil || len(state.IntegrationHashes) != 0 {
		t.Errorf("integration_hashes = %+v, want empty non-nil map", state.IntegrationHashes)
	}
}

func TestStateLoadSanitizesNonDictIntegrationData(t *testing.T) {
	cfg := testConfig(t)
	writeText(t, cfg.StatePath, `{"manifest": [], "integration_data": ["not", "a", "dict"]}`)

	state := StateLoad(cfg)
	if len(state.IntegrationData) != 0 {
		t.Errorf("integration_data = %+v, want empty", state.IntegrationData)
	}
}

func TestStateLoadDropsMalformedIntegrationDataEntries(t *testing.T) {
	cfg := testConfig(t)
	writeText(t, cfg.StatePath,
		`{"manifest": [], "integration_data": {"integration:workday_main": {"user": {"name": "Workday"}}, "integration:other": "not a dict"}}`)

	state := StateLoad(cfg)
	want := map[string]map[string]any{"integration:workday_main": {"user": map[string]any{"name": "Workday"}}}
	if !reflect.DeepEqual(state.IntegrationData, want) {
		t.Errorf("integration_data = %+v, want %+v", state.IntegrationData, want)
	}
}

func TestStateLoadMissingIntegrationDataDefaultsToEmptyDict(t *testing.T) {
	cfg := testConfig(t)
	writeText(t, cfg.StatePath, `{"last_good_sha": "abc123", "manifest": [], "last_apply_utc": null}`)

	state := StateLoad(cfg)
	if state.IntegrationData == nil || len(state.IntegrationData) != 0 {
		t.Errorf("integration_data = %+v, want empty non-nil map", state.IntegrationData)
	}
}

func TestStateLoadSanitizesNonDictIntegrationAttempts(t *testing.T) {
	cfg := testConfig(t)
	writeText(t, cfg.StatePath, `{"manifest": [], "integration_attempts": ["not", "a", "dict"]}`)

	state := StateLoad(cfg)
	if len(state.IntegrationAttempts) != 0 {
		t.Errorf("integration_attempts = %+v, want empty", state.IntegrationAttempts)
	}
}

func TestStateLoadDropsMalformedIntegrationAttemptsEntries(t *testing.T) {
	cfg := testConfig(t)
	writeText(t, cfg.StatePath,
		`{"manifest": [], "integration_attempts": {"integration:esphome_main": {"hash": "abc", "error": "boom"}, "integration:other": "not a dict"}}`)

	state := StateLoad(cfg)
	want := map[string]map[string]any{"integration:esphome_main": {"hash": "abc", "error": "boom"}}
	if !reflect.DeepEqual(state.IntegrationAttempts, want) {
		t.Errorf("integration_attempts = %+v, want %+v", state.IntegrationAttempts, want)
	}
}

func TestStateLoadMissingIntegrationAttemptsDefaultsToEmptyDict(t *testing.T) {
	cfg := testConfig(t)
	writeText(t, cfg.StatePath, `{"last_good_sha": "abc123", "manifest": [], "last_apply_utc": null}`)

	state := StateLoad(cfg)
	if state.IntegrationAttempts == nil || len(state.IntegrationAttempts) != 0 {
		t.Errorf("integration_attempts = %+v, want empty non-nil map", state.IntegrationAttempts)
	}
}

func TestStateSaveThenLoadRoundtripsIntegrationAttempts(t *testing.T) {
	cfg := testConfig(t)
	cfg.StatePath = filepath.Join(t.TempDir(), "state.json")
	want := map[string]map[string]any{"integration:esphome_main": {"hash": "abc123", "error": "domain esphome: flow step 'user' has no declared data"}}

	if err := StateSave(cfg, State{Manifest: []string{}, IntegrationAttempts: want}); err != nil {
		t.Fatalf("StateSave: %v", err)
	}
	loaded := StateLoad(cfg)

	if !reflect.DeepEqual(loaded.IntegrationAttempts, want) {
		t.Errorf("integration_attempts = %+v, want %+v", loaded.IntegrationAttempts, want)
	}
}

func TestStateLoadSanitizesNonDictSubentryManaged(t *testing.T) {
	cfg := testConfig(t)
	writeText(t, cfg.StatePath, `{"manifest": [], "subentry_managed": ["not", "a", "dict"]}`)

	state := StateLoad(cfg)
	if len(state.SubentryManaged) != 0 {
		t.Errorf("subentry_managed = %+v, want empty", state.SubentryManaged)
	}
}

func TestStateLoadDropsMalformedSubentryManagedEntries(t *testing.T) {
	cfg := testConfig(t)
	writeText(t, cfg.StatePath,
		`{"manifest": [], "subentry_managed": {"subentry:calendar_family": "sub-1", "subentry:other": 42}}`)

	state := StateLoad(cfg)
	want := map[string]string{"subentry:calendar_family": "sub-1"}
	if !reflect.DeepEqual(state.SubentryManaged, want) {
		t.Errorf("subentry_managed = %+v, want %+v", state.SubentryManaged, want)
	}
}

func TestStateLoadDropsMalformedSubentryHashesEntries(t *testing.T) {
	cfg := testConfig(t)
	writeText(t, cfg.StatePath,
		`{"manifest": [], "subentry_hashes": {"subentry:calendar_family": "deadbeef", "subentry:other": 42}}`)

	state := StateLoad(cfg)
	want := map[string]string{"subentry:calendar_family": "deadbeef"}
	if !reflect.DeepEqual(state.SubentryHashes, want) {
		t.Errorf("subentry_hashes = %+v, want %+v", state.SubentryHashes, want)
	}
}

func TestStateLoadDropsMalformedSubentryAttemptsEntries(t *testing.T) {
	cfg := testConfig(t)
	writeText(t, cfg.StatePath,
		`{"manifest": [], "subentry_attempts": {"subentry:calendar_family": {"hash": "abc", "error": "boom"}, "subentry:other": "not a dict"}}`)

	state := StateLoad(cfg)
	want := map[string]map[string]any{"subentry:calendar_family": {"hash": "abc", "error": "boom"}}
	if !reflect.DeepEqual(state.SubentryAttempts, want) {
		t.Errorf("subentry_attempts = %+v, want %+v", state.SubentryAttempts, want)
	}
}

func TestStateLoadMissingSubentryFieldsDefaultToEmptyDicts(t *testing.T) {
	cfg := testConfig(t)
	writeText(t, cfg.StatePath, `{"last_good_sha": "abc123", "manifest": [], "last_apply_utc": null}`)

	state := StateLoad(cfg)
	if state.SubentryManaged == nil || len(state.SubentryManaged) != 0 {
		t.Errorf("subentry_managed = %+v, want empty non-nil map", state.SubentryManaged)
	}
	if state.SubentryHashes == nil || len(state.SubentryHashes) != 0 {
		t.Errorf("subentry_hashes = %+v, want empty non-nil map", state.SubentryHashes)
	}
	if state.SubentryAttempts == nil || len(state.SubentryAttempts) != 0 {
		t.Errorf("subentry_attempts = %+v, want empty non-nil map", state.SubentryAttempts)
	}
}

func TestStateSaveThenLoadRoundtripsSubentryFields(t *testing.T) {
	cfg := testConfig(t)
	cfg.StatePath = filepath.Join(t.TempDir(), "state.json")
	managed := map[string]string{"subentry:calendar_family": "01JSUB0000000000000000000"}
	hashes := map[string]string{"subentry:calendar_family": "deadbeefcafe"}
	attempts := map[string]map[string]any{
		"subentry:calendar_zone": {"hash": "abc123", "error": "subentry type 'calendar' rejected field 'calendar_id'"},
	}

	if err := StateSave(cfg, State{
		Manifest: []string{}, SubentryManaged: managed, SubentryHashes: hashes, SubentryAttempts: attempts,
	}); err != nil {
		t.Fatalf("StateSave: %v", err)
	}
	loaded := StateLoad(cfg)

	if !reflect.DeepEqual(loaded.SubentryManaged, managed) {
		t.Errorf("subentry_managed = %+v, want %+v", loaded.SubentryManaged, managed)
	}
	if !reflect.DeepEqual(loaded.SubentryHashes, hashes) {
		t.Errorf("subentry_hashes = %+v, want %+v", loaded.SubentryHashes, hashes)
	}
	if !reflect.DeepEqual(loaded.SubentryAttempts, attempts) {
		t.Errorf("subentry_attempts = %+v, want %+v", loaded.SubentryAttempts, attempts)
	}
}

// --- hacs fields --------------------------------------------------------

func TestStateLoadDropsMalformedHacsManagedEntries(t *testing.T) {
	cfg := testConfig(t)
	writeText(t, cfg.StatePath,
		`{"manifest": [], "hacs_managed": {"hacs:anker_solix": "1234", "hacs:other": 42}}`)

	state := StateLoad(cfg)
	want := map[string]string{"hacs:anker_solix": "1234"}
	if !reflect.DeepEqual(state.HacsManaged, want) {
		t.Errorf("hacs_managed = %+v, want %+v", state.HacsManaged, want)
	}
}

func TestStateLoadDropsMalformedHacsAttemptsEntries(t *testing.T) {
	cfg := testConfig(t)
	writeText(t, cfg.StatePath,
		`{"manifest": [], "hacs_attempts": {"hacs:anker_solix": {"hash": "abc", "error": "boom"}, "hacs:other": "not a dict"}}`)

	state := StateLoad(cfg)
	want := map[string]map[string]any{"hacs:anker_solix": {"hash": "abc", "error": "boom"}}
	if !reflect.DeepEqual(state.HacsAttempts, want) {
		t.Errorf("hacs_attempts = %+v, want %+v", state.HacsAttempts, want)
	}
}

// The one list-shaped field beside the manifest: a malformed entry is
// dropped on its own, and what survives comes back sorted and deduplicated
// for the polled fragment that is compared byte for byte.
func TestStateLoadSanitizesHacsRestartPending(t *testing.T) {
	cfg := testConfig(t)
	writeText(t, cfg.StatePath,
		`{"manifest": [], "hacs_restart_pending": ["zeta", 42, "alpha", "zeta", ""]}`)

	state := StateLoad(cfg)
	want := []string{"alpha", "zeta"}
	if !reflect.DeepEqual(state.HacsRestartPending, want) {
		t.Errorf("hacs_restart_pending = %+v, want %+v", state.HacsRestartPending, want)
	}
}

func TestStateLoadSanitizesNonListHacsRestartPending(t *testing.T) {
	cfg := testConfig(t)
	writeText(t, cfg.StatePath, `{"manifest": [], "hacs_restart_pending": {"not": "a list"}}`)

	state := StateLoad(cfg)
	if state.HacsRestartPending == nil || len(state.HacsRestartPending) != 0 {
		t.Errorf("hacs_restart_pending = %+v, want empty non-nil list", state.HacsRestartPending)
	}
}

func TestStateLoadMissingHacsFieldsDefaultToEmpties(t *testing.T) {
	cfg := testConfig(t)
	writeText(t, cfg.StatePath, `{"last_good_sha": "abc123", "manifest": [], "last_apply_utc": null}`)

	state := StateLoad(cfg)
	if state.HacsManaged == nil || len(state.HacsManaged) != 0 {
		t.Errorf("hacs_managed = %+v, want empty non-nil map", state.HacsManaged)
	}
	if state.HacsAttempts == nil || len(state.HacsAttempts) != 0 {
		t.Errorf("hacs_attempts = %+v, want empty non-nil map", state.HacsAttempts)
	}
	if state.HacsRestartPending == nil || len(state.HacsRestartPending) != 0 {
		t.Errorf("hacs_restart_pending = %+v, want empty non-nil list", state.HacsRestartPending)
	}
}

func TestStateSaveThenLoadRoundtripsHacsFields(t *testing.T) {
	cfg := testConfig(t)
	cfg.StatePath = filepath.Join(t.TempDir(), "state.json")
	managed := map[string]string{"hacs:anker_solix": "1234"}
	attempts := map[string]map[string]any{
		"hacs:broken": {"hash": "abc123", "error": "no release tagged 9.9.9"},
	}

	if err := StateSave(cfg, State{
		Manifest: []string{}, HacsManaged: managed, HacsAttempts: attempts,
		// Written out of order, so the save's own sort is what the load
		// reads back rather than the caller having remembered.
		HacsRestartPending: []string{"zeta", "alpha"},
	}); err != nil {
		t.Fatalf("StateSave: %v", err)
	}
	loaded := StateLoad(cfg)

	if !reflect.DeepEqual(loaded.HacsManaged, managed) {
		t.Errorf("hacs_managed = %+v, want %+v", loaded.HacsManaged, managed)
	}
	if !reflect.DeepEqual(loaded.HacsAttempts, attempts) {
		t.Errorf("hacs_attempts = %+v, want %+v", loaded.HacsAttempts, attempts)
	}
	if !reflect.DeepEqual(loaded.HacsRestartPending, []string{"alpha", "zeta"}) {
		t.Errorf("hacs_restart_pending = %+v, want it sorted", loaded.HacsRestartPending)
	}
}

func TestStateSaveThenLoadRoundtripsCaptureAndConflictFields(t *testing.T) {
	cfg := testConfig(t)
	cfg.StatePath = filepath.Join(t.TempDir(), "state.json")

	if err := StateSave(cfg, State{
		Manifest: []string{},
		// Written out of order, so the save's own sort is what the load
		// reads back rather than the caller having remembered.
		ConflictedPaths:    []string{"scripts.yaml", "automations.yaml"},
		LastConflictBranch: "gitops/conflict-20260806T120000Z",
		LastConflictUTC:    "2026-08-06T12:00:00Z",
		LastCaptureSHA:     "cap123",
		LastCaptureUTC:     "2026-08-06T12:05:00Z",
		LastCapturePaths:   []string{"scenes.yaml", "packages/demo.yaml"},
	}); err != nil {
		t.Fatalf("StateSave: %v", err)
	}
	loaded := StateLoad(cfg)

	if !reflect.DeepEqual(loaded.ConflictedPaths, []string{"automations.yaml", "scripts.yaml"}) {
		t.Errorf("conflicted_paths = %+v, want it sorted", loaded.ConflictedPaths)
	}
	if !reflect.DeepEqual(loaded.LastCapturePaths, []string{"packages/demo.yaml", "scenes.yaml"}) {
		t.Errorf("last_capture_paths = %+v, want it sorted", loaded.LastCapturePaths)
	}
	if loaded.LastConflictBranch != "gitops/conflict-20260806T120000Z" || loaded.LastConflictUTC != "2026-08-06T12:00:00Z" {
		t.Errorf("conflict scalars = %q / %q", loaded.LastConflictBranch, loaded.LastConflictUTC)
	}
	// The merge-base override has to survive a restart, or the first cycle
	// after one reads the agent's own capture as a repository change.
	if loaded.LastCaptureSHA != "cap123" || loaded.LastCaptureUTC != "2026-08-06T12:05:00Z" {
		t.Errorf("capture scalars = %q / %q", loaded.LastCaptureSHA, loaded.LastCaptureUTC)
	}
}

// Both lists steer what an apply may touch and what a capture writes into a
// commit, so they get Manifest's guard, not the looser list sanitizer.
func TestStateLoadDropsUnsafeConflictedAndCapturedPaths(t *testing.T) {
	cfg := testConfig(t)
	cfg.ConfigRoot = filepath.Join(t.TempDir(), "config")
	writeText(t, cfg.StatePath, `{
		"manifest": [],
		"conflicted_paths": ["good.yaml", "../outside.yaml", "/etc/passwd", 42, ""],
		"last_capture_paths": ["kept.yaml", "../../escape.yaml", null]
	}`)

	state := StateLoad(cfg)

	if !reflect.DeepEqual(state.ConflictedPaths, []string{"good.yaml"}) {
		t.Errorf("conflicted_paths = %+v, want [good.yaml]", state.ConflictedPaths)
	}
	if !reflect.DeepEqual(state.LastCapturePaths, []string{"kept.yaml"}) {
		t.Errorf("last_capture_paths = %+v, want [kept.yaml]", state.LastCapturePaths)
	}
}

// A null would come back as a nil slice on the next load, which reads the
// same but makes the file itself ambiguous about whether anything was ever
// recorded.
func TestStateSaveEmitsEmptyPathListsRatherThanNull(t *testing.T) {
	cfg := testConfig(t)
	cfg.StatePath = filepath.Join(t.TempDir(), "state.json")

	if err := StateSave(cfg, State{Manifest: []string{}}); err != nil {
		t.Fatalf("StateSave: %v", err)
	}
	data, err := os.ReadFile(cfg.StatePath) // #nosec G304 -- t.TempDir() fixture path this test wrote
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("state.json does not parse: %v", err)
	}
	for _, field := range []string{"conflicted_paths", "last_capture_paths"} {
		list, ok := raw[field].([]any)
		if !ok {
			t.Errorf("%s = %#v, want an empty list", field, raw[field])
			continue
		}
		if len(list) != 0 {
			t.Errorf("%s = %v, want it empty", field, list)
		}
	}
}

func TestStateLoadDefaultsWhenMissing(t *testing.T) {
	cfg := testConfig(t)
	cfg.StatePath = filepath.Join(t.TempDir(), "does_not_exist.json")

	state := StateLoad(cfg)
	want := State{
		Manifest: []string{}, RegistryManaged: map[string]string{},
		EntityOriginals: map[string]map[string]any{}, DashboardManaged: map[string]string{},
		AddonOriginals: map[string]map[string]any{}, AddonRestartOnChange: map[string]bool{},
		IntegrationManaged: map[string]string{}, IntegrationHashes: map[string]string{},
		IntegrationData: map[string]map[string]any{},
		SubentryManaged: map[string]string{}, SubentryHashes: map[string]string{},
		SubentryAttempts: map[string]map[string]any{},
	}
	if state.LastGoodSHA != want.LastGoodSHA || state.LastApplyUTC != want.LastApplyUTC {
		t.Errorf("state = %+v", state)
	}
	if len(state.Manifest) != 0 || len(state.RegistryManaged) != 0 || len(state.EntityOriginals) != 0 || len(state.DashboardManaged) != 0 ||
		len(state.AddonOriginals) != 0 || len(state.AddonRestartOnChange) != 0 ||
		len(state.IntegrationManaged) != 0 || len(state.IntegrationHashes) != 0 || len(state.IntegrationData) != 0 ||
		len(state.SubentryManaged) != 0 || len(state.SubentryHashes) != 0 || len(state.SubentryAttempts) != 0 {
		t.Errorf("state = %+v", state)
	}
}

func TestStateSaveThenLoadRoundtrip(t *testing.T) {
	cfg := testConfig(t)
	cfg.StatePath = filepath.Join(t.TempDir(), "nested", "state.json")

	err := StateSave(cfg, State{LastGoodSHA: "abc123", Manifest: []string{"a.yaml"}, LastApplyUTC: "2026-01-01T00:00:00Z"})
	if err != nil {
		t.Fatalf("StateSave: %v", err)
	}
	loaded := StateLoad(cfg)

	if loaded.LastGoodSHA != "abc123" || loaded.LastApplyUTC != "2026-01-01T00:00:00Z" {
		t.Errorf("loaded = %+v", loaded)
	}
	if len(loaded.Manifest) != 1 || loaded.Manifest[0] != "a.yaml" {
		t.Errorf("manifest = %+v", loaded.Manifest)
	}
	if len(loaded.RegistryManaged) != 0 {
		t.Errorf("registry_managed = %+v, want empty", loaded.RegistryManaged)
	}
	if len(loaded.EntityOriginals) != 0 {
		t.Errorf("entity_originals = %+v, want empty", loaded.EntityOriginals)
	}
	if len(loaded.DashboardManaged) != 0 {
		t.Errorf("dashboard_managed = %+v, want empty", loaded.DashboardManaged)
	}
	if len(loaded.AddonOriginals) != 0 {
		t.Errorf("addon_originals = %+v, want empty", loaded.AddonOriginals)
	}
	if len(loaded.AddonRestartOnChange) != 0 {
		t.Errorf("addon_restart_on_change = %+v, want empty", loaded.AddonRestartOnChange)
	}
	if len(loaded.IntegrationManaged) != 0 {
		t.Errorf("integration_managed = %+v, want empty", loaded.IntegrationManaged)
	}
	if len(loaded.IntegrationHashes) != 0 {
		t.Errorf("integration_hashes = %+v, want empty", loaded.IntegrationHashes)
	}
	if len(loaded.IntegrationData) != 0 {
		t.Errorf("integration_data = %+v, want empty", loaded.IntegrationData)
	}
	if len(loaded.SubentryManaged) != 0 {
		t.Errorf("subentry_managed = %+v, want empty", loaded.SubentryManaged)
	}
	if len(loaded.SubentryHashes) != 0 {
		t.Errorf("subentry_hashes = %+v, want empty", loaded.SubentryHashes)
	}
	if len(loaded.SubentryAttempts) != 0 {
		t.Errorf("subentry_attempts = %+v, want empty", loaded.SubentryAttempts)
	}
	if loaded.LastDriftBranch != "" {
		t.Errorf("last_drift_branch = %q, want empty", loaded.LastDriftBranch)
	}
	if loaded.LastDriftBackHash != "" {
		t.Errorf("last_drift_back_hash = %q, want empty", loaded.LastDriftBackHash)
	}
	// Atomic write: no leftover .tmp file.
	if _, err := os.Stat(cfg.StatePath + ".tmp"); !os.IsNotExist(err) {
		t.Errorf(".tmp file left behind")
	}
}

func TestStateSaveThenLoadRoundtripsLastDriftBranchAndHash(t *testing.T) {
	cfg := testConfig(t)
	cfg.StatePath = filepath.Join(t.TempDir(), "state.json")

	err := StateSave(cfg, State{
		Manifest: []string{}, LastDriftBranch: "gitops/drift-20260802T120000Z", LastDriftBackHash: "deadbeef",
	})
	if err != nil {
		t.Fatalf("StateSave: %v", err)
	}
	loaded := StateLoad(cfg)

	if loaded.LastDriftBranch != "gitops/drift-20260802T120000Z" {
		t.Errorf("last_drift_branch = %q", loaded.LastDriftBranch)
	}
	if loaded.LastDriftBackHash != "deadbeef" {
		t.Errorf("last_drift_back_hash = %q", loaded.LastDriftBackHash)
	}
}

func TestStateSaveThenLoadRoundtripsLastImportFields(t *testing.T) {
	cfg := testConfig(t)
	cfg.StatePath = filepath.Join(t.TempDir(), "state.json")

	err := StateSave(cfg, State{
		Manifest:      []string{},
		LastImportSHA: "9d3b7c1a5e2f8046b3c9d7e1a2f5084b6c3d9e17",
		LastImportUTC: "2026-08-03T14:12:07Z",
	})
	if err != nil {
		t.Fatalf("StateSave: %v", err)
	}
	loaded := StateLoad(cfg)

	if loaded.LastImportSHA != "9d3b7c1a5e2f8046b3c9d7e1a2f5084b6c3d9e17" {
		t.Errorf("last_import_sha = %q", loaded.LastImportSHA)
	}
	if loaded.LastImportUTC != "2026-08-03T14:12:07Z" {
		t.Errorf("last_import_utc = %q", loaded.LastImportUTC)
	}
}

// TestStateSaveThenLoadNeverGrowsManifestFromAnImport is the persisted half
// of the anti-escalation rule: an import records that it happened, nothing
// more. Ownership for deletion is earned by an apply.
func TestStateSaveThenLoadNeverGrowsManifestFromAnImport(t *testing.T) {
	cfg := testConfig(t)
	cfg.StatePath = filepath.Join(t.TempDir(), "state.json")

	if err := StateSave(cfg, State{Manifest: []string{}, LastImportSHA: "abc123"}); err != nil {
		t.Fatalf("StateSave: %v", err)
	}
	if loaded := StateLoad(cfg); len(loaded.Manifest) != 0 {
		t.Errorf("Manifest = %v, want empty", loaded.Manifest)
	}
}

func TestStateSaveThenLoadRoundtripsDashboardManaged(t *testing.T) {
	cfg := testConfig(t)
	cfg.StatePath = filepath.Join(t.TempDir(), "state.json")
	want := map[string]string{"dashboard:home": "abc123"}

	if err := StateSave(cfg, State{Manifest: []string{}, DashboardManaged: want}); err != nil {
		t.Fatalf("StateSave: %v", err)
	}
	loaded := StateLoad(cfg)

	if !reflect.DeepEqual(loaded.DashboardManaged, want) {
		t.Errorf("dashboard_managed = %+v, want %+v", loaded.DashboardManaged, want)
	}
}

func TestStateSaveThenLoadRoundtripsEntityOriginals(t *testing.T) {
	cfg := testConfig(t)
	cfg.StatePath = filepath.Join(t.TempDir(), "state.json")
	want := map[string]map[string]any{"entity:light.x": {"name": "Original", "icon": nil}}

	if err := StateSave(cfg, State{Manifest: []string{}, EntityOriginals: want}); err != nil {
		t.Fatalf("StateSave: %v", err)
	}
	loaded := StateLoad(cfg)

	if !reflect.DeepEqual(loaded.EntityOriginals, want) {
		t.Errorf("entity_originals = %+v, want %+v", loaded.EntityOriginals, want)
	}
}

func TestStateSaveThenLoadRoundtripsAddonOriginals(t *testing.T) {
	cfg := testConfig(t)
	cfg.StatePath = filepath.Join(t.TempDir(), "state.json")
	want := map[string]map[string]any{"addon:core_configurator": {"dirsfirst": false, "hide_dotfiles": nil}}

	if err := StateSave(cfg, State{Manifest: []string{}, AddonOriginals: want}); err != nil {
		t.Fatalf("StateSave: %v", err)
	}
	loaded := StateLoad(cfg)

	if !reflect.DeepEqual(loaded.AddonOriginals, want) {
		t.Errorf("addon_originals = %+v, want %+v", loaded.AddonOriginals, want)
	}
}

func TestStateSaveThenLoadKeepsAnAbsentAddonOriginalApartFromANull(t *testing.T) {
	// internal/addonopts records "this option key had no value at all" as
	// a marker object rather than null, precisely because this file cannot
	// tell a Go nil from a stored null across a save/load round trip.
	cfg := testConfig(t)
	cfg.StatePath = filepath.Join(t.TempDir(), "state.json")
	want := map[string]map[string]any{
		"addon:a0d7b954_chrony": {
			"log_level": map[string]any{"__ha_gitops_agent_option_absent__": true},
			"mode":      nil,
		},
	}

	if err := StateSave(cfg, State{Manifest: []string{}, AddonOriginals: want}); err != nil {
		t.Fatalf("StateSave: %v", err)
	}
	loaded := StateLoad(cfg)

	if !reflect.DeepEqual(loaded.AddonOriginals, want) {
		t.Errorf("addon_originals = %+v, want %+v", loaded.AddonOriginals, want)
	}
}

func TestStateSaveThenLoadRoundtripsIntegrationManaged(t *testing.T) {
	cfg := testConfig(t)
	cfg.StatePath = filepath.Join(t.TempDir(), "state.json")
	want := map[string]string{"integration:workday_main": "abc123"}

	if err := StateSave(cfg, State{Manifest: []string{}, IntegrationManaged: want}); err != nil {
		t.Fatalf("StateSave: %v", err)
	}
	loaded := StateLoad(cfg)

	if !reflect.DeepEqual(loaded.IntegrationManaged, want) {
		t.Errorf("integration_managed = %+v, want %+v", loaded.IntegrationManaged, want)
	}
}

func TestStateSaveThenLoadRoundtripsIntegrationHashes(t *testing.T) {
	cfg := testConfig(t)
	cfg.StatePath = filepath.Join(t.TempDir(), "state.json")
	want := map[string]string{"integration:workday_main": "deadbeefcafe"}

	if err := StateSave(cfg, State{Manifest: []string{}, IntegrationHashes: want}); err != nil {
		t.Fatalf("StateSave: %v", err)
	}
	loaded := StateLoad(cfg)

	if !reflect.DeepEqual(loaded.IntegrationHashes, want) {
		t.Errorf("integration_hashes = %+v, want %+v", loaded.IntegrationHashes, want)
	}
}

func TestStateSaveThenLoadRoundtripsIntegrationData(t *testing.T) {
	cfg := testConfig(t)
	cfg.StatePath = filepath.Join(t.TempDir(), "state.json")
	want := map[string]map[string]any{"integration:workday_main": {"user": map[string]any{"name": "Workday", "country": "PL"}}}

	if err := StateSave(cfg, State{Manifest: []string{}, IntegrationData: want}); err != nil {
		t.Fatalf("StateSave: %v", err)
	}
	loaded := StateLoad(cfg)

	if !reflect.DeepEqual(loaded.IntegrationData, want) {
		t.Errorf("integration_data = %+v, want %+v", loaded.IntegrationData, want)
	}
}

func TestStateSaveThenLoadRoundtripsAddonRestartOnChange(t *testing.T) {
	cfg := testConfig(t)
	cfg.StatePath = filepath.Join(t.TempDir(), "state.json")
	want := map[string]bool{"addon:core_configurator": false}

	if err := StateSave(cfg, State{Manifest: []string{}, AddonRestartOnChange: want}); err != nil {
		t.Fatalf("StateSave: %v", err)
	}
	loaded := StateLoad(cfg)

	if !reflect.DeepEqual(loaded.AddonRestartOnChange, want) {
		t.Errorf("addon_restart_on_change = %+v, want %+v", loaded.AddonRestartOnChange, want)
	}
}

// --- health probe / reload / restart --------------------------------------

func TestHealthProbeSendsAuthorizationHeader(t *testing.T) {
	t.Setenv("SUPERVISOR_TOKEN", "test-token")
	cfg := testConfig(t)
	repoRoot := t.TempDir()
	configRoot := t.TempDir()
	writeText(t, filepath.Join(repoRoot, "automations.yaml"), "changed\n")
	writeText(t, filepath.Join(configRoot, "automations.yaml"), "original\n")

	changes := []Change{{Path: "automations.yaml", Kind: ChangeUpdate}}
	client := &fakeClient{
		authToken: "test-token",
		postResponses: map[string]jsonResponse{
			"/core/api/config/core/check_config":       {status: 200, body: map[string]any{"result": "valid", "errors": nil}},
			"/core/api/services/homeassistant/restart": {status: 200, body: map[string]any{}},
		},
	}

	result, err := Apply(context.Background(), cfg, changes, repoRoot, configRoot, testOptions("restart"), client)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Without the Authorization header on the probe, Supervisor 401s every
	// attempt, the probe "times out", and a valid apply is rolled back.
	if !result.OK || result.RolledBack {
		t.Fatalf("result = %+v", result)
	}
	if got := readText(t, filepath.Join(configRoot, "automations.yaml")); got != "changed\n" {
		t.Errorf("automations.yaml = %q", got)
	}
	if !hasCall(client.calls, "GET", cfg.Supervisor+"/core/api/") {
		t.Errorf("expected a GET to /core/api/, calls = %+v", client.calls)
	}
}

func TestHealthProbeTimeoutTriggersRollback(t *testing.T) {
	t.Setenv("SUPERVISOR_TOKEN", "test-token")
	cfg := testConfig(t)
	repoRoot := t.TempDir()
	configRoot := t.TempDir()
	writeText(t, filepath.Join(repoRoot, "automations.yaml"), "changed\n")
	writeText(t, filepath.Join(configRoot, "automations.yaml"), "original\n")

	changes := []Change{{Path: "automations.yaml", Kind: ChangeUpdate}}
	client := &fakeClient{
		postResponses: map[string]jsonResponse{
			"/core/api/config/core/check_config":       {status: 200, body: map[string]any{"result": "valid", "errors": nil}},
			"/core/api/services/homeassistant/restart": {status: 200, body: map[string]any{}},
		},
		getResponses: map[string]jsonResponse{"/core/api/": {status: 503, body: map[string]any{}}},
	}

	result, err := Apply(context.Background(), cfg, changes, repoRoot, configRoot, testOptions("restart"), client)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.OK || !result.RolledBack {
		t.Fatalf("result = %+v", result)
	}
	if len(result.Changed) != 0 {
		t.Errorf("changed = %+v", result.Changed)
	}
	if got := readText(t, filepath.Join(configRoot, "automations.yaml")); got != "original\n" {
		t.Errorf("automations.yaml = %q", got)
	}
	if !strings.Contains(result.Error, "health probe") {
		t.Errorf("error = %q", result.Error)
	}
}

func TestReloadSuccessReturnsOKWithoutRollback(t *testing.T) {
	t.Setenv("SUPERVISOR_TOKEN", "test-token")
	cfg := testConfig(t)
	repoRoot := t.TempDir()
	configRoot := t.TempDir()
	writeText(t, filepath.Join(repoRoot, "automations.yaml"), "changed\n")
	writeText(t, filepath.Join(configRoot, "automations.yaml"), "original\n")

	changes := []Change{{Path: "automations.yaml", Kind: ChangeUpdate}}
	client := validCheckConfigClient(map[string]jsonResponse{
		"/core/api/services/homeassistant/reload_all": {status: 200, body: map[string]any{}},
	})

	result, err := Apply(context.Background(), cfg, changes, repoRoot, configRoot, testOptions("reload"), client)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !result.OK || result.RolledBack || result.Error != "" {
		t.Fatalf("result = %+v", result)
	}
	if !hasCall(client.calls, "POST", cfg.Supervisor+"/core/api/services/homeassistant/reload_all") {
		t.Errorf("expected a reload_all call, calls = %+v", client.calls)
	}
}

func TestReloadHealthProbeTimeoutKeepsChangesWithoutRollback(t *testing.T) {
	t.Setenv("SUPERVISOR_TOKEN", "test-token")
	cfg := testConfig(t)
	repoRoot := t.TempDir()
	configRoot := t.TempDir()
	writeText(t, filepath.Join(repoRoot, "automations.yaml"), "changed\n")
	writeText(t, filepath.Join(configRoot, "automations.yaml"), "original\n")

	changes := []Change{{Path: "automations.yaml", Kind: ChangeUpdate}}
	client := &fakeClient{
		postResponses: map[string]jsonResponse{
			"/core/api/config/core/check_config":          {status: 200, body: map[string]any{"result": "valid", "errors": nil}},
			"/core/api/services/homeassistant/reload_all": {status: 200, body: map[string]any{}},
		},
		getResponses: map[string]jsonResponse{"/core/api/": {status: 503, body: map[string]any{}}},
	}

	result, err := Apply(context.Background(), cfg, changes, repoRoot, configRoot, testOptions("reload"), client)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// reload_all is in-process: HA never went down and the config already
	// passed check_config, so a slow probe is no grounds to undo it.
	if !result.OK || result.RolledBack {
		t.Fatalf("result = %+v", result)
	}
	if len(result.Changed) != 1 || result.Changed[0] != "automations.yaml" {
		t.Errorf("changed = %+v", result.Changed)
	}
	if got := readText(t, filepath.Join(configRoot, "automations.yaml")); got != "changed\n" {
		t.Errorf("automations.yaml = %q", got)
	}
	if !strings.Contains(strings.ToLower(result.Error), "healthy") {
		t.Errorf("error = %q", result.Error)
	}
	if !hasCall(client.calls, "POST", cfg.Supervisor+"/core/api/services/homeassistant/reload_all") {
		t.Errorf("expected a reload_all call, calls = %+v", client.calls)
	}
}

// --- rollback directory cleanup -------------------------------------------

func TestRollbackRemovesEmptyDirectoryCreatedByApply(t *testing.T) {
	t.Setenv("SUPERVISOR_TOKEN", "test-token")
	cfg := testConfig(t)
	repoRoot := t.TempDir()
	configRoot := t.TempDir()
	writeText(t, filepath.Join(repoRoot, "packages", "demo.yaml"), "pkg: true\n")

	changes := []Change{{Path: "packages/demo.yaml", Kind: ChangeAdd}}
	client := &fakeClient{postResponses: map[string]jsonResponse{
		"/core/api/config/core/check_config": {status: 200, body: map[string]any{"result": "invalid", "errors": "bad package"}},
	}}

	result, err := Apply(context.Background(), cfg, changes, repoRoot, configRoot, testOptions("off"), client)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.OK || !result.RolledBack {
		t.Fatalf("result = %+v", result)
	}
	if _, err := os.Stat(filepath.Join(configRoot, "packages", "demo.yaml")); !os.IsNotExist(err) {
		t.Errorf("packages/demo.yaml should be removed")
	}
	if _, err := os.Stat(filepath.Join(configRoot, "packages")); !os.IsNotExist(err) {
		t.Errorf("packages/ should be removed")
	}
	if _, err := os.Stat(configRoot); err != nil {
		t.Errorf("config root itself must survive: %v", err)
	}
}

func TestRollbackRemovesNestedEmptyDirsButKeepsPreexistingSiblings(t *testing.T) {
	t.Setenv("SUPERVISOR_TOKEN", "test-token")
	cfg := testConfig(t)
	repoRoot := t.TempDir()
	configRoot := t.TempDir()
	writeText(t, filepath.Join(configRoot, "packages", "keep.yaml"), "existing: true\n")
	writeText(t, filepath.Join(repoRoot, "packages", "sub", "demo.yaml"), "pkg: true\n")

	changes := []Change{{Path: "packages/sub/demo.yaml", Kind: ChangeAdd}}
	client := &fakeClient{postResponses: map[string]jsonResponse{
		"/core/api/config/core/check_config": {status: 200, body: map[string]any{"result": "invalid", "errors": "bad"}},
	}}

	result, err := Apply(context.Background(), cfg, changes, repoRoot, configRoot, testOptions("off"), client)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !result.RolledBack {
		t.Fatalf("result = %+v", result)
	}
	// "packages/sub" was created for this apply and is now empty -> gone.
	if _, err := os.Stat(filepath.Join(configRoot, "packages", "sub")); !os.IsNotExist(err) {
		t.Errorf("packages/sub should be removed")
	}
	// "packages" pre-dates this apply and still holds keep.yaml -> stays.
	if _, err := os.Stat(filepath.Join(configRoot, "packages")); err != nil {
		t.Errorf("packages/ should survive: %v", err)
	}
	if _, err := os.Stat(filepath.Join(configRoot, "packages", "keep.yaml")); err != nil {
		t.Errorf("packages/keep.yaml should survive: %v", err)
	}
}

// --- Config.TransformRepoFile: decrypt on the way into the config -------

// decryptingTransform stands in for sops: "ENC[<x>]" becomes the plaintext
// behind it, and every path it was handed is recorded so a test can prove
// which direction it ran in.
func decryptingTransform(seen *[]string) TransformRepoFileFunc {
	return func(rel string, data []byte) ([]byte, error) {
		if seen != nil {
			*seen = append(*seen, rel)
		}
		text := string(data)
		text = strings.ReplaceAll(text, "ENC[", "")
		text = strings.ReplaceAll(text, "]", "")
		return []byte(text), nil
	}
}

func TestTransformAppliedToAddAndUpdatePreservingMode(t *testing.T) {
	t.Setenv("SUPERVISOR_TOKEN", "test-token")
	cfg := testConfig(t)
	var seen []string
	cfg.TransformRepoFile = decryptingTransform(&seen)
	repoRoot := t.TempDir()
	configRoot := t.TempDir()

	if err := os.WriteFile(filepath.Join(repoRoot, "added.yaml"), []byte("password: ENC[added]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeText(t, filepath.Join(repoRoot, "updated.yaml"), "password: ENC[updated]\n")
	writeText(t, filepath.Join(configRoot, "updated.yaml"), "password: old\n")

	changes := []Change{
		{Path: "added.yaml", Kind: ChangeAdd},
		{Path: "updated.yaml", Kind: ChangeUpdate},
	}

	result, err := Apply(context.Background(), cfg, changes, repoRoot, configRoot, testOptions("off"), validCheckConfigClient(nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.OK || result.RolledBack {
		t.Fatalf("result = %+v", result)
	}
	if got := readText(t, filepath.Join(configRoot, "added.yaml")); got != "password: added\n" {
		t.Errorf("added.yaml = %q, want the transformed content", got)
	}
	if got := readText(t, filepath.Join(configRoot, "updated.yaml")); got != "password: updated\n" {
		t.Errorf("updated.yaml = %q, want the transformed content", got)
	}
	if !reflect.DeepEqual(seen, []string{"added.yaml", "updated.yaml"}) {
		t.Errorf("transform saw %+v, want both repo paths in change order", seen)
	}
	info, err := os.Stat(filepath.Join(configRoot, "added.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("added.yaml mode = %v, want the source file's 0600", info.Mode().Perm())
	}
}

// TestTransformNeverTouchesTheStash: the stash is the live config as it
// was, and the only thing a rollback restores from. Transforming on the way
// in would make it a copy of the repository instead.
func TestTransformNeverTouchesTheStash(t *testing.T) {
	t.Setenv("SUPERVISOR_TOKEN", "test-token")
	cfg := testConfig(t)
	cfg.TransformRepoFile = decryptingTransform(nil)
	repoRoot := t.TempDir()
	configRoot := t.TempDir()
	writeText(t, filepath.Join(repoRoot, "demo.yaml"), "password: ENC[new]\n")
	writeText(t, filepath.Join(configRoot, "demo.yaml"), "password: ENC[live]\n")

	changes := []Change{{Path: "demo.yaml", Kind: ChangeUpdate}}

	result, err := Apply(context.Background(), cfg, changes, repoRoot, configRoot, testOptions("off"), validCheckConfigClient(nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := readText(t, filepath.Join(result.StashDir, "demo.yaml")); got != "password: ENC[live]\n" {
		t.Errorf("stashed copy = %q, want the live bytes verbatim", got)
	}
}

// TestRollbackRestoresUntransformedStash covers the other direction of
// the same rule: the restore replays the stash byte for byte.
func TestRollbackRestoresUntransformedStash(t *testing.T) {
	t.Setenv("SUPERVISOR_TOKEN", "test-token")
	cfg := testConfig(t)
	cfg.TransformRepoFile = decryptingTransform(nil)
	repoRoot := t.TempDir()
	configRoot := t.TempDir()
	writeText(t, filepath.Join(repoRoot, "demo.yaml"), "password: ENC[new]\n")
	writeText(t, filepath.Join(configRoot, "demo.yaml"), "password: ENC[live]\n")

	changes := []Change{{Path: "demo.yaml", Kind: ChangeUpdate}}
	client := &fakeClient{postResponses: map[string]jsonResponse{
		"/core/api/config/core/check_config": {status: 200, body: map[string]any{"result": "invalid", "errors": "bad"}},
	}}

	result, err := Apply(context.Background(), cfg, changes, repoRoot, configRoot, testOptions("off"), client)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.RolledBack {
		t.Fatalf("result = %+v, want a rollback", result)
	}
	if got := readText(t, filepath.Join(configRoot, "demo.yaml")); got != "password: ENC[live]\n" {
		t.Errorf("restored demo.yaml = %q, want the original live bytes", got)
	}
}

// TestTransformFailureMidBatchRollsBackWhatWasWritten: a file that cannot
// be decrypted must not leave the config half-applied, or be written as
// ciphertext.
func TestTransformFailureMidBatchRollsBackWhatWasWritten(t *testing.T) {
	t.Setenv("SUPERVISOR_TOKEN", "test-token")
	cfg := testConfig(t)
	cfg.TransformRepoFile = func(rel string, data []byte) ([]byte, error) {
		if rel == "second.yaml" {
			return nil, errors.New("sops decrypt failed (exit 1)")
		}
		return data, nil
	}
	repoRoot := t.TempDir()
	configRoot := t.TempDir()
	writeText(t, filepath.Join(repoRoot, "first.yaml"), "a: repo\n")
	writeText(t, filepath.Join(configRoot, "first.yaml"), "a: live\n")
	writeText(t, filepath.Join(repoRoot, "second.yaml"), "b: repo\n")
	writeText(t, filepath.Join(configRoot, "second.yaml"), "b: live\n")

	changes := []Change{
		{Path: "first.yaml", Kind: ChangeUpdate},
		{Path: "second.yaml", Kind: ChangeUpdate},
	}

	result, err := Apply(context.Background(), cfg, changes, repoRoot, configRoot, testOptions("off"), validCheckConfigClient(nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.OK || !result.RolledBack {
		t.Fatalf("result = %+v, want a rolled-back failure", result)
	}
	if !strings.Contains(result.Error, "sops decrypt failed") {
		t.Errorf("error = %q, want the transform's own reason", result.Error)
	}
	if got := readText(t, filepath.Join(configRoot, "first.yaml")); got != "a: live\n" {
		t.Errorf("first.yaml = %q, want it rolled back to the live bytes", got)
	}
	if got := readText(t, filepath.Join(configRoot, "second.yaml")); got != "b: live\n" {
		t.Errorf("second.yaml = %q, want it untouched", got)
	}
	if got := readText(t, filepath.Join(result.StashDir, "first.yaml")); got != "a: live\n" {
		t.Errorf("stash first.yaml = %q, want the stash intact after the rollback", got)
	}
	if len(result.Changed) != 0 {
		t.Errorf("changed = %+v, want none", result.Changed)
	}
}

// TestNilTransformIsAPlainCopy pins the default: everything about an
// apply is byte-for-byte what it was before encryption existed.
func TestNilTransformIsAPlainCopy(t *testing.T) {
	t.Setenv("SUPERVISOR_TOKEN", "test-token")
	cfg := testConfig(t)
	repoRoot := t.TempDir()
	configRoot := t.TempDir()
	writeText(t, filepath.Join(repoRoot, "demo.yaml"), "password: ENC[still-here]\n")

	changes := []Change{{Path: "demo.yaml", Kind: ChangeAdd}}

	if _, err := Apply(context.Background(), cfg, changes, repoRoot, configRoot, testOptions("off"), validCheckConfigClient(nil)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := readText(t, filepath.Join(configRoot, "demo.yaml")); got != "password: ENC[still-here]\n" {
		t.Errorf("demo.yaml = %q, want the repo bytes copied verbatim", got)
	}
}
