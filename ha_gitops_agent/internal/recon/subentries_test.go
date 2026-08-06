package recon

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/flows"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/regapply"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/registries"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/subentries"
)

// declaredSubentry is one manifest item in the shape LoadManifest returns.
func declaredSubentry(id, domain string) map[string]any {
	return map[string]any{
		"id": id, "domain": domain, "subentry_type": "widget",
		"match": map[string]any{"title": id},
		"data":  map[string]any{"user": map[string]any{"slug": id}},
	}
}

// --- ReconcileNow(): wiring internal/subentries ---------------------------

func TestReconcileNowPlansSubentryOpsWhenDeclared(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.subentries.desired = subentries.Desired{Subentries: []map[string]any{declaredSubentry("kitchen", "pushward")}}
	fakes.subentries.planOps = []registries.RegOp{
		{Kind: subentries.KindCreate, RType: "subentry", Key: "kitchen", DiffText: "+subentry_type: widget\n"},
	}
	opts := baseOpts()
	opts.ReconcileSubentries = true
	r := fakes.reconciler(opts)

	r.ReconcileNow(context.Background())

	status := r.Status()
	if status.State != StateDriftPending {
		t.Errorf("state = %q, want drift_pending", status.State)
	}
	want := []PendingRegOp{{RType: "subentry", Key: "kitchen", Kind: "create", DiffText: "+subentry_type: widget\n"}}
	if !reflect.DeepEqual(status.PendingRegistry, want) {
		t.Errorf("pending_registry = %+v, want %+v", status.PendingRegistry, want)
	}
	if len(fakes.subentries.planCalls) != 1 {
		t.Fatalf("plan_calls = %d, want 1", len(fakes.subentries.planCalls))
	}
	if len(fakes.registryApplier.fetchSubentriesCalls) != 1 {
		t.Errorf("fetch_subentries_calls = %+v, want 1", fakes.registryApplier.fetchSubentriesCalls)
	}
}

func TestReconcileNowSubentriesIsIndependentOfOtherToggles(t *testing.T) {
	// Including ReconcileIntegrations: the parent may be hand-made.
	fakes := newReconcilerFakes()
	fakes.subentries.desired = subentries.Desired{Subentries: []map[string]any{declaredSubentry("kitchen", "pushward")}}
	fakes.subentries.planOps = []registries.RegOp{{Kind: subentries.KindCreate, RType: "subentry", Key: "kitchen", DiffText: "+k"}}
	opts := baseOpts()
	opts.ReconcileRegistries = false
	opts.ReconcileDashboards = false
	opts.ReconcileAddonOptions = false
	opts.ReconcileIntegrations = false
	opts.ReconcileSubentries = true
	r := fakes.reconciler(opts)

	r.ReconcileNow(context.Background())

	status := r.Status()
	if len(status.PendingRegistry) != 1 {
		t.Fatalf("pending_registry = %+v, want 1 entry", status.PendingRegistry)
	}
	if fakes.registryApplier.fetchIntegrationEntriesCalls != 1 {
		t.Errorf("fetch_integration_entries_calls = %d, want 1 - this layer pays for the fetch when integrations is off",
			fakes.registryApplier.fetchIntegrationEntriesCalls)
	}
}

func TestReconcileNowSkipsSubentryFetchWhenToggleOff(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.subentries.desired = subentries.Desired{Subentries: []map[string]any{declaredSubentry("kitchen", "pushward")}}
	opts := baseOpts()
	opts.ReconcileSubentries = false
	r := fakes.reconciler(opts)

	r.ReconcileNow(context.Background())

	if len(fakes.subentries.loadManifestCalls) != 0 {
		t.Errorf("load_manifest_calls = %v, want none", fakes.subentries.loadManifestCalls)
	}
	if len(fakes.registryApplier.fetchSubentriesCalls) != 0 {
		t.Errorf("fetch_subentries_calls = %+v, want none", fakes.registryApplier.fetchSubentriesCalls)
	}
}

