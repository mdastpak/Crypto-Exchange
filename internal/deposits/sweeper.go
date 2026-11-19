package deposits

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/mdastpak/crypto-exchange/internal/currencies"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"
)

// SweepState is where a sweep is, as the signer tells it.
type SweepState struct {
	// Status is pending, completed, or failed.
	Status string
	// Address is the deposit address that the signer swept: the one the
	// server derived, or the sweep is refused.
	Address   string
	Amount    string
	To        string
	TopUpTxID string
	TxID      string
	Reason    string
}

// The states of a sweep.
const (
	SweepPending   = "pending"
	SweepCompleted = "completed"
	SweepFailed    = "failed"
)

// ErrSweepRefused is a sweep that the signer refused for good.
var ErrSweepRefused = errors.New("deposits: the signer refused the sweep")

// SweepSigner takes a sweep a step further, once by its key: the TRON
// signer.
type SweepSigner interface {
	Sweep(ctx context.Context, key string, index uint32, asset string) (SweepState, error)
}

// Sweeper sweeps the credited deposits of a network from the deposit
// addresses to the custody's hot wallet, or to its cold storage, by the
// signer (#68). Every instance may run it: an address has one pending
// sweep at a time, which the database holds, and the signer takes each
// sweep once by its key.
type Sweeper struct {
	DB      *sql.DB
	Network string
	Signer  SweepSigner
	// RetryAfter is how long an address waits after a failed sweep of an
	// asset, so that a sweep that keeps failing does not burn its fees at
	// every pass.
	RetryAfter time.Duration
	// MaxPerRun bounds the sweeps that a run starts and steps.
	MaxPerRun int
}

// SweepResult counts what a run did.
type SweepResult struct {
	Started   int
	Completed int
	Failed    int
}

