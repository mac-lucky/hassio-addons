// Package statusd publishes the agent's status as a Home Assistant
// sensor, so dashboards and automations can react to it without polling
// the ingress API.
package statusd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/httpx"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/options"
)

// Timeout bounds the Supervisor call before Push treats it as failed.
const Timeout = 10 * time.Second

// States sensor.gitops_agent_status may be set to. A whitelist: Push
// drops anything absent here, so a new recon state must be added before
// recon can publish it.
var States = map[string]bool{
	"in_sync":       true,
	"drift_pending": true,
	"applying":      true,
	"error":         true,
	"disabled":      true,
	"unseeded":      true,
}

// EntityID is the entity this package publishes to.
const EntityID = "sensor.gitops_agent_status"

// HTTPClient is internal/httpx's Doer, aliased so this package's
// exported signatures keep naming it. Tests inject a fake.
type HTTPClient = httpx.Doer

// DefaultClient is the HTTPClient used when Push is called with a nil
// client.
var DefaultClient HTTPClient = http.DefaultClient

// Push sets sensor.gitops_agent_status to state with attrs, which
// conventionally carries last_sha, last_apply_utc, pending_changes,
// pending_registry_ops, error (only when state == "error") and warnings
// (a check_config warning never implies the error state).
//
// Pass a nil client to use DefaultClient. Returns false on any HTTP or
// token failure, all logged; the error is only for a state not in States.
func Push(state string, attrs map[string]any, client HTTPClient) (bool, error) {
	if !States[state] {
		return false, fmt.Errorf("statusd: invalid state %q", state)
	}

	body := map[string]any{
		"state":      state,
		"attributes": mergeAttrs(attrs),
	}

	token, err := options.SupervisorToken()
	if err != nil {
		slog.Warn("SUPERVISOR_TOKEN not set; cannot push status", "error", err)
		return false, nil
	}

	if client == nil {
		client = DefaultClient
	}
	url := options.Supervisor + "/core/api/states/" + EntityID

	payload, err := json.Marshal(body)
	if err != nil {
		// attrs comes from this process's own state, so this only fires
		// if a caller passed something json.Marshal cannot encode.
		slog.Warn("failed to encode status payload", "url", url, "error", err)
		return false, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		slog.Warn("failed to build status request", "url", url, "error", err)
		return false, nil
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := client.Do(req)
	if err != nil {
		slog.Warn("failed to push status", "url", url, "error", err)
		return false, nil
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		slog.Warn("failed to push status", "url", url, "status", resp.StatusCode)
		return false, nil
	}

	return true, nil
}

// maxAttrLen caps one string attribute. Sensor attributes reach a wider
// audience than anything else this agent writes - every HA user including
// non-admins, the recorder database, and anything exporting state - and
// error/warnings quote foreign text (a git error, a check_config block)
// with no bound of their own.
const maxAttrLen = 500

// mergeAttrs copies attrs, capping string values, and adds the fixed
// friendly_name and icon.
func mergeAttrs(attrs map[string]any) map[string]any {
	merged := make(map[string]any, len(attrs)+2)
	for k, v := range attrs {
		if s, ok := v.(string); ok && len(s) > maxAttrLen {
			v = s[:maxAttrLen] + "... (truncated)"
		}
		merged[k] = v
	}
	merged["friendly_name"] = "GitOps Agent Status"
	merged["icon"] = "mdi:source-branch"
	return merged
}
