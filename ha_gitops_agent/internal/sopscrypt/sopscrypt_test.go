package sopscrypt

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"
)

// An age key for this file alone, so the recipient derivation has a fixed
// expected answer. Nothing it could decrypt exists outside testdata/.
const (
	testIdentity  = "AGE-SECRET-KEY-1ZVEXNYJ990KE6NVCUYJE0YS5XHLTEY4ALPTT7P3AX3CM56ZDYNXQVEX4EM"
	testRecipient = "age13gfqtx8qq0zxqaqvfxyyh0mkucf8cnfach5m7lhy0sysvt8jx9zsw3z2vy"
)

// recordedRun captures one Runner.Run invocation.
type recordedRun struct {
	dir  string
	env  []string
	args []string
}

// fakeRunner records every call and returns a canned result, so no real
// sops process is ever spawned.
type fakeRunner struct {
	calls  []recordedRun
	stdout string
	stderr string
	exit   int
}

func (f *fakeRunner) Run(_ context.Context, dir string, env []string, args ...string) (RunResult, error) {
	f.calls = append(f.calls, recordedRun{
		dir:  dir,
		env:  append([]string(nil), env...),
		args: append([]string(nil), args...),
	})
	return RunResult{Stdout: f.stdout, Stderr: f.stderr, ExitCode: f.exit}, nil
}

func newTestCrypter(t *testing.T) (*Crypter, *fakeRunner) {
	t.Helper()
	c, err := New(testIdentity)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	fr := &fakeRunner{}
	c.Runner = fr
	return c, fr
}

// --- key handling ---------------------------------------------------------

func TestNewDerivesRecipientAndTrimsWhitespace(t *testing.T) {
	for _, input := range []string{testIdentity, "  " + testIdentity + "\n"} {
		c, err := New(input)
		if err != nil {
			t.Fatalf("New(%q): %v", input, err)
		}
		if got := c.Recipient(); got != testRecipient {
			t.Errorf("Recipient() = %q, want %q", got, testRecipient)
		}
		if !c.Enabled() {
			t.Error("Enabled() = false, want true")
		}
	}
}

func TestNewRejectsMalformedKey(t *testing.T) {
	for _, input := range []string{"", "   ", "not-an-age-key", "AGE-SECRET-KEY-1NOTVALID", testRecipient} {
		c, err := New(input)
		if err == nil {
			t.Errorf("New(%q) error = nil, want an error", input)
		}
		if c != nil {
			t.Errorf("New(%q) crypter = %v, want nil", input, c)
		}
	}
}

// Call sites are plain g.Crypter.Enabled(), so a nil *Crypter (no age key
// configured) must answer false rather than panic.
func TestEnabledIsNilAndZeroSafe(t *testing.T) {
	var nilCrypter *Crypter
	if nilCrypter.Enabled() {
		t.Error("(*Crypter)(nil).Enabled() = true, want false")
	}
	if got := nilCrypter.Recipient(); got != "" {
		t.Errorf("(*Crypter)(nil).Recipient() = %q, want empty", got)
	}
	if (&Crypter{}).Enabled() {
		t.Error("(&Crypter{}).Enabled() = true, want false")
	}
}

// --- argv and env ---------------------------------------------------------

func TestEncryptPassesRecipientAndRegexAndNoKeyMaterial(t *testing.T) {
	c, fr := newTestCrypter(t)

	if err := c.EncryptFileInPlace(context.Background(), "/repo/packages/mqtt.yaml", "packages/mqtt.yaml"); err != nil {
		t.Fatalf("EncryptFileInPlace: %v", err)
	}
	if len(fr.calls) != 1 {
		t.Fatalf("len(calls) = %d, want 1", len(fr.calls))
	}
	call := fr.calls[0]

	want := []string{
		"sops", "encrypt", "--in-place",
		"--age", testRecipient,
		"--encrypted-regex", SecretKeyRegex,
		"--input-type", "yaml", "--output-type", "yaml",
		"/repo/packages/mqtt.yaml",
	}
	if !equalStrings(call.args, want) {
		t.Errorf("argv = %q, want %q", call.args, want)
	}
	assertRunsOutsideTheWorktree(t, call.dir, "/repo/packages/mqtt.yaml")
	assertNoKeyMaterial(t, call)
}

// sops finds its config by walking up from the working directory, so
// running inside the worktree lets a planted "unencrypted_regex: '.*'"
// exit 0 with a valid sops block and every value still in the clear.
func assertRunsOutsideTheWorktree(t *testing.T, dir, target string) {
	t.Helper()
	if dir == "" {
		t.Fatal("sops ran with an empty working directory, which is the process's own - it must be a directory the agent controls")
	}
	if strings.HasPrefix(target, dir) {
		t.Errorf("sops ran from %q, which contains the file being encrypted: a .sops.yaml committed to the repository would be found from there", dir)
	}
	if entries, err := os.ReadDir(dir); err != nil {
		t.Errorf("sops working directory %q is not readable: %v", dir, err)
	} else if len(entries) != 0 {
		t.Errorf("sops working directory %q is not empty, so sops may find a config in it", dir)
	}
}

func TestDecryptAlsoRunsOutsideTheWorktree(t *testing.T) {
	c, fr := newTestCrypter(t)
	if _, err := c.DecryptFile(context.Background(), "/repo/secrets.yaml"); err != nil {
		t.Fatalf("DecryptFile: %v", err)
	}
	assertRunsOutsideTheWorktree(t, fr.calls[0].dir, "/repo/secrets.yaml")
}

// secrets.yaml is encrypted whole: --encrypted-regex would leave every
// not-secret-shaped key (latitude, a username) in the clear.
func TestEncryptSecretsFileOmitsEncryptedRegex(t *testing.T) {
	for _, rel := range []string{"secrets.yaml", "secrets.yml", "SECRETS.YAML", "sub/secrets.yaml"} {
		c, fr := newTestCrypter(t)
		if err := c.EncryptFileInPlace(context.Background(), "/repo/"+rel, rel); err != nil {
			t.Fatalf("EncryptFileInPlace(%q): %v", rel, err)
		}
		call := fr.calls[0]
		for _, arg := range call.args {
			if arg == "--encrypted-regex" {
				t.Errorf("argv for %q = %q, want no --encrypted-regex", rel, call.args)
			}
		}
		want := []string{
			"sops", "encrypt", "--in-place", "--age", testRecipient,
			"--input-type", "yaml", "--output-type", "yaml", "/repo/" + rel,
		}
		if !equalStrings(call.args, want) {
			t.Errorf("argv = %q, want %q", call.args, want)
		}
		assertNoKeyMaterial(t, call)
	}
}

