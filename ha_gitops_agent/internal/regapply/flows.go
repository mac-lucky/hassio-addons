// Package regapply's flows file is internal/flows' execution counterpart,
// driven over Home Assistant Core's plain REST API through the
// Supervisor's proxy at http://supervisor/core/api/... (same
// SUPERVISOR_TOKEN, via homeassistant_api: true): config-entry flows have
// no WebSocket surface at all. See ApplyFlowPlan for the verified endpoint
// shapes.
//
// # The one WebSocket exception: renaming a created entry
//
// Core's ConfigManagerEntryResourceView implements only DELETE for
// /api/config/config_entries/entry/<entry_id> - no REST route writes an
// entry's title, and the frontend's own Rename dialog uses the WebSocket
// command config_entries/update. That is the only thing this file's Dialer
// is for, and only when a create needs it (see applyDeclaredTitle).
//
// Like addonopts.go it keeps its rollback journal in its own
// <stashDir>/integration_stash.json, under the same
// reset-at-start/write-after-confirmed-op/write-before-invert discipline,
// so no unrelated WS Dialer has to be threaded through a REST-only flow
// state machine. recon.Reconciler.Rollback covers all three stashes.
package regapply

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

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/flows"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/httperr"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/httpx"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/options"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/registries"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/secretref"
)

// Timeouts for the Core config-entry endpoints this file calls.
const (
	integrationEntryListTimeout   = 15 * time.Second
	integrationFlowStepTimeout    = 20 * time.Second
	integrationFlowAbortTimeout   = 15 * time.Second
	integrationEntryDeleteTimeout = 15 * time.Second
)

// maxFlowSteps bounds driveFlow's loop: how many "form" steps it advances
// through before giving up and aborting. Generous against any known flow
// (most need one or two) while still guaranteeing termination.
const maxFlowSteps = 5

// msgConfigEntriesUpdate is the WebSocket command that writes a config
// entry's title - see the package doc comment.
const msgConfigEntriesUpdate = "config_entries/update"

// Bounds on the field list describeStepSchema renders into a "no declared
// data" error. Same reasoning as internal/httperr's maxDetailChars: these
// strings render one per row in the events feed, and a step's schema (an
// entity picker's few thousand options, say) is not under our control.
const (
	maxSchemaFields       = 12
	maxSelectOptions      = 12
	maxSchemaChars        = 400
	schemaTruncationMark  = " ... (truncated)"
	optionsTruncationMark = "..."
)

// IntegrationHTTPClient is internal/httpx's Doer, same as AddonHTTPClient
// but kept as its own name because the two default clients and their
// nil-handling helpers are per-file.
type IntegrationHTTPClient = httpx.Doer

// DefaultIntegrationHTTPClient is the IntegrationHTTPClient used when any
// function in this file is called with a nil client.
var DefaultIntegrationHTTPClient IntegrationHTTPClient = http.DefaultClient

func integrationClient(client IntegrationHTTPClient) IntegrationHTTPClient {
	if client == nil {
		return DefaultIntegrationHTTPClient
	}
	return client
}

func coreAPI(path string) string {
	return options.Supervisor + "/core/api" + path
}

// doIntegrationRequest issues one HTTP request against Core's REST API
// (through the Supervisor proxy), decoding a successful JSON body into out
// (nil to discard it). Shared by every call here for auth header, timeout
// and non-2xx handling; a non-2xx carries Core's own explanation read off
// the body (internal/httperr) - see advanceFlow for the one caller that
// scrubs its own submitted secrets back out of the result.
func doIntegrationRequest(
	ctx context.Context, client IntegrationHTTPClient, token, method, path string, body any, timeout time.Duration, out any,
) (status int, err error) {
	var reader *bytes.Reader
	if body != nil {
		encoded, marshalErr := json.Marshal(body)
		if marshalErr != nil {
			return 0, fmt.Errorf("failed to encode request body: %w", marshalErr)
		}
		reader = bytes.NewReader(encoded)
	} else {
		reader = bytes.NewReader(nil)
	}

	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, method, coreAPI(path), reader)
	if err != nil {
		return 0, fmt.Errorf("failed to build %s %s request: %w", method, path, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("%s %s request failed: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, fmt.Errorf("%s %s returned HTTP %d%s", method, path, resp.StatusCode, httperr.Suffix(resp))
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return resp.StatusCode, fmt.Errorf("%s %s returned invalid JSON: %w", method, path, err)
		}
	}
	return resp.StatusCode, nil
}

// FetchIntegrationEntries fetches every config entry Core knows about via
// GET /api/config/config_entries/entry, unfiltered: flows.Plan needs the
// FULL list, since one cycle may adopt-match across several domains, and
// filtering server-side per manifest item would cost a round trip each.
//
// # Verified response shape (home-assistant/core source)
//
// A bare JSON array, no {"result":"ok","data":...} envelope (Core's REST
// API never wraps, unlike Supervisor's), each element the
// ConfigEntry.as_json_fragment JSON - entry_id, domain, title, source and
// state at minimum, of which flows.Plan reads only the first three.
func FetchIntegrationEntries(ctx context.Context, client IntegrationHTTPClient) ([]map[string]any, error) {
	client = integrationClient(client)
	token, err := options.SupervisorToken()
	if err != nil {
		return nil, err
	}

	var entries []map[string]any
	if _, err := doIntegrationRequest(
		ctx, client, token, http.MethodGet, "/config/config_entries/entry", nil, integrationEntryListTimeout, &entries,
	); err != nil {
		return nil, err
	}
	if entries == nil {
		entries = []map[string]any{}
	}
	return entries, nil
}

