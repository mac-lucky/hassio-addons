package render

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/mac-lucky/hassio-addons/onepassword_secrets/internal/secret"
)

func d(key, value string) Desired { return Desired{Key: key, Value: secret.New(value)} }

func decode(t *testing.T, content []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := yaml.Unmarshal(content, &m); err != nil {
		t.Fatalf("rendered file does not parse: %v\n%s", err, content)
	}
	return m
}

func TestRenderRoundTripsTrickyValues(t *testing.T) {
	values := []string{
		"plain", "123", "0123", "1e3", "yes", "no", "on", "off", "true", "null", "~", "",
		"@at", "!secret other", "a: b", "#hash", "- dash", "* star", "&anchor", "%pct",
		"quote'single", `quote"double`, `back\slash`, "$1 and $& and $'", "{flow}", "[seq]",
		" leading space", "trailing space ", "tab\tinside", "multi\nline\nvalue", "multi\nline\n",
		"multi\n\n\nblank lines\n\n", "-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----\n",
		"line with trailing space \nnext", "\nleading newline", "crlf\r\nvalue", "unicode zazolc", "very long " + strings.Repeat("x", 300),
	}
	var desired []Desired
	want := map[string]any{}
	for i, v := range values {
		key := "k" + string(rune('a'+i%26)) + strings.Repeat("x", i/26)
		desired = append(desired, d(key, v))
		want[key] = v
	}
	plan, err := Render(Input{Desired: desired})
	if err != nil {
		t.Fatal(err)
	}
	got := decode(t, plan.Content)
	if !reflect.DeepEqual(got, want) {
		for k, v := range want {
			if got[k] != v {
				t.Errorf("%s: got %#v want %#v", k, got[k], v)
			}
		}
		t.Fatalf("content:\n%s", plan.Content)
	}
	if len(plan.Added) != len(values) || !strings.HasPrefix(string(plan.Content), "# Managed by the 1Password Secrets add-on") {
		t.Fatalf("plan = %+v\n%s", plan, plan.Content)
	}
}

