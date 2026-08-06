// Package statetest builds an applier.State whose every value position
// carries a sentinel secret, for the tests asserting nothing this agent
// displays ever reads a VALUE out of persisted state.
//
// Built by walking State's fields and refusing one it does not recognize,
// because a keyed struct literal would silently zero-value every field
// added after it - a new value-bearing field could then be leaked onto the
// dashboard with every secret test still green.
//
// A separate package rather than a helper inside internal/recon so both
// callers - recon's test over the marshalled Status, web's over the
// rendered dashboard - poison the same state from the same list.
package statetest

import (
	"reflect"
	"testing"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/applier"
)

// Sentinel is what PoisonedState plants in every value position, standing
// for a password submitted during a config flow, a live entity name or
// add-on option captured under management, and the hashes over them.
const Sentinel = "S3CRET-sentinel"

// PoisonedState is an applier.State a real agent could have written, with
// the sentinel in every value a display path might reach. Every field is
// classified below and an unclassified one fails the test outright - read
// the default branch before adding a case.
//
// Three kinds of exemption, each stated at its own case: the entries ARE
// names and showing them is the feature (Manifest, the "error" half of the
// attempt maps); the field cannot carry a string (AddonRestartOnChange);
// the field is displayed BY DESIGN (the commit, branch and timestamp
// scalars), so it gets a realistic value instead.
func PoisonedState(t *testing.T) applier.State {
	t.Helper()

	state := applier.State{}
	typ := reflect.TypeOf(state)
	for i := 0; i < typ.NumField(); i++ {
		switch name := typ.Field(i).Name; name {
		case "Manifest":
			// Exempt: a manifest entry is a config-relative path and listing
			// it is the feature. Out of order, to give sort assertions work.
			state.Manifest = []string{"packages/lights.yaml", "automations.yaml"}
		case "RegistryManaged":
			state.RegistryManaged = map[string]string{"floor:ground": Sentinel}
		case "EntityOriginals":
			state.EntityOriginals = map[string]map[string]any{
				"entity:light.kitchen":      {"name": Sentinel},
				"entity:sensor.outdoor_dew": {"name": Sentinel},
			}
		case "DashboardManaged":
			state.DashboardManaged = map[string]string{"dashboard:energy": Sentinel}
		case "AddonOriginals":
			state.AddonOriginals = map[string]map[string]any{
				"addon:core_ssh": {"authorized_keys": Sentinel},
			}
		case "AddonRestartOnChange":
			// Exempt by type: a bool cannot carry a secret. The key is
			// planted all the same, since the mirrors read it.
			state.AddonRestartOnChange = map[string]bool{"addon:core_ssh": true}
		case "IntegrationManaged":
			state.IntegrationManaged = map[string]string{"integration:workday_main": Sentinel}
		case "IntegrationHashes":
			state.IntegrationHashes = map[string]string{"integration:workday_main": Sentinel}
		case "IntegrationData":
			// The field that makes this whole exercise necessary: declared
			// flow data in the clear, credential fields included.
			state.IntegrationData = map[string]map[string]any{
				"integration:workday_main": {"user": map[string]any{"password": Sentinel}},
			}
		case "IntegrationAttempts":
			// "hash" is poisoned; "error" is exempt, since the Recorded
			// failures card renders it on purpose.
			state.IntegrationAttempts = map[string]map[string]any{
				"integration:old_weather": {"hash": Sentinel, "error": "invalid_auth"},
			}
		case "SubentryManaged":
			state.SubentryManaged = map[string]string{"subentry:widget_hall": Sentinel}
		case "SubentryHashes":
			state.SubentryHashes = map[string]string{"subentry:widget_hall": Sentinel}
		case "SubentryAttempts":
			// Same split as IntegrationAttempts above.
			state.SubentryAttempts = map[string]map[string]any{
				"subentry:widget_garage": {"hash": Sentinel, "error": "unexpected step"},
			}
		case "HacsManaged":
			state.HacsManaged = map[string]string{"hacs:anker_solix": Sentinel}
		case "HacsAttempts":
			// Same split as IntegrationAttempts above.
			state.HacsAttempts = map[string]map[string]any{
				"hacs:broken_card": {"hash": Sentinel, "error": "no release tagged 9.9.9"},
			}
		case "HacsRestartPending":
			// Exempt: the entries ARE names - downloaded integration domains
			// - and rendering them is the feature. Out of order, to give a
			// caller's sort assertion something to sort.
			state.HacsRestartPending = []string{"anker_solix", "adaptive_lighting"}
		case "LastDriftBackHash":
			// Poisoned, unlike the scalars below: it fingerprints a pending
			// change set and is never meant to be displayed.
			state.LastDriftBackHash = Sentinel
		case "LastGoodSHA":
			state.LastGoodSHA = "4f9c2a7e8b1d0c3f6a5e9d8c7b6a5f4e3d2c1b0a"
		case "LastApplyUTC":
			state.LastApplyUTC = "2026-08-01T21:14:22+00:00"
		case "LastDriftBranch":
			state.LastDriftBranch = "gitops/drift-20260802T063000Z"
		case "LastImportSHA":
			state.LastImportSHA = "9d3b7c1a5e2f8046b3c9d7e1a2f5084b6c3d9e17"
		case "LastImportUTC":
			state.LastImportUTC = "2026-08-03T14:12:07+00:00"
		default:
			t.Fatalf("applier.State.%s is new and unclassified: poison its values here, "+
				"or exempt it with the reason - a secret test over a state that zero-values "+
				"this field proves nothing about it", name)
		}
	}
	return state
}

// ManagedNames is what PoisonedState puts under management, one name per
// inventory group. Callers assert these DO show up: a display that rendered
// nothing would pass a "the sentinel is absent" check for free.
func ManagedNames() []string {
	return []string{
		"automations.yaml", // files
		"floor:ground",     // registry objects, prefix and all
		"light.kitchen",    // entities
		"energy",           // dashboards
		"core_ssh",         // add-on options
		"workday_main",     // integrations
		"widget_hall",      // subentries
		"anker_solix",      // HACS integrations
	}
}
