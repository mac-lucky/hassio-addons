package regapply

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/httperr"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/options"
)

// One read: the whole installed add-on list in a single Supervisor call,
// for the observed-version record internal/recon writes into the repository
// (track_addon_versions). Kept apart from addonupdate.go's per-slug loop
// because the caller does not know the slugs in advance and a not-installed
// add-on is not an error here. GET /addons needs hassio_role: manager,
// already declared in config.yaml.

// addonListTimeout bounds the single GET this file makes - the same budget
// as addonInfoTimeout, since Supervisor answers out of memory and only a
// busy Supervisor makes this slow.
const addonListTimeout = 15 * time.Second

// InstalledAddon is one installed add-on as the version record needs it.
// Three fields, not the dozens GET /addons returns: this shape is committed
// to the user's repository, and update_available or state would churn the
// file on movement that has nothing to do with versions.
type InstalledAddon struct {
	// Slug is the Supervisor slug (a0d7b954_esphome) the record is keyed
	// under - stable across renames, unlike Name.
	Slug string
	// Name is the display name, recorded so the file reads without every
	// slug memorized.
	Name string
	// Version is what is installed on this box. Never empty: an entry
	// without one is not installed (see FetchInstalledAddons).
	Version string
}

// FetchInstalledAddons fetches GET /addons over client
// (DefaultAddonHTTPClient if nil) and returns one entry per INSTALLED
// add-on, in Supervisor's own unsorted order - the caller sorts, where
// determinism is load-bearing (recon's renderAddonVersions).
//
// GET /addons answers {"result":"ok","data":{"addons":[...]}}; older
// Supervisors listed store entries there too, marked by a null "version".
// An entry counts as installed only with a non-empty "version" and a slug,
// which is correct for both shapes without sniffing the version.
func FetchInstalledAddons(ctx context.Context, client AddonHTTPClient) ([]InstalledAddon, error) {
	client = addonClient(client)
	token, err := options.SupervisorToken()
	if err != nil {
		return nil, err
	}

	reqCtx, cancel := context.WithTimeout(ctx, addonListTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, options.Supervisor+"/addons", nil)
	if err != nil {
		return nil, fmt.Errorf("failed to build add-on list request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("add-on list request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("add-on list returned HTTP %d%s", resp.StatusCode, httperr.Suffix(resp))
	}

	// Data is a pointer so a 200 with no data object fails loudly instead of
	// reading as an empty install - impossible, since this agent is itself an
	// installed add-on. Version is a pointer to keep null distinct from "".
	var decoded struct {
		Data *struct {
			Addons []struct {
				Slug    string  `json:"slug"`
				Name    string  `json:"name"`
				Version *string `json:"version"`
			} `json:"addons"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return nil, fmt.Errorf("add-on list returned invalid JSON: %w", err)
	}
	if decoded.Data == nil {
		return nil, fmt.Errorf("add-on list response carried no data object")
	}

	installed := make([]InstalledAddon, 0, len(decoded.Data.Addons))
	for _, a := range decoded.Data.Addons {
		if a.Slug == "" || a.Version == nil || *a.Version == "" {
			continue
		}
		installed = append(installed, InstalledAddon{Slug: a.Slug, Name: a.Name, Version: *a.Version})
	}
	return installed, nil
}
