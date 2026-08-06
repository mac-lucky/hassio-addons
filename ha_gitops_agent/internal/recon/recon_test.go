package recon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
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
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/subentries"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/wsclient"
)

// --- fakes ---------------------------------------------------------------
//
// One per Reconciler collaborator. Fakes and real implementations are
// pinned to the same interfaces in deps.go, so a mismatch fails to build.

func baseOpts() options.Options {
	return options.Options{
		RepoURL:            "https://example.invalid/demo.git",
		Branch:             "main",
		IntervalMinutes:    5,
		DryRun:             true,
		ApplyAfterPull:     "off",
		ReconcileYAMLFiles: true,
	}
}

// hasEventContaining reports whether any activity event carries sub. The
// web UI re-renders the same page, so the log is what marks a refusal.
func hasEventContaining(events []Event, sub string) bool {
	for _, e := range events {
		if strings.Contains(e.Message, sub) {
			return true
		}
	}
	return false
}

type commitBackCall struct {
	files      []gitsync.DriftFile
	configRoot string
	baseSHA    string
}

type importCall struct {
	configRoot string
	limits     gitsync.ImportLimits
}

type fakeGit struct {
	sha               string
	tracked           []string
	trackedRaw        []string
	secretsErr        error
	ensureCloneErr    error
	fetchErr          error
	trackedErr        error
	trackedRawErr     error
	checkoutErr       error
	ensureCloneCalls  int
	fetchCalls        int
	checkoutCalls     []string
	guardSecretsCalls [][]string

	commitBackBranch string
	commitBackErr    error
	commitBackCalls  []commitBackCall

	importResult  gitsync.ImportResult
	importErr     error
	importCalls   []importCall
	scanLivePlan  gitsync.ImportPlan
	scanLiveErr   error
	scanLiveCalls []importCall

	previewIgnored          map[string]bool
	previewIgnoredKeptBytes int64
	previewIgnoredErr       error

	recordFileErr   error
	recordFileCalls []recordFileCall
	// recorded stands in for the repository's copy of each path, so this
	// fake keeps RecordFile's contract: the same bytes twice commit once.
	recorded map[string][]byte

	// workdir is what Workdir() returns: "/data/repo" for tests that only
	// assert the path, a real temp dir for those driving a real layer.
	workdir string
}

type recordFileCall struct {
	relPath string
	content []byte
	message string
}

func newFakeGit() *fakeGit {
	return &fakeGit{sha: "deadbeef", tracked: []string{"automations.yaml"}, workdir: "/data/repo"}
}

func (f *fakeGit) EnsureClone(ctx context.Context) error {
	f.ensureCloneCalls++
	return f.ensureCloneErr
}

func (f *fakeGit) Fetch(ctx context.Context) (string, error) {
	f.fetchCalls++
	if f.fetchErr != nil {
		return "", f.fetchErr
	}
	return f.sha, nil
}

func (f *fakeGit) Checkout(ctx context.Context, sha string) error {
	f.checkoutCalls = append(f.checkoutCalls, sha)
	return f.checkoutErr
}

func (f *fakeGit) CurrentSHA(ctx context.Context) string { return f.sha }

func (f *fakeGit) TrackedFiles(ctx context.Context, sha string) ([]string, error) {
	if f.trackedErr != nil {
		return nil, f.trackedErr
	}
	return f.tracked, nil
}

func (f *fakeGit) TrackedFilesRaw(ctx context.Context, sha string) ([]string, error) {
	if f.trackedRawErr != nil {
		return nil, f.trackedRawErr
	}
	if f.trackedRaw != nil {
		return f.trackedRaw, nil
	}
	return f.tracked, nil
}

func (f *fakeGit) GuardSecretsAt(ctx context.Context, sha string, files []string) error {
	f.guardSecretsCalls = append(f.guardSecretsCalls, files)
	return f.secretsErr
}

func (f *fakeGit) Workdir() string { return f.workdir }

func (f *fakeGit) CommitBack(ctx context.Context, files []gitsync.DriftFile, configRoot, baseSHA string, now time.Time) (string, error) {
	f.commitBackCalls = append(f.commitBackCalls, commitBackCall{files: files, configRoot: configRoot, baseSHA: baseSHA})
	if f.commitBackErr != nil {
		return "", f.commitBackErr
	}
	branch := f.commitBackBranch
	if branch == "" {
		branch = "gitops/drift-20260101T000000Z"
	}
	return branch, nil
}

func (f *fakeGit) Import(ctx context.Context, configRoot string, limits gitsync.ImportLimits, now time.Time) (gitsync.ImportResult, error) {
	f.importCalls = append(f.importCalls, importCall{configRoot: configRoot, limits: limits})
	if f.importErr != nil {
		return gitsync.ImportResult{}, f.importErr
	}
	res := f.importResult
	if res.CommitSHA == "" {
		res.CommitSHA = "cafebabecafebabecafebabecafebabecafebabe"
	}
	return res, nil
}

func (f *fakeGit) ScanLive(configRoot string, limits gitsync.ImportLimits) (gitsync.ImportPlan, error) {
	f.scanLiveCalls = append(f.scanLiveCalls, importCall{configRoot: configRoot, limits: limits})
	if f.scanLiveErr != nil {
		return gitsync.ImportPlan{}, f.scanLiveErr
	}
	return f.scanLivePlan, nil
}

// PreviewIgnored defaults to "nothing is gitignored", answering with the
// plan's own total; the fields let a test say otherwise.
func (f *fakeGit) PreviewIgnored(_ context.Context, _ string, files []string) ([]string, int64, error) {
	if f.previewIgnoredErr != nil {
		return nil, 0, f.previewIgnoredErr
	}
	if f.previewIgnored == nil {
		return files, f.scanLivePlan.TotalBytes, nil
	}
	var kept []string
	for _, p := range files {
		if !f.previewIgnored[p] {
			kept = append(kept, p)
		}
	}
	return kept, f.previewIgnoredKeptBytes, nil
}

func (f *fakeGit) RecordFile(_ context.Context, relPath string, content []byte, message string) (bool, error) {
	f.recordFileCalls = append(f.recordFileCalls, recordFileCall{
		relPath: relPath, content: append([]byte(nil), content...), message: message,
	})
	if f.recordFileErr != nil {
		return false, f.recordFileErr
	}
	// Presence separately from equality: bytes.Equal cannot tell a missing
	// entry from an empty recording, and the real RecordFile can.
	if previous, recorded := f.recorded[relPath]; recorded && bytes.Equal(previous, content) {
		return false, nil
	}
	if f.recorded == nil {
		f.recorded = map[string][]byte{}
	}
	f.recorded[relPath] = append([]byte(nil), content...)
	return true, nil
}

var _ Git = (*fakeGit)(nil)

type fakeDiffer struct {
	changes            []differ.Change
	skippedContainment []string
	decryptFailures    []string
	computeCalls       int
	panicOnCompute     bool
	// beforeCompute runs inside the cycle, where a real diff spends most of
	// its time: the seam for anything that must happen mid-cycle.
	beforeCompute func()
}

func (f *fakeDiffer) Compute(repoRoot, configRoot string, tracked, prevManifest []string) ([]differ.Change, []string, []string) {
	f.computeCalls++
	if f.beforeCompute != nil {
		f.beforeCompute()
	}
	if f.panicOnCompute {
		panic("boom")
	}
	return f.changes, f.skippedContainment, f.decryptFailures
}

var _ Differ = (*fakeDiffer)(nil)

type fakeApplier struct {
	applyResult    applier.Result
	applyErr       error
	dynamicChanged bool
	rollbackResult applier.Result
	state          applier.State

	applyCalls          [][]applier.Change
	applyCtxs           []context.Context
	stateSaveCalls      []applier.State
	stateSaveErr        error
	rollbackCalls       []string
	pruneStashDirsCalls []string
	makeStashDirCalls   int
	makeStashDirResult  string
	makeStashDirErr     error
}

func newFakeApplier() *fakeApplier {
	return &fakeApplier{
		applyResult: applier.Result{
			OK: true, Changed: []string{"automations.yaml"}, StashDir: "/data/backup/x",
		},
		rollbackResult:     applier.Result{OK: true, RolledBack: true},
		state:              applier.State{Manifest: []string{}, RegistryManaged: map[string]string{}},
		makeStashDirResult: "/data/backup/registry-only",
	}
}

func (f *fakeApplier) Apply(
	ctx context.Context, changes []applier.Change, repoRoot, configRoot string, opts options.Options,
) (applier.Result, error) {
	f.applyCalls = append(f.applyCalls, changes)
	f.applyCtxs = append(f.applyCtxs, ctx)
	if f.applyErr != nil {
		return applier.Result{}, f.applyErr
	}
	if f.dynamicChanged {
		paths := make([]string, len(changes))
		for i, c := range changes {
			paths[i] = c.Path
		}
		res := f.applyResult
		res.Changed = paths
		return res, nil
	}
	return f.applyResult, nil
}

func (f *fakeApplier) StateLoad() applier.State { return f.state }

func (f *fakeApplier) StateSave(state applier.State) error {
	f.stateSaveCalls = append(f.stateSaveCalls, state)
	if f.stateSaveErr != nil {
		return f.stateSaveErr
	}
	f.state = state
	return nil
}

func (f *fakeApplier) RollbackFrom(stashDir, configRoot string) applier.Result {
	f.rollbackCalls = append(f.rollbackCalls, stashDir)
	return f.rollbackResult
}

func (f *fakeApplier) PruneStashDirs(keep int, exclude string) {
	f.pruneStashDirsCalls = append(f.pruneStashDirsCalls, exclude)
}

func (f *fakeApplier) MakeStashDir() (string, error) {
	f.makeStashDirCalls++
	if f.makeStashDirErr != nil {
		return "", f.makeStashDirErr
	}
	return f.makeStashDirResult, nil
}

var _ Applier = (*fakeApplier)(nil)

// fakeHistory records appends and can be told to fail; loaded stands in
// for /data/history.jsonl. Mutexed: ApplyNow appends outside r.mu.
type fakeHistory struct {
	mu        sync.Mutex
	loaded    []history.Record
	appended  []history.Record
	appendErr error
	loadCalls int
}

func (f *fakeHistory) Append(rec history.Record) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.appendErr != nil {
		return f.appendErr
	}
	f.appended = append(f.appended, rec)
	return nil
}

func (f *fakeHistory) Load() []history.Record {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loadCalls++
	return append([]history.Record(nil), f.loaded...)
}

// records returns a copy of what has been appended so far.
func (f *fakeHistory) records() []history.Record {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]history.Record(nil), f.appended...)
}

// setAppendErr flips the store into failing, or back, mid-test.
func (f *fakeHistory) setAppendErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.appendErr = err
}

var _ History = (*fakeHistory)(nil)

type fakeSnapshot struct {
	backupCalls int
	pruneCalls  int
	// backupErr fails PreApplyBackup the way a large install does: no slug,
	// and an error the caller reports without aborting the apply.
	backupErr error
}

func (f *fakeSnapshot) PreApplyBackup() (string, error) {
	f.backupCalls++
	if f.backupErr != nil {
		return "", f.backupErr
	}
	return "backup-slug", nil
}
func (f *fakeSnapshot) Prune(keep int) { f.pruneCalls++ }

var _ Snapshot = (*fakeSnapshot)(nil)

type statusPush struct {
	state string
	attrs map[string]any
}

// fakeStatusPusher records every push. Push takes mu because the loop and
// web operations both push; direct field reads need no loop running.
type fakeStatusPusher struct {
	mu     sync.Mutex
	pushes []statusPush
	// block holds every push until closed, standing in for the ten seconds
	// statusd.Push may spend waiting on Supervisor.
	block chan struct{}
}

func (f *fakeStatusPusher) Push(state string, attrs map[string]any) (bool, error) {
	f.mu.Lock()
	f.pushes = append(f.pushes, statusPush{state: state, attrs: attrs})
	block := f.block
	f.mu.Unlock()

	// Outside the lock, so a held push does not wedge later ones unrecorded.
	if block != nil {
		<-block
	}
	return true, nil
}

var _ StatusPusher = (*fakeStatusPusher)(nil)

type planCall struct {
	desired registries.Desired
	live    map[string][]map[string]any
	managed map[string]string
}

type fakeRegistries struct {
	desired            registries.Desired
	planOps            []registries.RegOp
	manifestErr        error
	loadManifestsCalls []string
	planCalls          []planCall
}

func (f *fakeRegistries) LoadManifests(workdir string) (registries.Desired, error) {
	f.loadManifestsCalls = append(f.loadManifestsCalls, workdir)
	if f.manifestErr != nil {
		return registries.Desired{}, f.manifestErr
	}
	return f.desired, nil
}

func (f *fakeRegistries) Plan(
	desired registries.Desired, live map[string][]map[string]any, managed map[string]string,
) []registries.RegOp {
	f.planCalls = append(f.planCalls, planCall{desired: desired, live: live, managed: managed})
	return f.planOps
}

var _ Registries = (*fakeRegistries)(nil)

type applyPlanCall struct {
	plan     []registries.RegOp
	stashDir string
}

type applyEntityPlanCall struct {
	ops      []registries.RegOp
	stashDir string
}

type applyDashboardPlanCall struct {
	ops      []registries.RegOp
	stashDir string
}

type fakeRegistryApplier struct {
	live               map[string][]map[string]any
	fetchErr           error
	fetchLiveCalls     int
	fetchLiveIncEntity []bool

	applyResult    regapply.RegistryApplyResult
	applyPlanCalls []applyPlanCall

	applyEntityResult    regapply.RegistryApplyResult
	applyEntityPlanCalls []applyEntityPlanCall

	fetchDashboardsResult  []map[string]any
	fetchDashboardsContent map[string]map[string]any
	fetchDashboardsErr     error
	fetchDashboardsCalls   [][]string

	applyDashboardResult    regapply.RegistryApplyResult
	applyDashboardPlanCalls []applyDashboardPlanCall

	rollbackResult          regapply.RegistryApplyResult
	rollbackCalls           []string
	rollbackOriginals       []map[string]map[string]any
	rollbackDashboardManage []map[string]string

	fetchAddonInfoResult map[string]map[string]any
	fetchAddonInfoErr    error
	fetchAddonInfoCalls  [][]string

	fetchSelfAddonSlugResult string
	fetchSelfAddonSlugErr    error
	fetchSelfAddonSlugCalls  int

	applyAddonResult    regapply.RegistryApplyResult
	applyAddonPlanCalls []applyAddonPlanCall

	rollbackAddonResult          regapply.RegistryApplyResult
	rollbackAddonCalls           []string
	rollbackAddonOriginals       []map[string]map[string]any
	rollbackAddonRestartOnChange []map[string]bool

	// addonUpdateInfo is the fake Supervisor's per-slug view; an absent slug
	// comes back as regapply.ErrAddonNotInstalled. addonUpdateInfoErr wins.
	addonUpdateInfo       map[string]regapply.AddonUpdateInfo
	addonUpdateInfoErr    map[string]error
	fetchAddonUpdateCalls []string
	// onFetchAddonUpdateInfo runs at the top of every fetch, for tests that
	// need the answer to change (or the call to panic) mid-cycle.
	onFetchAddonUpdateInfo func(f *fakeRegistryApplier, slug string)

	updateAddonErr   map[string]error
	updateAddonCalls []string
	updateAddonCtxs  []context.Context

	// installedAddons is every add-on on the box, for the version record;
	// addonUpdateInfo above is keyed by the slugs auto_update_addons names.
	installedAddons        []regapply.InstalledAddon
	installedAddonsErr     error
	fetchInstalledCalls    int
	onFetchInstalledAddons func(f *fakeRegistryApplier)

	fetchIntegrationEntriesResult []map[string]any
	fetchIntegrationEntriesErr    error
	fetchIntegrationEntriesCalls  int

	applyFlowResult    regapply.RegistryApplyResult
	applyFlowPlanCalls []applyFlowPlanCall

	// onApplyFlowPlan writes into the four state maps the real ApplyFlowPlan
	// mutates in place. Mirrors onApplySubentryPlan.
	onApplyFlowPlan func(managed, hashes map[string]string, dataSnapshots, attempts map[string]map[string]any)

	rollbackFlowResult    regapply.RegistryApplyResult
	rollbackFlowCalls     []string
	rollbackFlowManaged   []map[string]string
	rollbackFlowHashes    []map[string]string
	rollbackFlowDataSnaps []map[string]map[string]any
	rollbackFlowSecrets   []*secretref.Resolver

	fetchSubentriesResult map[string][]map[string]any
	fetchSubentriesErr    error
	fetchSubentriesCalls  [][]string

	applySubentryResult    regapply.RegistryApplyResult
	applySubentryPlanCalls []applySubentryPlanCall
	// onApplySubentryPlan writes into the three state maps the real
	// ApplySubentryPlan mutates in place.
	onApplySubentryPlan func(managed, hashes map[string]string, attempts map[string]map[string]any)

	fetchHacsLiveResult regapply.HacsLive
	fetchHacsLiveErr    error
	fetchHacsLiveCalls  int
	// fetchHacsLiveRequests records what each fetch was told: the manifest,
	// the ownership records and the standing reminders.
	fetchHacsLiveRequests []regapply.HacsFetchRequest

	applyHacsResult    regapply.RegistryApplyResult
	applyHacsPlanCalls []applyHacsPlanCall
	// onApplyHacsPlan writes into the two maps and the slice the real
	// ApplyHacsPlan mutates in place; the slice needs a pointer.
	onApplyHacsPlan func(managed map[string]string, attempts map[string]map[string]any, restartPending *[]string)
}

