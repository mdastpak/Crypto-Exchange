package adminauth

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base32"
	"encoding/base64"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/db"
	"github.com/mdastpak/crypto-exchange/internal/testutil/usertest"
	"github.com/mdastpak/crypto-exchange/internal/totp"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const adminAuthTestDBName = "testdb_adminauth"

// migratedTestDB resets a dedicated database, applies the repository
// migrations, and skips the test when PostgreSQL is not reachable.
func migratedTestDB(t *testing.T) *sql.DB {
	t.Helper()
	adminConn, err := sql.Open("pgx", "host=localhost port=5432 user=postgres password=test dbname=postgres sslmode=disable")
	require.NoError(t, err)
	defer adminConn.Close()
	if err := adminConn.Ping(); err != nil {
		t.Skipf("PostgreSQL is not reachable at localhost:5432: %v", err)
	}
	var exists bool
	require.NoError(t, adminConn.QueryRow(`SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)`, adminAuthTestDBName).Scan(&exists))
	if !exists {
		_, err := adminConn.Exec("CREATE DATABASE " + adminAuthTestDBName)
		require.NoError(t, err)
	}

	saved := config.Config.Postgres
	savedMigrations := config.MigrationsPath()
	t.Cleanup(func() {
		config.Config.Postgres = saved
		config.SetMigrationsPath(savedMigrations)
	})
	config.Config.Postgres.Host = "localhost"
	config.Config.Postgres.Port = "5432"
	config.Config.Postgres.User = "postgres"
	config.Config.Postgres.Password = "test"
	config.Config.Postgres.DBName = adminAuthTestDBName
	config.Config.Postgres.SSLMode = "disable"
	config.Config.Postgres.ConnectRetries = 1
	config.Config.Postgres.ConnectRetryDelayMs = 10
	migrations, err := filepath.Abs(filepath.Join("..", "db", "migrations"))
	require.NoError(t, err)
	config.SetMigrationsPath("file://" + filepath.ToSlash(migrations))

	reset, err := sql.Open("pgx", fmt.Sprintf("host=localhost port=5432 user=postgres password=test dbname=%s sslmode=disable", adminAuthTestDBName))
	require.NoError(t, err)
	_, err = reset.Exec("DROP SCHEMA public CASCADE; CREATE SCHEMA public;")
	reset.Close()
	require.NoError(t, err)

	service, err := db.NewPostgresService()
	require.NoError(t, err)
	t.Cleanup(func() { service.Close() })
	return service.DB
}

func newKey(t *testing.T) []byte {
	t.Helper()
	key := make([]byte, KeySize)
	_, err := rand.Read(key)
	require.NoError(t, err)
	return key
}

func newTestService(t *testing.T) (*Service, *sql.DB) {
	t.Helper()
	conn := migratedTestDB(t)
	service, err := NewService(conn, newKey(t), "Crypto Exchange")
	require.NoError(t, err)
	return service, conn
}

func decodeSecret(t *testing.T, encoded string) []byte {
	t.Helper()
	secret, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(encoded)
	require.NoError(t, err)
	return secret
}

// at fixes the service clock and returns the code for that moment.
func at(t *testing.T, service *Service, now time.Time, enrollment Enrollment) string {
	t.Helper()
	service.now = func() time.Time { return now }
	return totp.Code(decodeSecret(t, enrollment.Secret), totp.Step(now))
}

