// Package tcpproxy is a TCP proxy for tests. It sits between a client and a
// server, such as RabbitMQ, and simulates network failures and server
// restarts by cutting the connections and refusing new ones, a server that
// hangs by holding new connections without answering, and a server that
// stops reading from its open connections, as a RabbitMQ under a resource
// alarm does.
package tcpproxy

import (
	"io"
	"net"
	"sync"
	"time"
)

// Proxy forwards connections from a local address to an upstream address.
type Proxy struct {
	upstream string
	listener net.Listener

	mu      sync.Mutex
	conns   map[net.Conn]struct{}
	down    bool
	stalled bool
	// resumed is closed while the proxy reads from the clients, and open
	// while SetPaused holds their data.
	resumed   chan struct{}
	closing   chan struct{}
	closeOnce sync.Once
	wg        sync.WaitGroup
}

// New starts a proxy on a free local port that forwards to upstream.
func New(upstream string) (*Proxy, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	p := &Proxy{
		upstream: upstream, listener: listener, conns: make(map[net.Conn]struct{}),
		resumed: make(chan struct{}), closing: make(chan struct{}),
	}
	close(p.resumed)
	p.wg.Add(1)
	go p.accept()
	return p, nil
}

// Host and Port are the address that clients connect to.
func (p *Proxy) Host() string { return "127.0.0.1" }

func (p *Proxy) Port() string {
	_, port, _ := net.SplitHostPort(p.listener.Addr().String())
	return port
}

// SetDown makes the proxy refuse new connections while down is true, as a
// stopped server would. It does not affect open connections; see Cut.
func (p *Proxy) SetDown(down bool) {
	p.mu.Lock()
	p.down = down
	p.mu.Unlock()
}

// SetStalled makes the proxy accept new connections but forward nothing on
// them while stalled is true, as a server that hangs or a network that drops
// packets would: the client waits for an answer until it times out. It does
// not affect open connections; see Cut.
func (p *Proxy) SetStalled(stalled bool) {
	p.mu.Lock()
	p.stalled = stalled
	p.mu.Unlock()
}

// SetPaused stops, while paused is true, reading what the clients send on
// open and new connections, while the server's data still reaches them: a
// RabbitMQ that blocks publishing during a memory or disk alarm does this,
// and keeps heartbeating, so the client's writes block once the socket
// buffers are full. Data already read is still forwarded.
func (p *Proxy) SetPaused(paused bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	select {
	case <-p.resumed:
		if paused {
			p.resumed = make(chan struct{})
		}
	default:
		if !paused {
			close(p.resumed)
		}
	}
}

// Cut closes every open connection, as a server restart or a network failure
// would.
func (p *Proxy) Cut() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for conn := range p.conns {
		conn.Close()
		delete(p.conns, conn)
	}
}

// Close stops the proxy and closes every connection.
func (p *Proxy) Close() {
	p.closeOnce.Do(func() { close(p.closing) })
	p.listener.Close()
	p.Cut()
	p.wg.Wait()
}

func (p *Proxy) accept() {
	defer p.wg.Done()
	for {
		client, err := p.listener.Accept()
		if err != nil {
			return
		}
		p.mu.Lock()
		down, stalled := p.down, p.stalled
		p.mu.Unlock()
		if down {
			client.Close()
			continue
		}
		if stalled {
			// Held until Cut or Close.
			p.track(client)
			continue
		}
		server, err := net.DialTimeout("tcp", p.upstream, 5*time.Second)
		if err != nil {
			client.Close()
			continue
		}
		p.track(client, server)
		p.wg.Add(2)
		go p.pipe(client, server, false)
		go p.pipe(server, client, true)
	}
}

func (p *Proxy) track(conns ...net.Conn) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, conn := range conns {
		p.conns[conn] = struct{}{}
	}
}

// pipe copies from src to dst. When either side ends, both are closed, so the
// other direction ends too. fromClient marks the direction that SetPaused
// holds.
func (p *Proxy) pipe(dst, src net.Conn, fromClient bool) {
	defer p.wg.Done()
	if fromClient {
		p.copyUnlessPaused(dst, src)
	} else {
		_, _ = io.Copy(dst, src)
	}
	dst.Close()
	src.Close()
	p.mu.Lock()
	delete(p.conns, dst)
	delete(p.conns, src)
	p.mu.Unlock()
}

// copyUnlessPaused copies from src to dst, and reads nothing while the proxy
// is paused.
func (p *Proxy) copyUnlessPaused(dst, src net.Conn) {
	buf := make([]byte, 32*1024)
	for {
		p.mu.Lock()
		resumed := p.resumed
		p.mu.Unlock()
		select {
		case <-resumed:
		case <-p.closing:
			return
		}
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}
