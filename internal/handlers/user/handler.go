package user

import (
	"github.com/mdastpak/crypto-exchange/internal/auth"
	"github.com/mdastpak/crypto-exchange/internal/db"
	"github.com/mdastpak/crypto-exchange/internal/models"
	"github.com/mdastpak/crypto-exchange/internal/rabbitmq"
)

// UserHandler struct to hold dependencies for User routes
type UserHandler struct {
	PostgresDB      *db.PostgresService
	RedisClient     *db.RedisService
	RedisIndex      string // Redis index for base definitions
	RabbitMQService *rabbitmq.RabbitMQService
	// Sessions lists and ends the sessions of the signed-in user (#193).
	Sessions auth.SessionManager
}

// NewHandler function to initialize CurrenciesUserHandler with dependencies
func NewHandler(is *models.InfraServices) *UserHandler {
	return &UserHandler{
		PostgresDB:      is.Postgres,
		RedisClient:     is.Redis,
		RabbitMQService: is.RabbitMQ,
	}
}
