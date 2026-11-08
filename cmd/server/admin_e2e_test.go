package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"database/sql"
	"encoding/base32"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/adminauth"
	"github.com/mdastpak/crypto-exchange/internal/auth"
	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/middleware"
	"github.com/mdastpak/crypto-exchange/internal/notify"
	"github.com/mdastpak/crypto-exchange/internal/testutil/redistest"
	"github.com/mdastpak/crypto-exchange/internal/totp"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The end-to-end tests run the server's real services and routes. They need
// PostgreSQL, Redis, and RabbitMQ, and are skipped when one is not reachable.
const (
	e2eDBName     = "testdb_server_e2e"
	e2eRedisDB    = redistest.Server
	e2eRabbitMQ   = "amqp://guest:guest@localhost:5672/"
	e2ePostgresDS = "host=localhost port=5432 user=postgres password=test sslmode=disable dbname="
)

// capturingSender records the sign-in codes that the server would deliver.
type capturingSender struct {
	mu    sync.Mutex
	codes map[string][]string // contact -> codes, oldest first
}

func (s *capturingSender) Supports(contactType string) bool {
	return contactType == notify.ContactEmail || contactType == notify.ContactMobile
}

func (s *capturingSender) SendOTP(ctx context.Context, msg notify.OTPMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.codes == nil {
		s.codes = make(map[string][]string)
	}
	s.codes[msg.Contact] = append(s.codes[msg.Contact], msg.Code)
	return nil
}

func (s *capturingSender) last(contact string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	codes := s.codes[contact]
	if len(codes) == 0 {
		return ""
	}
	return codes[len(codes)-1]
}

func (s *capturingSender) count(contact string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.codes[contact])
}

type e2eEnv struct {
	router   *gin.Engine
	handlers *InfraHandlers
	sender   *capturingSender
	accounts *adminauth.Service
	db       *sql.DB
}

func skipUnlessReachable(t *testing.T) {
	t.Helper()
	conn, err := sql.Open("pgx", e2ePostgresDS+"postgres")
	require.NoError(t, err)
	defer conn.Close()
	if err := conn.Ping(); err != nil {
		t.Skipf("PostgreSQL is not reachable: %v", err)
	}
	client := redis.NewClient(&redis.Options{Addr: "localhost:6379", DB: e2eRedisDB})
	defer client.Close()
	if err := client.Ping(context.Background()).Err(); err != nil {
		t.Skipf("Redis is not reachable: %v", err)
	}
	broker, err := amqp.Dial(e2eRabbitMQ)
	if err != nil {
		t.Skipf("RabbitMQ is not reachable: %v", err)
	}
	broker.Close()
}

func resetE2EDatabase(t *testing.T) {
	t.Helper()
	admin, err := sql.Open("pgx", e2ePostgresDS+"postgres")
	require.NoError(t, err)
	defer admin.Close()
	var exists bool
	require.NoError(t, admin.QueryRow(`SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)`, e2eDBName).Scan(&exists))
	if !exists {
		_, err := admin.Exec("CREATE DATABASE " + e2eDBName)
		require.NoError(t, err)
	}
	conn, err := sql.Open("pgx", e2ePostgresDS+e2eDBName)
	require.NoError(t, err)
	defer conn.Close()
	_, err = conn.Exec("DROP SCHEMA public CASCADE; CREATE SCHEMA public;")
	require.NoError(t, err)
}

// The runtime role of the end-to-end tests. It is created once in the
// cluster and kept; each reset of the schema removes its privileges.
const (
	e2eRuntimeRole     = "testdb_server_e2e_runtime"
	e2eRuntimePassword = "runtime-test"
)

func ensureE2ERuntimeRole(t *testing.T) {
	t.Helper()
	admin, err := sql.Open("pgx", e2ePostgresDS+"postgres")
	require.NoError(t, err)
	defer admin.Close()
	_, err = admin.Exec(fmt.Sprintf(`DO $$ BEGIN
		CREATE ROLE %s LOGIN PASSWORD '%s';
	EXCEPTION WHEN duplicate_object THEN NULL;
	END $$`, e2eRuntimeRole, e2eRuntimePassword))
	require.NoError(t, err)
}

