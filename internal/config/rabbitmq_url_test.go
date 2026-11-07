package config

import (
	"testing"

	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRabbitMQURL_EscapesCredentialsAndVHost checks that every character
// that has a meaning in a URI reaches the broker unchanged, as the AMQP
// client parses the URI (#185).
func TestRabbitMQURL_EscapesCredentialsAndVHost(t *testing.T) {
	tricky := []string{"p@ss", "pa/ss", "pa:ss", "pa#ss", "pa?ss", "pa%ss", "pa ss", "p@:/#?% x"}
	for _, secret := range tricky {
		t.Run(secret, func(t *testing.T) {
			uri, err := amqp.ParseURI(RabbitMQURL("us@er:"+secret, secret, "broker.internal", "5672", "app/"+secret))
			require.NoError(t, err)
			assert.Equal(t, "broker.internal", uri.Host)
			assert.Equal(t, 5672, uri.Port)
			assert.Equal(t, "us@er:"+secret, uri.Username)
			assert.Equal(t, secret, uri.Password)
			assert.Equal(t, "app/"+secret, uri.Vhost)
		})
	}
}

func TestRabbitMQURL_DefaultVHost(t *testing.T) {
	for _, vhost := range []string{"", "/"} {
		uri, err := amqp.ParseURI(RabbitMQURL("guest", "guest", "localhost", "5672", vhost))
		require.NoError(t, err)
		assert.Equal(t, "/", uri.Vhost)
	}
}

func TestRabbitMQURL_IPv6Host(t *testing.T) {
	uri, err := amqp.ParseURI(RabbitMQURL("u", "p", "::1", "5672", ""))
	require.NoError(t, err)
	assert.Equal(t, "::1", uri.Host)
	assert.Equal(t, 5672, uri.Port)
}
