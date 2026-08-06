package regapply

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/httperr"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/options"
)

// This file is the Supervisor HTTP surface for add-on UPDATES and nothing
// else: one read (FetchAddonUpdateInfo) and one write (UpdateAddon). No
// stash or rollback machinery, because an update is not invertible -
// Supervisor has no downgrade call, and the partial backup this file asks
// for is restored by the user through Supervisor's own UI. It shares
// addonopts.go's transport seam (AddonHTTPClient/addonClient) and error
// rendering (httperr). Both endpoints are permitted by hassio_role:
// manager, already declared in config.yaml.

// addonUpdateInfoTimeout bounds the single GET this file makes. Same
// budget as addonInfoTimeout on the same endpoint: Supervisor answers an
// info read out of memory, so this only absorbs a busy Supervisor being
// slow to reach the request.
const addonUpdateInfoTimeout = 15 * time.Second

// addonUpdateTimeout bounds one POST /store/addons/<slug>/update. A var,
// not a const, so tests can shrink it - mirrors addonRestartPollTimeout.
//
// 30 minutes is deliberately generous: the non-background form blocks the
// response until the whole update finishes (partial backup, image pull,
// restart), and a several-hundred-megabyte image on a Pi with a slow SD
// card can spend a long time in the pull alone - a timeout fired mid-pull
// would report a failure for an update that then lands anyway.
var addonUpdateTimeout = 30 * time.Minute

// addonUpdateRequestBody is the update endpoint's request body, verbatim.
// {"backup": true} takes a partial backup of just this add-on first - the
// only rollback path an update has, restored by the user from
// Supervisor's own Backups page.
const addonUpdateRequestBody = `{"backup":true}`

// ErrAddonNotInstalled reports that the slug handed to
// FetchAddonUpdateInfo is not an installed add-on (unknown to Supervisor,
// or known only as a store entry). Returned wrapped with the slug so
// callers match it with errors.Is and render their own message: this is a
// manifest typo or an uninstalled add-on, not a Supervisor failure.
var ErrAddonNotInstalled = errors.New("not installed")

// AddonUpdateInfo is the update-relevant projection of one GET
// /addons/<slug>/info response.
type AddonUpdateInfo struct {
	// Slug is the Supervisor slug this info was fetched for, echoed back so
	// a caller collecting several keeps each one self-describing.
	Slug string
	// Name is the add-on's display name ("ESPHome Device Builder"), for the
	// events feed and the dashboard: a slug alone (a0d7b954_esphome) is not
	// what the user sees anywhere else.
	Name string
	// Version is the version currently installed. Never empty: an add-on
	// with no installed version comes back as ErrAddonNotInstalled instead.
	Version string
	// VersionLatest is the newest version the store offers for this
	// add-on.
	VersionLatest string
	// UpdateAvailable is Supervisor's own verdict, used rather than
	// comparing the two strings here: add-on versions are free-form
	// (calendar, semver, bare integers and date-plus-build all occur).
	UpdateAvailable bool
}

