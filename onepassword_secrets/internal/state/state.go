// Package state is what the add-on remembers across restarts, under
// /data: which keys it wrote to which file, a keyed fingerprint of each
// value (never the value, never a plain hash), and the activity log the
// UI shows. All of it is in Supervisor backups, which is why none of it
// may hold a secret.
package state

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/mac-lucky/hassio-addons/onepassword_secrets/internal/fsx"
)

// Version is the state file's schema version.
const Version = 1

// KeyState is one key's memory.
type KeyState struct {
	// FP is the HMAC fingerprint of the value last written.
	FP string `json:"fp"`
	// ChangedAt is when this add-on last saw the value change.
	ChangedAt time.Time `json:"changed_at"`
	// Source is where it came from, "vault > item > field".
	Source string `json:"source,omitempty"`
}

// State is state.json.
type State struct {
	Version int                 `json:"version"`
	Keys    map[string]KeyState `json:"keys"`
	// Managed is, per config-relative file, the keys this add-on wrote
	// there. Only these may ever be removed from that file.
	Managed map[string][]string `json:"managed"`
	// RestartPending names what is waiting for a Home Assistant restart
	// when core_restart is notify (or an automatic restart failed).
	RestartPending []string  `json:"restart_pending,omitempty"`
	LastWrite      time.Time `json:"last_write,omitzero"`
	// Pending is a written change whose consumers are not all refreshed
	// yet. Saved before the files are written and cleared once the
	// refresh is through, so a restart in between resumes it instead of
	// leaving a reload or add-on restart undone.
	Pending *PendingRefresh `json:"pending,omitempty"`
	// HeldBack is the signature of a change that failed to apply and was
	// rolled back; the same change is not retried until Sync now or a new
	// value. HeldBackReason says why, in words safe to show.
	HeldBack       string `json:"held_back,omitempty"`
	HeldBackReason string `json:"held_back_reason,omitempty"`
}

// PendingRefresh is a change in flight.
type PendingRefresh struct {
	// Changed is, per config-relative file, the keys whose value moved.
	Changed map[string][]string `json:"changed"`
	// Written are the files written, with their previous copies under
	// /data/previous for a rollback.
	Written []PendingFile `json:"written"`
	// Stage is "all", or "addons" once reloads and any Core restart are
	// done and only add-on restarts and notices are left.
	Stage string `json:"stage"`
	// Signature is the change's signature, held back if it fails.
	Signature string `json:"signature"`
	// Before is the state as it was before the write.
	Before Snapshot  `json:"before"`
	At     time.Time `json:"at"`
}

