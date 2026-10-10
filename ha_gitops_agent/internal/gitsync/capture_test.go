package gitsync

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/execx"
)

// --- the happy path -------------------------------------------------------

func TestCaptureFilesCommitsOntoTheTrackedBranchAndRestoresTheCheckout(t *testing.T) {
	gs, bare, _, configRoot, sha := driftClone(t, map[string]string{
		"automations.yaml": "- id: demo\n",
		"scripts.yaml":     "greet: {}\n",
	})
	ctx := context.Background()

	const liveAutomations = "- id: demo\n  alias: Edited in the HA UI\n"
	const liveScripts = "greet:\n  sequence: []\n"
	writeLiveFile(t, configRoot, "automations.yaml", liveAutomations)
	writeLiveFile(t, configRoot, "scripts.yaml", liveScripts)

	result, err := gs.CaptureFiles(ctx, []DriftFile{
		{Path: "automations.yaml", Kind: "update"},
		{Path: "scripts.yaml", Kind: "update"},
	}, configRoot, sha)
	if err != nil {
		t.Fatalf("CaptureFiles: %v", err)
	}

	if result.CommitSHA == "" {
		t.Fatal("CommitSHA is empty, want the capture commit")
	}
	if result.BaseSHA != sha {
		t.Errorf("BaseSHA = %q, want the fetched tip %q", result.BaseSHA, sha)
	}
	want := []string{"automations.yaml", "scripts.yaml"}
	if !slices.Equal(result.Paths, want) {
		t.Errorf("Paths = %v, want %v", result.Paths, want)
	}
	if tip := remoteTip(t, bare); tip != result.CommitSHA {
		t.Errorf("main is at %q, want the capture commit %q", tip, result.CommitSHA)
	}

	if got, _ := showAtRef(t, bare, "main", "automations.yaml"); got != liveAutomations {
		t.Errorf("automations.yaml on main = %q, want the live copy", got)
	}
	if got, _ := showAtRef(t, bare, "main", "scripts.yaml"); got != liveScripts {
		t.Errorf("scripts.yaml on main = %q, want the live copy", got)
	}

	// The capture lands on the tracked branch itself: no throwaway ref is
	// left behind on the remote.
	for _, name := range listRemoteBranches(t, bare) {
		if name != "main" {
			t.Errorf("remote grew an extra branch %q, want main only", name)
		}
	}
	// And the applier that runs next must read the tip it was given, not the
	// commit this just made.
	if got := gs.CurrentSHA(ctx); got != sha {
		t.Errorf("CurrentSHA() after CaptureFiles = %q, want the detached checkout restored to %q", got, sha)
	}
}

