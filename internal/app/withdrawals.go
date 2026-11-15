package app

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/confirm"
	"github.com/mdastpak/crypto-exchange/internal/denylink"
	"github.com/mdastpak/crypto-exchange/internal/destinations"
	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
	"github.com/mdastpak/crypto-exchange/internal/ledger"
	"github.com/mdastpak/crypto-exchange/internal/withdrawals"
)

// useWithdrawals gives the user and admin routes the withdrawals (#70),
// with the confirmations that send their codes: it runs after
// useConfirmationSender, which sets the sender of those codes. Without the
// confirmations, the destinations, or the verification levels, the routes
// answer withdrawals_unavailable.
func useWithdrawals(h *InfraHandlers, db *sql.DB, settings *config.Configuration) error {
	confirmer, ok := h.User.Confirmations.(confirm.Service)
	book, bookOK := h.User.Destinations.(withdrawals.DestinationBook)
	levels, levelsOK := h.User.Verification.(withdrawals.LevelLimits)
	if !ok || !bookOK || !levelsOK {
		return nil
	}
	review := make(map[string]shared.Decimal, len(settings.Withdrawals.ReviewAbove))
	for symbol, amount := range settings.Withdrawals.ReviewAbove {
		d, err := shared.NewDecimalFromString(amount)
		if err != nil {
			return fmt.Errorf("withdrawals.review_above.%s: %w", symbol, err)
		}
		review[symbol] = d
	}
	// The "this was not me" links (#374): each message of a code carries
	// one while security.deny_links.base_url is set, and the public route
	// uses those issued, also after the links were turned off.
	links := denylink.Service{DB: db, BaseURL: settings.Security.DenyLinks.BaseURL,
		TTL:  time.Duration(settings.Security.DenyLinks.TTLHours) * time.Hour,
		Hold: time.Duration(settings.Security.ChangeHoldHours) * time.Hour}
	if links.BaseURL != "" {
		confirmer.Links = links
	}
	s := withdrawals.Service{DB: db, Confirm: confirmer, Destinations: book, Levels: levels, Ledger: ledger.Ledger{DB: db},
		ReviewAbove: review}
	links.Confirmations, links.Withdrawals = confirmer, s
	h.User.Confirmations = confirmer
	h.User.Withdrawals, h.Admin.Withdrawals = s, s
	h.Public.Denials = links
	return nil
}

// newPayouts returns the payouts of withdrawals (#70), by the manual
// provider, and gives them to the admin routes. ok is false without the
// withdrawals.
func newPayouts(h *InfraHandlers, db *sql.DB, settings *config.Configuration) (withdrawals.Payouts, bool, error) {
	book, ok := h.User.Destinations.(destinations.Store)
	if !ok || h.User.Withdrawals == nil {
		return withdrawals.Payouts{}, false, nil
	}
	loc, err := settings.Withdrawals.Payouts.Location()
	if err != nil {
		return withdrawals.Payouts{}, false, fmt.Errorf("withdrawals.payouts.timezone: %w", err)
	}
	p := withdrawals.Payouts{DB: db, Ledger: ledger.Ledger{DB: db}, Destinations: book, Provider: withdrawals.Manual{},
		Settings: settings.Withdrawals.Payouts, Location: loc}
	h.Admin.Payouts = p
	return p, true, nil
}

// startPayouts runs the payouts every withdrawals.payouts.interval_seconds
// on every instance: each claims the rows it works on, so that a
// withdrawal is planned once and a payout sent once. It returns nil
// without the withdrawals.
func startPayouts(h *InfraHandlers, db *sql.DB, settings *config.Configuration) (*worker, error) {
	p, ok, err := newPayouts(h, db, settings)
	if err != nil || !ok {
		return nil, err
	}
	r := &payoutRunner{payouts: p, interval: settings.Withdrawals.Payouts.Interval(), stop: make(chan struct{}), done: make(chan struct{})}
	r.start()
	return &worker{"withdrawal payouts", r.Stop}, nil
}

// payoutRunner runs the payouts at once and then every interval, until
// Stop.
type payoutRunner struct {
	payouts  withdrawals.Payouts
	interval time.Duration
	once     sync.Once
	stop     chan struct{}
	done     chan struct{}
}

func (r *payoutRunner) start() {
	go func() {
		defer close(r.done)
		ticker := time.NewTicker(r.interval)
		defer ticker.Stop()
		for {
			r.pass()
			select {
			case <-ticker.C:
			case <-r.stop:
				return
			}
		}
	}()
}

func (r *payoutRunner) pass() {
	ctx, cancel := context.WithTimeout(context.Background(), r.interval)
	defer cancel()
	go func() {
		select {
		case <-r.stop:
			cancel()
		case <-ctx.Done():
		}
	}()
	if _, err := r.payouts.Run(ctx); err != nil && ctx.Err() == nil {
		log.Printf("Failed to run the payouts of withdrawals: %v", err)
	}
}

// Stop ends the runner, and waits for a pass in progress until ctx ends.
func (r *payoutRunner) Stop(ctx context.Context) error {
	r.once.Do(func() { close(r.stop) })
	select {
	case <-r.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
