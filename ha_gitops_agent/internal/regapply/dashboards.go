package regapply

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/registries"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/wsclient"
)

// dashboardStashPersistFailure marks an error as: the WS call succeeded
// live and dashboardManaged reflects it, but writing that to the rollback
// stash failed - so the change is real and outside
// inverseReplayAndPersist's reach. applyDashboardPlanInner detects it via
// errors.As and forces RolledBack false. Ordinary reconciliation still
// fixes the object on the next cycle.
type dashboardStashPersistFailure struct{ err error }

func (e *dashboardStashPersistFailure) Error() string { return e.err.Error() }

// appendDashboardStashEntry appends entry to *executed and persists
// preExisting+*executed, mutating *executed only once the write succeeds so
// memory and disk cannot disagree.
//
// Also used to record a two-call op's FIRST call before its second is
// attempted (B5): if the second fails, that persisted entry is what
// inverseReplayAndPersist needs. No same-connection compensation is
// attempted here - the failure that killed the second call usually killed
// the connection too.
func appendDashboardStashEntry(stashDir string, preExisting []stashEntry, executed *[]stashEntry, entry stashEntry) error {
	candidate := append(append([]stashEntry(nil), *executed...), entry)
	toWrite := append(append([]stashEntry(nil), preExisting...), candidate...)
	if err := writeRegistryStash(stashDir, toWrite); err != nil {
		return &dashboardStashPersistFailure{err: fmt.Errorf("%s dashboard:%s: rollback journal write failed: %w", entry.Kind, entry.Key, err)}
	}
	*executed = candidate
	return nil
}

// replaceLastDashboardStashEntry replaces *executed's last entry and
// persists the result, so a two-call op's second half extends its first
// half's entry rather than adding a second one. Falls back to an append on
// an empty *executed (defensive; no caller here reaches it).
func replaceLastDashboardStashEntry(stashDir string, preExisting []stashEntry, executed *[]stashEntry, entry stashEntry) error {
	if len(*executed) == 0 {
		return appendDashboardStashEntry(stashDir, preExisting, executed, entry)
	}
	candidate := append([]stashEntry(nil), *executed...)
	candidate[len(candidate)-1] = entry
	toWrite := append(append([]stashEntry(nil), preExisting...), candidate...)
	if err := writeRegistryStash(stashDir, toWrite); err != nil {
		return &dashboardStashPersistFailure{err: fmt.Errorf("%s dashboard:%s: rollback journal write failed: %w", entry.Kind, entry.Key, err)}
	}
	*executed = candidate
	return nil
}

// Dashboard metadata lives in a DictStorageCollectionWebsocket at
// "lovelace/dashboards", where an update/delete names its item with
// "dashboard_id", never "id". Full verified shape (and why create always
// sends allow_single_word) is in dashboards.go's package doc comment.
const (
	msgLovelaceDashboardsList   = "lovelace/dashboards/list"
	msgLovelaceDashboardsCreate = "lovelace/dashboards/create"
	msgLovelaceDashboardsUpdate = "lovelace/dashboards/update"
	msgLovelaceDashboardsDelete = "lovelace/dashboards/delete"
	msgLovelaceConfig           = "lovelace/config"
	msgLovelaceConfigSave       = "lovelace/config/save"
)

// fetchLiveDashboardList fetches every dashboard's metadata via
// lovelace/dashboards/list.
func fetchLiveDashboardList(ctx context.Context, ws WSClient) ([]map[string]any, error) {
	result, err := ws.Cmd(ctx, msgLovelaceDashboardsList, nil)
	if err != nil {
		return nil, err
	}
	return toObjectList(result), nil
}

// fetchLiveDashboardContent fetches saved content for every url_path in
// ids, one lovelace/config call each (there is no bulk equivalent). A
// "config_not_found" error records a nil entry - dashboards.Plan's
// liveContent reads that as "no content saved yet". Any other error aborts.
func fetchLiveDashboardContent(ctx context.Context, ws WSClient, ids []string) (map[string]map[string]any, error) {
	content := make(map[string]map[string]any, len(ids))
	for _, id := range ids {
		cfg, err := fetchOneDashboardContent(ctx, ws, id)
		if err != nil {
			return nil, err
		}
		content[id] = cfg
	}
	return content, nil
}