// sops infers its store from the extension, and dotenv has none: left to
// guess it uses the binary store and ignores --encrypted-regex.
func TestEncryptTellsSopsTheStoreForDotenv(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "meter-0001")
	if err := os.WriteFile(target, []byte("name=heater\nkey=00112233\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	c, fr := newTestCrypter(t)
	rel := "wmbusmeters/etc/wmbusmeters.d/meter-0001"
	if err := c.EncryptFileInPlace(context.Background(), target, rel); err != nil {
		t.Fatalf("EncryptFileInPlace: %v", err)
	}
	want := []string{
		"sops", "encrypt", "--in-place",
		"--age", testRecipient,
		"--encrypted-regex", SecretKeyRegex,
		"--input-type", "dotenv", "--output-type", "dotenv",
		target,
	}
	if !equalStrings(fr.calls[0].args, want) {
		t.Errorf("argv = %q, want %q", fr.calls[0].args, want)
	}
	assertNoKeyMaterial(t, fr.calls[0])
}

// The fallback for an unrecognized format is sops's binary store, so both
// ways of failing to establish one must stop before sops is invoked.
func TestEncryptRefusesAFileItCannotPlace(t *testing.T) {
	dir := t.TempDir()
	prose := filepath.Join(dir, "README")
	if err := os.WriteFile(prose, []byte("this is not a config file\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	for _, target := range []string{prose, filepath.Join(dir, "does-not-exist")} {
		c, fr := newTestCrypter(t)
		err := c.EncryptFileInPlace(context.Background(), target, filepath.Base(target))
		if err == nil {
			t.Errorf("EncryptFileInPlace(%s) error = nil, want a refusal", filepath.Base(target))
		}
		if len(fr.calls) != 0 {
			t.Errorf("EncryptFileInPlace(%s) ran sops anyway: %q", filepath.Base(target), fr.calls[0].args)
		}
	}
}

// sops reads the extension case-sensitively, so "config.JSON" is not JSON
// to it and falls back to the binary store; naming the store avoids that.
func TestEncryptNamesTheStoreForEveryFormat(t *testing.T) {
	cases := []struct{ rel, store string }{
		{"includes/sa.json", "json"},
		{"includes/SA.JSON", "json"},
		{"packages/mqtt.yaml", "yaml"},
		{"packages/MQTT.YAML", "yaml"},
		{"packages/mqtt.yml", "yaml"},
	}
	for _, tc := range cases {
		t.Run(tc.rel, func(t *testing.T) {
			c, fr := newTestCrypter(t)
			if err := c.EncryptFileInPlace(context.Background(), "/repo/"+tc.rel, tc.rel); err != nil {
				t.Fatalf("EncryptFileInPlace: %v", err)
			}
			want := []string{
				"sops", "encrypt", "--in-place",
				"--age", testRecipient,
				"--encrypted-regex", SecretKeyRegex,
				"--input-type", tc.store, "--output-type", tc.store,
				"/repo/" + tc.rel,
			}
			if !equalStrings(fr.calls[0].args, want) {
				t.Errorf("argv = %q, want %q", fr.calls[0].args, want)
			}
		})
	}
}

// sops needs telling on the way back out as much as in: without the flags
// it reads the file as binary and fails.
func TestDecryptTellsSopsTheStoreForDotenv(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "meter-0001")
	if err := os.WriteFile(target, []byte(dotenvCiphertext), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	c, fr := newTestCrypter(t)
	if _, err := c.DecryptFile(context.Background(), target); err != nil {
		t.Fatalf("DecryptFile: %v", err)
	}
	want := []string{"sops", "decrypt", "--input-type", "dotenv", "--output-type", "dotenv", target}
	if !equalStrings(fr.calls[0].args, want) {
		t.Errorf("argv = %q, want %q", fr.calls[0].args, want)
	}

	// Decrypt names the store for every format, mirroring encrypt: a .JSON
	// written through the json store and read back without it fails with
	// "no binary data found in tree" (sops 3.13.2).
	structured := []struct{ name, content, store string }{
		{"secrets.yaml", "sops:\n  mac: x\n  version: 3.13.2\n", "yaml"},
		{"SA.JSON", "{\"sops\": {\"mac\": \"x\", \"version\": \"3.13.2\"}}", "json"},
	}
	for _, tc := range structured {
		t.Run(tc.name, func(t *testing.T) {
			target := filepath.Join(dir, tc.name)
			if err := os.WriteFile(target, []byte(tc.content), 0o600); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
			c, fr := newTestCrypter(t)
			if _, err := c.DecryptFile(context.Background(), target); err != nil {
				t.Fatalf("DecryptFile: %v", err)
			}
			want := []string{"sops", "decrypt", "--input-type", tc.store, "--output-type", tc.store, target}
			if !equalStrings(fr.calls[0].args, want) {
				t.Errorf("argv = %q, want %q", fr.calls[0].args, want)
			}
		})
	}
}

func TestEncryptSurfacesNonZeroExit(t *testing.T) {
	c, fr := newTestCrypter(t)
	fr.exit = 203
	fr.stdout = "The file you have provided contains a top-level entry called 'sops'"

	err := c.EncryptFileInPlace(context.Background(), "/repo/secrets.yaml", "secrets.yaml")
	if err == nil {
		t.Fatal("EncryptFileInPlace() error = nil, want an error")
	}
	// sops explains this one on stdout, so an empty stderr must fall back
	// rather than produce a bare "(exit 203): ".
	if !strings.Contains(err.Error(), "top-level entry called 'sops'") {
		t.Errorf("error = %q, want it to carry sops's own stdout explanation", err)
	}
}

func TestDecryptPassesKeyOnlyInEnv(t *testing.T) {
	c, fr := newTestCrypter(t)
	fr.stdout = "mqtt_password: hunter2\n"

	plaintext, err := c.DecryptFile(context.Background(), "/repo/secrets.yaml")
	if err != nil {
		t.Fatalf("DecryptFile: %v", err)
	}
	if string(plaintext) != "mqtt_password: hunter2\n" {
		t.Errorf("plaintext = %q, want sops's stdout verbatim", plaintext)
	}
	if len(fr.calls) != 1 {
		t.Fatalf("len(calls) = %d, want 1", len(fr.calls))
	}
	call := fr.calls[0]

	want := []string{"sops", "decrypt", "/repo/secrets.yaml"}
	if !equalStrings(call.args, want) {
		t.Errorf("argv = %q, want %q (bare - no key, no recipient)", call.args, want)
	}
	for _, arg := range call.args {
		if strings.Contains(arg, testIdentity) {
			t.Fatalf("argv = %q, must never carry the private key", call.args)
		}
	}

	var found int
	for _, kv := range call.env {
		if kv == "SOPS_AGE_KEY="+testIdentity {
			found++
		}
	}
	if found != 1 {
		t.Errorf("env carries SOPS_AGE_KEY=<key> %d times, want exactly 1: only that one subprocess may see it", found)
	}
}

// sops's failure text reaches the status UI and the log, so an identity it
// echoed back must not survive the trip.
func TestDecryptRedactsKeyFromErrors(t *testing.T) {
	c, fr := newTestCrypter(t)
	fr.exit = 1
	fr.stderr = "failed to load key " + testIdentity + " for decryption"

	_, err := c.DecryptFile(context.Background(), "/repo/secrets.yaml")
	if err == nil {
		t.Fatal("DecryptFile() error = nil, want an error")
	}
	if strings.Contains(err.Error(), testIdentity) {
		t.Errorf("error = %q, still contains the private key", err)
	}
	if !strings.Contains(err.Error(), "***REDACTED***") {
		t.Errorf("error = %q, want a ***REDACTED*** marker", err)
	}
}

// assertNoKeyMaterial checks that neither argv nor env of an encrypt call
// carries the private key; encryption needs the public recipient alone.
func assertNoKeyMaterial(t *testing.T, call recordedRun) {
	t.Helper()
	for _, arg := range call.args {
		if strings.Contains(arg, testIdentity) {
			t.Errorf("argv = %q, must never carry the private key", call.args)
		}
	}
	for _, kv := range call.env {
		if strings.Contains(kv, testIdentity) {
			t.Errorf("env entry %q carries the private key", kv)
		}
		if strings.HasPrefix(kv, "SOPS_AGE_KEY=") || strings.HasPrefix(kv, "SOPS_AGE_KEY_FILE=") {
			t.Errorf("env entry %q must not reach an encrypt call", kv)
		}
	}
}

// On POSIX a duplicated variable resolves to its first occurrence, so an
// inherited value would win over the one DecryptFile appends.
func TestBaseEnvStripsInheritedAgeKey(t *testing.T) {
	t.Setenv("SOPS_AGE_KEY", "AGE-SECRET-KEY-1INHERITED")
	t.Setenv("SOPS_AGE_KEY_FILE", "/somewhere/keys.txt")

	for _, kv := range baseEnv() {
		if strings.HasPrefix(kv, "SOPS_AGE_KEY") {
			t.Errorf("baseEnv() kept %q", kv)
		}
	}
}

// --- Probe ----------------------------------------------------------------

func TestProbeRunsVersionWithoutKeyMaterial(t *testing.T) {
	c, fr := newTestCrypter(t)
	fr.stdout = "sops 3.12.0"

	if err := c.Probe(context.Background()); err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if len(fr.calls) != 1 {
		t.Fatalf("Runner calls = %d, want 1", len(fr.calls))
	}
	// --disable-version-check keeps the probe local: otherwise sops asks
	// GitHub for a newer release and an offline install fails to start.
	if want := []string{"sops", "--version", "--disable-version-check"}; !equalStrings(fr.calls[0].args, want) {
		t.Errorf("argv = %q, want %q", fr.calls[0].args, want)
	}
	assertNoKeyMaterial(t, fr.calls[0])
}

func TestProbeFailsOnNonZeroExit(t *testing.T) {
	c, fr := newTestCrypter(t)
	fr.exit = 127
	fr.stderr = "sops: not found"

	err := c.Probe(context.Background())
	if err == nil {
		t.Fatal("Probe() error = nil, want an error")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error = %q, want it to carry sops's own explanation", err)
	}
}

func TestProbeOnDisabledCrypter(t *testing.T) {
	var c *Crypter
	if err := c.Probe(context.Background()); err == nil {
		t.Error("Probe() on a nil Crypter error = nil, want an error")
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// --- SecretKeyRegex -------------------------------------------------------

func TestSecretKeyRegexMatchesSecretShapedKeysOnly(t *testing.T) {
	positives := []string{
		"password", "mqtt_password", "network_key", "client_secret", "bearer_token",
		"key", "keys", "api_key", "apikey", "psk", "auth",
		"PASSWORD", "Api_Key", "token", "credentials", "authorization",
	}
	for _, k := range positives {
		if !secretKeyRe.MatchString(k) {
			t.Errorf("SecretKeyRegex does not match %q, want a match", k)
		}
	}

	// Why the pattern is anchored: a substring match would encrypt half an
	// ordinary Home Assistant config.
	negatives := []string{
		"monkey", "keyboard", "service_account_key_file", "donkey", "pinboard",
		"name", "broker", "latitude", "keymap", "authenticated_users",
		// The suffix alternation is shorter than the whole-key list:
		// "secrets", "authorization" and "apikey" are whole keys only.
		"client_secrets", "google_client_secrets", "x_authorization", "foo_apikey",
		// In ESPHome a "pin" is a GPIO number: 13 files in the live config
		// matched on these alone.
		"pin", "cs_pin", "dc_pin", "clk_pin", "mosi_pin", "reset_pin", "i2s_bclk_pin", "pinboard",
	}
	for _, k := range negatives {
		if secretKeyRe.MatchString(k) {
			t.Errorf("SecretKeyRegex matches %q, want no match", k)
		}
	}
}

// --- NeedsEncryption ------------------------------------------------------

func TestNeedsEncryption(t *testing.T) {
	cases := []struct {
		name        string
		relPath     string
		content     string
		wantNeed    bool
		wantRefusal bool
	}{
		{
			name:     "inline secret scalar",
			relPath:  "packages/mqtt.yaml",
			content:  "mqtt:\n  broker: 10.0.0.1\n  password: hunter2\n",
			wantNeed: true,
		},
		{
			name:     "inline secret list of scalars",
			relPath:  "packages/zha.yaml",
			content:  "zha:\n  network_key:\n    - 1\n    - 2\n",
			wantNeed: true,
		},
		{
			name:     "secret key referencing secrets.yaml is not a trigger",
			relPath:  "configuration.yaml",
			content:  "mqtt:\n  password: !secret mqtt_password\n  broker: 10.0.0.1\n",
			wantNeed: false,
		},
		{
			name:     "secret-shaped key holding a mapping is not a trigger",
			relPath:  "configuration.yaml",
			content:  "auth:\n  provider: trusted_networks\n  allow: true\n",
			wantNeed: false,
		},
		{
			name:     "empty value is not a trigger",
			relPath:  "configuration.yaml",
			content:  "mqtt:\n  password:\n",
			wantNeed: false,
		},
		{
			name:        "inline secret alongside a custom tag elsewhere",
			relPath:     "configuration.yaml",
			content:     "mqtt:\n  password: hunter2\nautomation: !include automations.yaml\n",
			wantNeed:    false,
			wantRefusal: true,
		},
		{
			name:     "no secret-shaped key at all",
			relPath:  "automations.yaml",
			content:  "- id: demo\n  alias: Demo\n",
			wantNeed: false,
		},
		{
			name:     "json inline secret",
			relPath:  "www/config.json",
			content:  `{"password": "hunter2"}`,
			wantNeed: true,
		},
		{
			name:     "extension this package handles no format for",
			relPath:  "custom_components/foo/__init__.py",
			content:  "PASSWORD = \"hunter2\"\n",
			wantNeed: false,
		},
		{
			name:     "secrets.yaml always needs encryption",
			relPath:  "secrets.yaml",
			content:  "latitude: 52.1\n",
			wantNeed: true,
		},
		{
			name:        "unparseable secrets.yaml is refused, never committed as-is",
			relPath:     "secrets.yaml",
			content:     "a: [1,\nb: :::\n",
			wantNeed:    false,
			wantRefusal: true,
		},
		{
			name:     "unparseable ordinary yaml with nothing secret-shaped is left alone",
			relPath:  "configuration.yaml",
			content:  "a: [1,\nb: :::\n",
			wantNeed: false,
		},
		{
			// sops fails open here, this must not.
			name:        "unparseable ordinary yaml carrying a secret-shaped key is refused",
			relPath:     "configuration.yaml",
			content:     "mqtt:\n\tpassword: hunter2\n  broker: [1,\n",
			wantNeed:    false,
			wantRefusal: true,
		},
		{
			// automations.yaml is always a top-level list and sops exits 2
			// on one, which used to abort the whole import.
			name:        "top-level sequence holding a secret is refused, not handed to sops",
			relPath:     "automations.yaml",
			content:     "- id: demo\n  action:\n    api_key: abc123\n",
			wantNeed:    false,
			wantRefusal: true,
		},
		{
			// sops exits 2 on a document-less stream, which used to wedge
			// the very first import a new user runs.
			name:     "empty secrets.yaml has no secret to protect",
			relPath:  "secrets.yaml",
			content:  "",
			wantNeed: false,
		},
		{
			name:     "comment-only secrets.yaml has no secret to protect",
			relPath:  "secrets.yaml",
			content:  "# put your secrets here\n",
			wantNeed: false,
		},
		{
			// sops re-serializes the whole document, quoting these, and HA's
			// YAML 1.1 parser then reads a string where it read a boolean.
			name:        "unquoted YAML 1.1 boolean alongside an inline secret is refused",
			relPath:     "packages/mqtt.yaml",
			content:     "mqtt:\n  password: hunter2\n  discovery: yes\n",
			wantNeed:    false,
			wantRefusal: true,
		},
		{
			name:     "quoted YAML 1.1 boolean is already a string and does not block",
			relPath:  "packages/mqtt.yaml",
			content:  "mqtt:\n  password: hunter2\n  discovery: \"yes\"\n",
			wantNeed: true,
		},
		{
			name:     "YAML 1.1 boolean in secrets.yaml does not block, every value is encrypted",
			relPath:  "secrets.yaml",
			content:  "flag: yes\napi_key: abc123\n",
			wantNeed: true,
		},
		{
			// sops reserves this key for its own metadata and exits 203.
			name:        "literal top-level sops key is refused",
			relPath:     "packages/mqtt.yaml",
			content:     "sops: mine\npassword: hunter2\n",
			wantNeed:    false,
			wantRefusal: true,
		},
		{
			name:     "secret in the second document of a stream",
			relPath:  "packages/multi.yaml",
			content:  "first: value\n---\nmqtt:\n  password: hunter2\n",
			wantNeed: true,
		},
		{
			name:     "deeply nested secret",
			relPath:  "packages/deep.yaml",
			content:  "a:\n  b:\n    - c:\n        api_key: abc123\n",
			wantNeed: true,
		},

		// --- JSON. A live config held a GCP service account key in one:
		// "private_key" matched already, only the extension kept it clear.
		{
			name:     "json service account key",
			relPath:  "includes/HAandGHome.json",
			content:  "{\n  \"type\": \"service_account\",\n  \"private_key\": \"-----BEGIN PRIVATE KEY-----\\nabc\\n\"\n}\n",
			wantNeed: true,
		},
		{
			name:     "json with nothing secret-shaped",
			relPath:  "www/community/card/hacs.json",
			content:  "{\n  \"name\": \"Some card\",\n  \"render_readme\": true\n}\n",
			wantNeed: false,
		},
		{
			name:     "json nested secret",
			relPath:  "zigbee2mqtt/coordinator_backup.json",
			content:  "{\n  \"metadata\": {\"version\": 1},\n  \"network_key\": {\"key\": [1, 2, 3]}\n}\n",
			wantNeed: true,
		},
		{
			// sops exits 2: "SOPS only supports JSON files with a top-level
			// object".
			name:        "json top-level array holding a secret is refused",
			relPath:     "www/list.json",
			content:     `[{"api_key": "abc123"}]`,
			wantNeed:    false,
			wantRefusal: true,
		},
		{
			name:        "json top-level sops key is refused",
			relPath:     "www/config.json",
			content:     `{"sops": "mine", "password": "hunter2"}`,
			wantNeed:    false,
			wantRefusal: true,
		},
		{
			name:        "unparseable json carrying a secret-shaped line is refused",
			relPath:     "www/config.json",
			content:     "{\n  \"password\": \"hunter2\",\n",
			wantNeed:    false,
			wantRefusal: true,
		},
		{
			// YAML in a .json file parses fine through go-yaml but fails
			// inside sops, which picks its store from the extension.
			name:        "yaml wearing a json extension is refused",
			relPath:     "www/config.json",
			content:     "password: hunter2\n",
			wantNeed:    false,
			wantRefusal: true,
		},
		{
			name:     "unparseable json with nothing secret-shaped is left alone",
			relPath:  "www/config.json",
			content:  "{\n  \"name\": \"x\",\n",
			wantNeed: false,
		},

		// --- dotenv. wmbusmeters writes these extensionless, one meter per
		// file, with the wM-Bus AES key under a "key=" line.
		{
			name:     "dotenv meter definition",
			relPath:  "wmbusmeters/etc/wmbusmeters.d/meter-0001",
			content:  "name=heater\nid=12345678\nkey=00112233445566778899AABBCCDDEEFF\ndriver=multical21\n",
			wantNeed: true,
		},
		{
			name:     "dotenv with comments and blank lines",
			relPath:  "wmbusmeters/etc/wmbusmeters.d/meter-0002",
			content:  "# the kitchen meter\nname=water\n\nkey=DEADBEEF\n",
			wantNeed: true,
		},
		{
			name:     "extensionless key=value file with nothing secret-shaped",
			relPath:  "wmbusmeters/etc/wmbusmeters.conf",
			content:  "loglevel=normal\ndevice=/dev/ttyUSB0\n",
			wantNeed: false,
		},
		{
			name:     "dotenv secret key with an empty value",
			relPath:  "wmbusmeters/etc/wmbusmeters.d/meter-0003",
			content:  "name=gas\nkey=\n",
			wantNeed: false,
		},
		{
			// A flat format gets top-level "sops_*" metadata keys, so a
			// plaintext file carrying them reads as ciphertext.
			name:        "dotenv colliding with the sops metadata prefix is refused",
			relPath:     "wmbusmeters/etc/wmbusmeters.d/meter-0004",
			content:     "sops_mac=mine\nkey=00112233\n",
			wantNeed:    false,
			wantRefusal: true,
		},
		{
			// One non-assignment line disqualifies the whole file, so this
			// can be neither encrypted nor committed as-is.
			name:        "prose alongside a secret assignment is refused",
			relPath:     "NOTES",
			content:     "key=00112233\nthis line is prose\n",
			wantNeed:    false,
			wantRefusal: true,
		},
		{
			name:     "prose alongside a non-secret assignment is ignored",
			relPath:  "NOTES",
			content:  "title=notes\nthis line is prose\n",
			wantNeed: false,
		},
		{
			name:        "indented secret assignment is refused",
			relPath:     "notes",
			content:     "  key=00112233\n",
			wantRefusal: true,
			wantNeed:    false,
		},
		{
			// An extension is a claim about the format, and this package
			// would rather miss a file than argue with one.
			name:     "dotenv content behind an extension is not dotenv",
			relPath:  "config.env",
			content:  "key=00112233445566778899AABBCCDDEEFF\n",
			wantNeed: false,
		},
		{
			name:     "readme with no assignments at all",
			relPath:  "README",
			content:  "This directory holds meter definitions.\n",
			wantNeed: false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			need, refusal := NeedsEncryption(c.relPath, []byte(c.content))
			if need != c.wantNeed {
				t.Errorf("need = %v, want %v", need, c.wantNeed)
			}
			if (refusal != "") != c.wantRefusal {
				t.Errorf("refusal = %q, want refusal: %v", refusal, c.wantRefusal)
			}
			if need && refusal != "" {
				t.Errorf("need and refusal are mutually exclusive, got both (%q)", refusal)
			}
		})
	}
}

// !include directives with no inline secret are the ordinary case, and
// callers turn blockedByTags into a hard failure.
func TestNeedsEncryptionTagsOnlyBlockWhenEncryptionIsNeeded(t *testing.T) {
	content := "automation: !include automations.yaml\nsensor: !include_dir_merge_list sensors/\n"
	need, refusal := NeedsEncryption("configuration.yaml", []byte(content))
	if need || refusal != "" {
		t.Errorf("NeedsEncryption() = (%v, %q), want (false, \"\")", need, refusal)
	}
}

func TestIsYAMLFile(t *testing.T) {
	positives := []string{"configuration.yaml", "packages/mqtt.yml", "SECRETS.YAML", "a/b/c.YmL"}
	for _, p := range positives {
		if !IsYAMLFile(p) {
			t.Errorf("IsYAMLFile(%q) = false, want true", p)
		}
	}
	negatives := []string{"custom_components/foo/__init__.py", "www/config.json", "README", "a.yaml.bak"}
	for _, p := range negatives {
		if IsYAMLFile(p) {
			t.Errorf("IsYAMLFile(%q) = true, want false", p)
		}
	}
}

// --- Format detection ------------------------------------------------------

func TestFormatFor(t *testing.T) {
	const meter = "name=heater\nkey=00112233\n"
	cases := []struct {
		relPath string
		content string
		want    Format
	}{
		{"configuration.yaml", "a: 1\n", FormatYAML},
		{"packages/mqtt.YML", "a: 1\n", FormatYAML},
		{"www/config.json", `{"a": 1}`, FormatJSON},
		{"www/config.JSON", `{"a": 1}`, FormatJSON},
		{"wmbusmeters/etc/wmbusmeters.d/meter-0001", meter, FormatDotenv},
		{"meter-0001", "key=abc\n", FormatDotenv},
		// Every non-blank line has to be an assignment.
		{"meter-0001", "key=abc\nprose here\n", FormatNone},
		// And one has to look like a secret, or this is a line-oriented
		// file that would become a binary blob.
		{"units", "loglevel=normal\n", FormatNone},
		// An extension is a claim about the format.
		{"config.env", meter, FormatNone},
		{"meter.txt", meter, FormatNone},
		{"custom_components/foo/__init__.py", "PASSWORD = 'x'\n", FormatNone},
		{"README", "prose\n", FormatNone},
		{"empty", "", FormatNone},
	}
	for _, c := range cases {
		if got := FormatFor(c.relPath, []byte(c.content)); got != c.want {
			t.Errorf("FormatFor(%q, %q) = %q, want %q", c.relPath, c.content, got, c.want)
		}
	}
}

// Both call sites drop a path entirely on a false, so a false negative is
// a file that silently stops being compared or guarded.
func TestEncryptablePathIsASupersetOfFormatFor(t *testing.T) {
	const meter = "name=heater\nkey=00112233\n"
	for _, p := range []string{
		"configuration.yaml", "packages/mqtt.yml", "SECRETS.YAML",
		"www/config.json", "wmbusmeters/etc/wmbusmeters.d/meter-0001", "README",
	} {
		if !EncryptablePath(p) {
			t.Errorf("EncryptablePath(%q) = false, want true", p)
		}
	}
	for _, p := range []string{
		"custom_components/foo/__init__.py", "www/logo.png", "home-assistant_v2.db", "a.yaml.bak",
	} {
		if EncryptablePath(p) {
			t.Errorf("EncryptablePath(%q) = true, want false", p)
		}
		if got := FormatFor(p, []byte(meter)); got != FormatNone {
			t.Errorf("FormatFor(%q) = %q on a path the pre-filter drops", p, got)
		}
	}
}

func TestIsSecretsFile(t *testing.T) {
	positives := []string{"secrets.yaml", "secrets.yml", "SECRETS.YAML", "sub/dir/secrets.yaml"}
	for _, p := range positives {
		if !IsSecretsFile(p) {
			t.Errorf("IsSecretsFile(%q) = false, want true", p)
		}
	}
	negatives := []string{"secrets.json", "my_secrets.yaml", "secrets", "automations.yaml", "secrets.yaml.bak"}
	for _, p := range negatives {
		if IsSecretsFile(p) {
			t.Errorf("IsSecretsFile(%q) = true, want false", p)
		}
	}
}

// --- IsEncrypted ----------------------------------------------------------

func TestIsEncryptedGoldenFixture(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "encrypted-secrets.yaml"))
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	if !IsEncrypted(data) {
		t.Error("IsEncrypted(fixture) = false, want true")
	}
	if !strings.Contains(string(data), "ENC[") {
		t.Error("fixture does not look encrypted; regenerate it")
	}
	if strings.Contains(string(data), "hunter2") {
		t.Error("fixture leaks its plaintext")
	}
}

func TestIsEncryptedRejectsPlaintext(t *testing.T) {
	// The last two are why the substring sniff is confirmed by a parse:
	// reading either as ciphertext leaves a plaintext secret alone.
	negatives := []string{
		"",
		"mqtt_password: hunter2\n",
		"- id: demo\n  alias: Demo\n",
		"# encrypted with sops, honest\nmqtt_password: hunter2\n",
		"notes: we should look at sops one day\nmac: aa:bb:cc\nversion: 3\n",
		"sops: not-a-mapping\n",
		"sops:\n  version: 3.9.0\n",
	}
	for _, content := range negatives {
		if IsEncrypted([]byte(content)) {
			t.Errorf("IsEncrypted(%q) = true, want false", content)
		}
	}
}

// dotenvCiphertext is what sops 3.13.2 writes for a dotenv file, values
// shortened: flat metadata as top-level "sops_*" assignments.
const dotenvCiphertext = `name=heater
id=12345678
key=ENC[AES256_GCM,data:K66sD2+APh2DIY4=,iv:fsWS9/HpPmviCKtm=,tag:Qg7fT5p4DQBuHZIM4JysTA==,type:str]
sops_age__list_0__map_recipient=age1nc00ku2uxypny8vegyp8sh97gfyem6lftqgzmn6zjghaqsaa44fs0wkp8d
sops_encrypted_regex=` + SecretKeyRegex + `
sops_lastmodified=2026-08-04T11:02:58Z
sops_mac=ENC[AES256_GCM,data:Fm6FosEeS7SwmYJS=,iv:z4gxLI0O8dPcT9ov=,tag:7SPRoTm52PtRZdbiHQP0jA==,type:str]
sops_version=3.13.2
`

// A false here is unrecoverable downstream: the differ compares ciphertext
// against live and the applier writes ENC[...] into the config every cycle.
func TestIsEncryptedRecognisesDotenvCiphertext(t *testing.T) {
	if !IsEncrypted([]byte(dotenvCiphertext)) {
		t.Fatal("IsEncrypted(dotenv ciphertext) = false; the differ would compare ciphertext against live plaintext forever and the applier would write ENC[...] into the config")
	}
	// Half the metadata is not a sops document: reading a plaintext file as
	// ciphertext leaves a secret alone as if already protected.
	negatives := []string{
		"name=heater\nkey=00112233\n",
		"sops_mac=ENC[AES256_GCM,data:x,type:str]\nkey=00112233\n",
		"sops_version=3.13.2\nkey=00112233\n",
		"sops_mac=\nsops_version=\nkey=00112233\n",
		"# sops encrypted this, honest\nkey=00112233\n",
	}
	for _, content := range negatives {
		if IsEncrypted([]byte(content)) {
			t.Errorf("IsEncrypted(%q) = true, want false", content)
		}
	}
}

// UnsupportedKeySource returns early on anything not encrypted, so it must
// read the flat spelling too or a planted hc_vault address reaches sops.
func TestUnsupportedKeySourceReadsFlatMetadata(t *testing.T) {
	base := "key=ENC[AES256_GCM,data:x,type:str]\nsops_mac=ENC[AES256_GCM,data:y,type:str]\nsops_version=3.13.2\n"
	cases := []struct {
		name string
		line string
		want string
	}{
		{"top-level list", "sops_hc_vault__list_0__map_vault_address=https://vault.attacker.example\n", "hc_vault"},
		{"kms", "sops_kms__list_0__map_arn=arn:aws:kms:eu-west-1:1:key/abc\n", "kms"},
		{"pgp inside key_groups", "sops_key_groups__list_0__map_pgp__list_0__map_fp=DEADBEEF\n", "pgp"},
		{"age only", "sops_age__list_0__map_recipient=" + testRecipient + "\n", ""},
		// Whole segments, not substrings: "gcp_kms" must not answer "kms".
		{"gcp_kms is not kms", "sops_gcp_kms__list_0__map_resource_id=projects/x\n", "gcp_kms"},
		// A value cannot name a backend into existence.
		{"backend named in a value", "sops_age__list_0__map_enc=map_pgp__is_not_a_key\n", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := UnsupportedKeySource([]byte(base + c.line)); got != c.want {
				t.Errorf("UnsupportedKeySource() = %q, want %q", got, c.want)
			}
		})
	}
}

