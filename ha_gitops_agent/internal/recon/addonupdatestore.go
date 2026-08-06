package recon

import (
	"encoding/json"
	"os"
)

// addonUpdatesPath holds the last check's per-slug results, so the card is
// not blank for the first two minutes of a process (see
// addonUpdateStartupDelay) and a restart does not erase a waiting update.
//
// Its own file rather than a state.json field, like pausePath but for the
// other lock: state.json writes are load-modify-save under opLock while a
// check holds only checkLock, so routing these rows through it would mean
// either taking opLock for a check that never needs it or racing an
// in-flight save. A var so tests can point it at a temp directory.
var addonUpdatesPath = "/data/addon_updates.json"

// addonUpdateFile is the file's shape: an object rather than a bare array,
// so a second key (a schema marker, a batch timestamp) can be added later
// without older binaries choking on a changed top-level type. The rows are
// AddonUpdateStatus verbatim, so its json tags are frozen by this file too.
type addonUpdateFile struct {
	Rows []AddonUpdateStatus `json:"rows"`
}

// readAddonUpdatesFile returns the rows the last check persisted, or an
// empty list. Never errors, like applier.StateLoad: no caller could do
// anything with it, and a missing or damaged file costs one blank card.
// Strict about shape all the same - any unmarshal error discards
// everything, including the partial rows Go's json leaves after a type
// error, which would render as an add-on with no name and no verdict.
func readAddonUpdatesFile() []AddonUpdateStatus {
	data, err := os.ReadFile(addonUpdatesPath) // #nosec G304 -- the fixed /data path above; a var only so tests can retarget it
	if err != nil {
		return nil
	}

	var file addonUpdateFile
	if err := json.Unmarshal(data, &file); err != nil {
		return nil
	}
	return file.Rows
}

// writeAddonUpdatesFile persists one completed check's rows, replacing
// what was there. Returns the error rather than logging it; CheckAddonUpdates
// decides what a failed persist means.
//
// Atomic (.tmp then rename) like applier.StateSave: a torn write leaves
// JSON that readAddonUpdatesFile discards wholesale, costing every past
// check rather than the one being written. No MkdirAll, unlike StateSave -
// a missing /data is a broken volume the caller's warning is for.
func writeAddonUpdatesFile(rows []AddonUpdateStatus) error {
	// From the named field, so an older binary sees the one-key object it
	// expects. A nil slice emits "rows": null, which hydrate handles the
	// same as [], so no normalization is needed.
	data, err := json.Marshal(addonUpdateFile{Rows: rows})
	if err != nil {
		return err
	}

	tmpPath := addonUpdatesPath + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o644); err != nil { // #nosec G306 -- add-on-private check results, not secret
		return err
	}
	if err := os.Rename(tmpPath, addonUpdatesPath); err != nil {
		// Cleaned up like history.rotate's own rename failure: a .tmp
		// nothing will read again is litter in a volume users look at.
		_ = os.Remove(tmpPath)
		return err
	}
	return nil
}

// hydrateAddonUpdates reconciles what was persisted against what is
// watched now, producing the rows a fresh process starts with. The option
// says WHICH rows exist, the file says what each one SAYS:
//
//   - Walked over slugs in auto_update_addons' order, matching what a
//     check produces. The map is lookup only and never ranged over -
//     randomized row order would change the polled fragment's bytes every
//     render, so /fragment could never answer 204.
//   - A known slug is restored VERBATIM, LastCheckedUTC included: the
//     dashboard renders its age, and re-stamping it would claim a check
//     this agent never ran.
//   - An unknown slug gets a placeholder rather than being omitted, since
//     a silently missing row is how a typo'd slug stays invisible (see
//     AddonUpdateStatus). An empty file is the exception, below.
//   - A row whose slug is no longer watched is dropped.
//
// Nothing else is validated: every field is Supervisor's answer or this
// agent's timestamp, so a hand-edited file can only make the card wrong
// until the next check overwrites it.
func hydrateAddonUpdates(slugs []string, saved []AddonUpdateStatus) []AddonUpdateStatus {
	// No rows at all rather than a placeholder per watched slug: a
	// placeholder earns its place only in a list that HAS rows, where a
	// missing slug hides next to present ones. The card already says "no
	// results yet" in one line, and this keeps Status.AddonUpdates' empty
	// meaning "no check has ever recorded anything" true.
	if len(saved) == 0 {
		return nil
	}

	bySlug := addonUpdatesBySlug(saved)

	// Presized rather than append-to-nil, following Status's rule for the
	// lists it hands out: len(slugs) is exactly how many rows come back.
	rows := make([]AddonUpdateStatus, 0, len(slugs))
	for _, slug := range slugs {
		if row, ok := bySlug[slug]; ok {
			rows = append(rows, row)
			continue
		}
		rows = append(rows, AddonUpdateStatus{Slug: slug, LastResult: AddonUpdateNotCheckedYet})
	}
	return rows
}

// addonUpdatesBySlug indexes rows for lookup by slug. Shared by the two
// callers that need the previous rows keyed rather than ordered - this
// file's hydrate, and previousAddonUpdates for the live ones - so the key
// is decided in one place. A later row wins on a duplicate slug, which is
// what a duplicated entry in auto_update_addons should mean: one row, the
// last thing learned about it.
//
// The result is for LOOKUP only. Ranging over it would randomize row order
// and change the polled fragment's bytes every render, so /fragment could
// never answer 204.
func addonUpdatesBySlug(rows []AddonUpdateStatus) map[string]AddonUpdateStatus {
	bySlug := make(map[string]AddonUpdateStatus, len(rows))
	for _, row := range rows {
		bySlug[row.Slug] = row
	}
	return bySlug
}
