package state

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "state.json")
	s, err := Load(p)
	if err != nil || len(s.Keys) != 0 {
		t.Fatalf("missing file: %+v %v", s, err)
	}
	s.Keys["a"] = KeyState{FP: "abc", ChangedAt: time.Unix(1, 0).UTC(), Source: "v > i > a"}
	s.SetManaged("secrets.yaml", map[string]bool{"b": true, "a": true})
	s.SetManaged("empty.yaml", nil)
	if err := s.Save(p); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(p)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v", info.Mode())
	}
	got, err := Load(p)
	if err != nil || got.Keys["a"].FP != "abc" || strings.Join(got.Managed["secrets.yaml"], ",") != "a,b" || !got.ManagedSet("secrets.yaml")["b"] {
		t.Fatalf("round trip: %+v %v", got, err)
	}
	if _, ok := got.Managed["empty.yaml"]; ok {
		t.Fatal("empty managed set kept")
	}
	if err := os.WriteFile(p, []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil {
		t.Fatal("want an error for a broken file")
	}
}

func TestLoadOrCreateKey(t *testing.T) {
	p := filepath.Join(t.TempDir(), "fingerprint.key")
	k1, err := LoadOrCreateKey(p)
	if err != nil || len(k1) != 32 {
		t.Fatalf("key = %d bytes, %v", len(k1), err)
	}
	k2, err := LoadOrCreateKey(p)
	if err != nil || string(k1) != string(k2) {
		t.Fatal("key not stable")
	}
	info, _ := os.Stat(p)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v", info.Mode())
	}
}

func TestActivity(t *testing.T) {
	p := filepath.Join(t.TempDir(), "activity.jsonl")
	a := OpenActivity(p)
	for i := range 400 {
		a.Add(Entry{Kind: KindChange, Title: fmt.Sprintf("entry %d %s", i, strings.Repeat("x", 2000)), Keys: []string{fmt.Sprintf("k%d", i%3)}})
	}
	recent := a.Recent(5)
	if len(recent) != 5 || !strings.HasPrefix(recent[0].Title, "entry 399") {
		t.Fatalf("recent = %v", recent[0].Title[:12])
	}
	if got := a.ForKey("k0", 3); len(got) != 3 || !strings.HasPrefix(got[0].Title, "entry 399") {
		t.Fatalf("ForKey = %d", len(got))
	}
	info, err := os.Stat(p)
	if err != nil || info.Size() > maxFileBytes+64<<10 {
		t.Fatalf("file not compacted: %v %v", info.Size(), err)
	}
	reopened := OpenActivity(p)
	if r := reopened.Recent(1); len(r) != 1 || !strings.HasPrefix(r[0].Title, "entry 399") {
		t.Fatal("reopen lost the newest entry")
	}
}
