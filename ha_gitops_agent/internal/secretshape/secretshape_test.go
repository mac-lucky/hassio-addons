package secretshape

import (
	"slices"
	"strings"
	"testing"
)

func TestLiteralsYAML(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
		want    []string
	}{
		{
			name:    "literal under a secret key, nested",
			content: "wifi:\n  ssid: home\n  password: hunter2\n",
			want:    []string{"wifi.password"},
		},
		{
			name:    "a !secret tag is a reference",
			content: "wifi:\n  password: !secret wifi_password\n",
		},
		{
			name:    "!env_var and !include are references too",
			content: "token: !env_var HA_TOKEN\napi_key: !include key.txt\n",
		},
		{
			name: "Zigbee2MQTT string references",
			content: "mqtt:\n  user: '!secret user'\n  password: '!secret password'\n" +
				"advanced:\n  network_key: '!secret.yaml network_key'\n  pan_key: \"!secrets/z2m.yml pan\"\n",
		},
		{
			name:    "nothing to leak: null, empty, booleans",
			content: "password:\nsecret: ''\nauth: true\napi_key: null\ntoken: off\n",
		},
		{
			name:    "a nested provider block is walked, not reported",
			content: "auth:\n  type: homeassistant\n  password: plain\n",
			want:    []string{"auth.password"},
		},
		{
			name:    "lists of mappings carry an index",
			content: "users:\n  - name: a\n    token: t1\n  - name: b\n    token: !secret b_token\n",
			want:    []string{"users[0].token"},
		},
		{
			name:    "a list of literals under a secret key",
			content: "keys:\n  - one\n  - two\n",
			want:    []string{"keys"},
		},
		{
			name:    "an alias to a literal counts",
			content: "shared: &pw hunter2\nmqtt:\n  password: *pw\n",
			want:    []string{"mqtt.password"},
		},
		{
			name:    "numbers are literals",
			content: "api_key: 123456\n",
			want:    []string{"api_key"},
		},
		{
			name:    "keys that only contain a secret word are not secret-shaped",
			content: "monkey: banana\nkeyboard: us\npin: 4\npassword_hint: none\n",
		},
		{
			name:    "every document in a stream",
			content: "a: 1\n---\nmqtt_password: hunter2\n",
			want:    []string{"mqtt_password"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Literals("esphome/node.yaml", []byte(tc.content))
			if !slices.Equal(got, tc.want) {
				t.Errorf("Literals = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestLiteralsJSON(t *testing.T) {
	got := Literals("custom/settings.JSON", []byte(`{"broker": {"host": "x", "password": "hunter2"}, "token": "!secret t", "api_key": ""}`))
	if want := []string{"broker.password"}; !slices.Equal(got, want) {
		t.Errorf("Literals = %q, want %q", got, want)
	}
}

func TestLiteralsExtensionlessAssignments(t *testing.T) {
	content := "# wmbusmeters meter\nname=water\nmeter=multical21\nid=12345678\nkey=00112233445566778899AABBCCDDEEFF\nexport API_TOKEN=x\nEMPTY_TOKEN=\n"
	got := Literals("wmbusmeters/etc/wmbusmeters.d/meter-1", []byte(content))
	if want := []string{"key", "API_TOKEN"}; !slices.Equal(got, want) {
		t.Errorf("Literals = %q, want %q", got, want)
	}
}

func TestLiteralsIgnoresOtherFormats(t *testing.T) {
	for _, rel := range []string{"python_scripts/x.py", "www/notes.txt", "custom.conf"} {
		if got := Literals(rel, []byte("password: hunter2\npassword=hunter2\n")); got != nil {
			t.Errorf("Literals(%s) = %q, want nil", rel, got)
		}
	}
}

// A file that does not parse cannot say where a value ends, so every
// secret-shaped key with something after it is reported - tags excepted.
func TestLiteralsFailsClosedOnUnparseableYAML(t *testing.T) {
	content := "mqtt:\n  password: hunter2\n bad: [\n  token: !secret t\n"
	got := Literals("broken.yaml", []byte(content))
	if want := []string{"password"}; !slices.Equal(got, want) {
		t.Errorf("Literals = %q, want %q", got, want)
	}
	compact := `{"password":"x"` // unterminated
	if got := Literals("broken.json", []byte(compact)); !slices.Equal(got, []string{"password"}) {
		t.Errorf("Literals(compact JSON) = %q, want [password]", got)
	}
}

// The whole point of the package: the answer never carries a value.
func TestLiteralsNeverReturnsAValue(t *testing.T) {
	const secret = "s3cr3t-value-123"
	inputs := map[string]string{
		"a.yaml": "password: " + secret + "\n",
		"a.json": `{"token": "` + secret + `"}`,
		"a":      "PSK=" + secret + "\n",
		"b.yaml": "password: " + secret + "\n bad: [\n",
	}
	for rel, content := range inputs {
		for _, key := range Literals(rel, []byte(content)) {
			if strings.Contains(key, secret) {
				t.Errorf("Literals(%s) returned %q, which carries the value", rel, key)
			}
		}
	}
}

func TestIsSecretKey(t *testing.T) {
	for key, want := range map[string]bool{
		"password": true, "PASSWORD": true, "mqtt_password": true, "api_key": true, "apikey": true,
		"keys": true, "psk": true, "wifi_psk": true, "auth": true, "token": true,
		"pin": false, "keyboard": false, "monkey": false, "password_hint": false, "tokens_used": false,
	} {
		if got := IsSecretKey(key); got != want {
			t.Errorf("IsSecretKey(%q) = %v, want %v", key, got, want)
		}
	}
}
