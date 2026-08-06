package wsclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"
)

var (
	authRequired = map[string]any{"type": "auth_required", "ha_version": "2026.1.0"}
	authOK       = map[string]any{"type": "auth_ok", "ha_version": "2026.1.0"}
)

// fakeConn records every Write and replays a scripted queue of incoming
// frames, one per Read. A queued error is returned instead of a frame, so
// tests can simulate a timeout or a dropped connection mid-stream.
type fakeConn struct {
	script   []any
	sent     [][]byte
	writeErr error
	closed   bool
	closeErr error

	// readDeadlines records each Read's remaining budget, so tests can
	// assert the configured timeout actually reached the transport.
	readDeadlines []time.Duration
}

func newFakeConn(script ...any) *fakeConn {
	return &fakeConn{script: script}
}

func (f *fakeConn) Write(_ context.Context, data []byte) error {
	if f.writeErr != nil {
		return f.writeErr
	}
	f.sent = append(f.sent, data)
	return nil
}

func (f *fakeConn) Read(ctx context.Context) ([]byte, error) {
	if deadline, ok := ctx.Deadline(); ok {
		f.readDeadlines = append(f.readDeadlines, time.Until(deadline))
	}
	if len(f.script) == 0 {
		panic("fakeConn: Read called with an empty script")
	}
	item := f.script[0]
	f.script = f.script[1:]
	switch v := item.(type) {
	case error:
		return nil, v
	case string:
		return []byte(v), nil
	case []byte:
		return v, nil
	default:
		data, err := json.Marshal(v)
		if err != nil {
			panic(err)
		}
		return data, nil
	}
}

func (f *fakeConn) Close() error {
	f.closed = true
	return f.closeErr
}

// fakeDialer always returns conn, recording every url it was called with.
type fakeDialer struct {
	conn  *fakeConn
	err   error
	calls []string
}

func (d *fakeDialer) dial(_ context.Context, url string) (Conn, error) {
	d.calls = append(d.calls, url)
	if d.err != nil {
		return nil, d.err
	}
	return d.conn, nil
}

// decodeSent decodes a recorded frame for structural comparison, so
// marshaled map-key ordering does not matter.
func decodeSent(t *testing.T, data []byte) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatalf("decode sent frame: %v", err)
	}
	return v
}

func asError(t *testing.T, err error) *Error {
	t.Helper()
	var wsErr *Error
	if !errors.As(err, &wsErr) {
		t.Fatalf("error = %v (%T), want *wsclient.Error", err, err)
	}
	return wsErr
}

// dialedClient connects a Client against a scripted fakeConn, failing the
// test if Dial errors - shared setup for every Cmd()-focused test.
func dialedClient(t *testing.T, script ...any) (*Client, *fakeConn) {
	t.Helper()
	conn := newFakeConn(script...)
	dialer := &fakeDialer{conn: conn}
	client := New(WithToken("t"), WithDialer(dialer.dial))
	if err := client.Dial(context.Background()); err != nil {
		t.Fatalf("Dial: %v", err)
	}
	return client, conn
}

// --- handshake ---------------------------------------------------------

func TestDialPerformsHandshakeAndUsesDefaultURLAndToken(t *testing.T) {
	conn := newFakeConn(authRequired, authOK)
	dialer := &fakeDialer{conn: conn}
	client := New(WithToken("test-token"), WithDialer(dialer.dial))

	if err := client.Dial(context.Background()); err != nil {
		t.Fatalf("Dial: %v", err)
	}

	if !reflect.DeepEqual(dialer.calls, []string{DefaultURL}) {
		t.Errorf("dialer.calls = %v, want [%s]", dialer.calls, DefaultURL)
	}
	if len(conn.sent) != 1 {
		t.Fatalf("len(sent) = %d, want 1", len(conn.sent))
	}
	want := map[string]any{"type": "auth", "access_token": "test-token"}
	if got := decodeSent(t, conn.sent[0]); !reflect.DeepEqual(got, want) {
		t.Errorf("sent[0] = %v, want %v", got, want)
	}

	client.Close()
	if !conn.closed {
		t.Error("conn.closed = false, want true after Close")
	}
}

