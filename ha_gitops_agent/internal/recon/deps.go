package recon

import (
	"context"
	"time"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/addonopts"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/applier"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/dashboards"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/differ"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/entities"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/flows"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/gitsync"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/hacs"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/history"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/options"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/regapply"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/registries"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/secretref"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/snapshot"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/sopscrypt"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/statusd"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/subentries"
)

// Git is the internal/gitsync seam: everything Reconciler needs from a
// local clone. Satisfied by *gitsync.GitSync via realGit; tests fake it,
// except the secrets-guard test in recon_test.go, which drives a real
// GitSync because only real EXCLUDED-filtering distinguishes TrackedFiles
// from TrackedFilesRaw.
type Git interface {
	EnsureClone(ctx context.Context) error
	Fetch(ctx context.Context) (string, error)
	Checkout(ctx context.Context, sha string) error
	CurrentSHA(ctx context.Context) string
	TrackedFiles(ctx context.Context, sha string) ([]string, error)
	TrackedFilesRaw(ctx context.Context, sha string) ([]string, error)
	// GuardSecretsAt takes the sha the file list came from: with encryption
	// on, judging a tracked secrets.yaml means reading its blob there.
	GuardSecretsAt(ctx context.Context, sha string, files []string) error
	// Workdir is the checked-out tree Differ/Applier/Registries read from.
	// A method here, a plain field on gitsync.GitSync.
	Workdir() string
	// CommitBack is the gitsync.CommitBack seam, used only by
	// commitDriftBack, always under opLock.
	CommitBack(ctx context.Context, files []gitsync.DriftFile, configRoot, baseSHA string, now time.Time) (string, error)
	// Import is the gitsync.Import seam, used only by importLive, always
	// under opLock.
	Import(ctx context.Context, configRoot string, limits gitsync.ImportLimits, now time.Time) (gitsync.ImportResult, error)
	// ScanLive is the gitsync.ScanLive seam for PreviewImport: the scan
	// Import runs, with nothing after it. Adapted from a free function so
	// a test can drive a preview without a filesystem.
	ScanLive(configRoot string, limits gitsync.ImportLimits) (gitsync.ImportPlan, error)
	// PreviewIgnored is a preview's second half: which scanned files an
	// import would commit, and their size. Separate because only git can
	// answer it (gitsync.PreviewIgnored).
	PreviewIgnored(ctx context.Context, configRoot string, files []string) (kept []string, keptBytes int64, err error)
	// RecordFile is the gitsync.RecordFile seam, where false with a nil
	// error (already correct) is the ordinary outcome. Used only by
	// recordAddonVersions, always under opLock.
	RecordFile(ctx context.Context, relPath string, content []byte, message string) (committed bool, err error)
}

// realGit adapts *gitsync.GitSync to Git.
type realGit struct{ g *gitsync.GitSync }

func newRealGit(g *gitsync.GitSync) *realGit { return &realGit{g: g} }

func (r *realGit) EnsureClone(ctx context.Context) error          { return r.g.EnsureClone(ctx) }
func (r *realGit) Fetch(ctx context.Context) (string, error)      { return r.g.Fetch(ctx) }
func (r *realGit) Checkout(ctx context.Context, sha string) error { return r.g.Checkout(ctx, sha) }
func (r *realGit) CurrentSHA(ctx context.Context) string          { return r.g.CurrentSHA(ctx) }
func (r *realGit) TrackedFiles(ctx context.Context, sha string) ([]string, error) {
	return r.g.TrackedFiles(ctx, sha)
}

func (r *realGit) TrackedFilesRaw(ctx context.Context, sha string) ([]string, error) {
	return r.g.TrackedFilesRaw(ctx, sha)
}

func (r *realGit) GuardSecretsAt(ctx context.Context, sha string, files []string) error {
	return r.g.GuardSecretsAt(ctx, sha, files)
}
func (r *realGit) Workdir() string { return r.g.Workdir }

func (r *realGit) CommitBack(ctx context.Context, files []gitsync.DriftFile, configRoot, baseSHA string, now time.Time) (string, error) {
	return r.g.CommitBack(ctx, files, configRoot, baseSHA, now)
}

func (r *realGit) Import(ctx context.Context, configRoot string, limits gitsync.ImportLimits, now time.Time) (gitsync.ImportResult, error) {
	return r.g.Import(ctx, configRoot, limits, now)
}

