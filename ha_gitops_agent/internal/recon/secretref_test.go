package recon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/addonopts"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/differ"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/flows"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/options"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/regapply"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/subentries"
)

// reconResolvedSecret is what secrets.yaml holds below; the end-to-end
// test asserts it appears nowhere in a marshalled Status.
const reconResolvedSecret = "S3CRET-resolved"

// useSecretsFile points secretsRoot at a temp dir holding contents as its
// secrets.yaml, then restores it (same shape as usePauseFile).
func useSecretsFile(t *testing.T, contents string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "secrets.yaml"), []byte(contents), 0o600); err != nil {
		t.Fatalf("write secrets.yaml: %v", err)
	}
	previous := secretsRoot
	secretsRoot = root
	t.Cleanup(func() { secretsRoot = previous })
	return root
}

// secretOpts turns on the three layers that can carry a reference.
func secretOpts() options.Options {
	opts := baseOpts()
	opts.ReconcileAddonOptions = true
	opts.ReconcileIntegrations = true
	opts.ReconcileSubentries = true
	return opts
}

// One resolver per cycle: the file is read once, and two references in one
// plan can never resolve against two generations of it.
func TestReconcileNowSharesOneSecretResolverAcrossEveryLayer(t *testing.T) {
	useSecretsFile(t, "shared_key: "+reconResolvedSecret+"\n")

	fakes := newReconcilerFakes()
	fakes.addonOpts.desired = addonopts.Desired{Addons: []map[string]any{
		{"slug": "core_mqtt", "options": map[string]any{"password": "x"}, "restart_on_change": true},
	}}
	fakes.addonOpts.resolveProbe = "shared_key"
	fakes.registryApplier.fetchAddonInfoResult = map[string]map[string]any{
		"core_mqtt": {"options": map[string]any{"password": "old"}},
	}

	fakes.flows.desired = flows.Desired{Integrations: []map[string]any{
		{"id": "anker", "domain": "anker", "title": "Anker", "data": map[string]any{}},
	}}
	fakes.flows.resolveProbe = "shared_key"
	fakes.registryApplier.fetchIntegrationEntriesResult = []map[string]any{
		{"entry_id": "e1", "domain": "pushward", "title": "PushWard"},
	}

	fakes.subentries.desired = subentries.Desired{Subentries: []map[string]any{
		{
			"id": "widget_hall", "domain": "pushward", "subentry_type": "widget",
			"match": map[string]any{"title": "Hall"}, "data": map[string]any{},
		},
	}}
	fakes.subentries.resolveProbe = "shared_key"
	fakes.registryApplier.fetchSubentriesResult = map[string][]map[string]any{"e1": {}}

	fakes.reconciler(secretOpts()).ReconcileNow(context.Background())

	if len(fakes.addonOpts.planCalls) != 1 || len(fakes.flows.planCalls) != 1 || len(fakes.subentries.planCalls) != 1 {
		t.Fatalf("all three layers must have planned: addon=%d flows=%d subentries=%d",
			len(fakes.addonOpts.planCalls), len(fakes.flows.planCalls), len(fakes.subentries.planCalls))
	}

	shared := fakes.addonOpts.planCalls[0].secrets
	if shared == nil {
		t.Fatal("the addon layer was handed no resolver")
	}
	if fakes.flows.planCalls[0].secrets != shared || fakes.subentries.planCalls[0].secrets != shared {
		t.Errorf("each layer got its own resolver: addon=%p flows=%p subentries=%p",
			shared, fakes.flows.planCalls[0].secrets, fakes.subentries.planCalls[0].secrets)
	}

	// Each layer really resolved, so the count below covers three
	// resolutions rather than none.
	for _, fake := range []struct {
		what     string
		resolved []string
		err      error
	}{
		{"addon", fakes.addonOpts.resolved, fakes.addonOpts.resolveErr},
		{"flows", fakes.flows.resolved, fakes.flows.resolveErr},
		{"subentries", fakes.subentries.resolved, fakes.subentries.resolveErr},
	} {
		if fake.err != nil {
			t.Errorf("%s layer could not resolve: %v", fake.what, fake.err)
		}
		if len(fake.resolved) != 1 || fake.resolved[0] != reconResolvedSecret {
			t.Errorf("%s layer resolved %+v, want the secrets file value", fake.what, fake.resolved)
		}
	}

	if got := shared.LoadCount(); got != 1 {
		t.Errorf("secrets.yaml was read %d times in one cycle, want 1", got)
	}
}