func fetchOneDashboardContent(ctx context.Context, ws WSClient, urlPath string) (map[string]any, error) {
	result, err := ws.Cmd(ctx, msgLovelaceConfig, map[string]any{"url_path": urlPath, "force": false})
	if err != nil {
		var wsErr *wsclient.Error
		if errors.As(err, &wsErr) && wsErr.Code == "config_not_found" {
			return nil, nil
		}
		return nil, err
	}
	cfg, _ := result.(map[string]any)
	return cfg, nil
}

// FetchLiveDashboards fetches every live dashboard's metadata plus the
// saved content of each id in ids (nil if none was ever saved). ids should
// be every declared dashboard whose config file loaded; one that already
// hit a per-item error at plan time needs no content fetch.
func FetchLiveDashboards(ctx context.Context, ws WSClient, ids []string) ([]map[string]any, map[string]map[string]any, error) {
	dashboards, err := fetchLiveDashboardList(ctx, ws)
	if err != nil {
		return nil, nil, err
	}
	content, err := fetchLiveDashboardContent(ctx, ws, ids)
	if err != nil {
		return nil, nil, err
	}
	return dashboards, content, nil
}

// ApplyDashboardPlan executes ops (from dashboards.Plan) against Lovelace
// dashboards, over one connection dialed from dialer. A third sibling of
// ApplyPlan/ApplyEntityPlan: same independent dial and fetch, same refusal
// to undo an earlier layer, same shared registry_stash.json.
// dashboardManaged is state.DashboardManaged, mutated in place.
func ApplyDashboardPlan(
	ctx context.Context, dialer Dialer, ops []registries.RegOp, dashboardManaged map[string]string, stashDir string,
) RegistryApplyResult {
	return applyLayerPlan(ops, func(executable []registries.RegOp) RegistryApplyResult {
		return applyDashboardPlanInner(ctx, dialer, executable, dashboardManaged, stashDir)
	})
}

func applyDashboardPlanInner(
	ctx context.Context, dialer Dialer, executable []registries.RegOp, dashboardManaged map[string]string, stashDir string,
) (result RegistryApplyResult) {
	defer recoverToResult(&result, "regapply: apply_dashboard_plan failed")

	preExisting, err := readRegistryStashTolerant(stashDir)
	if err != nil {
		msg := fmt.Sprintf("unexpected failure: %v", err)
		slog.Warn("regapply: apply_dashboard_plan failed", "error", msg)
		return RegistryApplyResult{OK: false, Error: msg}
	}

	ws, err := dialer(ctx)
	if err != nil {
		msg := fmt.Sprintf("failed to connect: %v", err)
		slog.Warn("regapply: apply_dashboard_plan failed", "error", msg)
		return RegistryApplyResult{OK: false, Error: msg}
	}
	defer ws.Close()

	liveDashboards, err := fetchLiveDashboardList(ctx, ws)
	if err != nil {
		msg := fmt.Sprintf("unexpected failure: %v", err)
		slog.Warn("regapply: apply_dashboard_plan failed", "error", msg)
		return RegistryApplyResult{OK: false, Error: msg}
	}
	liveByLiveID := make(map[string]map[string]any, len(liveDashboards))
	for _, obj := range liveDashboards {
		if id, ok := obj["id"].(string); ok && id != "" {
			liveByLiveID[id] = obj
		}
	}

	liveContent, err := fetchLiveDashboardContent(ctx, ws, dashboardContentIDsNeeded(executable))
	if err != nil {
		msg := fmt.Sprintf("unexpected failure: %v", err)
		slog.Warn("regapply: apply_dashboard_plan failed", "error", msg)
		return RegistryApplyResult{OK: false, Error: msg}
	}

	var executed []stashEntry
	for _, op := range executable {
		execErr := executeDashboardOp(ctx, ws, op, liveByLiveID, liveContent, dashboardManaged, stashDir, preExisting, &executed)
		if execErr == nil {
			continue
		}

		var persistFail *dashboardStashPersistFailure
		if errors.As(execErr, &persistFail) {
			// This op's live change is known to be unrecorded and so
			// un-invertible, and inverseReplayAndPersist cannot see the
			// entry at all, so RolledBack is forced false here.
			msg := fmt.Sprintf(
				"%d dashboard op(s) applied successfully, but %s dashboard:%s could not be recorded for rollback: %v",
				len(executed), op.Kind, op.Key, execErr)
			slog.Warn("regapply: apply_dashboard_plan", "error", msg)
			return RegistryApplyResult{OK: false, Applied: appliedLabels(executed), Error: msg, RolledBack: false}
		}

		replayConn := ws
		if isTransportOrTimeoutError(execErr) {
			replayConn = nil
		}
		rolledBack, undoErr := inverseReplayAndPersist(
			ctx, replayConn, dialer, executed, map[string]string{}, nil, dashboardManaged, stashDir, preExisting)
		errMsg := fmt.Sprintf("%s dashboard:%s failed: %v", op.Kind, op.Key, execErr)
		if undoErr != "" {
			errMsg = fmt.Sprintf("%s; rollback also incomplete: %s", errMsg, undoErr)
		}
		slog.Warn("regapply: apply_dashboard_plan", "error", errMsg)
		return RegistryApplyResult{OK: false, Error: errMsg, RolledBack: rolledBack}
	}

	applied := appliedLabels(executed)
	slog.Info("regapply: apply_dashboard_plan executed", "applied", len(applied))
	return RegistryApplyResult{OK: true, Applied: applied}
}