func TestDialUsesSupervisorTokenWhenTokenNotGiven(t *testing.T) {
	t.Setenv("SUPERVISOR_TOKEN", "env-token")
	conn := newFakeConn(authRequired, authOK)
	dialer := &fakeDialer{conn: conn}
	client := New(WithDialer(dialer.dial))

	if err := client.Dial(context.Background()); err != nil {
		t.Fatalf("Dial: %v", err)
	}

	want := map[string]any{"type": "auth", "access_token": "env-token"}
	if got := decodeSent(t, conn.sent[0]); !reflect.DeepEqual(got, want) {
		t.Errorf("sent[0] = %v, want %v", got, want)
	}
}

func TestCustomURLIsPassedToDialer(t *testing.T) {
	conn := newFakeConn(authRequired, authOK)
	dialer := &fakeDialer{conn: conn}
	client := New(WithURL("ws://supervisor/core/websocket?x=1"), WithToken("t"), WithDialer(dialer.dial))

	if err := client.Dial(context.Background()); err != nil {
		t.Fatalf("Dial: %v", err)
	}

	want := []string{"ws://supervisor/core/websocket?x=1"}
	if !reflect.DeepEqual(dialer.calls, want) {
		t.Errorf("dialer.calls = %v, want %v", dialer.calls, want)
	}
}

func TestAuthInvalidReturnsError(t *testing.T) {
	conn := newFakeConn(authRequired, map[string]any{"type": "auth_invalid", "message": "invalid access token"})
	dialer := &fakeDialer{conn: conn}
	client := New(WithToken("bad-token"), WithDialer(dialer.dial))

	err := client.Dial(context.Background())
	if err == nil {
		t.Fatal("Dial() error = nil, want an error")
	}
	wsErr := asError(t, err)
	if wsErr.Code != "auth_invalid" {
		t.Errorf("Code = %q, want auth_invalid", wsErr.Code)
	}
	if wsErr.Message != "invalid access token" {
		t.Errorf("Message = %q, want %q", wsErr.Message, "invalid access token")
	}
}

func TestUnexpectedFirstMessageReturnsProtocolError(t *testing.T) {
	conn := newFakeConn(map[string]any{"type": "event", "event": map[string]any{}})
	dialer := &fakeDialer{conn: conn}
	client := New(WithToken("t"), WithDialer(dialer.dial))

	err := client.Dial(context.Background())
	if asError(t, err).Code != "protocol_error" {
		t.Errorf("Code = %q, want protocol_error", asError(t, err).Code)
	}
}

func TestUnexpectedSecondMessageReturnsProtocolError(t *testing.T) {
	conn := newFakeConn(authRequired, map[string]any{"type": "something_else"})
	dialer := &fakeDialer{conn: conn}
	client := New(WithToken("t"), WithDialer(dialer.dial))

	err := client.Dial(context.Background())
	if asError(t, err).Code != "protocol_error" {
		t.Errorf("Code = %q, want protocol_error", asError(t, err).Code)
	}
}

func TestTokenNeverAppearsInLogOutput(t *testing.T) {
	token := "s3cr3t-supervisor-token"
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)

	conn := newFakeConn(authRequired, authOK, map[string]any{"id": 1, "type": "result", "success": true, "result": []any{}})
	dialer := &fakeDialer{conn: conn}
	client := New(WithToken(token), WithDialer(dialer.dial))

	if err := client.Dial(context.Background()); err != nil {
		t.Fatalf("Dial: %v", err)
	}
	if _, err := client.Cmd(context.Background(), "config/floor_registry/list", nil); err != nil {
		t.Fatalf("Cmd: %v", err)
	}

	if strings.Contains(buf.String(), token) {
		t.Errorf("log output contains the token: %s", buf.String())
	}
}

