package binance

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
	"github.com/mdastpak/crypto-exchange/internal/marketdata/source"
	"github.com/mdastpak/crypto-exchange/internal/venues"
	"github.com/mdastpak/crypto-exchange/internal/venues/venuetest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newConnector(t *testing.T, mode venues.Mode) (*Binance, *fake) {
	f, srv := newFake(t)
	client, err := source.NewClient(source.ClientOptions{Source: "venue_binance", BaseURL: srv.URL, Timeout: 2 * time.Second, MaxAttempts: 2})
	require.NoError(t, err)
	creds := venues.NewCached(venues.Static{"binance": {APIKey: venues.NewSecret(f.key), APISecret: venues.NewSecret(f.secret)}}, time.Minute)
	b, err := New(Options{Name: "binance", Mode: mode, Client: client, Credentials: creds})
	require.NoError(t, err)
	return b, f
}

func harness(mode venues.Mode, unsupported ...venues.Capability) func(t *testing.T) venuetest.Harness {
	return func(t *testing.T) venuetest.Harness {
		b, f := newConnector(t, mode)
		return venuetest.Harness{Venue: b, Symbol: "BTCUSDT", Quantity: shared.MustNewDecimalFromString("0.01"),
			LimitPrice: shared.MustNewDecimalFromString("50000"), Secrets: []string{f.key, f.secret},
			LoseNextAnswer: func() { f.mu.Lock(); f.loseNext = true; f.mu.Unlock() }, Unsupported: unsupported}
	}
}

func TestBinanceTestnet_PassesTheContract(t *testing.T) {
	venuetest.Run(t, harness(venues.ModeTestnet, venues.CapabilityWithdraw, venues.CapabilityDepositAddress,
		venues.CapabilityNetworks, venues.CapabilityInternalTransfer, venues.CapabilityWhitelist))
}

func TestBinanceLive_PassesTheContract(t *testing.T) {
	venuetest.Run(t, harness(venues.ModeLive))
}

// The example of the documentation of Binance for a signed endpoint.
func TestSign_MatchesTheDocumentedExample(t *testing.T) {
	secret := venues.NewSecret("NhqPtmdSJYdKjVHjA7PZj4Mge3R5YNiP1e3UZjInClVN65XAbvqqM6A7H5fATj0j")
	payload := "symbol=LTCBTC&side=BUY&type=LIMIT&timeInForce=GTC&quantity=1&price=0.1&recvWindow=5000&timestamp=1499827319559"
	assert.Equal(t, "c8db56825ae71d6d79447849e617115f4a920fa2acdcab2b053c4b2838bd6b71", Sign(secret, payload))
}

func TestClassify(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
		kind   venues.Kind
		code   string
	}{
		{400, `{"code":-2010,"msg":"Duplicate order sent."}`, venues.KindRejected, venues.CodeDuplicate},
		{400, `{"code":-2010,"msg":"Account has insufficient balance for requested action."}`, venues.KindRejected, "-2010"},
		{400, `{"code":-1013,"msg":"Filter failure: LOT_SIZE"}`, venues.KindRejected, "-1013"},
		{400, `{"code":-1021,"msg":"Timestamp for this request is outside of the recvWindow."}`, venues.KindRetryable, "-1021"},
		{408, `{"code":-1007,"msg":"Timeout waiting for response from backend server."}`, venues.KindUnknown, "-1007"},
		{429, `{"code":-1003,"msg":"Too many requests."}`, venues.KindRetryable, "-1003"},
		{418, `{"code":-1003,"msg":"Way too many requests; IP banned."}`, venues.KindRetryable, "-1003"},
		{502, `<html>bad gateway</html>`, venues.KindRetryable, ""},
		{401, `{"code":-2015,"msg":"Invalid API-key, IP, or permissions for action."}`, venues.KindRejected, "-2015"},
	} {
		kind, code := Classify(tc.status, []byte(tc.body))
		assert.Equal(t, tc.kind, kind, tc.body)
		assert.Equal(t, tc.code, code, tc.body)
	}
}

