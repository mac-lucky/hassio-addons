package differ

import (
	"errors"
	"strings"
	"testing"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/gitsync"
)

// sopsDoc is the minimal shape sopscrypt.IsEncrypted recognizes: a
// top-level "sops" mapping carrying a mac and a version.
const sopsDoc = `mqtt:
    password: ENC[AES256_GCM,data:Zm9v,iv:YmFy,tag:YmF6,type:str]
sops:
    mac: ENC[AES256_GCM,data:bWFj,iv:aXY=,tag:dGFn,type:str]
    version: 3.9.0
`

// decryptTo returns a transform that hands back plain as encrypted content,
// recording its calls so a test can prove only the repo side was
// transformed.
func decryptTo(plain string, seen *[]string) RepoTransform {
	return func(rel string, data []byte) ([]byte, bool, error) {
		if seen != nil {
			*seen = append(*seen, string(data))
		}
		return []byte(plain), true, nil
	}
}

// withEncryptionOn flips gitsync's process-wide encryption switch for one
// test - with it off, secrets.yaml is excluded and never reaches Compute.
func withEncryptionOn(t *testing.T) {
	t.Helper()
	gitsync.SetEncryptionEnabled(true)
	t.Cleanup(func() { gitsync.SetEncryptionEnabled(false) })
}

func TestTransformAppliedToRepoSideOnly(t *testing.T) {
	repoRoot, configRoot := dirs(t)
	write(t, repoRoot, "packages/demo.yaml", []byte("ciphertext-in-repo\n"))
	write(t, configRoot, "packages/demo.yaml", []byte("live: plaintext\n"))

	var seen []string
	changes, _, failures := Compute(
		repoRoot, configRoot,
		[]string{"packages/demo.yaml"},
		nil,
		decryptTo("live: decrypted\n", &seen),
	)

	if len(failures) != 0 {
		t.Fatalf("decryptFailures = %+v, want none", failures)
	}
	if len(seen) != 1 || seen[0] != "ciphertext-in-repo\n" {
		t.Fatalf("transform inputs = %+v, want exactly the repo bytes", seen)
	}
	if len(changes) != 1 {
		t.Fatalf("len(changes) = %d, want 1", len(changes))
	}
	if !strings.Contains(changes[0].DiffText, "-live: plaintext") {
		t.Errorf("diff_text = %q, want the untransformed live side", changes[0].DiffText)
	}
}

// sops re-emits a decrypted file from its own parse, re-indenting it.
// Comparing raw bytes would report every encrypted file as drifted.
func TestReindentedDecryptionIsNotDrift(t *testing.T) {
	live := "mqtt:\n  server: mqtt://localhost\n  password: hunter2\n"
	reindented := "mqtt:\n    server: mqtt://localhost\n    password: hunter2\n"

	repoRoot, configRoot := dirs(t)
	write(t, repoRoot, "zigbee2mqtt/configuration.yaml", []byte(sopsDoc))
	write(t, configRoot, "zigbee2mqtt/configuration.yaml", []byte(live))

	changes, _, failures := Compute(
		repoRoot, configRoot,
		[]string{"zigbee2mqtt/configuration.yaml"},
		nil,
		decryptTo(reindented, nil),
	)

	if len(failures) != 0 {
		t.Fatalf("decryptFailures = %+v, want none", failures)
	}
	if len(changes) != 0 {
		t.Fatalf("changes = %+v, want none (indentation alone is not drift)", changes)
	}
}

// An unencrypted file differing by indentation alone IS drift: the
// semantic compare is a concession to sops, not a new definition.
func TestSemanticCompareOnlyForEncryptedFiles(t *testing.T) {
	repoRoot, configRoot := dirs(t)
	write(t, repoRoot, "packages/demo.yaml", []byte("mqtt:\n    server: x\n"))
	write(t, configRoot, "packages/demo.yaml", []byte("mqtt:\n  server: x\n"))

	plain := func(rel string, data []byte) ([]byte, bool, error) { return data, false, nil }
	changes, _, _ := Compute(repoRoot, configRoot, []string{"packages/demo.yaml"}, nil, plain)

	if len(changes) != 1 {
		t.Fatalf("len(changes) = %d, want 1", len(changes))
	}
}

