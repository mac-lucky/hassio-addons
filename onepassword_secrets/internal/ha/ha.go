// Package ha is the add-on's client for Home Assistant Core (through
// Supervisor's /core/api proxy) and Supervisor itself: the calls that
// validate, reload and restart after a secret changes, read other
// add-ons' options for references, and publish the status sensor,
// event and notifications. Nothing sent from here carries a value.
package ha

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/mac-lucky/hassio-addons/onepassword_secrets/internal/httperr"
	"github.com/mac-lucky/hassio-addons/onepassword_secrets/internal/httpx"
)

// Timeouts per kind of call.
const (
	callTimeout        = 30 * time.Second
	checkConfigTimeout = 120 * time.Second
	probeTimeout       = 5 * time.Second
	probeInterval      = 5 * time.Second
	// maxBody bounds any answer read here.
	maxBody = 4 << 20
)

// Client calls Supervisor and Core.
type Client struct {
	// Base is Supervisor's URL, "http://supervisor".
	Base  string
	Token string
	HTTP  httpx.Doer
}

func (c *Client) do(ctx context.Context, method, path string, body any, timeout time.Duration, out any) error {
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var rd io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("%s %s: encoding: %w", method, path, err)
		}
		rd = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(reqCtx, method, c.Base+path, rd)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &StatusError{Path: path, Code: resp.StatusCode, Detail: httperr.Detail(resp)}
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBody))
		return nil
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return fmt.Errorf("%s %s: reading: %w", method, path, err)
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("%s %s: invalid JSON answer", method, path)
	}
	return nil
}

// StatusError is a non-2xx answer.
type StatusError struct {
	Path   string
	Code   int
	Detail string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("%s returned HTTP %d%s", e.Path, e.Code, httperr.SuffixOf(e.Detail))
}

// CheckConfig asks Core to validate its configuration. Fails closed: any
// error is "invalid". The reason is reduced to where the problem is
// ("Invalid config for 'mqtt' at configuration.yaml, line 12"): Core's full
// text quotes the offending value, and with !secret resolved that value can
// be a secret, which must not reach a log, a notification or the panel.
func (c *Client) CheckConfig(ctx context.Context) (valid bool, reason string) {
	var out struct {
		Result string `json:"result"`
		Errors any    `json:"errors"`
	}
	if err := c.do(ctx, http.MethodPost, "/core/api/config/core/check_config", nil, checkConfigTimeout, &out); err != nil {
		var se *StatusError
		if errors.As(err, &se) {
			return false, fmt.Sprintf("check_config returned HTTP %d", se.Code)
		}
		return false, "check_config did not answer"
	}
	if out.Result == "valid" {
		return true, ""
	}
	s, _ := out.Errors.(string)
	return false, SafeCheckReason(s)
}

// invalidAt matches the location part of Core's config errors.
var invalidAt = regexp.MustCompile(`Invalid config for '([A-Za-z0-9_.-]+)'(?: at ([^,\n]{1,200}), line (\d+))?`)

// SafeCheckReason keeps only the integrations and locations from a
// check_config error text; the rest can quote a value.
func SafeCheckReason(text string) string {
	var parts []string
	seen := map[string]bool{}
	for _, m := range invalidAt.FindAllStringSubmatch(text, 8) {
		part := "Invalid config for '" + m[1] + "'"
		if m[2] != "" {
			part += " at " + m[2] + ", line " + m[3]
		}
		if !seen[part] {
			seen[part] = true
			parts = append(parts, part)
		}
	}
	if len(parts) == 0 {
		return "Home Assistant's configuration check failed; its log has the details (not repeated here, as they can quote a secret value)"
	}
	return strings.Join(parts, "; ") + " (details in Home Assistant's log)"
}

// HasState reports whether Core holds a state for entityID. States pushed
// over the API are not restored by a Core restart, so a pushed sensor that
// is gone means Core restarted since.
func (c *Client) HasState(ctx context.Context, entityID string) (bool, error) {
	err := c.do(ctx, http.MethodGet, "/core/api/states/"+entityID, nil, callTimeout, nil)
	var se *StatusError
	if errors.As(err, &se) && se.Code == http.StatusNotFound {
		return false, nil
	}
	return err == nil, err
}

