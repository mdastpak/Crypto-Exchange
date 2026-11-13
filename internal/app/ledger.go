package app

import (
	"context"
	"database/sql"
	"log"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/ledger"
	"github.com/mdastpak/crypto-exchange/internal/metrics"
)

// ledgerAuditInterval is how often the books are snapshotted and
// reconciled. A reconciliation reads every balance and its changes.
const ledgerAuditInterval = time.Hour

// startLedgerAudit starts the job that snapshots and reconciles the books
// (#60) at startup and every ledgerAuditInterval, on one instance at a time.
func startLedgerAudit(conn *sql.DB) worker {
	a := &ledgerAuditor{books: ledger.Ledger{DB: conn}, interval: ledgerAuditInterval,
		stop: make(chan struct{}), done: make(chan struct{})}
	a.start()
	return worker{"ledger audit", a.Stop}
}

// ledgerAuditor runs ledger.Audit at once and then every interval, until
// Stop.
type ledgerAuditor struct {
	books    ledger.Ledger
	interval time.Duration
	stop     chan struct{}
	done     chan struct{}
}

func (a *ledgerAuditor) start() {
	go func() {
		defer close(a.done)
		ticker := time.NewTicker(a.interval)
		defer ticker.Stop()
		for {
			a.pass()
			select {
			case <-ticker.C:
			case <-a.stop:
				return
			}
		}
	}()
}

func (a *ledgerAuditor) pass() {
	ctx, cancel := context.WithTimeout(context.Background(), a.interval)
	defer cancel()
	go func() {
		select {
		case <-a.stop:
			cancel()
		case <-ctx.Done():
		}
	}()
	// No source of custody holdings yet (#67): the books are checked
	// against themselves.
	report, snapshots, ran, err := a.books.Audit(ctx, nil)
	switch {
	case err != nil && ctx.Err() == nil:
		log.Printf("Failed to audit the ledger: %v", err)
		return
	case err != nil || !ran:
		return
	}
	recordAudit(report)
	if !report.OK() {
		log.Printf("SECURITY: the ledger does not reconcile: %d of %d balances differ from the replay of their changes, %d currencies do not balance; see admin ledger reconcile",
			len(report.Mismatches), report.Balances, imbalanced(report))
	} else if snapshots > 0 {
		log.Printf("Audited the ledger: %d balances replayed, %d snapshots recorded", report.Balances, snapshots)
	}
}

// recordAudit sets the metrics of a completed reconciliation.
func recordAudit(report ledger.Report) {
	metrics.LedgerMismatches.Set(float64(len(report.Mismatches)))
	metrics.LedgerImbalancedCurrencies.Set(float64(imbalanced(report)))
	metrics.LedgerLastReconciliation.SetToCurrentTime()
}

// imbalanced counts the currencies whose books do not sum to zero, or
// whose custody holds other than it must.
func imbalanced(report ledger.Report) int {
	n := 0
	for _, c := range report.Currencies {
		if !c.Imbalance.IsZero() || (c.Difference != nil && !c.Difference.IsZero()) {
			n++
		}
	}
	return n
}

// Stop ends the job and waits for a pass in progress, until ctx ends.
func (a *ledgerAuditor) Stop(ctx context.Context) error {
	select {
	case <-a.stop:
	default:
		close(a.stop)
	}
	select {
	case <-a.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
