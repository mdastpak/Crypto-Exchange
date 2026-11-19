package binance

import (
	"crypto/hmac"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
	"github.com/mdastpak/crypto-exchange/internal/venues"
)

// fake is Binance spot, in memory, as its API documents it: it checks the
// key, the signature, and the timestamp of every signed request, and
// answers from the fixtures of testdata.
type fake struct {
	t           *testing.T
	key, secret string
	price       shared.Decimal

	mu        sync.Mutex
	skew      time.Duration
	balances  map[string]shared.Decimal
	orders    map[string]map[string]any
	withdraws []map[string]any
	// historyQueries are the queries of the withdrawal history.
	historyQueries []url.Values
	transfers      []map[string]any
	nextID         int64
	loseNext       bool
	timeCalls      int
	queries        []string
	failStatus     int
}

func newFake(t *testing.T) (*fake, *httptest.Server) {
	f := &fake{t: t, key: "fake-api-key-AAAA", secret: "fake-api-secret-BBBB", price: shared.MustNewDecimalFromString("60000"),
		balances: map[string]shared.Decimal{"BTC": shared.MustNewDecimalFromString("0.5"), "USDT": shared.MustNewDecimalFromString("10000")},
		orders:   map[string]map[string]any{}, nextID: 1000}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fake) fixture(name string) []byte {
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		f.t.Fatal(err)
	}
	return b
}

func (f *fake) refuse(w http.ResponseWriter, status, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"code": code, "msg": msg})
}

func (f *fake) answer(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if b, ok := v.([]byte); ok {
		_, _ = w.Write(b)
		return
	}
	_ = json.NewEncoder(w).Encode(v)
}

// signed checks a signed request as Binance does.
func (f *fake) signed(w http.ResponseWriter, r *http.Request) (url.Values, bool) {
	raw := r.URL.RawQuery
	f.queries = append(f.queries, raw)
	payload, sig, ok := strings.Cut(raw, "&signature=")
	if !ok || r.Header.Get("X-MBX-APIKEY") != f.key {
		f.refuse(w, http.StatusUnauthorized, -2015, "Invalid API-key, IP, or permissions for action.")
		return nil, false
	}
	if !hmac.Equal([]byte(sig), []byte(Sign(venues.NewSecret(f.secret), payload))) {
		f.refuse(w, http.StatusBadRequest, -1022, "Signature for this request is not valid.")
		return nil, false
	}
	q, _ := url.ParseQuery(payload)
	ts, _ := strconv.ParseInt(q.Get("timestamp"), 10, 64)
	window, _ := strconv.ParseInt(q.Get("recvWindow"), 10, 64)
	now := time.Now().Add(f.skew).UnixMilli()
	if ts > now+1000 || now-ts > window {
		f.refuse(w, http.StatusBadRequest, -1021, "Timestamp for this request is outside of the recvWindow.")
		return nil, false
	}
	return q, true
}

// lose drops the connection after the change was made.
func (f *fake) lose(w http.ResponseWriter) bool {
	if !f.loseNext {
		return false
	}
	f.loseNext = false
	conn, _, err := w.(http.Hijacker).Hijack()
	if err == nil {
		_ = conn.Close()
	}
	return true
}