func TestBinance_SignsWithTheClockOfTheVenue(t *testing.T) {
	b, f := newConnector(t, venues.ModeTestnet)
	f.mu.Lock()
	f.skew = 30 * time.Second
	f.mu.Unlock()
	_, err := b.Balances(context.Background())
	require.NoError(t, err, "the offset of the venue's clock is applied")
	f.mu.Lock()
	assert.Equal(t, 1, f.timeCalls)
	f.mu.Unlock()
	_, err = b.Balances(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, f.timeCalls, "the offset is kept")

	// The venue's clock moves: the refused timestamp makes the offset read
	// again on the next call.
	f.mu.Lock()
	f.skew = -40 * time.Second
	f.mu.Unlock()
	_, err = b.Balances(context.Background())
	var ve *venues.Error
	require.ErrorAs(t, err, &ve)
	assert.Equal(t, venues.KindRetryable, ve.Kind)
	assert.Equal(t, "-1021", ve.Code)
	_, err = b.Balances(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 2, f.timeCalls)
}

func TestBinance_NeverPutsTheKeyInTheQuery(t *testing.T) {
	b, f := newConnector(t, venues.ModeLive)
	_, err := b.Balances(context.Background())
	require.NoError(t, err)
	require.NotEmpty(t, f.queries)
	for _, q := range f.queries {
		assert.NotContains(t, q, f.key)
		assert.NotContains(t, q, f.secret)
	}
}

func TestBinance_AFailureOfTheVenueOnAnOrderIsAnUnknownOutcome(t *testing.T) {
	b, f := newConnector(t, venues.ModeTestnet)
	_, err := b.Balances(context.Background())
	require.NoError(t, err)
	f.mu.Lock()
	f.failStatus = http.StatusInternalServerError
	f.mu.Unlock()
	_, err = b.PlaceOrder(context.Background(), venues.OrderRequest{ClientID: "c-500", Symbol: "BTCUSDT", Side: venues.SideBuy,
		Type: venues.OrderMarket, Quantity: shared.MustNewDecimalFromString("0.01")})
	assert.Equal(t, venues.KindUnknown, venues.KindOf(err), "%v", err)
	_, err = b.GetOrder(context.Background(), "BTCUSDT", "c-500")
	assert.Equal(t, venues.KindRetryable, venues.KindOf(err), "a read that failed did nothing")
}

func TestBinance_RefusesWhatItCannotSendBeforeSendingIt(t *testing.T) {
	b, f := newConnector(t, venues.ModeTestnet)
	ctx := context.Background()
	for _, req := range []venues.OrderRequest{
		{ClientID: "has space", Symbol: "BTCUSDT", Side: venues.SideBuy, Type: venues.OrderMarket, Quantity: shared.MustNewDecimalFromString("1")},
		{ClientID: strings.Repeat("a", 37), Symbol: "BTCUSDT", Side: venues.SideBuy, Type: venues.OrderMarket, Quantity: shared.MustNewDecimalFromString("1")},
		{ClientID: "c1", Symbol: "BTCUSDT", Side: venues.SideBuy, Type: venues.OrderLimit, Quantity: shared.MustNewDecimalFromString("1")},
		{ClientID: "c1", Symbol: "BTCUSDT", Side: "hold", Type: venues.OrderMarket, Quantity: shared.MustNewDecimalFromString("1")},
		{ClientID: "c1", Symbol: "BTCUSDT", Side: venues.SideBuy, Type: venues.OrderMarket, Quantity: shared.MustNewDecimalFromString("0")},
	} {
		_, err := b.PlaceOrder(ctx, req)
		assert.Equal(t, venues.KindRejected, venues.KindOf(err), "%+v", req)
	}
	assert.Empty(t, f.queries, "nothing was sent")
}

