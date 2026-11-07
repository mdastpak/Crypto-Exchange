package config

import (
	"crypto/tls"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClientTLS_DisabledHasNoConfig(t *testing.T) {
	cfg, err := ClientTLS{CAFile: "/ignored/when/disabled"}.TLSConfig("redis")
	require.NoError(t, err)
	assert.Nil(t, cfg)
}

func TestClientTLS_VerifiesTheHostByDefault(t *testing.T) {
	cfg, err := ClientTLS{Enabled: true}.TLSConfig("redis.internal")
	require.NoError(t, err)
	assert.Equal(t, "redis.internal", cfg.ServerName)
	assert.Equal(t, uint16(tls.VersionTLS12), cfg.MinVersion)
	assert.False(t, cfg.InsecureSkipVerify, "verification can never be turned off")
	assert.Nil(t, cfg.RootCAs, "the system authorities")
}

func TestClientTLS_ServerNameOverridesTheHost(t *testing.T) {
	cfg, err := ClientTLS{Enabled: true, ServerName: "redis.internal"}.TLSConfig("10.0.0.5")
	require.NoError(t, err)
	assert.Equal(t, "redis.internal", cfg.ServerName)
}

func TestClientTLS_Files(t *testing.T) {
	dir := t.TempDir()
	notPEM := filepath.Join(dir, "not.pem")
	require.NoError(t, os.WriteFile(notPEM, []byte("not a certificate"), 0o600))

	for name, tc := range map[string]struct {
		tls  ClientTLS
		want string
	}{
		"missing CA file":     {ClientTLS{Enabled: true, CAFile: filepath.Join(dir, "missing.pem")}, "read ca_file"},
		"CA file without PEM": {ClientTLS{Enabled: true, CAFile: notPEM}, "holds no PEM certificate"},
		"certificate alone":   {ClientTLS{Enabled: true, CertFile: notPEM}, "must be set together"},
		"key alone":           {ClientTLS{Enabled: true, KeyFile: notPEM}, "must be set together"},
		"unusable pair":       {ClientTLS{Enabled: true, CertFile: notPEM, KeyFile: notPEM}, "load cert_file and key_file"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := tc.tls.TLSConfig("host")
			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestRabbitMQURL_TLSUsesAMQPS(t *testing.T) {
	assert.Equal(t, "amqps://u:p@mq:5671/", RabbitMQURL("u", "p", "mq", "5671", "", true))
	assert.Equal(t, "amqp://u:p@mq:5672/", RabbitMQURL("u", "p", "mq", "5672", "", false))
}