// ErrIntegrationNotLoaded is what starting a flow for a domain Home
// Assistant does not have reports: Core answers an unregistered handler
// with 404 "Invalid handler specified", which ordinarily means the
// integration's CODE is not loaded - the case a gitops/hacs.yaml download
// creates, since custom_components are imported at startup.
//
// A sentinel because the failure is TRANSIENT and executeFlowOp must not
// remember it: the hash never changes afterwards, so a record would block
// the item forever on a condition that clears at the next restart.
var ErrIntegrationNotLoaded = errors.New("integration code is not loaded")

// startFlow starts a new config flow for domain via
// POST /api/config/config_entries/flow, body {"handler": domain}.
func startFlow(ctx context.Context, client IntegrationHTTPClient, token, domain string) (map[string]any, error) {
	var result map[string]any
	status, err := doIntegrationRequest(
		ctx, client, token, http.MethodPost, "/config/config_entries/flow",
		map[string]any{"handler": domain}, integrationFlowStepTimeout, &result,
	)
	if err != nil {
		if status == http.StatusNotFound {
			return nil, fmt.Errorf(
				"%w: home assistant has no '%s' integration to set up - if it was just downloaded through HACS, "+
					"restart home assistant and this will be attempted again on its own: %w",
				ErrIntegrationNotLoaded, domain, err)
		}
		return nil, fmt.Errorf("failed to start flow for domain %s: %w", domain, err)
	}
	return result, nil
}

// advanceFlow answers one "form" step via POST
// /api/config/config_entries/flow/<flow_id>, body = the step's declared
// field data verbatim (Core passes the decoded body straight through as
// that step's user_input, never wrapped under a key).
//
// A failure comes back with the step's own submitted secrets scrubbed out
// (redactStepSecrets): this is the one request in this add-on whose body
// may carry a credential, and a rejected step's error quotes Home
// Assistant's voluptuous message, which can name the offending value.
func advanceFlow(
	ctx context.Context, client IntegrationHTTPClient, token, flowID string, stepData map[string]any,
) (map[string]any, error) {
	if stepData == nil {
		stepData = map[string]any{}
	}
	var result map[string]any
	if _, err := doIntegrationRequest(
		ctx, client, token, http.MethodPost, "/config/config_entries/flow/"+flowID,
		stepData, integrationFlowStepTimeout, &result,
	); err != nil {
		return nil, errors.New(redactStepSecrets(err.Error(), stepData))
	}
	return result, nil
}

// abortFlow cancels an in-progress flow via DELETE
// /api/config/config_entries/flow/<flow_id>. A 404 ("Invalid flow
// specified" - already finished, expired or aborted) is treated as
// success: no flow is left open under this flow_id either way.
func abortFlow(ctx context.Context, client IntegrationHTTPClient, token, flowID string) error {
	status, err := doIntegrationRequest(
		ctx, client, token, http.MethodDelete, "/config/config_entries/flow/"+flowID, nil, integrationFlowAbortTimeout, nil)
	if err != nil && status == http.StatusNotFound {
		return nil
	}
	return err
}

// abortFlowBestEffort calls abortFlow and only logs a failure, never
// escalating it over the primary reason the flow is being abandoned -
// Home Assistant's flow manager garbage-collects abandoned flows on its
// own timeout anyway, so a failed cleanup is degraded, not catastrophic
// (see driveFlow for why every non-terminal path still attempts it).
func abortFlowBestEffort(ctx context.Context, client IntegrationHTTPClient, token, flowID, domain string) {
	if flowID == "" {
		return
	}
	if err := abortFlow(ctx, client, token, flowID); err != nil {
		slog.Warn("regapply: could not abort in-progress flow", "domain", domain, "flow_id", flowID, "error", err)
	}
}

// deleteEntry deletes one config entry via DELETE
// /api/config/config_entries/entry/<entry_id>. A 404 is success for the
// same reason abortFlow treats one as success: the goal already holds.
func deleteEntry(ctx context.Context, client IntegrationHTTPClient, token, entryID string) error {
	status, err := doIntegrationRequest(
		ctx, client, token, http.MethodDelete, "/config/config_entries/entry/"+entryID, nil, integrationEntryDeleteTimeout, nil)
	if err != nil && status == http.StatusNotFound {
		return nil
	}
	return err
}

// stepDataFor looks up stepID's declared field map inside data (step id ->
// field map). False both when the key is absent and when its value is not
// a mapping - driveFlow treats both as "no usable data", which aborts.
func stepDataFor(data map[string]any, stepID string) (map[string]any, bool) {
	raw, present := data[stepID]
	if !present {
		return nil, false
	}
	fields, ok := raw.(map[string]any)
	return fields, ok
}

// stepSchemaIsEmpty reports whether a "form" result's data_schema asks for
// nothing at all: absent, null, or an empty list. Home Assistant renders a
// confirm-only step (data_schema=None) and an empty schema exactly those
// two ways and answers both with {} - moon and local_ip are live examples.
// Anything else, including an unrecognised shape, is deliberately not
// empty: an unreadable schema is no evidence that the step wants nothing.
func stepSchemaIsEmpty(rawSchema any) bool {
	if rawSchema == nil {
		return true
	}
	fields, ok := rawSchema.([]any)
	return ok && len(fields) == 0
}

