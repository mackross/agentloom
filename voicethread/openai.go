package voicethread

import (
	"context"
	"fmt"
	"net/http"
	"net/url"

	"github.com/coder/websocket"
)

type realtimeConn interface {
	Read(context.Context) ([]byte, error)
	Write(context.Context, []byte) error
	CloseNow() error
}

type websocketRealtimeConn struct {
	conn *websocket.Conn
}

func dialRealtime(ctx context.Context, opts Options) (realtimeConn, error) {
	u, err := realtimeURL(opts)
	if err != nil {
		return nil, err
	}
	header := http.Header{}
	header.Set("Authorization", "Bearer "+opts.APIKey)
	conn, _, err := websocket.Dial(ctx, u, &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		return nil, err
	}
	return websocketRealtimeConn{conn: conn}, nil
}

func realtimeURL(opts Options) (string, error) {
	base := opts.BaseURL
	if base == "" {
		base = "wss://api.openai.com/v1/realtime"
	}
	u, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	q := u.Query()
	if q.Get("call_id") == "" && q.Get("model") == "" {
		q.Set("model", opts.Model)
	}
	u.RawQuery = q.Encode()
	if u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("voicethread: invalid realtime BaseURL %q", base)
	}
	return u.String(), nil
}

func (c websocketRealtimeConn) Read(ctx context.Context) ([]byte, error) {
	msgType, b, err := c.conn.Read(ctx)
	if err != nil {
		return nil, err
	}
	if msgType != websocket.MessageText && msgType != websocket.MessageBinary {
		return nil, fmt.Errorf("voicethread: unexpected websocket message type %v", msgType)
	}
	return b, nil
}

func (c websocketRealtimeConn) Write(ctx context.Context, b []byte) error {
	return c.conn.Write(ctx, websocket.MessageText, b)
}

func (c websocketRealtimeConn) CloseNow() error {
	return c.conn.Close(websocket.StatusNormalClosure, "")
}
