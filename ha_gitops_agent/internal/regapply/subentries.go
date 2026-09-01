// Package regapply's subentries file is internal/subentries' execution
// counterpart and flows.go's closest sibling, because a subentry is
// configured the same way an integration is: by driving a flow.
//
// Two transports, because that is what Core offers. The flows are REST-only
// (POST /api/config/config_entries/subentries/flow, DELETE to abandon),
// through the same doIntegrationRequest helper flows.go uses; listing a
// config entry's subentries has no REST route at all, only the WebSocket
// command config_entries/subentries/list - hence the Dialer here too.
//
// No stash file, unlike every other Apply*Plan: inverting an op would mean
// restoring the data the subentry held before it, and the list command
// never returns the data block. A create is not inverted either, per
// internal/subentries' rule 4 - deleting a subentry destroys its devices,
// entities and history. A bad reconfigure is corrected by fixing the
// manifest and reconciling again.
package regapply

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/options"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/registries"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/subentries"
)

// msgSubentriesList is the WebSocket command that lists one config entry's
// subentries - the only API Home Assistant has for it.
const msgSubentriesList = "config_entries/subentries/list"

// subentryFlowPath is the REST route subentry flows are driven over,
// relative to coreAPI's own /core/api prefix.
const subentryFlowPath = "/config/config_entries/subentries/flow"

// reconfigureSuccessReason is the abort reason a successful subentry
// RECONFIGURE terminates with. Not an integration quirk: core's own
// async_update_and_abort writes the subentry and aborts with it, and a
// reconfigure has no create_entry terminal at all.
const reconfigureSuccessReason = "reconfigure_successful"

// Step-1 aliasing: a create's first step is called "user" and a
// reconfigure's "reconfigure" for the same form, so data declared under
// either id answers whichever the live flow presents. An exact match wins.
const (
	stepIDUser        = "user"
	stepIDReconfigure = "reconfigure"
)

// expandableFieldType marks a serialized form section: a field whose value
// is a nested map of the fields under its own "schema". A section MUST be
// submitted nested - flattening it to the top level is rejected.
const expandableFieldType = "expandable"

// FetchSubentries fetches the live subentries of every config entry in
// entryIDs, keyed by entry_id, over one dialed connection.
//
// Verified against core: the command returns a bare array of objects
// carrying "subentry_id", "subentry_type", "title" and "unique_id" - never
// "data". An empty entryIDs dials nothing, since most cycles declare no
// subentries and this runs before the plan is known.
func FetchSubentries(ctx context.Context, dialer Dialer, entryIDs []string) (map[string][]map[string]any, error) {
	out := map[string][]map[string]any{}
	if len(entryIDs) == 0 {
		return out, nil
	}
	if dialer == nil {
		return nil, errors.New("no websocket dialer was configured for this call")
	}

	ws, err := dialer(ctx)
	if err != nil {
		return nil, err
	}
	defer ws.Close()

	for _, entryID := range entryIDs {
		if entryID == "" {
			continue
		}
		result, err := ws.Cmd(ctx, msgSubentriesList, map[string]any{"entry_id": entryID})
		if err != nil {
			return nil, fmt.Errorf("could not list subentries of config entry %s: %w", entryID, err)
		}
		out[entryID] = toObjectList(result)
	}
	return out, nil
}

// schemaDefaults reads what a step would submit if a human just pressed
// Submit: per field, description.suggested_value, else default, else
// nothing.
//
// On a RECONFIGURE those are the subentry's CURRENT live values, which is
// as close as this layer gets to reading its data back - and closer than
// the stored data, since a form's shape differs (a hex colour is suggested
// as the [R, G, B] triple the selector accepts, and only that round-trips).
// It is why a partial data block can edit one field and leave the rest.
//
// An "expandable" field is a SECTION: its schema is walked recursively into
// a NESTED map, since a section must be submitted nested. A section that
// yields nothing is omitted, like a scalar with no default. The schema is
// untrusted throughout - an unusable entry contributes nothing.
func schemaDefaults(rawSchema []any) map[string]any {
	out := map[string]any{}
	for _, entry := range rawSchema {
		field, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		name, _ := field["name"].(string)
		if name == "" {
			continue
		}

		if kind, _ := field["type"].(string); kind == expandableFieldType {
			nested, _ := field["schema"].([]any)
			if section := schemaDefaults(nested); len(section) > 0 {
				out[name] = section
			}
			continue
		}
		if value, ok := schemaFieldDefault(field); ok {
			out[name] = value
		}
	}
	return out
}