// --- SemanticallyEqual on dotenv -------------------------------------------

// Both directions this predicate can be wrong in, each with its own
// permanent failure mode.
func TestSemanticallyEqualDotenv(t *testing.T) {
	cases := []struct {
		name string
		a    string
		b    string
		want bool
	}{
		{
			// sops keeps a dotenv file's comments but drops blank lines;
			// calling that a change means a commit on every import.
			name: "a blank line sops dropped is not a change",
			a:    "name=heater\nkey=00112233\n",
			b:    "name=heater\n\nkey=00112233\n",
			want: true,
		},
		{
			// The YAML decoder folds a dotenv file into one scalar joined
			// by spaces, so these compared equal before this branch.
			name: "line breaks are not whitespace",
			a:    "a=1\nb=2\n",
			b:    "a=1 b=2\n",
			want: false,
		},
		{
			name: "a changed value is a change",
			a:    "name=heater\nkey=00112233\n",
			b:    "name=heater\nkey=44556677\n",
			want: false,
		},
		{
			name: "a removed assignment is a change",
			a:    "name=heater\nkey=00112233\n",
			b:    "name=heater\n",
			want: false,
		},
		{
			// A dotenv file may repeat a key, so a set comparison would
			// call these the same file.
			name: "reordered assignments are a change",
			a:    "a=1\na=2\n",
			b:    "a=2\na=1\n",
			want: false,
		},
		{
			name: "dotenv against something that is not dotenv",
			a:    "name=heater\nkey=00112233\n",
			b:    "name: heater\nkey: 00112233\n",
			want: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := SemanticallyEqual([]byte(c.a), []byte(c.b)); got != c.want {
				t.Errorf("SemanticallyEqual(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
			}
		})
	}
}

