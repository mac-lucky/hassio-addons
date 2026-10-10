package connectd

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// TestHelperProcess is the fake Connect binary: started by the tests
// below with FAKE_CONNECT set, it logs like Connect and then either exits
// (FAKE_CONNECT=crash) or waits for SIGTERM.
func TestHelperProcess(t *testing.T) {
	mode := os.Getenv("FAKE_CONNECT")
	if mode == "" {
		return
	}
	fmt.Printf(`{"log_message":"(I) starting on %s","timestamp":"x","level":3}`+"\n", os.Getenv("OP_HTTP_PORT"))
	fmt.Printf(`{"log_message":"(E) failed: %s","timestamp":"x","level":1}`+"\n", "bad thing")
	fmt.Println("plain panic line")
	if os.Getenv("SUPERVISOR_TOKEN") != "" {
		fmt.Println(`{"log_message":"(E) LEAK supervisor token visible","level":1}`)
	}
	if b, err := os.ReadFile(os.Getenv("OP_SESSION")); err != nil || len(b) == 0 {
		fmt.Println(`{"log_message":"(E) NO CREDENTIALS","level":1}`)
	}
	if mode == "crash" {
		os.Exit(3)
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM)
	<-sig
	os.Exit(0)
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func captureLogs(t *testing.T) *syncBuffer {
	t.Helper()
	buf := &syncBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

func testConfig(t *testing.T, mode string) Config {
	cfg := Defaults()
	cfg.APIBin = os.Args[0]
	cfg.SyncBin = os.Args[0]
	cfg.DataDir = filepath.Join(t.TempDir(), "connect")
	cfg.Credentials = []byte(`{"version":"2"}`)
	cfg.ExtraArgs = []string{"-test.run=^TestHelperProcess$"}
	cfg.ExtraEnv = []string{"FAKE_CONNECT=" + mode}
	cfg.StopGrace = 2 * time.Second
	return cfg
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestServerRunsAndStops(t *testing.T) {
	t.Setenv("SUPERVISOR_TOKEN", "must-not-reach-children")
	logs := captureLogs(t)
	cfg := testConfig(t, "run")
	srv := New(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	if err := srv.Start(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		st := srv.Status()
		return st[0].Running && st[1].Running && strings.Contains(logs.String(), "starting on 8080") && strings.Contains(logs.String(), "starting on 8081")
	})
	info, err := os.Stat(srv.CredentialsPath())
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("credentials file: %v %v", info, err)
	}
	cancel()
	done := make(chan struct{})
	go func() { srv.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("processes did not stop")
	}
	out := logs.String()
	for _, want := range []string{"component=connect-api", "component=connect-sync", "level=ERROR msg=\"failed: bad thing\"", "plain panic line"} {
		if !strings.Contains(out, want) {
			t.Errorf("logs missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "LEAK") || strings.Contains(out, "NO CREDENTIALS") {
		t.Fatalf("child environment wrong:\n%s", out)
	}
	if st := srv.Status(); st[1].LastError == "" {
		t.Error("LastError not recorded")
	}
}

func TestServerRestartsCrashedProcess(t *testing.T) {
	captureLogs(t)
	srv := New(testConfig(t, "crash"))
	ctx, cancel := context.WithCancel(context.Background())
	defer func() { cancel(); srv.Wait() }()
	if err := srv.Start(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		st := srv.Status()
		return st[0].Restarts >= 2 && strings.Contains(st[0].LastExit, "exit status 3")
	})
}

func TestStartNeedsCredentials(t *testing.T) {
	cfg := testConfig(t, "run")
	cfg.Credentials = nil
	if err := New(cfg).Start(context.Background()); err == nil {
		t.Fatal("want an error without credentials")
	}
}
