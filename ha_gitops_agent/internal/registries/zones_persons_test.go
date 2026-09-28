package registries

import (
	"reflect"
	"strings"
	"testing"
)

// loadHelpersError writes helpers.yaml, loads it, and returns the error
// message, failing the test when it loaded.
func loadHelpersError(t *testing.T, content string) string {
	t.Helper()
	workdir, gitops := mkGitops(t)
	writeFile(t, gitops, "helpers.yaml", content)
	_, err := LoadManifests(workdir)
	if err == nil {
		t.Fatal("manifest loaded, want an error")
	}
	return err.Error()
}

func wantAllIn(t *testing.T, message string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(message, want) {
			t.Errorf("err = %q, want it to contain %q", message, want)
		}
	}
}

// --- zone: load and validate ---------------------------------------------

func TestZoneManifestLoads(t *testing.T) {
	workdir, gitops := mkGitops(t)
	writeFile(t, gitops, "helpers.yaml", `
zone:
  - id: office
    name: Office
    latitude: 52.2297
    longitude: 21.0122
    radius: 150
    passive: false
    icon: mdi:briefcase
`)

	desired, err := LoadManifests(workdir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []map[string]any{{
		"id": "office", "name": "Office", "latitude": 52.2297, "longitude": 21.0122,
		"radius": 150, "passive": false, "icon": "mdi:briefcase",
	}}
	if !reflect.DeepEqual(desired.Helpers["zone"], want) {
		t.Errorf("zone = %+v, want %+v", desired.Helpers["zone"], want)
	}
}

func TestZoneRequiresNumericCoordinatesAndAPositiveRadius(t *testing.T) {
	message := loadHelpersError(t, `
zone:
  - id: no_lat
    name: No latitude
    longitude: 21.0
  - id: quoted
    name: Quoted
    latitude: "52.2"
    longitude: 21.0
  - id: off_the_map
    name: Off the map
    latitude: 91
    longitude: -181
  - id: zero_radius
    name: Zero radius
    latitude: 52.2
    longitude: 21.0
    radius: 0
  - id: text_radius
    name: Text radius
    latitude: 52.2
    longitude: 21.0
    radius: "100"
`)

	wantAllIn(t, message,
		"helpers.yaml: zone 'no_lat' is missing required field 'latitude'",
		"helpers.yaml: zone 'quoted' field 'latitude' must be a number between -90 and 90 (a quoted number reads as text)",
		"helpers.yaml: zone 'off_the_map' field 'latitude' must be a number between -90 and 90",
		"helpers.yaml: zone 'off_the_map' field 'longitude' must be a number between -180 and 180",
		"helpers.yaml: zone 'zero_radius' field 'radius' must be a positive number of meters",
		"helpers.yaml: zone 'text_radius' field 'radius' must be a positive number of meters",
	)
	if strings.Contains(message, "'no_lat' field 'longitude'") || strings.Contains(message, "'quoted' field 'longitude'") {
		t.Errorf("err = %q, flagged a valid longitude", message)
	}
}

// A zone update merges, so a null left out of it never clears anything:
// the stored value stays and the zone is re-applied on every cycle.
func TestZoneNullIsRefusedOnEveryField(t *testing.T) {
	message := loadHelpersError(t, `
zone:
  - id: office
    name: Office
    latitude: null
    longitude: 21.0
    radius: null
    passive: null
    icon: null
`)

	wantAllIn(t, message,
		"helpers.yaml: zone 'office' field 'icon' cannot be null - a zone update is merged into the stored zone",
		"helpers.yaml: zone 'office' field 'latitude' cannot be null",
		"helpers.yaml: zone 'office' field 'radius' cannot be null",
		"helpers.yaml: zone 'office' field 'passive' cannot be null",
	)
	// One message per field: a null coordinate is not also "not a number".
	for _, field := range []string{"latitude", "radius", "passive"} {
		if n := strings.Count(message, "'office' field '"+field+"'"); n != 1 {
			t.Errorf("field %s reported %d times in %q, want once", field, n, message)
		}
	}
}

func TestZonePassiveMustBeABoolean(t *testing.T) {
	message := loadHelpersError(t, `
zone:
  - id: office
    name: Office
    latitude: 52.2
    longitude: 21.0
    passive: yes
`)

	wantAllIn(t, message, "helpers.yaml: zone 'office' field 'passive' must be true or false")
}

// --- person: load and validate -------------------------------------------

func TestPersonManifestLoads(t *testing.T) {
	workdir, gitops := mkGitops(t)
	writeFile(t, gitops, "helpers.yaml", `
person:
  - id: anna
    name: Anna
    device_trackers:
      - device_tracker.anna_phone
      - device_tracker.anna_watch
  - id: guest
    name: Guest
`)

	desired, err := LoadManifests(workdir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []map[string]any{
		{"id": "anna", "name": "Anna", "device_trackers": []any{"device_tracker.anna_phone", "device_tracker.anna_watch"}},
		{"id": "guest", "name": "Guest"},
	}
	if !reflect.DeepEqual(desired.Helpers["person"], want) {
		t.Errorf("person = %+v, want %+v", desired.Helpers["person"], want)
	}
}

func TestPersonUserIDAndPictureAreRefusedAsNotPortable(t *testing.T) {
	message := loadHelpersError(t, `
person:
  - id: anna
    name: Anna
    user_id: 0d3f4a1b2c
    picture: /api/image/serve/abc/512x512
    icon: mdi:account
`)

	wantAllIn(t, message,
		"helpers.yaml: person 'anna' field 'user_id' cannot be managed here - a user id is a random id",
		"helpers.yaml: person 'anna' field 'picture' cannot be managed here - a picture is the URL of an image uploaded",
		"the agent keeps whatever is set there",
		"helpers.yaml: person 'anna' has unsupported field(s) icon (a person takes name and device_trackers)",
	)
	if strings.Contains(message, "unsupported field(s) icon, picture") || strings.Contains(message, "unsupported field(s) icon, user_id") {
		t.Errorf("err = %q, lists user_id/picture as merely unsupported too", message)
	}
}

// A null user_id reads like "unlink the user"; the answer is the same.
func TestPersonNullUserIDGetsOnlyTheNotPortableMessage(t *testing.T) {
	message := loadHelpersError(t, `
person:
  - id: anna
    name: Anna
    user_id: null
`)

	wantAllIn(t, message, "person 'anna' field 'user_id' cannot be managed here")
	if strings.Contains(message, "cannot be null") {
		t.Errorf("err = %q, want only the not-portable message", message)
	}
}

func TestPersonDeviceTrackersMustBeAListOfDeviceTrackerIDs(t *testing.T) {
	message := loadHelpersError(t, `
person:
  - id: bare
    name: Bare
    device_trackers: device_tracker.phone
  - id: mixed
    name: Mixed
    device_trackers:
      - device_tracker.phone
      - sensor.phone_battery
      - device_tracker.Phone
      - 5
`)

	wantAllIn(t, message,
		"helpers.yaml: person 'bare' field 'device_trackers' must be a list of device_tracker entity ids",
		"helpers.yaml: person 'mixed' device_trackers entry 'sensor.phone_battery' is not a device_tracker entity id",
		"helpers.yaml: person 'mixed' device_trackers entry 'device_tracker.Phone' is not a device_tracker entity id",
		"helpers.yaml: person 'mixed' device_trackers entry 5 is not a device_tracker entity id",
	)
	if strings.Contains(message, "'device_tracker.phone'") {
		t.Errorf("err = %q, flagged a valid tracker", message)
	}
}

// HA's person update defaults a missing device_trackers to [], so a null
// dropped from the update would unlink every tracker.
func TestPersonNullIsRefused(t *testing.T) {
	message := loadHelpersError(t, `
person:
  - id: anna
    name: Anna
    device_trackers: null
`)

	wantAllIn(t, message, "helpers.yaml: person 'anna' field 'device_trackers' cannot be null")
}

// --- zone: plan ------------------------------------------------------------

var liveOffice = map[string]any{
	"id": "office", "name": "Office", "latitude": 52.2297, "longitude": 21.0122,
	"radius": 100.0, "passive": false, "icon": "mdi:briefcase",
}

func zoneDesired(item map[string]any) Desired {
	return Desired{Helpers: map[string][]map[string]any{"zone": {item}}}
}

func TestZoneCreateWhenNothingMatches(t *testing.T) {
	desired := zoneDesired(map[string]any{"id": "office", "name": "Office", "latitude": 52.2297, "longitude": 21.0122})

	ops := Plan(desired, map[string][]map[string]any{"zone": {}}, nil)

	if len(ops) != 1 || ops[0].Kind != KindCreate || ops[0].RType != "zone" || ops[0].Key != "office" {
		t.Fatalf("ops = %+v, want one zone create", ops)
	}
	want := map[string]any{"name": "Office", "latitude": 52.2297, "longitude": 21.0122}
	if !reflect.DeepEqual(ops[0].Params, want) {
		t.Errorf("params = %+v, want %+v", ops[0].Params, want)
	}
}

func TestZoneAdoptByExactName(t *testing.T) {
	desired := zoneDesired(map[string]any{"id": "work", "name": "Office", "latitude": 52.2297, "longitude": 21.0122})

	ops := Plan(desired, map[string][]map[string]any{"zone": {liveOffice}}, nil)

	if len(ops) != 1 || ops[0].Kind != KindUpdate || ops[0].LiveID != "office" {
		t.Fatalf("ops = %+v, want an adopting update of live zone office", ops)
	}
	if !strings.Contains(ops[0].DiffText, "adopted existing zone") {
		t.Errorf("diff_text = %q", ops[0].DiffText)
	}
}

// HA stores radius as a float: a manifest int of the same value is no drift.
func TestZoneRadiusIntMatchesLiveFloat(t *testing.T) {
	desired := zoneDesired(map[string]any{
		"id": "office", "name": "Office", "latitude": 52.2297, "longitude": 21.0122, "radius": 100,
	})
	managed := map[string]string{"zone:office": "office"}

	if ops := Plan(desired, map[string][]map[string]any{"zone": {liveOffice}}, managed); len(ops) != 0 {
		t.Errorf("ops = %+v, want none", ops)
	}
}

func TestZoneUpdateOnDrift(t *testing.T) {
	desired := zoneDesired(map[string]any{
		"id": "office", "name": "Office", "latitude": 52.2297, "longitude": 21.0122, "radius": 250,
	})
	managed := map[string]string{"zone:office": "office"}

	ops := Plan(desired, map[string][]map[string]any{"zone": {liveOffice}}, managed)

	if len(ops) != 1 || ops[0].Kind != KindUpdate || ops[0].LiveID != "office" {
		t.Fatalf("ops = %+v, want one update", ops)
	}
	if !strings.Contains(ops[0].DiffText, "-radius: 100\n") || !strings.Contains(ops[0].DiffText, "+radius: 250") {
		t.Errorf("diff_text = %q", ops[0].DiffText)
	}
}

func TestZoneDeleteWhenRemovedFromManifest(t *testing.T) {
	managed := map[string]string{"zone:office": "office"}

	ops := Plan(Desired{}, map[string][]map[string]any{"zone": {liveOffice}}, managed)

	if len(ops) != 1 || ops[0].Kind != KindDelete || ops[0].RType != "zone" || ops[0].LiveID != "office" {
		t.Errorf("ops = %+v, want one zone delete", ops)
	}
}

// --- person: plan ----------------------------------------------------------

var liveAnna = map[string]any{
	"id": "anna", "name": "Anna", "user_id": "0d3f4a1b2c",
	"device_trackers": []any{"device_tracker.anna_phone"}, "picture": nil,
}

func personDesired(items ...map[string]any) Desired {
	return Desired{Helpers: map[string][]map[string]any{"person": items}}
}

func TestPersonCreateWhenNothingMatches(t *testing.T) {
	desired := personDesired(map[string]any{"id": "guest", "name": "Guest"})
	live := map[string][]map[string]any{"person": {liveAnna}, PersonYAMLBucket: {}}

	ops := Plan(desired, live, nil)

	if len(ops) != 1 || ops[0].Kind != KindCreate || ops[0].RType != "person" {
		t.Fatalf("ops = %+v, want one person create", ops)
	}
	if !reflect.DeepEqual(ops[0].Params, map[string]any{"name": "Guest"}) {
		t.Errorf("params = %+v", ops[0].Params)
	}
}

func TestPersonAdoptThenUpdateDeviceTrackers(t *testing.T) {
	desired := personDesired(map[string]any{
		"id": "anna", "name": "Anna", "device_trackers": []any{"device_tracker.anna_phone", "device_tracker.anna_watch"},
	})
	live := map[string][]map[string]any{"person": {liveAnna}}

	adopt := Plan(desired, live, nil)
	if len(adopt) != 1 || adopt[0].Kind != KindUpdate || adopt[0].LiveID != "anna" {
		t.Fatalf("ops = %+v, want an adopting update", adopt)
	}

	update := Plan(desired, live, map[string]string{"person:anna": "anna"})
	if len(update) != 1 || update[0].Kind != KindUpdate || !strings.Contains(update[0].DiffText, "device_tracker.anna_watch") {
		t.Errorf("ops = %+v, want a device_trackers update", update)
	}
}

// user_id and picture are never declared, so whatever the UI set on them
// is never drift.
func TestPersonUndeclaredUserIDAndPictureAreNotDrift(t *testing.T) {
	desired := personDesired(map[string]any{"id": "anna", "name": "Anna", "device_trackers": []any{"device_tracker.anna_phone"}})
	live := map[string][]map[string]any{"person": {liveAnna}}

	if ops := Plan(desired, live, map[string]string{"person:anna": "anna"}); len(ops) != 0 {
		t.Errorf("ops = %+v, want none", ops)
	}
}

func TestPersonDeleteWhenRemovedFromManifest(t *testing.T) {
	live := map[string][]map[string]any{"person": {liveAnna}}

	ops := Plan(Desired{}, live, map[string]string{"person:anna": "anna"})

	if len(ops) != 1 || ops[0].Kind != KindDelete || ops[0].RType != "person" || ops[0].LiveID != "anna" {
		t.Errorf("ops = %+v, want one person delete", ops)
	}
}

// person/list's config persons share the storage ones' id space, so a
// create next to one of the same name made a second person.
func TestPersonCreateClashingWithAYAMLPersonIsAnError(t *testing.T) {
	desired := personDesired(
		map[string]any{"id": "bob", "name": "Bob"},
		map[string]any{"id": "carol", "name": "Carol"},
	)
	live := map[string][]map[string]any{
		"person":         {},
		PersonYAMLBucket: {{"id": "bob_yaml", "name": "Bob", "device_trackers": []any{}}},
	}

	ops := Plan(desired, live, nil)

	if len(ops) != 2 {
		t.Fatalf("ops = %+v, want an error for bob and a create for carol", ops)
	}
	if ops[0].Kind != KindError || ops[0].Key != "bob" ||
		!strings.Contains(ops[0].Error, "a person named 'Bob' (id 'bob_yaml') is defined in configuration.yaml") {
		t.Errorf("bob op = %+v", ops[0])
	}
	if ops[1].Kind != KindCreate || ops[1].Key != "carol" {
		t.Errorf("carol op = %+v", ops[1])
	}
}

func TestPersonRecreateClashingWithAYAMLPersonIsAnError(t *testing.T) {
	desired := personDesired(map[string]any{"id": "bob", "name": "Bob"})
	live := map[string][]map[string]any{
		"person":         {},
		PersonYAMLBucket: {{"id": "bob", "name": "Bob"}},
	}

	ops := Plan(desired, live, map[string]string{"person:bob": "bob"})

	if len(ops) != 1 || ops[0].Kind != KindError {
		t.Errorf("ops = %+v, want one error op, not a recreate", ops)
	}
}

// A storage person of the same name is still adopted: nothing new is made.
func TestPersonAdoptIsUnaffectedByAYAMLPersonOfTheSameName(t *testing.T) {
	desired := personDesired(map[string]any{"id": "anna", "name": "Anna"})
	live := map[string][]map[string]any{
		"person":         {liveAnna},
		PersonYAMLBucket: {{"id": "anna_yaml", "name": "Anna"}},
	}

	ops := Plan(desired, live, nil)

	if len(ops) != 1 || ops[0].Kind != KindUpdate || ops[0].LiveID != "anna" {
		t.Errorf("ops = %+v, want the storage person adopted", ops)
	}
}

func TestHelperDomainsForIncludesManagedZonesAndPersons(t *testing.T) {
	managed := map[string]string{"zone:office": "office", "person:anna": "anna"}

	if got := HelperDomainsFor(Desired{}, managed); !reflect.DeepEqual(got, []string{"person", "zone"}) {
		t.Errorf("domains = %v, want [person zone]", got)
	}
}
