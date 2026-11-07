// Package tlsproxy terminates TLS in front of a plain TCP service, such as
// the Redis and RabbitMQ servers of the tests, so that tests can check the
// TLS settings of a client against the services CI already runs (#186).
package tlsproxy

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// Proxy accepts TLS connections and forwards them to an upstream address.
type Proxy struct {
	listener net.Listener
	upstream string
	// CAFile is a PEM file with the certificate authority that signed the
	// proxy's certificate, for the client's ca_file.
	CAFile string
	// ServerName is the name in the proxy's certificate.
	ServerName string

	wg sync.WaitGroup
}

// ServerName is the name that the certificate of every proxy holds. It is
// not the address the client dials, so a client must set server_name.
const ServerName = "tls-proxy.test"

// New starts a proxy for upstream with a certificate from a fresh
// certificate authority, and stops it when the test ends.
func New(t *testing.T, upstream string) *Proxy {
	t.Helper()
	caCert, caKey := newCA(t)
	serverCert := issue(t, caCert, caKey, ServerName)

	p := &Proxy{upstream: upstream, ServerName: ServerName}
	p.CAFile = filepath.Join(t.TempDir(), "ca.pem")
	writePEM(t, p.CAFile, "CERTIFICATE", caCert.Raw)

	listener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{serverCert},
	})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	p.listener = listener
	p.wg.Add(1)
	go p.accept()
	t.Cleanup(func() {
		listener.Close()
		p.wg.Wait()
	})
	return p
}

// Host and Port are the address of the proxy.
func (p *Proxy) Host() string { return "127.0.0.1" }

func (p *Proxy) Port() string {
	_, port, _ := net.SplitHostPort(p.listener.Addr().String())
	return port
}

func (p *Proxy) accept() {
	defer p.wg.Done()
	for {
		client, err := p.listener.Accept()
		if err != nil {
			return
		}
		go p.forward(client)
	}
}

func (p *Proxy) forward(client net.Conn) {
	defer client.Close()
	upstream, err := net.DialTimeout("tcp", p.upstream, 5*time.Second)
	if err != nil {
		return
	}
	defer upstream.Close()
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(upstream, client); done <- struct{}{} }()
	go func() { _, _ = io.Copy(client, upstream); done <- struct{}{} }()
	<-done
}

func newCA(t *testing.T) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "tlsproxy test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert, key
}

func issue(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey, name string) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: name},
		DNSNames:     []string{name},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func writePEM(t *testing.T, path, kind string, der []byte) {
	t.Helper()
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
}