// The leak test: DiffText reaches the dashboard, /status.json and statusd
// verbatim, so a changed secret shows as a change and never as its value.
func TestGenuineSecretChangeIsReportedWithoutTheSecret(t *testing.T) {
	live := "mqtt:\n  server: mqtt://localhost\n  password: OLDSECRETVALUE\n"
	decrypted := "mqtt:\n  server: mqtt://localhost\n  password: NEWSECRETVALUE\n"

	repoRoot, configRoot := dirs(t)
	write(t, repoRoot, "zigbee2mqtt/configuration.yaml", []byte(sopsDoc))
	write(t, configRoot, "zigbee2mqtt/configuration.yaml", []byte(live))

	changes, _, _ := Compute(
		repoRoot, configRoot,
		[]string{"zigbee2mqtt/configuration.yaml"},
		nil,
		decryptTo(decrypted, nil),
	)

	if len(changes) != 1 {
		t.Fatalf("len(changes) = %d, want 1", len(changes))
	}
	c := changes[0]
	if c.Kind != "update" {
		t.Errorf("kind = %q, want update", c.Kind)
	}
	if c.DiffText != encryptedSummary {
		t.Errorf("diff_text = %q, want the %q summary", c.DiffText, encryptedSummary)
	}
	for _, secret := range []string{"OLDSECRETVALUE", "NEWSECRETVALUE"} {
		if strings.Contains(c.DiffText, secret) {
			t.Fatalf("diff_text leaked %q: %q", secret, c.DiffText)
		}
	}
}

// Hiding the secret must not hide the edit next to it, or every change to
// an encrypted file becomes an unreviewable "something changed".
func TestNonSecretEditInEncryptedFileStaysVisible(t *testing.T) {
	live := "mqtt:\n  server: mqtt://old\n  password: KEEPMEHIDDEN\n"
	decrypted := "mqtt:\n  server: mqtt://new\n  password: KEEPMEHIDDEN\n"

	repoRoot, configRoot := dirs(t)
	write(t, repoRoot, "zigbee2mqtt/configuration.yaml", []byte(sopsDoc))
	write(t, configRoot, "zigbee2mqtt/configuration.yaml", []byte(live))

	changes, _, _ := Compute(
		repoRoot, configRoot,
		[]string{"zigbee2mqtt/configuration.yaml"},
		nil,
		decryptTo(decrypted, nil),
	)

	if len(changes) != 1 {
		t.Fatalf("len(changes) = %d, want 1", len(changes))
	}
	c := changes[0]
	if strings.Contains(c.DiffText, "KEEPMEHIDDEN") {
		t.Fatalf("diff_text leaked the secret: %q", c.DiffText)
	}
	if !strings.Contains(c.DiffText, "-  server: mqtt://old") || !strings.Contains(c.DiffText, "+  server: mqtt://new") {
		t.Errorf("diff_text = %q, want the non-secret edit visible", c.DiffText)
	}
	if !strings.Contains(c.DiffText, "password: "+maskMarker) {
		t.Errorf("diff_text = %q, want the secret line masked in context", c.DiffText)
	}
}

func TestBlockScalarSecretMaskedWhole(t *testing.T) {
	live := "tls:\n  private_key: |\n    -----BEGIN PRIVATE KEY-----\n    KEYMATERIALONE\n  verify: true\n"
	decrypted := "tls:\n  private_key: |\n    -----BEGIN PRIVATE KEY-----\n    KEYMATERIALTWO\n  verify: false\n"

	repoRoot, configRoot := dirs(t)
	write(t, repoRoot, "packages/tls.yaml", []byte(sopsDoc))
	write(t, configRoot, "packages/tls.yaml", []byte(live))

	changes, _, _ := Compute(
		repoRoot, configRoot,
		[]string{"packages/tls.yaml"},
		nil,
		decryptTo(decrypted, nil),
	)

	if len(changes) != 1 {
		t.Fatalf("len(changes) = %d, want 1", len(changes))
	}
	c := changes[0]
	for _, secret := range []string{"KEYMATERIALONE", "KEYMATERIALTWO", "BEGIN PRIVATE KEY"} {
		if strings.Contains(c.DiffText, secret) {
			t.Fatalf("diff_text leaked block scalar content %q: %q", secret, c.DiffText)
		}
	}
	if !strings.Contains(c.DiffText, "verify: false") {
		t.Errorf("diff_text = %q, want the non-secret change visible", c.DiffText)
	}
}

