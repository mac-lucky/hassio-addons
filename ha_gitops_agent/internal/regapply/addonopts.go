// Package regapply's addon-options file is internal/addonopts' execution
// counterpart, driven over the Supervisor's plain REST API rather than
// this package's Dialer/WSClient machinery: every call is a fresh,
// independent HTTP request, so there is no connection to dial, keep alive
// or redial after a timeout.
//
// For the same reason it keeps its own <stashDir>/addon_stash.json rather
// than adding "addon" entries to registry_stash.json, whose reverse-replay
// loop is written entirely in terms of a redialed WSClient. The discipline
// is IDENTICAL - reset at the start of an apply, whole file rewritten
// after each confirmed op, and on rollback an entry dropped BEFORE its
// inverse is attempted - just over its own file and a dial-free replay
// loop. recon.Reconciler.Rollback covers both stashes, calling
// RollbackRegistry and RollbackAddonPlan against the same stashDir.
package regapply

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/addonopts"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/difftext"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/httperr"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/httpx"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/options"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/registries"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/secretref"
)

// Timeouts for the four Supervisor add-on endpoints this file calls.
// addonRestartTimeout is generous because Supervisor's own restart handler
// blocks the response until the container settles (or its internal ~120s
// timeout elapses, logged but not raised) - see ApplyAddonPlan.
const (
	addonInfoTimeout    = 15 * time.Second
	addonOptionsTimeout = 15 * time.Second
	addonRestartTimeout = 150 * time.Second
)

// addonRestartPollTimeout/addonRestartPollInterval bound pollAddonStarted's
// wait loop. Vars, not consts, so tests can shrink them and exercise
// several iterations without a real sleep - mirrors writeRegistryStash.
var (
	addonRestartPollTimeout  = 60 * time.Second
	addonRestartPollInterval = 3 * time.Second
)

// AddonHTTPClient is internal/httpx's Doer, aliased so this file's
// exported signatures keep naming it. Tests inject a fake.
type AddonHTTPClient = httpx.Doer

// DefaultAddonHTTPClient is the AddonHTTPClient used when any function in
// this file is called with a nil client.
var DefaultAddonHTTPClient AddonHTTPClient = http.DefaultClient

func addonClient(client AddonHTTPClient) AddonHTTPClient {
	if client == nil {
		return DefaultAddonHTTPClient
	}
	return client
}

// FetchAddonInfoAll fetches GET /addons/<slug>/info for every slug,
// shaped as addonopts.Plan's live parameter expects: options/state for an
// installed add-on, or {"installed": false} for one Supervisor does not
// know (404) or never installed (see fetchAddonInfoRaw). Every requested
// slug always gets an entry, so there are no holes to interpret.
func FetchAddonInfoAll(ctx context.Context, client AddonHTTPClient, slugs []string) (map[string]map[string]any, error) {
	client = addonClient(client)
	token, err := options.SupervisorToken()
	if err != nil {
		return nil, err
	}

	out := make(map[string]map[string]any, len(slugs))
	for _, slug := range slugs {
		info, err := fetchAddonInfoRaw(ctx, client, token, slug)
		if err != nil {
			return nil, err
		}
		out[slug] = info
	}
	return out, nil
}

