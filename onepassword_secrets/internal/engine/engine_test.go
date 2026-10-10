package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/mac-lucky/hassio-addons/onepassword_secrets/internal/connectd"
	"github.com/mac-lucky/hassio-addons/onepassword_secrets/internal/ha"
	"github.com/mac-lucky/hassio-addons/onepassword_secrets/internal/opconnect"
	"github.com/mac-lucky/hassio-addons/onepassword_secrets/internal/options"
	"github.com/mac-lucky/hassio-addons/onepassword_secrets/internal/secret"
)

// Sentinel values: they may appear in the rendered secrets files and
// nowhere else.
const (
	sFan    = "SENTINEL-fan-token-1"
	sFan2   = "SENTINEL-fan-token-2"
	sMQTT   = "SENTINEL-mqtt-pass"
	sNode   = "SENTINEL-node-api-key"
	sGitTok = "SENTINEL-git-token"
	sNotif  = "SENTINEL-notify-key"
)

var sentinels = []string{sFan, sFan2, sMQTT, sNode, sGitTok, sNotif}

// fakeConnect serves one vault whose items the test edits.
type fakeConnect struct {
	mu      sync.Mutex
	items   map[string]opconnect.Item
	healthy bool
	down    bool
	fetches int
	// needsKick is a real Connect's fresh start: not synced until a
	// request with a token arrives.
	needsKick bool
}

func newFakeConnect() *fakeConnect {
	return &fakeConnect{healthy: true, items: map[string]opconnect.Item{}}
}

func (f *fakeConnect) set(id, title string, version int, fields map[string]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	item := opconnect.Item{ID: id, Title: title, Version: version, VaultID: "vault1", UpdatedAt: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)}
	for label, value := range fields {
		typ := "CONCEALED"
		if label == "rotated_at" || label == "interval" || label == "rotation" {
			typ = "STRING"
		}
		item.Fields = append(item.Fields, opconnect.Field{ID: label, Label: label, Type: typ, Value: secret.New(value)})
	}
	f.items[id] = item
}

func (f *fakeConnect) Health(context.Context) (opconnect.Health, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		return opconnect.Health{}, errors.New("dial tcp 127.0.0.1:8080: connect: connection refused")
	}
	status := "AVAILABLE"
	if !f.healthy || f.needsKick {
		status = "NOT_SYNCED"
	}
	return opconnect.Health{Version: "1.8.3", Dependencies: []opconnect.Dependency{{Service: "account_data", Status: status}}}, nil
}

func (f *fakeConnect) Vaults(context.Context) ([]opconnect.Vault, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.needsKick {
		f.needsKick = false
		return nil, &opconnect.StatusError{Code: 503, Message: "not synced"}
	}
	return []opconnect.Vault{{ID: "vault1", Name: "homeassistant"}}, nil
}

func (f *fakeConnect) Items(_ context.Context, vaultID string) ([]opconnect.ItemSummary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []opconnect.ItemSummary
	for _, it := range f.items {
		s := opconnect.ItemSummary{ID: it.ID, Title: it.Title, Version: it.Version}
		s.Vault.ID = vaultID
		out = append(out, s)
	}
	return out, nil
}

func (f *fakeConnect) Item(_ context.Context, _, itemID string) (opconnect.Item, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fetches++
	return f.items[itemID], nil
}

// fakeHA records every call, with its payload serialized.
type fakeHA struct {
	mu          sync.Mutex
	calls       []string
	payloads    []string
	checkOK     bool
	services    map[string]map[string]bool
	healthyBack bool
	up          bool
	addons      []ha.Addon
	options     map[string]map[string]any
	// restartErr is what RestartCore returns; coreRestarted makes HasState
	// report the pushed sensor gone (a restart from elsewhere).
	restartErr    error
	coreRestarted bool
	checkReason   string
}

