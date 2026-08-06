// Command ha-gitops-agent syncs Home Assistant configuration from a git
// repository: wires options, the reconciler and the ingress web UI, and
// runs the reconcile loop.
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/gitsync"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/hook"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/options"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/recon"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/sopscrypt"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/web"
)

// version is stamped at build time via -ldflags "-X main.version=...".
var version = "dev"

const (
	// optionsPath is where Supervisor writes the add-on's options.
	optionsPath = "/data/options.json"

	// bindAddr must match ingress_port in config.yaml.
	bindAddr = "0.0.0.0:8099"

	// hookBindAddr must match config.yaml's "8098/tcp" port. Started only
	// when opts.WebhookSecret is set; ingress only ever reaches bindAddr.
	hookBindAddr = "0.0.0.0:8098"

	readHeaderTimeout = 5 * time.Second
	readTimeout       = 30 * time.Second
	writeTimeout      = 30 * time.Second
	idleTimeout       = 120 * time.Second

	// sopsProbeTimeout bounds one local "sops --version" call at startup.
	sopsProbeTimeout = 10 * time.Second

	// waitIdleGrace is extra time, on top of shutdownTimeout, for an
	// in-flight apply/rollback: those run detached from the request, so
	// only recon.WaitIdle sees them.
	waitIdleGrace = 10 * time.Second
)

// shutdownTimeout bounds both HTTP servers' graceful shutdown and, in
// awaitLoops, the wait for the two background loops to return. A var, not
// a const, so awaitLoops' tests can shrink it.
var shutdownTimeout = 5 * time.Second

func main() {
	os.Exit(run())
}

func run() int {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel()})))
	slog.Info("starting ha-gitops-agent", "version", version)

	opts, err := options.Load(optionsPath)
	if err != nil {
		// In dev mode there is no Supervisor, so a missing options file
		// is expected; boot unconfigured. Fatal in production.
		if os.Getenv(web.DevEnvVar) != "1" {
			slog.Error("fatal: cannot load options", "path", optionsPath, "error", err)
			return 1
		}
		slog.Warn("dev mode: cannot load options, starting unconfigured", "path", optionsPath, "error", err)
		opts = options.Options{}
	}

	crypter, err := configureEncryption(opts.AgeKey)
	if err != nil {
		// Fatal, not "carry on unencrypted": the user configured a key,
		// so starting anyway would push secrets to git in the clear.
		slog.Error("fatal: age_key is set but secret encryption cannot start", "error", err)
		return 1
	}
	if crypter.Enabled() {
		// The recipient is the public half, safe to print: it is what the
		// user checks their key against when a repository will not decrypt.
		slog.Info("secret encryption enabled", "recipient", crypter.Recipient())
	}

	reconciler := recon.New(opts, recon.Deps{Crypter: crypter})

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	loopDone := make(chan struct{})
	if opts.RepoURL != "" {
		go func() {
			defer close(loopDone)
			reconciler.RunLoop(ctx)
		}()
	} else {
		close(loopDone)
		slog.Info("repo_url is not configured; reconcile loop will not start")
	}

	// Started unconditionally: RunAddonUpdateLoop returns at once when
	// auto_update_addons is empty. A SIGTERM mid-install is not covered -
	// that call is detached, and the process exits before it answers.
	addonUpdateDone := make(chan struct{})
	go func() {
		defer close(addonUpdateDone)
		reconciler.RunAddonUpdateLoop(ctx)
	}()
	if len(opts.AutoUpdateAddons) > 0 {
		slog.Info("add-on auto-update enabled", "addons", strings.Join(opts.AutoUpdateAddons, ", "))
	}
	// No goroutine: the version record runs at the tail of the reconcile
	// cycle (recon.maybeRecordAddonVersions). Logged here so a user who
	// turned it on can confirm the option took.
	if opts.TrackAddonVersions {
		slog.Info("add-on version recording enabled", "branch", opts.Branch)
	}

	srv := &http.Server{
		Addr:              bindAddr,
		Handler:           web.New(reconciler),
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
	}

	// The webhook trigger exists only when a secret is configured. Both
	// stay nil otherwise, and a receive on a nil channel never fires, so
	// the select below needs no "is it enabled" branch.
	var hookSrv *http.Server
	var hookServeErr chan error
	if opts.WebhookSecret != "" {
		hookSrv = &http.Server{
			Addr:              hookBindAddr,
			Handler:           hook.New(ctx, reconciler, opts.WebhookSecret),
			ReadHeaderTimeout: readHeaderTimeout,
			ReadTimeout:       readTimeout,
			WriteTimeout:      writeTimeout,
			IdleTimeout:       idleTimeout,
		}
		hookServeErr = make(chan error, 1)
		go func() { hookServeErr <- hookSrv.ListenAndServe() }()
		slog.Info("webhook trigger enabled", "addr", hookBindAddr)
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.ListenAndServe() }()

	select {
	case err := <-serveErr:
		if err != nil && err != http.ErrServerClosed {
			slog.Error("fatal: cannot start the web server, is another copy of the agent already running?",
				"addr", bindAddr, "error", err)
			return 1
		}
	case err := <-hookServeErr:
		if err != nil && err != http.ErrServerClosed {
			slog.Error("fatal: cannot start the webhook server, is another copy of the agent already running?",
				"addr", hookBindAddr, "error", err)
			return 1
		}
	case <-ctx.Done():
		slog.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			slog.Warn("graceful shutdown did not complete in time", "error", err)
		}
		if hookSrv != nil {
			if err := hookSrv.Shutdown(shutdownCtx); err != nil {
				slog.Warn("webhook server graceful shutdown did not complete in time", "error", err)
			}
		}
		awaitLoops(loopDone, addonUpdateDone)

		waitCtx, waitCancel := context.WithTimeout(context.Background(), waitIdleGrace)
		if err := reconciler.WaitIdle(waitCtx); err != nil {
			slog.Warn("exiting with an operation still in flight; state may lag live changes", "error", err)
		}
		waitCancel()
	}

	return 0
}