// schemaFieldDefault returns one non-section field's pre-filled value:
// description.suggested_value (what the dialog shows), then "default". An
// explicit null is treated as absent - the manifest must ask for null.
func schemaFieldDefault(field map[string]any) (any, bool) {
	if description, ok := field["description"].(map[string]any); ok {
		if suggested, present := description["suggested_value"]; present && suggested != nil {
			return suggested, true
		}
	}
	if value, present := field["default"]; present && value != nil {
		return value, true
	}
	return nil, false
}

// buildStepSubmission builds one form step's POST body: schemaDefaults with
// the manifest's declared fields laid over them.
//
// A declared field replaces its default wholesale - [1, 2] submits [1, 2],
// never merged into a longer default list. The exception is a SECTION,
// whose declared map merges one level into the default map, so declaring
// one field inside it keeps the section's other live values. Sections do
// not nest, so one level is the whole depth.
//
// A required field with neither a default nor a declaration is an error
// naming it, listing the step's whole schema - which makes "declare nothing
// and read the error" a working way to discover what a subentry type wants.
func buildStepSubmission(rawSchema []any, declared map[string]any) (map[string]any, error) {
	submission := schemaDefaults(rawSchema)
	for name, value := range declared {
		existing, isSection := submission[name].(map[string]any)
		override, overridesSection := value.(map[string]any)
		if isSection && overridesSection {
			merged := make(map[string]any, len(existing)+len(override))
			for k, v := range existing {
				merged[k] = v
			}
			for k, v := range override {
				merged[k] = v
			}
			submission[name] = merged
			continue
		}
		submission[name] = value
	}

	if missing := missingRequiredFields(rawSchema, submission); len(missing) > 0 {
		problem := fmt.Sprintf("no value for required field '%s'", missing[0])
		if len(missing) > 1 {
			problem = fmt.Sprintf("no value for required fields '%s'", strings.Join(missing, "', '"))
		}
		if described := describeStepSchema(rawSchema); described != "" {
			problem += fmt.Sprintf(" (the step accepts: %s)", described)
		}
		return nil, errors.New(problem)
	}
	return submission, nil
}

// missingRequiredFields lists, in schema order, every required field the
// submission has no value for, naming a section's own as "<section>.<field>"
// so the error points where the manifest must declare it. Only explicitly
// required fields count - a step Core would accept must never be refused.
func missingRequiredFields(rawSchema []any, submission map[string]any) []string {
	var missing []string
	for _, entry := range rawSchema {
		field, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		name, _ := field["name"].(string)
		if name == "" {
			continue
		}
		required, _ := field["required"].(bool)

		if kind, _ := field["type"].(string); kind == expandableFieldType {
			nested, isList := field["schema"].([]any)
			section, hasSection := submission[name].(map[string]any)
			if !hasSection {
				if required {
					missing = append(missing, name)
				}
				continue
			}
			if !isList {
				continue
			}
			for _, inner := range missingRequiredFields(nested, section) {
				missing = append(missing, name+"."+inner)
			}
			continue
		}

		if _, present := submission[name]; !present && required {
			missing = append(missing, name)
		}
	}
	return missing
}

// startSubentryFlow starts a subentry flow, body {"handler": [entry_id,
// subentry_type]} - a PAIR here, not the bare domain a config-entry flow
// takes, since a subentry flow is scoped to its config entry.
//
// subentryID alone selects the source: passing it makes the flow a
// RECONFIGURE of that subentry, omitting it a CREATE. There is no separate
// endpoint or source field.
func startSubentryFlow(
	ctx context.Context, client IntegrationHTTPClient, token, entryID, subentryType, subentryID string,
) (map[string]any, error) {
	body := map[string]any{"handler": []any{entryID, subentryType}}
	if subentryID != "" {
		body["subentry_id"] = subentryID
	}
	var result map[string]any
	if _, err := doIntegrationRequest(
		ctx, client, token, http.MethodPost, subentryFlowPath, body, integrationFlowStepTimeout, &result,
	); err != nil {
		return nil, fmt.Errorf("failed to start '%s' subentry flow on config entry %s: %w", subentryType, entryID, err)
	}
	return result, nil
}

