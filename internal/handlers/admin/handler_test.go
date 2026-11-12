package admin

import (
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/currencies"
	"github.com/mdastpak/crypto-exchange/internal/db"
	"github.com/mdastpak/crypto-exchange/internal/models"
	"github.com/mdastpak/crypto-exchange/internal/platform"
	"github.com/mdastpak/crypto-exchange/internal/restrictions"

	"github.com/stretchr/testify/assert"
)

// TestNewHandler_BuildsTheStoresOfTheServices checks that the handler gets
// the stores of the database of the services, and their configuration.
func TestNewHandler_BuildsTheStoresOfTheServices(t *testing.T) {
	postgres := &db.PostgresService{}
	h := NewHandler(&models.InfraServices{Config: &config.Config, Postgres: postgres, Redis: &db.RedisService{}})

	assert.Same(t, &config.Config, h.Config)
	assert.Equal(t, currencies.Store{DB: postgres.DB}, h.CurrencyStore)
	assert.Equal(t, restrictions.Store{DB: postgres.DB}, h.Restrictions)
	assert.Equal(t, platform.Store{DB: postgres.DB}, h.Controls)
	assert.Equal(t, audit.Log{DB: postgres.DB}, h.Audit)
	assert.NotNil(t, h.OTP)
}

// TestNewHandler_LeavesNoTypedNil builds the handler without services. Each
// dependency stays a nil interface, not one that holds a nil pointer, so
// that the checks of the routes see that it is missing.
func TestNewHandler_LeavesNoTypedNil(t *testing.T) {
	h := NewHandler(&models.InfraServices{})

	assert.True(t, h.CurrencyStore == nil)
	assert.True(t, h.Restrictions == nil)
	assert.True(t, h.Currencies == nil, "no catalog without PostgreSQL and Redis")
	assert.True(t, h.OTP == nil)
	assert.True(t, h.Accounts == nil)
	assert.True(t, h.Platform == nil)
}
