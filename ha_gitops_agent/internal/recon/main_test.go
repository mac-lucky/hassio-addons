package recon

import (
	"os"
	"path/filepath"
	"testing"
)

// packagePausePath is where every test's pause flag goes unless it picks
// its own with usePauseFile: a successful Rollback pauses, and the default
// /data/paused must never be written by a test run.
var packagePausePath string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "recon-test-")
	if err != nil {
		panic(err)
	}
	packagePausePath = filepath.Join(dir, "paused")
	pausePath = packagePausePath
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