// awaitLoops waits for the background loops to return after the root
// context is cancelled, naming whichever is still running when the shared
// window closes.
//
// A context rather than a timer channel, since a fired timer delivers
// once; the non-blocking pre-check keeps a loop that already stopped from
// losing the random pick once the window is spent.
func awaitLoops(loopDone, addonUpdateDone <-chan struct{}) {
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	for _, loop := range []struct {
		name string
		done <-chan struct{}
	}{
		{"reconcile", loopDone},
		{"add-on update", addonUpdateDone},
	} {
		select {
		case <-loop.done:
			continue
		default:
		}
		select {
		case <-loop.done:
		case <-ctx.Done():
			slog.Warn(loop.name + " loop did not stop within the shutdown window")
		}
	}
}

// configureEncryption turns the age_key option into the process-wide
// Crypter and sets gitsync's encryption switch - the only place the two
// are set together, since the switch on with no Crypter is what would push
// a plaintext secret (gitsync fails closed on it anyway).
//
// A key that cannot be used is returned as an error, fatal in run(); no
// error here carries the key.
func configureEncryption(ageKey string) (*sopscrypt.Crypter, error) {
	if strings.TrimSpace(ageKey) == "" {
		gitsync.SetEncryptionEnabled(false)
		return nil, nil
	}
	crypter, err := sopscrypt.New(ageKey)
	if err != nil {
		return nil, err
	}
	// At startup, because the first import would otherwise find sops
	// missing only after writing plaintext secrets into the worktree.
	probeCtx, cancel := context.WithTimeout(context.Background(), sopsProbeTimeout)
	defer cancel()
	if err := crypter.Probe(probeCtx); err != nil {
		return nil, err
	}
	gitsync.SetEncryptionEnabled(true)
	return crypter, nil
}

// logLevel reads LOG_LEVEL, defaulting to info.
func logLevel() slog.Level {
	switch strings.ToUpper(os.Getenv("LOG_LEVEL")) {
	case "DEBUG":
		return slog.LevelDebug
	case "WARNING", "WARN":
		return slog.LevelWarn
	case "ERROR", "CRITICAL":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
