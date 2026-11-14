package app

import (
	"database/sql"
	"fmt"

	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/confirm"
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
	s := withdrawals.Service{DB: db, Confirm: confirmer, Destinations: book, Levels: levels, Ledger: ledger.Ledger{DB: db},
		ReviewAbove: review}
	h.User.Withdrawals, h.Admin.Withdrawals = s, s
	return nil
}
