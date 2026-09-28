package recon

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// stubLive makes liveRegularFile answer from live for the rest of the test.
func stubLive(t *testing.T, live map[string]bool) {
	t.Helper()
	orig := liveRegularFile
	liveRegularFile = func(p string) bool { return live[p] }
	t.Cleanup(func() { liveRegularFile = orig })
}

// leftBehindFakes is the fixture the reconcile tests here share: the tip
// "new" tracks tracked, and the applier state names lastGood and
// lastImport. What moved since each base goes in changedBetweenByBase.
func leftBehindFakes(lastGood, lastImport string, tracked ...string) *reconcilerFakes {
	fakes := newReconcilerFakes()
	fakes.git.sha = "new"
	fakes.git.tracked = tracked
	fakes.applier.state.LastGoodSHA = lastGood
	fakes.applier.state.LastImportSHA = lastImport
	return fakes
}

func TestLeftBehindPaths(t *testing.T) {
	changed := []string{".storage/core.config_entries", "a/new.py", "a/old.py", "gone.yaml", "kept.yaml", "owned.yaml", "z.yaml"}
	tracked := []string{"kept.yaml", "a/new.py"}
	manifest := []string{"owned.yaml"}
	stubLive(t, map[string]bool{
		".storage/core.config_entries": true, "a/new.py": true, "a/old.py": true,
		"kept.yaml": true, "owned.yaml": true, "z.yaml": true,
	})

	got := leftBehindPaths(changed, tracked, manifest)

	// a/new.py and kept.yaml are still tracked, owned.yaml gets a planned
	// delete, gone.yaml is not live, and .storage/ is excluded, so never
	// tracked to begin with: none of them is left behind.
	want := []string{"a/old.py", "z.yaml"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("leftBehindPaths = %v, want %v", got, want)
	}
}

func TestReconcileNowWarnsOnceAboutUnownedFileLeftLive(t *testing.T) {
	fakes := leftBehindFakes("old", "", "automations.yaml", "pyscript/intrusion.py")
	fakes.git.changedBetweenByBase = map[string][]string{
		"old": {"pyscript/intrusion.py", "pyscript/intrusion_and_leaks.py"},
	}
	live := map[string]bool{"automations.yaml": true, "pyscript/intrusion_and_leaks.py": true}
	stubLive(t, live)
	r := fakes.reconciler(baseOpts())
	const want = "left in place: 1 file(s) removed from the repository stay live because this agent never wrote them - delete by hand if unwanted: pyscript/intrusion_and_leaks.py"

	r.ReconcileNow(context.Background())
	r.ReconcileNow(context.Background())

	if got := countEventsContaining(r.Status().Events, want); got != 1 {
		t.Fatalf("%d events %q, want exactly 1 across two cycles; events = %+v", got, want, r.Status().Events)
	}
	if got := r.Status().State; got != StateInSync {
		t.Errorf("state = %q, want in_sync: the warning is informational", got)
	}

	// A base that cannot be read keeps the remembered set, so recovering
	// from it is not a new set.
	fakes.git.changedBetweenErr = errors.New("fatal: bad object old")
	r.ReconcileNow(context.Background())
	fakes.git.changedBetweenErr = nil
	r.ReconcileNow(context.Background())
	if got := countEventsContaining(r.Status().Events, "left in place:"); got != 1 {
		t.Fatalf("%d left-in-place events after a failed read, want still 1", got)
	}

	// Deleted by hand: the set empties quietly, and a later return of the
	// same file is a new set worth another event.
	live["pyscript/intrusion_and_leaks.py"] = false
	r.ReconcileNow(context.Background())
	live["pyscript/intrusion_and_leaks.py"] = true
	r.ReconcileNow(context.Background())

	if got := countEventsContaining(r.Status().Events, "left in place:"); got != 2 {
		t.Errorf("%d left-in-place events, want 2 (first set, then the same set again after it emptied)", got)
	}
}

func TestReconcileNowWarnsAgainWhenLeftBehindSetGrows(t *testing.T) {
	fakes := leftBehindFakes("old", "", "automations.yaml")
	fakes.git.changedBetweenByBase = map[string][]string{"old": {"a.yaml", "b.yaml"}}
	live := map[string]bool{"a.yaml": true}
	stubLive(t, live)
	r := fakes.reconciler(baseOpts())

	r.ReconcileNow(context.Background())
	live["b.yaml"] = true
	r.ReconcileNow(context.Background())

	if !hasEventContaining(r.Status().Events, "left in place: 1 file(s)") ||
		!hasEventContaining(r.Status().Events, "left in place: 2 file(s)") {
		t.Errorf("want one event per set, {a} then {a,b}; events = %+v", r.Status().Events)
	}
}