// describeStepSchema renders a "form" result's data_schema as the field
// list driveFlow names in its "no declared data" error - the one place
// Home Assistant hands the agent the answer to "what goes in data?".
//
// # Verified shape (live probes against real hardware)
//
// A JSON array of field objects, each carrying "name", the booleans
// "required" and/or "optional", and either a "selector" (a one-key map
// naming the kind, e.g. {"select": {"options": [...], "multiple": false}},
// whose options are strings or {"value", "label"} objects) or a legacy
// "type" string. All of it is untrusted, since a custom integration can
// put anything here: an entry with no usable name is dropped, an
// unparseable selector degrades to just the name, and "" means "say
// nothing further" rather than "this step accepts nothing".
func describeStepSchema(rawSchema any) string {
	fields, ok := rawSchema.([]any)
	if !ok {
		return ""
	}

	clauses := make([]string, 0, len(fields))
	overLimit := false
	for _, entry := range fields {
		if len(clauses) >= maxSchemaFields {
			overLimit = true
			break
		}
		if clause := describeSchemaField(entry); clause != "" {
			clauses = append(clauses, clause)
		}
	}
	if len(clauses) == 0 {
		return ""
	}

	text := strings.Join(clauses, ", ")
	if chars := []rune(text); len(chars) > maxSchemaChars {
		return string(chars[:maxSchemaChars]) + schemaTruncationMark
	}
	if overLimit {
		return text + schemaTruncationMark
	}
	return text
}

// describeSchemaField renders one data_schema entry as "<name>" or
// "<name> (<attributes>)".
func describeSchemaField(entry any) string {
	field, ok := entry.(map[string]any)
	if !ok {
		return ""
	}
	name, _ := field["name"].(string)
	if name == "" {
		return ""
	}

	var attrs []string
	if required, _ := field["required"].(bool); required {
		attrs = append(attrs, "required")
	} else if optional, _ := field["optional"].(bool); optional {
		attrs = append(attrs, "optional")
	}
	if kind := describeSchemaKind(field); kind != "" {
		attrs = append(attrs, kind)
	}
	if len(attrs) == 0 {
		return name
	}
	return name + " (" + strings.Join(attrs, ", ") + ")"
}

// describeSchemaKind names one field's input kind: its selector's own key,
// marked "(multiple)" when it takes a list, with a select's options
// enumerated (the only values that step will accept), or the legacy "type"
// string for a schema too old to carry a selector.
func describeSchemaKind(field map[string]any) string {
	selector, ok := field["selector"].(map[string]any)
	if !ok || len(selector) == 0 {
		legacy, _ := field["type"].(string)
		return legacy
	}

	// A selector is a one-key map in every shape Home Assistant emits;
	// sorting makes a malformed multi-key one render deterministically.
	kinds := make([]string, 0, len(selector))
	for kind := range selector {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	kind := kinds[0]

	config := selector[kind]
	if selectorIsMultiple(config) {
		kind += " (multiple)"
	}

	options := selectorOptions(config)
	if len(options) == 0 {
		return kind
	}
	return kind + ": " + strings.Join(options, ", ")
}

// selectorIsMultiple reports whether a selector takes a list of values
// rather than one - what makes the option list actionable: time_date's
// display_options is multiple:false, and answering it with a one-element
// list is rejected ("expected str @ data['display_options']").
func selectorIsMultiple(config any) bool {
	cfg, ok := config.(map[string]any)
	if !ok {
		return false
	}
	multiple, _ := cfg["multiple"].(bool)
	return multiple
}

// selectorOptions renders a selector config's "options" list as the values
// a manifest would submit. A {"value", "label"} entry contributes its
// VALUE: the label is for humans, not what the flow accepts.
func selectorOptions(config any) []string {
	cfg, ok := config.(map[string]any)
	if !ok {
		return nil
	}
	raw, ok := cfg["options"].([]any)
	if !ok {
		return nil
	}

	out := make([]string, 0, len(raw))
	for _, opt := range raw {
		if len(out) >= maxSelectOptions {
			out = append(out, optionsTruncationMark)
			break
		}
		value := scalarOption(opt)
		if obj, isObject := opt.(map[string]any); isObject {
			value = scalarOption(obj["value"])
		}
		if value != "" {
			out = append(out, value)
		}
	}
	return out
}

// scalarOption renders one option value: a string as itself, any other
// scalar through Go formatting, anything structured as "" (dropped).
func scalarOption(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case bool, float64:
		return fmt.Sprint(v)
	}
	return ""
}

// stepAcceptsSuffix renders describeStepSchema as a sentence for the "no
// declared data" error, or "" when the schema could not be read.
func stepAcceptsSuffix(stepID string, rawSchema any) string {
	described := describeStepSchema(rawSchema)
	if described == "" {
		return ""
	}
	return fmt.Sprintf(". Step '%s' accepts: %s", stepID, described)
}

// applyDeclaredTitle renames a just-created config entry to the title the
// manifest declared, when the flow named it something else. Nothing to do
// when the manifest declared no title, or the flow already produced it.
//
// Not cosmetic: a flow titles its own entry (time_date's comes out "Time &
// Date time"), while adoption matches on domain plus EXACT live title, so
// an entry left with the flow's title can never be matched back to the
// item that created it - and the moment tracking is lost (a reset /data, a
// restored backup), the next reconcile creates a DUPLICATE beside it.
func applyDeclaredTitle(ctx context.Context, dialer Dialer, entryID, declared, live string) error {
	if declared == "" || declared == live {
		return nil
	}
	return renameEntry(ctx, dialer, entryID, declared)
}