// sops does not round-trip JSON byte-for-byte either: it re-indents, drops
// a trailing zero from 0.00 and resolves \u escapes.
func TestSemanticallyEqualJSON(t *testing.T) {
	// Live carries a JSON \u escape, decrypted the character itself,
	// written as a Go escape to keep this file ASCII.
	live := "{\n  \"type\": \"service_account\",\n  \"f\": 0.00,\n  \"u\": \"caf\\u00e9\"\n}\n"
	decrypted := "{\n\t\"type\": \"service_account\",\n\t\"f\": 0,\n\t\"u\": \"caf\u00e9\"\n}\n"
	if !SemanticallyEqual([]byte(decrypted), []byte(live)) {
		t.Error("SemanticallyEqual() = false for a JSON file sops only reformatted, which is permanent drift")
	}
	changed := "{\n\t\"type\": \"user_account\",\n\t\"f\": 0,\n\t\"u\": \"caf\u00e9\"\n}\n"
	if SemanticallyEqual([]byte(changed), []byte(live)) {
		t.Error("SemanticallyEqual() = true for a JSON file whose value changed, which is missed drift")
	}
}

// --- .sops.yaml -----------------------------------------------------------

func TestSopsConfigCarriesRecipientAndBothRules(t *testing.T) {
	c, err := New(testIdentity)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got := string(c.SopsConfig())

	for _, want := range []string{
		"creation_rules:",
		`path_regex: (^|/)secrets\.ya?ml$`,
		`path_regex: \.ya?ml$`,
		`path_regex: \.json$`,
		"encrypted_regex: '" + SecretKeyRegex + "'",
		"age: " + testRecipient,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("SopsConfig() = %q, want it to contain %q", got, want)
		}
	}
	if strings.Contains(got, testIdentity) {
		t.Fatal("SopsConfig() carries the private key")
	}
	// The secrets.yaml rule must come first, or the generic ".ya?ml" rule
	// encrypts only its secret-shaped keys.
	if strings.Index(got, `(^|/)secrets`) > strings.Index(got, `path_regex: \.ya?ml$`) {
		t.Error("the secrets.yaml rule must precede the generic YAML rule")
	}
	// No rule may match an extensionless path: a .sops.yaml cannot set the
	// input type, so a match turns a clean refusal into a binary blob.
	if strings.Contains(got, "[^/.]") || strings.Contains(got, "path_regex: .*") {
		t.Errorf("SopsConfig() has a rule that could match an extensionless dotenv path:\n%s", got)
	}
}

