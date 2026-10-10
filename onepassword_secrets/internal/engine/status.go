package engine

import (
	"time"

	"github.com/mac-lucky/hassio-addons/onepassword_secrets/internal/connectd"
	"github.com/mac-lucky/hassio-addons/onepassword_secrets/internal/opconnect"
	"github.com/mac-lucky/hassio-addons/onepassword_secrets/internal/state"
)

// Overall states, also the sensor's state.
const (
	StateUnconfigured = "unconfigured"
	StateStarting     = "starting"
	StateHealthy      = "ok"
	StateAttention    = "attention"
	StateError        = "error"
)

// Row states for one key.
const (
	RowSynced    = "synced"
	RowPending   = "pending"   // dry run: would be written
	RowMissing   = "missing"   // referenced, in neither 1Password nor the file
	RowUnmanaged = "unmanaged" // in the file only: not migrated yet
	RowConflict  = "conflict"  // label used twice, or a broken reference
	RowUnused    = "unused"    // in 1Password, referenced nowhere
)

// Status is everything the UI and the sensor show. It holds no value.
type Status struct {
	Version  string
	State    string
	Headline string
	Detail   string
	DryRun   bool
	Embedded bool
	Syncing  bool
	// Phase is what a long operation is doing right now ("Restarting
	// Home Assistant"), "" otherwise.
	Phase string

	Connect ConnectInfo
	Token   TokenInfo

	LastSync   time.Time
	NextSync   time.Time
	LastChange time.Time

	Vaults   []VaultInfo
	Files    []FileInfo
	Secrets  []SecretRow
	Problems []Problem
	Counts   Counts

	RestartPending []string
	Setup          []SetupStep
	Activity       []state.Entry
}

// ConnectInfo is the Connect server's health.
type ConnectInfo struct {
	URL           string // external server only
	ServerVersion string
	Reachable     bool
	Synced        bool
	Dependencies  []opconnect.Dependency
	Processes     []connectd.ProcState
	Error         string
}

// TokenInfo is the access token's expiry, from its unverified claims.
type TokenInfo struct {
	Known     bool
	ExpiresAt time.Time
	DaysLeft  int
}

// VaultInfo is one vault the add-on reads.
type VaultInfo struct {
	Name     string
	ID       string
	Items    int
	Discover bool
	Missing  bool
}

// FileInfo is one rendered secrets file.
type FileInfo struct {
	Path      string
	Exists    bool
	Keys      int
	Managed   int
	Unmanaged []string
	Orphaned  []string
	// Pending* is what a dry run would do.
	PendingAdded   []string
	PendingChanged []string
	PendingRemoved []string
	Error          string
}

// Use is one consumer of a key.
type Use struct {
	// Kind is "ha", "addon", "esphome" or "file".
	Kind   string
	Label  string
	File   string
	Line   int
	Domain string
}

// SecretRow is one key.
type SecretRow struct {
	Key        string
	State      string
	Source     string
	VaultName  string
	ItemTitle  string
	FieldLabel string
	Ref        string
	Structured bool
	Files      []string
	UsedBy     []Use
	ChangedAt  time.Time

	Rotation    string
	Interval    string
	RotatedAt   time.Time
	Due         time.Time
	Overdue     bool
	DueSoon     bool
	RotationPct int
}

// Problem is something to fix, with the fix.
type Problem struct {
	Severity string // "error", "warning", "info"
	Title    string
	Detail   string
	Fix      string
	Key      string
}

// Counts are the summary tiles.
type Counts struct {
	Keys      int
	Managed   int
	Missing   int
	Conflicts int
	Unmanaged int
	Unused    int
	Overdue   int
	DueSoon   int
	Errors    int
	Warnings  int
}

// SetupStep is one row of the first-run checklist.
type SetupStep struct {
	Label  string
	Done   bool
	Detail string
}