// renameEntry retitles one config entry over the WebSocket command
// config_entries/update - the only API Home Assistant has for it (see the
// package doc comment). Dialed and closed per call: a rename only happens
// on a create, and coder/websocket kills a connection on any error anyway.
func renameEntry(ctx context.Context, dialer Dialer, entryID, title string) error {
	if dialer == nil {
		return errors.New("no websocket dialer was configured for this call")
	}
	ws, err := dialer(ctx)
	if err != nil {
		return err
	}
	defer ws.Close()

	_, err = ws.Cmd(ctx, msgConfigEntriesUpdate, map[string]any{"entry_id": entryID, "title": title})
	return err
}

// driveFlow starts and drives domain's config flow to completion,
// answering every "form" step from data (the manifest's declared "data"
// mapping) until Home Assistant reports "create_entry", returning the new
// entry_id and the title the flow chose - never anything submitted here
// (see applyDeclaredTitle).
//
// A step whose data_schema is empty (absent, null, or []) is answered with
// {} whether the manifest declares anything for it or not; only a step
// that genuinely wants fields with nothing declared is an error, and that
// error names the fields off the schema (see describeStepSchema).
//
// # Bounded v1: every non-terminal exit aborts the flow first
//
// "create_entry" and "abort" are the only terminal types - Home Assistant
// has already ended the flow by then. Every other exit (missing declared
// data, a step HA itself rejected via a non-empty "errors" map, more than
// maxFlowSteps forms, a transport failure mid-advance, or a type outside
// form/create_entry/abort - menu, external auth, discovery, show-progress,
// all out of bounded v1's scope) calls abortFlowBestEffort first, so this
// function NEVER leaves a flow open that it started.
//
// # Verified FlowResult shapes (homeassistant/data_entry_flow.py)
//
//   - "form": flow_id, handler, step_id, data_schema, errors (nil, or a
//     populated field->message map meaning HA rejected the PREVIOUS step's
//     data and this response re-asks for it), description_placeholders,
//     last_step.
//   - "create_entry": flow_id, handler, title, description,
//     description_placeholders, and "result" - the created ConfigEntry's
//     as_json_fragment, where entry_id lives, NOT at the top level (see
//     config/config_entries.py's _prepare_config_flow_result_json).
//   - "abort": flow_id, handler, reason, description_placeholders.
func driveFlow(
	ctx context.Context, client IntegrationHTTPClient, token, domain string, data map[string]any,
) (entryID string, liveTitle string, err error) {
	if data == nil {
		data = map[string]any{}
	}

	result, err := startFlow(ctx, client, token, domain)
	if err != nil {
		return "", "", err
	}
	flowID, _ := result["flow_id"].(string)

	for steps := 0; ; {
		typ, _ := result["type"].(string)
		switch typ {
		case "create_entry":
			resultObj, _ := result["result"].(map[string]any)
			newID, _ := resultObj["entry_id"].(string)
			if newID == "" {
				return "", "", fmt.Errorf("domain %s: create_entry result carried no entry_id", domain)
			}
			// The ConfigEntry fragment's own title first: it is what a later
			// adopt-match reads back off /config/config_entries/entry. The
			// top-level title is the fallback for an integration that omits it.
			title, _ := resultObj["title"].(string)
			if title == "" {
				title, _ = result["title"].(string)
			}
			return newID, title, nil

		case "abort":
			reason, _ := result["reason"].(string)
			return "", "", fmt.Errorf("domain %s: home assistant aborted the flow: %s", domain, reason)

		case "form":
			stepID, _ := result["step_id"].(string)
			if errs, ok := result["errors"].(map[string]any); ok && len(errs) > 0 {
				abortFlowBestEffort(ctx, client, token, flowID, domain)
				return "", "", fmt.Errorf("domain %s: step '%s' rejected the declared data: %v", domain, stepID, errs)
			}

			schema := result["data_schema"]
			stepData, hasStep := stepDataFor(data, stepID)
			if !hasStep {
				if !stepSchemaIsEmpty(schema) {
					abortFlowBestEffort(ctx, client, token, flowID, domain)
					return "", "", fmt.Errorf(
						"domain %s: flow step '%s' has no declared data in the manifest (add a data.%s mapping)%s",
						domain, stepID, stepID, stepAcceptsSuffix(stepID, schema))
				}
				stepData = map[string]any{}
			}

			steps++
			if steps > maxFlowSteps {
				abortFlowBestEffort(ctx, client, token, flowID, domain)
				return "", "", fmt.Errorf("domain %s: flow exceeded %d steps without completing", domain, maxFlowSteps)
			}

			next, advErr := advanceFlow(ctx, client, token, flowID, stepData)
			if advErr != nil {
				abortFlowBestEffort(ctx, client, token, flowID, domain)
				return "", "", fmt.Errorf("domain %s: step '%s': %w", domain, stepID, advErr)
			}
			result = next

		default:
			stepID, _ := result["step_id"].(string)
			abortFlowBestEffort(ctx, client, token, flowID, domain)
			return "", "", fmt.Errorf(
				"domain %s: flow step '%s' has type %q, which this bounded v1 layer does not support "+
					"(only plain form steps are handled - no OAuth/external-auth, discovery, menu, reauth or "+
					"progress flows); delete the entry by hand if Home Assistant already created one",
				domain, stepID, typ)
		}
	}
}