// "--only -- <paths...>" is what makes the one-commit guarantee true even
// when an earlier operation's best-effort restore left the worktree dirty.
func TestCaptureFilesCommitsOnlyTheStagedPaths(t *testing.T) {
	gs, bare, _, configRoot, _ := driftClone(t, map[string]string{
		"automations.yaml": "- id: demo\n",
		"scripts.yaml":     "greet: {}\n",
	})
	ctx := context.Background()

	// Stands in for a failed restore: a tracked file modified in the
	// worktree that no caller asked to capture.
	leftover := filepath.Join(gs.Workdir, "automations.yaml")
	if err := os.WriteFile(leftover, []byte("- id: LEFTOVER\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	writeLiveFile(t, configRoot, "scripts.yaml", "greet:\n  sequence: []\n")
	result, err := gs.CaptureFiles(ctx, []DriftFile{{Path: "scripts.yaml", Kind: "update"}}, configRoot, "")
	if err != nil {
		t.Fatalf("CaptureFiles: %v", err)
	}
	if !slices.Equal(result.Paths, []string{"scripts.yaml"}) {
		t.Errorf("Paths = %v, want only scripts.yaml", result.Paths)
	}

	got, ok := showAtRef(t, bare, "main", "automations.yaml")
	if !ok {
		t.Fatal("automations.yaml vanished from main")
	}
	if got != "- id: demo\n" {
		t.Errorf("automations.yaml on main = %q, want the untouched original: a dirty index rode onto the tracked branch", got)
	}
}

func TestCaptureFilesStagesARemovalForAGenuinelyDeletedLiveFile(t *testing.T) {
	gs, bare, _, configRoot, _ := driftClone(t, map[string]string{
		"automations.yaml": "- id: demo\n",
		"scripts.yaml":     "greet: {}\n",
	})
	ctx := context.Background()

	// scripts.yaml never written live: differ reports that as "add".
	writeLiveFile(t, configRoot, "automations.yaml", "- id: demo\n")

	result, err := gs.CaptureFiles(ctx, []DriftFile{{Path: "scripts.yaml", Kind: "add"}}, configRoot, "")
	if err != nil {
		t.Fatalf("CaptureFiles: %v", err)
	}
	if !slices.Equal(result.Paths, []string{"scripts.yaml"}) {
		t.Errorf("Paths = %v, want scripts.yaml staged as a removal", result.Paths)
	}
	if _, ok := showAtRef(t, bare, "main", "scripts.yaml"); ok {
		t.Error("scripts.yaml still on main, want the live deletion captured")
	}
	if _, ok := showAtRef(t, bare, "main", "automations.yaml"); !ok {
		t.Error("automations.yaml gone from main, want it untouched")
	}
}

// differ reports EVERY stat failure as "add", so an unreadable live file is
// not a deletion. Getting this wrong deletes a file from the repository and
// then, next cycle, from the box.
func TestCaptureFilesDoesNotRemoveAnUnreadableLiveFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission bits do not apply")
	}
	gs, bare, _, configRoot, _ := driftClone(t, map[string]string{
		"locked/still-there.yaml": "- id: repo\n",
	})
	ctx := context.Background()

	writeLiveFile(t, configRoot, "locked/still-there.yaml", "- id: live\n")
	locked := filepath.Join(configRoot, "locked")
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Skipf("chmod unavailable: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o750) })

	result, err := gs.CaptureFiles(ctx, []DriftFile{{Path: "locked/still-there.yaml", Kind: "add"}}, configRoot, "")
	if err != nil {
		t.Fatalf("CaptureFiles: %v", err)
	}
	if len(result.Paths) != 0 {
		t.Errorf("Paths = %v, want nothing staged for a file that is still there", result.Paths)
	}
	if result.CommitSHA != "" {
		t.Errorf("CommitSHA = %q, want no commit at all", result.CommitSHA)
	}
	kept, ok := showAtRef(t, bare, "main", "locked/still-there.yaml")
	if !ok {
		t.Fatal("locked/still-there.yaml was removed from main - an unreadable live file is not a deletion")
	}
	if kept != "- id: repo\n" {
		t.Errorf("content on main = %q, want the repository's own untouched version", kept)
	}
}

// --- push races -----------------------------------------------------------

func TestCaptureFilesRetriesOnceWhenTheBranchMovedUnderIt(t *testing.T) {
	gs, bare, work, configRoot, _ := driftClone(t, map[string]string{"automations.yaml": "- id: demo\n"})
	ctx := context.Background()

	const live = "- id: demo\n  alias: Edited in the HA UI\n"
	writeLiveFile(t, configRoot, "automations.yaml", live)

	competing := 0
	gs.Runner = &racingRunner{inner: execx.CommandRunner{}, races: 1, onPush: func() {
		competing++
		commitFile(t, work, "packages/user.yaml", "user: 1\n", "user commit")
	}}

	result, err := gs.CaptureFiles(ctx, []DriftFile{{Path: "automations.yaml", Kind: "update"}}, configRoot, "")
	if err != nil {
		t.Fatalf("CaptureFiles: %v", err)
	}
	if result.CommitSHA == "" {
		t.Error("CommitSHA is empty, want the retry to have landed the capture")
	}
	if competing != 1 {
		t.Fatalf("competing pushes = %d, want 1", competing)
	}
	if got, _ := showAtRef(t, bare, "main", "automations.yaml"); got != live {
		t.Errorf("automations.yaml on main = %q, want the live copy", got)
	}
	// The point of the fast-forward-only push: the winning commit survives.
	if _, ok := showAtRef(t, bare, "main", "packages/user.yaml"); !ok {
		t.Error("the competing user commit is gone from main, want it preserved")
	}
}

func TestCaptureFilesGivesUpAfterASecondRejection(t *testing.T) {
	gs, bare, work, configRoot, sha := driftClone(t, map[string]string{"automations.yaml": "- id: demo\n"})
	ctx := context.Background()

	writeLiveFile(t, configRoot, "automations.yaml", "- id: demo\n  alias: Edited in the HA UI\n")

	competing := 0
	gs.Runner = &racingRunner{inner: execx.CommandRunner{}, races: 2, onPush: func() {
		competing++
		commitFile(t, work, "packages/user.yaml", strings.Repeat("user: 1\n", competing), "user commit")
	}}

	result, err := gs.CaptureFiles(ctx, []DriftFile{{Path: "automations.yaml", Kind: "update"}}, configRoot, "")
	if err == nil || !strings.Contains(err.Error(), "moved on the remote twice") {
		t.Fatalf("error = %v, want it to report losing the race twice", err)
	}
	if result.CommitSHA != "" {
		t.Errorf("CommitSHA = %q, want nothing reported as captured", result.CommitSHA)
	}
	if competing != 2 {
		t.Errorf("competing pushes = %d, want 2 (one per attempt)", competing)
	}
	if got, _ := showAtRef(t, bare, "main", "automations.yaml"); got != "- id: demo\n" {
		t.Errorf("automations.yaml on main = %q, want the original: nothing should have landed", got)
	}
	gs.Runner = execx.CommandRunner{}
	if got := gs.CurrentSHA(ctx); got != sha {
		t.Errorf("CurrentSHA() after a failed capture = %q, want the checkout restored to %q", got, sha)
	}
}

// The claim a capture makes is "live is the truth RIGHT NOW", so a retry has
// to re-read live rather than replay the tree the first attempt built.
func TestCaptureFilesRetryRestagesFromLive(t *testing.T) {
	gs, bare, work, configRoot, _ := driftClone(t, map[string]string{"automations.yaml": "- id: demo\n"})
	ctx := context.Background()

	const firstLive = "- id: demo\n  alias: First edit\n"
	const secondLive = "- id: demo\n  alias: Second edit, made while the push was losing\n"
	writeLiveFile(t, configRoot, "automations.yaml", firstLive)

	gs.Runner = &racingRunner{inner: execx.CommandRunner{}, races: 1, onPush: func() {
		commitFile(t, work, "packages/user.yaml", "user: 1\n", "user commit")
		writeLiveFile(t, configRoot, "automations.yaml", secondLive)
	}}

	if _, err := gs.CaptureFiles(ctx, []DriftFile{{Path: "automations.yaml", Kind: "update"}}, configRoot, ""); err != nil {
		t.Fatalf("CaptureFiles: %v", err)
	}

	got, ok := showAtRef(t, bare, "main", "automations.yaml")
	if !ok {
		t.Fatal("automations.yaml missing from main")
	}
	if got != secondLive {
		t.Errorf("automations.yaml on main = %q, want the SECOND live edit %q: the retry replayed a stale tree", got, secondLive)
	}
}

// --- guards ---------------------------------------------------------------

func TestCaptureFilesRefusesExcludedAndSecretShapedPaths(t *testing.T) {
	tests := []struct {
		name string
		path string
	}{
		{"excluded storage", ".storage/core.entity_registry"},
		{"secret shaped", "secrets.yaml"},
		{"private key", "id_rsa"},
		{"agent manifests", "gitops/registries.yaml"},
		{"escapes the root", "../outside.yaml"},
		{"absolute", "/etc/passwd"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gs, bare, _, configRoot, _ := driftClone(t, map[string]string{"automations.yaml": "- id: demo\n"})
			writeLiveFile(t, configRoot, "automations.yaml", "- id: live\n")

			if _, err := gs.CaptureFiles(context.Background(), []DriftFile{{Path: tc.path, Kind: "update"}}, configRoot, ""); err == nil {
				t.Errorf("CaptureFiles(%q) error = nil, want a refusal", tc.path)
			}
			if got, _ := showAtRef(t, bare, "main", "automations.yaml"); got != "- id: demo\n" {
				t.Error("main moved despite the refusal")
			}
		})
	}
}

