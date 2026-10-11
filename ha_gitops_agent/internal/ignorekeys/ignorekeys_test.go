package ignorekeys

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// sentinel is the literal password the fixtures carry. No output of this
// package may contain it unless the input it came from was meant to.
const sentinel = "S3ntinel-mqtt-pw-7f2c"

// repoZ2M is Zigbee2MQTT's configuration.yaml as the repository holds it:
// four-space indentation and no MQTT login, which the add-on supplies
// through its environment.
const repoZ2M = `homeassistant:
    enabled: true
frontend:
    enabled: true
    port: 8099
mqtt:
    base_topic: zigbee2mqtt
serial:
    port: /dev/ttyUSB0
    adapter: zstack
advanced:
    log_level: info
    network_key: '!secret.yaml network_key'
devices:
    '0x00158d0001a2b3c4':
        friendly_name: kitchen_sensor
`

// liveZ2M is the same file after a Zigbee2MQTT settings write: two-space
// indentation, other quoting, and the login from the environment persisted
// at the end of the mqtt mapping.
const liveZ2M = `homeassistant:
  enabled: true
frontend:
  enabled: true
  port: 8099
mqtt:
  base_topic: zigbee2mqtt
  server: mqtt://core-mosquitto:1883
  user: addons
  password: ` + sentinel + `
serial:
  port: "/dev/ttyUSB0"
  adapter: zstack
advanced:
  log_level: info
  network_key: "!secret.yaml network_key"
devices:
  "0x00158d0001a2b3c4":
    friendly_name: kitchen_sensor
`

var mqttLogin = []KeyPath{{"mqtt", "server"}, {"mqtt", "user"}, {"mqtt", "password"}}

func TestParseEntry(t *testing.T) {
	got, err := ParseEntry("zigbee2mqtt/configuration.yaml:mqtt.password")
	if err != nil {
		t.Fatalf("ParseEntry: %v", err)
	}
	if got.File != "zigbee2mqtt/configuration.yaml" || got.Key.String() != "mqtt.password" || len(got.Key) != 2 {
		t.Errorf("ParseEntry = %+v, want zigbee2mqtt/configuration.yaml and mqtt.password", got)
	}
	for _, ok := range []string{"configuration.yaml:http", "a/b.YML:x.y.z", "esphome/node.yml:wifi.ap.password"} {
		if _, err := ParseEntry(ok); err != nil {
			t.Errorf("ParseEntry(%q): %v, want it accepted", ok, err)
		}
	}
}

func TestParseEntryRefusesBadEntriesWithoutQuotingThem(t *testing.T) {
	for _, tc := range []struct {
		entry, want string
	}{
		{"", "is empty"},
		{" z.yaml:" + sentinel, "leading or trailing spaces"},
		{"z.yaml:a\t" + sentinel, "control character"},
		{"z.yaml." + sentinel, "has no ':'"},
		{":" + sentinel, "names no file"},
		{"z.yaml:", "names no key"},
		{"z.yaml :" + sentinel, "space next to the ':'"},
		{"/config/z.yaml:" + sentinel, "absolute path"},
		{`z2m\z.yaml:` + sentinel, "backslash"},
		{"z2m/*.yaml:" + sentinel, "pattern"},
		{"z2m//z.yaml:" + sentinel, "empty path segment"},
		{"../z.yaml:" + sentinel, `".." segment`},
		{"z2m/./z.yaml:" + sentinel, `"." segment`},
		{"z2m/settings.json:" + sentinel, "not YAML"},
		{"z2m/notes:" + sentinel, "not YAML"},
		{"z.yaml:mqtt..password" + sentinel, "empty key segment"},
		{"z.yaml:." + sentinel, "empty key segment"},
		{"z.yaml:" + sentinel + ".", "empty key segment"},
		{"z.yaml:devices[0]." + sentinel, "indexes a list"},
		{"z.yaml:mqtt.password: " + sentinel, "looks like a value"},
		{"z.yaml:mqtt.password=" + sentinel, "looks like a value"},
		{"z.yaml:mqtt.pass word" + sentinel, "looks like a value"},
		{"z.yaml:mqtt.password:", "looks like a value"},
		{"z.yaml:mqtt.password:" + sentinel, "looks like a value"},
		{"z.yaml:mqtt.password\u00a0" + sentinel, "looks like a value"},
		{"z.yaml:mqtt.pass\u0085word" + sentinel, "control character"},
		{"z.yaml:mqtt.pass\u200bword" + sentinel, "invisible formatting character"},
		{"\ufeffz.yaml:mqtt.password" + sentinel, "invisible formatting character"},
	} {
		_, err := ParseEntry(tc.entry)
		if err == nil {
			t.Errorf("ParseEntry(%q) = nil error, want %q", tc.entry, tc.want)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("ParseEntry(%q) error = %q, want it to say %q", tc.entry, err, tc.want)
		}
		if strings.Contains(err.Error(), sentinel) {
			t.Errorf("ParseEntry(%q) error quotes the entry: %q", tc.entry, err)
		}
	}
}