func TestRenderKeepsCommentsAndUnmanaged(t *testing.T) {
	current := []byte(`# my comment about wifi
wifi_ssid: home # trailing
wifi_password: old
port: 1883
untouched: value
gone_key: was-managed
still_used: was-managed-too
`)
	plan, err := Render(Input{
		Current: current,
		Desired: []Desired{d("wifi_password", "new"), d("port", "1883"), d("brand_new", "x")},
		PreviouslyManaged: map[string]bool{
			"wifi_password": true, "gone_key": true, "still_used": true,
		},
		Referenced: map[string]bool{"still_used": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	out := string(plan.Content)
	for _, s := range []string{"# my comment about wifi", "wifi_ssid: home # trailing", "port: 1883", "untouched: value", `wifi_password: "new"`, `brand_new: "x"`, "still_used: was-managed-too"} {
		if !strings.Contains(out, s) {
			t.Errorf("missing %q in:\n%s", s, out)
		}
	}
	if strings.Contains(out, "gone_key") {
		t.Errorf("gone_key should be removed:\n%s", out)
	}
	check := func(name string, got, want []string) {
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s = %v, want %v", name, got, want)
		}
	}
	check("Changed", plan.Changed, []string{"wifi_password"})
	check("Unchanged", plan.Unchanged, []string{"port"})
	check("Added", plan.Added, []string{"brand_new"})
	check("Removed", plan.Removed, []string{"gone_key"})
	check("Orphaned", plan.Orphaned, []string{"still_used"})
	check("Unmanaged", plan.Unmanaged, []string{"untouched", "wifi_ssid"})
	if decode(t, plan.Content)["port"] != 1883 {
		t.Error("an unchanged int must stay an int")
	}

	again, err := Render(Input{Current: plan.Content, Desired: []Desired{d("wifi_password", "new"), d("port", "1883"), d("brand_new", "x")}})
	if err != nil {
		t.Fatal(err)
	}
	if again.Touched() || string(again.Content) != out || strings.Count(out, "1Password Secrets add-on") != 1 {
		t.Fatalf("second render not stable:\n%s\n---\n%s", out, again.Content)
	}
}

func TestRenderStructured(t *testing.T) {
	sa := `{"type":"service_account","project_id":"p","private_key":"-----BEGIN\nAAA\n-----END\n","n":[1,2,3]}`
	plan, err := Render(Input{Desired: []Desired{{Key: "google_sa_json", Value: secret.New(sa), Structured: true}, {Key: "network_key_json", Value: secret.New("[1, 3, 5, 7]"), Structured: true}}})
	if err != nil {
		t.Fatal(err)
	}
	got := decode(t, plan.Content)
	var want map[string]any
	_ = json.Unmarshal([]byte(sa), &want)
	gotSA, _ := json.Marshal(got["google_sa_json"])
	wantSA, _ := json.Marshal(want)
	if string(gotSA) != string(wantSA) {
		t.Fatalf("structured mismatch:\n%s\n%s\n%s", gotSA, wantSA, plan.Content)
	}
	if strings.Contains(string(plan.Content), "{") {
		t.Fatalf("structured value should be block style:\n%s", plan.Content)
	}
	again, err := Render(Input{Current: plan.Content, Desired: []Desired{{Key: "google_sa_json", Value: secret.New(sa), Structured: true}, {Key: "network_key_json", Value: secret.New("[1,3,5,7]"), Structured: true}}})
	if err != nil || again.Touched() {
		t.Fatalf("structured value not stable: %+v %v", again, err)
	}
}

func TestRenderKeepAndEdgeFiles(t *testing.T) {
	plan, err := Render(Input{
		Current:           []byte("conflicted: last-good\n"),
		Keep:              map[string]bool{"conflicted": true},
		PreviouslyManaged: map[string]bool{"conflicted": true},
	})
	if err != nil || !strings.Contains(string(plan.Content), "conflicted: last-good") || plan.Touched() {
		t.Fatalf("kept key lost: %v\n%s", err, plan.Content)
	}
	for _, current := range []string{"# only a comment\n", "{}\n", "---\n"} {
		plan, err := Render(Input{Current: []byte(current), Desired: []Desired{d("a", "b")}})
		if err != nil || decode(t, plan.Content)["a"] != "b" {
			t.Fatalf("%q: %v\n%s", current, err, plan.Content)
		}
	}
	if _, err := Render(Input{Current: []byte("- a\n- b\n")}); err != ErrNotMapping {
		t.Fatalf("list file: err = %v", err)
	}
	_, err = Render(Input{Current: []byte("a: [s3cret-in-broken-file\n")})
	if err == nil || strings.Contains(err.Error(), "s3cret") {
		t.Fatalf("broken file error must not quote content: %v", err)
	}
}

func TestKeys(t *testing.T) {
	keys, err := Keys([]byte("b: 1\na: 2\n"))
	if err != nil || !reflect.DeepEqual(keys, []string{"b", "a"}) {
		t.Fatalf("Keys = %v, %v", keys, err)
	}
}

func TestRenderKeepsAnchorsAndAliases(t *testing.T) {
	current := []byte("mqtt_password: &mq old\nz2m_password: *mq\nshared: &s keep\nuser: *s\n")
	plan, err := Render(Input{
		Current:           current,
		Desired:           []Desired{d("mqtt_password", "new")},
		PreviouslyManaged: map[string]bool{"mqtt_password": true, "shared": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	m := decode(t, plan.Content)
	if m["mqtt_password"] != "new" || m["z2m_password"] != "new" || m["user"] != "keep" {
		t.Fatalf("aliases broken:\n%s", plan.Content)
	}
	if len(plan.Removed) != 0 || len(plan.Orphaned) != 1 {
		t.Fatalf("an anchored key must not be removed: %+v", plan)
	}
}

func TestRenderQuotesForYAML11(t *testing.T) {
	values := []string{"yes", "on", "NO", "Off", "12:30", "1:20", "=", "<<", "0x1F", "0o17", "1_000", ".inf", "~"}
	var desired []Desired
	for i, v := range values {
		desired = append(desired, d("k"+strings.Repeat("x", i), v))
	}
	desired = append(desired, d("on", "key-is-on"), d("123", "key-is-number"))
	plan, err := Render(Input{Desired: desired})
	if err != nil {
		t.Fatal(err)
	}
	out := string(plan.Content)
	for _, v := range values {
		if !strings.Contains(out, ": \""+v+"\"") {
			t.Errorf("%q not double-quoted:\n%s", v, out)
		}
	}
	if !strings.Contains(out, "\"on\": ") || !strings.Contains(out, "\"123\": ") {
		t.Errorf("ambiguous keys not quoted:\n%s", out)
	}
}

func TestRenderStructuredEdgeCases(t *testing.T) {
	for _, js := range []string{`{"a":"x\/y"}`, `{"e":"\ud83d\ude00"}`, `{"n":1e400}`, `[1, 2.5, true, null, "s"]`, `{"z":1,"a":2}`} {
		in := Input{Desired: []Desired{{Key: "v_json", Value: secret.New(js), Structured: true}}}
		plan, err := Render(in)
		if err != nil || len(plan.KeyErrors) != 0 {
			t.Fatalf("%s: %v %v", js, err, plan.KeyErrors)
		}
		in.Current = plan.Content
		again, err := Render(in)
		if err != nil || again.Touched() || len(again.KeyErrors) != 0 {
			t.Fatalf("%s: not stable: %v %+v\n%s", js, err, again, plan.Content)
		}
	}
	plan, err := Render(Input{
		Current: []byte("ok: \"1\"\n"),
		Desired: []Desired{d("ok", "2"), {Key: "bad_json", Value: secret.New("[1,"), Structured: true}},
	})
	if err != nil || plan.KeyErrors["bad_json"] == "" || decode(t, plan.Content)["ok"] != "2" {
		t.Fatalf("a bad key must not block the file: %v %+v", err, plan)
	}
}