func newFakeHA() *fakeHA {
	return &fakeHA{
		checkOK:     true,
		healthyBack: true,
		up:          true,
		services: map[string]map[string]bool{
			"template":                {"reload": true},
			"persistent_notification": {"create": true},
		},
		addons: []ha.Addon{
			{Slug: "core_mosquitto", Name: "Mosquitto broker", State: "started"},
			{Slug: "45df7312_zigbee2mqtt", Name: "Zigbee2MQTT", State: "started"},
			{Slug: "self_slug", Name: "1Password Secrets", State: "started"},
		},
		options: map[string]map[string]any{
			"core_mosquitto":       {"logins": []any{map[string]any{"username": "z2m", "password": "!secret mqtt_password"}}},
			"45df7312_zigbee2mqtt": {},
			"self_slug":            {"connect_token": "!secret should_be_ignored"},
		},
	}
}

func (h *fakeHA) rec(call string, payload any) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = append(h.calls, call)
	b, _ := json.Marshal(payload)
	h.payloads = append(h.payloads, string(b))
}

func (h *fakeHA) callList() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return strings.Join(h.calls, "\n")
}

func (h *fakeHA) reset() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = nil
}

func (h *fakeHA) CheckConfig(context.Context) (bool, string) {
	h.rec("check_config", nil)
	if h.checkOK {
		return true, ""
	}
	if h.checkReason != "" {
		return false, h.checkReason
	}
	return false, "Invalid config for 'xiaomi_miio' at configuration.yaml, line 2: got 'x'"
}

func (h *fakeHA) Services(context.Context) (map[string]map[string]bool, error) {
	h.rec("services", nil)
	return h.services, nil
}

func (h *fakeHA) CallService(_ context.Context, domain, service string, data map[string]any) error {
	h.rec("service "+domain+"."+service, data)
	return nil
}

func (h *fakeHA) Probe(context.Context) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	up := h.up
	// After a restart Core is down for one probe.
	h.up = true
	return up
}

func (h *fakeHA) WaitHealthy(context.Context, time.Duration) bool {
	h.rec("wait_healthy", nil)
	return h.healthyBack
}

func (h *fakeHA) RestartCore(context.Context) error {
	h.rec("restart_core", nil)
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.restartErr != nil {
		return h.restartErr
	}
	h.up = false
	return nil
}

func (h *fakeHA) FireEvent(_ context.Context, eventType string, data map[string]any) error {
	h.rec("event "+eventType, data)
	return nil
}

func (h *fakeHA) SetState(_ context.Context, entityID, st string, attrs map[string]any) error {
	h.rec("state "+entityID+"="+st, attrs)
	return nil
}

func (h *fakeHA) HasState(_ context.Context, entityID string) (bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return !h.coreRestarted, nil
}

func (h *fakeHA) Notify(_ context.Context, id, title, message string) error {
	h.rec("notify "+id, map[string]string{"title": title, "message": message})
	return nil
}

func (h *fakeHA) Dismiss(_ context.Context, id string) error {
	h.rec("dismiss "+id, nil)
	return nil
}

func (h *fakeHA) Addons(context.Context) ([]ha.Addon, error) {
	h.rec("addons", nil)
	return h.addons, nil
}

func (h *fakeHA) AddonOptions(_ context.Context, slug string) (map[string]any, error) {
	h.rec("addon_options "+slug, nil)
	return h.options[slug], nil
}

func (h *fakeHA) RestartAddon(_ context.Context, slug string) error {
	h.rec("restart_addon "+slug, nil)
	return nil
}

type harness struct {
	t       *testing.T
	root    string
	data    string
	connect *fakeConnect
	ha      *fakeHA
	engine  *Engine
	logs    *bytes.Buffer
}

