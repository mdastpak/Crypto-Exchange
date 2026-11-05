package user

import (
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/db"
	"github.com/mdastpak/crypto-exchange/internal/models"
	"github.com/mdastpak/crypto-exchange/internal/rabbitmq"

	"github.com/stretchr/testify/assert"
)

func TestNewHandler_WiresInfraServices(t *testing.T) {
	postgres := &db.PostgresService{}
	redisSvc := &db.RedisService{}
	rmq := &rabbitmq.RabbitMQService{}

	is := &models.InfraServices{
		Postgres: postgres,
		Redis:    redisSvc,
		RabbitMQ: rmq,
	}

	h := NewHandler(is)

	assert.Same(t, postgres, h.PostgresDB)
	assert.Same(t, redisSvc, h.RedisClient)
	assert.Same(t, rmq, h.RabbitMQService)
}

func TestNewHandler_WithNilRabbitMQ(t *testing.T) {
	is := &models.InfraServices{
		Postgres: &db.PostgresService{},
		Redis:    &db.RedisService{},
		RabbitMQ: nil,
	}

	h := NewHandler(is)

	assert.NotNil(t, h.PostgresDB)
	assert.NotNil(t, h.RedisClient)
	assert.Nil(t, h.RabbitMQService)
}