// dashboardContentIDsNeeded returns the url_paths needing fresh content at
// apply time, purely to stash an accurate PriorObject - what to change was
// decided at plan time. A create needs none, an update only when its Params
// carries "content", a delete always (to restore on invert).
func dashboardContentIDsNeeded(ops []registries.RegOp) []string {
	var ids []string
	for _, op := range ops {
		switch op.Kind {
		case registries.KindUpdate:
			if _, ok := op.Params["content"]; ok {
				ids = append(ids, op.Key)
			}
		case registries.KindDelete:
			ids = append(ids, op.Key)
		}
	}
	return ids
}

// executeDashboardOp executes a single dashboard create/update/delete op,
// persisting into *executed/stashDir as it goes rather than handing a
// record back: a two-call op must record its first call before attempting
// its second, so every op kind persists through the same two helpers.
// Mutates dashboardManaged in place, like executeOne does managed.
func executeDashboardOp(
	ctx context.Context, ws WSClient, op registries.RegOp,
	liveByLiveID map[string]map[string]any, liveContent map[string]map[string]any, dashboardManaged map[string]string,
	stashDir string, preExisting []stashEntry, executed *[]stashEntry,
) error {
	switch op.Kind {
	case registries.KindCreate:
		return executeDashboardCreate(ctx, ws, op, dashboardManaged, stashDir, preExisting, executed)
	case registries.KindUpdate:
		return executeDashboardUpdate(ctx, ws, op, liveByLiveID[op.LiveID], liveContent[op.Key], dashboardManaged, stashDir, preExisting, executed)
	case registries.KindDelete:
		return executeDashboardDelete(ctx, ws, op, liveByLiveID[op.LiveID], liveContent[op.Key], dashboardManaged, stashDir, preExisting, executed)
	}
	return fmt.Errorf("unreachable: unknown op kind %q", op.Kind)
}

// executeDashboardCreate performs the two WS calls a create needs -
// lovelace/dashboards/create for metadata, then lovelace/config/save for
// initial content when there is any.
//
// The metadata half is persisted BEFORE the content call is attempted (B5):
// otherwise a failed content save leaves a real dashboard with no stash
// entry for any rollback to find. A failed content call returns as-is -
// invertDashboardOp deletes regardless of whether content was saved.
func executeDashboardCreate(
	ctx context.Context, ws WSClient, op registries.RegOp, dashboardManaged map[string]string,
	stashDir string, preExisting []stashEntry, executed *[]stashEntry,
) error {
	metadata, _ := op.Params["metadata"].(map[string]any)
	createParams := map[string]any{
		"url_path":          op.Key,
		"allow_single_word": true,
		"title":             metadata["title"],
		"show_in_sidebar":   metadata["show_in_sidebar"],
	}
	if v, ok := metadata["icon"]; ok && v != nil {
		createParams["icon"] = v
	}

	result, err := ws.Cmd(ctx, msgLovelaceDashboardsCreate, createParams)
	if err != nil {
		return err
	}
	resultMap, _ := result.(map[string]any)
	newID, _ := resultMap["id"].(string)
	if newID == "" {
		return fmt.Errorf("create of dashboard:%s did not return a live id", op.Key)
	}

	fullKey := "dashboard:" + op.Key
	dashboardManaged[fullKey] = newID
	entry := stashEntry{Kind: registries.KindCreate, RType: "dashboard", Key: op.Key, LiveID: newID}
	if err := appendDashboardStashEntry(stashDir, preExisting, executed, entry); err != nil {
		return err
	}

	if content, ok := op.Params["content"]; ok {
		if _, err := ws.Cmd(ctx, msgLovelaceConfigSave, map[string]any{"url_path": op.Key, "config": content}); err != nil {
			return err
		}
	}
	return nil
}

