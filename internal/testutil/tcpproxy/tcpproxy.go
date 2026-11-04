// Package tcpproxy is a TCP proxy for tests. It sits between a client and a
// server, such as RabbitMQ, and simulates network failures and server
// restarts by cutting the connections and refusing new ones.
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

	mu    sync.Mutex
	conns map[net.Conn]struct{}
	down  bool
	wg    sync.WaitGroup
}

// New starts a proxy on a free local port that forwards to upstream.
func New(upstream string) (*Proxy, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	p := &Proxy{upstream: upstream, listener: listener, conns: make(map[net.Conn]struct{})}
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

// Cut closes every open connection, as a server restart or a network failure
// would.
func (p *Proxy) Cut() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for conn := range p.conns {
		conn.Close()
	}
}

// Close stops the proxy and closes every connection.
func (p *Proxy) Close() {
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
		down := p.down
		p.mu.Unlock()
		if down {
			client.Close()
			continue
		}
		server, err := net.DialTimeout("tcp", p.upstream, 5*time.Second)
		if err != nil {
			client.Close()
			continue
		}
		p.track(client, server)
		p.wg.Add(2)
		go p.pipe(client, server)
		go p.pipe(server, client)
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
// other direction ends too.
func (p *Proxy) pipe(dst, src net.Conn) {
	defer p.wg.Done()
	_, _ = io.Copy(dst, src)
	dst.Close()
	src.Close()
	p.mu.Lock()
	delete(p.conns, dst)
	delete(p.conns, src)
	p.mu.Unlock()
}