type applyHacsPlanCall struct {
	ops            []registries.RegOp
	managed        map[string]string
	attempts       map[string]map[string]any
	restartPending []string
}

type applySubentryPlanCall struct {
	ops      []registries.RegOp
	managed  map[string]string
	hashes   map[string]string
	attempts map[string]map[string]any
}

type applyFlowPlanCall struct {
	ops           []registries.RegOp
	managed       map[string]string
	hashes        map[string]string
	dataSnapshots map[string]map[string]any
	attempts      map[string]map[string]any
	stashDir      string
}

type applyAddonPlanCall struct {
	ops                     []registries.RegOp
	declaredRestartOnChange map[string]bool
	restartOnChangeState    map[string]bool
	stashDir                string
}

func newFakeRegistryApplier() *fakeRegistryApplier {
	return &fakeRegistryApplier{
		applyResult:          regapply.RegistryApplyResult{OK: true},
		applyEntityResult:    regapply.RegistryApplyResult{OK: true},
		applyDashboardResult: regapply.RegistryApplyResult{OK: true},
		rollbackResult:       regapply.RegistryApplyResult{OK: true, RolledBack: true},
		applyAddonResult:     regapply.RegistryApplyResult{OK: true},
		rollbackAddonResult:  regapply.RegistryApplyResult{OK: true, RolledBack: true},
		applyFlowResult:      regapply.RegistryApplyResult{OK: true},
		rollbackFlowResult:   regapply.RegistryApplyResult{OK: true, RolledBack: true},
		applySubentryResult:  regapply.RegistryApplyResult{OK: true},
		applyHacsResult:      regapply.RegistryApplyResult{OK: true},
	}
}

func (f *fakeRegistryApplier) FetchLive(ctx context.Context, includeEntities bool) (map[string][]map[string]any, error) {
	f.fetchLiveCalls++
	f.fetchLiveIncEntity = append(f.fetchLiveIncEntity, includeEntities)
	if f.fetchErr != nil {
		return nil, f.fetchErr
	}
	if f.live != nil {
		return f.live, nil
	}
	return map[string][]map[string]any{}, nil
}

func (f *fakeRegistryApplier) ApplyPlan(
	ctx context.Context, plan []registries.RegOp, managed map[string]string, stashDir string,
) regapply.RegistryApplyResult {
	f.applyPlanCalls = append(f.applyPlanCalls, applyPlanCall{plan: plan, stashDir: stashDir})
	return f.applyResult
}

func (f *fakeRegistryApplier) ApplyEntityPlan(
	ctx context.Context, ops []registries.RegOp, originals map[string]map[string]any, stashDir string,
) regapply.RegistryApplyResult {
	f.applyEntityPlanCalls = append(f.applyEntityPlanCalls, applyEntityPlanCall{ops: ops, stashDir: stashDir})
	return f.applyEntityResult
}

func (f *fakeRegistryApplier) FetchLiveDashboards(
	ctx context.Context, ids []string,
) ([]map[string]any, map[string]map[string]any, error) {
	f.fetchDashboardsCalls = append(f.fetchDashboardsCalls, ids)
	if f.fetchDashboardsErr != nil {
		return nil, nil, f.fetchDashboardsErr
	}
	return f.fetchDashboardsResult, f.fetchDashboardsContent, nil
}

func (f *fakeRegistryApplier) ApplyDashboardPlan(
	ctx context.Context, ops []registries.RegOp, dashboardManaged map[string]string, stashDir string,
) regapply.RegistryApplyResult {
	f.applyDashboardPlanCalls = append(f.applyDashboardPlanCalls, applyDashboardPlanCall{ops: ops, stashDir: stashDir})
	return f.applyDashboardResult
}

func (f *fakeRegistryApplier) RollbackRegistry(
	ctx context.Context, stashDir string,
	managed map[string]string, originals map[string]map[string]any, dashboardManaged map[string]string,
) regapply.RegistryApplyResult {
	f.rollbackCalls = append(f.rollbackCalls, stashDir)
	f.rollbackOriginals = append(f.rollbackOriginals, originals)
	f.rollbackDashboardManage = append(f.rollbackDashboardManage, dashboardManaged)
	return f.rollbackResult
}

func (f *fakeRegistryApplier) FetchAddonInfoAll(ctx context.Context, slugs []string) (map[string]map[string]any, error) {
	f.fetchAddonInfoCalls = append(f.fetchAddonInfoCalls, slugs)
	if f.fetchAddonInfoErr != nil {
		return nil, f.fetchAddonInfoErr
	}
	if f.fetchAddonInfoResult != nil {
		return f.fetchAddonInfoResult, nil
	}
	return map[string]map[string]any{}, nil
}

func (f *fakeRegistryApplier) FetchSelfAddonSlug(ctx context.Context) (string, error) {
	f.fetchSelfAddonSlugCalls++
	if f.fetchSelfAddonSlugErr != nil {
		return "", f.fetchSelfAddonSlugErr
	}
	return f.fetchSelfAddonSlugResult, nil
}

func (f *fakeRegistryApplier) ApplyAddonPlan(
	ctx context.Context, ops []registries.RegOp, declaredRestartOnChange map[string]bool,
	originals map[string]map[string]any, restartOnChangeState map[string]bool, stashDir string,
) regapply.RegistryApplyResult {
	f.applyAddonPlanCalls = append(f.applyAddonPlanCalls, applyAddonPlanCall{
		ops: ops, declaredRestartOnChange: declaredRestartOnChange, restartOnChangeState: restartOnChangeState, stashDir: stashDir,
	})
	return f.applyAddonResult
}

func (f *fakeRegistryApplier) RollbackAddonPlan(
	ctx context.Context, stashDir string, originals map[string]map[string]any, restartOnChangeState map[string]bool,
) regapply.RegistryApplyResult {
	f.rollbackAddonCalls = append(f.rollbackAddonCalls, stashDir)
	f.rollbackAddonOriginals = append(f.rollbackAddonOriginals, originals)
	f.rollbackAddonRestartOnChange = append(f.rollbackAddonRestartOnChange, restartOnChangeState)
	return f.rollbackAddonResult
}

func (f *fakeRegistryApplier) FetchAddonUpdateInfo(ctx context.Context, slug string) (regapply.AddonUpdateInfo, error) {
	f.fetchAddonUpdateCalls = append(f.fetchAddonUpdateCalls, slug)
	if f.onFetchAddonUpdateInfo != nil {
		f.onFetchAddonUpdateInfo(f, slug)
	}
	if err := f.addonUpdateInfoErr[slug]; err != nil {
		return regapply.AddonUpdateInfo{}, err
	}
	info, ok := f.addonUpdateInfo[slug]
	if !ok {
		// Wrapped as the real fetch wraps it, so errors.Is is exercised.
		return regapply.AddonUpdateInfo{}, fmt.Errorf("add-on %s: %w", slug, regapply.ErrAddonNotInstalled)
	}
	info.Slug = slug
	return info, nil
}

func (f *fakeRegistryApplier) UpdateAddon(ctx context.Context, slug string) error {
	f.updateAddonCalls = append(f.updateAddonCalls, slug)
	f.updateAddonCtxs = append(f.updateAddonCtxs, ctx)
	if err := f.updateAddonErr[slug]; err != nil {
		return err
	}
	// What Supervisor does on success: the add-on lands on the offered
	// version. A stubborn one is faked through onFetchAddonUpdateInfo.
	if info, ok := f.addonUpdateInfo[slug]; ok {
		info.Version = info.VersionLatest
		info.UpdateAvailable = false
		f.addonUpdateInfo[slug] = info
	}
	return nil
}

func (f *fakeRegistryApplier) FetchInstalledAddons(ctx context.Context) ([]regapply.InstalledAddon, error) {
	f.fetchInstalledCalls++
	if f.onFetchInstalledAddons != nil {
		f.onFetchInstalledAddons(f)
	}
	if f.installedAddonsErr != nil {
		return nil, f.installedAddonsErr
	}
	return f.installedAddons, nil
}

func (f *fakeRegistryApplier) FetchIntegrationEntries(ctx context.Context) ([]map[string]any, error) {
	f.fetchIntegrationEntriesCalls++
	if f.fetchIntegrationEntriesErr != nil {
		return nil, f.fetchIntegrationEntriesErr
	}
	if f.fetchIntegrationEntriesResult != nil {
		return f.fetchIntegrationEntriesResult, nil
	}
	return []map[string]any{}, nil
}

func (f *fakeRegistryApplier) ApplyFlowPlan(
	ctx context.Context, ops []registries.RegOp,
	managed map[string]string, hashes map[string]string, dataSnapshots map[string]map[string]any,
	attempts map[string]map[string]any, stashDir string,
) regapply.RegistryApplyResult {
	f.applyFlowPlanCalls = append(f.applyFlowPlanCalls, applyFlowPlanCall{
		ops: ops, managed: managed, hashes: hashes, dataSnapshots: dataSnapshots, attempts: attempts, stashDir: stashDir,
	})
	if f.onApplyFlowPlan != nil {
		f.onApplyFlowPlan(managed, hashes, dataSnapshots, attempts)
	}
	return f.applyFlowResult
}

func (f *fakeRegistryApplier) RollbackFlowPlan(
	ctx context.Context, stashDir string,
	managed map[string]string, hashes map[string]string, dataSnapshots map[string]map[string]any,
	secrets *secretref.Resolver,
) regapply.RegistryApplyResult {
	f.rollbackFlowCalls = append(f.rollbackFlowCalls, stashDir)
	f.rollbackFlowSecrets = append(f.rollbackFlowSecrets, secrets)
	f.rollbackFlowManaged = append(f.rollbackFlowManaged, managed)
	f.rollbackFlowHashes = append(f.rollbackFlowHashes, hashes)
	f.rollbackFlowDataSnaps = append(f.rollbackFlowDataSnaps, dataSnapshots)
	return f.rollbackFlowResult
}

func (f *fakeRegistryApplier) FetchSubentries(
	ctx context.Context, entryIDs []string,
) (map[string][]map[string]any, error) {
	f.fetchSubentriesCalls = append(f.fetchSubentriesCalls, entryIDs)
	if f.fetchSubentriesErr != nil {
		return nil, f.fetchSubentriesErr
	}
	if f.fetchSubentriesResult != nil {
		return f.fetchSubentriesResult, nil
	}
	return map[string][]map[string]any{}, nil
}

func (f *fakeRegistryApplier) ApplySubentryPlan(
	ctx context.Context, ops []registries.RegOp,
	managed map[string]string, hashes map[string]string, attempts map[string]map[string]any,
) regapply.RegistryApplyResult {
	f.applySubentryPlanCalls = append(f.applySubentryPlanCalls, applySubentryPlanCall{
		ops: ops, managed: managed, hashes: hashes, attempts: attempts,
	})
	if f.onApplySubentryPlan != nil {
		f.onApplySubentryPlan(managed, hashes, attempts)
	}
	return f.applySubentryResult
}

func (f *fakeRegistryApplier) FetchHacsLive(
	ctx context.Context, req regapply.HacsFetchRequest,
) (regapply.HacsLive, error) {
	f.fetchHacsLiveCalls++
	f.fetchHacsLiveRequests = append(f.fetchHacsLiveRequests, req)
	if f.fetchHacsLiveErr != nil {
		return regapply.HacsLive{}, f.fetchHacsLiveErr
	}
	return f.fetchHacsLiveResult, nil
}

func (f *fakeRegistryApplier) ApplyHacsPlan(
	ctx context.Context, ops []registries.RegOp,
	managed map[string]string, attempts map[string]map[string]any, restartPending *[]string,
) regapply.RegistryApplyResult {
	f.applyHacsPlanCalls = append(f.applyHacsPlanCalls, applyHacsPlanCall{
		ops: ops, managed: managed, attempts: attempts,
		restartPending: append([]string{}, *restartPending...),
	})
	if f.onApplyHacsPlan != nil {
		f.onApplyHacsPlan(managed, attempts, restartPending)
	}
	return f.applyHacsResult
}

var _ RegistryApplier = (*fakeRegistryApplier)(nil)

type entityPlanCall struct {
	desired   entities.Desired
	live      []map[string]any
	originals map[string]map[string]any
	refs      entities.RefResolver
}

type fakeEntities struct {
	desired           entities.Desired
	planOps           []registries.RegOp
	manifestErr       error
	loadManifestCalls []string
	planCalls         []entityPlanCall
}

func (f *fakeEntities) LoadManifest(workdir string) (entities.Desired, error) {
	f.loadManifestCalls = append(f.loadManifestCalls, workdir)
	if f.manifestErr != nil {
		return entities.Desired{}, f.manifestErr
	}
	return f.desired, nil
}

func (f *fakeEntities) Plan(
	desired entities.Desired, liveEntities []map[string]any, originals map[string]map[string]any, refs entities.RefResolver,
) []registries.RegOp {
	f.planCalls = append(f.planCalls, entityPlanCall{desired: desired, live: liveEntities, originals: originals, refs: refs})
	return f.planOps
}

var _ Entities = (*fakeEntities)(nil)

type dashboardPlanCall struct {
	desired     dashboards.Desired
	live        []map[string]any
	liveContent map[string]map[string]any
	managed     map[string]string
}