func write(t *testing.T, root, rel, body string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func newHarness(t *testing.T, mutate func(*options.Options)) *harness {
	t.Helper()
	root := t.TempDir()
	data := t.TempDir()
	write(t, root, "configuration.yaml", `xiaomi_miio:
  token: !secret xiaomi_fan_token
template: !include templates.yaml
notify:
  - platform: x
    api_key: !secret notify_key
`)
	write(t, root, "templates.yaml", "- sensor:\n    - name: t\n      state: !secret xiaomi_fan_token\n")
	write(t, root, "esphome/node.yaml", "api:\n  encryption:\n    key: !secret node_api_key\n")
	write(t, root, "esphome/secrets.yaml", "node_api_key: "+sNode+"\nwifi_ssid: home\n")
	write(t, root, "zigbee2mqtt/configuration.yaml", "mqtt:\n  password: '!secret mqtt_password'\n")
	write(t, root, "secrets.yaml", "# existing comment\nxiaomi_fan_token: "+sFan+"\nhand_written: keep-me\nmqtt_password: "+sMQTT+"\nnotify_key: "+sNotif+"\n")

	opts := options.Defaults()
	opts.ConnectToken = "tok"
	// As main leaves them: the bytes wiped, the flag kept.
	opts.CredentialsSet = true
	opts.SecretsFiles = []string{"secrets.yaml", "esphome/secrets.yaml", "zigbee2mqtt/secret.yaml"}
	if mutate != nil {
		mutate(&opts)
	}

	conn := newFakeConnect()
	conn.set("i1", "homeassistant fan", 1, map[string]string{"xiaomi_fan_token": sFan, "rotated_at": "2025-01-01", "interval": "365d", "rotation": "r3-manual"})
	conn.set("i2", "homeassistant mqtt", 1, map[string]string{"mqtt_password": sMQTT})
	conn.set("i3", "esphome node", 1, map[string]string{"node_api_key": sNode})
	conn.set("i4", "homeassistant gitops", 1, map[string]string{"gitops_git_token": sGitTok})
	conn.set("i5", "homeassistant notify", 1, map[string]string{"notify_key": sNotif})

	logs := &bytes.Buffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	fha := newFakeHA()
	e, err := New(Config{
		Options:                opts,
		Version:                "test",
		SelfSlug:               "self_slug",
		Root:                   root,
		StatePath:              filepath.Join(data, "state.json"),
		KeyPath:                filepath.Join(data, "fingerprint.key"),
		ActivityPath:           filepath.Join(data, "activity.jsonl"),
		PreviousDir:            filepath.Join(data, "previous"),
		SupervisorSecretsDelay: time.Millisecond,
		RestartDownTimeout:     time.Second,
		RestartUpTimeout:       time.Second,
	}, conn, fha, nil)
	if err != nil {
		t.Fatal(err)
	}
	return &harness{t: t, root: root, data: data, connect: conn, ha: fha, engine: e, logs: logs}
}

func (h *harness) read(rel string) map[string]any {
	h.t.Helper()
	b, err := os.ReadFile(filepath.Join(h.root, filepath.FromSlash(rel)))
	if err != nil {
		h.t.Fatalf("reading %s: %v", rel, err)
	}
	var m map[string]any
	if err := yaml.Unmarshal(b, &m); err != nil {
		h.t.Fatalf("%s: %v", rel, err)
	}
	return m
}

func (h *harness) sync() Status {
	h.engine.SyncOnce(context.Background())
	return h.engine.Status()
}

// assertNoLeak checks every place a value must never reach.
func (h *harness) assertNoLeak() {
	h.t.Helper()
	st := h.engine.Status()
	statusJSON, _ := json.Marshal(st)
	places := map[string]string{
		"status":        string(statusJSON),
		"status %+v":    fmt.Sprintf("%+v", st),
		"logs":          h.logs.String(),
		"ha calls":      h.ha.callList() + strings.Join(h.ha.payloads, "\n"),
		"history":       fmt.Sprintf("%+v", h.engine.History(1000)),
		"state.json":    readOr(filepath.Join(h.data, "state.json")),
		"activity file": readOr(filepath.Join(h.data, "activity.jsonl")),
	}
	for name, text := range places {
		for _, s := range sentinels {
			if strings.Contains(text, s) {
				h.t.Fatalf("value %s leaked into %s", s, name)
			}
		}
	}
	if entries, _ := os.ReadDir(filepath.Join(h.data, "previous")); len(entries) != 0 {
		h.t.Fatalf("plaintext previous copies left behind: %v", entries)
	}
}

func readOr(p string) string {
	b, _ := os.ReadFile(p)
	return string(b)
}

func TestAdoptThenRotate(t *testing.T) {
	h := newHarness(t, nil)
	st := h.sync()

	root := h.read("secrets.yaml")
	if root["xiaomi_fan_token"] != sFan || root["hand_written"] != "keep-me" || root["mqtt_password"] != sMQTT || root["gitops_git_token"] != sGitTok {
		t.Fatalf("root file = %v", root)
	}
	esp := h.read("esphome/secrets.yaml")
	if esp["node_api_key"] != sNode || esp["wifi_ssid"] != "home" || esp["mqtt_password"] != nil {
		t.Fatalf("esphome file = %v", esp)
	}
	z2m := h.read("zigbee2mqtt/secret.yaml")
	if len(z2m) != 1 || z2m["mqtt_password"] != sMQTT {
		t.Fatalf("zigbee2mqtt file = %v", z2m)
	}
	if !strings.Contains(readOr(filepath.Join(h.root, "secrets.yaml")), "# existing comment") {
		t.Fatal("comment lost")
	}
	if st.State != StateHealthy && st.State != StateAttention {
		t.Fatalf("state = %s %s %+v", st.State, st.Headline, st.Problems)
	}
	// Adoption: values that did not change trigger no reload or restart.
	// Only Z2M, whose secret.yaml did not exist, is restarted.
	calls := h.ha.callList()
	for _, unwanted := range []string{"restart_core", "service template.reload", "restart_addon core_mosquitto", "notify onepassword_secrets_devices"} {
		if strings.Contains(calls, unwanted) {
			t.Fatalf("adoption refreshed unchanged values (%s):\n%s", unwanted, calls)
		}
	}
	if !strings.Contains(calls, "restart_addon 45df7312_zigbee2mqtt") {
		t.Fatalf("Z2M not restarted for its new file:\n%s", calls)
	}
	rows := map[string]SecretRow{}
	for _, r := range st.Secrets {
		rows[r.Key] = r
	}
	if rows["hand_written"].State != RowUnmanaged || rows["xiaomi_fan_token"].State != RowSynced || !rows["xiaomi_fan_token"].Overdue {
		t.Fatalf("rows = %+v", rows)
	}
	if got := rows["mqtt_password"].UsedBy; len(got) != 2 {
		t.Fatalf("mqtt_password used by = %+v", got)
	}
	h.assertNoLeak()

	// A second sync with nothing changed fetches nothing and writes nothing.
	h.ha.reset()
	fetches := h.connect.fetches
	before := readOr(filepath.Join(h.root, "secrets.yaml"))
	h.sync()
	if h.connect.fetches != fetches || readOr(filepath.Join(h.root, "secrets.yaml")) != before {
		t.Fatal("an unchanged vault was refetched or rewritten")
	}

	// Rotate the fan token: xiaomi_miio has no reload service -> guarded
	// restart; template reloads.
	h.ha.reset()
	h.connect.set("i1", "homeassistant fan", 2, map[string]string{"xiaomi_fan_token": sFan2, "rotated_at": "2026-10-01", "interval": "365d"})
	st = h.sync()
	if h.read("secrets.yaml")["xiaomi_fan_token"] != sFan2 {
		t.Fatal("rotated value not written")
	}
	calls = h.ha.callList()
	for _, want := range []string{"check_config", "service template.reload", "restart_core", "wait_healthy", "event onepassword_secrets_changed"} {
		if !strings.Contains(calls, want) {
			t.Fatalf("missing %q in calls:\n%s", want, calls)
		}
	}
	if strings.Contains(calls, "restart_addon") {
		t.Fatalf("no add-on uses the fan token:\n%s", calls)
	}
	for _, r := range st.Secrets {
		if r.Key == "xiaomi_fan_token" && r.Overdue {
			t.Fatal("rotation metadata not refreshed")
		}
	}
	h.assertNoLeak()

	// Rotate the MQTT password: Mosquitto (option !secret) and Z2M (string
	// ref in its directory) restart; HA does not.
	h.ha.reset()
	h.connect.set("i2", "homeassistant mqtt", 2, map[string]string{"mqtt_password": "SENTINEL-mqtt-pass-2"})
	sentinels = append(sentinels, "SENTINEL-mqtt-pass-2")
	h.sync()
	calls = h.ha.callList()
	if !strings.Contains(calls, "restart_addon core_mosquitto") || !strings.Contains(calls, "restart_addon 45df7312_zigbee2mqtt") || strings.Contains(calls, "restart_core") || strings.Contains(calls, "self_slug") && strings.Contains(calls, "restart_addon self_slug") {
		t.Fatalf("calls:\n%s", calls)
	}
	if h.read("zigbee2mqtt/secret.yaml")["mqtt_password"] != "SENTINEL-mqtt-pass-2" {
		t.Fatal("z2m file not updated")
	}

	// ESPHome key: devices notified, nothing restarted.
	h.ha.reset()
	h.connect.set("i3", "esphome node", 2, map[string]string{"node_api_key": "SENTINEL-node-2"})
	sentinels = append(sentinels, "SENTINEL-node-2")
	h.sync()
	calls = h.ha.callList()
	if !strings.Contains(calls, "notify onepassword_secrets_devices") || strings.Contains(calls, "restart") {
		t.Fatalf("calls:\n%s", calls)
	}
	h.assertNoLeak()
}

func TestRestartFailureRollsBackAndHoldsBack(t *testing.T) {
	h := newHarness(t, nil)
	h.sync()
	h.ha.healthyBack = false
	h.connect.set("i1", "homeassistant fan", 2, map[string]string{"xiaomi_fan_token": sFan2})
	st := h.sync()
	if got := h.read("secrets.yaml")["xiaomi_fan_token"]; got != sFan {
		t.Fatalf("not rolled back: %v", got)
	}
	calls := h.ha.callList()
	if strings.Count(calls, "restart_core") != 2 || !strings.Contains(calls, "notify onepassword_secrets_error") {
		t.Fatalf("calls:\n%s", calls)
	}
	if st.State != StateError {
		t.Fatalf("state = %s", st.State)
	}
	// Same change next poll: held back, not retried.
	h.ha.reset()
	h.ha.healthyBack = true
	h.sync()
	if strings.Contains(h.ha.callList(), "restart_core") || h.read("secrets.yaml")["xiaomi_fan_token"] != sFan {
		t.Fatalf("held-back change retried:\n%s", h.ha.callList())
	}
	// Sync now clears the hold.
	h.engine.st.HeldBack = ""
	h.sync()
	if h.read("secrets.yaml")["xiaomi_fan_token"] != sFan2 {
		t.Fatal("change not applied after the hold was cleared")
	}
	h.assertNoLeak()
}

func TestCheckConfigFailureRollsBack(t *testing.T) {
	h := newHarness(t, nil)
	h.sync()
	h.ha.checkOK = false
	h.connect.set("i5", "homeassistant notify", 2, map[string]string{"notify_key": "SENTINEL-notify-2"})
	sentinels = append(sentinels, "SENTINEL-notify-2")
	h.sync()
	if got := h.read("secrets.yaml")["notify_key"]; got != sNotif {
		t.Fatalf("not rolled back: %v", got)
	}
	if strings.Contains(h.ha.callList(), "restart_core") {
		t.Fatal("restarted despite a failed check")
	}
	h.assertNoLeak()
}

func TestNotifyModeAndDryRun(t *testing.T) {
	h := newHarness(t, func(o *options.Options) { o.CoreRestart = "notify" })
	h.sync()
	h.connect.set("i1", "homeassistant fan", 2, map[string]string{"xiaomi_fan_token": sFan2})
	st := h.sync()
	if strings.Contains(h.ha.callList(), "restart_core") || len(st.RestartPending) == 0 || !strings.Contains(h.ha.callList(), "notify onepassword_secrets_restart") {
		t.Fatalf("notify mode: pending %v calls:\n%s", st.RestartPending, h.ha.callList())
	}
	if err := h.engine.RestartCoreNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st := h.engine.Status(); len(h.engine.st.RestartPending) != 0 {
		t.Fatalf("pending not cleared: %v", st.RestartPending)
	}

	d := newHarness(t, func(o *options.Options) { o.DryRun = true })
	before := readOr(filepath.Join(d.root, "secrets.yaml"))
	st = d.sync()
	if readOr(filepath.Join(d.root, "secrets.yaml")) != before {
		t.Fatal("dry run wrote the file")
	}
	if _, err := os.Stat(filepath.Join(d.root, "zigbee2mqtt/secret.yaml")); err == nil {
		t.Fatal("dry run created a file")
	}
	var pending int
	for _, r := range st.Secrets {
		if r.State == RowPending {
			pending++
		}
	}
	if pending == 0 || !strings.HasPrefix(st.Detail, "Dry run") {
		t.Fatalf("dry run status: %s %+v", st.Detail, st.Files)
	}
	d.assertNoLeak()
}

func TestConflictKeepsLastGoodAndMissingIsReported(t *testing.T) {
	h := newHarness(t, nil)
	h.sync()
	h.connect.set("i9", "duplicate", 1, map[string]string{"xiaomi_fan_token": "SENTINEL-dup"})
	sentinels = append(sentinels, "SENTINEL-dup")
	write(t, h.root, "configuration.yaml", readOr(filepath.Join(h.root, "configuration.yaml"))+"other:\n  k: !secret not_anywhere\n")
	st := h.sync()
	if h.read("secrets.yaml")["xiaomi_fan_token"] != sFan {
		t.Fatal("conflicted key lost its last good value")
	}
	var titles []string
	for _, p := range st.Problems {
		titles = append(titles, p.Title)
	}
	joined := strings.Join(titles, "|")
	if !strings.Contains(joined, "xiaomi_fan_token comes from more than one field") || !strings.Contains(joined, "not_anywhere is missing") || st.State != StateError {
		t.Fatalf("problems = %v state %s", titles, st.State)
	}
	h.assertNoLeak()
}

func TestRefusesSymlinkOutsideRoot(t *testing.T) {
	h := newHarness(t, nil)
	outside := filepath.Join(t.TempDir(), "target.yaml")
	write(t, filepath.Dir(outside), "target.yaml", "a: b\n")
	if err := os.Remove(filepath.Join(h.root, "secrets.yaml")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(h.root, "secrets.yaml")); err != nil {
		t.Fatal(err)
	}
	st := h.sync()
	if readOr(outside) != "a: b\n" {
		t.Fatal("wrote through a symlink out of the config directory")
	}
	if st.State != StateError {
		t.Fatalf("state = %s", st.State)
	}
}

func TestUnconfiguredAndNotSynced(t *testing.T) {
	h := newHarness(t, func(o *options.Options) { o.ConnectToken = "" })
	if st := h.sync(); st.State != StateUnconfigured || len(st.Setup) == 0 || st.Setup[1].Done {
		t.Fatalf("unconfigured = %+v", st)
	}
	h2 := newHarness(t, nil)
	h2.connect.healthy = false
	if st := h2.sync(); st.State != StateStarting {
		t.Fatalf("not synced = %s", st.State)
	}
}

type fakeProcs []connectd.ProcState

func (f fakeProcs) Status() []connectd.ProcState { return f }

func TestConnectDownAndCrashLooping(t *testing.T) {
	h := newHarness(t, nil)
	h.connect.down = true
	if st := h.sync(); st.State != StateError || st.Headline != "Connect is not answering" {
		t.Fatalf("down = %s %q", st.State, st.Headline)
	}
	looping := fakeProcs{
		{Name: "connect-api", Restarts: 1, QuickExits: 1, LastExitAt: time.Now()},
		{Name: "connect-sync", Restarts: 5, QuickExits: 3, LastExitAt: time.Now().Add(-10 * time.Second)},
	}
	h.engine.procs = looping
	if st := h.sync(); st.State != StateError || st.Headline != "Connect keeps stopping" {
		t.Fatalf("crash loop = %s %q", st.State, st.Headline)
	}
	for _, procs := range []fakeProcs{
		{{Name: "connect-sync", Restarts: 2, QuickExits: 2, LastExitAt: time.Now()}},
		{{Name: "connect-sync", Restarts: 9, QuickExits: 1, LastExitAt: time.Now()}},
		{{Name: "connect-sync", Restarts: 5, QuickExits: 5, LastExitAt: time.Now().Add(-time.Hour)}},
	} {
		h.engine.procs = procs
		if st := h.sync(); st.Headline != "Connect is not answering" {
			t.Fatalf("%+v: headline = %q", procs, st.Headline)
		}
	}

	// connect-api answers /health with connect-sync down once its
	// database exists: the loop must still be named, not "waiting".
	h.connect.down = false
	h.connect.healthy = false
	h.engine.procs = looping
	if st := h.sync(); st.State != StateError || st.Headline != "Connect keeps stopping" {
		t.Fatalf("sync crash loop = %s %q", st.State, st.Headline)
	}
	h.engine.procs = nil
	if st := h.sync(); st.State != StateStarting {
		t.Fatalf("not synced = %s", st.State)
	}
}

func TestConnectStartingIsNotAnError(t *testing.T) {
	h := newHarness(t, nil)
	h.sync()
	managed := h.engine.Status().Counts.Managed
	h.connect.down = true
	h.engine.procs = fakeProcs{
		{Name: "connect-sync", Running: true, StartedAt: time.Now().Add(-2 * time.Second)},
		{Name: "connect-api", Running: true, StartedAt: time.Now().Add(-2 * time.Second)},
	}
	st := h.sync()
	if st.State != StateStarting || st.Headline != "Starting Connect" || st.Counts.Managed != managed || managed == 0 {
		t.Fatalf("just started = %s %q managed %d/%d", st.State, st.Headline, st.Counts.Managed, managed)
	}
	h.ha.mu.Lock()
	calls := strings.Join(h.ha.calls, "\n")
	h.ha.mu.Unlock()
	if strings.Contains(calls, "notify "+notifyError) {
		t.Fatalf("error notification while Connect starts:\n%s", calls)
	}
	h.engine.procs = fakeProcs{{Name: "connect-api", Running: true, StartedAt: time.Now().Add(-5 * time.Minute)}}
	if st := h.sync(); st.State != StateError || st.Headline != "Connect is not answering" {
		t.Fatalf("long down = %s %q", st.State, st.Headline)
	}
}

func TestCheckConfigTextNeverLeaks(t *testing.T) {
	h := newHarness(t, nil)
	h.sync()
	h.ha.checkOK = false
	h.ha.checkReason = "Invalid config for 'notify' at configuration.yaml, line 4: expected a url, got 'SENTINEL-notify-3'"
	sentinels = append(sentinels, "SENTINEL-notify-3")
	h.connect.set("i5", "homeassistant notify", 3, map[string]string{"notify_key": "SENTINEL-notify-3"})
	st := h.sync()
	if st.State != StateError || !strings.Contains(st.Detail, "Invalid config for 'notify' at configuration.yaml, line 4") {
		t.Fatalf("status = %s %q", st.State, st.Detail)
	}
	h.assertNoLeak()
}

func TestMissingVaultRemovesNothing(t *testing.T) {
	h := newHarness(t, nil)
	h.sync()
	h.engine.cfg.Options.Vaults = []string{"homeassistant", "gone"}
	h.connect.mu.Lock()
	delete(h.connect.items, "i4") // gitops_git_token vanishes from the view too
	h.connect.mu.Unlock()
	st := h.sync()
	if h.read("secrets.yaml")["gitops_git_token"] != sGitTok {
		t.Fatal("a key was removed while a vault was missing")
	}
	if st.State != StateError {
		t.Fatalf("state = %s", st.State)
	}
}

func TestRefusedRestartIsAFailure(t *testing.T) {
	h := newHarness(t, nil)
	h.sync()
	h.ha.restartErr = &ha.StatusError{Path: "/core/api/services/homeassistant/restart", Code: 500, Detail: "SENTINEL-in-detail"}
	sentinels = append(sentinels, "SENTINEL-in-detail")
	h.connect.set("i1", "homeassistant fan", 2, map[string]string{"xiaomi_fan_token": sFan2})
	st := h.sync()
	if h.read("secrets.yaml")["xiaomi_fan_token"] != sFan {
		t.Fatal("a refused restart was taken as applied")
	}
	if strings.Contains(h.ha.callList(), "wait_healthy") || st.State != StateError {
		t.Fatalf("state %s calls:\n%s", st.State, h.ha.callList())
	}
	h.assertNoLeak()
}

func TestInterruptedRefreshResumes(t *testing.T) {
	h := newHarness(t, nil)
	h.sync()
	h.engine.cfg.SupervisorSecretsDelay = time.Hour
	h.connect.set("i2", "homeassistant mqtt", 2, map[string]string{"mqtt_password": "SENTINEL-mqtt-3"})
	sentinels = append(sentinels, "SENTINEL-mqtt-3")
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(200 * time.Millisecond); cancel() }()
	h.engine.SyncOnce(ctx)
	if h.engine.st.Pending == nil || h.engine.st.Pending.Stage != "addons" {
		t.Fatalf("pending not kept: %+v", h.engine.st.Pending)
	}
	if strings.Contains(h.ha.callList(), "restart_addon core_mosquitto") {
		t.Fatal("restarted before the delay")
	}
	// A fresh engine on the same /data, as after an add-on restart.
	h.ha.reset()
	e2, err := New(Config{
		Options: h.engine.cfg.Options, SelfSlug: "self_slug", Root: h.root,
		StatePath: h.engine.cfg.StatePath, KeyPath: h.engine.cfg.KeyPath, ActivityPath: h.engine.cfg.ActivityPath,
		PreviousDir: h.engine.cfg.PreviousDir, SupervisorSecretsDelay: time.Millisecond,
		RestartDownTimeout: time.Second, RestartUpTimeout: time.Second,
	}, h.connect, h.ha, nil)
	if err != nil {
		t.Fatal(err)
	}
	h.engine = e2
	h.sync()
	if !strings.Contains(h.ha.callList(), "restart_addon core_mosquitto") || h.engine.st.Pending != nil {
		t.Fatalf("not resumed: pending %+v calls:\n%s", h.engine.st.Pending, h.ha.callList())
	}
	h.assertNoLeak()
}

func TestOutsideRestartClearsPending(t *testing.T) {
	h := newHarness(t, func(o *options.Options) { o.CoreRestart = "notify" })
	h.sync()
	h.connect.set("i1", "homeassistant fan", 2, map[string]string{"xiaomi_fan_token": sFan2})
	if st := h.sync(); len(st.RestartPending) == 0 {
		t.Fatal("no restart pending")
	}
	h.ha.coreRestarted = true
	h.engine.publishedAt = time.Now().Add(-time.Minute)
	if st := h.sync(); len(st.RestartPending) != 0 {
		t.Fatalf("pending after an outside restart: %v", st.RestartPending)
	}
}

func TestFirstSyncIsKicked(t *testing.T) {
	h := newHarness(t, nil)
	h.connect.needsKick = true
	st := h.sync()
	if st.State == StateStarting || h.read("secrets.yaml")["mqtt_password"] != sMQTT {
		t.Fatalf("Connect was never asked with the token: %s %q", st.State, st.Headline)
	}
}