// advanceSubentryFlow answers one form step, the body passed through as
// that step's user_input - advanceFlow's shape exactly, including scrubbing
// the step's own submitted secrets out of a failure (redactStepSecrets).
func advanceSubentryFlow(
	ctx context.Context, client IntegrationHTTPClient, token, flowID string, stepData map[string]any,
) (map[string]any, error) {
	if stepData == nil {
		stepData = map[string]any{}
	}
	var result map[string]any
	if _, err := doIntegrationRequest(
		ctx, client, token, http.MethodPost, subentryFlowPath+"/"+flowID, stepData, integrationFlowStepTimeout, &result,
	); err != nil {
		return nil, errors.New(redactStepSecrets(err.Error(), stepData))
	}
	return result, nil
}

// abortSubentryFlowBestEffort cancels an in-progress subentry flow and only
// logs a failure, for abortFlowBestEffort's reasons: the caller is already
// returning the primary failure, and Core garbage-collects abandoned flows
// anyway. A 404 is the end state this call exists to reach.
func abortSubentryFlowBestEffort(ctx context.Context, client IntegrationHTTPClient, token, flowID, label string) {
	if flowID == "" {
		return
	}
	status, err := doIntegrationRequest(
		ctx, client, token, http.MethodDelete, subentryFlowPath+"/"+flowID, nil, integrationFlowAbortTimeout, nil)
	if err != nil && status != http.StatusNotFound {
		slog.Warn("regapply: could not abort in-progress subentry flow", "subentry", label, "flow_id", flowID, "error", err)
	}
}

// stepIDAlias returns the other conventional name for a step-1 form -
// "user" on a create, "reconfigure" on a reconfigure - or "" for any other
// step id (see internal/subentries' package doc comment).
func stepIDAlias(stepID string) string {
	switch stepID {
	case stepIDUser:
		return stepIDReconfigure
	case stepIDReconfigure:
		return stepIDUser
	}
	return ""
}

// subentryStepData looks up stepID's declared fields in data, falling back
// to the step's alias. An exact key wins even when unusable: substituting
// the alias would submit values written for a different form.
func subentryStepData(data map[string]any, stepID string) map[string]any {
	if _, present := data[stepID]; present {
		fields, _ := stepDataFor(data, stepID)
		return fields
	}
	if alias := stepIDAlias(stepID); alias != "" {
		fields, _ := stepDataFor(data, alias)
		return fields
	}
	return nil
}

