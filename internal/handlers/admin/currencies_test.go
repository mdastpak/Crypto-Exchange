package admin

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/auth"
	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/currencies"
	"github.com/mdastpak/crypto-exchange/internal/db"
	"github.com/mdastpak/crypto-exchange/internal/models"
	"github.com/mdastpak/crypto-exchange/internal/outbox"
	"github.com/mdastpak/crypto-exchange/internal/pagination"
	"github.com/mdastpak/crypto-exchange/internal/rabbitmq"
	"github.com/mdastpak/crypto-exchange/internal/testutil/pgtest"
	"github.com/mdastpak/crypto-exchange/internal/testutil/redistest"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
)

// currenciesTestDBName is the database of this suite, so that its resets do
// not race the tests of other packages.
const currenciesTestDBName = "testdb_admin_currencies"

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
	events      *recordingPublisher
	// relay publishes the events of the outbox to events; the tests flush
	// it instead of starting it.
	relay *outbox.Relay
	// adminID is the administrator of the requests, as the admin middleware
	// would set it.
	adminID string
	// cacheDown makes the relay's drop of the cached list fail, as while
	// Redis is unavailable.
	cacheDown atomic.Bool
}

// recordingPublisher records the currency events instead of publishing them,
// and fails while down is set, as while RabbitMQ is unavailable.
type recordingPublisher struct {
	mu     sync.Mutex
	events []models.CurrencyEvent
	down   atomic.Bool
}

func (p *recordingPublisher) Publish(ctx context.Context, exchange, message string) error {
	if p.down.Load() {
		return errors.New("RabbitMQ is unavailable")
	}
	var event models.CurrencyEvent
	if err := json.Unmarshal([]byte(message), &event); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, event)
	return nil
}

// takeEvents relays the outbox and returns the "action hk" pairs of the
// events published since the last call.
func (s *AdminCurrenciesTestSuite) takeEvents() []string {
	_, err := s.relay.Flush(context.Background())
	require.NoError(s.T(), err)
	return s.events.take()
}

// take returns the recorded "action hk" pairs and forgets them.
func (p *recordingPublisher) take() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	taken := make([]string, 0, len(p.events))
	for _, e := range p.events {
		taken = append(taken, e.Action+" "+e.HK)
	}
	p.events = nil
	return taken
}

func (s *AdminCurrenciesTestSuite) SetupSuite() {
	gin.SetMode(gin.TestMode)

	// The schema comes from the repository migrations, so the suite sees the
	// constraints and indexes of production.
	s.postgresSvc = &db.PostgresService{DB: pgtest.Migrated(s.T(), currenciesTestDBName)}
}

