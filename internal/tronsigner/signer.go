package tronsigner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"

	"github.com/mdastpak/crypto-exchange/internal/tron"
)

// Node is the chain as the signer uses it: tron.Node.
type Node interface {
	Head(ctx context.Context) (tron.Block, error)
	SolidHead(ctx context.Context) (tron.Block, error)
	SolidReceipt(ctx context.Context, id string) (tron.Receipt, error)
	Balance(ctx context.Context, a tron.Address) (*big.Int, error)
	TokenBalance(ctx context.Context, token, a tron.Address) (*big.Int, error)
	Receipt(ctx context.Context, id string) (tron.Receipt, error)
	Broadcast(ctx context.Context, t tron.Tx) error
}

// Request asks the signer to pay Amount of Asset to To, once by Key.
type Request struct {
	// Key names the payment: the same key never pays twice.
	Key string `json:"key"`
	To  string `json:"to"`
	// Asset is the symbol of an asset of the configuration, or a token's
	// contract: TRX, or TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t for USDT.
	Asset string `json:"asset"`
	// Amount is in the smallest unit of the asset, such as 1500000 for
	// 1.5 USDT.
	Amount string `json:"amount"`
}

// The states of a payment.
const (
	StatusPending     = "pending"
	StatusCompleted   = "completed"
	StatusFailed      = "failed"
	StatusNotReceived = "not_received"
)

// Payment is a payment that the signer signed.
type Payment struct {
	Key       string    `json:"key"`
	TxID      string    `json:"tx_id"`
	Status    string    `json:"status"`
	Reason    string    `json:"reason,omitempty"`
	ExpiresAt time.Time `json:"expires_at"`
}

// record is a payment as the state keeps it.
type record struct {
	Digest     string    `json:"digest"`
	Asset      string    `json:"asset"`
	Amount     string    `json:"amount"`
	Day        string    `json:"day"`
	TxID       string    `json:"tx_id"`
	Tx         string    `json:"tx"`
	Expiration time.Time `json:"expiration"`
	// PreviousTxIDs are the transactions signed for the key before TxID,
	// each expired untaken by the chain's view: one of them taken after
	// all is still the payment (#550).
	PreviousTxIDs []string `json:"previous_tx_ids,omitempty"`
}

// ids are the transactions ever signed for the payment, the newest last.
func (r *record) ids() []string { return append(append([]string{}, r.PreviousTxIDs...), r.TxID) }

type state struct {
	Payments map[string]*record      `json:"payments"`
	Sweeps   map[string]*sweepRecord `json:"sweeps"`
}

// Errors of the signer.
var (
	// ErrRefused is a payment that the signer will not sign: an asset it
	// does not pay, an address that is not one, or a cap.
	ErrRefused = errors.New("tron-signer: the payment is refused")
	// ErrKeyReused is a key of another payment.
	ErrKeyReused = errors.New("tron-signer: the key names another payment")
	// ErrNotFound is a key that names no payment.
	ErrNotFound = errors.New("tron-signer: no such payment")
)

// Signer signs the payments of the hot wallet.
type Signer struct {
	cfg  *Config
	node Node
	key  *secp256k1.PrivateKey
	// deposits is the key of the account of the deposit addresses, which
	// the sweeps sign with.
	deposits *tron.ExtendedPrivateKey
	hot      tron.Address
	xpub     string
	now      func() time.Time

	mu    sync.Mutex
	state state
}

// keyPattern is the shape of a key: the server's idempotency keys.
var keyPattern = regexp.MustCompile(`^[A-Za-z0-9:_-]{1,128}$`)

// New returns the signer of the hot wallet of the seed, with the payments
// of the state file. The seed is not kept: the caller may clear it.
func New(cfg *Config, node Node, seed []byte, now func() time.Time) (*Signer, error) {
	m, err := tron.MasterKey(seed)
	if err != nil {
		return nil, err
	}
	acct, err := m.Account(tron.HotAccount)
	if err != nil {
		return nil, err
	}
	deposits, err := m.Account(tron.DepositAccount)
	if err != nil {
		return nil, err
	}
	key, err := acct.AddressKey(0)
	if err != nil {
		return nil, err
	}
	if now == nil {
		now = time.Now
	}
	s := &Signer{cfg: cfg, node: node, key: key, deposits: deposits, hot: tron.AddressOf(key.PubKey()), xpub: deposits.Public().String(), now: now,
		state: state{Payments: map[string]*record{}, Sweeps: map[string]*sweepRecord{}}}
	raw, err := os.ReadFile(cfg.StatePath)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		if err := json.Unmarshal(raw, &s.state); err != nil {
			return nil, fmt.Errorf("tron-signer: the state %s does not read: %w", cfg.StatePath, err)
		}
		if s.state.Payments == nil {
			s.state.Payments = map[string]*record{}
		}
		if s.state.Sweeps == nil {
			s.state.Sweeps = map[string]*sweepRecord{}
		}
	}
	return s, nil
}

// DepositXPub is the xpub of the account of the deposit addresses,
// m/44'/195'/0', which the server's custody.providers[].xpub is set to.
func (s *Signer) DepositXPub() string { return s.xpub }

// HotWallet is the address that pays.
func (s *Signer) HotWallet() string { return s.hot.String() }

// save writes the state, whole, to a new file that replaces the old one.
func (s *Signer) save() error {
	raw, err := json.Marshal(s.state)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.cfg.StatePath), ".state-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.cfg.StatePath)
}

func digest(r Request) string {
	sum := sha256.Sum256([]byte(r.To + "\x00" + r.Asset + "\x00" + r.Amount))
	return hex.EncodeToString(sum[:])
}