// executeDashboardUpdate performs whichever of the two WS calls op.Params
// asks for - dashboards/update for metadata, config/save for content,
// either or both - recording each touched axis's pre-op value so
// invertDashboardOp restores exactly what this op changed.
//
// With both axes, the metadata half is persisted before the content call
// and extended once it succeeds, the same B5 discipline
// executeDashboardCreate uses. A single-axis update persists once.
func executeDashboardUpdate(
	ctx context.Context, ws WSClient, op registries.RegOp, liveObj map[string]any,
	liveContentForID map[string]any, dashboardManaged map[string]string,
	stashDir string, preExisting []stashEntry, executed *[]stashEntry,
) error {
	_, hasMetadata := op.Params["metadata"].(map[string]any)
	_, hasContent := op.Params["content"]
	twoCall := hasMetadata && hasContent
	fullKey := "dashboard:" + op.Key
	// Before any write into dashboardManaged: that write IS the adoption,
	// and the entry has to say so for invertDashboardOp to release it.
	_, wasManaged := dashboardManaged[fullKey]

	prior := map[string]any{}
	forward := map[string]any{}

	if metadata, ok := op.Params["metadata"].(map[string]any); ok {
		if liveObj == nil {
			// The dashboard left between plan and apply; updating a nil
			// prior would jam its own rollback with "field: null" restores.
			return fmt.Errorf("dashboard %s no longer exists; re-check to plan against current state", op.LiveID)
		}
		priorMeta := map[string]any{}
		for f := range metadata {
			priorMeta[f] = liveObj[f]
		}
		updateParams := make(map[string]any, len(metadata)+1)
		updateParams["dashboard_id"] = op.LiveID
		for f, v := range metadata {
			updateParams[f] = v
		}
		if _, err := ws.Cmd(ctx, msgLovelaceDashboardsUpdate, updateParams); err != nil {
			return err
		}
		prior["metadata"] = priorMeta
		forward["metadata"] = metadata

		if twoCall {
			dashboardManaged[fullKey] = op.LiveID
			// Fresh copies, not prior/forward themselves: those two are
			// mutated by the content half below, and this interim entry must
			// keep describing exactly what has run so far even if the final
			// stash write fails and it is all a rollback ever sees.
			entry := stashEntry{
				Kind: registries.KindUpdate, RType: "dashboard", Key: op.Key, LiveID: op.LiveID,
				PriorObject:   map[string]any{"metadata": priorMeta},
				ForwardParams: map[string]any{"metadata": metadata},
				Adopted:       !wasManaged,
			}
			if err := appendDashboardStashEntry(stashDir, preExisting, executed, entry); err != nil {
				return err
			}
		}
	}

	if content, ok := op.Params["content"]; ok {
		if _, err := ws.Cmd(ctx, msgLovelaceConfigSave, map[string]any{"url_path": op.Key, "config": content}); err != nil {
			return err
		}
		prior["content"] = liveContentForID // nil if this url_path had no content saved yet
		forward["content"] = content
	}

	dashboardManaged[fullKey] = op.LiveID
	entry := stashEntry{
		Kind: registries.KindUpdate, RType: "dashboard", Key: op.Key, LiveID: op.LiveID,
		PriorObject: prior, ForwardParams: forward, Adopted: !wasManaged,
	}
	if twoCall {
		return replaceLastDashboardStashEntry(stashDir, preExisting, executed, entry)
	}
	return appendDashboardStashEntry(stashDir, preExisting, executed, entry)
}

