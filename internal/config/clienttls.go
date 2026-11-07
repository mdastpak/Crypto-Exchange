package config

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
)

// ClientTLS configures TLS for the connections the server opens to a
// dependency, such as Redis or RabbitMQ (#186).
type ClientTLS struct {
	// Enabled connects with TLS. Without CAFile, the server's certificate
	// is checked against the system's certificate authorities.
	Enabled bool `yaml:"enabled"`
	// CAFile is a PEM file with the certificate authorities that sign the
	// server's certificate, for a private certificate authority.
	CAFile string `yaml:"ca_file"`
	// ServerName is the name the server's certificate must hold, when it
	// differs from the host the server connects to, such as an IP address.
	ServerName string `yaml:"server_name"`
	// CertFile and KeyFile are an optional client certificate, for a
	// dependency that authenticates its clients with mutual TLS.
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
	// PrivateNetwork acknowledges, in release mode, a connection without
	// TLS because the dependency is reached only over a trusted private
	// network, such as a service mesh that encrypts every hop.
	PrivateNetwork bool `yaml:"private_network"`
}

// TLSConfig returns the TLS configuration of the connection to host, or nil
// when TLS is not enabled. The certificate is always verified: there is
// deliberately no setting that skips verification.
func (t ClientTLS) TLSConfig(host string) (*tls.Config, error) {
	if !t.Enabled {
		return nil, nil
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: host}
	if t.ServerName != "" {
		cfg.ServerName = t.ServerName
	}
	if t.CAFile != "" {
		pem, err := os.ReadFile(t.CAFile)
		if err != nil {
			return nil, fmt.Errorf("read ca_file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("ca_file %s holds no PEM certificate", t.CAFile)
		}
		cfg.RootCAs = pool
	}
	if t.CertFile != "" || t.KeyFile != "" {
		if t.CertFile == "" || t.KeyFile == "" {
			return nil, fmt.Errorf("cert_file and key_file must be set together")
		}
		pair, err := tls.LoadX509KeyPair(t.CertFile, t.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("load cert_file and key_file: %w", err)
		}
		cfg.Certificates = []tls.Certificate{pair}
	}
	return cfg, nil
}

// clientTLSProblems reports TLS settings of a dependency that cannot be
// used, and, in release mode, a connection without TLS that is not
// acknowledged as a private network.
func clientTLSProblems(p *problems, key, host string, t ClientTLS, release bool) {
	if _, err := t.TLSConfig(host); err != nil {
		p.add(key+".tls", "cannot be used: %v", err)
	}
	if release && !t.Enabled && !t.PrivateNetwork {
		p.add(key+".tls.enabled", "is false; release mode needs TLS for %s: enable it, or set %s.tls.private_network (%s) when only a trusted private network carries the connection",
			key, key, envVarFor(key+".tls.private_network"))
	}
}