// usedToday is what the payments of the day took of an asset.
func (s *Signer) usedToday(asset, day string) *big.Int {
	used := new(big.Int)
	for _, r := range s.state.Payments {
		if r.Asset == asset && r.Day == day {
			if v, ok := new(big.Int).SetString(r.Amount, 10); ok {
				used.Add(used, v)
			}
		}
	}
	return used
}

// Pay pays a request, once by its key: a request whose transaction the
// network may still take returns that transaction, broadcast again; one
// whose transaction expired untaken is signed again. The payment is
// recorded before it is broadcast, so that a crash between the two leaves
// a payment that the next call finds.
func (s *Signer) Pay(ctx context.Context, r Request) (Payment, error) {
	if !keyPattern.MatchString(r.Key) {
		return Payment{}, fmt.Errorf("%w: the key is not valid", ErrRefused)
	}
	asset, ok := s.cfg.asset(r.Asset)
	if !ok {
		return Payment{}, fmt.Errorf("%w: the hot wallet does not pay %q", ErrRefused, r.Asset)
	}
	to, err := tron.ParseAddress(r.To)
	if err != nil || to == s.hot || (!asset.isNative && to == asset.token) {
		return Payment{}, fmt.Errorf("%w: the destination is not an address it pays", ErrRefused)
	}
	amount, ok := new(big.Int).SetString(r.Amount, 10)
	if !ok || amount.Sign() <= 0 || amount.String() != r.Amount {
		return Payment{}, fmt.Errorf("%w: the amount is not a positive integer", ErrRefused)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now().UTC()
	rec, seen := s.state.Payments[r.Key]
	if seen {
		if rec.Digest != digest(r) {
			return Payment{}, ErrKeyReused
		}
		p, inBlock, err := s.status(ctx, r.Key, rec)
		if err != nil || p.Status != StatusNotReceived {
			// Sent again only while no block has it and the network may
			// still take it.
			if err == nil && p.Status == StatusPending && !inBlock && now.Before(rec.Expiration) {
				err = s.rebroadcast(ctx, rec)
			}
			return p, err
		}
		// Expired untaken by the chain's view: signed again below, in the
		// same day's count, and the transaction of before is kept.
		rec.PreviousTxIDs = append(rec.PreviousTxIDs, rec.TxID)
	} else {
		if amount.Cmp(asset.perOne) > 0 {
			return Payment{}, fmt.Errorf("%w: above the cap of one payment of %s", ErrRefused, r.Asset)
		}
		day := now.Format(time.DateOnly)
		if new(big.Int).Add(s.usedToday(r.Asset, day), amount).Cmp(asset.perDay) > 0 {
			return Payment{}, fmt.Errorf("%w: above the daily cap of %s", ErrRefused, r.Asset)
		}
		rec = &record{Digest: digest(r), Asset: r.Asset, Amount: r.Amount, Day: day}
	}

	head, err := s.node.Head(ctx)
	if err != nil {
		return Payment{}, err
	}
	p := tron.Payment{From: s.hot, To: to, Amount: amount}
	if !asset.isNative {
		p.Token, p.FeeLimit = asset.token, s.cfg.FeeLimitSun
	}
	tx, err := tron.Build(p, tron.RefBlock{Number: head.Number, ID: head.ID}, now)
	if err != nil {
		return Payment{}, fmt.Errorf("%w: %w", ErrRefused, err)
	}
	tx = tx.Sign(s.key)
	previous := rec.TxID
	rec.TxID, rec.Tx, rec.Expiration = tx.ID(), hex.EncodeToString(tx.Encode()), tx.Expiration
	s.state.Payments[r.Key] = rec
	if err := s.save(); err != nil {
		if !seen {
			delete(s.state.Payments, r.Key)
		} else {
			// Not recorded: the payment stays the transaction of before.
			rec.TxID = previous
			rec.PreviousTxIDs = rec.PreviousTxIDs[:len(rec.PreviousTxIDs)-1]
		}
		return Payment{}, fmt.Errorf("tron-signer: record the payment: %w", err)
	}
	if err := s.node.Broadcast(ctx, tx); err != nil {
		// Recorded: the next call finds it, and sends it again while the
		// network may take it, or knows that it never was.
		return Payment{Key: r.Key, TxID: rec.TxID, Status: StatusPending, ExpiresAt: rec.Expiration}, err
	}
	return Payment{Key: r.Key, TxID: rec.TxID, Status: StatusPending, ExpiresAt: rec.Expiration}, nil
}

func (s *Signer) rebroadcast(ctx context.Context, rec *record) error {
	raw, err := hex.DecodeString(rec.Tx)
	if err != nil {
		return err
	}
	tx, err := tron.DecodeTx(raw)
	if err != nil {
		return err
	}
	return s.node.Broadcast(ctx, tx)
}

// Status returns where a payment is.
func (s *Signer) Status(ctx context.Context, key string) (Payment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.state.Payments[key]
	if !ok {
		return Payment{}, ErrNotFound
	}
	p, _, err := s.status(ctx, key, rec)
	return p, err
}

// status tells where a payment is, by the chain's view of every
// transaction signed for it, and whether a block has one.
func (s *Signer) status(ctx context.Context, key string, rec *record) (Payment, bool, error) {
	p := Payment{Key: key, TxID: rec.TxID, Status: StatusPending, ExpiresAt: rec.Expiration}
	v, err := s.settle(ctx, rec.ids(), rec.Expiration)
	if err != nil {
		return p, false, err
	}
	switch v.state {
	case txFinal:
		p.Status, p.TxID = StatusCompleted, v.id
	case txFailed:
		p.Status, p.TxID, p.Reason = StatusFailed, v.id, "the transaction failed on chain: "+v.reason
	case txExpired:
		p.Status = StatusNotReceived
	}
	return p, v.inBlock, nil
}