type fakeDashboards struct {
	desired           dashboards.Desired
	planOps           []registries.RegOp
	manifestErr       error
	loadManifestCalls []string
	planCalls         []dashboardPlanCall
}

func (f *fakeDashboards) LoadManifest(workdir string) (dashboards.Desired, error) {
	f.loadManifestCalls = append(f.loadManifestCalls, workdir)
	if f.manifestErr != nil {
		return dashboards.Desired{}, f.manifestErr
	}
	return f.desired, nil
}

func (f *fakeDashboards) Plan(
	desired dashboards.Desired, liveDashboards []map[string]any,
	liveContent map[string]map[string]any, managed map[string]string,
) []registries.RegOp {
	f.planCalls = append(f.planCalls, dashboardPlanCall{desired: desired, live: liveDashboards, liveContent: liveContent, managed: managed})
	return f.planOps
}

var _ Dashboards = (*fakeDashboards)(nil)

type addonPlanCall struct {
	desired   addonopts.Desired
	live      map[string]map[string]any
	originals map[string]map[string]any
	selfSlug  string
	secrets   *secretref.Resolver
}

type fakeAddonOpts struct {
	desired           addonopts.Desired
	planOps           []registries.RegOp
	manifestErr       error
	loadManifestCalls []string
	planCalls         []addonPlanCall
	// resolveProbe makes Plan resolve "secret://<probe>" through the
	// resolver it was handed, as a real layer would.
	resolveProbe string
	resolved     []string
	resolveErr   error
}

func (f *fakeAddonOpts) LoadManifest(workdir string) (addonopts.Desired, error) {
	f.loadManifestCalls = append(f.loadManifestCalls, workdir)
	if f.manifestErr != nil {
		return addonopts.Desired{}, f.manifestErr
	}
	return f.desired, nil
}

func (f *fakeAddonOpts) Plan(
	desired addonopts.Desired, live map[string]map[string]any, originals map[string]map[string]any, selfSlug string,
	secrets *secretref.Resolver,
) []registries.RegOp {
	f.planCalls = append(f.planCalls, addonPlanCall{
		desired: desired, live: live, originals: originals, selfSlug: selfSlug, secrets: secrets,
	})
	f.resolved, f.resolveErr = probeResolve(secrets, f.resolveProbe)
	return f.planOps
}

var _ AddonOpts = (*fakeAddonOpts)(nil)

type flowPlanCall struct {
	desired  flows.Desired
	live     []map[string]any
	managed  map[string]string
	hashes   map[string]string
	attempts map[string]map[string]any
	secrets  *secretref.Resolver
}

type fakeFlows struct {
	desired           flows.Desired
	planOps           []registries.RegOp
	manifestErr       error
	loadManifestCalls []string
	planCalls         []flowPlanCall
	// See fakeAddonOpts.resolveProbe.
	resolveProbe string
	resolved     []string
	resolveErr   error
}

func (f *fakeFlows) LoadManifest(workdir string) (flows.Desired, error) {
	f.loadManifestCalls = append(f.loadManifestCalls, workdir)
	if f.manifestErr != nil {
		return flows.Desired{}, f.manifestErr
	}
	return f.desired, nil
}

func (f *fakeFlows) Plan(
	desired flows.Desired, liveEntries []map[string]any,
	managed map[string]string, hashes map[string]string, attempts map[string]map[string]any,
	secrets *secretref.Resolver,
) []registries.RegOp {
	f.planCalls = append(f.planCalls, flowPlanCall{
		desired: desired, live: liveEntries, managed: managed, hashes: hashes, attempts: attempts, secrets: secrets,
	})
	f.resolved, f.resolveErr = probeResolve(secrets, f.resolveProbe)
	return f.planOps
}

var _ Flows = (*fakeFlows)(nil)

type subentryPlanCall struct {
	desired  subentries.Desired
	live     []map[string]any
	liveSubs map[string][]map[string]any
	managed  map[string]string
	hashes   map[string]string
	attempts map[string]map[string]any
	secrets  *secretref.Resolver
}

type fakeSubentries struct {
	desired           subentries.Desired
	planOps           []registries.RegOp
	manifestErr       error
	loadManifestCalls []string
	planCalls         []subentryPlanCall
	// See fakeAddonOpts.resolveProbe.
	resolveProbe string
	resolved     []string
	resolveErr   error
}

func (f *fakeSubentries) LoadManifest(workdir string) (subentries.Desired, error) {
	f.loadManifestCalls = append(f.loadManifestCalls, workdir)
	if f.manifestErr != nil {
		return subentries.Desired{}, f.manifestErr
	}
	return f.desired, nil
}

func (f *fakeSubentries) Plan(
	desired subentries.Desired, liveEntries []map[string]any,
	liveSubentriesByEntryID map[string][]map[string]any,
	managed map[string]string, hashes map[string]string, attempts map[string]map[string]any,
	secrets *secretref.Resolver,
) []registries.RegOp {
	f.planCalls = append(f.planCalls, subentryPlanCall{
		desired: desired, live: liveEntries, liveSubs: liveSubentriesByEntryID,
		managed: managed, hashes: hashes, attempts: attempts, secrets: secrets,
	})
	f.resolved, f.resolveErr = probeResolve(secrets, f.resolveProbe)
	return f.planOps
}

// probeResolve resolves "secret://<probe>" through the resolver the cycle
// handed the planner, so a test can watch what resolution would do.
func probeResolve(secrets *secretref.Resolver, probe string) ([]string, error) {
	if probe == "" {
		return nil, nil
	}
	_, values, err := secrets.ResolveMap(map[string]any{"probe": secretref.Scheme + probe})
	return values, err
}

var _ Subentries = (*fakeSubentries)(nil)

type hacsPlanCall struct {
	desired  hacs.Desired
	live     []map[string]any
	managed  map[string]string
	attempts map[string]map[string]any
}

type fakeHacs struct {
	desired           hacs.Desired
	planOps           []registries.RegOp
	manifestErr       error
	loadManifestCalls []string
	planCalls         []hacsPlanCall
}

func (f *fakeHacs) LoadManifest(workdir string) (hacs.Desired, error) {
	f.loadManifestCalls = append(f.loadManifestCalls, workdir)
	if f.manifestErr != nil {
		return hacs.Desired{}, f.manifestErr
	}
	return f.desired, nil
}

func (f *fakeHacs) Plan(
	desired hacs.Desired, liveRepos []map[string]any,
	managed map[string]string, attempts map[string]map[string]any,
) []registries.RegOp {
	f.planCalls = append(f.planCalls, hacsPlanCall{
		desired: desired, live: liveRepos, managed: managed, attempts: attempts,
	})
	return f.planOps
}

var _ Hacs = (*fakeHacs)(nil)

// reconcilerFakes bundles one instance of every fake collaborator.
type reconcilerFakes struct {
	git             *fakeGit
	differ          *fakeDiffer
	applier         *fakeApplier
	snapshot        *fakeSnapshot
	status          *fakeStatusPusher
	registries      *fakeRegistries
	registryApplier *fakeRegistryApplier
	entities        *fakeEntities
	dashboards      *fakeDashboards
	addonOpts       *fakeAddonOpts
	flows           *fakeFlows
	subentries      *fakeSubentries
	hacs            *fakeHacs
	history         *fakeHistory
}

func newReconcilerFakes() *reconcilerFakes {
	return &reconcilerFakes{
		git:             newFakeGit(),
		differ:          &fakeDiffer{},
		applier:         newFakeApplier(),
		snapshot:        &fakeSnapshot{},
		status:          &fakeStatusPusher{},
		registries:      &fakeRegistries{},
		registryApplier: newFakeRegistryApplier(),
		entities:        &fakeEntities{},
		dashboards:      &fakeDashboards{},
		addonOpts:       &fakeAddonOpts{},
		flows:           &fakeFlows{},
		subentries:      &fakeSubentries{},
		hacs:            &fakeHacs{},
		// Never nil: a nil History makes New open the real
		// /data/history.jsonl, whose write failure lands in the event log.
		history: &fakeHistory{},
	}
}

func (f *reconcilerFakes) reconciler(opts options.Options) *Reconciler {
	return New(opts, Deps{
		Git:             f.git,
		Differ:          f.differ,
		Applier:         f.applier,
		Snapshot:        f.snapshot,
		Status:          f.status,
		Registries:      f.registries,
		RegistryApplier: f.registryApplier,
		Entities:        f.entities,
		Dashboards:      f.dashboards,
		AddonOpts:       f.addonOpts,
		Flows:           f.flows,
		Subentries:      f.subentries,
		Hacs:            f.hacs,
		History:         f.history,
	})
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// --- reconcile_now / status --------------------------------------------

func TestReconcileNowSetsDriftPendingWhenChangesExist(t *testing.T) {
	fakes := newReconcilerFakes()
	changes := []differ.Change{{Path: "automations.yaml", Kind: "update", DiffText: "+x\n-y"}}
	fakes.differ.changes = changes
	r := fakes.reconciler(baseOpts())

	result := r.ReconcileNow(context.Background())

	if !reflect.DeepEqual(result, changes) {
		t.Errorf("result = %+v, want %+v", result, changes)
	}
	status := r.Status()
	if status.State != StateDriftPending {
		t.Errorf("state = %q, want drift_pending", status.State)
	}
	if status.PendingCount != 1 {
		t.Errorf("pending_count = %d, want 1", status.PendingCount)
	}
}

// differ.Compute's symlink/containment refusal is otherwise only a
// slog.Warn per path, invisible on the dashboard and the sensor.
func TestReconcileNowLogsSkippedContainmentPaths(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.differ.changes = nil
	fakes.differ.skippedContainment = []string{"automations.yaml", "sub/scripts.yaml"}
	r := fakes.reconciler(baseOpts())

	r.ReconcileNow(context.Background())

	var found bool
	want := "skipped 2 non-regular/escaping path(s): automations.yaml, sub/scripts.yaml"
	for _, e := range r.Status().Events {
		if e.Message == want {
			found = true
		}
	}
	if !found {
		t.Errorf("no event %q; events = %+v", want, r.Status().Events)
	}
	// Informational only: no drift, no error.
	if got := r.Status().State; got != StateInSync {
		t.Errorf("state = %q, want in_sync unaffected by the containment skip", got)
	}
}

func TestReconcileNowNoEventWhenNothingSkipped(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.differ.changes = nil
	fakes.differ.skippedContainment = nil
	r := fakes.reconciler(baseOpts())

	r.ReconcileNow(context.Background())

	if events := r.Status().Events; hasEventContaining(events, "non-regular/escaping") {
		t.Errorf("unexpected containment-skip event; events = %+v", events)
	}
}

func TestReconcileNowSetsInSyncWhenNoChanges(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.differ.changes = nil
	r := fakes.reconciler(baseOpts())

	result := r.ReconcileNow(context.Background())

	if len(result) != 0 {
		t.Errorf("result = %+v, want none", result)
	}
	if got := r.Status().State; got != StateInSync {
		t.Errorf("state = %q, want in_sync", got)
	}
}

func TestSecretsTrackedErrorSetsErrorAndSkipsCheckoutAndApply(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.git.secretsErr = &gitsync.SecretsTrackedError{Files: []string{"secrets.yaml"}}
	r := fakes.reconciler(baseOpts())

	r.ReconcileNow(context.Background())

	status := r.Status()
	if status.State != StateError {
		t.Errorf("state = %q, want error", status.State)
	}
	if want := "refusing to sync: secrets tracked in repository: secrets.yaml"; status.LastError != want {
		t.Errorf("last_error = %q, want %q", status.LastError, want)
	}
	if len(fakes.git.checkoutCalls) != 0 {
		t.Errorf("checkout_calls = %v, want none", fakes.git.checkoutCalls)
	}
	if len(fakes.applier.applyCalls) != 0 {
		t.Errorf("apply_calls = %v, want none", fakes.applier.applyCalls)
	}
}

// Content the agent cannot decrypt makes every verdict about those paths
// a guess, so the cycle errors rather than applying the half it read.
func TestDecryptFailureFailsTheCycleWithoutApplying(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.differ.changes = []differ.Change{{Path: "automations.yaml", Kind: "update", DiffText: "+x"}}
	fakes.differ.decryptFailures = []string{"secrets.yaml: sops decrypt failed (exit 1)"}
	r := fakes.reconciler(baseOpts())

	result := r.ReconcileNow(context.Background())

	if len(result) != 0 {
		t.Errorf("result = %+v, want no plan", result)
	}
	status := r.Status()
	if status.State != StateError {
		t.Errorf("state = %q, want error", status.State)
	}
	if want := "refusing to sync: secrets.yaml: sops decrypt failed (exit 1)"; status.LastError != want {
		t.Errorf("last_error = %q, want %q", status.LastError, want)
	}
	if len(fakes.applier.applyCalls) != 0 {
		t.Errorf("apply_calls = %v, want none", fakes.applier.applyCalls)
	}
}

func TestTickWithDryRunTrueNeverCallsApply(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.differ.changes = []differ.Change{{Path: "automations.yaml", Kind: "update", DiffText: "+x"}}
	opts := baseOpts()
	opts.DryRun = true
	r := fakes.reconciler(opts)

	r.tick(context.Background())

	if len(fakes.applier.applyCalls) != 0 {
		t.Errorf("apply_calls = %v, want none", fakes.applier.applyCalls)
	}
	if got := r.Status().State; got != StateDriftPending {
		t.Errorf("state = %q, want drift_pending", got)
	}
}

func TestTickWithDryRunFalseCallsApplyOncePerChangeSet(t *testing.T) {
	fakes := newReconcilerFakes()
	changes := []differ.Change{
		{Path: "automations.yaml", Kind: "update", DiffText: "+x"},
		{Path: "scripts.yaml", Kind: "add", DiffText: "+y"},
	}
	fakes.differ.changes = changes
	opts := baseOpts()
	opts.DryRun = false
	r := fakes.reconciler(opts)

	r.tick(context.Background())

	if len(fakes.applier.applyCalls) != 1 {
		t.Fatalf("apply_calls = %d, want 1", len(fakes.applier.applyCalls))
	}
	if len(fakes.applier.applyCalls[0]) != len(changes) {
		t.Errorf("apply_calls[0] len = %d, want %d", len(fakes.applier.applyCalls[0]), len(changes))
	}
	if got := r.Status().State; got != StateInSync {
		t.Errorf("state = %q, want in_sync", got)
	}
}

// applier.Apply reads the SIGTERM-cancelled ctx RunLoop passes down as a
// failed check_config, so only the apply step detaches from it.
func TestTickApplySurvivesCanceledParentContext(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.differ.changes = []differ.Change{{Path: "automations.yaml", Kind: "update", DiffText: "+x"}}
	opts := baseOpts()
	opts.DryRun = false
	r := fakes.reconciler(opts)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // as if SIGTERM had fired before tick reached its apply step

	r.tick(ctx)

	if len(fakes.applier.applyCtxs) != 1 {
		t.Fatalf("apply calls = %d, want 1", len(fakes.applier.applyCtxs))
	}
	if err := fakes.applier.applyCtxs[0].Err(); err != nil {
		t.Errorf("ApplyNow's context.Err() = %v, want nil: tick() must detach the context "+
			"before calling ApplyNow, or a routine SIGTERM mid-apply spuriously rolls back a valid change", err)
	}
}

func TestExceptionInsideReconcileNowDoesNotPropagateOutOfTick(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.git.ensureCloneErr = errors.New("network is down")
	r := fakes.reconciler(baseOpts())

	r.tick(context.Background()) // must not panic

	status := r.Status()
	if status.State != StateError {
		t.Errorf("state = %q, want error", status.State)
	}
	if !strings.Contains(status.LastError, "network is down") {
		t.Errorf("last_error = %q, want it to contain 'network is down'", status.LastError)
	}
}

func TestTickRecoversFromPanicInReconcileCollaborator(t *testing.T) {
	// tick's recover() is what keeps one bad cycle from killing the loop.
	fakes := newReconcilerFakes()
	fakes.differ.panicOnCompute = true
	r := fakes.reconciler(baseOpts())

	r.tick(context.Background()) // must not panic
}

// --- next check countdown ---------------------------------------------

func TestNextCheckIsUnknownUntilTheFirstCycleFinishes(t *testing.T) {
	fakes := newReconcilerFakes()
	r := fakes.reconciler(baseOpts())

	if got := r.Status().NextCheckUTC; got != "" {
		t.Errorf("next_check_utc = %q, want empty before the first tick", got)
	}
}

func TestTickSetsTheNextCheckOneIntervalOut(t *testing.T) {
	fakes := newReconcilerFakes()
	opts := baseOpts() // IntervalMinutes: 5
	r := fakes.reconciler(opts)

	before := time.Now().UTC()
	r.tick(context.Background())
	after := time.Now().UTC()

	got := r.Status().NextCheckUTC
	next, err := time.Parse(time.RFC3339, got)
	if err != nil {
		t.Fatalf("next_check_utc = %q, want an RFC3339 timestamp: %v", got, err)
	}
	interval := time.Duration(opts.IntervalMinutes) * time.Minute
	// Second-resolution format, so the window allows the truncation at each
	// end as well as the time the cycle took.
	if next.Before(before.Add(interval).Add(-time.Second)) || next.After(after.Add(interval).Add(time.Second)) {
		t.Errorf("next_check_utc = %v, want within one interval (%v) of the tick", next, interval)
	}
}

// RunLoop re-arms the interval regardless, so a countdown frozen on the
// failed tick would read "due now" forever while the loop ran on schedule.
func TestTickSetsTheNextCheckEvenWhenTheCycleFails(t *testing.T) {
	for _, tc := range []struct {
		name     string
		sabotage func(*reconcilerFakes)
	}{
		{"cycle error", func(f *reconcilerFakes) { f.git.ensureCloneErr = errors.New("network is down") }},
		{"collaborator panic", func(f *reconcilerFakes) { f.differ.panicOnCompute = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fakes := newReconcilerFakes()
			tc.sabotage(fakes)
			r := fakes.reconciler(baseOpts())

			r.tick(context.Background())

			if r.Status().NextCheckUTC == "" {
				t.Error("next_check_utc is empty after a failed cycle, want it moved forward anyway")
			}
		})
	}
}

// --- WaitIdle() -------------------------------------------------------

// main's ordinary shutdown path: nothing in flight, so no added delay.
func TestWaitIdleReturnsImmediatelyWhenIdle(t *testing.T) {
	fakes := newReconcilerFakes()
	r := fakes.reconciler(baseOpts())

	start := time.Now()
	if err := r.WaitIdle(context.Background()); err != nil {
		t.Fatalf("WaitIdle() = %v, want nil", err)
	}
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Errorf("WaitIdle took %v, want near-instant when idle", elapsed)
	}
}