// --- Cmd(): success, error, ids -----------------------------------------

func TestCmdReturnsResultOnSuccess(t *testing.T) {
	client, conn := dialedClient(t, authRequired, authOK, map[string]any{
		"id": 1, "type": "result", "success": true,
		"result": []any{map[string]any{"floor_id": "ground", "name": "Ground floor"}},
	})

	result, err := client.Cmd(context.Background(), "config/floor_registry/list", nil)
	if err != nil {
		t.Fatalf("Cmd: %v", err)
	}
	want := []any{map[string]any{"floor_id": "ground", "name": "Ground floor"}}
	if !reflect.DeepEqual(result, want) {
		t.Errorf("result = %v, want %v", result, want)
	}

	// The auth message carries no id; the first Cmd call is id=1.
	last := decodeSent(t, conn.sent[len(conn.sent)-1])
	wantSent := map[string]any{"id": float64(1), "type": "config/floor_registry/list"}
	if !reflect.DeepEqual(last, wantSent) {
		t.Errorf("last sent = %v, want %v", last, wantSent)
	}
}

func TestCmdPassesExtraParamsThrough(t *testing.T) {
	client, conn := dialedClient(t, authRequired, authOK, map[string]any{
		"id": 1, "type": "result", "success": true,
		"result": map[string]any{"floor_id": "ground"},
	})

	if _, err := client.Cmd(context.Background(), "config/floor_registry/create", map[string]any{
		"name": "Ground floor", "icon": "mdi:home",
	}); err != nil {
		t.Fatalf("Cmd: %v", err)
	}

	last := decodeSent(t, conn.sent[len(conn.sent)-1])
	want := map[string]any{
		"id": float64(1), "type": "config/floor_registry/create",
		"name": "Ground floor", "icon": "mdi:home",
	}
	if !reflect.DeepEqual(last, want) {
		t.Errorf("last sent = %v, want %v", last, want)
	}
}

func TestCmdIDsAreMonotonicallyIncreasingAcrossCalls(t *testing.T) {
	client, conn := dialedClient(t,
		authRequired, authOK,
		map[string]any{"id": 1, "type": "result", "success": true, "result": nil},
		map[string]any{"id": 2, "type": "result", "success": true, "result": nil},
		map[string]any{"id": 3, "type": "result", "success": true, "result": nil},
	)

	for _, msgType := range []string{"a", "b", "c"} {
		if _, err := client.Cmd(context.Background(), msgType, nil); err != nil {
			t.Fatalf("Cmd(%q): %v", msgType, err)
		}
	}

	var sentIDs []float64
	for _, raw := range conn.sent {
		msg := decodeSent(t, raw)
		if id, ok := msg["id"]; ok {
			sentIDs = append(sentIDs, id.(float64))
		}
	}
	want := []float64{1, 2, 3}
	if !reflect.DeepEqual(sentIDs, want) {
		t.Errorf("sentIDs = %v, want %v", sentIDs, want)
	}
}

func TestCmdReturnsErrorOnSuccessFalse(t *testing.T) {
	client, _ := dialedClient(t, authRequired, authOK, map[string]any{
		"id": 1, "type": "result", "success": false,
		"error": map[string]any{"code": "not_found", "message": "no such floor"},
	})

	_, err := client.Cmd(context.Background(), "config/floor_registry/delete", map[string]any{"floor_id": "ghost"})
	wsErr := asError(t, err)
	if wsErr.Code != "not_found" {
		t.Errorf("Code = %q, want not_found", wsErr.Code)
	}
	if wsErr.Message != "no such floor" {
		t.Errorf("Message = %q, want %q", wsErr.Message, "no such floor")
	}
}