func (r *realGit) ScanLive(configRoot string, limits gitsync.ImportLimits) (gitsync.ImportPlan, error) {
	return gitsync.ScanLive(configRoot, limits)
}

func (r *realGit) PreviewIgnored(ctx context.Context, configRoot string, files []string) ([]string, int64, error) {
	return r.g.PreviewIgnored(ctx, configRoot, files)
}

func (r *realGit) RecordFile(ctx context.Context, relPath string, content []byte, message string) (bool, error) {
	return r.g.RecordFile(ctx, relPath, content, message)
}

var _ Git = (*realGit)(nil)

// Differ is the internal/differ.Compute seam.
type Differ interface {
	Compute(
		repoRoot, configRoot string, tracked, prevManifest []string,
	) (changes []differ.Change, skippedContainment, decryptFailures []string)
}

// realDiffer holds the transform differ.Compute reads encrypted files
// through, rather than taking it per call, so the reconcile loop's call
// site knows nothing about encryption - like realApplier and its Config.
// A zero realDiffer has no transform: exactly "no age key configured".
type realDiffer struct {
	transform differ.RepoTransform
}

func (d realDiffer) Compute(repoRoot, configRoot string, tracked, prevManifest []string) ([]differ.Change, []string, []string) {
	return differ.Compute(repoRoot, configRoot, tracked, prevManifest, d.transform)
}

var _ Differ = realDiffer{}

// Applier is the internal/applier seam: apply a diff, and persist/inspect
// the agent's sync state and per-apply stash directories.
type Applier interface {
	Apply(ctx context.Context, changes []applier.Change, repoRoot, configRoot string, opts options.Options) (applier.Result, error)
	StateLoad() applier.State
	StateSave(state applier.State) error
	RollbackFrom(stashDir, configRoot string) applier.Result
	PruneStashDirs(keep int, exclude string)
	MakeStashDir() (string, error)
}

type realApplier struct {
	cfg applier.Config
}

func (r *realApplier) Apply(
	ctx context.Context, changes []applier.Change, repoRoot, configRoot string, opts options.Options,
) (applier.Result, error) {
	return applier.Apply(ctx, r.cfg, changes, repoRoot, configRoot, opts, nil)
}

func (r *realApplier) StateLoad() applier.State { return applier.StateLoad(r.cfg) }
func (r *realApplier) StateSave(state applier.State) error {
	return applier.StateSave(r.cfg, state)
}

func (r *realApplier) RollbackFrom(stashDir, configRoot string) applier.Result {
	return applier.RollbackFrom(r.cfg, stashDir, configRoot)
}

func (r *realApplier) PruneStashDirs(keep int, exclude string) {
	applier.PruneStashDirs(r.cfg, keep, exclude)
}

func (r *realApplier) MakeStashDir() (string, error) { return applier.MakeStashDir(r.cfg) }

var _ Applier = (*realApplier)(nil)

// History is the internal/history seam: the durable per-run record under
// /data. Append returns its error, like Snapshot.PreApplyBackup - no
// caller aborts a run over it, but a read-only /data would otherwise lose
// every future restart's history silently. Load does not: internal/history
// degrades to fewer records rather than failing.
type History interface {
	Append(rec history.Record) error
	Load() []history.Record
}

// No real* wrapper, unlike the seams above: those front free functions
// needing a Config or dialer bound, while *history.Store already has these
// two methods.
var _ History = (*history.Store)(nil)

// Snapshot is the internal/snapshot seam: best-effort whole-system
// Supervisor backups around an apply. PreApplyBackup returns its error
// even though no caller aborts over one - best-effort describes what the
// caller does, not whether the user is told, and ApplyNow reports it.
// Prune has none: nothing observes whether an old backup was tidied.
type Snapshot interface {
	PreApplyBackup() (string, error)
	Prune(keep int)
}

type realSnapshot struct{}

func (realSnapshot) PreApplyBackup() (string, error) { return snapshot.PreApplyBackup(nil) }
func (realSnapshot) Prune(keep int)                  { snapshot.Prune(keep, nil) }

var _ Snapshot = realSnapshot{}

// StatusPusher is the internal/statusd seam: push the agent's current
// state to sensor.gitops_agent_status.
type StatusPusher interface {
	Push(state string, attrs map[string]any) (bool, error)
}