// Services returns domain -> set of service names Core offers.
func (c *Client) Services(ctx context.Context) (map[string]map[string]bool, error) {
	var out []struct {
		Domain   string                     `json:"domain"`
		Services map[string]json.RawMessage `json:"services"`
	}
	if err := c.do(ctx, http.MethodGet, "/core/api/services", nil, callTimeout, &out); err != nil {
		return nil, err
	}
	m := make(map[string]map[string]bool, len(out))
	for _, d := range out {
		set := make(map[string]bool, len(d.Services))
		for s := range d.Services {
			set[s] = true
		}
		m[d.Domain] = set
	}
	return m, nil
}

// CallService calls <domain>.<service> with data (may be nil).
func (c *Client) CallService(ctx context.Context, domain, service string, data map[string]any) error {
	if data == nil {
		data = map[string]any{}
	}
	return c.do(ctx, http.MethodPost, "/core/api/services/"+domain+"/"+service, data, callTimeout, nil)
}

// Probe reports whether Core answers its API root.
func (c *Client) Probe(ctx context.Context) bool {
	return c.do(ctx, http.MethodGet, "/core/api/", nil, probeTimeout, nil) == nil
}

// WaitHealthy polls Probe until it succeeds or timeout passes. A restart
// takes Core down first, so the caller waits for it to go away before
// calling this (see engine).
func (c *Client) WaitHealthy(ctx context.Context, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if c.Probe(ctx) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(probeInterval):
		}
	}
}

// RestartCore calls homeassistant.restart, which runs Core's own config
// check before it stops. The connection may drop as Core goes down; the
// caller judges the outcome by WaitHealthy, not by this error.
func (c *Client) RestartCore(ctx context.Context) error {
	return c.CallService(ctx, "homeassistant", "restart", nil)
}

// FireEvent fires an event on Core's bus.
func (c *Client) FireEvent(ctx context.Context, eventType string, data map[string]any) error {
	return c.do(ctx, http.MethodPost, "/core/api/events/"+eventType, data, callTimeout, nil)
}

// SetState publishes an entity state.
func (c *Client) SetState(ctx context.Context, entityID, state string, attrs map[string]any) error {
	return c.do(ctx, http.MethodPost, "/core/api/states/"+entityID, map[string]any{"state": state, "attributes": attrs}, callTimeout, nil)
}

// Notify creates (or replaces) a persistent notification.
func (c *Client) Notify(ctx context.Context, id, title, message string) error {
	return c.CallService(ctx, "persistent_notification", "create", map[string]any{
		"notification_id": id,
		"title":           title,
		"message":         message,
	})
}

// Dismiss removes a persistent notification.
func (c *Client) Dismiss(ctx context.Context, id string) error {
	return c.CallService(ctx, "persistent_notification", "dismiss", map[string]any{"notification_id": id})
}

// Addon is one installed add-on.
type Addon struct {
	Slug  string `json:"slug"`
	Name  string `json:"name"`
	State string `json:"state"`
}

type envelope[T any] struct {
	Result string `json:"result"`
	Data   T      `json:"data"`
}

// Addons lists the installed add-ons.
func (c *Client) Addons(ctx context.Context) ([]Addon, error) {
	var out envelope[struct {
		Addons []Addon `json:"addons"`
	}]
	if err := c.do(ctx, http.MethodGet, "/addons", nil, callTimeout, &out); err != nil {
		return nil, err
	}
	return out.Data.Addons, nil
}

// AddonOptions returns an add-on's stored options, "!secret" values
// unresolved. Supervisor shows them to a manager-role caller only.
func (c *Client) AddonOptions(ctx context.Context, slug string) (map[string]any, error) {
	var out envelope[struct {
		Options map[string]any `json:"options"`
	}]
	if err := c.do(ctx, http.MethodGet, "/addons/"+slug+"/info", nil, callTimeout, &out); err != nil {
		return nil, err
	}
	return out.Data.Options, nil
}

// SelfSlug returns this add-on's own slug.
func (c *Client) SelfSlug(ctx context.Context) (string, error) {
	var out envelope[struct {
		Slug string `json:"slug"`
	}]
	if err := c.do(ctx, http.MethodGet, "/addons/self/info", nil, callTimeout, &out); err != nil {
		return "", err
	}
	if out.Data.Slug == "" {
		return "", errors.New("supervisor did not report this add-on's slug")
	}
	return out.Data.Slug, nil
}

// RestartAddon restarts one add-on.
func (c *Client) RestartAddon(ctx context.Context, slug string) error {
	return c.do(ctx, http.MethodPost, "/addons/"+slug+"/restart", nil, checkConfigTimeout, nil)
}