func TestCmdErrorWithoutErrorFieldUsesFallbackCodeAndMessage(t *testing.T) {
	client, _ := dialedClient(t, authRequired, authOK, map[string]any{
		"id": 1, "type": "result", "success": false,
	})

	_, err := client.Cmd(context.Background(), "config/floor_registry/list", nil)
	if asError(t, err).Code != "unknown_error" {
		t.Errorf("Code = %q, want unknown_error", asError(t, err).Code)
	}
}

// --- timeout -------------------------------------------------------------

func TestCmdReturnsErrorOnTimeout(t *testing.T) {
	client, _ := dialedClient(t, authRequired, authOK, context.DeadlineExceeded)

	_, err := client.Cmd(context.Background(), "config/floor_registry/list", nil)
	if asError(t, err).Code != "timeout" {
		t.Errorf("Code = %q, want timeout", asError(t, err).Code)
	}
}

func TestTimeoutOptionIsAppliedToReads(t *testing.T) {
	conn := newFakeConn(authRequired, authOK, map[string]any{"id": 1, "type": "result", "success": true, "result": nil})
	dialer := &fakeDialer{conn: conn}
	client := New(WithToken("t"), WithDialer(dialer.dial), WithTimeout(2500*time.Millisecond))

	if err := client.Dial(context.Background()); err != nil {
		t.Fatalf("Dial: %v", err)
	}
	if _, err := client.Cmd(context.Background(), "config/floor_registry/list", nil); err != nil {
		t.Fatalf("Cmd: %v", err)
	}

	if len(conn.readDeadlines) == 0 {
		t.Fatal("no read deadlines recorded")
	}
	last := conn.readDeadlines[len(conn.readDeadlines)-1]
	if last <= 2*time.Second || last > 2500*time.Millisecond {
		t.Errorf("last read deadline = %v, want ~2.5s", last)
	}
}

// CmdTimeout's point is that the READ waits that long, so this asserts on
// what actually reached the transport.
func TestCmdTimeoutAppliesTheCallersOwnBudget(t *testing.T) {
	conn := newFakeConn(authRequired, authOK, map[string]any{"id": 1, "type": "result", "success": true, "result": nil})
	dialer := &fakeDialer{conn: conn}
	client := New(WithToken("t"), WithDialer(dialer.dial), WithTimeout(2*time.Second))

	if err := client.Dial(context.Background()); err != nil {
		t.Fatalf("Dial: %v", err)
	}
	if _, err := client.CmdTimeout(context.Background(), "hacs/repository/download", nil, 5*time.Minute); err != nil {
		t.Fatalf("CmdTimeout: %v", err)
	}

	last := conn.readDeadlines[len(conn.readDeadlines)-1]
	if last <= 4*time.Minute || last > 5*time.Minute {
		t.Errorf("last read deadline = %v, want ~5m rather than the client default", last)
	}
}

// Zero means the client's own timeout, so a caller computing a budget
// need not special-case an unset one.
func TestCmdTimeoutZeroFallsBackToTheClientDefault(t *testing.T) {
	conn := newFakeConn(authRequired, authOK, map[string]any{"id": 1, "type": "result", "success": true, "result": nil})
	dialer := &fakeDialer{conn: conn}
	client := New(WithToken("t"), WithDialer(dialer.dial), WithTimeout(2500*time.Millisecond))

	if err := client.Dial(context.Background()); err != nil {
		t.Fatalf("Dial: %v", err)
	}
	if _, err := client.CmdTimeout(context.Background(), "config/floor_registry/list", nil, 0); err != nil {
		t.Fatalf("CmdTimeout: %v", err)
	}

	last := conn.readDeadlines[len(conn.readDeadlines)-1]
	if last <= 2*time.Second || last > 2500*time.Millisecond {
		t.Errorf("last read deadline = %v, want the client's own 2.5s", last)
	}
}

