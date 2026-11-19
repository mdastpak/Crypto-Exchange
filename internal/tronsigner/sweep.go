package tronsigner

import (
	"context"
	"encoding/hex"
	"fmt"
	"math/big"
	"time"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"

	"github.com/mdastpak/crypto-exchange/internal/tron"
)

// SweepRequest asks the signer to sweep an asset from deposit address
// Index, once by Key.
type SweepRequest struct {
	Key string `json:"key"`
	// Index is that of the address in the deposit account: m/44'/195'/0'/0/i.
	Index uint32 `json:"index"`
	// Asset is TRX, or a token by its symbol or its contract.
	Asset string `json:"asset"`
}

// Sweep is where a sweep is.
type Sweep struct {
	Key     string `json:"key"`
	Address string `json:"address"`
	Status  string `json:"status"`
	Reason  string `json:"reason,omitempty"`
	// Amount is what it moved, in the smallest unit, once known; To is the
	// hot wallet or the cold storage.
	Amount string `json:"amount,omitempty"`
	To     string `json:"to,omitempty"`
	// TopUpTxID is the transaction of the TRX that the hot wallet sent the
	// address for the fee of a token sweep, and TxID that of the sweep.
	TopUpTxID string `json:"top_up_tx_id,omitempty"`
	TxID      string `json:"tx_id,omitempty"`
}

// sentTx is a transaction that the signer signed and sent.
type sentTx struct {
	ID         string    `json:"id"`
	Tx         string    `json:"tx"`
	Expiration time.Time `json:"expiration"`
}

type sweepRecord struct {
	Index   uint32  `json:"index"`
	Asset   string  `json:"asset"`
	Address string  `json:"address"`
	Status  string  `json:"status"`
	Reason  string  `json:"reason,omitempty"`
	Amount  string  `json:"amount,omitempty"`
	To      string  `json:"to,omitempty"`
	TopUp   *sentTx `json:"top_up,omitempty"`
	Tx      *sentTx `json:"tx,omitempty"`
}

func (r *sweepRecord) public(key string) Sweep {
	out := Sweep{Key: key, Address: r.Address, Status: r.Status, Reason: r.Reason, Amount: r.Amount, To: r.To}
	if r.TopUp != nil {
		out.TopUpTxID = r.TopUp.ID
	}
	if r.Tx != nil {
		out.TxID = r.Tx.ID
	}
	return out
}

// The states of a sent transaction.
const (
	txPending = iota
	txFinal
	txFailed
	txExpired
)

// track tells where a sent transaction is: pending, final after the
// confirmations, failed on chain, or expired untaken. One that no block
// has is sent again while the network may take it.
func (s *Signer) track(ctx context.Context, t *sentTx) (int, string, error) {
	receipt, err := s.node.Receipt(ctx, t.ID)
	if err != nil {
		return txPending, "", err
	}
	switch {
	case !receipt.Found:
		if s.now().After(t.Expiration.Add(expiryMargin)) {
			return txExpired, "", nil
		}
		if s.now().Before(t.Expiration) {
			raw, err := hex.DecodeString(t.Tx)
			if err != nil {
				return txPending, "", err
			}
			tx, err := tron.DecodeTx(raw)
			if err != nil {
				return txPending, "", err
			}
			if err := s.node.Broadcast(ctx, tx); err != nil {
				return txPending, "", err
			}
		}
		return txPending, "", nil
	case !receipt.Success:
		return txFailed, receipt.Result, nil
	}
	head, err := s.node.Head(ctx)
	if err != nil {
		return txPending, "", err
	}
	if head.Number-receipt.Block+1 >= s.cfg.Confirmations {
		return txFinal, "", nil
	}
	return txPending, "", nil
}

// send signs a payment with key, records it in the state, and broadcasts
// it. It is recorded first, so that a crash leaves a transaction that the
// next call finds.
func (s *Signer) send(ctx context.Context, key *secp256k1.PrivateKey, p tron.Payment, into **sentTx) error {
	head, err := s.node.Head(ctx)
	if err != nil {
		return err
	}
	tx, err := tron.Build(p, tron.RefBlock{Number: head.Number, ID: head.ID}, s.now().UTC())
	if err != nil {
		return err
	}
	tx = tx.Sign(key)
	*into = &sentTx{ID: tx.ID(), Tx: hex.EncodeToString(tx.Encode()), Expiration: tx.Expiration}
	if err := s.save(); err != nil {
		*into = nil
		return fmt.Errorf("tron-signer: record the sweep: %w", err)
	}
	return s.node.Broadcast(ctx, tx)
}

