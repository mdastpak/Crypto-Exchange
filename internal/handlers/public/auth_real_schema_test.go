package public

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"database/sql"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/auth"
	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/db"
	"github.com/mdastpak/crypto-exchange/internal/models"
	"github.com/mdastpak/crypto-exchange/internal/testutil/pgtest"
	"github.com/mdastpak/crypto-exchange/internal/testutil/redistest"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The tests in this file run the OTP sign-in flow against a database built by
// the real migrations, so that they see the production constraints on users.

const realSchemaTestDBName = "testdb_public_real_schema"

// realSchemaRedisDB is the database of this package. The suites of a package
// run one after another, so they can share it.
const realSchemaRedisDB = redistest.Public

// setupRealSchemaHandler migrates a dedicated database with the repository
// migrations and returns a handler wired to it and to Redis database 2.
func setupRealSchemaHandler(t *testing.T) (*PublicHandler, *sql.DB, *redis.Client) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	savedConfig := config.Config
	t.Cleanup(func() { config.Config = savedConfig })
	config.Config.OTP.Auth.Length = 6
	config.Config.OTP.Auth.TTL = 300
	config.Config.OTP.Auth.RetryLimit = 5
	config.Config.JWT.AccessToken.Expiration = 15
	config.Config.JWT.RefreshToken.Expiration = 60
	config.Config.JWT.Issuer = "test"
	initTestSigningKeys(t)

	postgresService := migrateRealSchema(t)

	redisClient := redis.NewClient(&redis.Options{Addr: "localhost:6379", DB: realSchemaRedisDB})
	require.NoError(t, redisClient.Ping(context.Background()).Err())
	t.Cleanup(func() {
		redisClient.FlushDB(context.Background())
		redisClient.Close()
	})

	handler := NewHandler(&models.InfraServices{Config: &config.Config,
		Postgres: postgresService,
		Redis:    &db.RedisService{Client: redisClient},
	})
	return handler, postgresService.DB, redisClient
}

// migrateRealSchema resets the dedicated database, applies the repository
// migrations through db.NewPostgresService, and closes the service when the
// test ends.
func migrateRealSchema(t *testing.T) *db.PostgresService {
	t.Helper()
	savedConfig := config.Config
	savedMigrationsPath := config.MigrationsPath()
	t.Cleanup(func() {
		config.Config = savedConfig
		config.SetMigrationsPath(savedMigrationsPath)
	})

	config.Config.Postgres.Host = "localhost"
	config.Config.Postgres.Port = "5432"
	config.Config.Postgres.User = "postgres"
	config.Config.Postgres.Password = "test"
	config.Config.Postgres.DBName = realSchemaTestDBName
	config.Config.Postgres.SSLMode = "disable"
	config.Config.Postgres.ConnectRetries = 1
	config.Config.Postgres.ConnectRetryDelayMs = 10

	migrationsDir, err := filepath.Abs(filepath.Join("..", "..", "db", "migrations"))
	require.NoError(t, err)
	config.SetMigrationsPath("file://" + strings.ReplaceAll(migrationsDir, "\\", "/"))

	ensureRealSchemaTestDB(t)
	// Emptied and migrated one package at a time (#338).
	unlock := pgtest.Lock(t)
	resetRealSchema(t)
	t.Cleanup(func() {
		defer pgtest.Lock(t)()
		resetRealSchema(t)
	})

	postgresService, err := db.NewPostgresService(db.Options{Config: &config.Config, MigrationsPath: config.MigrationsPath()})
	unlock()
	require.NoError(t, err, "the repository migrations must apply to an empty database")
	t.Cleanup(func() { postgresService.Close() })
	return postgresService
}

func initTestSigningKeys(t *testing.T) {
	t.Helper()
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	privateKeyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(privateKey)})
	publicKeyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PUBLIC KEY", Bytes: x509.MarshalPKCS1PublicKey(&privateKey.PublicKey)})
	require.NoError(t, auth.InitKeys(privateKeyPEM, publicKeyPEM))
}

// ensureRealSchemaTestDB creates the dedicated database through the default
// administrative database, because CREATE DATABASE cannot target itself.
func ensureRealSchemaTestDB(t *testing.T) {
	t.Helper()
	adminDB, err := sql.Open("pgx", "host=localhost port=5432 user=postgres password=test dbname=postgres sslmode=disable")
	require.NoError(t, err)
	defer adminDB.Close()

	var exists bool
	require.NoError(t, adminDB.QueryRow("SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname = $1)", realSchemaTestDBName).Scan(&exists))
	if !exists {
		_, err = adminDB.Exec("CREATE DATABASE " + realSchemaTestDBName)
		require.NoError(t, err)
	}
}

func resetRealSchema(t *testing.T) {
	t.Helper()
	conn, err := sql.Open("pgx", fmt.Sprintf("host=localhost port=5432 user=postgres password=test dbname=%s sslmode=disable", realSchemaTestDBName))
	require.NoError(t, err)
	defer conn.Close()
	_, err = conn.Exec("DROP SCHEMA public CASCADE; CREATE SCHEMA public;")
	require.NoError(t, err)
}