func canonical(t *testing.T, keys []KeyPath, data string) string {
	t.Helper()
	out, err := Canonical(keys, []byte(data))
	if err != nil {
		t.Fatalf("Canonical(%q): %v", data, err)
	}
	return string(out)
}

// The production case: everything that differs between the two copies is
// the app's login and the app's formatting.
func TestCanonicalSetsAsideTheIgnoredKeysAndTheFormatting(t *testing.T) {
	repo, live := canonical(t, mqttLogin, repoZ2M), canonical(t, mqttLogin, liveZ2M)
	if repo != live {
		t.Fatalf("canonical forms differ:\nrepo:\n%s\nlive:\n%s", repo, live)
	}
	if strings.Contains(live, sentinel) || strings.Contains(live, "core-mosquitto") || strings.Contains(live, "addons") {
		t.Errorf("canonical form still carries an ignored key:\n%s", live)
	}
	if !strings.Contains(live, "base_topic: zigbee2mqtt") {
		t.Errorf("canonical form lost a key that is not ignored:\n%s", live)
	}

	// Without the entries the same two files are not equal: it is the
	// option, not the normalization alone, that settles them.
	if canonical(t, nil, repoZ2M) == canonical(t, nil, liveZ2M) {
		t.Error("the copies compare equal with no keys ignored")
	}
}

func TestCanonicalStillSeesEveryOtherChange(t *testing.T) {
	for _, tc := range []struct {
		name, edit string
	}{
		{"a value", strings.Replace(liveZ2M, "port: 8099", "port: 8100", 1)},
		{"a key that is not ignored, beside the ignored ones", strings.Replace(liveZ2M, "  user: addons\n", "  user: addons\n  keepalive: 30\n", 1)},
		{"a type", strings.Replace(liveZ2M, "enabled: true\nfrontend", "enabled: \"true\"\nfrontend", 1)},
		{"key order", strings.Replace(liveZ2M, "  enabled: true\n  port: 8099\n", "  port: 8099\n  enabled: true\n", 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.edit == liveZ2M {
				t.Fatal("the edit did not apply to the fixture")
			}
			if canonical(t, mqttLogin, repoZ2M) == canonical(t, mqttLogin, tc.edit) {
				t.Error("canonical forms are equal, want the change to count")
			}
		})
	}
}

func TestCanonicalIgnoresCommentsQuotingAndFlowStyle(t *testing.T) {
	a := "# managed in git\nmqtt:\n  base_topic: zigbee2mqtt # the default\n  list: [a, 'b']\n"
	b := "mqtt: {base_topic: \"zigbee2mqtt\", list: [\"a\", b], password: " + sentinel + "}\n"
	if canonical(t, mqttLogin, a) != canonical(t, mqttLogin, b) {
		t.Errorf("canonical forms differ:\n%s\n%s", canonical(t, mqttLogin, a), canonical(t, mqttLogin, b))
	}
}

// A parent the app added only to hold its keys must not keep the two sides
// apart, whether the other side lacks it, has it empty, or has it null.
func TestCanonicalDropsAParentLeftHoldingNothing(t *testing.T) {
	live := "frontend: true\nmqtt:\n  password: " + sentinel + "\n"
	for _, repo := range []string{"frontend: true\n", "frontend: true\nmqtt: {}\n", "frontend: true\nmqtt:\n"} {
		if got, want := canonical(t, mqttLogin, repo), canonical(t, mqttLogin, live); got != want {
			t.Errorf("Canonical(%q) = %q, want %q", repo, got, want)
		}
	}
	// A mapping that is not on an ignored key's path stays, empty or not.
	if got := canonical(t, mqttLogin, "frontend: {}\n"); got != "frontend: {}\n" {
		t.Errorf("Canonical = %q, want the unrelated empty mapping kept", got)
	}
}

