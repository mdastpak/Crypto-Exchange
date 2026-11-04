package admin

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"encrypted-db/internal/db"
	"encrypted-db/internal/models"
	"encrypted-db/internal/rabbitmq"

	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis/v8"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
)

const (
	testPGHost     = "localhost"
	testPGPort     = "5432"
	testPGUser     = "postgres"
	testPGPassword = "test"
	testPGDBName   = "testdb"
	testPGSSLMode  = "disable"
)

// AdminCurrenciesTestSuite exercises the currency admin handlers against a
// real Postgres and Redis instance (the same shared test infrastructure used
// by the other packages in this repository). RabbitMQ is not available in
// this environment, so a zero-value *rabbitmq.RabbitMQService is used: its
// Publish call fails fast and is only ever logged by the handler, never
// surfaced to the HTTP response.
type AdminCurrenciesTestSuite struct {
	suite.Suite
	router      *gin.Engine
	handler     *AdminHandler
	postgresSvc *db.PostgresService
	redisClient *redis.Client
}

func (s *AdminCurrenciesTestSuite) SetupSuite() {
	gin.SetMode(gin.TestMode)

	svc, err := db.NewTestPostgresService(testPGHost, testPGPort, testPGUser, testPGPassword, testPGDBName, testPGSSLMode)
	require.NoError(s.T(), err)
	s.postgresSvc = svc

	_, err = s.postgresSvc.DB.Exec(`
		DROP TABLE IF EXISTS currencies;
		DROP TYPE IF EXISTS STATUS_ENUM;
		CREATE TYPE STATUS_ENUM AS ENUM ('approved', 'deleted', 'pending', 'suspend');
		CREATE TABLE currencies (
			id SERIAL PRIMARY KEY,
			hk UUID UNIQUE NOT NULL DEFAULT gen_random_uuid(),
			status STATUS_ENUM DEFAULT 'pending',
			info JSONB NOT NULL,
			created_at TIMESTAMP DEFAULT NOW(),
			updated_at TIMESTAMP DEFAULT NOW(),
			deleted_at TIMESTAMP NULL
		);
		ALTER TABLE currencies
		ADD CONSTRAINT info_structure CHECK (
			info ? 'fa_name' AND
			info ? 'en_name' AND
			info ? 'symbol' AND
			info ? 'if_fiat' AND
			info ? 'is_token' AND
			info ? 'contract'
		);
		CREATE UNIQUE INDEX currencies_info_symbol_unique ON currencies ((info->>'symbol'));
		-- As migration 13: only non-empty contracts are unique.
		CREATE UNIQUE INDEX currencies_info_contract_unique ON currencies ((info->>'contract'))
			WHERE info->>'contract' <> '';
	`)
	require.NoError(s.T(), err)
}