func TestSecretsFileMasksEveryValue(t *testing.T) {
	withEncryptionOn(t)

	live := "# secrets\nmqtt_password: LIVEONE\napi_token: LIVETWO\n"
	decrypted := "# secrets\nmqtt_password: REPOONE\napi_token: LIVETWO\nnew_entry: REPOTHREE\n"

	repoRoot, configRoot := dirs(t)
	write(t, repoRoot, "secrets.yaml", []byte(sopsDoc))
	write(t, configRoot, "secrets.yaml", []byte(live))

	changes, _, _ := Compute(repoRoot, configRoot, []string{"secrets.yaml"}, nil, decryptTo(decrypted, nil))

	if len(changes) != 1 {
		t.Fatalf("len(changes) = %d, want 1", len(changes))
	}
	c := changes[0]
	for _, secret := range []string{"LIVEONE", "LIVETWO", "REPOONE", "REPOTHREE"} {
		if strings.Contains(c.DiffText, secret) {
			t.Fatalf("diff_text leaked %q from secrets.yaml: %q", secret, c.DiffText)
		}
	}
	// The added key is structure, not secret: naming it is what makes the
	// diff worth showing at all.
	if !strings.Contains(c.DiffText, "+new_entry: "+maskMarker) {
		t.Errorf("diff_text = %q, want the added key visible with a masked value", c.DiffText)
	}
}

// A delete diff quotes the live file in full, and secrets.yaml only
// reaches this package because encryption made it syncable.
func TestDeletedSecretsFileIsMasked(t *testing.T) {
	withEncryptionOn(t)

	repoRoot, configRoot := dirs(t)
	write(t, configRoot, "secrets.yaml", []byte("mqtt_password: DELETEDSECRET\n"))

	changes, _, _ := Compute(repoRoot, configRoot, nil, []string{"secrets.yaml"}, nil)

	if len(changes) != 1 {
		t.Fatalf("len(changes) = %d, want 1", len(changes))
	}
	c := changes[0]
	if c.Kind != "delete" {
		t.Errorf("kind = %q, want delete", c.Kind)
	}
	if strings.Contains(c.DiffText, "DELETEDSECRET") {
		t.Fatalf("delete diff leaked the live secret: %q", c.DiffText)
	}
	if !strings.Contains(c.DiffText, "mqtt_password: "+maskMarker) {
		t.Errorf("diff_text = %q, want the key visible with a masked value", c.DiffText)
	}
}

func TestAddedEncryptedFileIsMasked(t *testing.T) {
	repoRoot, configRoot := dirs(t)
	write(t, repoRoot, "packages/new.yaml", []byte(sopsDoc))

	changes, _, _ := Compute(
		repoRoot, configRoot,
		[]string{"packages/new.yaml"},
		nil,
		decryptTo("mqtt:\n  server: mqtt://localhost\n  password: BRANDNEWSECRET\n", nil),
	)

	if len(changes) != 1 {
		t.Fatalf("len(changes) = %d, want 1", len(changes))
	}
	c := changes[0]
	if c.Kind != "add" {
		t.Errorf("kind = %q, want add", c.Kind)
	}
	if strings.Contains(c.DiffText, "BRANDNEWSECRET") {
		t.Fatalf("add diff leaked the secret: %q", c.DiffText)
	}
	if !strings.Contains(c.DiffText, "+  server: mqtt://localhost") {
		t.Errorf("diff_text = %q, want the non-secret lines of the new file", c.DiffText)
	}
}

