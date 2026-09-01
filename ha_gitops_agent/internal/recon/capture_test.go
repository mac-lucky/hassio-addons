package recon

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/applier"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/differ"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/gitsync"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/options"
)

// One row per line of the decision table. The two rows that matter most are
// the traps: an "add" whose live file is present-but-unreadable must never
// become a deletion, and an "add" the base never tracked is a create rather
// than one.
func TestClassifyVerdicts(t *testing.T) {
	cases := []struct {
		name string
		in   facts
		want verdict
	}{
		{
			name: "no base at all falls back to the one-way behaviour",
			in:   facts{kind: "update", baseKnown: false, baseTracks: true, repoMoved: true},
			want: verdictApply,
		},
		{
			name: "no base does not turn a live edit into a capture either",
			in:   facts{kind: "update", baseKnown: false, baseTracks: true, repoMoved: false},
			want: verdictApply,
		},

		// --- update -------------------------------------------------------
		{
			name: "only the repository moved",
			in:   facts{kind: "update", baseKnown: true, baseTracks: true, repoMoved: true, liveIsBase: true},
			want: verdictApply,
		},
		{
			name: "only live moved",
			in:   facts{kind: "update", baseKnown: true, baseTracks: true, repoMoved: false, liveIsBase: false},
			want: verdictCapture,
		},
		{
			name: "both moved",
			in:   facts{kind: "update", baseKnown: true, baseTracks: true, repoMoved: true, liveIsBase: false},
			want: verdictConflict,
		},
		{
			name: "neither moved, so the two reads disagreed and waiting settles it",
			in:   facts{kind: "update", baseKnown: true, baseTracks: true, repoMoved: false, liveIsBase: true},
			want: verdictDefer,
		},
		{
			name: "added on both sides with different content",
			in:   facts{kind: "update", baseKnown: true, baseTracks: false, repoMoved: true},
			want: verdictConflict,
		},

		// --- add: tracked in the repository, not readable live -------------
		{
			name: "the trap: present but unreadable is never a deletion",
			in:   facts{kind: "add", baseKnown: true, baseTracks: true, repoMoved: false, liveGone: false},
			want: verdictDefer,
		},
		{
			name: "unreadable stays a defer even when the repository moved",
			in:   facts{kind: "add", baseKnown: true, baseTracks: true, repoMoved: true, liveGone: false},
			want: verdictDefer,
		},
		{
			name: "new in the repository and never live is a create",
			in:   facts{kind: "add", baseKnown: true, baseTracks: false, repoMoved: true, liveGone: true},
			want: verdictApply,
		},
		{
			name: "deleted live, repository unmoved",
			in:   facts{kind: "add", baseKnown: true, baseTracks: true, repoMoved: false, liveGone: true, agentWroteIt: true},
			want: verdictCaptureDelete,
		},
		{
			name: "deleted live, edited in the repository",
			in:   facts{kind: "add", baseKnown: true, baseTracks: true, repoMoved: true, liveGone: true, agentWroteIt: true},
			want: verdictConflict,
		},
		{
			// README.md, LICENSE and .github/ are tracked, are not excluded,
			// and are not meant to be live. Reading "the base tracks it and it
			// is not live" as a deletion would git rm them off the branch.
			name: "tracked but never applied live is a create, not a deletion",
			in:   facts{kind: "add", baseKnown: true, baseTracks: true, repoMoved: false, liveGone: true},
			want: verdictApply,
		},
		{
			name: "tracked but never applied live, edited in the repository",
			in:   facts{kind: "add", baseKnown: true, baseTracks: true, repoMoved: true, liveGone: true},
			want: verdictApply,
		},

		// --- delete: left the repository, still live -----------------------
		{
			name: "left the repository and live is untouched",
			in:   facts{kind: "delete", baseKnown: true, baseTracks: true, repoMoved: true, liveIsBase: true},
			want: verdictApply,
		},
		{
			name: "left the repository but live was edited",
			in:   facts{kind: "delete", baseKnown: true, baseTracks: true, repoMoved: true, liveIsBase: false},
			want: verdictConflict,
		},
		{
			name: "left the repository and the base never held it",
			in:   facts{kind: "delete", baseKnown: true, baseTracks: false, repoMoved: true},
			want: verdictConflict,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classify(tc.in); got != tc.want {
				t.Errorf("classify(%+v) = %s, want %s", tc.in, got, tc.want)
			}
		})
	}
}