func postJSON(t *testing.T, router *gin.Engine, path string, body interface{}) (int, models.APIResponse) {
	t.Helper()
	payload, err := json.Marshal(body)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	var resp models.APIResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	return w.Code, resp
}

func countUsersWithContact(t *testing.T, conn *sql.DB, contactType, contact string) int {
	t.Helper()
	var count int
	require.NoError(t, conn.QueryRow(
		"SELECT COUNT(*) FROM user_contacts WHERE type = $1::contact_type AND value = $2", contactType, contact,
	).Scan(&count))
	return count
}

func TestVerifyOTP_RealSchema_FirstSignInCreatesUser(t *testing.T) {
	handler, conn, _ := setupRealSchemaHandler(t)
	sender := &recordingOTPSender{}
	handler.OTPSender = sender
	router := gin.New()
	router.POST("/public/auth", handler.RequestOTP)
	router.POST("/public/auth/:uuid", handler.VerifyOTP)

	const contact = "first.signin@example.com"
	status, resp := postJSON(t, router, "/public/auth", map[string]string{"username": contact})
	require.Equal(t, http.StatusOK, status, resp.Message)
	sessionID := resp.Info.(map[string]interface{})["uuid"].(string)

	status, resp = postJSON(t, router, "/public/auth/"+sessionID, map[string]string{"otp": lastSentCode(t, sender)})
	require.Equal(t, http.StatusOK, status, resp.Message)
	tokens := resp.Info.(map[string]interface{})
	assert.NotEmpty(t, tokens["access_token"])
	assert.NotContains(t, tokens, "refresh_token", "the refresh token is only delivered in the cookie")

	assert.Equal(t, 1, countUsersWithContact(t, conn, "email", contact))
	var userStatus, role string
	var verified, primary bool
	require.NoError(t, conn.QueryRow(
		`SELECT u.status::text, u.role, c.verified_at IS NOT NULL, c.is_primary
		 FROM user_contacts c JOIN users u ON u.hk = c.user_hk WHERE c.type = 'email' AND c.value = $1`, contact,
	).Scan(&userStatus, &role, &verified, &primary))
	assert.Equal(t, "ACTIVE", userStatus)
	assert.Equal(t, "user", role)
	assert.True(t, verified, "the sign-in code proved control of the contact")
	assert.True(t, primary)
}

func TestCheckOrCreateUser_RealSchema_ReusesExistingUser(t *testing.T) {
	handler, conn, _ := setupRealSchemaHandler(t)
	ctx := context.Background()

	first, err := handler.checkOrCreateUser(ctx, audit.Event{}, "mobile", "09121234567", signUpInfo{})
	require.NoError(t, err)
	require.NotEqual(t, uuid.Nil, first)

	second, err := handler.checkOrCreateUser(ctx, audit.Event{}, "mobile", "09121234567", signUpInfo{})
	require.NoError(t, err)
	assert.Equal(t, first, second)
	assert.Equal(t, 1, countUsersWithContact(t, conn, "mobile", "09121234567"))
}

func TestCheckOrCreateUser_RealSchema_EmailIgnoresCase(t *testing.T) {
	handler, _, _ := setupRealSchemaHandler(t)
	ctx := context.Background()

	first, err := handler.checkOrCreateUser(ctx, audit.Event{}, "email", "Mixed.Case@Example.COM", signUpInfo{})
	require.NoError(t, err)
	second, err := handler.checkOrCreateUser(ctx, audit.Event{}, "email", "mixed.case@example.com", signUpInfo{})
	require.NoError(t, err)
	assert.Equal(t, first, second, "email addresses that differ only in case must resolve to the same user")
}

func TestCheckOrCreateUser_RealSchema_UsernameIsNotAContact(t *testing.T) {
	handler, conn, _ := setupRealSchemaHandler(t)

	_, err := handler.checkOrCreateUser(context.Background(), audit.Event{}, "username", "alice_2026", signUpInfo{})

	assert.Error(t, err, "usernames cannot receive codes, so they must not sign in or create users")
	assert.Equal(t, 0, countUsersWithContact(t, conn, "username", "alice_2026"))
}

func TestCheckOrCreateUser_RealSchema_ConcurrentFirstSignInCreatesOneUser(t *testing.T) {
	handler, conn, _ := setupRealSchemaHandler(t)
	const contact = "concurrent@example.com"
	const workers = 10

	start := make(chan struct{})
	results := make([]uuid.UUID, workers)
	errs := make([]error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			results[i], errs[i] = handler.checkOrCreateUser(ctx, audit.Event{}, "email", contact, signUpInfo{})
		}(i)
	}
	close(start)
	wg.Wait()

	for i := 0; i < workers; i++ {
		require.NoError(t, errs[i], "worker %d", i)
		assert.NotEqual(t, uuid.Nil, results[i])
		assert.Equal(t, results[0], results[i], "every concurrent sign-in must resolve to the same user")
	}
	assert.Equal(t, 1, countUsersWithContact(t, conn, "email", contact))
}

