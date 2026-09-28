package recon

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

// captureLogs redirects slog for one test and returns everything written.
func captureLogs(t *testing.T) (logged func() string) {
	t.Helper()
	prev := slog.Default()
	var buf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf.String
}

// A failed cycle reached the add-on log as level=INFO, so a log shipper
// filtering on level never saw one.
func TestFailedCycleIsLoggedAtErrorLevel(t *testing.T) {
	logged := captureLogs(t)
	fakes := newReconcilerFakes()
	fakes.git.fetchErr = errors.New("git fetch failed (exit 128): fatal: unable to access")
	r := fakes.reconciler(baseOpts())

	r.ReconcileNow(context.Background())

	var line string
	for l := range strings.SplitSeq(logged(), "\n") {
		if strings.Contains(l, "git fetch failed") {
			line = l
		}
	}
	if !strings.Contains(line, "level=ERROR") {
		t.Errorf("fetch failure logged as %q, want level=ERROR", line)
	}
}

func TestRoutineEventStaysAtInfoLevel(t *testing.T) {
	logged := captureLogs(t)
	fakes := newReconcilerFakes()
	r := fakes.reconciler(baseOpts())

	r.ReconcileNow(context.Background())

	if !strings.Contains(logged(), `level=INFO msg="in sync: no changes detected"`) {
		t.Errorf("log = %q, want the in-sync event at INFO", logged())
	}
}