// integrationStashEntry is the in-memory (and, via
// toIntegrationStashOnDisk, on-disk) record of one executed integration
// op, enough to invert it later. Kind is flows.KindCreate,
// flows.KindUpdate (adopt) or registries.KindDelete.
type integrationStashEntry struct {
	Kind    string
	Key     string
	Domain  string
	Title   string
	EntryID string
	// Data is the declared "data" mapping this op recorded against Key when
	// it ran (see state.IntegrationData). For a create/adopt it is what the
	// forward op used; for a delete it is whatever was snapshotted back
	// when the key was created or adopted - the manifest no longer declares
	// it - fed straight back into driveFlow if the delete is rolled back.
	Data map[string]any
}

// executeFlowOp executes a single create/update(adopt)/delete op and
// returns a record of it, for both the stash file and a later invertFlowOp
// call.
//
// The middle return is a degraded-but-succeeded note: a create whose entry
// exists and is tracked, but whose declared title could not be written
// (see applyDeclaredTitle). Deliberately NOT an error - failing the op
// would discard a config entry Home Assistant genuinely created and leave
// it untracked - but not silence either, since a lost-tracking reconcile
// would then duplicate it.
//
// liveByEntryID is this apply's own fresh fetch of every config entry
// (ApplyFlowPlan calls FetchIntegrationEntries once up front), consumed
// only by a KindDelete op, which needs the live entry's domain/title
// because op.Params is empty for a delete.
//
// attempts is state.IntegrationAttempts (see internal/flows' "Failure
// memory"): a KindCreate that fails here records this key's hash and a
// short error, so the NEXT plan for the same still-broken data refuses to
// retry it; any success clears a stale entry for the key.
func executeFlowOp(
	ctx context.Context, client IntegrationHTTPClient, dialer Dialer, token string, op registries.RegOp,
	liveByEntryID map[string]map[string]any,
	managed map[string]string, hashes map[string]string, dataSnapshots map[string]map[string]any,
	attempts map[string]map[string]any,
) (integrationStashEntry, string, error) {
	key := "integration:" + op.Key

	switch op.Kind {
	case flows.KindCreate:
		domain, _ := op.Params["domain"].(string)
		title, _ := op.Params["title"].(string)
		data, _ := op.Params["data"].(map[string]any)
		declared := declaredDataOf(op)

		entryID, liveTitle, err := driveFlow(ctx, client, token, domain, data)
		// Scrubbed before anything below reports or records it, and
		// identity-preserving so the sentinel check further down still
		// matches through it (see redactedErr).
		err = redactedError(err, op.Secrets)
		if err != nil {
			// Failure memory (see internal/flows): record the hash of the
			// data that just failed plus a short reason, so the NEXT plan
			// for this key refuses to retry the identical failure every
			// cycle - the create/fail/clean-up/repeat churn the VM e2e
			// reproduced live. Overwriting any prior entry is how "clear
			// when the hash changes" comes for free.
			//
			// The one failure NOT remembered is a domain Home Assistant does
			// not have yet (ErrIntegrationNotLoaded): its hash will never
			// change, so a record would block the item permanently on a
			// condition that clears itself at the next restart.
			if !errors.Is(err, ErrIntegrationNotLoaded) {
				attempts[key] = map[string]any{"hash": flows.HashData(data), "error": err.Error()}
			}
			return integrationStashEntry{}, "", err
		}
		delete(attempts, key)
		managed[key] = entryID
		hashes[key] = flows.HashData(data)
		// The reference, not what it resolved to (see internal/flows'
		// "Secret references"). The hash above is of the RESOLVED data, so a
		// rotated secret still reads as a change on the next plan.
		dataSnapshots[key] = declared
		executed := integrationStashEntry{
			Kind: flows.KindCreate, Key: op.Key, Domain: domain, Title: title, EntryID: entryID, Data: declared,
		}

		if renameErr := applyDeclaredTitle(ctx, dialer, entryID, title, liveTitle); renameErr != nil {
			slog.Warn("regapply: created integration keeps the title its flow chose",
				"domain", domain, "entry_id", entryID, "declared_title", title, "live_title", liveTitle, "error", renameErr)
			return executed, fmt.Sprintf(
				"create integration:%s created entry %s, but it is still titled %q instead of the declared %q, "+
					"so a later adopt cannot match it: %v",
				op.Key, entryID, liveTitle, title, renameErr), nil
		}
		return executed, "", nil

	case flows.KindUpdate: // adopt - pure bookkeeping, no live call (see the internal/flows package doc comment)
		domain, _ := op.Params["domain"].(string)
		title, _ := op.Params["title"].(string)
		data, _ := op.Params["data"].(map[string]any)
		declared := declaredDataOf(op)
		entryID := op.LiveID

		// An adopt can never itself fail, but a stale attempts[key] from an
		// earlier failed CREATE of the same key is no longer relevant once
		// a match puts it under management - drop it defensively.
		delete(attempts, key)
		managed[key] = entryID
		hashes[key] = flows.HashData(data)
		dataSnapshots[key] = declared
		return integrationStashEntry{
			Kind: flows.KindUpdate, Key: op.Key, Domain: domain, Title: title, EntryID: entryID, Data: declared,
		}, "", nil

	case registries.KindDelete:
		entryID := op.LiveID
		liveEntry := liveByEntryID[entryID]
		if liveEntry == nil {
			// Vanished between plan and apply. deleteEntry would map the
			// 404 to success and stash an entry with no domain, whose
			// inverse can only start a flow with handler "" - a rollback
			// that can never succeed. Nothing changed live, so nothing goes
			// in the stash; the bookkeeping still clears.
			slog.Info("regapply: apply_flow_plan: config entry already absent, nothing to delete",
				"key", op.Key, "entry_id", entryID)
			delete(managed, key)
			delete(hashes, key)
			delete(dataSnapshots, key)
			return integrationStashEntry{}, "", nil
		}
		domain, _ := liveEntry["domain"].(string)
		title, _ := liveEntry["title"].(string)
		data := dataSnapshots[key]

		if err := deleteEntry(ctx, client, token, entryID); err != nil {
			return integrationStashEntry{}, "", err
		}
		delete(managed, key)
		delete(hashes, key)
		delete(dataSnapshots, key)
		return integrationStashEntry{
			Kind: registries.KindDelete, Key: op.Key, Domain: domain, Title: title, EntryID: entryID, Data: data,
		}, "", nil
	}

	return integrationStashEntry{}, "", fmt.Errorf("unreachable: unknown op kind %q", op.Kind)
}

