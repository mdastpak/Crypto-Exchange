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

	"encrypted-db/config"
	"encrypted-db/internal/auth"
	"encrypted-db/internal/db"
	"encrypted-db/internal/models"

	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis/v8"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The tests in this file run the OTP sign-in flow against a database built by
// the real migrations. The suite in auth_integration_test.go uses the
// hand-written schema from db.NewTestPostgresService, which lacks the
// production constraints on users and therefore cannot catch schema defects.

const realSchemaTestDBName = "testdb_public_real_schema"

// setupRealSchemaHandler migrates a dedicated database with the repository
// migrations and returns a handler wired to it and to Redis database 1.
func setupRealSchemaHandler(t *testing.T) (*PublicHandler, *sql.DB, *redis.Client) {
	t.Helper()
	gin.SetMode(gin.TestMode)

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
	config.Config.OTP.Auth.Length = 6
	config.Config.OTP.Auth.TTL = 300
	config.Config.OTP.Auth.RetryLimit = 5
	config.Config.JWT.AccessToken.Expiration = 15
	config.Config.JWT.RefreshToken.Expiration = 60
	config.Config.JWT.Issuer = "test"

	migrationsDir, err := filepath.Abs(filepath.Join("..", "..", "db", "migrations"))
	require.NoError(t, err)
	config.SetMigrationsPath("file://" + strings.ReplaceAll(migrationsDir, "\\", "/"))

	initTestSigningKeys(t)
	ensureRealSchemaTestDB(t)
	resetRealSchema(t)
	t.Cleanup(func() { resetRealSchema(t) })

	postgresService, err := db.NewPostgresService()
	require.NoError(t, err, "the repository migrations must apply to an empty database")
	t.Cleanup(func() { postgresService.Close() })

	redisClient := redis.NewClient(&redis.Options{Addr: "localhost:6379", DB: 1})
	require.NoError(t, redisClient.Ping(context.Background()).Err())
	t.Cleanup(func() {
		redisClient.FlushDB(context.Background())
		redisClient.Close()
	})

	handler := NewHandler(&models.InfraServices{
		Postgres: postgresService,
		Redis:    &db.RedisService{Client: redisClient},
	})
	return handler, postgresService.DB, redisClient
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
		"SELECT COUNT(*) FROM users WHERE info->'contact'->>$1 = $2", contactType, contact,
	).Scan(&count))
	return count
}

func TestVerifyOTP_RealSchema_FirstSignInCreatesUser(t *testing.T) {
	handler, conn, redisClient := setupRealSchemaHandler(t)
	router := gin.New()
	router.POST("/public/auth", handler.RequestOTP)
	router.POST("/public/auth/:uuid", handler.VerifyOTP)

	const contact = "first.signin@example.com"
	status, resp := postJSON(t, router, "/public/auth", map[string]string{"username": contact})
	require.Equal(t, http.StatusOK, status, resp.Message)
	sessionID := resp.Info.(map[string]interface{})["uuid"].(string)

	keys, err := redisClient.Keys(context.Background(), otpKeyPrefix+sessionID+":*").Result()
	require.NoError(t, err)
	require.Len(t, keys, 1)
	code := strings.TrimPrefix(keys[0], otpKeyPrefix+sessionID+":")

	status, resp = postJSON(t, router, "/public/auth/"+sessionID, map[string]string{"otp": code})
	require.Equal(t, http.StatusOK, status, resp.Message)
	tokens := resp.Info.(map[string]interface{})
	assert.NotEmpty(t, tokens["access_token"])
	assert.NotEmpty(t, tokens["refresh_token"])

	assert.Equal(t, 1, countUsersWithContact(t, conn, "email", contact))
}

func TestCheckOrCreateUser_RealSchema_ReusesExistingUser(t *testing.T) {
	handler, conn, _ := setupRealSchemaHandler(t)
	ctx := context.Background()

	first, err := handler.checkOrCreateUser(ctx, "mobile", "09121234567")
	require.NoError(t, err)
	require.NotEqual(t, uuid.Nil, first)

	second, err := handler.checkOrCreateUser(ctx, "mobile", "09121234567")
	require.NoError(t, err)
	assert.Equal(t, first, second)
	assert.Equal(t, 1, countUsersWithContact(t, conn, "mobile", "09121234567"))
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
			results[i], errs[i] = handler.checkOrCreateUser(ctx, "email", contact)
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

func TestUsersSchema_RealSchema_EnforcesContactRules(t *testing.T) {
	_, conn, _ := setupRealSchemaHandler(t)

	insert := func(info string) error {
		_, err := conn.Exec("INSERT INTO users (info, status) VALUES ($1::jsonb, 'approved')", info)
		return err
	}
	pgCode := func(err error) string {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			return pgErr.Code
		}
		return ""
	}

	for _, contactType := range []string{"email", "mobile", "username"} {
		t.Run("unique "+contactType, func(t *testing.T) {
			info := fmt.Sprintf(`{"contact": {%q: "duplicate-%s"}}`, contactType, contactType)
			require.NoError(t, insert(info))
			assert.Equal(t, "23505", pgCode(insert(info)), "a second user with the same %s must be rejected", contactType)
		})
	}

	t.Run("contact is required", func(t *testing.T) {
		assert.Equal(t, "23514", pgCode(insert(`{}`)))
		assert.Equal(t, "23514", pgCode(insert(`{"contact": "not-an-object"}`)))
	})
}
