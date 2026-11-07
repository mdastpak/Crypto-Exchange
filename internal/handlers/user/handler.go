package user

import (
	"github.com/mdastpak/crypto-exchange/internal/auth"
	"github.com/mdastpak/crypto-exchange/internal/db"
	"github.com/mdastpak/crypto-exchange/internal/models"
	"github.com/mdastpak/crypto-exchange/internal/notify"
	"github.com/mdastpak/crypto-exchange/internal/otp"
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
	// ContactOTP issues and checks the codes of contact changes (#199), in
	// their own namespace, so that they cannot sign in.
	ContactOTP *otp.Store
	// OTPSender delivers those codes. The server sets the configured
	// delivery; without one, the contact changes are unavailable.
	OTPSender notify.OTPSender
}

// NewHandler function to initialize CurrenciesUserHandler with dependencies
func NewHandler(is *models.InfraServices) *UserHandler {
	h := &UserHandler{
		PostgresDB:      is.Postgres,
		RedisClient:     is.Redis,
		RabbitMQService: is.RabbitMQ,
	}
	if is.Redis != nil {
		settings := otp.SettingsFromConfig()
		settings.Namespace = ContactOTPNamespace
		h.ContactOTP = otp.NewStore(is.Redis.Client, settings)
	}
	return h
}