// Nothing may capture or conflict without a base: an agent that has never
// applied would otherwise push its whole config on the first cycle.
func TestClassifyWithoutABaseOnlyEverApplies(t *testing.T) {
	for _, kind := range []string{"add", "update", "delete"} {
		for _, repoMoved := range []bool{false, true} {
			for _, liveGone := range []bool{false, true} {
				in := facts{kind: kind, baseKnown: false, repoMoved: repoMoved, liveGone: liveGone}
				if got := classify(in); got != verdictApply {
					t.Errorf("classify(%+v) = %s, want apply", in, got)
				}
			}
		}
	}
}

func TestCaptureBasesPrefersTheCaptureCommitForCapturedPaths(t *testing.T) {
	bases := captureBases{
		capture:  "capture1",
		captured: map[string]bool{"automations.yaml": true},
		fallback: "lastgood",
	}

	if got := bases.forPath("automations.yaml"); got != "capture1" {
		t.Errorf("forPath(captured) = %q, want the capture commit: classifying it against the older base reads the agent's own capture as a repository change", got)
	}
	if got := bases.forPath("scripts.yaml"); got != "lastgood" {
		t.Errorf("forPath(other) = %q, want the fallback", got)
	}
}

func TestCaptureBasesFallBackAndReportNoBase(t *testing.T) {
	cases := []struct {
		name  string
		bases captureBases
		path  string
		want  string
	}{
		{
			name:  "no capture outstanding",
			bases: captureBases{fallback: "lastgood"},
			path:  "automations.yaml",
			want:  "lastgood",
		},
		{
			name:  "a capture commit but this path is not in it",
			bases: captureBases{capture: "capture1", captured: map[string]bool{"other.yaml": true}, fallback: "lastgood"},
			path:  "automations.yaml",
			want:  "lastgood",
		},
		{
			name:  "nothing at all",
			bases: captureBases{},
			path:  "automations.yaml",
			want:  "",
		},
		{
			name:  "a captured path still resolves with no fallback",
			bases: captureBases{capture: "capture1", captured: map[string]bool{"automations.yaml": true}},
			path:  "automations.yaml",
			want:  "capture1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.bases.forPath(tc.path); got != tc.want {
				t.Errorf("forPath(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}

// An import does not advance LastGoodSHA, so an install that applied before
// it imported holds two disagreeing bases. Classifying against the older one
// reads every path the import moved as a repository change, which turns the
// user's next live edit to those files into a conflict that is not real.
func TestResolveBasesPrefersTheLaterOfTheApplyAndImportCommits(t *testing.T) {
	// Both candidates descend from the tip unless a case says otherwise.
	descends := map[string]bool{"good1->tip1": true, "import1->tip1": true}
	with := func(extra ...string) map[string]bool {
		m := map[string]bool{}
		for k, v := range descends {
			m[k] = v
		}
		for _, k := range extra {
			m[k] = true
		}
		return m
	}

	cases := []struct {
		name     string
		state    applier.State
		ancestor map[string]bool
		want     string
	}{
		{
			name:     "the import came after the apply",
			state:    applier.State{LastGoodSHA: "good1", LastImportSHA: "import1"},
			ancestor: with("good1->import1"),
			want:     "import1",
		},
		{
			name:     "the apply came after the import",
			state:    applier.State{LastGoodSHA: "good1", LastImportSHA: "import1"},
			ancestor: with(),
			want:     "good1",
		},
		{
			name:     "only an import has ever run",
			state:    applier.State{LastImportSHA: "import1"},
			ancestor: with(),
			want:     "import1",
		},
		{
			name:     "only an apply has ever run",
			state:    applier.State{LastGoodSHA: "good1"},
			ancestor: with(),
			want:     "good1",
		},
		{
			name:     "neither",
			state:    applier.State{},
			ancestor: with(),
			want:     "",
		},
		{
			// VM e2e: force-pushing the tracked branch leaves the recorded
			// base in the object database but off the tip's line. Diffing the
			// two then reports everything that differs across the divergence
			// as a repository change, and a live edit to any of it became a
			// conflict that never happened.
			name:     "a base orphaned by a force-push is no base",
			state:    applier.State{LastGoodSHA: "good1", LastImportSHA: "import1"},
			ancestor: map[string]bool{},
			want:     "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fakes := newReconcilerFakes()
			fakes.git.ancestorOf = tc.ancestor
			r := fakes.reconciler(captureOpts())

			if got := r.resolveBases(context.Background(), "tip1", tc.state).fallback; got != tc.want {
				t.Errorf("fallback = %q, want %q", got, tc.want)
			}
		})
	}
}

// The capture override is orphaned by the same force-push, and dropping it
// must not silently promote the fallback for those paths either.
func TestAnOrphanedCaptureBaseIsDropped(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.git.ancestorOf = map[string]bool{"good1->tip1": true}
	r := fakes.reconciler(captureOpts())

	bases := r.resolveBases(context.Background(), "tip1", applier.State{
		LastGoodSHA:      "good1",
		LastCaptureSHA:   "orphan1",
		LastCapturePaths: []string{"automations.yaml"},
	})

	if bases.capture != "" {
		t.Errorf("capture base = %q, want empty: it is not on the tip's line", bases.capture)
	}
	if got := bases.forPath("automations.yaml"); got != "good1" {
		t.Errorf("forPath = %q, want good1", got)
	}
}

// --- the cycle ------------------------------------------------------------

// captureOpts is the mode the whole feature is about: the agent both applies
// repository changes and captures live ones.
func captureOpts() options.Options {
	opts := baseOpts()
	opts.DryRun = false
	opts.CaptureLiveChanges = true
	return opts
}

// capturedPaths is what CaptureFiles was handed across every call.
func capturedPaths(f *fakeGit) []string {
	var paths []string
	for _, call := range f.captureCalls {
		for _, file := range call.files {
			paths = append(paths, file.Path)
		}
	}
	return paths
}

// appliedPaths is what the applier was handed across every call.
func appliedPaths(f *fakeApplier) []string {
	var paths []string
	for _, call := range f.applyCalls {
		for _, change := range call {
			paths = append(paths, change.Path)
		}
	}
	return paths
}

func TestReconcileCapturesLiveOnlyChangesAndRemovesThemFromThePlan(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.git.sha = "tip1"
	fakes.applier.state = applier.State{Manifest: []string{"automations.yaml"}, LastGoodSHA: "base1"}
	fakes.differ.changes = []differ.Change{{Path: "automations.yaml", Kind: "update"}}
	// Nothing moved in the repository, so the live side is the only one that
	// can have.
	fakes.git.changedBetween = nil

	r := fakes.reconciler(captureOpts())
	r.runCycle(context.Background())

	if got := capturedPaths(fakes.git); !slices.Equal(got, []string{"automations.yaml"}) {
		t.Errorf("captured = %v, want [automations.yaml]", got)
	}
	if got := appliedPaths(fakes.applier); len(got) != 0 {
		t.Errorf("applied = %v, want nothing: a captured path must never be applied over", got)
	}
	if got := r.Status().Pending; len(got) != 0 {
		t.Errorf("pending = %+v, want empty: the capture resolved the drift", got)
	}
	if got := r.Status().State; got != StateInSync {
		t.Errorf("state = %q, want %q", got, StateInSync)
	}
}

// A repository holds files that are not supposed to be live - README.md,
// LICENSE, .github/ - and none of them is excluded, so the differ reports each
// as "add". Before the manifest guard, an import base tracked them and the
// classifier read "not live" as "the user deleted it" and git rm'd them off
// the tracked branch. This is the flow the unseeded bootstrap steers into.
func TestRepositoryOnlyFilesAreNeverDeletedFromTheBranch(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.git.sha = "tip1"
	// An import ran and nothing has ever been applied, so the manifest is
	// empty: the agent has put nothing live and can vouch for no deletion.
	fakes.applier.state = applier.State{LastImportSHA: "import1"}
	fakes.differ.changes = []differ.Change{
		{Path: "README.md", Kind: "add"},
		{Path: ".github/workflows/ci.yaml", Kind: "add"},
	}
	fakes.git.liveFacts = map[string]gitsync.LiveFacts{
		"README.md":                 {BaseTracks: true, Gone: true},
		".github/workflows/ci.yaml": {BaseTracks: true, Gone: true},
	}

	r := fakes.reconciler(captureOpts())
	r.runCycle(context.Background())

	if got := capturedPaths(fakes.git); len(got) != 0 {
		t.Errorf("captured = %v, want nothing: these were never live, so their absence is not a deletion", got)
	}
	if got := appliedPaths(fakes.applier); !slices.Equal(got, []string{"README.md", ".github/workflows/ci.yaml"}) {
		t.Errorf("applied = %v, want both paths: an unapplied repository file is an ordinary create", got)
	}
	if got := r.Status().Conflicts; len(got) != 0 {
		t.Errorf("conflicts = %v, want none", got)
	}
}

// The one-way path must be untouched by all of this.
func TestReconcileAppliesRepoOnlyChangesAndCapturesNothing(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.git.sha = "tip1"
	fakes.applier.state = applier.State{Manifest: []string{"automations.yaml"}, LastGoodSHA: "base1"}
	fakes.differ.changes = []differ.Change{{Path: "automations.yaml", Kind: "update"}}
	fakes.git.changedBetween = []string{"automations.yaml"}
	fakes.git.liveFacts = map[string]gitsync.LiveFacts{
		"automations.yaml": {BaseTracks: true, MatchesBase: true},
	}

	r := fakes.reconciler(captureOpts())
	r.runCycle(context.Background())

	if len(fakes.git.captureCalls) != 0 {
		t.Errorf("CaptureFiles called %d time(s), want none: only the repository moved", len(fakes.git.captureCalls))
	}
	if got := appliedPaths(fakes.applier); !slices.Equal(got, []string{"automations.yaml"}) {
		t.Errorf("applied = %v, want [automations.yaml]", got)
	}
}

// The headline: one cycle, two paths, opposite directions.
func TestOneCycleCapturesOnePathAndAppliesAnother(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.git.sha = "tip1"
	fakes.applier.state = applier.State{Manifest: []string{"a.yaml", "b.yaml"}, LastGoodSHA: "base1"}
	fakes.differ.changes = []differ.Change{
		{Path: "a.yaml", Kind: "update"},
		{Path: "b.yaml", Kind: "update"},
	}
	// b.yaml moved in the repository and live still matches the base; a.yaml
	// did neither, so live is what moved there.
	fakes.git.changedBetween = []string{"b.yaml"}
	fakes.git.liveFacts = map[string]gitsync.LiveFacts{
		"b.yaml": {BaseTracks: true, MatchesBase: true},
	}

	r := fakes.reconciler(captureOpts())
	r.runCycle(context.Background())

	if got := capturedPaths(fakes.git); !slices.Equal(got, []string{"a.yaml"}) {
		t.Errorf("captured = %v, want [a.yaml] only", got)
	}
	if got := appliedPaths(fakes.applier); !slices.Equal(got, []string{"b.yaml"}) {
		t.Errorf("applied = %v, want [b.yaml] only", got)
	}
}

func TestAConflictedPathIsNeitherAppliedNorCapturedAndIsParked(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.git.sha = "tip1"
	fakes.applier.state = applier.State{Manifest: []string{"a.yaml", "b.yaml"}, LastGoodSHA: "base1"}
	fakes.differ.changes = []differ.Change{
		{Path: "a.yaml", Kind: "update"},
		{Path: "b.yaml", Kind: "update"},
	}
	// a.yaml moved on BOTH sides; b.yaml only in the repository.
	fakes.git.changedBetween = []string{"a.yaml", "b.yaml"}
	fakes.git.liveFacts = map[string]gitsync.LiveFacts{
		"a.yaml": {BaseTracks: true, MatchesBase: false},
		"b.yaml": {BaseTracks: true, MatchesBase: true},
	}

	r := fakes.reconciler(captureOpts())
	r.runCycle(context.Background())

	if len(fakes.git.captureCalls) != 0 {
		t.Errorf("CaptureFiles called %d time(s), want none", len(fakes.git.captureCalls))
	}
	if got := appliedPaths(fakes.applier); !slices.Equal(got, []string{"b.yaml"}) {
		t.Errorf("applied = %v, want [b.yaml]: the conflicted path must be left alone in both directions", got)
	}
	if len(fakes.git.parkCalls) != 1 {
		t.Fatalf("ParkConflicts called %d time(s), want 1", len(fakes.git.parkCalls))
	}
	if got := fakes.git.parkCalls[0].files; len(got) != 1 || got[0].Path != "a.yaml" {
		t.Errorf("parked = %+v, want a.yaml", got)
	}

	if got := r.Status().Conflicts; !slices.Equal(got, []string{"a.yaml"}) {
		t.Fatalf("conflicts = %+v, want [a.yaml]", got)
	}
	if got := r.Status().ConflictBranch; got != "gitops/conflict-20260806T120000Z" {
		t.Errorf("conflict branch = %q, want the parked branch", got)
	}
	if !r.Status().HasHealthWarnings() && r.Status().CaptureFailing {
		t.Error("a conflict is a verdict, not a failure")
	}
	if r.Status().LastError != "" {
		t.Errorf("last_error = %q, want empty: a conflict does not fail the cycle", r.Status().LastError)
	}
}

// The second line of defence: a conflicted path must be unapplyable even
// from a plan built before it was one.
func TestAConflictedPathIsRefusedByApplyEvenFromAStalePlan(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.applier.state = applier.State{
		Manifest: []string{"a.yaml"}, LastGoodSHA: "base1", ConflictedPaths: []string{"a.yaml"},
	}

	r := fakes.reconciler(captureOpts())
	// Seeded directly, standing in for a plan made before the conflict was
	// recorded.
	r.withMu(func() { r.pending = []differ.Change{{Path: "a.yaml", Kind: "update"}} })

	r.ApplyNow(context.Background(), true)

	if got := appliedPaths(fakes.applier); len(got) != 0 {
		t.Errorf("applied = %v, want nothing: the conflict record must outrank a stale plan", got)
	}
}

// A transient diff failure must not dissolve the conflict record: the batch
// error branch routes everything to apply, and before this test existed that
// included the very paths the record was protecting, persisting an emptied
// list on the way.
func TestADiffErrorKeepsAStandingConflictOutOfTheApply(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.git.sha = "tip1"
	fakes.applier.state = applier.State{
		Manifest: []string{"a.yaml", "b.yaml"}, LastGoodSHA: "base1",
		ConflictedPaths:    []string{"a.yaml"},
		LastConflictBranch: "gitops/conflict-20260805T120000Z",
	}
	fakes.differ.changes = []differ.Change{
		{Path: "a.yaml", Kind: "update"},
		{Path: "b.yaml", Kind: "update"},
	}
	fakes.git.changedBetweenErr = errors.New("object store hiccup")

	r := fakes.reconciler(captureOpts())
	r.runCycle(context.Background())

	if got := appliedPaths(fakes.applier); !slices.Equal(got, []string{"b.yaml"}) {
		t.Errorf("applied = %v, want [b.yaml]: a diff error must not release a recorded conflict", got)
	}
	if got := r.Status().Conflicts; !slices.Equal(got, []string{"a.yaml"}) {
		t.Errorf("conflicts = %+v, want [a.yaml] still standing", got)
	}
	if len(fakes.git.parkCalls) != 0 {
		t.Errorf("ParkConflicts called %d time(s), want none: the set did not change", len(fakes.git.parkCalls))
	}
}

func TestAConflictClearsOnceTheTwoSidesAgree(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.git.sha = "tip1"
	fakes.applier.state = applier.State{
		Manifest: []string{"a.yaml"}, LastGoodSHA: "base1", ConflictedPaths: []string{"a.yaml"},
		LastConflictBranch: "gitops/conflict-20260805T120000Z",
	}
	// Nothing drifts any more, so nothing conflicts.
	fakes.differ.changes = nil

	r := fakes.reconciler(captureOpts())
	if len(r.Status().Conflicts) != 1 {
		t.Fatal("the fixture should start with a standing conflict")
	}

	r.runCycle(context.Background())

	if got := r.Status().Conflicts; len(got) != 0 {
		t.Errorf("conflicts = %+v, want cleared once the two sides agree", got)
	}
}

func TestAFailedCaptureBlocksTheApplyForThatPath(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.git.sha = "tip1"
	fakes.applier.state = applier.State{Manifest: []string{"a.yaml"}, LastGoodSHA: "base1"}
	fakes.differ.changes = []differ.Change{{Path: "a.yaml", Kind: "update"}}
	fakes.git.captureErr = errors.New("remote: pre-receive hook declined")

	r := fakes.reconciler(captureOpts())
	r.runCycle(context.Background())

	if got := appliedPaths(fakes.applier); len(got) != 0 {
		t.Errorf("applied = %v, want nothing: applying would destroy the edit the capture just failed to save", got)
	}
	status := r.Status()
	if !status.CaptureFailing {
		t.Error("capture_failing = false, want the standing flag raised")
	}
	if status.LastError != "" || status.State == StateError {
		t.Errorf("last_error = %q / state = %q, want a repository-write failure kept out of both", status.LastError, status.State)
	}
	if !hasEventContaining(status.Events, "pre-receive hook declined") {
		t.Error("the activity log does not name the push failure")
	}

	// A failed capture must not read as "in sync": the edit is still waiting
	// to be saved, it is only being kept away from the apply.
	if got := status.State; got != StateDriftPending {
		t.Errorf("state = %q, want %q while a capture is failing", got, StateDriftPending)
	}

	// Second failing cycle: the flag stands, and the transition guard means
	// the failure itself is not logged again.
	before := countEventsContaining(status.Events, "pre-receive hook declined")
	r.runCycle(context.Background())
	after := countEventsContaining(r.Status().Events, "pre-receive hook declined")
	if after != before {
		t.Errorf("the failure was logged %d time(s), then %d: the transition guard should log it once", before, after)
	}
	if !r.Status().CaptureFailing {
		t.Error("capture_failing cleared on the second failure")
	}
}

func TestCaptureRecoveryLogsOnce(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.git.sha = "tip1"
	fakes.applier.state = applier.State{Manifest: []string{"a.yaml"}, LastGoodSHA: "base1"}
	fakes.differ.changes = []differ.Change{{Path: "a.yaml", Kind: "update"}}
	fakes.git.captureErr = errors.New("remote rejected")

	r := fakes.reconciler(captureOpts())
	r.runCycle(context.Background())

	fakes.git.captureErr = nil
	r.runCycle(context.Background())

	status := r.Status()
	if status.CaptureFailing {
		t.Error("capture_failing = true after a successful capture")
	}
	if !hasEventContaining(status.Events, "capture is working again") {
		t.Error("the recovery was not logged")
	}
}

func TestCaptureIsSkippedWhenTheOptionIsOff(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.git.sha = "tip1"
	fakes.applier.state = applier.State{Manifest: []string{"a.yaml"}, LastGoodSHA: "base1"}
	fakes.differ.changes = []differ.Change{{Path: "a.yaml", Kind: "update"}}

	opts := baseOpts()
	opts.DryRun = false
	r := fakes.reconciler(opts)
	r.runCycle(context.Background())

	if len(fakes.git.captureCalls) != 0 {
		t.Errorf("CaptureFiles called %d time(s) with the option off", len(fakes.git.captureCalls))
	}
	if got := appliedPaths(fakes.applier); !slices.Equal(got, []string{"a.yaml"}) {
		t.Errorf("applied = %v, want the unchanged one-way behaviour", got)
	}
}

// Both would push the same live content, one to a throwaway branch nobody
// needs once the other has merged it.
func TestCaptureSupersedesTheAutomaticCommitBack(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.git.sha = "tip1"
	fakes.applier.state = applier.State{Manifest: []string{"a.yaml"}, LastGoodSHA: "base1"}
	fakes.differ.changes = []differ.Change{{Path: "a.yaml", Kind: "update"}}

	opts := baseOpts()
	opts.DryRun = true
	opts.CommitBack = true
	opts.CaptureLiveChanges = true
	r := fakes.reconciler(opts)
	r.runCycle(context.Background())

	if len(fakes.git.commitBackCalls) != 0 {
		t.Errorf("CommitBack called %d time(s), want none while capture is on", len(fakes.git.commitBackCalls))
	}
	if len(fakes.git.captureCalls) != 1 {
		t.Errorf("CaptureFiles called %d time(s), want 1", len(fakes.git.captureCalls))
	}
}

// THE regression test for the self-conflict trap: the agent's own capture
// moves the tracked branch, so without the merge-base override a second edit
// to the same file reads as "the repository moved too" and false-conflicts.
func TestTheCaptureCommitBecomesTheMergeBaseForTheNextCycle(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.git.sha = "tip1"
	fakes.applier.state = applier.State{Manifest: []string{"a.yaml"}, LastGoodSHA: "base1"}
	fakes.differ.changes = []differ.Change{{Path: "a.yaml", Kind: "update"}}

	r := fakes.reconciler(captureOpts())
	r.runCycle(context.Background())

	if got := capturedPaths(fakes.git); !slices.Equal(got, []string{"a.yaml"}) {
		t.Fatalf("cycle 1 captured = %v, want [a.yaml]", got)
	}
	state := fakes.applier.state
	if state.LastCaptureSHA != "capture1" || !slices.Equal(state.LastCapturePaths, []string{"a.yaml"}) {
		t.Fatalf("after cycle 1: LastCaptureSHA = %q, LastCapturePaths = %v", state.LastCaptureSHA, state.LastCapturePaths)
	}

	// Cycle 2: the branch has moved to the capture commit, the user edits the
	// same file again, and a.yaml HAS moved since base1 - because of our own
	// capture - but not since capture1.
	fakes.git.sha = "tip2"
	fakes.git.changedBetweenByBase = map[string][]string{
		"base1":    {"a.yaml"},
		"capture1": nil,
	}

	r.runCycle(context.Background())

	if len(fakes.git.parkCalls) != 0 {
		t.Errorf("ParkConflicts called %d time(s): the agent's own capture was read as a repository change", len(fakes.git.parkCalls))
	}
	if got := r.Status().Conflicts; len(got) != 0 {
		t.Errorf("conflicts = %+v, want none: editing the same file twice is not a conflict", got)
	}
	if got := capturedPaths(fakes.git); !slices.Equal(got, []string{"a.yaml", "a.yaml"}) {
		t.Errorf("captured across both cycles = %v, want a.yaml twice", got)
	}
}

// The override's whole reason for existing: it must survive a cycle whose
// apply failed, since that is exactly when LastGoodSHA does not advance.
func TestAFailedApplyAfterASuccessfulCaptureStillLeavesTheCaptureAsTheBase(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.git.sha = "tip1"
	fakes.applier.state = applier.State{Manifest: []string{"a.yaml", "b.yaml"}, LastGoodSHA: "base1"}
	fakes.differ.changes = []differ.Change{
		{Path: "a.yaml", Kind: "update"},
		{Path: "b.yaml", Kind: "update"},
	}
	fakes.git.changedBetween = []string{"b.yaml"}
	fakes.git.liveFacts = map[string]gitsync.LiveFacts{"b.yaml": {BaseTracks: true, MatchesBase: true}}
	// The apply of b.yaml fails; the capture of a.yaml already succeeded.
	fakes.applier.applyResult = applier.Result{OK: false, Error: "check_config rejected the change"}

	r := fakes.reconciler(captureOpts())
	r.runCycle(context.Background())

	state := fakes.applier.state
	if state.LastCaptureSHA != "capture1" {
		t.Fatalf("LastCaptureSHA = %q, want it kept across a failed apply", state.LastCaptureSHA)
	}
	if !slices.Equal(state.LastCapturePaths, []string{"a.yaml"}) {
		t.Fatalf("LastCapturePaths = %v, want [a.yaml]", state.LastCapturePaths)
	}
	if state.LastGoodSHA != "base1" {
		t.Errorf("LastGoodSHA = %q, want it unmoved by a failed apply", state.LastGoodSHA)
	}

	// And the next cycle still reads a.yaml against the capture commit.
	fakes.git.sha = "tip2"
	fakes.git.changedBetweenByBase = map[string][]string{"base1": {"a.yaml", "b.yaml"}, "capture1": nil}
	fakes.differ.changes = []differ.Change{{Path: "a.yaml", Kind: "update"}}
	r.runCycle(context.Background())

	if len(fakes.git.parkCalls) != 0 {
		t.Errorf("ParkConflicts called after a failed apply, want the override to have survived")
	}
}

// A conflict is by definition something only a person clears, so it is still
// here next cycle and every cycle after. Re-parking each time would push a
// new gitops/conflict-<second> branch to the user's remote every interval
// forever and repeat the same feed line until it had evicted everything else.
func TestAStandingConflictIsParkedAndLoggedOnlyOnce(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.git.sha = "tip1"
	fakes.applier.state = applier.State{Manifest: []string{"a.yaml"}, LastGoodSHA: "base1"}
	fakes.differ.changes = []differ.Change{{Path: "a.yaml", Kind: "update"}}
	fakes.git.changedBetween = []string{"a.yaml"}
	fakes.git.liveFacts = map[string]gitsync.LiveFacts{"a.yaml": {BaseTracks: true, MatchesBase: false}}

	r := fakes.reconciler(captureOpts())
	r.runCycle(context.Background())
	r.runCycle(context.Background())
	r.runCycle(context.Background())

	if got := len(fakes.git.parkCalls); got != 1 {
		t.Errorf("ParkConflicts called %d time(s) across three cycles, want 1", got)
	}
	if got := countCaptureEvents(r.Status().Events, "conflict on 1 path(s)"); got != 1 {
		t.Errorf("the conflict was logged %d time(s), want 1", got)
	}
	// It is still a conflict, though - the dedup must not clear the record.
	if got := r.Status().Conflicts; !slices.Equal(got, []string{"a.yaml"}) {
		t.Errorf("conflicts = %v, want a.yaml still standing", got)
	}
	if got := appliedPaths(fakes.applier); len(got) != 0 {
		t.Errorf("applied = %v, want the conflicted path still refused on every cycle", got)
	}
}

// A conflict set that GROWS is a new fact and must be parked again, or the
// second path's live copy is never preserved anywhere.
func TestAChangedConflictSetIsParkedAgain(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.git.sha = "tip1"
	fakes.applier.state = applier.State{Manifest: []string{"a.yaml", "b.yaml"}, LastGoodSHA: "base1"}
	fakes.differ.changes = []differ.Change{{Path: "a.yaml", Kind: "update"}}
	fakes.git.changedBetween = []string{"a.yaml", "b.yaml"}
	fakes.git.liveFacts = map[string]gitsync.LiveFacts{
		"a.yaml": {BaseTracks: true, MatchesBase: false},
		"b.yaml": {BaseTracks: true, MatchesBase: false},
	}

	r := fakes.reconciler(captureOpts())
	r.runCycle(context.Background())

	fakes.differ.changes = []differ.Change{
		{Path: "a.yaml", Kind: "update"},
		{Path: "b.yaml", Kind: "update"},
	}
	r.runCycle(context.Background())

	if got := len(fakes.git.parkCalls); got != 2 {
		t.Fatalf("ParkConflicts called %d time(s), want 2: the conflict set changed", got)
	}
	if got := len(fakes.git.parkCalls[1].files); got != 2 {
		t.Errorf("second park carried %d file(s), want both", got)
	}
}

// The option is the only writer of the conflict record, so turning it off has
// to clear one - otherwise those paths are excluded from every future apply
// with nothing left that could ever release them.
func TestTurningCaptureOffClearsAStandingConflict(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.git.sha = "tip1"
	fakes.applier.state = applier.State{
		Manifest: []string{"a.yaml"}, LastGoodSHA: "base1", ConflictedPaths: []string{"a.yaml"},
	}
	fakes.differ.changes = []differ.Change{{Path: "a.yaml", Kind: "update"}}

	opts := baseOpts()
	opts.DryRun = false
	opts.CaptureLiveChanges = false
	r := fakes.reconciler(opts)
	if len(r.Status().Conflicts) != 1 {
		t.Fatal("the fixture should start with a standing conflict")
	}

	r.runCycle(context.Background())

	if got := r.Status().Conflicts; len(got) != 0 {
		t.Errorf("conflicts = %v, want them cleared once the feature is off", got)
	}
	if got := appliedPaths(fakes.applier); !slices.Equal(got, []string{"a.yaml"}) {
		t.Errorf("applied = %v, want the path syncing normally again", got)
	}
}

// The ordinary "the repository moved, apply it" cycle changes nothing the
// capture phase owns, and rewriting the largest file the agent has on
// flash-backed hardware every interval buys nothing.
func TestAnApplyOnlyCycleDoesNotRewriteStateFromTheCapturePhase(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.git.sha = "tip1"
	fakes.applier.state = applier.State{Manifest: []string{"a.yaml"}, LastGoodSHA: "base1"}
	fakes.differ.changes = []differ.Change{{Path: "a.yaml", Kind: "update"}}
	fakes.git.changedBetween = []string{"a.yaml"}
	fakes.git.liveFacts = map[string]gitsync.LiveFacts{"a.yaml": {BaseTracks: true, MatchesBase: true}}

	r := fakes.reconciler(captureOpts())
	r.runCycle(context.Background())

	// Exactly one save, the apply's own - the capture phase adds none.
	if got := len(fakes.applier.stateSaveCalls); got != 1 {
		t.Errorf("StateSave called %d time(s), want 1 (the apply's)", got)
	}
}

func countCaptureEvents(events []Event, sub string) int {
	n := 0
	for _, e := range events {
		if strings.Contains(e.Message, sub) {
			n++
		}
	}
	return n
}
