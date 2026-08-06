package gitsync

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/sopscrypt"
)

// --- real sops: the whole import path against the actual binary ---------
//
// The fake sops used elsewhere is byte-exact and never fails; the real one
// re-serializes, refuses shapes, and rewrites values. Skipped when absent.

func realSopsCrypter(t *testing.T) *sopscrypt.Crypter {
	t.Helper()
	if _, err := exec.LookPath("sops"); err != nil {
		t.Skip("sops is not installed")
	}
	if _, err := exec.LookPath("age-keygen"); err != nil {
		t.Skip("age-keygen is not installed")
	}
	out, err := exec.Command("age-keygen").Output() // #nosec G204 -- fixed binary, no arguments
	if err != nil {
		t.Fatalf("age-keygen: %v", err)
	}
	var identity string
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "AGE-SECRET-KEY-") {
			identity = strings.TrimSpace(line)
		}
	}
	if identity == "" {
		t.Fatal("age-keygen produced no identity")
	}
	crypter, err := sopscrypt.New(identity)
	if err != nil {
		t.Fatalf("sopscrypt.New: %v", err)
	}
	return crypter
}

// A realistic config through import and back out through the differ, with
// the real binary at both ends.
func TestRealSopsImportRoundTrip(t *testing.T) {
	crypter := realSopsCrypter(t)

	const (
		secrets = "mqtt_password: \"abc#123\"\nlatitude: 52.1\nempty_one:\n"
		// The zigbee2mqtt shape: an inline secret beside ordinary settings.
		z2m = "mqtt:\n  server: mqtt://10.0.0.1\n  user: addons\n  password: LIVESECRET\nserial:\n  port: /dev/ttyUSB0\n"
		// A top-level list, which sops refuses outright.
		automations = "- id: demo\n  alias: Demo\n  action:\n    api_key: LEAKME\n"
		// Unquoted YAML 1.1 bool: sops quotes it, changing how HA reads it.
		legacyBool = "mqtt:\n  password: LEAKME\n  discovery: yes\n"
		// GCP service account key: "private_key" always matched
		// SecretKeyRegex; only the .json extension kept it in the clear.
		serviceAccount = "{\n  \"type\": \"service_account\",\n  \"project_id\": \"demo\",\n" +
			"  \"private_key\": \"-----BEGIN PRIVATE KEY-----\\nJSONSECRET\\n-----END PRIVATE KEY-----\\n\",\n" +
			"  \"client_email\": \"ha@demo.example\"\n}\n"
		// wmbusmeters: extensionless key=value with the AES key on "key=".
		// sops binary-encrypts unless told the format, and drops blank lines.
		meter = "# kitchen\nname=MyHeater\ndriver=multical21\n\nid=12345678\nkey=DOTENVSECRET\n"
	)

	tmp := t.TempDir()
	bare, work := makeRemote(t, tmp, "repo")
	commitFile(t, work, "README.md", "readme\n", "init")

	configRoot := filepath.Join(tmp, "config")
	writeLiveText(t, configRoot, "secrets.yaml", secrets)
	writeLiveText(t, configRoot, "zigbee2mqtt/configuration.yaml", z2m)
	writeLiveText(t, configRoot, "includes/HAandGHome.json", serviceAccount)
	writeLiveText(t, configRoot, "wmbusmeters/etc/wmbusmeters.d/meter-0001", meter)

	gs := importGitSync(t, bare, filepath.Join(tmp, "workdir"))
	SetEncryptionEnabled(true)
	t.Cleanup(func() { SetEncryptionEnabled(false) })
	gs.Crypter = crypter

	res, err := gs.Import(context.Background(), configRoot, generousLimits(), fixedImportTime)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}

	// 1. secrets.yaml: every value encrypted, nothing readable.
	pushed, ok := showAtRef(t, bare, "main", "secrets.yaml")
	if !ok {
		t.Fatal("secrets.yaml missing from the tracked branch")
	}
	for _, plaintext := range []string{"abc#123", "52.1"} {
		if strings.Contains(pushed, plaintext) {
			t.Errorf("pushed secrets.yaml still carries %q:\n%s", plaintext, pushed)
		}
	}
	if !strings.Contains(pushed, "ENC[AES256_GCM") {
		t.Errorf("pushed secrets.yaml is not sops ciphertext:\n%s", pushed)
	}

	// 2. The mixed file: secret value encrypted, everything else readable.
	z2mPushed, ok := showAtRef(t, bare, "main", "zigbee2mqtt/configuration.yaml")
	if !ok {
		t.Fatal("zigbee2mqtt/configuration.yaml missing from the tracked branch")
	}
	if strings.Contains(z2mPushed, "LIVESECRET") {
		t.Errorf("the inline secret reached the remote in the clear:\n%s", z2mPushed)
	}
	for _, readable := range []string{"mqtt://10.0.0.1", "/dev/ttyUSB0", "addons"} {
		if !strings.Contains(z2mPushed, readable) {
			t.Errorf("non-secret value %q was encrypted too, the file is no longer reviewable:\n%s", readable, z2mPushed)
		}
	}

	// 2b. The service account key: PEM gone, the rest of the file readable.
	jsonPushed, ok := showAtRef(t, bare, "main", "includes/HAandGHome.json")
	if !ok {
		t.Fatal("includes/HAandGHome.json missing from the tracked branch")
	}
	if strings.Contains(jsonPushed, "JSONSECRET") {
		t.Errorf("the service account private key reached the remote in the clear:\n%s", jsonPushed)
	}
	for _, readable := range []string{"service_account", "demo", "ha@demo.example"} {
		if !strings.Contains(jsonPushed, readable) {
			t.Errorf("non-secret value %q was encrypted too:\n%s", readable, jsonPushed)
		}
	}

	// 2c. The meter definition. Readable fields surviving also proves sops
	// did not fall back to its binary store (one base64 "data" field).
	meterPushed, ok := showAtRef(t, bare, "main", "wmbusmeters/etc/wmbusmeters.d/meter-0001")
	if !ok {
		t.Fatal("wmbusmeters/etc/wmbusmeters.d/meter-0001 missing from the tracked branch")
	}
	if strings.Contains(meterPushed, "DOTENVSECRET") {
		t.Errorf("the meter key reached the remote in the clear:\n%s", meterPushed)
	}
	for _, readable := range []string{"name=MyHeater", "driver=multical21", "id=12345678"} {
		if !strings.Contains(meterPushed, readable) {
			t.Errorf("expected %q to stay readable; sops may have encrypted the whole file as binary:\n%s", readable, meterPushed)
		}
	}
	if strings.Contains(meterPushed, `"data"`) {
		t.Errorf("the meter file was encrypted as one binary blob rather than value by value:\n%s", meterPushed)
	}

	// 3. The managed .sops.yaml rode along and carries only the public half.
	cfg, ok := showAtRef(t, bare, "main", sopscrypt.ConfigFile)
	if !ok {
		t.Fatalf("%s missing from the tracked branch", sopscrypt.ConfigFile)
	}
	if !strings.Contains(cfg, crypter.Recipient()) {
		t.Errorf("%s does not carry the recipient:\n%s", sopscrypt.ConfigFile, cfg)
	}

	// 4. Steady state: nothing changed live, so a second import finds
	// nothing to do - the churn check against real re-serialization.
	if _, err := gs.Import(context.Background(), configRoot, generousLimits(), fixedImportTime); err == nil {
		t.Error("second Import committed again; unchanged config must produce no commit")
	} else if !strings.Contains(err.Error(), "nothing to import") {
		t.Errorf("second Import error = %v, want the nothing-to-import refusal", err)
	}
	if head := headSHA(t, bare, "main"); head != res.CommitSHA {
		t.Errorf("main moved from %s to %s over an unchanged config", res.CommitSHA, head)
	}

	// 5. The repo decrypts to what is live, by the differ's own predicate;
	// a mismatch is permanent phantom drift. Read the pushed blobs, not the
	// worktree: Import leaves the checkout detached at the base commit.
	scratch := t.TempDir()
	for _, rel := range []string{
		"secrets.yaml",
		"zigbee2mqtt/configuration.yaml",
		"includes/HAandGHome.json",
		"wmbusmeters/etc/wmbusmeters.d/meter-0001",
	} {
		blob, ok := showAtRef(t, bare, "main", rel)
		if !ok {
			t.Fatalf("%s missing from the tracked branch", rel)
		}
		onDisk := filepath.Join(scratch, filepath.Base(rel))
		if err := os.WriteFile(onDisk, []byte(blob), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		plain, err := crypter.DecryptFile(context.Background(), onDisk)
		if err != nil {
			t.Fatalf("DecryptFile(%s): %v", rel, err)
		}
		live, err := os.ReadFile(filepath.Join(configRoot, filepath.FromSlash(rel))) // #nosec G304 -- test fixture under t.TempDir()
		if err != nil {
			t.Fatalf("ReadFile(%s): %v", rel, err)
		}
		if !sopscrypt.SemanticallyEqual(plain, live) {
			t.Errorf("%s decrypts to something other than live, which is permanent drift:\n--- repo ---\n%s\n--- live ---\n%s", rel, plain, live)
		}
	}

	// 7. Shapes sops cannot handle are refused by name rather than handed
	// to it, which used to abort the entire import.
	for name, content := range map[string]string{
		"automations.yaml":   automations,
		"packages/mqtt.yaml": legacyBool,
	} {
		need, refusal := sopscrypt.NeedsEncryption(name, []byte(content))
		if need {
			t.Errorf("NeedsEncryption(%s) said encrypt; the real binary cannot", name)
		}
		if refusal == "" {
			t.Errorf("NeedsEncryption(%s) neither encrypts nor refuses, so the secret would be committed in the clear", name)
		}
	}
}