// A gitignored path is skipped rather than fatal, and must not appear in
// Paths - a caller that recorded it would treat a merge base as covering a
// path the commit never carried.
func TestCaptureFilesSkipsGitignoredPathsAndOmitsThemFromResultPaths(t *testing.T) {
	gs, bare, _, configRoot, _ := driftClone(t, map[string]string{
		".gitignore":       "www/community/\n",
		"automations.yaml": "- id: demo\n",
	})
	ctx := context.Background()

	const live = "- id: demo\n  alias: Edited in the HA UI\n"
	writeLiveFile(t, configRoot, "automations.yaml", live)
	writeLiveFile(t, configRoot, "www/community/card.js", "console.log('card');\n")

	result, err := gs.CaptureFiles(ctx, []DriftFile{
		{Path: "automations.yaml", Kind: "update"},
		{Path: "www/community/card.js", Kind: "add"},
	}, configRoot, "")
	if err != nil {
		t.Fatalf("CaptureFiles: %v", err)
	}
	if !slices.Equal(result.Paths, []string{"automations.yaml"}) {
		t.Errorf("Paths = %v, want the gitignored path left out", result.Paths)
	}
	if _, ok := showAtRef(t, bare, "main", "www/community/card.js"); ok {
		t.Error("the gitignored path landed on main")
	}
	if got, _ := showAtRef(t, bare, "main", "automations.yaml"); got != live {
		t.Errorf("automations.yaml on main = %q, want the live copy captured alongside the skip", got)
	}
}

