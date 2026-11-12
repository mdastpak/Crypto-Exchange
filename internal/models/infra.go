package models

import (
	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/db"
	"github.com/mdastpak/crypto-exchange/internal/rabbitmq"
)

type InfraServices struct {
	Postgres *db.PostgresService
	Redis    *db.RedisService
	RabbitMQ *rabbitmq.RabbitMQService
	// Config is the configuration of the process, which the composition
	// root passes with the services (#48), so that what the services build
	// reads no global configuration. Nil reads as the zero configuration.
	Config *config.Configuration
}

// zeroConfig is the configuration of services without one.
var zeroConfig config.Configuration

// Settings returns the configuration of the services, or the zero
// configuration without one.
func (is *InfraServices) Settings() *config.Configuration {
	if is == nil || is.Config == nil {
		return &zeroConfig
	}
	return is.Config
}
