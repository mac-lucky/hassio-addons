// Command ha-gitops-agent syncs Home Assistant configuration from a git
// repository: wires options, the reconciler and the ingress web UI, and
// runs the reconcile loop.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/failmemory"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/gitsync"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/hook"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/options"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/recon"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/secretref"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/web"
)

// version is stamped at build time via -ldflags "-X main.version=...".
var version = "dev"

const (
	// optionsPath is where Supervisor writes the add-on's options.
	optionsPath = "/data/options.json"

	// hashKeyPath persists failmemory's HMAC key, beside state.json but
	// never inside it: state.json is documented as safe to share, and the
	// key is what keeps its hashes from verifying secret guesses offline.
	hashKeyPath = "/data/failmemory.key"

	// bindAddr must match ingress_port in config.yaml.
	bindAddr = "0.0.0.0:8099"

	// hookBindAddr must match config.yaml's "8098/tcp" port. Started only
	// when opts.WebhookSecret is set; ingress only ever reaches bindAddr.
	hookBindAddr = "0.0.0.0:8098"

	readHeaderTimeout = 5 * time.Second
	readTimeout       = 30 * time.Second
	writeTimeout      = 30 * time.Second
	idleTimeout       = 120 * time.Second

	// waitIdleGrace is extra time, on top of shutdownTimeout, for an
	// in-flight apply/rollback: those run detached from the request, so
	// only recon.WaitIdle sees them. Deliberately NOT the apply's own
	// worst case (a restart-mode health probe alone can run 300s):
	// Supervisor's container stop kills the process on its own schedule
	// regardless, so a longer wait here only defers the same cut. What
	// makes the cut survivable is the persisted rollback pointer - the
	// stash is on disk before the first write, and Roll Back works after
	// a restart.
	waitIdleGrace = 30 * time.Second
)

// shutdownTimeout bounds both HTTP servers' graceful shutdown and, in
// awaitLoops, the wait for the two background loops to return. A var, not
// a const, so awaitLoops' tests can shrink it.
var shutdownTimeout = 5 * time.Second