// A capture must never put a plaintext secret on the tracked branch: a
// file with a literal under a secret-shaped key is held back, named with
// its key paths only, and the rest of the batch still lands. References -
// HA's !secret tag and Zigbee2MQTT's '!secret key' strings - are fine.
func TestCaptureFilesHoldsBackLiteralSecrets(t *testing.T) {
	gs, bare, _, configRoot, _ := driftClone(t, map[string]string{
		"automations.yaml":               "- id: demo\n",
		"esphome/node.yaml":              "wifi:\n  password: !secret wifi_password\n",
		"zigbee2mqtt/configuration.yaml": "mqtt:\n  server: mqtt://old\n",
		"settings.json":                  "{\"name\": \"x\"}\n",
	})
	ctx := context.Background()

	writeLiveFile(t, configRoot, "automations.yaml", "- id: edited\n")
	writeLiveFile(t, configRoot, "esphome/node.yaml", "wifi:\n  password: LEAKEDWIFI\n")
	writeLiveFile(t, configRoot, "zigbee2mqtt/configuration.yaml",
		"mqtt:\n  server: mqtt://new\n  password: '!secret.yaml password'\n")
	writeLiveFile(t, configRoot, "settings.json", "{\"name\": \"x\", \"api\": {\"token\": \"LEAKEDTOKEN\"}}\n")

	result, err := gs.CaptureFiles(ctx, []DriftFile{
		{Path: "automations.yaml", Kind: "update"},
		{Path: "esphome/node.yaml", Kind: "update"},
		{Path: "settings.json", Kind: "update"},
		{Path: "zigbee2mqtt/configuration.yaml", Kind: "update"},
	}, configRoot, "")
	if err != nil {
		t.Fatalf("CaptureFiles: %v", err)
	}
	if want := []string{"automations.yaml", "zigbee2mqtt/configuration.yaml"}; !slices.Equal(result.Paths, want) {
		t.Errorf("Paths = %v, want %v", result.Paths, want)
	}
	want := []HeldBack{
		{Path: "esphome/node.yaml", Keys: []string{"wifi.password"}},
		{Path: "settings.json", Keys: []string{"api.token"}},
	}
	if len(result.HeldBack) != len(want) {
		t.Fatalf("HeldBack = %+v, want %+v", result.HeldBack, want)
	}
	for i := range want {
		if result.HeldBack[i].Path != want[i].Path || !slices.Equal(result.HeldBack[i].Keys, want[i].Keys) {
			t.Errorf("HeldBack[%d] = %+v, want %+v", i, result.HeldBack[i], want[i])
		}
		if strings.Contains(result.HeldBack[i].String(), "LEAKED") {
			t.Errorf("HeldBack[%d] renders a value: %q", i, result.HeldBack[i].String())
		}
	}

	for path, secret := range map[string]string{"esphome/node.yaml": "LEAKEDWIFI", "settings.json": "LEAKEDTOKEN"} {
		got, _ := showAtRef(t, bare, "main", path)
		if strings.Contains(got, secret) {
			t.Fatalf("%s on main carries the literal secret: %q", path, got)
		}
	}
	if got, _ := showAtRef(t, bare, "main", "zigbee2mqtt/configuration.yaml"); !strings.Contains(got, "'!secret.yaml password'") {
		t.Errorf("zigbee2mqtt/configuration.yaml on main = %q, want the reference captured", got)
	}
}