func TestCanonicalOfNothing(t *testing.T) {
	for _, in := range []string{"", "# only a comment\n", "---\n", "~\n"} {
		if got := canonical(t, mqttLogin, in); got != "" {
			t.Errorf("Canonical(%q) = %q, want empty", in, got)
		}
	}
	// A root that is not a mapping has no keys to ignore and is still
	// normalized.
	if got := canonical(t, mqttLogin, "-   a\n-   'b'\n"); got != "- a\n- b\n" {
		t.Errorf("Canonical(list) = %q", got)
	}
}

func TestCanonicalRefusesWhatItCannotRead(t *testing.T) {
	for _, tc := range []struct {
		name, data string
		want       error
	}{
		{"broken YAML", "mqtt:\n  password: " + sentinel + "\n bad: [\n", ErrNotYAML},
		{"a tab for indentation", "mqtt:\n\tpassword: " + sentinel + "\n", ErrNotYAML},
		{"two documents", "a: 1\n---\nmqtt:\n  password: " + sentinel + "\n", ErrNotYAML},
		{"a NUL byte", "mqtt:\n  password: " + sentinel + "\x00\n", ErrNotYAML},
		{"invalid UTF-8", "mqtt:\n  password: " + sentinel + "\xff\n", ErrNotYAML},
		{"an anchored value", "mqtt:\n  password: &pw " + sentinel + "\nother: *pw\n", ErrAnchored},
		{"an alias as the value", "shared: &pw " + sentinel + "\nmqtt:\n  password: *pw\n", ErrAnchored},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Canonical(mqttLogin, []byte(tc.data))
			if !errors.Is(err, tc.want) {
				t.Fatalf("Canonical error = %v, want %v", err, tc.want)
			}
			if strings.Contains(err.Error(), sentinel) {
				t.Errorf("error quotes the file: %q", err)
			}
		})
	}
}

func overlay(t *testing.T, keys []KeyPath, target, source string) string {
	t.Helper()
	var src []byte
	if source != "<nil>" {
		src = []byte(source)
	}
	out, err := Overlay(keys, []byte(target), src)
	if err != nil {
		t.Fatalf("Overlay: %v", err)
	}
	return string(out)
}

func TestOverlay(t *testing.T) {
	password := []KeyPath{{"mqtt", "password"}}
	for _, tc := range []struct {
		name                 string
		keys                 []KeyPath
		target, source, want string
	}{
		{
			name:   "the key is added at the end of the mapping that has it",
			keys:   password,
			target: "mqtt:\n    base_topic: z2m # keep me\nother: 1\n",
			source: "mqtt:\n  password: hunter2\n",
			want:   "mqtt:\n  base_topic: z2m # keep me\n  password: hunter2\nother: 1\n",
		},
		{
			name:   "a missing parent is created",
			keys:   password,
			target: "other: 1\n",
			source: "mqtt:\n  password: hunter2\n",
			want:   "other: 1\nmqtt:\n  password: hunter2\n",
		},
		{
			name:   "a null parent is filled in",
			keys:   password,
			target: "mqtt:\nother: 1\n",
			source: "mqtt:\n  password: hunter2\n",
			want:   "mqtt:\n  password: hunter2\nother: 1\n",
		},
		{
			name:   "a different value is replaced where it stands",
			keys:   password,
			target: "mqtt:\n  password: '!secret mqtt_password'\n  base_topic: z2m\n",
			source: "mqtt:\n  password: hunter2\n",
			want:   "mqtt:\n  password: hunter2\n  base_topic: z2m\n",
		},
		{
			name:   "a key the source lacks is removed, and a parent it emptied with it",
			keys:   []KeyPath{{"mqtt", "user"}, {"mqtt", "password"}},
			target: "mqtt:\n  user: addons\n  password: hunter2\nother: 1\n",
			source: "other: 2\n",
			want:   "other: 1\n",
		},
		{
			name:   "a nil source has none of the keys",
			keys:   password,
			target: "mqtt:\n  base_topic: z2m\n  password: hunter2\n",
			source: "<nil>",
			want:   "mqtt:\n  base_topic: z2m\n",
		},
		{
			name:   "a parent that is a list is left alone",
			keys:   password,
			target: "mqtt:\n  - a\n",
			source: "mqtt:\n  password: hunter2\n",
			want:   "mqtt:\n  - a\n",
		},
		{
			name:   "a target with no top-level mapping is left alone",
			keys:   password,
			target: "- a\n",
			source: "mqtt:\n  password: hunter2\n",
			want:   "- a\n",
		},
		{
			name:   "an empty target stays empty",
			keys:   password,
			target: "",
			source: "mqtt:\n  password: hunter2\n",
			want:   "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := overlay(t, tc.keys, tc.target, tc.source); got != tc.want {
				t.Errorf("Overlay =\n%s\nwant\n%s", got, tc.want)
			}
		})
	}
}

