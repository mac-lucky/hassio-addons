package regapply

// On-disk plumbing shared by the three rollback stashes, which stay in
// separate files because the layers fail and invert independently.
//
// The filenames and JSON shapes are a compatibility surface: a stash under
// /data is routinely read by a newer binary than wrote it, so renaming one
// strands whatever the previous version left behind.

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/fsx"
)

const (
	registryStashFile    = "registry_stash.json"
	addonStashFile       = "addon_stash.json"
	integrationStashFile = "integration_stash.json"
)

// writeStashFile rewrites <stashDir>/filename with payload's JSON
// atomically (fsync before rename, see fsx.WriteFileAtomic), so a crash
// mid-write leaves the previous stash intact - a truncated or zero-length
// one would parse as fewer ops and under-revert.
func writeStashFile(stashDir, filename string, payload any) error {
	if err := os.MkdirAll(stashDir, 0o755); err != nil { // #nosec G301 -- 0755 deliberate: Home Assistant's own process must traverse/read these dirs
		return err
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	// 0600, not the 0644 of other /data files: only this process reads a
	// stash, and a stash can hold a live credential (flow field data, or an
	// option value the manifest did not reference out of secrets.yaml).
	// Defence in depth - refs are substituted in stashPriorOptions - but the
	// file survives five applies and rides along in Supervisor backups.
	return fsx.WriteFileAtomic(filepath.Join(stashDir, filename), data, 0o600)
}

// readStashFile JSON-decodes <stashDir>/filename into a T, reporting a
// missing file as (zero, false, nil): most applies touch only one layer, so
// an absent stash means "this layer had no ops", not an error.
func readStashFile[T any](stashDir, filename string) (T, bool, error) {
	decoded, err := readStashFileStrict[T](stashDir, filename)
	if err != nil {
		var zero T
		if os.IsNotExist(err) {
			return zero, false, nil
		}
		return zero, false, err
	}
	return decoded, true, nil
}

// readStashFileStrict is readStashFile without the missing-file leniency:
// an absent file returns the os.ReadFile error, which RollbackRegistry
// reports - an explicit rollback with no stash is a genuine failure.
func readStashFileStrict[T any](stashDir, filename string) (T, error) {
	var decoded T
	data, err := os.ReadFile(filepath.Join(stashDir, filename)) // #nosec G304 -- stashDir is agent-managed, under the applier backup root
	if err != nil {
		return decoded, err
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		return decoded, err
	}
	return decoded, nil
}

// stashFileExists reports whether stashDir holds filename as a regular
// file - regular, so a directory of that name cannot advertise a rollback
// that can never be read.
func stashFileExists(stashDir, filename string) bool {
	info, err := os.Stat(filepath.Join(stashDir, filename))
	return err == nil && info.Mode().IsRegular()
}

// RegistryStashExists reports whether stashDir holds a registry_stash.json,
// the sibling of AddonStashExists and IntegrationStashExists that
// internal/recon consults. Exported so the filenames stay in this file.
func RegistryStashExists(stashDir string) bool {
	return stashFileExists(stashDir, registryStashFile)
}
