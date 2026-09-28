package gitsync

import (
	"os"
	"testing"
	"time"
)

// Retries stay on, so every transient-error path still runs them, but
// without the real waits: several tests fetch from a dead loopback port.
func TestMain(m *testing.M) {
	fetchRetryDelays = []time.Duration{0, 0}
	os.Exit(m.Run())
}