// An import never moves LastGoodSHA and usually plans nothing, so the files
// it brought in are only visible against LastImportSHA.
func TestReconcileNowWarnsAboutImportedFileLeftLive(t *testing.T) {
	cases := []struct {
		name     string
		lastGood string
	}{
		{name: "import-seeded, never applied", lastGood: ""},
		{name: "applied before the import", lastGood: "applied"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fakes := leftBehindFakes(tc.lastGood, "imp", "automations.yaml", "scripts/renamed.yaml")
			fakes.git.changedBetweenByBase = map[string][]string{
				"applied": {"scripts/renamed.yaml"},
				"imp":     {"scripts/imported.yaml", "scripts/renamed.yaml"},
			}
			stubLive(t, map[string]bool{"automations.yaml": true, "scripts/imported.yaml": true})
			r := fakes.reconciler(baseOpts())

			r.ReconcileNow(context.Background())

			if !hasEventContaining(r.Status().Events, "left in place: 1 file(s)") ||
				!hasEventContaining(r.Status().Events, "scripts/imported.yaml") {
				t.Errorf("no left-in-place event for the imported file; events = %+v", r.Status().Events)
			}
		})
	}
}

func TestReconcileNowNoLeftBehindWarning(t *testing.T) {
	cases := []struct {
		name         string
		lastGood     string
		manifest     []string
		changedErr   error
		reachable    map[string]bool
		ancestorOf   map[string]bool
		yamlFilesOff bool
	}{
		// changedBetweenByBase[""] names old.yaml, so this fails if the
		// empty SHA is read.
		{name: "no apply or import yet", lastGood: ""},
		{name: "agent owns the file, so the apply deletes it", lastGood: "old", manifest: []string{"old.yaml"}},
		{name: "base unreadable", lastGood: "old", changedErr: errors.New("fatal: bad object old")},
		{name: "base gone from the clone", lastGood: "old", reachable: map[string]bool{"new": true}},
		{name: "base off this branch (force-push)", lastGood: "old", ancestorOf: map[string]bool{"new->new": true}},
		{name: "yaml_files off, which never deletes a file anyway", lastGood: "old", yamlFilesOff: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fakes := leftBehindFakes(tc.lastGood, "", "automations.yaml")
			fakes.git.changedBetweenByBase = map[string][]string{"old": {"old.yaml"}, "": {"old.yaml"}}
			fakes.git.changedBetweenErr = tc.changedErr
			fakes.git.commitReachable = tc.reachable
			fakes.git.ancestorOf = tc.ancestorOf
			if tc.manifest != nil {
				fakes.applier.state.Manifest = tc.manifest
			}
			stubLive(t, map[string]bool{"automations.yaml": true, "old.yaml": true})
			opts := baseOpts()
			opts.ReconcileYAMLFiles = !tc.yamlFilesOff
			r := fakes.reconciler(opts)

			r.ReconcileNow(context.Background())

			if hasEventContaining(r.Status().Events, "left in place:") {
				t.Errorf("unexpected left-in-place event; events = %+v", r.Status().Events)
			}
			if got := r.Status().State; got != StateInSync {
				t.Errorf("state = %q, want in_sync", got)
			}
		})
	}
}

func TestReconcileNowLeftBehindEventListsAtMost20Paths(t *testing.T) {
	fakes := leftBehindFakes("old", "", "automations.yaml")
	var removed []string
	live := map[string]bool{}
	for i := range 25 {
		p := fmt.Sprintf("www/community/card%02d.js", i)
		removed = append(removed, p)
		live[p] = true
	}
	fakes.git.changedBetweenByBase = map[string][]string{"old": removed}
	stubLive(t, live)
	r := fakes.reconciler(baseOpts())

	r.ReconcileNow(context.Background())

	msg := eventContaining(t, r.Status().Events, "left in place: 25 file(s)")
	if !strings.HasSuffix(msg, "www/community/card19.js and 5 more (full list in the add-on log)") ||
		strings.Contains(msg, "card20.js") {
		t.Errorf("event = %q, want the first 20 paths and \"and 5 more\"", msg)
	}
}
