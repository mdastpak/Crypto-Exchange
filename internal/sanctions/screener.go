package sanctions

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/aml"
	"github.com/mdastpak/crypto-exchange/internal/db"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"
)

// The screening (#73). The AML rules of type SANCTIONS that name the ofac
// provider screen, by config.screen:
//
//   - counterparty: the address on chain of the other side, such as a
//     crypto withdrawal destination, against the addresses of the list. A
//     match takes the rule's action.
//   - user: the user's first and last name, as the profile has them, against
//     the names and other names of the list, by similarity. A name alone
//     does not tell a person, so a match is held for review, never more.
//
// Without a list, or with one that no download found current for MaxAge,
// every operation that a rule screens is held for review: the screening
// fails closed.

// Reason of a hit without a usable list.
const reasonListUnavailable = "list_unavailable"

// DefaultThreshold is the similarity from which names match.
const DefaultThreshold = 0.92

// Screener screens against the newest version of a list, which it keeps in
// memory and reloads when another is stored.
type Screener struct {
	DB *sql.DB
	// Provider is the list it loads: ProviderOFAC.
	Provider string
	// Threshold is the similarity from which names match.
	Threshold float64
	// MaxAge is how long a list that no download found current is used.
	MaxAge time.Duration
	// Now is the clock; nil is time.Now.
	Now func() time.Time

	mu      sync.RWMutex
	list    *List
	checked time.Time
}

func (s *Screener) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Screens reports whether s screens for r: a SANCTIONS rule of its
// provider.
func (s *Screener) Screens(r aml.Rule) bool {
	return r.Type == aml.TypeSanctions && slices.Contains(r.Config.Providers, s.Provider)
}

// Refresh loads the newest version of the list when it is not the one in
// memory, and when the newest was last found current. A version that
// fails its seal is not loaded: the one in memory, if any, stays until it
// is too old.
func (s *Screener) Refresh(ctx context.Context) error {
	v, err := Latest(ctx, s.DB, s.Provider)
	if err != nil {
		return err
	}
	s.mu.RLock()
	same := s.list != nil && s.list.Version == v.ID
	s.mu.RUnlock()
	if same {
		s.mu.Lock()
		s.checked = v.CheckedAt
		s.mu.Unlock()
		return nil
	}
	list, err := Load(ctx, s.DB, v)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.list, s.checked = list, v.CheckedAt
	s.mu.Unlock()
	log.Printf("Sanctions list %s version %d loaded: %d entries", v.Provider, v.ID, v.EntryCount)
	return nil
}

// current returns the list in memory, or nil when there is none, or it is
// too old.
func (s *Screener) current() *List {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.list == nil || s.now().Sub(s.checked) > s.MaxAge {
		return nil
	}
	return s.list
}

// Screen implements aml.Screener.
func (s *Screener) Screen(ctx context.Context, q aml.Querier, r aml.Stored, e aml.Event) (aml.Action, map[string]any, error) {
	if r.Config.Screen == "counterparty" && e.CounterpartyAddress == "" {
		// Nothing to screen, such as a fiat destination, list or not.
		return aml.ActionNone, nil, nil
	}
	list := s.current()
	if list == nil {
		return aml.ActionReview, map[string]any{"reason": reasonListUnavailable, "list": s.Provider}, nil
	}
	data := map[string]any{"list": list.Provider, "list_version": list.Version}
	switch r.Config.Screen {
	case "counterparty":
		if e.CounterpartyAddress == "" {
			return aml.ActionNone, nil, nil
		}
		matches := list.MatchAddress(e.CounterpartyAddress)
		if len(matches) == 0 {
			return aml.ActionNone, nil, nil
		}
		data["matched"] = "address"
		data["entries"] = describe(matches)
		return r.Action, data, nil
	case "user":
		name, err := userName(ctx, q, e.UserHK)
		if err != nil {
			return aml.ActionNone, nil, err
		}
		threshold := s.Threshold
		if threshold <= 0 {
			threshold = DefaultThreshold
		}
		matches := list.MatchName(name, threshold)
		if len(matches) == 0 {
			return aml.ActionNone, nil, nil
		}
		data["matched"] = "name"
		data["entries"] = describe(matches)
		return aml.ActionReview.Weaker(r.Action), data, nil
	}
	return aml.ActionNone, nil, nil
}

// describe is what an alert shows of the entries that matched: public
// facts of the list, at most five.
func describe(matches []Match) []map[string]any {
	out := make([]map[string]any, 0, min(len(matches), 5))
	for _, m := range matches[:min(len(matches), 5)] {
		out = append(out, map[string]any{"id": m.Entry.ID, "name": m.Entry.Name, "type": m.Entry.Type,
			"programs": m.Entry.Programs, "score": fmt.Sprintf("%.3f", m.Score)})
	}
	return out
}

// userName is the first and last name of the user's profile, "" when the
// user set none, checked against the seal of the user's row.
func userName(ctx context.Context, q aml.Querier, userHK string) (string, error) {
	var plain db.Names
	var sealed db.SealedNames
	t := rowseal.Users
	r := t.NewRow()
	if err := q.QueryRowContext(ctx, `SELECT first_name, last_name, first_name_enc, last_name_enc, `+t.Fields("")+` FROM users WHERE hk = $1`, userHK).
		Scan(append([]any{&plain.First, &plain.Last, &sealed.First, &sealed.Last}, r.Dest()...)...); err != nil {
		return "", fmt.Errorf("sanctions: read the user's name: %w", err)
	}
	// The seal covers the names: one changed in SQL to slip past the list
	// is refused.
	if err := rowseal.Check(ctx, t, r); err != nil {
		return "", err
	}
	n, err := db.OpenNames(userHK, plain, sealed)
	if err != nil {
		return "", fmt.Errorf("sanctions: open the user's name: %w", err)
	}
	var parts []string
	for _, p := range []*string{n.First, n.Last} {
		if p != nil && strings.TrimSpace(*p) != "" {
			parts = append(parts, *p)
		}
	}
	return strings.Join(parts, " "), nil
}

// Status is the state of the list in memory, for the administrators.
type Status struct {
	Version
	// Stale is a list that no download found current for the maximum
	// age, or none: every operation that a rule screens is held.
	Stale bool `json:"stale"`
}

// Status returns the stored list and whether it is usable.
func (s *Screener) Status(ctx context.Context) (Status, error) {
	v, err := Latest(ctx, s.DB, s.Provider)
	if err != nil && !errors.Is(err, ErrNoList) {
		return Status{}, err
	}
	return Status{Version: v, Stale: s.current() == nil}, nil
}

// Update downloads the OFAC list when the newest stored version was last
// found current longer than every ago, or there is none, and stores it.
// Each instance may call it; the first to save wins, and the others find
// the list current.
func Update(ctx context.Context, conn *sql.DB, client *http.Client, sdnURL string, every time.Duration) (Version, bool, error) {
	if v, err := Latest(ctx, conn, ProviderOFAC); err == nil && time.Since(v.CheckedAt) < every {
		return v, false, nil
	}
	sdn, alt, err := FetchOFAC(ctx, client, sdnURL)
	if err != nil {
		return Version{}, false, err
	}
	entries, err := ParseOFAC(sdn, alt)
	if err != nil {
		return Version{}, false, err
	}
	source := append(append([]byte{}, sdn...), alt...)
	return Save(ctx, conn, ProviderOFAC, source, entries)
}