// The message names the budget that actually elapsed: "no response within
// 10s" on a call that waited ten minutes misdirects the reader.
func TestCmdTimeoutReportsTheBudgetItActuallyWaited(t *testing.T) {
	conn := newFakeConn(authRequired, authOK, context.DeadlineExceeded)
	dialer := &fakeDialer{conn: conn}
	client := New(WithToken("t"), WithDialer(dialer.dial), WithTimeout(10*time.Second))

	if err := client.Dial(context.Background()); err != nil {
		t.Fatalf("Dial: %v", err)
	}
	_, err := client.CmdTimeout(context.Background(), "hacs/repository/download", nil, 3*time.Minute)

	wsErr := asError(t, err)
	if wsErr.Code != "timeout" {
		t.Fatalf("Code = %q, want timeout", wsErr.Code)
	}
	if !strings.Contains(wsErr.Message, "3m0s") {
		t.Errorf("Message = %q, want it to name the 3m budget", wsErr.Message)
	}
}

// --- out-of-order / interleaved responses ---------------------------------

func TestCmdSkipsNonMatchingMessagesAndReturnsTheMatchingOne(t *testing.T) {
	client, _ := dialedClient(t,
		authRequired, authOK,
		// A stale response to an abandoned exchange (id=99) arrives
		// before the real answer to id=1.
		map[string]any{"id": 99, "type": "result", "success": true, "result": "stale, ignore me"},
		map[string]any{"id": 1, "type": "result", "success": true, "result": "the real answer"},
	)

	result, err := client.Cmd(context.Background(), "config/floor_registry/list", nil)
	if err != nil {
		t.Fatalf("Cmd: %v", err)
	}
	if result != "the real answer" {
		t.Errorf("result = %v, want %q", result, "the real answer")
	}
}

func TestCmdSkipsUnrelatedEventPushBeforeMatchingResponse(t *testing.T) {
	client, _ := dialedClient(t,
		authRequired, authOK,
		map[string]any{"type": "event", "event": map[string]any{"event_type": "state_changed"}},
		map[string]any{"id": 1, "type": "result", "success": true, "result": "ok"},
	)

	result, err := client.Cmd(context.Background(), "config/floor_registry/list", nil)
	if err != nil {
		t.Fatalf("Cmd: %v", err)
	}
	if result != "ok" {
		t.Errorf("result = %v, want ok", result)
	}
}

func TestSecondCmdOnlyMatchesItsOwnIDEvenWhenFirstIDsResponseRepeats(t *testing.T) {
	client, _ := dialedClient(t,
		authRequired, authOK,
		map[string]any{"id": 1, "type": "result", "success": true, "result": "first"},
		// A late echo of id=1 while Cmd #2 waits for id=2 must be
		// skipped, not mistaken for it.
		map[string]any{"id": 1, "type": "result", "success": true, "result": "duplicate"},
		map[string]any{"id": 2, "type": "result", "success": true, "result": "second"},
	)

	first, err := client.Cmd(context.Background(), "a", nil)
	if err != nil {
		t.Fatalf("Cmd(a): %v", err)
	}
	second, err := client.Cmd(context.Background(), "b", nil)
	if err != nil {
		t.Fatalf("Cmd(b): %v", err)
	}
	if first != "first" {
		t.Errorf("first = %v, want first", first)
	}
	if second != "second" {
		t.Errorf("second = %v, want second", second)
	}
}

// --- transport errors: only *Error ever escapes ---------------------------

func TestSendErrorBecomesTransportError(t *testing.T) {
	// The first outgoing frame is the handshake's auth message, so a
	// transport that always fails to send fails Dial itself.
	conn := newFakeConn(authRequired, authOK)
	conn.writeErr = errors.New("socket is already closed.")
	dialer := &fakeDialer{conn: conn}
	client := New(WithToken("t"), WithDialer(dialer.dial))

	err := client.Dial(context.Background())
	if asError(t, err).Code != "transport" {
		t.Errorf("Code = %q, want transport", asError(t, err).Code)
	}
}