func (s *AdminCurrenciesTestSuite) SetupTest() {
	s.redisClient = redis.NewClient(&redis.Options{
		Addr: "localhost:6379",
		DB:   1,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(s.T(), s.redisClient.Ping(ctx).Err())

	_, err := s.postgresSvc.DB.Exec("TRUNCATE TABLE currencies RESTART IDENTITY")
	require.NoError(s.T(), err)

	services := &models.InfraServices{
		Postgres: s.postgresSvc,
		Redis:    &db.RedisService{Client: s.redisClient},
		RabbitMQ: &rabbitmq.RabbitMQService{},
	}
	s.handler = NewHandler(services)

	s.router = gin.New()
	s.router.POST("/currencies", s.handler.CreateCurrency)
	s.router.PUT("/currencies/:hk", s.handler.UpdateCurrency)
	s.router.DELETE("/currencies/:hk", s.handler.DeleteCurrency)
}

func (s *AdminCurrenciesTestSuite) TearDownTest() {
	s.redisClient.FlushDB(context.Background())
	s.redisClient.Close()
}

func (s *AdminCurrenciesTestSuite) TearDownSuite() {
	if s.postgresSvc != nil {
		s.postgresSvc.DB.Exec("DROP TABLE IF EXISTS currencies")
		s.postgresSvc.DB.Exec("DROP TYPE IF EXISTS STATUS_ENUM")
		s.postgresSvc.Close()
	}
}

func TestAdminCurrenciesSuite(t *testing.T) {
	suite.Run(t, new(AdminCurrenciesTestSuite))
}

func (s *AdminCurrenciesTestSuite) makeRequest(method, path string, body interface{}) *httptest.ResponseRecorder {
	var reqBody []byte
	if body != nil {
		reqBody, _ = json.Marshal(body)
	}

	req := httptest.NewRequest(method, path, bytes.NewBuffer(reqBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.router.ServeHTTP(w, req)
	return w
}

func sampleCurrencyPayload(symbol, contract string) map[string]interface{} {
	return map[string]interface{}{
		"status": "pending",
		"info": map[string]interface{}{
			"en_name":  "Bitcoin",
			"fa_name":  "Bitcoin (Persian name)",
			"symbol":   symbol,
			"if_fiat":  false,
			"is_token": false,
			"contract": contract,
		},
	}
}

func (s *AdminCurrenciesTestSuite) createCurrency(symbol, contract string) string {
	w := s.makeRequest(http.MethodPost, "/currencies", sampleCurrencyPayload(symbol, contract))
	require.Equal(s.T(), http.StatusCreated, w.Code)

	var hk string
	row := s.postgresSvc.DB.QueryRow("SELECT hk FROM currencies WHERE info->>'symbol' = $1", symbol)
	require.NoError(s.T(), row.Scan(&hk))
	return hk
}

func (s *AdminCurrenciesTestSuite) TestCreateCurrency_Success() {
	w := s.makeRequest(http.MethodPost, "/currencies", sampleCurrencyPayload("BTC", "native-btc"))

	assert.Equal(s.T(), http.StatusCreated, w.Code)

	var resp models.APIResponse
	require.NoError(s.T(), json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(s.T(), "Currency created successfully.", resp.Message)

	var count int
	require.NoError(s.T(), s.postgresSvc.DB.QueryRow("SELECT COUNT(*) FROM currencies WHERE info->>'symbol' = 'BTC'").Scan(&count))
	assert.Equal(s.T(), 1, count)

	// The handler caches the currency in Redis under its generated hk.
	var hk string
	require.NoError(s.T(), s.postgresSvc.DB.QueryRow("SELECT hk FROM currencies WHERE info->>'symbol' = 'BTC'").Scan(&hk))
	val, err := s.redisClient.Get(context.Background(), fmt.Sprintf("base_definitions:currency:%s", hk)).Result()
	require.NoError(s.T(), err)
	assert.Contains(s.T(), val, "BTC")
}

func (s *AdminCurrenciesTestSuite) TestCreateCurrency_InvalidJSON() {
	req := httptest.NewRequest(http.MethodPost, "/currencies", bytes.NewBufferString("{not-json"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.router.ServeHTTP(w, req)

	assert.Equal(s.T(), http.StatusBadRequest, w.Code)
}

// decodeResponse returns the envelope of a response.
func (s *AdminCurrenciesTestSuite) decodeResponse(w *httptest.ResponseRecorder) models.APIResponse {
	var resp models.APIResponse
	require.NoError(s.T(), json.Unmarshal(w.Body.Bytes(), &resp), w.Body.String())
	return resp
}

// assertConflict checks a 409 answer with its stable code and field.
func (s *AdminCurrenciesTestSuite) assertConflict(w *httptest.ResponseRecorder, code, field string) {
	s.T().Helper()
	assert.Equal(s.T(), http.StatusConflict, w.Code, w.Body.String())
	info, ok := s.decodeResponse(w).Info.(map[string]interface{})
	require.True(s.T(), ok, w.Body.String())
	assert.Equal(s.T(), code, info["code"])
	assert.Equal(s.T(), field, info["field"])
}

// assertFieldErrors checks a 400 answer that names exactly these fields.
func (s *AdminCurrenciesTestSuite) assertFieldErrors(w *httptest.ResponseRecorder, fields ...string) {
	s.T().Helper()
	assert.Equal(s.T(), http.StatusBadRequest, w.Code, w.Body.String())
	resp := s.decodeResponse(w)
	assert.Equal(s.T(), "Validation failed", resp.Message)
	info, ok := resp.Info.(map[string]interface{})
	require.True(s.T(), ok, w.Body.String())
	errs, ok := info["errors"].(map[string]interface{})
	require.True(s.T(), ok, w.Body.String())
	got := make([]string, 0, len(errs))
	for field := range errs {
		got = append(got, field)
	}
	assert.ElementsMatch(s.T(), fields, got, w.Body.String())
}

func (s *AdminCurrenciesTestSuite) TestCreateCurrency_DuplicateSymbolRejected() {
	w := s.makeRequest(http.MethodPost, "/currencies", sampleCurrencyPayload("ETH", "native-eth"))
	require.Equal(s.T(), http.StatusCreated, w.Code)

	w = s.makeRequest(http.MethodPost, "/currencies", sampleCurrencyPayload("eth", "another-contract"))
	s.assertConflict(w, "duplicate_symbol", "info.symbol")

	var count int
	require.NoError(s.T(), s.postgresSvc.DB.QueryRow("SELECT COUNT(*) FROM currencies WHERE info->>'symbol' = 'ETH'").Scan(&count))
	assert.Equal(s.T(), 1, count, "the duplicate insert must not have created a second row")
}

func (s *AdminCurrenciesTestSuite) TestUpdateCurrency_PartialUpdateMergesFields() {
	hk := s.createCurrency("USDT", "native-usdt")

	w := s.makeRequest(http.MethodPut, "/currencies/"+hk, map[string]interface{}{
		"status": "approved",
		"info": map[string]interface{}{
			"fa_name": "Tether (new Persian name)",
		},
	})

	assert.Equal(s.T(), http.StatusOK, w.Code)

	var status string
	var infoData []byte
	require.NoError(s.T(), s.postgresSvc.DB.QueryRow("SELECT status, info FROM currencies WHERE hk = $1", hk).Scan(&status, &infoData))
	assert.Equal(s.T(), "approved", status)

	var info models.CurrencyInfo
	require.NoError(s.T(), json.Unmarshal(infoData, &info))
	assert.Equal(s.T(), "Tether (new Persian name)", info.FaName)
	assert.Equal(s.T(), "USDT", info.Symbol, "symbol should be preserved when not part of the update payload")
	assert.Equal(s.T(), "Bitcoin", info.EnName, "en_name should be preserved when not part of the update payload")
}

func (s *AdminCurrenciesTestSuite) TestUpdateCurrency_InvalidUUID() {
	w := s.makeRequest(http.MethodPut, "/currencies/not-a-uuid", sampleCurrencyPayload("XRP", "native-xrp"))
	assert.Equal(s.T(), http.StatusBadRequest, w.Code)
}

func (s *AdminCurrenciesTestSuite) TestUpdateCurrency_NotFound() {
	w := s.makeRequest(http.MethodPut, "/currencies/"+uuid.New().String(), sampleCurrencyPayload("XRP", "native-xrp"))
	assert.Equal(s.T(), http.StatusNotFound, w.Code)
}

func (s *AdminCurrenciesTestSuite) TestUpdateCurrency_DuplicateSymbolConflict() {
	s.createCurrency("BNB", "native-bnb")
	otherHK := s.createCurrency("SOL", "native-sol")

	w := s.makeRequest(http.MethodPut, "/currencies/"+otherHK, map[string]interface{}{
		"info": map[string]interface{}{
			"symbol": "BNB",
		},
	})

	s.assertConflict(w, "duplicate_symbol", "info.symbol")
}

func (s *AdminCurrenciesTestSuite) TestDeleteCurrency_Success() {
	hk := s.createCurrency("ADA", "native-ada")

	w := s.makeRequest(http.MethodDelete, "/currencies/"+hk, nil)
	assert.Equal(s.T(), http.StatusOK, w.Code)

	var status string
	var deletedAt sql.NullTime
	require.NoError(s.T(), s.postgresSvc.DB.QueryRow("SELECT status, deleted_at FROM currencies WHERE hk = $1", hk).Scan(&status, &deletedAt))
	assert.Equal(s.T(), "deleted", status)
	assert.True(s.T(), deletedAt.Valid)

	_, err := s.redisClient.Get(context.Background(), fmt.Sprintf("base_definitions:currency:%s", hk)).Result()
	assert.ErrorIs(s.T(), err, redis.Nil, "cache entry should be removed after deletion")
}

func (s *AdminCurrenciesTestSuite) TestDeleteCurrency_InvalidUUID() {
	w := s.makeRequest(http.MethodDelete, "/currencies/not-a-uuid", nil)
	assert.Equal(s.T(), http.StatusBadRequest, w.Code)
}

func (s *AdminCurrenciesTestSuite) TestDeleteCurrency_NotFound() {
	w := s.makeRequest(http.MethodDelete, "/currencies/"+uuid.New().String(), nil)
	assert.Equal(s.T(), http.StatusNotFound, w.Code)
}

func (s *AdminCurrenciesTestSuite) TestDeleteCurrency_AlreadyDeleted() {
	hk := s.createCurrency("DOT", "native-dot")

	w := s.makeRequest(http.MethodDelete, "/currencies/"+hk, nil)
	require.Equal(s.T(), http.StatusOK, w.Code)

	w = s.makeRequest(http.MethodDelete, "/currencies/"+hk, nil)
	assert.Equal(s.T(), http.StatusNotFound, w.Code)

	var resp models.APIResponse
	require.NoError(s.T(), json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(s.T(), "Currency not found or already deleted", resp.Message)
}

func (s *AdminCurrenciesTestSuite) TestAddAndDeleteCurrencyCache() {
	currency := models.Currency{
		HK:     uuid.New().String(),
		Status: "approved",
		Info:   models.CurrencyInfo{EnName: "Litecoin", Symbol: "LTC"},
	}

	require.NoError(s.T(), s.handler.AddOrUpdateCurrencyInCache(context.Background(), currency))

	val, err := s.redisClient.Get(context.Background(), fmt.Sprintf("base_definitions:currency:%s", currency.HK)).Result()
	require.NoError(s.T(), err)
	assert.Contains(s.T(), val, "LTC")

	require.NoError(s.T(), s.handler.DeleteCurrencyFromCache(context.Background(), currency.HK))

	_, err = s.redisClient.Get(context.Background(), fmt.Sprintf("base_definitions:currency:%s", currency.HK)).Result()
	assert.ErrorIs(s.T(), err, redis.Nil)
}

func (s *AdminCurrenciesTestSuite) TestCleanupBaseDefinitions_RemovesAllCurrencyKeys() {
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		key := fmt.Sprintf("base_definitions:currency:%d", i)
		require.NoError(s.T(), s.redisClient.Set(ctx, key, "{}", 0).Err())
	}
	require.NoError(s.T(), s.redisClient.Set(ctx, "unrelated:key", "keep-me", 0).Err())

	require.NoError(s.T(), s.handler.CleanupBaseDefinitions(ctx))

	keys, err := s.redisClient.Keys(ctx, "base_definitions:currency:*").Result()
	require.NoError(s.T(), err)
	assert.Empty(s.T(), keys)

	stillThere, err := s.redisClient.Get(ctx, "unrelated:key").Result()
	require.NoError(s.T(), err)
	assert.Equal(s.T(), "keep-me", stillThere)
}

func (s *AdminCurrenciesTestSuite) TestLoadAndCacheCurrencies_OnlyCachesApprovedNonDeleted() {
	ctx := context.Background()

	insert := func(status, symbol string) {
		info, _ := json.Marshal(models.CurrencyInfo{
			EnName: symbol, FaName: symbol, Symbol: symbol, Contract: "contract-" + symbol,
		})
		_, err := s.postgresSvc.DB.Exec("INSERT INTO currencies (status, info) VALUES ($1, $2::jsonb)", status, info)
		require.NoError(s.T(), err)
	}

	insert("approved", "AAA")
	insert("pending", "BBB")
	insert("suspend", "CCC")

	require.NoError(s.T(), s.handler.LoadAndCacheCurrencies(ctx))

	keys, err := s.redisClient.Keys(ctx, "base_definitions:currency:*").Result()
	require.NoError(s.T(), err)
	require.Len(s.T(), keys, 1)

	val, err := s.redisClient.Get(ctx, keys[0]).Result()
	require.NoError(s.T(), err)
	assert.Contains(s.T(), val, "AAA")
}

func (s *AdminCurrenciesTestSuite) TestCreateCurrency_DuplicateContractRejected() {
	payload := sampleCurrencyPayload("USDT", "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t")
	payload["info"].(map[string]interface{})["is_token"] = true
	require.Equal(s.T(), http.StatusCreated, s.makeRequest(http.MethodPost, "/currencies", payload).Code)

	other := sampleCurrencyPayload("USDC", "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t")
	other["info"].(map[string]interface{})["is_token"] = true
	s.assertConflict(s.makeRequest(http.MethodPost, "/currencies", other), "duplicate_contract", "info.contract")
}

func (s *AdminCurrenciesTestSuite) TestCreateCurrency_CurrenciesWithoutContract() {
	for _, symbol := range []string{"BTC", "ETH", "USD"} {
		w := s.makeRequest(http.MethodPost, "/currencies", sampleCurrencyPayload(symbol, ""))
		assert.Equal(s.T(), http.StatusCreated, w.Code, "%s: several currencies may have no contract", symbol)
	}
}

func (s *AdminCurrenciesTestSuite) TestCreateCurrency_DefaultsToPending() {
	payload := sampleCurrencyPayload("ADA", "")
	delete(payload, "status")

	w := s.makeRequest(http.MethodPost, "/currencies", payload)

	require.Equal(s.T(), http.StatusCreated, w.Code, w.Body.String())
	info := s.decodeResponse(w).Info.(map[string]interface{})
	assert.Equal(s.T(), "pending", info["status"])
	var status string
	require.NoError(s.T(), s.postgresSvc.DB.QueryRow("SELECT status FROM currencies WHERE hk = $1", info["hk"]).Scan(&status))
	assert.Equal(s.T(), "pending", status)
}

func (s *AdminCurrenciesTestSuite) TestCreateCurrency_NormalizesTheSymbol() {
	w := s.makeRequest(http.MethodPost, "/currencies", sampleCurrencyPayload("  dot ", ""))

	require.Equal(s.T(), http.StatusCreated, w.Code, w.Body.String())
	var count int
	require.NoError(s.T(), s.postgresSvc.DB.QueryRow("SELECT COUNT(*) FROM currencies WHERE info->>'symbol' = 'DOT'").Scan(&count))
	assert.Equal(s.T(), 1, count)
}

func (s *AdminCurrenciesTestSuite) TestCreateCurrency_ValidationErrors() {
	cases := map[string]struct {
		mutate func(map[string]interface{})
		fields []string
	}{
		"unknown status": {func(p map[string]interface{}) { p["status"] = "live" }, []string{"status"}},
		"empty status":   {func(p map[string]interface{}) { p["status"] = "" }, []string{"status"}},
		"deleted status": {func(p map[string]interface{}) { p["status"] = "deleted" }, []string{"status"}},
		"missing names": {func(p map[string]interface{}) {
			info := p["info"].(map[string]interface{})
			delete(info, "en_name")
			info["fa_name"] = "  "
		}, []string{"info.en_name", "info.fa_name"}},
		"long name": {func(p map[string]interface{}) {
			p["info"].(map[string]interface{})["en_name"] = strings.Repeat("x", 101)
		}, []string{"info.en_name"}},
		"invalid symbol": {func(p map[string]interface{}) {
			p["info"].(map[string]interface{})["symbol"] = "U$D"
		}, []string{"info.symbol"}},
		"token without contract": {func(p map[string]interface{}) {
			info := p["info"].(map[string]interface{})
			info["is_token"] = true
			info["contract"] = ""
		}, []string{"info.contract"}},
		"invalid contract": {func(p map[string]interface{}) {
			p["info"].(map[string]interface{})["contract"] = "0x12 34"
		}, []string{"info.contract"}},
		"fiat token": {func(p map[string]interface{}) {
			info := p["info"].(map[string]interface{})
			info["if_fiat"] = true
			info["is_token"] = true
			info["contract"] = "0xabc"
		}, []string{"info.is_token"}},
		"server-controlled hk": {func(p map[string]interface{}) { p["hk"] = uuid.NewString() }, []string{"hk"}},
		"server-controlled id": {func(p map[string]interface{}) { p["id"] = 7 }, []string{"id"}},
		"timestamps":           {func(p map[string]interface{}) { p["created_at"] = "2026-01-01" }, []string{"created_at"}},
	}
	for name, tc := range cases {
		s.Run(name, func() {
			payload := sampleCurrencyPayload("XLM", "")
			tc.mutate(payload)

			s.assertFieldErrors(s.makeRequest(http.MethodPost, "/currencies", payload), tc.fields...)

			var count int
			require.NoError(s.T(), s.postgresSvc.DB.QueryRow("SELECT COUNT(*) FROM currencies").Scan(&count))
			assert.Zero(s.T(), count, "nothing may be stored")
		})
	}
}

func (s *AdminCurrenciesTestSuite) TestUpdateCurrency_CanResetFlagsToFalse() {
	payload := sampleCurrencyPayload("WBTC", "0x2260fac5e5542a773aa44fbcfedf7c193bc2c599")
	payload["info"].(map[string]interface{})["is_token"] = true
	require.Equal(s.T(), http.StatusCreated, s.makeRequest(http.MethodPost, "/currencies", payload).Code)
	var hk string
	require.NoError(s.T(), s.postgresSvc.DB.QueryRow("SELECT hk FROM currencies WHERE info->>'symbol' = 'WBTC'").Scan(&hk))

	w := s.makeRequest(http.MethodPut, "/currencies/"+hk, map[string]interface{}{
		"info": map[string]interface{}{"is_token": false, "contract": ""},
	})

	require.Equal(s.T(), http.StatusOK, w.Code, w.Body.String())
	var infoData []byte
	require.NoError(s.T(), s.postgresSvc.DB.QueryRow("SELECT info FROM currencies WHERE hk = $1", hk).Scan(&infoData))
	var info models.CurrencyInfo
	require.NoError(s.T(), json.Unmarshal(infoData, &info))
	assert.False(s.T(), info.IsToken, "an update can set a flag back to false")
	assert.Empty(s.T(), info.Contract)
	assert.Equal(s.T(), "WBTC", info.Symbol)
}

func (s *AdminCurrenciesTestSuite) TestUpdateCurrency_ValidationErrors() {
	hk := s.createCurrency("TRX", "")

	cases := map[string]struct {
		body   map[string]interface{}
		fields []string
	}{
		"unknown status":         {map[string]interface{}{"status": "live"}, []string{"status"}},
		"empty name":             {map[string]interface{}{"info": map[string]interface{}{"en_name": ""}}, []string{"info.en_name"}},
		"token without contract": {map[string]interface{}{"info": map[string]interface{}{"is_token": true}}, []string{"info.contract"}},
		"server-controlled hk":   {map[string]interface{}{"hk": uuid.NewString()}, []string{"hk"}},
	}
	for name, tc := range cases {
		s.Run(name, func() {
			s.assertFieldErrors(s.makeRequest(http.MethodPut, "/currencies/"+hk, tc.body), tc.fields...)
		})
	}

	var status string
	var infoData []byte
	require.NoError(s.T(), s.postgresSvc.DB.QueryRow("SELECT status, info FROM currencies WHERE hk = $1", hk).Scan(&status, &infoData))
	assert.Equal(s.T(), "pending", status, "a rejected update changes nothing")
	var info models.CurrencyInfo
	require.NoError(s.T(), json.Unmarshal(infoData, &info))
	assert.Equal(s.T(), "Bitcoin", info.EnName)
	assert.False(s.T(), info.IsToken)
}

func (s *AdminCurrenciesTestSuite) TestCurrencyRequests_RejectMalformedBodies() {
	hk := s.createCurrency("XMR", "")
	for _, body := range []string{`{"status": "pending"} {"status": "approved"}`, `["status"]`, `{"info": "BTC"}`} {
		for _, method := range []string{http.MethodPost, http.MethodPut} {
			path := "/currencies"
			if method == http.MethodPut {
				path += "/" + hk
			}
			req := httptest.NewRequest(method, path, strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			s.router.ServeHTTP(w, req)
			assert.Equal(s.T(), http.StatusBadRequest, w.Code, "%s %s", method, body)
		}
	}
}
