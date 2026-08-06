// Package snapshot requests and prunes Supervisor backups around apply
// runs. Unlike applier's per-file stash, these are real partial backups,
// restorable from the Supervisor UI even if the add-on is gone.
package snapshot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/httperr"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/httpx"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/options"
)

// BackupNamePrefix is the name prefix used to identify backups this
// package created, so Prune never touches a backup made some other way.
const BackupNamePrefix = "gitops-agent pre-apply "

// RequestTimeout bounds Prune's Supervisor calls, which are cheap
// metadata operations. A var so tests can shorten it.
var RequestTimeout = 30 * time.Second

// BackupTimeout bounds PreApplyBackup's one call. Far larger than
// RequestTimeout: /backups/new/partial stays open while Supervisor tars
// the whole core config, recorder database included. A var so tests can
// shorten it.
var BackupTimeout = 15 * time.Minute

// HTTPClient is internal/httpx's Doer, aliased so this package's
// exported signatures keep naming it. Tests inject a fake.
type HTTPClient = httpx.Doer

// DefaultClient is the HTTPClient used when PreApplyBackup or Prune is
// called with a nil client.
var DefaultClient HTTPClient = http.DefaultClient

// PreApplyBackup requests a Supervisor partial backup before an apply
// and returns its slug, or ("", err). A best-effort safety net, not the
// primary rollback path: every failure is logged and returned so the
// caller can report it and carry on rather than abort the apply.
//
// Pass a nil client to use DefaultClient.
func PreApplyBackup(client HTTPClient) (string, error) {
	if client == nil {
		client = DefaultClient
	}

	token, err := options.SupervisorToken()
	if err != nil {
		slog.Warn("pre_apply_backup: SUPERVISOR_TOKEN not set", "error", err)
		return "", fmt.Errorf("SUPERVISOR_TOKEN not set: %w", err)
	}

	ts := time.Now().UTC().Format("2006-01-02T15:04:05Z")
	name := BackupNamePrefix + ts
	body, err := json.Marshal(map[string]any{
		"name":          name,
		"homeassistant": true,
		"compressed":    true,
	})
	if err != nil {
		slog.Warn("pre_apply_backup: failed to encode request body", "error", err)
		return "", fmt.Errorf("failed to encode request body: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), BackupTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, options.Supervisor+"/backups/new/partial", bytes.NewReader(body))
	if err != nil {
		slog.Warn("pre_apply_backup: failed to build request", "error", err)
		return "", fmt.Errorf("failed to build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := client.Do(req)
	if err != nil {
		slog.Warn("pre_apply_backup failed", "error", err)
		return "", fmt.Errorf("supervisor request failed after %s: %w", BackupTimeout, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		detail := httperr.Detail(resp)
		slog.Warn("pre_apply_backup: supervisor returned HTTP error", "status", resp.StatusCode, "detail", detail)
		return "", fmt.Errorf("supervisor returned HTTP %d%s", resp.StatusCode, httperr.SuffixOf(detail))
	}

	var decoded struct {
		Data struct {
			Slug string `json:"slug"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		slog.Warn("pre_apply_backup: failed to decode supervisor response", "error", err)
		return "", fmt.Errorf("failed to decode supervisor response: %w", err)
	}
	if decoded.Data.Slug == "" {
		slog.Warn("pre_apply_backup: no slug in supervisor response")
		return "", errors.New("no slug in supervisor response")
	}
	return decoded.Data.Slug, nil
}

// backupInfo is the subset of a Supervisor backup list entry Prune needs.
type backupInfo struct {
	Slug string `json:"slug"`
	Name string `json:"name"`
	Date string `json:"date"`
}

// Prune deletes backups created by PreApplyBackup, keeping the keep most
// recent. Only names carrying BackupNamePrefix are touched, and every
// failure is logged and swallowed so pruning never fails an apply. Pass
// a nil client to use DefaultClient.
func Prune(keep int, client HTTPClient) {
	if client == nil {
		client = DefaultClient
	}

	token, err := options.SupervisorToken()
	if err != nil {
		slog.Warn("prune: SUPERVISOR_TOKEN not set", "error", err)
		return
	}

	backups, ok := listBackups(client, token)
	if !ok {
		return
	}

	var ours []backupInfo
	for _, b := range backups {
		if strings.HasPrefix(b.Name, BackupNamePrefix) {
			ours = append(ours, b)
		}
	}
	sort.SliceStable(ours, func(i, j int) bool { return ours[i].Date > ours[j].Date })

	if keep >= len(ours) {
		return
	}
	for _, b := range ours[keep:] {
		if b.Slug == "" {
			continue
		}
		deleteBackup(client, token, b.Slug)
	}
}

// listBackups fetches every backup Supervisor knows about. ok is false,
// with a warning already logged, on any failure.
func listBackups(client HTTPClient, token string) (backups []backupInfo, ok bool) {
	ctx, cancel := context.WithTimeout(context.Background(), RequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, options.Supervisor+"/backups", nil)
	if err != nil {
		slog.Warn("prune: failed to build list request", "error", err)
		return nil, false
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := client.Do(req)
	if err != nil {
		slog.Warn("prune: failed to list backups", "error", err)
		return nil, false
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		slog.Warn("prune: supervisor returned HTTP error listing backups", "status", resp.StatusCode, "detail", httperr.Detail(resp))
		return nil, false
	}

	var decoded struct {
		Data struct {
			Backups []backupInfo `json:"backups"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		slog.Warn("prune: failed to decode backups list", "error", err)
		return nil, false
	}
	return decoded.Data.Backups, true
}

// deleteBackup deletes one backup by slug, logging a transport failure
// and moving on: one bad delete must not stop the rest.
func deleteBackup(client HTTPClient, token, slug string) {
	ctx, cancel := context.WithTimeout(context.Background(), RequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, options.Supervisor+"/backups/"+slug, nil)
	if err != nil {
		slog.Warn("prune: failed to build delete request", "slug", slug, "error", err)
		return
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := client.Do(req)
	if err != nil {
		slog.Warn("prune: failed to delete backup", "slug", slug, "error", err)
		return
	}
	_ = resp.Body.Close()
}
