package db

import (
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/testutil/pgtest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// notMoney are the numeric columns outside the exchange's books: scores, and
// the observed figures of third-party market data (docs/market-data.md),
// which no balance is computed from.
func notMoney(table, column string) bool {
	if column == "risk_score" {
		return true
	}
	return len(table) > len("market_") && table[:len("market_")] == "market_" && table != "market_stats_24h"
}

// TestSchema_EveryAmountIsExact checks that every amount, price, rate, and
// ratio of the exchange is an exact_decimal (#314,
// docs/architecture.md#amounts-prices-and-ratios), never a NUMERIC with a
// fixed scale, which rounds what it cannot hold.
func TestSchema_EveryAmountIsExact(t *testing.T) {
	conn := pgtest.Migrated(t, "testdb_amounts")
	rows, err := conn.Query(`SELECT table_name, column_name, COALESCE(domain_name, '') FROM information_schema.columns
		WHERE table_schema = 'public' AND data_type = 'numeric' ORDER BY 1, 2`)
	require.NoError(t, err)
	defer rows.Close()
	var wrong []string
	exact := 0
	for rows.Next() {
		var table, column, domain string
		require.NoError(t, rows.Scan(&table, &column, &domain))
		switch {
		case notMoney(table, column):
		case domain == "exact_decimal":
			exact++
		default:
			wrong = append(wrong, table+"."+column)
		}
	}
	require.NoError(t, rows.Err())
	assert.Empty(t, wrong, "money columns that are not exact_decimal")
	assert.Greater(t, exact, 60, "the books' columns are found")
}

// TestExactDecimal_HoldsTheSmallestUnitOfEveryAsset stores the smallest unit
// of a 24-decimal asset (1 yoctoNEAR) and a 36-decimal one without loss, and
// refuses, instead of rounding, a value or an asset it cannot hold.
func TestExactDecimal_HoldsTheSmallestUnitOfEveryAsset(t *testing.T) {
	conn := pgtest.Migrated(t, "testdb_amounts")
	near := insertCurrency(t, conn, "NEAR")
	_, err := conn.Exec(`INSERT INTO currency_networks (currency_hk, network, decimals, custody_mode, min_withdrawal, withdrawal_fee)
		VALUES ($1, 'near', 24, 'ON_CHAIN', 0.000000000000000000000001, 123456789012345678.000000000000000000000001)`, near)
	require.NoError(t, err)
	var minimum, fee string
	require.NoError(t, conn.QueryRow(`SELECT min_withdrawal::text, withdrawal_fee::text FROM currency_networks WHERE currency_hk = $1`, near).
		Scan(&minimum, &fee))
	assert.Equal(t, "0.000000000000000000000001", minimum)
	assert.Equal(t, "123456789012345678.000000000000000000000001", fee)

	var widest string
	require.NoError(t, conn.QueryRow(`SELECT ('0.' || repeat('0', 35) || '1')::exact_decimal::text`).Scan(&widest))
	assert.Equal(t, "0."+repeatZero(35)+"1", widest)

	for name, value := range map[string]string{
		"37 decimals":       "0." + repeatZero(36) + "1",
		"43 integer digits": "1" + repeatZero(42),
	} {
		_, err := conn.Exec(`SELECT $1::numeric::exact_decimal`, value)
		assert.Equal(t, "23514", pgCode(err), "%s is refused, not rounded: %v", name, err)
	}

	_, err = conn.Exec(`INSERT INTO currency_networks (currency_hk, network, decimals, custody_mode) VALUES ($1, 'wide', 37, 'ON_CHAIN')`,
		insertCurrency(t, conn, "WIDE"))
	assert.Equal(t, "23514", pgCode(err), "a network with more decimals than an amount holds is refused: %v", err)
}

func repeatZero(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = '0'
	}
	return string(b)
}
