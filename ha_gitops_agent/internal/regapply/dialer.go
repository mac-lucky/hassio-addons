// Package regapply executes a registry reconciliation plan over Home
// Assistant's Core WebSocket API, with a stash-based rollback path.
// registries.Plan decides what should change; this package does it, one
// op at a time over a connection dialed from a Dialer. It never reads or
// writes state.json - managed (state.RegistryManaged) is mutated in place
// and the caller persists it afterward.
//
// # Stash format
//
// <stashDir>/registry_stash.json holds {"ops": [...]}: at every instant,
// exactly the ops CONFIRMED executed so far, never a plan of what is
// about to be attempted. ApplyPlan resets it, then rewrites the whole
// file after each op's WS call returns successfully.
//
//	{"kind": "create", "rtype": "floor", "key": "ground",
//	 "live_id": "abc123", "live_object": null, "forward_params": null}
//	{"kind": "update", "rtype": "area", "key": "living_room",
//	 "live_id": "def456", "live_object": {...as HA returned it...},
//	 "forward_params": {...only the fields this update sent...}}
//
// A create entry's live_id is always the real id Home Assistant assigned,
// never a placeholder, so a disk-only RollbackRegistry after a crash
// mid-apply can never invert an op that did not genuinely run.
//
// An update-inverse restores forward_params' fields one at a time out of
// live_object, never live_object wholesale: that snapshot carries
// server-generated created_at/modified_at which the registries' own
// create/update schemas reject outright, and a per-field restore cannot
// clobber a field some concurrent process changed in between. A
// delete-inverse recreates from the full live_object, stripped of id and
// those same server-generated fields.
//
// Inverse-replay drops an entry from the stash BEFORE attempting its
// inverse, so any failure under-reverts rather than risking a
// double-invert (recreating or re-deleting an already-handled object) on
// a retry; managed, not this stash, is the durable record of ownership,
// so an inverse that silently did not run is picked up by the next
// ordinary reconcile.
//
// # Go-specific design adaptation: the Dialer
//
// coder/websocket closes the underlying connection on ANY error from a
// Conn method, a plain Cmd timeout included (see internal/wsclient/
// dial.go), so a client that errored once cannot be reused. ApplyPlan and
// RollbackRegistry therefore take a Dialer and redial for inverse-replay;
// a failed redial stops the replay with RolledBack=false, which the
// write-before-invert polarity above makes safe to retry later.
package regapply

import (
	"context"
	"errors"
	"time"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/wsclient"
)

// WSClient is the subset of *wsclient.Client this package needs, small
// enough for tests to inject a fake. CmdTimeout carries a caller-chosen
// budget for the one command family whose work is long rather than slow (a
// HACS download unpacks release archives before answering); zero there
// means the client's own default.
type WSClient interface {
	Cmd(ctx context.Context, msgType string, params map[string]any) (any, error)
	CmdTimeout(ctx context.Context, msgType string, params map[string]any, timeout time.Duration) (any, error)
	Close()
}

// Dialer produces a freshly connected WSClient - see the package doc
// comment for why ApplyPlan and RollbackRegistry take one of these.
type Dialer func(ctx context.Context) (WSClient, error)

// NewDialer returns a Dialer that opens a real Home Assistant Core
// WebSocket connection via wsclient.New(opts...).Dial(ctx).
func NewDialer(opts ...wsclient.Option) Dialer {
	return func(ctx context.Context) (WSClient, error) {
		c := wsclient.New(opts...)
		if err := c.Dial(ctx); err != nil {
			return nil, err
		}
		return c, nil
	}
}

// lazyConn is a connection dialed on first use and redialed after the
// transport dies under it. Dialing lazily keeps a pure-bookkeeping plan
// (internal/hacs' adopt sends nothing at all) working while the box is
// briefly unreachable. A transport or timeout error retires the client
// (dropIfDead) because coder/websocket has already closed the socket under
// it, and a cached one that saw a single timeout answers every LATER
// command with "use of closed network connection" - failing ops that were
// never attempted. A failed DIAL is remembered for the rest of the call,
// so a box that is down is dialed once rather than once per op.
type lazyConn struct {
	dialer Dialer
	ws     WSClient
	err    error
	tried  bool
}

func newLazyConn(dialer Dialer) *lazyConn { return &lazyConn{dialer: dialer} }

// cmd runs one command over the connection, dialing it if needed, and
// retires the connection if the failure was one it cannot survive. timeout
// goes to WSClient.CmdTimeout, where zero means the client's own default -
// a long command (see hacsDownloadTimeout) passes its own budget.
func (l *lazyConn) cmd(ctx context.Context, msgType string, params map[string]any, timeout time.Duration) (any, error) {
	ws, err := l.get(ctx)
	if err != nil {
		return nil, err
	}
	result, err := ws.CmdTimeout(ctx, msgType, params, timeout)
	l.dropIfDead(err)
	return result, err
}

func (l *lazyConn) get(ctx context.Context) (WSClient, error) {
	if l.tried {
		return l.ws, l.err
	}
	l.tried = true
	if l.dialer == nil {
		l.err = errors.New("no websocket dialer was configured for this call")
		return nil, l.err
	}
	l.ws, l.err = l.dialer(ctx)
	return l.ws, l.err
}

// dropIfDead retires the cached client when err is one that closed the
// connection underneath it. Any other failure (a refused or unknown
// command, a protocol error) leaves the connection perfectly usable.
func (l *lazyConn) dropIfDead(err error) {
	if err == nil || !isTransportOrTimeoutError(err) {
		return
	}
	if l.ws != nil {
		l.ws.Close()
	}
	l.ws, l.err, l.tried = nil, nil, false
}

func (l *lazyConn) close() {
	if l.ws != nil {
		l.ws.Close()
		l.ws = nil
	}
}
