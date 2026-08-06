package recon

import (
	"fmt"
	"sort"
	"strings"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/applier"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/failmemory"
)

// refreshStateMirrors rebuilds the in-memory views of /data/state.json
// that Status serves, from a state the caller already holds. Refreshed at
// the few sites where a fresh state exists rather than per Status() call,
// which the dashboard makes every few seconds, so a mirror lags by at most
// one cycle.
//
// SECRET BOUNDARY: only names, keys and error strings may cross into a
// mirror - everything here reaches the dashboard and /status.json. The
// attempts' "hash", IntegrationData and the entity/add-on originals must
// not (see managedInventory).
func (r *Reconciler) refreshStateMirrors(state applier.State) {
	blocked := make([]BlockedItem, 0,
		len(state.IntegrationAttempts)+len(state.SubentryAttempts)+len(state.HacsAttempts))
	blocked = appendBlockedItems(blocked, rtypeIntegration, state.IntegrationAttempts)
	blocked = appendBlockedItems(blocked, rtypeSubentry, state.SubentryAttempts)
	blocked = appendBlockedItems(blocked, rtypeHacs, state.HacsAttempts)
	// By whole key, so the differently prefixed maps get one total order -
	// the polled fragment needs that. Stable for the case where a
	// hand-edited state.json puts the same key in two maps, which an
	// unstable sort would reorder between polls.
	sort.SliceStable(blocked, func(i, j int) bool { return blocked[i].Key < blocked[j].Key })

	// Restart intent for managed add-ons, keyed by BARE slug (state stores
	// it "addon:"-prefixed, an addon op's Key is not). A restore op
	// restarts off this, its manifest entry being gone by then.
	managedRestart := make(map[string]bool, len(state.AddonRestartOnChange))
	for key, restart := range state.AddonRestartOnChange {
		managedRestart[strings.TrimPrefix(key, "addon:")] = restart
	}

	// Sorted for hacsRestartPending's reason: the fragment holding it is
	// polled and compared byte for byte, so an unsorted list would re-swap
	// the page on every poll.
	conflicts := append([]string{}, state.ConflictedPaths...)
	sort.Strings(conflicts)

	managed := managedInventory(state)
	// A sorted copy, since the slice is on its way back into state.json.
	// Narrowed by hacsLoaded, because the disk copy lags this process's
	// own observation and would put a retired reminder back on the
	// dashboard. Filtered inside the publishing withMu, so an ApplyNow
	// that clears hacsLoaded cannot land between read and publish.
	r.withMu(func() {
		hacsRestartPending := make([]string, 0, len(state.HacsRestartPending))
		for _, domain := range state.HacsRestartPending {
			if !r.hacsLoaded[domain] {
				hacsRestartPending = append(hacsRestartPending, domain)
			}
		}
		sort.Strings(hacsRestartPending)

		r.blocked = blocked
		r.addonRestartOnChange = managedRestart
		r.managed = managed
		r.hacsRestartPending = hacsRestartPending
		r.conflicts = conflicts
		r.lastConflictBranch = state.LastConflictBranch
		r.lastConflictUTC = state.LastConflictUTC
		r.lastCaptureSHA = state.LastCaptureSHA
		r.lastCaptureUTC = state.LastCaptureUTC
	})
}

// managedInventory renders the state's ownership records for display:
// every file and live object this agent manages.
//
// SECRET BOUNDARY, and why this is written group by group rather than
// ranging over state generically: it takes keys and paths, nothing else.
// IntegrationData (declared flow data verbatim, credentials included),
// EntityOriginals/AddonOriginals and the hash fields sit beside those keys
// and must never cross - everything returned here reaches /status.json.
func managedInventory(state applier.State) ManagedInventory {
	// A sorted copy: the caller's slice is on its way back into state.json.
	files := append([]string{}, state.Manifest...)
	sort.Strings(files)

	return ManagedInventory{
		Files: files,
		// Whole keys, prefix and all - see ManagedInventory.Registry.
		Registry:     managedNames(state.RegistryManaged, ""),
		Entities:     managedNames(state.EntityOriginals, "entity:"),
		Dashboards:   managedNames(state.DashboardManaged, "dashboard:"),
		Addons:       managedNames(state.AddonOriginals, "addon:"),
		Integrations: managedNames(state.IntegrationManaged, "integration:"),
		Subentries:   managedNames(state.SubentryManaged, "subentry:"),
		Hacs:         managedNames(state.HacsManaged, rtypeHacs+":"),
	}
}

