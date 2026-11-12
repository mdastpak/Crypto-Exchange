package user

import (
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/db"
	"github.com/mdastpak/crypto-exchange/internal/favorites"
	"github.com/mdastpak/crypto-exchange/internal/models"
	"github.com/mdastpak/crypto-exchange/internal/restrictions"

	"github.com/stretchr/testify/assert"
)

// TestNewHandler_BuildsTheStoresOfTheServices checks that the handler gets
// the stores of the database of the services, and their configuration.
func TestNewHandler_BuildsTheStoresOfTheServices(t *testing.T) {
	postgres := &db.PostgresService{}
	h := NewHandler(&models.InfraServices{Config: &config.Config, Postgres: postgres, Redis: &db.RedisService{}})

	assert.Same(t, &config.Config, h.Config)
	assert.Equal(t, &DBStore{DB: postgres.DB, Config: &config.Config}, h.Store)
	assert.Equal(t, audit.Log{DB: postgres.DB}, h.Audit)
	assert.Equal(t, favorites.Store{DB: postgres.DB}, h.Favorites)
	assert.Equal(t, restrictions.Store{DB: postgres.DB}, h.Restrictions)
	assert.NotNil(t, h.ContactOTP)
}

// TestNewHandler_LeavesNoTypedNil builds the handler without services: each
// dependency stays a nil interface, so that the routes see it is missing.
func TestNewHandler_LeavesNoTypedNil(t *testing.T) {
	h := NewHandler(&models.InfraServices{})

	assert.True(t, h.Store == nil)
	assert.True(t, h.Audit == nil)
	assert.True(t, h.Redis == nil)
	assert.True(t, h.ContactOTP == nil)
	assert.True(t, h.Passwords == nil)
}
