package failmemory

import (
	"strings"
	"testing"
)

func TestRefusalBlocksOnlyTheHashThatFailed(t *testing.T) {
	attempts := map[string]map[string]any{
		"integration:workday_main": {"hash": "abc", "error": "invalid_auth"},
	}

	message, blocked := Refusal(attempts, "integration:workday_main", "abc")
	if !blocked {
		t.Fatal("blocked = false, want the recorded failure to hold")
	}
	// Both halves are load bearing: the reason, and the ways out - Retry
	// being the only one when the cause was outside the repository.
	if !strings.Contains(message, "invalid_auth") || !strings.Contains(message, "press Retry") {
		t.Errorf("message = %q", message)
	}

	// A changed hash means the manifest was edited since the failure,
	// which is the ordinary way to retry.
	if _, blocked := Refusal(attempts, "integration:workday_main", "def"); blocked {
		t.Error("blocked = true for a declaration that has since changed")
	}
	if _, blocked := Refusal(attempts, "integration:other", "abc"); blocked {
		t.Error("blocked = true for a key with no record at all")
	}
}

// state.json is user-writable, so a hashless entry is reachable. It must
// not block: there is nothing to say it describes this declaration.
func TestRefusalIgnoresAnEntryWithNoHash(t *testing.T) {
	attempts := map[string]map[string]any{"hacs:solix": {"error": "boom"}}

	if _, blocked := Refusal(attempts, "hacs:solix", "abc"); blocked {
		t.Error("blocked = true on an entry carrying no hash")
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
