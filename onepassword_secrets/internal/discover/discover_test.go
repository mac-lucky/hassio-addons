package discover

import (
	"strings"
	"testing"
	"time"

	"github.com/mac-lucky/hassio-addons/onepassword_secrets/internal/opconnect"
	"github.com/mac-lucky/hassio-addons/onepassword_secrets/internal/secret"
)

func field(label, typ, value string) opconnect.Field {
	return opconnect.Field{ID: "id-" + label, Label: label, Type: typ, Value: secret.New(value)}
}

func vaultWith(name string, discover bool, items ...opconnect.Item) Vault {
	return Vault{Vault: opconnect.Vault{ID: "id-" + name, Name: name}, Items: items, Discover: discover}
}

func TestBuildDiscoversLabelledFields(t *testing.T) {
	fan := opconnect.Item{ID: "i1", Title: "homeassistant fan", Version: 4, Fields: []opconnect.Field{
		field("xiaomi_fan_token", "CONCEALED", "tok"),
		field("rotation", "STRING", "r3-manual"),
		field("rotated_at", "STRING", "2026-01-02"),
		field("interval", "STRING", "365d"),
		field("password", "CONCEALED", "template-field"),
		field("Has Spaces", "CONCEALED", "x"),
		field("otp_seed", "OTP", "x"),
		{ID: "notes", Label: "notesPlain", Purpose: "NOTES", Type: "STRING", Value: secret.New("n")},
	}}
	sa := opconnect.Item{ID: "i2", Title: "homeassistant google", Fields: []opconnect.Field{
		field("google_sa_json", "CONCEALED", `{"type":"service_account","private_key":"-----BEGIN\nX\n-----END\n"}`),
		field("broken_json", "CONCEALED", "not json"),
	}}
	cat := Build([]Vault{vaultWith("homeassistant", true, fan, sa)}, nil)

	if len(cat.Entries) != 2 {
		t.Fatalf("entries = %v", keysOf(cat))
	}
	e := cat.Entries["xiaomi_fan_token"]
	if e.Value.Reveal() != "tok" || e.ItemVersion != 4 || e.Source() != "homeassistant > homeassistant fan > xiaomi_fan_token" {
		t.Fatalf("entry = %+v", e)
	}
	if e.Meta.Rotation != "r3-manual" || e.Meta.IntervalDur != 365*24*time.Hour ||
		!e.Meta.Due().Equal(time.Date(2027, 1, 2, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("meta = %+v due %v", e.Meta, e.Meta.Due())
	}
	if !cat.Entries["google_sa_json"].Structured {
		t.Fatal("_json key not structured")
	}
	if !hasProblem(cat, "not_json", "broken_json") || !cat.Conflicted["broken_json"] {
		t.Fatalf("problems = %+v", cat.Problems)
	}
}

func TestBuildConflictKeepsNeither(t *testing.T) {
	a := opconnect.Item{ID: "a", Title: "one", Fields: []opconnect.Field{field("dup_key", "CONCEALED", "1")}}
	b := opconnect.Item{ID: "b", Title: "two", Fields: []opconnect.Field{field("dup_key", "CONCEALED", "2")}}
	cat := Build([]Vault{vaultWith("homeassistant", true, a, b)}, nil)
	if _, ok := cat.Entries["dup_key"]; ok || !cat.Conflicted["dup_key"] || !hasProblem(cat, "conflict", "dup_key") {
		t.Fatalf("conflict not reported: %+v", cat)
	}
	for _, p := range cat.Problems {
		if strings.Contains(p.Detail, "1") && strings.Contains(p.Detail, "2") && !strings.Contains(p.Detail, "one") {
			t.Fatalf("problem detail looks like it quotes values: %q", p.Detail)
		}
	}
}

func TestBuildReferences(t *testing.T) {
	shared := opconnect.Item{ID: "s1", Title: "victorialogs ha", Fields: []opconnect.Field{
		{ID: "f1", Label: "password", Type: "CONCEALED", Section: "auth", Value: secret.New("vlpw")},
	}}
	local := opconnect.Item{ID: "l1", Title: "local", Fields: []opconnect.Field{field("victorialogs_password", "CONCEALED", "shadowed")}}
	vaults := []Vault{vaultWith("homeassistant", true, local), vaultWith("shared-m2m", false, shared)}
	cat := Build(vaults, []Reference{
		{Key: "victorialogs_password", Ref: "op://shared-m2m/victorialogs ha/auth/password"},
		{Key: "missing_one", Ref: "op://shared-m2m/nope/password"},
		{Key: "bad_one", Ref: "https://x"},
	})
	if got := cat.Entries["victorialogs_password"]; got.Value.Reveal() != "vlpw" || got.Ref == "" {
		t.Fatalf("explicit reference did not win: %+v", got)
	}
	if !hasProblem(cat, "unresolved_reference", "missing_one") || !hasProblem(cat, "unresolved_reference", "bad_one") {
		t.Fatalf("problems = %+v", cat.Problems)
	}
	if _, ok := cat.Entries["missing_one"]; ok {
		t.Fatal("unresolved reference produced a key")
	}
}

func TestParseRef(t *testing.T) {
	for ref, ok := range map[string]bool{
		"op://v/i/f":       true,
		"op://v/i/s/f":     true,
		"op://v/i":         false,
		"op://v//f":        false,
		"op://v/i/s/f/x":   false,
		"vault/item/field": false,
	} {
		if _, err := ParseRef(ref); (err == nil) != ok {
			t.Errorf("ParseRef(%q) err = %v", ref, err)
		}
	}
}

func TestParseInterval(t *testing.T) {
	cases := map[string]time.Duration{"7d": 7 * 24 * time.Hour, "12h": 12 * time.Hour, "on-compromise": 0, "": 0, "-1d": 0, "99999d": 0}
	for in, want := range cases {
		if got := ParseInterval(in); got != want {
			t.Errorf("ParseInterval(%q) = %v, want %v", in, got, want)
		}
	}
}

func keysOf(c Catalog) []string {
	var out []string
	for k := range c.Entries {
		out = append(out, k)
	}
	return out
}

func hasProblem(c Catalog, kind, key string) bool {
	for _, p := range c.Problems {
		if p.Kind == kind && p.Key == key {
			return true
		}
	}
	return false
}
