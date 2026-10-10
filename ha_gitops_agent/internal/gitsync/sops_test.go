package gitsync

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// sopsFixture is a fetched clone of a remote holding files, nothing checked
// out: GuardSecretsAt runs before any checkout.
func sopsFixture(t *testing.T, files map[string]string) (*GitSync, string, []string) {
	t.Helper()
	tmp := t.TempDir()
	bare, work := makeRemote(t, tmp, "remote")
	for p, content := range files {
		commitFile(t, work, p, content, "add "+p)
	}
	gs := New(makeOpts("file://"+bare), filepath.Join(tmp, "clone"))
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
	return gs, sha, raw
}

const (
	sopsYAML = "mqtt:\n    password: ENC[AES256_GCM,data:Zm9v,iv:YmFy,tag:YmF6,type:str]\n" +
		"sops:\n    age:\n        - recipient: age1test\n    lastmodified: \"2026-08-01T00:00:00Z\"\n" +
		"    mac: ENC[AES256_GCM,data:bWFj,iv:aXY=,tag:dGFn,type:str]\n    version: 3.9.0\n"
	// Metadata only, no encrypted value: still a SOPS document.
	sopsJSONMetaOnly = "{\n  \"broker\": \"x\",\n  \"sops\": {\"mac\": \"m\", \"lastmodified\": \"2026-08-01T00:00:00Z\"}\n}\n"
	sopsDotenv       = "name=heater\nkey=ENC[AES256_GCM,data:a,iv:b,tag:c,type:str]\nsops_mac=x\nsops_version=3.9.0\n"
	sopsDotenvFlat   = "name=heater\nsops_lastmodified=2026-08-01T00:00:00Z\n"
	sopsINI          = "[db]\npassword = ENC[AES256_GCM,data:a,iv:b,tag:c,type:str]\n[sops]\nmac = x\n"
)

// Every shape SOPS writes is a hard stop, named by path and never by
// content, and the error says what to do.
func TestGuardSecretsAtRefusesSOPSFiles(t *testing.T) {
	gs, sha, raw := sopsFixture(t, map[string]string{
		"automations.yaml":                    "- id: demo\n",
		"packages/mqtt.yaml":                  sopsYAML,
		"zigbee2mqtt/coordinator_backup.json": sopsJSONMetaOnly,
		"wmbusmeters/meter-1":                 sopsDotenv,
		"wmbusmeters/meter-2":                 sopsDotenvFlat,
		"custom/app.ini":                      sopsINI,
		"secrets.yaml":                        sopsYAML,
	})

	err := gs.GuardSecretsAt(context.Background(), sha, raw)
	var sops *SopsTrackedError
	if !errors.As(err, &sops) {
		t.Fatalf("GuardSecretsAt() error = %v, want *SopsTrackedError", err)
	}
	want := []string{"custom/app.ini", "packages/mqtt.yaml", "secrets.yaml", "wmbusmeters/meter-1", "wmbusmeters/meter-2", "zigbee2mqtt/coordinator_backup.json"}
	if !slices.Equal(sops.Files, want) {
		t.Errorf("Files = %v, want %v", sops.Files, want)
	}
	// An encrypted secrets.yaml is reported once, as SOPS: that error is
	// the one that says how to get it out.
	var secrets *SecretsTrackedError
	if errors.As(err, &secrets) {
		t.Errorf("also a SecretsTrackedError %v, want the encrypted secrets.yaml reported as SOPS only", secrets.Files)
	}
	msg := err.Error()
	for _, leak := range []string{"Zm9v", "ENC[", "age1test"} {
		if strings.Contains(msg, leak) {
			t.Errorf("error carries file content %q: %s", leak, msg)
		}
	}
	for _, need := range []string{"1Password", "!secret", ".sops.yaml"} {
		if !strings.Contains(msg, need) {
			t.Errorf("error = %q, want it to mention %q", msg, need)
		}
	}
}

// A plaintext secrets file and a SOPS file together: both reported.
func TestGuardSecretsAtReportsBothKinds(t *testing.T) {
	gs, sha, raw := sopsFixture(t, map[string]string{
		"esphome/secrets.yaml": "wifi_password: plain\n",
		"packages/mqtt.yaml":   sopsYAML,
	})
	err := gs.GuardSecretsAt(context.Background(), sha, raw)
	var sops *SopsTrackedError
	var secrets *SecretsTrackedError
	if !errors.As(err, &sops) || !errors.As(err, &secrets) {
		t.Fatalf("GuardSecretsAt() error = %v, want both error types", err)
	}
	if !slices.Equal(secrets.Files, []string{"esphome/secrets.yaml"}) || !slices.Equal(sops.Files, []string{"packages/mqtt.yaml"}) {
		t.Errorf("secrets = %v, sops = %v", secrets.Files, sops.Files)
	}
}