// Nothing to change means nothing rewritten: a file is only reformatted
// when one of its ignored keys actually has to move.
func TestOverlayReturnsAnAgreeingTargetByteForByte(t *testing.T) {
	target := "mqtt:\n    base_topic: z2m   # odd spacing\n    password: \"hunter2\"\n"
	for _, source := range []string{"mqtt:\n  password: hunter2\n", "mqtt: {password: 'hunter2'}\n"} {
		if got := overlay(t, mqttLogin, target, source); got != target {
			t.Errorf("Overlay(source %q) = %q, want the target untouched", source, got)
		}
	}
	if got := overlay(t, mqttLogin, repoZ2M, "frontend: true\n"); got != repoZ2M {
		t.Errorf("Overlay with no ignored key on either side rewrote the target:\n%s", got)
	}
}

// The two directions the agent writes in must each land on something the
// next comparison calls in sync, or every cycle would apply (or capture)
// the same file again.
func TestOverlayResultsCompareEqualToWhatTheyWereBuiltFrom(t *testing.T) {
	edited := strings.Replace(liveZ2M, "log_level: info", "log_level: debug", 1)
	repoEdited := strings.Replace(repoZ2M, "port: 8099", "port: 8100", 1)

	// Apply: the repository's content, live's login.
	applied := overlay(t, mqttLogin, repoEdited, edited)
	if !strings.Contains(applied, "password: "+sentinel) || !strings.Contains(applied, "server: mqtt://core-mosquitto:1883") {
		t.Errorf("applied copy lost live's login:\n%s", applied)
	}
	if canonical(t, mqttLogin, applied) != canonical(t, mqttLogin, repoEdited) {
		t.Errorf("an applied copy does not compare equal to the repository:\n%s", applied)
	}

	// Capture: live's content, the repository's (absent) login.
	captured := overlay(t, mqttLogin, edited, repoZ2M)
	if strings.Contains(captured, sentinel) || strings.Contains(captured, "core-mosquitto") {
		t.Errorf("a captured copy carries live's login:\n%s", captured)
	}
	if !strings.Contains(captured, "log_level: debug") {
		t.Errorf("a captured copy lost live's edit:\n%s", captured)
	}
	if canonical(t, mqttLogin, captured) != canonical(t, mqttLogin, edited) {
		t.Errorf("a captured copy does not compare equal to live:\n%s", captured)
	}
	// And what was committed, overlaid back for the next apply, is live
	// again as far as the comparison goes.
	if canonical(t, mqttLogin, overlay(t, mqttLogin, captured, edited)) != canonical(t, mqttLogin, edited) {
		t.Error("a round trip through capture and apply does not settle")
	}
}

func TestOverlayNamesTheSideItCannotUse(t *testing.T) {
	broken := []byte("mqtt:\n  password: " + sentinel + "\n bad: [\n")
	good := []byte("mqtt:\n  password: hunter2\n")

	_, err := Overlay(mqttLogin, broken, good)
	if !errors.Is(err, ErrTarget) || !errors.Is(err, ErrNotYAML) || errors.Is(err, ErrSource) {
		t.Errorf("broken target: error = %v, want ErrTarget and ErrNotYAML", err)
	}
	_, err = Overlay(mqttLogin, good, broken)
	if !errors.Is(err, ErrSource) || !errors.Is(err, ErrNotYAML) || errors.Is(err, ErrTarget) {
		t.Errorf("broken source: error = %v, want ErrSource and ErrNotYAML", err)
	}
	_, err = Overlay(mqttLogin, good, []byte("shared: &pw "+sentinel+"\nmqtt:\n  password: *pw\n"))
	if !errors.Is(err, ErrSource) || !errors.Is(err, ErrAnchored) {
		t.Errorf("aliased source value: error = %v, want ErrSource and ErrAnchored", err)
	}
	_, err = Overlay(mqttLogin, []byte("mqtt:\n  password: &pw x\nother: *pw\n"), good)
	if !errors.Is(err, ErrTarget) || !errors.Is(err, ErrAnchored) {
		t.Errorf("anchored target value: error = %v, want ErrTarget and ErrAnchored", err)
	}
	_, err = Overlay(mqttLogin, []byte("mqtt:\n  password: &pw x\nother: *pw\n"), nil)
	if !errors.Is(err, ErrTarget) || !errors.Is(err, ErrAnchored) {
		t.Errorf("removing an anchored target value: error = %v, want ErrTarget and ErrAnchored", err)
	}
	if err != nil && bytes.Contains([]byte(err.Error()), []byte(sentinel)) {
		t.Errorf("error quotes the file: %q", err)
	}
}

