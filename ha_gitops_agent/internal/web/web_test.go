package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/applier"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/applier/statetest"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/differ"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/history"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/options"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/recon"
)

// dispatched counts what the handlers asked the agent to do. Snapshotted
// under fakeAgent's lock, since operations run in their own goroutine.
type dispatched struct {
	reconcile  int
	apply      []bool
	rollback   int
	commitBack int
	importLive int
	preview    int
	// retry carries the keys the retry route asked for, not a count: the
	// whole point of that route is which item was pressed.
	retry []string
	// dismissPreview counts presses of the import preview's Dismiss.
	dismissPreview int
	// addonCheck counts presses of the add-on card's Check. A count, not a
	// bool: that route accepts a second press while the agent is busy, and
	// only a count tells that apart from a refusal.
	addonCheck int
	// setPaused carries what each press asked for: /pause and /resume are
	// one call with two arguments.
	setPaused []bool
}

// fakeAgent stands in for *recon.Reconciler in every test below. Every
// field is read and written under mu: the counters and the busy/state
// pair are touched by the goroutines the action routes start.
type fakeAgent struct {
	mu                sync.Mutex
	configured        bool
	busy              bool
	dryRun            bool
	pendingCount      int
	state             string
	pendingRegistry   []recon.PendingRegOp
	lastStashDir      string
	repoURLOverride   string
	warnings          string
	commitBackEnabled bool
	lastDriftBranch   string
	commitBackErr     error
	importEnabled     bool
	lastImportUTC     string
	lastImportSHA     string
	lastImportError   string
	importPreview     *recon.ImportPreview
	importErr         error
	lastBackupError   string
	autoUpdateEnabled bool
	addonUpdates      []recon.AddonUpdateStatus
	// addonCheckRunning is Status.AddonCheckRunning: checkLock, NOT busy.
	// Fixtures set the two independently, which is why the field exists.
	addonCheckRunning bool
	// addonCheckIntervalSeconds is the staleness threshold handed to the
	// client. Zero means no stale marker, as a shrunk interval does.
	addonCheckIntervalSeconds int
	importRecordFailing       bool
	runHistory                []history.Record
	nextCheckUTC              string
	// historyAll is what HistoryAll answers with, standing in for runs held
	// beyond the card's. Left nil it is runHistory, so the two agree.
	historyAll []history.Record
	// historyTotal is Status.HistoryTotal. Zero means len(runHistory), the
	// state where the heading has nothing to link to.
	historyTotal    int
	rollbackPreview string

	paused              bool
	setPausedErr        error
	blocked             []recon.BlockedItem
	retryErr            error
	pendingRestartSlugs []string
	managed             recon.ManagedInventory
	hacsRestartPending  []string

	historyWriteFailing        bool
	versionRecordFailing       bool
	addonCheckFailing          []string
	addonUpdateSelfSlugFailing bool

	calls dispatched

	// gate, when armed, holds every operation inside the agent - busy and
	// applying, as the reconciler is - until the test lets it go. Left nil
	// the operations run straight through.
	gate chan struct{}
	// started and finished carry one operation name each, so a test can
	// wait for an operation instead of racing it. Buffered deep enough
	// that an operation nobody waits for never blocks.
	started  chan string
	finished chan string
}

func newFakeAgent() *fakeAgent {
	return &fakeAgent{
		configured:   true,
		busy:         false,
		dryRun:       false,
		pendingCount: 1,
		state:        "drift_pending",
		started:      make(chan string, 8),
		finished:     make(chan string, 8),
	}
}

// gated arms the gate, so every operation blocks inside the agent until
// release is called. Returns the release function.
func (f *fakeAgent) gated() (release func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gate = make(chan struct{})
	gate := f.gate
	return sync.OnceFunc(func() { close(gate) })
}

// begin records that an operation started and, when gated, holds it busy
// until released - leaving the agent in sync, as a finished apply does.
func (f *fakeAgent) begin(name string) {
	f.hold(name,
		func() {
			f.busy = true
			f.state = recon.StateApplying
		},
		func() {
			f.busy = false
			f.state = recon.StateInSync
			f.pendingCount = 0
			f.pendingRegistry = nil
		})
}

// beginCheck marks the agent CHECKING and never busy, since the real
// check takes checkLock, and leaves state and pending counts alone.
func (f *fakeAgent) beginCheck(name string) {
	f.hold(name,
		func() { f.addonCheckRunning = true },
		func() { f.addonCheckRunning = false })
}

// hold is the shared body of the two above: announce name, then block on
// an armed gate. enter and leave are called with f.mu held, and neither
// runs at all on an unarmed gate.
func (f *fakeAgent) hold(name string, enter, leave func()) {
	f.mu.Lock()
	gate := f.gate
	if gate != nil {
		enter()
	}
	f.mu.Unlock()

	report(f.started, name)
	if gate == nil {
		return
	}
	<-gate

	f.mu.Lock()
	leave()
	f.mu.Unlock()
}

// report announces name on ch without ever blocking on a test that is not
// listening.
func report(ch chan string, name string) {
	select {
	case ch <- name:
	default:
	}
}

// awaitStart blocks until an operation has begun, by which point it has
// recorded its call and, if gated, marked itself busy.
func (f *fakeAgent) awaitStart(t *testing.T) string {
	t.Helper()
	return await(t, f.started, "start")
}

// awaitFinish blocks until an operation has run to completion.
func (f *fakeAgent) awaitFinish(t *testing.T) string {
	t.Helper()
	return await(t, f.finished, "finish")
}

func await(t *testing.T, ch chan string, what string) string {
	t.Helper()
	select {
	case name := <-ch:
		return name
	case <-time.After(5 * time.Second):
		t.Fatalf("no operation reached %s within 5s", what)
		return ""
	}
}

// setState moves the reported state, for cases that need the status to
// change between requests.
func (f *fakeAgent) setState(state string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state = state
}

// dispatchedCalls snapshots what the handlers dispatched, under the lock.
// The slices are copied so a later append cannot write under the caller.
func (f *fakeAgent) dispatchedCalls() dispatched {
	f.mu.Lock()
	defer f.mu.Unlock()
	snapshot := f.calls
	snapshot.apply = append([]bool(nil), f.calls.apply...)
	snapshot.retry = append([]string(nil), f.calls.retry...)
	snapshot.setPaused = append([]bool(nil), f.calls.setPaused...)
	return snapshot
}

func (f *fakeAgent) Busy() bool { return f.Status().Busy }

func (f *fakeAgent) Status() recon.Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	repoURL := f.repoURLOverride
	if repoURL == "" && f.configured {
		repoURL = "https://example.invalid/demo.git"
	}
	lastError := ""
	if f.state == "error" {
		lastError = "check_config failed: bad indentation"
	}
	var pending []recon.PendingChange
	if f.pendingCount > 0 {
		pending = []recon.PendingChange{{Path: "automations.yaml", Kind: "update", DiffText: "+alias: Demo\n-old: 1"}}
	}
	// Never less than the card's rows - the reconciler's invariant by
	// construction, and what the heading's link is gated on.
	historyTotal := f.historyTotal
	if historyTotal < len(f.runHistory) {
		historyTotal = len(f.runHistory)
	}
	return recon.Status{
		State:             f.state,
		Busy:              f.busy,
		Configured:        f.configured,
		DryRun:            f.dryRun,
		RepoURL:           repoURL,
		Branch:            "main",
		IntervalMinutes:   5,
		LastSHA:           "abcdef1234567890",
		LastSHAShort:      "abcdef1",
		LastApplyUTC:      "2026-08-01T00:00:00+00:00",
		NextCheckUTC:      f.nextCheckUTC,
		Paused:            f.paused,
		LastStashDir:      f.lastStashDir,
		RollbackPreview:   f.rollbackPreview,
		LastError:         lastError,
		LastBackupError:   f.lastBackupError,
		Warnings:          f.warnings,
		CommitBackEnabled: f.commitBackEnabled,
		LastDriftBranch:   f.lastDriftBranch,
		History:           f.runHistory,
		HistoryTotal:      historyTotal,
		ImportEnabled:     f.importEnabled,
		LastImportUTC:     f.lastImportUTC,
		LastImportSHA:     f.lastImportSHA,
		LastImportSHAShort: func() string {
			if len(f.lastImportSHA) > 7 {
				return f.lastImportSHA[:7]
			}
			return f.lastImportSHA
		}(),
		LastImportError:   f.lastImportError,
		ImportPreview:     f.importPreview,
		AutoUpdateEnabled: f.autoUpdateEnabled,
		// Copied, so an unset fake gives the empty-but-not-nil slice the
		// real Status always returns.
		AddonUpdates:              append([]recon.AddonUpdateStatus{}, f.addonUpdates...),
		AddonCheckRunning:         f.addonCheckRunning,
		AddonCheckIntervalSeconds: f.addonCheckIntervalSeconds,
		// Copied for the same reason; sorted by the caller, since the real
		// Status sorts these itself.
		Blocked: append([]recon.BlockedItem{}, f.blocked...),
		// Re-sliced group by group: the real inventory is non-nil in every
		// group, so a nil one here is a shape no reconciler can produce.
		Managed:                    normalizeManaged(f.managed),
		HacsRestartPending:         f.hacsRestartPending,
		PendingRestartSlugs:        f.pendingRestartSlugs,
		HistoryWriteFailing:        f.historyWriteFailing,
		VersionRecordFailing:       f.versionRecordFailing,
		ImportRecordFailing:        f.importRecordFailing,
		AddonCheckFailing:          f.addonCheckFailing,
		AddonUpdateSelfSlugFailing: f.addonUpdateSelfSlugFailing,
		// Derived as recon.Status derives it (files plus registry ops,
		// error kinds included), not echoed from f.pendingCount.
		PendingCount:    len(pending) + len(f.pendingRegistry),
		Pending:         pending,
		PendingRegistry: f.pendingRegistry,
		// Its own TS, not LastApplyUTC's: different parts of the template
		// render them, and sharing would let one assertion pass on the other.
		Events: []recon.Event{{TS: "2026-08-04T11:22:33+00:00", Message: "agent started"}},
	}
}

// normalizeManaged fills in the shape recon.ManagedInventory guarantees:
// every group non-nil, whatever the fixture left unset.
func normalizeManaged(m recon.ManagedInventory) recon.ManagedInventory {
	return recon.ManagedInventory{
		Files:        append([]string{}, m.Files...),
		Registry:     append([]string{}, m.Registry...),
		Entities:     append([]string{}, m.Entities...),
		Dashboards:   append([]string{}, m.Dashboards...),
		Addons:       append([]string{}, m.Addons...),
		Integrations: append([]string{}, m.Integrations...),
		Subentries:   append([]string{}, m.Subentries...),
		Hacs:         append([]string{}, m.Hacs...),
	}
}

// Each operation below records its call FIRST, then begins, so a test
// that has seen a start can already assert on what was dispatched.

func (f *fakeAgent) ReconcileNow(ctx context.Context) []differ.Change {
	f.mu.Lock()
	f.calls.reconcile++
	f.mu.Unlock()
	f.begin("reconcile")
	report(f.finished, "reconcile")
	return nil
}

func (f *fakeAgent) ApplyNow(ctx context.Context, force bool) applier.Result {
	f.mu.Lock()
	f.calls.apply = append(f.calls.apply, force)
	f.mu.Unlock()
	f.begin("apply")
	report(f.finished, "apply")
	return applier.Result{}
}

func (f *fakeAgent) Rollback(ctx context.Context) applier.Result {
	f.mu.Lock()
	f.calls.rollback++
	f.mu.Unlock()
	f.begin("rollback")
	report(f.finished, "rollback")
	return applier.Result{}
}

func (f *fakeAgent) CommitDriftBack(ctx context.Context) (string, error) {
	f.mu.Lock()
	f.calls.commitBack++
	err, branch := f.commitBackErr, f.lastDriftBranch
	f.mu.Unlock()
	f.begin("commitback")
	report(f.finished, "commitback")
	if err != nil {
		return "", err
	}
	return branch, nil
}

func (f *fakeAgent) PreviewImport(ctx context.Context) (recon.ImportPreview, error) {
	f.mu.Lock()
	f.calls.preview++
	err, preview := f.importErr, f.importPreview
	f.mu.Unlock()
	f.begin("preview")
	report(f.finished, "preview")
	if err != nil {
		return recon.ImportPreview{}, err
	}
	if preview != nil {
		return *preview, nil
	}
	return recon.ImportPreview{}, nil
}

func (f *fakeAgent) ImportLive(ctx context.Context) (recon.ImportSummary, error) {
	f.mu.Lock()
	f.calls.importLive++
	err, sha := f.importErr, f.lastImportSHA
	f.mu.Unlock()
	f.begin("import")
	report(f.finished, "import")
	if err != nil {
		return recon.ImportSummary{}, err
	}
	return recon.ImportSummary{Files: 2, Bytes: 1024, CommitSHA: sha, Branch: "main"}, nil
}

// RetryBlocked is not an operation of its own: the route runs it and then
// dispatches a reconcile, so it only reports finishing on the failure
// path, where that reconcile never happens.
func (f *fakeAgent) RetryBlocked(key string) error {
	f.mu.Lock()
	f.calls.retry = append(f.calls.retry, key)
	err := f.retryErr
	f.mu.Unlock()

	report(f.started, "retry")
	if err != nil {
		report(f.finished, "retry")
		return err
	}
	return nil
}

// CheckAddonUpdates is a background operation that never marks the agent
// busy: it takes checkLock, not opLock, and a fake that flipped busy
// would make the two locks look like one.
func (f *fakeAgent) CheckAddonUpdates(ctx context.Context) {
	f.mu.Lock()
	f.calls.addonCheck++
	f.mu.Unlock()
	f.beginCheck("addoncheck")
	report(f.finished, "addoncheck")
}

// DismissImportPreview is not an operation: called inline, reporting
// nothing. Clearing the field is what takes the card off the next render.
func (f *fakeAgent) DismissImportPreview() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls.dismissPreview++
	f.importPreview = nil
}

// SetPaused is not an operation either. setPausedErr fails the flag WRITE
// while the flag still takes, as the real reconciler promises.
func (f *fakeAgent) SetPaused(paused bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls.setPaused = append(f.calls.setPaused, paused)
	f.paused = paused
	return f.setPausedErr
}

// HistoryAll is a read, not an operation. Copied out for the reason the
// real reconciler copies it: a caller must not be handed the ring.
func (f *fakeAgent) HistoryAll() []history.Record {
	f.mu.Lock()
	defer f.mu.Unlock()
	records := f.historyAll
	if records == nil {
		records = f.runHistory
	}
	return append([]history.Record{}, records...)
}

var _ Agent = (*fakeAgent)(nil)

func devEnv(t *testing.T) {
	t.Helper()
	t.Setenv(DevEnvVar, "1")
}