// --- real sops, when the binary is available ------------------------------

// A fake Runner agrees with flags sops does not have, and the failure would
// surface only as a refusal to start on a user's install.
func TestProbeWithRealSops(t *testing.T) {
	if _, err := exec.LookPath("sops"); err != nil {
		t.Skip("sops binary not on PATH")
	}
	c, err := New(testIdentity)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := c.Probe(context.Background()); err != nil {
		t.Fatalf("Probe: %v", err)
	}
}

// realSopsCrypter returns a Crypter on a throwaway per-run identity, and
// skips when sops is absent: it is in the add-on image, not every runner.
func realSopsCrypter(t *testing.T) *Crypter {
	t.Helper()
	if _, err := exec.LookPath("sops"); err != nil {
		t.Skip("sops binary not on PATH")
	}
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("GenerateX25519Identity: %v", err)
	}
	c, err := New(identity.String())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

// The first of the tests here that run the actual binary.
func TestEncryptDecryptRoundTripWithRealSops(t *testing.T) {
	c := realSopsCrypter(t)

	dir := t.TempDir()
	plaintext := "mqtt:\n  broker: 10.0.0.1\n  password: hunter2\nnotify_url: https://example.invalid\n"
	abs := filepath.Join(dir, "mqtt.yaml")
	if err := os.WriteFile(abs, []byte(plaintext), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	if err := c.EncryptFileInPlace(ctx, abs, "packages/mqtt.yaml"); err != nil {
		t.Fatalf("EncryptFileInPlace: %v", err)
	}

	encrypted, err := os.ReadFile(abs) // #nosec G304 -- t.TempDir() fixture path this test wrote
	if err != nil {
		t.Fatal(err)
	}
	body := string(encrypted)
	if strings.Contains(body, "hunter2") {
		t.Error("the secret value survived in the clear")
	}
	if !strings.Contains(body, "password: ENC[") {
		t.Errorf("encrypted file = %q, want the password value replaced by ENC[...]", body)
	}
	// Values-only, not whole-file: this is what keeps a config repo
	// reviewable, and what distinguishes this from encrypting secrets.yaml.
	if !strings.Contains(body, "broker: 10.0.0.1") {
		t.Errorf("encrypted file = %q, want non-secret values left readable", body)
	}
	if !strings.Contains(body, "notify_url: https://example.invalid") {
		t.Errorf("encrypted file = %q, want non-secret values left readable", body)
	}
	if !IsEncrypted(encrypted) {
		t.Error("IsEncrypted(real sops output) = false, want true")
	}

	decrypted, err := c.DecryptFile(ctx, abs)
	if err != nil {
		t.Fatalf("DecryptFile: %v", err)
	}
	if !strings.Contains(string(decrypted), "password: hunter2") {
		t.Errorf("decrypted = %q, want the original secret back", decrypted)
	}
	if IsEncrypted(decrypted) {
		t.Error("IsEncrypted(decrypted) = true, want false")
	}
}

// The other encryption shape: every value gone, including the ones whose
// keys look innocuous.
func TestSecretsFileRoundTripWithRealSops(t *testing.T) {
	c := realSopsCrypter(t)

	dir := t.TempDir()
	plaintext := "mqtt_user: bob\nmqtt_password: hunter2\nlatitude: 52.1\n"
	abs := filepath.Join(dir, "secrets.yaml")
	if err := os.WriteFile(abs, []byte(plaintext), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	if err := c.EncryptFileInPlace(ctx, abs, "secrets.yaml"); err != nil {
		t.Fatalf("EncryptFileInPlace: %v", err)
	}
	encrypted, err := os.ReadFile(abs) // #nosec G304 -- t.TempDir() fixture path this test wrote
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"bob", "hunter2", "52.1"} {
		if strings.Contains(string(encrypted), secret) {
			t.Errorf("value %q survived in the clear: secrets.yaml must be encrypted whole", secret)
		}
	}

	decrypted, err := c.DecryptFile(ctx, abs)
	if err != nil {
		t.Fatalf("DecryptFile: %v", err)
	}
	// Flat scalar mappings are the one shape sops re-serializes byte for
	// byte, which is what gitsync's anti-churn check rests on here.
	if string(decrypted) != plaintext {
		t.Errorf("decrypted = %q, want %q byte for byte", decrypted, plaintext)
	}
}

// The GCP service account key the live config leaked is a .json whose
// "private_key" matched already; only the extension kept it in the clear.
func TestJSONRoundTripWithRealSops(t *testing.T) {
	c := realSopsCrypter(t)

	dir := t.TempDir()
	plaintext := "{\n  \"type\": \"service_account\",\n  \"project_id\": \"demo\",\n" +
		"  \"private_key\": \"-----BEGIN PRIVATE KEY-----\\nhunter2\\n-----END PRIVATE KEY-----\\n\",\n" +
		"  \"client_email\": \"a@b.example\"\n}\n"
	abs := filepath.Join(dir, "HAandGHome.json")
	if err := os.WriteFile(abs, []byte(plaintext), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	if err := c.EncryptFileInPlace(ctx, abs, "includes/HAandGHome.json"); err != nil {
		t.Fatalf("EncryptFileInPlace: %v", err)
	}
	encrypted, err := os.ReadFile(abs) // #nosec G304 -- t.TempDir() fixture path this test wrote
	if err != nil {
		t.Fatal(err)
	}
	body := string(encrypted)
	if strings.Contains(body, "hunter2") {
		t.Errorf("the private key survived in the clear:\n%s", body)
	}
	// Values-only here too: readable project and client email keep the file
	// reviewable in a pull request.
	for _, readable := range []string{"service_account", "demo", "a@b.example"} {
		if !strings.Contains(body, readable) {
			t.Errorf("non-secret value %q was encrypted too:\n%s", readable, body)
		}
	}
	if !IsEncrypted(encrypted) {
		t.Error("IsEncrypted(encrypted json) = false, want true")
	}

	decrypted, err := c.DecryptFile(ctx, abs)
	if err != nil {
		t.Fatalf("DecryptFile: %v", err)
	}
	if !strings.Contains(string(decrypted), "hunter2") {
		t.Errorf("decrypted = %q, want the original secret back", decrypted)
	}
	// Not byte-identical: sops re-indents JSON, so the anti-churn
	// comparison has to be the semantic one.
	if !SemanticallyEqual(decrypted, []byte(plaintext)) {
		t.Errorf("decrypted json is not semantically equal to the original, which is permanent drift:\n%s", decrypted)
	}
}

// The format nothing in the path announces: extensionless meter files
// holding a wM-Bus AES key. Every step had to be spelled out, and failure
// is silent.
func TestDotenvRoundTripWithRealSops(t *testing.T) {
	c := realSopsCrypter(t)

	dir := t.TempDir()
	plaintext := "# the kitchen meter\nname=MyHeater\ndriver=multical21\n\nid=12345678\nkey=00112233445566778899AABBCCDDEEFF\n"
	abs := filepath.Join(dir, "meter-0001")
	if err := os.WriteFile(abs, []byte(plaintext), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	rel := "wmbusmeters/etc/wmbusmeters.d/meter-0001"
	if err := c.EncryptFileInPlace(ctx, abs, rel); err != nil {
		t.Fatalf("EncryptFileInPlace: %v", err)
	}
	encrypted, err := os.ReadFile(abs) // #nosec G304 -- t.TempDir() fixture path this test wrote
	if err != nil {
		t.Fatal(err)
	}
	body := string(encrypted)
	if strings.Contains(body, "00112233445566778899AABBCCDDEEFF") {
		t.Errorf("the meter key survived in the clear:\n%s", body)
	}
	// Values-only, which here also proves sops did not fall back to its
	// binary store and swallow the whole file into one base64 blob.
	for _, readable := range []string{"name=MyHeater", "driver=multical21", "id=12345678"} {
		if !strings.Contains(body, readable) {
			t.Errorf("expected %q to stay readable; sops may have encrypted the whole file as binary:\n%s", readable, body)
		}
	}
	if strings.Contains(body, `"data"`) {
		t.Errorf("the file was encrypted as a binary blob rather than value by value:\n%s", body)
	}
	if !IsEncrypted(encrypted) {
		t.Fatalf("IsEncrypted(encrypted dotenv) = false; the differ would never decrypt this file:\n%s", body)
	}

	decrypted, err := c.DecryptFile(ctx, abs)
	if err != nil {
		t.Fatalf("DecryptFile: %v", err)
	}
	if !strings.Contains(string(decrypted), "key=00112233445566778899AABBCCDDEEFF") {
		t.Errorf("decrypted = %q, want the original secret back", decrypted)
	}
	// sops keeps this file's comments but drops its blank line, so a byte
	// comparison would re-encrypt and commit on every cycle.
	if bytes.Equal(decrypted, []byte(plaintext)) {
		t.Log("note: this sops round-tripped the dotenv file byte for byte; the semantic comparison is still what the import relies on")
	}
	if !SemanticallyEqual(decrypted, []byte(plaintext)) {
		t.Errorf("decrypted dotenv is not semantically equal to the original, which is permanent drift:\n%s", decrypted)
	}
}

// --- review: repository content must not steer where sops connects ------

func TestUnsupportedKeySourceSpotsRemoteBackends(t *testing.T) {
	const body = "mqtt:\n    password: ENC[AES256_GCM,data:Zm9v,iv:YmFy,tag:YmF6,type:str]\n"
	meta := func(inner string) []byte {
		return []byte(body + "sops:\n" + inner + "    mac: ENC[AES256_GCM,data:bWFj,iv:aXY=,tag:dGFn,type:str]\n    version: 3.13.2\n")
	}

	cases := []struct {
		name string
		doc  []byte
		want string
	}{
		{
			name: "age only is what the agent writes",
			doc:  meta("    age:\n        - recipient: " + testRecipient + "\n          enc: x\n"),
			want: "",
		},
		{
			name: "empty backend lists are written by sops itself",
			doc:  meta("    kms: []\n    gcp_kms: []\n    azure_kv: []\n    hc_vault: []\n    pgp: []\n"),
			want: "",
		},
		{
			name: "hc_vault reaches a repository-chosen URL",
			doc:  meta("    hc_vault:\n        - vault_address: http://127.0.0.1:9/\n          enc: x\n"),
			want: "hc_vault",
		},
		{
			name: "kms",
			doc:  meta("    kms:\n        - arn: arn:aws:kms:eu-west-1:1:key/x\n          enc: x\n"),
			want: "kms",
		},
		{
			name: "hidden one level down in key_groups",
			doc:  meta("    key_groups:\n        - hc_vault:\n            - vault_address: http://127.0.0.1:9/\n              enc: x\n"),
			want: "hc_vault",
		},
		{
			name: "plaintext config is not a sops document at all",
			doc:  []byte("mqtt:\n  password: hunter2\n"),
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := UnsupportedKeySource(tc.doc); got != tc.want {
				t.Errorf("UnsupportedKeySource() = %q, want %q", got, tc.want)
			}
		})
	}
}

// The refusal only counts before the subprocess: running sops is what
// makes the outbound request.
func TestDecryptRefusesARemoteBackendWithoutRunningSops(t *testing.T) {
	c, fr := newTestCrypter(t)

	path := filepath.Join(t.TempDir(), "secrets.yaml")
	doc := "a: ENC[AES256_GCM,data:Zm9v,iv:YmFy,tag:YmF6,type:str]\n" +
		"sops:\n    hc_vault:\n        - vault_address: http://127.0.0.1:9/\n          enc: x\n" +
		"    mac: ENC[AES256_GCM,data:bWFj,iv:aXY=,tag:dGFn,type:str]\n    version: 3.13.2\n"
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := c.DecryptFile(context.Background(), path)
	if err == nil {
		t.Fatal("DecryptFile() = nil error, want a refusal for a non-age master key")
	}
	if !strings.Contains(err.Error(), "hc_vault") {
		t.Errorf("error = %v, want it to name the backend", err)
	}
	if len(fr.calls) != 0 {
		t.Errorf("sops ran anyway (%d calls), so the outbound request was still made", len(fr.calls))
	}
}

// --- VM e2e: found on real hardware, 2026-08-04 ---------------------------

// sops does not preserve how a number was written: "default: 0.00" comes
// back as "0" and the file then reported drift on every cycle.
func TestSemanticallyEqualIgnoresNumericRepresentation(t *testing.T) {
	equal := []struct{ name, a, b string }{
		{"float zero rewritten as int", "fields:\n  price:\n    default: 0.00\n", "fields:\n  price:\n    default: 0\n"},
		{"trailing zeroes", "a: 1.50\n", "a: 1.5\n"},
		{"int written as float", "a: 2\n", "a: 2.0\n"},
		{"nested in a list", "x:\n  - v: 0.00\n", "x:\n  - v: 0\n"},
	}
	for _, tc := range equal {
		t.Run(tc.name, func(t *testing.T) {
			if !SemanticallyEqual([]byte(tc.a), []byte(tc.b)) {
				t.Errorf("SemanticallyEqual(%q, %q) = false, want true: this is permanent phantom drift", tc.a, tc.b)
			}
		})
	}

	different := []struct{ name, a, b string }{
		{"different numbers", "a: 1\n", "a: 2\n"},
		{"different floats", "a: 1.5\n", "a: 1.6\n"},
		{"number against string", "a: 1\n", "a: \"1\"\n"},
		{"large ints that float64 would round together", "a: 9007199254740993\n", "a: 9007199254740992\n"},
		{"missing key", "a: 1\nb: 2\n", "a: 1\n"},
		{"extra key", "a: 1\n", "a: 1\nb: 2\n"},
		{"list order", "a: [1, 2]\n", "a: [2, 1]\n"},
	}
	for _, tc := range different {
		t.Run(tc.name, func(t *testing.T) {
			if SemanticallyEqual([]byte(tc.a), []byte(tc.b)) {
				t.Errorf("SemanticallyEqual(%q, %q) = true, want false: a real change would be silently dropped", tc.a, tc.b)
			}
		})
	}
}

// A meter file stops being dotenv-shaped over an added note, an indent or
// an "export", and FormatFor then places it nowhere: a leak reachable by
// an ordinary content edit, where the YAML equivalent needs a rename.
func TestExtensionlessSecretThatIsNotDotenvIsRefused(t *testing.T) {
	const meter = "wmbusmeters/etc/wmbusmeters.d/meter-0001"

	refused := []struct{ name, content string }{
		{"prose line added", "name=meter one\nkey=00112233445566778899AABBCCDDEEFF\nthis one is in the basement\n"},
		{"indented assignment", "name=meter one\n  key=00112233445566778899AABBCCDDEEFF\n"},
		{"export prefix", "export key=00112233445566778899AABBCCDDEEFF\n"},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			need, refusal := NeedsEncryption(meter, []byte(tc.content))
			if need {
				t.Errorf("need = true, but the format is not one sops can be pointed at")
			}
			if refusal == "" {
				t.Error("no refusal: the secret would be committed in the clear")
			}
		})
	}

	// The other half: a refusal aborts the whole operation, so innocent
	// content must not trip one.
	ignored := []struct{ name, content string }{
		{"prose only", "hello world\nsome notes\n"},
		{"non-secret assignment amid prose", "title=notes\njust prose here\n"},
		{"empty", ""},
		{"comments only", "# nothing here\n"},
	}
	for _, tc := range ignored {
		t.Run(tc.name, func(t *testing.T) {
			need, refusal := NeedsEncryption(meter, []byte(tc.content))
			if need || refusal != "" {
				t.Errorf("need = %v, refusal = %q, want both empty: this file holds no secret", need, refusal)
			}
		})
	}

	// A well-formed meter file is still encrypted, not refused.
	need, refusal := NeedsEncryption(meter, []byte("name=meter one\nkey=00112233445566778899AABBCCDDEEFF\n"))
	if !need || refusal != "" {
		t.Errorf("need = %v, refusal = %q, want (true, \"\") for a proper dotenv meter file", need, refusal)
	}
}

