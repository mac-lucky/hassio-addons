package regapply

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// minRedactableLength both ways: secrets.yaml values like "1" are
// legitimate, and blanking every 1-to-3 char string turns errors to noise.
func TestRedactValuesLeavesVeryShortValuesAlone(t *testing.T) {
	text := "add-on core_mqtt: option 'retain' rejected: expected bool, got 1 (at line 3)"

	if got := redactValues(text, []string{"true", "1", ""}); got != text {
		t.Errorf("redactValues shredded the message: %q", got)
	}
	// "true" is exactly the threshold and IS redacted; shorter ones are not.
	if got := redactValues("value true here", []string{"true"}); !strings.Contains(got, "***REDACTED***") {
		t.Errorf("redactValues = %q, want a four-character value scrubbed", got)
	}
}

func TestRedactValuesScrubsEveryOccurrence(t *testing.T) {
	got := redactValues("tried S3CRET-resolved, then S3CRET-resolved again", []string{"S3CRET-resolved"})
	if strings.Contains(got, "S3CRET-resolved") {
		t.Errorf("redactValues = %q, want every occurrence gone", got)
	}
}

// Redaction rewrites what an error says, never what it is: callers that
// branch on identity (executeFlowOp's ErrIntegrationNotLoaded) must match.
func TestRedactedErrorKeepsTheCauseMatchable(t *testing.T) {
	sentinel := errors.New("integration code is not loaded")
	err := redactedError(
		fmt.Errorf("%w: rejected S3CRET-resolved", sentinel), []string{"S3CRET-resolved"})

	if !errors.Is(err, sentinel) {
		t.Errorf("errors.Is = false, want redaction to leave the cause matchable")
	}
	if strings.Contains(err.Error(), "S3CRET-resolved") {
		t.Errorf("err = %q, want the value scrubbed out of the text", err)
	}
}

func TestRedactedErrorPassesNilAndEmptyThrough(t *testing.T) {
	if err := redactedError(nil, []string{"S3CRET-resolved"}); err != nil {
		t.Errorf("redactedError(nil) = %v, want nil", err)
	}
	original := errors.New("boom")
	if err := redactedError(original, nil); err != original {
		t.Errorf("redactedError with no values returned a new error: %v", err)
	}
}
