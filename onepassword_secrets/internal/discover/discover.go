// Package discover turns 1Password items into the set of secrets keys this
// add-on renders. The mapping lives in 1Password itself: a field whose
// label is a valid secrets key (lowercase letters, digits, underscore)
// becomes that key, the same convention as "field label = env var name"
// elsewhere. Explicit op:// references from the add-on options cover keys
// whose field lives somewhere else (another vault, a differently named
// field).
package discover

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mac-lucky/hassio-addons/onepassword_secrets/internal/opconnect"
	"github.com/mac-lucky/hassio-addons/onepassword_secrets/internal/secret"
)

// KeyPattern is what a field label must match to become a secrets key.
var KeyPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_]{0,127}$`)

// StructuredSuffix marks a field whose value is JSON, rendered into the
// secrets file as a YAML mapping or list instead of a string (a service
// account key for `service_account: !secret ...`, Zigbee2MQTT's
// network_key list).
const StructuredSuffix = "_json"

// metaLabels are the item fields the rotation register reads; they
// describe the item and are never keys themselves.
var metaLabels = map[string]bool{
	"rotation":   true,
	"rotated_at": true,
	"interval":   true,
	"expires_at": true,
	"runbook":    true,
}

// templateLabels are the labels 1Password's own item templates give their
// built-in fields. A Login's "password" in two items would otherwise
// collide on every account; a field meant as a key gets its own label.
var templateLabels = map[string]bool{
	"username":   true,
	"password":   true,
	"credential": true,
	"type":       true,
	"filename":   true,
	"hostname":   true,
	"expires":    true,
	"notesplain": true,
	"server":     true,
	"port":       true,
	"database":   true,
	"sid":        true,
	"alias":      true,
	"options":    true,
	"url":        true,
	"website":    true,
}

// keyTypes are the field types a key may come from. OTP (a seed that
// would be a moving target), DATE, MENU and the like are skipped.
var keyTypes = map[string]bool{
	"CONCEALED": true,
	"STRING":    true,
	"URL":       true,
}

// Meta is an item's rotation metadata, from its STRING fields.
type Meta struct {
	Rotation  string
	RotatedAt time.Time
	Interval  string
	// IntervalDur is Interval parsed ("7d", "365d", "12h"); zero for
	// "on-compromise" or anything unparsable.
	IntervalDur time.Duration
	ExpiresAt   time.Time
	Runbook     string
}

// Due is when the item next needs rotating: RotatedAt + IntervalDur, or
// ExpiresAt when that comes first. Zero when neither is known.
func (m Meta) Due() time.Time {
	var due time.Time
	if !m.RotatedAt.IsZero() && m.IntervalDur > 0 {
		due = m.RotatedAt.Add(m.IntervalDur)
	}
	if !m.ExpiresAt.IsZero() && (due.IsZero() || m.ExpiresAt.Before(due)) {
		due = m.ExpiresAt
	}
	return due
}

// Entry is one key and where its value came from.
type Entry struct {
	Key        string
	Value      secret.Value
	Structured bool
	VaultID    string
	VaultName  string
	ItemID     string
	ItemTitle  string
	FieldID    string
	FieldLabel string
	Section    string
	// ItemVersion and ItemUpdated identify the item revision the value
	// was read from.
	ItemVersion int
	ItemUpdated time.Time
	Meta        Meta
	// Ref is the op:// reference when the key came from the references
	// option, "" for a discovered field.
	Ref string
}

// Source renders where the value lives, for the UI: "vault > item > field".
func (e Entry) Source() string {
	parts := []string{e.VaultName, e.ItemTitle}
	if e.Section != "" {
		parts = append(parts, e.Section)
	}
	return strings.Join(append(parts, e.FieldLabel), " > ")
}

// Problem is something the owner should fix in 1Password or the options.
// Never carries a value.
type Problem struct {
	// Kind is a short machine name: "conflict", "not_json",
	// "bad_reference", "unresolved_reference", "empty_value".
	Kind   string
	Key    string
	Detail string
}

// Vault is one vault's fetched content.
type Vault struct {
	Vault opconnect.Vault
	Items []opconnect.Item
	// Discover makes this vault's fields keys. Vaults fetched only to
	// resolve references have it off.
	Discover bool
}

// Reference is one entry of the references option.
type Reference struct {
	Key string
	Ref string
}

// Catalog is the result of Build.
type Catalog struct {
	Entries  map[string]Entry
	Problems []Problem
	// Conflicted keys are left out of Entries; the renderer keeps their
	// last good value in the file instead of picking one.
	Conflicted map[string]bool
}

// Build resolves every vault's fields and every reference into keys.
func Build(vaults []Vault, refs []Reference) Catalog {
	cat := Catalog{Entries: map[string]Entry{}, Conflicted: map[string]bool{}}
	origins := map[string][]Entry{}

	for _, v := range vaults {
		if !v.Discover {
			continue
		}
		for _, item := range v.Items {
			meta := ReadMeta(item)
			for _, f := range item.Fields {
				if !isKeyField(f) {
					continue
				}
				origins[f.Label] = append(origins[f.Label], entryFor(v.Vault, item, f, meta))
			}
		}
	}

	explicit := map[string]bool{}
	for _, r := range refs {
		explicit[r.Key] = true
		e, err := Resolve(vaults, r.Ref)
		if err != nil {
			cat.Problems = append(cat.Problems, Problem{Kind: "unresolved_reference", Key: r.Key, Detail: err.Error()})
			// A broken reference must not fall back to a same-named
			// discovered field: the owner said where this key lives.
			cat.Conflicted[r.Key] = true
			continue
		}
		e.Key = r.Key
		e.Ref = r.Ref
		origins[r.Key] = []Entry{e}
	}

	keys := make([]string, 0, len(origins))
	for k := range origins {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		list := origins[key]
		if len(list) > 1 && !explicit[key] {
			sources := make([]string, len(list))
			for i, e := range list {
				sources[i] = e.Source()
			}
			sort.Strings(sources)
			cat.Problems = append(cat.Problems, Problem{
				Kind:   "conflict",
				Key:    key,
				Detail: "label used by " + strings.Join(sources, ", "),
			})
			cat.Conflicted[key] = true
			continue
		}
		if cat.Conflicted[key] {
			continue
		}
		e := list[0]
		e.Key = key
		e.Structured = strings.HasSuffix(key, StructuredSuffix)
		if e.Structured && !isJSONContainer(e.Value.Reveal()) {
			cat.Problems = append(cat.Problems, Problem{
				Kind:   "not_json",
				Key:    key,
				Detail: e.Source() + ": a label ending in _json needs a JSON object or list as its value",
			})
			cat.Conflicted[key] = true
			continue
		}
		if e.Value.IsZero() {
			cat.Problems = append(cat.Problems, Problem{Kind: "empty_value", Key: key, Detail: e.Source() + " is empty"})
		}
		cat.Entries[key] = e
	}
	return cat
}

// ReadMeta reads an item's rotation metadata fields.
func ReadMeta(item opconnect.Item) Meta {
	var m Meta
	for _, f := range item.Fields {
		label := strings.ToLower(f.Label)
		if !metaLabels[label] {
			continue
		}
		v := strings.TrimSpace(f.Value.Reveal())
		switch label {
		case "rotation":
			m.Rotation = v
		case "rotated_at":
			m.RotatedAt = parseWhen(v)
		case "interval":
			m.Interval = v
			m.IntervalDur = ParseInterval(v)
		case "expires_at":
			m.ExpiresAt = parseWhen(v)
		case "runbook":
			m.Runbook = v
		}
	}
	return m
}

// ParseInterval parses "7d", "365d", "12h", "90m"; anything else is zero.
func ParseInterval(s string) time.Duration {
	s = strings.TrimSpace(strings.ToLower(s))
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil || n <= 0 || n > 3650 {
			return 0
		}
		return time.Duration(n) * 24 * time.Hour
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0
	}
	return d
}

// parseWhen accepts an ISO date or an RFC 3339 time.
func parseWhen(s string) time.Time {
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

func isKeyField(f opconnect.Field) bool {
	if f.Purpose == "NOTES" || !keyTypes[f.Type] {
		return false
	}
	if !KeyPattern.MatchString(f.Label) {
		return false
	}
	return !metaLabels[f.Label] && !templateLabels[f.Label]
}

func entryFor(v opconnect.Vault, item opconnect.Item, f opconnect.Field, meta Meta) Entry {
	return Entry{
		Value:       f.Value,
		VaultID:     v.ID,
		VaultName:   v.Name,
		ItemID:      item.ID,
		ItemTitle:   item.Title,
		FieldID:     f.ID,
		FieldLabel:  f.Label,
		Section:     f.Section,
		ItemVersion: item.Version,
		ItemUpdated: item.UpdatedAt,
		Meta:        meta,
	}
}

func isJSONContainer(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" || (s[0] != '{' && s[0] != '[') {
		return false
	}
	return json.Valid([]byte(s))
}

// ParsedRef is an op:// reference split into its parts.
type ParsedRef struct {
	Vault   string
	Item    string
	Section string
	Field   string
}

// ParseRef splits "op://vault/item/field" or "op://vault/item/section/field".
func ParseRef(ref string) (ParsedRef, error) {
	rest, ok := strings.CutPrefix(ref, "op://")
	if !ok {
		return ParsedRef{}, fmt.Errorf("%q does not start with op://", ref)
	}
	parts := strings.Split(rest, "/")
	if slices.Contains(parts, "") {
		return ParsedRef{}, fmt.Errorf("%q has an empty segment", ref)
	}
	switch len(parts) {
	case 3:
		return ParsedRef{Vault: parts[0], Item: parts[1], Field: parts[2]}, nil
	case 4:
		return ParsedRef{Vault: parts[0], Item: parts[1], Section: parts[2], Field: parts[3]}, nil
	default:
		return ParsedRef{}, fmt.Errorf("%q needs op://vault/item/field or op://vault/item/section/field", ref)
	}
}

// Resolve finds the field ref names among the fetched vaults. Vault and
// item match by name or ID, the field by label or ID. Ambiguity is an
// error, never a guess.
func Resolve(vaults []Vault, ref string) (Entry, error) {
	p, err := ParseRef(ref)
	if err != nil {
		return Entry{}, err
	}
	var vault *Vault
	for i := range vaults {
		if vaults[i].Vault.ID == p.Vault || vaults[i].Vault.Name == p.Vault {
			if vault != nil {
				return Entry{}, fmt.Errorf("vault %q is ambiguous", p.Vault)
			}
			vault = &vaults[i]
		}
	}
	if vault == nil {
		return Entry{}, fmt.Errorf("vault %q is not readable with this token", p.Vault)
	}
	var item *opconnect.Item
	for i := range vault.Items {
		if vault.Items[i].ID == p.Item || vault.Items[i].Title == p.Item {
			if item != nil {
				return Entry{}, fmt.Errorf("item %q in %s is ambiguous", p.Item, vault.Vault.Name)
			}
			item = &vault.Items[i]
		}
	}
	if item == nil {
		return Entry{}, fmt.Errorf("item %q not found in %s", p.Item, vault.Vault.Name)
	}
	var field *opconnect.Field
	for i := range item.Fields {
		f := &item.Fields[i]
		if f.ID != p.Field && f.Label != p.Field {
			continue
		}
		if p.Section != "" && f.Section != p.Section {
			continue
		}
		if field != nil {
			return Entry{}, fmt.Errorf("field %q in %s > %s is ambiguous", p.Field, vault.Vault.Name, item.Title)
		}
		field = f
	}
	if field == nil {
		return Entry{}, fmt.Errorf("field %q not found in %s > %s", p.Field, vault.Vault.Name, item.Title)
	}
	return entryFor(vault.Vault, *item, *field, ReadMeta(*item)), nil
}