// A .json parsing as neither JSON nor YAML falls to the line scan, which
// cannot see a key behind a brace: one was committed in the clear and its
// delete diff published the plaintext on the dashboard.
func TestMalformedCompactJSONWithASecretIsRefused(t *testing.T) {
	refused := []struct{ name, content string }{
		{"compact object with trailing junk", `{"password":"hunter2"} trailing`},
		{"ndjson", "{\"password\":\"a\"}\n{\"password\":\"b\"}"},
		{"truncated pretty json", "{\n  \"private_key\": \"-----BEGIN PRIVATE KEY-----\",\n"},
		{"compact with several keys", `{"host":"x","api_key":"hunter2"} oops`},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			need, refusal := NeedsEncryption("includes/a.json", []byte(tc.content))
			if need {
				t.Error("need = true for a file no parser accepts")
			}
			if refusal == "" {
				t.Error("no refusal: the secret would be committed in the clear and published in a delete diff")
			}
		})
	}

	ignored := []struct{ name, content string }{
		{"malformed but nothing secret", `{"host":"10.0.0.1"} trailing`},
		{"prose", "not json at all"},
	}
	for _, tc := range ignored {
		t.Run(tc.name, func(t *testing.T) {
			if _, refusal := NeedsEncryption("includes/a.json", []byte(tc.content)); refusal != "" {
				t.Errorf("refusal = %q, want none: refusing this would halt the whole import", refusal)
			}
		})
	}
}