// declaredDataOf returns the manifest's own, still-unresolved declared
// data for a create/adopt op - what state.IntegrationData and the stash
// must hold, as opposed to Params' "data", the resolved copy that goes on
// the wire (see internal/flows' "Secret references").
//
// op.Declared and nothing else, deliberately: falling back to the RESOLVED
// data when Declared is unset would fail open on the one field whose job
// is keeping a credential out of state.json, persisting the resolved
// password instead of the reference. An op that declares nothing records
// nothing.
func declaredDataOf(op registries.RegOp) map[string]any {
	if op.Declared == nil {
		return map[string]any{}
	}
	return op.Declared
}

// invertFlowOp inverts one executed integration stash entry:
//
//   - create -> delete the entry, then drop the bookkeeping it added.
//   - update (adopt) -> no live call at all (nothing was ever sent to
//     adopt it), just drop the bookkeeping, restoring "unmanaged".
//   - delete -> re-run entry.Domain's flow with entry.Data, producing a
//     NEW entry_id (Home Assistant never guarantees it matches the deleted
//     one), then re-record the bookkeeping under it and put the declared
//     title back (applyDeclaredTitle - a re-created entry carries the same
//     adopt-matching hazard as a fresh one).
//
// entry.Data is the declared data as WRITTEN, references and all (see
// declaredDataOf), so the delete branch resolves it against the live
// secrets file at the moment of the rollback, not the value that was in it
// when the integration was deleted.
func invertFlowOp(
	ctx context.Context, client IntegrationHTTPClient, dialer Dialer, token string, entry integrationStashEntry,
	managed map[string]string, hashes map[string]string, dataSnapshots map[string]map[string]any,
	secrets *secretref.Resolver,
) error {
	key := "integration:" + entry.Key

	switch entry.Kind {
	case flows.KindCreate:
		if err := deleteEntry(ctx, client, token, entry.EntryID); err != nil {
			return err
		}
		delete(managed, key)
		delete(hashes, key)
		delete(dataSnapshots, key)
		return nil

	case flows.KindUpdate:
		delete(managed, key)
		delete(hashes, key)
		delete(dataSnapshots, key)
		return nil

	case registries.KindDelete:
		data, secretValues, resolveErr := secrets.ResolveMap(entry.Data)
		if resolveErr != nil {
			return fmt.Errorf(
				"could not re-create integration '%s' (domain %s) after rolling back its deletion: %s",
				entry.Key, entry.Domain, secretref.UnresolvedMessage("integration", entry.Key, resolveErr))
		}
		newEntryID, liveTitle, err := driveFlow(ctx, client, token, entry.Domain, data)
		if err != nil {
			return fmt.Errorf(
				"could not re-create integration '%s' (domain %s) after rolling back its deletion: %w",
				entry.Key, entry.Domain, redactedError(err, secretValues))
		}
		// Bookkeeping first, then the rename: the entry exists either way,
		// and recording it keeps the next reconcile from treating it as an
		// unmanaged stranger.
		managed[key] = newEntryID
		hashes[key] = flows.HashData(data)
		dataSnapshots[key] = entry.Data
		if renameErr := applyDeclaredTitle(ctx, dialer, newEntryID, entry.Title, liveTitle); renameErr != nil {
			return fmt.Errorf(
				"re-created integration '%s' (domain %s) as entry %s after rolling back its deletion, "+
					"but it is titled %q instead of %q: %w",
				entry.Key, entry.Domain, newEntryID, liveTitle, entry.Title, renameErr)
		}
		return nil
	}

	return fmt.Errorf("unreachable: unknown stash entry kind %q", entry.Kind)
}

