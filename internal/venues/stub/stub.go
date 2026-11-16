// Package stub is a venue in memory, for the tests of what uses venues
// (#384): the router (#386, #387) and the reconciliation (#385). It fills
// a market order, and a limit order that its price crosses, at once at its
// price, keeps any other limit order open, and can
// lose the answer of the next call that changes something, as a timeout
// would after the venue did it.
package stub

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
	"github.com/mdastpak/crypto-exchange/internal/venues"
)

// Venue is a venue in memory. Its zero value is not usable: use New.
type Venue struct {
	name string

	mu          sync.Mutex
	balances    map[string]venues.Balance
	instruments []venues.Instrument
	networks    map[string][]venues.Network
	orders      map[string]venues.Order
	withdrawals map[string]venues.Withdrawal
	transfers   map[string]venues.Transfer
	whitelist   map[string]bool
	prices      map[string]shared.Decimal
	loseNext    bool
	rejectNext  string
}

// New returns an empty venue named name.
func New(name string) *Venue {
	return &Venue{name: name, balances: map[string]venues.Balance{}, networks: map[string][]venues.Network{},
		orders: map[string]venues.Order{}, withdrawals: map[string]venues.Withdrawal{}, transfers: map[string]venues.Transfer{},
		whitelist: map[string]bool{}, prices: map[string]shared.Decimal{}}
}

// SetBalance sets the free amount of asset.
func (v *Venue) SetBalance(asset string, free shared.Decimal) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.balances[asset] = venues.Balance{Asset: asset, Free: free, Locked: shared.MustNewDecimalFromString("0")}
}

// AddInstrument adds a market, filled by market orders at price.
func (v *Venue) AddInstrument(i venues.Instrument, price shared.Decimal) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.instruments = append(v.instruments, i)
	v.prices[i.Symbol] = price
}

// AddNetwork adds a network of asset.
func (v *Venue) AddNetwork(asset string, n venues.Network) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.networks[asset] = append(v.networks[asset], n)
}

// Whitelist puts address on the withdrawal whitelist.
func (v *Venue) Whitelist(asset, network, address string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.whitelist[asset+"|"+network+"|"+address] = true
}

// LoseNextAnswer makes the next call that changes something take effect,
// and answer with an unknown outcome.
func (v *Venue) LoseNextAnswer() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.loseNext = true
}

// RejectNext makes the next call that changes something refused with code.
func (v *Venue) RejectNext(code string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.rejectNext = code
}

func (v *Venue) Name() string                    { return v.name }
func (v *Venue) Mode() venues.Mode               { return venues.ModeTestnet }
func (v *Venue) Supports(venues.Capability) bool { return true }

func (v *Venue) Balances(context.Context) ([]venues.Balance, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	out := make([]venues.Balance, 0, len(v.balances))
	for _, b := range v.balances {
		out = append(out, b)
	}
	return out, nil
}

func (v *Venue) Instruments(context.Context) ([]venues.Instrument, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]venues.Instrument(nil), v.instruments...), nil
}

func (v *Venue) Networks(_ context.Context, asset string) ([]venues.Network, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]venues.Network(nil), v.networks[asset]...), nil
}

// change runs a call that changes something, with the injected faults.
func (v *Venue) change(op string, apply func() error) error {
	if v.rejectNext != "" {
		code := v.rejectNext
		v.rejectNext = ""
		return &venues.Error{Venue: v.name, Op: op, Kind: venues.KindRejected, Code: code, Err: errors.New("rejected by the stub")}
	}
	if err := apply(); err != nil {
		return err
	}
	if v.loseNext {
		v.loseNext = false
		return &venues.Error{Venue: v.name, Op: op, Kind: venues.KindUnknown, Err: errors.New("the answer was lost")}
	}
	return nil
}

func (v *Venue) instrument(symbol string) (venues.Instrument, bool) {
	for _, i := range v.instruments {
		if i.Symbol == symbol {
			return i, true
		}
	}
	return venues.Instrument{}, false
}

func (v *Venue) PlaceOrder(_ context.Context, req venues.OrderRequest) (venues.Order, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if _, ok := v.orders[req.ClientID]; ok {
		return venues.Order{}, &venues.Error{Venue: v.name, Op: "place_order", Kind: venues.KindRejected, Code: venues.CodeDuplicate,
			Err: fmt.Errorf("the client id %s is taken", req.ClientID)}
	}
	var o venues.Order
	err := v.change("place_order", func() error {
		inst, ok := v.instrument(req.Symbol)
		if !ok {
			return &venues.Error{Venue: v.name, Op: "place_order", Kind: venues.KindRejected, Code: "unknown_symbol", Err: venues.ErrNotFound}
		}
		zero := shared.MustNewDecimalFromString("0")
		o = venues.Order{ClientID: req.ClientID, VenueID: fmt.Sprintf("stub-%d", len(v.orders)+1), Symbol: req.Symbol, Side: req.Side,
			Type: req.Type, Status: venues.OrderNew, Quantity: req.Quantity, Price: req.Price, Filled: zero, FilledQuote: zero,
			UpdatedAt: time.Now().UTC()}
		price := v.prices[req.Symbol]
		marketable := req.Type == venues.OrderMarket ||
			req.Side == venues.SideBuy && !req.Price.LessThan(price) || req.Side == venues.SideSell && !req.Price.GreaterThan(price)
		if marketable {
			if err := v.fill(&o, inst, req.Quantity, price); err != nil {
				return err
			}
		}
		v.orders[req.ClientID] = o
		return nil
	})
	return o, err
}

