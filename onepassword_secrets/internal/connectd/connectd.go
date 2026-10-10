// Package connectd runs the embedded 1Password Connect server: the
// connect-sync and connect-api processes from 1Password's own images,
// supervised by this add-on instead of s6. Each gets a fixed HTTP and bus
// port, the credentials file from the add-on options (written 0600 under
// /data), and an environment built from scratch, so neither ever sees
// SUPERVISOR_TOKEN. Connect has no bind-address setting: the ports listen on
// every interface of the add-on's container, so other containers on the
// hassio network can reach them (the API refuses anything without the
// token), as in 1Password's own Kubernetes deployment. A crashed process is
// restarted with backoff; their JSON log lines are re-emitted through slog.
package connectd

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mac-lucky/hassio-addons/onepassword_secrets/internal/fsx"
	"github.com/mac-lucky/hassio-addons/onepassword_secrets/internal/humanize"
)

// Process names, also the log "component".
const (
	API  = "connect-api"
	Sync = "connect-sync"
)

// Config is how to run the pair.
type Config struct {
	APIBin  string
	SyncBin string
	// DataDir holds the credentials file and Connect's encrypted local
	// copy of the vaults (XDG_DATA_HOME, so DataDir/.op/data).
	DataDir     string
	Credentials []byte
	APIPort     int
	SyncPort    int
	APIBusPort  int
	SyncBusPort int
	// LogLevel is the add-on's level; Connect gets info, debug or error.
	LogLevel string
	// ExtraArgs and ExtraEnv are for tests, which run a fake binary.
	ExtraArgs []string
	ExtraEnv  []string
	// StopGrace is how long a process gets between SIGTERM and SIGKILL.
	StopGrace time.Duration
}

// Defaults are the ports the add-on uses. None of them is published on the
// host.
func Defaults() Config {
	return Config{
		APIBin:      "/usr/local/bin/connect-api",
		SyncBin:     "/usr/local/bin/connect-sync",
		DataDir:     "/data/connect",
		APIPort:     8080,
		SyncPort:    8081,
		APIBusPort:  11220,
		SyncBusPort: 11221,
		LogLevel:    "info",
		StopGrace:   10 * time.Second,
	}
}

// URL is the API's base URL.
func (c Config) URL() string { return "http://127.0.0.1:" + strconv.Itoa(c.APIPort) }

// ProcState is one process's supervision state.
type ProcState struct {
	Name      string
	Running   bool
	PID       int
	StartedAt time.Time
	Restarts  int
	// QuickExits counts the exits since the process last stayed up for
	// stableAfter: a crash loop, where Restarts also counts one-off
	// crashes weeks apart.
	QuickExits int
	LastExit   string
	LastExitAt time.Time
	// LastError is the newest error-level line the process logged.
	LastError   string
	LastErrorAt time.Time
}

// Server supervises the pair.
type Server struct {
	cfg Config
	mu  sync.Mutex
	st  map[string]*ProcState
	wg  sync.WaitGroup
}

// New returns a Server; Start runs it.
func New(cfg Config) *Server {
	if cfg.StopGrace <= 0 {
		cfg.StopGrace = 10 * time.Second
	}
	return &Server{cfg: cfg, st: map[string]*ProcState{
		Sync: {Name: Sync},
		API:  {Name: API},
	}}
}

// CredentialsPath is where the credentials file is written.
func (s *Server) CredentialsPath() string {
	return filepath.Join(s.cfg.DataDir, "1password-credentials.json")
}

// Start writes the credentials and launches both supervisors. They run
// until ctx ends; Wait blocks until both processes are gone.
func (s *Server) Start(ctx context.Context) error {
	if len(s.cfg.Credentials) == 0 {
		return errors.New("connectd: no credentials")
	}
	if err := os.MkdirAll(s.cfg.DataDir, 0o700); err != nil {
		return fmt.Errorf("connectd: creating %s: %w", s.cfg.DataDir, err)
	}
	// Tighten a directory a previous version may have made wider.
	if err := os.Chmod(s.cfg.DataDir, 0o700); err != nil { // #nosec G302 -- a directory: 0700 is owner-only
		return fmt.Errorf("connectd: %w", err)
	}
	if err := fsx.WriteFileAtomic(s.CredentialsPath(), s.cfg.Credentials, 0o600); err != nil {
		return fmt.Errorf("connectd: writing the credentials file: %w", err)
	}
	// Stamp both as starting now, before their goroutines run: the
	// engine's first cycle follows Start at once, and reads StartedAt to
	// tell a server that is still starting from one that is down.
	now := time.Now()
	s.mu.Lock()
	for _, p := range s.st {
		p.StartedAt = now
	}
	s.mu.Unlock()
	s.wg.Add(2)
	go s.supervise(ctx, Sync, s.cfg.SyncBin, s.cfg.SyncPort, s.cfg.SyncBusPort, s.cfg.APIBusPort)
	go s.supervise(ctx, API, s.cfg.APIBin, s.cfg.APIPort, s.cfg.APIBusPort, s.cfg.SyncBusPort)
	return nil
}

// Wait blocks until both supervisors returned.
func (s *Server) Wait() { s.wg.Wait() }