func TestTransformErrorBecomesDecryptFailureNotSilence(t *testing.T) {
	repoRoot, configRoot := dirs(t)
	write(t, repoRoot, "packages/broken.yaml", []byte(sopsDoc))
	write(t, configRoot, "packages/broken.yaml", []byte("live: content\n"))
	write(t, repoRoot, "packages/fine.yaml", []byte("fine: yes\n"))

	failing := func(rel string, data []byte) ([]byte, bool, error) {
		if rel == "packages/broken.yaml" {
			return nil, false, errors.New("sops decrypt failed (exit 1)")
		}
		return data, false, nil
	}

	changes, _, failures := Compute(
		repoRoot, configRoot,
		[]string{"packages/broken.yaml", "packages/fine.yaml"},
		nil,
		failing,
	)

	if len(failures) != 1 {
		t.Fatalf("decryptFailures = %+v, want 1", failures)
	}
	if !strings.Contains(failures[0], "packages/broken.yaml") || !strings.Contains(failures[0], "sops decrypt failed") {
		t.Errorf("decryptFailure = %q, want the path and the reason", failures[0])
	}
	for _, c := range changes {
		if c.Path == "packages/broken.yaml" {
			t.Fatalf("undecryptable path produced a change: %+v", c)
		}
	}
	// The rest of the tree is still diffed - the caller decides what a
	// failure means for the cycle, not this package.
	if len(changes) != 1 || changes[0].Path != "packages/fine.yaml" {
		t.Errorf("changes = %+v, want only packages/fine.yaml", changes)
	}
}

func TestEncryptedRepoContentWithoutTransformIsAFailure(t *testing.T) {
	repoRoot, configRoot := dirs(t)
	write(t, repoRoot, "packages/enc.yaml", []byte(sopsDoc))
	write(t, configRoot, "packages/enc.yaml", []byte("mqtt:\n  password: plaintext\n"))

	changes, _, failures := Compute(repoRoot, configRoot, []string{"packages/enc.yaml"}, nil, nil)

	if len(changes) != 0 {
		t.Fatalf("changes = %+v, want none: ciphertext must never be treated as apply-able plaintext", changes)
	}
	if len(failures) != 1 {
		t.Fatalf("decryptFailures = %+v, want 1", failures)
	}
	if !strings.Contains(failures[0], noAgeKeyReason) {
		t.Errorf("decryptFailure = %q, want it to name the missing age_key", failures[0])
	}
}

func TestPlainRepoContentWithoutTransformIsUnchangedBehaviour(t *testing.T) {
	repoRoot, configRoot := dirs(t)
	write(t, repoRoot, "packages/plain.yaml", []byte("a: 1\n"))
	write(t, configRoot, "packages/plain.yaml", []byte("a: 2\n"))

	changes, _, failures := Compute(repoRoot, configRoot, []string{"packages/plain.yaml"}, nil, nil)

	if len(failures) != 0 {
		t.Fatalf("decryptFailures = %+v, want none", failures)
	}
	if len(changes) != 1 || !strings.Contains(changes[0].DiffText, "+a: 1") {
		t.Errorf("changes = %+v, want an ordinary unmasked diff", changes)
	}
}

// --- masking unit tests -------------------------------------------------

func TestMaskSecretsKeepsStructureAndHidesValues(t *testing.T) {
	in := `# comment
mqtt:
  server: mqtt://localhost
  password: hunter2
  keys:
  - keyone
  - keytwo
  nested:
    api_token: TOKENVALUE
list:
  - id: one
    client_secret: CLIENTSECRETVALUE
`
	masked, ok := maskSecrets([]byte(in), "packages/demo.yaml")
	if !ok {
		t.Fatalf("maskSecrets refused a plain YAML file: %q", masked)
	}
	for _, secret := range []string{"hunter2", "keyone", "keytwo", "TOKENVALUE", "CLIENTSECRETVALUE"} {
		if strings.Contains(masked, secret) {
			t.Fatalf("masked output leaked %q:\n%s", secret, masked)
		}
	}
	for _, keep := range []string{"# comment", "mqtt:", "server: mqtt://localhost", "- id: one"} {
		if !strings.Contains(masked, keep) {
			t.Errorf("masked output dropped %q:\n%s", keep, masked)
		}
	}
	// The same-indent sequence under "keys:" is YAML-legal and must be
	// swallowed by the key it belongs to, not published item by item.
	if strings.Count(masked, maskMarker) != 4 {
		t.Errorf("masked %d values, want 4:\n%s", strings.Count(masked, maskMarker), masked)
	}
}

func TestMaskSecretsHandlesMultiLinePlainScalar(t *testing.T) {
	in := "alias: a very long\n  alias continued here\npassword: hunter2\n"

	masked, ok := maskSecrets([]byte(in), "packages/demo.yaml")
	if !ok {
		t.Fatalf("maskSecrets refused a folded plain scalar: %q", masked)
	}
	if !strings.Contains(masked, "alias continued here") {
		t.Errorf("masked output dropped a non-secret continuation:\n%s", masked)
	}
	if strings.Contains(masked, "hunter2") {
		t.Errorf("masked output leaked the secret:\n%s", masked)
	}
}