// Pins the refusals against the binary, so a future sops that changes its
// mind shows up here rather than in a failed import on someone's install.
func TestRealSopsRefusesWhatWeRefuse(t *testing.T) {
	crypter := realSopsCrypter(t)
	dir := t.TempDir()

	cases := []struct {
		name    string
		rel     string
		content string
	}{
		{"top-level list", "automations.yaml", "- id: demo\n  api_key: abc\n"},
		{"empty document", "secrets.yaml", ""},
		{"comment only", "secrets.yaml", "# nothing here yet\n"},
		{"literal sops key", "packages/x.yaml", "sops: mine\npassword: abc\n"},
		{"json top-level array", "www/list.json", `[{"api_key": "abc"}]`},
		{"json literal sops key", "www/x.json", `{"sops": "mine", "password": "abc"}`},
		// A flat format nests sops metadata as top-level "sops_" keys, so a
		// file that already has one collides.
		{"dotenv sops_ prefix", "wmbusmeters/etc/wmbusmeters.d/meter-0004", "sops_mac=mine\nkey=abc\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, filepath.Base(tc.rel))
			if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
			sopsErr := crypter.EncryptFileInPlace(context.Background(), path, tc.rel)
			need, refusal := sopscrypt.NeedsEncryption(tc.rel, []byte(tc.content))
			// Only "hand it to sops anyway" is unacceptable: it aborts the
			// whole import.
			if sopsErr != nil && need {
				t.Errorf("sops refuses this file (%v) but NeedsEncryption wants to encrypt it, so an import would abort on it", sopsErr)
			}
			if sopsErr == nil && refusal != "" {
				t.Logf("note: refused (%s) although this sops accepts the file", refusal)
			}
		})
	}
}

// A committed .sops.yaml must not switch encryption off while still
// producing a file that passes every "is this encrypted" check.
func TestRealSopsCannotBeSteeredByARepoConfig(t *testing.T) {
	crypter := realSopsCrypter(t)

	worktree := t.TempDir()
	nested := filepath.Join(worktree, "packages")
	if err := os.MkdirAll(nested, 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	hostile := "creation_rules:\n  - path_regex: .*\n    unencrypted_regex: '.*'\n    age: " + crypter.Recipient() + "\n"
	if err := os.WriteFile(filepath.Join(nested, sopscrypt.ConfigFile), []byte(hostile), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	target := filepath.Join(nested, "secrets.yaml")
	if err := os.WriteFile(target, []byte("mqtt_password: LEAKME\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if err := crypter.EncryptFileInPlace(context.Background(), target, "packages/secrets.yaml"); err != nil {
		t.Fatalf("EncryptFileInPlace: %v", err)
	}
	got, err := os.ReadFile(target) // #nosec G304 -- test fixture under t.TempDir()
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if strings.Contains(string(got), "LEAKME") {
		t.Fatalf("a repository-supplied .sops.yaml disabled encryption while still producing a sops document:\n%s", got)
	}
}
