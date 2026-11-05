package db

import (
	"bytes"
	"database/sql"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// insertSubAccount creates a user with a sub-account and returns the
// sub-account's ID, which API keys reference.
func insertSubAccount(t *testing.T, conn *sql.DB) string {
	t.Helper()
	var userHK, subAccount string
	require.NoError(t, conn.QueryRow(`INSERT INTO users (status) VALUES ('ACTIVE') RETURNING hk`).Scan(&userHK))
	require.NoError(t, conn.QueryRow(`INSERT INTO sub_accounts (user_id, label) VALUES ($1, 'trading') RETURNING id`, userHK).Scan(&subAccount))
	return subAccount
}

func checkViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23514"
}

func TestMigration16_KeepsOnlyTheVerificationMaterialOfEachKeyType(t *testing.T) {
	m := newMigrationsTestMigrator(t)
	require.NoError(t, m.Migrate(16))
	conn, err := sql.Open("pgx", migrationsTestDBConnStr())
	require.NoError(t, err)
	defer conn.Close()
	subAccount := insertSubAccount(t, conn)

	var secretHashExists bool
	require.NoError(t, conn.QueryRow(`SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'api_keys' AND column_name = 'secret_hash')`).Scan(&secretHashExists))
	assert.False(t, secretHashExists, "the plaintext column is gone")

	publicKey := bytes.Repeat([]byte{1}, 32)
	insert := func(keyID, keyType string, sealed, public []byte) error {
		_, err := conn.Exec(`INSERT INTO api_keys (sub_account_id, name, public_key, key_type, secret_sealed, ed25519_public_key)
			VALUES ($1, 'bot', $2, $3, $4, $5)`, subAccount, keyID, keyType, sealed, public)
		return err
	}
	require.NoError(t, insert("hmac_1", "HMAC", []byte("1:sealed"), nil))
	require.NoError(t, insert("ed25519_1", "ED25519", nil, publicKey))

	for name, err := range map[string]error{
		"HMAC without its sealed secret":  insert("hmac_2", "HMAC", nil, nil),
		"HMAC with a public key too":      insert("hmac_3", "HMAC", []byte("1:sealed"), publicKey),
		"Ed25519 without a public key":    insert("ed25519_2", "ED25519", nil, nil),
		"Ed25519 with a short public key": insert("ed25519_3", "ED25519", nil, publicKey[:31]),
		"Ed25519 with a secret stored":    insert("ed25519_4", "ED25519", []byte("1:sealed"), publicKey),
	} {
		assert.True(t, checkViolation(err), "%s: %v", name, err)
	}

	// Rolling back refuses while keys exist, because their secrets cannot go
	// back into a plaintext column.
	err = m.Migrate(15)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "delete them first")
	require.NoError(t, m.Force(16))
	_, err = conn.Exec(`DELETE FROM api_keys`)
	require.NoError(t, err)
	require.NoError(t, m.Migrate(15))
	require.NoError(t, conn.QueryRow(`SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'api_keys' AND column_name = 'secret_hash')`).Scan(&secretHashExists))
	assert.True(t, secretHashExists, "the down migration restores the schema of migration 8")
}

func TestMigration16_PreflightStopsOnPlaintextSecrets(t *testing.T) {
	m := newMigrationsTestMigrator(t)
	require.NoError(t, m.Migrate(15))
	conn, err := sql.Open("pgx", migrationsTestDBConnStr())
	require.NoError(t, err)
	defer conn.Close()
	subAccount := insertSubAccount(t, conn)
	_, err = conn.Exec(`INSERT INTO api_keys (sub_account_id, name, public_key, secret_hash) VALUES ($1, 'bot', 'hmac_old', 'plaintext secret')`, subAccount)
	require.NoError(t, err)

	err = m.Migrate(16)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "store their secret in plaintext")
	assert.Contains(t, err.Error(), `run "migrate force 15"`, "the error must explain how to recover from the dirty version")

	var secret string
	require.NoError(t, conn.QueryRow(`SELECT secret_hash FROM api_keys`).Scan(&secret))
	assert.Equal(t, "plaintext secret", secret, "a failed preflight leaves the table unchanged")
}