func (f *fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failStatus != 0 {
		w.WriteHeader(f.failStatus)
		return
	}
	route := r.Method + " " + r.URL.Path
	switch route {
	case "GET /api/v3/time":
		f.timeCalls++
		f.answer(w, map[string]any{"serverTime": time.Now().Add(f.skew).UnixMilli()})
		return
	case "GET /api/v3/exchangeInfo":
		f.answer(w, f.fixture("exchange_info.json"))
		return
	case "GET /api/v3/ticker/bookTicker":
		if r.Header.Get("X-MBX-APIKEY") != "" || r.URL.Query().Get("signature") != "" {
			f.t.Error("the book ticker is public: nothing of the key is sent")
		}
		f.answer(w, map[string]string{"symbol": r.URL.Query().Get("symbol"), "bidPrice": "59990.00000000", "bidQty": "1.2",
			"askPrice": "60010.00000000", "askQty": "0.8"})
		return
	}
	q, ok := f.signed(w, r)
	if !ok {
		return
	}
	switch route {
	case "GET /api/v3/account":
		var balances []map[string]string
		for asset, free := range f.balances {
			balances = append(balances, map[string]string{"asset": asset, "free": free.String(), "locked": "0.00000000"})
		}
		f.answer(w, map[string]any{"accountType": "SPOT", "balances": balances})
	case "POST /api/v3/order":
		f.placeOrder(w, q)
	case "GET /api/v3/order":
		o, ok := f.orders[q.Get("origClientOrderId")]
		if !ok {
			f.refuse(w, http.StatusBadRequest, -2013, "Order does not exist.")
			return
		}
		f.answer(w, o)
	case "DELETE /api/v3/order":
		o, ok := f.orders[q.Get("origClientOrderId")]
		if !ok || o["status"] != "NEW" {
			f.refuse(w, http.StatusBadRequest, -2011, "Unknown order sent.")
			return
		}
		o["status"] = "CANCELED"
		answer := map[string]any{}
		for k, v := range o {
			answer[k] = v
		}
		answer["origClientOrderId"], answer["clientOrderId"] = o["clientOrderId"], "cancel-xyz"
		f.answer(w, answer)
	case "GET /sapi/v1/capital/config/getall":
		f.answer(w, f.fixture("capital_config.json"))
	case "POST /sapi/v1/capital/withdraw/apply":
		f.nextID++
		id := fmt.Sprintf("w%d", f.nextID)
		f.withdraws = append(f.withdraws, map[string]any{"id": id, "amount": q.Get("amount"), "transactionFee": "1",
			"coin": q.Get("coin"), "status": 4, "network": q.Get("network"), "withdrawOrderId": q.Get("withdrawOrderId")})
		if f.lose(w) {
			return
		}
		f.answer(w, map[string]string{"id": id})
	case "GET /sapi/v1/capital/withdraw/history":
		f.historyQueries = append(f.historyQueries, q)
		out := []map[string]any{}
		for _, x := range f.withdraws {
			switch {
			case q.Get("withdrawOrderId") != "":
				if x["withdrawOrderId"] == q.Get("withdrawOrderId") {
					out = append(out, x)
				}
			case q.Get("status") == "" || fmt.Sprint(x["status"]) == q.Get("status"):
				out = append(out, x)
			}
		}
		f.answer(w, out)
	case "GET /sapi/v1/capital/deposit/address":
		f.answer(w, map[string]string{"address": "TDepositAddressOfOurs", "coin": q.Get("coin"), "tag": ""})
	case "POST /sapi/v1/sub-account/universalTransfer":
		f.nextID++
		f.transfers = append(f.transfers, map[string]any{"tranId": f.nextID, "asset": q.Get("asset"), "amount": q.Get("amount"),
			"status": "SUCCESS", "clientTranId": q.Get("clientTranId"), "toEmail": q.Get("toEmail")})
		f.answer(w, map[string]any{"tranId": f.nextID, "clientTranId": q.Get("clientTranId")})
	case "GET /sapi/v1/sub-account/universalTransfer":
		var out []map[string]any
		for _, x := range f.transfers {
			if x["clientTranId"] == q.Get("clientTranId") {
				out = append(out, x)
			}
		}
		f.answer(w, map[string]any{"result": out, "totalCount": len(out)})
	case "GET /sapi/v1/capital/withdraw/address/list":
		f.answer(w, []map[string]any{{"address": "TWhitelisted", "addressTag": "", "coin": "USDT", "network": "TRX", "whiteStatus": true},
			{"address": "TNotWhite", "addressTag": "", "coin": "USDT", "network": "TRX", "whiteStatus": false}})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fake) placeOrder(w http.ResponseWriter, q url.Values) {
	id := q.Get("newClientOrderId")
	if o, ok := f.orders[id]; ok && o["status"] == "NEW" {
		f.refuse(w, http.StatusBadRequest, -2010, "Duplicate order sent.")
		return
	}
	qty, err := shared.NewDecimalFromString(q.Get("quantity"))
	if err != nil || q.Get("symbol") != "BTCUSDT" {
		f.refuse(w, http.StatusBadRequest, -1013, "Filter failure: LOT_SIZE")
		return
	}
	f.nextID++
	o := map[string]any{"symbol": "BTCUSDT", "orderId": f.nextID, "clientOrderId": id, "origQty": qty.String(),
		"side": q.Get("side"), "type": q.Get("type"), "transactTime": time.Now().UnixMilli(), "updateTime": time.Now().UnixMilli(),
		"price": "0.00000000", "executedQty": "0.00000000", "cummulativeQuoteQty": "0.00000000", "status": "NEW"}
	if q.Get("type") == "LIMIT" {
		o["price"] = q.Get("price")
	} else {
		cost := qty.Mul(f.price)
		if q.Get("side") == "BUY" && f.balances["USDT"].LessThan(cost) {
			f.refuse(w, http.StatusBadRequest, -2010, "Account has insufficient balance for requested action.")
			return
		}
		f.balances["USDT"] = f.balances["USDT"].Sub(cost)
		f.balances["BTC"] = f.balances["BTC"].Add(qty)
		o["status"], o["executedQty"], o["cummulativeQuoteQty"] = "FILLED", qty.String(), cost.String()
	}
	f.orders[id] = o
	if f.lose(w) {
		return
	}
	f.answer(w, o)
}
