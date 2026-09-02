// Package failmemory is the failure memory internal/flows,
// internal/subentries and internal/hacs share: regapply records {"hash",
// "error"} under an item's state key, and the next plan refuses that item
// while the hash still matches. Reason and Hash live here so the planners
// and internal/recon's blocked-items mirror cannot drift apart.
//
// Deciding when to WRITE a record stays per layer - what counts as worth
// remembering differs (see regapply/flows.go).
package failmemory

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/fsx"
)

// unknownReason is what an entry with no readable "error" reports:
// state.json is user-writable, so a non-string value gets here easily.
const unknownReason = "unknown error"

// keyMu guards key. A nil key means unkeyed legacy hashing - the mode
// before LoadKey existed, and the fallback when it fails.
var (
	keyMu sync.Mutex
	key   []byte
)

// keySize is the HMAC key length LoadKey writes and requires.
const keySize = 32

// LoadKey installs the persistent key Hash fingerprints with, creating
// path (0600) with fresh random bytes on first run. Called once at
// startup, before anything hashes.
//
// The key exists because the hashed input is RESOLVED data - secret://
// references already replaced by live secret values - and the hashes land
// in state.json, which is documented as safe to share: an unkeyed sha256
// over a structure the gitops manifest spells out would let anyone
// holding that file verify password guesses offline. Keyed, new hashes
// verify nothing without the key file, which no support paste carries.
// (A Supervisor backup of /data still holds both files together; the
// paste and the shared state file are the threats this closes.)
//
// A key file of the wrong size is an error, never silently rotated:
// rotation orphans every stored hash, and that must be a decision, not a
// side effect. On any error the caller should log and carry on - Hash
// then stays in unkeyed legacy mode, stable across restarts, rather than
// using an ephemeral key that would re-plan every hashed item on every
// restart.
func LoadKey(path string) error {
	keyMu.Lock()
	defer keyMu.Unlock()
	data, err := os.ReadFile(path) // #nosec G304 -- fixed path under /data, chosen by main
	switch {
	case err == nil && len(data) == keySize:
		key = data
		return nil
	case err == nil:
		return fmt.Errorf("failmemory: key file %s holds %d bytes, want %d; hashes stay unkeyed until it is restored or removed", path, len(data), keySize)
	case !errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("failmemory: cannot read hash key at %s: %w", path, err)
	}
	fresh := make([]byte, keySize)
	rand.Read(fresh) //nolint:errcheck // never fails as of Go 1.24; it panics on impossibility
	if writeErr := fsx.WriteFileAtomic(path, fresh, 0o600); writeErr != nil {
		return fmt.Errorf("failmemory: cannot persist hash key at %s: %w", path, writeErr)
	}
	key = fresh
	return nil
}

// hashKey returns the loaded key, or nil for unkeyed legacy mode.
func hashKey() []byte {
	keyMu.Lock()
	defer keyMu.Unlock()
	return key
}

// encode is the canonical JSON both hash forms digest. A nil map encodes
// as the empty object, and so does an unmarshalable one, since a hash
// must never crash reconciliation.
func encode(m map[string]any) []byte {
	if m == nil {
		m = map[string]any{}
	}
	encoded, err := json.Marshal(m)
	if err != nil {
		encoded = []byte("{}")
	}
	return encoded
}

// Hash fingerprints one declared thing - a flow's data mapping, a HACS
// manifest entry - for Matches to compare against. HMAC under LoadKey's
// key; unkeyed sha256 when no key is loaded (see LoadKey).
func Hash(m map[string]any) string {
	return keyedSum(encode(m))
}

func keyedSum(encoded []byte) string {
	k := hashKey()
	if k == nil {
		return legacySum(encoded)
	}
	mac := hmac.New(sha256.New, k)
	mac.Write(encoded)
	return hex.EncodeToString(mac.Sum(nil))
}

// legacyHash is the unkeyed fingerprint every version before the key
// wrote. Kept so Matches can recognise hashes persisted by those
// versions: without it, installing the keyed build would make every
// stored hash read as "data changed" at once - a permanent error card
// per managed integration, a reconfigure flow per subentry, and a
// disarmed failure memory (whose worst case re-creates a subentry that
// already exists).
func legacyHash(m map[string]any) string {
	return legacySum(encode(m))
}

func legacySum(encoded []byte) string {
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

// Matches reports whether stored fingerprints m - under the current key,
// or in the legacy unkeyed form a previous version persisted. New writes
// always store Hash, so legacy entries age out as items change; an empty
// stored hash matches nothing. One marshal serves both comparisons: on an
// upgraded install every unchanged item takes the legacy branch every
// cycle until its hash is next rewritten.
func Matches(stored string, m map[string]any) bool {
	if stored == "" {
		return false
	}
	encoded := encode(m)
	return stored == keyedSum(encoded) || stored == legacySum(encoded)
}

// Reason reads one attempt entry's stored reason, defensively. Shared by
// the refusal message and the dashboard so the two cannot disagree.
func Reason(entry map[string]any) string {
	text, _ := entry["error"].(string)
	if text == "" {
		return unknownReason
	}
	return text
}

// CreatedUnidentified reports whether attempts[key] records a create whose
// flow COMPLETED but whose result could not be identified afterwards. The
// one failure shape a later adopt-by-match HEALS rather than repeats: the
// subentry exists untracked, the data is proven to drive its flow to
// completion, and adoption is the only way it ever comes under management.
func CreatedUnidentified(attempts map[string]map[string]any, key string) bool {
	entry, ok := attempts[key]
	if !ok {
		return false
	}
	created, _ := entry["created"].(bool)
	return created
}

// Refusal reports whether attempts[key] records a failed attempt at
// exactly the data m fingerprints (either hash form - see Matches), with
// the per-item error message that says so. The two ways out are editing
// the manifest (a changed hash) and the dashboard's Retry
// (recon.RetryBlocked); nothing here mutates attempts.
func Refusal(attempts map[string]map[string]any, key string, m map[string]any) (message string, blocked bool) {
	entry, ok := attempts[key]
	if !ok {
		return "", false
	}
	storedHash, _ := entry["hash"].(string)
	if !Matches(storedHash, m) {
		return "", false
	}
	return fmt.Sprintf(
		"previous attempt failed: %s; change its manifest entry or press Retry on the dashboard",
		Reason(entry)), true
}