// Status returns both processes' state, sync first.
func (s *Server) Status() []ProcState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return []ProcState{*s.st[Sync], *s.st[API]}
}

func (s *Server) update(name string, f func(*ProcState)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f(s.st[name])
}

// Backoff bounds between restarts. A process that stayed up for
// stableAfter resets the backoff.
const (
	minBackoff  = time.Second
	maxBackoff  = time.Minute
	stableAfter = 5 * time.Minute
)

func (s *Server) supervise(ctx context.Context, name, bin string, port, bus, peer int) {
	defer s.wg.Done()
	backoff := minBackoff
	for ctx.Err() == nil {
		started := time.Now()
		err := s.runOnce(ctx, name, bin, port, bus, peer)
		if ctx.Err() != nil {
			return
		}
		exit := "exited"
		if err != nil {
			exit = humanize.Truncate(err.Error(), 300)
		}
		stable := time.Since(started) > stableAfter
		s.update(name, func(p *ProcState) {
			p.Running = false
			p.PID = 0
			p.Restarts++
			if stable {
				p.QuickExits = 0
			}
			p.QuickExits++
			p.LastExit = exit
			p.LastExitAt = time.Now()
		})
		if stable {
			backoff = minBackoff
		}
		slog.Warn("connect process stopped, restarting", "component", name, "exit", exit, "in", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

func (s *Server) env(port, bus, peer int) []string {
	level := "info"
	switch s.cfg.LogLevel {
	case "debug":
		level = "debug"
	case "error":
		level = "error"
	}
	env := []string{
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"HOME=" + s.cfg.DataDir,
		"XDG_DATA_HOME=" + s.cfg.DataDir,
		"SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt",
		"OP_SESSION=" + s.CredentialsPath(),
		"OP_HTTP_PORT=" + strconv.Itoa(port),
		"OP_BUS_PORT=" + strconv.Itoa(bus),
		"OP_BUS_PEERS=127.0.0.1:" + strconv.Itoa(peer),
		"OP_LOG_LEVEL=" + level,
		// SQLite tries /var/tmp before /tmp for its temp files; keep them
		// where the AppArmor profile allows.
		"TMPDIR=/tmp",
	}
	return append(env, s.cfg.ExtraEnv...)
}

func (s *Server) runOnce(ctx context.Context, name, bin string, port, bus, peer int) error {
	cmd := exec.CommandContext(ctx, bin, s.cfg.ExtraArgs...) // #nosec G204 -- fixed binary paths from Config
	cmd.Env = s.env(port, bus, peer)
	cmd.Dir = s.cfg.DataDir
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = s.cfg.StopGrace
	// An os.Pipe, not StdoutPipe: Wait closes a StdoutPipe as soon as the
	// process exits, which drops the last lines a crashing process wrote -
	// the ones that say why. This read end is ours to close after EOF.
	out, pw, err := os.Pipe()
	if err != nil {
		return err
	}
	cmd.Stdout, cmd.Stderr = pw, pw
	if err := cmd.Start(); err != nil {
		_ = out.Close()
		_ = pw.Close()
		return err
	}
	_ = pw.Close()
	s.update(name, func(p *ProcState) {
		p.Running = true
		p.PID = cmd.Process.Pid
		p.StartedAt = time.Now()
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() { _ = out.Close() }()
		s.forward(name, out)
	}()
	err = cmd.Wait()
	<-done
	return err
}

// line is Connect's log format.
type line struct {
	Message string `json:"log_message"`
	Level   int    `json:"level"`
}

// forward re-emits a process's output through slog. Connect logs one JSON
// object per line: level 1 error, 2 warning, 3 info, 4 debug, and a
// message prefixed "(E) ", "(W) ", "(I) " or "(D) ".
func (s *Server) forward(name string, r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		raw := sc.Bytes()
		var l line
		msg := ""
		level := slog.LevelWarn
		if json.Unmarshal(raw, &l) == nil && l.Message != "" {
			msg = strings.TrimSpace(l.Message)
			if len(msg) > 4 && msg[0] == '(' && msg[2] == ')' && msg[3] == ' ' {
				msg = msg[4:]
			}
			switch l.Level {
			case 1:
				level = slog.LevelError
			case 2:
				level = slog.LevelWarn
			case 3:
				level = slog.LevelInfo
			default:
				level = slog.LevelDebug
			}
		} else {
			// A panic trace or other plain output.
			msg = strings.TrimSpace(string(raw))
		}
		if msg == "" {
			continue
		}
		msg = humanize.Truncate(msg, 2000)
		// Per-request lines are noise at info; keep them for debug.
		if level == slog.LevelInfo && (strings.HasPrefix(msg, "GET ") || strings.HasPrefix(msg, "POST ")) {
			level = slog.LevelDebug
		}
		if level >= slog.LevelError {
			s.update(name, func(p *ProcState) {
				p.LastError = humanize.Truncate(msg, 300)
				p.LastErrorAt = time.Now()
			})
		}
		slog.Log(context.Background(), level, msg, "component", name)
	}
	if err := sc.Err(); err != nil {
		slog.Warn("connect output unreadable, discarding the rest", "component", name, "error", err)
	}
	// Keep draining after a line too long to scan, or the process blocks
	// on a full pipe and stops answering.
	_, _ = io.Copy(io.Discard, r)
}
