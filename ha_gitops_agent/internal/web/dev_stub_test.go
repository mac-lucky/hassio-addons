//go:build !dev

package web

import (
	"net/http"
	"strings"
	"testing"
)

// Dev mode on and a name dev.go really defines: still the real status.
func TestPreviewsAreNotCompiledIntoAnUntaggedBuild(t *testing.T) {
	devEnv(t)
	handler := New(newFakeAgent())

	rec := doRequest(t, handler, http.MethodGet, "/?preview=drift", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "example.invalid") {
		t.Error("body does not render the agent's real status")
	}
	if strings.Contains(body, "scripts/vacuum_kitchen.yaml") {
		t.Error("a preview fixture rendered in a build that should not contain any")
	}
}