// ApplyFlowPlan executes ops (as computed by flows.Plan) against Home
// Assistant Core's config-entry REST API, sequentially, over client
// (DefaultIntegrationHTTPClient if nil). dialer is used for one thing
// only, and only when a create needs it: writing the declared title onto
// the entry the flow just made (see applyDeclaredTitle).
//
// registries.KindError ops are never executed and never block the rest of
// the plan; they come back in RegistryApplyResult.SkippedErrors.
//
// # Per-op isolation, not mid-plan inverse-replay
//
// Unlike every other Apply*Plan here, one op failing never undoes a
// SIBLING that already succeeded. Other layers' ops cross-reference each
// other within a plan (an area references a floor) or share a live
// namespace an inverse-replay protects; integrations have neither - each
// item drives its own flow against its own domain. Undoing a good
// integration because an unrelated one is broken was a live-reproduced
// bug: the valid entry got created, deleted to "roll back" the broken
// sibling, and recreated every reconcile forever - a real config entry
// flapping on an interval.
//
// Every op is therefore always attempted, in order. A success is persisted
// exactly as before (write-after-every-confirmed-op); a failure is
// collected and execution moves on, giving OK=false with the failures
// joined into Error and RolledBack always false. The Rollback button is
// unaffected: it still targets exactly what integration_stash.json holds,
// via RollbackFlowPlan/integrationInverseReplayAndPersist below.
//
// managed (state.IntegrationManaged), hashes (state.IntegrationHashes),
// dataSnapshots (state.IntegrationData) and attempts
// (state.IntegrationAttempts, see executeFlowOp) are all mutated in place;
// the caller persists them afterward.
func ApplyFlowPlan(
	ctx context.Context, client IntegrationHTTPClient, dialer Dialer, ops []registries.RegOp,
	managed map[string]string, hashes map[string]string, dataSnapshots map[string]map[string]any,
	attempts map[string]map[string]any, stashDir string,
) RegistryApplyResult {
	return applyLayerPlan(ops, func(executable []registries.RegOp) RegistryApplyResult {
		return applyFlowPlanInner(ctx, client, dialer, executable, managed, hashes, dataSnapshots, attempts, stashDir)
	})
}

func applyFlowPlanInner(
	ctx context.Context, client IntegrationHTTPClient, dialer Dialer, executable []registries.RegOp,
	managed map[string]string, hashes map[string]string, dataSnapshots map[string]map[string]any,
	attempts map[string]map[string]any, stashDir string,
) (result RegistryApplyResult) {
	defer recoverToResult(&result, "regapply: apply_flow_plan failed")

	client = integrationClient(client)
	token, err := options.SupervisorToken()
	if err != nil {
		msg := fmt.Sprintf("unexpected failure: %v", err)
		slog.Warn("regapply: apply_flow_plan failed", "error", msg)
		return RegistryApplyResult{OK: false, Error: msg}
	}

	liveEntries, err := FetchIntegrationEntries(ctx, client)
	if err != nil {
		msg := fmt.Sprintf("unexpected failure: %v", err)
		slog.Warn("regapply: apply_flow_plan failed", "error", msg)
		return RegistryApplyResult{OK: false, Error: msg}
	}
	liveByEntryID := map[string]map[string]any{}
	for _, e := range liveEntries {
		if id, ok := e["entry_id"].(string); ok && id != "" {
			liveByEntryID[id] = e
		}
	}

	if err := writeIntegrationStash(stashDir, nil); err != nil {
		msg := fmt.Sprintf("unexpected failure: %v", err)
		slog.Warn("regapply: apply_flow_plan failed", "error", msg)
		return RegistryApplyResult{OK: false, Error: msg}
	}

	var executed []integrationStashEntry
	var failures []string
	for _, op := range executable {
		entry, warning, execErr := executeFlowOp(
			ctx, client, dialer, token, op, liveByEntryID, managed, hashes, dataSnapshots, attempts)
		if execErr != nil {
			// Per-op isolation (see ApplyFlowPlan): record the failure and
			// keep going, rather than undoing everything executed so far.
			failures = append(failures, fmt.Sprintf("%s integration:%s failed: %v", op.Kind, op.Key, execErr))
			continue
		}
		if entry.Kind == "" {
			// The op needed no live change (an already-absent delete), so
			// there is nothing to stash or invert.
			continue
		}
		executed = append(executed, entry)
		// A create whose title could not be written (see executeFlowOp)
		// stays applied, stashed and tracked - it just also gets reported,
		// since what is live no longer matches the manifest.
		if warning != "" {
			failures = append(failures, warning)
		}

		if err := writeIntegrationStash(stashDir, executed); err != nil {
			msg := fmt.Sprintf(
				"%d integration op(s) applied successfully, but the rollback journal could not be written after %s integration:%s, "+
					"so no further ops were attempted and these cannot be rolled back from disk: %v",
				len(executed), op.Kind, op.Key, err)
			slog.Warn("regapply: apply_flow_plan", "error", msg)
			return RegistryApplyResult{OK: false, Applied: appliedIntegrationLabels(executed), Error: msg}
		}
	}

	applied := appliedIntegrationLabels(executed)
	if len(failures) > 0 {
		errMsg := strings.Join(failures, "; ")
		slog.Warn("regapply: apply_flow_plan", "error", errMsg)
		return RegistryApplyResult{OK: false, Applied: applied, Error: errMsg}
	}

	slog.Info("regapply: apply_flow_plan executed", "applied", len(applied))
	return RegistryApplyResult{OK: true, Applied: applied}
}

func appliedIntegrationLabels(executed []integrationStashEntry) []string {
	out := make([]string, len(executed))
	for i, e := range executed {
		out[i] = fmt.Sprintf("%s integration:%s", e.Kind, e.Key)
	}
	return out
}