// What WaitIdle exists for: an operation holding opLock when shutdown
// begins. It has to block, not check once and give up.
func TestWaitIdleWaitsForHeldOpLock(t *testing.T) {
	fakes := newReconcilerFakes()
	r := fakes.reconciler(baseOpts())
	prevInterval := waitIdlePollInterval
	waitIdlePollInterval = time.Millisecond
	defer func() { waitIdlePollInterval = prevInterval }()

	r.opLock.Lock()
	const holdFor = 30 * time.Millisecond
	go func() {
		time.Sleep(holdFor)
		r.opLock.Unlock()
	}()

	start := time.Now()
	if err := r.WaitIdle(context.Background()); err != nil {
		t.Fatalf("WaitIdle() = %v, want nil", err)
	}
	if elapsed := time.Since(start); elapsed < holdFor {
		t.Errorf("WaitIdle returned after %v, want it to have waited at least %v for the held lock", elapsed, holdFor)
	}
}

// The bounded-grace path: past main's waitIdleGrace, WaitIdle errors
// rather than blocking forever, and main logs it and exits anyway.
func TestWaitIdleTimesOutWhileLockIsHeld(t *testing.T) {
	fakes := newReconcilerFakes()
	r := fakes.reconciler(baseOpts())
	prevInterval := waitIdlePollInterval
	waitIdlePollInterval = time.Millisecond
	defer func() { waitIdlePollInterval = prevInterval }()

	r.opLock.Lock()
	defer r.opLock.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	if err := r.WaitIdle(ctx); err == nil {
		t.Fatal("WaitIdle() = nil, want a timeout error while the lock is held")
	}
}

func TestRollbackWithNoStashReturnsErrorResult(t *testing.T) {
	fakes := newReconcilerFakes()
	r := fakes.reconciler(baseOpts())

	result := r.Rollback(context.Background())

	if result.OK {
		t.Errorf("result = %+v, want ok=false", result)
	}
	if result.Error == "" {
		t.Error("want a non-empty error")
	}
}

// The Roll Back button re-renders the same page, so its busy refusal has
// to say so.
func TestRollbackLogsWhenItRefusesBecauseAnotherOperationIsRunning(t *testing.T) {
	fakes := newReconcilerFakes()
	r := fakes.reconciler(baseOpts())
	if !r.opLock.TryLock() {
		t.Fatal("could not seize opLock for the test")
	}
	defer r.opLock.Unlock()

	result := r.Rollback(context.Background())

	if result.OK || !strings.Contains(result.Error, "another operation is already running") {
		t.Errorf("result = %+v, want the busy refusal", result)
	}
	if events := r.Status().Events; !hasEventContaining(events, "rollback skipped: another operation is already running") {
		t.Errorf("events = %+v, want a skipped-rollback entry", events)
	}
}

func TestRollbackAfterSuccessfulApplyCallsRollbackFrom(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.differ.changes = []differ.Change{{Path: "automations.yaml", Kind: "update", DiffText: "+x"}}
	opts := baseOpts()
	opts.DryRun = false
	r := fakes.reconciler(opts)
	r.ReconcileNow(context.Background())
	applyResult := r.ApplyNow(context.Background(), true)
	if !applyResult.OK {
		t.Fatalf("apply result = %+v, want ok", applyResult)
	}

	result := r.Rollback(context.Background())

	if !reflect.DeepEqual(fakes.applier.rollbackCalls, []string{"/data/backup/x"}) {
		t.Errorf("rollback_calls = %v, want [/data/backup/x]", fakes.applier.rollbackCalls)
	}
	if !result.OK {
		t.Errorf("result = %+v, want ok", result)
	}
}

func TestRunLoopHonoursStopEventQuickly(t *testing.T) {
	fakes := newReconcilerFakes()
	opts := baseOpts()
	opts.IntervalMinutes = 60
	opts.DryRun = true
	r := fakes.reconciler(opts)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		r.RunLoop(ctx)
		close(done)
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunLoop did not return promptly after context cancellation")
	}
}

func TestApplyNowRefusesWhenDryRunAndNotForced(t *testing.T) {
	fakes := newReconcilerFakes()
	opts := baseOpts()
	opts.DryRun = true
	r := fakes.reconciler(opts)

	result := r.ApplyNow(context.Background(), false)

	if result.OK {
		t.Errorf("result = %+v, want ok=false", result)
	}
	if len(fakes.applier.applyCalls) != 0 {
		t.Errorf("apply_calls = %v, want none", fakes.applier.applyCalls)
	}
	// Deliberately not an event: only the reconcile loop reaches this, once
	// per interval, and the dry-run banner already says it.
	if events := r.Status().Events; hasEventContaining(events, "apply skipped") {
		t.Errorf("events = %+v, want no entry for the dry-run refusal", events)
	}
}

// Unlike the dry-run refusal above, this one is transient and worth a
// line: an interval firing during a long web-triggered operation.
func TestApplyNowLogsWhenItRefusesBecauseAnotherOperationIsRunning(t *testing.T) {
	fakes := newReconcilerFakes()
	r := fakes.reconciler(baseOpts())
	if !r.opLock.TryLock() {
		t.Fatal("could not seize opLock for the test")
	}
	defer r.opLock.Unlock()

	result := r.ApplyNow(context.Background(), true)

	if result.OK || !strings.Contains(result.Error, "another operation is already running") {
		t.Errorf("result = %+v, want the busy refusal", result)
	}
	if len(fakes.applier.applyCalls) != 0 {
		t.Errorf("apply_calls = %v, want none", fakes.applier.applyCalls)
	}
	if events := r.Status().Events; !hasEventContaining(events, "apply skipped: another operation is already running") {
		t.Errorf("events = %+v, want a skipped-apply entry", events)
	}
}

// --- Status.ApplyableCount -------------------------------------------------

func TestApplyableCountExcludesErrorRegistryOpsOnly(t *testing.T) {
	status := Status{
		Pending: []PendingChange{{Path: "automations.yaml", Kind: "update"}},
		PendingRegistry: []PendingRegOp{
			{RType: "floor", Key: "ground", Kind: registries.KindCreate},
			{RType: "area", Key: "office", Kind: registries.KindError, Error: "ambiguous adopt"},
			{RType: "label", Key: "stale", Kind: registries.KindDelete},
		},
		PendingCount: 4,
	}

	if got := status.ApplyableCount(); got != 3 {
		t.Errorf("applyable_count = %d, want 3", got)
	}
	// PendingCount keeps the error op: it feeds the sensor and
	// /status.json, where an unresolved item still has to be visible.
	if status.PendingCount != 4 {
		t.Errorf("pending_count = %d, want 4 - ApplyableCount must not change it", status.PendingCount)
	}
}

func TestApplyableCountIsZeroWhenOnlyErrorOpsArePending(t *testing.T) {
	status := Status{
		PendingRegistry: []PendingRegOp{{RType: "area", Key: "office", Kind: registries.KindError}},
		PendingCount:    1,
	}

	if got := status.ApplyableCount(); got != 0 {
		t.Errorf("applyable_count = %d, want 0 - an apply would execute nothing", got)
	}
}

// The count the Apply button quotes, against a real plan rather than a
// hand-built Status.
func TestApplyableCountMatchesARealPlanWithAnErrorOp(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.differ.changes = []differ.Change{{Path: "automations.yaml", Kind: "update"}}
	fakes.registries.desired = registries.Desired{Floors: []map[string]any{{"id": "ground", "name": "Ground"}}}
	fakes.registries.planOps = []registries.RegOp{
		{Kind: registries.KindCreate, RType: "floor", Key: "ground"},
		{Kind: registries.KindError, RType: "area", Key: "office", Error: "ambiguous adopt"},
	}
	opts := baseOpts()
	opts.ReconcileRegistries = true
	r := fakes.reconciler(opts)

	r.ReconcileNow(context.Background())

	status := r.Status()
	if status.PendingCount != 3 {
		t.Fatalf("pending_count = %d, want 3", status.PendingCount)
	}
	if got := status.ApplyableCount(); got != 2 {
		t.Errorf("applyable_count = %d, want 2", got)
	}
}

// state.Manifest accumulates every managed path, not just what the latest
// apply touched (see ApplyNow's doc comment).
func TestSuccessfulApplyMergesManifestInsteadOfOverwritingIt(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.applier.dynamicChanged = true
	opts := baseOpts()
	opts.DryRun = false
	r := fakes.reconciler(opts)

	fakes.differ.changes = []differ.Change{{Path: "a.yaml", Kind: "add", DiffText: "+a"}}
	r.ReconcileNow(context.Background())
	r.ApplyNow(context.Background(), true)
	lastState := fakes.applier.stateSaveCalls[len(fakes.applier.stateSaveCalls)-1]
	if !reflect.DeepEqual(lastState.Manifest, []string{"a.yaml"}) {
		t.Fatalf("manifest = %v, want [a.yaml]", lastState.Manifest)
	}

	fakes.differ.changes = []differ.Change{{Path: "b.yaml", Kind: "add", DiffText: "+b"}}
	r.ReconcileNow(context.Background())
	r.ApplyNow(context.Background(), true)
	lastState = fakes.applier.stateSaveCalls[len(fakes.applier.stateSaveCalls)-1]
	got := append([]string(nil), lastState.Manifest...)
	sort.Strings(got)
	if !reflect.DeepEqual(got, []string{"a.yaml", "b.yaml"}) {
		t.Fatalf("manifest = %v, want [a.yaml b.yaml]", got)
	}

	fakes.differ.changes = []differ.Change{{Path: "a.yaml", Kind: "delete", DiffText: "-a"}}
	r.ReconcileNow(context.Background())
	r.ApplyNow(context.Background(), true)
	lastState = fakes.applier.stateSaveCalls[len(fakes.applier.stateSaveCalls)-1]
	if !reflect.DeepEqual(lastState.Manifest, []string{"b.yaml"}) {
		t.Fatalf("manifest = %v, want [b.yaml]", lastState.Manifest)
	}
	if lastState.LastApplyUTC == "" {
		t.Error("last_apply_utc not set")
	}
	if lastState.LastGoodSHA != fakes.git.sha {
		t.Errorf("last_good_sha = %q, want %q", lastState.LastGoodSHA, fakes.git.sha)
	}
}

func TestApplyNowAppliesWhenForcedEvenInDryRun(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.differ.changes = []differ.Change{{Path: "automations.yaml", Kind: "update", DiffText: "+x"}}
	opts := baseOpts()
	opts.DryRun = true
	r := fakes.reconciler(opts)
	r.ReconcileNow(context.Background())

	result := r.ApplyNow(context.Background(), true)

	if !result.OK {
		t.Errorf("result = %+v, want ok", result)
	}
	if len(fakes.applier.applyCalls) != 1 {
		t.Errorf("apply_calls = %d, want 1", len(fakes.applier.applyCalls))
	}
}

// --- pre-apply Supervisor backup: failures are reported, never fatal ---

// The Supervisor backup is a second safety net; the one Rollback uses is
// applier's per-file stash, taken separately with its own fatal error.
func TestFailedPreApplyBackupDoesNotBlockTheApply(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.snapshot.backupErr = errors.New("context deadline exceeded")
	fakes.differ.changes = []differ.Change{{Path: "automations.yaml", Kind: "update", DiffText: "+x"}}
	opts := baseOpts()
	opts.DryRun = false
	r := fakes.reconciler(opts)
	r.ReconcileNow(context.Background())

	result := r.ApplyNow(context.Background(), true)

	if !result.OK {
		t.Errorf("result = %+v, want ok despite the backup failure", result)
	}
	if len(fakes.applier.applyCalls) != 1 {
		t.Errorf("apply_calls = %d, want 1", len(fakes.applier.applyCalls))
	}
	if got := r.Status().State; got != StateInSync {
		t.Errorf("state = %q, want %q - a missing backup is not a sync error", got, StateInSync)
	}
	if got := r.Status().LastError; got != "" {
		t.Errorf("last_error = %q, want empty - the apply itself succeeded", got)
	}
}

