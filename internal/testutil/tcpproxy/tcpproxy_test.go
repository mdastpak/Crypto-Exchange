package tcpproxy

import (
	"bufio"
	"io"
	"net"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// echoServer answers every line with the same line.
func echoServer(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_, _ = io.Copy(conn, conn)
			}()
		}
	}()
	return listener.Addr().String()
}

// roundTrip sends a line through the proxy and returns the answer, or the
// error of reading it within wait.
func roundTrip(t *testing.T, p *Proxy, wait time.Duration) (string, error) {
	t.Helper()
	conn, err := net.Dial("tcp", net.JoinHostPort(p.Host(), p.Port()))
	require.NoError(t, err)
	defer conn.Close()
	_, err = conn.Write([]byte("ping\n"))
	require.NoError(t, err)
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(wait)))
	return bufio.NewReader(conn).ReadString('\n')
}

func TestProxy_StalledHoldsNewConnectionsWithoutAnswering(t *testing.T) {
	p, err := New(echoServer(t))
	require.NoError(t, err)
	t.Cleanup(p.Close)

	answer, err := roundTrip(t, p, time.Second)
	require.NoError(t, err)
	assert.Equal(t, "ping\n", answer)

	p.SetStalled(true)
	_, err = roundTrip(t, p, 200*time.Millisecond)
	require.ErrorIs(t, err, os.ErrDeadlineExceeded, "the connection is accepted but gets no answer")

	p.SetStalled(false)
	answer, err = roundTrip(t, p, time.Second)
	require.NoError(t, err)
	assert.Equal(t, "ping\n", answer, "new connections are forwarded again")
}

func TestProxy_CutClosesStalledConnections(t *testing.T) {
	p, err := New(echoServer(t))
	require.NoError(t, err)
	t.Cleanup(p.Close)
	p.SetStalled(true)

	conn, err := net.Dial("tcp", net.JoinHostPort(p.Host(), p.Port()))
	require.NoError(t, err)
	defer conn.Close()
	// The proxy tracks the connection once it has accepted it.
	require.Eventually(t, func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		return len(p.conns) == 1
	}, time.Second, 10*time.Millisecond)

	p.Cut()
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(time.Second)))
	_, err = conn.Read(make([]byte, 1))
	assert.ErrorIs(t, err, io.EOF, "a cut connection is closed, not left hanging")
}