func TestReconcileNowSkipsSubentryFetchWhenNoWork(t *testing.T) {
	fakes := newReconcilerFakes()
	opts := baseOpts()
	opts.ReconcileSubentries = true
	r := fakes.reconciler(opts)

	r.ReconcileNow(context.Background())

	if len(fakes.subentries.loadManifestCalls) != 1 {
		t.Errorf("load_manifest_calls = %v, want a single call regardless", fakes.subentries.loadManifestCalls)
	}
	if fakes.registryApplier.fetchIntegrationEntriesCalls != 0 {
		t.Errorf("fetch_integration_entries_calls = %d, want 0 (no work, never fetched)",
			fakes.registryApplier.fetchIntegrationEntriesCalls)
	}
	if len(fakes.registryApplier.fetchSubentriesCalls) != 0 {
		t.Errorf("fetch_subentries_calls = %+v, want none", fakes.registryApplier.fetchSubentriesCalls)
	}
	if len(fakes.subentries.planCalls) != 0 {
		t.Errorf("plan_calls = %d, want 0", len(fakes.subentries.planCalls))
	}
}

func TestReconcileNowFetchesSubentriesWhenOnlyManagedExists(t *testing.T) {
	// The "manifest emptied but still managed" case: nothing declared,
	// but a managed subentry may still need unmanaging.
	fakes := newReconcilerFakes()
	fakes.applier.state.SubentryManaged = map[string]string{"subentry:kitchen": "sub123"}
	opts := baseOpts()
	opts.ReconcileSubentries = true
	r := fakes.reconciler(opts)

	r.ReconcileNow(context.Background())

	if len(fakes.subentries.planCalls) != 1 {
		t.Fatalf("plan_calls = %d, want 1", len(fakes.subentries.planCalls))
	}
	if !reflect.DeepEqual(fakes.subentries.planCalls[0].managed, fakes.applier.state.SubentryManaged) {
		t.Errorf("plan managed = %+v, want %+v", fakes.subentries.planCalls[0].managed, fakes.applier.state.SubentryManaged)
	}
	// No parent resolves, but the parent list is still read: the plan
	// needs it to decide that.
	if fakes.registryApplier.fetchIntegrationEntriesCalls != 1 {
		t.Errorf("fetch_integration_entries_calls = %d, want 1", fakes.registryApplier.fetchIntegrationEntriesCalls)
	}
	if len(fakes.registryApplier.fetchSubentriesCalls) != 1 || len(fakes.registryApplier.fetchSubentriesCalls[0]) != 0 {
		t.Errorf("fetch_subentries_calls = %+v, want one call with no entry ids", fakes.registryApplier.fetchSubentriesCalls)
	}
}

func TestReconcileNowSharesOneConfigEntryFetchWithIntegrations(t *testing.T) {
	// Both layers plan against the same live entry list, and with both
	// toggles on the box is asked once (see integrationEntriesCache).
	fakes := newReconcilerFakes()
	fakes.registryApplier.fetchIntegrationEntriesResult = []map[string]any{
		{"entry_id": "e1", "domain": "pushward", "title": "PushWard"},
	}
	fakes.flows.desired = flows.Desired{
		Integrations: []map[string]any{{"id": "pw", "domain": "pushward", "title": "PushWard", "data": map[string]any{}}},
	}
	fakes.subentries.desired = subentries.Desired{Subentries: []map[string]any{declaredSubentry("kitchen", "pushward")}}
	opts := baseOpts()
	opts.ReconcileIntegrations = true
	opts.ReconcileSubentries = true
	r := fakes.reconciler(opts)

	r.ReconcileNow(context.Background())

	if fakes.registryApplier.fetchIntegrationEntriesCalls != 1 {
		t.Errorf("fetch_integration_entries_calls = %d, want 1 - the two layers share one fetch",
			fakes.registryApplier.fetchIntegrationEntriesCalls)
	}
	if len(fakes.subentries.planCalls) != 1 {
		t.Fatalf("plan_calls = %d, want 1", len(fakes.subentries.planCalls))
	}
	if len(fakes.subentries.planCalls[0].live) != 1 {
		t.Errorf("plan live entries = %+v, want the shared fetch's own list", fakes.subentries.planCalls[0].live)
	}
}

