package differ

import (
	"strings"
	"testing"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/gitsync"
)

// The "+" side quotes the repository. A secret committed there in the
// clear is still a secret on the dashboard.
func TestGenuineSecretChangeIsReportedWithoutTheSecret(t *testing.T) {
	live := "mqtt:\n  server: mqtt://localhost\n  password: OLDSECRETVALUE\n"
	repo := "mqtt:\n  server: mqtt://localhost\n  password: NEWSECRETVALUE\n"

	repoRoot, configRoot := dirs(t)
	write(t, repoRoot, "zigbee2mqtt/configuration.yaml", []byte(repo))
	write(t, configRoot, "zigbee2mqtt/configuration.yaml", []byte(live))

	changes, _ := Compute(repoRoot, configRoot, []string{"zigbee2mqtt/configuration.yaml"}, nil)

	if len(changes) != 1 {
		t.Fatalf("len(changes) = %d, want 1", len(changes))
	}
	c := changes[0]
	if c.Kind != "update" {
		t.Errorf("kind = %q, want update", c.Kind)
	}
	if c.DiffText != secretSummary {
		t.Errorf("diff_text = %q, want the %q summary", c.DiffText, secretSummary)
	}
	for _, secret := range []string{"OLDSECRETVALUE", "NEWSECRETVALUE"} {
		if strings.Contains(c.DiffText, secret) {
			t.Fatalf("diff_text leaked %q: %q", secret, c.DiffText)
		}
	}
}

