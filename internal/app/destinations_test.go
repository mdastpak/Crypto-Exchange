package app

import (
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/destinations"
	"github.com/mdastpak/crypto-exchange/internal/handlers/user"
	"github.com/mdastpak/crypto-exchange/internal/identity"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestConfigureDestinations_SetsTheProviderOfTheSettings checks that the
// server gives the destinations the provider that identity.bank_ownership
// names, and refuses an unknown one.
func TestConfigureDestinations_SetsTheProviderOfTheSettings(t *testing.T) {
	saved := config.Config.Identity.BankOwnership
	t.Cleanup(func() { config.Config.Identity.BankOwnership = saved })

	h := &InfraHandlers{User: &user.UserHandler{Destinations: destinations.Store{}}}
	config.Config.Identity.BankOwnership = config.BankOwnershipConfig{Provider: config.BankOwnershipStub, TimeoutSeconds: 7}
	require.NoError(t, configureDestinations(h))
	store, ok := h.User.Destinations.(destinations.Store)
	require.True(t, ok)
	_, isStub := store.Ownership.(*identity.Stub)
	assert.True(t, isStub)
	assert.Equal(t, "7s", store.OwnershipTimeout.String())

	config.Config.Identity.BankOwnership = config.BankOwnershipConfig{Provider: config.BankOwnershipNone}
	require.NoError(t, configureDestinations(h))
	store = h.User.Destinations.(destinations.Store)
	assert.Nil(t, store.Ownership, "none is no provider: cards, IBANs, and accounts cannot be added")

	config.Config.Identity.BankOwnership = config.BankOwnershipConfig{Provider: "finnotech"}
	require.Error(t, configureDestinations(h))
}
