// Package secrettest builds a secretref.Resolver over a throwaway config
// root, for the tests of every layer that resolves "secret://<name>"
// references. Shared rather than copied per package so the sentinel below
// cannot drift into an assertion nothing writes any more.
package secrettest

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/secretref"
)

// Resolved is the credential a secrets file built here holds - one
// distinctive string, because several tests assert it appears NOWHERE in a
// plan line, a stash file or an error message.
const Resolved = "S3CRET-resolved"

// From returns a Resolver over a temp config root holding contents as its
// secrets.yaml. The file is written even for the common contents == ""
// call, so a test that starts referencing something gets a real file
// rather than a missing-file error.
func From(t *testing.T, contents string) *secretref.Resolver {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, "secrets.yaml")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return secretref.NewResolver(root)
}