// A backup timing out on every apply is otherwise indistinguishable from
// one that works: nothing else reaches Status, the sensor or the feed.
func TestFailedPreApplyBackupIsSurfacedOnStatusAndInEvents(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.snapshot.backupErr = errors.New("context deadline exceeded")
	fakes.differ.changes = []differ.Change{{Path: "automations.yaml", Kind: "update", DiffText: "+x"}}
	opts := baseOpts()
	opts.DryRun = false
	r := fakes.reconciler(opts)
	r.ReconcileNow(context.Background())

	r.ApplyNow(context.Background(), true)

	status := r.Status()
	if !strings.Contains(status.LastBackupError, "context deadline exceeded") {
		t.Errorf("last_backup_error = %q, want it to carry the underlying failure", status.LastBackupError)
	}
	if !hasEventContaining(status.Events, "pre-apply supervisor backup failed") {
		t.Errorf("events = %+v, want one naming the failed pre-apply backup", status.Events)
	}
}

// LastBackupError describes the most recent apply, so one transient
// timeout must not leave the callout on screen permanently.
func TestSuccessfulPreApplyBackupClearsAnEarlierFailure(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.snapshot.backupErr = errors.New("context deadline exceeded")
	fakes.differ.changes = []differ.Change{{Path: "automations.yaml", Kind: "update", DiffText: "+x"}}
	opts := baseOpts()
	opts.DryRun = false
	r := fakes.reconciler(opts)
	r.ReconcileNow(context.Background())
	r.ApplyNow(context.Background(), true)

	if r.Status().LastBackupError == "" {
		t.Fatal("last_backup_error is empty after a failed backup, want it set")
	}

	fakes.snapshot.backupErr = nil
	fakes.differ.changes = []differ.Change{{Path: "scripts.yaml", Kind: "update", DiffText: "+y"}}
	r.ReconcileNow(context.Background())
	r.ApplyNow(context.Background(), true)

	if got := r.Status().LastBackupError; got != "" {
		t.Errorf("last_backup_error = %q, want empty after a successful backup", got)
	}
}

func TestFailedApplyStillRecordsStashDirForManualRollback(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.applier.applyResult = applier.Result{
		OK: false, Error: "check_config: invalid", RolledBack: true, StashDir: "/data/backup/failed-1",
	}
	fakes.differ.changes = []differ.Change{{Path: "automations.yaml", Kind: "update", DiffText: "+x"}}
	opts := baseOpts()
	opts.DryRun = false
	r := fakes.reconciler(opts)
	r.ReconcileNow(context.Background())

	r.ApplyNow(context.Background(), true)

	if got := r.Status().LastStashDir; got != "/data/backup/failed-1" {
		t.Errorf("last_stash_dir = %q, want /data/backup/failed-1", got)
	}
}

func TestRollbackAvailableAfterFailedApplyUsesItsStash(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.applier.applyResult = applier.Result{
		OK: false, Error: "check_config: invalid", RolledBack: true, StashDir: "/data/backup/failed-1",
	}
	fakes.differ.changes = []differ.Change{{Path: "automations.yaml", Kind: "update", DiffText: "+x"}}
	opts := baseOpts()
	opts.DryRun = false
	r := fakes.reconciler(opts)
	r.ReconcileNow(context.Background())
	r.ApplyNow(context.Background(), true)

	result := r.Rollback(context.Background())

	if !reflect.DeepEqual(fakes.applier.rollbackCalls, []string{"/data/backup/failed-1"}) {
		t.Errorf("rollback_calls = %v, want [/data/backup/failed-1]", fakes.applier.rollbackCalls)
	}
	if !result.OK {
		t.Errorf("result = %+v, want ok", result)
	}
}

// --- rollback preview -----------------------------------------------------

// What the Roll Back dialog quotes, composed once rather than re-derived
// on every Status call. Only the stash files' presence is ever read.
func TestApplyComposesTheRollbackPreviewFromWhatItStashed(t *testing.T) {
	fakes := newReconcilerFakes()
	stashDir := t.TempDir()
	writeFile(t, filepath.Join(stashDir, "registry_stash.json"), `{"ops": []}`)
	writeFile(t, filepath.Join(stashDir, "integration_stash.json"), `{"ops": []}`)
	fakes.differ.changes = oneChange()
	fakes.applier.applyResult = applier.Result{
		OK: true, Changed: []string{"automations.yaml", "scripts.yaml"}, StashDir: stashDir,
	}
	opts := baseOpts()
	opts.DryRun = false
	r := fakes.reconciler(opts)
	r.ReconcileNow(context.Background())

	r.ApplyNow(context.Background(), true)

	want := "2 file(s), registry objects and integrations"
	if got := r.Status().RollbackPreview; got != want {
		t.Errorf("rollback_preview = %q, want %q", got, want)
	}
}

// A files-only stash says so, with no trailing "and" over an empty half.
func TestApplyRollbackPreviewNamesOnlyTheLayersThatLeftAStash(t *testing.T) {
	fakes := newReconcilerFakes()
	stashDir := t.TempDir()
	fakes.differ.changes = oneChange()
	fakes.applier.applyResult = applier.Result{
		OK: true, Changed: []string{"automations.yaml"}, StashDir: stashDir,
	}
	opts := baseOpts()
	opts.DryRun = false
	r := fakes.reconciler(opts)
	r.ReconcileNow(context.Background())

	r.ApplyNow(context.Background(), true)

	if got := r.Status().RollbackPreview; got != "1 file(s)" {
		t.Errorf("rollback_preview = %q, want %q", got, "1 file(s)")
	}
}

// A failed apply records its own stash and clears the preview with it;
// applier.Apply fills Changed on success only, so nothing replaces it.
func TestAFailedApplyClearsThePreviewWithTheStashItReplaces(t *testing.T) {
	fakes := newReconcilerFakes()
	first := t.TempDir()
	writeFile(t, filepath.Join(first, "registry_stash.json"), `{"ops": []}`)
	fakes.differ.changes = oneChange()
	fakes.applier.applyResult = applier.Result{
		OK: true, Changed: []string{"automations.yaml"}, StashDir: first,
	}
	opts := baseOpts()
	opts.DryRun = false
	r := fakes.reconciler(opts)
	r.ReconcileNow(context.Background())
	r.ApplyNow(context.Background(), true)
	if r.Status().RollbackPreview == "" {
		t.Fatal("the first apply composed no preview for the second to replace")
	}

	second := t.TempDir()
	fakes.applier.applyResult = applier.Result{
		OK: false, Error: "check_config: invalid", RolledBack: true, StashDir: second,
	}
	r.ReconcileNow(context.Background())
	r.ApplyNow(context.Background(), true)

	status := r.Status()
	if status.LastStashDir != second {
		t.Fatalf("last_stash_dir = %q, want the failed apply's stash %q", status.LastStashDir, second)
	}
	if got := status.RollbackPreview; got != "" {
		t.Errorf("rollback_preview = %q, want it cleared - it described the previous apply", got)
	}
}

// A stash with nothing restorable in it is not the same as no preview at
// all: rolling this one back succeeds and changes nothing.
func TestAnApplyWithNothingToRestoreSaysSoRatherThanNothing(t *testing.T) {
	fakes := newReconcilerFakes()
	// The subentry layer writes no stash, so the directory stays empty.
	stashDir := t.TempDir()
	fakes.differ.changes = nil
	fakes.subentries.desired = subentries.Desired{Subentries: []map[string]any{declaredSubentry("hall", "pushward")}}
	fakes.subentries.planOps = []registries.RegOp{
		{RType: "subentry", Key: "hall", Kind: subentries.KindCreate, DiffText: "+widget\n"},
	}
	fakes.applier.applyResult = applier.Result{OK: true, StashDir: ""}
	fakes.applier.makeStashDirResult = stashDir
	opts := baseOpts()
	opts.DryRun = false
	opts.ReconcileSubentries = true
	r := fakes.reconciler(opts)
	r.ReconcileNow(context.Background())

	r.ApplyNow(context.Background(), true)

	status := r.Status()
	if status.LastStashDir != stashDir {
		t.Fatalf("last_stash_dir = %q, want the allocated stash %q", status.LastStashDir, stashDir)
	}
	if got := status.RollbackPreview; got != RollbackPreviewNothing {
		t.Errorf("rollback_preview = %q, want %q", got, RollbackPreviewNothing)
	}
	if !status.RollbackRestoresNothing() {
		t.Error("RollbackRestoresNothing() is false for an apply that stashed nothing")
	}
}

// A failed bookkeeping write is when someone reaches for Roll Back, and
// the registry inverses are already on disk, so the stash records first.
func TestAStateSaveFailureStillRecordsTheStashAndItsPreview(t *testing.T) {
	fakes := newReconcilerFakes()
	stashDir := t.TempDir()
	writeFile(t, filepath.Join(stashDir, "registry_stash.json"), `{"ops": []}`)
	fakes.differ.changes = nil
	fakes.registries.desired = registries.Desired{Floors: []map[string]any{{"id": "ground", "name": "Ground"}}}
	fakes.registries.planOps = []registries.RegOp{
		{RType: "floor", Key: "ground", Kind: registries.KindCreate, DiffText: "+name: Ground\n"},
	}
	fakes.registryApplier.applyResult = regapply.RegistryApplyResult{OK: true, Applied: []string{"create floor:ground"}}
	fakes.applier.applyResult = applier.Result{OK: true, StashDir: ""}
	fakes.applier.makeStashDirResult = stashDir
	fakes.applier.stateSaveErr = errors.New("no space left on device")
	opts := baseOpts()
	opts.DryRun = false
	opts.ReconcileRegistries = true
	r := fakes.reconciler(opts)
	r.ReconcileNow(context.Background())

	r.ApplyNow(context.Background(), true)

	status := r.Status()
	if status.LastStashDir != stashDir {
		t.Fatalf("last_stash_dir = %q, want the stash the registry layer wrote into, %q",
			status.LastStashDir, stashDir)
	}
	if got := status.RollbackPreview; got != "registry objects" {
		t.Errorf("rollback_preview = %q, want it to name the registry inverses on disk", got)
	}
}

// Cleared with the directory it describes, or the summary comes back
// attached to the next apply's stash.
func TestASuccessfulRollbackClearsTheRollbackPreview(t *testing.T) {
	fakes := newReconcilerFakes()
	stashDir := t.TempDir()
	fakes.differ.changes = oneChange()
	fakes.applier.applyResult = applier.Result{
		OK: true, Changed: []string{"automations.yaml"}, StashDir: stashDir,
	}
	opts := baseOpts()
	opts.DryRun = false
	r := fakes.reconciler(opts)
	r.ReconcileNow(context.Background())
	r.ApplyNow(context.Background(), true)
	if r.Status().RollbackPreview == "" {
		t.Fatal("the apply composed no preview to clear")
	}

	r.Rollback(context.Background())

	status := r.Status()
	if status.LastStashDir != "" {
		t.Fatalf("last_stash_dir = %q, want it cleared", status.LastStashDir)
	}
	if got := status.RollbackPreview; got != "" {
		t.Errorf("rollback_preview = %q, want it cleared with the stash it describes", got)
	}
}

// A failed rollback keeps both: the stash is still there and the button is
// still how to retry it.
func TestAFailedRollbackKeepsTheRollbackPreview(t *testing.T) {
	fakes := newReconcilerFakes()
	stashDir := t.TempDir()
	fakes.differ.changes = oneChange()
	fakes.applier.applyResult = applier.Result{
		OK: true, Changed: []string{"automations.yaml"}, StashDir: stashDir,
	}
	fakes.applier.rollbackResult = applier.Result{OK: false, Error: "cannot read stash"}
	opts := baseOpts()
	opts.DryRun = false
	r := fakes.reconciler(opts)
	r.ReconcileNow(context.Background())
	r.ApplyNow(context.Background(), true)

	r.Rollback(context.Background())

	if got := r.Status().RollbackPreview; got != "1 file(s)" {
		t.Errorf("rollback_preview = %q, want it kept while the stash still is", got)
	}
}

// --- check_config warnings ------------------------------------------------

func TestApplyNowStoresWarningsInStatusAndSensorPush(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.applier.applyResult = applier.Result{
		OK: true, Changed: []string{"automations.yaml"}, StashDir: "/data/backup/x",
		Warnings: "Integration 'templete' not found.",
	}
	fakes.differ.changes = []differ.Change{{Path: "automations.yaml", Kind: "update", DiffText: "+x"}}
	opts := baseOpts()
	opts.DryRun = false
	r := fakes.reconciler(opts)
	r.ReconcileNow(context.Background())

	result := r.ApplyNow(context.Background(), true)

	if !result.OK {
		t.Fatalf("result = %+v, want ok", result)
	}
	status := r.Status()
	if status.Warnings != "Integration 'templete' not found." {
		t.Errorf("warnings = %q, want the check_config text", status.Warnings)
	}
	// Warnings never imply an error: check_config called the config valid.
	if status.State != StateInSync {
		t.Errorf("state = %q, want in_sync", status.State)
	}
	lastPush := fakes.status.pushes[len(fakes.status.pushes)-1]
	if lastPush.attrs["warnings"] != "Integration 'templete' not found." {
		t.Errorf("pushed warnings attr = %v, want the check_config text", lastPush.attrs["warnings"])
	}
	if !hasEventContaining(status.Events, "config warnings after apply") {
		t.Errorf("no event mentions config warnings; events = %+v", status.Events)
	}
}

func TestApplyNowClearsWarningsOnNextApplyWithNone(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.applier.applyResult = applier.Result{
		OK: true, Changed: []string{"automations.yaml"}, StashDir: "/data/backup/x",
		Warnings: "Integration 'templete' not found.",
	}
	fakes.differ.changes = []differ.Change{{Path: "automations.yaml", Kind: "update", DiffText: "+x"}}
	opts := baseOpts()
	opts.DryRun = false
	r := fakes.reconciler(opts)
	r.ReconcileNow(context.Background())
	r.ApplyNow(context.Background(), true)
	if r.Status().Warnings == "" {
		t.Fatal("sanity: first apply should have recorded a warning")
	}

	// A reconcile cycle in between must not touch lastWarnings.
	fakes.differ.changes = []differ.Change{{Path: "scripts.yaml", Kind: "add", DiffText: "+y"}}
	r.ReconcileNow(context.Background())
	if r.Status().Warnings == "" {
		t.Error("warnings cleared by a reconcile cycle; should persist until the next apply")
	}

	fakes.applier.applyResult = applier.Result{OK: true, Changed: []string{"scripts.yaml"}, StashDir: "/data/backup/y"}
	r.ApplyNow(context.Background(), true)

	if got := r.Status().Warnings; got != "" {
		t.Errorf("warnings = %q, want cleared by the second apply", got)
	}
	lastPush := fakes.status.pushes[len(fakes.status.pushes)-1]
	if lastPush.attrs["warnings"] != nil {
		t.Errorf("pushed warnings attr = %v, want nil (JSON null)", lastPush.attrs["warnings"])
	}
}

// The sensor is not restored across a Home Assistant restart, so pushing
// only on a state transition would leave it missing once things settle.
func TestStatusPushedEveryCycleEvenWithoutAStateChange(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.differ.changes = nil
	r := fakes.reconciler(baseOpts())

	r.ReconcileNow(context.Background())
	r.ReconcileNow(context.Background())
	r.ReconcileNow(context.Background())

	if len(fakes.status.pushes) != 3 {
		t.Fatalf("pushes = %d, want 3", len(fakes.status.pushes))
	}
	for _, p := range fakes.status.pushes {
		if p.state != StateInSync {
			t.Errorf("push state = %q, want in_sync", p.state)
		}
	}
}

// --- registry reconciliation integration --------------------------------