func TestRecvErrorBecomesTransportError(t *testing.T) {
	client, _ := dialedClient(t, authRequired, authOK, errors.New("socket is already closed."))

	_, err := client.Cmd(context.Background(), "config/floor_registry/list", nil)
	if asError(t, err).Code != "transport" {
		t.Errorf("Code = %q, want transport", asError(t, err).Code)
	}
}

func TestRecvConnectionResetBecomesTransportError(t *testing.T) {
	client, _ := dialedClient(t, authRequired, authOK, errors.New("connection reset by peer"))

	_, err := client.Cmd(context.Background(), "config/floor_registry/list", nil)
	if asError(t, err).Code != "transport" {
		t.Errorf("Code = %q, want transport", asError(t, err).Code)
	}
}

func TestDialerConnectionRefusedBecomesTransportError(t *testing.T) {
	dialer := &fakeDialer{err: errors.New("connection refused")}
	client := New(WithToken("t"), WithDialer(dialer.dial))

	err := client.Dial(context.Background())
	if asError(t, err).Code != "transport" {
		t.Errorf("Code = %q, want transport", asError(t, err).Code)
	}
}

func TestDialerHandshakeFailureBecomesTransportError(t *testing.T) {
	dialer := &fakeDialer{err: errors.New("handshake failed")}
	client := New(WithToken("t"), WithDialer(dialer.dial))

	err := client.Dial(context.Background())
	if asError(t, err).Code != "transport" {
		t.Errorf("Code = %q, want transport", asError(t, err).Code)
	}
}

// --- malformed frames become protocol_error, never a raw panic ------------

func TestMalformedJSONResponseBecomesProtocolError(t *testing.T) {
	client, _ := dialedClient(t, authRequired, authOK, "not json at all")

	_, err := client.Cmd(context.Background(), "config/floor_registry/list", nil)
	if asError(t, err).Code != "protocol_error" {
		t.Errorf("Code = %q, want protocol_error", asError(t, err).Code)
	}
}

func TestJSONArrayResponseBecomesProtocolErrorNotPanic(t *testing.T) {
	// Valid JSON that is not an object must be rejected as *Error, not
	// reach a bare type assertion and panic.
	client, _ := dialedClient(t, authRequired, authOK, "[1, 2, 3]")

	_, err := client.Cmd(context.Background(), "config/floor_registry/list", nil)
	if asError(t, err).Code != "protocol_error" {
		t.Errorf("Code = %q, want protocol_error", asError(t, err).Code)
	}
}

func TestMalformedJSONDuringHandshakeBecomesProtocolError(t *testing.T) {
	conn := newFakeConn("not json at all")
	dialer := &fakeDialer{conn: conn}
	client := New(WithToken("t"), WithDialer(dialer.dial))

	err := client.Dial(context.Background())
	if asError(t, err).Code != "protocol_error" {
		t.Errorf("Code = %q, want protocol_error", asError(t, err).Code)
	}
}

// --- Close() ---------------------------------------------------------------

func TestCloseIsIdempotentAndSwallowsTransportErrors(t *testing.T) {
	conn := newFakeConn(authRequired, authOK)
	conn.closeErr = errors.New("socket already gone")
	dialer := &fakeDialer{conn: conn}
	client := New(WithToken("t"), WithDialer(dialer.dial))

	if err := client.Dial(context.Background()); err != nil {
		t.Fatalf("Dial: %v", err)
	}

	client.Close() // must not panic
	client.Close() // second call is a no-op, must not panic
}

// --- Cmd() before Dial ----------------------------------------------------

func TestCmdOnUndialedClientReturnsTransportErrorNotPanic(t *testing.T) {
	client := New(WithToken("t"), WithDialer((&fakeDialer{}).dial))

	_, err := client.Cmd(context.Background(), "config/floor_registry/list", nil)
	if asError(t, err).Code != "transport" {
		t.Errorf("Code = %q, want transport", asError(t, err).Code)
	}
}