func TestMaskSecretsFailsClosed(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"tab indentation", "mqtt:\n\tpassword: hunter2\n"},
		{"secret inside a flow mapping", "mqtt: {password: hunter2}\n"},
		{"secret inside a flow mapping in a sequence item", "- {client_secret: hunter2}\n"},
		{"unclassifiable line at top level", "mqtt:\n  server: x\nnot a yaml line\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if masked, ok := maskSecrets([]byte(tc.in), "packages/demo.yaml"); ok {
				t.Fatalf("maskSecrets accepted %q, want a fail-closed refusal: %q", tc.in, masked)
			}
		})
	}
}

// The shapes that slip past a naive line reader: a quoted key, a value in
// the block under a trailing comment, and a secret buried in a flow
// mapping. Each must end up masked or refused, never published.
func TestMaskSecretsHidesAwkwardlyWrittenSecrets(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"double-quoted key", "\"password\": LEAKME\n"},
		{"single-quoted key", "'client_secret': LEAKME\n"},
		{"value under a trailing comment", "keys: # two of them\n- LEAKME\n- second\n"},
		{"nested block under a trailing comment", "auth: # note\n  password: LEAKME\n"},
		{"flow mapping past the first key", "mqtt: {port: 1, password: LEAKME}\n"},
		{"quoted key inside a flow mapping", "mqtt: {'password': LEAKME}\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			masked, ok := maskSecrets([]byte(tc.in), "packages/demo.yaml")
			if ok && strings.Contains(masked, "LEAKME") {
				t.Fatalf("masked output leaked the secret:\n%s", masked)
			}
		})
	}
}

func TestMaskSecretsAllowsJinjaTemplates(t *testing.T) {
	in := "value_template: \"{{ states('sensor.x') | float }}\"\npassword: hunter2\n"

	masked, ok := maskSecrets([]byte(in), "packages/demo.yaml")
	if !ok {
		t.Fatalf("maskSecrets refused an ordinary Jinja template: %q", masked)
	}
	if !strings.Contains(masked, "states('sensor.x')") {
		t.Errorf("masked output dropped the template:\n%s", masked)
	}
	if strings.Contains(masked, "hunter2") {
		t.Errorf("masked output leaked the secret:\n%s", masked)
	}
}

func TestUnmaskableEncryptedFileFallsBackToSummary(t *testing.T) {
	live := "mqtt: {password: LIVESECRET}\n"
	decrypted := "mqtt: {password: REPOSECRET}\n"

	repoRoot, configRoot := dirs(t)
	write(t, repoRoot, "packages/flow.yaml", []byte(sopsDoc))
	write(t, configRoot, "packages/flow.yaml", []byte(live))

	changes, _, _ := Compute(repoRoot, configRoot, []string{"packages/flow.yaml"}, nil, decryptTo(decrypted, nil))

	if len(changes) != 1 {
		t.Fatalf("len(changes) = %d, want 1", len(changes))
	}
	if changes[0].DiffText != encryptedSummary {
		t.Errorf("diff_text = %q, want the %q summary", changes[0].DiffText, encryptedSummary)
	}
}

func TestYAMLSemanticallyEqual(t *testing.T) {
	cases := []struct {
		name  string
		a, b  string
		equal bool
	}{
		{"reindented nesting", "a:\n  b: 1\n", "a:\n    b: 1\n", true},
		{"requoted scalar", "a: \"x\"\n", "a: x\n", true},
		{"changed value", "a: 1\n", "a: 2\n", false},
		{"string versus int", "a: \"1\"\n", "a: 1\n", false},
		{"block scalar content", "a: |\n  one\n  two\n", "a: |\n  one\n  three\n", false},
		{"block scalar reindented", "a:\n  b: |\n    one\n", "a:\n    b: |\n        one\n", true},
		{"sequence order", "a:\n  - 1\n  - 2\n", "a:\n  - 2\n  - 1\n", false},
		{"multi document stream", "a: 1\n---\nb: 2\n", "a: 1\n---\nb: 3\n", false},
		{"unparseable side", "a: [1\n", "a: [1\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := yamlSemanticallyEqual([]byte(tc.a), []byte(tc.b)); got != tc.equal {
				t.Errorf("yamlSemanticallyEqual(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.equal)
			}
		})
	}
}

