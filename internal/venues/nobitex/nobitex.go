// Package nobitex is the connector of Nobitex (#384), an Iranian exchange,
// which only a release of the ir region may turn on: our wallets, its
// markets, orders by our client order id, and deposit addresses.
//
// Nobitex has no testnet, no API for withdrawals without a one-time code,
// no sub-accounts, and no whitelist API, so those capabilities are absent;
// it publishes no filters of its markets either, so the steps and minimums
// of Instruments are zero, which is unknown. The token travels in the
// Authorization header, and is never in a log or an error. Rial markets
// are in rials (rls), as Nobitex trades them. See docs/venues.md#nobitex.
package nobitex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
	"github.com/mdastpak/crypto-exchange/internal/marketdata/source"
	"github.com/mdastpak/crypto-exchange/internal/venues"
)

// BaseURL is the address of the API of Nobitex.
const BaseURL = "https://apiv2.nobitex.ir"

// Options configure a connector.
type Options struct {
	Name        string
	Client      *source.Client
	Credentials venues.CredentialSource
}

// Nobitex is a connector, in live mode only. It is safe for concurrent use.
type Nobitex struct {
	name  string
	call  venues.Caller
	creds venues.CredentialSource
}

// New returns a connector. It does not contact the venue.
func New(o Options) (*Nobitex, error) {
	if o.Client == nil || o.Credentials == nil {
		return nil, errors.New("nobitex: a client and a credential source are needed")
	}
	if o.Name == "" {
		o.Name = "nobitex"
	}
	return &Nobitex{name: o.Name, creds: o.Credentials, call: venues.Caller{Venue: o.Name, Client: o.Client, Classify: Classify}}, nil
}

func (n *Nobitex) Name() string      { return n.name }
func (n *Nobitex) Mode() venues.Mode { return venues.ModeLive }

func (n *Nobitex) Supports(c venues.Capability) bool { return c == venues.CapabilityDepositAddress }

// envelope is the part of every answer of Nobitex that says whether it was
// done: status "ok", or "failed" with a code, also on HTTP 200.
type envelope struct {
	Status  string `json:"status"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Classify reads the error of Nobitex from an answer that is not 2xx.
func Classify(status int, body []byte) (venues.Kind, string) {
	var e envelope
	_ = json.Unmarshal(body, &e)
	return venues.DefaultKind(status), e.Code
}

// notFoundCodes are the codes of an order that Nobitex does not have.
var notFoundCodes = map[string]bool{"NotFound": true, "OrderNotFound": true, "InvalidOrderId": true}

// failed turns an answer of status "failed" into an error.
func (n *Nobitex) failed(op string, e envelope) error {
	if e.Status == "ok" {
		return nil
	}
	code := e.Code
	if code == "" {
		code = e.Status
	}
	err := fmt.Errorf("the venue refused it: %s", strings.TrimSpace(e.Message))
	switch {
	case notFoundCodes[e.Code]:
		err = venues.ErrNotFound
	case strings.Contains(strings.ToLower(e.Code), "duplicate"):
		code = venues.CodeDuplicate
	}
	return &venues.Error{Venue: n.name, Op: op, Kind: venues.KindRejected, Code: code, Err: err}
}

// do makes a request with the token; body is sent as JSON.
func (n *Nobitex) do(ctx context.Context, op, method, path string, body any, authed, idempotent bool, out any) error {
	r := source.Request{Method: method, Path: path, Idempotent: idempotent}
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r.Body, r.ContentType = b, "application/json"
	}
	if authed {
		creds, err := n.creds.Credentials(ctx, n.name)
		if err != nil {
			return &venues.Error{Venue: n.name, Op: op, Kind: venues.KindRetryable, Err: err}
		}
		if creds.Token.Empty() {
			return &venues.Error{Venue: n.name, Op: op, Kind: venues.KindRejected, Err: venues.ErrNoCredentials}
		}
		r.Sign = func(req *http.Request) error {
			req.Header.Set("Authorization", "Token "+creds.Token.Reveal())
			return nil
		}
	}
	err := n.call.Call(ctx, op, r, out)
	var status *source.StatusError
	if errors.As(err, &status) && (status.Code == http.StatusUnauthorized || status.Code == http.StatusForbidden) {
		if c, ok := n.creds.(interface{ Forget(string) }); ok {
			c.Forget(n.name)
		}
	}
	if errors.As(err, &status) && status.Code == http.StatusNotFound && idempotent {
		return &venues.Error{Venue: n.name, Op: op, Kind: venues.KindRejected, Err: venues.ErrNotFound}
	}
	return err
}

// amount reads an amount of Nobitex, a string or a number; empty or null is
// zero.
func amount(v json.RawMessage) (shared.Decimal, error) {
	s := strings.Trim(strings.TrimSpace(string(v)), `"`)
	if s == "" || s == "null" {
		return shared.NewDecimalFromInt64(0), nil
	}
	return shared.NewDecimalFromString(s)
}

type amounts struct {
	op, venue string
	err       error
}

