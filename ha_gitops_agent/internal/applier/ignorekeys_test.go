package applier

import (
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/gitsync"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/ignorekeys"
)

// z2mSentinel is the literal MQTT password Zigbee2MQTT persists into its
// own configuration.yaml. It belongs in the live file and nowhere else.
const z2mSentinel = "S3ntinel-mqtt-pw-7f2c"

const z2mPath = "zigbee2mqtt/configuration.yaml"

// z2mRepo is the file as the repository holds it: four-space indentation
// and no MQTT login, which the add-on supplies through its environment.
const z2mRepo = `homeassistant:
    enabled: true
frontend:
    enabled: true
    port: 8099
mqtt:
    base_topic: zigbee2mqtt
advanced:
    log_level: info
`

// z2mLive is the same file after a Zigbee2MQTT settings write: two-space
// indentation and the login persisted from the environment.
const z2mLive = `homeassistant:
  enabled: true
frontend:
  enabled: true
  port: 8099
mqtt:
  base_topic: zigbee2mqtt
  server: mqtt://core-mosquitto:1883
  user: addons
  password: ` + z2mSentinel + `
advanced:
  log_level: info
`

var z2mLogin = []ignorekeys.KeyPath{{"mqtt", "server"}, {"mqtt", "user"}, {"mqtt", "password"}}

// withZ2MLoginIgnored installs the option the way main does, so the test
// also covers DefaultConfig's wiring to it.
func withZ2MLoginIgnored(t *testing.T) {
	t.Helper()
	if err := gitsync.SetIgnoredKeys([]string{z2mPath + ":mqtt.server", z2mPath + ":mqtt.user", z2mPath + ":mqtt.password"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = gitsync.SetIgnoredKeys(nil) })
}

func captureApplierLogs(t *testing.T) func() string {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf.String
}

// Applying a repository change to the file must not strip the login the
// app wrote into it: the next Zigbee2MQTT start would have none.
func TestApplyKeepsTheLiveValuesOfIgnoredKeys(t *testing.T) {
	t.Setenv("SUPERVISOR_TOKEN", "test-token")
	withZ2MLoginIgnored(t)
	cfg := testConfig(t)
	repoRoot, configRoot := t.TempDir(), t.TempDir()
	repo := strings.Replace(z2mRepo, "log_level: info", "log_level: debug", 1)
	writeText(t, filepath.Join(repoRoot, z2mPath), repo)
	writeText(t, filepath.Join(configRoot, z2mPath), z2mLive)

	result, err := Apply(context.Background(), cfg, []Change{{Path: z2mPath, Kind: ChangeUpdate}},
		repoRoot, configRoot, testOptions("off"), validCheckConfigClient(nil))
	if err != nil || !result.OK {
		t.Fatalf("Apply = %+v, %v", result, err)
	}

	got := readText(t, filepath.Join(configRoot, z2mPath))
	for _, want := range []string{"log_level: debug", "server: mqtt://core-mosquitto:1883", "user: addons", "password: " + z2mSentinel} {
		if !strings.Contains(got, want) {
			t.Errorf("live file lacks %q:\n%s", want, got)
		}
	}
	// And it is what the next comparison calls in sync with the repository,
	// or every cycle would apply it again.
	if equal, err := ignorekeys.Equal(z2mLogin, []byte(got), []byte(repo)); err != nil || !equal {
		t.Errorf("the applied file does not compare equal to the repository (%v):\n%s", err, got)
	}
	// The stash holds live as it was, login included, for a rollback.
	if stashed := readText(t, filepath.Join(result.StashDir, z2mPath)); stashed != z2mLive {
		t.Errorf("stashed copy = %q, want the live file byte for byte", stashed)
	}
}

// A rollback puts back what was live before, ignored keys and all.
func TestRollbackRestoresTheIgnoredKeysByteForByte(t *testing.T) {
	t.Setenv("SUPERVISOR_TOKEN", "test-token")
	withZ2MLoginIgnored(t)
	cfg := testConfig(t)
	repoRoot, configRoot := t.TempDir(), t.TempDir()
	writeText(t, filepath.Join(repoRoot, z2mPath), strings.Replace(z2mRepo, "port: 8099", "port: 8100", 1))
	writeText(t, filepath.Join(configRoot, z2mPath), z2mLive)
	client := &fakeClient{postResponses: map[string]jsonResponse{
		"/core/api/config/core/check_config": {status: 200, body: map[string]any{"result": "invalid", "errors": "bad"}},
	}}

	result, err := Apply(context.Background(), cfg, []Change{{Path: z2mPath, Kind: ChangeUpdate}},
		repoRoot, configRoot, testOptions("off"), client)
	if err != nil || result.OK || !result.RolledBack {
		t.Fatalf("Apply = %+v, %v; want a rollback", result, err)
	}
	if got := readText(t, filepath.Join(configRoot, z2mPath)); got != z2mLive {
		t.Errorf("after the rollback the live file = %q, want it byte for byte as it was", got)
	}

	// A manual rollback of a successful apply does the same.
	writeText(t, filepath.Join(configRoot, z2mPath), z2mLive)
	result, err = Apply(context.Background(), cfg, []Change{{Path: z2mPath, Kind: ChangeUpdate}},
		repoRoot, configRoot, testOptions("off"), validCheckConfigClient(nil))
	if err != nil || !result.OK {
		t.Fatalf("Apply = %+v, %v", result, err)
	}
	if rb := RollbackFrom(cfg, result.StashDir, configRoot); !rb.OK {
		t.Fatalf("RollbackFrom = %+v", rb)
	}
	if got := readText(t, filepath.Join(configRoot, z2mPath)); got != z2mLive {
		t.Errorf("after RollbackFrom the live file = %q, want it byte for byte as it was", got)
	}
}

