package differ

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/gitsync"
)

// z2mSentinel is the literal MQTT password Zigbee2MQTT persists into its
// own configuration.yaml. No published diff may show it.
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

// withZ2MLoginIgnored installs the three ignore_keys entries for one test.
func withZ2MLoginIgnored(t *testing.T) {
	t.Helper()
	if err := gitsync.SetIgnoredKeys([]string{z2mPath + ":mqtt.server", z2mPath + ":mqtt.user", z2mPath + ":mqtt.password"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = gitsync.SetIgnoredKeys(nil) })
}

// captureDifferLogs routes slog into a buffer for one test.
func captureDifferLogs(t *testing.T) func() string {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf.String
}

// assertNoLoginShown fails if a published diff shows any part of the
// ignored login.
func assertNoLoginShown(t *testing.T, diff string) {
	t.Helper()
	for _, shown := range []string{z2mSentinel, "core-mosquitto", "user: addons", "password"} {
		if strings.Contains(diff, shown) {
			t.Errorf("diff shows %q:\n%s", shown, diff)
		}
	}
}

// The production case: the app rewrote its file with the login in it and
// its own indentation, and nothing else changed. That is no drift.
func TestAFileDifferingOnlyInIgnoredKeysIsInSync(t *testing.T) {
	repoRoot, configRoot := dirs(t)
	write(t, repoRoot, z2mPath, []byte(z2mRepo))
	write(t, configRoot, z2mPath, []byte(z2mLive))

	// The premise: without the option this is drift, and its diff is the
	// masked one.
	changes, _ := Compute(repoRoot, configRoot, []string{z2mPath}, []string{z2mPath})
	if len(changes) != 1 {
		t.Fatalf("without ignore_keys: changes = %+v, want the file drifting", changes)
	}
	if strings.Contains(changes[0].DiffText, z2mSentinel) {
		t.Fatalf("the masked diff leaked the password:\n%s", changes[0].DiffText)
	}

	withZ2MLoginIgnored(t)
	if changes, _ := Compute(repoRoot, configRoot, []string{z2mPath}, []string{z2mPath}); len(changes) != 0 {
		t.Errorf("changes = %+v, want none: only the ignored keys and the indentation differ", changes)
	}
}

// A real edit in the same file is still drift, and its diff shows the edit
// and none of the login.
func TestARealEditIsDiffedWithoutTheIgnoredKeys(t *testing.T) {
	withZ2MLoginIgnored(t)
	repoRoot, configRoot := dirs(t)
	write(t, repoRoot, z2mPath, []byte(z2mRepo))
	write(t, configRoot, z2mPath, []byte(strings.Replace(z2mLive, "port: 8099", "port: 8100", 1)))

	changes, _ := Compute(repoRoot, configRoot, []string{z2mPath}, nil)
	if len(changes) != 1 || changes[0].Kind != "update" {
		t.Fatalf("changes = %+v, want one update", changes)
	}
	diff := changes[0].DiffText
	if !strings.Contains(diff, "-  port: 8100") || !strings.Contains(diff, "+  port: 8099") {
		t.Errorf("diff does not show the edit:\n%s", diff)
	}
	assertNoLoginShown(t, diff)
}

// An add quotes the repository's copy and a delete the live one; neither
// shows an ignored key either.
func TestAddAndDeleteDiffsLeaveTheIgnoredKeysOut(t *testing.T) {
	withZ2MLoginIgnored(t)

	repoRoot, configRoot := dirs(t)
	write(t, repoRoot, z2mPath, []byte(strings.Replace(z2mLive, "log_level: info", "log_level: debug", 1)))
	changes, _ := Compute(repoRoot, configRoot, []string{z2mPath}, nil)
	if len(changes) != 1 || changes[0].Kind != "add" {
		t.Fatalf("changes = %+v, want one add", changes)
	}
	if !strings.Contains(changes[0].DiffText, "+  log_level: debug") {
		t.Errorf("add diff does not show the file:\n%s", changes[0].DiffText)
	}
	assertNoLoginShown(t, changes[0].DiffText)

	repoRoot, configRoot = dirs(t)
	write(t, configRoot, z2mPath, []byte(z2mLive))
	changes, _ = Compute(repoRoot, configRoot, nil, []string{z2mPath})
	if len(changes) != 1 || changes[0].Kind != "delete" {
		t.Fatalf("changes = %+v, want one delete", changes)
	}
	if !strings.Contains(changes[0].DiffText, "-  base_topic: zigbee2mqtt") {
		t.Errorf("delete diff does not show the file:\n%s", changes[0].DiffText)
	}
	assertNoLoginShown(t, changes[0].DiffText)
}

// Only the files with entries change behaviour: another file is still
// compared byte for byte, so reindenting it is drift as before.
func TestFilesWithoutEntriesAreStillComparedByteForByte(t *testing.T) {
	withZ2MLoginIgnored(t)
	repoRoot, configRoot := dirs(t)
	write(t, repoRoot, "configuration.yaml", []byte("homeassistant:\n    name: Home\n"))
	write(t, configRoot, "configuration.yaml", []byte("homeassistant:\n  name: Home\n"))

	if changes, _ := Compute(repoRoot, configRoot, []string{"configuration.yaml"}, nil); len(changes) != 1 {
		t.Errorf("changes = %+v, want the reindented file reported", changes)
	}
}

