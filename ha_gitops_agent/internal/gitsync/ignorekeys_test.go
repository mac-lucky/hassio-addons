package gitsync

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/ignorekeys"
)

// z2mSentinel is the literal MQTT password Zigbee2MQTT writes into its own
// configuration.yaml in these fixtures. It must reach no commit, no error
// and no log line.
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

var z2mEntries = []string{
	z2mPath + ":mqtt.server",
	z2mPath + ":mqtt.user",
	z2mPath + ":mqtt.password",
}

// withIgnoredKeys installs ignore_keys entries for one test.
func withIgnoredKeys(t *testing.T, entries ...string) {
	t.Helper()
	if err := SetIgnoredKeys(entries); err != nil {
		t.Fatalf("SetIgnoredKeys(%q): %v", entries, err)
	}
	t.Cleanup(func() { _ = SetIgnoredKeys(nil) })
}

// captureGitsyncLogs routes slog into a buffer for one test.
func captureGitsyncLogs(t *testing.T) func() string {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf.String
}

// allHistory is every commit on every ref of the bare remote, patches
// included: the place a value that reached git at all would show.
func allHistory(t *testing.T, bare string) string {
	t.Helper()
	cmd := exec.Command("git", "--git-dir="+bare, "log", "-p", "--all") // #nosec G204 -- see showAtRef
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git log: %v", err)
	}
	return string(out)
}

func TestSetIgnoredKeysInstallsAndClears(t *testing.T) {
	withIgnoredKeys(t, append(z2mEntries, z2mPath+":mqtt.password", "esphome/node.yaml:wifi.ap.password")...)

	got := IgnoredKeys(z2mPath)
	want := []string{"mqtt.server", "mqtt.user", "mqtt.password"}
	if len(got) != len(want) {
		t.Fatalf("IgnoredKeys = %v, want %v with the repeat dropped", got, want)
	}
	for i := range want {
		if got[i].String() != want[i] {
			t.Errorf("IgnoredKeys[%d] = %s, want %s", i, got[i], want[i])
		}
	}
	if got := IgnoredKeys("esphome/node.yaml"); len(got) != 1 || !slices.Equal(got[0], ignorekeys.KeyPath{"wifi", "ap", "password"}) {
		t.Errorf("IgnoredKeys(esphome/node.yaml) = %v", got)
	}
	if got := IgnoredKeys("configuration.yaml"); got != nil {
		t.Errorf("IgnoredKeys(configuration.yaml) = %v, want nil", got)
	}
	if got := IgnoredKeyFiles(); !slices.Equal(got, []string{"esphome/node.yaml", z2mPath}) {
		t.Errorf("IgnoredKeyFiles = %v", got)
	}
	// Exact paths only, as the entry is written.
	if got := IgnoredKeys("/" + z2mPath); got != nil {
		t.Errorf("IgnoredKeys(/%s) = %v, want nil", z2mPath, got)
	}

	if err := SetIgnoredKeys(nil); err != nil {
		t.Fatalf("SetIgnoredKeys(nil): %v", err)
	}
	if got := IgnoredKeys(z2mPath); got != nil {
		t.Errorf("an emptied ignore_keys still answers %v", got)
	}
}

