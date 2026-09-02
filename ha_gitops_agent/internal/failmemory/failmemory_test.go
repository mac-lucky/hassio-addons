package failmemory

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRefusalBlocksOnlyTheHashThatFailed(t *testing.T) {
	failed := map[string]any{"user": map[string]any{"password": "wrong"}}
	attempts := map[string]map[string]any{
		"integration:workday_main": {"hash": Hash(failed), "error": "invalid_auth"},
	}

	message, blocked := Refusal(attempts, "integration:workday_main", failed)
	if !blocked {
		t.Fatal("blocked = false, want the recorded failure to hold")
	}
	// Both halves are load bearing: the reason, and the ways out - Retry
	// being the only one when the cause was outside the repository.
	if !strings.Contains(message, "invalid_auth") || !strings.Contains(message, "press Retry") {
		t.Errorf("message = %q", message)
	}

	// A changed declaration means the manifest was edited since the
	// failure, which is the ordinary way to retry.
	edited := map[string]any{"user": map[string]any{"password": "edited"}}
	if _, blocked := Refusal(attempts, "integration:workday_main", edited); blocked {
		t.Error("blocked = true for a declaration that has since changed")
	}
	if _, blocked := Refusal(attempts, "integration:other", failed); blocked {
		t.Error("blocked = true for a key with no record at all")
	}
}

// state.json is user-writable, so a hashless entry is reachable. It must
// not block: there is nothing to say it describes this declaration.
func TestRefusalIgnoresAnEntryWithNoHash(t *testing.T) {
	attempts := map[string]map[string]any{"hacs:solix": {"error": "boom"}}

	if _, blocked := Refusal(attempts, "hacs:solix", map[string]any{"id": "solix"}); blocked {
		t.Error("blocked = true on an entry carrying no hash")
	}
}

// Hashes persisted by versions before the key existed are bare sha256.
// They must keep matching after the upgrade, or installing the keyed
// build would read every stored hash as "data changed" at once: an error
// card per managed integration, a reconfigure flow per subentry, and a
// disarmed failure memory.
func TestMatchesRecognisesLegacyUnkeyedHashes(t *testing.T) {
	if err := LoadKey(filepath.Join(t.TempDir(), "failmemory.key")); err != nil {
		t.Fatal(err)
	}
	m := map[string]any{"user": map[string]any{"password": "hunter2"}}

	if !Matches(legacyHash(m), m) {
		t.Error("Matches = false for a pre-key stored hash of the same data")
	}
	if !Matches(Hash(m), m) {
		t.Error("Matches = false for a keyed hash of the same data")
	}
	if Matches("", m) {
		t.Error("Matches = true for an empty stored hash")
	}
	if Matches(legacyHash(map[string]any{"other": true}), m) {
		t.Error("Matches = true across different data")
	}

	attempts := map[string]map[string]any{
		"subentry:cal": {"hash": legacyHash(m), "error": "boom", "created": true},
	}
	if _, blocked := Refusal(attempts, "subentry:cal", m); !blocked {
		t.Error("a pre-key failure record no longer blocks; a created-but-unidentified subentry would be created twice")
	}
}

// A key file of the wrong size must be an error, not a silent rotation:
// rotating orphans every stored keyed hash with nothing in the log.
func TestLoadKeyRefusesCorruptKeyFileWithoutRotating(t *testing.T) {
	path := filepath.Join(t.TempDir(), "failmemory.key")
	if err := os.WriteFile(path, []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := LoadKey(path); err == nil {
		t.Fatal("LoadKey = nil error on a corrupt key file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "short" {
		t.Error("LoadKey rewrote a corrupt key file; rotation must be a decision, not a side effect")
	}
}

// The reason is rendered on the dashboard as well as in the refusal, so
// an unreadable one must read the same in both places, never as blank.
func TestReasonFallsBackForAnUnreadableError(t *testing.T) {
	for _, entry := range []map[string]any{
		{"hash": "abc"},
		{"hash": "abc", "error": 42},
		{"hash": "abc", "error": ""},
	} {
		if got := Reason(entry); got != "unknown error" {
			t.Errorf("Reason(%+v) = %q, want the shared fallback", entry, got)
		}
	}
	if got := Reason(map[string]any{"error": "invalid_auth"}); got != "invalid_auth" {
		t.Errorf("Reason = %q, want the stored text", got)
	}
}

// The hashed input is resolved data - secret values included - and the
// hashes persist in state.json, which is documented as safe to share. An
// unkeyed digest there is an offline guessing oracle for the secrets, so
// Hash must be keyed and the key must live in its own file.
func TestHashIsKeyedNotABareDigest(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "failmemory.key")
	if err := LoadKey(keyPath); err != nil {
		t.Fatal(err)
	}
	m := map[string]any{"password": "hunter2"}
	h1 := Hash(m)

	sum := sha256.Sum256([]byte(`{"password":"hunter2"}`))
	if h1 == hex.EncodeToString(sum[:]) {
		t.Error("Hash is a bare sha256 of the payload: state.json verifies guesses offline")
	}

	if err := LoadKey(keyPath); err != nil {
		t.Fatal(err)
	}
	if Hash(m) != h1 {
		t.Error("hash changed under the same key file; failure memory would reset every restart")
	}

	if err := LoadKey(filepath.Join(dir, "other.key")); err != nil {
		t.Fatal(err)
	}
	if Hash(m) == h1 {
		t.Error("hash did not change under a fresh key")
	}

	info, err := os.Stat(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("key file mode = %v, want 0600", info.Mode().Perm())
	}
}