func TestBinance_ReadsTheFiltersOfTheMarkets(t *testing.T) {
	b, _ := newConnector(t, venues.ModeTestnet)
	instruments, err := b.Instruments(context.Background())
	require.NoError(t, err)
	require.Len(t, instruments, 2)
	btc := instruments[0]
	assert.Equal(t, "BTCUSDT", btc.Symbol)
	assert.True(t, btc.Trading)
	assert.Equal(t, "0.01", btc.TickSize.String())
	assert.Equal(t, "0.00001", btc.LotSize.String())
	assert.Equal(t, "5", btc.MinNotional.String())
	assert.False(t, instruments[1].Trading, "a halted market")
	assert.Equal(t, "0.0001", instruments[1].MinNotional.String(), "the older MIN_NOTIONAL filter")
}

func TestBinanceLive_WithdrawsAndLooksTheWithdrawalUp(t *testing.T) {
	b, f := newConnector(t, venues.ModeLive)
	ctx := context.Background()
	req := venues.WithdrawRequest{ClientID: "w-1", Asset: "USDT", Network: "TRX", Address: "TWhitelisted",
		Amount: shared.MustNewDecimalFromString("25")}
	f.mu.Lock()
	f.loseNext = true
	f.mu.Unlock()
	_, err := b.Withdraw(ctx, req)
	assert.Equal(t, venues.KindUnknown, venues.KindOf(err), "%v", err)
	w, err := b.GetWithdrawal(ctx, "w-1")
	require.NoError(t, err)
	assert.Equal(t, venues.TransferPending, w.Status)
	assert.Equal(t, "25", w.Amount.String())
	_, err = b.GetWithdrawal(ctx, "w-2")
	assert.ErrorIs(t, err, venues.ErrNotFound)

	networks, err := b.Networks(ctx, "USDT")
	require.NoError(t, err)
	require.Len(t, networks, 2)
	assert.Equal(t, "TRX", networks[0].Network)
	assert.Equal(t, "10", networks[0].WithdrawMin.String())
	assert.False(t, networks[1].WithdrawEnabled)
	xrp, err := b.Networks(ctx, "XRP")
	require.NoError(t, err)
	assert.True(t, xrp[0].AddressTagNeeded)

	ok, err := b.Whitelisted(ctx, "USDT", "TRX", "TWhitelisted", "")
	require.NoError(t, err)
	assert.True(t, ok)
	ok, err = b.Whitelisted(ctx, "USDT", "TRX", "TNotWhite", "")
	require.NoError(t, err)
	assert.False(t, ok)
	ok, err = b.Whitelisted(ctx, "USDT", "TRX", "TElsewhere", "")
	require.NoError(t, err)
	assert.False(t, ok)

	addr, err := b.DepositAddress(ctx, "USDT", "TRX")
	require.NoError(t, err)
	assert.Equal(t, "TDepositAddressOfOurs", addr.Address)
}

func TestBinanceLive_TransfersToASubAccountByOurID(t *testing.T) {
	b, _ := newConnector(t, venues.ModeLive)
	ctx := context.Background()
	_, err := b.InternalTransfer(ctx, venues.TransferRequest{ClientID: "t-1", Asset: "USDT", Amount: shared.MustNewDecimalFromString("100"),
		To: "otc-sub@example.invalid"})
	require.NoError(t, err)
	got, err := b.GetTransfer(ctx, "t-1")
	require.NoError(t, err)
	assert.Equal(t, venues.TransferCompleted, got.Status)
	_, err = b.GetTransfer(ctx, "t-2")
	assert.ErrorIs(t, err, venues.ErrNotFound)
	_, err = b.InternalTransfer(ctx, venues.TransferRequest{ClientID: "t-3", Asset: "USDT", Amount: shared.MustNewDecimalFromString("1")})
	assert.Equal(t, venues.KindRejected, venues.KindOf(err), "a transfer from the master account to itself")
}