func TestCreate_StoresAnAdministratorWithAnEncryptedSecret(t *testing.T) {
	service, conn := newTestService(t)
	ctx := context.Background()

	enrollment, err := service.Create(ctx, "Admin@Example.com")
	require.NoError(t, err)

	assert.Equal(t, "admin@example.com", enrollment.Email, "addresses are stored in lower case")
	assert.Len(t, enrollment.Secret, 32)
	assert.Contains(t, enrollment.URI, "otpauth://totp/Crypto%20Exchange:admin@example.com?")
	assert.Contains(t, enrollment.URI, "secret="+enrollment.Secret)

	var role, status, stored string
	var enabled bool
	require.NoError(t, conn.QueryRow(
		`SELECT role, status::text, totp_secret, totp_enabled FROM users WHERE hk = $1`, enrollment.AdminID,
	).Scan(&role, &status, &stored, &enabled))
	assert.Equal(t, "admin", role)
	assert.Equal(t, "ACTIVE", status)
	assert.True(t, enabled)
	var address string
	var verified bool
	require.NoError(t, conn.QueryRow(
		`SELECT value, verified_at IS NOT NULL FROM user_contacts WHERE user_hk = $1 AND type = 'email' AND is_primary`, enrollment.AdminID,
	).Scan(&address, &verified))
	assert.Equal(t, "admin@example.com", address)
	assert.False(t, verified, "the operator entered the address; the first sign-in verifies it")
	assert.True(t, strings.HasPrefix(stored, "v1:"))
	assert.NotContains(t, stored, enrollment.Secret, "the secret is stored encrypted")
	assert.False(t, bytes.Contains([]byte(stored), decodeSecret(t, enrollment.Secret)))

	admin, err := service.FindActive(ctx, "ADMIN@example.com")
	require.NoError(t, err)
	assert.Equal(t, enrollment.AdminID, admin.ID)
}

func TestCreate_RefusesExistingAndInvalidAddresses(t *testing.T) {
	service, conn := newTestService(t)
	ctx := context.Background()

	_, err := service.Create(ctx, "admin@example.com")
	require.NoError(t, err)
	_, err = service.Create(ctx, "ADMIN@example.com")
	assert.ErrorIs(t, err, ErrUserExists)

	usertest.Insert(t, conn, usertest.User{Email: "trader@example.com"})
	_, err = service.Create(ctx, "trader@example.com")
	assert.ErrorIs(t, err, ErrUserExists, "an existing user is not silently turned into an administrator")
	_, err = service.FindActive(ctx, "trader@example.com")
	assert.ErrorIs(t, err, ErrNotFound, "a user is not an administrator")

	for _, address := range []string{"", "not-an-address", "Admin <admin2@example.com>", "admin2@example.com, other@example.com"} {
		_, err := service.Create(ctx, address)
		assert.ErrorIs(t, err, ErrInvalidEmail, "%q", address)
	}
}

func TestVerifyTOTP_AcceptsEachCodeOnce(t *testing.T) {
	service, _ := newTestService(t)
	ctx := context.Background()
	enrollment, err := service.Create(ctx, "admin@example.com")
	require.NoError(t, err)
	now := time.Unix(1_900_000_000, 0)

	code := at(t, service, now, enrollment)
	require.NoError(t, service.VerifyTOTP(ctx, enrollment.AdminID, code))
	assert.ErrorIs(t, service.VerifyTOTP(ctx, enrollment.AdminID, code), ErrInvalidCode, "a code cannot be replayed")

	earlier := totp.Code(decodeSecret(t, enrollment.Secret), totp.Step(now)-1)
	assert.ErrorIs(t, service.VerifyTOTP(ctx, enrollment.AdminID, earlier), ErrInvalidCode,
		"a code older than the last accepted one is refused")

	assert.ErrorIs(t, service.VerifyTOTP(ctx, enrollment.AdminID, "000000"), ErrInvalidCode)

	next := at(t, service, now.Add(totp.Period), enrollment)
	assert.NoError(t, service.VerifyTOTP(ctx, enrollment.AdminID, next), "the next code works")
}

func TestVerifyTOTP_NeedsTheKeyAndTheRightAdministrator(t *testing.T) {
	service, conn := newTestService(t)
	ctx := context.Background()
	first, err := service.Create(ctx, "first@example.com")
	require.NoError(t, err)
	second, err := service.Create(ctx, "second@example.com")
	require.NoError(t, err)
	now := time.Unix(1_900_000_000, 0)

	otherKey, err := NewService(conn, newKey(t), "Crypto Exchange")
	require.NoError(t, err)
	otherKey.now = func() time.Time { return now }
	err = otherKey.VerifyTOTP(ctx, first.AdminID, at(t, service, now, first))
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrInvalidCode, "a wrong key is a configuration error, not a wrong code")

	// A secret copied to another administrator does not decrypt.
	_, err = conn.Exec(`UPDATE users SET totp_secret = (SELECT totp_secret FROM users WHERE hk = $1) WHERE hk = $2`,
		first.AdminID, second.AdminID)
	require.NoError(t, err)
	assert.Error(t, service.VerifyTOTP(ctx, second.AdminID, at(t, service, now, first)))
}