// e2eOwnerDB connects to the test database as postgres, which owns it, for
// the fixtures that the server's runtime role may not create.
func e2eOwnerDB(t *testing.T) *sql.DB {
	t.Helper()
	conn, err := sql.Open("pgx", e2ePostgresDS+e2eDBName)
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })
	return conn
}

func pemKeyPair(t *testing.T) auth.KeyPair {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	return auth.KeyPair{
		PrivateKey: pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}),
		PublicKey:  pem.EncodeToMemory(&pem.Block{Type: "RSA PUBLIC KEY", Bytes: x509.MarshalPKCS1PublicKey(&key.PublicKey)}),
	}
}

// newE2EEnv configures the server like main does, against test services.
// newE2EEnv starts the services and handlers as main does, against the
// test servers. options change the configuration before the services
// connect.
func newE2EEnv(t *testing.T, options ...func(*config.Configuration)) *e2eEnv {
	t.Helper()
	skipUnlessReachable(t)
	resetE2EDatabase(t)

	saved := config.Config
	savedMigrations := config.MigrationsPath()
	t.Cleanup(func() {
		config.Config = saved
		config.SetMigrationsPath(savedMigrations)
	})
	config.Config = config.Configuration{}
	c := &config.Config
	// The server runs with a runtime role that cannot change the schema,
	// and migrates with postgres, as production does (#180); so every
	// end-to-end test also checks that the grants are enough.
	ensureE2ERuntimeRole(t)
	c.Postgres.Host, c.Postgres.Port, c.Postgres.User, c.Postgres.Password = "localhost", "5432", e2eRuntimeRole, e2eRuntimePassword
	c.Postgres.Migrations.User, c.Postgres.Migrations.Password = "postgres", "test"
	c.Postgres.DBName, c.Postgres.SSLMode = e2eDBName, "disable"
	c.Postgres.ConnectRetries, c.Postgres.ConnectRetryDelayMs = 1, 10
	c.Redis.Host, c.Redis.Port, c.Redis.DB = "localhost", "6379", e2eRedisDB
	c.Redis.ConnectRetries, c.Redis.ConnectRetryDelayMs = 1, 10
	c.RabbitMQ.Host, c.RabbitMQ.Port, c.RabbitMQ.Username, c.RabbitMQ.Password = "localhost", "5672", "guest", "guest"
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	c.RabbitMQ.Exchanges.PriceUpdates = "e2e_price_updates_" + suffix
	c.RabbitMQ.Exchanges.TradeEvents = "e2e_trade_events_" + suffix
	c.RabbitMQ.Exchanges.Notifications = "e2e_notifications_" + suffix
	c.JWT.Issuer = "e2e-issuer"
	c.JWT.AccessToken.Expiration = 15
	c.JWT.RefreshToken.Expiration = 60
	c.OTP.Auth.Length, c.OTP.Auth.TTL, c.OTP.Auth.RetryLimit = 6, 60, 3
	// One secret for all OTP stores, as in production; only the namespace
	// separates user and admin sign-in.
	c.OTP.Auth.Secret = "an-e2e-otp-secret-of-at-least-32-bytes"
	c.Exchange.Name = "E2E Exchange"
	c.Security.ChangeHoldHours = 24
	c.Identity.MobileOwnership.TimeoutSeconds = 5
	totpKey := make([]byte, adminauth.KeySize)
	_, err := rand.Read(totpKey)
	require.NoError(t, err)
	c.Admin.TOTPKey = base64.StdEncoding.EncodeToString(totpKey)
	migrations, err := filepath.Abs(filepath.Join("..", "..", "internal", "db", "migrations"))
	require.NoError(t, err)
	config.SetMigrationsPath("file://" + filepath.ToSlash(migrations))

	require.NoError(t, auth.SetKeys(map[string]auth.KeyPair{
		auth.RoleUser:  pemKeyPair(t),
		auth.RoleAdmin: pemKeyPair(t),
	}))

	for _, option := range options {
		option(c)
	}
	services, cleanup, err := initializeServices()
	require.NoError(t, err)
	t.Cleanup(func() {
		services.Redis.Client.FlushDB(context.Background())
		cleanup()
		if conn, err := amqp.Dial(e2eRabbitMQ); err == nil {
			if ch, err := conn.Channel(); err == nil {
				for _, exchange := range []string{c.RabbitMQ.Exchanges.PriceUpdates, c.RabbitMQ.Exchanges.TradeEvents, c.RabbitMQ.Exchanges.Notifications} {
					_ = ch.ExchangeDelete(exchange, false, false)
				}
			}
			conn.Close()
		}
	})

	handlers := initializeHandlers(services)
	sender := &capturingSender{}
	handlers.Public.OTPSender = sender
	handlers.User.OTPSender = sender
	key, err := adminSignInKey()
	require.NoError(t, err)
	require.NoError(t, configureAdminSignIn(handlers, services, key, sender))
	require.NoError(t, handlers.Socket.StartConsumer(context.Background()))
	watcher, err := startPlatform(services, handlers)
	require.NoError(t, err)
	relay := newOutboxRelay(services, handlers, watcher)
	relay.Start()
	// Registered after the services cleanup, so it runs first, as in main.
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = watcher.Stop(ctx)
		_ = relay.Stop(ctx)
		_ = handlers.Socket.Shutdown(ctx)
	})

	gin.SetMode(gin.TestMode)
	router, err := newEngine()
	require.NoError(t, err)
	router.Use(contractCheck(t))
	router.Use(middleware.RecoveryMiddleware())
	require.NoError(t, RouteHandler(router, handlers))
	return &e2eEnv{router: router, handlers: handlers, sender: sender, accounts: handlers.Admin.Accounts, db: services.Postgres.DB}
}

