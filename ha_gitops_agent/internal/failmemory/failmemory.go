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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// unknownReason is what an entry with no readable "error" reports:
// state.json is user-writable, so a non-string value gets here easily.
const unknownReason = "unknown error"

// Hash fingerprints one declared thing - a flow's data mapping, a HACS
// manifest entry - for Refusal to compare against. A nil map hashes as
// the empty object, and so does an unmarshalable one, since a hash must
// never crash reconciliation.
func Hash(m map[string]any) string {
	if m == nil {
		m = map[string]any{}
	}
	encoded, err := json.Marshal(m)
	if err != nil {
		encoded = []byte("{}")
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
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
// exactly currentHash, with the per-item error message that says so. The
// two ways out are editing the manifest (a changed hash) and the
// dashboard's Retry (recon.RetryBlocked); nothing here mutates attempts.
func Refusal(attempts map[string]map[string]any, key, currentHash string) (message string, blocked bool) {
	entry, ok := attempts[key]
	if !ok {
		return "", false
	}
	storedHash, _ := entry["hash"].(string)
	if storedHash == "" || storedHash != currentHash {
		return "", false
	}
	return fmt.Sprintf(
		"previous attempt failed: %s; change its manifest entry or press Retry on the dashboard",
		Reason(entry)), true
}