// Sweep is a sweep as an administrator sees it.
type Sweep struct {
	ID         string    `json:"id"`
	AddressID  string    `json:"deposit_address_id"`
	CurrencyHK string    `json:"currency_hk"`
	Key        string    `json:"key"`
	Status     string    `json:"status" example:"completed"`
	Amount     string    `json:"amount,omitempty" example:"25000000"`
	To         string    `json:"to,omitempty"`
	TopUpTxID  string    `json:"top_up_tx_id,omitempty"`
	TxID       string    `json:"tx_id,omitempty"`
	Reason     string    `json:"reason,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

// Run steps the pending sweeps, and then starts one for each address with
// deposits credited since its last completed sweep of the asset.
func (s *Sweeper) Run(ctx context.Context) (SweepResult, error) {
	var res SweepResult
	if err := s.stepPending(ctx, &res); err != nil {
		return res, err
	}
	started, err := s.start(ctx)
	res.Started = started
	if err != nil || started == 0 {
		return res, err
	}
	// Step the new ones at once: a sweep with nothing to move completes.
	return res, s.stepPending(ctx, &res)
}

func (s *Sweeper) limit() int {
	if s.MaxPerRun > 0 {
		return s.MaxPerRun
	}
	return 100
}

// start records a sweep for each address that is due: one per address,
// its tokens before its TRX, and none for an asset whose sweep failed
// within RetryAfter.
func (s *Sweeper) start(ctx context.Context) (int, error) {
	retry := s.RetryAfter
	if retry <= 0 {
		retry = time.Hour
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT DISTINCT ON (a.id) a.id::text, d.currency_hk::text
		FROM deposits d
		JOIN deposit_addresses a ON a.network = d.network AND a.address = d.address
		JOIN currency_networks n ON n.currency_hk = d.currency_hk AND n.network = d.network
		WHERE d.network = $1 AND d.status = 'CREDITED'
			AND d.credited_at > COALESCE((SELECT max(w.created_at) FROM sweeps w WHERE w.deposit_address_id = a.id
				AND w.currency_hk = d.currency_hk AND w.status = 'completed'), '-infinity')
			AND NOT EXISTS (SELECT 1 FROM sweeps w WHERE w.deposit_address_id = a.id AND w.status = 'pending')
			AND NOT EXISTS (SELECT 1 FROM sweeps w WHERE w.deposit_address_id = a.id AND w.currency_hk = d.currency_hk
				AND w.status = 'failed' AND w.updated_at > NOW() - make_interval(secs => $2))
		ORDER BY a.id, COALESCE(n.contract_address, '') = '', d.currency_hk
		LIMIT $3`, s.Network, retry.Seconds(), s.limit())
	if err != nil {
		return 0, err
	}
	type due struct{ address, currency string }
	var list []due
	for rows.Next() {
		var d due
		if err := rows.Scan(&d.address, &d.currency); err != nil {
			rows.Close()
			return 0, err
		}
		list = append(list, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	n := 0
	for _, d := range list {
		id := uuid.NewString()
		tx, err := s.DB.BeginTx(ctx, nil)
		if err != nil {
			return n, err
		}
		r, err := tx.ExecContext(ctx, `INSERT INTO sweeps (id, deposit_address_id, currency_hk, sweep_key, status)
			VALUES ($1, $2, $3, $4, 'pending') ON CONFLICT (deposit_address_id) WHERE status = 'pending' DO NOTHING`,
			id, d.address, d.currency, "sweep-"+id)
		if err == nil {
			if inserted, _ := r.RowsAffected(); inserted == 1 {
				err = rowseal.Reseal(ctx, tx, rowseal.Sweeps, "id = $1", id)
				n++
			}
		}
		if err == nil {
			err = tx.Commit()
		}
		_ = tx.Rollback()
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

// readSweeps reads the sweeps that match where, each checked against its
// seal.
func readSweeps(ctx context.Context, q querier, where string, args ...any) ([]Sweep, error) {
	t := rowseal.Sweeps
	rows, err := q.QueryContext(ctx, `SELECT `+t.Fields("s")+`, COALESCE(s.reason, '') FROM sweeps s WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Sweep{}
	for rows.Next() {
		r := t.NewRow()
		var reason string
		if err := rows.Scan(append(r.Dest(), &reason)...); err != nil {
			return nil, err
		}
		if err := rowseal.Require(ctx, t, r); err != nil {
			return nil, err
		}
		v := func(name string) string { return r.Values[t.Index(name)].String }
		created, err := time.Parse(timeLayout, v("created_at"))
		if err != nil {
			return nil, err
		}
		out = append(out, Sweep{ID: r.Key, AddressID: v("deposit_address_id"), CurrencyHK: v("currency_hk"), Key: v("sweep_key"),
			Status: v("status"), Amount: v("amount"), To: v("to_address"), TopUpTxID: v("top_up_tx_id"), TxID: v("tx_id"),
			Reason: reason, CreatedAt: created.UTC()})
	}
	return out, rows.Err()
}

// addressOf reads a deposit address, checked against its seal: the index
// that the signer derives its key by, and the address it must sweep.
func addressOf(ctx context.Context, q querier, id string) (address string, index uint32, err error) {
	t := rowseal.DepositAddresses
	r := t.NewRow()
	if err := q.QueryRowContext(ctx, `SELECT `+t.Fields("")+` FROM deposit_addresses WHERE id = $1`, id).Scan(r.Dest()...); err != nil {
		return "", 0, err
	}
	if err := rowseal.Require(ctx, t, r); err != nil {
		return "", 0, err
	}
	i, err := strconv.ParseUint(r.Values[t.Index("index")].String, 10, 31)
	if err != nil {
		return "", 0, err
	}
	return r.Values[t.Index("address")].String, uint32(i), nil
}

// assetOf names a currency of the network to the signer: a token by its
// contract, and the native asset by TRX.
func (s *Sweeper) assetOf(ctx context.Context, currencyHK string) (string, error) {
	// Checked against its seal: a contract changed in SQL would have the
	// signer sweep another asset.
	n, err := currencies.NetworkOf(ctx, s.DB, currencyHK, s.Network, false)
	if err != nil {
		return "", err
	}
	if n.ContractAddress != "" {
		return n.ContractAddress, nil
	}
	return "TRX", nil
}

// stepPending takes each pending sweep of the network a step further. The
// signer is asked between transactions; the answer is recorded on the
// sweep, if it is still pending.
func (s *Sweeper) stepPending(ctx context.Context, res *SweepResult) error {
	pending, err := readSweeps(ctx, s.DB, `s.status = 'pending' AND s.deposit_address_id IN
		(SELECT id FROM deposit_addresses WHERE network = $1) ORDER BY s.created_at LIMIT $2`, s.Network, s.limit())
	if err != nil {
		return err
	}
	for _, sw := range pending {
		if err := s.step(ctx, sw, res); err != nil {
			log.Printf("deposits: sweep %s of %s not stepped: %v", sw.ID, s.Network, err)
		}
	}
	return nil
}

func (s *Sweeper) step(ctx context.Context, sw Sweep, res *SweepResult) error {
	address, index, err := addressOf(ctx, s.DB, sw.AddressID)
	if err != nil {
		return err
	}
	asset, err := s.assetOf(ctx, sw.CurrencyHK)
	if err != nil {
		return err
	}
	state, err := s.Signer.Sweep(ctx, sw.Key, index, asset)
	switch {
	case errors.Is(err, ErrSweepRefused):
		state = SweepState{Status: SweepFailed, Reason: err.Error()}
	case err != nil:
		return err
	case state.Address != address:
		// The signer derived another address than the server's xpub:
		// they hold different seeds, and nothing more is swept.
		log.Printf("SECURITY: the signer swept %s for deposit address %s (index %d) of %s", state.Address, address, index, s.Network)
		state = SweepState{Status: SweepFailed, Reason: "the signer's address of the index is not the deposit address"}
	}
	if state.Status == SweepPending && state.TxID == sw.TxID && state.TopUpTxID == sw.TopUpTxID {
		return nil
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	locked, err := readSweeps(ctx, tx, `s.id = $1 FOR UPDATE`, sw.ID)
	if err != nil {
		return err
	}
	if len(locked) == 0 || locked[0].Status != SweepPending {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sweeps SET status = $2, amount = NULLIF($3, ''), to_address = NULLIF($4, ''),
			top_up_tx_id = NULLIF($5, ''), tx_id = NULLIF($6, ''), reason = NULLIF(left($7, 500), ''), updated_at = NOW()
		WHERE id = $1`, sw.ID, state.Status, state.Amount, state.To, state.TopUpTxID, state.TxID, state.Reason); err != nil {
		return fmt.Errorf("record the sweep: %w", err)
	}
	if err := rowseal.Reseal(ctx, tx, rowseal.Sweeps, "id = $1", sw.ID); err != nil {
		return err
	}
	switch state.Status {
	case SweepCompleted:
		res.Completed++
	case SweepFailed:
		res.Failed++
		log.Printf("deposits: sweep %s of %s failed: %s", sw.ID, address, state.Reason)
	}
	return tx.Commit()
}

// Sweeps returns the newest sweeps, at most MaxListed, of a status or of
// any, for an administrator.
func Sweeps(ctx context.Context, db *sql.DB, status string) ([]Sweep, error) {
	return readSweeps(ctx, db, `($1 = '' OR s.status = $1) ORDER BY s.created_at DESC, s.id LIMIT `+strconv.Itoa(MaxListed), status)
}