// Every file held back: no commit, no error, the list still reported.
func TestCaptureFilesWithEverythingHeldBackCommitsNothing(t *testing.T) {
	gs, bare, _, configRoot, sha := driftClone(t, map[string]string{"esphome/node.yaml": "api:\n  key: !secret api_key\n"})
	writeLiveFile(t, configRoot, "esphome/node.yaml", "api:\n  key: LEAKED\n")

	result, err := gs.CaptureFiles(context.Background(), []DriftFile{{Path: "esphome/node.yaml", Kind: "update"}}, configRoot, "")
	if err != nil {
		t.Fatalf("CaptureFiles: %v", err)
	}
	if result.CommitSHA != "" {
		t.Errorf("CommitSHA = %q, want no commit", result.CommitSHA)
	}
	if len(result.HeldBack) != 1 || result.HeldBack[0].Path != "esphome/node.yaml" {
		t.Errorf("HeldBack = %+v, want esphome/node.yaml", result.HeldBack)
	}
	if got := headSHA(t, bare, "main"); got != sha {
		t.Errorf("main moved to %s, want it left at %s", got, sha)
	}
}

// A secrets file at any depth is never captured, whatever a caller asks.
func TestCaptureFilesRefusesSecretsFilesAtAnyDepth(t *testing.T) {
	gs, _, _, configRoot, _ := driftClone(t, map[string]string{"automations.yaml": "- id: demo\n"})
	for _, p := range []string{"secrets.yaml", "esphome/secrets.yaml", "zigbee2mqtt/secret.yaml", "zigbee2mqtt/secret.yml"} {
		writeLiveFile(t, configRoot, p, "x: 1\n")
		_, err := gs.CaptureFiles(context.Background(), []DriftFile{{Path: p, Kind: "update"}}, configRoot, "")
		if err == nil || !strings.Contains(err.Error(), "excluded/secret-shaped") {
			t.Errorf("CaptureFiles(%s) error = %v, want the excluded/secret-shaped refusal", p, err)
		}
	}
}

func TestCaptureFilesRefusesWithNothingToCapture(t *testing.T) {
	gs, _, _, configRoot, _ := driftClone(t, map[string]string{"automations.yaml": "- id: demo\n"})

	if _, err := gs.CaptureFiles(context.Background(), nil, configRoot, ""); err == nil {
		t.Error("CaptureFiles(nil) error = nil, want a refusal")
	}
}