func doRequest(t *testing.T, handler http.Handler, method, path string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

// doForm posts a form-encoded body for POST /retry, the only route that
// reads one. r.FormValue needs the content type as well as the bytes.
func doForm(t *testing.T, handler http.Handler, path string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

// fragmentHashOf pulls the ?h= back out of rendered markup. Read rather
// than recomputed, so it is exactly what a browser would send back.
func fragmentHashOf(t *testing.T, body string) string {
	t.Helper()
	const marker = `hx-get="fragment?h=`
	start := strings.Index(body, marker)
	if start < 0 {
		t.Fatal("body carries no polling URL to take a hash from")
	}
	rest := body[start+len(marker):]
	end := strings.IndexAny(rest, `"&`)
	if end <= 0 {
		t.Fatal("could not delimit the fragment hash")
	}
	return rest[:end]
}

func TestIngressCheckReturns403WithoutDevFlagOrProxyAddr(t *testing.T) {
	t.Setenv(DevEnvVar, "0")
	handler := New(newFakeAgent())

	rec := doRequest(t, handler, http.MethodGet, "/", nil)

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
}

func TestIndexReturns200WithDevFlag(t *testing.T) {
	devEnv(t)
	handler := New(newFakeAgent())

	rec := doRequest(t, handler, http.MethodGet, "/", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "automations.yaml") {
		t.Error("body does not contain automations.yaml")
	}
}

func TestReconcileRouteDispatchesTheAgentAndReturns200(t *testing.T) {
	devEnv(t)
	agent := newFakeAgent()
	handler := New(agent)

	rec := doRequest(t, handler, http.MethodPost, "/reconcile", nil)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	agent.awaitStart(t)
	if got := agent.dispatchedCalls().reconcile; got != 1 {
		t.Errorf("reconcile calls = %d, want 1", got)
	}
}

func TestApplyRouteDispatchesTheAgentWithForceTrueAndReturns200(t *testing.T) {
	devEnv(t)
	agent := newFakeAgent()
	handler := New(agent)

	rec := doRequest(t, handler, http.MethodPost, "/apply", nil)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	agent.awaitStart(t)
	if got := agent.dispatchedCalls().apply; len(got) != 1 || !got[0] {
		t.Errorf("apply calls = %v, want [true]", got)
	}
}

func TestRollbackRouteDispatchesTheAgentAndReturns200(t *testing.T) {
	devEnv(t)
	agent := newFakeAgent()
	handler := New(agent)

	rec := doRequest(t, handler, http.MethodPost, "/rollback", nil)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	agent.awaitStart(t)
	if got := agent.dispatchedCalls().rollback; got != 1 {
		t.Errorf("rollback calls = %d, want 1", got)
	}
}

// --- asynchronous operations ------------------------------------------

// The gate is released only AFTER the response is read, so a handler that
// waited for its own operation deadlocks here rather than fails.
func TestActionRouteAnswersWhileTheOperationIsStillRunning(t *testing.T) {
	devEnv(t)
	agent := newFakeAgent()
	release := agent.gated()
	defer release()
	handler := New(agent)

	rec := doRequest(t, handler, http.MethodPost, "/apply", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if name := agent.awaitStart(t); name != "apply" {
		t.Errorf("started operation = %q, want apply", name)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "pill-applying") {
		t.Error("the immediate response does not show the operation as running")
	}
	if !strings.Contains(body, "already running") {
		t.Error("the immediate response does not tell the user an operation is in progress")
	}
	if !strings.Contains(body, `hx-trigger="every 2s"`) {
		t.Error("the immediate response does not poll at the faster busy interval")
	}

	release()
	agent.awaitFinish(t)
}

// ...and the result reaches the page through a poll, not the click.
func TestPollPicksUpTheResultAfterTheOperationFinishes(t *testing.T) {
	devEnv(t)
	agent := newFakeAgent()
	release := agent.gated()
	handler := New(agent)

	clicked := doRequest(t, handler, http.MethodPost, "/apply", nil)
	agent.awaitStart(t)
	stale := fragmentHashOf(t, clicked.Body.String())

	release()
	agent.awaitFinish(t)

	rec := doRequest(t, handler, http.MethodGet, "/fragment?h="+stale, nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 - the fragment moved on", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "pill-in_sync") {
		t.Error("the poll does not show the finished state")
	}
	if strings.Contains(body, "already running") {
		t.Error("the poll still shows the operation as running")
	}
	if !strings.Contains(body, `hx-trigger="every 5s"`) {
		t.Error("the poll did not fall back to the idle interval once the agent was free")
	}
}

// panickingAgent stands in for an operation blowing up below the agent
// interface, where nothing recovers. entered closes while the panic is
// still unwinding, so a test can wait for it instead of racing.
type panickingAgent struct {
	*fakeAgent
	entered chan struct{}
}

func (p *panickingAgent) ApplyNow(ctx context.Context, force bool) applier.Result {
	defer close(p.entered)
	panic("applier exploded")
}

var _ Agent = (*panickingAgent)(nil)

// net/http's recover does not reach the goroutine opRoute starts, so
// without recoverOp one bad apply kills the process. A regression crashes
// the test binary rather than failing it.
func TestPanicInAnOperationDoesNotKillTheProcess(t *testing.T) {
	devEnv(t)
	agent := &panickingAgent{fakeAgent: newFakeAgent(), entered: make(chan struct{})}
	handler := New(agent)

	rec := doRequest(t, handler, http.MethodPost, "/apply", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	select {
	case <-agent.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the operation goroutine never ran")
	}
	// Left usable, not wedged: opLock unwinds with the panic through its
	// deferred Unlock, so the next press is accepted.
	time.Sleep(20 * time.Millisecond)
	next := doRequest(t, handler, http.MethodGet, "/", nil)
	if next.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 - the dashboard should still be serving", next.Code)
	}
	if strings.Contains(next.Body.String(), "already running") {
		t.Error("the agent is still reported busy after the panic unwound")
	}
}

// awaitStart makes this deterministic: the first operation has recorded
// its call and marked the agent busy before the second request is made.
func TestSecondPressWhileAnOperationRunsDispatchesNothing(t *testing.T) {
	devEnv(t)
	agent := newFakeAgent()
	release := agent.gated()
	defer release()
	handler := New(agent)

	doRequest(t, handler, http.MethodPost, "/apply", nil)
	agent.awaitStart(t)
	rec := doRequest(t, handler, http.MethodPost, "/apply", nil)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if got := agent.dispatchedCalls().apply; len(got) != 1 {
		t.Errorf("apply calls = %v, want exactly one", got)
	}
	if !strings.Contains(rec.Body.String(), "already running") {
		t.Error("the second response does not say an operation is already running")
	}
}

func TestStatusJSONShape(t *testing.T) {
	devEnv(t)
	handler := New(newFakeAgent())

	rec := doRequest(t, handler, http.MethodGet, "/status.json", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	for _, key := range []string{
		"state", "busy", "configured", "dry_run", "repo_url", "branch",
		"last_sha", "pending_count", "pending_registry", "last_error", "last_apply_utc",
		"commit_back_enabled", "last_drift_branch", "history_total", "rollback_preview",
		"operation",
	} {
		if _, ok := body[key]; !ok {
			t.Errorf("status.json missing key %q", key)
		}
	}
}

func TestDoubleApplyWhileBusyDoesNotCallAgent(t *testing.T) {
	devEnv(t)
	agent := newFakeAgent()
	agent.busy = true
	handler := New(agent)

	rec := doRequest(t, handler, http.MethodPost, "/apply", nil)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if got := agent.dispatchedCalls().apply; len(got) != 0 {
		t.Errorf("apply calls = %v, want none", got)
	}
	if !strings.Contains(rec.Body.String(), "already running") {
		t.Error("body does not mention 'already running'")
	}
}

// Every URL is relative and never rewritten server-side (see web.go's
// package doc), so an X-Ingress-Path header must change nothing.
func TestHxPostURLsStayRelativeRegardlessOfIngressPathHeader(t *testing.T) {
	devEnv(t)
	handler := New(newFakeAgent())

	rec := doRequest(t, handler, http.MethodGet, "/", map[string]string{
		"X-Ingress-Path": "/api/hassio_ingress/mytoken",
	})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `hx-post="reconcile"`) {
		t.Error(`body does not contain hx-post="reconcile"`)
	}
	if !strings.Contains(body, `hx-post="apply"`) {
		t.Error(`body does not contain hx-post="apply"`)
	}
	if strings.Contains(body, "mytoken") {
		t.Error("body must not echo the ingress path back into any URL")
	}
}

func TestFirstRunPageShownWhenRepoURLUnset(t *testing.T) {
	devEnv(t)
	agent := newFakeAgent()
	agent.configured = false
	handler := New(agent)

	rec := doRequest(t, handler, http.MethodGet, "/", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "not configured yet") {
		t.Error("body does not contain 'not configured yet'")
	}
	if strings.Contains(body, "Pending changes") {
		t.Error("body must not show the pending-changes card")
	}
}

func TestApplyButtonConfirmsWhenDryRunIsOn(t *testing.T) {
	// With dry_run on, Apply overrides it and writes live config - the
	// more dangerous path, so it must always ask first.
	devEnv(t)
	agent := newFakeAgent()
	agent.dryRun = true
	handler := New(agent)

	rec := doRequest(t, handler, http.MethodGet, "/", nil)

	body := rec.Body.String()
	if !strings.Contains(body, "hx-confirm=") {
		t.Error("body does not contain hx-confirm=")
	}
	if !strings.Contains(body, "Dry run is ON") {
		t.Error("body does not contain 'Dry run is ON'")
	}
}

func TestApplyButtonConfirmsWhenDryRunIsOff(t *testing.T) {
	devEnv(t)
	agent := newFakeAgent()
	agent.dryRun = false
	handler := New(agent)

	rec := doRequest(t, handler, http.MethodGet, "/", nil)

	body := rec.Body.String()
	if !strings.Contains(body, "hx-confirm=") {
		t.Error("body does not contain hx-confirm=")
	}
	if strings.Contains(body, "Dry run is ON") {
		t.Error("body must not contain 'Dry run is ON' when dry_run is off")
	}
}

func TestRepoURLCredentialsStrippedFromRenderedPage(t *testing.T) {
	devEnv(t)
	agent := newFakeAgent()
	agent.repoURLOverride = "https://myuser:mytoken@example.invalid/demo.git"
	handler := New(agent)

	rec := doRequest(t, handler, http.MethodGet, "/", nil)

	body := rec.Body.String()
	if strings.Contains(body, "mytoken") || strings.Contains(body, "myuser") {
		t.Error("body leaks credentials from repo_url")
	}
	if !strings.Contains(body, "https://example.invalid/demo.git") {
		t.Error("body does not contain the redacted repo_url")
	}
}

func TestRepoURLCredentialsStrippedFromStatusJSON(t *testing.T) {
	devEnv(t)
	agent := newFakeAgent()
	agent.repoURLOverride = "https://myuser:mytoken@example.invalid/demo.git"
	handler := New(agent)

	rec := doRequest(t, handler, http.MethodGet, "/status.json", nil)

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if got := body["repo_url"]; got != "https://example.invalid/demo.git" {
		t.Errorf("repo_url = %v, want redacted URL", got)
	}
}

func TestRegistryChangesSectionHiddenWhenEmpty(t *testing.T) {
	devEnv(t)
	agent := newFakeAgent()
	agent.pendingRegistry = nil
	handler := New(agent)

	rec := doRequest(t, handler, http.MethodGet, "/", nil)

	if strings.Contains(rec.Body.String(), "Registry changes") {
		t.Error("body must not show the registry-changes card when empty")
	}
}

func TestRegistryChangesSectionShownWithCreateUpdateDeleteOps(t *testing.T) {
	devEnv(t)
	agent := newFakeAgent()
	agent.pendingRegistry = []recon.PendingRegOp{
		{RType: "floor", Key: "ground", Kind: "create", DiffText: "+name: 'Ground floor'\n"},
		{RType: "area", Key: "living_room", Kind: "update", DiffText: "-icon: 'mdi:old'\n+icon: 'mdi:sofa'\n"},
		{RType: "label", Key: "stale", Kind: "delete", DiffText: "-name: 'Stale'\n"},
	}
	handler := New(agent)

	rec := doRequest(t, handler, http.MethodGet, "/", nil)

	body := rec.Body.String()
	if !strings.Contains(body, `Registry changes <span class="count">3</span>`) {
		t.Error("body does not contain the registry-changes heading with count 3")
	}
	if !strings.Contains(body, "floor:ground") {
		t.Error("body does not contain floor:ground")
	}
	if !strings.Contains(body, "area:living_room") {
		t.Error("body does not contain area:living_room")
	}
	if !strings.Contains(body, "label:stale") {
		t.Error("body does not contain label:stale")
	}
}

func TestRegistryErrorOpRenderedDistinctly(t *testing.T) {
	devEnv(t)
	agent := newFakeAgent()
	agent.pendingRegistry = []recon.PendingRegOp{
		{RType: "area", Key: "office", Kind: "error", Error: "ambiguous adopt: 2 live area objects named 'Office'"},
	}
	handler := New(agent)

	rec := doRequest(t, handler, http.MethodGet, "/", nil)

	body := rec.Body.String()
	if !strings.Contains(body, "badge-error") {
		t.Error("body does not contain badge-error")
	}
	if !strings.Contains(body, "ambiguous adopt: 2 live area objects named") {
		t.Error("body does not contain the error message")
	}
}

func TestWarningsCalloutRendersWhenPresent(t *testing.T) {
	devEnv(t)
	agent := newFakeAgent()
	agent.warnings = "Integration 'templete' not found.\nPlease check your config."
	handler := New(agent)

	rec := doRequest(t, handler, http.MethodGet, "/", nil)

	body := rec.Body.String()
	if !strings.Contains(body, "Configuration warnings") {
		t.Error("body does not contain the 'Configuration warnings' heading")
	}
	if !strings.Contains(body, "Integration &#39;templete&#39; not found.") &&
		!strings.Contains(body, "Integration 'templete' not found.") {
		t.Error("body does not contain the warnings text")
	}
	if !strings.Contains(body, "card-warning") {
		t.Error("body does not contain the card-warning class")
	}
}

func TestWarningsCalloutAbsentWhenEmpty(t *testing.T) {
	devEnv(t)
	agent := newFakeAgent()
	agent.warnings = ""
	handler := New(agent)

	rec := doRequest(t, handler, http.MethodGet, "/", nil)

	if strings.Contains(rec.Body.String(), "Configuration warnings") {
		t.Error("body must not show the warnings callout when there are none")
	}
}

func TestBackupCalloutRendersWithTheReasonWhenPresent(t *testing.T) {
	devEnv(t)
	agent := newFakeAgent()
	agent.lastBackupError = "supervisor request failed after 15m0s: context deadline exceeded"
	handler := New(agent)

	rec := doRequest(t, handler, http.MethodGet, "/", nil)

	body := rec.Body.String()
	if !strings.Contains(body, "Pre-apply backup did not run") {
		t.Error("body does not contain the 'Pre-apply backup did not run' heading")
	}
	if !strings.Contains(body, "context deadline exceeded") {
		t.Error("body does not carry the reason the backup failed")
	}
}

// The callout is about a missing Supervisor backup, not a failed apply,
// and must not be conflated with the check_config warnings callout.
func TestBackupCalloutAbsentWhenTheBackupSucceeded(t *testing.T) {
	devEnv(t)
	agent := newFakeAgent()
	agent.lastBackupError = ""
	agent.warnings = "Integration 'templete' not found."
	handler := New(agent)

	rec := doRequest(t, handler, http.MethodGet, "/", nil)

	if strings.Contains(rec.Body.String(), "Pre-apply backup did not run") {
		t.Error("body must not show the backup callout when the backup succeeded")
	}
}

// --- add-on updates ---------------------------------------------------

func TestAddonUpdateCardHiddenWhenTheOptionIsEmpty(t *testing.T) {
	devEnv(t)
	agent := newFakeAgent()
	agent.autoUpdateEnabled = false
	handler := New(agent)

	if strings.Contains(doRequest(t, handler, http.MethodGet, "/", nil).Body.String(), "Add-on updates") {
		t.Error("body must not show the add-on update card when auto_update_addons is empty")
	}
}

// Two minutes of startup delay precede the first check, so a card that
// waited for results would look like the option had been ignored.
func TestAddonUpdateCardShownBeforeTheFirstCheck(t *testing.T) {
	devEnv(t)
	agent := newFakeAgent()
	agent.autoUpdateEnabled = true
	handler := New(agent)

	body := doRequest(t, handler, http.MethodGet, "/", nil).Body.String()

	if !strings.Contains(body, `Add-on updates <span class="count">0</span>`) {
		t.Error("body does not show the add-on update card with an empty count")
	}
	if !strings.Contains(body, "No results yet") {
		t.Error("body does not explain that there is nothing to show yet")
	}
}

// addonUpdateCard is the rendered add-on update card alone. It holds no
// nested <section>, so the first closing tag after the heading is its own.
func addonUpdateCard(t *testing.T, body string) string {
	t.Helper()
	_, card, ok := strings.Cut(body, "<h2>Add-on updates")
	if !ok {
		t.Fatal("no add-on update card in the rendered page")
	}
	card, _, ok = strings.Cut(card, "</section>")
	if !ok {
		t.Fatal("the add-on update card is not closed")
	}
	return card
}

// addonFoldMarker opens the <details> the never-actionable rows sit in,
// and is the only <details> the card renders.
const addonFoldMarker = `<details class="change">`

// addonUpdateRows splits the card's MAIN list per row. Cutting at the
// fold is load bearing: both lists use the same partial, so document
// order alone would let a folded row satisfy a main-list assertion.
func addonUpdateRows(t *testing.T, body string) []string {
	t.Helper()
	above, _, _ := strings.Cut(addonUpdateCard(t, body), addonFoldMarker)
	return addonRowChunks(above)
}

// addonUpdateFoldedRows is the same for rows behind the fold. No
// <details> means no folded rows: every verdict was actionable.
func addonUpdateFoldedRows(t *testing.T, body string) []string {
	t.Helper()
	_, fold, ok := strings.Cut(addonUpdateCard(t, body), addonFoldMarker)
	if !ok {
		return nil
	}
	return addonRowChunks(fold)
}

// addonRowChunks splits one list at a row's opening tag. The closing
// bracket is part of the separator so a row's inner change-summary
// cannot split that row in half.
func addonRowChunks(list string) []string {
	return strings.Split(list, `<div class="change">`)[1:]
}

func TestAddonUpdateRowsRenderVersionsAndVerdicts(t *testing.T) {
	devEnv(t)
	agent := newFakeAgent()
	agent.autoUpdateEnabled = true
	agent.addonUpdates = []recon.AddonUpdateStatus{
		{
			Slug: "core_configurator", Name: "File editor",
			Version: "5.9.0", LatestVersion: "5.9.0",
			LastResult: "up to date", LastCheckedUTC: "2026-08-03T14:12:07+00:00",
		},
		{
			Slug: "a0d7b954_esphome", Name: "ESPHome Device Builder",
			Version: "2026.7.3", LatestVersion: "2026.8.0", UpdateAvailable: true,
			LastResult: "update available (dry run, not installing)", LastCheckedUTC: "2026-08-03T14:12:07+00:00",
		},
		// No name, no versions - a check that got no answer. The row must
		// still say which add-on it is about.
		{Slug: "core_typo", LastResult: "not installed", LastCheckedUTC: "2026-08-03T14:12:07+00:00"},
	}
	handler := New(agent)

	body := doRequest(t, handler, http.MethodGet, "/", nil).Body.String()

	for _, want := range []string{
		// The count is how many are behind, not how many are watched.
		`Add-on updates <span class="count">1</span>`,
		"File editor",
		"<code>5.9.0</code>",
		"ESPHome Device Builder",
		"<code>2026.7.3 -&gt; 2026.8.0</code>",
		"core_typo",
		"not installed",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body does not contain %q", want)
		}
	}
	// A current add-on shows the one version it is on, not it twice.
	if strings.Contains(body, "<code>5.9.0 -&gt; 5.9.0</code>") {
		t.Error("an up-to-date add-on must not render as an update")
	}

	// One badge per row shape, asserted against its own row. core_typo is
	// never actionable so it folds, while staying THIRD in document order.
	rows := addonUpdateRows(t, body)
	if len(rows) != 2 {
		t.Fatalf("the main list rendered %d rows, want 2", len(rows))
	}
	for i, want := range []struct{ name, badge string }{
		{"File editor", "badge-add"},
		{"ESPHome Device Builder", "badge-update"},
	} {
		if !strings.Contains(rows[i], want.name) {
			t.Errorf("row %d is not %s: %s", i, want.name, rows[i])
			continue
		}
		if !strings.Contains(rows[i], want.badge) {
			t.Errorf("row %d (%s) does not carry %s: %s", i, want.name, want.badge, rows[i])
		}
	}

	folded := addonUpdateFoldedRows(t, body)
	if len(folded) != 1 {
		t.Fatalf("the fold holds %d rows, want the one not-installed row", len(folded))
	}
	if !strings.Contains(folded[0], "core_typo") {
		t.Errorf("the folded row is not core_typo: %s", folded[0])
	}
	// The same "unknown" badge a failed check renders, which is why the
	// verdict decides the fold and not the badge.
	if !strings.Contains(folded[0], "badge-restore") {
		t.Errorf("the folded row does not carry badge-restore: %s", folded[0])
	}
}

// The fold's whole contract in one render: which verdicts sit behind it,
// what the summary counts, and that folding never drops - a silently
// missing row is how a typo'd slug stays invisible.
func TestNeverActionableRowsRenderBehindTheFold(t *testing.T) {
	devEnv(t)
	agent := newFakeAgent()
	agent.autoUpdateEnabled = true
	agent.addonUpdates = []recon.AddonUpdateStatus{
		{
			Slug: "core_configurator", Name: "File editor",
			Version: "5.9.0", LatestVersion: "5.9.0", LastResult: "up to date",
		},
		{
			Slug: "a0d7b954_esphome", Name: "ESPHome Device Builder",
			Version: "2026.7.3", LatestVersion: "2026.8.0", UpdateAvailable: true,
			LastResult: "updated to 2026.8.0",
		},
		// The one "unknown" badge that stays above the fold: Supervisor
		// was unreachable, the next check may succeed, and this is the
		// only genuinely broken row. Folding it would hide it.
		{Slug: "core_mariadb", Name: "MariaDB", LastResult: "check failed: supervisor request failed with 502: Bad Gateway"},
		// Written from the consts Actionable switches on, so a reworded
		// verdict fails here rather than silently changing lists.
		{Slug: "core_typo", LastResult: recon.AddonUpdateNotInstalled},
		{Slug: "local_ha_gitops_agent", Name: "local_ha_gitops_agent", LastResult: recon.AddonUpdateRefusedSelf},
	}
	handler := New(agent)

	body := doRequest(t, handler, http.MethodGet, "/", nil).Body.String()

	rows := addonUpdateRows(t, body)
	if len(rows) != 3 {
		t.Fatalf("the main list rendered %d rows, want 3", len(rows))
	}
	for i, want := range []string{"File editor", "ESPHome Device Builder", "MariaDB"} {
		if !strings.Contains(rows[i], want) {
			t.Errorf("main row %d is not %s: %s", i, want, rows[i])
		}
	}

	folded := addonUpdateFoldedRows(t, body)
	if len(folded) != 2 {
		t.Fatalf("the fold holds %d rows, want 2", len(folded))
	}
	for i, want := range []string{"core_typo", "local_ha_gitops_agent"} {
		if !strings.Contains(folded[i], want) {
			t.Errorf("folded row %d is not %s: %s", i, want, folded[i])
		}
	}

	card := addonUpdateCard(t, body)
	// The unit is spelled out for a screen reader that would otherwise
	// hear the label and a bare 2.
	if !strings.Contains(card, `<span class="count">2<span class="sr-only"> add-ons</span>`) {
		t.Error("the fold's summary does not count the rows behind it")
	}
	// Folding is not dropping: the heading still counts updates waiting,
	// and every watched slug is still on the page.
	if !strings.Contains(body, `Add-on updates <span class="count">1</span>`) {
		t.Error("the heading count moved when rows were folded")
	}
	for _, slug := range []string{"core_configurator", "a0d7b954_esphome", "core_mariadb", "core_typo", "local_ha_gitops_agent"} {
		if !strings.Contains(body, slug) {
			t.Errorf("the card dropped %s instead of folding it", slug)
		}
	}
}

// Without the line, a fully folded card is a heading, a 0 and one
// collapsed row - which reads as a check that found nothing.
func TestACardWithNothingAboveTheFoldSaysSo(t *testing.T) {
	devEnv(t)
	agent := newFakeAgent()
	agent.autoUpdateEnabled = true
	agent.addonUpdates = []recon.AddonUpdateStatus{
		{Slug: "core_typo", LastResult: recon.AddonUpdateNotInstalled},
		{Slug: "local_ha_gitops_agent", Name: "local_ha_gitops_agent", LastResult: recon.AddonUpdateRefusedSelf},
	}
	handler := New(agent)

	body := doRequest(t, handler, http.MethodGet, "/", nil).Body.String()

	if rows := addonUpdateRows(t, body); len(rows) != 0 {
		t.Fatalf("the main list rendered %d rows, want none", len(rows))
	}
	if !strings.Contains(body, "Nothing here this agent can update") {
		t.Error("the card does not explain that its results are all below the fold")
	}
	// The other empty state means no results at all. Rendering both would
	// say the check has not run AND that it ran and found nothing.
	if strings.Contains(body, "No results yet") {
		t.Error("a card holding results renders the never-checked empty state")
	}
}

// unwrapped collapses whitespace runs to one space, for assertions about
// a rendered SENTENCE rather than where the template hard-wraps it -
// otherwise rewrapping a paragraph breaks a test about its words.
func unwrapped(body string) string {
	return strings.Join(strings.Fields(body), " ")
}

// Unlike every toolbar button, this one is disabled by a check IN FLIGHT
// and never by Busy (checkLock, not opLock), and it confirms first,
// because a found update is installed and restarts that add-on.
func TestCheckButtonDisablesItselfWhileCheckingAndConfirmsBeforeInstalling(t *testing.T) {
	devEnv(t)
	for _, tc := range []struct {
		name         string
		dryRun       bool
		checkRunning bool
		busy         bool
		wantDisabled bool
		wantConfirm  bool
	}{
		{name: "idle", wantConfirm: true},
		{name: "checking", checkRunning: true, wantDisabled: true, wantConfirm: true},
		{name: "dry run installs nothing", dryRun: true},
		{name: "another operation running", busy: true, wantConfirm: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agent := newFakeAgent()
			agent.autoUpdateEnabled = true
			agent.dryRun = tc.dryRun
			agent.addonCheckRunning = tc.checkRunning
			agent.busy = tc.busy

			body := doRequest(t, New(agent), http.MethodGet, "/", nil).Body.String()

			button := hxButton(t, body, "addons/check")
			for _, want := range []string{`hx-disabled-elt="this"`, `<span class="spinner" aria-hidden="true"></span>`} {
				if !strings.Contains(button, want) {
					t.Errorf("the check button does not carry %s", want)
				}
			}
			if got := buttonIsDisabled(button); got != tc.wantDisabled {
				t.Errorf("disabled = %v, want %v: %s", got, tc.wantDisabled, button)
			}
			if got := strings.Contains(button, "hx-confirm="); got != tc.wantConfirm {
				t.Errorf("hx-confirm = %v, want %v: %s", got, tc.wantConfirm, button)
			}
			// The line under the heading covers the check itself: the
			// spinner goes as soon as the route answers, which is before
			// the check has done anything.
			if got := strings.Contains(body, "Checking for updates now"); got != tc.checkRunning {
				t.Errorf("the checking-now line is %v, want %v", got, tc.checkRunning)
			}
		})
	}
}

func TestPostAddonsCheckDispatchesTheAgentAndReturns200(t *testing.T) {
	devEnv(t)
	agent := newFakeAgent()
	agent.autoUpdateEnabled = true
	handler := New(agent)

	rec := doRequest(t, handler, http.MethodPost, "/addons/check", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	agent.awaitStart(t)
	if got := agent.dispatchedCalls().addonCheck; got != 1 {
		t.Errorf("check calls = %d, want 1", got)
	}
	// The press answers with the dashboard; results arrive by poll.
	if !strings.Contains(rec.Body.String(), "Add-on updates") {
		t.Error("the press does not answer with the rendered fragment")
	}
}

// The one route that does not refuse while something else runs, and why
// it is not an opRoute: Busy is opLock, this gates on checkLock. Contrast
// TestSecondPressWhileAnOperationRunsDispatchesNothing.
func TestAddonCheckDispatchesEvenWhileAnOperationIsRunning(t *testing.T) {
	devEnv(t)
	agent := newFakeAgent()
	agent.autoUpdateEnabled = true
	release := agent.gated()
	defer release()
	handler := New(agent)

	doRequest(t, handler, http.MethodPost, "/apply", nil)
	// The apply holds the agent busy before the check is pressed, which
	// is what makes this deterministic.
	if name := agent.awaitStart(t); name != "apply" {
		t.Fatalf("first press started %q, want apply", name)
	}

	rec := doRequest(t, handler, http.MethodPost, "/addons/check", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if name := agent.awaitStart(t); name != "addoncheck" {
		t.Fatalf("second press started %q, want the check to have been dispatched anyway", name)
	}
	if got := agent.dispatchedCalls().addonCheck; got != 1 {
		t.Errorf("check calls = %d, want 1 even with an operation in flight", got)
	}
	release()
	agent.awaitFinish(t)
}

// The combined pending_count (files + registry) belongs to the Apply
// confirm, not this heading, which lists only file changes.
func TestPendingChangesHeadingCountsOnlyFileChanges(t *testing.T) {
	devEnv(t)
	agent := newFakeAgent()
	agent.pendingCount = 3
	agent.pendingRegistry = []recon.PendingRegOp{
		{RType: "floor", Key: "ground", Kind: "create", DiffText: "+x"},
		{RType: "label", Key: "gitops", Kind: "create", DiffText: "+x"},
	}
	handler := New(agent)

	rec := doRequest(t, handler, http.MethodGet, "/", nil)

	body := rec.Body.String()
	if !strings.Contains(body, `Pending changes <span class="count">1</span>`) {
		t.Error("body does not contain the pending-changes heading with count 1")
	}
	if !strings.Contains(body, `Registry changes <span class="count">2</span>`) {
		t.Error("body does not contain the registry-changes heading with count 2")
	}
}

// applyButton returns the Apply <button> alone, so an assertion about its
// attributes cannot be satisfied by another button on the page.
func applyButton(t *testing.T, body string) string {
	t.Helper()
	return hxButton(t, body, "apply")
}

// buttonIsDisabled reports whether a button carries the bare disabled
// attribute - not just the word, which hx-disabled-elt also contains.
func buttonIsDisabled(markup string) bool {
	return strings.Contains(strings.ReplaceAll(markup, "hx-disabled-elt", ""), "disabled")
}

// The confirm quotes ApplyableCount, not PendingCount: an error-kind op
// is counted but never executed, and a dialog that promises four and
// makes three is one nobody reads twice.
func TestApplyConfirmCountsOnlyWhatApplyWillAttempt(t *testing.T) {
	devEnv(t)
	agent := newFakeAgent()
	agent.pendingCount = 1
	agent.pendingRegistry = []recon.PendingRegOp{
		{RType: "floor", Key: "ground", Kind: "create", DiffText: "+x"},
		{RType: "area", Key: "living_room", Kind: "update", DiffText: "+x"},
		{RType: "area", Key: "office", Kind: "error", Error: "ambiguous adopt"},
	}
	handler := New(agent)

	body := doRequest(t, handler, http.MethodGet, "/", nil).Body.String()

	if got := agent.Status().PendingCount; got != 4 {
		t.Fatalf("fixture pending_count = %d, want 4 - this case only means something when the two counts differ", got)
	}
	button := applyButton(t, body)
	if !strings.Contains(button, "Apply 3 change(s)") {
		t.Errorf("apply button = %q, want a confirm quoting 3 applyable change(s)", button)
	}
	if strings.Contains(button, "Apply 4 change(s)") {
		t.Error("the apply confirm counts the error-kind registry op it will not apply")
	}
}

// The same in the dry-run wording, a separate template string that has
// drifted from its sibling before.
func TestApplyConfirmCountsOnlyWhatApplyWillAttemptInDryRun(t *testing.T) {
	devEnv(t)
	agent := newFakeAgent()
	agent.dryRun = true
	agent.pendingCount = 1
	agent.pendingRegistry = []recon.PendingRegOp{
		{RType: "area", Key: "office", Kind: "error", Error: "ambiguous adopt"},
	}
	handler := New(agent)

	body := doRequest(t, handler, http.MethodGet, "/", nil).Body.String()

	button := applyButton(t, body)
	if !strings.Contains(button, "Write 1 change(s)") {
		t.Errorf("apply button = %q, want the dry-run confirm quoting 1 applyable change(s)", button)
	}
}

// A plan made up entirely of error-kind ops has nothing to apply: the
// reconcile loop already refuses to act on one (see recon's tick), and
// pressing Apply would take a full Supervisor backup to execute nothing.
func TestApplyButtonDisabledWhenOnlyErrorRegistryOpsArePending(t *testing.T) {
	devEnv(t)
	agent := newFakeAgent()
	agent.pendingCount = 0
	agent.pendingRegistry = []recon.PendingRegOp{
		{RType: "area", Key: "office", Kind: "error", Error: "ambiguous adopt"},
	}
	handler := New(agent)

	body := doRequest(t, handler, http.MethodGet, "/", nil).Body.String()

	if got := agent.Status().PendingCount; got != 1 {
		t.Fatalf("fixture pending_count = %d, want 1 - the item must still be counted as pending", got)
	}
	if button := applyButton(t, body); !buttonIsDisabled(button) {
		t.Errorf("apply button = %q, want it disabled with nothing applyable", button)
	}
}

func TestApplyButtonEnabledWithExecutableRegistryOpsAndNoFileChanges(t *testing.T) {
	devEnv(t)
	agent := newFakeAgent()
	agent.pendingCount = 0
	agent.pendingRegistry = []recon.PendingRegOp{
		{RType: "floor", Key: "ground", Kind: "create", DiffText: "+x"},
	}
	handler := New(agent)

	body := doRequest(t, handler, http.MethodGet, "/", nil).Body.String()

	button := applyButton(t, body)
	if buttonIsDisabled(button) {
		t.Errorf("apply button = %q, want it enabled - the registry op is applyable", button)
	}
	if !strings.Contains(button, "Apply 1 change(s)") {
		t.Errorf("apply button = %q, want a confirm quoting 1 change", button)
	}
}

// With nothing composed at apply time, the dialog says what is true of
// any apply rather than nothing at all.
func TestRollbackConfirmMentionsRegistryObjects(t *testing.T) {
	devEnv(t)
	agent := newFakeAgent()
	agent.lastStashDir = "/data/backup/x"
	handler := New(agent)

	rec := doRequest(t, handler, http.MethodGet, "/", nil)

	if !strings.Contains(rec.Body.String(), "files and registry objects") {
		t.Error("body does not mention 'files and registry objects'")
	}
}

// With a preview composed at apply time, the dialog quotes what THIS
// stash holds rather than what an apply holds in general.
func TestRollbackConfirmQuotesWhatTheLastApplyStashed(t *testing.T) {
	devEnv(t)
	agent := newFakeAgent()
	agent.lastStashDir = "/data/backup/x"
	agent.rollbackPreview = "3 file(s), registry objects and integrations"
	handler := New(agent)

	body := doRequest(t, handler, http.MethodGet, "/", nil).Body.String()

	if !strings.Contains(body, "This puts 3 file(s), registry objects and integrations back as they were before it") {
		t.Error("body does not quote the composed rollback preview")
	}
	if strings.Contains(body, "files and registry objects touched by the most recent apply") {
		t.Error("body still carries the generic wording beside a composed preview")
	}
}

// Half of a rollback is deletion: a file the apply CREATED was stashed as
// absent and is removed again, so the dialog must not call it a restore.
func TestRollbackConfirmSaysCreatedFilesAreRemoved(t *testing.T) {
	devEnv(t)
	agent := newFakeAgent()
	agent.lastStashDir = "/data/backup/x"
	agent.rollbackPreview = "12 file(s)"
	handler := New(agent)

	body := doRequest(t, handler, http.MethodGet, "/", nil).Body.String()

	if !strings.Contains(body, "files the apply created are removed") {
		t.Error("the confirm does not say that created files are removed")
	}
	if strings.Contains(body, "backup.") {
		t.Error("the confirm still calls the rollback a restore from a backup")
	}
}

// An apply whose layers kept no stash rolls back successfully and changes
// nothing, so promising files there would be an untruth.
func TestRollbackConfirmSaysSoWhenThereIsNothingToRestore(t *testing.T) {
	devEnv(t)
	agent := newFakeAgent()
	agent.lastStashDir = "/data/backup/x"
	agent.rollbackPreview = recon.RollbackPreviewNothing
	handler := New(agent)

	body := doRequest(t, handler, http.MethodGet, "/", nil).Body.String()

	if !strings.Contains(body, "rolling back will not change your configuration") {
		t.Error("the confirm does not say that a rollback would restore nothing")
	}
	for _, unwanted := range []string{
		"files and registry objects touched by the most recent apply",
		"back as they were before it",
	} {
		if strings.Contains(body, unwanted) {
			t.Errorf("the confirm still carries %q beside a stash holding nothing", unwanted)
		}
	}
}

// hangingAgent stands in for an agent mid-apply: ApplyNow blocks until it
// observes cancellation or a grace period elapses, recording which. Long
// applies are normal - the health probe alone runs up to five minutes.
type hangingAgent struct {
	*fakeAgent
	started  chan struct{}
	observed chan error
}

func newHangingAgent() *hangingAgent {
	return &hangingAgent{
		fakeAgent: newFakeAgent(),
		started:   make(chan struct{}),
		observed:  make(chan error, 1),
	}
}

func (h *hangingAgent) ApplyNow(ctx context.Context, force bool) applier.Result {
	close(h.started)
	select {
	case <-ctx.Done():
		h.observed <- ctx.Err()
	case <-time.After(250 * time.Millisecond):
		h.observed <- nil
	}
	return applier.Result{OK: true}
}

var _ Agent = (*hangingAgent)(nil)

// An apply must outlive its request - both a client going away and the
// response finishing (see opContext). Needs a real server, since
// httptest.NewRequest's context is not cancellable.
func TestApplyOutlivesTheRequestThatStartedIt(t *testing.T) {
	devEnv(t)
	agent := newHangingAgent()
	srv := httptest.NewServer(New(agent))
	defer srv.Close()

	reqCtx, cancel := context.WithCancel(context.Background())
	go func() {
		req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, srv.URL+"/apply", nil)
		if err != nil {
			return
		}
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_ = resp.Body.Close()
		}
	}()

	<-agent.started
	cancel()

	select {
	case err := <-agent.observed:
		if err != nil {
			t.Errorf("apply saw %v after the client hung up; it must run to completion", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("apply never returned")
	}
}

// --- commit drift back ------------------------------------------------

func TestCommitBackButtonHiddenWhenOptionDisabled(t *testing.T) {
	devEnv(t)
	agent := newFakeAgent()
	agent.commitBackEnabled = false
	handler := New(agent)

	rec := doRequest(t, handler, http.MethodGet, "/", nil)

	if strings.Contains(rec.Body.String(), "Commit Back") {
		t.Error("body must not show the commit-back button when commit_back is disabled")
	}
}

func TestCommitBackButtonHiddenWhenNoPendingFiles(t *testing.T) {
	devEnv(t)
	agent := newFakeAgent()
	agent.commitBackEnabled = true
	agent.pendingCount = 0
	handler := New(agent)

	rec := doRequest(t, handler, http.MethodGet, "/", nil)

	if strings.Contains(rec.Body.String(), "Commit Back") {
		t.Error("body must not show the commit-back button with no pending file drift")
	}
}

func TestCommitBackButtonShownWhenEnabledWithPendingFiles(t *testing.T) {
	devEnv(t)
	agent := newFakeAgent()
	agent.commitBackEnabled = true
	handler := New(agent)

	rec := doRequest(t, handler, http.MethodGet, "/", nil)

	body := rec.Body.String()
	if !strings.Contains(body, "Commit Back") {
		t.Error("body does not show the commit-back button")
	}
	if !strings.Contains(body, `hx-post="commitback"`) {
		t.Error(`body does not contain hx-post="commitback"`)
	}
}

func TestCommitBackRouteDispatchesTheAgentAndReturns200(t *testing.T) {
	devEnv(t)
	agent := newFakeAgent()
	handler := New(agent)

	rec := doRequest(t, handler, http.MethodPost, "/commitback", nil)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	agent.awaitStart(t)
	if got := agent.dispatchedCalls().commitBack; got != 1 {
		t.Errorf("commit back calls = %d, want 1", got)
	}
}

func TestCommitBackRouteSkipsAgentWhenBusy(t *testing.T) {
	devEnv(t)
	agent := newFakeAgent()
	agent.busy = true
	handler := New(agent)

	rec := doRequest(t, handler, http.MethodPost, "/commitback", nil)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if got := agent.dispatchedCalls().commitBack; got != 0 {
		t.Errorf("commit back calls = %d, want 0", got)
	}
}

func TestLastDriftBranchRenderedWhenSet(t *testing.T) {
	devEnv(t)
	agent := newFakeAgent()
	agent.lastDriftBranch = "gitops/drift-20260802T120000Z"
	handler := New(agent)

	rec := doRequest(t, handler, http.MethodGet, "/", nil)

	if !strings.Contains(rec.Body.String(), "gitops/drift-20260802T120000Z") {
		t.Error("body does not show the last drift branch")
	}
}

func TestLastDriftBranchHiddenWhenUnset(t *testing.T) {
	devEnv(t)
	agent := newFakeAgent()
	agent.lastDriftBranch = ""
	handler := New(agent)

	rec := doRequest(t, handler, http.MethodGet, "/", nil)

	if strings.Contains(rec.Body.String(), "Last drift branch") {
		t.Error("body must not mention the last drift branch when unset")
	}
}

// --- import -----------------------------------------------------------

func TestImportButtonsHiddenWhenOptionDisabled(t *testing.T) {
	devEnv(t)
	agent := newFakeAgent()
	agent.importEnabled = false
	handler := New(agent)

	body := doRequest(t, handler, http.MethodGet, "/", nil).Body.String()

	if strings.Contains(body, "title=\"Import from Home Assistant\"") || strings.Contains(body, "title=\"Preview what an import would copy\"") {
		t.Error("body must not show the import buttons when allow_import is disabled")
	}
}

// Import exists to capture files that produce no drift, so gating it on
// pending changes would hide it in the state it is for.
func TestImportButtonsShownWithNoPendingDrift(t *testing.T) {
	devEnv(t)
	agent := newFakeAgent()
	agent.importEnabled = true
	agent.pendingCount = 0
	handler := New(agent)

	body := doRequest(t, handler, http.MethodGet, "/", nil).Body.String()

	if !strings.Contains(body, "title=\"Import from Home Assistant\"") {
		t.Error("body does not show the import button with no pending drift")
	}
	if !strings.Contains(body, `hx-post="import"`) {
		t.Error(`body does not contain hx-post="import"`)
	}
	if !strings.Contains(body, `hx-post="import/preview"`) {
		t.Error(`body does not contain hx-post="import/preview"`)
	}
}

// The only button that writes to the branch the user works on, so a
// future edit must not soften what it says.
func TestImportConfirmNamesTheTrackedBranch(t *testing.T) {
	devEnv(t)
	agent := newFakeAgent()
	agent.importEnabled = true
	handler := New(agent)

	body := doRequest(t, handler, http.MethodGet, "/", nil).Body.String()

	if !strings.Contains(body, "directly onto the main branch") {
		t.Error("the import confirmation does not warn that it pushes directly onto the tracked branch")
	}
}

func TestImportConfirmMentionsAPreviousImport(t *testing.T) {
	devEnv(t)
	agent := newFakeAgent()
	agent.importEnabled = true
	agent.lastImportUTC = "2026-08-01T00:00:00+00:00"
	handler := New(agent)

	body := doRequest(t, handler, http.MethodGet, "/", nil).Body.String()

	if !strings.Contains(body, "already seeded") {
		t.Error("a repeat import does not warn that the repository was already seeded")
	}
}

// The confirm is server-rendered and stays UTC while the "Last import"
// line below is localized, so a timestamp here would put the same import
// on screen twice, hours apart.
func TestImportConfirmQuotesNoTimestamp(t *testing.T) {
	devEnv(t)
	agent := newFakeAgent()
	agent.importEnabled = true
	agent.lastImportUTC = "2026-08-01T09:30:00+00:00"
	handler := New(agent)

	body := doRequest(t, handler, http.MethodGet, "/", nil).Body.String()

	if !strings.Contains(body, "This repository was already seeded. Import again") {
		t.Error("the import confirm does not warn that the repository was already seeded")
	}
	for _, unwanted := range []string{
		"already seeded on",  // the humanized form the confirm used to carry
		"seeded on Aug 1",    // ... in either shape
		"seeded on 2026-08-", // or the raw RFC3339 one
	} {
		if strings.Contains(body, unwanted) {
			t.Errorf("the import confirm still quotes a timestamp (%q)", unwanted)
		}
	}
}

func TestPostImportDispatchesTheAgent(t *testing.T) {
	devEnv(t)
	agent := newFakeAgent()
	agent.importEnabled = true
	handler := New(agent)

	rec := doRequest(t, handler, http.MethodPost, "/import", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	agent.awaitStart(t)
	if got := agent.dispatchedCalls().importLive; got != 1 {
		t.Errorf("import calls = %d, want 1", got)
	}
}

// The preview's result reaches the page by poll, not by the press, so
// this dispatches and then polls for the render.
func TestPostImportPreviewDispatchesTheAgentAndThePollRendersTheResult(t *testing.T) {
	devEnv(t)
	agent := newFakeAgent()
	agent.importEnabled = true
	agent.importPreview = &recon.ImportPreview{
		Files:           []string{"configuration.yaml", "packages/lights.yaml"},
		TotalBytes:      2048,
		SkippedExcluded: 4,
		SkippedSecret:   1,
	}
	handler := New(agent)

	pressed := doRequest(t, handler, http.MethodPost, "/import/preview", nil)

	if pressed.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", pressed.Code)
	}
	agent.awaitFinish(t)
	if got := agent.dispatchedCalls().preview; got != 1 {
		t.Errorf("preview calls = %d, want 1", got)
	}

	rec := doRequest(t, handler, http.MethodGet, "/fragment", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("poll status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Import preview") {
		t.Error("body does not render the preview card")
	}
	if !strings.Contains(body, "packages/lights.yaml") {
		t.Error("body does not list the previewed files")
	}
	if !strings.Contains(body, "2.0 KB") {
		t.Error("body does not render the total size in human units")
	}
	if !strings.Contains(body, "1 secret-shaped") {
		t.Error("body does not report what the scan passed over")
	}
	// The names collapse in CSS, not a second request, which is why the
	// assertions above still hold.
	if !strings.Contains(body, `<ul class="inventory" tabindex="0">`) {
		t.Error("the previewed files are not in the collapsed inventory list")
	}
	if !strings.Contains(hxButton(t, body, "import/preview/dismiss"), "Dismiss") {
		t.Error("the card the preview produced carries no Dismiss button")
	}
}

// importPreviewCard is the preview card alone, so a file-list assertion
// cannot be satisfied by the managed-inventory card's own "files" group,
// rendered by the same partial further down.
func importPreviewCard(t *testing.T, body string) string {
	t.Helper()
	_, card, ok := strings.Cut(body, "<h2>Import preview")
	if !ok {
		t.Fatal("no import preview card in the rendered page")
	}
	card, _, ok = strings.Cut(card, "</section>")
	if !ok {
		t.Fatal("the import preview card is not closed")
	}
	return card
}

// Dismiss answers inline, not by poll: nothing blocks, so the response to
// the press is already the page without the card.
func TestPostImportPreviewDismissClearsTheCard(t *testing.T) {
	devEnv(t)
	agent := newFakeAgent()
	agent.importEnabled = true
	agent.importPreview = &recon.ImportPreview{
		Files:      []string{"configuration.yaml", "packages/lights.yaml"},
		TotalBytes: 2048,
	}
	handler := New(agent)

	if !strings.Contains(doRequest(t, handler, http.MethodGet, "/", nil).Body.String(), "Import preview") {
		t.Fatal("the fixture does not render a preview to dismiss")
	}

	rec := doRequest(t, handler, http.MethodPost, "/import/preview/dismiss", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := agent.dispatchedCalls().dismissPreview; got != 1 {
		t.Errorf("dismiss calls = %d, want 1", got)
	}
	if strings.Contains(rec.Body.String(), "Import preview") {
		t.Error("the response to the press still renders the card it dismissed")
	}
	// And it stays gone: the agent forgot it, so the poll brings nothing.
	if strings.Contains(doRequest(t, handler, http.MethodGet, "/fragment", nil).Body.String(), "Import preview") {
		t.Error("the next poll renders the dismissed card again")
	}
}

// Clearing a card this tall is most wanted while something else runs, so
// it is not gated on Busy, and one press rebuilds it, so it asks nothing.
func TestDismissStaysAvailableWhileAnOperationRunsAndAsksNoConfirmation(t *testing.T) {
	devEnv(t)
	agent := newFakeAgent()
	agent.importEnabled = true
	agent.importPreview = &recon.ImportPreview{Files: []string{"configuration.yaml"}, TotalBytes: 2048}
	agent.busy = true
	handler := New(agent)

	body := doRequest(t, handler, http.MethodGet, "/", nil).Body.String()

	button := hxButton(t, body, "import/preview/dismiss")
	if buttonIsDisabled(button) {
		t.Errorf("the Dismiss button is disabled while the agent is busy: %s", button)
	}
	if strings.Contains(button, "hx-confirm") {
		t.Errorf("the Dismiss button confirms before discarding a preview one press rebuilds: %s", button)
	}

	rec := doRequest(t, handler, http.MethodPost, "/import/preview/dismiss", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := agent.dispatchedCalls().dismissPreview; got != 1 {
		t.Errorf("dismiss calls = %d, want 1 while busy", got)
	}
}

// The preview list uses the same capped partial the managed card does: a
// real install previews 191 names, re-hashed every poll. A DISPLAY
// decision, so /status.json stays whole and the cap line can point at it.
func TestImportPreviewListIsCappedWhileStatusJSONStaysComplete(t *testing.T) {
	devEnv(t)
	const extra = 42
	files := make([]string, inventoryMax+extra)
	for i := range files {
		files[i] = fmt.Sprintf("packages/p%04d.yaml", i)
	}
	agent := newFakeAgent()
	agent.importEnabled = true
	agent.importPreview = &recon.ImportPreview{Files: files, TotalBytes: 17_931_872}
	handler := New(agent)

	body := doRequest(t, handler, http.MethodGet, "/", nil).Body.String()

	card := importPreviewCard(t, body)
	if !strings.Contains(card, "<details") {
		t.Error("the previewed files are not collapsed behind a details")
	}
	// tabindex, because the list is height-capped and scrolled in CSS:
	// without it a keyboard user cannot reach the names below the fold.
	if !strings.Contains(card, `<ul class="inventory" tabindex="0">`) {
		t.Error("the previewed files are not in the keyboard-reachable inventory list")
	}
	if rendered := strings.Count(card, "<code>packages/p"); rendered > inventoryMax {
		t.Errorf("rendered %d names, want at most %d", rendered, inventoryMax)
	}
	// The summary counts the group's REAL size: a capped count would say
	// the import is smaller than it is.
	summary := summaryOf(t, card, "files")
	if !strings.Contains(summary, `<span class="count">`+strconv.Itoa(len(files))) {
		t.Errorf("the files summary = %q, want the full count %d", summary, len(files))
	}
	if !strings.Contains(card, "... and 42 more, listed in full by status.json") {
		t.Error("the card does not say how many names it left out")
	}

	var status recon.Status
	if err := json.NewDecoder(doRequest(t, handler, http.MethodGet, "/status.json", nil).Body).Decode(&status); err != nil {
		t.Fatalf("decoding status.json: %v", err)
	}
	if status.ImportPreview == nil {
		t.Fatal("status.json carries no import preview")
	}
	if got := len(status.ImportPreview.Files); got != len(files) {
		t.Errorf("status.json carries %d files, want all %d - the cap is the view's, not the API's", got, len(files))
	}
}

func TestPostImportSkippedWhenBusy(t *testing.T) {
	devEnv(t)
	agent := newFakeAgent()
	agent.importEnabled = true
	agent.busy = true
	handler := New(agent)

	doRequest(t, handler, http.MethodPost, "/import", nil)

	if got := agent.dispatchedCalls().importLive; got != 0 {
		t.Errorf("import calls = %d, want 0 while busy", got)
	}
}

func TestImportErrorRenderedInItsOwnCallout(t *testing.T) {
	devEnv(t)
	agent := newFakeAgent()
	agent.importEnabled = true
	agent.lastImportError = "refusing to import: total size 412.7 MB exceeds the 100.0 MB limit"
	handler := New(agent)

	body := doRequest(t, handler, http.MethodGet, "/", nil).Body.String()

	if !strings.Contains(body, "Last import error") {
		t.Error("body does not render the import error callout")
	}
	if !strings.Contains(body, "412.7 MB") {
		t.Error("body does not render the cap message itself")
	}
}

// --- polling ----------------------------------------------------------

// The hash a fragment polls itself with is that same fragment's hash, so
// an unchanged dashboard converges on 204. Hash the bytes INCLUDING the
// hash and every poll swaps, taking open diffs and scroll with it.
func TestPollConvergesOnTheHashTheFragmentRendered(t *testing.T) {
	devEnv(t)
	handler := New(newFakeAgent())

	first := doRequest(t, handler, http.MethodGet, "/fragment", nil)

	if first.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 - a poll with no hash has nothing to compare", first.Code)
	}
	assertHashSubstituted(t, first.Body.String())
	second := doRequest(t, handler, http.MethodGet, "/fragment?h="+fragmentHashOf(t, first.Body.String()), nil)

	if second.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 - nothing changed between the two", second.Code)
	}
	if second.Body.Len() != 0 {
		t.Errorf("the 204 carries %d bytes of body, want none", second.Body.Len())
	}
}

// The same across both templates: index.html renders the fragment inline,
// so if the two disagreed about the hash, every page load would swap the
// whole dashboard a second later.
func TestFirstPollAfterAPageLoadDoesNotSwap(t *testing.T) {
	devEnv(t)
	handler := New(newFakeAgent())

	page := doRequest(t, handler, http.MethodGet, "/", nil).Body.String()

	assertHashSubstituted(t, page)
	rec := doRequest(t, handler, http.MethodGet, "/fragment?h="+fragmentHashOf(t, page), nil)

	if rec.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204 - the page and the fragment route disagree on the hash", rec.Code)
	}
}

// assertHashSubstituted catches the silent half of renderFragment: the
// single bytes.Replace is a no-op if its target stops being the first
// occurrence, and the page then polls with a literal placeholder forever.
func assertHashSubstituted(t *testing.T, body string) {
	t.Helper()
	if strings.Contains(body, fragmentHashPlaceholder) {
		t.Error("the rendered output still carries the hash placeholder; the substitution missed")
	}
}

func TestPollSwapsAgainAsSoonAsTheStatusMoves(t *testing.T) {
	devEnv(t)
	agent := newFakeAgent()
	handler := New(agent)

	stale := fragmentHashOf(t, doRequest(t, handler, http.MethodGet, "/fragment", nil).Body.String())
	agent.setState("error")

	rec := doRequest(t, handler, http.MethodGet, "/fragment?h="+stale, nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "check_config failed") {
		t.Error("the fragment does not carry the new state")
	}
	if fragmentHashOf(t, rec.Body.String()) == stale {
		t.Error("the fragment still polls with the hash it had before the status changed")
	}
}

// The interval is the only part of the poll that tracks the status: two
// seconds while an operation runs, five while only waiting for a tick.
func TestPollIntervalTightensWhileTheAgentIsBusy(t *testing.T) {
	devEnv(t)
	for name, tc := range map[string]struct {
		busy bool
		want string
	}{
		"idle": {busy: false, want: `hx-trigger="every 5s"`},
		"busy": {busy: true, want: `hx-trigger="every 2s"`},
	} {
		t.Run(name, func(t *testing.T) {
			agent := newFakeAgent()
			agent.busy = tc.busy
			handler := New(agent)

			body := doRequest(t, handler, http.MethodGet, "/", nil).Body.String()

			if !strings.Contains(body, tc.want) {
				t.Errorf("body does not poll with %s", tc.want)
			}
		})
	}
}

// The poll URL follows the package rule: relative, so it resolves against
// whatever ingress path Supervisor mounted the page under.
func TestPollURLIsRelative(t *testing.T) {
	devEnv(t)
	handler := New(newFakeAgent())

	body := doRequest(t, handler, http.MethodGet, "/", nil).Body.String()

	if !strings.Contains(body, `hx-get="fragment?h=`) {
		t.Error(`body does not poll a relative "fragment" URL`)
	}
	if strings.Contains(body, `hx-get="/fragment`) {
		t.Error("the poll URL went absolute - ingress serves this page under a prefix")
	}
}

// --- static assets ----------------------------------------------------

// Both come out of an embed.FS, whose zero ModTime means net/http sends
// no validator of its own - and the browser refetches 47 KB of htmx per
// page load unless this package supplies one.
func TestStaticAssetsAreServedWithAValidatorAndCachedForever(t *testing.T) {
	devEnv(t)
	handler := New(newFakeAgent())

	for _, name := range []string{"app.css", "htmx.min.js"} {
		t.Run(name, func(t *testing.T) {
			rec := doRequest(t, handler, http.MethodGet, "/static/"+name, nil)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			if rec.Body.Len() == 0 {
				t.Error("body is empty")
			}
			if got, want := rec.Header().Get("ETag"), `"`+assetVersion+`"`; got != want {
				t.Errorf("ETag = %q, want %q", got, want)
			}
			if got := rec.Header().Get("Cache-Control"); got != staticCacheControl {
				t.Errorf("Cache-Control = %q, want %q", got, staticCacheControl)
			}
		})
	}
}

func TestStaticAssetAnswers304WhenTheClientAlreadyHasIt(t *testing.T) {
	devEnv(t)
	handler := New(newFakeAgent())

	rec := doRequest(t, handler, http.MethodGet, "/static/htmx.min.js", map[string]string{
		"If-None-Match": `"` + assetVersion + `"`,
	})

	if rec.Code != http.StatusNotModified {
		t.Fatalf("status = %d, want 304", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("304 carries %d bytes of body, want none", rec.Body.Len())
	}
	if got := rec.Header().Get("Cache-Control"); got != staticCacheControl {
		t.Errorf("Cache-Control = %q, want it kept on the 304", got)
	}
}

// An asset whose content moved must come back in full, or the immutable
// cache strands every user on a stale stylesheet.
func TestStaticAssetIsResentWhenTheVersionMoved(t *testing.T) {
	devEnv(t)
	handler := New(newFakeAgent())

	rec := doRequest(t, handler, http.MethodGet, "/static/app.css", map[string]string{
		"If-None-Match": `"0000dead"`,
	})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if rec.Body.Len() == 0 {
		t.Error("body is empty")
	}
}

// Only real files get the immutable headers. The directory listing needs
// the guard: a 200 the stdlib never sanitizes, so it would be pinned for
// a year. net/http strips them on error paths, so the 404 holds anyway.
func TestOnlyRealFilesAreCachedForever(t *testing.T) {
	devEnv(t)
	handler := New(newFakeAgent())

	for name, tc := range map[string]struct {
		path string
		code int
	}{
		"directory listing": {path: "/static/", code: http.StatusOK},
		"missing file":      {path: "/static/does-not-exist.js", code: http.StatusNotFound},
	} {
		t.Run(name, func(t *testing.T) {
			rec := doRequest(t, handler, http.MethodGet, tc.path, nil)

			if rec.Code != tc.code {
				t.Fatalf("status = %d, want %d", rec.Code, tc.code)
			}
			if got := rec.Header().Get("Cache-Control"); got != "" {
				t.Errorf("Cache-Control = %q, want none - this response is not immutable", got)
			}
			if got := rec.Header().Get("ETag"); got != "" {
				t.Errorf("ETag = %q, want none", got)
			}
		})
	}
}

func TestAssetVersionIsAShortStableContentHash(t *testing.T) {
	if len(assetVersion) != 8 {
		t.Errorf("assetVersion = %q, want 8 hex digits", assetVersion)
	}
	if got := computeAssetVersion(); got != assetVersion {
		t.Errorf("recomputed asset version = %q, want the stable %q", got, assetVersion)
	}
}

// The immutable Cache-Control is only safe because the URL moves with the
// content, so every asset must be asked for by version.
func TestPageAsksForItsAssetsByVersion(t *testing.T) {
	devEnv(t)
	handler := New(newFakeAgent())

	body := doRequest(t, handler, http.MethodGet, "/", nil).Body.String()

	for _, want := range []string{
		`href="static/app.css?v=` + assetVersion + `"`,
		`src="static/htmx.min.js?v=` + assetVersion + `"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body does not contain %s", want)
		}
	}
	if strings.Contains(body, "<style>") {
		t.Error("body still carries an inline stylesheet")
	}
	if strings.Contains(body, "/static/") {
		t.Error("a static URL went absolute - ingress serves this page under a prefix")
	}
}

// And only while the HTML naming them is refetched: a cached dashboard
// would go on asking for the previous release's ?v= forever.
func TestRenderedHTMLIsAlwaysRevalidated(t *testing.T) {
	devEnv(t)
	handler := New(newFakeAgent())

	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/"},
		{http.MethodGet, "/fragment"},
		{http.MethodPost, "/reconcile"},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			rec := doRequest(t, handler, tc.method, tc.path, nil)

			if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
				t.Errorf("Cache-Control = %q, want no-cache", got)
			}
		})
	}
}

// --- markup shape -----------------------------------------------------

// toolbar returns the .toolbar element. It has no nested div, so the
// first closing tag after it is its own.
func toolbar(t *testing.T, body string) string {
	t.Helper()
	open := strings.Index(body, `<div class="toolbar"`)
	if open < 0 {
		t.Fatal("body does not contain the toolbar")
	}
	closed := strings.Index(body[open:], "</div>")
	if closed < 0 {
		t.Fatal("could not delimit the toolbar element")
	}
	return body[open : open+closed]
}

// Every button swaps the same thing, so target and swap style are stated
// once on #app and inherited; one outside it loses its target. Seven,
// since Pause and Resume share a slot.
func TestSwapAttributesAreStatedOnceOnTheSwapTarget(t *testing.T) {
	devEnv(t)
	agent := newFakeAgent()
	agent.commitBackEnabled = true
	agent.importEnabled = true
	agent.lastStashDir = "/data/backup/x"
	handler := New(agent)

	body := doRequest(t, handler, http.MethodGet, "/", nil).Body.String()

	for _, want := range []string{`hx-target="#app"`, `hx-swap="outerHTML"`, `hx-sync="this:replace"`} {
		if got := strings.Count(body, want); got != 1 {
			t.Errorf("%s appears %d times, want 1 - it is inherited from #app, not repeated", want, got)
		}
		if !strings.Contains(mainElement(t, body), want) {
			t.Errorf("%s is not on the #app element every request under it inherits from", want)
		}
	}
	if got := strings.Count(body, "hx-post="); got != 7 {
		t.Fatalf("hx-post appears %d times, want all 7 buttons rendered", got)
	}
	if got := strings.Count(toolbar(t, body), "hx-post="); got != 7 {
		t.Errorf("the toolbar holds %d of the 7 buttons; one outside it inherits no target", got)
	}
}

// mainElement returns the opening <main id="app"> tag alone, so an
// inherited-attribute assertion cannot be satisfied further down.
func mainElement(t *testing.T, body string) string {
	t.Helper()
	open := strings.Index(body, `<main id="app"`)
	if open < 0 {
		t.Fatal(`body does not contain <main id="app"`)
	}
	closed := strings.Index(body[open:], ">")
	if closed < 0 {
		t.Fatal("could not delimit the #app element")
	}
	return body[open : open+closed]
}

// hx-disabled-elt must say "this" ON the button: htmx resolves it to the
// closest element carrying it, so hoisting it disables the toolbar. The
// spinner needs no attribute - the firing element is htmx's default.
func TestEveryButtonDisablesItselfAndShowsASpinner(t *testing.T) {
	devEnv(t)
	agent := newFakeAgent()
	agent.commitBackEnabled = true
	agent.importEnabled = true
	agent.lastStashDir = "/data/backup/x"
	handler := New(agent)

	body := doRequest(t, handler, http.MethodGet, "/", nil).Body.String()

	for _, want := range []string{`hx-disabled-elt="this"`, `<span class="spinner" aria-hidden="true"></span>`} {
		if got := strings.Count(body, want); got != 7 {
			t.Errorf("%s appears %d times, want one per button (7)", want, got)
		}
	}
	if got := strings.Count(toolbar(t, body), `hx-disabled-elt="this"`); got != 7 {
		t.Errorf("the toolbar holds %d of the 7 buttons that disable themselves", got)
	}
	if strings.Contains(body, "hx-indicator") {
		t.Error("hx-indicator is back; it only restates htmx's default indicator")
	}
}

// Both diff sites render through one partial, so a file change and a
// registry op cannot drift apart in markup.
func TestFileAndRegistryDiffsRenderThroughTheSamePartial(t *testing.T) {
	devEnv(t)
	agent := newFakeAgent()
	agent.pendingRegistry = []recon.PendingRegOp{
		{RType: "area", Key: "living_room", Kind: "update", DiffText: "-icon: 'mdi:old'\n+icon: 'mdi:sofa'\n"},
	}
	handler := New(agent)

	body := doRequest(t, handler, http.MethodGet, "/", nil).Body.String()

	if got := strings.Count(body, `<div class="diff-wrap" tabindex="0"><pre class="diff">`); got != 2 {
		t.Errorf("diff-wrap blocks = %d, want one for the file change and one for the registry op", got)
	}
	// html/template escapes a leading "+" to &#43;, which is the escaper's
	// behavior and not this template's.
	if !strings.Contains(body, `<span class="diff-add">&#43;icon: &#39;mdi:sofa&#39;</span>`) {
		t.Error("the registry diff does not colour its added line")
	}
	if !strings.Contains(body, `<span class="diff-add">&#43;alias: Demo</span>`) {
		t.Error("the file diff does not colour its added line")
	}
}

// All four callouts render through one partial - only the variant and
// the optional hint differ.
func TestCalloutsShareOneShape(t *testing.T) {
	devEnv(t)
	agent := newFakeAgent()
	agent.state = "error"
	agent.importEnabled = true
	agent.lastImportError = "refusing to import: total size 412.7 MB exceeds the 100.0 MB limit"
	agent.warnings = "Integration 'templete' not found."
	agent.lastBackupError = "supervisor request failed after 15m0s"
	handler := New(agent)

	body := doRequest(t, handler, http.MethodGet, "/", nil).Body.String()

	for _, heading := range []string{
		"Last import error", "Last error", "Configuration warnings", "Pre-apply backup did not run",
	} {
		if !strings.Contains(body, "<h2>"+heading+"</h2>") {
			t.Errorf("body does not render the %q callout", heading)
		}
	}
	if got := strings.Count(body, `<section class="card card-error">`); got != 2 {
		t.Errorf("card-error callouts = %d, want 2 (last import error, last error)", got)
	}
	if got := strings.Count(body, `<section class="card card-warning">`); got != 2 {
		t.Errorf("card-warning callouts = %d, want 2 (warnings, failed backup)", got)
	}
	// Only the failed-backup card fills the partial's optional hint slot.
	if got := strings.Count(body, "Roll Back still works"); got != 1 {
		t.Errorf("the backup hint appears %d times, want exactly 1", got)
	}
}

// --- accessibility ----------------------------------------------------

// The dashboard is the main landmark and keeps id="app": every POST's
// fragment must be the same element, or the second swap has no target.
func TestDashboardIsAMainLandmarkHtmxCanStillTarget(t *testing.T) {
	devEnv(t)
	handler := New(newFakeAgent())

	page := doRequest(t, handler, http.MethodGet, "/", nil).Body.String()
	fragment := doRequest(t, handler, http.MethodPost, "/reconcile", nil).Body.String()

	for name, body := range map[string]string{"page": page, "fragment": fragment} {
		if !strings.Contains(body, `<main id="app"`) {
			t.Errorf("%s does not wrap the dashboard in <main id=\"app\">", name)
		}
		if strings.Contains(body, `<div id="app"`) {
			t.Errorf("%s still uses a plain div for the swap target", name)
		}
	}
}

// The live region must live OUTSIDE #app: one inside is replaced whole on
// every swap rather than having its text changed, and assistive tech
// announces only the latter.
func TestStatusIsAnnouncedFromALiveRegionOutsideTheSwappedFragment(t *testing.T) {
	devEnv(t)
	handler := New(newFakeAgent())

	page := doRequest(t, handler, http.MethodGet, "/", nil).Body.String()

	region := `<div id="sr-status" role="status" aria-live="polite" class="sr-only"></div>`
	if !strings.Contains(page, region) {
		t.Fatal("the page carries no permanent live region")
	}
	if strings.Index(page, region) > strings.Index(page, `<main id="app"`) {
		t.Error("the live region is inside the swapped fragment, where it can never announce")
	}
	if !strings.Contains(page, `<span class="pill pill-drift_pending">`) {
		t.Error("the status pill is not rendered")
	}
	fragment := doRequest(t, handler, http.MethodGet, "/fragment", nil).Body.String()
	if strings.Contains(fragment, "aria-live") || strings.Contains(fragment, `role="status"`) {
		t.Error("the fragment still declares a live region of its own, which would never fire")
	}
}

// Same constraint for the error banner, different reason: a swap that
// replaced it would take down the message saying the last swap failed.
func TestTheErrorBannerLivesOutsideTheSwappedFragment(t *testing.T) {
	devEnv(t)
	handler := New(newFakeAgent())

	page := doRequest(t, handler, http.MethodGet, "/", nil).Body.String()

	banner := strings.Index(page, `<div id="req-error"`)
	if banner < 0 {
		t.Fatal("the page carries no failed-request banner")
	}
	if banner > strings.Index(page, `<main id="app"`) {
		t.Error("the banner is inside the swapped fragment, which would take it down on the next swap")
	}
	// The span ships empty: the three failure modes need different advice,
	// so a default in the markup could never appear.
	if !strings.Contains(page, `<span id="req-error-text"></span>`) {
		t.Error("the banner ships with text in it, which no path can ever show")
	}
	for _, event := range []string{"htmx:responseError", "htmx:sendError", "htmx:timeout"} {
		if !strings.Contains(page, event) {
			t.Errorf("nothing on the page listens for %s", event)
		}
	}
	for _, message := range []string{
		"Cannot reach the add-on", // htmx:sendError
		"Request failed (HTTP ",   // htmx:responseError, with the status
		"Request timed out",       // htmx:timeout
	} {
		if !strings.Contains(page, message) {
			t.Errorf("the banner cannot say %q - no path writes it", message)
		}
	}
	// htmx ships timeout:0, so without this the XHR never times out and
	// the listener above is unreachable.
	if !strings.Contains(page, "htmx.config.timeout =") {
		t.Error("nothing sets a request timeout, so htmx:timeout can never fire")
	}
	if strings.Contains(doRequest(t, handler, http.MethodGet, "/fragment", nil).Body.String(), "req-error") {
		t.Error("the fragment carries a banner of its own")
	}
}

// Every region with its own scrollbar needs tabindex="0", or what
// scrolled out of view is unreachable by keyboard.
func TestScrollableRegionsAreKeyboardReachable(t *testing.T) {
	devEnv(t)
	agent := newFakeAgent()
	agent.state = "error"
	agent.warnings = "Integration 'templete' not found."
	handler := New(agent)

	body := doRequest(t, handler, http.MethodGet, "/", nil).Body.String()

	for _, want := range []string{
		`<div class="diff-wrap" tabindex="0">`,
		`<pre class="error-text" tabindex="0">`,
		`<pre class="warning-text" tabindex="0">`,
		`<ul class="activity" tabindex="0">`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body does not contain %s", want)
		}
	}
}

func TestStatusJSONIncludesImportFields(t *testing.T) {
	devEnv(t)
	agent := newFakeAgent()
	agent.importEnabled = true
	handler := New(agent)

	body := doRequest(t, handler, http.MethodGet, "/status.json", nil).Body.String()

	for _, key := range []string{`"import_enabled"`, `"last_import_utc"`, `"last_import_error"`, `"import_preview"`} {
		if !strings.Contains(body, key) {
			t.Errorf("status.json missing %s", key)
		}
	}
}

// --- run history card ---------------------------------------------------

// historyAgent is a fakeAgent that differs only in the run history it
// reports.
func historyAgent(records ...history.Record) *fakeAgent {
	agent := newFakeAgent()
	agent.runHistory = records
	return agent
}

func okApplyRun() history.Record {
	return history.Record{
		Kind: history.KindApply, StartedUTC: "2026-08-02T07:15:09+00:00", DurationMS: 4200,
		SHA: "a3f9c2140e5b6d7889aabbccddeeff0011223344", Outcome: history.OutcomeOK,
		Files: 6, RegOps: 3, StashDir: "/data/backup/20260802T071509Z",
	}
}

func TestRunHistoryCardShowsTheEmptyStateWithNoRuns(t *testing.T) {
	devEnv(t)
	handler := New(historyAgent())

	body := doRequest(t, handler, http.MethodGet, "/", nil).Body.String()

	if !strings.Contains(body, "Run history") {
		t.Error("body does not render the Run history card")
	}
	if !strings.Contains(body, "No runs recorded yet") {
		t.Error("body does not render the empty state")
	}
	if strings.Contains(body, `class="runs"`) {
		t.Error("body renders an empty run list, want the empty state instead")
	}
}

func TestRunHistoryRowRendersEveryColumn(t *testing.T) {
	devEnv(t)
	handler := New(historyAgent(okApplyRun()))

	body := doRequest(t, handler, http.MethodGet, "/", nil).Body.String()

	for _, want := range []string{
		`class="runs"`,
		`class="badge badge-apply"`,
		"a3f9c21",     // the SHA, abbreviated on read
		`outcome-ok"`, // the outcome class the colour hangs off
		"6 file(s), 3 reg op(s)",
		"4.2s",
		"Aug 2, 07:15", // absolute, via humanTime
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body does not render %q", want)
		}
	}
}

// Outcomes go through humanState, so "rolled_back" reaches the page as
// words rather than its wire value.
func TestRunHistoryRendersOutcomesAsWords(t *testing.T) {
	devEnv(t)
	rolledBack := okApplyRun()
	rolledBack.Outcome = history.OutcomeRolledBack
	rolledBack.Files = 0
	rolledBack.RegOps = 0
	rolledBack.Error = "check_config failed"
	handler := New(historyAgent(rolledBack))

	body := doRequest(t, handler, http.MethodGet, "/", nil).Body.String()

	if !strings.Contains(body, "rolled back") {
		t.Error("body does not render the outcome as words")
	}
	if !strings.Contains(body, "outcome-rolled_back") {
		t.Error("body does not carry the outcome class the colour hangs off")
	}
}

// A rollback stores no SHA on purpose; the column must not collapse.
func TestRunHistoryRollbackRowRendersADashForTheMissingSHA(t *testing.T) {
	devEnv(t)
	handler := New(historyAgent(history.Record{
		Kind: history.KindRollback, StartedUTC: "2026-08-02T08:54:40+00:00", DurationMS: 2100,
		Outcome: history.OutcomeOK, Files: 3,
	}))

	body := doRequest(t, handler, http.MethodGet, "/", nil).Body.String()

	if !strings.Contains(body, `<span class="sha" title="">-</span>`) {
		t.Error("body does not render a dash for a run with no SHA")
	}
}

// Neither a rollback nor an import has a registry count, and "0 reg
// op(s)" reads as a layer that ran and did nothing.
func TestRunHistoryOmitsTheHalfOfTheCountsThatIsZero(t *testing.T) {
	devEnv(t)
	filesOnly := okApplyRun()
	filesOnly.Kind = history.KindImport
	filesOnly.RegOps = 0
	handler := New(historyAgent(filesOnly))

	body := doRequest(t, handler, http.MethodGet, "/", nil).Body.String()

	if !strings.Contains(body, "6 file(s)") {
		t.Error("body does not render the file count")
	}
	if strings.Contains(body, "reg op(s)") {
		t.Error("body renders a zero registry count, want that half omitted")
	}
}

func TestRunHistoryRendersADashWhenThereIsNothingToCount(t *testing.T) {
	devEnv(t)
	inSync := okApplyRun()
	inSync.Kind = history.KindReconcile
	inSync.Outcome = history.OutcomeInSync
	inSync.Files = 0
	inSync.RegOps = 0
	handler := New(historyAgent(inSync))

	body := doRequest(t, handler, http.MethodGet, "/", nil).Body.String()

	if !strings.Contains(body, `<span class="counts">-</span>`) {
		t.Error("body does not render a dash for a run with no counts")
	}
}

// A partial apply shows its counts AND its error, so the error takes a
// full-width row rather than a column.
func TestRunHistoryErrorRendersOnItsOwnRowBesideTheCounts(t *testing.T) {
	devEnv(t)
	partial := okApplyRun()
	partial.Outcome = history.OutcomePartial
	partial.Error = "integrations: create pushward failed"
	handler := New(historyAgent(partial))

	body := doRequest(t, handler, http.MethodGet, "/", nil).Body.String()

	if !strings.Contains(body, `class="run-error"`) {
		t.Error("body does not render the error on its own row")
	}
	if !strings.Contains(body, "integrations: create pushward failed") {
		t.Error("body does not render the error text")
	}
	if !strings.Contains(body, "6 file(s), 3 reg op(s)") {
		t.Error("body drops the counts on a partial row, want them beside the error")
	}
}

// These must never become relative times: a "2 min ago" would move the
// bytes every poll. No wait between the two renders - a relative time is
// composed at render, so the hashes diverge without elapsed time.
func TestRunHistoryRendersStableBytesAcrossPolls(t *testing.T) {
	devEnv(t)
	handler := New(historyAgent(okApplyRun()))

	first := doRequest(t, handler, http.MethodGet, "/", nil).Body.String()
	second := doRequest(t, handler, http.MethodGet, "/", nil).Body.String()

	if fragmentHashOf(t, first) != fragmentHashOf(t, second) {
		t.Error("the fragment hash moved between two renders of the same history; " +
			"run-history timestamps must be absolute, not relative")
	}
}

// A fragment poll carrying the current hash must still answer 204 with a
// history on the page.
func TestFragmentStillAnswers204WithAHistoryRendered(t *testing.T) {
	devEnv(t)
	handler := New(historyAgent(okApplyRun()))

	page := doRequest(t, handler, http.MethodGet, "/", nil).Body.String()
	hash := fragmentHashOf(t, page)

	rec := doRequest(t, handler, http.MethodGet, "/fragment?h="+hash, nil)

	if rec.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204 for an unchanged fragment", rec.Code)
	}
}

// A long-lived tab polls fresh markup but never refetches its stylesheet,
// so after an update it renders new markup with old CSS. The shell script
// reloads when data-assets moves; both sides must carry that value.
func TestPageAndFragmentCarryTheAssetHashForStalenessDetection(t *testing.T) {
	devEnv(t)
	handler := New(newFakeAgent())
	marker := `data-assets="` + assetVersion + `"`

	page := doRequest(t, handler, http.MethodGet, "/", nil).Body.String()
	if !strings.Contains(page, marker) {
		t.Errorf("the page does not carry %s", marker)
	}
	if !strings.Contains(page, "swappedAssets !== assetsAtLoad") {
		t.Error("the shell script lost the stale-assets reload comparison")
	}

	frag := doRequest(t, handler, http.MethodGet, "/fragment?h=stale", nil).Body.String()
	if !strings.Contains(frag, marker) {
		t.Errorf("the swapped fragment does not carry %s - a stale tab could never notice an update", marker)
	}
}

// --- the standalone history page ----------------------------------------

// olderRun builds a distinguishable record for pages rendering more than
// one - numbered, since what matters is which rows arrived.
func olderRun(n int) history.Record {
	return history.Record{
		Kind: history.KindReconcile, StartedUTC: "2026-08-02T06:00:00+00:00", DurationMS: int64(n) * 1000,
		SHA: "91be004c1a2b3d4e5f60718293a4b5c6d7e8f900", Outcome: history.OutcomeInSync,
	}
}

// Everything the agent holds, not the card's cut. Both lists come off the
// same fake, so a page rendering Status.History would show 2 and fail.
func TestHistoryPageRendersEveryRecordTheAgentHolds(t *testing.T) {
	devEnv(t)
	agent := newFakeAgent()
	agent.runHistory = []history.Record{okApplyRun(), olderRun(1)}
	agent.historyAll = []history.Record{okApplyRun(), olderRun(1), olderRun(2), olderRun(3)}
	handler := New(agent)

	rec := doRequest(t, handler, http.MethodGet, "/history", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if got := strings.Count(body, "<li>"); got != 4 {
		t.Errorf("page renders %d rows, want all 4 the agent holds", got)
	}
	for _, want := range []string{
		"<h1>Run history</h1>",
		`<span class="count">4</span>`,
		`class="badge badge-apply"`,
		"6 file(s), 3 reg op(s)",
		"Aug 2, 07:15", // absolute, via humanTime, like the card's rows
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page does not render %q", want)
		}
	}
}

// Reachable only through ingress, so the link back must resolve against
// whatever prefix Supervisor mounted this add-on under.
func TestHistoryPageLinksBackToTheDashboardRelatively(t *testing.T) {
	devEnv(t)
	handler := New(historyAgent(okApplyRun()))

	body := doRequest(t, handler, http.MethodGet, "/history", nil).Body.String()

	if !strings.Contains(body, `<a href="./">&larr; Dashboard</a>`) {
		t.Error("page has no relative back link to the dashboard")
	}
	if strings.Contains(body, `href="/"`) {
		t.Error("page links to an absolute path, which ingress would resolve outside the add-on")
	}
}

// A snapshot on purpose: no htmx, no polling, no hash. 200 rows re-hashed
// every few seconds is what the dashboard's cap exists to avoid.
func TestHistoryPageDoesNotPollAndLoadsNoScriptItDoesNotNeed(t *testing.T) {
	devEnv(t)
	handler := New(historyAgent(okApplyRun()))

	body := doRequest(t, handler, http.MethodGet, "/history", nil).Body.String()

	for _, unwanted := range []string{"htmx", "hx-get", "hx-post", "fragment?h="} {
		if strings.Contains(body, unwanted) {
			t.Errorf("page carries %q, want a static snapshot", unwanted)
		}
	}
	if !strings.Contains(body, "static/app.css?v="+assetVersion) {
		t.Error("page does not link the versioned stylesheet the dashboard uses")
	}
}

// Same headers as the dashboard, same reason: this HTML names the ?v= of
// the stylesheet it links.
func TestHistoryPageIsAlwaysRevalidated(t *testing.T) {
	devEnv(t)
	handler := New(historyAgent(okApplyRun()))

	rec := doRequest(t, handler, http.MethodGet, "/history", nil)

	if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
		t.Errorf("Cache-Control = %q, want no-cache", got)
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", got)
	}
}

