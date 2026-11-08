package identity

import (
	"fmt"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/config"
)

// FromConfig returns the provider of identity.mobile_ownership, or nil for
// none.
func FromConfig(c config.MobileOwnershipConfig) (MobileOwnership, error) {
	switch c.Provider {
	case "", config.MobileOwnershipNone:
		return nil, nil
	case config.MobileOwnershipStub:
		return &Stub{MatchUnlisted: true}, nil
	case config.MobileOwnershipJibit:
		return NewJibit(c.Endpoint, c.APIKey, c.SecretKey, time.Duration(c.TimeoutSeconds)*time.Second), nil
	}
	return nil, fmt.Errorf("unknown mobile ownership provider %q", c.Provider)
}