func TestReconcileNowDetectsRegistryDriftWhenEnabled(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.differ.changes = nil
	fakes.registries.desired = registries.Desired{Floors: []map[string]any{{"id": "ground", "name": "Ground floor"}}}
	fakes.registries.planOps = []registries.RegOp{
		{Kind: registries.KindCreate, RType: "floor", Key: "ground", Params: map[string]any{"name": "Ground floor"}, DiffText: "+name: 'Ground floor'\n"},
	}
	opts := baseOpts()
	opts.ReconcileRegistries = true
	r := fakes.reconciler(opts)

	r.ReconcileNow(context.Background())

	status := r.Status()
	if status.State != StateDriftPending {
		t.Errorf("state = %q, want drift_pending", status.State)
	}
	want := []PendingRegOp{{RType: "floor", Key: "ground", Kind: "create", DiffText: "+name: 'Ground floor'\n"}}
	if !reflect.DeepEqual(status.PendingRegistry, want) {
		t.Errorf("pending_registry = %+v, want %+v", status.PendingRegistry, want)
	}
	if status.PendingCount != 1 {
		t.Errorf("pending_count = %d, want 1", status.PendingCount)
	}
	if fakes.registryApplier.fetchLiveCalls != 1 {
		t.Errorf("fetch_live_calls = %d, want 1", fakes.registryApplier.fetchLiveCalls)
	}
}

func TestReconcileNowSkipsRegistriesEntirelyWhenToggleOff(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.registries.desired = registries.Desired{Floors: []map[string]any{{"id": "ground", "name": "Ground"}}}
	opts := baseOpts()
	opts.ReconcileRegistries = false
	r := fakes.reconciler(opts)

	r.ReconcileNow(context.Background())

	if len(fakes.registries.loadManifestsCalls) != 0 {
		t.Errorf("load_manifests_calls = %v, want none", fakes.registries.loadManifestsCalls)
	}
	if fakes.registryApplier.fetchLiveCalls != 0 {
		t.Errorf("fetch_live_calls = %d, want 0", fakes.registryApplier.fetchLiveCalls)
	}
	if len(r.Status().PendingRegistry) != 0 {
		t.Errorf("pending_registry = %v, want none", r.Status().PendingRegistry)
	}
}

func TestReconcileNowSkipsWsFetchWhenManifestSetIsEmpty(t *testing.T) {
	fakes := newReconcilerFakes()
	opts := baseOpts()
	opts.ReconcileRegistries = true
	r := fakes.reconciler(opts)

	r.ReconcileNow(context.Background())

	if !reflect.DeepEqual(fakes.registries.loadManifestsCalls, []string{"/data/repo"}) {
		t.Errorf("load_manifests_calls = %v, want [/data/repo]", fakes.registries.loadManifestsCalls)
	}
	if fakes.registryApplier.fetchLiveCalls != 0 {
		t.Errorf("fetch_live_calls = %d, want 0", fakes.registryApplier.fetchLiveCalls)
	}
	status := r.Status()
	if len(status.PendingRegistry) != 0 {
		t.Errorf("pending_registry = %v, want none", status.PendingRegistry)
	}
	if status.State != StateInSync {
		t.Errorf("state = %q, want in_sync", status.State)
	}
}

// A ManifestError lists every problem across both files and must reach
// last_error unmodified; failCycle leaves neither pending list behind.
func TestReconcileNowManifestErrorSurfacesVerbatimAndClearsPending(t *testing.T) {
	fakes := newReconcilerFakes()
	problems := []string{
		"registries.yaml: floors[0] has an invalid or missing 'id'",
		"helpers.yaml: unknown helper domain 'nope'",
	}
	wantErr := strings.Join(problems, "; ")
	fakes.registries.desired = registries.Desired{Floors: []map[string]any{{"id": "x", "name": "X"}}}
	fakes.registries.manifestErr = &registries.ManifestError{Problems: problems}
	fakes.differ.changes = []differ.Change{{Path: "automations.yaml", Kind: "update", DiffText: "+x"}}
	opts := baseOpts()
	opts.ReconcileRegistries = true
	r := fakes.reconciler(opts)

	r.ReconcileNow(context.Background()) // must not panic

	status := r.Status()
	if status.State != StateError {
		t.Errorf("state = %q, want error", status.State)
	}
	if status.LastError != wantErr {
		t.Errorf("last_error = %q, want %q", status.LastError, wantErr)
	}
	if len(status.Pending) != 0 {
		t.Errorf("pending = %v, want none", status.Pending)
	}
	if len(status.PendingRegistry) != 0 {
		t.Errorf("pending_registry = %v, want none", status.PendingRegistry)
	}
}

func TestReconcileNowWsFailureDoesNotCrashTheLoop(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.registries.desired = registries.Desired{Floors: []map[string]any{{"id": "ground", "name": "Ground"}}}
	fakes.registryApplier.fetchErr = &wsclient.Error{Code: "timeout", Message: "no response for id=1"}
	opts := baseOpts()
	opts.ReconcileRegistries = true
	r := fakes.reconciler(opts)

	r.ReconcileNow(context.Background()) // must not panic

	status := r.Status()
	if status.State != StateError {
		t.Errorf("state = %q, want error", status.State)
	}
	if !strings.Contains(status.LastError, "timeout") {
		t.Errorf("last_error = %q, want it to contain timeout", status.LastError)
	}
}

func TestTickRegistryOnlyDriftDoesNotApplyInDryRun(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.registries.desired = registries.Desired{Floors: []map[string]any{{"id": "ground", "name": "Ground"}}}
	fakes.registries.planOps = []registries.RegOp{
		{Kind: registries.KindCreate, RType: "floor", Key: "ground", Params: map[string]any{"name": "Ground"}, DiffText: "+x"},
	}
	opts := baseOpts()
	opts.DryRun = true
	opts.ReconcileRegistries = true
	r := fakes.reconciler(opts)

	r.tick(context.Background())

	if len(fakes.applier.applyCalls) != 0 {
		t.Errorf("apply_calls = %v, want none", fakes.applier.applyCalls)
	}
	if len(fakes.registryApplier.applyPlanCalls) != 0 {
		t.Errorf("apply_plan_calls = %v, want none", fakes.registryApplier.applyPlanCalls)
	}
	if got := r.Status().State; got != StateDriftPending {
		t.Errorf("state = %q, want drift_pending", got)
	}
}

func TestTickRegistryOnlyDriftTriggersApplyWhenDryRunOff(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.registries.desired = registries.Desired{Floors: []map[string]any{{"id": "ground", "name": "Ground"}}}
	fakes.registries.planOps = []registries.RegOp{
		{Kind: registries.KindCreate, RType: "floor", Key: "ground", Params: map[string]any{"name": "Ground"}, DiffText: "+x"},
	}
	fakes.registryApplier.applyResult = regapply.RegistryApplyResult{OK: true, Applied: []string{"create floor:ground"}}
	fakes.applier.applyResult = applier.Result{OK: true}
	opts := baseOpts()
	opts.DryRun = false
	opts.ReconcileRegistries = true
	r := fakes.reconciler(opts)

	r.tick(context.Background())

	if len(fakes.registryApplier.applyPlanCalls) != 1 {
		t.Errorf("apply_plan_calls = %d, want 1", len(fakes.registryApplier.applyPlanCalls))
	}
	status := r.Status()
	if status.State != StateInSync {
		t.Errorf("state = %q, want in_sync", status.State)
	}
	if len(status.PendingRegistry) != 0 {
		t.Errorf("pending_registry = %v, want none", status.PendingRegistry)
	}
}

// Error ops can never execute, so their presence must not trigger an
// auto-apply: an unresolved conflict would cost a backup every interval.
func TestTickWithOnlyErrorRegistryOpsNeverAppliesOrBacksUp(t *testing.T) {
	fakes := newReconcilerFakes()
	errorOp := registries.RegOp{
		Kind: registries.KindError, RType: "floor", Key: "ground",
		Error: "ambiguous adopt: 2 live floor objects named 'Ground floor'",
	}
	fakes.registries.desired = registries.Desired{Floors: []map[string]any{{"id": "ground", "name": "Ground floor"}}}
	fakes.registries.planOps = []registries.RegOp{errorOp}
	opts := baseOpts()
	opts.DryRun = false
	opts.ReconcileRegistries = true
	r := fakes.reconciler(opts)

	for i := 0; i < 3; i++ {
		r.tick(context.Background())
	}

	if len(fakes.applier.applyCalls) != 0 {
		t.Errorf("apply_calls = %v, want none", fakes.applier.applyCalls)
	}
	if len(fakes.registryApplier.applyPlanCalls) != 0 {
		t.Errorf("apply_plan_calls = %v, want none", fakes.registryApplier.applyPlanCalls)
	}
	if fakes.snapshot.backupCalls != 0 {
		t.Errorf("backup_calls = %d, want 0", fakes.snapshot.backupCalls)
	}
	status := r.Status()
	if status.State != StateDriftPending {
		t.Errorf("state = %q, want drift_pending", status.State)
	}
	if len(status.PendingRegistry) != 1 {
		t.Errorf("pending_registry = %v, want 1 entry", status.PendingRegistry)
	}
}

// With no file changes there is no stash to reuse, so MakeStashDir has to
// supply one for registry_stash.json.
func TestApplyNowRegistryOnlyAllocatesItsOwnStashDir(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.registries.desired = registries.Desired{Floors: []map[string]any{{"id": "ground", "name": "Ground"}}}
	fakes.registries.planOps = []registries.RegOp{{Kind: registries.KindCreate, RType: "floor", Key: "ground", DiffText: "+x"}}
	fakes.registryApplier.applyResult = regapply.RegistryApplyResult{OK: true, Applied: []string{"create floor:ground"}}
	fakes.applier.applyResult = applier.Result{OK: true, StashDir: ""}
	opts := baseOpts()
	opts.DryRun = false
	opts.ReconcileRegistries = true
	r := fakes.reconciler(opts)
	r.ReconcileNow(context.Background())

	result := r.ApplyNow(context.Background(), true)

	if !result.OK {
		t.Fatalf("result = %+v, want ok", result)
	}
	if fakes.applier.makeStashDirCalls != 1 {
		t.Errorf("make_stash_dir_calls = %d, want 1", fakes.applier.makeStashDirCalls)
	}
	if len(fakes.registryApplier.applyPlanCalls) != 1 || fakes.registryApplier.applyPlanCalls[0].stashDir != "/data/backup/registry-only" {
		t.Errorf("apply_plan_calls = %+v, want stash /data/backup/registry-only", fakes.registryApplier.applyPlanCalls)
	}
	status := r.Status()
	if status.LastStashDir != "/data/backup/registry-only" {
		t.Errorf("last_stash_dir = %q", status.LastStashDir)
	}
	if len(status.PendingRegistry) != 0 {
		t.Errorf("pending_registry = %v, want none", status.PendingRegistry)
	}
	if status.State != StateInSync {
		t.Errorf("state = %q, want in_sync", status.State)
	}
}

func TestApplyNowReusesFileStashDirForRegistriesWhenFilesAlsoChanged(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.differ.changes = []differ.Change{{Path: "automations.yaml", Kind: "update", DiffText: "+x"}}
	fakes.registries.desired = registries.Desired{Floors: []map[string]any{{"id": "ground", "name": "Ground"}}}
	fakes.registries.planOps = []registries.RegOp{{Kind: registries.KindCreate, RType: "floor", Key: "ground", DiffText: "+x"}}
	fakes.registryApplier.applyResult = regapply.RegistryApplyResult{OK: true, Applied: []string{"create floor:ground"}}
	// fakes.applier default applyResult.StashDir = "/data/backup/x".
	opts := baseOpts()
	opts.DryRun = false
	opts.ReconcileRegistries = true
	r := fakes.reconciler(opts)
	r.ReconcileNow(context.Background())

	r.ApplyNow(context.Background(), true)

	if fakes.applier.makeStashDirCalls != 0 {
		t.Errorf("make_stash_dir_calls = %d, want 0", fakes.applier.makeStashDirCalls)
	}
	if len(fakes.registryApplier.applyPlanCalls) != 1 || fakes.registryApplier.applyPlanCalls[0].stashDir != "/data/backup/x" {
		t.Errorf("apply_plan_calls = %+v, want stash /data/backup/x", fakes.registryApplier.applyPlanCalls)
	}
}

func TestApplyNowRegistryFailureAfterFileSuccessDoesNotUndoFiles(t *testing.T) {
	fakes := newReconcilerFakes()
	changes := []differ.Change{{Path: "automations.yaml", Kind: "update", DiffText: "+x"}}
	fakes.differ.changes = changes
	fakes.registries.desired = registries.Desired{Floors: []map[string]any{{"id": "ground", "name": "Ground"}}}
	fakes.registries.planOps = []registries.RegOp{{Kind: registries.KindCreate, RType: "floor", Key: "ground", DiffText: "+x"}}
	fakes.registryApplier.applyResult = regapply.RegistryApplyResult{
		OK: false, Error: "create floor:ground failed: boom", RolledBack: true,
	}
	opts := baseOpts()
	opts.DryRun = false
	opts.ReconcileRegistries = true
	r := fakes.reconciler(opts)
	r.ReconcileNow(context.Background())

	result := r.ApplyNow(context.Background(), true)

	// The files landed and the manifest merge ran; the registry failure
	// must not have skipped or undone either.
	if len(fakes.applier.applyCalls) != 1 {
		t.Errorf("apply_calls = %d, want 1", len(fakes.applier.applyCalls))
	}
	if len(fakes.applier.stateSaveCalls) == 0 {
		t.Error("state was not persisted")
	}

	if result.OK {
		t.Errorf("result = %+v, want ok=false", result)
	}
	if !strings.Contains(result.Error, "create floor:ground failed") {
		t.Errorf("result.Error = %q", result.Error)
	}

	status := r.Status()
	if status.State != StateError {
		t.Errorf("state = %q, want error", status.State)
	}
	if !strings.Contains(status.LastError, "create floor:ground failed") {
		t.Errorf("last_error = %q", status.LastError)
	}
	if len(status.Events) == 0 || !strings.Contains(status.Events[len(status.Events)-1].Message, "files applied") {
		t.Errorf("events = %+v, want last message to mention files applied", status.Events)
	}
	if len(status.Pending) != 0 {
		t.Errorf("pending = %v, want none (file changes succeeded, cleared)", status.Pending)
	}
	if len(status.PendingRegistry) != 1 {
		t.Errorf("pending_registry = %v, want 1 entry kept for visibility/retry", status.PendingRegistry)
	}
}

// The fetch has to succeed for there to be a plan at all, so the failure
// is configured on the fake between the reconcile and the apply.
func TestApplyNowWsFailureDuringRegistryApplyDoesNotCrash(t *testing.T) {
	fakes := newReconcilerFakes()
	changes := []differ.Change{{Path: "automations.yaml", Kind: "update", DiffText: "+x"}}
	fakes.differ.changes = changes
	fakes.registries.desired = registries.Desired{Floors: []map[string]any{{"id": "ground", "name": "Ground"}}}
	fakes.registries.planOps = []registries.RegOp{{Kind: registries.KindCreate, RType: "floor", Key: "ground", DiffText: "+x"}}
	opts := baseOpts()
	opts.DryRun = false
	opts.ReconcileRegistries = true
	r := fakes.reconciler(opts)
	r.ReconcileNow(context.Background())
	if len(r.Status().PendingRegistry) != 1 {
		t.Fatal("sanity: a plan was computed")
	}

	fakes.registryApplier.applyResult = regapply.RegistryApplyResult{
		OK: false, Error: "failed to connect: auth_invalid: bad token",
	}

	result := r.ApplyNow(context.Background(), true) // must not panic

	if result.OK {
		t.Errorf("result = %+v, want ok=false", result)
	}
	if !strings.Contains(result.Error, "auth_invalid") && !strings.Contains(result.Error, "bad token") {
		t.Errorf("result.Error = %q", result.Error)
	}
	if r.Status().State != StateError {
		t.Errorf("state = %q, want error", r.Status().State)
	}
}