func TestSetIgnoredKeysRefusesBadEntries(t *testing.T) {
	withUserExclusions(t, "node-red/")
	t.Cleanup(func() { _ = SetIgnoredKeys(nil) })

	for _, tc := range []struct {
		entry, want string
	}{
		{"secrets.yaml:" + z2mSentinel, "secrets file"},
		{"zigbee2mqtt/secret.yaml:" + z2mSentinel, "secrets file"},
		{"esphome/Secrets.YML:" + z2mSentinel, "secrets file"},
		{".storage/lovelace.yaml:" + z2mSentinel, "excluded"},
		{"gitops/addons.yaml:" + z2mSentinel, "excluded"},
		{"node-red/settings.yaml:" + z2mSentinel, "excluded"},
		{".env.yaml:" + z2mSentinel, "excluded"},
		{"zigbee2mqtt/settings.json:" + z2mSentinel, "not YAML"},
		{"/homeassistant/" + z2mPath + ":" + z2mSentinel, "absolute path"},
		{"zigbee2mqtt/../" + z2mPath + ":" + z2mSentinel, `".."`},
		{z2mPath + ":mqtt..password" + z2mSentinel, "empty key segment"},
		{z2mPath + ":" + strings.Repeat("k", maxIgnoreKeyEntryLen) + z2mSentinel, "longer than"},
	} {
		// Second position, behind a good entry, so the index is checked.
		err := SetIgnoredKeys([]string{z2mEntries[0], tc.entry})
		if err == nil {
			t.Errorf("SetIgnoredKeys(%q) = nil, want %q", tc.entry, tc.want)
			continue
		}
		msg := err.Error()
		if !strings.Contains(msg, "ignore_keys entry 2:") || !strings.Contains(msg, tc.want) {
			t.Errorf("SetIgnoredKeys(%q) error = %q, want entry 2 named and %q", tc.entry, msg, tc.want)
		}
		if strings.Contains(msg, z2mSentinel) {
			t.Errorf("SetIgnoredKeys(%q) error quotes the entry: %q", tc.entry, msg)
		}
	}

	tooMany := make([]string, maxIgnoreKeyEntries+1)
	for i := range tooMany {
		tooMany[i] = z2mPath + ":k" + strconv.Itoa(i)
	}
	if err := SetIgnoredKeys(tooMany); err == nil || !strings.Contains(err.Error(), "at most") {
		t.Errorf("SetIgnoredKeys(%d entries) error = %v, want the bound named", len(tooMany), err)
	}

	// A refused list installs nothing, and leaves what was there.
	withIgnoredKeys(t, z2mEntries...)
	if err := SetIgnoredKeys([]string{"secrets.yaml:x"}); err == nil {
		t.Fatal("a secrets file entry was accepted")
	}
	if got := IgnoredKeys(z2mPath); len(got) != 3 {
		t.Errorf("IgnoredKeys after a refused call = %v, want the earlier three", got)
	}
}

// The production failure: every Zigbee2MQTT settings write puts the MQTT
// login back into the live file, and the capture of any real edit to it
// was held back for the literal password. With the keys ignored the edit
// is captured, and the login is not.
func TestCaptureFilesCommitsAnEditWithoutTheIgnoredKeys(t *testing.T) {
	logs := captureGitsyncLogs(t)
	gs, bare, _, configRoot, sha := driftClone(t, map[string]string{z2mPath: z2mRepo})
	ctx := context.Background()
	writeLiveFile(t, configRoot, z2mPath, strings.Replace(z2mLive, "port: 8099", "port: 8100", 1))

	// The premise: without the option the edit is held back.
	result, err := gs.CaptureFiles(ctx, []DriftFile{{Path: z2mPath, Kind: "update"}}, configRoot, sha)
	if err != nil {
		t.Fatalf("CaptureFiles without ignore_keys: %v", err)
	}
	if len(result.HeldBack) != 1 || !slices.Contains(result.HeldBack[0].Keys, "mqtt.password") || result.CommitSHA != "" {
		t.Fatalf("without ignore_keys: %+v, want the file held back for mqtt.password and nothing committed", result)
	}

	withIgnoredKeys(t, z2mEntries...)
	result, err = gs.CaptureFiles(ctx, []DriftFile{{Path: z2mPath, Kind: "update"}}, configRoot, sha)
	if err != nil {
		t.Fatalf("CaptureFiles: %v", err)
	}
	if len(result.HeldBack) != 0 {
		t.Errorf("HeldBack = %+v, want none", result.HeldBack)
	}
	if result.CommitSHA == "" || !slices.Equal(result.Paths, []string{z2mPath}) {
		t.Fatalf("result = %+v, want %s committed", result, z2mPath)
	}

	got, _ := showAtRef(t, bare, "main", z2mPath)
	if !strings.Contains(got, "port: 8100") || !strings.Contains(got, "base_topic: zigbee2mqtt") {
		t.Errorf("committed copy lost the edit or the rest of the file:\n%s", got)
	}
	for _, leaked := range []string{"password", "core-mosquitto", "addons"} {
		if strings.Contains(got, leaked) {
			t.Errorf("committed copy carries the ignored %q:\n%s", leaked, got)
		}
	}
	if strings.Contains(allHistory(t, bare), z2mSentinel) {
		t.Error("the password reached the repository history")
	}
	if strings.Contains(logs(), z2mSentinel) {
		t.Errorf("the password reached the log:\n%s", logs())
	}
}