// sops reads a BOM as part of the first key, so --encrypted-regex cannot
// match it and the value stays clear while the agent reports success.
func TestBOMedDotenvSecretIsRefusedByName(t *testing.T) {
	content := append([]byte{0xEF, 0xBB, 0xBF}, []byte("name=meter one\nkey=00112233445566778899AABBCCDDEEFF\n")...)

	need, refusal := NeedsEncryption("wmbusmeters/etc/wmbusmeters.d/meter-0001", content)
	if need {
		t.Error("need = true, but sops would mis-key the first setting")
	}
	if !strings.Contains(refusal, "byte order mark") {
		t.Errorf("refusal = %q, want it to name the byte order mark", refusal)
	}
}

// gitsync's import copies live .gitignore files through the encrypting
// path. filepath.Ext sees an extension, so one is never mistaken for an
// extensionless KEY=value file, whose content it can otherwise resemble.
func TestGitignoreIsNeverEncryptable(t *testing.T) {
	body := []byte("# secrets\npassword=secret\ncustom_components/\n")
	for _, p := range []string{".gitignore", "esphome/.gitignore", "sub/dir/.gitignore"} {
		if EncryptablePath(p) {
			t.Errorf("EncryptablePath(%q) = true, want false", p)
		}
		if f := FormatFor(p, body); f != FormatNone {
			t.Errorf("FormatFor(%q) = %v, want FormatNone", p, f)
		}
		need, refusal := NeedsEncryption(p, body)
		if need || refusal != "" {
			t.Errorf("NeedsEncryption(%q) = (%v, %q), want (false, \"\")", p, need, refusal)
		}
	}
}