type e2eResponse struct {
	Status  int                    `json:"status"`
	Message string                 `json:"message"`
	Info    map[string]interface{} `json:"info"`
	Code    string                 `json:"code"`
}

func (e *e2eEnv) post(t *testing.T, path string, body interface{}, token string) (int, e2eResponse) {
	t.Helper()
	payload, err := json.Marshal(body)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	e.router.ServeHTTP(w, req)
	var resp e2eResponse
	if w.Body.Len() > 0 {
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp), w.Body.String())
	}
	return w.Code, resp
}

// requestAdminCode starts an admin sign-in and returns the session ID with
// the emailed code.
func (e *e2eEnv) requestAdminCode(t *testing.T, email string) (string, string) {
	t.Helper()
	status, resp := e.post(t, "/api/v1/admin/auth", map[string]string{"email": email}, "")
	require.Equal(t, http.StatusOK, status, resp.Message)
	return resp.Info["uuid"].(string), e.sender.last(email)
}

// signInUser completes the user OTP sign-in and returns the access token.
func (e *e2eEnv) signInUser(t *testing.T, contact string) (int, e2eResponse) {
	t.Helper()
	status, resp := e.post(t, "/api/v1/public/auth", map[string]string{"username": contact}, "")
	require.Equal(t, http.StatusOK, status, resp.Message)
	return e.post(t, "/api/v1/public/auth/"+resp.Info["uuid"].(string), map[string]string{"otp": e.sender.last(contact)}, "")
}

func currentTOTP(t *testing.T, enrollment adminauth.Enrollment) string {
	t.Helper()
	secret, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(enrollment.Secret)
	require.NoError(t, err)
	return totp.Code(secret, totp.Step(time.Now()))
}

func testCurrency(symbol string) map[string]interface{} {
	return map[string]interface{}{
		"status": "approved",
		"info": map[string]interface{}{
			"en_name": "Test " + symbol, "fa_name": "Test " + symbol, "symbol": symbol,
			"if_fiat": false, "is_token": true, "contract": "contract-" + symbol,
		},
	}
}