func TestHistoryPageShowsTheEmptyStateWithNoRuns(t *testing.T) {
	devEnv(t)
	handler := New(newFakeAgent())

	body := doRequest(t, handler, http.MethodGet, "/history", nil).Body.String()

	if !strings.Contains(body, "No runs recorded yet") {
		t.Error("page does not render the empty state")
	}
	if strings.Contains(body, `class="runs`) {
		t.Error("page renders an empty run list, want the empty state instead")
	}
	// A "0" badge over a sentence that already says there is nothing.
	if strings.Contains(body, `class="count"`) {
		t.Error("page renders a count badge beside its empty state")
	}
}

// The page says what it is once: a second "Run history" over the list is
// the same words twice for anyone reading the headings out.
func TestHistoryPageHasOneHeading(t *testing.T) {
	devEnv(t)
	handler := New(historyAgent(okApplyRun()))

	body := doRequest(t, handler, http.MethodGet, "/history", nil).Body.String()

	if got := strings.Count(body, "Run history"); got != 2 {
		// The <title> and the <h1>, and nothing else.
		t.Errorf("the page says \"Run history\" %d times, want 2 (the tab title and the heading)", got)
	}
	if strings.Contains(body, "<h2>") {
		t.Error("the card carries a heading of its own, duplicating the page's")
	}
}