// FetchAddonUpdateInfo fetches GET /addons/<slug>/info over client
// (DefaultAddonHTTPClient if nil) and projects it onto AddonUpdateInfo.
//
// A slug that is not an installed add-on comes back satisfying
// errors.Is(err, ErrAddonNotInstalled), never as a generic HTTP failure.
// Three shapes mean that, all handled because Supervisor's answer has
// changed across releases: 404 (the current shape); 400 whose message says
// the add-on does not exist or is not installed (older Supervisors, and
// the store routes for a never-installed slug) - a 400 saying anything
// else is a real error; and 200 whose data object carries a null or
// missing "version", or an explicit "installed": false.
func FetchAddonUpdateInfo(ctx context.Context, client AddonHTTPClient, slug string) (AddonUpdateInfo, error) {
	client = addonClient(client)
	token, err := options.SupervisorToken()
	if err != nil {
		return AddonUpdateInfo{}, err
	}

	reqCtx, cancel := context.WithTimeout(ctx, addonUpdateInfoTimeout)
	defer cancel()
	// The slug is interpolated unescaped, exactly as fetchAddonInfoRaw
	// does: addonopts validates it against ^[a-z0-9_-]+$ before it can
	// reach here, so URL escaping would change nothing in it.
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, options.Supervisor+"/addons/"+slug+"/info", nil)
	if err != nil {
		return AddonUpdateInfo{}, fmt.Errorf("add-on %s: failed to build info request: %w", slug, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := client.Do(req)
	if err != nil {
		return AddonUpdateInfo{}, fmt.Errorf("add-on %s: info request failed: %w", slug, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Read the body once and reuse the rendered detail for both the
		// not-installed test and the message - httperr.Detail consumes the
		// body, so a second read comes back empty.
		detail := httperr.Detail(resp)
		if resp.StatusCode == http.StatusNotFound ||
			(resp.StatusCode == http.StatusBadRequest && detailSaysNotInstalled(detail)) {
			return AddonUpdateInfo{}, fmt.Errorf("add-on %s: %w", slug, ErrAddonNotInstalled)
		}
		return AddonUpdateInfo{}, fmt.Errorf(
			"add-on %s: info returned HTTP %d%s", slug, resp.StatusCode, httperr.SuffixOf(detail))
	}

	// Data is a pointer so a 200 carrying no data object fails loudly below
	// rather than passing through as a zero value that would be misreported
	// as "not installed". Version is a pointer so an explicit null stays
	// distinct from an empty string, and Installed so an absent key (an
	// installed add-on never carries it) stays distinct from a false one.
	var decoded struct {
		Data *struct {
			Name            string  `json:"name"`
			Version         *string `json:"version"`
			VersionLatest   string  `json:"version_latest"`
			UpdateAvailable bool    `json:"update_available"`
			Installed       *bool   `json:"installed"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return AddonUpdateInfo{}, fmt.Errorf("add-on %s: info returned invalid JSON: %w", slug, err)
	}
	if decoded.Data == nil {
		return AddonUpdateInfo{}, fmt.Errorf("add-on %s: info response carried no data object", slug)
	}

	data := decoded.Data
	if (data.Installed != nil && !*data.Installed) || data.Version == nil || *data.Version == "" {
		return AddonUpdateInfo{}, fmt.Errorf("add-on %s: %w", slug, ErrAddonNotInstalled)
	}
	return AddonUpdateInfo{
		Slug:            slug,
		Name:            data.Name,
		Version:         *data.Version,
		VersionLatest:   data.VersionLatest,
		UpdateAvailable: data.UpdateAvailable,
	}, nil
}

// notInstalledMarkers are the lowercased fragments Supervisor's rejection
// message uses when the slug is not an installed add-on. Matching on the
// message is unavoidable: the status code alone cannot separate this from
// a malformed request, and Supervisor's error envelope carries no
// machine-readable code, only "result" and "message" (see internal/httperr).
var notInstalledMarkers = []string{
	// Observed: "Addon <slug> does not exist" is what Supervisor raises;
	// "not installed" covers the install-guarded routes' phrasing.
	"does not exist",
	"not installed",
	// Defensive, NOT observed in any Supervisor release checked - here so a
	// fork or a future rewording degrades to the right classification. Do
	// not cite these as verified Supervisor wording.
	"unknown addon",
	"unknown add-on",
}

// detailSaysNotInstalled reports whether an already-rendered error detail
// (httperr.Detail) is Supervisor saying the slug is not installed.
func detailSaysNotInstalled(detail string) bool {
	lowered := strings.ToLower(detail)
	for _, marker := range notInstalledMarkers {
		if strings.Contains(lowered, marker) {
			return true
		}
	}
	return false
}

// UpdateAddon updates one add-on to the newest version its store offers,
// over client (DefaultAddonHTTPClient if nil), returning once Supervisor
// reports the whole update finished.
//
// # Verified Supervisor endpoint shape (home-assistant/supervisor source)
//
// POST /store/addons/<slug>/update is the current route (the older
// /addons/<slug>/update is deprecated). Body {"backup": true} takes a
// PARTIAL backup of just this add-on first - the only way back if the new
// version misbehaves, restored by the user from Supervisor's Backups page.
// Sending no background flag makes the request BLOCK until backup, image
// pull and restart are all done (see addonUpdateTimeout). Success is
// {"result":"ok","data":{}} with nothing worth reading, so only the status
// is checked; a failure carries Supervisor's usual "message", which
// httperr.Suffix appends.
func UpdateAddon(ctx context.Context, client AddonHTTPClient, slug string) error {
	client = addonClient(client)
	token, err := options.SupervisorToken()
	if err != nil {
		return err
	}

	reqCtx, cancel := context.WithTimeout(ctx, addonUpdateTimeout)
	defer cancel()
	// Unescaped slug, for the same reason as in FetchAddonUpdateInfo.
	req, err := http.NewRequestWithContext(
		reqCtx, http.MethodPost, options.Supervisor+"/store/addons/"+slug+"/update",
		strings.NewReader(addonUpdateRequestBody))
	if err != nil {
		return fmt.Errorf("add-on %s: failed to build update request: %w", slug, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("add-on %s: update request failed: %w", slug, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("add-on %s: update returned HTTP %d%s", slug, resp.StatusCode, httperr.Suffix(resp))
	}
	return nil
}