// --- review: leak paths found by review, each pinned here ----------------

// YAML accepts a tab after a key's colon and sops encrypts that value, but
// mappingLineRe matches neither form - without the tab check the line fell
// through to the continuation branch and was published verbatim.
func TestMaskSecretsFailsClosedOnTabSeparatedValue(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"tab after the colon, nested", "mqtt:\n  password:\tLEAKME\n"},
		{"tab before the colon, nested", "mqtt:\n  password\t: LEAKME\n"},
		{"tab after the colon, top level", "password:\tLEAKME\n"},
		{"tab inside a deeper continuation", "mqtt:\n  note: x\n    password:\tLEAKME\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			masked, ok := maskSecrets([]byte(tc.in), "packages/demo.yaml")
			if ok {
				t.Fatalf("maskSecrets accepted a tab-separated line, want a fail-closed refusal: %q", masked)
			}
			if strings.Contains(masked, "LEAKME") {
				t.Fatalf("refused output still carried the secret:\n%s", masked)
			}
		})
	}
}

// A comment in secrets.yaml is ciphertext in the repo, so publishing it
// leaks what encryption hid - an old password parked in a comment.
func TestSecretsFileCommentsAreMasked(t *testing.T) {
	in := "# old password was LEAKME\napi_key: hunter2\n"

	masked, ok := maskSecrets([]byte(in), "secrets.yaml")
	if !ok {
		t.Fatalf("maskSecrets refused an ordinary secrets.yaml: %q", masked)
	}
	if strings.Contains(masked, "LEAKME") {
		t.Errorf("comment in secrets.yaml was published in the clear:\n%s", masked)
	}
	if strings.Contains(masked, "hunter2") {
		t.Errorf("masked output leaked the value:\n%s", masked)
	}
}

// The other half: outside secrets.yaml comments are plaintext in the repo
// too, so hiding them costs review value for nothing.
func TestOrdinaryFileCommentsStayVisible(t *testing.T) {
	in := "# broker lives on the NAS\nmqtt:\n  password: hunter2\n"

	masked, ok := maskSecrets([]byte(in), "packages/mqtt.yaml")
	if !ok {
		t.Fatalf("maskSecrets refused an ordinary file: %q", masked)
	}
	if !strings.Contains(masked, "broker lives on the NAS") {
		t.Errorf("masked output dropped an ordinary comment:\n%s", masked)
	}
}

// unquoteKey strips quotes but does not decode escapes, so an escaped
// key's real name cannot be checked - while sops decodes it and encrypts.
func TestMaskSecretsFailsClosedOnEscapedKey(t *testing.T) {
	in := "\"pass\\u0077ord\": LEAKME\n"

	masked, ok := maskSecrets([]byte(in), "packages/demo.yaml")
	if ok && strings.Contains(masked, "LEAKME") {
		t.Fatalf("masked output leaked a secret behind an escaped key:\n%s", masked)
	}
}

// A delete diff quotes the LIVE file in full, so gating the masking on
// secrets.yaml alone published every other encrypted file's secrets.
func TestDeletedEncryptedFileIsMaskedNotOnlySecretsYaml(t *testing.T) {
	live := "mqtt:\n  broker: 10.0.0.1\n  password: LEAKME\n"

	repoRoot, configRoot := dirs(t)
	write(t, configRoot, "packages/mqtt.yaml", []byte(live))

	changes, _, _ := Compute(repoRoot, configRoot, nil, []string{"packages/mqtt.yaml"}, nil)

	if len(changes) != 1 || changes[0].Kind != "delete" {
		t.Fatalf("changes = %+v, want one delete", changes)
	}
	if strings.Contains(changes[0].DiffText, "LEAKME") {
		t.Errorf("delete diff published the live secret:\n%s", changes[0].DiffText)
	}
	if !strings.Contains(changes[0].DiffText, maskMarker) && changes[0].DiffText != encryptedSummary {
		t.Errorf("delete diff was neither masked nor summarized:\n%s", changes[0].DiffText)
	}
}