// driveSubentryFlow drives one subentry flow to completion, answering every
// form step from data (keyed by step id) merged over that step's defaults.
// An empty subentryID drives a CREATE, a non-empty one a RECONFIGURE;
// nothing else about the call differs.
//
// The two have different success terminals: a create ends with
// "create_entry", a reconfigure only ever with an abort carrying
// "reconfigure_successful". Mismatched pairs are refused rather than
// guessed at - a reconfigure reporting "create_entry" has made a second
// subentry beside the one it was asked to edit.
//
// Only plain form steps are handled, and every non-terminal exit DELETEs
// the flow best-effort, so this never leaves a flow it started open.
func driveSubentryFlow(
	ctx context.Context, client IntegrationHTTPClient, token, entryID, subentryType, subentryID string,
	data map[string]any,
) error {
	if data == nil {
		data = map[string]any{}
	}
	reconfiguring := subentryID != ""
	label := fmt.Sprintf("'%s' subentry on config entry %s", subentryType, entryID)
	if reconfiguring {
		label = fmt.Sprintf("subentry %s ('%s')", subentryID, subentryType)
	}

	result, err := startSubentryFlow(ctx, client, token, entryID, subentryType, subentryID)
	if err != nil {
		return err
	}
	flowID, _ := result["flow_id"].(string)

	// What the previous iteration submitted, for scrubbing a rejection: on
	// a reconfigure the submission merges the manifest over the step's
	// LIVE defaults, credentials included, and a validator can quote any
	// of it back in "errors".
	var lastSubmission map[string]any
	for steps := 0; ; {
		typ, _ := result["type"].(string)
		switch typ {
		case "create_entry":
			if reconfiguring {
				return fmt.Errorf(
					"%s: the reconfigure flow ended by CREATING a subentry instead of updating this one, "+
						"so home assistant may now hold a duplicate - check it in the UI", label)
			}
			return nil

		case "abort":
			reason, _ := result["reason"].(string)
			if reconfiguring && reason == reconfigureSuccessReason {
				return nil
			}
			return fmt.Errorf("%s: home assistant aborted the flow: %s", label, reason)

		case "form":
			stepID, _ := result["step_id"].(string)
			if errs, ok := result["errors"].(map[string]any); ok && len(errs) > 0 {
				abortSubentryFlowBestEffort(ctx, client, token, flowID, label)
				return fmt.Errorf("%s: step '%s' rejected the submitted data: %s",
					label, stepID, redactStepSecrets(fmt.Sprintf("%v", errs), lastSubmission))
			}

			rawSchema, _ := result["data_schema"].([]any)
			submission, buildErr := buildStepSubmission(rawSchema, subentryStepData(data, stepID))
			if buildErr != nil {
				abortSubentryFlowBestEffort(ctx, client, token, flowID, label)
				return fmt.Errorf(
					"%s: flow step '%s' cannot be answered - %v; declare it under data.%s in the manifest",
					label, stepID, buildErr, stepID)
			}

			steps++
			if steps > maxFlowSteps {
				abortSubentryFlowBestEffort(ctx, client, token, flowID, label)
				return fmt.Errorf("%s: flow exceeded %d steps without completing", label, maxFlowSteps)
			}

			lastSubmission = submission
			next, advErr := advanceSubentryFlow(ctx, client, token, flowID, submission)
			if advErr != nil {
				abortSubentryFlowBestEffort(ctx, client, token, flowID, label)
				return fmt.Errorf("%s: step '%s': %w", label, stepID, advErr)
			}
			result = next

		default:
			stepID, _ := result["step_id"].(string)
			abortSubentryFlowBestEffort(ctx, client, token, flowID, label)
			return fmt.Errorf(
				"%s: flow step '%s' has type %q, which this layer does not support "+
					"(only plain form steps are handled - no menu, external-auth, reauth or progress flows)",
				label, stepID, typ)
		}
	}
}

// subentryStateKey namespaces one manifest id inside the shared state maps
// - internal/subentries' own keyPrefix, as flows.go writes "integration:".
func subentryStateKey(id string) string { return "subentry:" + id }

// listSubentriesOf reads one config entry's live subentries over a
// freshly dialed connection. Used either side of a create, which is the
// only way to learn the id of what a subentry flow just made.
func listSubentriesOf(ctx context.Context, dialer Dialer, entryID string) ([]map[string]any, error) {
	byEntry, err := FetchSubentries(ctx, dialer, []string{entryID})
	if err != nil {
		return nil, err
	}
	return byEntry[entryID], nil
}

// discoverCreatedSubentry finds the subentry a create flow just added, by
// diffing the parent entry's list from before against now. Necessary
// because core's _prepare_config_flow_result_json STRIPS the result off a
// subentry flow's response, so the new subentry_id is simply not in it.
//
// Among the new subentries of subentryType: the one matching the declared
// unique_id, else the declared title, else the only one. Anything else is
// an error - a subentry missing from managed is created again next cycle.
func discoverCreatedSubentry(before, after []map[string]any, subentryType, matchUniqueID, matchTitle string) (string, error) {
	existing := map[string]bool{}
	for _, sub := range before {
		if id, _ := sub["subentry_id"].(string); id != "" {
			existing[id] = true
		}
	}

	var added []map[string]any
	for _, sub := range after {
		id, _ := sub["subentry_id"].(string)
		typ, _ := sub["subentry_type"].(string)
		if id == "" || existing[id] || typ != subentryType {
			continue
		}
		added = append(added, sub)
	}

	if matchUniqueID != "" {
		if id, ok := onlySubentryWith(added, "unique_id", matchUniqueID); ok {
			return id, nil
		}
	}
	if matchTitle != "" {
		if id, ok := onlySubentryWith(added, "title", matchTitle); ok {
			return id, nil
		}
	}
	if len(added) == 1 {
		id, _ := added[0]["subentry_id"].(string)
		return id, nil
	}
	return "", fmt.Errorf(
		"the flow reported success but the new subentry could not be identified afterwards "+
			"(%d new '%s' subentries appeared under this config entry); it exists in home assistant but is not tracked here, "+
			"so remove or rename it in the UI before the next reconcile creates another",
		len(added), subentryType)
}