func TestReconcileNowFetchesOnlyCandidateParentEntryIDs(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.registryApplier.fetchIntegrationEntriesResult = []map[string]any{
		{"entry_id": "e1", "domain": "pushward", "title": "PushWard"},
		{"entry_id": "e2", "domain": "pushward", "title": "Other"},
		{"entry_id": "e3", "domain": "hue", "title": "Hue"},
		{"entry_id": "", "domain": "pushward", "title": "PushWard"},
	}
	item := declaredSubentry("kitchen", "pushward")
	item["entry_title"] = "PushWard"
	// A second item on the same parent must not produce a second entry id.
	fakes.subentries.desired = subentries.Desired{Subentries: []map[string]any{item, declaredSubentry("hall", "hue")}}
	opts := baseOpts()
	opts.ReconcileSubentries = true
	r := fakes.reconciler(opts)

	r.ReconcileNow(context.Background())

	if len(fakes.registryApplier.fetchSubentriesCalls) != 1 {
		t.Fatalf("fetch_subentries_calls = %+v, want 1", fakes.registryApplier.fetchSubentriesCalls)
	}
	got := fakes.registryApplier.fetchSubentriesCalls[0]
	want := []string{"e1", "e2", "e3"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("fetched entry ids = %v, want %v - every entry of a declared domain is listed even though "+
			"entry_title pins one of them, and the id-less entry is unusable", got, want)
	}
}

func TestReconcileNowListsEveryEntryOfADeclaredDomainSoARenamedParentStillResolves(t *testing.T) {
	// Plan locates a managed subentry by scanning the fetched listings, so
	// withholding a renamed parent would make it look missing and duplicate.
	fakes := newReconcilerFakes()
	fakes.registryApplier.fetchIntegrationEntriesResult = []map[string]any{
		{"entry_id": "e1", "domain": "pushward", "title": "Renamed Since"},
	}
	item := declaredSubentry("kitchen", "pushward")
	item["entry_title"] = "PushWard"
	fakes.subentries.desired = subentries.Desired{Subentries: []map[string]any{item}}
	fakes.applier.state.SubentryManaged = map[string]string{"subentry:kitchen": "sub-1"}
	opts := baseOpts()
	opts.ReconcileSubentries = true
	r := fakes.reconciler(opts)

	r.ReconcileNow(context.Background())

	if len(fakes.registryApplier.fetchSubentriesCalls) != 1 {
		t.Fatalf("fetch_subentries_calls = %+v, want 1", fakes.registryApplier.fetchSubentriesCalls)
	}
	got := fakes.registryApplier.fetchSubentriesCalls[0]
	if !reflect.DeepEqual(got, []string{"e1"}) {
		t.Errorf("fetched entry ids = %v, want [e1] - the renamed parent must still be listed", got)
	}
}

func TestReconcileNowPassesSubentryHashesAndAttemptsToPlan(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.applier.state.SubentryHashes = map[string]string{"subentry:kitchen": "h1"}
	fakes.applier.state.SubentryAttempts = map[string]map[string]any{
		"subentry:hall": {"hash": "h2", "error": "flow step 'user' has no declared data"},
	}
	fakes.subentries.desired = subentries.Desired{Subentries: []map[string]any{declaredSubentry("kitchen", "pushward")}}
	opts := baseOpts()
	opts.ReconcileSubentries = true
	r := fakes.reconciler(opts)

	r.ReconcileNow(context.Background())

	if len(fakes.subentries.planCalls) != 1 {
		t.Fatalf("plan_calls = %d, want 1", len(fakes.subentries.planCalls))
	}
	call := fakes.subentries.planCalls[0]
	if !reflect.DeepEqual(call.hashes, fakes.applier.state.SubentryHashes) {
		t.Errorf("plan hashes = %+v, want %+v", call.hashes, fakes.applier.state.SubentryHashes)
	}
	if !reflect.DeepEqual(call.attempts, fakes.applier.state.SubentryAttempts) {
		t.Errorf("plan attempts = %+v, want %+v", call.attempts, fakes.applier.state.SubentryAttempts)
	}
}

func TestReconcileNowSubentryManifestErrorSurfacesVerbatim(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.subentries.manifestErr = &subentries.ManifestError{
		Problems: []string{"subentries.yaml: subentry 'x' has an invalid or missing 'subentry_type'"},
	}
	opts := baseOpts()
	opts.ReconcileSubentries = true
	r := fakes.reconciler(opts)

	r.ReconcileNow(context.Background())

	status := r.Status()
	if status.State != StateError {
		t.Errorf("state = %q, want error", status.State)
	}
	if !strings.Contains(status.LastError, "invalid or missing 'subentry_type'") {
		t.Errorf("last_error = %q", status.LastError)
	}
}