type realStatusPusher struct{}

func (realStatusPusher) Push(state string, attrs map[string]any) (bool, error) {
	return statusd.Push(state, attrs, nil)
}

var _ StatusPusher = realStatusPusher{}

// Registries is the internal/registries seam: load the gitops/ manifests
// and plan a reconciliation against live state.
type Registries interface {
	LoadManifests(workdir string) (registries.Desired, error)
	Plan(desired registries.Desired, live map[string][]map[string]any, managed map[string]string) []registries.RegOp
}

type realRegistries struct{}

func (realRegistries) LoadManifests(workdir string) (registries.Desired, error) {
	return registries.LoadManifests(workdir)
}

func (realRegistries) Plan(
	desired registries.Desired, live map[string][]map[string]any, managed map[string]string,
) []registries.RegOp {
	return registries.Plan(desired, live, managed)
}

var _ Registries = realRegistries{}

// Entities is the internal/entities seam: load gitops/entities.yaml and
// plan against live state. Separate from Registries because entities.Plan
// needs a per-field originals snapshot and a RefResolver, not a live-id
// managed map.
type Entities interface {
	LoadManifest(workdir string) (entities.Desired, error)
	Plan(
		desired entities.Desired, liveEntities []map[string]any,
		originals map[string]map[string]any, refs entities.RefResolver,
	) []registries.RegOp
}

type realEntities struct{}

func (realEntities) LoadManifest(workdir string) (entities.Desired, error) {
	return entities.LoadManifest(workdir)
}

func (realEntities) Plan(
	desired entities.Desired, liveEntities []map[string]any,
	originals map[string]map[string]any, refs entities.RefResolver,
) []registries.RegOp {
	return entities.Plan(desired, liveEntities, originals, refs)
}

var _ Entities = realEntities{}

// Dashboards is the internal/dashboards seam: load gitops/dashboards.yaml
// plus each declared config file, and plan against live state. Separate
// because dashboards.Plan needs a live content map; its ownership rules
// are registries.Plan's, not entities' update-only model.
type Dashboards interface {
	LoadManifest(workdir string) (dashboards.Desired, error)
	Plan(
		desired dashboards.Desired, liveDashboards []map[string]any,
		liveContent map[string]map[string]any, managed map[string]string,
	) []registries.RegOp
}

type realDashboards struct{}

func (realDashboards) LoadManifest(workdir string) (dashboards.Desired, error) {
	return dashboards.LoadManifest(workdir)
}

func (realDashboards) Plan(
	desired dashboards.Desired, liveDashboards []map[string]any,
	liveContent map[string]map[string]any, managed map[string]string,
) []registries.RegOp {
	return dashboards.Plan(desired, liveDashboards, liveContent, managed)
}

var _ Dashboards = realDashboards{}

// AddonOpts is the internal/addonopts seam: load gitops/addons.yaml and
// plan against live Supervisor state. Separate because addonopts.Plan
// takes raw per-slug info plus the self-slug guard; its ownership rules
// are entities.Plan's UPDATE-ONLY model.
type AddonOpts interface {
	LoadManifest(workdir string) (addonopts.Desired, error)
	Plan(
		desired addonopts.Desired, live map[string]map[string]any,
		originals map[string]map[string]any, selfSlug string, secrets *secretref.Resolver,
	) []registries.RegOp
}

type realAddonOpts struct{}

func (realAddonOpts) LoadManifest(workdir string) (addonopts.Desired, error) {
	return addonopts.LoadManifest(workdir)
}

func (realAddonOpts) Plan(
	desired addonopts.Desired, live map[string]map[string]any,
	originals map[string]map[string]any, selfSlug string, secrets *secretref.Resolver,
) []registries.RegOp {
	return addonopts.Plan(desired, live, originals, selfSlug, secrets)
}

var _ AddonOpts = realAddonOpts{}

// Flows is the internal/flows seam: load gitops/integrations.yaml and plan
// against live config entries. Separate because flows.Plan takes a live
// entry list plus a per-key hash map; its ownership rules are close to
// registries.Plan's, with one asymmetry (see flows.Plan).
type Flows interface {
	LoadManifest(workdir string) (flows.Desired, error)
	Plan(
		desired flows.Desired, liveEntries []map[string]any,
		managed map[string]string, hashes map[string]string, attempts map[string]map[string]any,
		secrets *secretref.Resolver,
	) []registries.RegOp
}