// SIGTERM cancels the cycle's ctx mid-capture. The restore used to run on
// that same ctx, which cannot start a process once cancelled, and left the
// worktree on the capture branch for the apply that runs on regardless.
func TestCaptureFilesRestoresTheCheckoutWhenCancelledMidway(t *testing.T) {
	gs, _, _, configRoot, sha := driftClone(t, map[string]string{"automations.yaml": "- id: demo\n"})
	writeLiveFile(t, configRoot, "automations.yaml", "- id: demo\n  alias: Edited\n")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	gs.Runner = &racingRunner{inner: execx.CommandRunner{}, races: 1, onPush: cancel}

	if _, err := gs.CaptureFiles(ctx, []DriftFile{{Path: "automations.yaml", Kind: "update"}}, configRoot, ""); err == nil {
		t.Fatal("CaptureFiles succeeded with its ctx cancelled before the push")
	}

	if got := gs.CurrentSHA(context.Background()); got != sha {
		t.Errorf("CurrentSHA() = %q, want the detached checkout restored to %q", got, sha)
	}
}

// A push to a captured path after the caller classified it: the capture's
// own fetch sees a newer tip, and a fast-forward onto it would have
// reverted that push without any rejection to notice.
func TestCaptureFilesRefusesAPathTheRemoteChangedSinceClassification(t *testing.T) {
	gs, bare, work, configRoot, sha := driftClone(t, map[string]string{
		"automations.yaml": "- id: demo\n",
		"scripts.yaml":     "greet: {}\n",
	})
	writeLiveFile(t, configRoot, "automations.yaml", "- id: demo\n  alias: Edited live\n")
	writeLiveFile(t, configRoot, "scripts.yaml", "greet:\n  sequence: []\n")
	const pushed = "- id: demo\n  alias: Pushed meanwhile\n"
	commitFile(t, work, "automations.yaml", pushed, "user commit")

	result, err := gs.CaptureFiles(context.Background(), []DriftFile{
		{Path: "automations.yaml", Kind: "update"},
		{Path: "scripts.yaml", Kind: "update"},
	}, configRoot, sha)
	if err != nil {
		t.Fatalf("CaptureFiles: %v", err)
	}

	if !slices.Equal(result.Refused, []string{"automations.yaml"}) {
		t.Errorf("Refused = %v, want [automations.yaml]", result.Refused)
	}
	if !slices.Equal(result.Paths, []string{"scripts.yaml"}) {
		t.Errorf("Paths = %v, want only the untouched scripts.yaml captured", result.Paths)
	}
	if got, _ := showAtRef(t, bare, "main", "automations.yaml"); got != pushed {
		t.Errorf("automations.yaml on main = %q, want the pushed commit kept", got)
	}
}

// The same race, lost at the push and seen only by the retry's fetch.
func TestCaptureFilesRetryRefusesAPathTheRacingPushChanged(t *testing.T) {
	gs, bare, work, configRoot, sha := driftClone(t, map[string]string{"automations.yaml": "- id: demo\n"})
	writeLiveFile(t, configRoot, "automations.yaml", "- id: demo\n  alias: Edited live\n")
	const pushed = "- id: demo\n  alias: Pushed meanwhile\n"
	gs.Runner = &racingRunner{inner: execx.CommandRunner{}, races: 1, onPush: func() {
		commitFile(t, work, "automations.yaml", pushed, "user commit")
	}}

	result, err := gs.CaptureFiles(context.Background(), []DriftFile{{Path: "automations.yaml", Kind: "update"}}, configRoot, sha)
	if err != nil {
		t.Fatalf("CaptureFiles: %v", err)
	}

	if result.CommitSHA != "" || !slices.Equal(result.Refused, []string{"automations.yaml"}) {
		t.Errorf("result = %+v, want no commit and automations.yaml refused", result)
	}
	if got, _ := showAtRef(t, bare, "main", "automations.yaml"); got != pushed {
		t.Errorf("automations.yaml on main = %q, want the pushed commit kept", got)
	}
}