// A second cycle re-reads the file, so a rotated secret is picked up on
// the next interval rather than at the next restart.
func TestEachCycleGetsAFreshSecretResolver(t *testing.T) {
	useSecretsFile(t, "shared_key: "+reconResolvedSecret+"\n")

	fakes := newReconcilerFakes()
	fakes.flows.desired = flows.Desired{Integrations: []map[string]any{
		{"id": "anker", "domain": "anker", "title": "Anker", "data": map[string]any{}},
	}}
	fakes.flows.resolveProbe = "shared_key"

	opts := baseOpts()
	opts.ReconcileIntegrations = true
	r := fakes.reconciler(opts)

	r.ReconcileNow(context.Background())
	first := fakes.flows.planCalls[0].secrets
	r.ReconcileNow(context.Background())
	second := fakes.flows.planCalls[1].secrets

	if first == second {
		t.Error("two cycles shared one resolver, so a rotated secret would never be seen again")
	}
}

// --- end to end, with the real layers ------------------------------------

// realLayerReconciler runs the three secret-carrying layers for real
// against manifests in workdir, with every other collaborator faked.
func realLayerReconciler(fakes *reconcilerFakes, workdir string, opts options.Options) *Reconciler {
	fakes.git.workdir = workdir
	return New(opts, Deps{
		Git:             fakes.git,
		Differ:          fakes.differ,
		Applier:         fakes.applier,
		Snapshot:        fakes.snapshot,
		Status:          fakes.status,
		Registries:      fakes.registries,
		RegistryApplier: fakes.registryApplier,
		Entities:        fakes.entities,
		Dashboards:      fakes.dashboards,
		History:         fakes.history,
		// AddonOpts, Flows and Subentries left nil: New fills in the real ones.
	})
}