// onlySubentryWith returns the id of the single candidate whose field
// equals value. Several matches is not a match - picking one would tie the
// manifest id to a coin flip.
func onlySubentryWith(candidates []map[string]any, field, value string) (string, bool) {
	var found string
	for _, sub := range candidates {
		if actual, _ := sub[field].(string); actual != value {
			continue
		}
		if found != "" {
			return "", false
		}
		found, _ = sub["subentry_id"].(string)
	}
	return found, found != ""
}

// executeSubentryOp executes a single create / reconfigure / unmanage op,
// mutating state.SubentryManaged, -Hashes and -Attempts in place for the
// caller to persist.
//
// Failure memory, the rule subentries.Plan reads back: a failed create or
// reconfigure records the data's hash plus a reason into attempts so the
// next plan refuses to re-drive a doomed flow, and a success clears it.
// Unlike internal/flows this covers UPDATES, since a rejected reconfigure
// would otherwise hammer a live subentry every interval.
//
// A create whose flow SUCCEEDED but whose subentry could not be identified
// is recorded too, and matters most: retrying it would add a second
// subentry beside the untracked one, every cycle, forever.
func executeSubentryOp(
	ctx context.Context, client IntegrationHTTPClient, dialer Dialer, token string, op registries.RegOp,
	managed map[string]string, hashes map[string]string, attempts map[string]map[string]any,
) error {
	key := subentryStateKey(op.Key)

	// Unmanage is bookkeeping only - no flow, no HTTP, no WebSocket. The
	// live subentry is left as it is; this layer never deletes one (rule 4).
	if unmanage, _ := op.Params["unmanage"].(bool); unmanage {
		delete(managed, key)
		delete(hashes, key)
		delete(attempts, key)
		return nil
	}

	entryID, _ := op.Params["entry_id"].(string)
	subentryType, _ := op.Params["subentry_type"].(string)
	data, _ := op.Params["data"].(map[string]any)

	switch op.Kind {
	case subentries.KindCreate:
		before, err := listSubentriesOf(ctx, dialer, entryID)
		if err != nil {
			// Nothing driven yet, so this is a transient read failure, not
			// the manifest's fault - deliberately NOT remembered in attempts,
			// which would strand the item behind a manifest edit.
			return err
		}

		// redactedError, not the raw failure: op.Secrets holds what this
		// item's "secret://" references resolved to, and this text lands in
		// attempts on disk and in the activity feed.
		if flowErr := redactedError(
			driveSubentryFlow(ctx, client, token, entryID, subentryType, "", data), op.Secrets); flowErr != nil {
			attempts[key] = map[string]any{"hash": subentries.HashData(data), "error": flowErr.Error()}
			return flowErr
		}

		// Both failures below record "created": true - the flow completed,
		// only identifying its product failed - so subentries.Plan can let
		// a later adopt-by-match through as the recovery instead of the
		// failure memory stranding the untracked subentry forever.
		after, err := listSubentriesOf(ctx, dialer, entryID)
		if err != nil {
			attempts[key] = map[string]any{"hash": subentries.HashData(data), "error": err.Error(), "created": true}
			return fmt.Errorf("created the subentry, but could not list the config entry afterwards to find it: %w", err)
		}
		matchUniqueID, _ := op.Params["match_unique_id"].(string)
		matchTitle, _ := op.Params["match_title"].(string)
		subentryID, err := discoverCreatedSubentry(before, after, subentryType, matchUniqueID, matchTitle)
		if err != nil {
			attempts[key] = map[string]any{"hash": subentries.HashData(data), "error": err.Error(), "created": true}
			return err
		}

		delete(attempts, key)
		managed[key] = subentryID
		hashes[key] = subentries.HashData(data)
		return nil

	case subentries.KindUpdate:
		subentryID, _ := op.Params["subentry_id"].(string)
		if subentryID == "" {
			// driveSubentryFlow reads an empty subentry_id as "create", so an
			// update without one would quietly make a SECOND subentry and
			// report success - the one outcome this layer cannot undo. The
			// plan layer guards both paths, so reaching here is a bug.
			err := fmt.Errorf("refusing to reconfigure: the planned op carries no subentry_id")
			attempts[key] = map[string]any{"hash": subentries.HashData(data), "error": err.Error()}
			return err
		}
		if flowErr := redactedError(
			driveSubentryFlow(ctx, client, token, entryID, subentryType, subentryID, data), op.Secrets); flowErr != nil {
			attempts[key] = map[string]any{"hash": subentries.HashData(data), "error": flowErr.Error()}
			return flowErr
		}
		// An adopt is this same op against a subentry the manifest did not
		// create, so writing managed here is what puts it under management.
		delete(attempts, key)
		managed[key] = subentryID
		hashes[key] = subentries.HashData(data)
		return nil
	}

	return fmt.Errorf("unreachable: unknown op kind %q", op.Kind)
}