// TestAdminSignIn_EndToEnd creates an administrator as an operator would,
// signs in with the emailed code and the authenticator code, and calls the
// admin API with the token.
func TestAdminSignIn_EndToEnd(t *testing.T) {
	env := newE2EEnv(t)
	ctx := context.Background()
	const email = "admin@example.com"
	enrollment, err := env.accounts.Create(ctx, email)
	require.NoError(t, err)

	sessionID, code := env.requestAdminCode(t, email)
	require.NotEmpty(t, code, "the sign-in code must be emailed to the administrator")
	adminTOTP := currentTOTP(t, enrollment)
	status, resp := env.post(t, "/api/v1/admin/auth/"+sessionID, map[string]string{"otp": code, "totp": adminTOTP}, "")
	require.Equal(t, http.StatusOK, status, resp.Message)
	adminToken := resp.Info["access_token"].(string)
	require.NotEmpty(t, adminToken)
	claims, err := auth.ParseToken(adminToken, auth.RoleAdmin, auth.TokenTypeAccess)
	require.NoError(t, err)
	assert.Equal(t, enrollment.AdminID, claims.UUID)
	var verified bool
	require.NoError(t, env.db.QueryRow(
		`SELECT verified_at IS NOT NULL FROM user_contacts WHERE user_hk = $1 AND type = 'email'`, enrollment.AdminID,
	).Scan(&verified))
	assert.True(t, verified, "the emailed code verifies the address that the operator entered")

	status, resp = env.post(t, "/api/v1/admin/currencies", testCurrency("E2E"), adminToken)
	assert.Equal(t, http.StatusCreated, status, resp.Message)

	t.Run("the admin list finds a currency, and a delete can be undone", func(t *testing.T) {
		hk, _ := resp.Info["hk"].(string)
		require.NotEmpty(t, hk)
		status, body := env.call(t, http.MethodGet, "/api/v1/admin/currencies?status=approved&symbol=e2e", nil, adminToken)
		require.Equal(t, http.StatusOK, status, string(body))
		var list struct {
			Info struct {
				Items []struct {
					HK      string `json:"hk"`
					Version int64  `json:"version"`
				} `json:"items"`
			} `json:"info"`
		}
		require.NoError(t, json.Unmarshal(body, &list))
		require.Len(t, list.Info.Items, 1)
		assert.Equal(t, hk, list.Info.Items[0].HK)
		assert.Equal(t, int64(1), list.Info.Items[0].Version)

		status, body = env.call(t, http.MethodDelete, "/api/v1/admin/currencies/"+hk, nil, adminToken)
		require.Equal(t, http.StatusOK, status, string(body))
		status, body = env.call(t, http.MethodPost, "/api/v1/admin/currencies/"+hk+"/restore", nil, adminToken)
		require.Equal(t, http.StatusOK, status, string(body))

		status, _ = env.call(t, http.MethodGet, "/api/v1/admin/currencies", nil, "")
		assert.Equal(t, http.StatusUnauthorized, status, "anonymous clients cannot list")
	})

	t.Run("an authenticator code works once", func(t *testing.T) {
		sessionID, code := env.requestAdminCode(t, email)
		status, resp := env.post(t, "/api/v1/admin/auth/"+sessionID, map[string]string{"otp": code, "totp": adminTOTP}, "")
		assert.Equal(t, http.StatusUnauthorized, status)
		assert.Equal(t, "Invalid or expired code", resp.Message)
	})

	t.Run("a wrong authenticator code ends the sign-in", func(t *testing.T) {
		sessionID, code := env.requestAdminCode(t, email)
		status, _ := env.post(t, "/api/v1/admin/auth/"+sessionID, map[string]string{"otp": code, "totp": "000000"}, "")
		assert.Equal(t, http.StatusUnauthorized, status)
		status, _ = env.post(t, "/api/v1/admin/auth/"+sessionID, map[string]string{"otp": code, "totp": currentTOTP(t, enrollment)}, "")
		assert.Equal(t, http.StatusUnauthorized, status, "the emailed code was used up by the failed attempt")
	})

	t.Run("unknown addresses get the same answer and no email", func(t *testing.T) {
		status, resp := env.post(t, "/api/v1/admin/auth", map[string]string{"email": "nobody@example.com"}, "")
		assert.Equal(t, http.StatusOK, status)
		assert.NotEmpty(t, resp.Info["uuid"])
		assert.Zero(t, env.sender.count("nobody@example.com"))
	})

	t.Run("user sign-in sessions are unknown to admin sign-in", func(t *testing.T) {
		const contact = "someone@example.com"
		status, resp := env.post(t, "/api/v1/public/auth", map[string]string{"username": contact}, "")
		require.Equal(t, http.StatusOK, status, resp.Message)
		userSession := resp.Info["uuid"].(string)
		code := env.sender.last(contact)

		status, _ = env.post(t, "/api/v1/admin/auth/"+userSession, map[string]string{"otp": code, "totp": "000000"}, "")
		assert.Equal(t, http.StatusUnauthorized, status)

		// Admin sign-in did not find, and so did not use up, the user session.
		status, resp = env.post(t, "/api/v1/public/auth/"+userSession, map[string]string{"otp": code}, "")
		assert.Equal(t, http.StatusOK, status, resp.Message)
	})

	t.Run("administrators cannot use user sign-in", func(t *testing.T) {
		status, resp := env.signInUser(t, email)
		assert.Equal(t, http.StatusForbidden, status)
		assert.Equal(t, "Administrators must use the admin sign-in.", resp.Message)
	})

	t.Run("user tokens cannot call the admin API", func(t *testing.T) {
		status, resp := env.signInUser(t, "trader@example.com")
		require.Equal(t, http.StatusOK, status, resp.Message)
		userToken := resp.Info["access_token"].(string)

		status, _ = env.post(t, "/api/v1/admin/currencies", testCurrency("USR"), userToken)
		assert.Equal(t, http.StatusUnauthorized, status)
		status, _ = env.call(t, http.MethodGet, "/api/v1/admin/currencies", nil, userToken)
		assert.Equal(t, http.StatusUnauthorized, status)
	})

	t.Run("logout ends the admin token", func(t *testing.T) {
		status, resp := env.post(t, "/api/v1/admin/logout", nil, adminToken)
		require.Equal(t, http.StatusOK, status, resp.Message)

		status, _ = env.post(t, "/api/v1/admin/currencies", testCurrency("OUT"), adminToken)
		assert.Equal(t, http.StatusUnauthorized, status)
	})

	t.Run("disabled administrators cannot sign in", func(t *testing.T) {
		require.NoError(t, env.accounts.Disable(ctx, email))
		before := env.sender.count(email)

		status, _ := env.post(t, "/api/v1/admin/auth", map[string]string{"email": email}, "")
		assert.Equal(t, http.StatusOK, status)
		assert.Equal(t, before, env.sender.count(email), "no code is sent to a disabled administrator")
	})
}