func (s *AdminCurrenciesTestSuite) SetupTest() {
	s.redisClient = redis.NewClient(&redis.Options{
		Addr: "localhost:6379",
		DB:   redistest.Admin,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(s.T(), s.redisClient.Ping(ctx).Err())

	// CASCADE empties the tables that reference the currencies too (#309):
	// markets, balances, custody, and the networks of the currencies. It
	// reaches the books of the ledger (#60), whose triggers refuse a
	// truncation to every role; the reset of a test turns the triggers off
	// for its own transaction, as only a superuser can.
	tx, err := s.postgresSvc.DB.Begin()
	require.NoError(s.T(), err)
	defer tx.Rollback()
	_, err = tx.Exec("SET LOCAL session_replication_role = replica")
	require.NoError(s.T(), err)
	_, err = tx.Exec("TRUNCATE TABLE currencies, outbox_events, user_restrictions, user_favorites, currency_capability_overrides, currency_delistings, currency_delisting_steps, currency_symbols, currency_migrations, currency_migration_sources, currency_migration_phases RESTART IDENTITY CASCADE")
	require.NoError(s.T(), err)
	require.NoError(s.T(), tx.Commit())

	services := &models.InfraServices{Config: &config.Config,
		Postgres: s.postgresSvc,
		Redis:    &db.RedisService{Client: s.redisClient},
		RabbitMQ: &rabbitmq.RabbitMQService{},
	}
	s.handler = NewHandler(services)
	s.events = &recordingPublisher{}
	catalog := s.handler.Currencies
	s.cacheDown.Store(false)
	s.relay = outbox.NewRelay(s.postgresSvc.DB, s.events, map[string]outbox.Route{
		models.CurrencyEventTopic: {Exchange: "price_updates", Before: func(ctx context.Context) error {
			if s.cacheDown.Load() {
				return errors.New("Redis is unavailable")
			}
			return catalog.Invalidate(ctx)
		}},
	}, outbox.Options{Interval: time.Hour, Logger: log.New(io.Discard, "", 0)})
	s.handler.Outbox = s.relay

	s.adminID = uuid.NewString()
	s.router = gin.New()
	s.router.Use(func(c *gin.Context) {
		auth.SetClaims(c, &auth.Claims{UUID: s.adminID, Role: auth.RoleAdmin})
	})
	s.router.POST("/currencies", s.handler.CreateCurrency)
	s.router.GET("/currencies", s.handler.ListCurrencies)
	s.router.GET("/currencies/:hk", s.handler.GetCurrency)
	s.router.PUT("/currencies/:hk", s.handler.UpdateCurrency)
	s.router.DELETE("/currencies/:hk", s.handler.DeleteCurrency)
	s.router.POST("/currencies/:hk/restore", s.handler.RestoreCurrency)
}

func (s *AdminCurrenciesTestSuite) TearDownTest() {
	s.redisClient.FlushDB(context.Background())
	s.redisClient.Close()
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
	payload := sampleCurrencyPayload("BTC", "native-btc")
	payload["status"] = "approved"
	w := s.makeRequest(http.MethodPost, "/currencies", payload)

	assert.Equal(s.T(), http.StatusCreated, w.Code)

	var resp models.APIResponse
	require.NoError(s.T(), json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(s.T(), "Currency created successfully.", resp.Message)

	var count int
	require.NoError(s.T(), s.postgresSvc.DB.QueryRow("SELECT COUNT(*) FROM currencies WHERE info->>'symbol' = 'BTC'").Scan(&count))
	assert.Equal(s.T(), 1, count)

	// An approved currency is public at once.
	var hk string
	require.NoError(s.T(), s.postgresSvc.DB.QueryRow("SELECT hk FROM currencies WHERE info->>'symbol' = 'BTC'").Scan(&hk))
	assert.True(s.T(), s.isPublic(hk))
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
	s.update(hk, map[string]interface{}{"status": "approved"})
	require.True(s.T(), s.isPublic(hk))

	w := s.makeRequest(http.MethodDelete, "/currencies/"+hk, nil)
	assert.Equal(s.T(), http.StatusOK, w.Code)

	var status string
	var deletedAt sql.NullTime
	require.NoError(s.T(), s.postgresSvc.DB.QueryRow("SELECT status, deleted_at FROM currencies WHERE hk = $1", hk).Scan(&status, &deletedAt))
	assert.Equal(s.T(), "deleted", status)
	assert.True(s.T(), deletedAt.Valid)

	assert.False(s.T(), s.isPublic(hk), "the delete drops the cached list")
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

// TestRestoreCurrency_UndoesADelete deletes a public currency by mistake and
// restores it (#196): it keeps its hk and symbol, comes back as pending, so
// not public, and the restore is audited.
func (s *AdminCurrenciesTestSuite) TestRestoreCurrency_UndoesADelete() {
	hk := s.createCurrency("USDT", "0xdac17f958d2ee523a2206206994597c13d831ec7")
	s.update(hk, map[string]interface{}{"status": "approved"})
	require.Equal(s.T(), http.StatusOK, s.makeRequest(http.MethodDelete, "/currencies/"+hk, nil).Code)

	again := s.makeRequest(http.MethodPost, "/currencies", sampleCurrencyPayload("USDT", "other"))
	s.assertConflict(again, "duplicate_symbol", "info.symbol")
	assert.Contains(s.T(), s.decodeResponse(again).Message, "restore it")

	s.takeEvents()
	w := s.makeRequest(http.MethodPost, "/currencies/"+hk+"/restore?version=3", nil)
	require.Equal(s.T(), http.StatusOK, w.Code, w.Body.String())
	var resp struct {
		Info models.VersionInfo `json:"info"`
	}
	require.NoError(s.T(), json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(s.T(), int64(4), resp.Info.Version)

	var status string
	var deletedAt sql.NullTime
	require.NoError(s.T(), s.postgresSvc.DB.QueryRow(
		"SELECT status, deleted_at FROM currencies WHERE hk = $1", hk).Scan(&status, &deletedAt))
	assert.Equal(s.T(), models.CurrencyStatusPending, status)
	assert.False(s.T(), deletedAt.Valid)
	assert.False(s.T(), s.isPublic(hk), "a restored currency waits for approval")
	assert.Empty(s.T(), s.takeEvents(), "a pending currency is not announced")

	trail := s.auditTrail(hk)
	last := trail[len(trail)-1]
	assert.Equal(s.T(), audit.ActionCurrencyRestore, last.Action)
	assert.Equal(s.T(), s.adminID, last.ActorID)
	assert.Equal(s.T(), map[string]interface{}{"status": "deleted"}, last.Changes.Before)
	assert.Equal(s.T(), map[string]interface{}{"status": "pending"}, last.Changes.After)

	// It can be updated and approved again, as any pending currency.
	s.update(hk, map[string]interface{}{"status": "approved"})
	assert.True(s.T(), s.isPublic(hk))
}

func (s *AdminCurrenciesTestSuite) TestRestoreCurrency_Refusals() {
	hk := s.createCurrency("XMR", "")

	w := s.makeRequest(http.MethodPost, "/currencies/"+hk+"/restore", nil)
	assert.Equal(s.T(), http.StatusConflict, w.Code)
	assert.Equal(s.T(), "not_deleted", s.decodeResponse(w).Code)
	assert.Equal(s.T(), http.StatusBadRequest, s.makeRequest(http.MethodPost, "/currencies/not-a-uuid/restore", nil).Code)
	assert.Equal(s.T(), http.StatusNotFound, s.makeRequest(http.MethodPost, "/currencies/"+uuid.NewString()+"/restore", nil).Code)

	require.Equal(s.T(), http.StatusOK, s.makeRequest(http.MethodDelete, "/currencies/"+hk, nil).Code)
	w = s.makeRequest(http.MethodPost, "/currencies/"+hk+"/restore?version=1", nil)
	assert.Equal(s.T(), http.StatusConflict, w.Code, "a stale version")
	assert.Equal(s.T(), "version_conflict", s.decodeResponse(w).Code)
	assert.Equal(s.T(), http.StatusBadRequest, s.makeRequest(http.MethodPost, "/currencies/"+hk+"/restore?version=x", nil).Code)
}

// TestRestoreCurrency_ConcurrentRestoresSucceedOnce restores one currency
// from many requests at once, as several instances would: the row lock lets
// one of them through.
func (s *AdminCurrenciesTestSuite) TestRestoreCurrency_ConcurrentRestoresSucceedOnce() {
	hk := s.createCurrency("ZEC", "")
	require.Equal(s.T(), http.StatusOK, s.makeRequest(http.MethodDelete, "/currencies/"+hk, nil).Code)

	const n = 8
	codes := make(chan int, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes <- s.makeRequest(http.MethodPost, "/currencies/"+hk+"/restore", nil).Code
		}()
	}
	wg.Wait()
	close(codes)
	ok := 0
	for code := range codes {
		if code == http.StatusOK {
			ok++
		} else {
			assert.Equal(s.T(), http.StatusConflict, code)
		}
	}
	assert.Equal(s.T(), 1, ok)
	restores := 0
	for _, e := range s.auditTrail(hk) {
		if e.Action == audit.ActionCurrencyRestore {
			restores++
		}
	}
	assert.Equal(s.T(), 1, restores)
}

// TestCurrencyTimestamps_HaveATimeZone checks that the timestamps of
// currencies do not depend on the session time zone of the writer.
func (s *AdminCurrenciesTestSuite) TestCurrencyTimestamps_HaveATimeZone() {
	var types []string
	rows, err := s.postgresSvc.DB.Query(`SELECT data_type FROM information_schema.columns
		WHERE table_name = 'currencies' AND column_name IN ('created_at', 'updated_at', 'deleted_at')`)
	require.NoError(s.T(), err)
	defer rows.Close()
	for rows.Next() {
		var t string
		require.NoError(s.T(), rows.Scan(&t))
		types = append(types, t)
	}
	require.NoError(s.T(), rows.Err())
	assert.Equal(s.T(), []string{"timestamp with time zone", "timestamp with time zone", "timestamp with time zone"}, types)
}

// listPage is the info of a page of the admin list, as the tests read it.
type listPage struct {
	Items      []models.Currency `json:"items"`
	NextCursor string            `json:"next_cursor"`
}

func (s *AdminCurrenciesTestSuite) list(query string) listPage {
	s.T().Helper()
	w := s.makeRequest(http.MethodGet, "/currencies"+query, nil)
	require.Equal(s.T(), http.StatusOK, w.Code, w.Body.String())
	var resp struct {
		Info listPage `json:"info"`
	}
	require.NoError(s.T(), json.Unmarshal(w.Body.Bytes(), &resp))
	return resp.Info
}

func symbols(items []models.Currency) []string {
	out := make([]string, len(items))
	for i, c := range items {
		out[i] = c.Info.Symbol
	}
	return out
}

// TestListCurrencies_EveryStatusNewestFirst lists currencies of every status,
// deleted ones included, newest first, with their versions (#198).
func (s *AdminCurrenciesTestSuite) TestListCurrencies_EveryStatusNewestFirst() {
	pending := s.createCurrency("PEN", "")
	approved := s.createCurrency("APR", "")
	s.update(approved, map[string]interface{}{"status": "approved"})
	suspended := s.createCurrency("SUS", "")
	s.update(suspended, map[string]interface{}{"status": "suspend"})
	deleted := s.createCurrency("DEL", "")
	require.Equal(s.T(), http.StatusOK, s.makeRequest(http.MethodDelete, "/currencies/"+deleted, nil).Code)
	// Distinct creation times, so that the order is the order of creation.
	_, err := s.postgresSvc.DB.Exec(`UPDATE currencies SET created_at = NOW() - make_interval(secs => id) `)
	require.NoError(s.T(), err)

	page := s.list("")
	assert.Equal(s.T(), []string{"PEN", "APR", "SUS", "DEL"}, symbols(page.Items))
	assert.Empty(s.T(), page.NextCursor)
	byHK := map[string]models.Currency{}
	for _, c := range page.Items {
		byHK[c.HK] = c
	}
	assert.Equal(s.T(), models.CurrencyStatusPending, byHK[pending].Status)
	assert.Equal(s.T(), int64(2), byHK[approved].Version)
	assert.Equal(s.T(), models.CurrencyStatusDeleted, byHK[deleted].Status)
	assert.NotNil(s.T(), byHK[deleted].DeletedAt)
	assert.NotEmpty(s.T(), byHK[pending].CreatedAt)

	assert.Equal(s.T(), []string{"DEL"}, symbols(s.list("?status=deleted").Items))
	assert.Equal(s.T(), []string{"SUS"}, symbols(s.list("?status=suspend").Items))
	assert.Equal(s.T(), []string{"APR"}, symbols(s.list("?symbol=apr").Items), "the symbol in any case")
	assert.Empty(s.T(), s.list("?symbol=NONE").Items)
	assert.NotNil(s.T(), s.list("?symbol=NONE").Items, "an empty page has an empty list, not null")
}

func (s *AdminCurrenciesTestSuite) TestListCurrencies_FlagFilters() {
	token := sampleCurrencyPayload("TOK", "0xabc")
	token["info"].(map[string]interface{})["is_token"] = true
	require.Equal(s.T(), http.StatusCreated, s.makeRequest(http.MethodPost, "/currencies", token).Code)
	fiat := sampleCurrencyPayload("EUR", "")
	fiat["info"].(map[string]interface{})["if_fiat"] = true
	require.Equal(s.T(), http.StatusCreated, s.makeRequest(http.MethodPost, "/currencies", fiat).Code)
	s.createCurrency("BTC", "")

	assert.Equal(s.T(), []string{"TOK"}, symbols(s.list("?is_token=true").Items))
	assert.Equal(s.T(), []string{"EUR"}, symbols(s.list("?if_fiat=true").Items))
	assert.ElementsMatch(s.T(), []string{"EUR", "BTC"}, symbols(s.list("?is_token=false").Items))
	assert.Equal(s.T(), []string{"BTC"}, symbols(s.list("?is_token=false&if_fiat=false").Items), "the filters combine")
}

// TestListCurrencies_PagesCoverEveryCurrencyOnce walks the list a page at a
// time, with currencies created at the same instant, so that the hk breaks
// the ties, and with a currency created between two pages.
func (s *AdminCurrenciesTestSuite) TestListCurrencies_PagesCoverEveryCurrencyOnce() {
	want := map[string]bool{}
	for _, symbol := range []string{"AA", "BB", "CC", "DD", "EE"} {
		want[s.createCurrency(symbol, "")] = true
	}
	_, err := s.postgresSvc.DB.Exec(`UPDATE currencies SET created_at = '2026-11-07T10:00:00.123456Z'`)
	require.NoError(s.T(), err)

	seen := map[string]bool{}
	query := "?limit=2"
	pages := 0
	for {
		page := s.list(query)
		pages++
		assert.LessOrEqual(s.T(), len(page.Items), 2)
		for _, c := range page.Items {
			assert.False(s.T(), seen[c.HK], "a currency on two pages")
			seen[c.HK] = true
		}
		if pages == 1 {
			// A currency created now is newer than the cursor, so it does not
			// show on the next pages.
			s.createCurrency("NEW", "")
		}
		if page.NextCursor == "" {
			break
		}
		require.Less(s.T(), pages, 10)
		query = "?limit=2&cursor=" + page.NextCursor
	}
	assert.Equal(s.T(), 3, pages)
	assert.Equal(s.T(), want, seen)
}

// TestListCurrencies_SharedFilters checks the filters that the admin list
// shares with the public list (#218), with statuses that only it has.
func (s *AdminCurrenciesTestSuite) TestListCurrencies_SharedFilters() {
	btc := s.createCurrency("BTC", "")
	s.update(btc, map[string]interface{}{"status": "approved"})
	wbtc := s.createCurrency("WBTC", "0x2260fac5e5542a773aa44fbcfedf7c193bc2c599")
	s.update(wbtc, map[string]interface{}{"status": "suspend"})
	s.createCurrency("ETH", "")
	// Distinct creation times, in the order of creation.
	_, err := s.postgresSvc.DB.Exec(`UPDATE currencies SET created_at = NOW() + make_interval(secs => id)`)
	require.NoError(s.T(), err)

	assert.Equal(s.T(), []string{"BTC", "WBTC"}, symbols(s.list("?q=btc").Items), "the symbol that is the text first")
	assert.Equal(s.T(), []string{"WBTC", "BTC"}, symbols(s.list("?status=approved,suspend&sort=-symbol").Items))
	assert.Equal(s.T(), []string{"WBTC"}, symbols(s.list("?contract=0X2260FAC5E5542A773AA44FBCFEDF7C193BC2C599").Items))
	assert.Equal(s.T(), []string{"ETH", "BTC"}, symbols(s.list("?has_contract=false").Items), "newest first")
	assert.Equal(s.T(), []string{"BTC", "ETH", "WBTC"}, symbols(s.list("?sort=symbol").Items))
	w := s.makeRequest(http.MethodGet, "/currencies?sort=price&colour=red", nil)
	s.assertFieldErrors(w, "sort", "colour")
}

func (s *AdminCurrenciesTestSuite) TestListCurrencies_ValidationErrors() {
	w := s.makeRequest(http.MethodGet, "/currencies?status=gone&is_token=maybe&limit=0&symbol=ABCDEFGHIJKLMNOP", nil)
	s.assertFieldErrors(w, "status", "is_token", "limit", "symbol")
	w = s.makeRequest(http.MethodGet, "/currencies?cursor=bm90LWEtY3Vyc29y", nil)
	s.assertFieldErrors(w, "cursor")
	// A cursor of another list, well formed but with another position.
	other, err := pagination.Cursor(map[string]string{"hk": uuid.NewString()})
	require.NoError(s.T(), err)
	w = s.makeRequest(http.MethodGet, "/currencies?cursor="+other, nil)
	s.assertFieldErrors(w, "cursor")
}

// insertApproved writes an approved currency to the database directly, as a
// change that bypasses the admin routes would, so that it shows in the
// public list only once the cached list is dropped.
func (s *AdminCurrenciesTestSuite) insertApproved(symbol string) string {
	info, err := json.Marshal(models.CurrencyInfo{EnName: symbol, FaName: symbol, Symbol: symbol})
	require.NoError(s.T(), err)
	var hk string
	require.NoError(s.T(), s.postgresSvc.DB.QueryRow(
		"INSERT INTO currencies (status, info) VALUES ('approved', $1::jsonb) RETURNING hk", info,
	).Scan(&hk))
	return hk
}

// TestChanges_DropTheCachedListWhenThePublicViewChanges checks which changes
// drop the cached list of public currencies: those to a currency that was
// or becomes public, and no others.
func (s *AdminCurrenciesTestSuite) TestChanges_DropTheCachedListWhenThePublicViewChanges() {
	public := s.createCurrency("LTC", "")
	s.update(public, map[string]interface{}{"status": "approved"})
	hidden := s.createCurrency("XLM", "")
	require.True(s.T(), s.isPublic(public))

	direct := s.insertApproved("ZZZ")
	s.update(hidden, map[string]interface{}{"info": map[string]interface{}{"en_name": "Stellar"}})
	assert.False(s.T(), s.isPublic(direct), "a change to a hidden currency keeps the cached list")

	s.update(public, map[string]interface{}{"info": map[string]interface{}{"en_name": "Litecoin"}})
	assert.True(s.T(), s.isPublic(direct), "a change to a public currency drops the cached list")
	list, err := s.handler.Currencies.(*currencies.Catalog).List(context.Background())
	require.NoError(s.T(), err)
	assert.Equal(s.T(), "Litecoin", list[0].Info.EnName, "the public list shows the change")
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
		"unknown info field": {func(p map[string]interface{}) {
			p["info"].(map[string]interface{})["decimals"] = 8
		}, []string{"info.decimals"}},
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

// TestUpdateCurrency_EachFieldAlone changes one field per update, for every
// field and both directions of each flag, and checks that the stored row has
// the new value and that every other field kept its value.
func (s *AdminCurrenciesTestSuite) TestUpdateCurrency_EachFieldAlone() {
	type row struct {
		status string
		info   models.CurrencyInfo
	}
	coin := row{"pending", models.CurrencyInfo{EnName: "Coin", FaName: "Coin (fa)", IfFiat: false, IsToken: false}}
	token := row{"pending", models.CurrencyInfo{EnName: "Token", FaName: "Token (fa)", IsToken: true, Contract: "contract-token"}}
	fiat := row{"approved", models.CurrencyInfo{EnName: "Fiat", FaName: "Fiat (fa)", IfFiat: true}}

	cases := []struct {
		name   string
		start  row
		update map[string]interface{}
		change func(*row)
	}{
		{"status", coin, map[string]interface{}{"status": "suspend"}, func(r *row) { r.status = "suspend" }},
		{"en_name", coin, map[string]interface{}{"info": map[string]interface{}{"en_name": "Renamed"}}, func(r *row) { r.info.EnName = "Renamed" }},
		{"fa_name", coin, map[string]interface{}{"info": map[string]interface{}{"fa_name": "Renamed (fa)"}}, func(r *row) { r.info.FaName = "Renamed (fa)" }},
		{"symbol", coin, map[string]interface{}{"info": map[string]interface{}{"symbol": "newsym"}}, func(r *row) { r.info.Symbol = "NEWSYM" }},
		{"contract set", coin, map[string]interface{}{"info": map[string]interface{}{"contract": "contract-new"}}, func(r *row) { r.info.Contract = "contract-new" }},
		{"contract cleared", row{"pending", models.CurrencyInfo{EnName: "Coin", FaName: "Coin (fa)", Contract: "contract-old"}},
			map[string]interface{}{"info": map[string]interface{}{"contract": ""}}, func(r *row) { r.info.Contract = "" }},
		{"is_token to false", token, map[string]interface{}{"info": map[string]interface{}{"is_token": false}}, func(r *row) { r.info.IsToken = false }},
		{"is_token to true", row{"pending", models.CurrencyInfo{EnName: "Coin", FaName: "Coin (fa)", Contract: "contract-coin"}},
			map[string]interface{}{"info": map[string]interface{}{"is_token": true}}, func(r *row) { r.info.IsToken = true }},
		{"if_fiat to false", fiat, map[string]interface{}{"info": map[string]interface{}{"if_fiat": false}}, func(r *row) { r.info.IfFiat = false }},
		{"if_fiat to true", coin, map[string]interface{}{"info": map[string]interface{}{"if_fiat": true}}, func(r *row) { r.info.IfFiat = true }},
	}
	for i, tc := range cases {
		s.Run(tc.name, func() {
			start := tc.start
			start.info.Symbol = fmt.Sprintf("FLD%d", i)
			payload := map[string]interface{}{"status": start.status, "info": start.info}
			require.Equal(s.T(), http.StatusCreated, s.makeRequest(http.MethodPost, "/currencies", payload).Code)
			var hk string
			require.NoError(s.T(), s.postgresSvc.DB.QueryRow("SELECT hk FROM currencies WHERE info->>'symbol' = $1", start.info.Symbol).Scan(&hk))

			w := s.makeRequest(http.MethodPut, "/currencies/"+hk, tc.update)
			require.Equal(s.T(), http.StatusOK, w.Code, w.Body.String())

			want := start
			tc.change(&want)
			var got row
			var infoData []byte
			require.NoError(s.T(), s.postgresSvc.DB.QueryRow("SELECT status, info FROM currencies WHERE hk = $1", hk).Scan(&got.status, &infoData))
			require.NoError(s.T(), json.Unmarshal(infoData, &got.info))
			assert.Equal(s.T(), want, got)
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
		"unknown info field":     {map[string]interface{}{"info": map[string]interface{}{"foo": 1}}, []string{"info.foo"}},
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

// TestUpdateCurrency_ConcurrentUpdatesKeepEachOthersFields holds the row lock
// in a transaction, as a concurrent update would, while the handler updates
// another field. The handler must wait and apply its change on top of the
// committed one instead of overwriting it with the row it read before.
func (s *AdminCurrenciesTestSuite) TestUpdateCurrency_ConcurrentUpdatesKeepEachOthersFields() {
	hk := s.createCurrency("LTC", "")
	ctx := context.Background()

	tx, err := s.postgresSvc.DB.BeginTx(ctx, nil)
	require.NoError(s.T(), err)
	defer tx.Rollback()
	_, err = tx.Exec(`SELECT 1 FROM currencies WHERE hk = $1 FOR UPDATE`, hk)
	require.NoError(s.T(), err)

	done := make(chan int, 1)
	go func() {
		w := s.makeRequest(http.MethodPut, "/currencies/"+hk, map[string]interface{}{
			"info": map[string]interface{}{"fa_name": "Litecoin (new Persian name)"},
		})
		done <- w.Code
	}()

	// The handler must still be waiting for the lock.
	select {
	case code := <-done:
		s.T().Fatalf("the update finished with %d while the row was locked", code)
	case <-time.After(300 * time.Millisecond):
	}
	_, err = tx.Exec(`UPDATE currencies SET info = jsonb_set(info, '{en_name}', '"Litecoin"') WHERE hk = $1`, hk)
	require.NoError(s.T(), err)
	require.NoError(s.T(), tx.Commit())

	select {
	case code := <-done:
		require.Equal(s.T(), http.StatusOK, code)
	case <-time.After(10 * time.Second):
		s.T().Fatal("the update did not finish after the lock was released")
	}
	var infoData []byte
	require.NoError(s.T(), s.postgresSvc.DB.QueryRow("SELECT info FROM currencies WHERE hk = $1", hk).Scan(&infoData))
	var info models.CurrencyInfo
	require.NoError(s.T(), json.Unmarshal(infoData, &info))
	assert.Equal(s.T(), "Litecoin", info.EnName, "the concurrent change must not be lost")
	assert.Equal(s.T(), "Litecoin (new Persian name)", info.FaName)
}

// isPublic reports whether the currency is in the list that public responses
// read. The list is cached once it is read, so a change that does not drop
// it does not show.
func (s *AdminCurrenciesTestSuite) isPublic(hk string) bool {
	list, err := s.handler.Currencies.(*currencies.Catalog).List(context.Background())
	require.NoError(s.T(), err)
	for _, currency := range list {
		if currency.HK == hk {
			return true
		}
	}
	return false
}

func (s *AdminCurrenciesTestSuite) update(hk string, body map[string]interface{}) {
	s.T().Helper()
	w := s.makeRequest(http.MethodPut, "/currencies/"+hk, body)
	require.Equal(s.T(), http.StatusOK, w.Code, w.Body.String())
}

// TestCurrencyVisibility_FollowsStatusTransitions walks one currency through
// every status and checks what public clients can see after each step: the
// public list and the events.
func (s *AdminCurrenciesTestSuite) TestCurrencyVisibility_FollowsStatusTransitions() {
	hk := s.createCurrency("NEAR", "")
	assert.False(s.T(), s.isPublic(hk), "a pending currency must not be public")
	assert.Empty(s.T(), s.takeEvents(), "a hidden currency must not be announced")

	steps := []struct {
		name   string
		body   map[string]interface{}
		public bool
		events []string
	}{
		{"approve", map[string]interface{}{"status": "approved"}, true, []string{"create " + hk}},
		{"edit while approved", map[string]interface{}{"info": map[string]interface{}{"en_name": "Near"}}, true, []string{"update " + hk}},
		{"suspend", map[string]interface{}{"status": "suspend"}, false, []string{"delete " + hk}},
		{"edit while suspended", map[string]interface{}{"info": map[string]interface{}{"en_name": "NEAR"}}, false, []string{}},
		{"approve again", map[string]interface{}{"status": "approved"}, true, []string{"create " + hk}},
		{"back to pending", map[string]interface{}{"status": "pending"}, false, []string{"delete " + hk}},
		{"pending to suspended", map[string]interface{}{"status": "suspend"}, false, []string{}},
	}
	for _, step := range steps {
		s.update(hk, step.body)
		assert.Equal(s.T(), step.public, s.isPublic(hk), step.name)
		assert.Equal(s.T(), step.events, s.takeEvents(), step.name)
	}

	s.update(hk, map[string]interface{}{"status": "approved"})
	s.takeEvents()
	w := s.makeRequest(http.MethodDelete, "/currencies/"+hk, nil)
	require.Equal(s.T(), http.StatusOK, w.Code)
	assert.False(s.T(), s.isPublic(hk))
	assert.Equal(s.T(), []string{"delete " + hk}, s.takeEvents(), "deleting a public currency removes it from public view")
}

func (s *AdminCurrenciesTestSuite) TestCurrencyVisibility_HiddenCurrenciesStayUnannounced() {
	payload := sampleCurrencyPayload("ALGO", "")
	payload["status"] = "suspend"
	require.Equal(s.T(), http.StatusCreated, s.makeRequest(http.MethodPost, "/currencies", payload).Code)
	var hk string
	require.NoError(s.T(), s.postgresSvc.DB.QueryRow("SELECT hk FROM currencies WHERE info->>'symbol' = 'ALGO'").Scan(&hk))

	require.Equal(s.T(), http.StatusOK, s.makeRequest(http.MethodDelete, "/currencies/"+hk, nil).Code)

	assert.Empty(s.T(), s.takeEvents(), "creating and deleting a hidden currency must not reach WebSocket clients")
	assert.False(s.T(), s.isPublic(hk))
}

func (s *AdminCurrenciesTestSuite) TestCurrencyVisibility_SameWithAnEmptyCache() {
	approved := s.createCurrency("ATOM", "")
	s.update(approved, map[string]interface{}{"status": "approved"})
	pending := s.createCurrency("AVAX", "")
	suspended := s.createCurrency("FIL", "")
	s.update(suspended, map[string]interface{}{"status": "approved"})
	s.update(suspended, map[string]interface{}{"status": "suspend"})
	deleted := s.createCurrency("ETC", "")
	s.update(deleted, map[string]interface{}{"status": "approved"})
	require.Equal(s.T(), http.StatusOK, s.makeRequest(http.MethodDelete, "/currencies/"+deleted, nil).Code)

	visible := func() map[string]bool {
		return map[string]bool{
			"approved": s.isPublic(approved), "pending": s.isPublic(pending),
			"suspended": s.isPublic(suspended), "deleted": s.isPublic(deleted),
		}
	}
	before := visible()
	assert.Equal(s.T(), map[string]bool{"approved": true, "pending": false, "suspended": false, "deleted": false}, before)

	// Another instance starts with a catalog of its own, and Redis may
	// lose its data: the list is loaded from the database again.
	s.handler.Currencies = currencies.New(s.postgresSvc.DB, s.redisClient, time.Minute)
	assert.Equal(s.T(), before, visible(), "visibility must not change for another instance")
	require.NoError(s.T(), s.redisClient.FlushDB(context.Background()).Err())
	assert.Equal(s.T(), before, visible(), "visibility must not change when Redis lost its data")
}

// TestDeleteCurrency_WaitsForAConcurrentChange holds the row lock while it
// approves the currency, as a concurrent update would. The delete must wait
// and decide from the approved status: a public currency is deleted, so
// WebSocket clients must be told.
func (s *AdminCurrenciesTestSuite) TestDeleteCurrency_WaitsForAConcurrentChange() {
	hk := s.createCurrency("DOT", "")
	ctx := context.Background()

	tx, err := s.postgresSvc.DB.BeginTx(ctx, nil)
	require.NoError(s.T(), err)
	defer tx.Rollback()
	_, err = tx.Exec(`SELECT 1 FROM currencies WHERE hk = $1 FOR UPDATE`, hk)
	require.NoError(s.T(), err)

	done := make(chan int, 1)
	go func() {
		done <- s.makeRequest(http.MethodDelete, "/currencies/"+hk, nil).Code
	}()

	select {
	case code := <-done:
		s.T().Fatalf("the delete finished with %d while the row was locked", code)
	case <-time.After(300 * time.Millisecond):
	}
	_, err = tx.Exec(`UPDATE currencies SET status = 'approved' WHERE hk = $1`, hk)
	require.NoError(s.T(), err)
	require.NoError(s.T(), tx.Commit())

	select {
	case code := <-done:
		require.Equal(s.T(), http.StatusOK, code)
	case <-time.After(10 * time.Second):
		s.T().Fatal("the delete did not finish after the lock was released")
	}
	assert.Equal(s.T(), []string{"delete " + hk}, s.takeEvents(), "the delete must see the approved status")
}

func (s *AdminCurrenciesTestSuite) TestDeleteCurrency_ConcurrentDeletesSucceedOnce() {
	hk := s.createCurrency("UNI", "")
	s.update(hk, map[string]interface{}{"status": "approved"})
	s.takeEvents()

	const deletes = 5
	codes := make(chan int, deletes)
	var wg sync.WaitGroup
	for i := 0; i < deletes; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes <- s.makeRequest(http.MethodDelete, "/currencies/"+hk, nil).Code
		}()
	}
	wg.Wait()
	close(codes)

	counts := map[int]int{}
	for code := range codes {
		counts[code]++
	}
	assert.Equal(s.T(), map[int]int{http.StatusOK: 1, http.StatusNotFound: deletes - 1}, counts)
	assert.Equal(s.T(), []string{"delete " + hk}, s.takeEvents(), "one deletion, one event")
}

// makeOutboxReady ends the backoff of every pending message, as time would.
func (s *AdminCurrenciesTestSuite) makeOutboxReady() {
	_, err := s.postgresSvc.DB.Exec(`UPDATE outbox_events SET next_attempt_at = NOW() WHERE published_at IS NULL`)
	require.NoError(s.T(), err)
}

// TestEvents_ReachTheBrokerOnceItRecovers creates, updates, and deletes a
// public currency while RabbitMQ is unavailable. Every change succeeds, and
// its event is published, in order, once the broker is back.
func (s *AdminCurrenciesTestSuite) TestEvents_ReachTheBrokerOnceItRecovers() {
	s.events.down.Store(true)
	payload := sampleCurrencyPayload("SOL", "")
	payload["status"] = "approved"
	require.Equal(s.T(), http.StatusCreated, s.makeRequest(http.MethodPost, "/currencies", payload).Code)
	var hk string
	require.NoError(s.T(), s.postgresSvc.DB.QueryRow("SELECT hk FROM currencies WHERE info->>'symbol' = 'SOL'").Scan(&hk))
	s.update(hk, map[string]interface{}{"info": map[string]interface{}{"en_name": "Solana"}})
	require.Equal(s.T(), http.StatusOK, s.makeRequest(http.MethodDelete, "/currencies/"+hk, nil).Code)

	assert.Empty(s.T(), s.takeEvents(), "nothing is published while the broker is down")
	var pending, attempts int
	require.NoError(s.T(), s.postgresSvc.DB.QueryRow(
		`SELECT COUNT(*), COALESCE(MAX(attempts), 0) FROM outbox_events WHERE published_at IS NULL`,
	).Scan(&pending, &attempts))
	assert.Equal(s.T(), 3, pending, "the events wait in the outbox")
	assert.Equal(s.T(), 1, attempts, "the first one failed once, and the later ones wait for it")

	s.events.down.Store(false)
	s.makeOutboxReady()
	assert.Equal(s.T(), []string{"create " + hk, "update " + hk, "delete " + hk}, s.takeEvents(),
		"the events are published in order once the broker is back")
}

// deadRedis returns a client of a Redis that is not there.
func deadRedis(t *testing.T) *redis.Client {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := listener.Addr().String()
	require.NoError(t, listener.Close())
	client := redis.NewClient(&redis.Options{Addr: addr, DialTimeout: 100 * time.Millisecond, MaxRetries: -1})
	t.Cleanup(func() { client.Close() })
	return client
}

// TestCachedList_CatchesUpOnceRedisRecovers changes a public currency while
// Redis is unavailable, so that neither the handler nor the relay can drop
// the cached list. The change succeeds, its event waits for the drop, and
// once Redis is back the list shows the change.
func (s *AdminCurrenciesTestSuite) TestCachedList_CatchesUpOnceRedisRecovers() {
	ctx := context.Background()
	hk := s.createCurrency("NEO", "")
	s.update(hk, map[string]interface{}{"status": "approved"})
	// The relay drops the list for the approval first; then it is cached.
	s.takeEvents()
	require.True(s.T(), s.isPublic(hk), "the list is cached")

	catalog := s.handler.Currencies
	defer func() { s.handler.Currencies = catalog }()
	s.handler.Currencies = currencies.New(s.postgresSvc.DB, deadRedis(s.T()), time.Minute)
	s.cacheDown.Store(true)
	s.update(hk, map[string]interface{}{"info": map[string]interface{}{"en_name": "Neo"}})

	assert.Empty(s.T(), s.takeEvents(), "the event waits for the drop of the cached list")
	list, err := catalog.(*currencies.Catalog).List(ctx)
	require.NoError(s.T(), err)
	assert.Equal(s.T(), "Bitcoin", list[0].Info.EnName, "the cached list is out of date meanwhile")

	s.cacheDown.Store(false)
	s.makeOutboxReady()
	assert.Equal(s.T(), []string{"update " + hk}, s.takeEvents())
	list, err = catalog.(*currencies.Catalog).List(ctx)
	require.NoError(s.T(), err)
	assert.Equal(s.T(), "Neo", list[0].Info.EnName, "the relay dropped the list once Redis was back")
}

// TestEvents_HaveIDs checks that every event carries a version 7 UUID of
// its own, with which clients can drop a repeated delivery.
func (s *AdminCurrenciesTestSuite) TestEvents_HaveIDs() {
	hk := s.createCurrency("APT", "")
	s.update(hk, map[string]interface{}{"status": "approved"})
	s.update(hk, map[string]interface{}{"info": map[string]interface{}{"en_name": "Aptos"}})
	_, err := s.relay.Flush(context.Background())
	require.NoError(s.T(), err)

	s.events.mu.Lock()
	events := s.events.events
	s.events.mu.Unlock()
	require.Len(s.T(), events, 2)
	for _, event := range events {
		id, err := uuid.Parse(event.ID)
		require.NoError(s.T(), err)
		assert.Equal(s.T(), uuid.Version(7), id.Version())
	}
	assert.NotEqual(s.T(), events[0].ID, events[1].ID)
}

// TestChanges_OfHiddenCurrenciesWriteNoEvent checks that the outbox only
// holds changes to the public view.
func (s *AdminCurrenciesTestSuite) TestChanges_OfHiddenCurrenciesWriteNoEvent() {
	hk := s.createCurrency("HID", "")
	s.update(hk, map[string]interface{}{"info": map[string]interface{}{"en_name": "Hidden"}})
	var n int
	require.NoError(s.T(), s.postgresSvc.DB.QueryRow(`SELECT COUNT(*) FROM outbox_events`).Scan(&n))
	assert.Zero(s.T(), n)
}

// storedVersion reads the version of a currency from the database.
func (s *AdminCurrenciesTestSuite) storedVersion(hk string) int64 {
	var version int64
	require.NoError(s.T(), s.postgresSvc.DB.QueryRow("SELECT version FROM currencies WHERE hk = $1", hk).Scan(&version))
	return version
}

// responseVersion returns the version in the info of a response.
func (s *AdminCurrenciesTestSuite) responseVersion(w *httptest.ResponseRecorder) int64 {
	s.T().Helper()
	info, ok := s.decodeResponse(w).Info.(map[string]interface{})
	require.True(s.T(), ok, w.Body.String())
	version, ok := info["version"].(float64)
	require.True(s.T(), ok, w.Body.String())
	return int64(version)
}

func (s *AdminCurrenciesTestSuite) TestVersions_CountTheChanges() {
	w := s.makeRequest(http.MethodPost, "/currencies", sampleCurrencyPayload("VER", ""))
	require.Equal(s.T(), http.StatusCreated, w.Code)
	assert.Equal(s.T(), int64(1), s.responseVersion(w))
	var hk string
	require.NoError(s.T(), s.postgresSvc.DB.QueryRow("SELECT hk FROM currencies WHERE info->>'symbol' = 'VER'").Scan(&hk))

	w = s.makeRequest(http.MethodPut, "/currencies/"+hk, map[string]interface{}{"info": map[string]interface{}{"en_name": "Version"}})
	require.Equal(s.T(), http.StatusOK, w.Code)
	assert.Equal(s.T(), int64(2), s.responseVersion(w), "an update without a version still counts")

	w = s.makeRequest(http.MethodPut, "/currencies/"+hk, map[string]interface{}{"status": "approved", "version": 2})
	require.Equal(s.T(), http.StatusOK, w.Code)
	assert.Equal(s.T(), int64(3), s.responseVersion(w))

	w = s.makeRequest(http.MethodDelete, "/currencies/"+hk+"?version=3", nil)
	require.Equal(s.T(), http.StatusOK, w.Code)
	assert.Equal(s.T(), int64(4), s.responseVersion(w))
	assert.Equal(s.T(), int64(4), s.storedVersion(hk))
}

// TestVersions_AStaleUpdateIsRefused sends an update based on a version that
// another update replaced. It gets 409 with the current version, and
// changes nothing.
func (s *AdminCurrenciesTestSuite) TestVersions_AStaleUpdateIsRefused() {
	hk := s.createCurrency("OLD", "")
	s.update(hk, map[string]interface{}{"status": "approved", "info": map[string]interface{}{"en_name": "First"}})
	s.takeEvents()

	w := s.makeRequest(http.MethodPut, "/currencies/"+hk, map[string]interface{}{"info": map[string]interface{}{"en_name": "Second"}, "version": 1})
	require.Equal(s.T(), http.StatusConflict, w.Code)
	info, ok := s.decodeResponse(w).Info.(map[string]interface{})
	require.True(s.T(), ok)
	assert.Equal(s.T(), "version_conflict", info["code"])
	assert.Equal(s.T(), float64(2), info["version"], "the response tells the current version")

	var enName string
	require.NoError(s.T(), s.postgresSvc.DB.QueryRow("SELECT info->>'en_name' FROM currencies WHERE hk = $1", hk).Scan(&enName))
	assert.Equal(s.T(), "First", enName)
	assert.Equal(s.T(), int64(2), s.storedVersion(hk))
	assert.Empty(s.T(), s.takeEvents(), "a refused update announces nothing")
}

// TestVersions_OneOfConcurrentUpdatesWins sends several updates based on the
// same version at once. Exactly one is applied, the others get 409, and the
// final state is the one of the update that was applied.
func (s *AdminCurrenciesTestSuite) TestVersions_OneOfConcurrentUpdatesWins() {
	hk := s.createCurrency("RACE", "")
	const writers = 8
	codes := make([]int, writers)
	var wg sync.WaitGroup
	for i := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := s.makeRequest(http.MethodPut, "/currencies/"+hk, map[string]interface{}{
				"info": map[string]interface{}{"en_name": fmt.Sprintf("Writer %d", i)}, "version": 1,
			})
			codes[i] = w.Code
		}()
	}
	wg.Wait()

	winner := -1
	for i, code := range codes {
		switch code {
		case http.StatusOK:
			assert.Equal(s.T(), -1, winner, "only one update is applied")
			winner = i
		case http.StatusConflict:
		default:
			s.T().Errorf("writer %d got %d", i, code)
		}
	}
	require.NotEqual(s.T(), -1, winner, "one update is applied")
	var enName string
	require.NoError(s.T(), s.postgresSvc.DB.QueryRow("SELECT info->>'en_name' FROM currencies WHERE hk = $1", hk).Scan(&enName))
	assert.Equal(s.T(), fmt.Sprintf("Writer %d", winner), enName)
	assert.Equal(s.T(), int64(2), s.storedVersion(hk))
}

func (s *AdminCurrenciesTestSuite) TestVersions_AStaleDeleteIsRefused() {
	hk := s.createCurrency("DEL", "")
	s.update(hk, map[string]interface{}{"info": map[string]interface{}{"en_name": "Changed"}})

	assert.Equal(s.T(), http.StatusConflict, s.makeRequest(http.MethodDelete, "/currencies/"+hk+"?version=1", nil).Code)
	var deleted bool
	require.NoError(s.T(), s.postgresSvc.DB.QueryRow("SELECT deleted_at IS NOT NULL FROM currencies WHERE hk = $1", hk).Scan(&deleted))
	assert.False(s.T(), deleted)

	for _, bad := range []string{"abc", "0", "-1", ""} {
		w := s.makeRequest(http.MethodDelete, "/currencies/"+hk+"?version="+bad, nil)
		assert.Equal(s.T(), http.StatusBadRequest, w.Code, "version=%q", bad)
	}
	assert.Equal(s.T(), http.StatusOK, s.makeRequest(http.MethodDelete, "/currencies/"+hk+"?version=2", nil).Code)
}

func (s *AdminCurrenciesTestSuite) TestVersions_AreSetByTheServer() {
	payload := sampleCurrencyPayload("SET", "")
	payload["version"] = 5
	w := s.makeRequest(http.MethodPost, "/currencies", payload)
	assert.Equal(s.T(), http.StatusBadRequest, w.Code)
	s.assertFieldErrors(w, "version")

	hk := s.createCurrency("LOW", "")
	w = s.makeRequest(http.MethodPut, "/currencies/"+hk, map[string]interface{}{"version": 0})
	assert.Equal(s.T(), http.StatusBadRequest, w.Code)
	s.assertFieldErrors(w, "version")
}

func (s *AdminCurrenciesTestSuite) TestGetCurrency_ReturnsAnyStatusWithItsVersion() {
	hk := s.createCurrency("GET", "")
	w := s.makeRequest(http.MethodGet, "/currencies/"+hk, nil)
	require.Equal(s.T(), http.StatusOK, w.Code)
	var resp struct {
		Info models.Currency `json:"info"`
	}
	require.NoError(s.T(), json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(s.T(), hk, resp.Info.HK)
	assert.Equal(s.T(), models.CurrencyStatusPending, resp.Info.Status, "a hidden currency is returned to administrators")
	assert.Equal(s.T(), int64(1), resp.Info.Version)
	assert.NotEmpty(s.T(), resp.Info.CreatedAt)
	assert.Nil(s.T(), resp.Info.DeletedAt)

	require.Equal(s.T(), http.StatusOK, s.makeRequest(http.MethodDelete, "/currencies/"+hk, nil).Code)
	w = s.makeRequest(http.MethodGet, "/currencies/"+strings.ToUpper(hk), nil)
	require.Equal(s.T(), http.StatusOK, w.Code)
	require.NoError(s.T(), json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(s.T(), models.CurrencyStatusDeleted, resp.Info.Status)
	assert.Equal(s.T(), int64(2), resp.Info.Version)
	assert.NotNil(s.T(), resp.Info.DeletedAt)

	assert.Equal(s.T(), http.StatusNotFound, s.makeRequest(http.MethodGet, "/currencies/"+uuid.NewString(), nil).Code)
	assert.Equal(s.T(), http.StatusBadRequest, s.makeRequest(http.MethodGet, "/currencies/not-a-uuid", nil).Code)
}

// auditedChange is an audit event of a currency, as the tests read it.
type auditedChange struct {
	Action, ActorType, ActorID, Outcome string
	Changes                             audit.Changes
}

// auditTrail returns the audit events of a currency, oldest first.
func (s *AdminCurrenciesTestSuite) auditTrail(hk string) []auditedChange {
	rows, err := s.postgresSvc.DB.Query(`SELECT action, actor_type, actor_id, outcome, changes::text
		FROM audit_events WHERE target_type = 'currency' AND target_id = $1 ORDER BY hk`, hk)
	require.NoError(s.T(), err)
	defer rows.Close()
	var trail []auditedChange
	for rows.Next() {
		var e auditedChange
		var changes string
		require.NoError(s.T(), rows.Scan(&e.Action, &e.ActorType, &e.ActorID, &e.Outcome, &changes))
		require.NoError(s.T(), json.Unmarshal([]byte(changes), &e.Changes))
		trail = append(trail, e)
	}
	require.NoError(s.T(), rows.Err())
	return trail
}

// TestAudit_EveryChangeWritesOneEvent creates, updates, and deletes a
// currency, and expects one event for each, by the administrator of the
// request, with the fields that changed (#51).
func (s *AdminCurrenciesTestSuite) TestAudit_EveryChangeWritesOneEvent() {
	hk := s.createCurrency("AUD", "")
	s.update(hk, map[string]interface{}{"status": "approved", "info": map[string]interface{}{"en_name": "Audited"}})
	require.Equal(s.T(), http.StatusOK, s.makeRequest(http.MethodDelete, "/currencies/"+hk, nil).Code)

	trail := s.auditTrail(hk)
	require.Len(s.T(), trail, 3)
	for _, e := range trail {
		assert.Equal(s.T(), audit.ActorAdmin, e.ActorType)
		assert.Equal(s.T(), s.adminID, e.ActorID)
		assert.Equal(s.T(), audit.Success, e.Outcome)
	}
	assert.Equal(s.T(), audit.ActionCurrencyCreate, trail[0].Action)
	assert.Equal(s.T(), "AUD", trail[0].Changes.After["symbol"])
	assert.Empty(s.T(), trail[0].Changes.Before)

	assert.Equal(s.T(), audit.ActionCurrencyUpdate, trail[1].Action)
	assert.Equal(s.T(), map[string]interface{}{"status": "pending", "en_name": "Bitcoin"}, trail[1].Changes.Before,
		"only the fields that changed")
	assert.Equal(s.T(), map[string]interface{}{"status": "approved", "en_name": "Audited"}, trail[1].Changes.After)

	assert.Equal(s.T(), audit.ActionCurrencyDelete, trail[2].Action)
	assert.Equal(s.T(), map[string]interface{}{"status": "approved"}, trail[2].Changes.Before)
	assert.Equal(s.T(), map[string]interface{}{"status": "deleted"}, trail[2].Changes.After)
}

func (s *AdminCurrenciesTestSuite) TestAudit_RefusedChangesWriteNoEvent() {
	hk := s.createCurrency("NOP", "")
	stale := s.makeRequest(http.MethodPut, "/currencies/"+hk, map[string]interface{}{"info": map[string]interface{}{"en_name": "X"}, "version": 9})
	require.Equal(s.T(), http.StatusConflict, stale.Code)
	invalid := s.makeRequest(http.MethodPut, "/currencies/"+hk, map[string]interface{}{"info": map[string]interface{}{"symbol": "x"}})
	require.Equal(s.T(), http.StatusBadRequest, invalid.Code)

	trail := s.auditTrail(hk)
	require.Len(s.T(), trail, 1, "only the create")
	assert.Equal(s.T(), audit.ActionCurrencyCreate, trail[0].Action)
}

// TestAudit_AChangeWithoutAnAdministratorIsRefused calls the handler without
// the claims of an administrator. Its event cannot name the actor, so the
// change rolls back with it.
func (s *AdminCurrenciesTestSuite) TestAudit_AChangeWithoutAnAdministratorIsRefused() {
	router := gin.New()
	router.POST("/currencies", s.handler.CreateCurrency)
	body, err := json.Marshal(sampleCurrencyPayload("ORP", ""))
	require.NoError(s.T(), err)
	req := httptest.NewRequest(http.MethodPost, "/currencies", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(s.T(), http.StatusInternalServerError, w.Code)
	var n int
	require.NoError(s.T(), s.postgresSvc.DB.QueryRow("SELECT COUNT(*) FROM currencies WHERE info->>'symbol' = 'ORP'").Scan(&n))
	assert.Zero(s.T(), n, "no change without its event")
}