// ApplySubentryPlan executes ops (from subentries.Plan) over client
// (DefaultIntegrationHTTPClient if nil) for the flows and dialer for the
// listings a create needs either side of itself. KindError ops come back in
// SkippedErrors.
//
// Per-op isolation like ApplyFlowPlan: declared subentries are independent,
// so every op is attempted regardless of what came before and failures are
// joined into Error. This layer goes further - no stash file and no inverse
// at all, since the data a reconfigure overwrote is unreadable, so
// RolledBack is always false.
//
// state.SubentryManaged, -Hashes and -Attempts are mutated in place; the
// caller persists them.
func ApplySubentryPlan(
	ctx context.Context, client IntegrationHTTPClient, dialer Dialer, ops []registries.RegOp,
	managed map[string]string, hashes map[string]string, attempts map[string]map[string]any,
) RegistryApplyResult {
	return applyLayerPlan(ops, func(executable []registries.RegOp) RegistryApplyResult {
		return applySubentryPlanInner(ctx, client, dialer, executable, managed, hashes, attempts)
	})
}

func applySubentryPlanInner(
	ctx context.Context, client IntegrationHTTPClient, dialer Dialer, executable []registries.RegOp,
	managed map[string]string, hashes map[string]string, attempts map[string]map[string]any,
) (result RegistryApplyResult) {
	defer recoverToResult(&result, "regapply: apply_subentry_plan failed")

	client = integrationClient(client)
	token, err := options.SupervisorToken()
	if err != nil {
		msg := fmt.Sprintf("unexpected failure: %v", err)
		slog.Warn("regapply: apply_subentry_plan failed", "error", msg)
		return RegistryApplyResult{OK: false, Error: msg}
	}

	var applied, failures []string
	for _, op := range executable {
		if execErr := executeSubentryOp(ctx, client, dialer, token, op, managed, hashes, attempts); execErr != nil {
			failures = append(failures, fmt.Sprintf("%s subentry:%s failed: %v", op.Kind, op.Key, execErr))
			continue
		}
		applied = append(applied, fmt.Sprintf("%s subentry:%s", op.Kind, op.Key))
	}

	if len(failures) > 0 {
		errMsg := strings.Join(failures, "; ")
		slog.Warn("regapply: apply_subentry_plan", "error", errMsg)
		return RegistryApplyResult{OK: false, Applied: applied, Error: errMsg}
	}

	slog.Info("regapply: apply_subentry_plan executed", "applied", len(applied))
	return RegistryApplyResult{OK: true, Applied: applied}
}
