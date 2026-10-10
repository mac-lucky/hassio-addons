// Package engine is the sync loop: read 1Password through Connect, work out
// which key belongs in which secrets file, render the files, and when a
// value really changed, refresh whatever uses it - reload the integration,
// restart the add-on, or (guarded by check_config, a health probe and a
// rollback) restart Home Assistant. It is read-only toward 1Password.
package engine

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/mac-lucky/hassio-addons/onepassword_secrets/internal/connectd"
	"github.com/mac-lucky/hassio-addons/onepassword_secrets/internal/ha"
	"github.com/mac-lucky/hassio-addons/onepassword_secrets/internal/opconnect"
	"github.com/mac-lucky/hassio-addons/onepassword_secrets/internal/options"
	"github.com/mac-lucky/hassio-addons/onepassword_secrets/internal/refscan"
	"github.com/mac-lucky/hassio-addons/onepassword_secrets/internal/state"
)

// Connect is the subset of *opconnect.Client the engine uses.
type Connect interface {
	Health(ctx context.Context) (opconnect.Health, error)
	Vaults(ctx context.Context) ([]opconnect.Vault, error)
	Items(ctx context.Context, vaultID string) ([]opconnect.ItemSummary, error)
	Item(ctx context.Context, vaultID, itemID string) (opconnect.Item, error)
}

// HA is the subset of *ha.Client the engine uses.
type HA interface {
	CheckConfig(ctx context.Context) (bool, string)
	Services(ctx context.Context) (map[string]map[string]bool, error)
	CallService(ctx context.Context, domain, service string, data map[string]any) error
	Probe(ctx context.Context) bool
	WaitHealthy(ctx context.Context, timeout time.Duration) bool
	RestartCore(ctx context.Context) error
	FireEvent(ctx context.Context, eventType string, data map[string]any) error
	SetState(ctx context.Context, entityID, st string, attrs map[string]any) error
	HasState(ctx context.Context, entityID string) (bool, error)
	Notify(ctx context.Context, id, title, message string) error
	Dismiss(ctx context.Context, id string) error
	Addons(ctx context.Context) ([]ha.Addon, error)
	AddonOptions(ctx context.Context, slug string) (map[string]any, error)
	RestartAddon(ctx context.Context, slug string) error
}

// Processes reports the embedded Connect server's processes; nil for an
// external server.
type Processes interface {
	Status() []connectd.ProcState
}

// Config wires an Engine.
type Config struct {
	Options  options.Options
	Version  string
	SelfSlug string
	// Root is the Home Assistant config directory.
	Root string
	// Data paths, all under /data in the add-on.
	StatePath    string
	KeyPath      string
	ActivityPath string
	PreviousDir  string

	// Timings; zero picks the production value.
	SupervisorSecretsDelay time.Duration
	RestartDownTimeout     time.Duration
	RestartUpTimeout       time.Duration
	AddonRefsTTL           time.Duration
	// Now is the clock; nil is time.Now.
	Now func() time.Time
}

func (c *Config) defaults() {
	if c.SupervisorSecretsDelay == 0 {
		// Supervisor re-reads secrets.yaml when an add-on starts, but at
		// most once every 60 s (HomeAssistantSecrets._read_secrets).
		c.SupervisorSecretsDelay = 65 * time.Second
	}
	if c.RestartDownTimeout == 0 {
		c.RestartDownTimeout = 90 * time.Second
	}
	if c.RestartUpTimeout == 0 {
		c.RestartUpTimeout = 10 * time.Minute
	}
	if c.AddonRefsTTL == 0 {
		c.AddonRefsTTL = 5 * time.Minute
	}
	if c.Now == nil {
		c.Now = time.Now
	}
}

// startingPoll is the poll interval while Connect has not synced yet.
const startingPoll = 10 * time.Second

// Engine runs the loop. Status is safe from any goroutine.
type Engine struct {
	cfg     Config
	connect Connect
	ha      HA
	procs   Processes

	key []byte
	st  *state.State
	act *state.Activity

	runMu sync.Mutex // one cycle (or restart) at a time
	wake  chan struct{}

	mu     sync.Mutex
	status Status

	// Caches, touched only under runMu.
	items       map[string]cachedItem
	scan        refscan.Result
	scanned     bool
	addonRefs   []refscan.AddonRef
	addons      []ha.Addon
	addonRefsAt time.Time
	lastErr     string
	lastDryRun  string
	// addonsLoaded is set once other add-ons' options were read: until
	// then nothing is known about who uses a key from add-on options, so
	// no previously written key may be removed.
	addonsLoaded  bool
	lastPublished string
	publishedAt   time.Time
	tokenNotified bool
}