func TestReconcileNowSubentriesRunAfterIntegrationsOnlyIfItSucceeded(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.flows.manifestErr = errors.New("integrations.yaml: boom")
	opts := baseOpts()
	opts.ReconcileIntegrations = true
	opts.ReconcileSubentries = true
	r := fakes.reconciler(opts)

	r.ReconcileNow(context.Background())

	if len(fakes.subentries.loadManifestCalls) != 0 {
		t.Errorf("load_manifest_calls = %v, want none - the cycle must have failed before reaching subentries",
			fakes.subentries.loadManifestCalls)
	}
	status := r.Status()
	if status.State != StateError {
		t.Errorf("state = %q, want error", status.State)
	}
}

func TestReconcileNowMixesSubentryOpsIntoOnePendingList(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.flows.desired = flows.Desired{
		Integrations: []map[string]any{{"id": "pw", "domain": "pushward", "title": "PushWard", "data": map[string]any{}}},
	}
	fakes.flows.planOps = []registries.RegOp{{Kind: flows.KindCreate, RType: "integration", Key: "pw", DiffText: "+pw"}}
	fakes.subentries.desired = subentries.Desired{Subentries: []map[string]any{declaredSubentry("kitchen", "pushward")}}
	fakes.subentries.planOps = []registries.RegOp{
		{Kind: subentries.KindCreate, RType: "subentry", Key: "kitchen", DiffText: "+k"},
	}
	opts := baseOpts()
	opts.ReconcileIntegrations = true
	opts.ReconcileSubentries = true
	r := fakes.reconciler(opts)

	r.ReconcileNow(context.Background())

	status := r.Status()
	if len(status.PendingRegistry) != 2 {
		t.Fatalf("pending_registry = %+v, want 2 entries", status.PendingRegistry)
	}
	// Subentries plan last, after the integrations ops they depend on.
	if status.PendingRegistry[0].RType != "integration" || status.PendingRegistry[1].RType != "subentry" {
		t.Errorf("pending_registry order = %+v, want integration then subentry", status.PendingRegistry)
	}
}

// --- ApplyNow(): subentries run after integrations, only if it succeeded --

// pendingSubentryReconciler plans one integration op and one subentry op,
// dry_run off, both toggles on.
func pendingSubentryReconciler(t *testing.T, fakes *reconcilerFakes) *Reconciler {
	t.Helper()
	fakes.flows.desired = flows.Desired{
		Integrations: []map[string]any{{"id": "pw", "domain": "pushward", "title": "PushWard", "data": map[string]any{}}},
	}
	fakes.flows.planOps = []registries.RegOp{{Kind: flows.KindCreate, RType: "integration", Key: "pw", DiffText: "+pw"}}
	fakes.subentries.desired = subentries.Desired{Subentries: []map[string]any{declaredSubentry("kitchen", "pushward")}}
	fakes.subentries.planOps = []registries.RegOp{
		{Kind: subentries.KindCreate, RType: "subentry", Key: "kitchen", DiffText: "+k"},
	}
	opts := baseOpts()
	opts.DryRun = false
	opts.ReconcileIntegrations = true
	opts.ReconcileSubentries = true
	r := fakes.reconciler(opts)
	r.ReconcileNow(context.Background())
	return r
}

func TestApplyNowAppliesSubentriesAfterIntegrationsInOrder(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.registryApplier.applyFlowResult = regapply.RegistryApplyResult{OK: true, Applied: []string{"create integration:pw"}}
	fakes.registryApplier.applySubentryResult = regapply.RegistryApplyResult{
		OK: true, Applied: []string{"create subentry:kitchen"},
	}
	r := pendingSubentryReconciler(t, fakes)

	result := r.ApplyNow(context.Background(), true)

	if !result.OK {
		t.Fatalf("result = %+v", result)
	}
	if len(fakes.registryApplier.applyFlowPlanCalls) != 1 {
		t.Fatalf("apply_flow_plan_calls = %+v", fakes.registryApplier.applyFlowPlanCalls)
	}
	if len(fakes.registryApplier.applySubentryPlanCalls) != 1 {
		t.Fatalf("apply_subentry_plan_calls = %+v", fakes.registryApplier.applySubentryPlanCalls)
	}
	if rtype := fakes.registryApplier.applySubentryPlanCalls[0].ops[0].RType; rtype != "subentry" {
		t.Errorf("subentry layer got an op of rtype %q", rtype)
	}
	if r.Status().State != StateInSync {
		t.Errorf("state = %q, want in_sync", r.Status().State)
	}
}