// A copy whose keys cannot be set aside is compared byte for byte, as
// without the option, but no diff of it is published: the ignored keys
// need not be secret-shaped for the masking to catch them - mqtt.user, or
// a login inside mqtt.server. The warning names the file only.
func TestAFileWhoseIgnoredKeysCannotBeSetAsidePublishesNoDiff(t *testing.T) {
	login := "  server: mqtt://addons:" + z2mSentinel + "@core-mosquitto\n  user: " + z2mSentinel + "\n"
	for _, tc := range []struct {
		name, live string
	}{
		{"broken YAML", "mqtt:\n" + login + " bad: [\n"},
		{"two documents", "a: 1\n---\nmqtt:\n" + login},
		{"a repeated parent", "mqtt:\n  base_topic: zigbee2mqtt\nmqtt:\n" + login},
		{"an alias on the path", "creds: &c\n" + login + "mqtt: *c\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureDifferLogs(t)
			withZ2MLoginIgnored(t)

			repoRoot, configRoot := dirs(t)
			write(t, repoRoot, z2mPath, []byte(z2mRepo))
			write(t, configRoot, z2mPath, []byte(tc.live))
			changes, _ := Compute(repoRoot, configRoot, []string{z2mPath}, nil)
			if len(changes) != 1 || changes[0].DiffText != ignoreKeysSummary {
				t.Fatalf("update: changes = %+v, want one with the summary in place of a diff", changes)
			}

			repoRoot, configRoot = dirs(t)
			write(t, configRoot, z2mPath, []byte(tc.live))
			changes, _ = Compute(repoRoot, configRoot, nil, []string{z2mPath})
			if len(changes) != 1 || changes[0].DiffText != ignoreKeysSummary {
				t.Fatalf("delete: changes = %+v, want one with the summary in place of a diff", changes)
			}

			repoRoot, configRoot = dirs(t)
			write(t, repoRoot, z2mPath, []byte(tc.live))
			changes, _ = Compute(repoRoot, configRoot, []string{z2mPath}, nil)
			if len(changes) != 1 || changes[0].DiffText != ignoreKeysSummary {
				t.Fatalf("add: changes = %+v, want one with the summary in place of a diff", changes)
			}

			logged := logs()
			if !strings.Contains(logged, "ignore_keys not applied") || !strings.Contains(logged, z2mPath) {
				t.Errorf("log = %q, want a warning naming the file", logged)
			}
			if strings.Contains(logged, z2mSentinel) {
				t.Errorf("the warning quotes the file: %q", logged)
			}
		})
	}

	// Byte for byte still decides: identical copies are no change.
	withZ2MLoginIgnored(t)
	repoRoot, configRoot := dirs(t)
	broken := "mqtt:\n" + login + " bad: [\n"
	write(t, repoRoot, z2mPath, []byte(broken))
	write(t, configRoot, z2mPath, []byte(broken))
	if changes, _ := Compute(repoRoot, configRoot, []string{z2mPath}, nil); len(changes) != 0 {
		t.Errorf("changes = %+v, want none for identical copies", changes)
	}
}

// A file with entries is set aside at any size: the applier and the
// three-way classifier do, and a chunk compare that did not would plan the
// same apply every cycle.
func TestALargeFileWithEntriesIsStillComparedWithoutTheIgnoredKeys(t *testing.T) {
	withLargeFileThreshold(t, 16)
	withZ2MLoginIgnored(t)
	repoRoot, configRoot := dirs(t)
	write(t, repoRoot, z2mPath, []byte(z2mRepo))
	write(t, configRoot, z2mPath, []byte(z2mLive))

	if changes, _ := Compute(repoRoot, configRoot, []string{z2mPath}, nil); len(changes) != 0 {
		t.Errorf("changes = %+v, want none: only the ignored keys and the indentation differ", changes)
	}

	write(t, configRoot, z2mPath, []byte(strings.Replace(z2mLive, "log_level: info", "log_level: debug", 1)))
	changes, _ := Compute(repoRoot, configRoot, []string{z2mPath}, nil)
	if len(changes) != 1 || !strings.Contains(changes[0].DiffText, "log_level: debug") {
		t.Fatalf("changes = %+v, want a real diff rather than a size summary", changes)
	}
	assertNoLoginShown(t, changes[0].DiffText)

	// A file without entries keeps the chunk compare.
	write(t, repoRoot, "configuration.yaml", []byte("homeassistant:\n    name: Home\n"))
	write(t, configRoot, "configuration.yaml", []byte("homeassistant:\n  name: Home\n"))
	changes, _ = Compute(repoRoot, configRoot, []string{"configuration.yaml"}, nil)
	if len(changes) != 1 || !strings.Contains(changes[0].DiffText, "large file changed") {
		t.Errorf("changes = %+v, want the size summary for a file without entries", changes)
	}
}
