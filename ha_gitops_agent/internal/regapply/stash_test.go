package regapply

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// --- cross-version stash compatibility ---------------------------------------
//
// A stash under /data that a newer binary cannot decode is a rollback that
// silently reverts nothing. The literals are hand-pasted on purpose: a
// regenerated fixture follows a format change and still passes, so a
// failure here is the finding, not something to fix by editing them.

const (
	registryStashFixture = `{"ops":[{"kind":"create","rtype":"floor","key":"ground","live_id":"F1","live_object":{"level":0,"name":"Ground"},"forward_params":{"name":"Ground Floor"}},{"kind":"update","rtype":"entity","key":"light.kitchen","live_id":"light.kitchen","live_object":{"name":"Kitchen"},"forward_params":{"name":"Kitchen Light"},"entity_originals_existed":true,"entity_originals_snapshot":{"icon":"mdi:bulb","name":null}},{"kind":"create","rtype":"area","key":"kitchen","live_id":"A1","live_object":null,"forward_params":null}]}`

	addonStashFixture = `{"ops":[{"kind":"update","slug":"core_configurator","prior_options":{"logins":"__absent__","port":1883},"forward_options":{"port":8883},"restart_on_change":true,"originals_existed_before":true,"originals_snapshot_before":{"port":1883},"restart_map_existed_before":true,"prior_restart_on_change_before":false},{"kind":"restore","slug":"a0d7b954_nodered","prior_options":null,"forward_options":null,"restart_on_change":false,"originals_existed_before":false,"restart_map_existed_before":false,"prior_restart_on_change_before":false}]}`

	integrationStashFixture = `{"ops":[{"kind":"create","key":"hue","domain":"hue","title":"Philips Hue","entry_id":"abc123","data":{"host":"10.0.0.5"}},{"kind":"delete","key":"mqtt","domain":"mqtt","title":"","entry_id":"def456","data":null}]}`
)

// registryStashFixtureEntries etc. are what each fixture must decode to.
// Every field is set in one entry and left nil in another, so a dropped
// field or a new omitempty cannot pass as "zero was expected anyway".
func registryStashFixtureEntries() []stashEntry {
	return []stashEntry{
		{
			Kind: "create", RType: "floor", Key: "ground", LiveID: "F1",
			PriorObject:   map[string]any{"name": "Ground", "level": float64(0)},
			ForwardParams: map[string]any{"name": "Ground Floor"},
		},
		{
			Kind: "update", RType: "entity", Key: "light.kitchen", LiveID: "light.kitchen",
			PriorObject:       map[string]any{"name": "Kitchen"},
			ForwardParams:     map[string]any{"name": "Kitchen Light"},
			OriginalsExisted:  true,
			OriginalsSnapshot: map[string]any{"name": nil, "icon": "mdi:bulb"},
		},
		// A fresh create leaves PriorObject and ForwardParams nil; with no
		// omitempty on their tags they must serialize as explicit nulls.
		{Kind: "create", RType: "area", Key: "kitchen", LiveID: "A1"},
	}
}

func addonStashFixtureEntries() []addonStashEntry {
	return []addonStashEntry{
		{
			Kind: "update", Slug: "core_configurator",
			PriorOptions:               map[string]any{"logins": "__absent__", "port": float64(1883)},
			ForwardOptions:             map[string]any{"port": float64(8883)},
			RestartOnChange:            true,
			OriginalsExistedBefore:     true,
			OriginalsSnapshotBefore:    map[string]any{"port": float64(1883)},
			RestartMapExistedBefore:    true,
			PriorRestartOnChangeBefore: false,
		},
		{Kind: "restore", Slug: "a0d7b954_nodered"},
	}
}

func integrationStashFixtureEntries() []integrationStashEntry {
	return []integrationStashEntry{
		{
			Kind: "create", Key: "hue", Domain: "hue", Title: "Philips Hue", EntryID: "abc123",
			Data: map[string]any{"host": "10.0.0.5"},
		},
		{Kind: "delete", Key: "mqtt", Domain: "mqtt", Title: "", EntryID: "def456"},
	}
}

func writeStashFixture(t *testing.T, stashDir, filename, fixture string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(stashDir, filename), []byte(fixture), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRegistryStashFixtureStillLoads(t *testing.T) {
	stashDir := t.TempDir()
	writeStashFixture(t, stashDir, registryStashFile, registryStashFixture)

	got, err := readRegistryStashTolerant(stashDir)
	if err != nil {
		t.Fatalf("readRegistryStashTolerant: %v", err)
	}
	if want := registryStashFixtureEntries(); !reflect.DeepEqual(got, want) {
		t.Errorf("a stash written by the previous version no longer loads\n got %+v\nwant %+v", got, want)
	}
}

func TestAddonStashFixtureStillLoads(t *testing.T) {
	stashDir := t.TempDir()
	writeStashFixture(t, stashDir, addonStashFile, addonStashFixture)

	got, err := readAddonStash(stashDir)
	if err != nil {
		t.Fatalf("readAddonStash: %v", err)
	}
	if want := addonStashFixtureEntries(); !reflect.DeepEqual(got, want) {
		t.Errorf("a stash written by the previous version no longer loads\n got %+v\nwant %+v", got, want)
	}
}

func TestIntegrationStashFixtureStillLoads(t *testing.T) {
	stashDir := t.TempDir()
	writeStashFixture(t, stashDir, integrationStashFile, integrationStashFixture)

	got, err := readIntegrationStash(stashDir)
	if err != nil {
		t.Fatalf("readIntegrationStash: %v", err)
	}
	if want := integrationStashFixtureEntries(); !reflect.DeepEqual(got, want) {
		t.Errorf("a stash written by the previous version no longer loads\n got %+v\nwant %+v", got, want)
	}
}

// An OLDER binary must still read what this version writes, so the
// encoding is pinned down to key order and omitempty, not just round-trip.
func TestStashWritersReproduceFixtureBytes(t *testing.T) {
	stashDir := t.TempDir()

	if err := writeRegistryStashReal(stashDir, registryStashFixtureEntries()); err != nil {
		t.Fatal(err)
	}
	if err := writeAddonStashReal(stashDir, addonStashFixtureEntries()); err != nil {
		t.Fatal(err)
	}
	if err := writeIntegrationStashReal(stashDir, integrationStashFixtureEntries()); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct{ filename, fixture string }{
		{registryStashFile, registryStashFixture},
		{addonStashFile, addonStashFixture},
		{integrationStashFile, integrationStashFixture},
	} {
		data, err := os.ReadFile(filepath.Join(stashDir, tc.filename))
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != tc.fixture {
			t.Errorf("%s encoding changed - a previous version can no longer read it\n got %s\nwant %s",
				tc.filename, data, tc.fixture)
		}
	}
}

// A one-layer apply leaves the other two stashes uncreated, so a missing
// file must read as no entries - a nil slice, not an empty non-nil one.
func TestReadStashFileMissingIsNotAnError(t *testing.T) {
	stashDir := t.TempDir()

	regEntries, err := readRegistryStashTolerant(stashDir)
	if err != nil || regEntries != nil {
		t.Errorf("readRegistryStashTolerant = %v, %v; want nil, nil", regEntries, err)
	}
	addonEntries, err := readAddonStash(stashDir)
	if err != nil || addonEntries != nil {
		t.Errorf("readAddonStash = %v, %v; want nil, nil", addonEntries, err)
	}
	integEntries, err := readIntegrationStash(stashDir)
	if err != nil || integEntries != nil {
		t.Errorf("readIntegrationStash = %v, %v; want nil, nil", integEntries, err)
	}
}