// goldenRunRow is okApplyRun() as the runrow partial renders it, byte for
// byte. A page-to-page comparison could not fail - both call the same
// partial - so this pins the whitespace a re-indent would eat.
//
// Rebuild from a rendered page if the row changes; never hand-edit it.
const goldenRunRow = "<li>\n" +
	"      <span class=\"ts\" title=\"2026-08-02T07:15:09&#43;00:00\" data-utc=\"2026-08-02T07:15:09&#43;00:00\">Aug 2, 07:15</span>\n" +
	"      <span class=\"badge badge-apply\">apply</span>\n" +
	"      <span class=\"sha\" title=\"a3f9c2140e5b6d7889aabbccddeeff0011223344\">a3f9c21</span>\n" +
	"      <span class=\"outcome outcome-ok\">ok</span>\n" +
	"      <span class=\"counts\">6 file(s), 3 reg op(s)</span>\n" +
	"      <span class=\"dur\">4.2s</span>\n" +
	"      \n" +
	"      \n" +
	"    </li>"

// oneRunRow is the first run's rendered row, for the golden above.
func oneRunRow(t *testing.T, body string) string {
	t.Helper()
	start := strings.Index(body, "<li>")
	if start < 0 {
		t.Fatal("body renders no run row at all")
	}
	end := strings.Index(body[start:], "</li>")
	if end < 0 {
		t.Fatal("body renders an unterminated run row")
	}
	return body[start : start+end+len("</li>")]
}