// Sweep moves an asset from a deposit address to the hot wallet, or to the
// cold storage while the hot wallet holds the asset's cap, once by its
// key. Each call takes it a step further and tells where it is:
//
//   - a token sweep first has the hot wallet send the address the TRX of
//     its fee, top_up_sun, when it holds less, and waits for it to be
//     final;
//   - it then sends the whole balance of the asset, a TRX sweep leaving
//     reserve_sun, and waits for it to be final.
//
// A transaction that expired untaken is signed again; one that failed on
// chain fails the sweep, and a new key sweeps again. An address that holds
// nothing of the asset completes with nothing moved.
func (s *Signer) Sweep(ctx context.Context, r SweepRequest) (Sweep, error) {
	if !keyPattern.MatchString(r.Key) {
		return Sweep{}, fmt.Errorf("%w: the key is not valid", ErrRefused)
	}
	if r.Index >= tron.Hardened {
		return Sweep{}, fmt.Errorf("%w: the index is not that of a deposit address", ErrRefused)
	}
	asset, ok := s.cfg.asset(r.Asset)
	if !ok {
		return Sweep{}, fmt.Errorf("%w: the signer does not sweep %q", ErrRefused, r.Asset)
	}
	key, err := s.deposits.AddressKey(r.Index)
	if err != nil {
		return Sweep{}, fmt.Errorf("%w: %w", ErrRefused, err)
	}
	from := tron.AddressOf(key.PubKey())

	s.mu.Lock()
	defer s.mu.Unlock()
	rec, seen := s.state.Sweeps[r.Key]
	if seen && (rec.Index != r.Index || rec.Asset != asset.Symbol) {
		return Sweep{}, ErrKeyReused
	}
	if !seen {
		rec = &sweepRecord{Index: r.Index, Asset: asset.Symbol, Address: from.String(), Status: StatusPending}
		s.state.Sweeps[r.Key] = rec
		if err := s.save(); err != nil {
			delete(s.state.Sweeps, r.Key)
			return Sweep{}, fmt.Errorf("tron-signer: record the sweep: %w", err)
		}
	}
	if rec.Status != StatusPending {
		return rec.public(r.Key), nil
	}
	if err := s.step(ctx, rec, asset, key, from); err != nil {
		return rec.public(r.Key), err
	}
	return rec.public(r.Key), nil
}

// finish ends a sweep, recorded.
func (s *Signer) finish(rec *sweepRecord, status, reason string) error {
	rec.Status, rec.Reason = status, reason
	return s.save()
}

// step takes a pending sweep a step further.
func (s *Signer) step(ctx context.Context, rec *sweepRecord, asset *Asset, key *secp256k1.PrivateKey, from tron.Address) error {
	if rec.Tx != nil {
		state, reason, err := s.track(ctx, rec.Tx)
		switch {
		case err != nil:
			return err
		case state == txFinal:
			return s.finish(rec, StatusCompleted, "")
		case state == txFailed:
			return s.finish(rec, StatusFailed, "the sweep failed on chain: "+reason)
		case state == txPending:
			return nil
		}
		// Expired untaken: built again from the balance below.
		rec.Tx, rec.Amount, rec.To = nil, "", ""
	}
	if rec.TopUp != nil {
		state, reason, err := s.track(ctx, rec.TopUp)
		switch {
		case err != nil:
			return err
		case state == txFailed:
			return s.finish(rec, StatusFailed, "the top-up failed on chain: "+reason)
		case state == txPending:
			return nil
		case state == txExpired:
			rec.TopUp = nil
		}
	}

	trx, err := s.node.Balance(ctx, from)
	if err != nil {
		return err
	}
	var amount *big.Int
	payment := tron.Payment{From: from}
	if asset.isNative {
		amount = new(big.Int).Sub(trx, big.NewInt(s.cfg.Sweeps.ReserveSun))
	} else {
		if amount, err = s.node.TokenBalance(ctx, asset.token, from); err != nil {
			return err
		}
		if amount.Sign() > 0 && trx.Cmp(big.NewInt(s.cfg.Sweeps.TopUpSun)) < 0 {
			if rec.TopUp != nil {
				// Final, and the address still lacks the fee: spent by
				// something else. A person looks.
				return s.finish(rec, StatusFailed, "the address lacks the fee after its top-up")
			}
			need := new(big.Int).Sub(big.NewInt(s.cfg.Sweeps.TopUpSun), trx)
			return s.send(ctx, s.key, tron.Payment{From: s.hot, To: from, Amount: need}, &rec.TopUp)
		}
		payment.Token, payment.FeeLimit = asset.token, s.cfg.FeeLimitSun
	}
	if amount.Sign() <= 0 {
		rec.Amount = "0"
		return s.finish(rec, StatusCompleted, "")
	}
	to, err := s.target(ctx, asset, amount)
	if err != nil {
		return err
	}
	payment.To, payment.Amount = to, amount
	rec.Amount, rec.To = amount.String(), to.String()
	return s.send(ctx, key, payment, &rec.Tx)
}

// target is where a sweep of amount goes: the hot wallet, or the cold
// storage when the hot wallet would hold more than the asset's cap.
func (s *Signer) target(ctx context.Context, asset *Asset, amount *big.Int) (tron.Address, error) {
	if asset.hotCap == nil {
		return s.hot, nil
	}
	var held *big.Int
	var err error
	if asset.isNative {
		held, err = s.node.Balance(ctx, s.hot)
	} else {
		held, err = s.node.TokenBalance(ctx, asset.token, s.hot)
	}
	if err != nil {
		return tron.Address{}, err
	}
	if new(big.Int).Add(held, amount).Cmp(asset.hotCap) > 0 {
		return s.cfg.Sweeps.cold, nil
	}
	return s.hot, nil
}

// SweepStatus returns where a sweep is, without taking it further.
func (s *Signer) SweepStatus(key string) (Sweep, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.state.Sweeps[key]
	if !ok {
		return Sweep{}, ErrNotFound
	}
	return rec.public(key), nil
}