type realFlows struct{}

func (realFlows) LoadManifest(workdir string) (flows.Desired, error) {
	return flows.LoadManifest(workdir)
}

func (realFlows) Plan(
	desired flows.Desired, liveEntries []map[string]any,
	managed map[string]string, hashes map[string]string, attempts map[string]map[string]any,
	secrets *secretref.Resolver,
) []registries.RegOp {
	return flows.Plan(desired, liveEntries, managed, hashes, attempts, secrets)
}

var _ Flows = realFlows{}

// Subentries is the internal/subentries seam: load gitops/subentries.yaml
// and plan against the subentries hanging off live config entries. Borrows
// Flows' hash-based drift model, plus one argument: a live entry list says
// nothing about a parent's subentries, so those are fetched separately.
type Subentries interface {
	LoadManifest(workdir string) (subentries.Desired, error)
	Plan(
		desired subentries.Desired, liveEntries []map[string]any,
		liveSubentriesByEntryID map[string][]map[string]any,
		managed map[string]string, hashes map[string]string, attempts map[string]map[string]any,
		secrets *secretref.Resolver,
	) []registries.RegOp
}

type realSubentries struct{}

func (realSubentries) LoadManifest(workdir string) (subentries.Desired, error) {
	return subentries.LoadManifest(workdir)
}

func (realSubentries) Plan(
	desired subentries.Desired, liveEntries []map[string]any,
	liveSubentriesByEntryID map[string][]map[string]any,
	managed map[string]string, hashes map[string]string, attempts map[string]map[string]any,
	secrets *secretref.Resolver,
) []registries.RegOp {
	return subentries.Plan(desired, liveEntries, liveSubentriesByEntryID, managed, hashes, attempts, secrets)
}

var _ Subentries = realSubentries{}

// Hacs is the internal/hacs seam: load gitops/hacs.yaml and plan the
// integrations it declares against what HACS has. The narrowest Plan here
// - a repository list and two bookkeeping maps, no hashes and no secret
// resolver - because nothing in this manifest is a data payload.
type Hacs interface {
	LoadManifest(workdir string) (hacs.Desired, error)
	Plan(
		desired hacs.Desired, liveRepos []map[string]any,
		managed map[string]string, attempts map[string]map[string]any,
	) []registries.RegOp
}

type realHacs struct{}

func (realHacs) LoadManifest(workdir string) (hacs.Desired, error) {
	return hacs.LoadManifest(workdir)
}

func (realHacs) Plan(
	desired hacs.Desired, liveRepos []map[string]any,
	managed map[string]string, attempts map[string]map[string]any,
) []registries.RegOp {
	return hacs.Plan(desired, liveRepos, managed, attempts)
}

var _ Hacs = realHacs{}

