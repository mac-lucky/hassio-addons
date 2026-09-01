package applier

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/httperr"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/httpx"
)

// HTTPClient is internal/httpx's Doer, aliased so this package's signatures
// keep naming it and every fake satisfies exactly one interface.
type HTTPClient = httpx.Doer

// checkConfig POSTs check_config, failing closed: any non-2xx, timeout or
// connection error counts as invalid. warnings carries the response's
// "warnings" field verbatim and is meaningful only alongside valid == true,
// since an invalid config rolls back anyway.
func checkConfig(ctx context.Context, client HTTPClient, cfg Config, token string) (valid bool, errMsg, warnings string) {
	reqCtx, cancel := context.WithTimeout(ctx, cfg.CheckConfigTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, cfg.Supervisor+"/core/api/config/core/check_config", nil)
	if err != nil {
		return false, fmt.Sprintf("check_config request failed: %v", err), ""
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := client.Do(req)
	if err != nil {
		return false, fmt.Sprintf("check_config request failed: %v", err), ""
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false, fmt.Sprintf("check_config returned HTTP %d%s", resp.StatusCode, httperr.Suffix(resp)), ""
	}

	var data map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return false, fmt.Sprintf("check_config returned invalid JSON: %v", err), ""
	}

	if result, _ := data["result"].(string); result == "valid" {
		return true, "", warningsText(data["warnings"])
	}

	errsRaw, hasErrors := data["errors"]
	if !hasErrors || !isTruthy(errsRaw) {
		return false, "check_config reported invalid configuration", ""
	}
	return false, fmt.Sprintf("%v", errsRaw), ""
}

// warningsText extracts check_config's "warnings" field as a string,
// mirroring the endpoint's "string or null" contract. Any other JSON type,
// null included, counts as absent rather than being stringified.
func warningsText(v any) string {
	s, _ := v.(string)
	return s
}

// isTruthy is Python's bool() truthiness for a JSON-decoded value: an empty
// string, list or map falls through to the default, not just null.
func isTruthy(v any) bool {
	switch vv := v.(type) {
	case nil:
		return false
	case string:
		return vv != ""
	case bool:
		return vv
	case float64:
		return vv != 0
	case []any:
		return len(vv) != 0
	case map[string]any:
		return len(vv) != 0
	default:
		return true
	}
}

// callService calls the Home Assistant service homeassistant.<service>.
func callService(ctx context.Context, client HTTPClient, cfg Config, token, service string) (ok bool, errMsg string) {
	url := fmt.Sprintf("%s/core/api/services/homeassistant/%s", cfg.Supervisor, service)
	reqCtx, cancel := context.WithTimeout(ctx, cfg.ServiceCallTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, url, nil)
	if err != nil {
		return false, fmt.Sprintf("failed to call homeassistant.%s: %v", service, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := client.Do(req)
	if err != nil {
		return false, fmt.Sprintf("failed to call homeassistant.%s: %v", service, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false, fmt.Sprintf("homeassistant.%s returned HTTP %d%s", service, resp.StatusCode, httperr.Suffix(resp))
	}
	return true, ""
}

// healthProbe polls GET <cfg.Supervisor>/core/api/ until it returns 200 or
// timeout elapses, sleeping cfg.HealthProbeInterval between attempts. The
// bearer token is load-bearing: without it the endpoint returns 401, never
// 200, so the probe would always "time out" and roll back every apply.
func healthProbe(ctx context.Context, client HTTPClient, cfg Config, token string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if probeOnce(ctx, client, cfg, token) {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		// Checked explicitly: a cancelled ctx makes probeOnce fail instantly
		// and sleepCtx return immediately, which would otherwise turn the
		// rest of the timeout into a busy loop.
		if ctx.Err() != nil {
			return false
		}
		sleepCtx(ctx, cfg.HealthProbeInterval)
	}
}

func probeOnce(ctx context.Context, client HTTPClient, cfg Config, token string) bool {
	reqCtx, cancel := context.WithTimeout(ctx, cfg.HealthProbeRequestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, cfg.Supervisor+"/core/api/", nil)
	if err != nil {
		return false
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode == 200
}

// sleepCtx sleeps for d, returning early if ctx is done first, so a long
// health-probe wait still responds promptly to process shutdown.
func sleepCtx(ctx context.Context, d time.Duration) {
	if d <= 0 {
		return
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}