func TestResetTOTP_ReplacesTheSecret(t *testing.T) {
	service, _ := newTestService(t)
	ctx := context.Background()
	old, err := service.Create(ctx, "admin@example.com")
	require.NoError(t, err)
	now := time.Unix(1_900_000_000, 0)
	require.NoError(t, service.VerifyTOTP(ctx, old.AdminID, at(t, service, now, old)))

	renewed, err := service.ResetTOTP(ctx, "Admin@Example.com")
	require.NoError(t, err)
	assert.Equal(t, old.AdminID, renewed.AdminID)
	assert.NotEqual(t, old.Secret, renewed.Secret)

	later := now.Add(totp.Period)
	assert.ErrorIs(t, service.VerifyTOTP(ctx, old.AdminID, at(t, service, later, old)), ErrInvalidCode,
		"the old secret stops working")
	assert.NoError(t, service.VerifyTOTP(ctx, old.AdminID, at(t, service, now, renewed)),
		"the replay guard starts over with the new secret")

	_, err = service.ResetTOTP(ctx, "nobody@example.com")
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestDisable_StopsSignIn(t *testing.T) {
	service, _ := newTestService(t)
	ctx := context.Background()
	enrollment, err := service.Create(ctx, "admin@example.com")
	require.NoError(t, err)
	now := time.Unix(1_900_000_000, 0)

	require.NoError(t, service.Disable(ctx, "admin@example.com"))

	_, err = service.FindActive(ctx, "admin@example.com")
	assert.ErrorIs(t, err, ErrNotFound)
	assert.ErrorIs(t, service.VerifyTOTP(ctx, enrollment.AdminID, at(t, service, now, enrollment)), ErrNotFound)
	assert.ErrorIs(t, service.Disable(ctx, "nobody@example.com"), ErrNotFound)
}

func TestDecodeKey(t *testing.T) {
	key := newKey(t)
	decoded, err := DecodeKey(base64.StdEncoding.EncodeToString(key))
	require.NoError(t, err)
	assert.Equal(t, key, decoded)

	for _, encoded := range []string{"", "not base64!", base64.StdEncoding.EncodeToString(key[:16])} {
		_, err := DecodeKey(encoded)
		assert.ErrorIs(t, err, ErrInvalidKey, "%q", encoded)
	}
	_, err = NewService(nil, key[:16], "")
	assert.ErrorIs(t, err, ErrInvalidKey)
}

func TestMaskEmail(t *testing.T) {
	assert.Equal(t, "adm*****@example.com", MaskEmail("admin@example.com"))
	assert.Equal(t, "a*****@example.com", MaskEmail("a@example.com"))
	assert.Equal(t, "*****", MaskEmail("no-at-sign"))
}

func TestIsActive(t *testing.T) {
	service, conn := newTestService(t)
	ctx := context.Background()
	enrollment, err := service.Create(ctx, "admin@example.com")
	require.NoError(t, err)

	active, err := service.IsActive(ctx, enrollment.AdminID)
	require.NoError(t, err)
	assert.True(t, active)

	for name, change := range map[string]string{
		"demoted":      `UPDATE users SET role = 'user' WHERE hk = $1`,
		"suspended":    `UPDATE users SET status = 'SUSPENDED' WHERE hk = $1`,
		"inactive":     `UPDATE users SET status = 'INACTIVE' WHERE hk = $1`,
		"without TOTP": `UPDATE users SET totp_enabled = FALSE WHERE hk = $1`,
		"soft-deleted": `UPDATE users SET deleted_at = NOW() WHERE hk = $1`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := conn.Exec(change, enrollment.AdminID)
			require.NoError(t, err)
			t.Cleanup(func() {
				_, err := conn.Exec(`UPDATE users SET role = 'admin', status = 'ACTIVE', totp_enabled = TRUE, deleted_at = NULL WHERE hk = $1`, enrollment.AdminID)
				require.NoError(t, err)
			})

			active, err := service.IsActive(ctx, enrollment.AdminID)
			require.NoError(t, err)
			assert.False(t, active)
		})
	}

	active, err = service.IsActive(ctx, "5b0c8f0e-58a6-4c3b-9d8e-2f7a1c3e9b10")
	require.NoError(t, err)
	assert.False(t, active)
}