type cachedItem struct {
	item opconnect.Item
}

// New builds an Engine. Load errors (an unreadable state file) are
// returned so main can refuse to start rather than forget what it manages.
func New(cfg Config, connect Connect, haClient HA, procs Processes) (*Engine, error) {
	cfg.defaults()
	key, err := state.LoadOrCreateKey(cfg.KeyPath)
	if err != nil {
		return nil, err
	}
	st, err := state.Load(cfg.StatePath)
	if err != nil {
		return nil, err
	}
	e := &Engine{
		cfg:     cfg,
		connect: connect,
		ha:      haClient,
		procs:   procs,
		key:     key,
		st:      st,
		act:     state.OpenActivity(cfg.ActivityPath),
		wake:    make(chan struct{}, 1),
		items:   map[string]cachedItem{},
	}
	e.cleanPrevious()
	e.status = Status{
		Version:        cfg.Version,
		State:          StateStarting,
		Headline:       "Starting",
		DryRun:         cfg.Options.DryRun,
		Embedded:       cfg.Options.Embedded(),
		Token:          tokenInfo(cfg.Options.ConnectToken, cfg.Now()),
		RestartPending: append([]string(nil), st.RestartPending...),
		LastChange:     st.LastWrite,
	}
	if !cfg.Options.Configured() {
		e.status.State = StateUnconfigured
		e.status.Headline = "Not set up yet"
		e.status.Setup = setupSteps(cfg.Options, Status{}, nil)
	}
	return e, nil
}

// Status returns a snapshot for the UI.
func (e *Engine) Status() Status {
	e.mu.Lock()
	defer e.mu.Unlock()
	s := e.status
	if e.procs != nil {
		s.Connect.Processes = e.procs.Status()
	}
	s.Activity = e.act.Recent(40)
	return s
}

// History returns the newest n activity entries.
func (e *Engine) History(n int) []state.Entry { return e.act.Recent(n) }

// KeyHistory returns the newest n activity entries naming key.
func (e *Engine) KeyHistory(key string, n int) []state.Entry { return e.act.ForKey(key, n) }

// SyncNow is the UI's button: like Wake, and it also retries a change
// that was held back after a failed apply, and re-reads add-on options.
func (e *Engine) SyncNow() {
	go func() {
		e.runMu.Lock()
		if e.st.HeldBack != "" {
			e.st.HeldBack, e.st.HeldBackReason = "", ""
			if err := e.st.Save(e.cfg.StatePath); err != nil {
				slog.Warn("saving state failed", "error", err)
			}
		}
		e.addonRefsAt = time.Time{}
		e.runMu.Unlock()
		e.Wake()
	}()
}

// Wake asks for a sync as soon as the current one (if any) ends.
func (e *Engine) Wake() {
	select {
	case e.wake <- struct{}{}:
	default:
	}
}

// Run syncs once at start, then every poll interval or on Wake, until ctx
// ends.
func (e *Engine) Run(ctx context.Context) {
	interval := time.Duration(e.cfg.Options.PollIntervalSeconds) * time.Second
	for {
		e.SyncOnce(ctx)
		wait := interval
		if e.Status().State == StateStarting && wait > startingPoll {
			// Connect's first sync usually takes seconds, not a minute.
			wait = startingPoll
		}
		e.setNext(e.cfg.Now().Add(wait))
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		case <-e.wake:
			timer.Stop()
		}
	}
}

func (e *Engine) setNext(t time.Time) {
	e.mu.Lock()
	e.status.NextSync = t
	e.mu.Unlock()
}

func (e *Engine) setSyncing(on bool) {
	e.mu.Lock()
	e.status.Syncing = on
	e.mu.Unlock()
}

// logf records an activity entry and logs it.
func (e *Engine) record(kind state.Kind, title, detail string, keys []string) {
	e.act.Add(state.Entry{Kind: kind, Title: title, Detail: detail, Keys: keys})
	level := slog.LevelInfo
	if kind == state.KindError {
		level = slog.LevelError
	}
	slog.Log(context.Background(), level, title, "detail", detail, "keys", keys)
}

func tokenInfo(token string, now time.Time) TokenInfo {
	exp, ok := opconnect.TokenExpiry(token)
	if !ok {
		return TokenInfo{}
	}
	return TokenInfo{Known: true, ExpiresAt: exp, DaysLeft: int(exp.Sub(now).Hours() / 24)}
}