func (a *amounts) read(v json.RawMessage) shared.Decimal {
	if a.err != nil {
		return shared.Decimal{}
	}
	d, err := amount(v)
	if err != nil {
		a.err = &venues.Error{Venue: a.venue, Op: a.op, Kind: venues.KindRetryable, Err: fmt.Errorf("the amount %s cannot be read", v)}
	}
	return d
}

func (n *Nobitex) Balances(ctx context.Context) ([]venues.Balance, error) {
	const op = "balances"
	var body struct {
		envelope
		Wallets []struct {
			Currency       string          `json:"currency"`
			Balance        json.RawMessage `json:"balance"`
			BlockedBalance json.RawMessage `json:"blockedBalance"`
		} `json:"wallets"`
	}
	if err := n.do(ctx, op, http.MethodPost, "users/wallets/list", struct{}{}, true, true, &body); err != nil {
		return nil, err
	}
	if err := n.failed(op, body.envelope); err != nil {
		return nil, err
	}
	a := amounts{op: op, venue: n.name}
	out := make([]venues.Balance, 0, len(body.Wallets))
	for _, w := range body.Wallets {
		total, blocked := a.read(w.Balance), a.read(w.BlockedBalance)
		if a.err != nil {
			return nil, a.err
		}
		out = append(out, venues.Balance{Asset: w.Currency, Free: total.Sub(blocked), Locked: blocked})
	}
	return out, nil
}

// Instruments are the markets of /market/stats, named as it names them,
// such as btc-rls; Nobitex publishes no filters, so they are zero.
func (n *Nobitex) Instruments(ctx context.Context) ([]venues.Instrument, error) {
	const op = "instruments"
	var body struct {
		envelope
		Stats map[string]struct {
			IsClosed bool `json:"isClosed"`
		} `json:"stats"`
	}
	if err := n.do(ctx, op, http.MethodGet, "market/stats", nil, false, true, &body); err != nil {
		return nil, err
	}
	if err := n.failed(op, body.envelope); err != nil {
		return nil, err
	}
	zero := shared.NewDecimalFromInt64(0)
	out := make([]venues.Instrument, 0, len(body.Stats))
	for symbol, s := range body.Stats {
		base, quote, ok := strings.Cut(symbol, "-")
		if !ok || base == "" || quote == "" {
			continue
		}
		out = append(out, venues.Instrument{Symbol: symbol, Base: base, Quote: quote, Trading: !s.IsClosed,
			TickSize: zero, LotSize: zero, MinQuantity: zero, MinNotional: zero})
	}
	return out, nil
}

func (n *Nobitex) Networks(context.Context, string) ([]venues.Network, error) {
	return nil, venues.ErrUnsupported
}

// order is an order as Nobitex answers it.
type order struct {
	ID            json.Number     `json:"id"`
	ClientOrderID string          `json:"clientOrderId"`
	Type          string          `json:"type"`
	Execution     string          `json:"execution"`
	SrcCurrency   string          `json:"srcCurrency"`
	DstCurrency   string          `json:"dstCurrency"`
	Price         json.RawMessage `json:"price"`
	Amount        json.RawMessage `json:"amount"`
	MatchedAmount json.RawMessage `json:"matchedAmount"`
	TotalPrice    json.RawMessage `json:"totalPrice"`
	Status        string          `json:"status"`
	CreatedAt     string          `json:"created_at"`
}

func (n *Nobitex) order(op string, o order) (venues.Order, error) {
	a := amounts{op: op, venue: n.name}
	filled := a.read(o.MatchedAmount)
	out := venues.Order{ClientID: o.ClientOrderID, VenueID: o.ID.String(), Symbol: o.SrcCurrency + "-" + o.DstCurrency,
		Side: venues.Side(o.Type), Quantity: a.read(o.Amount), Price: a.read(o.Price), Filled: filled,
		FilledQuote: a.read(o.TotalPrice)}
	if a.err != nil {
		return venues.Order{}, a.err
	}
	switch o.Execution {
	case "market", "Market":
		out.Type = venues.OrderMarket
	default:
		out.Type = venues.OrderLimit
	}
	switch o.Status {
	case "Active", "Inactive", "New":
		out.Status = venues.OrderNew
		if filled.IsPositive() {
			out.Status = venues.OrderPartiallyFilled
		}
	case "Done":
		out.Status = venues.OrderFilled
	case "Canceled", "Cancelled":
		out.Status = venues.OrderCancelled
	default:
		return venues.Order{}, &venues.Error{Venue: n.name, Op: op, Kind: venues.KindRetryable, Err: fmt.Errorf("unknown order status %q", o.Status)}
	}
	if t, err := time.Parse(time.RFC3339Nano, o.CreatedAt); err == nil {
		out.UpdatedAt = t.UTC()
	}
	return out, nil
}

// clientIDPattern bounds our client order ids at Nobitex.
var clientIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)

func (n *Nobitex) rejected(op, what string) error {
	return &venues.Error{Venue: n.name, Op: op, Kind: venues.KindRejected, Err: errors.New(what)}
}

