// Package wsclient is a client for Home Assistant Core's WebSocket API,
// reached through the Supervisor proxy. It speaks only the generic auth +
// command/result envelope; the registry layers on top of it use it to
// reach storage collections that have no REST equivalent.
package wsclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/options"
)

// DefaultURL is the Core WebSocket endpoint, reachable through the
// Supervisor proxy from any add-on container.
const DefaultURL = "ws://supervisor/core/websocket"

// DefaultTimeout is how long one Client.Cmd waits for its response.
const DefaultTimeout = 10 * time.Second

// Conn is a connected transport, one frame at a time. Satisfied by
// dial.go's coder/websocket adapter and by any test fake.
type Conn interface {
	// Write sends one frame. Honors ctx's deadline/cancellation.
	Write(ctx context.Context, data []byte) error
	// Read blocks for the next frame. Must return an error satisfying
	// errors.Is(err, context.DeadlineExceeded) when ctx's deadline ended
	// the wait, so Client can tell a timeout from a transport failure.
	Read(ctx context.Context) ([]byte, error)
	// Close closes the connection. Client calls it once, but a second
	// call must not panic.
	Close() error
}

// Dialer builds a connected Conn for url; injectable so tests need no real
// socket. The production default is in dial.go.
type Dialer func(ctx context.Context, url string) (Conn, error)

// Client is a client for Home Assistant's Core WebSocket API: Dial (which
// completes the auth_required -> auth -> auth_ok handshake), then Cmd per
// command, then Close.
//
// Used strictly sequentially, one command in flight; each Cmd takes the
// next message id and waits for the response carrying it, discarding
// anything else as a stale response or an event push. Not safe for
// concurrent use.
type Client struct {
	url      string
	token    string
	hasToken bool
	dialer   Dialer
	timeout  time.Duration

	conn   Conn
	nextID atomic.Uint64
}

// Option configures a Client built by New.
type Option func(*Client)

// WithURL overrides DefaultURL.
func WithURL(url string) Option {
	return func(c *Client) { c.url = url }
}

// WithToken sets the Supervisor bearer token for the auth message.
// Without it the token comes from options.SupervisorToken lazily on Dial,
// so a Client can be built before SUPERVISOR_TOKEN is needed.
func WithToken(token string) Option {
	return func(c *Client) {
		c.token = token
		c.hasToken = true
	}
}

// WithDialer overrides the production dialer, so tests open no socket.
func WithDialer(d Dialer) Option {
	return func(c *Client) { c.dialer = d }
}

// WithTimeout overrides DefaultTimeout.
func WithTimeout(d time.Duration) Option {
	return func(c *Client) { c.timeout = d }
}