// TestCheckOrCreateUser_RealSchema_RecoversFromCompetingInsert forces the race
// deterministically: an open transaction inserts the same contact, so the
// handler's lookup misses it and the handler's insert waits on the unique
// index. Committing the transaction makes that insert fail with a unique
// violation, which must lead to the competing user, including its status check.
func TestCheckOrCreateUser_RealSchema_RecoversFromCompetingInsert(t *testing.T) {
	handler, conn, _ := setupRealSchemaHandler(t)

	cases := []struct {
		name, contact, status string
		wantErr               bool
	}{
		{name: "active user is reused", contact: "race-active@example.com", status: "ACTIVE"},
		{name: "inactive user stays blocked", contact: "race-inactive@example.com", status: "INACTIVE", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()

			tx, err := conn.BeginTx(ctx, nil)
			require.NoError(t, err)
			defer tx.Rollback()
			var competitor uuid.UUID
			require.NoError(t, tx.QueryRowContext(ctx,
				`INSERT INTO users (status) VALUES ($1) RETURNING hk`, tc.status,
			).Scan(&competitor))
			_, err = tx.ExecContext(ctx, `INSERT INTO user_contacts (user_hk, type, value) VALUES ($1, 'email', $2)`, competitor, tc.contact)
			require.NoError(t, err)

			type result struct {
				hk  uuid.UUID
				err error
			}
			done := make(chan result, 1)
			go func() {
				hk, err := handler.checkOrCreateUser(ctx, audit.Event{}, "email", tc.contact, signUpInfo{})
				done <- result{hk, err}
			}()

			// The handler's contact insert blocks on the uncommitted contact once its lookup has missed.
			require.Eventually(t, func() bool {
				var blocked bool
				err := conn.QueryRowContext(ctx, `SELECT EXISTS (
					SELECT 1 FROM pg_stat_activity
					WHERE datname = current_database() AND wait_event_type = 'Lock'
					  AND query LIKE 'INSERT INTO user_contacts %')`).Scan(&blocked)
				return err == nil && blocked
			}, 10*time.Second, 20*time.Millisecond, "the handler insert never waited on the competing row")
			require.NoError(t, tx.Commit())

			res := <-done
			if tc.wantErr {
				assert.Error(t, res.err, "the status check must also apply to a user created by a competing request")
				assert.Equal(t, uuid.Nil, res.hk)
				return
			}
			require.NoError(t, res.err)
			assert.Equal(t, competitor, res.hk, "the handler must return the user created by the competing transaction")
		})
	}
}

func TestUsersSchema_RealSchema_EnforcesContactRules(t *testing.T) {
	_, conn, _ := setupRealSchemaHandler(t)

	newUser := func() string {
		var hk string
		require.NoError(t, conn.QueryRow(`INSERT INTO users (status) VALUES ('ACTIVE') RETURNING hk`).Scan(&hk))
		return hk
	}
	addContact := func(userHK, contactType, value string) error {
		_, err := conn.Exec(`INSERT INTO user_contacts (user_hk, type, value) VALUES ($1, $2::contact_type, $3)`, userHK, contactType, value)
		return err
	}
	pgCode := func(err error) string {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			return pgErr.Code
		}
		return ""
	}

	uniqueCases := []struct {
		contactType, first, second string
	}{
		{"email", "duplicate@example.com", "duplicate@example.com"},
		{"email", "Case@Example.com", "case@EXAMPLE.COM"},
		{"mobile", "+989120000000", "+989120000000"},
		{"username", "duplicate_user", "duplicate_user"},
		{"username", "Case_User", "case_user"},
	}
	for _, tc := range uniqueCases {
		t.Run(fmt.Sprintf("unique %s %s", tc.contactType, tc.second), func(t *testing.T) {
			require.NoError(t, addContact(newUser(), tc.contactType, tc.first))
			err := addContact(newUser(), tc.contactType, tc.second)
			assert.Equal(t, "23505", pgCode(err), "%q of another user must conflict with %q", tc.second, tc.first)
		})
	}

	t.Run("one primary contact of each type", func(t *testing.T) {
		user := newUser()
		require.NoError(t, addContact(user, "email", "first@example.com"))
		assert.Equal(t, "23505", pgCode(addContact(user, "email", "second@example.com")))
		assert.NoError(t, addContact(user, "mobile", "+989120000009"), "a contact of another type is fine")
	})
	t.Run("rejects an empty contact", func(t *testing.T) {
		assert.Equal(t, "23514", pgCode(addContact(newUser(), "email", "")))
	})
	t.Run("rejects an unknown contact type", func(t *testing.T) {
		assert.Equal(t, "22P02", pgCode(addContact(newUser(), "telegram", "@someone")))
	})
	t.Run("rejects a contact without a user", func(t *testing.T) {
		assert.Equal(t, "23503", pgCode(addContact(uuid.NewString(), "email", "orphan@example.com")))
	})
}