// --- JSON and dotenv: masking is written for YAML and only YAML ----------

// maskSecrets classifies with YAML rules, so the JSON and dotenv files
// sopscrypt also encrypts fail closed rather than be judged by a grammar
// they are not written in.
func TestNonYAMLEncryptedDiffIsSummarized(t *testing.T) {
	cases := []struct {
		name      string
		path      string
		live      string
		decrypted string
		secrets   []string
	}{
		{
			name:      "service account json",
			path:      "includes/HAandGHome.json",
			live:      "{\n  \"client_email\": \"a@b.example\",\n  \"private_key\": \"LIVESECRET\"\n}\n",
			decrypted: "{\n\t\"client_email\": \"a@b.example\",\n\t\"private_key\": \"REPOSECRET\"\n}\n",
			secrets:   []string{"LIVESECRET", "REPOSECRET"},
		},
		{
			name:      "wmbusmeters meter definition",
			path:      "wmbusmeters/etc/wmbusmeters.d/meter-0001",
			live:      "name=heater\nkey=LIVESECRET\n",
			decrypted: "name=heater\nkey=REPOSECRET\n",
			secrets:   []string{"LIVESECRET", "REPOSECRET"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			repoRoot, configRoot := dirs(t)
			write(t, repoRoot, c.path, []byte(sopsDoc))
			write(t, configRoot, c.path, []byte(c.live))

			changes, _, _ := Compute(repoRoot, configRoot, []string{c.path}, nil, decryptTo(c.decrypted, nil))

			if len(changes) != 1 {
				t.Fatalf("len(changes) = %d, want 1", len(changes))
			}
			if changes[0].DiffText != encryptedSummary {
				t.Errorf("diff_text = %q, want the %q summary", changes[0].DiffText, encryptedSummary)
			}
			for _, secret := range c.secrets {
				if strings.Contains(changes[0].DiffText, secret) {
					t.Fatalf("diff_text leaked %q: %q", secret, changes[0].DiffText)
				}
			}
		})
	}
}

// Pins the gate, not its effect: most non-YAML fails closed incidentally,
// but a dotenv line whose VALUE holds ": " reads as a legal mapping line
// with a non-secret key and would be published verbatim.
func TestMaskedDiffRefusesNonYAMLBeforeClassifying(t *testing.T) {
	before := "name=heater: kitchen\nkey=hunter2: LEAKME\n"
	after := "name=heater: kitchen\nkey=hunter3: LEAKME\n"

	got := maskedDiff([]byte(before), []byte(after), "wmbusmeters/etc/wmbusmeters.d/meter-0001")
	if got != encryptedSummary {
		t.Errorf("maskedDiff() = %q, want the %q summary", got, encryptedSummary)
	}
	for _, secret := range []string{"hunter2", "hunter3", "LEAKME"} {
		if strings.Contains(got, secret) {
			t.Fatalf("maskedDiff() leaked %q: %q", secret, got)
		}
	}
}

// With no repo copy to decrypt, the decision to mask a delete diff rests
// on sopscrypt.NeedsEncryption reading the live plaintext.
func TestDeletedNonYAMLSecretFileIsMasked(t *testing.T) {
	cases := []struct {
		name string
		path string
		live string
	}{
		{"json", "includes/sa.json", "{\n  \"client_email\": \"a@b.example\",\n  \"private_key\": \"LEAKME\"\n}\n"},
		{"dotenv", "wmbusmeters/etc/wmbusmeters.d/meter-0001", "name=heater\nkey=LEAKME\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			repoRoot, configRoot := dirs(t)
			write(t, configRoot, c.path, []byte(c.live))

			changes, _, _ := Compute(repoRoot, configRoot, nil, []string{c.path}, nil)

			if len(changes) != 1 || changes[0].Kind != "delete" {
				t.Fatalf("changes = %+v, want one delete", changes)
			}
			if strings.Contains(changes[0].DiffText, "LEAKME") {
				t.Errorf("delete diff published the live secret:\n%s", changes[0].DiffText)
			}
		})
	}
}