// A key that could sit in more than one place is not moved or compared:
// a repeat, a merge key, or a mapping shared through an alias or anchor
// on the way to it. Without the alias rule, "mqtt: *c" would publish and
// push the password that sits under creds.
func TestAmbiguousPathsAreRefused(t *testing.T) {
	good := "mqtt:\n  password: hunter2\n"
	for _, tc := range []struct {
		name, data string
	}{
		{"a repeated leaf", "mqtt:\n  password: a\n  base_topic: z2m\n  password: " + sentinel + "\n"},
		{"a repeated parent", "mqtt:\n  base_topic: z2m\nmqtt:\n  password: " + sentinel + "\n"},
		{"a merge key on the path", "base: &b {password: " + sentinel + "}\nmqtt:\n  <<: *b\n  base_topic: z2m\n"},
		{"an alias on the path", "creds: &c\n  password: " + sentinel + "\nmqtt: *c\n"},
		{"an anchored parent", "mqtt: &m\n  password: " + sentinel + "\nbackup: *m\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Canonical(mqttLogin, []byte(tc.data)); !errors.Is(err, ErrAmbiguous) {
				t.Errorf("Canonical error = %v, want ErrAmbiguous", err)
			}
			_, err := Overlay(mqttLogin, []byte(tc.data), []byte(good))
			if !errors.Is(err, ErrTarget) || !errors.Is(err, ErrAmbiguous) {
				t.Errorf("Overlay as target: error = %v, want ErrTarget and ErrAmbiguous", err)
			}
			_, err = Overlay(mqttLogin, []byte(good), []byte(tc.data))
			if !errors.Is(err, ErrSource) || !errors.Is(err, ErrAmbiguous) {
				t.Errorf("Overlay as source: error = %v, want ErrSource and ErrAmbiguous", err)
			}
			if err != nil && strings.Contains(err.Error(), sentinel) {
				t.Errorf("error quotes the file: %q", err)
			}
		})
	}
	// A merge key or an alias elsewhere in the file is no business of an
	// ignored key's.
	if _, err := Canonical(mqttLogin, []byte("d: &d {x: 1}\nother:\n  <<: *d\nmqtt:\n  password: x\n")); err != nil {
		t.Errorf("Canonical with a merge key off the path: %v", err)
	}
}

// Zigbee2MQTT reads its file with js-yaml (YAML 1.2) and writes back what
// it read, so every number, boolean and null comes back spelled its way.
func TestScalarsCompareByTheirYAML12Value(t *testing.T) {
	for _, tc := range []struct {
		repo, live string
	}{
		{"pan_id: 0x1a62\n", "pan_id: 6754\n"},
		{"ext_pan_id: [0xDD, 0xDD, 0x0a]\n", "ext_pan_id:\n  - 221\n  - 221\n  - 10\n"},
		{"enabled: True\n", "enabled: true\n"},
		{"last_seen: ~\n", "last_seen: null\n"},
		{"last_seen:\n", "last_seen: null\n"},
		{"channel: 011\n", "channel: 11\n"},
		{"channel: 08\n", "channel: 8\n"},
		{"retain: 1_000\n", "retain: 1000\n"},
		{"rate: 1.0\n", "rate: 1\n"},
		{"rate: 1.50\n", "rate: 1.5\n"},
		{"mode: 0o17\n", "mode: 15\n"},
		{"devices:\n  1:\n    friendly_name: a\n", "devices:\n  '1':\n    friendly_name: a\n"},
		{"devices:\n  0x1a:\n    friendly_name: a\n", "devices:\n  '26':\n    friendly_name: a\n"},
	} {
		if equal, err := Equal(mqttLogin, []byte(tc.repo), []byte(tc.live)); err != nil || !equal {
			t.Errorf("Equal(%q, %q) = %v, %v; want equal", tc.repo, tc.live, equal, err)
		}
	}
	for _, tc := range []struct {
		a, b string
	}{
		{"pan_id: 6754\n", "pan_id: \"6754\"\n"},
		{"enabled: true\n", "enabled: 'true'\n"},
		{"enabled: on\n", "enabled: true\n"},
		{"rate: 1.5\n", "rate: 1.6\n"},
		{"pan_id: !!str 6754\n", "pan_id: 6754\n"},
	} {
		if equal, err := Equal(mqttLogin, []byte(tc.a), []byte(tc.b)); err != nil || equal {
			t.Errorf("Equal(%q, %q) = %v, %v; want different", tc.a, tc.b, equal, err)
		}
	}
}

