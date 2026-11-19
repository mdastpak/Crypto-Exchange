package deposits

import (
	"context"
	"database/sql"
	"fmt"
	"math/big"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mdastpak/crypto-exchange/internal/aml"
	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
	"github.com/mdastpak/crypto-exchange/internal/ledger"
	"github.com/mdastpak/crypto-exchange/internal/restrictions"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"
)

const (
	usdtContract = "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t"
	stranger     = "TLa2f6VPqDgRE67v1736s7bJ8Ray5wYjU7"
	hotWallet    = "TNPeeaaFB7K9cmo4uQpcU32zGK8G1NYqeL"
)

// fakeChain is a chain that a test builds block by block, and
// reorganizes.
type fakeChain struct {
	mu     sync.Mutex
	blocks []Block
	fork   int
}

func newChain() *fakeChain {
	c := &fakeChain{}
	c.blocks = []Block{{Number: 0, ID: "b0"}}
	return c
}

// add appends a block with the transfers, and returns its number.
func (c *fakeChain) add(transfers ...Transfer) int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	last := c.blocks[len(c.blocks)-1]
	n := last.Number + 1
	c.blocks = append(c.blocks, Block{Number: n, ID: fmt.Sprintf("b%d-%d", n, c.fork), ParentID: last.ID, Transfers: transfers})
	return n
}

func (c *fakeChain) empty(n int) {
	for range n {
		c.add()
	}
}

// reorg drops the blocks after number: the next ones are another fork.
func (c *fakeChain) reorg(number int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.blocks = c.blocks[:number+1]
	c.fork++
}

func (c *fakeChain) Head(context.Context) (Block, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	b := c.blocks[len(c.blocks)-1]
	b.Transfers = nil
	return b, nil
}

func (c *fakeChain) Block(_ context.Context, n int64) (Block, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if n < 0 || n >= int64(len(c.blocks)) {
		return Block{}, fmt.Errorf("block %d not found", n)
	}
	return c.blocks[n], nil
}

func (c *fakeChain) TxBlock(_ context.Context, txID string) (int64, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, b := range c.blocks {
		for _, t := range b.Transfers {
			if t.TxID == txID {
				return b.Number, true, nil
			}
		}
	}
	return 0, false, nil
}

// fakeAML answers every deposit with action.
type fakeAML struct {
	action   aml.Action
	enforced int
}

func (a *fakeAML) Evaluate(_ context.Context, _ *sql.Tx, ev aml.Event) (aml.Decision, error) {
	return aml.Decision{Event: ev, Action: a.action}, nil
}

func (a *fakeAML) Enforce(context.Context, *sql.DB, aml.Decision, audit.Event) error {
	a.enforced++
	return aml.ErrRefused
}

func (a *fakeAML) Notify(context.Context, []aml.Notice) {}

type watchFixture struct {
	fixture
	chain   *fakeChain
	watcher *Watcher
	alice   string
	address string
}

func newWatch(t *testing.T) watchFixture {
	t.Helper()
	f := newFixture(t)
	w := watchFixture{fixture: f, chain: newChain()}
	w.alice = f.user("alice@example.com")
	a, err := f.Get(context.Background(), w.alice, f.usdt, "tron", audit.Event{})
	require.NoError(t, err)
	w.address = a.Address
	w.watcher = &Watcher{DB: f.DB, Network: "tron", Chain: w.chain, Confirmations: 19, ReorgDepth: 19, Start: 1, MaxBlocks: 100,
		Ignore: map[string]bool{hotWallet: true}}
	return w
}

// usdt is a transfer of amount USDT, with 6 decimals, to the address.
func usdt(tx string, index int, to string, amount int64) Transfer {
	return Transfer{TxID: tx, Index: index, Token: usdtContract, From: stranger, To: to, Amount: big.NewInt(amount * 1_000_000)}
}

func (w watchFixture) run() Result {
	w.t.Helper()
	res, err := w.watcher.Run(context.Background())
	require.NoError(w.t, err)
	return res
}

func (w watchFixture) balance(currency string) (available, pending string) {
	w.t.Helper()
	user, _ := shared.UUIDFromString(w.alice)
	cur, _ := shared.UUIDFromString(currency)
	b, err := ledger.Ledger{DB: w.DB}.Balance(context.Background(), ledger.Account{UserHK: user}, cur)
	require.NoError(w.t, err)
	return b.Available.String(), b.PendingDeposit.String()
}

func (w watchFixture) deposit() Deposit {
	w.t.Helper()
	list, err := List(context.Background(), w.DB, Filter{UserHK: w.alice})
	require.NoError(w.t, err)
	require.Len(w.t, list, 1)
	return list[0]
}