// RegistryApplier is the internal/regapply seam: fetch live registry
// state, execute a plan, and roll one back. ApplyPlan and RollbackRegistry
// take a Dialer and own their dial/redial lifecycle, because
// coder/websocket closes the connection on ANY error, a timeout included.
// realRegistryApplier owns that Dialer; FetchLive needs no redial and
// dials once.
type RegistryApplier interface {
	// FetchLive fetches every live registry/helper object, plus every live
	// entity when includeEntities is set (gated separately - see
	// regapply.FetchLive).
	FetchLive(ctx context.Context, includeEntities bool) (map[string][]map[string]any, error)
	ApplyPlan(ctx context.Context, plan []registries.RegOp, managed map[string]string, stashDir string) regapply.RegistryApplyResult
	// ApplyEntityPlan is ApplyPlan's sibling for internal/entities' ops.
	ApplyEntityPlan(
		ctx context.Context, ops []registries.RegOp, originals map[string]map[string]any, stashDir string,
	) regapply.RegistryApplyResult
	// FetchLiveDashboards fetches every dashboard's metadata, plus the
	// saved content of each id in ids.
	FetchLiveDashboards(ctx context.Context, ids []string) ([]map[string]any, map[string]map[string]any, error)
	// ApplyDashboardPlan is ApplyPlan's sibling for internal/dashboards.
	ApplyDashboardPlan(
		ctx context.Context, ops []registries.RegOp, dashboardManaged map[string]string, stashDir string,
	) regapply.RegistryApplyResult
	RollbackRegistry(
		ctx context.Context, stashDir string,
		managed map[string]string, originals map[string]map[string]any, dashboardManaged map[string]string,
	) regapply.RegistryApplyResult

	// FetchAddonInfoAll fetches GET /addons/<slug>/info per slug - see
	// regapply.FetchAddonInfoAll for each entry's shape.
	FetchAddonInfoAll(ctx context.Context, slugs []string) (map[string]map[string]any, error)
	// FetchSelfAddonSlug resolves the Supervisor slug of the add-on making
	// this call.
	FetchSelfAddonSlug(ctx context.Context) (string, error)
	// ApplyAddonPlan is ApplyPlan's sibling for internal/addonopts' ops.
	// Alone among them it has no shared WS stash to prefix, keeping its own
	// addon_stash.json.
	ApplyAddonPlan(
		ctx context.Context, ops []registries.RegOp, declaredRestartOnChange map[string]bool,
		originals map[string]map[string]any, restartOnChangeState map[string]bool, stashDir string,
	) regapply.RegistryApplyResult
	// RollbackAddonPlan is RollbackRegistry's sibling for addon_stash.json.
	RollbackAddonPlan(
		ctx context.Context, stashDir string,
		originals map[string]map[string]any, restartOnChangeState map[string]bool,
	) regapply.RegistryApplyResult

	// FetchAddonUpdateInfo reads one add-on's installed version, the newest
	// the store offers, and Supervisor's verdict on the two. Separate from
	// FetchAddonInfoAll despite the same endpoint: that returns raw maps
	// for the options layer to diff and fails all-or-nothing.
	FetchAddonUpdateInfo(ctx context.Context, slug string) (regapply.AddonUpdateInfo, error)
	// UpdateAddon updates one add-on to the store's newest version, taking
	// a partial backup first, and returns once Supervisor reports it done
	// (see regapply.UpdateAddon for the budget).
	UpdateAddon(ctx context.Context, slug string) error
	// FetchInstalledAddons lists every installed add-on and its version in
	// one call, for the version record, which asks about the whole box
	// rather than the slugs auto_update_addons names.
	FetchInstalledAddons(ctx context.Context) ([]regapply.InstalledAddon, error)

	// FetchIntegrationEntries fetches every live config entry - see
	// regapply.FetchIntegrationEntries for each entry's shape.
	FetchIntegrationEntries(ctx context.Context) ([]map[string]any, error)
	// ApplyFlowPlan is ApplyPlan's sibling for internal/flows' ops. Like
	// ApplyAddonPlan it keeps its own integration_stash.json.
	ApplyFlowPlan(
		ctx context.Context, ops []registries.RegOp,
		managed map[string]string, hashes map[string]string, dataSnapshots map[string]map[string]any,
		attempts map[string]map[string]any, stashDir string,
	) regapply.RegistryApplyResult
	// RollbackFlowPlan is RollbackRegistry's sibling for
	// integration_stash.json. It takes a secret resolver because a stashed
	// delete replays declared data as WRITTEN, references included.
	RollbackFlowPlan(
		ctx context.Context, stashDir string,
		managed map[string]string, hashes map[string]string, dataSnapshots map[string]map[string]any,
		secrets *secretref.Resolver,
	) regapply.RegistryApplyResult

	// FetchSubentries fetches the subentries hanging off each of entryIDs -
	// see regapply.FetchSubentries for their shape and for why it is a
	// separate call from FetchIntegrationEntries.
	FetchSubentries(ctx context.Context, entryIDs []string) (map[string][]map[string]any, error)
	// ApplySubentryPlan is ApplyFlowPlan's sibling for
	// internal/subentries' ops. Alone among the Apply*Plan methods it takes
	// no stash directory: a subentry's configured data is unreadable, so
	// there is nothing to stash and no RollbackSubentryPlan either.
	ApplySubentryPlan(
		ctx context.Context, ops []registries.RegOp,
		managed map[string]string, hashes map[string]string, attempts map[string]map[string]any,
	) regapply.RegistryApplyResult

	// FetchHacsLive reads what HACS has and what HA has loaded, in one dial
	// (see regapply.FetchHacsLive for a box without HACS). The request
	// carries this cycle's manifest and state, which is what lets it read
	// one repository at a time instead of the whole store.
	FetchHacsLive(ctx context.Context, req regapply.HacsFetchRequest) (regapply.HacsLive, error)
	// ApplyHacsPlan is ApplySubentryPlan's sibling for internal/hacs' ops.
	// No stash directory - this layer never uninstalls, so nothing it does
	// has an inverse - and a POINTER to the restart-reminder list, since a
	// slice cannot grow in place the way the maps beside it do.
	ApplyHacsPlan(
		ctx context.Context, ops []registries.RegOp,
		managed map[string]string, attempts map[string]map[string]any, restartPending *[]string,
	) regapply.RegistryApplyResult
}