// The same rule in the overlay: a value spelled differently is not moved,
// and what an apply writes compares equal to the repository.
func TestOverlayComparesScalarsByTheirYAML12Value(t *testing.T) {
	target := "mqtt:\n    password: 0x10\n    base_topic: z2m\n"
	if got := overlay(t, mqttLogin, target, "mqtt:\n  password: 16\n"); got != target {
		t.Errorf("Overlay rewrote a value that only differs in spelling:\n%s", got)
	}

	repo := "advanced:\n    pan_id: 0x1a62\n    ext_pan_id: [0xDD, 0xDD]\n    legacy: True\nmqtt:\n    base_topic: z2m\n"
	live := "advanced:\n  pan_id: 6754\n  ext_pan_id:\n    - 221\n    - 221\n  legacy: true\nmqtt:\n  base_topic: z2m\n  password: " + sentinel + "\n"
	if equal, err := Equal(mqttLogin, []byte(repo), []byte(live)); err != nil || !equal {
		t.Fatalf("Equal = %v, %v; want the rewrite to be in sync", equal, err)
	}
	applied := overlay(t, mqttLogin, repo, live)
	if equal, err := Equal(mqttLogin, []byte(applied), []byte(repo)); err != nil || !equal {
		t.Errorf("the applied copy does not compare equal to the repository (%v):\n%s", err, applied)
	}
	captured := overlay(t, mqttLogin, live, repo)
	if strings.Contains(captured, sentinel) {
		t.Errorf("the captured copy carries the password:\n%s", captured)
	}
	if equal, err := Equal(mqttLogin, []byte(captured), []byte(live)); err != nil || !equal {
		t.Errorf("the captured copy does not compare equal to live (%v):\n%s", err, captured)
	}
}

// The encoder writes an empty null in a flow collection, or as a key, as
// an empty quoted string. An overlay that rewrites such a file must not
// turn its nulls into strings, or the applied copy would never compare
// equal.
func TestOverlayKeepsNullsNullInFlowCollectionsAndKeys(t *testing.T) {
	for _, target := range []string{
		"advanced: {log_level: info, network_key: }\n",
		"map: {a, b}\n",
		"? \n: value\nother: 1\n",
		"list: [a, ~]\n",
	} {
		out := overlay(t, mqttLogin, target, "mqtt:\n  password: hunter2\n")
		if out == target {
			t.Fatalf("Overlay(%q) did not rewrite the target", target)
		}
		if strings.Contains(out, "''") {
			t.Errorf("Overlay(%q) turned a null into a string:\n%s", target, out)
		}
		if equal, err := Equal(mqttLogin, []byte(out), []byte(target)); err != nil || !equal {
			t.Errorf("Overlay(%q) does not compare equal to its target (%v):\n%s", target, err, out)
		}
	}
}

// A merge key elsewhere in the file survives a rewrite as the merge key it
// was, without the tag the encoder would otherwise print.
func TestOverlayKeepsAMergeKeyOffThePathPlain(t *testing.T) {
	target := "defaults: &d\n  retain: true\nother:\n  <<: *d\n  qos: 1\n"
	out := overlay(t, mqttLogin, target, "mqtt:\n  password: hunter2\n")
	if strings.Contains(out, "!!merge") || !strings.Contains(out, "<<: *d") {
		t.Errorf("Overlay =\n%s\nwant the merge key written as <<: *d", out)
	}
}