// A deposit is pending in its block, and credited once, after 19
// confirmations (#68).
func TestWatcher_CreditsAfterConfirmations(t *testing.T) {
	w := newWatch(t)
	w.chain.add(usdt("aa01", 0, w.address, 25))
	res := w.run()
	assert.Equal(t, 1, res.Detected)
	d := w.deposit()
	assert.Equal(t, StatusDetected, d.Status)
	assert.Equal(t, "25", d.Amount.String())
	assert.Equal(t, stranger, d.FromAddress)
	available, pending := w.balance(w.usdt)
	assert.Equal(t, "0", available)
	assert.Equal(t, "25", pending)

	w.chain.empty(17) // 18 confirmations
	w.run()
	assert.Equal(t, StatusDetected, w.deposit().Status)
	w.chain.empty(1)
	res = w.run()
	assert.Equal(t, 1, res.Credited)
	d = w.deposit()
	assert.Equal(t, StatusCredited, d.Status)
	require.NotNil(t, d.CreditedAt)
	available, pending = w.balance(w.usdt)
	assert.Equal(t, "25", available)
	assert.Equal(t, "0", pending)

	// Read again, as after a restart: nothing twice.
	_, err := w.DB.Exec(`UPDATE chain_cursors SET block_number = 0, block_id = 'b0'`)
	require.NoError(t, err)
	require.NoError(t, rowseal.Reseal(context.Background(), w.DB, rowseal.ChainCursors, "network = 'tron'"))
	res = w.run()
	assert.Zero(t, res.Detected+res.Credited)
	available, _ = w.balance(w.usdt)
	assert.Equal(t, "25", available)

	mine, err := UserDeposits(context.Background(), w.DB, w.alice)
	require.NoError(t, err)
	require.Len(t, mine, 1)
	assert.False(t, mine[0].OnHold)
}

// Only transfers of a listed asset to a deposit address are deposits: a
// transfer from the hot wallet, as the fee of a sweep, is not.
func TestWatcher_IgnoresOtherTransfers(t *testing.T) {
	w := newWatch(t)
	fromHot := Transfer{TxID: "aa02", Index: -1, From: hotWallet, To: w.address, Amount: big.NewInt(30_000_000)}
	unknown := usdt("aa03", 0, w.address, 5)
	unknown.Token = stranger
	trx := Transfer{TxID: "aa05", Index: -1, From: stranger, To: w.address, Amount: big.NewInt(7_500_000)}
	w.chain.add(fromHot, unknown, usdt("aa04", 0, stranger, 5), trx)
	res := w.run()
	assert.Equal(t, 1, res.Detected)
	d := w.deposit()
	assert.Equal(t, w.trx, d.CurrencyHK, "the native asset")
	assert.Equal(t, "7.5", d.Amount.String())
	assert.Equal(t, -1, d.LogIndex)
}

// A reorg that moves the deposit's transaction to another block credits it
// once, in that block; one that drops it fails it, and it is pending again
// when it comes back (#68).
func TestWatcher_Reorgs(t *testing.T) {
	w := newWatch(t)
	w.chain.empty(3)
	w.chain.add(usdt("bb01", 0, w.address, 10)) // block 4
	w.chain.empty(3)
	w.run()
	assert.Equal(t, int64(4), w.deposit().BlockHeight)

	// Another fork from block 2, where the transaction is in block 5.
	w.chain.reorg(2)
	w.chain.empty(2)
	w.chain.add(usdt("bb01", 0, w.address, 10))
	w.chain.empty(3)
	res := w.run()
	assert.Equal(t, 1, res.Reorgs)
	d := w.deposit()
	assert.Equal(t, int64(5), d.BlockHeight)
	assert.Equal(t, "b5-1", d.BlockHash)
	_, pending := w.balance(w.usdt)
	assert.Equal(t, "10", pending, "detected once")

	w.chain.empty(20)
	w.run()
	assert.Equal(t, StatusCredited, w.deposit().Status)
	available, pending := w.balance(w.usdt)
	assert.Equal(t, "10", available)
	assert.Equal(t, "0", pending)
}

// A deposit whose transaction a reorg drops is failed at confirmation, and
// pending again if the transaction comes back.
func TestWatcher_DroppedByReorg(t *testing.T) {
	w := newWatch(t)
	w.chain.add(usdt("cc01", 0, w.address, 10)) // block 1
	w.chain.empty(5)
	w.run()
	// Another fork from block 0, which is long and has no transaction: the
	// watcher reads it after rewinding, and the deposit is confirmed deep
	// enough to settle, in a block that is no longer canonical.
	w.chain.reorg(0)
	w.chain.empty(25)
	w.watcher.MaxBlocks = 1000
	res := w.run()
	assert.Equal(t, 1, res.Failed)
	assert.Equal(t, StatusFailed, w.deposit().Status)
	_, pending := w.balance(w.usdt)
	assert.Equal(t, "0", pending)

	w.chain.add(usdt("cc01", 0, w.address, 10))
	w.run()
	assert.Equal(t, StatusDetected, w.deposit().Status, "back on chain")
	_, pending = w.balance(w.usdt)
	assert.Equal(t, "10", pending)
	w.chain.empty(18)
	w.run()
	assert.Equal(t, StatusCredited, w.deposit().Status)
	available, _ := w.balance(w.usdt)
	assert.Equal(t, "10", available)
}