func (n *Nobitex) PlaceOrder(ctx context.Context, req venues.OrderRequest) (venues.Order, error) {
	const op = "place_order"
	if !clientIDPattern.MatchString(req.ClientID) {
		return venues.Order{}, n.rejected(op, "the client id must be 1 to 32 of letters, digits, _ and -")
	}
	src, dst, ok := strings.Cut(req.Symbol, "-")
	if !ok || src == "" || dst == "" {
		return venues.Order{}, n.rejected(op, "the symbol must be src-dst, such as btc-rls")
	}
	if req.Side != venues.SideBuy && req.Side != venues.SideSell {
		return venues.Order{}, n.rejected(op, "the side must be buy or sell")
	}
	if !req.Quantity.IsPositive() {
		return venues.Order{}, n.rejected(op, "the quantity must be positive")
	}
	body := map[string]string{"type": string(req.Side), "srcCurrency": src, "dstCurrency": dst,
		"amount": req.Quantity.String(), "clientOrderId": req.ClientID}
	switch req.Type {
	case venues.OrderMarket:
		body["execution"] = "market"
	case venues.OrderLimit:
		if !req.Price.IsPositive() {
			return venues.Order{}, n.rejected(op, "a limit order needs a positive price")
		}
		body["execution"] = "limit"
		body["price"] = req.Price.String()
	default:
		return venues.Order{}, n.rejected(op, "the type must be limit or market")
	}
	var answer struct {
		envelope
		Order order `json:"order"`
	}
	if err := n.do(ctx, op, http.MethodPost, "market/orders/add", body, true, false, &answer); err != nil {
		return venues.Order{}, err
	}
	if err := n.failed(op, answer.envelope); err != nil {
		return venues.Order{}, err
	}
	if answer.Order.ClientOrderID == "" {
		answer.Order.ClientOrderID = req.ClientID
	}
	return n.order(op, answer.Order)
}

func (n *Nobitex) CancelOrder(ctx context.Context, symbol, clientID string) (venues.Order, error) {
	const op = "cancel_order"
	var answer struct {
		envelope
		UpdatedStatus string `json:"updatedStatus"`
	}
	body := map[string]string{"clientOrderId": clientID, "status": "canceled"}
	if err := n.do(ctx, op, http.MethodPost, "market/orders/update-status", body, true, false, &answer); err != nil {
		return venues.Order{}, err
	}
	if err := n.failed(op, answer.envelope); err != nil {
		return venues.Order{}, err
	}
	return n.GetOrder(ctx, symbol, clientID)
}

func (n *Nobitex) GetOrder(ctx context.Context, _, clientID string) (venues.Order, error) {
	const op = "get_order"
	var answer struct {
		envelope
		Order order `json:"order"`
	}
	if err := n.do(ctx, op, http.MethodPost, "market/orders/status", map[string]string{"clientOrderId": clientID}, true, true, &answer); err != nil {
		return venues.Order{}, err
	}
	if err := n.failed(op, answer.envelope); err != nil {
		if errors.Is(err, venues.ErrNotFound) {
			return venues.Order{}, venues.ErrNotFound
		}
		return venues.Order{}, err
	}
	return n.order(op, answer.Order)
}

func (n *Nobitex) Withdraw(context.Context, venues.WithdrawRequest) (venues.Withdrawal, error) {
	return venues.Withdrawal{}, venues.ErrUnsupported
}

func (n *Nobitex) GetWithdrawal(context.Context, string) (venues.Withdrawal, error) {
	return venues.Withdrawal{}, venues.ErrUnsupported
}

func (n *Nobitex) DepositAddress(ctx context.Context, asset, network string) (venues.DepositAddress, error) {
	const op = "deposit_address"
	var answer struct {
		envelope
		Address string `json:"address"`
		Tag     string `json:"tag"`
	}
	body := map[string]string{"currency": asset}
	if network != "" {
		body["network"] = network
	}
	// The address of a wallet is generated once, and the same one is
	// answered again, so the call is safe to repeat.
	if err := n.do(ctx, op, http.MethodPost, "users/wallets/generate-address", body, true, true, &answer); err != nil {
		return venues.DepositAddress{}, err
	}
	if err := n.failed(op, answer.envelope); err != nil {
		return venues.DepositAddress{}, err
	}
	if answer.Address == "" {
		return venues.DepositAddress{}, &venues.Error{Venue: n.name, Op: op, Kind: venues.KindRetryable, Err: errors.New("the venue answered no address")}
	}
	return venues.DepositAddress{Asset: asset, Network: network, Address: answer.Address, Tag: answer.Tag}, nil
}

func (n *Nobitex) InternalTransfer(context.Context, venues.TransferRequest) (venues.Transfer, error) {
	return venues.Transfer{}, venues.ErrUnsupported
}

func (n *Nobitex) GetTransfer(context.Context, string) (venues.Transfer, error) {
	return venues.Transfer{}, venues.ErrUnsupported
}

func (n *Nobitex) Whitelisted(context.Context, string, string, string, string) (bool, error) {
	return false, venues.ErrUnsupported
}

var _ venues.Venue = (*Nobitex)(nil)