func TestApplyNowRegistriesNeverRunWhenFileApplyItselfFails(t *testing.T) {
	fakes := newReconcilerFakes()
	changes := []differ.Change{{Path: "automations.yaml", Kind: "update", DiffText: "+x"}}
	fakes.differ.changes = changes
	fakes.registries.desired = registries.Desired{Floors: []map[string]any{{"id": "ground", "name": "Ground"}}}
	fakes.registries.planOps = []registries.RegOp{{Kind: registries.KindCreate, RType: "floor", Key: "ground", DiffText: "+x"}}
	fakes.applier.applyResult = applier.Result{
		OK: false, Error: "check_config: invalid", RolledBack: true, StashDir: "/data/backup/failed",
	}
	opts := baseOpts()
	opts.DryRun = false
	opts.ReconcileRegistries = true
	r := fakes.reconciler(opts)
	r.ReconcileNow(context.Background())

	r.ApplyNow(context.Background(), true)

	if len(fakes.registryApplier.applyPlanCalls) != 0 {
		t.Errorf("apply_plan_calls = %v, want none", fakes.registryApplier.applyPlanCalls)
	}
}

func TestRollbackCoversRegistriesWhenStashHasRegistryStashJSON(t *testing.T) {
	fakes := newReconcilerFakes()
	stashDir := t.TempDir()
	writeFile(t, filepath.Join(stashDir, "manifest.json"), `{"files": {}, "created_dirs": []}`)
	writeFile(t, filepath.Join(stashDir, "registry_stash.json"), `{"ops": []}`)

	changes := []differ.Change{{Path: "automations.yaml", Kind: "update", DiffText: "+x"}}
	fakes.differ.changes = changes
	fakes.applier.applyResult = applier.Result{OK: true, Changed: []string{"automations.yaml"}, StashDir: stashDir}
	fakes.registryApplier.rollbackResult = regapply.RegistryApplyResult{OK: true, RolledBack: true}
	opts := baseOpts()
	opts.DryRun = false
	r := fakes.reconciler(opts)
	r.ReconcileNow(context.Background())
	r.ApplyNow(context.Background(), true)

	result := r.Rollback(context.Background())

	if !reflect.DeepEqual(fakes.applier.rollbackCalls, []string{stashDir}) {
		t.Errorf("rollback_calls = %v, want [%s]", fakes.applier.rollbackCalls, stashDir)
	}
	if !reflect.DeepEqual(fakes.registryApplier.rollbackCalls, []string{stashDir}) {
		t.Errorf("registry rollback_calls = %v, want [%s]", fakes.registryApplier.rollbackCalls, stashDir)
	}
	if !result.OK {
		t.Errorf("result = %+v, want ok", result)
	}
}

func TestRollbackSkipsRegistriesWhenNoRegistryStashPresent(t *testing.T) {
	fakes := newReconcilerFakes()
	changes := []differ.Change{{Path: "automations.yaml", Kind: "update", DiffText: "+x"}}
	fakes.differ.changes = changes
	// default fakes.applier applyResult.StashDir = "/data/backup/x" (no registry_stash.json there).
	opts := baseOpts()
	opts.DryRun = false
	r := fakes.reconciler(opts)
	r.ReconcileNow(context.Background())
	r.ApplyNow(context.Background(), true)

	result := r.Rollback(context.Background())

	if len(fakes.registryApplier.rollbackCalls) != 0 {
		t.Errorf("registry rollback_calls = %v, want none", fakes.registryApplier.rollbackCalls)
	}
	if !result.OK {
		t.Errorf("result = %+v, want ok", result)
	}
}

func TestRollbackRegistryFailureMakesCombinedResultFail(t *testing.T) {
	fakes := newReconcilerFakes()
	stashDir := t.TempDir()
	writeFile(t, filepath.Join(stashDir, "manifest.json"), `{"files": {}, "created_dirs": []}`)
	writeFile(t, filepath.Join(stashDir, "registry_stash.json"), `{"ops": []}`)

	changes := []differ.Change{{Path: "automations.yaml", Kind: "update", DiffText: "+x"}}
	fakes.differ.changes = changes
	fakes.applier.applyResult = applier.Result{OK: true, Changed: []string{"automations.yaml"}, StashDir: stashDir}
	fakes.registryApplier.rollbackResult = regapply.RegistryApplyResult{OK: false, Error: "rollback boom"}
	opts := baseOpts()
	opts.DryRun = false
	r := fakes.reconciler(opts)
	r.ReconcileNow(context.Background())
	r.ApplyNow(context.Background(), true)

	result := r.Rollback(context.Background())

	if result.OK {
		t.Errorf("result = %+v, want ok=false", result)
	}
	if !strings.Contains(result.Error, "rollback boom") {
		t.Errorf("result.Error = %q", result.Error)
	}
	if r.Status().State != StateError {
		t.Errorf("state = %q, want error", r.Status().State)
	}
}

// --- secrets guard must see the unfiltered tracked tree -----------------

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...) // #nosec G204 -- fixed "git" binary; args are test-controlled fixture setup, never external input
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.invalid",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.invalid",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, out)
	}
}

func makeRemoteRepo(t *testing.T, tmp, name string) (bare, work string) {
	t.Helper()
	bare = filepath.Join(tmp, name+".git")
	work = filepath.Join(tmp, name+"-work")
	runGit(t, tmp, "init", "--bare", "-b", "main", bare)
	runGit(t, tmp, "init", "-b", "main", work)
	runGit(t, work, "remote", "add", "origin", "file://"+bare)
	return bare, work
}

func commitFile(t *testing.T, work, relPath, content string) {
	t.Helper()
	full := filepath.Join(work, relPath)
	if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
		t.Fatal(err)
	}
	writeFile(t, full, content)
	runGit(t, work, "add", relPath)
	runGit(t, work, "commit", "-m", "commit")
	runGit(t, work, "push", "origin", "main")
}

// A fake cannot tell TrackedFiles from TrackedFilesRaw; only real gitsync,
// whose TrackedFiles strips secrets.yaml, proves the guard reads the raw one.
func TestReconcileNowRealGitsyncHardStopsOnTrackedSecrets(t *testing.T) {
	tmp := t.TempDir()
	bare, work := makeRemoteRepo(t, tmp, "remote")
	commitFile(t, work, "automations.yaml", "- id: demo\n")
	commitFile(t, work, "secrets.yaml", "api_password: hunter2\n")

	workdir := filepath.Join(tmp, "clone")
	opts := baseOpts()
	opts.RepoURL = "file://" + bare
	opts.DryRun = true
	gs := gitsync.New(opts, workdir)

	fakes := newReconcilerFakes()
	r := New(opts, Deps{
		Git:             newRealGit(gs),
		Differ:          fakes.differ,
		Applier:         fakes.applier,
		Snapshot:        fakes.snapshot,
		Status:          fakes.status,
		Registries:      fakes.registries,
		RegistryApplier: fakes.registryApplier,
	})

	r.ReconcileNow(context.Background())

	status := r.Status()
	if status.State != StateError {
		t.Errorf("state = %q, want error", status.State)
	}
	if !strings.Contains(status.LastError, "secrets.yaml") {
		t.Errorf("last_error = %q, want it to contain secrets.yaml", status.LastError)
	}
	// A hard stop: checkout never ran, so the tree is as --no-checkout
	// left it.
	if _, err := os.Stat(filepath.Join(workdir, "automations.yaml")); err == nil {
		t.Error("checkout must never have run; workdir should still be empty")
	}
}

// --- emptying the manifest must still plan rule-4 deletes ----------------

func TestReconcileNowSkipsWsFetchWhenDesiredAndRegistryManagedBothEmpty(t *testing.T) {
	fakes := newReconcilerFakes()
	// default fakes.applier.state carries no registry_managed entries.
	opts := baseOpts()
	opts.ReconcileRegistries = true
	r := fakes.reconciler(opts)

	r.ReconcileNow(context.Background())

	if !reflect.DeepEqual(fakes.registries.loadManifestsCalls, []string{"/data/repo"}) {
		t.Errorf("load_manifests_calls = %v, want [/data/repo]", fakes.registries.loadManifestsCalls)
	}
	if fakes.registryApplier.fetchLiveCalls != 0 {
		t.Errorf("fetch_live_calls = %d, want 0", fakes.registryApplier.fetchLiveCalls)
	}
	if len(r.Status().PendingRegistry) != 0 {
		t.Errorf("pending_registry = %v, want none", r.Status().PendingRegistry)
	}
}

func TestReconcileNowPlansDeletesWhenManifestEmptiedButRegistryManagedNonempty(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.registries.planOps = []registries.RegOp{
		{Kind: registries.KindDelete, RType: "floor", Key: "ground", LiveID: "F1", DiffText: "-name: 'Ground'\n"},
	}
	fakes.applier.state.RegistryManaged = map[string]string{"floor:ground": "F1"}
	opts := baseOpts()
	opts.ReconcileRegistries = true
	r := fakes.reconciler(opts)

	r.ReconcileNow(context.Background())

	if fakes.registryApplier.fetchLiveCalls != 1 {
		t.Errorf("fetch_live_calls = %d, want 1", fakes.registryApplier.fetchLiveCalls)
	}
	lastPlanCall := fakes.registries.planCalls[len(fakes.registries.planCalls)-1]
	if !reflect.DeepEqual(lastPlanCall.managed, map[string]string{"floor:ground": "F1"}) {
		t.Errorf("plan managed = %v", lastPlanCall.managed)
	}
	status := r.Status()
	want := []PendingRegOp{{RType: "floor", Key: "ground", Kind: "delete", DiffText: "-name: 'Ground'\n"}}
	if !reflect.DeepEqual(status.PendingRegistry, want) {
		t.Errorf("pending_registry = %+v, want %+v", status.PendingRegistry, want)
	}
	if status.State != StateDriftPending {
		t.Errorf("state = %q, want drift_pending", status.State)
	}
}

// --- skipped registry error ops stay visible ------------------------------

// A clean apply that skipped an error op must keep it visible in the UI
// and the sensor: the cycle is not fully in sync.
func TestApplyNowKeepsSkippedRegistryErrorsPendingAndSetsDriftPending(t *testing.T) {
	fakes := newReconcilerFakes()
	errorOp := registries.RegOp{Kind: registries.KindError, RType: "area", Key: "x", Error: "ambiguous adopt: 2 live area objects named 'X'"}
	createOp := registries.RegOp{Kind: registries.KindCreate, RType: "floor", Key: "ground", Params: map[string]any{"name": "Ground"}, DiffText: "+x"}
	fakes.registries.desired = registries.Desired{Floors: []map[string]any{{"id": "ground", "name": "Ground"}}}
	fakes.registries.planOps = []registries.RegOp{createOp, errorOp}
	fakes.registryApplier.applyResult = regapply.RegistryApplyResult{
		OK: true, Applied: []string{"create floor:ground"}, SkippedErrors: []registries.RegOp{errorOp},
	}
	fakes.applier.applyResult = applier.Result{OK: true}
	opts := baseOpts()
	opts.DryRun = false
	opts.ReconcileRegistries = true
	r := fakes.reconciler(opts)
	r.ReconcileNow(context.Background())

	result := r.ApplyNow(context.Background(), true)

	if !result.OK {
		t.Errorf("result = %+v, want ok", result)
	}
	status := r.Status()
	if status.State != StateDriftPending {
		t.Errorf("state = %q, want drift_pending", status.State)
	}
	want := []PendingRegOp{{RType: "area", Key: "x", Kind: "error", Error: "ambiguous adopt: 2 live area objects named 'X'"}}
	if !reflect.DeepEqual(status.PendingRegistry, want) {
		t.Errorf("pending_registry = %+v, want %+v", status.PendingRegistry, want)
	}
}

// ApplyPlan executes nothing for an all-error plan, and in_sync would
// swallow the conflict.
func TestApplyNowPlanOfOnlyErrorOpsStaysDriftPending(t *testing.T) {
	fakes := newReconcilerFakes()
	errorOp := registries.RegOp{Kind: registries.KindError, RType: "label", Key: "gitops", Error: "ambiguous adopt: 2 live label objects named 'GitOps'"}
	fakes.registries.desired = registries.Desired{Labels: []map[string]any{{"id": "gitops", "name": "GitOps"}}}
	fakes.registries.planOps = []registries.RegOp{errorOp}
	fakes.registryApplier.applyResult = regapply.RegistryApplyResult{OK: true, SkippedErrors: []registries.RegOp{errorOp}}
	fakes.applier.applyResult = applier.Result{OK: true}
	opts := baseOpts()
	opts.DryRun = false
	opts.ReconcileRegistries = true
	r := fakes.reconciler(opts)
	r.ReconcileNow(context.Background())

	r.ApplyNow(context.Background(), true)

	status := r.Status()
	if status.State != StateDriftPending {
		t.Errorf("state = %q, want drift_pending", status.State)
	}
	if len(status.PendingRegistry) != 1 {
		t.Errorf("pending_registry = %v, want 1 entry", status.PendingRegistry)
	}
}

// Some failures leave real ops in effect (an inverse-replay that could not
// redial), and claiming a rollback says the registries are untouched.
func TestApplyNowEventLogDistinguishesRolledBackFromNot(t *testing.T) {
	for _, tc := range []struct {
		name       string
		rolledBack bool
		want       string
	}{
		{"rolled back", true, "registries failed and were rolled back"},
		{"not rolled back", false, "registries failed and could NOT be fully rolled back"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fakes := newReconcilerFakes()
			fakes.registries.desired = registries.Desired{Floors: []map[string]any{{"id": "ground", "name": "Ground"}}}
			fakes.registries.planOps = []registries.RegOp{
				{Kind: registries.KindCreate, RType: "floor", Key: "ground", Params: map[string]any{"name": "Ground"}},
			}
			fakes.registryApplier.applyResult = regapply.RegistryApplyResult{
				OK: false, Error: "boom", RolledBack: tc.rolledBack,
			}
			fakes.applier.applyResult = applier.Result{OK: true}
			opts := baseOpts()
			opts.DryRun = false
			opts.ReconcileRegistries = true
			r := fakes.reconciler(opts)
			r.ReconcileNow(context.Background())

			r.ApplyNow(context.Background(), true)

			if events := r.Status().Events; !hasEventContaining(events, tc.want) {
				t.Errorf("no event containing %q; events = %+v", tc.want, events)
			}
		})
	}
}