// signInAdmin completes an admin sign-in and returns the access token.
func (e *e2eEnv) signInAdmin(t *testing.T, enrollment adminauth.Enrollment) string {
	t.Helper()
	sessionID, code := e.requestAdminCode(t, enrollment.Email)
	status, resp := e.post(t, "/api/v1/admin/auth/"+sessionID, map[string]string{"otp": code, "totp": currentTOTP(t, enrollment)}, "")
	require.Equal(t, http.StatusOK, status, resp.Message)
	return resp.Info["access_token"].(string)
}

// TestAdminRole_DatabaseChanges covers changes made directly in the
// database, outside the CLI: they can take admin rights away at once, but
// cannot grant them without admin.totp_key.
func TestAdminRole_DatabaseChanges(t *testing.T) {
	env := newE2EEnv(t)
	ctx := context.Background()
	admin, err := env.accounts.Create(ctx, "ops@example.com")
	require.NoError(t, err)

	t.Run("demotion revokes issued admin tokens at once", func(t *testing.T) {
		token := env.signInAdmin(t, admin)
		status, resp := env.post(t, "/api/v1/admin/currencies", testCurrency("DB1"), token)
		require.Equal(t, http.StatusCreated, status, resp.Message)

		_, err := env.db.Exec(`UPDATE users SET role = 'user' WHERE hk = $1`, admin.AdminID)
		require.NoError(t, err)
		status, resp = env.post(t, "/api/v1/admin/currencies", testCurrency("DB2"), token)
		assert.Equal(t, http.StatusUnauthorized, status, "a valid signature is not enough once the stored role changed")
		assert.Equal(t, "Unauthorized - Administrator is not active", resp.Message)

		_, err = env.db.Exec(`UPDATE users SET role = 'admin' WHERE hk = $1`, admin.AdminID)
		require.NoError(t, err)
		status, _ = env.post(t, "/api/v1/admin/currencies", testCurrency("DB3"), token)
		assert.Equal(t, http.StatusCreated, status)

		require.NoError(t, env.accounts.Disable(ctx, admin.Email))
		status, _ = env.post(t, "/api/v1/admin/currencies", testCurrency("DB4"), token)
		assert.Equal(t, http.StatusUnauthorized, status, "disabling also revokes issued tokens")
	})

	t.Run("promotion in the database does not grant admin sign-in", func(t *testing.T) {
		status, resp := env.signInUser(t, "intruder@example.com")
		require.Equal(t, http.StatusOK, status, resp.Message)
		var intruderID string
		require.NoError(t, env.db.QueryRow(
			`SELECT user_hk FROM user_contacts WHERE type = 'email' AND value = 'intruder@example.com'`).Scan(&intruderID))

		// The role alone: no enrolled authenticator, so no code is sent.
		_, err := env.db.Exec(`UPDATE users SET role = 'admin' WHERE hk = $1`, intruderID)
		require.NoError(t, err)
		before := env.sender.count("intruder@example.com")
		status, _ = env.post(t, "/api/v1/admin/auth", map[string]string{"email": "intruder@example.com"}, "")
		assert.Equal(t, http.StatusOK, status)
		assert.Equal(t, before, env.sender.count("intruder@example.com"))

		// Copying a real administrator's sealed secret does not help either:
		// it is bound to that administrator's ID.
		other, err := env.accounts.Create(ctx, "victim@example.com")
		require.NoError(t, err)
		_, err = env.db.Exec(`UPDATE users SET status = 'ACTIVE', totp_enabled = TRUE,
			totp_secret = (SELECT totp_secret FROM users WHERE hk = $1) WHERE hk = $2`, other.AdminID, intruderID)
		require.NoError(t, err)
		sessionID, code := env.requestAdminCode(t, "intruder@example.com")
		status, resp = env.post(t, "/api/v1/admin/auth/"+sessionID, map[string]string{"otp": code, "totp": currentTOTP(t, other)}, "")
		assert.NotEqual(t, http.StatusOK, status, "no admin token without a secret sealed with admin.totp_key")
		assert.Empty(t, resp.Info["access_token"])
	})
}

func TestAdminSignIn_UnavailableWithoutAdminKeys(t *testing.T) {
	env := newE2EEnv(t)
	// Without admin keys, sign-in cannot issue tokens, so it does not start.
	require.NoError(t, auth.SetKeys(map[string]auth.KeyPair{auth.RoleUser: pemKeyPair(t)}))

	status, resp := env.post(t, "/api/v1/admin/auth", map[string]string{"email": "admin@example.com"}, "")
	assert.Equal(t, http.StatusServiceUnavailable, status)
	assert.Equal(t, "Admin sign-in is not available", resp.Message)

	status, _ = env.post(t, "/api/v1/admin/auth/"+uuid.NewString(), map[string]string{"otp": "123456", "totp": "123456"}, "")
	assert.Equal(t, http.StatusServiceUnavailable, status)
}