func TestARunRowRendersItsGoldenMarkupOnTheDashboard(t *testing.T) {
	devEnv(t)
	handler := New(historyAgent(okApplyRun()))

	body := doRequest(t, handler, http.MethodGet, "/", nil).Body.String()

	if got := oneRunRow(t, body); got != goldenRunRow {
		t.Errorf("run row drifted from its golden markup:\ngot  %q\nwant %q", got, goldenRunRow)
	}
}

// The same golden on the other page: one partial, two callers.
func TestARunRowRendersItsGoldenMarkupOnTheHistoryPage(t *testing.T) {
	devEnv(t)
	handler := New(historyAgent(okApplyRun()))

	body := doRequest(t, handler, http.MethodGet, "/history", nil).Body.String()

	if got := oneRunRow(t, body); got != goldenRunRow {
		t.Errorf("run row drifted from its golden markup:\ngot  %q\nwant %q", got, goldenRunRow)
	}
}

// The link is the page's whole discoverability, so it appears only when
// there are rows the reader cannot already see.
func TestRunHistoryHeadingLinksOnWhenMoreRunsExist(t *testing.T) {
	devEnv(t)
	agent := historyAgent(okApplyRun(), olderRun(1))
	agent.historyTotal = 200
	handler := New(agent)

	body := doRequest(t, handler, http.MethodGet, "/", nil).Body.String()

	if !strings.Contains(body, `<a class="view-all" href="history">all 200 &rarr;</a>`) {
		t.Error("the card heading does not link to the full history")
	}
}