func TestBinance_ForgetsCredentialsThatTheVenueRefused(t *testing.T) {
	f, srv := newFake(t)
	client, err := source.NewClient(source.ClientOptions{Source: "venue_binance", BaseURL: srv.URL, Timeout: 2 * time.Second, MaxAttempts: 1})
	require.NoError(t, err)
	src := &switching{creds: venues.Credentials{APIKey: venues.NewSecret("old-key"), APISecret: venues.NewSecret("old")}}
	b, err := New(Options{Mode: venues.ModeTestnet, Client: client, Credentials: venues.NewCached(src, time.Hour)})
	require.NoError(t, err)
	_, err = b.Balances(context.Background())
	var ve *venues.Error
	require.ErrorAs(t, err, &ve)
	assert.Equal(t, "-2015", ve.Code)
	src.creds = venues.Credentials{APIKey: venues.NewSecret(f.key), APISecret: venues.NewSecret(f.secret)}
	_, err = b.Balances(context.Background())
	require.NoError(t, err, "the rotated credentials are read at once")
}

type switching struct{ creds venues.Credentials }

func (s *switching) Credentials(context.Context, string) (venues.Credentials, error) {
	return s.creds, nil
}

func TestNew_RefusesAModeWithoutAnAPI(t *testing.T) {
	_, err := New(Options{Mode: venues.ModeDryRun, Client: &source.Client{}, Credentials: venues.Static{}})
	assert.Error(t, err, "a dry run wraps a live connector")
	_, err = New(Options{Mode: venues.ModeLive})
	assert.True(t, err != nil && !errors.Is(err, venues.ErrUnsupported))
}

// TestBinanceLive_ReportsTheFeesOfCompletedWithdrawals covers #503: the
// fees of the withdrawals completed in the window, by the id of each, and
// a window that Binance answers.
func TestBinanceLive_ReportsTheFeesOfCompletedWithdrawals(t *testing.T) {
	b, f := newConnector(t, venues.ModeLive)
	ctx := context.Background()
	f.mu.Lock()
	f.withdraws = append(f.withdraws,
		map[string]any{"id": "done1", "amount": "25", "transactionFee": "1", "coin": "USDT", "status": 6, "withdrawOrderId": "a"},
		map[string]any{"id": "pending", "amount": "25", "transactionFee": "1", "coin": "USDT", "status": 4, "withdrawOrderId": "b"},
		map[string]any{"id": "free", "amount": "25", "transactionFee": "0", "coin": "USDT", "status": 6, "withdrawOrderId": "c"},
		map[string]any{"id": "done2", "amount": "0.1", "transactionFee": "0.0005", "coin": "BTC", "status": 6, "withdrawOrderId": "d"})
	f.mu.Unlock()
	fees, err := b.Fees(ctx, time.Now().Add(-365*24*time.Hour))
	require.NoError(t, err)
	assert.Equal(t, []venues.Fee{
		{ID: "withdraw:done1", Asset: "USDT", Amount: shared.MustNewDecimalFromString("1")},
		{ID: "withdraw:done2", Asset: "BTC", Amount: shared.MustNewDecimalFromString("0.0005")},
	}, fees)
	f.mu.Lock()
	q := f.historyQueries[len(f.historyQueries)-1]
	f.mu.Unlock()
	start, err := strconv.ParseInt(q.Get("startTime"), 10, 64)
	require.NoError(t, err)
	end, err := strconv.ParseInt(q.Get("endTime"), 10, 64)
	require.NoError(t, err)
	assert.LessOrEqual(t, end-start, (90 * 24 * time.Hour).Milliseconds(), "within the window that Binance answers")
	assert.Equal(t, "6", q.Get("status"))

	tb, _ := newConnector(t, venues.ModeTestnet)
	none, err := tb.Fees(ctx, time.Now().Add(-time.Hour))
	require.NoError(t, err)
	assert.Empty(t, none, "a testnet has no withdrawals")
}

// TestBinance_ReadsTheBestPrices covers the pre-check of #515: the public
// book ticker of a symbol.
func TestBinance_ReadsTheBestPrices(t *testing.T) {
	b, _ := newConnector(t, venues.ModeTestnet)
	book, err := b.BestPrices(context.Background(), "BTCUSDT")
	require.NoError(t, err)
	assert.Equal(t, "BTCUSDT", book.Symbol)
	assert.Equal(t, "59990", book.Bid.String())
	assert.Equal(t, "60010", book.Ask.String())
}