func TestConfirmEmail_VerifiesTheAddressOnce(t *testing.T) {
	service, conn := newTestService(t)
	ctx := context.Background()
	enrollment, err := service.Create(ctx, "admin@example.com")
	require.NoError(t, err)
	verifiedAt := func() sql.NullTime {
		var at sql.NullTime
		require.NoError(t, conn.QueryRow(`SELECT verified_at FROM user_contacts WHERE user_hk = $1 AND type = 'email'`, enrollment.AdminID).Scan(&at))
		return at
	}

	require.NoError(t, service.ConfirmEmail(ctx, enrollment.AdminID))
	first := verifiedAt()
	require.True(t, first.Valid, "the first sign-in verifies the address")

	require.NoError(t, service.ConfirmEmail(ctx, enrollment.AdminID))
	assert.Equal(t, first.Time, verifiedAt().Time, "later sign-ins keep the first verification")
}

// auditRows returns the action, actor type, target, changes, and user agent
// of the audit events, oldest first.
func auditRows(t *testing.T, conn *sql.DB) []string {
	t.Helper()
	rows, err := conn.Query(`SELECT action || ' ' || actor_type || ' ' || target_type || '/' || target_id || ' ' ||
		COALESCE(changes::text, '-') || ' ' || COALESCE(user_agent, '-') FROM audit_events ORDER BY hk`)
	require.NoError(t, err)
	defer rows.Close()
	var got []string
	for rows.Next() {
		var row string
		require.NoError(t, rows.Scan(&row))
		got = append(got, row)
	}
	require.NoError(t, rows.Err())
	return got
}

// Every change of an administrator records one event in its transaction,
// with the operator and never the secret (#195).
func TestChanges_AreAudited(t *testing.T) {
	service, conn := newTestService(t)
	service.Origin = audit.Operator("cmd/admin test", "alice", "root", "ops-1")
	ctx := context.Background()

	enrollment, err := service.Create(ctx, "audited@example.com")
	require.NoError(t, err)
	reset, err := service.ResetTOTP(ctx, "audited@example.com")
	require.NoError(t, err)
	require.NoError(t, service.Disable(ctx, "audited@example.com"))

	target := "user/" + enrollment.AdminID
	agent := "cmd/admin test; operator=alice; os_user=root; host=ops-1"
	assert.Equal(t, []string{
		`admin.create system ` + target + ` {"after": {"role": "admin", "email": "audited@example.com", "status": "ACTIVE"}} ` + agent,
		`admin.totp_reset system ` + target + ` - ` + agent,
		`admin.disable system ` + target + ` {"after": {"status": "INACTIVE"}, "before": {"status": "ACTIVE"}} ` + agent,
	}, auditRows(t, conn))
	var all string
	require.NoError(t, conn.QueryRow(`SELECT string_agg(row_to_json(audit_events)::text, '') FROM audit_events`).Scan(&all))
	assert.NotContains(t, all, enrollment.Secret)
	assert.NotContains(t, all, reset.Secret)
}

// A change is refused when its event cannot be written: here, an origin
// that the checks of audit_events refuse, an admin actor without an id.
func TestChanges_AreRefusedWithoutTheirEvent(t *testing.T) {
	service, conn := newTestService(t)
	ctx := context.Background()
	enrollment, err := service.Create(ctx, "kept@example.com")
	require.NoError(t, err)
	service.Origin = audit.Event{ActorType: audit.ActorAdmin}

	_, err = service.Create(ctx, "refused@example.com")
	assert.Error(t, err)
	_, err = service.FindActive(ctx, "refused@example.com")
	assert.ErrorIs(t, err, ErrNotFound, "the administrator is not created")

	_, err = service.ResetTOTP(ctx, "kept@example.com")
	assert.Error(t, err)
	now := time.Unix(1_900_000_000, 0)
	require.NoError(t, service.VerifyTOTP(ctx, enrollment.AdminID, at(t, service, now, enrollment)), "the old secret still works")

	assert.Error(t, service.Disable(ctx, "kept@example.com"))
	_, err = service.FindActive(ctx, "kept@example.com")
	assert.NoError(t, err, "the administrator is not disabled")
	assert.Len(t, auditRows(t, conn), 1, "only the first creation is recorded")
}