// A later layer's failure names that layer, not "registries", and says the
// earlier layers stayed applied - only Rollback reverts those.
func TestApplyNowEventLogNamesTheFailingLayerAndKeepsEarlierCountsHonest(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.registries.desired = registries.Desired{Floors: []map[string]any{{"id": "ground", "name": "Ground"}}}
	fakes.registries.planOps = []registries.RegOp{
		{Kind: registries.KindCreate, RType: "floor", Key: "ground", Params: map[string]any{"name": "Ground"}},
	}
	fakes.registryApplier.applyResult = regapply.RegistryApplyResult{OK: true, Applied: []string{"create floor:ground"}}

	fakes.addonOpts.desired = addonopts.Desired{Addons: []map[string]any{{"slug": "core_configurator"}}}
	fakes.addonOpts.planOps = []registries.RegOp{
		{Kind: registries.KindUpdate, RType: "addon", Key: "core_configurator", Params: map[string]any{"x": 1}},
	}
	fakes.registryApplier.applyAddonResult = regapply.RegistryApplyResult{
		OK: false, Error: "supervisor said no", RolledBack: true,
	}
	fakes.applier.applyResult = applier.Result{OK: true}

	opts := baseOpts()
	opts.DryRun = false
	opts.ReconcileRegistries = true
	opts.ReconcileAddonOptions = true
	r := fakes.reconciler(opts)
	r.ReconcileNow(context.Background())

	r.ApplyNow(context.Background(), true)

	var msg string
	for _, e := range r.Status().Events {
		if strings.Contains(e.Message, "failed") {
			msg = e.Message
		}
	}
	if !strings.Contains(msg, "add-on options failed") {
		t.Errorf("event %q does not name the failing layer", msg)
	}
	if strings.Contains(msg, "registries failed") {
		t.Errorf("event %q blames registries for an add-on options failure", msg)
	}
	if !strings.Contains(msg, "1 earlier registry change(s) stayed applied") {
		t.Errorf("event %q hides that the registry layer's op stayed applied", msg)
	}
}

// ApplyPlan issues a KindCreate unconditionally, so the second ApplyNow -
// no ReconcileNow between - would otherwise duplicate the live floor.
func TestApplyNowRetryAfterPartialFailureDoesNotResubmitAlreadyAppliedOps(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.registries.desired = registries.Desired{Floors: []map[string]any{{"id": "ground", "name": "Ground"}}}
	fakes.registries.planOps = []registries.RegOp{
		{Kind: registries.KindCreate, RType: "floor", Key: "ground", Params: map[string]any{"name": "Ground"}},
	}
	fakes.registryApplier.applyResult = regapply.RegistryApplyResult{OK: true, Applied: []string{"create floor:ground"}}

	fakes.addonOpts.desired = addonopts.Desired{Addons: []map[string]any{{"slug": "core_configurator"}}}
	fakes.addonOpts.planOps = []registries.RegOp{
		{Kind: registries.KindUpdate, RType: "addon", Key: "core_configurator", Params: map[string]any{"x": 1}},
	}
	fakes.registryApplier.applyAddonResult = regapply.RegistryApplyResult{
		OK: false, Error: "supervisor said no", RolledBack: true,
	}
	fakes.applier.applyResult = applier.Result{OK: true}

	opts := baseOpts()
	opts.DryRun = false
	opts.ReconcileRegistries = true
	opts.ReconcileAddonOptions = true
	r := fakes.reconciler(opts)
	r.ReconcileNow(context.Background())

	r.ApplyNow(context.Background(), true)

	for _, op := range r.Status().PendingRegistry {
		if op.RType == "floor" && op.Key == "ground" {
			t.Fatalf("pending registry still lists the already-applied floor create: %+v", r.Status().PendingRegistry)
		}
	}

	r.ApplyNow(context.Background(), true)

	if len(fakes.registryApplier.applyPlanCalls) != 1 {
		t.Fatalf(
			"apply_plan_calls = %d, want 1 (the retry must not resubmit the already-applied floor create): %+v",
			len(fakes.registryApplier.applyPlanCalls), fakes.registryApplier.applyPlanCalls)
	}
}

// ApplyAddonPlan drops a reverted sibling (aaa) from Applied but keeps the
// stuck op (bbb), so aaa stays pending for retry and bbb does not.
func TestApplyNowAddonDoubleFaultKeepsRevertedSiblingPendingNotStuckOp(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.addonOpts.desired = addonopts.Desired{Addons: []map[string]any{{"slug": "aaa"}, {"slug": "bbb"}}}
	fakes.addonOpts.planOps = []registries.RegOp{
		{Kind: registries.KindUpdate, RType: "addon", Key: "aaa", Params: map[string]any{"enabled": true}},
		{Kind: registries.KindUpdate, RType: "addon", Key: "bbb", Params: map[string]any{"dirsfirst": true}},
	}
	fakes.registryApplier.applyAddonResult = regapply.RegistryApplyResult{
		OK: false, Applied: []string{"update addon:bbb"}, Error: "bbb double fault", RolledBack: false,
	}
	fakes.applier.applyResult = applier.Result{OK: true}

	opts := baseOpts()
	opts.DryRun = false
	opts.ReconcileAddonOptions = true
	r := fakes.reconciler(opts)
	r.ReconcileNow(context.Background())

	r.ApplyNow(context.Background(), true)

	var sawAAA, sawBBB bool
	for _, op := range r.Status().PendingRegistry {
		if op.RType == "addon" && op.Key == "aaa" {
			sawAAA = true
		}
		if op.RType == "addon" && op.Key == "bbb" {
			sawBBB = true
		}
	}
	if !sawAAA {
		t.Errorf("pending registry = %+v, want aaa (genuinely reverted) to stay pending for retry", r.Status().PendingRegistry)
	}
	if sawBBB {
		t.Errorf("pending registry = %+v, want bbb (stuck live, per Applied) not resubmitted", r.Status().PendingRegistry)
	}
}

// Nothing ever ran, so there is no rollback outcome to report and no
// registries-layer op to blame for a MakeStashDir failure.
func TestApplyNowStashAllocationFailureDoesNotBlameRegistriesOrClaimRollback(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.addonOpts.desired = addonopts.Desired{Addons: []map[string]any{{"slug": "core_configurator"}}}
	fakes.addonOpts.planOps = []registries.RegOp{
		{Kind: registries.KindUpdate, RType: "addon", Key: "core_configurator", Params: map[string]any{"x": 1}},
	}
	fakes.applier.applyResult = applier.Result{OK: true}
	fakes.applier.makeStashDirErr = errors.New("no space left on device")

	opts := baseOpts()
	opts.DryRun = false
	opts.ReconcileAddonOptions = true
	r := fakes.reconciler(opts)
	r.ReconcileNow(context.Background())

	r.ApplyNow(context.Background(), true)

	var msg string
	for _, e := range r.Status().Events {
		if strings.Contains(e.Message, "no space left on device") {
			msg = e.Message
		}
	}
	if msg == "" {
		t.Fatalf("no event mentions the stash allocation failure; events = %+v", r.Status().Events)
	}
	if strings.Contains(msg, "registries failed") {
		t.Errorf("event %q blames the registries layer for a stash allocation failure", msg)
	}
	if strings.Contains(msg, "rolled back") {
		t.Errorf("event %q claims a rollback outcome, but nothing was ever attempted: %q", msg, msg)
	}
}

// --- VM e2e: a failed cycle must leave no plan behind --------------------
//
// On a real install, a cycle that failed at manifest load still showed the
// previous commit's plan, and Apply wrote it and reported in_sync.
func brokenManifest() *dashboards.ManifestError {
	return &dashboards.ManifestError{
		Problems: []string{"dashboards.yaml: dashboard id 'default' is reserved and cannot be managed"},
	}
}

// driftThenBrokenManifest leaves one file change pending from a good
// cycle, then runs one that fails at dashboards manifest load.
func driftThenBrokenManifest(t *testing.T) (*reconcilerFakes, *Reconciler) {
	t.Helper()
	fakes := newReconcilerFakes()
	fakes.differ.changes = []differ.Change{{Path: "dashboards/e2e-probe.txt", Kind: "update", DiffText: "+probe"}}
	opts := baseOpts()
	opts.DryRun = false
	opts.ReconcileDashboards = true
	r := fakes.reconciler(opts)

	r.ReconcileNow(context.Background())
	if r.Status().PendingCount != 1 {
		t.Fatalf("pending_count = %d after the good cycle, want 1", r.Status().PendingCount)
	}

	fakes.dashboards.manifestErr = brokenManifest()
	r.ReconcileNow(context.Background())
	return fakes, r
}

func TestFailedCycleLeavesNothingPending(t *testing.T) {
	_, r := driftThenBrokenManifest(t)

	status := r.Status()
	if status.State != StateError {
		t.Errorf("state = %q, want error", status.State)
	}
	if !strings.Contains(status.LastError, "'default' is reserved") {
		t.Errorf("last_error = %q, want the manifest problem", status.LastError)
	}
	if status.PendingCount != 0 || len(status.Pending) != 0 || len(status.PendingRegistry) != 0 {
		t.Errorf(
			"pending_count = %d, pending = %+v, pending_registry = %+v; want an empty plan - what was there "+
				"was computed against the commit before the broken one",
			status.PendingCount, status.Pending, status.PendingRegistry)
	}
}

func TestApplyAfterAFailedCycleWritesNothingAndKeepsTheError(t *testing.T) {
	fakes, r := driftThenBrokenManifest(t)
	wantErr := r.Status().LastError

	result := r.ApplyNow(context.Background(), true)

	if result.OK {
		t.Errorf("result = %+v, want a refusal", result)
	}
	if len(fakes.applier.applyCalls) != 0 {
		t.Errorf("apply_calls = %+v, want none - there was no plan to apply", fakes.applier.applyCalls)
	}
	if fakes.snapshot.backupCalls != 0 {
		t.Errorf("backup_calls = %d, want 0 - a refused apply must not take a Supervisor backup", fakes.snapshot.backupCalls)
	}
	status := r.Status()
	if status.State != StateError {
		t.Errorf("state = %q, want error to stand - the apply resolved nothing", status.State)
	}
	if status.LastError != wantErr {
		t.Errorf("last_error = %q, want it unchanged at %q", status.LastError, wantErr)
	}
}

func TestRollbackStillWorksAfterAFailedCycle(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.differ.changes = []differ.Change{{Path: "dashboards/e2e-probe.txt", Kind: "update", DiffText: "+probe"}}
	opts := baseOpts()
	opts.DryRun = false
	opts.ReconcileDashboards = true
	r := fakes.reconciler(opts)

	r.ReconcileNow(context.Background())
	r.ApplyNow(context.Background(), true)

	fakes.dashboards.manifestErr = brokenManifest()
	r.ReconcileNow(context.Background())
	if r.Status().State != StateError {
		t.Fatalf("state = %q, want error before the rollback", r.Status().State)
	}

	result := r.Rollback(context.Background())

	if !result.OK {
		t.Errorf("result = %+v, want the rollback to succeed", result)
	}
	if want := []string{"/data/backup/x"}; !reflect.DeepEqual(fakes.applier.rollbackCalls, want) {
		t.Errorf("rollback_calls = %v, want %v - rollback is the recovery path OUT of the error state", fakes.applier.rollbackCalls, want)
	}
	if got := r.Status().State; got != StateInSync {
		t.Errorf("state = %q, want in_sync after a clean rollback", got)
	}
}

// The refusal above is scoped to a cycle that could not plan and lifts the
// moment one can; a retry after a partial registry failure is still valid.
func TestApplyRunsAgainOnceAReconcileSucceeds(t *testing.T) {
	fakes, r := driftThenBrokenManifest(t)

	r.ApplyNow(context.Background(), true)
	if len(fakes.applier.applyCalls) != 0 {
		t.Fatalf("apply_calls = %+v, want none while the cycle is failed", fakes.applier.applyCalls)
	}

	fakes.dashboards.manifestErr = nil
	r.ReconcileNow(context.Background())

	result := r.ApplyNow(context.Background(), true)

	if !result.OK {
		t.Errorf("result = %+v, want a normal successful apply", result)
	}
	if len(fakes.applier.applyCalls) != 1 {
		t.Fatalf("apply_calls = %+v, want exactly the one apply", fakes.applier.applyCalls)
	}
	if got := fakes.applier.applyCalls[0][0].Path; got != "dashboards/e2e-probe.txt" {
		t.Errorf("applied path = %q, want dashboards/e2e-probe.txt", got)
	}
	status := r.Status()
	if status.State != StateInSync {
		t.Errorf("state = %q, want in_sync", status.State)
	}
	if status.LastError != "" {
		t.Errorf("last_error = %q, want cleared by the cycle that actually re-read the repository", status.LastError)
	}
}

// The interval's own route to the same stale apply: a failed apply keeps
// its plan for retry, and then the manifest breaks under it.
func TestTickAfterAFailedCycleDoesNotApplyTheStalePlan(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.differ.changes = []differ.Change{{Path: "dashboards/e2e-probe.txt", Kind: "update", DiffText: "+probe"}}
	fakes.applier.applyResult = applier.Result{OK: false, Error: "check_config failed", StashDir: "/data/backup/x"}
	opts := baseOpts()
	opts.DryRun = false
	opts.ReconcileDashboards = true
	r := fakes.reconciler(opts)

	r.tick(context.Background())
	if len(fakes.applier.applyCalls) != 1 {
		t.Fatalf("apply_calls = %+v, want the first tick's own apply", fakes.applier.applyCalls)
	}

	fakes.dashboards.manifestErr = brokenManifest()
	r.tick(context.Background())

	if len(fakes.applier.applyCalls) != 1 {
		t.Errorf("apply_calls = %+v, want no second apply from the tick that failed to plan", fakes.applier.applyCalls)
	}
	if got := r.Status().PendingCount; got != 0 {
		t.Errorf("pending_count = %d, want 0", got)
	}
}

// The failures that end a cycle before a single manifest is read; the
// secrets guard is the one that has to route through failCycle.
func TestEarlyGitFailuresLeaveNothingPendingAndRefuseApply(t *testing.T) {
	cases := []struct {
		name string
		fail func(*fakeGit)
	}{
		{"ensure_clone", func(g *fakeGit) { g.ensureCloneErr = errors.New("could not read Username") }},
		{"fetch", func(g *fakeGit) { g.fetchErr = errors.New("no such ref") }},
		{"tracked_files_raw", func(g *fakeGit) { g.trackedRawErr = errors.New("bad object") }},
		{"guard_secrets", func(g *fakeGit) { g.secretsErr = &gitsync.SecretsTrackedError{Files: []string{"secrets.yaml"}} }},
		{"tracked_files", func(g *fakeGit) { g.trackedErr = errors.New("bad object") }},
		{"checkout", func(g *fakeGit) { g.checkoutErr = errors.New("index.lock exists") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fakes := newReconcilerFakes()
			fakes.differ.changes = []differ.Change{{Path: "automations.yaml", Kind: "update", DiffText: "+x"}}
			opts := baseOpts()
			opts.DryRun = false
			r := fakes.reconciler(opts)

			r.ReconcileNow(context.Background())
			if r.Status().PendingCount != 1 {
				t.Fatalf("pending_count = %d after the good cycle, want 1", r.Status().PendingCount)
			}

			tc.fail(fakes.git)
			r.ReconcileNow(context.Background())

			status := r.Status()
			if status.State != StateError {
				t.Errorf("state = %q, want error", status.State)
			}
			if status.PendingCount != 0 {
				t.Errorf("pending_count = %d, want 0 - the plan predates whatever the tip is now", status.PendingCount)
			}

			if result := r.ApplyNow(context.Background(), true); result.OK {
				t.Errorf("result = %+v, want a refusal", result)
			}
			if len(fakes.applier.applyCalls) != 0 {
				t.Errorf("apply_calls = %+v, want none", fakes.applier.applyCalls)
			}
		})
	}
}