// executeDashboardDelete deletes a managed dashboard's metadata, which
// destroys its content too, and stashes everything an inverse needs:
// full prior metadata including require_admin, so a rollback cannot widen
// an admin-only dashboard back to non-admin (S2), plus any prior content.
func executeDashboardDelete(
	ctx context.Context, ws WSClient, op registries.RegOp,
	liveObj map[string]any, liveContentForID map[string]any, dashboardManaged map[string]string,
	stashDir string, preExisting []stashEntry, executed *[]stashEntry,
) error {
	if liveObj == nil {
		// Already gone live. Deleting again would fail, and stashing an
		// empty prior would let a rollback recreate the dashboard with none
		// of its real metadata - an admin-only one coming back admin-open
		// included. Nothing changed live, so nothing goes in the stash.
		slog.Info("regapply: apply_dashboard_plan: dashboard already absent, nothing to delete", "key", op.Key)
		delete(dashboardManaged, "dashboard:"+op.Key)
		return nil
	}

	if _, err := ws.Cmd(ctx, msgLovelaceDashboardsDelete, map[string]any{"dashboard_id": op.LiveID}); err != nil {
		return err
	}

	priorMeta := map[string]any{}
	for _, f := range []string{"title", "icon", "show_in_sidebar", "require_admin"} {
		if v, ok := liveObj[f]; ok {
			priorMeta[f] = v
		}
	}
	prior := map[string]any{"metadata": priorMeta}
	if liveContentForID != nil {
		prior["content"] = liveContentForID
	}

	delete(dashboardManaged, "dashboard:"+op.Key)
	entry := stashEntry{Kind: registries.KindDelete, RType: "dashboard", Key: op.Key, LiveID: op.LiveID, PriorObject: prior}
	return appendDashboardStashEntry(stashDir, preExisting, executed, entry)
}

// invertDashboardOp inverts one executed "dashboard" stash entry: create ->
// delete; update -> restore whichever of metadata/content ForwardParams
// touched, from PriorObject; delete -> recreate from the stashed metadata,
// then re-save any prior content.
//
// A nil prior content - this op saved the url_path's first content ever -
// is left in place rather than deleted: lovelace/config/delete is
// deliberately unused here, and keeping content the user may have since
// edited is the safer default.
func invertDashboardOp(ctx context.Context, ws WSClient, entry stashEntry, dashboardManaged map[string]string) error {
	fullKey := "dashboard:" + entry.Key

	switch entry.Kind {
	case registries.KindCreate:
		if _, err := ws.Cmd(ctx, msgLovelaceDashboardsDelete, map[string]any{"dashboard_id": entry.LiveID}); err != nil {
			return err
		}
		delete(dashboardManaged, fullKey)
		return nil

	case registries.KindUpdate:
		if metaForward, ok := entry.ForwardParams["metadata"].(map[string]any); ok {
			priorMeta, _ := entry.PriorObject["metadata"].(map[string]any)
			restoreParams := make(map[string]any, len(metaForward)+1)
			restoreParams["dashboard_id"] = entry.LiveID
			for f := range metaForward {
				var v any
				if priorMeta != nil {
					v = priorMeta[f]
				}
				restoreParams[f] = v
			}
			if _, err := ws.Cmd(ctx, msgLovelaceDashboardsUpdate, restoreParams); err != nil {
				return err
			}
		}
		if _, ok := entry.ForwardParams["content"]; ok {
			if priorContent, ok := entry.PriorObject["content"].(map[string]any); ok && priorContent != nil {
				if _, err := ws.Cmd(ctx, msgLovelaceConfigSave, map[string]any{"url_path": entry.Key, "config": priorContent}); err != nil {
					return err
				}
			}
		}
		if entry.Adopted {
			// The update was the adoption; releasing the key keeps a later
			// manifest removal from deleting a user-made dashboard.
			delete(dashboardManaged, fullKey)
		}
		return nil

	case registries.KindDelete:
		priorMeta, _ := entry.PriorObject["metadata"].(map[string]any)
		createParams := map[string]any{"url_path": entry.Key, "allow_single_word": true}
		for k, v := range priorMeta {
			if v == nil {
				// A live "icon: null" was stashed verbatim; create's schema
				// is not update's and treats null as a value, not a clear.
				continue
			}
			createParams[k] = v
		}
		if _, hasTitle := createParams["title"]; !hasTitle {
			createParams["title"] = entry.Key
		}
		result, err := ws.Cmd(ctx, msgLovelaceDashboardsCreate, createParams)
		if err != nil {
			return err
		}
		resultMap, _ := result.(map[string]any)
		newID, _ := resultMap["id"].(string)
		if newID != "" {
			dashboardManaged[fullKey] = newID
		}
		if priorContent, ok := entry.PriorObject["content"].(map[string]any); ok && priorContent != nil {
			if _, err := ws.Cmd(ctx, msgLovelaceConfigSave, map[string]any{"url_path": entry.Key, "config": priorContent}); err != nil {
				return err
			}
		}
		return nil
	}

	return fmt.Errorf("unreachable: unknown op kind %q", entry.Kind)
}