func TestApplyNowSubentryFailureDoesNotUndoIntegrationSuccess(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.registryApplier.applyFlowResult = regapply.RegistryApplyResult{OK: true, Applied: []string{"create integration:pw"}}
	// RolledBack is always false here: per-op isolation, and no stash.
	fakes.registryApplier.applySubentryResult = regapply.RegistryApplyResult{
		OK: false, Error: "create subentry:kitchen failed: boom", RolledBack: false,
	}
	r := pendingSubentryReconciler(t, fakes)

	result := r.ApplyNow(context.Background(), true)

	if result.OK {
		t.Errorf("result = %+v, want ok=false", result)
	}
	if !strings.Contains(result.Error, "create subentry:kitchen failed") {
		t.Errorf("result.Error = %q", result.Error)
	}
	if len(fakes.registryApplier.applyFlowPlanCalls) != 1 {
		t.Errorf("apply_flow_plan_calls = %d, want 1 (the integrations layer ran and succeeded)",
			len(fakes.registryApplier.applyFlowPlanCalls))
	}
	if r.Status().State != StateError {
		t.Errorf("state = %q, want error", r.Status().State)
	}
}

func TestApplyNowEventLogUsesIndependentWordingForSubentriesFailure(t *testing.T) {
	// Same as the integrations layer: count what stayed applied, and never
	// claim a rollback this layer cannot do.
	fakes := newReconcilerFakes()
	fakes.registryApplier.applyFlowResult = regapply.RegistryApplyResult{OK: true, Applied: []string{"create integration:pw"}}
	fakes.registryApplier.applySubentryResult = regapply.RegistryApplyResult{
		OK: false, Applied: []string{"create subentry:kitchen"}, Error: "reconfigure subentry:hall failed: boom",
	}
	r := pendingSubentryReconciler(t, fakes)

	r.ApplyNow(context.Background(), true)

	var msg string
	for _, e := range r.Status().Events {
		if strings.Contains(e.Message, "subentries:") {
			msg = e.Message
		}
	}
	if !strings.Contains(msg, "subentries: reconfigure subentry:hall failed: boom") {
		t.Errorf("event %q, want the subentries-specific wording naming the failed op", msg)
	}
	if strings.Contains(msg, "rolled back") {
		t.Errorf("event %q, want no rollback wording - this layer never rolls anything back", msg)
	}
	if !strings.Contains(msg, "2 registry change(s) stayed applied") {
		t.Errorf("event %q, want both the earlier layer's op and this layer's own sibling counted", msg)
	}
}

func TestApplyNowSubentriesNeverRunWhenIntegrationApplyItselfFails(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.registryApplier.applyFlowResult = regapply.RegistryApplyResult{
		OK: false, Error: "create integration:pw failed: boom",
	}
	r := pendingSubentryReconciler(t, fakes)

	r.ApplyNow(context.Background(), true)

	if len(fakes.registryApplier.applySubentryPlanCalls) != 0 {
		t.Errorf("apply_subentry_plan_calls = %+v, want none", fakes.registryApplier.applySubentryPlanCalls)
	}
}