func TestRunHistoryHeadingHasNoLinkWhenEverythingIsShown(t *testing.T) {
	devEnv(t)
	handler := New(historyAgent(okApplyRun(), olderRun(1)))

	body := doRequest(t, handler, http.MethodGet, "/", nil).Body.String()

	if strings.Contains(body, "view-all") {
		t.Error("the card links to a longer history than it is holding")
	}
}

// The link moves only when a run lands, which already rewrites the rows
// under it, so it must not cost the poll its 204.
func TestTheViewAllLinkKeepsTheFragmentByteStable(t *testing.T) {
	devEnv(t)
	agent := historyAgent(okApplyRun())
	agent.historyTotal = 200
	handler := New(agent)

	page := doRequest(t, handler, http.MethodGet, "/", nil).Body.String()

	rec := doRequest(t, handler, http.MethodGet, "/fragment?h="+fragmentHashOf(t, page), nil)
	if rec.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204 for an unchanged fragment", rec.Code)
	}
}

// --- local times and the next-check countdown --------------------------

// Every timestamp carries its raw UTC for index.html's localizeTimes. The
// TEXT stays absolute UTC, which the byte-stability guard depends on.
func TestTimestampsCarryTheirRawUTCForClientSideLocalisation(t *testing.T) {
	devEnv(t)
	agent := historyAgent(okApplyRun())
	agent.autoUpdateEnabled = true
	agent.addonCheckIntervalSeconds = 21600
	agent.addonUpdates = []recon.AddonUpdateStatus{{
		Slug:           "core_samba",
		Name:           "Samba share",
		Version:        "12.3.2",
		LastResult:     "up to date",
		LastCheckedUTC: "2026-08-03T14:12:07+00:00",
		LastUpdatedUTC: "2026-07-11T05:03:44+00:00",
	}}
	agent.importEnabled = true
	agent.lastImportUTC = "2026-07-30T09:01:02+00:00"
	handler := New(agent)

	body := doRequest(t, handler, http.MethodGet, "/", nil).Body.String()

	// Every value is distinct, so each literal is satisfied only by the
	// site named beside it. The "+" arrives as &#43;, escaped in every
	// attribute and decoded again by getAttribute.
	for _, want := range []string{
		`data-utc="2026-08-01T00:00:00&#43;00:00"`, // the header's last-applied
		`data-utc="2026-08-04T11:22:33&#43;00:00"`, // the activity row
		`data-utc="2026-07-30T09:01:02&#43;00:00"`, // the import meta line
		`data-utc="2026-08-03T14:12:07&#43;00:00"`, // an add-on row's check
		`data-utc="2026-07-11T05:03:44&#43;00:00"`, // an add-on row's update
		`data-utc="2026-08-02T07:15:09&#43;00:00"`, // the run-history row
		"Aug 2, 07:15", // still the server's absolute text
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body does not render %q", want)
		}
	}

	// data-utc-zone is what asks localizeTimes for the zone-naming format.
	// It belongs on the timestamps standing alone in a sentence and not on
	// the row ones, which sit in a fixed-width column.
	if !strings.Contains(body, `data-utc="2026-08-01T00:00:00&#43;00:00" data-utc-zone`) {
		t.Error("the header's last-applied does not ask for the reader's zone to be named")
	}
	if !strings.Contains(body, `data-utc="2026-07-30T09:01:02&#43;00:00" data-utc-zone`) {
		t.Error("the import meta line does not ask for the reader's zone to be named")
	}
	if strings.Contains(body, `data-utc="2026-08-02T07:15:09&#43;00:00" data-utc-zone`) {
		t.Error("a run-history row asks for the zone to be named, want the compact format there")
	}

	// data-utc-rel marks the one span rewritten into a relative AGE. A
	// marker and not a value, so it renders constant bytes and the
	// fragment stays hashable.
	if !strings.Contains(body, `data-utc="2026-08-03T14:12:07&#43;00:00" data-utc-rel`) {
		t.Error("the add-on row's check timestamp does not ask to be rendered as an age")
	}
	// Only the check carries it: an install cannot go stale, it just
	// reports how long that version has been in place.
	if strings.Contains(body, `data-utc="2026-07-11T05:03:44&#43;00:00" data-utc-rel`) {
		t.Error("the add-on row's install timestamp asks to be aged and marked stale")
	}
	// The threshold in seconds, read off the nearest ancestor. Fixed for
	// the process lifetime, so it is a constant in the fragment.
	if !strings.Contains(body, `data-stale-after="21600"`) {
		t.Error("the add-on card does not carry the interval a stale check is judged against")
	}
}

// The fold must not cost the 204: two renders must be byte-identical with
// rows on BOTH sides, which addonUpdatesFunc's source-order walk keeps.
// A fold built from a map would answer 200 here.
func TestFragmentStillAnswers204WithFoldedAddonRows(t *testing.T) {
	devEnv(t)
	agent := newFakeAgent()
	agent.autoUpdateEnabled = true
	agent.addonCheckIntervalSeconds = 21600
	agent.addonUpdates = []recon.AddonUpdateStatus{
		{
			Slug: "a0d7b954_esphome", Name: "ESPHome Device Builder",
			Version: "2026.7.3", LatestVersion: "2026.8.0", UpdateAvailable: true,
			LastResult: "updated to 2026.8.0", LastCheckedUTC: "2026-08-03T14:12:07+00:00",
		},
		{Slug: "core_typo", LastResult: recon.AddonUpdateNotInstalled, LastCheckedUTC: "2026-08-03T14:12:07+00:00"},
		{
			Slug: "local_ha_gitops_agent", Name: "local_ha_gitops_agent",
			LastResult: recon.AddonUpdateRefusedSelf, LastCheckedUTC: "2026-08-03T14:12:07+00:00",
		},
	}
	handler := New(agent)

	page := doRequest(t, handler, http.MethodGet, "/", nil).Body.String()

	if !strings.Contains(page, "not updatable by this agent") {
		t.Fatal("the fixture rendered no fold, so this proves nothing about one")
	}
	rec := doRequest(t, handler, http.MethodGet, "/fragment?h="+fragmentHashOf(t, page), nil)
	if rec.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204 for an unchanged fragment holding a fold", rec.Code)
	}
}

// The "Z" shape, not an offset: recon.utcISO writes a zero offset that
// way, so it is the only shape the countdown receives in production.
func TestNextCheckCountdownRendersWhenTheAgentKnowsIt(t *testing.T) {
	devEnv(t)
	agent := newFakeAgent()
	agent.nextCheckUTC = "2026-08-01T00:05:00Z"
	handler := New(agent)

	body := doRequest(t, handler, http.MethodGet, "/", nil).Body.String()

	for _, want := range []string{
		`class="next-check"`,
		`data-next-utc="2026-08-01T00:05:00Z"`,
		"next check by Aug 1, 00:05 UTC", // the static, hash-safe fallback
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body does not render %q", want)
		}
	}
}

// "" means the first cycle has not finished, so there is nothing honest
// to count down to and the span stays off the page.
func TestNextCheckCountdownAbsentWhenUnknown(t *testing.T) {
	devEnv(t)
	handler := New(newFakeAgent()) // nextCheckUTC left unset

	body := doRequest(t, handler, http.MethodGet, "/", nil).Body.String()

	if strings.Contains(body, "next-check") {
		t.Error("body renders the countdown span with no next check known")
	}
	if strings.Contains(body, "next check by") {
		t.Error("body renders a next-check time with none known")
	}
}

// Rendered from the agent's fixed timestamp, not "now", so the bytes move
// once per tick and never per poll. No sleep: nothing in the render path
// reads a clock, so waiting would prove only that waiting takes time.
func TestNextCheckCountdownKeepsTheFragmentByteStable(t *testing.T) {
	devEnv(t)
	agent := newFakeAgent()
	agent.nextCheckUTC = "2026-08-01T00:05:00Z"
	handler := New(agent)

	page := doRequest(t, handler, http.MethodGet, "/", nil).Body.String()
	rec := doRequest(t, handler, http.MethodGet, "/fragment?h="+fragmentHashOf(t, page), nil)

	if rec.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204 - the countdown must not be rendered from the clock", rec.Code)
	}
}

func TestStatusJSONCarriesTheNextCheck(t *testing.T) {
	devEnv(t)
	agent := newFakeAgent()
	agent.nextCheckUTC = "2026-08-01T00:05:00Z"
	handler := New(agent)

	var got map[string]any
	body := doRequest(t, handler, http.MethodGet, "/status.json", nil).Body.Bytes()
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("status.json is not valid JSON: %v", err)
	}

	if got["next_check_utc"] != "2026-08-01T00:05:00Z" {
		t.Errorf("next_check_utc = %v, want the agent's value", got["next_check_utc"])
	}
}

func TestStatusJSONCarriesTheRunHistory(t *testing.T) {
	devEnv(t)
	handler := New(historyAgent(okApplyRun()))

	rec := doRequest(t, handler, http.MethodGet, "/status.json", nil)

	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode status.json: %v", err)
	}
	runs, ok := got["history"].([]any)
	if !ok {
		t.Fatalf("status.json has no history array: %v", got["history"])
	}
	if len(runs) != 1 {
		t.Fatalf("history has %d entries, want 1", len(runs))
	}
	row, _ := runs[0].(map[string]any)
	for _, key := range []string{"kind", "started_utc", "duration_ms", "sha", "outcome", "files", "reg_ops"} {
		if _, ok := row[key]; !ok {
			t.Errorf("history row is missing the %q key", key)
		}
	}
}

