package identity

import (
	"context"
	"fmt"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/config"

	"github.com/google/uuid"
)

// BankAccount is a card, an IBAN (Sheba), or a bank account number of a
// bank, as a withdrawal destination names it (#160).
type BankAccount struct {
	// Kind is card, iban, or account.
	Kind string
	// Number is the card number, the IBAN, or the account number, in its
	// canonical form: digits only, and IR with digits for an IBAN.
	Number string
	// Bank is the bank of an account number, which needs it; and of a
	// card or an IBAN, when known.
	Bank string
}

// BankOwnership checks that a card, an IBAN, or a bank account belongs to
// a national ID (#158). Providers such as Jibit and Finnotech offer it;
// their clients are added as their contracts arrive. Implementations are
// safe for concurrent use.
type BankOwnership interface {
	// Provider names the provider, as the destinations record it.
	Provider() string
	// MatchBank reports whether the account belongs to the person: their
	// national ID, and the date of birth that Jibit checks with it. An
	// error means that the provider could not answer; it wraps
	// ErrUnavailable when trying again later may work, and ErrUnsupported
	// for a kind of account that the provider does not check.
	MatchBank(ctx context.Context, p Person, account BankAccount) (Match, error)
}

// Provider checks the owner of a card or an IBAN, and a national ID with
// its date of birth (#158): what identity.bank_ownership selects.
type Provider interface {
	BankOwnership
	IdentityInquiry
}

// BankFromConfig returns the provider of identity.bank_ownership, or nil
// for none. jibit uses the endpoint and the credentials of mobile, the
// configuration of identity.mobile_ownership.
func BankFromConfig(c config.BankOwnershipConfig, mobile config.MobileOwnershipConfig) (Provider, error) {
	switch c.Provider {
	case "", config.BankOwnershipNone:
		return nil, nil
	case config.BankOwnershipStub:
		return &Stub{MatchUnlisted: true}, nil
	case config.BankOwnershipJibit:
		return NewJibit(mobile.Endpoint, mobile.APIKey, mobile.SecretKey, c.Timeout()), nil
	}
	return nil, fmt.Errorf("unknown bank ownership provider %q", c.Provider)
}

// MatchBank answers from Owners, which maps the number of a card, an IBAN,
// or an account to the national ID it belongs to.
func (s *Stub) MatchBank(_ context.Context, p Person, account BankAccount) (Match, error) {
	nationalID := p.NationalID
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Calls++
	if s.Unavailable {
		return Match{}, ErrUnavailable
	}
	owner, listed := s.Owners[account.Number]
	matched := (listed && owner == nationalID) || (!listed && s.MatchUnlisted)
	return Match{Matched: matched, Ref: "stub-" + uuid.NewString()}, nil
}

var _ BankOwnership = (*Stub)(nil)

// Identify answers from Births, which maps a national ID to its date of
// birth, YYYY-MM-DD; a national ID it does not list matches when
// MatchUnlisted is set.
func (s *Stub) Identify(_ context.Context, p Person) (Match, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Calls++
	if s.Unavailable {
		return Match{}, ErrUnavailable
	}
	birth, listed := s.Births[p.NationalID]
	matched := (listed && birth == p.BirthDate.Format(time.DateOnly)) || (!listed && s.MatchUnlisted)
	return Match{Matched: matched, Ref: "stub-" + uuid.NewString()}, nil
}

var _ IdentityInquiry = (*Stub)(nil)
