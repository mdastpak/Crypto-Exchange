package models

import (
	"github.com/mdastpak/crypto-exchange/internal/db"
	"github.com/mdastpak/crypto-exchange/internal/rabbitmq"
)

type InfraServices struct {
	Postgres *db.PostgresService
	Redis    *db.RedisService
	RabbitMQ *rabbitmq.RabbitMQService
}