// --- blocked items ------------------------------------------------------

func blockedAgent(items ...recon.BlockedItem) *fakeAgent {
	agent := newFakeAgent()
	agent.blocked = items
	return agent
}

func blockedIntegration() recon.BlockedItem {
	return recon.BlockedItem{
		RType: "integration",
		Key:   "integration:workday_main",
		Name:  "workday_main",
		Error: "flow step 'user' rejected the submitted data (invalid_auth)",
	}
}

// A record whose item left the manifest is planned as nothing, so this
// card is the only place it is visible or can be cleared.
func TestRecordedFailuresCardRendersEachItemWithItsOwnRetryButton(t *testing.T) {
	devEnv(t)
	handler := New(blockedAgent(blockedIntegration(), recon.BlockedItem{
		RType: "subentry",
		Key:   "subentry:widget_kitchen",
		Name:  "widget_kitchen",
		Error: "reconfigure flow ended in an unexpected step",
	}, recon.BlockedItem{
		// The third layer keeping a failure memory. Same row as the other
		// two, which is why it needed nothing of its own here.
		RType: "hacs",
		Key:   "hacs:anker_solix",
		Name:  "anker_solix",
		Error: "could not download thomluther/ha-anker-solix: no release tagged 9.9.9",
	}))

	body := doRequest(t, handler, http.MethodGet, "/", nil).Body.String()

	for _, want := range []string{
		`Recorded failures <span class="count">3</span>`,
		"An item no longer declared keeps its record until you clear it here.",
		"workday_main",
		"invalid_auth",
		"widget_kitchen",
		"unexpected step",
		"anker_solix",
		"no release tagged 9.9.9",
		`<span class="badge badge-error">hacs</span>`,
		`hx-post="retry"`,
		`hx-vals="{&#34;key&#34;:&#34;integration:workday_main&#34;}"`,
		`hx-vals="{&#34;key&#34;:&#34;subentry:widget_kitchen&#34;}"`,
		`hx-vals="{&#34;key&#34;:&#34;hacs:anker_solix&#34;}"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body does not render %q", want)
		}
	}
}

// The copy must not claim only a manifest edit clears a record: half
// belong to items the manifest no longer mentions.
func TestRecordedFailuresCopyCoversItemsThatAreNoLongerDeclared(t *testing.T) {
	devEnv(t)
	handler := New(blockedAgent(blockedIntegration()))

	body := doRequest(t, handler, http.MethodGet, "/", nil).Body.String()

	if strings.Contains(body, "will not be retried until their declared data changes") {
		t.Error("body still claims every record needs a manifest edit")
	}
	if !strings.Contains(body, "or immediately when you press Retry") {
		t.Error("body does not say the button clears the record")
	}
}

// Every Retry button reads "Retry", so without the name a screen reader
// cannot tell which row it is on.
func TestRetryButtonsCarryTheItemNameForScreenReaders(t *testing.T) {
	devEnv(t)
	handler := New(blockedAgent(blockedIntegration()))

	body := doRequest(t, handler, http.MethodGet, "/", nil).Body.String()

	if !strings.Contains(body, `>Retry<span class="sr-only"> workday_main</span>`) {
		t.Error("the retry button carries no distinguishing accessible name")
	}
}

func TestRecordedFailuresCardHiddenWhenThereAreNone(t *testing.T) {
	devEnv(t)
	handler := New(newFakeAgent())

	body := doRequest(t, handler, http.MethodGet, "/", nil).Body.String()

	if strings.Contains(body, "Recorded failures") {
		t.Error("body renders the card with nothing recorded")
	}
	if strings.Contains(body, `hx-post="retry"`) {
		t.Error("body renders a retry button with nothing recorded")
	}
}

// htmx parses bad hx-vals as null, which posts no key and is refused
// silently, so the attribute must survive escaping for any key.
func TestRetryValsIsValidJSONForAnyKey(t *testing.T) {
	for _, key := range []string{
		"integration:workday_main",
		`integration:wo"rk`,
		`subentry:back\slash`,
	} {
		var got map[string]string
		if err := json.Unmarshal([]byte(retryVals(key)), &got); err != nil {
			t.Fatalf("retryVals(%q) = %q, which is not JSON: %v", key, retryVals(key), err)
		}
		if got["key"] != key {
			t.Errorf("retryVals(%q) decoded to %q", key, got["key"])
		}
	}
}

// Without the chained reconcile the row would vanish for a whole
// interval, which reads as the button having done nothing.
func TestRetryRouteClearsTheFailureAndThenChecksAgain(t *testing.T) {
	devEnv(t)
	agent := blockedAgent(blockedIntegration())
	handler := New(agent)

	rec := doForm(t, handler, "/retry", url.Values{"key": {"integration:workday_main"}})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if name := agent.awaitFinish(t); name != "reconcile" {
		t.Fatalf("finished operation = %q, want the chained reconcile", name)
	}
	calls := agent.dispatchedCalls()
	if len(calls.retry) != 1 || calls.retry[0] != "integration:workday_main" {
		t.Errorf("retry calls = %v, want the pressed item's key", calls.retry)
	}
	if calls.reconcile != 1 {
		t.Errorf("reconcile calls = %d, want 1", calls.reconcile)
	}
}

// A refused retry - item gone, or another operation holds the lock - must
// not spend a whole reconcile cycle on nothing.
func TestRetryRouteSkipsTheCheckWhenTheAgentRefuses(t *testing.T) {
	devEnv(t)
	agent := blockedAgent(blockedIntegration())
	agent.retryErr = errors.New("another operation is already running")
	handler := New(agent)

	rec := doForm(t, handler, "/retry", url.Values{"key": {"integration:workday_main"}})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 - the page still re-renders", rec.Code)
	}
	if name := agent.awaitFinish(t); name != "retry" {
		t.Fatalf("finished operation = %q, want the refused retry", name)
	}
	if got := agent.dispatchedCalls().reconcile; got != 0 {
		t.Errorf("reconcile calls = %d, want none after a refused retry", got)
	}
}

// --- standing health warnings -------------------------------------------

func TestHealthChipsRenderEveryRaisedFlag(t *testing.T) {
	devEnv(t)
	agent := newFakeAgent()
	agent.historyWriteFailing = true
	agent.versionRecordFailing = true
	agent.addonUpdateSelfSlugFailing = true
	agent.addonCheckFailing = []string{"a0d7b954_esphome", "core_samba"}
	handler := New(agent)

	body := doRequest(t, handler, http.MethodGet, "/", nil).Body.String()

	for _, want := range []string{
		`class="chips"`,
		"history writes failing",
		"version record failing",
		"update check cannot resolve own slug",
		"update check failing: a0d7b954_esphome",
		"update check failing: core_samba",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body does not render %q", want)
		}
	}
}

func TestHealthChipsAbsentWhenNothingIsFailing(t *testing.T) {
	devEnv(t)
	handler := New(newFakeAgent())

	body := doRequest(t, handler, http.MethodGet, "/", nil).Body.String()

	if strings.Contains(body, `class="chips"`) {
		t.Error("body renders the chip row on a healthy agent")
	}
}

// --- restart warning ----------------------------------------------------

// Restarting an add-on is the most disruptive thing an apply does, and
// dry_run alone picks which confirm wording shows - so both carry it.
func TestApplyConfirmNamesTheAddonsItWillRestart(t *testing.T) {
	for _, dryRun := range []bool{false, true} {
		devEnv(t)
		agent := newFakeAgent()
		agent.dryRun = dryRun
		agent.pendingRestartSlugs = []string{"core_configurator", "core_ssh"}
		handler := New(agent)

		body := doRequest(t, handler, http.MethodGet, "/", nil).Body.String()

		button := applyButton(t, body)
		if !strings.Contains(button, "This will restart add-on(s): core_configurator, core_ssh.") {
			t.Errorf("apply button (dry_run=%v) = %q, want it to name both add-ons", dryRun, button)
		}
	}
}

func TestApplyConfirmSaysNothingAboutRestartsWhenNoneArePlanned(t *testing.T) {
	devEnv(t)
	handler := New(newFakeAgent())

	body := doRequest(t, handler, http.MethodGet, "/", nil).Body.String()

	if button := applyButton(t, body); strings.Contains(button, "restart") {
		t.Errorf("apply button = %q, want no restart sentence with no add-on op pending", button)
	}
}

func TestStatusJSONCarriesTheHiddenState(t *testing.T) {
	devEnv(t)
	agent := blockedAgent(blockedIntegration())
	agent.historyWriteFailing = true
	agent.addonCheckFailing = []string{"core_samba"}
	agent.pendingRestartSlugs = []string{"core_configurator"}
	handler := New(agent)

	var got map[string]any
	body := doRequest(t, handler, http.MethodGet, "/status.json", nil).Body.Bytes()
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("status.json is not valid JSON: %v", err)
	}

	for _, key := range []string{
		"blocked", "pending_restart_slugs", "history_write_failing",
		"version_record_failing", "addon_check_failing", "addon_update_self_slug_failing",
	} {
		if _, ok := got[key]; !ok {
			t.Errorf("status.json missing key %q", key)
		}
	}
	items, ok := got["blocked"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("blocked = %v, want one item", got["blocked"])
	}
	row, _ := items[0].(map[string]any)
	for _, key := range []string{"key", "rtype", "name", "error"} {
		if _, ok := row[key]; !ok {
			t.Errorf("blocked row is missing the %q key", key)
		}
	}
	if _, ok := row["hash"]; ok {
		t.Error("blocked row carries the declared-data hash")
	}
}

// --- managed inventory ---------------------------------------------------

func managedAgent(inventory recon.ManagedInventory) *fakeAgent {
	agent := newFakeAgent()
	agent.managed = inventory
	return agent
}

// summaryOf extracts the <summary> block carrying label, so assertions
// are about its contents rather than the template's indentation.
func summaryOf(t *testing.T, body, label string) string {
	t.Helper()
	labelSpan := `<span class="path">` + label + `</span>`
	for rest := body; ; {
		opened := strings.Index(rest, "<summary>")
		if opened < 0 {
			break
		}
		rest = rest[opened+len("<summary>"):]
		closed := strings.Index(rest, "</summary>")
		if closed < 0 {
			break
		}
		if block := rest[:closed]; strings.Contains(block, labelSpan) {
			return block
		}
		rest = rest[closed:]
	}
	t.Fatalf("no group summary carries the label %q", label)
	return ""
}

func TestManagedCardRendersEachGroupWithItsOwnCount(t *testing.T) {
	devEnv(t)
	handler := New(managedAgent(recon.ManagedInventory{
		Files:        []string{"automations.yaml", "packages/lights.yaml"},
		Registry:     []string{"area:kitchen", "floor:ground", "input_boolean:guest_mode"},
		Entities:     []string{"light.living_room_ceiling"},
		Dashboards:   []string{"energy"},
		Addons:       []string{"core_ssh"},
		Integrations: []string{"workday_main"},
		Hacs:         []string{"adaptive_lighting", "anker_solix"},
	}))

	body := doRequest(t, handler, http.MethodGet, "/", nil).Body.String()

	for _, want := range []string{
		`Managed by this agent <span class="count">11</span>`,
		`<span class="path">HACS integrations</span>`,
		"<code>anker_solix</code>",
		"<code>automations.yaml</code>",
		"<code>packages/lights.yaml</code>",
		// The registry group keeps its prefixes: the only thing telling a
		// floor from an area from a helper domain.
		"<code>area:kitchen</code>",
		"<code>input_boolean:guest_mode</code>",
		"<code>light.living_room_ceiling</code>",
		"<code>core_ssh</code>",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body does not render %q", want)
		}
	}
	// Per-group counts, since the summary is all that is readable with
	// every group collapsed. The unit is for screen readers, which would
	// otherwise hear "files 12".
	for _, group := range []struct{ label, count string }{
		{"files", "2"},
		{"floors, areas, labels and helpers", "3"},
		{"add-on options", "1"},
		{"HACS integrations", "2"},
	} {
		summary := summaryOf(t, body, group.label)
		want := `<span class="count">` + group.count + `<span class="sr-only"> items</span></span>`
		if !strings.Contains(summary, want) {
			t.Errorf("the %q summary = %q, want it to carry %q", group.label, summary, want)
		}
	}
}

// An empty group is a layer the agent manages nothing through, and a
// heading over an empty list would say otherwise.
func TestManagedCardOmitsEmptyGroups(t *testing.T) {
	devEnv(t)
	handler := New(managedAgent(recon.ManagedInventory{Files: []string{"automations.yaml"}}))

	body := doRequest(t, handler, http.MethodGet, "/", nil).Body.String()

	if !strings.Contains(body, `<span class="path">files</span>`) {
		t.Fatal("body does not render the one group that has anything in it")
	}
	for _, absent := range []string{
		"floors, areas, labels and helpers", "add-on options", "subentries", "dashboards", "HACS integrations",
	} {
		if strings.Contains(body, `<span class="path">`+absent+`</span>`) {
			t.Errorf("body renders the empty %q group", absent)
		}
	}
}

// Unlike the recorded-failures card, this one stays on the page when
// empty: "owns nothing yet" is the answer a first run needs.
func TestManagedCardRendersItsEmptyState(t *testing.T) {
	devEnv(t)
	handler := New(newFakeAgent())

	body := doRequest(t, handler, http.MethodGet, "/", nil).Body.String()

	if !strings.Contains(body, `Managed by this agent <span class="count">0</span>`) {
		t.Error("body does not render the card on an agent that manages nothing")
	}
	if !strings.Contains(body, "Nothing written yet - files come under management the first time an apply writes them.") {
		t.Error("body does not render the empty state")
	}
	if strings.Contains(body, `class="inventory"`) {
		t.Error("body renders an inventory list with nothing under management")
	}
}

// The card reads as what the agent will delete or restore, and two things
// break that reading: imported files are synced but not owned, and a
// record outlives the option that made it. Both belong on the card.
func TestManagedCardSaysWhatOwnershipMeans(t *testing.T) {
	devEnv(t)
	handler := New(managedAgent(recon.ManagedInventory{Files: []string{"automations.yaml"}}))

	body := doRequest(t, handler, http.MethodGet, "/", nil).Body.String()

	for _, want := range []string{
		"Only what is listed here is ever deleted or restored",
		"not when they are imported",
		"stay listed and are not acted on until it is turned back on",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body does not say %q", want)
		}
	}
}

// A fully managed config runs to thousands of files, re-rendered and
// re-hashed every poll. Capped in the view; /status.json carries the lot.
func TestManagedGroupsAreCappedWithACountOfTheRest(t *testing.T) {
	devEnv(t)
	for _, tc := range []struct {
		name     string
		files    int
		wantMore string
	}{
		{name: "at the cap", files: inventoryMax, wantMore: ""},
		{name: "one over", files: inventoryMax + 1, wantMore: "... and 1 more"},
		{name: "well over", files: inventoryMax + 42, wantMore: "... and 42 more"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files := make([]string, tc.files)
			for i := range files {
				files[i] = fmt.Sprintf("packages/p%04d.yaml", i)
			}
			body := doRequest(t, New(managedAgent(recon.ManagedInventory{Files: files})), http.MethodGet, "/", nil).Body.String()

			rendered := strings.Count(body, "<code>packages/p")
			if rendered > inventoryMax {
				t.Errorf("rendered %d names, want at most %d", rendered, inventoryMax)
			}
			// The summary counts the group's real size, whatever the list
			// below it shows.
			summary := summaryOf(t, body, "files")
			if !strings.Contains(summary, `<span class="count">`+strconv.Itoa(tc.files)) {
				t.Errorf("the files summary = %q, want the full count %d", summary, tc.files)
			}
			if tc.wantMore == "" {
				if strings.Contains(body, "more, listed in full by status.json") {
					t.Error("body counts names as left out with nothing left out")
				}
				if rendered != tc.files {
					t.Errorf("rendered %d names, want all %d", rendered, tc.files)
				}
				return
			}
			if !strings.Contains(body, tc.wantMore+", listed in full by status.json") {
				t.Errorf("body does not say %q", tc.wantMore)
			}
		})
	}
}

// state.json holds declared flow data in the clear and this card is
// served to anyone who can reach ingress, so this runs the real path with
// statetest.PoisonedState - which it must use, not a literal.
func TestDashboardRendersNoValueOutOfTheAgentsState(t *testing.T) {
	devEnv(t)
	agent := recon.New(
		options.Options{RepoURL: "https://example.invalid/demo.git", Branch: "main", IntervalMinutes: 5},
		recon.Deps{Applier: &stateOnlyApplier{state: statetest.PoisonedState(t)}, History: noHistory{}},
	)
	handler := New(agent)

	body := doRequest(t, handler, http.MethodGet, "/", nil).Body.String()

	if strings.Contains(body, statetest.Sentinel) {
		t.Error("the dashboard renders a value out of state.json")
	}
	// Without this the check above would pass on a card that rendered
	// nothing at all.
	for _, name := range statetest.ManagedNames() {
		if !strings.Contains(body, "<code>"+name+"</code>") {
			t.Errorf("body does not render %q, so the check above proves nothing", name)
		}
	}
}

// stateOnlyApplier answers with one canned state and does nothing else -
// all of the seam the test above needs.
type stateOnlyApplier struct{ state applier.State }

func (s *stateOnlyApplier) Apply(
	context.Context, []applier.Change, string, string, options.Options,
) (applier.Result, error) {
	return applier.Result{}, nil
}

func (s *stateOnlyApplier) StateLoad() applier.State      { return s.state }
func (s *stateOnlyApplier) StateSave(applier.State) error { return nil }
func (s *stateOnlyApplier) RollbackFrom(string, string) applier.Result {
	return applier.Result{}
}
func (s *stateOnlyApplier) PruneStashDirs(int, string)    {}
func (s *stateOnlyApplier) MakeStashDir() (string, error) { return "", nil }

// noHistory keeps the reconciler above off /data/history.jsonl, which the
// real store would stat and read on this machine.
type noHistory struct{}

func (noHistory) Append(history.Record) error { return nil }
func (noHistory) Load() []history.Record      { return nil }

// Every group serializes as [] and never null, on an empty agent as much
// as a full one, so no /status.json reader has to special-case it.
func TestStatusJSONCarriesTheManagedInventory(t *testing.T) {
	devEnv(t)
	handler := New(newFakeAgent())

	var got map[string]json.RawMessage
	body := doRequest(t, handler, http.MethodGet, "/status.json", nil).Body.Bytes()
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("status.json is not valid JSON: %v", err)
	}

	managed, ok := got["managed"]
	if !ok {
		t.Fatalf("status.json has no managed object: %s", body)
	}
	want := `{"files":[],"registry":[],"entities":[],"dashboards":[],"addons":[],"integrations":[],"subentries":[],"hacs":[]}`
	if string(managed) != want {
		t.Errorf("managed = %s, want %s", managed, want)
	}
}

