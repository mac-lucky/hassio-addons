package web

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/recon"
)

const waitingReason = "waiting for secrets.yaml key 'forge_token' (git_token): secrets.yaml has no key 'forge_token'"

func waitingHandler() http.Handler {
	return Waiting(func() recon.Status {
		return recon.Status{
			State:            recon.StateWaiting,
			Configured:       true,
			RepoURL:          "https://user:hunter2@example.invalid/demo.git",
			Branch:           "main",
			WaitingForSecret: waitingReason,
		}
	})
}

// The page startup serves while a secret:// option cannot resolve: it says
// which key it waits for, offers no action, and polls like the dashboard so
// the real one replaces it on its own.
func TestWaitingPageNamesWhatStartupWaitsFor(t *testing.T) {
	devEnv(t)
	handler := waitingHandler()

	rec := doRequest(t, handler, http.MethodGet, "/", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"Waiting for secrets.yaml", "forge_token", "git_token", `hx-get="fragment?h=`} {
		if !strings.Contains(body, want) {
			t.Errorf("page does not contain %q", want)
		}
	}
	for _, unwanted := range []string{`hx-post="apply"`, `hx-post="reconcile"`, "hunter2"} {
		if strings.Contains(body, unwanted) {
			t.Errorf("page contains %q", unwanted)
		}
	}

	hash := fragmentHashOf(t, body)
	if rec := doRequest(t, handler, http.MethodGet, "/fragment?h="+hash, nil); rec.Code != http.StatusNoContent {
		t.Errorf("GET /fragment with the current hash = %d, want 204", rec.Code)
	}

	rec = doRequest(t, handler, http.MethodGet, "/status.json", nil)
	var status map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
		t.Fatalf("status.json: %v", err)
	}
	if status["waiting_for_secret"] != waitingReason || status["state"] != recon.StateWaiting {
		t.Errorf("status.json = %v, want the waiting state and reason", status)
	}
	if strings.Contains(rec.Body.String(), "hunter2") {
		t.Error("status.json carries the repo_url password")
	}

	for _, route := range []string{"/apply", "/reconcile", "/import"} {
		if rec := doRequest(t, handler, http.MethodPost, route, nil); rec.Code != http.StatusServiceUnavailable {
			t.Errorf("POST %s = %d, want 503 while waiting", route, rec.Code)
		}
	}
}

func TestWaitingPageKeepsTheIngressCheck(t *testing.T) {
	t.Setenv(DevEnvVar, "0")
	if rec := doRequest(t, waitingHandler(), http.MethodGet, "/", nil); rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
}

// The held-back card names each file by key path and says how to clear it.
func TestDashboardRendersTheHeldBackCard(t *testing.T) {
	body, _ := renderFragment(recon.Status{
		State:      recon.StateDriftPending,
		Configured: true,
		Branch:     "main",
		HeldBack:   []string{"esphome/node.yaml (wifi.password)", "settings.json (api.token)"},
	}, "")
	for _, want := range []string{"Held back from git", "esphome/node.yaml (wifi.password)", "settings.json (api.token)", "exclude_paths", "!secret"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("fragment does not contain %q", want)
		}
	}
	empty, _ := renderFragment(recon.Status{State: recon.StateInSync, Configured: true}, "")
	if strings.Contains(string(empty), "Held back from git") {
		t.Error("the held-back card renders with nothing held back")
	}
}
