// Package binance is the connector of Binance spot (#384): our balances,
// its markets, orders by our client order id, and, in live mode, the
// withdrawals, deposit addresses, transfers to and from sub-accounts, and
// the withdrawal whitelist of the /sapi API.
//
// Every signed request carries a timestamp of the venue's clock, which is
// read from /api/v3/time and kept as an offset, and an HMAC-SHA256 of its
// query with the API secret; the key travels in the X-MBX-APIKEY header.
// Neither the key, the secret, nor the signature is ever in a log, an
// error, or a URL that an error names. See docs/venues.md#binance.
package binance

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
	"github.com/mdastpak/crypto-exchange/internal/marketdata/source"
	"github.com/mdastpak/crypto-exchange/internal/venues"
)

// The addresses of Binance spot. The testnet has no /sapi API, so it has no
// withdrawals, networks, deposit addresses, transfers, or whitelist.
const (
	LiveBaseURL    = "https://api.binance.com"
	TestnetBaseURL = "https://testnet.binance.vision"
)

// DefaultRecvWindow is how long after its timestamp Binance accepts a
// signed request.
const DefaultRecvWindow = 5 * time.Second

// clockTTL is how long the offset of the venue's clock is kept.
const clockTTL = 10 * time.Minute

// Options configure a connector.
type Options struct {
	// Name is the venue's name in the configuration, such as binance.
	Name string
	// Mode is testnet or live.
	Mode venues.Mode
	// Client calls the venue; its base URL is LiveBaseURL or
	// TestnetBaseURL, or another in tests.
	Client      *source.Client
	Credentials venues.CredentialSource
	RecvWindow  time.Duration
	// Now is the clock of this node, in tests.
	Now func() time.Time
}

// Binance is a connector. It is safe for concurrent use.
type Binance struct {
	name   string
	mode   venues.Mode
	call   venues.Caller
	creds  venues.CredentialSource
	window time.Duration
	now    func() time.Time

	mu       sync.Mutex
	offset   time.Duration
	offsetAt time.Time
}