// --- pause / resume -----------------------------------------------------

// hxButton is the whole <button> posting to route, for assertions about
// markup rather than dispatch. applyButton's extraction, made generic.
func hxButton(t *testing.T, body, route string) string {
	t.Helper()
	marker := strings.Index(body, `hx-post="`+route+`"`)
	if marker < 0 {
		t.Fatalf("body does not contain hx-post=%q", route)
	}
	open := strings.LastIndex(body[:marker], "<button")
	closed := strings.Index(body[marker:], "</button>")
	if open < 0 || closed < 0 {
		t.Fatalf("could not delimit the %s button element", route)
	}
	return body[open : marker+closed]
}

func TestPauseRouteSwitchesTheLoopOffAndSaysSo(t *testing.T) {
	devEnv(t)
	agent := newFakeAgent()
	handler := New(agent)

	rec := doRequest(t, handler, http.MethodPost, "/pause", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := agent.dispatchedCalls().setPaused; len(got) != 1 || !got[0] {
		t.Fatalf("set_paused calls = %v, want one true", got)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`<span class="chip chip-neutral chip-paused">paused</span>`,
		`hx-post="resume"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the answer does not render %q", want)
		}
	}
	// The banner is the only place the pause contract is written down, so
	// its promises are asserted, not just its presence. Against the
	// unwrapped text - the paragraph is hard-wrapped (see unwrapped).
	banner := unwrapped(body)
	for _, want := range []string{
		"Automatic checks are paused.",
		"will not check, apply or update add-ons on its own",
		// One phrase per promise, not the whole sentence: the banner gets
		// reworded, and pinning its prose fails on edits about nothing.
		"Roll Back still work",
		"the last check before the pause",
	} {
		if !strings.Contains(banner, want) {
			t.Errorf("the paused banner does not promise %q", want)
		}
	}
	// The add-on clause is NOT among them: this agent watches no add-ons,
	// so there is no card to point at. See the two tests below.
	if strings.Contains(banner, "Check for updates on the add-on card") {
		t.Error("the paused banner promises a card this agent does not render")
	}
	if strings.Contains(body, `hx-post="pause"`) {
		t.Error("the answer still offers Pause, want it swapped for Resume")
	}
}

// The banner is the only written record of what a pause does NOT stop, so
// each clause is asserted against the state that earns it. Promising a
// control that is not on the page is the failure these two guard.
func TestThePausedBannerPromisesTheAddonCheckOnlyWhenThatCardExists(t *testing.T) {
	devEnv(t)
	agent := newFakeAgent()
	agent.paused = true
	agent.autoUpdateEnabled = true

	banner := unwrapped(doRequest(t, New(agent), http.MethodPost, "/pause", nil).Body.String())

	for _, want := range []string{
		// A person pressing a button is not unattended activity, so the
		// check still runs while paused - and outside dry run it installs.
		"Check for updates on the add-on card",
		"still installs what it finds",
	} {
		if !strings.Contains(banner, want) {
			t.Errorf("the paused banner does not promise %q", want)
		}
	}
}

func TestThePausedBannerDoesNotPromiseAnInstallUnderDryRun(t *testing.T) {
	devEnv(t)
	agent := newFakeAgent()
	agent.paused = true
	agent.autoUpdateEnabled = true
	agent.dryRun = true

	banner := unwrapped(doRequest(t, New(agent), http.MethodPost, "/pause", nil).Body.String())

	// The card is still named: the button is there and still works.
	if !strings.Contains(banner, "Check for updates on the add-on card") {
		t.Error("the paused banner does not name the add-on check")
	}
	// What it must not claim is the install. checkOneAddon answers "update
	// available (dry run, not installing)" and writes nothing, which is why
	// the button drops its confirm here - the banner has to agree with it.
	if strings.Contains(banner, "still installs what it finds") {
		t.Error("the paused banner promises an install that dry run will not do")
	}
}

func TestResumeRouteSwitchesTheLoopBackOn(t *testing.T) {
	devEnv(t)
	agent := newFakeAgent()
	agent.paused = true
	handler := New(agent)

	rec := doRequest(t, handler, http.MethodPost, "/resume", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := agent.dispatchedCalls().setPaused; len(got) != 1 || got[0] {
		t.Fatalf("set_paused calls = %v, want one false", got)
	}
	body := rec.Body.String()
	if strings.Contains(body, "Automatic checks are paused.") {
		t.Error("the answer still renders the paused banner")
	}
	if strings.Contains(body, `<span class="chip chip-neutral chip-paused">paused</span>`) {
		t.Error("the answer still renders the paused chip")
	}
	if !strings.Contains(body, `hx-post="pause"`) {
		t.Error("the answer does not offer Pause again")
	}
}

// Why this route skips opRoute: the moment a user most wants the loop
// stopped is when an unexpected apply is already running.
func TestPauseWorksWhileAnOperationIsRunning(t *testing.T) {
	devEnv(t)
	agent := newFakeAgent()
	agent.busy = true
	handler := New(agent)

	rec := doRequest(t, handler, http.MethodPost, "/pause", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := agent.dispatchedCalls().setPaused; len(got) != 1 || !got[0] {
		t.Errorf("set_paused calls = %v, want one true even while busy", got)
	}
}

func TestPauseButtonIsNeverDisabledAndNeverConfirms(t *testing.T) {
	for _, busy := range []bool{false, true} {
		devEnv(t)
		agent := newFakeAgent()
		agent.busy = busy
		handler := New(agent)

		body := doRequest(t, handler, http.MethodGet, "/", nil).Body.String()

		button := hxButton(t, body, "pause")
		if buttonIsDisabled(button) {
			t.Errorf("pause button (busy=%v) = %q, want it enabled", busy, button)
		}
		// Instantly reversible and it changes nothing on the box, so a
		// dialog here would only teach people to click through dialogs.
		if strings.Contains(button, "hx-confirm") {
			t.Errorf("pause button (busy=%v) asks for confirmation", busy)
		}
	}
}

func TestResumeButtonIsNeverDisabledEither(t *testing.T) {
	devEnv(t)
	agent := newFakeAgent()
	agent.paused = true
	agent.busy = true
	handler := New(agent)

	body := doRequest(t, handler, http.MethodGet, "/", nil).Body.String()

	if button := hxButton(t, body, "resume"); buttonIsDisabled(button) {
		t.Errorf("resume button = %q, want it enabled while busy", button)
	}
}

// A flag that could not be written to /data still took, so the page says
// paused rather than erroring about a press that worked.
func TestPauseRouteRendersThePausedPageWhenTheFlagCouldNotBeRecorded(t *testing.T) {
	devEnv(t)
	agent := newFakeAgent()
	agent.setPausedErr = errors.New("open /data/paused: read-only file system")
	handler := New(agent)

	rec := doRequest(t, handler, http.MethodPost, "/pause", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Automatic checks are paused.") {
		t.Error("the answer does not show the agent as paused")
	}
}

func TestStatusJSONCarriesPaused(t *testing.T) {
	devEnv(t)
	agent := newFakeAgent()
	agent.paused = true
	handler := New(agent)

	var got map[string]any
	body := doRequest(t, handler, http.MethodGet, "/status.json", nil).Body.Bytes()
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("status.json is not valid JSON: %v", err)
	}

	if got["paused"] != true {
		t.Errorf("paused = %v, want true", got["paused"])
	}
	// Not a state: statusd.States is a closed vocabulary automations key
	// on, so paused rides beside it.
	if got["state"] == "paused" {
		t.Error("paused leaked into the state field, want it beside the state")
	}
}

// The header must agree with the banner: "Checks every 5 min" over a
// paused agent contradicts itself where a reader looks first.
func TestMetaLineSaysChecksArePausedInsteadOfTheInterval(t *testing.T) {
	devEnv(t)
	agent := newFakeAgent()
	agent.paused = true
	handler := New(agent)

	body := doRequest(t, handler, http.MethodGet, "/", nil).Body.String()

	if !strings.Contains(body, "Checks paused &middot;") {
		t.Error("the meta line does not say checks are paused")
	}
	if strings.Contains(body, "Checks every 5 min") {
		t.Error("the meta line still advertises the interval while paused")
	}
}

// A freshly downloaded integration is on disk and not imported. Nothing
// is failing, so it is a neutral chip and not an orange health one - and
// it names the domains, since "restart Home Assistant" says nothing.
func TestRestartReminderChipNamesTheDownloadedDomains(t *testing.T) {
	devEnv(t)
	agent := newFakeAgent()
	agent.hacsRestartPending = []string{"adaptive_lighting", "anker_solix"}
	handler := New(agent)

	body := doRequest(t, handler, http.MethodGet, "/", nil).Body.String()

	want := `<span class="chip chip-neutral chip-restart">downloaded, not loaded yet: adaptive_lighting, anker_solix &middot; restart Home Assistant, then set it up (or declare its entry in gitops/integrations.yaml)</span>`
	if !strings.Contains(body, want) {
		t.Errorf("body does not render %q", want)
	}
	// Not a health warning: the orange row is gated on HasHealthWarnings,
	// and a download waiting for a restart is not a failing side job.
	if strings.Contains(body, `class="chips"`) {
		t.Error("a restart reminder raised the standing-health chip row")
	}
	// Nor the PAUSE sentinel: index.html reads .chip-paused to announce
	// the loop stopped. Matched on the rendered chip, since the class name
	// alone also appears in the page's script.
	if strings.Contains(body, `class="chip chip-neutral chip-paused"`) {
		t.Error("the restart reminder carries the paused sentinel")
	}
}

// An empty list must not leave a chip telling every user to restart Home
// Assistant forever.
func TestRestartReminderChipAbsentWithNothingPending(t *testing.T) {
	devEnv(t)
	handler := New(newFakeAgent())

	body := doRequest(t, handler, http.MethodGet, "/", nil).Body.String()

	if strings.Contains(body, "downloaded, not loaded yet") {
		t.Error("the restart reminder is rendered on an agent that downloaded nothing")
	}
}

// The paused chip is not a health warning: those are orange failing side
// jobs, announced as a group. chip-neutral keeps it out of both.
func TestPausedChipIsNeutralAndOutsideTheHealthChipRow(t *testing.T) {
	devEnv(t)
	agent := newFakeAgent()
	agent.paused = true
	handler := New(agent)

	body := doRequest(t, handler, http.MethodGet, "/", nil).Body.String()

	if !strings.Contains(body, `<span class="chip chip-neutral chip-paused">paused</span>`) {
		t.Error("the paused chip is not marked neutral")
	}
	// The health row itself is gated on HasHealthWarnings and must stay
	// absent on an agent whose only chip is this one.
	if strings.Contains(body, `class="chips"`) {
		t.Error("a pause raised the standing-health chip row")
	}
}

// Bug fix: a POST answers before its operation has necessarily started, so
// a script reading /status.json straight afterwards could not tell "not
// started yet" from "finished". The id it hands back closes that gap.
func TestPostImportHandsBackAnOperationIdAPollerCanWaitOn(t *testing.T) {
	devEnv(t)
	agent := newFakeAgent()
	agent.importEnabled = true
	release := agent.gated()
	handler := New(agent)

	rec := doRequest(t, handler, http.MethodPost, "/import", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	id := rec.Header().Get("X-GitOps-Op-Id")
	if id != "1" {
		t.Fatalf("X-GitOps-Op-Id = %q, want %q", id, "1")
	}
	agent.awaitStart(t)

	running := operationView(t, handler)
	if !running.Running {
		t.Error("operation.running = false while the import is still held")
	}
	if running.Name != "import" {
		t.Errorf("operation.name = %q, want %q", running.Name, "import")
	}

	release()
	agent.awaitFinish(t)

	done := operationView(t, handler)
	if done.Running {
		t.Error("operation.running = true after the import finished")
	}
	if done.Error != "" {
		t.Errorf("operation.error = %q, want empty", done.Error)
	}
	if done.FinishedUTC == "" {
		t.Error("operation.finished_utc is empty")
	}
}

func TestAFailedOperationRecordsItsError(t *testing.T) {
	devEnv(t)
	agent := newFakeAgent()
	agent.importEnabled = true
	agent.importErr = errors.New("refusing to import: nothing importable")
	handler := New(agent)

	doRequest(t, handler, http.MethodPost, "/import", nil)
	agent.awaitFinish(t)

	got := operationView(t, handler)
	if got.Running {
		t.Error("operation.running = true after the operation returned")
	}
	if !strings.Contains(got.Error, "nothing importable") {
		t.Errorf("operation.error = %q, want the agent's error", got.Error)
	}
}

func TestARefusedOperationStartsNothingAndSaysSo(t *testing.T) {
	devEnv(t)
	agent := newFakeAgent()
	agent.importEnabled = true
	agent.busy = true
	handler := New(agent)

	rec := doRequest(t, handler, http.MethodPost, "/import", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("X-GitOps-Op-Id"); got != "" {
		t.Errorf("X-GitOps-Op-Id = %q, want none on a refusal", got)
	}
	if got := rec.Header().Get("X-GitOps-Op-Refused"); got != "busy" {
		t.Errorf("X-GitOps-Op-Refused = %q, want %q", got, "busy")
	}
	if got := operationView(t, handler); got.ID != 0 {
		t.Errorf("operation.id = %d, want 0: nothing was started", got.ID)
	}
}

// operationView reads /status.json's operation object.
func operationView(t *testing.T, handler http.Handler) opView {
	t.Helper()
	rec := doRequest(t, handler, http.MethodGet, "/status.json", nil)
	var body struct {
		Operation opView `json:"operation"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	return body.Operation
}

func TestUnseededBannerNamesTheBranchAndTheImportButton(t *testing.T) {
	devEnv(t)
	agent := newFakeAgent()
	agent.state = recon.StateUnseeded
	agent.importEnabled = true
	handler := New(agent)

	body := doRequest(t, handler, http.MethodGet, "/", nil).Body.String()
	if !strings.Contains(body, "does not exist in this repository yet") {
		t.Error("no unseeded banner on the page")
	}
	if !strings.Contains(body, "Press <strong>Import</strong>") {
		t.Error("the banner does not point at the Import button")
	}
}

func TestUnseededBannerExplainsAllowImportWhenImportIsOff(t *testing.T) {
	devEnv(t)
	agent := newFakeAgent()
	agent.state = recon.StateUnseeded
	agent.importEnabled = false
	handler := New(agent)

	body := doRequest(t, handler, http.MethodGet, "/", nil).Body.String()
	if !strings.Contains(body, "allow_import") {
		t.Error("the banner does not say how to turn import on")
	}
}

func TestImportRecordFailingChipRenders(t *testing.T) {
	devEnv(t)
	agent := newFakeAgent()
	agent.importRecordFailing = true
	handler := New(agent)

	body := doRequest(t, handler, http.MethodGet, "/", nil).Body.String()
	if !strings.Contains(body, "import record not saved") {
		t.Error("no chip for an import whose record could not be saved")
	}
}

// The conflicts card is the only place a path the agent refuses to sync in
// either direction is visible. Driven from a persisted state, since New
// hydrates the mirror at startup.
func TestDashboardRendersTheConflictsCard(t *testing.T) {
	devEnv(t)
	state := applier.State{
		Manifest:           []string{},
		ConflictedPaths:    []string{"scenes.yaml", "packages/climate.yaml"},
		LastConflictBranch: "gitops/conflict-20260806T091500Z",
		LastConflictUTC:    "2026-08-06T09:15:00Z",
	}
	agent := recon.New(
		options.Options{RepoURL: "https://example.invalid/demo.git", Branch: "main", IntervalMinutes: 5, CaptureLiveChanges: true},
		recon.Deps{Applier: &stateOnlyApplier{state: state}, History: noHistory{}},
	)
	handler := New(agent)

	body := doRequest(t, handler, http.MethodGet, "/", nil).Body.String()

	for _, want := range []string{"Needs your decision", "scenes.yaml", "packages/climate.yaml", "gitops/conflict-20260806T091500Z"} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard does not render %q", want)
		}
	}

	// The fragment is polled every few seconds and compared byte for byte,
	// so an unsorted conflict list would re-swap the page on every poll.
	second := doRequest(t, handler, http.MethodGet, "/", nil).Body.String()
	if body != second {
		t.Error("two identical renders differ: something in the conflicts card is unstable across polls")
	}
}

// A conflict whose live copies could not be parked is still a conflict, and
// the card must say so rather than naming a branch that does not exist.
func TestDashboardSaysWhenAConflictCouldNotBeParked(t *testing.T) {
	devEnv(t)
	state := applier.State{Manifest: []string{}, ConflictedPaths: []string{"scenes.yaml"}}
	agent := recon.New(
		options.Options{RepoURL: "https://example.invalid/demo.git", Branch: "main", IntervalMinutes: 5, CaptureLiveChanges: true},
		recon.Deps{Applier: &stateOnlyApplier{state: state}, History: noHistory{}},
	)

	body := doRequest(t, New(agent), http.MethodGet, "/", nil).Body.String()

	if !strings.Contains(body, "could not be pushed to a branch") {
		t.Error("the card does not say the live copies were not parked")
	}
	if strings.Contains(body, "Live copies preserved on <code></code>") {
		t.Error("the card names an empty branch")
	}
}

// A browser-marked cross-site POST must be refused: requireIngress only
// checks the network hop, and a cross-site form rides the user's own
// ingress session through the same proxy.
func TestCrossSitePostIsRefused(t *testing.T) {
	devEnv(t)
	agent := newFakeAgent()
	handler := New(agent)

	req := httptest.NewRequest(http.MethodPost, "/apply", nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if got := agent.dispatchedCalls().apply; len(got) != 0 {
		t.Errorf("apply calls = %v, want none", got)
	}
}

// The dashboard's own htmx calls (same-origin), a user-typed request
// (none) and a header-less API script must all still pass.
func TestSameOriginAndHeaderlessPostsPass(t *testing.T) {
	for _, site := range []string{"same-origin", "none", ""} {
		devEnv(t)
		agent := newFakeAgent()
		handler := New(agent)

		req := httptest.NewRequest(http.MethodPost, "/reconcile", nil)
		if site != "" {
			req.Header.Set("Sec-Fetch-Site", site)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("Sec-Fetch-Site %q: status = %d, want 200", site, rec.Code)
		}
		agent.awaitFinish(t)
	}
}

// An oversized retry key must be refused before it can reach the event
// ring, which renders into every fragment poll.
func TestRetryRefusesAnOversizedKey(t *testing.T) {
	devEnv(t)
	agent := blockedAgent(blockedIntegration())
	handler := New(agent)

	rec := doForm(t, handler, "/retry", url.Values{"key": {strings.Repeat("x", maxRetryKeyLen+1)}})

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if got := agent.dispatchedCalls().retry; len(got) != 0 {
		t.Errorf("retry calls = %v, want none", got)
	}
}