// A key the repository does hold keeps the repository's value in git:
// the '!secret' reference stays, live's literal never replaces it.
func TestCaptureFilesKeepsTheRepositoryValueOfAnIgnoredKey(t *testing.T) {
	const repo = "mqtt:\n  base_topic: zigbee2mqtt\n  password: '!secret mqtt_password'\nadvanced:\n  log_level: info\n"
	gs, bare, _, configRoot, sha := driftClone(t, map[string]string{z2mPath: repo})
	withIgnoredKeys(t, z2mEntries...)
	writeLiveFile(t, configRoot, z2mPath,
		"mqtt:\n  base_topic: zigbee2mqtt\n  password: "+z2mSentinel+"\nadvanced:\n  log_level: debug\n")

	result, err := gs.CaptureFiles(context.Background(), []DriftFile{{Path: z2mPath, Kind: "update"}}, configRoot, sha)
	if err != nil {
		t.Fatalf("CaptureFiles: %v", err)
	}
	if result.CommitSHA == "" || len(result.HeldBack) != 0 {
		t.Fatalf("result = %+v, want a commit and nothing held back", result)
	}
	got, _ := showAtRef(t, bare, "main", z2mPath)
	want := "mqtt:\n  base_topic: zigbee2mqtt\n  password: '!secret mqtt_password'\nadvanced:\n  log_level: debug\n"
	if got != want {
		t.Errorf("committed copy =\n%s\nwant\n%s", got, want)
	}
	if strings.Contains(allHistory(t, bare), z2mSentinel) {
		t.Error("the password reached the repository history")
	}
}

// Every other write to git goes through the same copy: a commit-back
// branch, a conflict copy and an import.
func TestOtherWritesToGitLeaveTheIgnoredKeysOut(t *testing.T) {
	edited := strings.Replace(z2mLive, "log_level: info", "log_level: debug", 1)

	t.Run("commit back", func(t *testing.T) {
		gs, bare, _, configRoot, sha := driftClone(t, map[string]string{z2mPath: z2mRepo})
		withIgnoredKeys(t, z2mEntries...)
		writeLiveFile(t, configRoot, z2mPath, edited)
		branch, held, err := gs.CommitBack(context.Background(), []DriftFile{{Path: z2mPath, Kind: "update"}}, configRoot, sha, fixedDriftTime)
		if err != nil || len(held) != 0 {
			t.Fatalf("CommitBack = %q, %+v, %v; want a branch and nothing held back", branch, held, err)
		}
		if got, _ := showAtRef(t, bare, branch, z2mPath); !strings.Contains(got, "log_level: debug") {
			t.Errorf("drift branch copy =\n%s\nwant the edit", got)
		}
		if strings.Contains(allHistory(t, bare), z2mSentinel) {
			t.Error("the password reached the repository history")
		}
	})

	t.Run("conflict copy", func(t *testing.T) {
		gs, bare, _, configRoot, sha := driftClone(t, map[string]string{z2mPath: z2mRepo})
		withIgnoredKeys(t, z2mEntries...)
		writeLiveFile(t, configRoot, z2mPath, edited)
		branch, held, err := gs.ParkConflicts(context.Background(), []DriftFile{{Path: z2mPath, Kind: "update"}}, configRoot, sha, fixedDriftTime)
		if err != nil || len(held) != 0 || branch == "" {
			t.Fatalf("ParkConflicts = %q, %+v, %v; want a branch and nothing held back", branch, held, err)
		}
		if strings.Contains(allHistory(t, bare), z2mSentinel) {
			t.Error("the password reached the repository history")
		}
	})

	t.Run("import", func(t *testing.T) {
		gs, bare, _, configRoot, _ := driftClone(t, map[string]string{z2mPath: z2mRepo})
		withIgnoredKeys(t, z2mEntries...)
		writeLiveFile(t, configRoot, z2mPath, edited)
		res, err := gs.Import(context.Background(), configRoot, generousLimits(), fixedImportTime)
		if err != nil || len(res.HeldBack) != 0 {
			t.Fatalf("Import = %+v, %v; want it to land with nothing held back", res, err)
		}
		if got, _ := showAtRef(t, bare, "main", z2mPath); !strings.Contains(got, "log_level: debug") {
			t.Errorf("imported copy =\n%s\nwant the edit", got)
		}
		if strings.Contains(allHistory(t, bare), z2mSentinel) {
			t.Error("the password reached the repository history")
		}
	})
}