func TestApplyNowOnlySubentryOpsNeverCallsOtherApplyPlans(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.subentries.desired = subentries.Desired{Subentries: []map[string]any{declaredSubentry("kitchen", "pushward")}}
	fakes.subentries.planOps = []registries.RegOp{
		{Kind: subentries.KindCreate, RType: "subentry", Key: "kitchen", DiffText: "+k"},
	}
	fakes.registryApplier.applySubentryResult = regapply.RegistryApplyResult{
		OK: true, Applied: []string{"create subentry:kitchen"},
	}
	opts := baseOpts()
	opts.DryRun = false
	opts.ReconcileSubentries = true
	r := fakes.reconciler(opts)
	r.ReconcileNow(context.Background())

	result := r.ApplyNow(context.Background(), true)

	if !result.OK {
		t.Fatalf("result = %+v", result)
	}
	if len(fakes.registryApplier.applyPlanCalls) != 0 {
		t.Errorf("apply_plan_calls = %+v, want none", fakes.registryApplier.applyPlanCalls)
	}
	if len(fakes.registryApplier.applyEntityPlanCalls) != 0 {
		t.Errorf("apply_entity_plan_calls = %+v, want none", fakes.registryApplier.applyEntityPlanCalls)
	}
	if len(fakes.registryApplier.applyDashboardPlanCalls) != 0 {
		t.Errorf("apply_dashboard_plan_calls = %+v, want none", fakes.registryApplier.applyDashboardPlanCalls)
	}
	if len(fakes.registryApplier.applyAddonPlanCalls) != 0 {
		t.Errorf("apply_addon_plan_calls = %+v, want none", fakes.registryApplier.applyAddonPlanCalls)
	}
	if len(fakes.registryApplier.applyFlowPlanCalls) != 0 {
		t.Errorf("apply_flow_plan_calls = %+v, want none", fakes.registryApplier.applyFlowPlanCalls)
	}
	if len(fakes.registryApplier.applySubentryPlanCalls) != 1 {
		t.Errorf("apply_subentry_plan_calls = %+v, want 1", fakes.registryApplier.applySubentryPlanCalls)
	}
}

func TestApplyNowKeepsSkippedSubentryErrorsPending(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.registryApplier.applyFlowResult = regapply.RegistryApplyResult{OK: true}
	fakes.registryApplier.applySubentryResult = regapply.RegistryApplyResult{
		OK: true,
		SkippedErrors: []registries.RegOp{
			{Kind: registries.KindError, RType: "subentry", Key: "hall", Error: "no live integration entry for domain 'hue'"},
		},
	}
	r := pendingSubentryReconciler(t, fakes)

	r.ApplyNow(context.Background(), true)

	status := r.Status()
	if len(status.PendingRegistry) != 1 || status.PendingRegistry[0].Key != "hall" {
		t.Errorf("pending_registry = %+v", status.PendingRegistry)
	}
	if status.State != StateDriftPending {
		t.Errorf("state = %q, want drift_pending", status.State)
	}
}

func TestApplyNowRebuildsPendingFromUnappliedSubentryOps(t *testing.T) {
	// Only the op that never took effect may stay pending: re-submitting
	// the applied one would create a duplicate subentry.
	fakes := newReconcilerFakes()
	fakes.subentries.desired = subentries.Desired{
		Subentries: []map[string]any{declaredSubentry("kitchen", "pushward"), declaredSubentry("hall", "pushward")},
	}
	fakes.subentries.planOps = []registries.RegOp{
		{Kind: subentries.KindCreate, RType: "subentry", Key: "kitchen", DiffText: "+k"},
		{Kind: subentries.KindUpdate, RType: "subentry", Key: "hall", DiffText: "+h"},
	}
	fakes.registryApplier.applySubentryResult = regapply.RegistryApplyResult{
		OK: false, Applied: []string{"create subentry:kitchen"}, Error: "update subentry:hall failed: boom",
	}
	opts := baseOpts()
	opts.DryRun = false
	opts.ReconcileSubentries = true
	r := fakes.reconciler(opts)
	r.ReconcileNow(context.Background())

	r.ApplyNow(context.Background(), true)

	status := r.Status()
	if len(status.PendingRegistry) != 1 || status.PendingRegistry[0].Key != "hall" {
		t.Errorf("pending_registry = %+v, want only the op that never took effect", status.PendingRegistry)
	}
	if status.State != StateError {
		t.Errorf("state = %q, want error", status.State)
	}
}

