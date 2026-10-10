package options

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const creds = `{"verifier":{"salt":"x"},"encCredentials":{"kid":"s3cr3t-cred"},"version":"2","deviceUuid":"d","uniqueKey":{"k":"s3cr3t-cred"}}`

func load(t *testing.T, body string) (Options, error) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "options.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return Load(p)
}

func TestLoadDefaultsAndValues(t *testing.T) {
	o, err := load(t, `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(o, Defaults()) || o.Configured() {
		t.Fatalf("defaults = %+v", o)
	}

	b64 := base64.StdEncoding.EncodeToString([]byte(creds))
	o, err = load(t, `{"connect_credentials":"`+b64[:20]+"\\n"+b64[20:]+`","connect_token":" tok ","vaults":["homeassistant","shared-m2m"],
		"references":[{"key":"vl_password","ref":"op://shared-m2m/victorialogs/password"}],
		"secrets_files":["secrets.yaml","esphome/secrets.yaml","/zigbee2mqtt/secret.yaml","esphome/secrets.yaml"],
		"poll_interval_seconds":30,"restart_addons":false,"core_restart":"notify","dry_run":true,"log_level":"debug"}`)
	if err != nil {
		t.Fatal(err)
	}
	if string(o.ConnectCredentials) != creds || o.ConnectToken != "tok" || !o.Configured() || !o.Embedded() {
		t.Fatalf("credentials/token not decoded: %+v", o.ConnectToken)
	}
	// main wipes the bytes once Connect has them on disk.
	o.ConnectCredentials = nil
	if !o.Configured() {
		t.Fatal("wiping the credential bytes must not unconfigure the add-on")
	}
	if !reflect.DeepEqual(o.SecretsFiles, []string{"secrets.yaml", "esphome/secrets.yaml", "zigbee2mqtt/secret.yaml"}) {
		t.Fatalf("SecretsFiles = %v", o.SecretsFiles)
	}
	if o.PollIntervalSeconds != 30 || o.RestartAddons || o.CoreRestart != "notify" || !o.DryRun || o.LogLevel != "debug" || len(o.References) != 1 {
		t.Fatalf("options = %+v", o)
	}
	o, err = load(t, `{"connect_credentials":`+jsonString(creds)+`,"connect_token":"t","connect_url":"https://op.example/"}`)
	if err != nil || string(o.ConnectCredentials) != creds || o.ConnectURL != "https://op.example" || o.Embedded() {
		t.Fatalf("raw JSON credentials / url: %+v %v", o.ConnectURL, err)
	}
}

func TestLoadRejectsBadValuesWithoutEchoing(t *testing.T) {
	bad := []string{
		`{"connect_credentials":"s3cr3t-not-json-or-base64!!"}`,
		`{"connect_credentials":"{\"s3cr3t\": 1}"}`,
		`{"connect_url":"ftp://s3cr3t@x"}`,
		`{"vaults":[""]}`,
		`{"references":[{"key":"Bad-Key","ref":"op://a/b/c"}]}`,
		`{"references":[{"key":"k","ref":"https://s3cr3t"}]}`,
		`{"references":[{"key":"k","ref":"op://a/b/c"},{"key":"k","ref":"op://a/b/d"}]}`,
		`{"secrets_files":["../outside.yaml"]}`,
		`{"secrets_files":[".storage/x.yaml"]}`,
		`{"secrets_files":["configuration.yaml"]}`,
		`{"secrets_files":["notyaml.txt"]}`,
		`{"secrets_files":["esphome/common/creds.yaml"]}`,
		`{"poll_interval_seconds":1}`,
		`{"core_restart":"always"}`,
		`{"log_level":"trace"}`,
		`not json s3cr3t`,
	}
	for _, body := range bad {
		_, err := load(t, body)
		if err == nil {
			t.Errorf("%s: want an error", body)
			continue
		}
		if strings.Contains(err.Error(), "s3cr3t") {
			t.Errorf("%s: error echoes input: %v", body, err)
		}
	}
}

func jsonString(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
}