// A deposit below the minimum fails with its hold, and support may credit
// it.
func TestWatcher_BelowMinimum(t *testing.T) {
	ctx := context.Background()
	w := newWatch(t)
	_, err := w.DB.Exec(`UPDATE currency_networks SET min_deposit = 10 WHERE network = 'tron' AND currency_hk = $1`, w.usdt)
	require.NoError(t, err)
	require.NoError(t, rowseal.Reseal(ctx, w.DB, rowseal.CurrencyNetworks, "network = 'tron'"))
	w.chain.add(usdt("dd01", 0, w.address, 3))
	res := w.run()
	assert.Equal(t, 1, res.Failed)
	d := w.deposit()
	assert.Equal(t, StatusFailed, d.Status)
	assert.Equal(t, HoldBelowMinimum, d.HoldReason)
	_, pending := w.balance(w.usdt)
	assert.Equal(t, "0", pending)

	credited, err := Credit(ctx, w.DB, d.ID, audit.Event{ActorType: audit.ActorAdmin, ActorID: "00000000-0000-0000-0000-000000000001"})
	require.NoError(t, err)
	assert.Equal(t, StatusCredited, credited.Status)
	assert.Empty(t, credited.HoldReason)
	available, _ := w.balance(w.usdt)
	assert.Equal(t, "3", available)
	_, err = Credit(ctx, w.DB, d.ID, audit.Event{ActorType: audit.ActorAdmin})
	assert.ErrorIs(t, err, ErrNotHeld)
	var n int
	require.NoError(t, w.DB.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action = 'admin.deposit_credit' AND target_id = $1`, d.ID).Scan(&n))
	assert.Equal(t, 1, n)
}

// A confirmed deposit of a user whose deposits are restricted, or of a
// currency whose deposits closed, is held and stays pending until support
// credits it.
func TestWatcher_Holds(t *testing.T) {
	ctx := context.Background()
	w := newWatch(t)
	w.chain.add(usdt("ee01", 0, w.address, 40))
	w.run()
	_, err := restrictions.Create(ctx, w.DB, restrictions.New{UserHK: w.alice, Scope: restrictions.ScopeOperation,
		Operations: []restrictions.Operation{restrictions.OpDeposit}, Reason: restrictions.ReasonAMLReview,
		CreatedBy: "00000000-0000-0000-0000-000000000000"}, audit.Event{ActorType: audit.ActorSystem})
	require.NoError(t, err)
	w.chain.empty(18)
	res := w.run()
	assert.Equal(t, 1, res.Held)
	d := w.deposit()
	assert.Equal(t, StatusConfirmed, d.Status)
	assert.Equal(t, HoldRestricted, d.HoldReason)
	_, pending := w.balance(w.usdt)
	assert.Equal(t, "40", pending)
	mine, err := UserDeposits(ctx, w.DB, w.alice)
	require.NoError(t, err)
	assert.True(t, mine[0].OnHold)
	held, err := List(ctx, w.DB, Filter{Held: true})
	require.NoError(t, err)
	assert.Len(t, held, 1)

	_, err = Credit(ctx, w.DB, d.ID, audit.Event{ActorType: audit.ActorAdmin, ActorID: "00000000-0000-0000-0000-000000000001"})
	require.NoError(t, err)
	available, pending := w.balance(w.usdt)
	assert.Equal(t, "40", available)
	assert.Equal(t, "0", pending)
}

func TestWatcher_DepositsClosed(t *testing.T) {
	w := newWatch(t)
	w.chain.add(usdt("ef01", 0, w.address, 40))
	w.run()
	_, err := w.DB.Exec(`UPDATE currency_networks SET deposit_enabled = false WHERE network = 'tron' AND currency_hk = $1`, w.usdt)
	require.NoError(t, err)
	require.NoError(t, rowseal.Reseal(context.Background(), w.DB, rowseal.CurrencyNetworks, "network = 'tron'"))
	w.chain.empty(18)
	w.run()
	assert.Equal(t, HoldDepositsClosed, w.deposit().HoldReason)
}

// The AML rules hold a deposit that they review or refuse; a refusal is
// recorded first (#68, #73).
func TestWatcher_AML(t *testing.T) {
	for _, action := range []aml.Action{aml.ActionReview, aml.ActionBlock, aml.ActionAlert} {
		t.Run(string(action), func(t *testing.T) {
			w := newWatch(t)
			rules := &fakeAML{action: action}
			w.watcher.AML = rules
			w.chain.add(usdt("ff01", 0, w.address, 40))
			w.chain.empty(18)
			w.run()
			d := w.deposit()
			if action == aml.ActionAlert {
				assert.Equal(t, StatusCredited, d.Status)
				return
			}
			assert.Equal(t, StatusConfirmed, d.Status)
			assert.Equal(t, HoldAML, d.HoldReason)
			if action == aml.ActionBlock {
				assert.Equal(t, 1, rules.enforced)
			}
			var n int
			require.NoError(t, w.DB.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action = 'deposit.held'`).Scan(&n))
			assert.Equal(t, 1, n)
		})
	}
}