// RollbackFlowPlan undoes a previous ApplyFlowPlan call by replaying
// <stashDir>/integration_stash.json in reverse - the integration-layer
// counterpart of RollbackRegistry/RollbackAddonPlan, called alongside them
// by the same Rollback button. A missing stash file is not an error: most
// cycles have no integration ops, so ApplyFlowPlan never ran.
//
// secrets resolves the "secret://<name>" references a stashed delete's
// declared data still carries - the one path here that replays data out of
// storage rather than a freshly planned op. One resolver per rollback, so
// several deletes read the live secrets file once.
func RollbackFlowPlan(
	ctx context.Context, client IntegrationHTTPClient, dialer Dialer, stashDir string,
	managed map[string]string, hashes map[string]string, dataSnapshots map[string]map[string]any,
	secrets *secretref.Resolver,
) (result RegistryApplyResult) {
	defer recoverToResult(&result, "regapply: rollback_flow_plan")

	client = integrationClient(client)
	token, err := options.SupervisorToken()
	if err != nil {
		msg := fmt.Sprintf("unexpected failure: %v", err)
		slog.Warn("regapply: rollback_flow_plan failed", "error", msg)
		return RegistryApplyResult{OK: false, Error: msg}
	}

	executed, err := readIntegrationStash(stashDir)
	if err != nil {
		msg := fmt.Sprintf("cannot read integration rollback stash: %v", err)
		slog.Warn("regapply: rollback_flow_plan", "error", msg)
		return RegistryApplyResult{OK: false, Error: msg}
	}

	rolledBack, errMsg := integrationInverseReplayAndPersist(
		ctx, client, dialer, token, executed, managed, hashes, dataSnapshots, stashDir, secrets)
	if errMsg != "" {
		slog.Warn("regapply: rollback_flow_plan", "error", errMsg)
	} else {
		slog.Info("regapply: rollback_flow_plan undid ops", "count", len(executed))
	}
	return RegistryApplyResult{OK: rolledBack, RolledBack: rolledBack, Error: errMsg}
}

// integrationInverseReplayAndPersist best-effort inverts every entry in
// executed, in reverse order - the integration-layer counterpart of
// addonInverseReplayAndPersist, under the identical write-before-invert
// discipline (see that function for why).
func integrationInverseReplayAndPersist(
	ctx context.Context, client IntegrationHTTPClient, dialer Dialer, token string, executed []integrationStashEntry,
	managed map[string]string, hashes map[string]string, dataSnapshots map[string]map[string]any, stashDir string,
	secrets *secretref.Resolver,
) (rolledBack bool, errMsg string) {
	var failures []string
	outstanding := make([]int, len(executed))
	for i := range executed {
		outstanding[i] = i
	}

	for pos := len(executed) - 1; pos >= 0; pos-- {
		entry := executed[pos]
		label := fmt.Sprintf("%s integration:%s", entry.Kind, entry.Key)

		// Committed only after a successful write - see
		// addonInverseReplayAndPersist for why a plain truncation loses a
		// skipped entry from every later retry.
		shortenedIdx := removeInt(outstanding, pos)
		if err := writeIntegrationStash(stashDir, entriesFor(executed, shortenedIdx)); err != nil {
			failures = append(failures, fmt.Sprintf("%s: stash write failed: %v", label, err))
			continue
		}
		outstanding = shortenedIdx

		if err := invertFlowOp(ctx, client, dialer, token, entry, managed, hashes, dataSnapshots, secrets); err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", label, err))
		}
	}

	if len(failures) > 0 {
		return false, strings.Join(failures, "; ")
	}
	return true, ""
}

// integrationStashOpOnDisk is one entry's on-disk shape inside
// integration_stash.json.
type integrationStashOpOnDisk struct {
	Kind    string         `json:"kind"`
	Key     string         `json:"key"`
	Domain  string         `json:"domain"`
	Title   string         `json:"title"`
	EntryID string         `json:"entry_id"`
	Data    map[string]any `json:"data"`
}

type integrationStashFileOnDisk struct {
	Ops []integrationStashOpOnDisk `json:"ops"`
}

func toIntegrationStashOnDisk(executed []integrationStashEntry) []integrationStashOpOnDisk {
	out := make([]integrationStashOpOnDisk, len(executed))
	for i, e := range executed {
		out[i] = integrationStashOpOnDisk(e)
	}
	return out
}

func fromIntegrationStashOnDisk(disk []integrationStashOpOnDisk) []integrationStashEntry {
	out := make([]integrationStashEntry, len(disk))
	for i, d := range disk {
		out[i] = integrationStashEntry(d)
	}
	return out
}

// writeIntegrationStash atomically rewrites
// <stashDir>/integration_stash.json to hold exactly entries. A var, not a
// plain func, so tests can substitute a failing implementation - mirrors
// writeAddonStash/writeRegistryStash.
var writeIntegrationStash = writeIntegrationStashReal

func writeIntegrationStashReal(stashDir string, entries []integrationStashEntry) error {
	return writeStashFile(stashDir, integrationStashFile,
		integrationStashFileOnDisk{Ops: toIntegrationStashOnDisk(entries)})
}

// readIntegrationStash reads <stashDir>/integration_stash.json. A missing
// file returns (nil, nil), not an error, mirroring readAddonStash.
func readIntegrationStash(stashDir string) ([]integrationStashEntry, error) {
	decoded, found, err := readStashFile[integrationStashFileOnDisk](stashDir, integrationStashFile)
	if err != nil || !found {
		return nil, err
	}
	return fromIntegrationStashOnDisk(decoded.Ops), nil
}

// IntegrationStashExists reports whether stashDir holds an
// integration_stash.json - the integration-layer analogue of
// AddonStashExists.
func IntegrationStashExists(stashDir string) bool {
	return stashFileExists(stashDir, integrationStashFile)
}
