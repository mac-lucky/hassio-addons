package recon

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/applier"
)

// stubLive makes liveRegularFile answer from live for the rest of the test.
func stubLive(t *testing.T, live map[string]bool) {
	t.Helper()
	orig := liveRegularFile
	liveRegularFile = func(p string) bool { return live[p] }
	t.Cleanup(func() { liveRegularFile = orig })
}

func leftBehindState(lastGood, lastImport string, manifest ...string) applier.State {
	if manifest == nil {
		manifest = []string{}
	}
	return applier.State{
		LastGoodSHA: lastGood, LastImportSHA: lastImport,
		Manifest: manifest, RegistryManaged: map[string]string{},
	}
}

func TestLeftBehindPaths(t *testing.T) {
	// "a/old.py" twice: two bases can both track it.
	prev := []string{"z.yaml", "kept.yaml", "owned.yaml", "gone.yaml", "a/old.py", "a/old.py"}
	tracked := []string{"kept.yaml", "a/new.py"}
	manifest := []string{"owned.yaml"}
	live := map[string]bool{"z.yaml": true, "kept.yaml": true, "owned.yaml": true, "a/old.py": true}

	got := leftBehindPaths(prev, tracked, manifest, func(p string) bool { return live[p] })

	// kept.yaml is still tracked, owned.yaml gets a planned delete, gone.yaml
	// is not live: none of them is left behind.
	want := []string{"a/old.py", "z.yaml"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("leftBehindPaths = %v, want %v", got, want)
	}
}

func TestReconcileNowWarnsOnceAboutUnownedFileLeftLive(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.git.sha = "new"
	fakes.git.tracked = []string{"automations.yaml", "pyscript/intrusion.py"}
	fakes.git.trackedAt = map[string][]string{
		"old": {"automations.yaml", "pyscript/intrusion_and_leaks.py"},
	}
	fakes.applier.state = leftBehindState("old", "")
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
	fakes.git.trackedAtErr = map[string]error{"old": errors.New("fatal: not a tree object")}
	r.ReconcileNow(context.Background())
	fakes.git.trackedAtErr = nil
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
	fakes := newReconcilerFakes()
	fakes.git.sha = "new"
	fakes.git.trackedAt = map[string][]string{"old": {"automations.yaml", "a.yaml", "b.yaml"}}
	fakes.applier.state = leftBehindState("old", "")
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
// it brought in are only visible in LastImportSHA's tree.
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
			fakes := newReconcilerFakes()
			fakes.git.sha = "new"
			fakes.git.tracked = []string{"automations.yaml", "scripts/renamed.yaml"}
			fakes.git.trackedAt = map[string][]string{
				"applied": {"automations.yaml"},
				"imp":     {"automations.yaml", "scripts/imported.yaml"},
			}
			fakes.applier.state = leftBehindState(tc.lastGood, "imp")
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
		name       string
		lastGood   string
		manifest   []string
		trackedErr error
		reachable  map[string]bool
		ancestorOf map[string]bool
	}{
		// trackedAt[""] holds old.yaml, so this fails if the empty SHA is read.
		{name: "no apply or import yet", lastGood: ""},
		{name: "agent owns the file, so the apply deletes it", lastGood: "old", manifest: []string{"old.yaml"}},
		{name: "base unreadable", lastGood: "old", trackedErr: errors.New("fatal: not a tree object")},
		{name: "base gone from the clone", lastGood: "old", reachable: map[string]bool{"new": true}},
		{name: "base off this branch (force-push)", lastGood: "old", ancestorOf: map[string]bool{"new->new": true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fakes := newReconcilerFakes()
			fakes.git.sha = "new"
			fakes.git.tracked = []string{"automations.yaml"}
			fakes.git.trackedAt = map[string][]string{
				"old": {"automations.yaml", "old.yaml"},
				"":    {"automations.yaml", "old.yaml"},
			}
			if tc.trackedErr != nil {
				fakes.git.trackedAtErr = map[string]error{"old": tc.trackedErr}
			}
			fakes.git.commitReachable = tc.reachable
			fakes.git.ancestorOf = tc.ancestorOf
			fakes.applier.state = leftBehindState(tc.lastGood, "", tc.manifest...)
			stubLive(t, map[string]bool{"automations.yaml": true, "old.yaml": true})
			r := fakes.reconciler(baseOpts())

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
	fakes := newReconcilerFakes()
	fakes.git.sha = "new"
	old := []string{"automations.yaml"}
	live := map[string]bool{}
	for i := range 25 {
		p := fmt.Sprintf("www/community/card%02d.js", i)
		old = append(old, p)
		live[p] = true
	}
	fakes.git.trackedAt = map[string][]string{"old": old}
	fakes.applier.state = leftBehindState("old", "")
	stubLive(t, live)
	r := fakes.reconciler(baseOpts())

	r.ReconcileNow(context.Background())

	const want = "left in place: 25 file(s)"
	var msg string
	for _, e := range r.Status().Events {
		if strings.Contains(e.Message, want) {
			msg = e.Message
		}
	}
	if msg == "" {
		t.Fatalf("no %q event; events = %+v", want, r.Status().Events)
	}
	if !strings.HasSuffix(msg, "www/community/card19.js and 5 more (full list in the add-on log)") ||
		strings.Contains(msg, "card20.js") {
		t.Errorf("event = %q, want the first 20 paths and \"and 5 more\"", msg)
	}
}

func TestReconcileNowSkipsLeftBehindCheckWithYAMLFilesOff(t *testing.T) {
	fakes := newReconcilerFakes()
	fakes.git.sha = "new"
	fakes.git.trackedAt = map[string][]string{"old": {"automations.yaml", "old.yaml"}}
	fakes.applier.state = leftBehindState("old", "")
	stubLive(t, map[string]bool{"old.yaml": true})
	opts := baseOpts()
	opts.ReconcileYAMLFiles = false
	r := fakes.reconciler(opts)

	r.ReconcileNow(context.Background())

	if hasEventContaining(r.Status().Events, "left in place:") {
		t.Errorf("left-in-place event with yaml_files off, which never deletes a file anyway; events = %+v", r.Status().Events)
	}
}