// New returns a connector. It does not contact the venue.
func New(o Options) (*Binance, error) {
	if o.Mode != venues.ModeTestnet && o.Mode != venues.ModeLive {
		return nil, fmt.Errorf("binance: the mode must be testnet or live, not %q", o.Mode)
	}
	if o.Client == nil || o.Credentials == nil {
		return nil, errors.New("binance: a client and a credential source are needed")
	}
	if o.Name == "" {
		o.Name = "binance"
	}
	if o.RecvWindow <= 0 {
		o.RecvWindow = DefaultRecvWindow
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Binance{name: o.Name, mode: o.Mode, creds: o.Credentials, window: o.RecvWindow, now: o.Now,
		call: venues.Caller{Venue: o.Name, Client: o.Client, Classify: Classify}}, nil
}

func (b *Binance) Name() string      { return b.name }
func (b *Binance) Mode() venues.Mode { return b.mode }

// Supports reports the capabilities of /sapi, which only live mode has.
func (b *Binance) Supports(c venues.Capability) bool {
	switch c {
	case venues.CapabilityWithdraw, venues.CapabilityDepositAddress, venues.CapabilityNetworks,
		venues.CapabilityInternalTransfer, venues.CapabilityWhitelist:
		return b.mode == venues.ModeLive
	}
	return false
}

// Classify reads the error of Binance, {"code": -2010, "msg": "..."}.
func Classify(status int, body []byte) (venues.Kind, string) {
	var e struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	_ = json.Unmarshal(body, &e)
	code := ""
	if e.Code != 0 {
		code = strconv.Itoa(e.Code)
	}
	switch {
	case e.Code == -2010 && strings.Contains(strings.ToLower(e.Msg), "duplicate"):
		return venues.KindRejected, venues.CodeDuplicate
	case e.Code == -1007, e.Code == -1006:
		// "Send status unknown; execution status unknown."
		return venues.KindUnknown, code
	case e.Code == -1021, e.Code == -1003, e.Code == -1008, e.Code == -1001:
		// Out of the receive window, too many requests, the server busy,
		// or disconnected: nothing was done.
		return venues.KindRetryable, code
	case status == http.StatusTeapot:
		// The address is banned for a while, for too many requests.
		return venues.KindRetryable, code
	}
	return venues.DefaultKind(status), code
}

// The codes of a signature or key that Binance refused: the credentials are
// read again on the next call.
var credentialCodes = map[string]bool{"-1022": true, "-2014": true, "-2015": true}

// do makes a request; signed requests are signed on every attempt.
func (b *Binance) do(ctx context.Context, op, method, path string, q url.Values, signed, idempotent bool, out any) error {
	r := source.Request{Method: method, Path: path, Query: q, Idempotent: idempotent}
	if signed {
		creds, err := b.creds.Credentials(ctx, b.name)
		if err != nil {
			return &venues.Error{Venue: b.name, Op: op, Kind: venues.KindRetryable, Err: err}
		}
		if creds.APIKey.Empty() || creds.APISecret.Empty() {
			return &venues.Error{Venue: b.name, Op: op, Kind: venues.KindRejected, Err: venues.ErrNoCredentials}
		}
		offset, err := b.clockOffset(ctx)
		if err != nil {
			return &venues.Error{Venue: b.name, Op: op, Kind: venues.KindRetryable, Err: fmt.Errorf("reading the clock of the venue: %w", err)}
		}
		r.Sign = b.signer(creds, offset)
	}
	err := b.call.Call(ctx, op, r, out)
	var ve *venues.Error
	if errors.As(err, &ve) {
		switch {
		case ve.Code == "-1021":
			b.forgetClock()
		case credentialCodes[ve.Code]:
			if c, ok := b.creds.(interface{ Forget(string) }); ok {
				c.Forget(b.name)
			}
		}
	}
	return err
}

// signer adds the timestamp, the receive window, and the signature to the
// query, and the key to the header.
func (b *Binance) signer(creds venues.Credentials, offset time.Duration) func(*http.Request) error {
	return func(req *http.Request) error {
		q := req.URL.RawQuery
		if q != "" {
			q += "&"
		}
		q += "recvWindow=" + strconv.FormatInt(b.window.Milliseconds(), 10) +
			"&timestamp=" + strconv.FormatInt(b.now().Add(offset).UnixMilli(), 10)
		req.URL.RawQuery = q + "&signature=" + Sign(creds.APISecret, q)
		req.Header.Set("X-MBX-APIKEY", creds.APIKey.Reveal())
		return nil
	}
}

// Sign is the signature of payload: the hex HMAC-SHA256 with secret.
func Sign(secret venues.Secret, payload string) string {
	mac := hmac.New(sha256.New, []byte(secret.Reveal()))
	mac.Write([]byte(payload))
	return hex.EncodeToString(mac.Sum(nil))
}

// clockOffset is how far the venue's clock is ahead of ours, read again
// every clockTTL, or after the venue refused a timestamp.
func (b *Binance) clockOffset(ctx context.Context) (time.Duration, error) {
	b.mu.Lock()
	if !b.offsetAt.IsZero() && b.now().Sub(b.offsetAt) < clockTTL {
		defer b.mu.Unlock()
		return b.offset, nil
	}
	b.mu.Unlock()
	before := b.now()
	var body struct {
		ServerTime int64 `json:"serverTime"`
	}
	if err := b.call.Call(ctx, "time", source.Request{Method: http.MethodGet, Path: "api/v3/time", Idempotent: true}, &body); err != nil {
		return 0, err
	}
	after := b.now()
	if body.ServerTime <= 0 {
		return 0, errors.New("the venue answered no time")
	}
	// The venue read its clock about halfway through the request.
	mid := before.Add(after.Sub(before) / 2)
	offset := time.UnixMilli(body.ServerTime).Sub(mid)
	b.mu.Lock()
	b.offset, b.offsetAt = offset, after
	b.mu.Unlock()
	return offset, nil
}

func (b *Binance) forgetClock() {
	b.mu.Lock()
	b.offsetAt = time.Time{}
	b.mu.Unlock()
}

// decimal reads an amount of the venue, which Binance sends as a string.
func decimal(op string, venue string, v json.Number) (shared.Decimal, error) {
	s := strings.TrimSpace(v.String())
	if s == "" {
		return shared.NewDecimalFromInt64(0), nil
	}
	d, err := shared.NewDecimalFromString(s)
	if err != nil {
		return shared.Decimal{}, &venues.Error{Venue: venue, Op: op, Kind: venues.KindRetryable, Err: fmt.Errorf("the amount %q cannot be read", s)}
	}
	return d, nil
}

// decimals reads several amounts, stopping at the first that cannot be read.
type decimals struct {
	op, venue string
	err       error
}

func (d *decimals) read(v json.Number) shared.Decimal {
	if d.err != nil {
		return shared.Decimal{}
	}
	out, err := decimal(d.op, d.venue, v)
	d.err = err
	return out
}

func (b *Binance) Balances(ctx context.Context) ([]venues.Balance, error) {
	var body struct {
		Balances []struct {
			Asset  string      `json:"asset"`
			Free   json.Number `json:"free"`
			Locked json.Number `json:"locked"`
		} `json:"balances"`
	}
	if err := b.do(ctx, "balances", http.MethodGet, "api/v3/account", url.Values{"omitZeroBalances": {"true"}}, true, true, &body); err != nil {
		return nil, err
	}
	d := decimals{op: "balances", venue: b.name}
	out := make([]venues.Balance, 0, len(body.Balances))
	for _, x := range body.Balances {
		out = append(out, venues.Balance{Asset: x.Asset, Free: d.read(x.Free), Locked: d.read(x.Locked)})
	}
	return out, d.err
}

func (b *Binance) Instruments(ctx context.Context) ([]venues.Instrument, error) {
	var body struct {
		Symbols []struct {
			Symbol     string `json:"symbol"`
			Status     string `json:"status"`
			BaseAsset  string `json:"baseAsset"`
			QuoteAsset string `json:"quoteAsset"`
			Filters    []struct {
				FilterType  string      `json:"filterType"`
				TickSize    json.Number `json:"tickSize"`
				StepSize    json.Number `json:"stepSize"`
				MinQty      json.Number `json:"minQty"`
				MinNotional json.Number `json:"minNotional"`
			} `json:"filters"`
		} `json:"symbols"`
	}
	q := url.Values{"permissions": {"SPOT"}}
	if err := b.do(ctx, "instruments", http.MethodGet, "api/v3/exchangeInfo", q, false, true, &body); err != nil {
		return nil, err
	}
	d := decimals{op: "instruments", venue: b.name}
	zero := shared.NewDecimalFromInt64(0)
	out := make([]venues.Instrument, 0, len(body.Symbols))
	for _, s := range body.Symbols {
		i := venues.Instrument{Symbol: s.Symbol, Base: s.BaseAsset, Quote: s.QuoteAsset, Trading: s.Status == "TRADING",
			TickSize: zero, LotSize: zero, MinQuantity: zero, MinNotional: zero}
		for _, f := range s.Filters {
			switch f.FilterType {
			case "PRICE_FILTER":
				i.TickSize = d.read(f.TickSize)
			case "LOT_SIZE":
				i.LotSize, i.MinQuantity = d.read(f.StepSize), d.read(f.MinQty)
			case "NOTIONAL", "MIN_NOTIONAL":
				i.MinNotional = d.read(f.MinNotional)
			}
		}
		out = append(out, i)
	}
	return out, d.err
}

func (b *Binance) Networks(ctx context.Context, asset string) ([]venues.Network, error) {
	if !b.Supports(venues.CapabilityNetworks) {
		return nil, venues.ErrUnsupported
	}
	var body []struct {
		Coin        string `json:"coin"`
		NetworkList []struct {
			Network        string      `json:"network"`
			DepositEnable  bool        `json:"depositEnable"`
			WithdrawEnable bool        `json:"withdrawEnable"`
			WithdrawFee    json.Number `json:"withdrawFee"`
			WithdrawMin    json.Number `json:"withdrawMin"`
			MinConfirm     int         `json:"minConfirm"`
			SameAddress    bool        `json:"sameAddress"`
		} `json:"networkList"`
	}
	if err := b.do(ctx, "networks", http.MethodGet, "sapi/v1/capital/config/getall", nil, true, true, &body); err != nil {
		return nil, err
	}
	d := decimals{op: "networks", venue: b.name}
	var out []venues.Network
	for _, c := range body {
		if c.Coin != asset {
			continue
		}
		for _, n := range c.NetworkList {
			out = append(out, venues.Network{Network: n.Network, DepositEnabled: n.DepositEnable, WithdrawEnabled: n.WithdrawEnable,
				WithdrawFee: d.read(n.WithdrawFee), WithdrawMin: d.read(n.WithdrawMin), Confirmations: n.MinConfirm,
				AddressTagNeeded: n.SameAddress})
		}
	}
	return out, d.err
}

// clientIDPattern is what Binance accepts as a client order id.
var clientIDPattern = regexp.MustCompile(`^[.A-Z:/a-z0-9_-]{1,36}$`)

func (b *Binance) rejected(op, what string) error {
	return &venues.Error{Venue: b.name, Op: op, Kind: venues.KindRejected, Err: errors.New(what)}
}

// order is an order as Binance answers it.
type order struct {
	Symbol              string      `json:"symbol"`
	OrderID             int64       `json:"orderId"`
	ClientOrderID       string      `json:"clientOrderId"`
	OrigClientOrderID   string      `json:"origClientOrderId"`
	Price               json.Number `json:"price"`
	OrigQty             json.Number `json:"origQty"`
	ExecutedQty         json.Number `json:"executedQty"`
	CummulativeQuoteQty json.Number `json:"cummulativeQuoteQty"`
	Status              string      `json:"status"`
	Type                string      `json:"type"`
	Side                string      `json:"side"`
	TransactTime        int64       `json:"transactTime"`
	UpdateTime          int64       `json:"updateTime"`
}

var orderStatuses = map[string]venues.OrderStatus{
	"NEW": venues.OrderNew, "PENDING_NEW": venues.OrderNew, "PARTIALLY_FILLED": venues.OrderPartiallyFilled,
	"FILLED": venues.OrderFilled, "CANCELED": venues.OrderCancelled, "PENDING_CANCEL": venues.OrderNew,
	"REJECTED": venues.OrderRejected, "EXPIRED": venues.OrderExpired, "EXPIRED_IN_MATCH": venues.OrderExpired,
}

func (b *Binance) order(op string, o order) (venues.Order, error) {
	status, ok := orderStatuses[o.Status]
	if !ok {
		return venues.Order{}, &venues.Error{Venue: b.name, Op: op, Kind: venues.KindRetryable, Err: fmt.Errorf("unknown order status %q", o.Status)}
	}
	clientID := o.ClientOrderID
	if o.OrigClientOrderID != "" {
		// A cancel answers the id of the cancel as clientOrderId.
		clientID = o.OrigClientOrderID
	}
	d := decimals{op: op, venue: b.name}
	at := o.UpdateTime
	if at == 0 {
		at = o.TransactTime
	}
	out := venues.Order{ClientID: clientID, VenueID: strconv.FormatInt(o.OrderID, 10), Symbol: o.Symbol,
		Side: venues.Side(strings.ToLower(o.Side)), Type: venues.OrderType(strings.ToLower(o.Type)), Status: status,
		Quantity: d.read(o.OrigQty), Price: d.read(o.Price), Filled: d.read(o.ExecutedQty), FilledQuote: d.read(o.CummulativeQuoteQty)}
	if at > 0 {
		out.UpdatedAt = time.UnixMilli(at).UTC()
	}
	return out, d.err
}

func (b *Binance) PlaceOrder(ctx context.Context, req venues.OrderRequest) (venues.Order, error) {
	const op = "place_order"
	if !clientIDPattern.MatchString(req.ClientID) {
		return venues.Order{}, b.rejected(op, "the client id must be 1 to 36 of letters, digits, and .:/_-")
	}
	if !req.Quantity.IsPositive() {
		return venues.Order{}, b.rejected(op, "the quantity must be positive")
	}
	q := url.Values{"symbol": {req.Symbol}, "newClientOrderId": {req.ClientID}, "quantity": {req.Quantity.String()},
		"newOrderRespType": {"RESULT"}}
	switch req.Side {
	case venues.SideBuy, venues.SideSell:
		q.Set("side", strings.ToUpper(string(req.Side)))
	default:
		return venues.Order{}, b.rejected(op, "the side must be buy or sell")
	}
	switch req.Type {
	case venues.OrderMarket:
		q.Set("type", "MARKET")
	case venues.OrderLimit:
		if !req.Price.IsPositive() {
			return venues.Order{}, b.rejected(op, "a limit order needs a positive price")
		}
		q.Set("type", "LIMIT")
		q.Set("timeInForce", "GTC")
		q.Set("price", req.Price.String())
	default:
		return venues.Order{}, b.rejected(op, "the type must be limit or market")
	}
	var body order
	if err := b.do(ctx, op, http.MethodPost, "api/v3/order", q, true, false, &body); err != nil {
		return venues.Order{}, err
	}
	return b.order(op, body)
}

func (b *Binance) CancelOrder(ctx context.Context, symbol, clientID string) (venues.Order, error) {
	var body order
	q := url.Values{"symbol": {symbol}, "origClientOrderId": {clientID}}
	// -2011 is a cancel refused, of an order that is unknown or done
	// alike: the caller reads the order with GetOrder.
	if err := b.do(ctx, "cancel_order", http.MethodDelete, "api/v3/order", q, true, false, &body); err != nil {
		return venues.Order{}, err
	}
	return b.order("cancel_order", body)
}

func (b *Binance) GetOrder(ctx context.Context, symbol, clientID string) (venues.Order, error) {
	var body order
	q := url.Values{"symbol": {symbol}, "origClientOrderId": {clientID}}
	if err := b.do(ctx, "get_order", http.MethodGet, "api/v3/order", q, true, true, &body); err != nil {
		return venues.Order{}, notFound(err, "-2013")
	}
	return b.order("get_order", body)
}

// notFound turns the venue's code of an unknown order into ErrNotFound.
func notFound(err error, code string) error {
	var ve *venues.Error
	if errors.As(err, &ve) && ve.Code == code {
		return &venues.Error{Venue: ve.Venue, Op: ve.Op, Kind: venues.KindRejected, Code: ve.Code, Err: venues.ErrNotFound}
	}
	return err
}

func (b *Binance) Withdraw(ctx context.Context, req venues.WithdrawRequest) (venues.Withdrawal, error) {
	const op = "withdraw"
	if !b.Supports(venues.CapabilityWithdraw) {
		return venues.Withdrawal{}, venues.ErrUnsupported
	}
	if req.ClientID == "" || req.Address == "" || !req.Amount.IsPositive() {
		return venues.Withdrawal{}, b.rejected(op, "a withdrawal needs a client id, an address, and a positive amount")
	}
	q := url.Values{"coin": {req.Asset}, "network": {req.Network}, "address": {req.Address}, "amount": {req.Amount.String()},
		"withdrawOrderId": {req.ClientID}}
	if req.Tag != "" {
		q.Set("addressTag", req.Tag)
	}
	var body struct {
		ID string `json:"id"`
	}
	if err := b.do(ctx, op, http.MethodPost, "sapi/v1/capital/withdraw/apply", q, true, false, &body); err != nil {
		return venues.Withdrawal{}, err
	}
	return venues.Withdrawal{ClientID: req.ClientID, VenueID: body.ID, Asset: req.Asset, Network: req.Network, Amount: req.Amount,
		Fee: shared.NewDecimalFromInt64(0), Status: venues.TransferPending}, nil
}

// withdrawStatuses are the statuses of the withdrawal history: 0 an email
// sent, 1 cancelled, 2 awaiting approval, 3 rejected, 4 processing, 5 a
// failure, and 6 completed.
var withdrawStatuses = map[int]venues.TransferStatus{
	0: venues.TransferPending, 1: venues.TransferCancelled, 2: venues.TransferPending, 3: venues.TransferFailed,
	4: venues.TransferPending, 5: venues.TransferFailed, 6: venues.TransferCompleted,
}

func (b *Binance) GetWithdrawal(ctx context.Context, clientID string) (venues.Withdrawal, error) {
	const op = "get_withdrawal"
	if !b.Supports(venues.CapabilityWithdraw) {
		return venues.Withdrawal{}, venues.ErrUnsupported
	}
	var body []struct {
		ID              string      `json:"id"`
		Amount          json.Number `json:"amount"`
		TransactionFee  json.Number `json:"transactionFee"`
		Coin            string      `json:"coin"`
		Status          int         `json:"status"`
		TxID            string      `json:"txId"`
		Network         string      `json:"network"`
		WithdrawOrderID string      `json:"withdrawOrderId"`
	}
	if err := b.do(ctx, op, http.MethodGet, "sapi/v1/capital/withdraw/history", url.Values{"withdrawOrderId": {clientID}}, true, true, &body); err != nil {
		return venues.Withdrawal{}, err
	}
	for _, w := range body {
		if w.WithdrawOrderID != clientID {
			continue
		}
		status, ok := withdrawStatuses[w.Status]
		if !ok {
			return venues.Withdrawal{}, &venues.Error{Venue: b.name, Op: op, Kind: venues.KindRetryable, Err: fmt.Errorf("unknown withdrawal status %d", w.Status)}
		}
		d := decimals{op: op, venue: b.name}
		out := venues.Withdrawal{ClientID: clientID, VenueID: w.ID, Asset: w.Coin, Network: w.Network, Amount: d.read(w.Amount),
			Fee: d.read(w.TransactionFee), Status: status, TxID: w.TxID}
		return out, d.err
	}
	return venues.Withdrawal{}, venues.ErrNotFound
}

// withdrawalWindow is the longest window of the withdrawal history that
// Binance answers.
const withdrawalWindow = 90 * 24 * time.Hour

// withdrawalPage is the most withdrawals that Binance answers at once.
const withdrawalPage = 1000

// Fees implements venues.FeeReporter (#503): the fees of the withdrawals
// completed since since, from the withdrawal history, each by the id of its
// withdrawal. A testnet, which has no withdrawals, reports none. The
// trading commissions are not here: they belong to the fills.
func (b *Binance) Fees(ctx context.Context, since time.Time) ([]venues.Fee, error) {
	const op = "fees"
	if !b.Supports(venues.CapabilityWithdraw) {
		return nil, nil
	}
	now := time.Now()
	if since.Before(now.Add(-withdrawalWindow)) {
		since = now.Add(-withdrawalWindow + time.Hour)
	}
	var out []venues.Fee
	for offset := 0; ; offset += withdrawalPage {
		var body []struct {
			ID             string      `json:"id"`
			TransactionFee json.Number `json:"transactionFee"`
			Coin           string      `json:"coin"`
			Status         int         `json:"status"`
		}
		q := url.Values{"status": {"6"}, "startTime": {strconv.FormatInt(since.UnixMilli(), 10)},
			"endTime": {strconv.FormatInt(now.UnixMilli(), 10)}, "offset": {strconv.Itoa(offset)}, "limit": {strconv.Itoa(withdrawalPage)}}
		if err := b.do(ctx, op, http.MethodGet, "sapi/v1/capital/withdraw/history", q, true, true, &body); err != nil {
			return nil, err
		}
		d := decimals{op: op, venue: b.name}
		for _, w := range body {
			fee := d.read(w.TransactionFee)
			if w.Status != 6 || w.ID == "" || !fee.IsPositive() {
				continue
			}
			out = append(out, venues.Fee{ID: "withdraw:" + w.ID, Asset: w.Coin, Amount: fee})
		}
		if d.err != nil {
			return nil, d.err
		}
		if len(body) < withdrawalPage {
			return out, nil
		}
	}
}

func (b *Binance) DepositAddress(ctx context.Context, asset, network string) (venues.DepositAddress, error) {
	if !b.Supports(venues.CapabilityDepositAddress) {
		return venues.DepositAddress{}, venues.ErrUnsupported
	}
	var body struct {
		Address string `json:"address"`
		Coin    string `json:"coin"`
		Tag     string `json:"tag"`
	}
	q := url.Values{"coin": {asset}, "network": {network}}
	if err := b.do(ctx, "deposit_address", http.MethodGet, "sapi/v1/capital/deposit/address", q, true, true, &body); err != nil {
		return venues.DepositAddress{}, err
	}
	if body.Address == "" {
		return venues.DepositAddress{}, &venues.Error{Venue: b.name, Op: "deposit_address", Kind: venues.KindRetryable, Err: errors.New("the venue answered no address")}
	}
	return venues.DepositAddress{Asset: asset, Network: network, Address: body.Address, Tag: body.Tag}, nil
}

// InternalTransfer moves an asset between the spot accounts of the master
// account and its sub-accounts, named by their email; an empty From or To
// is the master account. It needs the key of the master account.
func (b *Binance) InternalTransfer(ctx context.Context, req venues.TransferRequest) (venues.Transfer, error) {
	const op = "internal_transfer"
	if !b.Supports(venues.CapabilityInternalTransfer) {
		return venues.Transfer{}, venues.ErrUnsupported
	}
	if req.ClientID == "" || !req.Amount.IsPositive() || req.From == req.To {
		return venues.Transfer{}, b.rejected(op, "a transfer needs a client id, a positive amount, and two accounts")
	}
	q := url.Values{"fromAccountType": {"SPOT"}, "toAccountType": {"SPOT"}, "asset": {req.Asset}, "amount": {req.Amount.String()},
		"clientTranId": {req.ClientID}}
	if req.From != "" {
		q.Set("fromEmail", req.From)
	}
	if req.To != "" {
		q.Set("toEmail", req.To)
	}
	var body struct {
		TranID json.Number `json:"tranId"`
	}
	if err := b.do(ctx, op, http.MethodPost, "sapi/v1/sub-account/universalTransfer", q, true, false, &body); err != nil {
		return venues.Transfer{}, err
	}
	return venues.Transfer{ClientID: req.ClientID, VenueID: body.TranID.String(), Asset: req.Asset, Amount: req.Amount,
		Status: venues.TransferPending}, nil
}

var transferStatuses = map[string]venues.TransferStatus{
	"SUCCESS": venues.TransferCompleted, "PROCESS": venues.TransferPending, "FAILURE": venues.TransferFailed,
}

func (b *Binance) GetTransfer(ctx context.Context, clientID string) (venues.Transfer, error) {
	const op = "get_transfer"
	if !b.Supports(venues.CapabilityInternalTransfer) {
		return venues.Transfer{}, venues.ErrUnsupported
	}
	var body struct {
		Result []struct {
			TranID       json.Number `json:"tranId"`
			Asset        string      `json:"asset"`
			Amount       json.Number `json:"amount"`
			Status       string      `json:"status"`
			ClientTranID string      `json:"clientTranId"`
		} `json:"result"`
	}
	if err := b.do(ctx, op, http.MethodGet, "sapi/v1/sub-account/universalTransfer", url.Values{"clientTranId": {clientID}}, true, true, &body); err != nil {
		return venues.Transfer{}, err
	}
	for _, t := range body.Result {
		if t.ClientTranID != clientID {
			continue
		}
		status, ok := transferStatuses[t.Status]
		if !ok {
			return venues.Transfer{}, &venues.Error{Venue: b.name, Op: op, Kind: venues.KindRetryable, Err: fmt.Errorf("unknown transfer status %q", t.Status)}
		}
		d := decimals{op: op, venue: b.name}
		out := venues.Transfer{ClientID: clientID, VenueID: t.TranID.String(), Asset: t.Asset, Amount: d.read(t.Amount), Status: status}
		return out, d.err
	}
	return venues.Transfer{}, venues.ErrNotFound
}

// Whitelisted reads the withdrawal address book of the account, where
// whiteStatus marks an address of the whitelist.
func (b *Binance) Whitelisted(ctx context.Context, asset, network, address, tag string) (bool, error) {
	if !b.Supports(venues.CapabilityWhitelist) {
		return false, venues.ErrUnsupported
	}
	var body []struct {
		Address     string `json:"address"`
		AddressTag  string `json:"addressTag"`
		Coin        string `json:"coin"`
		Network     string `json:"network"`
		WhiteStatus bool   `json:"whiteStatus"`
	}
	if err := b.do(ctx, "whitelisted", http.MethodGet, "sapi/v1/capital/withdraw/address/list", nil, true, true, &body); err != nil {
		return false, err
	}
	for _, a := range body {
		if a.Coin == asset && a.Network == network && a.Address == address && a.AddressTag == tag {
			return a.WhiteStatus, nil
		}
	}
	return false, nil
}

var (
	_ venues.Venue       = (*Binance)(nil)
	_ venues.FeeReporter = (*Binance)(nil)
)