// A file not live yet has no values to keep, so an add leaves every
// ignored key out - the file written is what the add's published diff
// showed, and what the next comparison calls in sync.
func TestApplyOfANewFileLeavesTheIgnoredKeysOut(t *testing.T) {
	t.Setenv("SUPERVISOR_TOKEN", "test-token")
	withZ2MLoginIgnored(t)
	cfg := testConfig(t)
	repoRoot, configRoot := t.TempDir(), t.TempDir()
	repo := strings.Replace(z2mRepo, "    base_topic: zigbee2mqtt\n", "    base_topic: zigbee2mqtt\n    user: repo-user\n", 1)
	writeText(t, filepath.Join(repoRoot, z2mPath), repo)

	result, err := Apply(context.Background(), cfg, []Change{{Path: z2mPath, Kind: ChangeAdd}},
		repoRoot, configRoot, testOptions("off"), validCheckConfigClient(nil))
	if err != nil || !result.OK {
		t.Fatalf("Apply = %+v, %v", result, err)
	}
	got := readText(t, filepath.Join(configRoot, z2mPath))
	if strings.Contains(got, "repo-user") || !strings.Contains(got, "base_topic: zigbee2mqtt") {
		t.Errorf("live file =\n%s\nwant the repository copy without its ignored key", got)
	}
	if equal, err := ignorekeys.Equal(z2mLogin, []byte(got), []byte(repo)); err != nil || !equal {
		t.Errorf("the new file does not compare equal to the repository (%v):\n%s", err, got)
	}
}

// A live copy that cannot be read as YAML has no values to keep: the
// repository's copy is written as it is, as without the option - which
// strips what the app wrote until it writes it again. The warning names
// the file and never quotes it.
func TestApplyWritesTheRepositoryCopyAsItIsOverALiveFileThatDoesNotParse(t *testing.T) {
	t.Setenv("SUPERVISOR_TOKEN", "test-token")
	withZ2MLoginIgnored(t)
	logs := captureApplierLogs(t)
	cfg := testConfig(t)
	for _, live := range []string{
		"mqtt:\n  password: " + z2mSentinel + "\n bad: [\n",
		"mqtt:\n  user: " + z2mSentinel + "\nmqtt:\n  base_topic: x\n",
	} {
		repoRoot, configRoot := t.TempDir(), t.TempDir()
		writeText(t, filepath.Join(repoRoot, z2mPath), z2mRepo)
		writeText(t, filepath.Join(configRoot, z2mPath), live)
		result, err := Apply(context.Background(), cfg, []Change{{Path: z2mPath, Kind: ChangeUpdate}},
			repoRoot, configRoot, testOptions("off"), validCheckConfigClient(nil))
		if err != nil || !result.OK {
			t.Fatalf("Apply = %+v, %v", result, err)
		}
		if got := readText(t, filepath.Join(configRoot, z2mPath)); got != z2mRepo {
			t.Errorf("live file = %q, want the repository copy as it is", got)
		}
	}
	logged := logs()
	if !strings.Contains(logged, "ignore_keys not applied") || !strings.Contains(logged, z2mPath) {
		t.Errorf("log = %q, want a warning naming the file", logged)
	}
	if strings.Contains(logged, z2mSentinel) {
		t.Errorf("the warning quotes the file: %q", logged)
	}
}

// cfg.IgnoredKeys nil is the option off, whatever the global holds: the
// seam a test uses to opt out.
func TestApplyWithoutAnIgnoredKeysFuncCopiesAsBefore(t *testing.T) {
	t.Setenv("SUPERVISOR_TOKEN", "test-token")
	withZ2MLoginIgnored(t)
	cfg := testConfig(t)
	cfg.IgnoredKeys = nil
	repoRoot, configRoot := t.TempDir(), t.TempDir()
	writeText(t, filepath.Join(repoRoot, z2mPath), z2mRepo)
	writeText(t, filepath.Join(configRoot, z2mPath), z2mLive)

	result, err := Apply(context.Background(), cfg, []Change{{Path: z2mPath, Kind: ChangeUpdate}},
		repoRoot, configRoot, testOptions("off"), validCheckConfigClient(nil))
	if err != nil || !result.OK {
		t.Fatalf("Apply = %+v, %v", result, err)
	}
	if got := readText(t, filepath.Join(configRoot, z2mPath)); got != z2mRepo {
		t.Errorf("live file = %q, want the repository copy as it is", got)
	}
}