// writeSecretManifests writes one manifest per secret-carrying layer,
// each declaring a value as a reference.
func writeSecretManifests(t *testing.T, workdir string) {
	t.Helper()
	gitops := filepath.Join(workdir, "gitops")
	if err := os.MkdirAll(gitops, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"addons.yaml": "addons:\n" +
			"  - slug: core_mqtt\n" +
			"    options:\n" +
			"      password: secret://shared_key\n",
		"integrations.yaml": "integrations:\n" +
			"  - id: anker\n" +
			"    domain: anker\n" +
			"    title: Anker\n" +
			"    data:\n" +
			"      user:\n" +
			"        password: secret://shared_key\n",
		"subentries.yaml": "subentries:\n" +
			"  - id: widget_hall\n" +
			"    domain: pushward\n" +
			"    subentry_type: widget\n" +
			"    match:\n" +
			"      title: Hall\n" +
			"    data:\n" +
			"      user:\n" +
			"        api_key: secret://shared_key\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(gitops, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// The boundary test: /status.json is served to anyone who reaches ingress,
// so the whole marshalled Status is checked, not just the pending ops.
func TestStatusNeverCarriesAResolvedSecret(t *testing.T) {
	useSecretsFile(t, "shared_key: "+reconResolvedSecret+"\n")
	workdir := t.TempDir()
	writeSecretManifests(t, workdir)

	fakes := newReconcilerFakes()
	fakes.registryApplier.fetchAddonInfoResult = map[string]map[string]any{
		"core_mqtt": {"options": map[string]any{"password": "before"}},
	}
	fakes.registryApplier.fetchIntegrationEntriesResult = []map[string]any{
		{"entry_id": "e1", "domain": "pushward", "title": "PushWard"},
	}
	fakes.registryApplier.fetchSubentriesResult = map[string][]map[string]any{"e1": {}}

	r := realLayerReconciler(fakes, workdir, secretOpts())
	r.ReconcileNow(context.Background())

	status := r.Status()
	encoded, err := json.Marshal(status)
	if err != nil {
		t.Fatalf("marshal status: %v", err)
	}
	if strings.Contains(string(encoded), reconResolvedSecret) {
		t.Errorf("status.json carries a value out of the live secrets file:\n%s", encoded)
	}

	// The other half of the assertion: a cycle that planned nothing would
	// pass the check above without proving anything.
	kinds := map[string]bool{}
	for _, op := range status.PendingRegistry {
		kinds[op.RType] = true
	}
	for _, want := range []string{"addon", "integration", "subentry"} {
		if !kinds[want] {
			t.Errorf("no %s op planned, so the check above proves nothing: %+v", want, status.PendingRegistry)
		}
	}
	// The references themselves are shown: that is what makes a masked
	// add-on diff line readable.
	if !strings.Contains(string(encoded), "secret://shared_key") {
		t.Errorf("nothing names the reference, so a user cannot tell which secret a line is about:\n%s", encoded)
	}
}

// A missing key blocks only the items referencing it: the cycle completes
// and each broken item comes back as an error op Retry can reach.
func TestAnUnresolvableSecretBlocksOnlyTheItemsThatReferenceIt(t *testing.T) {
	useSecretsFile(t, "some_other_key: "+reconResolvedSecret+"\n")
	workdir := t.TempDir()
	writeSecretManifests(t, workdir)

	fakes := newReconcilerFakes()
	fakes.registryApplier.fetchAddonInfoResult = map[string]map[string]any{
		"core_mqtt": {"options": map[string]any{"password": "before"}},
	}
	fakes.registryApplier.fetchIntegrationEntriesResult = []map[string]any{
		{"entry_id": "e1", "domain": "pushward", "title": "PushWard"},
	}
	fakes.registryApplier.fetchSubentriesResult = map[string][]map[string]any{"e1": {}}
	// Work for another layer, so "the cycle kept going" is visible.
	fakes.differ.changes = []differ.Change{{Path: "automations.yaml", Kind: "update", DiffText: "+x"}}

	r := realLayerReconciler(fakes, workdir, secretOpts())
	r.ReconcileNow(context.Background())

	status := r.Status()
	if status.State != StateDriftPending {
		t.Errorf("state = %q, want drift pending: an unresolvable secret must not fail the cycle", status.State)
	}
	if status.LastError != "" {
		t.Errorf("last_error = %q, want empty: this is a per-item refusal", status.LastError)
	}

	errored := map[string]string{}
	for _, op := range status.PendingRegistry {
		if op.Kind == "error" {
			errored[op.RType] = op.Error
		}
	}
	for _, rtype := range []string{"addon", "integration", "subentry"} {
		text, ok := errored[rtype]
		if !ok {
			t.Errorf("no error op for the %s layer: %+v", rtype, status.PendingRegistry)
			continue
		}
		if !strings.Contains(text, "no key 'shared_key'") {
			t.Errorf("%s error = %q, want it to name the missing key", rtype, text)
		}
	}
	// The unrelated file layer still planned its own work.
	if len(status.Pending) != 1 || status.Pending[0].Path != "automations.yaml" {
		t.Errorf("pending files = %+v, so one bad secret stopped the cycle", status.Pending)
	}
}

// The apply-side half: a rejected op's text fills last_error, the feed, the
// Blocked card and history.jsonl at once. Real layers, faked scrubbing applier.
func TestAFailingApplyNeverSurfacesAResolvedSecret(t *testing.T) {
	useSecretsFile(t, "shared_key: "+reconResolvedSecret+"\n")
	workdir := t.TempDir()
	writeSecretManifests(t, workdir)

	fakes := newReconcilerFakes()
	fakes.registryApplier.fetchAddonInfoResult = map[string]map[string]any{
		"core_mqtt": {"options": map[string]any{"password": "before"}},
	}
	fakes.registryApplier.fetchIntegrationEntriesResult = []map[string]any{
		{"entry_id": "e1", "domain": "pushward", "title": "PushWard"},
	}
	fakes.registryApplier.fetchSubentriesResult = map[string][]map[string]any{"e1": {}}

	// The add-on layer runs first and must succeed, or the integration
	// layer is never asked; the integration layer then fails pre-scrubbed.
	fakes.registryApplier.applyAddonResult = regapply.RegistryApplyResult{
		OK: true, Applied: []string{"update addon:core_mqtt"},
	}
	const scrubbed = "step 'user' rejected the declared data: password ***REDACTED*** was refused"
	fakes.registryApplier.applyFlowResult = regapply.RegistryApplyResult{
		OK: false, Error: "create integration:anker failed: " + scrubbed,
	}
	fakes.registryApplier.onApplyFlowPlan = func(_, _ map[string]string, _, attempts map[string]map[string]any) {
		attempts["integration:anker"] = map[string]any{"hash": "h", "error": scrubbed}
	}
	// StateLoad returns every map non-nil; the fake's default does not,
	// and ApplyFlowPlan writes into this one in place.
	fakes.applier.state.IntegrationAttempts = map[string]map[string]any{}

	opts := secretOpts()
	opts.DryRun = false
	r := realLayerReconciler(fakes, workdir, opts)
	r.ReconcileNow(context.Background())

	r.ApplyNow(context.Background(), true)

	// The ops that reached the applier really do carry the credential.
	carried := false
	for _, call := range fakes.registryApplier.applyAddonPlanCalls {
		for _, one := range call.ops {
			if one.Params["password"] == reconResolvedSecret {
				carried = true
			}
		}
	}
	if !carried {
		t.Fatal("no op reached the applier carrying the resolved value, so this test proves nothing")
	}

	status := r.Status()
	encoded, err := json.Marshal(status)
	if err != nil {
		t.Fatalf("marshal status: %v", err)
	}
	if strings.Contains(string(encoded), reconResolvedSecret) {
		t.Errorf("status.json carries the resolved secret after a failed apply:\n%s", encoded)
	}
	if len(status.Blocked) == 0 {
		t.Error("nothing is blocked, so the Blocked half of the check above proves nothing")
	}

	for _, event := range status.Events {
		if strings.Contains(event.Message, reconResolvedSecret) {
			t.Errorf("activity feed carries the resolved secret: %q", event.Message)
		}
	}
	for _, record := range r.HistoryAll() {
		if strings.Contains(record.Error, reconResolvedSecret) {
			t.Errorf("history record carries the resolved secret: %q", record.Error)
		}
	}
}
