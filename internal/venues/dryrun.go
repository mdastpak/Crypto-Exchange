package venues

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"
)

// DryRun reads a venue as it is, and logs the orders, withdrawals, and
// transfers without sending them (ModeDryRun). What it logged is kept in
// memory, so that the caller can look it up by its id as it would at the
// venue; it does not survive a restart, as nothing was sent.
type DryRun struct {
	Venue
	Log *log.Logger

	mu        sync.Mutex
	orders    map[string]Order
	withdraws map[string]Withdrawal
	transfers map[string]Transfer
}

// NewDryRun wraps v.
func NewDryRun(v Venue, logger *log.Logger) *DryRun {
	if logger == nil {
		logger = log.Default()
	}
	return &DryRun{Venue: v, Log: logger, orders: map[string]Order{}, withdraws: map[string]Withdrawal{}, transfers: map[string]Transfer{}}
}

func (d *DryRun) Mode() Mode { return ModeDryRun }

func (d *DryRun) PlaceOrder(_ context.Context, req OrderRequest) (Order, error) {
	if req.ClientID == "" {
		return Order{}, d.rejected("place_order", "the order has no client id")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.orders[req.ClientID]; ok {
		return Order{}, &Error{Venue: d.Name(), Op: "place_order", Kind: KindRejected, Code: CodeDuplicate,
			Err: fmt.Errorf("the client id %s is taken", req.ClientID)}
	}
	o := Order{ClientID: req.ClientID, VenueID: "dry-run-" + req.ClientID, Symbol: req.Symbol, Side: req.Side, Type: req.Type,
		Status: OrderNew, Quantity: req.Quantity, Price: req.Price, UpdatedAt: time.Now().UTC(), DryRun: true}
	d.orders[req.ClientID] = o
	d.Log.Printf("venue %s (dry run): would place order %s: %s %s %s at %s", d.Name(), req.ClientID, req.Side, req.Quantity, req.Symbol, req.Price)
	return o, nil
}

func (d *DryRun) CancelOrder(ctx context.Context, symbol, clientID string) (Order, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	o, ok := d.orders[clientID]
	if !ok {
		return Order{}, &Error{Venue: d.Name(), Op: "cancel_order", Kind: KindRejected, Err: ErrNotFound}
	}
	if !o.Status.Done() {
		o.Status, o.UpdatedAt = OrderCancelled, time.Now().UTC()
		d.orders[clientID] = o
		d.Log.Printf("venue %s (dry run): would cancel order %s", d.Name(), clientID)
	}
	return o, nil
}

func (d *DryRun) GetOrder(ctx context.Context, symbol, clientID string) (Order, error) {
	d.mu.Lock()
	o, ok := d.orders[clientID]
	d.mu.Unlock()
	if !ok {
		return Order{}, ErrNotFound
	}
	return o, nil
}

func (d *DryRun) Withdraw(_ context.Context, req WithdrawRequest) (Withdrawal, error) {
	if !d.Supports(CapabilityWithdraw) {
		return Withdrawal{}, ErrUnsupported
	}
	if req.ClientID == "" {
		return Withdrawal{}, d.rejected("withdraw", "the withdrawal has no client id")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if w, ok := d.withdraws[req.ClientID]; ok {
		return w, nil
	}
	w := Withdrawal{ClientID: req.ClientID, VenueID: "dry-run-" + req.ClientID, Asset: req.Asset, Network: req.Network,
		Amount: req.Amount, Status: TransferPending, DryRun: true}
	d.withdraws[req.ClientID] = w
	// The address is the user's: it is not logged.
	d.Log.Printf("venue %s (dry run): would withdraw %s %s on %s (%s)", d.Name(), req.Amount, req.Asset, req.Network, req.ClientID)
	return w, nil
}

func (d *DryRun) GetWithdrawal(_ context.Context, clientID string) (Withdrawal, error) {
	d.mu.Lock()
	w, ok := d.withdraws[clientID]
	d.mu.Unlock()
	if !ok {
		return Withdrawal{}, ErrNotFound
	}
	return w, nil
}

func (d *DryRun) InternalTransfer(_ context.Context, req TransferRequest) (Transfer, error) {
	if !d.Supports(CapabilityInternalTransfer) {
		return Transfer{}, ErrUnsupported
	}
	if req.ClientID == "" {
		return Transfer{}, d.rejected("internal_transfer", "the transfer has no client id")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if t, ok := d.transfers[req.ClientID]; ok {
		return t, nil
	}
	t := Transfer{ClientID: req.ClientID, VenueID: "dry-run-" + req.ClientID, Asset: req.Asset, Amount: req.Amount,
		Status: TransferCompleted, DryRun: true}
	d.transfers[req.ClientID] = t
	d.Log.Printf("venue %s (dry run): would transfer %s %s from %q to %q (%s)", d.Name(), req.Amount, req.Asset, req.From, req.To, req.ClientID)
	return t, nil
}

func (d *DryRun) GetTransfer(_ context.Context, clientID string) (Transfer, error) {
	d.mu.Lock()
	t, ok := d.transfers[clientID]
	d.mu.Unlock()
	if !ok {
		return Transfer{}, ErrNotFound
	}
	return t, nil
}

func (d *DryRun) rejected(op, msg string) error {
	return &Error{Venue: d.Name(), Op: op, Kind: KindRejected, Err: fmt.Errorf("%s", msg)}
}