func TestApplyNowPassesSubentryStateThroughAndPersistsIt(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.applier.state.SubentryManaged = map[string]string{"subentry:existing": "sub1"}
	fakes.applier.state.SubentryHashes = map[string]string{"subentry:existing": "h"}
	fakes.applier.state.SubentryAttempts = map[string]map[string]any{"subentry:broken": {"hash": "h2", "error": "boom"}}
	// ApplySubentryPlan writes into these three maps in place; ApplyNow's
	// StateSave is what makes them durable.
	fakes.registryApplier.onApplySubentryPlan = func(
		managed, hashes map[string]string, attempts map[string]map[string]any,
	) {
		managed["subentry:kitchen"] = "sub2"
		hashes["subentry:kitchen"] = "h3"
		delete(attempts, "subentry:broken")
	}
	fakes.registryApplier.applySubentryResult = regapply.RegistryApplyResult{
		OK: true, Applied: []string{"create subentry:kitchen"},
	}
	r := pendingSubentryReconciler(t, fakes)

	r.ApplyNow(context.Background(), true)

	if len(fakes.registryApplier.applySubentryPlanCalls) != 1 {
		t.Fatalf("apply_subentry_plan_calls = %+v", fakes.registryApplier.applySubentryPlanCalls)
	}
	call := fakes.registryApplier.applySubentryPlanCalls[0]
	if !reflect.DeepEqual(call.managed, fakes.applier.state.SubentryManaged) {
		t.Errorf("managed = %+v, want the state's own map %+v", call.managed, fakes.applier.state.SubentryManaged)
	}
	if !reflect.DeepEqual(call.hashes, fakes.applier.state.SubentryHashes) {
		t.Errorf("hashes = %+v, want %+v", call.hashes, fakes.applier.state.SubentryHashes)
	}
	if !reflect.DeepEqual(call.attempts, fakes.applier.state.SubentryAttempts) {
		t.Errorf("attempts = %+v, want %+v", call.attempts, fakes.applier.state.SubentryAttempts)
	}
	if len(fakes.applier.stateSaveCalls) != 1 {
		t.Fatalf("state_save_calls = %d, want 1", len(fakes.applier.stateSaveCalls))
	}
	saved := fakes.applier.stateSaveCalls[0]
	wantManaged := map[string]string{"subentry:existing": "sub1", "subentry:kitchen": "sub2"}
	if !reflect.DeepEqual(saved.SubentryManaged, wantManaged) {
		t.Errorf("saved subentry_managed = %+v, want %+v", saved.SubentryManaged, wantManaged)
	}
	wantHashes := map[string]string{"subentry:existing": "h", "subentry:kitchen": "h3"}
	if !reflect.DeepEqual(saved.SubentryHashes, wantHashes) {
		t.Errorf("saved subentry_hashes = %+v, want %+v", saved.SubentryHashes, wantHashes)
	}
	if len(saved.SubentryAttempts) != 0 {
		t.Errorf("saved subentry_attempts = %+v, want the cleared attempt gone", saved.SubentryAttempts)
	}
}

// --- status: pending_subentry_ops ----------------------------------------

func TestPushStatusReportsPendingSubentryOpsSeparately(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.flows.desired = flows.Desired{
		Integrations: []map[string]any{{"id": "pw", "domain": "pushward", "title": "PushWard", "data": map[string]any{}}},
	}
	fakes.flows.planOps = []registries.RegOp{{Kind: flows.KindCreate, RType: "integration", Key: "pw", DiffText: "+pw"}}
	fakes.subentries.desired = subentries.Desired{Subentries: []map[string]any{declaredSubentry("kitchen", "pushward")}}
	fakes.subentries.planOps = []registries.RegOp{
		{Kind: subentries.KindCreate, RType: "subentry", Key: "kitchen", DiffText: "+k"},
	}
	opts := baseOpts()
	opts.ReconcileIntegrations = true
	opts.ReconcileSubentries = true
	r := fakes.reconciler(opts)

	r.ReconcileNow(context.Background())

	var pushed map[string]any
	for _, call := range fakes.status.pushes {
		pushed = call.attrs
	}
	if pushed["pending_integration_ops"] != 1 {
		t.Errorf("pending_integration_ops = %v, want 1", pushed["pending_integration_ops"])
	}
	if pushed["pending_subentry_ops"] != 1 {
		t.Errorf("pending_subentry_ops = %v, want 1", pushed["pending_subentry_ops"])
	}
}