// A deposit raised in SQL is refused with its finding, and credits
// nothing; the other deposits are credited (#68).
func TestWatcher_DepositChangedInSQL(t *testing.T) {
	ctx := context.Background()
	w := newWatch(t)
	bob := w.user("bob@example.com")
	b, err := w.Get(ctx, bob, w.usdt, "tron", audit.Event{})
	require.NoError(t, err)
	w.chain.add(usdt("gg01", 0, w.address, 5), usdt("gg02", 0, b.Address, 6))
	w.run()
	_, err = w.DB.Exec(`UPDATE deposits SET amount = 5000 WHERE tx_hash = 'gg01'`)
	require.NoError(t, err)
	w.chain.empty(18)
	_, err = w.watcher.Run(ctx)
	require.ErrorIs(t, err, rowseal.ErrUnverified)
	available, _ := w.balance(w.usdt)
	assert.Equal(t, "0", available)
	var status string
	require.NoError(t, w.DB.QueryRow(`SELECT status::text FROM deposits WHERE tx_hash = 'gg02'`).Scan(&status))
	assert.Equal(t, "CREDITED", status, "the others are credited")
	var findings int
	require.NoError(t, w.DB.QueryRow(`SELECT COUNT(*) FROM integrity_findings WHERE table_name = 'deposits'`).Scan(&findings))
	assert.Positive(t, findings)
	_, err = List(ctx, w.DB, Filter{UserHK: w.alice})
	assert.ErrorIs(t, err, rowseal.ErrUnverified)
}

// A cursor moved in SQL would skip blocks: it is refused.
func TestWatcher_CursorChangedInSQL(t *testing.T) {
	w := newWatch(t)
	w.chain.empty(3)
	w.run()
	_, err := w.DB.Exec(`UPDATE chain_cursors SET block_number = 100`)
	require.NoError(t, err)
	_, err = w.watcher.Run(context.Background())
	require.ErrorIs(t, err, rowseal.ErrUnverified)
}

// Two instances that run at once record each deposit once.
func TestWatcher_Concurrent(t *testing.T) {
	w := newWatch(t)
	for i := range 10 {
		w.chain.add(usdt(fmt.Sprintf("hh%02d", i), 0, w.address, 1))
	}
	w.chain.empty(18)
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			other := *w.watcher
			_, err := other.Run(context.Background())
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	available, pending := w.balance(w.usdt)
	assert.Equal(t, "10", available)
	assert.Equal(t, "0", pending)
}

// A deposit that a reorg dropped is pending again when the chain returns
// to its first fork, in the very block it was first in, and credited once.
// The reorg back is deeper than TRON's finality allows; a watcher that reads
// that far back must still credit the deposit once.
func TestWatcher_BackToTheFirstFork(t *testing.T) {
	w := newWatch(t)
	w.watcher.MaxBlocks = 1000
	w.watcher.ReorgDepth = 40
	w.chain.add(usdt("ii01", 0, w.address, 10)) // b1-0
	w.chain.empty(3)
	w.run()
	w.chain.reorg(0)
	w.chain.empty(25)
	w.run()
	require.Equal(t, StatusFailed, w.deposit().Status)

	// The first fork wins after all.
	w.chain.reorg(0)
	w.chain.mu.Lock()
	w.chain.fork = 0
	w.chain.mu.Unlock()
	w.chain.add(usdt("ii01", 0, w.address, 10))
	w.chain.empty(30)
	w.run()
	d := w.deposit()
	assert.Equal(t, "b1-0", d.BlockHash)
	assert.Equal(t, StatusCredited, d.Status)
	available, pending := w.balance(w.usdt)
	assert.Equal(t, "10", available)
	assert.Equal(t, "0", pending)
}