// Startup secret:// resolution: how often an unresolved reference is
// retried, how often the wait is logged, and the config root whose
// secrets.yaml answers. Vars so awaitSecretRefs' tests can shrink them.
var (
	secretRetryInterval = 10 * time.Second
	secretLogInterval   = time.Minute
	secretsRoot         = recon.ConfigRoot
)

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

	// Before anything reads a path: syncing with an exclusion dropped would
	// write or delete exactly what the user asked to protect.
	if err := gitsync.SetUserExclusions(opts.ExcludePaths); err != nil {
		slog.Error("fatal: the exclude_paths option is invalid", "error", err)
		return 1
	}
	if len(opts.ExcludePaths) > 0 {
		slog.Info("extra exclusions configured", "exclude_paths", strings.Join(opts.ExcludePaths, ", "))
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// The ingress UI comes up first, on a page saying what startup waits
	// for; the dashboard replaces it once the reconciler exists.
	// A copy: opts takes the resolved credentials below while a request
	// may still be reading this one.
	waitOpts := opts
	var waitingFor atomic.Pointer[string]
	ui := &uiHandler{}
	ui.set(web.Waiting(func() recon.Status { return waitingStatus(waitOpts, waitingFor.Load()) }))
	srv := &http.Server{
		Addr:              bindAddr,
		Handler:           ui,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.ListenAndServe() }()

	// secret:// options resolve against the live secrets.yaml, which on a
	// fresh box may not exist until the add-on rendering it has run, so an
	// unresolvable one is waited for rather than fatal. A malformed one
	// still is - no wait fixes a typo - and a reference never falls back
	// to its literal text, which would authenticate as "secret://...".
	resolved, err := awaitSecretRefs(ctx, opts, serveErr, func(reason string) { waitingFor.Store(&reason) })
	if err != nil {
		if ctx.Err() != nil {
			slog.Info("shutting down while waiting for secrets.yaml")
			shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
			defer cancel()
			_ = srv.Shutdown(shutdownCtx)
			return 0
		}
		if errors.Is(err, errServeStopped) {
			slog.Error("fatal: cannot start the web server, is another copy of the agent already running?",
				"addr", bindAddr, "error", err)
			return 1
		}
		slog.Error("fatal: cannot resolve a secret reference in the add-on options", "error", err)
		return 1
	}
	opts = resolved

	// Not fatal: a failed load keeps hashing in the legacy unkeyed form,
	// stable across restarts, so nothing replans - state.json just stays
	// as guessable as it was before the key existed.
	if err := failmemory.LoadKey(hashKeyPath); err != nil {
		slog.Warn("hash key unavailable; state hashes stay unkeyed", "error", err)
	}

	reconciler := recon.New(opts, recon.Deps{})
	ui.set(web.New(reconciler))

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

	// The webhook trigger exists only when a secret is configured. Both
	// stay nil otherwise, and a receive on a nil channel never fires, so
	// the select below needs no "is it enabled" branch.
	var hookSrv *http.Server
	var hookServeErr chan error
	if opts.WebhookSecret != "" && len(opts.WebhookSecret) < hook.MinSecretLen {
		// Refuse to expose an endpoint gated by a guessable secret; the
		// agent still runs, only the trigger stays off.
		slog.Error("webhook trigger DISABLED: webhook_secret is shorter than the minimum",
			"min_length", hook.MinSecretLen)
		opts.WebhookSecret = ""
	}
	if opts.WebhookSecret != "" {
		hookSrv = &http.Server{
			Addr:              hookBindAddr,
			Handler:           hook.New(ctx, reconciler, opts.WebhookSecret, opts.Branch),
			ReadHeaderTimeout: readHeaderTimeout,
			ReadTimeout:       readTimeout,
			WriteTimeout:      writeTimeout,
			IdleTimeout:       idleTimeout,
		}
		hookServeErr = make(chan error, 1)
		go func() { hookServeErr <- hookSrv.ListenAndServe() }()
		slog.Info("webhook trigger enabled", "addr", hookBindAddr)
	}

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

// uiHandler serves whichever handler was stored last: the waiting page
// until the reconciler exists, the dashboard from then on. Swapped once and
// read per request, so an atomic pointer rather than a lock.
type uiHandler struct {
	current atomic.Pointer[http.Handler]
}

func (u *uiHandler) set(h http.Handler) { u.current.Store(&h) }

func (u *uiHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	(*u.current.Load()).ServeHTTP(w, r)
}

// waitingStatus is what the waiting page shows: the repository it will
// sync and why it has not started. reason is nil before the first attempt
// has finished, a moment at most.
func waitingStatus(opts options.Options, reason *string) recon.Status {
	waiting := "resolving secret:// references in the add-on options"
	if reason != nil {
		waiting = *reason
	}
	return recon.Status{
		State:            recon.StateWaiting,
		Configured:       opts.RepoURL != "",
		DryRun:           opts.DryRun,
		RepoURL:          opts.RepoURL,
		Branch:           opts.Branch,
		IntervalMinutes:  opts.IntervalMinutes,
		WaitingForSecret: waiting,
	}
}

// errServeStopped is awaitSecretRefs' report that the web server stopped
// while it waited.
var errServeStopped = errors.New("the web server stopped while waiting for secrets.yaml")

// awaitSecretRefs resolves opts' secret:// references against the live
// secrets.yaml under secretsRoot, retrying every secretRetryInterval while
// a well-formed one cannot be answered yet. Each attempt reads the file
// afresh, and waiting gets the reason every time, which names the option,
// the key and why - never a value. Logged once per secretLogInterval.
//
// Returns the resolved options, the first error that waiting cannot fix
// (a malformed reference), ctx's error once it is cancelled, or the web
// server's if it stops meanwhile: nothing would show the wait then.
func awaitSecretRefs(ctx context.Context, opts options.Options, abort <-chan error, waiting func(string)) (options.Options, error) {
	var lastLogged time.Time
	for {
		resolved := opts
		err := resolved.ResolveSecretRefs(secretref.NewResolver(secretsRoot))
		if err == nil {
			if !lastLogged.IsZero() {
				slog.Info("startup: every secret:// reference resolved, starting")
			}
			return resolved, nil
		}
		var pending *options.PendingSecretError
		if !errors.As(err, &pending) {
			return options.Options{}, err
		}
		reason := fmt.Sprintf("waiting for secrets.yaml key '%s' (%s): %v", pending.Name, pending.Option, pending.Err)
		waiting(reason)
		if lastLogged.IsZero() || time.Since(lastLogged) >= secretLogInterval {
			slog.Warn("startup: "+reason, "retry_every", secretRetryInterval.String())
			lastLogged = time.Now()
		}
		select {
		case <-ctx.Done():
			return options.Options{}, ctx.Err()
		case err := <-abort:
			return options.Options{}, fmt.Errorf("%w: %v", errServeStopped, err)
		case <-time.After(secretRetryInterval):
		}
	}
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
