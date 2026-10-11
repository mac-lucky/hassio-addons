package recon

import (
	"context"
	"strings"
	"testing"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/gitsync"
)

// An entry naming a file the repository does not track does nothing - a
// .yml written for a .yaml, or a file since removed - and says so once per
// set, naming the file and never the key.
func TestReconcileNowWarnsOnceAboutAnIgnoreKeysFileTheRepositoryDoesNotTrack(t *testing.T) {
	if err := gitsync.SetIgnoredKeys([]string{
		"zigbee2mqtt/configuration.yml:mqtt.password",
		"zigbee2mqtt/configuration.yaml:mqtt.user",
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = gitsync.SetIgnoredKeys(nil) })

	fakes := newReconcilerFakes()
	fakes.git.tracked = []string{"automations.yaml", "zigbee2mqtt/configuration.yaml"}
	r := fakes.reconciler(baseOpts())
	const want = "ignore_keys names 1 file(s) the repository does not track, so those entries do nothing: zigbee2mqtt/configuration.yml"

	r.ReconcileNow(context.Background())
	r.ReconcileNow(context.Background())
	if got := countEventsContaining(r.Status().Events, want); got != 1 {
		t.Fatalf("%d events %q, want exactly 1 across two cycles; events = %+v", got, want, r.Status().Events)
	}
	for _, e := range r.Status().Events {
		if strings.Contains(e.Message, "mqtt.password") {
			t.Errorf("event %q names a key, want files only", e.Message)
		}
	}

	// Tracked after all: the set empties quietly; untracked again, it is
	// worth another event.
	fakes.git.tracked = append(fakes.git.tracked, "zigbee2mqtt/configuration.yml")
	r.ReconcileNow(context.Background())
	fakes.git.tracked = fakes.git.tracked[:2]
	r.ReconcileNow(context.Background())
	if got := countEventsContaining(r.Status().Events, "the repository does not track"); got != 2 {
		t.Errorf("%d events, want 2 (the set, then the same set again after it emptied)", got)
	}
}