// The classifier asks whether live still says what the base says. A live
// copy the app only rewrote must answer yes, or an apply of a repository
// change to the file would read as a conflict.
func TestLiveFactsAtSetsTheIgnoredKeysAside(t *testing.T) {
	gs, _, _, configRoot, sha := driftClone(t, map[string]string{z2mPath: z2mRepo})
	ctx := context.Background()
	writeLiveFile(t, configRoot, z2mPath, z2mLive)

	facts, err := gs.LiveFactsAt(ctx, sha, configRoot, z2mPath)
	if err != nil {
		t.Fatalf("LiveFactsAt: %v", err)
	}
	if facts.MatchesBase {
		t.Error("MatchesBase = true without ignore_keys, want the byte compare to say no")
	}

	withIgnoredKeys(t, z2mEntries...)
	if facts, err = gs.LiveFactsAt(ctx, sha, configRoot, z2mPath); err != nil || !facts.MatchesBase {
		t.Errorf("LiveFactsAt = %+v, %v; want MatchesBase with the login and the indentation set aside", facts, err)
	}
	if equivalent, tracked, err := gs.BlobEquivalent(ctx, sha, z2mPath, []byte(z2mLive)); err != nil || !equivalent || !tracked {
		t.Errorf("BlobEquivalent = %v, %v, %v; want equivalent and tracked", equivalent, tracked, err)
	}

	writeLiveFile(t, configRoot, z2mPath, strings.Replace(z2mLive, "log_level: info", "log_level: debug", 1))
	if facts, err = gs.LiveFactsAt(ctx, sha, configRoot, z2mPath); err != nil || facts.MatchesBase {
		t.Errorf("LiveFactsAt = %+v, %v; want a real edit to count", facts, err)
	}
}

