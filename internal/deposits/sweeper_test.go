package deposits

import (
	"context"
	"math/big"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mdastpak/crypto-exchange/internal/rowseal"
)

type sweepCall struct {
	key   string
	index uint32
	asset string
}

// fakeSigner answers each sweep with the state the test gives the asset,
// for the address the server derived.
type fakeSigner struct {
	mu      sync.Mutex
	calls   []sweepCall
	address func(index uint32) string
	states  map[string]SweepState
	err     error
}

func (f *fakeSigner) Sweep(_ context.Context, key string, index uint32, asset string) (SweepState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, sweepCall{key, index, asset})
	if f.err != nil {
		return SweepState{}, f.err
	}
	st := f.states[asset]
	if st.Address == "" {
		st.Address = f.address(index)
	}
	return st, nil
}

func (f *fakeSigner) set(asset string, st SweepState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.states[asset] = st
}

const txA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// credited credits a USDT and a TRX deposit to alice's address.
func (w watchFixture) credited() {
	w.t.Helper()
	trx := Transfer{TxID: "tt01", Index: -1, From: stranger, To: w.address, Amount: big.NewInt(5_000_000)}
	w.chain.add(usdt("uu01", 0, w.address, 25), trx)
	w.chain.empty(18)
	w.run()
}

func (w watchFixture) sweeper(signer *fakeSigner) *Sweeper {
	return &Sweeper{DB: w.DB, Network: "tron", Signer: signer, RetryAfter: time.Hour}
}

func (w watchFixture) signer() *fakeSigner {
	return &fakeSigner{address: func(uint32) string { return w.address }, states: map[string]SweepState{}}
}

// The credited deposits of an address are swept, its token first, one
// sweep at a time; an address with nothing new is not swept again (#68).
func TestSweeper_SweepsTheCreditedDeposits(t *testing.T) {
	ctx := context.Background()
	w := newWatch(t)
	w.credited()
	signer := w.signer()
	signer.set(usdtContract, SweepState{Status: SweepPending, TopUpTxID: txA})
	s := w.sweeper(signer)

	res, err := s.Run(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, res.Started, "one sweep per address")
	require.Len(t, signer.calls, 1)
	assert.Equal(t, usdtContract, signer.calls[0].asset, "the token first")
	assert.Equal(t, uint32(0), signer.calls[0].index)
	list, err := Sweeps(ctx, w.DB, "")
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, SweepPending, list[0].Status)
	assert.Equal(t, txA, list[0].TopUpTxID)

	res, err = s.Run(ctx)
	require.NoError(t, err)
	assert.Zero(t, res.Started, "the address has a pending sweep")

	signer.set(usdtContract, SweepState{Status: SweepCompleted, TopUpTxID: txA, TxID: txA, Amount: "25000000", To: "hot"})
	signer.set("TRX", SweepState{Status: SweepCompleted, Amount: "4000000", To: "hot"})
	res, err = s.Run(ctx)
	require.NoError(t, err)
	assert.Equal(t, 2, res.Completed, "the token's, then the TRX's, started and completed in the same run")
	assert.Equal(t, 1, res.Started)
	list, err = Sweeps(ctx, w.DB, SweepCompleted)
	require.NoError(t, err)
	assert.Len(t, list, 2)

	calls := len(signer.calls)
	res, err = s.Run(ctx)
	require.NoError(t, err)
	assert.Zero(t, res.Started, "nothing credited since")
	assert.Len(t, signer.calls, calls)

	// A new deposit is swept again.
	w.chain.add(usdt("uu02", 0, w.address, 3))
	w.chain.empty(18)
	w.run()
	res, err = s.Run(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, res.Started)
	assert.Equal(t, usdtContract, signer.calls[len(signer.calls)-1].asset)
}

// A sweep that the signer refuses, or of another address than the
// server's, fails; the asset waits RetryAfter before it is swept again.
func TestSweeper_FailuresWait(t *testing.T) {
	ctx := context.Background()
	w := newWatch(t)
	w.credited()
	signer := w.signer()
	signer.err = ErrSweepRefused
	s := w.sweeper(signer)
	res, err := s.Run(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, res.Failed)
	res, err = s.Run(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, res.Started, "the TRX, as the token waits")
	assert.Equal(t, "TRX", signer.calls[len(signer.calls)-1].asset)

	w2 := newWatch(t)
	w2.credited()
	other := w2.signer()
	other.address = func(uint32) string { return stranger }
	other.set(usdtContract, SweepState{Status: SweepPending})
	res, err = w2.sweeper(other).Run(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, res.Failed, "the signer derived another address")
	list, err := Sweeps(ctx, w2.DB, SweepFailed)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Contains(t, list[0].Reason, "not the deposit address")
}

// A sweep marked completed in SQL is refused with its finding, and the
// address is not swept on it.
func TestSweeper_SweepChangedInSQL(t *testing.T) {
	ctx := context.Background()
	w := newWatch(t)
	w.credited()
	signer := w.signer()
	signer.set(usdtContract, SweepState{Status: SweepPending, TopUpTxID: txA})
	s := w.sweeper(signer)
	_, err := s.Run(ctx)
	require.NoError(t, err)
	_, err = w.DB.Exec(`UPDATE sweeps SET status = 'completed'`)
	require.NoError(t, err)
	_, err = Sweeps(ctx, w.DB, "")
	require.ErrorIs(t, err, rowseal.ErrUnverified)
	var findings int
	require.NoError(t, w.DB.QueryRow(`SELECT COUNT(*) FROM integrity_findings WHERE table_name = 'sweeps'`).Scan(&findings))
	assert.Positive(t, findings)

	// A contract changed in SQL is not swept.
	w2 := newWatch(t)
	w2.credited()
	other := w2.signer()
	other.set(usdtContract, SweepState{Status: SweepPending})
	_, err = w2.DB.Exec(`UPDATE currency_networks SET contract_address = 'TNPeeaaFB7K9cmo4uQpcU32zGK8G1NYqeL' WHERE network = 'tron' AND currency_hk = $1`, w2.usdt)
	require.NoError(t, err)
	_, err = w2.sweeper(other).Run(ctx)
	require.NoError(t, err)
	for _, c := range other.calls {
		assert.NotEqual(t, "TNPeeaaFB7K9cmo4uQpcU32zGK8G1NYqeL", c.asset, "the contract changed in SQL is never asked")
	}
}

// Sweepers that run at once start one sweep per address.
func TestSweeper_Concurrent(t *testing.T) {
	ctx := context.Background()
	w := newWatch(t)
	w.credited()
	signer := w.signer()
	signer.set(usdtContract, SweepState{Status: SweepPending, TopUpTxID: txA})
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := w.sweeper(signer).Run(ctx)
			assert.NoError(t, err)
		}()
	}
	wg.Wait()
	var n int
	require.NoError(t, w.DB.QueryRow(`SELECT COUNT(*) FROM sweeps`).Scan(&n))
	assert.Equal(t, 1, n)
}