// The candidate search is broad on purpose; the parse is what decides. A
// config that merely talks about sops, or nests a key called sops, syncs.
func TestGuardSecretsAtIgnoresConfigThatOnlyMentionsSops(t *testing.T) {
	gs, sha, raw := sopsFixture(t, map[string]string{
		"README.md":           "We used to use sops.\n",
		"packages/tools.yaml": "shell_command:\n  sops: echo sops\n",
		"custom/x.json":       "{\"tools\": {\"sops\": {\"enabled\": true}}}\n",
		"notes.yaml":          "sops:\n  enabled: true\n",
		"automations.yaml":    "- id: demo\n",
	})
	if err := gs.GuardSecretsAt(context.Background(), sha, raw); err != nil {
		t.Errorf("GuardSecretsAt() error = %v, want nil", err)
	}
}

// Only the paths the caller passed are judged, which is what lets the raw
// list (and nothing filtered) be the guard's input.
func TestGuardSecretsAtJudgesOnlyTheGivenFiles(t *testing.T) {
	gs, sha, _ := sopsFixture(t, map[string]string{
		"automations.yaml":   "- id: demo\n",
		"packages/mqtt.yaml": sopsYAML,
	})
	if err := gs.GuardSecretsAt(context.Background(), sha, []string{"automations.yaml"}); err != nil {
		t.Errorf("GuardSecretsAt() error = %v, want nil", err)
	}
}

func TestIsSopsDocument(t *testing.T) {
	for _, tc := range []struct {
		name string
		data string
		want bool
	}{
		{"yaml", sopsYAML, true},
		{"json metadata only", sopsJSONMetaOnly, true},
		{"dotenv", sopsDotenv, true},
		{"dotenv flat metadata", sopsDotenvFlat, true},
		{"ini", sopsINI, true},
		{"an encrypted value alone", "token: ENC[AES256_GCM,data:x]\n", true},
		{"yaml sops key without mac", "sops:\n  version: 3\n", false},
		{"nested sops key", "a:\n  sops:\n    mac: x\n", false},
		{"plain", "a: 1\n", false},
		{"not parseable", "a: [\n", false},
	} {
		if got := isSopsDocument([]byte(tc.data)); got != tc.want {
			t.Errorf("%s: isSopsDocument = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// "*.yaml -diff" in the tree's .gitattributes makes git call the file
// binary, and git grep -I would then skip it: the ciphertext must still be
// found.
func TestGuardSecretsAtSeesThroughABinaryAttribute(t *testing.T) {
	gs, sha, raw := sopsFixture(t, map[string]string{
		".gitattributes":     "*.yaml -diff\n*.json binary\n",
		"packages/mqtt.yaml": sopsYAML,
		"custom/x.json":      sopsJSONMetaOnly,
	})
	err := gs.GuardSecretsAt(context.Background(), sha, raw)
	var sops *SopsTrackedError
	if !errors.As(err, &sops) {
		t.Fatalf("GuardSecretsAt() error = %v, want *SopsTrackedError", err)
	}
	if want := []string{"custom/x.json", "packages/mqtt.yaml"}; !slices.Equal(sops.Files, want) {
		t.Errorf("Files = %v, want %v", sops.Files, want)
	}
}

// A base committed by 0.8.x holds ciphertext while live holds what that
// version decrypted, so the two differ whoever moved. LiveFactsAt says so,
// in every branch, for the classifier to treat the base as unknown.
func TestLiveFactsAtFlagsASOPSBase(t *testing.T) {
	gs, sha, _ := sopsFixture(t, map[string]string{
		"zigbee2mqtt/configuration.yaml": sopsYAML,
		"automations.yaml":               "- id: demo\n",
	})
	ctx := context.Background()
	configRoot := t.TempDir()
	writeLiveText(t, configRoot, "zigbee2mqtt/configuration.yaml", "mqtt:\n    password: decrypted-by-0.8\n")
	writeLiveText(t, configRoot, "automations.yaml", "- id: edited\n")

	facts, err := gs.LiveFactsAt(ctx, sha, configRoot, "zigbee2mqtt/configuration.yaml")
	if err != nil {
		t.Fatalf("LiveFactsAt: %v", err)
	}
	if !facts.BaseIsSops || !facts.BaseTracks || facts.MatchesBase {
		t.Errorf("facts = %+v, want a tracked SOPS base that does not match", facts)
	}

	plain, err := gs.LiveFactsAt(ctx, sha, configRoot, "automations.yaml")
	if err != nil {
		t.Fatalf("LiveFactsAt: %v", err)
	}
	if plain.BaseIsSops {
		t.Errorf("facts = %+v, want an ordinary base not flagged", plain)
	}

	// Gone live: the blob is still read for the flag.
	if err := os.Remove(filepath.Join(configRoot, "zigbee2mqtt", "configuration.yaml")); err != nil {
		t.Fatal(err)
	}
	gone, err := gs.LiveFactsAt(ctx, sha, configRoot, "zigbee2mqtt/configuration.yaml")
	if err != nil {
		t.Fatalf("LiveFactsAt: %v", err)
	}
	if !gone.Gone || !gone.BaseIsSops {
		t.Errorf("facts = %+v, want gone with a SOPS base", gone)
	}
}