// The size guard once required YAML: a large encrypted JSON or dotenv file
// past it compares as ciphertext and is applied verbatim.
func TestLargeEncryptedNonYAMLFileIsRefused(t *testing.T) {
	withLargeFileThreshold(t, 64)

	for _, path := range []string{"zigbee2mqtt/coordinator_backup.json", "wmbusmeters/etc/wmbusmeters.d/meter-0001"} {
		t.Run(path, func(t *testing.T) {
			padded := sopsDoc + "# " + strings.Repeat("x", 256) + "\n"
			repoRoot, configRoot := dirs(t)
			write(t, repoRoot, path, []byte(padded))
			write(t, configRoot, path, []byte("key=LEAKME\n"))

			changes, _, failures := Compute(repoRoot, configRoot, []string{path}, nil, decryptTo("", nil))

			if len(changes) != 0 {
				t.Errorf("changes = %+v, want none: an encrypted file too large to decrypt must not be applied", changes)
			}
			if len(failures) != 1 {
				t.Fatalf("decryptFailures = %v, want one", failures)
			}
		})
	}
}

// Files above the threshold never reach the transform, so a large
// encrypted one would drift forever and write ENC[...] into the config.
func TestLargeEncryptedFileIsRefusedNotCompared(t *testing.T) {
	withLargeFileThreshold(t, 64)

	padded := sopsDoc + "# " + strings.Repeat("x", 256) + "\n"
	repoRoot, configRoot := dirs(t)
	write(t, repoRoot, "packages/big.yaml", []byte(padded))
	write(t, configRoot, "packages/big.yaml", []byte("mqtt:\n  password: LEAKME\n"))

	changes, _, failures := Compute(repoRoot, configRoot, []string{"packages/big.yaml"}, nil, decryptTo("", nil))

	if len(changes) != 0 {
		t.Errorf("changes = %+v, want none: an encrypted file too large to decrypt must not be applied", changes)
	}
	if len(failures) != 1 {
		t.Fatalf("decryptFailures = %v, want one", failures)
	}
	if !strings.Contains(failures[0], "packages/big.yaml") {
		t.Errorf("failure does not name the file: %q", failures[0])
	}
}

// Value-level encryption puts ENC[...] wherever the secret is, so a
// head-only sniff calls a late-keyed backup plaintext. sops always appends
// its metadata last, making the tail the reliable witness.
func TestLargeEncryptedFileDetectedByItsTail(t *testing.T) {
	withLargeFileThreshold(t, 64)

	// Padding first, sops metadata last, exactly as sops emits it.
	padded := "{\n  \"devices\": \"" + strings.Repeat("x", 200*1024) + "\",\n" +
		"  \"network_key\": \"ENC[AES256_GCM,data:zzz,type:str]\",\n" +
		"  \"sops\": { \"mac\": \"x\", \"version\": \"3.13.2\" }\n}\n"

	repoRoot, configRoot := dirs(t)
	write(t, repoRoot, "zigbee2mqtt/coordinator_backup.json", []byte(padded))
	write(t, configRoot, "zigbee2mqtt/coordinator_backup.json", []byte("{\"network_key\": \"LEAKME\"}\n"))

	changes, _, failures := Compute(repoRoot, configRoot,
		[]string{"zigbee2mqtt/coordinator_backup.json"}, nil, decryptTo("", nil))

	if len(changes) != 0 {
		t.Errorf("changes = %+v, want none: ciphertext must never be applied verbatim", changes)
	}
	if len(failures) != 1 {
		t.Fatalf("decryptFailures = %v, want one naming the file", failures)
	}
}

// The other half: the tail sniff must not refuse an ordinary large config
// that merely carries the word "version".
func TestLargeOrdinaryFileIsNotMistakenForCiphertext(t *testing.T) {
	withLargeFileThreshold(t, 64)

	plain := "version: 2\nnotes: " + strings.Repeat("y", 200*1024) + "\ntrailing_version: 3\n"

	repoRoot, configRoot := dirs(t)
	write(t, repoRoot, "packages/big.yaml", []byte(plain))
	write(t, configRoot, "packages/big.yaml", []byte(plain))

	changes, _, failures := Compute(repoRoot, configRoot, []string{"packages/big.yaml"}, nil, nil)
	if len(failures) != 0 {
		t.Errorf("decryptFailures = %v, want none for an ordinary large file", failures)
	}
	if len(changes) != 0 {
		t.Errorf("changes = %+v, want none: identical files", changes)
	}
}
