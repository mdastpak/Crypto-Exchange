package identity

import (
	"context"
	"sync"

	"github.com/google/uuid"
)

// Stub is a MobileOwnership and a BankOwnership for development and tests.
// It matches the numbers that Owners lists to their national ID, and every
// other number when MatchUnlisted is set. Release mode refuses it.
type Stub struct {
	mu sync.Mutex
	// Owners maps a mobile number, or the number of a card, an IBAN, or an
	// account, to the national ID it belongs to.
	Owners map[string]string
	// MatchUnlisted matches the numbers that Owners does not list.
	MatchUnlisted bool
	// Unavailable makes every check fail with ErrUnavailable.
	Unavailable bool
	// Calls counts the checks.
	Calls int
}

// Provider names the stub.
func (s *Stub) Provider() string { return "stub" }

// SetOwner records that mobile belongs to nationalID.
func (s *Stub) SetOwner(mobile, nationalID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Owners == nil {
		s.Owners = map[string]string{}
	}
	s.Owners[mobile] = nationalID
}

// SetUnavailable makes the checks fail, or answer again.
func (s *Stub) SetUnavailable(unavailable bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Unavailable = unavailable
}

// Match answers from Owners.
func (s *Stub) Match(_ context.Context, nationalID, mobile string) (Match, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Calls++
	if s.Unavailable {
		return Match{}, ErrUnavailable
	}
	owner, listed := s.Owners[mobile]
	matched := (listed && owner == nationalID) || (!listed && s.MatchUnlisted)
	return Match{Matched: matched, Ref: "stub-" + uuid.NewString()}, nil
}
