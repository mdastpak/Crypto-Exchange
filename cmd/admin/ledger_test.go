package main

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
	"github.com/mdastpak/crypto-exchange/internal/ledger"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseArgs_Ledger(t *testing.T) {
	for _, name := range []string{"reconcile", "snapshot"} {
		cmd, err := parseArgs([]string{"ledger", name}, &bytes.Buffer{})
		require.NoError(t, err, name)
		assert.Equal(t, "ledger "+name, cmd.name)
	}
	for _, args := range []string{"ledger", "ledger fix", "ledger reconcile now"} {
		_, err := parseArgs(strings.Fields(args), &bytes.Buffer{})
		assert.Error(t, err, args)
	}
}

// TestRun_LedgerReconcile checks the books from the command, which exits
// with 1 once a balance was changed outside the ledger.
func TestRun_LedgerReconcile(t *testing.T) {
	ctx := context.Background()
	resetCLITestDB(t)
	conn, err := sql.Open("pgx", fmt.Sprintf("host=localhost port=5432 user=postgres password=test dbname=%s sslmode=disable", cliTestDBName))
	require.NoError(t, err)
	defer conn.Close()
	// The first command migrates the empty database; empty books reconcile.
	code, stdout, stderr := runCLI(t, append(writeConfig(t), "ledger", "reconcile")...)
	require.Equal(t, 0, code, stderr)
	assert.Contains(t, stdout, "Replayed 0 balances")

	var user, currency shared.UUID
	require.NoError(t, conn.QueryRow(`INSERT INTO users (status) VALUES ('ACTIVE') RETURNING hk`).Scan(&user))
	require.NoError(t, conn.QueryRow(`INSERT INTO currencies (status, info) VALUES ('approved', '{"symbol": "USDT", "en_name": "Tether",
		"fa_name": "تتر", "if_fiat": false, "is_token": true, "contract": "T"}') RETURNING hk`).Scan(&currency))
	books := ledger.Ledger{DB: conn}
	deposit := ledger.Amount{Currency: currency, Value: shared.MustNewDecimalFromString("100")}
	_, err = books.DetectDeposit(ctx, "d:detect", ledger.Account{UserHK: user}, deposit, ledger.Ref{})
	require.NoError(t, err)
	_, err = books.CreditDeposit(ctx, "d:credit", ledger.Account{UserHK: user}, deposit, ledger.Ref{})
	require.NoError(t, err)

	code, stdout, stderr = runCLI(t, append(writeConfig(t), "ledger", "reconcile")...)
	require.Equal(t, 0, code, stderr)
	assert.Contains(t, stdout, "USDT")
	assert.Contains(t, stdout, "The ledger reconciles.")

	code, stdout, stderr = runCLI(t, append(writeConfig(t), "ledger", "snapshot")...)
	require.Equal(t, 0, code, stderr)
	assert.Contains(t, stdout, "Recorded 1 snapshots")

	_, err = conn.Exec(`UPDATE balances SET available = available + 1`)
	require.NoError(t, err)
	code, stdout, stderr = runCLI(t, append(writeConfig(t), "ledger", "reconcile")...)
	assert.Equal(t, 1, code)
	assert.Contains(t, stdout, "1 differ from their changes")
	assert.Contains(t, stderr, "does not reconcile")
}