// Hiding the secret must not hide the edit next to it, or every change to
// such a file becomes an unreviewable "something changed".
func TestNonSecretEditNextToASecretStaysVisible(t *testing.T) {
	live := "mqtt:\n  server: mqtt://old\n  password: KEEPMEHIDDEN\n"
	repo := "mqtt:\n  server: mqtt://new\n  password: KEEPMEHIDDEN\n"

	repoRoot, configRoot := dirs(t)
	write(t, repoRoot, "zigbee2mqtt/configuration.yaml", []byte(repo))
	write(t, configRoot, "zigbee2mqtt/configuration.yaml", []byte(live))

	changes, _ := Compute(repoRoot, configRoot, []string{"zigbee2mqtt/configuration.yaml"}, nil)

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
	repo := "tls:\n  private_key: |\n    -----BEGIN PRIVATE KEY-----\n    KEYMATERIALTWO\n  verify: false\n"

	repoRoot, configRoot := dirs(t)
	write(t, repoRoot, "packages/tls.yaml", []byte(repo))
	write(t, configRoot, "packages/tls.yaml", []byte(live))

	changes, _ := Compute(repoRoot, configRoot, []string{"packages/tls.yaml"}, nil)

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

// A reference is not a secret, so a file holding only references diffs
// like any other - masking it would hide every ordinary edit for nothing.
func TestReferencesOnlyFileIsNotMasked(t *testing.T) {
	repoRoot, configRoot := dirs(t)
	write(t, repoRoot, "esphome/node.yaml", []byte("wifi:\n  ssid: home\n  password: !secret wifi_password\n"))
	write(t, configRoot, "esphome/node.yaml", []byte("wifi:\n  ssid: work\n  password: !secret wifi_password\n"))

	changes, _ := Compute(repoRoot, configRoot, []string{"esphome/node.yaml"}, nil)

	if len(changes) != 1 {
		t.Fatalf("len(changes) = %d, want 1", len(changes))
	}
	if !strings.Contains(changes[0].DiffText, "+  ssid: home") || !strings.Contains(changes[0].DiffText, "!secret wifi_password") {
		t.Errorf("diff_text = %q, want an ordinary unmasked diff", changes[0].DiffText)
	}
}

func TestAddedSecretBearingFileIsMasked(t *testing.T) {
	repoRoot, configRoot := dirs(t)
	write(t, repoRoot, "packages/new.yaml", []byte("mqtt:\n  server: mqtt://localhost\n  password: BRANDNEWSECRET\n"))

	changes, _ := Compute(repoRoot, configRoot, []string{"packages/new.yaml"}, nil)

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

// Secrets files are rendered on the box by something else and never
// synced: not at the root, not nested, not as Zigbee2MQTT's secret.yaml,
// and not even when a state.json from 0.8.x lists one as applied and the
// repository no longer tracks it - that live file must survive.
func TestSecretsFilesAreNeverDiffedOrDeleted(t *testing.T) {
	files := []string{"secrets.yaml", "secrets.yml", "esphome/secrets.yaml", "zigbee2mqtt/secret.yaml", "zigbee2mqtt/secret.yml"}

	repoRoot, configRoot := dirs(t)
	for _, p := range files {
		write(t, repoRoot, p, []byte("mqtt_password: REPOVALUE\n"))
		write(t, configRoot, p, []byte("mqtt_password: LIVEVALUE\n"))
	}
	write(t, configRoot, "Secrets.YAML", []byte("x: LIVEVALUE\n"))

	if changes, _ := Compute(repoRoot, configRoot, files, nil); len(changes) != 0 {
		t.Errorf("tracked secrets files produced changes %+v, want none", changes)
	}
	// Untracked now, listed as applied by an older version.
	if changes, _ := Compute(repoRoot, configRoot, nil, append(files, "Secrets.YAML")); len(changes) != 0 {
		t.Errorf("manifest-listed secrets files produced changes %+v, want none - a delete would remove the live secrets", changes)
	}
}

// An exclude_paths entry gets the same treatment: a file an older apply
// wrote and the repository has since dropped is never planned for
// deletion, and a tracked one is never planned at all.
func TestUserExcludedPathsAreNeverDiffedOrDeleted(t *testing.T) {
	if err := gitsync.SetUserExclusions([]string{"wmbusmeters/etc/wmbusmeters.d/"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = gitsync.SetUserExclusions(nil) })

	repoRoot, configRoot := dirs(t)
	write(t, configRoot, "wmbusmeters/etc/wmbusmeters.d/meter-0001", []byte("name=water\n"))
	write(t, repoRoot, "wmbusmeters/etc/wmbusmeters.d/meter-0002", []byte("name=heat\n"))
	write(t, configRoot, "wmbusmeters/etc/wmbusmeters.d/meter-0002", []byte("name=other\n"))

	changes, _ := Compute(repoRoot, configRoot,
		[]string{"wmbusmeters/etc/wmbusmeters.d/meter-0002"},
		[]string{"wmbusmeters/etc/wmbusmeters.d/meter-0001", "wmbusmeters/etc/wmbusmeters.d/meter-0002"})
	if len(changes) != 0 {
		t.Errorf("changes = %+v, want none: an excluded path is neither applied nor deleted", changes)
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

// --- review: leak paths found by review, each pinned here ----------------

// YAML accepts a tab after a key's colon, but mappingLineRe matches
// neither form - without the tab check the line fell
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

// A comment in a secrets file can carry a secret as easily as a value -
// an old password parked in a comment.
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

// The other half: outside a secrets file comments are ordinary config, so
// hiding them costs review value for nothing.
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
// key's real name cannot be checked, and the line must fail closed.
func TestMaskSecretsFailsClosedOnEscapedKey(t *testing.T) {
	in := "\"pass\\u0077ord\": LEAKME\n"

	masked, ok := maskSecrets([]byte(in), "packages/demo.yaml")
	if ok && strings.Contains(masked, "LEAKME") {
		t.Fatalf("masked output leaked a secret behind an escaped key:\n%s", masked)
	}
}

// A delete diff quotes the LIVE file in full.
func TestDeletedSecretBearingFileIsMasked(t *testing.T) {
	live := "mqtt:\n  broker: 10.0.0.1\n  password: LEAKME\n"

	repoRoot, configRoot := dirs(t)
	write(t, configRoot, "packages/mqtt.yaml", []byte(live))

	changes, _ := Compute(repoRoot, configRoot, nil, []string{"packages/mqtt.yaml"})

	if len(changes) != 1 || changes[0].Kind != "delete" {
		t.Fatalf("changes = %+v, want one delete", changes)
	}
	if strings.Contains(changes[0].DiffText, "LEAKME") {
		t.Errorf("delete diff published the live secret:\n%s", changes[0].DiffText)
	}
	if !strings.Contains(changes[0].DiffText, maskMarker) && changes[0].DiffText != secretSummary {
		t.Errorf("delete diff was neither masked nor summarized:\n%s", changes[0].DiffText)
	}
}

// --- JSON and extensionless: masking is written for YAML and only YAML ---

// Pins the gate, not its effect: most non-YAML fails closed incidentally,
// but a KEY=value line whose VALUE holds ": " reads as a legal mapping line
// with a non-secret key and would be published verbatim.
func TestMaskedDiffRefusesNonYAMLBeforeClassifying(t *testing.T) {
	before := "name=heater: kitchen\nkey=hunter2: LEAKME\n"
	after := "name=heater: kitchen\nkey=hunter3: LEAKME\n"

	got := maskedDiff([]byte(before), []byte(after), "wmbusmeters/etc/wmbusmeters.d/meter-0001")
	if got != secretSummary {
		t.Errorf("maskedDiff() = %q, want the %q summary", got, secretSummary)
	}
	for _, secret := range []string{"hunter2", "hunter3", "LEAKME"} {
		if strings.Contains(got, secret) {
			t.Fatalf("maskedDiff() leaked %q: %q", secret, got)
		}
	}
}

func TestDeletedNonYAMLSecretFileIsMasked(t *testing.T) {
	cases := []struct {
		name string
		path string
		live string
	}{
		{"json", "includes/sa.json", "{\n  \"client_email\": \"a@b.example\",\n  \"private_key\": \"LEAKME\"\n}\n"},
		{"extensionless", "wmbusmeters/etc/wmbusmeters.d/meter-0001", "name=heater\nkey=LEAKME\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			repoRoot, configRoot := dirs(t)
			write(t, configRoot, c.path, []byte(c.live))

			changes, _ := Compute(repoRoot, configRoot, nil, []string{c.path})

			if len(changes) != 1 || changes[0].Kind != "delete" {
				t.Fatalf("changes = %+v, want one delete", changes)
			}
			if strings.Contains(changes[0].DiffText, "LEAKME") {
				t.Errorf("delete diff published the live secret:\n%s", changes[0].DiffText)
			}
			if changes[0].DiffText != secretSummary {
				t.Errorf("diff_text = %q, want the %q summary", changes[0].DiffText, secretSummary)
			}
		})
	}
}

// An update diff quotes the live file on its "-" lines, so a secret typed
// into a tracked file in the HA editor must not reach the dashboard and
// /status.json through it.
func TestUpdateDiffMasksASecretOnlyTheLiveCopyHolds(t *testing.T) {
	repoRoot, configRoot := dirs(t)
	write(t, repoRoot, "configuration.yaml", []byte("weather:\n  name: Home\n"))
	write(t, configRoot, "configuration.yaml", []byte("weather:\n  name: Home\n  api_key: hunter2\n"))

	changes, _ := Compute(repoRoot, configRoot, []string{"configuration.yaml"}, nil)

	if len(changes) != 1 || changes[0].Kind != "update" {
		t.Fatalf("changes = %+v, want one update", changes)
	}
	if strings.Contains(changes[0].DiffText, "hunter2") {
		t.Errorf("update diff published the live secret:\n%s", changes[0].DiffText)
	}
}

// A JSON or extensionless file with a secret-shaped key cannot be masked
// line by line, so its whole diff is replaced by the summary.
func TestNonYAMLSecretFileDiffIsSummarized(t *testing.T) {
	cases := []struct {
		name, path, repo, live string
	}{
		{
			"json", "includes/sa.json",
			"{\n  \"client_email\": \"a@b.example\",\n  \"private_key\": \"REPOSECRET\"\n}\n",
			"{\n  \"client_email\": \"c@d.example\",\n  \"private_key\": \"LIVESECRET\"\n}\n",
		},
		{
			"dotenv", "wmbusmeters/etc/wmbusmeters.d/meter-0001",
			"name=heater\nkey=REPOSECRET\n",
			"name=boiler\nkey=LIVESECRET\n",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			repoRoot, configRoot := dirs(t)
			write(t, repoRoot, c.path, []byte(c.repo))
			write(t, configRoot, c.path, []byte(c.live))

			changes, _ := Compute(repoRoot, configRoot, []string{c.path}, nil)

			if len(changes) != 1 || changes[0].Kind != "update" {
				t.Fatalf("changes = %+v, want one update", changes)
			}
			if changes[0].DiffText != secretSummary {
				t.Errorf("diff_text = %q, want the %q summary", changes[0].DiffText, secretSummary)
			}
		})
	}
}
