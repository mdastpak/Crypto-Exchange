package nobitex

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
)

// fake is Nobitex, in memory, as its API documents it: the token in the
// Authorization header, JSON bodies, and status "failed" with a code on
// HTTP 200 for a refused order.
type fake struct {
	t     *testing.T
	token string
	price shared.Decimal

	mu       sync.Mutex
	balances map[string]shared.Decimal
	orders   map[string]map[string]any
	nextID   int64
	loseNext bool
}

func newFake(t *testing.T) (*fake, *httptest.Server) {
	f := &fake{t: t, token: "fake-nobitex-token-CCCC", price: shared.MustNewDecimalFromString("60000"),
		balances: map[string]shared.Decimal{"usdt": shared.MustNewDecimalFromString("10000"), "btc": shared.MustNewDecimalFromString("0.2")},
		orders:   map[string]map[string]any{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fake) answer(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if b, ok := v.([]byte); ok {
		_, _ = w.Write(b)
		return
	}
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fake) failed(w http.ResponseWriter, code, message string) {
	f.answer(w, map[string]string{"status": "failed", "code": code, "message": message})
}

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
	if r.Method == http.MethodGet && r.URL.Path == "/market/stats" {
		b, err := os.ReadFile("testdata/market_stats.json")
		if err != nil {
			f.t.Fatal(err)
		}
		f.answer(w, b)
		return
	}
	if r.Header.Get("Authorization") != "Token "+f.token {
		w.WriteHeader(http.StatusUnauthorized)
		f.answer(w, map[string]string{"detail": "Invalid token."})
		return
	}
	var body map[string]string
	if r.Method == http.MethodPost {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
	}
	switch r.URL.Path {
	case "/users/wallets/list":
		var wallets []map[string]any
		for c, b := range f.balances {
			wallets = append(wallets, map[string]any{"id": 1, "currency": c, "balance": b.String(), "blockedBalance": "0"})
		}
		f.answer(w, map[string]any{"status": "ok", "wallets": wallets})
	case "/market/orders/add":
		f.add(w, body)
	case "/market/orders/status":
		o, ok := f.orders[body["clientOrderId"]]
		if !ok {
			f.failed(w, "NotFound", "Order not found")
			return
		}
		f.answer(w, map[string]any{"status": "ok", "order": o})
	case "/market/orders/update-status":
		o, ok := f.orders[body["clientOrderId"]]
		if !ok || o["status"] != "Active" {
			f.failed(w, "InvalidOrder", "the order cannot be cancelled")
			return
		}
		o["status"] = "Canceled"
		f.answer(w, map[string]any{"status": "ok", "updatedStatus": "Canceled"})
	case "/users/wallets/generate-address":
		f.answer(w, map[string]any{"status": "ok", "address": "TNobitexDepositOfOurs"})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fake) add(w http.ResponseWriter, body map[string]string) {
	id := body["clientOrderId"]
	if o, ok := f.orders[id]; ok && o["status"] == "Active" {
		f.failed(w, "DuplicateClientOrderId", "clientOrderId is used by an open order")
		return
	}
	qty, err := shared.NewDecimalFromString(body["amount"])
	if err != nil || body["srcCurrency"] != "btc" || body["dstCurrency"] != "usdt" {
		f.failed(w, "InvalidMarketPair", "market is not valid")
		return
	}
	f.nextID++
	o := map[string]any{"id": f.nextID, "clientOrderId": id, "type": body["type"], "execution": body["execution"],
		"srcCurrency": "btc", "dstCurrency": "usdt", "amount": qty.String(), "matchedAmount": "0", "totalPrice": "0",
		"price": body["price"], "status": "Active", "created_at": time.Now().UTC().Format(time.RFC3339Nano)}
	if body["execution"] == "market" {
		cost := qty.Mul(f.price)
		if body["type"] == "buy" && f.balances["usdt"].LessThan(cost) {
			f.failed(w, "OverValueOrder", "insufficient balance")
			return
		}
		f.balances["usdt"] = f.balances["usdt"].Sub(cost)
		f.balances["btc"] = f.balances["btc"].Add(qty)
		o["status"], o["matchedAmount"], o["totalPrice"], o["price"] = "Done", qty.String(), cost.String(), f.price.String()
	}
	f.orders[id] = o
	if f.lose(w) {
		return
	}
	f.answer(w, map[string]any{"status": "ok", "order": o})
}