type realRegistryApplier struct {
	dialer regapply.Dialer
}

func newRealRegistryApplier(dialer regapply.Dialer) *realRegistryApplier {
	return &realRegistryApplier{dialer: dialer}
}

func (r *realRegistryApplier) FetchLive(ctx context.Context, includeEntities bool) (map[string][]map[string]any, error) {
	ws, err := r.dialer(ctx)
	if err != nil {
		return nil, err
	}
	defer ws.Close()
	return regapply.FetchLive(ctx, ws, includeEntities)
}

func (r *realRegistryApplier) ApplyPlan(
	ctx context.Context, plan []registries.RegOp, managed map[string]string, stashDir string,
) regapply.RegistryApplyResult {
	return regapply.ApplyPlan(ctx, r.dialer, plan, managed, stashDir)
}

func (r *realRegistryApplier) ApplyEntityPlan(
	ctx context.Context, ops []registries.RegOp, originals map[string]map[string]any, stashDir string,
) regapply.RegistryApplyResult {
	return regapply.ApplyEntityPlan(ctx, r.dialer, ops, originals, stashDir)
}

func (r *realRegistryApplier) FetchLiveDashboards(
	ctx context.Context, ids []string,
) ([]map[string]any, map[string]map[string]any, error) {
	ws, err := r.dialer(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer ws.Close()
	return regapply.FetchLiveDashboards(ctx, ws, ids)
}

func (r *realRegistryApplier) ApplyDashboardPlan(
	ctx context.Context, ops []registries.RegOp, dashboardManaged map[string]string, stashDir string,
) regapply.RegistryApplyResult {
	return regapply.ApplyDashboardPlan(ctx, r.dialer, ops, dashboardManaged, stashDir)
}

func (r *realRegistryApplier) RollbackRegistry(
	ctx context.Context, stashDir string,
	managed map[string]string, originals map[string]map[string]any, dashboardManaged map[string]string,
) regapply.RegistryApplyResult {
	return regapply.RollbackRegistry(ctx, r.dialer, stashDir, managed, originals, dashboardManaged)
}

// The add-on methods below go over Supervisor's REST API, not the WS
// dialer the rest of this type uses (see regapply's addonopts.go). nil
// means "use regapply.DefaultAddonHTTPClient".
func (r *realRegistryApplier) FetchAddonInfoAll(ctx context.Context, slugs []string) (map[string]map[string]any, error) {
	return regapply.FetchAddonInfoAll(ctx, nil, slugs)
}

func (r *realRegistryApplier) FetchSelfAddonSlug(ctx context.Context) (string, error) {
	return regapply.FetchSelfAddonSlug(ctx, nil)
}

func (r *realRegistryApplier) ApplyAddonPlan(
	ctx context.Context, ops []registries.RegOp, declaredRestartOnChange map[string]bool,
	originals map[string]map[string]any, restartOnChangeState map[string]bool, stashDir string,
) regapply.RegistryApplyResult {
	return regapply.ApplyAddonPlan(ctx, nil, ops, declaredRestartOnChange, originals, restartOnChangeState, stashDir)
}

func (r *realRegistryApplier) RollbackAddonPlan(
	ctx context.Context, stashDir string,
	originals map[string]map[string]any, restartOnChangeState map[string]bool,
) regapply.RegistryApplyResult {
	return regapply.RollbackAddonPlan(ctx, nil, stashDir, originals, restartOnChangeState)
}

func (r *realRegistryApplier) FetchAddonUpdateInfo(ctx context.Context, slug string) (regapply.AddonUpdateInfo, error) {
	return regapply.FetchAddonUpdateInfo(ctx, nil, slug)
}

func (r *realRegistryApplier) UpdateAddon(ctx context.Context, slug string) error {
	return regapply.UpdateAddon(ctx, nil, slug)
}

func (r *realRegistryApplier) FetchInstalledAddons(ctx context.Context) ([]regapply.InstalledAddon, error) {
	return regapply.FetchInstalledAddons(ctx, nil)
}

// FetchIntegrationEntries/ApplyFlowPlan/RollbackFlowPlan go over Core's
// REST API through the Supervisor proxy (see regapply's flows.go); nil
// means "use regapply.DefaultIntegrationHTTPClient". The two that create
// entries also take the WS dialer, for the one command with no REST route:
// writing a created entry's declared title.
func (r *realRegistryApplier) FetchIntegrationEntries(ctx context.Context) ([]map[string]any, error) {
	return regapply.FetchIntegrationEntries(ctx, nil)
}

func (r *realRegistryApplier) ApplyFlowPlan(
	ctx context.Context, ops []registries.RegOp,
	managed map[string]string, hashes map[string]string, dataSnapshots map[string]map[string]any,
	attempts map[string]map[string]any, stashDir string,
) regapply.RegistryApplyResult {
	return regapply.ApplyFlowPlan(ctx, nil, r.dialer, ops, managed, hashes, dataSnapshots, attempts, stashDir)
}

func (r *realRegistryApplier) RollbackFlowPlan(
	ctx context.Context, stashDir string,
	managed map[string]string, hashes map[string]string, dataSnapshots map[string]map[string]any,
	secrets *secretref.Resolver,
) regapply.RegistryApplyResult {
	return regapply.RollbackFlowPlan(ctx, nil, r.dialer, stashDir, managed, hashes, dataSnapshots, secrets)
}

// FetchSubentries takes the dialer alone: no REST route lists a parent's
// subentries. ApplySubentryPlan drives flows over REST like the flows
// layer but needs the dialer too, to find the subentry a create flow just
// made. nil means "use regapply.DefaultIntegrationHTTPClient".
func (r *realRegistryApplier) FetchSubentries(
	ctx context.Context, entryIDs []string,
) (map[string][]map[string]any, error) {
	return regapply.FetchSubentries(ctx, r.dialer, entryIDs)
}

func (r *realRegistryApplier) ApplySubentryPlan(
	ctx context.Context, ops []registries.RegOp,
	managed map[string]string, hashes map[string]string, attempts map[string]map[string]any,
) regapply.RegistryApplyResult {
	return regapply.ApplySubentryPlan(ctx, nil, r.dialer, ops, managed, hashes, attempts)
}

// Both HACS calls speak HACS's own WebSocket command family, which has no
// REST route at all, so both take the dialer alone (see regapply/hacs.go).
func (r *realRegistryApplier) FetchHacsLive(
	ctx context.Context, req regapply.HacsFetchRequest,
) (regapply.HacsLive, error) {
	return regapply.FetchHacsLive(ctx, r.dialer, req)
}

func (r *realRegistryApplier) ApplyHacsPlan(
	ctx context.Context, ops []registries.RegOp,
	managed map[string]string, attempts map[string]map[string]any, restartPending *[]string,
) regapply.RegistryApplyResult {
	return regapply.ApplyHacsPlan(ctx, r.dialer, ops, managed, attempts, restartPending)
}

var _ RegistryApplier = (*realRegistryApplier)(nil)

// Deps bundles every collaborator Reconciler needs. A nil field is filled
// in by New with the real production implementation.
type Deps struct {
	Git             Git
	Differ          Differ
	Applier         Applier
	Snapshot        Snapshot
	Status          StatusPusher
	Registries      Registries
	RegistryApplier RegistryApplier
	Entities        Entities
	Dashboards      Dashboards
	AddonOpts       AddonOpts
	Flows           Flows
	Subentries      Subentries
	Hacs            Hacs
	History         History

	// Crypter is the age identity from the age_key option, or nil. Unlike
	// the seams above it has no real implementation to fall back on: nil is
	// a supported configuration meaning encryption is off, and New must not
	// invent one.
	//
	// One field rather than three constructor arguments because it reaches
	// Git (encrypt), Differ and Applier (decrypt) at once, and a build
	// where only some of them got the key is the shape of every bad
	// outcome. Ignored when the seam is supplied explicitly, so an injected
	// collaborator is never half-configured.
	Crypter *sopscrypt.Crypter
}