// managedNames is one ownership map's keys with prefix stripped, sorted.
// Generic over the value type only so the value is never looked at, which
// is the point (see managedInventory). TrimPrefix rather than a split, so
// a key missing its prefix is listed as it stands, not as an empty name.
func managedNames[V any](m map[string]V, prefix string) []string {
	out := make([]string, 0, len(m))
	for key := range m {
		out = append(out, strings.TrimPrefix(key, prefix))
	}
	sort.Strings(out)
	return out
}

// rtypeIntegration/rtypeSubentry/rtypeHacs are the three layers keeping a
// failure memory, so the only three an item can be blocked in. They double
// as their state keys' prefix ("integration:<manifest id>") and as the
// RType a rendered row is badged with.
const (
	rtypeIntegration = "integration"
	rtypeSubentry    = "subentry"
	rtypeHacs        = "hacs"
)

// appendBlockedItems turns one layer's attempts map into BlockedItems.
func appendBlockedItems(out []BlockedItem, rtype string, attempts map[string]map[string]any) []BlockedItem {
	prefix := rtype + ":"
	for key, attempt := range attempts {
		out = append(out, BlockedItem{
			Key:   key,
			RType: rtype,
			Name:  strings.TrimPrefix(key, prefix),
			// Only "error"; the sibling "hash" fingerprints declared data
			// and is never rendered (refreshStateMirrors). Read through
			// failmemory, as the planners' refusal message also does.
			Error: failmemory.Reason(attempt),
		})
	}
	return out
}

// blockedRType splits a blocked item's key into the layer that recorded
// it. This is the whole validation RetryBlocked does on the form value.
func blockedRType(key string) (string, bool) {
	switch {
	case strings.HasPrefix(key, rtypeIntegration+":"):
		return rtypeIntegration, true
	case strings.HasPrefix(key, rtypeSubentry+":"):
		return rtypeSubentry, true
	case strings.HasPrefix(key, rtypeHacs+":"):
		return rtypeHacs, true
	default:
		return "", false
	}
}

// RetryBlocked forgets one recorded failure, so the next cycle attempts
// that item again. The failure memory skips an item whose declared data
// still hashes to what failed (applier.State.IntegrationAttempts); this is
// the escape hatch for a cause fixed OUTSIDE the repository, where the
// manifest is already correct and nothing can move the hash. Takes opLock,
// and logs on EVERY exit including refusals, since the button only
// re-renders the same page.
func (r *Reconciler) RetryBlocked(key string) error {
	rtype, ok := blockedRType(key)
	if !ok {
		err := fmt.Errorf("cannot retry %q: not an integration, subentry or hacs key", key)
		r.logEvent("retry skipped: " + err.Error())
		return err
	}

	if !r.opLock.TryLock() {
		// Logged like ApplyNow's and Rollback's busy refusals.
		r.logEvent("retry skipped: " + errBusy.Error())
		return errBusy
	}
	defer r.opLock.Unlock()

	state := r.applier.StateLoad()
	attempts := state.IntegrationAttempts
	switch rtype {
	case rtypeSubentry:
		attempts = state.SubentryAttempts
	case rtypeHacs:
		attempts = state.HacsAttempts
	}
	if _, found := attempts[key]; !found {
		// Routine: the button renders from a mirror, so a cycle between
		// render and press can already have cleared the entry. Refreshed
		// here so the phantom row goes with this press.
		r.refreshStateMirrors(state)
		err := fmt.Errorf("nothing to retry: %s is not recorded as failed", key)
		r.logEvent("retry skipped: " + err.Error())
		return err
	}
	delete(attempts, key)

	if err := r.applier.StateSave(state); err != nil {
		// Gone from the copy in hand but not from disk, so the next cycle
		// is still blocked by it and the row will still be there.
		r.logEvent("retry failed for " + key + ": " + err.Error())
		return err
	}
	r.refreshStateMirrors(state)
	r.logEvent("retry: cleared failure memory for " + key + "; the next check will attempt it again")
	return nil
}
