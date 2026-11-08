package stream

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/gorilla/websocket"
)

// Defaults of the connections.
const (
	// DefaultQuiet is how long a stream may stay quiet before the
	// connection counts as dead: the venues send a ticker, a heartbeat, or
	// a ping well within it.
	DefaultQuiet = time.Minute
	// handshakeTimeout bounds the opening of a connection.
	handshakeTimeout = 15 * time.Second
	// writeTimeout bounds a write.
	writeTimeout = 10 * time.Second
	// maxMessage bounds a message of a venue.
	maxMessage = 1 << 20
)

// NewDialer returns the dialer of the streams, through proxyURL (http,
// https, or socks5) when it is set, as the egress of the node routes the
// venue (#265). quiet is how long a stream may stay quiet.
func NewDialer(proxyURL string, quiet time.Duration) (Dialer, error) {
	d := websocket.Dialer{HandshakeTimeout: handshakeTimeout}
	if proxyURL != "" {
		proxy, err := url.Parse(proxyURL)
		if err != nil || proxy.Host == "" {
			return nil, fmt.Errorf("stream: invalid proxy URL")
		}
		switch proxy.Scheme {
		case "http", "https", "socks5":
		default:
			return nil, fmt.Errorf("stream: the proxy must be http, https, or socks5")
		}
		d.Proxy = http.ProxyURL(proxy)
	}
	if quiet <= 0 {
		quiet = DefaultQuiet
	}
	return func(ctx context.Context, address string) (Conn, error) {
		conn, resp, err := d.DialContext(ctx, address, nil)
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		if err != nil {
			// The error of a proxy may name it and its credentials.
			if proxyURL != "" {
				return nil, fmt.Errorf("stream: connecting through the proxy failed")
			}
			return nil, err
		}
		conn.SetReadLimit(maxMessage)
		return &wsConn{conn: conn, quiet: quiet}, nil
	}, nil
}

// wsConn is a Conn on a WebSocket connection. The venues' pings are
// answered by the default ping handler while it reads.
type wsConn struct {
	conn  *websocket.Conn
	quiet time.Duration
}

func (c *wsConn) ReadMessage() ([]byte, error) {
	if err := c.conn.SetReadDeadline(time.Now().Add(c.quiet)); err != nil {
		return nil, err
	}
	_, m, err := c.conn.ReadMessage()
	return m, err
}

func (c *wsConn) WriteMessage(message []byte) error {
	if err := c.conn.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
		return err
	}
	return c.conn.WriteMessage(websocket.TextMessage, message)
}

func (c *wsConn) Close() error { return c.conn.Close() }
