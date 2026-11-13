package app

import (
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
	"github.com/mdastpak/crypto-exchange/internal/ledger"
	"github.com/mdastpak/crypto-exchange/internal/metrics"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
)

func TestRecordAudit_SetsTheMetricsOfAReconciliation(t *testing.T) {
	off := shared.MustNewDecimalFromString("-0.5")
	report := ledger.Report{
		Balances:   3,
		Mismatches: []ledger.Mismatch{{Reason: "x"}, {Reason: "y"}},
		Currencies: []ledger.CurrencyReport{
			{Symbol: "BTC"},
			{Symbol: "USDT", Imbalance: shared.MustNewDecimalFromString("1")},
			{Symbol: "TRX", Difference: &off},
		},
	}
	recordAudit(report)
	assert.Equal(t, 2.0, testutil.ToFloat64(metrics.LedgerMismatches))
	assert.Equal(t, 2.0, testutil.ToFloat64(metrics.LedgerImbalancedCurrencies), "an imbalance and a custody difference")
	assert.Positive(t, testutil.ToFloat64(metrics.LedgerLastReconciliation))

	recordAudit(ledger.Report{Currencies: []ledger.CurrencyReport{{Symbol: "BTC"}}})
	assert.Zero(t, testutil.ToFloat64(metrics.LedgerMismatches))
	assert.Zero(t, testutil.ToFloat64(metrics.LedgerImbalancedCurrencies))
}
