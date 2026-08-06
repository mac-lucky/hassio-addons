package wsclient

import (
	"context"

	"github.com/coder/websocket"
)

// maxMessageBytes raises coder/websocket's default 32KiB read limit, which
// a registry list on a large install exceeds comfortably. Generous rather
// than exact; nothing here needs a tight cap.
const maxMessageBytes = 10 << 20 // 10 MiB

// defaultDialer is the production Dialer; tests inject a fake instead.
func defaultDialer(ctx context.Context, url string) (Conn, error) {
	conn, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		return nil, err
	}
	conn.SetReadLimit(maxMessageBytes)
	return &wsConn{conn: conn}, nil
}

// wsConn adapts *websocket.Conn to Conn. Per coder/websocket's docs any
// error from any method - a deadline-hitting Read included - closes the
// connection, so a Cmd that returns "timeout" needs a fresh Dial.
type wsConn struct {
	conn *websocket.Conn
}

func (w *wsConn) Write(ctx context.Context, data []byte) error {
	return w.conn.Write(ctx, websocket.MessageText, data)
}

func (w *wsConn) Read(ctx context.Context) ([]byte, error) {
	_, data, err := w.conn.Read(ctx)
	if err != nil {
		return nil, err
	}
	return data, nil
}

func (w *wsConn) Close() error {
	return w.conn.Close(websocket.StatusNormalClosure, "")
}