// Keys are every changed key, sorted, once.
func (p *PendingRefresh) Keys() []string {
	set := map[string]bool{}
	for _, keys := range p.Changed {
		for _, k := range keys {
			set[k] = true
		}
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// PendingFile is one written file.
type PendingFile struct {
	Path     string `json:"path"`
	Previous string `json:"previous,omitempty"`
	Existed  bool   `json:"existed"`
}

// Snapshot is the part of State a rollback restores.
type Snapshot struct {
	Keys      map[string]KeyState `json:"keys"`
	Managed   map[string][]string `json:"managed"`
	LastWrite time.Time           `json:"last_write,omitzero"`
}

// Snap copies the restorable part of s.
func (s *State) Snap() Snapshot {
	out := Snapshot{Keys: map[string]KeyState{}, Managed: map[string][]string{}, LastWrite: s.LastWrite}
	for k, v := range s.Keys {
		out.Keys[k] = v
	}
	for k, v := range s.Managed {
		out.Managed[k] = append([]string(nil), v...)
	}
	return out
}

// Restore puts a snapshot back.
func (s *State) Restore(snap Snapshot) {
	s.Keys = snap.Keys
	s.Managed = snap.Managed
	s.LastWrite = snap.LastWrite
	if s.Keys == nil {
		s.Keys = map[string]KeyState{}
	}
	if s.Managed == nil {
		s.Managed = map[string][]string{}
	}
}

// New returns an empty state.
func New() *State {
	return &State{Version: Version, Keys: map[string]KeyState{}, Managed: map[string][]string{}}
}

// ManagedSet returns file's managed keys as a set.
func (s *State) ManagedSet(file string) map[string]bool {
	out := map[string]bool{}
	for _, k := range s.Managed[file] {
		out[k] = true
	}
	return out
}

// SetManaged records file's managed keys.
func (s *State) SetManaged(file string, keys map[string]bool) {
	list := make([]string, 0, len(keys))
	for k := range keys {
		list = append(list, k)
	}
	sort.Strings(list)
	if len(list) == 0 {
		delete(s.Managed, file)
		return
	}
	s.Managed[file] = list
}

// Load reads path; a missing file is an empty state.
func Load(path string) (*State, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- fixed /data path
	if errors.Is(err, fs.ErrNotExist) {
		return New(), nil
	}
	if err != nil {
		return nil, err
	}
	s := New()
	if err := json.Unmarshal(data, s); err != nil {
		return nil, fmt.Errorf("state: %s is not valid JSON", path)
	}
	if s.Keys == nil {
		s.Keys = map[string]KeyState{}
	}
	if s.Managed == nil {
		s.Managed = map[string][]string{}
	}
	s.Version = Version
	return s, nil
}

// Save writes s to path atomically, 0600.
func (s *State) Save(path string) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return fsx.WriteFileAtomic(path, append(data, '\n'), 0o600)
}

// LoadOrCreateKey returns the fingerprint key at path, creating 32 random
// bytes (0600) the first time.
func LoadOrCreateKey(path string) ([]byte, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- fixed /data path
	if err == nil && len(data) >= 32 {
		return data, nil
	}
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if err := fsx.WriteFileAtomic(path, key, 0o600); err != nil {
		return nil, err
	}
	return key, nil
}

// Kind classifies an activity entry for the UI.
type Kind string

// Activity kinds.
const (
	KindChange  Kind = "change"
	KindRefresh Kind = "refresh"
	KindRestart Kind = "restart"
	KindError   Kind = "error"
	KindInfo    Kind = "info"
	KindDryRun  Kind = "dry_run"
)

// Entry is one activity log line. Keys and text only, never a value.
type Entry struct {
	Time   time.Time `json:"time"`
	Kind   Kind      `json:"kind"`
	Title  string    `json:"title"`
	Keys   []string  `json:"keys,omitempty"`
	Detail string    `json:"detail,omitempty"`
}

// Activity is the bounded log: the newest entries in memory, all of them
// appended to a JSONL file that is cut back to its newer half once it
// passes maxFileBytes.
type Activity struct {
	mu   sync.Mutex
	path string
	ring []Entry
}

const (
	ringSize     = 300
	maxFileBytes = 512 << 10
)

// OpenActivity loads the newest entries from path ("" keeps memory only).
func OpenActivity(path string) *Activity {
	a := &Activity{path: path}
	if path == "" {
		return a
	}
	data, err := os.ReadFile(path) // #nosec G304 -- fixed /data path
	if err != nil {
		return a
	}
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var e Entry
		if json.Unmarshal(sc.Bytes(), &e) == nil && !e.Time.IsZero() {
			a.ring = append(a.ring, e)
		}
	}
	if len(a.ring) > ringSize {
		a.ring = a.ring[len(a.ring)-ringSize:]
	}
	return a
}

// Add appends e (Time defaults to now).
func (a *Activity) Add(e Entry) {
	if e.Time.IsZero() {
		e.Time = time.Now().UTC()
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.ring = append(a.ring, e)
	if len(a.ring) > ringSize {
		a.ring = a.ring[len(a.ring)-ringSize:]
	}
	if a.path == "" {
		return
	}
	line, err := json.Marshal(e)
	if err != nil {
		return
	}
	f, err := os.OpenFile(a.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600) // #nosec G304 -- fixed /data path
	if err != nil {
		return
	}
	_, _ = f.Write(append(line, '\n'))
	info, statErr := f.Stat()
	_ = f.Close()
	if statErr == nil && info.Size() > maxFileBytes {
		a.compactLocked()
	}
}

func (a *Activity) compactLocked() {
	keep := a.ring
	if len(keep) > ringSize/2 {
		keep = keep[len(keep)-ringSize/2:]
	}
	var buf bytes.Buffer
	for _, e := range keep {
		if line, err := json.Marshal(e); err == nil {
			buf.Write(line)
			buf.WriteByte('\n')
		}
	}
	_ = fsx.WriteFileAtomic(a.path, buf.Bytes(), 0o600)
}

// Recent returns up to n entries, newest first.
func (a *Activity) Recent(n int) []Entry {
	a.mu.Lock()
	defer a.mu.Unlock()
	if n <= 0 || n > len(a.ring) {
		n = len(a.ring)
	}
	out := make([]Entry, 0, n)
	for i := len(a.ring) - 1; i >= 0 && len(out) < n; i-- {
		out = append(out, a.ring[i])
	}
	return out
}

// ForKey returns up to n entries naming key, newest first.
func (a *Activity) ForKey(key string, n int) []Entry {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []Entry
	for i := len(a.ring) - 1; i >= 0 && len(out) < n; i-- {
		if slices.Contains(a.ring[i].Keys, key) {
			out = append(out, a.ring[i])
		}
	}
	return out
}
