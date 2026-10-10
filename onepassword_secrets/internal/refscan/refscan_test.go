package refscan

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, body := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestScanResolvesDomainsAndTargets(t *testing.T) {
	root := writeTree(t, map[string]string{
		"configuration.yaml": `homeassistant:
  packages: !include_dir_named packages
xiaomi_miio:
  token: !secret xiaomi_fan_token
mqtt: !include includes/mqtt.yaml
google_assistant:
  service_account: !secret google_sa_json
sensor: !include_dir_merge_list sensors
`,
		"includes/mqtt.yaml":             "sensor:\n  - name: x\n    password: !secret mqtt_password\n    more: !include more.yaml\n",
		"includes/more.yaml":             "api_key: !secret nested_key\n",
		"packages/net.yaml":              "rest:\n  - headers:\n      Authorization: !secret rest_token\n",
		"sensors/a.yaml":                 "- platform: x\n  key: !secret sensor_key\n",
		"esphome/node.yaml":              "api:\n  encryption:\n    key: !secret node_api_key\n",
		"esphome/secrets.yaml":           "node_api_key: old\n",
		"esphome/archive/gone.yaml":      "api:\n  encryption:\n    key: !secret archived_key\n",
		"zigbee2mqtt/configuration.yaml": "mqtt:\n  password: '!secret mqtt_password'\nadvanced:\n  network_key: '!secret network_key_json'\n  pan_id: 1234\n  other: '!creds.yaml other'\n",
		"broken.yaml":                    "a: [unclosed\nb: !secret from_broken # !secret commented\n",
		".storage/core.yaml":             "x: !secret hidden_store\n",
		"custom_components/c/x.yaml":     "x: !secret in_custom\n",
		"secrets.yaml":                   "xiaomi_fan_token: x\n",
	})
	skip := func(rel string) bool { return rel == "secrets.yaml" || rel == "esphome/secrets.yaml" }
	res, err := Scan(Options{Root: root, Skip: skip})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Ref{}
	for _, r := range res.Refs {
		got[r.Key+"@"+r.File] = r
	}
	want := map[string]struct {
		domain string
		kind   Kind
		target string
	}{
		"xiaomi_fan_token@configuration.yaml":             {"xiaomi_miio", TagRef, ""},
		"google_sa_json@configuration.yaml":               {"google_assistant", TagRef, ""},
		"mqtt_password@includes/mqtt.yaml":                {"mqtt", TagRef, ""},
		"nested_key@includes/more.yaml":                   {"mqtt", TagRef, ""},
		"rest_token@packages/net.yaml":                    {"rest", TagRef, ""},
		"sensor_key@sensors/a.yaml":                       {"sensor", TagRef, ""},
		"node_api_key@esphome/node.yaml":                  {"", TagRef, ""},
		"mqtt_password@zigbee2mqtt/configuration.yaml":    {"", StringRef, "zigbee2mqtt/secret.yaml"},
		"network_key_json@zigbee2mqtt/configuration.yaml": {"", StringRef, "zigbee2mqtt/secret.yaml"},
		"other@zigbee2mqtt/configuration.yaml":            {"", StringRef, "zigbee2mqtt/creds.yaml"},
		"from_broken@broken.yaml":                         {"", TagRef, ""},
	}
	for k, w := range want {
		r, ok := got[k]
		if !ok {
			t.Errorf("missing ref %s (got %v)", k, keys(got))
			continue
		}
		if r.Domain != w.domain || r.Kind != w.kind || r.Target != w.target || r.Line == 0 {
			t.Errorf("%s = %+v, want domain %q kind %v target %q", k, r, w.domain, w.kind, w.target)
		}
	}
	if len(got) != len(want) {
		t.Errorf("got %d refs, want %d: %v", len(got), len(want), keys(got))
	}
	if len(res.Warnings) != 1 {
		t.Errorf("warnings = %v", res.Warnings)
	}

	fp, err := Fingerprint(Options{Root: root, Skip: skip})
	if err != nil || fp != res.Fingerprint {
		t.Fatalf("Fingerprint = %q, %v; scan had %q", fp, err, res.Fingerprint)
	}
	if err := os.WriteFile(filepath.Join(root, "packages/new.yaml"), []byte("a: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if fp2, _ := Fingerprint(Options{Root: root, Skip: skip}); fp2 == fp {
		t.Fatal("fingerprint did not move after a new file")
	}
}

func TestTargetFor(t *testing.T) {
	targets := []string{"secrets.yaml", "esphome/secrets.yaml", "zigbee2mqtt/secret.yaml"}
	cases := []struct {
		ref    Ref
		want   string
		wantOK bool
	}{
		{Ref{File: "configuration.yaml", Kind: TagRef}, "secrets.yaml", true},
		{Ref{File: "includes/deep/x.yaml", Kind: TagRef}, "secrets.yaml", true},
		{Ref{File: "esphome/node.yaml", Kind: TagRef}, "esphome/secrets.yaml", true},
		{Ref{File: "esphome/sub/node.yaml", Kind: TagRef}, "esphome/secrets.yaml", true},
		{Ref{File: "zigbee2mqtt/configuration.yaml", Kind: StringRef, Target: "zigbee2mqtt/secret.yaml"}, "zigbee2mqtt/secret.yaml", true},
		{Ref{File: "zigbee2mqtt/configuration.yaml", Kind: StringRef, Target: "zigbee2mqtt/creds.yaml"}, "zigbee2mqtt/creds.yaml", false},
	}
	for _, c := range cases {
		got, ok := TargetFor(c.ref, targets)
		if got != c.want || ok != c.wantOK {
			t.Errorf("TargetFor(%+v) = %q, %v; want %q, %v", c.ref, got, ok, c.want, c.wantOK)
		}
	}
	if _, ok := TargetFor(Ref{File: "configuration.yaml"}, []string{"esphome/secrets.yaml"}); ok {
		t.Error("a root file must not resolve to a subdirectory's secrets file")
	}
}

func TestAddonOptionRefs(t *testing.T) {
	opts := map[string]any{
		"logins": []any{
			map[string]any{"username": "z2m", "password": "!secret mqtt_password"},
			map[string]any{"username": "plain", "password": "literal-not-a-ref"},
		},
		"git_token": "secret://gitops_git_token",
		"nested":    map[string]any{"deep": []any{"!secret  spaced_key "}},
		"flag":      true,
	}
	got := AddonOptionRefs("core_mosquitto", "Mosquitto", opts)
	want := []AddonRef{
		{Key: "gitops_git_token", Slug: "core_mosquitto", Name: "Mosquitto", Option: "git_token", SecretURL: true},
		{Key: "mqtt_password", Slug: "core_mosquitto", Name: "Mosquitto", Option: "logins[0].password"},
		{Key: "spaced_key", Slug: "core_mosquitto", Name: "Mosquitto", Option: "nested.deep[0]"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
}

func keys(m map[string]Ref) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