// New builds a Client without connecting; call Dial for that.
func New(opts ...Option) *Client {
	c := &Client{
		url:     DefaultURL,
		dialer:  defaultDialer,
		timeout: DefaultTimeout,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Dial connects and completes the auth handshake. Returns a *Error coded
// "transport", "auth_invalid" or "protocol_error"; a handshake that fails
// after connecting closes the connection before returning.
func (c *Client) Dial(ctx context.Context) error {
	conn, err := c.dialer(ctx, c.url)
	if err != nil {
		return newError("transport", err.Error())
	}
	c.conn = conn

	if err := c.authenticate(ctx); err != nil {
		c.Close()
		return err
	}
	return nil
}

// Close closes the connection if open. Best-effort and idempotent: a
// failure is logged, never returned, so cleanup cannot itself fail.
func (c *Client) Close() {
	if c.conn == nil {
		return
	}
	conn := c.conn
	c.conn = nil
	if err := conn.Close(); err != nil {
		slog.Debug("wsclient: error closing connection", "error", err)
	}
}

// Cmd sends {"id": <next id>, "type": msgType, <params...>} and returns
// the response's "result", which callers type-assert per command ([]any
// for */list, map[string]any for create/update, nil for delete).
//
// Returns a *Error on "success": false (code/message from the response's
// "error" field) or when no matching response arrives in time ("timeout").
func (c *Client) Cmd(ctx context.Context, msgType string, params map[string]any) (any, error) {
	return c.cmd(ctx, msgType, params, c.timeout)
}

// CmdTimeout is Cmd under a caller-chosen budget, for commands that do
// real work before answering - HACS's download fetches and unpacks release
// archives, which takes minutes on a Pi. The default timeout is a
// statement about the transport, and timing one of these out records a
// failure while the work is still running.
//
// A timeout of zero or less means the client's default. Every other
// guarantee is Cmd's.
func (c *Client) CmdTimeout(
	ctx context.Context, msgType string, params map[string]any, timeout time.Duration,
) (any, error) {
	if timeout <= 0 {
		timeout = c.timeout
	}
	return c.cmd(ctx, msgType, params, timeout)
}

func (c *Client) cmd(ctx context.Context, msgType string, params map[string]any, timeout time.Duration) (any, error) {
	if c.conn == nil {
		return nil, newError("transport", "not connected")
	}

	id := c.nextID.Add(1)
	msg := make(map[string]any, len(params)+2)
	for k, v := range params {
		msg[k] = v
	}
	msg["id"] = id
	msg["type"] = msgType

	if err := c.send(ctx, msg, timeout); err != nil {
		return nil, err
	}

	resp, err := c.recvMatching(ctx, id, timeout)
	if err != nil {
		return nil, err
	}

	success, _ := resp["success"].(bool)
	if !success {
		errObj, _ := resp["error"].(map[string]any)
		code, _ := errObj["code"].(string)
		if code == "" {
			code = "unknown_error"
		}
		message, _ := errObj["message"].(string)
		if message == "" {
			message = "command failed"
		}
		return nil, newError(code, message)
	}
	return resp["result"], nil
}

// authenticate runs the auth_required -> auth -> auth_ok handshake. The
// token goes into the outgoing auth message only, never to the logger.
func (c *Client) authenticate(ctx context.Context) error {
	first, err := c.recvRaw(ctx)
	if err != nil {
		return err
	}
	if t, _ := first["type"].(string); t != "auth_required" {
		return newError("protocol_error", fmt.Sprintf("expected auth_required, got %q", first["type"]))
	}

	token := c.token
	if !c.hasToken {
		resolved, err := options.SupervisorToken()
		if err != nil {
			// Only *Error may escape Client, so a missing token folds in
			// under "transport" rather than leaking a second error type.
			return newError("transport", err.Error())
		}
		token = resolved
	}

	if err := c.send(ctx, map[string]any{"type": "auth", "access_token": token}, c.timeout); err != nil {
		return err
	}

	resp, err := c.recvRaw(ctx)
	if err != nil {
		return err
	}
	msgType, _ := resp["type"].(string)
	switch msgType {
	case "auth_invalid":
		message, _ := resp["message"].(string)
		if message == "" {
			message = "invalid authentication"
		}
		return newError("auth_invalid", message)
	case "auth_ok":
		slog.Debug("wsclient: authenticated", "url", c.url)
		return nil
	default:
		return newError("protocol_error", fmt.Sprintf("expected auth_ok, got %q", msgType))
	}
}

// send marshals msg and writes it as one frame under timeout.
func (c *Client) send(ctx context.Context, msg map[string]any, timeout time.Duration) *Error {
	data, err := json.Marshal(msg)
	if err != nil {
		// Unreachable for the shapes this package builds; the one gap is
		// a caller passing params json.Marshal rejects, e.g. a channel.
		return newError("transport", err.Error())
	}

	writeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := c.conn.Write(writeCtx, data); err != nil {
		return newError("transport", err.Error())
	}
	return nil
}

// recvRaw reads and parses the next frame with no id matching - handshake
// only, where exactly one message is expected next.
func (c *Client) recvRaw(ctx context.Context) (map[string]any, *Error) {
	return c.readMessage(ctx, c.timeout, fmt.Sprintf("no response within %s", c.timeout))
}

// recvMatching reads frames until one carries "id" == id, discarding the
// rest, or returns a "timeout" *Error. Each read gets a fresh budget, so a
// run of unrelated frames does not eat the wait for the real response - but
// the loop as a whole is capped at three budgets, or a steady stream of
// frames that never match (this client subscribes to nothing, so all of
// them are stale or pushed) would hold the Cmd open forever.
func (c *Client) recvMatching(ctx context.Context, id uint64, timeout time.Duration) (map[string]any, *Error) {
	overallCtx, cancel := context.WithTimeout(ctx, 3*timeout)
	defer cancel()
	timeoutMsg := fmt.Sprintf("no response for id=%d within %s", id, timeout)
	for {
		msg, err := c.readMessage(overallCtx, timeout, timeoutMsg)
		if err != nil {
			return nil, err
		}
		if idMatches(msg["id"], id) {
			return msg, nil
		}
		slog.Debug("wsclient: discarding message while waiting for response",
			"got_id", msg["id"], "want_id", id)
	}
}

// readMessage reads one frame under a fresh per-call timeout, then parses
// it as a JSON object.
func (c *Client) readMessage(ctx context.Context, timeout time.Duration, timeoutMsg string) (map[string]any, *Error) {
	readCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	raw, err := c.conn.Read(readCtx)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, newError("timeout", timeoutMsg)
		}
		return nil, newError("transport", err.Error())
	}
	return parseMessage(raw)
}

// idMatches reports whether a JSON-decoded "id" value equals id. JSON
// numbers decode to float64, so this is the only shape ever compared.
func idMatches(value any, id uint64) bool {
	f, ok := value.(float64)
	return ok && uint64(f) == id
}