// FetchSelfAddonSlug resolves this add-on's own Supervisor slug via GET
// /addons/self/info - "self" is the literal alias Supervisor's
// token-validation middleware resolves to whichever add-on's
// SUPERVISOR_TOKEN sent the request. internal/recon caches it once and
// passes it into addonopts.Plan as the self-protection guard.
func FetchSelfAddonSlug(ctx context.Context, client AddonHTTPClient) (string, error) {
	client = addonClient(client)
	token, err := options.SupervisorToken()
	if err != nil {
		return "", err
	}

	reqCtx, cancel := context.WithTimeout(ctx, addonInfoTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, options.Supervisor+"/addons/self/info", nil)
	if err != nil {
		return "", fmt.Errorf("failed to build self-info request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("self-info request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("self-info returned HTTP %d%s", resp.StatusCode, httperr.Suffix(resp))
	}

	var decoded struct {
		Data map[string]any `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return "", fmt.Errorf("self-info returned invalid JSON: %w", err)
	}
	slug, _ := decoded.Data["slug"].(string)
	if slug == "" {
		return "", fmt.Errorf("self-info response carried no slug")
	}
	return slug, nil
}

// fetchAddonInfoRaw fetches GET /addons/<slug>/info and normalizes its
// three shapes (verified, see ApplyAddonPlan) into what addonopts.Plan
// expects: an unknown slug (404) or a known-but-never-installed one (200
// with installed explicitly false) both become {"installed": false}; an
// installed add-on's data object, which carries no "installed" key at
// all, passes through as-is.
func fetchAddonInfoRaw(ctx context.Context, client AddonHTTPClient, token, slug string) (map[string]any, error) {
	reqCtx, cancel := context.WithTimeout(ctx, addonInfoTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, options.Supervisor+"/addons/"+slug+"/info", nil)
	if err != nil {
		return nil, fmt.Errorf("add-on %s: failed to build info request: %w", slug, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("add-on %s: info request failed: %w", slug, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return map[string]any{"installed": false}, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("add-on %s: info returned HTTP %d%s", slug, resp.StatusCode, httperr.Suffix(resp))
	}

	var decoded struct {
		Data map[string]any `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return nil, fmt.Errorf("add-on %s: info returned invalid JSON: %w", slug, err)
	}
	if decoded.Data == nil {
		decoded.Data = map[string]any{}
	}
	return decoded.Data, nil
}

// fetchAddonStoreDefaultsRaw fetches GET /store/addons/<slug> and, IF the
// response happens to carry an "options" field, returns it as (defaults,
// true, nil) - the add-on's schema-default options.
//
// Best-effort by necessity: verified against home-assistant/supervisor
// source, no route (store or otherwise) and no bind-mountable path exposes
// an ALREADY-INSTALLED add-on's config.yaml defaults, so on today's
// Supervisor this always comes back hasDefaults false. executeAddonOp then
// falls back to its pre-fix behavior for that write, which pins every
// currently-effective option rather than only the declared ones -
// documented in DOCS.md's "Ownership (add-ons)". The fix stays strictly
// additive: it can only improve on a Supervisor that does populate the
// field, and never makes today's verified behavior worse.
func fetchAddonStoreDefaultsRaw(ctx context.Context, client AddonHTTPClient, token, slug string) (map[string]any, bool, error) {
	reqCtx, cancel := context.WithTimeout(ctx, addonInfoTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, options.Supervisor+"/store/addons/"+slug, nil)
	if err != nil {
		return nil, false, fmt.Errorf("add-on %s: failed to build store-info request: %w", slug, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := client.Do(req)
	if err != nil {
		return nil, false, fmt.Errorf("add-on %s: store-info request failed: %w", slug, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return nil, false, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, false, fmt.Errorf("add-on %s: store-info returned HTTP %d%s", slug, resp.StatusCode, httperr.Suffix(resp))
	}

	var decoded struct {
		Data map[string]any `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return nil, false, fmt.Errorf("add-on %s: store-info returned invalid JSON: %w", slug, err)
	}
	defaults, ok := decoded.Data["options"].(map[string]any)
	if !ok {
		return nil, false, nil
	}
	return defaults, true, nil
}

// persistedOnlyOptions reconstructs the add-on's TRUE persisted overrides
// out of current (the schema-defaults-merged view GET .../info returns) by
// dropping every key deep-equal to its schema default: such a key is
// indistinguishable from an uncustomized default showing through, and
// re-POSTing it would pin that value as a permanent override, surviving a
// future add-on release that changes its own default. hasDefaults false
// (see fetchAddonStoreDefaultsRaw) falls back to current wholesale.
func persistedOnlyOptions(current, defaults map[string]any, hasDefaults bool) map[string]any {
	if !hasDefaults {
		out := make(map[string]any, len(current))
		for k, v := range current {
			out[k] = v
		}
		return out
	}
	out := make(map[string]any, len(current))
	for k, v := range current {
		if defaultVal, hasDefault := defaults[k]; hasDefault && registries.ValuesEqual(v, defaultVal) {
			continue
		}
		out[k] = v
	}
	return out
}

// optionsDiffer reports whether a and b differ in key set or any shared
// key's value - the test for whether a reconstructed options object is
// worth POSTing at all.
func optionsDiffer(a, b map[string]any) bool {
	if len(a) != len(b) {
		return true
	}
	for k, v := range a {
		bv, ok := b[k]
		if !ok || !registries.ValuesEqual(v, bv) {
			return true
		}
	}
	return false
}

// overlayOption applies one op-param key onto the merged options object
// about to be POSTed. An addonopts.AbsentMarker value ("this key had no
// value before this agent touched it") is applied by DROPPING the key:
// omission is the only way to tell Supervisor a key is unset, since an
// explicit null is rejected with HTTP 400 "Missing required option
// '<key>'" even for a key the add-on's schema marks optional.
func overlayOption(merged map[string]any, key string, value any) {
	if addonopts.IsAbsent(value) {
		delete(merged, key)
		return
	}
	merged[key] = value
}

// optionChanges reports whether writing value at key would change the
// add-on's current effective options - the trigger for a restart.
// Presence-aware, like optionsDiff on the plan side: an absent marker's
// change is the key's removal, which a value comparison cannot see.
func optionChanges(current map[string]any, key string, value any) bool {
	if addonopts.IsAbsent(value) {
		_, present := current[key]
		return present
	}
	return !registries.ValuesEqual(current[key], value)
}

// priorOptionValue snapshots one key's pre-op value, recording
// addonopts.AbsentMarker rather than a bare nil for a key that is not
// there at all: both the addon stash and state.AddonOriginals round-trip
// through JSON, where a stored null and a missing key are
// indistinguishable, leaving a restore no way to put the key back.
func priorOptionValue(current map[string]any, key string) any {
	if v, present := current[key]; present {
		return v
	}
	return addonopts.AbsentMarker()
}

// postAddonOptions POSTs the full merged options object - see
// executeAddonOp for why this is never a bare partial update.
func postAddonOptions(ctx context.Context, client AddonHTTPClient, token, slug string, merged map[string]any) error {
	body, err := json.Marshal(map[string]any{"options": merged})
	if err != nil {
		return fmt.Errorf("add-on %s: failed to encode options: %w", slug, err)
	}

	reqCtx, cancel := context.WithTimeout(ctx, addonOptionsTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(
		reqCtx, http.MethodPost, options.Supervisor+"/addons/"+slug+"/options", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("add-on %s: failed to build options request: %w", slug, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("add-on %s: options request failed: %w", slug, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("add-on %s: options returned HTTP %d%s", slug, resp.StatusCode, httperr.Suffix(resp))
	}
	return nil
}

func postAddonRestart(ctx context.Context, client AddonHTTPClient, token, slug string) error {
	reqCtx, cancel := context.WithTimeout(ctx, addonRestartTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, options.Supervisor+"/addons/"+slug+"/restart", nil)
	if err != nil {
		return fmt.Errorf("add-on %s: failed to build restart request: %w", slug, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("add-on %s: restart request failed: %w", slug, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("add-on %s: restart returned HTTP %d%s", slug, resp.StatusCode, httperr.Suffix(resp))
	}
	return nil
}

// pollAddonStarted polls GET .../info until state == "started", bounded by
// addonRestartPollTimeout. Supervisor's restart call already blocks until
// the add-on settles, so this only covers the case where that internal
// wait times out without raising and restart returns 200 anyway.
func pollAddonStarted(ctx context.Context, client AddonHTTPClient, token, slug string) error {
	deadline := time.Now().Add(addonRestartPollTimeout)
	for {
		info, err := fetchAddonInfoRaw(ctx, client, token, slug)
		if err == nil {
			if state, _ := info["state"].(string); state == "started" {
				return nil
			}
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("add-on %s did not report state \"started\" within %s after restart", slug, addonRestartPollTimeout)
		}
		// A cancelled ctx makes the fetch fail instantly and the sleep
		// return immediately - without this check, the rest of the deadline
		// would be a tight loop of doomed requests.
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("add-on %s: restart poll cancelled: %w", slug, err)
		}
		sleepAddonCtx(ctx, addonRestartPollInterval)
	}
}

func sleepAddonCtx(ctx context.Context, d time.Duration) {
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

func restartAndPoll(ctx context.Context, client AddonHTTPClient, token, slug string) error {
	if err := postAddonRestart(ctx, client, token, slug); err != nil {
		return err
	}
	return pollAddonStarted(ctx, client, token, slug)
}

// addonStashEntry is the in-memory (and, via toAddonStashOnDisk, on-disk)
// record of one executed addon op, enough to invert it later. Kind is
// addonopts.KindUpdate or addonopts.KindRestore; both invert through
// identical options-restore mechanics (see invertAddonOp), differing only
// in the Originals*/RestartMap* bookkeeping - mirrors stashEntry's own
// convention for "entity" entries.
type addonStashEntry struct {
	Kind string
	Slug string
	// PriorOptions holds the touched keys' pre-op live values, with
	// addonopts.AbsentMarker for a key that had no value at all (see
	// priorOptionValue) - never a bare nil, which off disk is
	// indistinguishable from a real null.
	//
	// One exception: a key the manifest declared as "secret://<name>" whose
	// prior value this agent itself wrote holds the reference instead, so
	// the credential stays off disk. See stashPriorOptions, and
	// invertAddonOp for what that makes a rollback do with it.
	PriorOptions map[string]any
	// ForwardOptions holds the touched keys' values this op wrote, with the
	// same substitution. The reference left behind is what tells a rollback
	// which keys hold a credential live (see liveSecretValues); what a
	// rollback restores FROM is PriorOptions.
	ForwardOptions map[string]any
	// RestartOnChange is the restart_on_change value THIS op used, not
	// necessarily what is currently declared, so invert restarts under the
	// same policy the forward op did.
	RestartOnChange bool

	// OriginalsExistedBefore/OriginalsSnapshotBefore snapshot
	// state.AddonOriginals' entry for this slug from immediately BEFORE
	// this op mutated it - mirrors stashEntry's own fields.
	OriginalsExistedBefore  bool
	OriginalsSnapshotBefore map[string]any
	// RestartMapExistedBefore/PriorRestartOnChangeBefore is the same
	// snapshot-before-mutation discipline applied to
	// state.AddonRestartOnChange.
	RestartMapExistedBefore    bool
	PriorRestartOnChangeBefore bool
}

// executeAddonOp executes a single addon update/restore op via
// READ-RECONSTRUCT-MERGE-WRITE and returns a record of it, for both the
// addon stash file and a later invertAddonOp call.
//
// Both kinds are the same wire sequence, because Supervisor's options
// endpoint has no server-side merge - a partial POST silently drops every
// key not included (verified, see ApplyAddonPlan). The current effective
// options are read fresh and the whole persisted-only object is sent back
// with the declared keys overlaid through overlayOption, which DROPS a key
// whose value is addonopts.AbsentMarker rather than writing it.
//
// "Persisted-only" (see persistedOnlyOptions) is what stops a naive resend
// of GET .../info's defaults-merged view from PINNING every default-valued
// key as a permanent override; the write is skipped entirely
// (optionsDiffer) when it would change nothing.
//
// declaredRestartOnChange is addonopts.DeclaredRestartOnChange(desired),
// the manifest's CURRENT value per slug, consulted for a KindUpdate.
// restartOnChangeState is state.AddonRestartOnChange, mutated in place
// like originals - a KindRestore consults its OWN prior value instead,
// since the manifest entry is already gone by the time an un-manage
// restore runs.
func executeAddonOp(
	ctx context.Context, client AddonHTTPClient, token string, op registries.RegOp,
	declaredRestartOnChange map[string]bool, originals map[string]map[string]any, restartOnChangeState map[string]bool,
) (entry addonStashEntry, err error) {
	slug := op.Key

	// The POSTs below send the add-on's WHOLE merged options object, and a
	// Supervisor rejection can quote any of it back (internal/httperr keeps
	// 400 characters of the body). The caller scrubs the declared
	// op.Secrets; the live values under credential-shaped keys - a restore
	// op declares no secrets at all - are only known here, so every failure
	// leaves through this scrub.
	var current, merged map[string]any
	defer func() { err = redactedStepError(err, current, merged) }()

	info, err := fetchAddonInfoRaw(ctx, client, token, slug)
	if err != nil {
		return addonStashEntry{}, err
	}
	if installedRaw, ok := info["installed"]; ok {
		if installed, _ := installedRaw.(bool); !installed {
			return addonStashEntry{}, fmt.Errorf("add-on not installed: %s", slug)
		}
	}
	current, _ = info["options"].(map[string]any)

	defaults, hasDefaults, defaultsErr := fetchAddonStoreDefaultsRaw(ctx, client, token, slug)
	if defaultsErr != nil {
		// Best-effort only (see fetchAddonStoreDefaultsRaw): a failed
		// defaults fetch must never fail the option write, it just falls
		// back to the full effective view as its base.
		slog.Debug("regapply: execute_addon_op could not fetch store defaults, falling back",
			"slug", slug, "error", defaultsErr)
		hasDefaults = false
	}
	base := persistedOnlyOptions(current, defaults, hasDefaults)

	merged = make(map[string]any, len(base)+len(op.Params))
	for k, v := range base {
		merged[k] = v
	}
	priorTouched := make(map[string]any, len(op.Params))
	changed := false
	for k, v := range op.Params {
		priorTouched[k] = priorOptionValue(current, k)
		if optionChanges(current, k, v) {
			changed = true
		}
		overlayOption(merged, k, v)
	}

	// Skip a write that would be a no-op: no declared key differs from its
	// live value, or merged already matches what Supervisor has persisted.
	if optionsDiffer(merged, current) {
		if err := postAddonOptions(ctx, client, token, slug, merged); err != nil {
			return addonStashEntry{}, err
		}
	}

	key := "addon:" + slug
	var restartOnChange bool
	if op.Kind == addonopts.KindRestore {
		restartOnChange = restartOnChangeState[key]
	} else {
		restartOnChange = declaredRestartOnChange[slug]
	}

	if changed && restartOnChange {
		if err := restartAndPoll(ctx, client, token, slug); err != nil {
			// The options POST above already succeeded, so the add-on holds
			// the NEW values, and this op never reaches the caller's
			// "executed" list for addonInverseReplayAndPersist to undo.
			// Recover locally: put the touched keys back to their pre-op
			// values, then best-effort restart to apply that.
			revert := make(map[string]any, len(merged))
			for k, v := range merged {
				revert[k] = v
			}
			for k := range op.Params {
				overlayOption(revert, k, priorTouched[k])
			}
			if revertErr := postAddonOptions(ctx, client, token, slug, revert); revertErr != nil {
				// Double fault: the forward write landed and the local
				// revert could not undo it, so the live options are stuck at
				// op.Params' values. The returned entry is non-zero (Slug
				// set) - the caller's signal to treat this as a real,
				// executed op needing its bookkeeping recorded and its stash
				// entry kept. See addonEntryAfterForwardWrite.
				entry := addonEntryAfterForwardWrite(
					op, slug, key, priorTouched, restartOnChange, declaredRestartOnChange, originals, restartOnChangeState)
				return entry, fmt.Errorf(
					"add-on %s: %w; additionally, could not restore its prior options: %v", slug, err, revertErr)
			}
			if restartErr := postAddonRestart(ctx, client, token, slug); restartErr != nil {
				slog.Warn("regapply: execute_addon_op could not restart after reverting options", "slug", slug, "error", restartErr)
			}
			return addonStashEntry{}, fmt.Errorf("add-on %s: %w (prior options restored)", slug, err)
		}
	}

	return addonEntryAfterForwardWrite(
		op, slug, key, priorTouched, restartOnChange, declaredRestartOnChange, originals, restartOnChangeState), nil
}

// addonEntryAfterForwardWrite records the bookkeeping for, and builds the
// addonStashEntry describing, a forward options write that genuinely
// landed live - whether the follow-up restart (and, on its failure, the
// local revert) then succeeded or double-faulted. The add-on's true
// pre-management baseline is fixed the instant the write lands, not when a
// later restart confirms healthy, so both callers mutate
// originals[key]/restartOnChangeState[key] the same way.
func addonEntryAfterForwardWrite(
	op registries.RegOp, slug, key string, priorTouched map[string]any, restartOnChange bool,
	declaredRestartOnChange map[string]bool, originals map[string]map[string]any, restartOnChangeState map[string]bool,
) addonStashEntry {
	existingOriginals, hasOriginals := originals[key]
	var priorOriginalsSnapshot map[string]any
	if hasOriginals {
		priorOriginalsSnapshot = make(map[string]any, len(existingOriginals))
		for k, v := range existingOriginals {
			priorOriginalsSnapshot[k] = v
		}
	}
	priorRestartOnChange, hadRestartEntry := restartOnChangeState[key]

	switch op.Kind {
	case addonopts.KindRestore:
		delete(originals, key)
		delete(restartOnChangeState, key)
	default: // addonopts.KindUpdate
		updated := make(map[string]any, len(existingOriginals)+len(op.Params))
		for k, v := range existingOriginals {
			updated[k] = v
		}
		for field := range op.Params {
			if _, already := updated[field]; !already {
				// priorTouched[field] is every op.Params key's live value
				// from before the forward write, or the absent marker, so
				// current does not have to be threaded in here too.
				updated[field] = priorTouched[field]
			}
		}
		originals[key] = updated
		restartOnChangeState[key] = declaredRestartOnChange[slug]
	}

	return addonStashEntry{
		Kind: op.Kind, Slug: slug,
		// The stash is JSON under /data/backup, kept for five applies and
		// carried inside any Supervisor backup of this add-on, so what goes
		// into it is sanitized here.
		PriorOptions:            stashPriorOptions(op, priorTouched, existingOriginals),
		ForwardOptions:          stashForwardOptions(op),
		RestartOnChange:         restartOnChange,
		OriginalsExistedBefore:  hasOriginals,
		OriginalsSnapshotBefore: priorOriginalsSnapshot,
		RestartMapExistedBefore: hadRestartEntry, PriorRestartOnChangeBefore: priorRestartOnChange,
	}
}

// stashForwardOptions is op.Params with every key the manifest declared as
// a "secret://<name>" reference carrying that reference instead of what it
// resolved to. Load-bearing twice over: the file stops holding the
// credential, and the marker left behind is how liveSecretValues later
// tells which keys are credential-bearing.
func stashForwardOptions(op registries.RegOp) map[string]any {
	out := make(map[string]any, len(op.Params))
	for k, v := range op.Params {
		if declared, isRef := declaredRefFor(op, k); isRef {
			out[k] = declared
			continue
		}
		out[k] = v
	}
	return out
}

// stashPriorOptions is priorTouched with the same substitution applied to
// exactly the keys whose prior LIVE value is one this agent put there,
// decided on existingOriginals (state.AddonOriginals before this op): a key
// not in it holds the user's own value, recorded into state.AddonOriginals
// in the clear either way and needed for a faithful rollback; a key already
// in it last held the credential the reference resolved to, which exists
// nowhere else in persisted state, so the reference goes in instead (see
// invertAddonOp for what a rollback then does with the key).
func stashPriorOptions(op registries.RegOp, priorTouched, existingOriginals map[string]any) map[string]any {
	out := make(map[string]any, len(priorTouched))
	for k, v := range priorTouched {
		declared, isRef := declaredRefFor(op, k)
		if _, alreadyManaged := existingOriginals[k]; isRef && alreadyManaged {
			out[k] = declared
			continue
		}
		out[k] = v
	}
	return out
}

// declaredRefFor reports whether the manifest declared this option key as
// a secret reference and hands back that declared value as written - the
// op-shaped form of secretref.RefAt. A restore op carries no Declared map
// at all (the manifest no longer names the slug), the gap the package doc
// comment documents.
func declaredRefFor(op registries.RegOp, key string) (any, bool) {
	return secretref.RefAt(op.Declared, key)
}

// invertAddonOp inverts one executed addon stash entry: restores
// entry.ForwardOptions' keys to entry.PriorOptions' values by the same
// read-merge-write discipline, EXCEPT a key whose recorded prior value is
// a "secret://<name>" reference, which is left exactly as it is and is the
// one thing this rollback does not undo (see stashPriorOptions, and the
// loop below for why re-resolving would not be a rollback). Restarts
// best-effort if that changed anything and entry.RestartOnChange was in
// effect (a restart failure is logged, not returned - the options, the
// state-critical part, are already restored), then restores the
// originals/restartOnChangeState bookkeeping to what it held before this
// op ran. Works unchanged for KindUpdate and KindRestore entries alike.
func invertAddonOp(
	ctx context.Context, client AddonHTTPClient, token string, entry addonStashEntry,
	originals map[string]map[string]any, restartOnChangeState map[string]bool,
) (err error) {
	// Populated as soon as the live options are in hand, and applied to
	// every failure this function reports: the POST below sends the whole
	// merged options object, so Supervisor's rejection can quote a
	// credential back (internal/httperr keeps 400 characters of the body)
	// into the activity feed, /data/history.jsonl and the log. Nothing
	// before the fetch can carry one.
	var secretValues []string
	defer func() { err = redactedError(err, secretValues) }()

	info, err := fetchAddonInfoRaw(ctx, client, token, entry.Slug)
	if err != nil {
		return err
	}
	if installedRaw, ok := info["installed"]; ok {
		if installed, _ := installedRaw.(bool); !installed {
			return fmt.Errorf("add-on not installed: %s", entry.Slug)
		}
	}
	current, _ := info["options"].(map[string]any)
	secretValues = liveSecretValues(entry, current)

	defaults, hasDefaults, defaultsErr := fetchAddonStoreDefaultsRaw(ctx, client, token, entry.Slug)
	if defaultsErr != nil {
		slog.Debug("regapply: invert_addon_op could not fetch store defaults, falling back",
			"slug", entry.Slug, "error", defaultsErr)
		hasDefaults = false
	}
	base := persistedOnlyOptions(current, defaults, hasDefaults)

	merged := make(map[string]any, len(base)+len(entry.ForwardOptions))
	for k, v := range base {
		merged[k] = v
	}
	changed := false
	for k := range entry.ForwardOptions {
		restoreVal := entry.PriorOptions[k]
		if secretref.ContainsRef(restoreVal) {
			// No prior value was recorded for this key, on purpose (see
			// stashPriorOptions): it held a credential resolved from the
			// manifest's reference, which a rollback stash must not keep.
			// Left exactly as it is rather than guessed at - re-resolving
			// would write whatever secrets.yaml says NOW, which in the one
			// case that puts a reference here (the secret was rotated) is
			// the very value the rollback is trying to undo.
			continue
		}
		if optionChanges(current, k, restoreVal) {
			changed = true
		}
		overlayOption(merged, k, restoreVal)
	}

	if optionsDiffer(merged, current) {
		if err := postAddonOptions(ctx, client, token, entry.Slug, merged); err != nil {
			return err
		}
	}

	if changed && entry.RestartOnChange {
		if err := restartAndPoll(ctx, client, token, entry.Slug); err != nil {
			slog.Warn("regapply: invert_addon_op restart failed (options already restored)", "slug", entry.Slug, "error", err)
		}
	}

	key := "addon:" + entry.Slug
	if entry.OriginalsExistedBefore {
		originals[key] = entry.OriginalsSnapshotBefore
	} else {
		delete(originals, key)
	}
	if entry.RestartMapExistedBefore {
		restartOnChangeState[key] = entry.PriorRestartOnChangeBefore
	} else {
		delete(restartOnChangeState, key)
	}
	return nil
}

// liveSecretValues is what an inverse of entry must never echo: the
// add-on's CURRENT live value of every option key the manifest declared as
// a reference - the resolved credential, learned by reading it back off the
// live add-on rather than ever storing it. Sorted and deduplicated,
// matching registries.RegOp.Secrets' contract, so redaction walks a
// byte-stable list.
func liveSecretValues(entry addonStashEntry, current map[string]any) []string {
	seen := map[string]bool{}
	for k, declared := range entry.ForwardOptions {
		if !secretref.ContainsRef(declared) {
			continue
		}
		if live, ok := current[k].(string); ok && live != "" {
			seen[live] = true
		}
	}
	if len(seen) == 0 {
		return nil
	}
	return difftext.SortedKeys(seen)
}

// ApplyAddonPlan executes ops (as computed by addonopts.Plan) against
// Supervisor's add-on options/restart REST endpoints, sequentially, over
// client (DefaultAddonHTTPClient if nil).
//
// # Verified Supervisor endpoint shapes (home-assistant/supervisor source)
//
//   - GET /addons/<slug>/info: {"result":"ok","data":{...}}. An installed
//     add-on carries "options" (EFFECTIVE - config defaults merged with
//     persisted overrides) and "state" ("startup"/"started"/"stopped"/
//     "unknown"/"error", never "starting"/"stopping"), and no "installed"
//     key. A slug Supervisor knows but never installed returns 200 with an
//     explicit "installed": false; one it does not know returns 404.
//   - POST /addons/<slug>/options, body {"options": {...}}: replaces the
//     persisted object wholesale, no server-side merge, so any key omitted
//     from a partial POST is dropped (it may still resurface from a
//     config.yaml default on the next read) - see executeAddonOp. Omitting
//     a key is also the ONLY way to unset it: an explicit null is rejected
//     with HTTP 400 "Missing required option '<key>'" even for a key the
//     add-on's schema marks optional (see overlayOption).
//   - POST /addons/<slug>/restart: blocks until the container settles, up
//     to Supervisor's own ~120s internal timeout (logged, not raised).
//   - GET /addons/self/info: "self" resolves to whichever installed add-on
//     the calling SUPERVISOR_TOKEN belongs to - see FetchSelfAddonSlug.
//
// registries.KindError ops are never executed and never block the rest of
// the plan; they come back in RegistryApplyResult.SkippedErrors. On the
// first op that fails, every op already executed here is best-effort
// inverted in reverse order (addonInverseReplayAndPersist) and execution
// stops. Never panics.
//
// A restart-then-poll timeout is the special case: this op's options POST
// already landed, but the op never reaches "executed", so the replay above
// will never see it - executeAddonOp does its own local recovery first.
//
// declaredRestartOnChange (see executeAddonOp), originals
// (state.AddonOriginals) and restartOnChangeState
// (state.AddonRestartOnChange) are all mutated in place; the caller
// persists them afterward.
func ApplyAddonPlan(
	ctx context.Context, client AddonHTTPClient, ops []registries.RegOp,
	declaredRestartOnChange map[string]bool, originals map[string]map[string]any, restartOnChangeState map[string]bool,
	stashDir string,
) RegistryApplyResult {
	return applyLayerPlan(ops, func(executable []registries.RegOp) RegistryApplyResult {
		return applyAddonPlanInner(
			ctx, client, executable, declaredRestartOnChange, originals, restartOnChangeState, stashDir)
	})
}

func applyAddonPlanInner(
	ctx context.Context, client AddonHTTPClient, executable []registries.RegOp,
	declaredRestartOnChange map[string]bool, originals map[string]map[string]any, restartOnChangeState map[string]bool,
	stashDir string,
) (result RegistryApplyResult) {
	defer recoverToResult(&result, "regapply: apply_addon_plan failed")

	client = addonClient(client)
	token, err := options.SupervisorToken()
	if err != nil {
		msg := fmt.Sprintf("unexpected failure: %v", err)
		slog.Warn("regapply: apply_addon_plan failed", "error", msg)
		return RegistryApplyResult{OK: false, Error: msg}
	}

	if err := writeAddonStash(stashDir, nil); err != nil {
		msg := fmt.Sprintf("unexpected failure: %v", err)
		slog.Warn("regapply: apply_addon_plan failed", "error", msg)
		return RegistryApplyResult{OK: false, Error: msg}
	}

	var executed []addonStashEntry
	for _, op := range executable {
		entry, execErr := executeAddonOp(ctx, client, token, op, declaredRestartOnChange, originals, restartOnChangeState)
		// Supervisor's rejection text is quoted verbatim below, and a value
		// it objects to can be one declared as "secret://<name>" (see
		// registries.RegOp.Secrets).
		execErr = redactedError(execErr, op.Secrets)
		if execErr != nil {
			rolledBack, undoErr, notInverted := addonInverseReplayAndPersist(
				ctx, client, token, executed, originals, restartOnChangeState, stashDir)
			errMsg := fmt.Sprintf("%s addon:%s failed: %v", op.Kind, op.Key, execErr)
			if undoErr != "" {
				errMsg = fmt.Sprintf("%s; rollback also incomplete: %s", errMsg, undoErr)
			}
			if entry.Slug != "" {
				// A double fault from executeAddonOp: the forward write
				// landed and could not be undone. The inverse-replay above
				// already reverted every sibling it could, and those must
				// not be re-listed as applied - recon's pending-ops filter
				// reads "in Applied" as "took effect" and would never
				// resubmit a reverted sibling. Only notInverted plus this
				// op belong in the final applied/stash list.
				executed = append(append([]addonStashEntry(nil), notInverted...), entry)
				if writeErr := writeAddonStash(stashDir, executed); writeErr != nil {
					errMsg = fmt.Sprintf("%s; additionally, could not persist the stuck op to the rollback journal: %v", errMsg, writeErr)
				}
				slog.Warn("regapply: apply_addon_plan", "error", errMsg)
				return RegistryApplyResult{OK: false, Applied: appliedAddonLabels(executed), Error: errMsg, RolledBack: false}
			}
			slog.Warn("regapply: apply_addon_plan", "error", errMsg)
			return RegistryApplyResult{OK: false, Error: errMsg, RolledBack: rolledBack}
		}
		executed = append(executed, entry)

		if err := writeAddonStash(stashDir, executed); err != nil {
			msg := fmt.Sprintf(
				"%d addon op(s) applied successfully, but the rollback journal could not be written after %s addon:%s, "+
					"so no further ops were attempted and these cannot be rolled back from disk: %v",
				len(executed), op.Kind, op.Key, err)
			slog.Warn("regapply: apply_addon_plan", "error", msg)
			return RegistryApplyResult{OK: false, Applied: appliedAddonLabels(executed), Error: msg}
		}
	}

	applied := appliedAddonLabels(executed)
	slog.Info("regapply: apply_addon_plan executed", "applied", len(applied))
	return RegistryApplyResult{OK: true, Applied: applied}
}

func appliedAddonLabels(executed []addonStashEntry) []string {
	out := make([]string, len(executed))
	for i, e := range executed {
		out[i] = fmt.Sprintf("%s addon:%s", e.Kind, e.Slug)
	}
	return out
}

// RollbackAddonPlan undoes a previous ApplyAddonPlan call by replaying
// <stashDir>/addon_stash.json in reverse - the addon-options counterpart
// of RollbackRegistry, called alongside it by the same Rollback button
// (see recon.Reconciler.Rollback). A missing addon_stash.json is not an
// error: most apply cycles have no addon ops, so ApplyAddonPlan never ran
// and internal/recon checks for the file's presence first.
func RollbackAddonPlan(
	ctx context.Context, client AddonHTTPClient, stashDir string,
	originals map[string]map[string]any, restartOnChangeState map[string]bool,
) (result RegistryApplyResult) {
	defer recoverToResult(&result, "regapply: rollback_addon_plan")

	client = addonClient(client)
	token, err := options.SupervisorToken()
	if err != nil {
		msg := fmt.Sprintf("unexpected failure: %v", err)
		slog.Warn("regapply: rollback_addon_plan failed", "error", msg)
		return RegistryApplyResult{OK: false, Error: msg}
	}

	executed, err := readAddonStash(stashDir)
	if err != nil {
		msg := fmt.Sprintf("cannot read addon rollback stash: %v", err)
		slog.Warn("regapply: rollback_addon_plan", "error", msg)
		return RegistryApplyResult{OK: false, Error: msg}
	}

	rolledBack, errMsg, _ := addonInverseReplayAndPersist(ctx, client, token, executed, originals, restartOnChangeState, stashDir)
	if errMsg != "" {
		slog.Warn("regapply: rollback_addon_plan", "error", errMsg)
	} else {
		slog.Info("regapply: rollback_addon_plan undid ops", "count", len(executed))
	}
	return RegistryApplyResult{OK: rolledBack, RolledBack: rolledBack, Error: errMsg}
}

// addonInverseReplayAndPersist best-effort inverts every entry in
// executed, in reverse order - the addon-options counterpart of
// inverseReplayAndPersist, with no WS Dialer/redial concept and the same
// write-before-invert polarity, for the same reason (see that function):
// dropping an entry BEFORE attempting its inverse under-reverts rather
// than risking a double-invert on a later retry.
//
// notInverted holds exactly the entries NOT successfully inverted (a
// stash-write failure that skipped the attempt, or invertAddonOp itself
// failing), in chronological order. A caller with a further op to record
// alongside them (executeAddonOp's double fault) needs this precise set:
// reusing executed wholesale would re-list ops that were just reverted
// live as still "applied".
func addonInverseReplayAndPersist(
	ctx context.Context, client AddonHTTPClient, token string, executed []addonStashEntry,
	originals map[string]map[string]any, restartOnChangeState map[string]bool, stashDir string,
) (rolledBack bool, errMsg string, notInverted []addonStashEntry) {
	var failures []string
	outstanding := make([]int, len(executed))
	for i := range executed {
		outstanding[i] = i
	}
	var notInvertedReversed []addonStashEntry // built newest-first, this loop's own order

	for pos := len(executed) - 1; pos >= 0; pos-- {
		entry := executed[pos]
		label := fmt.Sprintf("%s addon:%s", entry.Kind, entry.Slug)

		// Committed only after a successful write, the registry replay's
		// discipline: a plain truncation would let the NEXT successful
		// write persist a journal missing an entry this failure skipped,
		// dropping it from every later retry with its options still live.
		shortenedIdx := removeInt(outstanding, pos)
		if err := writeAddonStash(stashDir, entriesFor(executed, shortenedIdx)); err != nil {
			failures = append(failures, fmt.Sprintf("%s: stash write failed: %v", label, err))
			notInvertedReversed = append(notInvertedReversed, entry)
			continue
		}
		outstanding = shortenedIdx

		if err := invertAddonOp(ctx, client, token, entry, originals, restartOnChangeState); err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", label, err))
			notInvertedReversed = append(notInvertedReversed, entry)
		}
	}

	for i := len(notInvertedReversed) - 1; i >= 0; i-- {
		notInverted = append(notInverted, notInvertedReversed[i])
	}

	if len(failures) > 0 {
		return false, strings.Join(failures, "; "), notInverted
	}
	return true, "", notInverted
}

// addonStashOpOnDisk is one entry's on-disk shape inside addon_stash.json.
type addonStashOpOnDisk struct {
	Kind                       string         `json:"kind"`
	Slug                       string         `json:"slug"`
	PriorOptions               map[string]any `json:"prior_options"`
	ForwardOptions             map[string]any `json:"forward_options"`
	RestartOnChange            bool           `json:"restart_on_change"`
	OriginalsExistedBefore     bool           `json:"originals_existed_before"`
	OriginalsSnapshotBefore    map[string]any `json:"originals_snapshot_before,omitempty"`
	RestartMapExistedBefore    bool           `json:"restart_map_existed_before"`
	PriorRestartOnChangeBefore bool           `json:"prior_restart_on_change_before"`
}

type addonStashFileOnDisk struct {
	Ops []addonStashOpOnDisk `json:"ops"`
}

// toAddonStashOnDisk/fromAddonStashOnDisk convert element-wise between
// addonStashEntry and addonStashOpOnDisk - identical field sets in the
// same order, differing only in JSON tags, so a direct type conversion
// stays exhaustive: adding a field to one alone fails to compile.
func toAddonStashOnDisk(executed []addonStashEntry) []addonStashOpOnDisk {
	out := make([]addonStashOpOnDisk, len(executed))
	for i, e := range executed {
		out[i] = addonStashOpOnDisk(e)
	}
	return out
}

func fromAddonStashOnDisk(disk []addonStashOpOnDisk) []addonStashEntry {
	out := make([]addonStashEntry, len(disk))
	for i, d := range disk {
		out[i] = addonStashEntry(d)
	}
	return out
}

// writeAddonStash atomically rewrites <stashDir>/addon_stash.json to hold
// exactly entries. A var, not a plain func, so tests can substitute a
// failing implementation - mirrors writeRegistryStash.
var writeAddonStash = writeAddonStashReal

func writeAddonStashReal(stashDir string, entries []addonStashEntry) error {
	return writeStashFile(stashDir, addonStashFile, addonStashFileOnDisk{Ops: toAddonStashOnDisk(entries)})
}

// readAddonStash reads <stashDir>/addon_stash.json. A missing file returns
// (nil, nil), not an error, mirroring readRegistryStashTolerant: an apply
// cycle with no addon ops never creates the file at all.
func readAddonStash(stashDir string) ([]addonStashEntry, error) {
	decoded, found, err := readStashFile[addonStashFileOnDisk](stashDir, addonStashFile)
	if err != nil || !found {
		return nil, err
	}
	return fromAddonStashOnDisk(decoded.Ops), nil
}

// AddonStashExists reports whether stashDir holds an addon_stash.json -
// the addon-layer analogue of the registry_stash.json presence check
// recon.Reconciler.Rollback makes before calling RollbackRegistry.
func AddonStashExists(stashDir string) bool {
	return stashFileExists(stashDir, addonStashFile)
}
