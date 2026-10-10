// Command onepassword-secrets is the 1Password Secrets add-on: it runs an
// embedded 1Password Connect server, renders Home Assistant's secrets files
// from it, follows every change, and serves the ingress panel.
package main

import (
	"bufio"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/mac-lucky/hassio-addons/onepassword_secrets/internal/connectd"
	"github.com/mac-lucky/hassio-addons/onepassword_secrets/internal/engine"
	"github.com/mac-lucky/hassio-addons/onepassword_secrets/internal/ha"
	"github.com/mac-lucky/hassio-addons/onepassword_secrets/internal/opconnect"
	"github.com/mac-lucky/hassio-addons/onepassword_secrets/internal/options"
	"github.com/mac-lucky/hassio-addons/onepassword_secrets/internal/web"
)

// version is stamped at build time (-X main.version).
var version = "dev"

// Fixed paths inside the add-on container.
const (
	configRoot = "/homeassistant"
	dataDir    = "/data"
	listenAddr = ":8099"
)

func main() {
	os.Exit(run())
}

func run() int {
	dev := os.Getenv(web.DevEnvVar) == "1"
	root, data := configRoot, dataDir
	if dev {
		root = envOr("OPS_ROOT", "./.dev/config")
		data = envOr("OPS_DATA", "./.dev/data")
		_ = os.MkdirAll(root, 0o750)
		_ = os.MkdirAll(data, 0o700)
	}

	opts, err := options.Load(filepath.Join(data, "options.json"))
	if err != nil {
		if !dev || !errors.Is(err, os.ErrNotExist) {
			setupLogging("info")
			slog.Error("invalid add-on options; fix them on the Configuration tab", "error", err)
			return 1
		}
		opts = options.Defaults()
	}
	setupLogging(opts.LogLevel)
	slog.Info("starting 1Password Secrets", "version", version, "embedded_connect", opts.Embedded(), "dry_run", opts.DryRun, "files", strings.Join(opts.SecretsFiles, ", "))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	token, err := options.SupervisorToken()
	if err != nil && !dev {
		slog.Error("SUPERVISOR_TOKEN is not set; this binary runs only as a Home Assistant add-on", "error", err)
		return 1
	}
	haClient := &ha.Client{Base: options.Supervisor, Token: token}
	selfSlug := ""
	if !dev {
		if s, err := haClient.SelfSlug(ctx); err == nil {
			selfSlug = s
		} else {
			slog.Warn("could not read this add-on's slug; it will not skip itself when restarting add-ons", "error", err)
		}
	}

	var procs engine.Processes
	var connectSrv *connectd.Server
	baseURL := opts.ConnectURL
	if opts.Configured() && opts.Embedded() {
		cfg := connectd.Defaults()
		cfg.DataDir = filepath.Join(data, "connect")
		cfg.Credentials = opts.ConnectCredentials
		cfg.LogLevel = opts.LogLevel
		connectSrv = connectd.New(cfg)
		if err := connectSrv.Start(ctx); err != nil {
			slog.Error("could not start the Connect server", "error", err)
			return 1
		}
		procs = connectSrv
		baseURL = cfg.URL()
	}
	// The credentials are on disk for Connect now; drop this copy.
	opts.ConnectCredentials = clearBytes(opts.ConnectCredentials)

	eng, err := engine.New(engine.Config{
		Options:      opts,
		Version:      version,
		SelfSlug:     selfSlug,
		Root:         root,
		StatePath:    filepath.Join(data, "state.json"),
		KeyPath:      filepath.Join(data, "fingerprint.key"),
		ActivityPath: filepath.Join(data, "activity.jsonl"),
		PreviousDir:  filepath.Join(data, "previous"),
	}, &opconnect.Client{BaseURL: baseURL, Token: opts.ConnectToken}, haClient, procs)
	if err != nil {
		slog.Error("could not load the add-on state", "error", err)
		return 1
	}

	srv := &http.Server{
		Addr:              listenAddr,
		Handler:           web.New(eng),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("panel server stopped", "error", err)
			stop()
		}
	}()

	go readStdin(eng)

	loopDone := make(chan struct{})
	go func() {
		defer close(loopDone)
		if opts.Configured() || !dev {
			eng.Run(ctx)
			return
		}
		<-ctx.Done()
	}()

	<-ctx.Done()
	slog.Info("stopping")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	select {
	case <-loopDone:
	case <-time.After(30 * time.Second):
		slog.Warn("sync still running at shutdown; leaving it")
	}
	if connectSrv != nil {
		connectSrv.Wait()
	}
	return 0
}

// readStdin turns `hassio.addon_stdin` input into actions. Supervisor
// writes the service's input as one JSON value per line; "sync" (quoted or
// not) asks for a sync.
func readStdin(eng *engine.Engine) {
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		cmd := strings.Trim(strings.TrimSpace(sc.Text()), `"`)
		switch strings.ToLower(cmd) {
		case "sync":
			slog.Info("sync requested through stdin")
			eng.SyncNow()
		case "":
		default:
			slog.Warn("unknown stdin command; only \"sync\" is understood", "command_length", len(cmd))
		}
	}
}

func setupLogging(level string) {
	var l slog.Level
	switch level {
	case "debug":
		l = slog.LevelDebug
	case "warning":
		l = slog.LevelWarn
	case "error":
		l = slog.LevelError
	default:
		l = slog.LevelInfo
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: l})))
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

func clearBytes(b []byte) []byte {
	for i := range b {
		b[i] = 0
	}
	return nil
}