// fill fills quantity of the order o at price, moving the balances.
func (v *Venue) fill(o *venues.Order, inst venues.Instrument, quantity, price shared.Decimal) error {
	zero := shared.MustNewDecimalFromString("0")
	quote := quantity.Mul(price)
	give, get, giveAmount, getAmount := inst.Quote, inst.Base, quote, quantity
	if o.Side == venues.SideSell {
		give, get, giveAmount, getAmount = inst.Base, inst.Quote, quantity, quote
	}
	if v.balances[give].Free.LessThan(giveAmount) {
		return &venues.Error{Venue: v.name, Op: "place_order", Kind: venues.KindRejected, Code: "insufficient_balance",
			Err: errors.New("insufficient balance")}
	}
	g, r := v.balances[give], v.balances[get]
	g.Asset, r.Asset = give, get
	g.Free, r.Free = g.Free.Sub(giveAmount), r.Free.Add(getAmount)
	if r.Locked.Decimal().IsZero() {
		r.Locked = zero
	}
	v.balances[give], v.balances[get] = g, r
	o.Filled, o.FilledQuote = o.Filled.Add(quantity), o.FilledQuote.Add(quote)
	o.Status = venues.OrderPartiallyFilled
	if !o.Filled.LessThan(o.Quantity) {
		o.Status = venues.OrderFilled
	}
	if o.Type == venues.OrderMarket {
		o.Price = price
	}
	o.UpdatedAt = time.Now().UTC()
	return nil
}

// SetPrice sets the price of a market, at which its orders fill.
func (v *Venue) SetPrice(symbol string, price shared.Decimal) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.prices[symbol] = price
}

// Fill fills quantity more of the open order clientID at its limit, as a
// trade against it would.
func (v *Venue) Fill(clientID string, quantity shared.Decimal) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	o, ok := v.orders[clientID]
	if !ok || o.Status.Done() {
		return venues.ErrNotFound
	}
	inst, _ := v.instrument(o.Symbol)
	if err := v.fill(&o, inst, quantity, o.Price); err != nil {
		return err
	}
	v.orders[clientID] = o
	return nil
}

func (v *Venue) CancelOrder(_ context.Context, _, clientID string) (venues.Order, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	o, ok := v.orders[clientID]
	if !ok {
		return venues.Order{}, venues.ErrNotFound
	}
	err := v.change("cancel_order", func() error {
		if !o.Status.Done() {
			o.Status, o.UpdatedAt = venues.OrderCancelled, time.Now().UTC()
			v.orders[clientID] = o
		}
		return nil
	})
	return o, err
}

func (v *Venue) GetOrder(_ context.Context, _, clientID string) (venues.Order, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	o, ok := v.orders[clientID]
	if !ok {
		return venues.Order{}, venues.ErrNotFound
	}
	return o, nil
}

func (v *Venue) Withdraw(_ context.Context, req venues.WithdrawRequest) (venues.Withdrawal, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if w, ok := v.withdrawals[req.ClientID]; ok {
		return w, nil
	}
	var w venues.Withdrawal
	err := v.change("withdraw", func() error {
		b := v.balances[req.Asset]
		if b.Free.LessThan(req.Amount) {
			return &venues.Error{Venue: v.name, Op: "withdraw", Kind: venues.KindRejected, Code: "insufficient_balance", Err: errors.New("insufficient balance")}
		}
		b.Free = b.Free.Sub(req.Amount)
		v.balances[req.Asset] = b
		w = venues.Withdrawal{ClientID: req.ClientID, VenueID: fmt.Sprintf("stub-w-%d", len(v.withdrawals)+1), Asset: req.Asset,
			Network: req.Network, Amount: req.Amount, Fee: shared.MustNewDecimalFromString("0"), Status: venues.TransferPending}
		v.withdrawals[req.ClientID] = w
		return nil
	})
	return w, err
}

func (v *Venue) GetWithdrawal(_ context.Context, clientID string) (venues.Withdrawal, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	w, ok := v.withdrawals[clientID]
	if !ok {
		return venues.Withdrawal{}, venues.ErrNotFound
	}
	return w, nil
}

func (v *Venue) DepositAddress(_ context.Context, asset, network string) (venues.DepositAddress, error) {
	return venues.DepositAddress{Asset: asset, Network: network, Address: "stub-" + v.name + "-" + asset + "-" + network}, nil
}

func (v *Venue) InternalTransfer(_ context.Context, req venues.TransferRequest) (venues.Transfer, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if t, ok := v.transfers[req.ClientID]; ok {
		return t, nil
	}
	var t venues.Transfer
	err := v.change("internal_transfer", func() error {
		t = venues.Transfer{ClientID: req.ClientID, VenueID: fmt.Sprintf("stub-t-%d", len(v.transfers)+1), Asset: req.Asset,
			Amount: req.Amount, Status: venues.TransferCompleted}
		v.transfers[req.ClientID] = t
		return nil
	})
	return t, err
}

func (v *Venue) GetTransfer(_ context.Context, clientID string) (venues.Transfer, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	t, ok := v.transfers[clientID]
	if !ok {
		return venues.Transfer{}, venues.ErrNotFound
	}
	return t, nil
}

func (v *Venue) Whitelisted(_ context.Context, asset, network, address, _ string) (bool, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.whitelist[asset+"|"+network+"|"+address], nil
}

var _ venues.Venue = (*Venue)(nil)