// A live file whose ignored keys cannot be set aside is held back, never
// written to git as it is: the literal-secret check only catches
// secret-shaped keys, and an ignored mqtt.user would go through it in the
// clear. The sentinel here sits under mqtt.user for that reason.
func TestCaptureFilesHoldsBackALiveFileWhoseIgnoredKeysCannotBeSetAside(t *testing.T) {
	user := "  user: " + z2mSentinel + "\n"
	for _, tc := range []struct {
		name, live, reason string
	}{
		{"broken YAML", "mqtt:\n" + user + " bad: [\n", "does not parse"},
		{"invalid UTF-8", "mqtt:\n" + user + "  base_topic: z\xff\n", "does not parse"},
		{"two documents", "advanced: {}\n---\nmqtt:\n" + user, "does not parse"},
		{"an anchored value", "mqtt:\n  user: &u " + z2mSentinel + "\nother: *u\n", "anchor"},
		{"a repeated parent", "mqtt:\n  base_topic: zigbee2mqtt\nmqtt:\n" + user, "repeats a key"},
		{"a repeated leaf", "mqtt:\n" + user + "  base_topic: zigbee2mqtt\n" + user, "repeats a key"},
		{"an alias on the path", "creds: &c\n" + user + "mqtt: *c\n", "repeats a key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureGitsyncLogs(t)
			gs, bare, _, configRoot, sha := driftClone(t, map[string]string{z2mPath: z2mRepo})
			withIgnoredKeys(t, z2mEntries...)
			writeLiveFile(t, configRoot, z2mPath, tc.live)

			result, err := gs.CaptureFiles(context.Background(), []DriftFile{{Path: z2mPath, Kind: "update"}}, configRoot, sha)
			if err != nil {
				t.Fatalf("CaptureFiles: %v", err)
			}
			if result.CommitSHA != "" || len(result.HeldBack) != 1 {
				t.Fatalf("result = %+v, want the file held back and nothing committed", result)
			}
			held := result.HeldBack[0]
			if held.Path != z2mPath || !slices.Equal(held.Keys, []string{"mqtt.server", "mqtt.user", "mqtt.password"}) ||
				!strings.Contains(held.Reason, "ignore_keys could not be set aside") || !strings.Contains(held.Reason, tc.reason) {
				t.Errorf("held back = %+v, want %s named by its ignored keys, the reason saying %q", held, z2mPath, tc.reason)
			}
			for _, published := range []string{held.String(), HeldBackSummary(result.HeldBack), HeldBackCause(result.HeldBack), HeldBackAdviceFor(result.HeldBack)} {
				if strings.Contains(published, z2mSentinel) || strings.Contains(published, "literal value") {
					t.Errorf("held-back text %q quotes the file or calls it a literal secret", published)
				}
			}
			if strings.Contains(allHistory(t, bare), z2mSentinel) {
				t.Error("the ignored value reached the repository history")
			}
			logged := logs()
			if !strings.Contains(logged, z2mPath) || !strings.Contains(logged, "ignore_keys could not be set aside") {
				t.Errorf("log = %q, want the file and why named", logged)
			}
			if strings.Contains(logged, z2mSentinel) {
				t.Errorf("the log quotes the file: %q", logged)
			}
		})
	}
}

// The same refusal on every other write to git: a commit-back branch whose
// only file was refused is not made at all.
func TestCommitBackHoldsBackALiveFileWhoseIgnoredKeysCannotBeSetAside(t *testing.T) {
	gs, bare, _, configRoot, sha := driftClone(t, map[string]string{z2mPath: z2mRepo})
	withIgnoredKeys(t, z2mEntries...)
	writeLiveFile(t, configRoot, z2mPath, "mqtt:\n  user: "+z2mSentinel+"\n  user: again\n")

	_, held, err := gs.CommitBack(context.Background(), []DriftFile{{Path: z2mPath, Kind: "update"}}, configRoot, sha, fixedDriftTime)
	if !errors.Is(err, ErrAllHeldBack) || len(held) != 1 {
		t.Fatalf("CommitBack = %+v, %v; want everything held back", held, err)
	}
	if strings.Contains(err.Error(), z2mSentinel) || !strings.Contains(err.Error(), "ignore_keys") {
		t.Errorf("error = %q, want the reason and no value", err)
	}
	if strings.Contains(allHistory(t, bare), z2mSentinel) {
		t.Error("the ignored value reached the repository history")
	}
}

// A repository copy that does not parse cannot supply values, so the
// ignored keys are removed from what is committed rather than taken from
// live.
func TestCaptureFilesRemovesTheKeysWhenTheRepositoryCopyDoesNotParse(t *testing.T) {
	gs, bare, _, configRoot, sha := driftClone(t, map[string]string{z2mPath: "mqtt: [\n"})
	withIgnoredKeys(t, z2mEntries...)
	writeLiveFile(t, configRoot, z2mPath, z2mLive)

	result, err := gs.CaptureFiles(context.Background(), []DriftFile{{Path: z2mPath, Kind: "update"}}, configRoot, sha)
	if err != nil || result.CommitSHA == "" || len(result.HeldBack) != 0 {
		t.Fatalf("CaptureFiles = %+v, %v; want a commit and nothing held back", result, err)
	}
	if got, _ := showAtRef(t, bare, "main", z2mPath); strings.Contains(got, "password") || !strings.Contains(got, "base_topic") {
		t.Errorf("committed copy =\n%s\nwant live without the login", got)
	}
}
