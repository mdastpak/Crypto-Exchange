package balance

import (
	"encoding/json"
	"math/rand"
	"strconv"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/domain/shared"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func dec(s string) shared.Decimal { return shared.MustNewDecimalFromString(s) }

// newBalance returns a balance with the given components and a consistent
// total.
func newBalance(available, locked, pending string) *Balance {
	b := &Balance{
		ID: shared.NewUUID(), UserID: shared.NewUUID(), CurrencyHK: shared.NewUUID(),
		Available: dec(available), Locked: dec(locked), PendingDeposit: dec(pending), Version: 1,
	}
	b.Total = b.Available.Add(b.Locked)
	return b
}

func components(b *Balance) [4]string {
	return [4]string{b.Available.String(), b.Locked.String(), b.PendingDeposit.String(), b.Total.String()}
}

// TestApply_Transitions applies each transition to a balance of 100
// available, 50 locked, and 30 pending, and checks the components and the
// total afterwards: [available, locked, pending, total].
func TestApply_Transitions(t *testing.T) {
	for change, want := range map[BalanceChangeType][4]string{
		BalanceChangePendingDep:    {"100", "50", "40", "150"},
		BalanceChangeDeposit:       {"110", "50", "20", "160"},
		BalanceChangeDepositFailed: {"100", "50", "20", "150"},
		BalanceChangeLock:          {"90", "60", "30", "150"},
		BalanceChangeUnlock:        {"110", "40", "30", "150"},
		BalanceChangeTradeCredit:   {"110", "50", "30", "160"},
		BalanceChangeTradeDebit:    {"100", "40", "30", "140"},
		BalanceChangeFee:           {"100", "40", "30", "140"},
		BalanceChangeWithdrawal:    {"100", "40", "30", "140"},
	} {
		t.Run(change.String(), func(t *testing.T) {
			b := newBalance("100", "50", "30")
			orderID := shared.NewUUID()
			event, err := b.Apply(change, dec("10"), Reference{ID: &orderID, Type: "ORDER"})
			require.NoError(t, err)

			assert.Equal(t, want, components(b))
			assert.Equal(t, 2, b.Version)
			assert.Equal(t, change, event.ChangeType)
			assert.Equal(t, "10", event.Amount.String())
			assert.Equal(t, [3]string{"100", "50", "30"}, [3]string{event.OldAvailable.String(), event.OldLocked.String(), event.OldPending.String()})
			assert.Equal(t, [3]string{want[0], want[1], want[2]}, [3]string{event.NewAvailable.String(), event.NewLocked.String(), event.NewPending.String()})
			assert.Equal(t, [2]int{1, 2}, [2]int{event.OldVersion, event.NewVersion})
			assert.Equal(t, b.ID, event.BalanceID)
			assert.Equal(t, &orderID, event.ReferenceID)
			assert.Equal(t, "ORDER", event.ReferenceType)
		})
	}
}

func TestApply_RefusesWithoutChangingAnything(t *testing.T) {
	for name, tc := range map[string]struct {
		change BalanceChangeType
		amount string
		want   error
	}{
		"lock more than available":        {BalanceChangeLock, "100.000000000000000001", ErrInsufficientBalance},
		"unlock more than locked":         {BalanceChangeUnlock, "51", ErrInsufficientLocked},
		"trade debit more than locked":    {BalanceChangeTradeDebit, "51", ErrInsufficientLocked},
		"fee more than locked":            {BalanceChangeFee, "51", ErrInsufficientLocked},
		"withdraw more than locked":       {BalanceChangeWithdrawal, "51", ErrInsufficientLocked},
		"credit more than pending":        {BalanceChangeDeposit, "31", ErrInsufficientPending},
		"fail more than pending":          {BalanceChangeDepositFailed, "31", ErrInsufficientPending},
		"zero amount":                     {BalanceChangeTradeCredit, "0", ErrInvalidAmount},
		"negative amount":                 {BalanceChangeLock, "-1", ErrInvalidAmount},
		"negative amount that would grow": {BalanceChangeTradeDebit, "-1", ErrInvalidAmount},
		"unknown transition":              {"SETTLEMENT", "1", ErrUnknownTransition},
	} {
		t.Run(name, func(t *testing.T) {
			b := newBalance("100", "50", "30")
			before := *b

			_, err := b.Apply(tc.change, dec(tc.amount), Reference{})
			assert.ErrorIs(t, err, tc.want)
			assert.Equal(t, before, *b, "a refused transition changes nothing, not even the version")
		})
	}

	t.Run("exactly everything", func(t *testing.T) {
		b := newBalance("100", "50", "30")
		_, err := b.Apply(BalanceChangeLock, dec("100"), Reference{})
		require.NoError(t, err)
		_, err = b.Apply(BalanceChangeWithdrawal, dec("150"), Reference{})
		require.NoError(t, err)
		_, err = b.Apply(BalanceChangeDepositFailed, dec("30"), Reference{})
		require.NoError(t, err)
		assert.Equal(t, [4]string{"0", "0", "0", "0"}, components(b))
	})
}

func TestTotalEffect(t *testing.T) {
	for change, want := range map[BalanceChangeType]int{
		BalanceChangePendingDep: 0, BalanceChangeDeposit: +1, BalanceChangeDepositFailed: 0,
		BalanceChangeLock: 0, BalanceChangeUnlock: 0,
		BalanceChangeTradeCredit: +1, BalanceChangeTradeDebit: -1, BalanceChangeFee: -1, BalanceChangeWithdrawal: -1,
		BalanceChangeTransferOut: -1, BalanceChangeTransferIn: +1,
	} {
		got, ok := change.TotalEffect()
		assert.True(t, ok, change)
		assert.Equal(t, want, got, change)
	}
	assert.Len(t, transitions, 11, "every transition is listed above")
	_, ok := BalanceChangeType("SETTLEMENT").TotalEffect()
	assert.False(t, ok)
}

// TestApply_PropertiesOverRandomSequences applies random transitions with
// random amounts, many of them invalid, and checks after every step that:
//
//   - no component is ever negative;
//   - the total is available plus locked;
//   - the total changes by exactly the amount of the funds that entered or
//     left, and moving funds within the balance conserves it;
//   - a refused transition changes nothing;
//   - the version counts the transitions that were applied.
func TestApply_PropertiesOverRandomSequences(t *testing.T) {
	changes := make([]BalanceChangeType, 0, len(transitions)+1)
	for change := range transitions {
		changes = append(changes, change)
	}
	changes = append(changes, "UNKNOWN")
	appliedPerChange := make(map[BalanceChangeType]int)

	for seed := int64(1); seed <= 300; seed++ {
		rng := rand.New(rand.NewSource(seed))
		b := newBalance("0", "0", "0")
		expectedTotal := dec("0")
		applied := 0

		for step := 0; step < 200; step++ {
			change := changes[rng.Intn(len(changes))]
			// Amounts from -10 to 1000, with up to 18 decimal places, so
			// that both refusals and exact boundaries happen.
			amount := dec(strconv.FormatInt(rng.Int63n(1_010_000)-10_000, 10)).Mul(dec("0.001"))
			if rng.Intn(10) == 0 {
				amount = dec("0." + strconv.FormatInt(rng.Int63n(1e9), 10) + "000000001")
			}
			before := *b

			event, err := b.Apply(change, amount, Reference{})
			if err != nil {
				require.Equal(t, before, *b, "seed %d step %d: a refused %s of %s changed the balance", seed, step, change, amount)
				continue
			}
			applied++
			appliedPerChange[change]++
			effect, _ := change.TotalEffect()
			expectedTotal = expectedTotal.Add(amount.Mul(shared.NewDecimalFromInt64(int64(effect))))

			for name, value := range map[string]shared.Decimal{"available": b.Available, "locked": b.Locked, "pending": b.PendingDeposit} {
				require.False(t, value.IsNegative(), "seed %d step %d: %s is negative after %s of %s", seed, step, name, change, amount)
			}
			require.True(t, b.Total.Equal(b.Available.Add(b.Locked)), "seed %d step %d: total is not available + locked", seed, step)
			require.True(t, b.Total.Equal(expectedTotal), "seed %d step %d: total %s, expected %s", seed, step, b.Total, expectedTotal)
			require.Equal(t, applied+1, b.Version)
			require.True(t, event.NewAvailable.Equal(b.Available) && event.NewLocked.Equal(b.Locked) && event.NewPending.Equal(b.PendingDeposit))
			require.True(t, event.OldAvailable.Equal(before.Available) && event.OldLocked.Equal(before.Locked) && event.OldPending.Equal(before.PendingDeposit))
		}
	}
	for change := range transitions {
		assert.Greater(t, appliedPerChange[change], 500, "the sequences must exercise %s", change)
	}
}

func TestBalance_CanWithdrawAndTrade(t *testing.T) {
	b := newBalance("100", "50", "30")
	for amount, want := range map[string]bool{"100": true, "99.99": true, "100.01": false, "0": false, "-1": false, "150": false} {
		assert.Equal(t, want, b.CanWithdraw(dec(amount)), "withdraw %s", amount)
		assert.Equal(t, want, b.CanTrade(dec(amount)), "trade %s", amount)
	}
}

func TestBalanceChangeType_String(t *testing.T) {
	for change := range transitions {
		assert.Equal(t, string(change), change.String())
	}
}

func TestBalanceChangeEvent_JSON(t *testing.T) {
	b := newBalance("100", "0", "0")
	event, err := b.Apply(BalanceChangeLock, dec("40"), Reference{Type: "WITHDRAWAL"})
	require.NoError(t, err)

	encoded, err := json.Marshal(event)
	require.NoError(t, err)
	var decoded map[string]interface{}
	require.NoError(t, json.Unmarshal(encoded, &decoded))
	assert.Equal(t, "LOCK", decoded["change_type"])
	assert.Equal(t, "40", decoded["amount"], "decimals are encoded as strings")
	assert.Equal(t, "60", decoded["new_available"])
	assert.Equal(t, "40", decoded["new_locked"])
	assert.Equal(t, float64(2), decoded["new_version"])
	assert.NotContains(t, decoded, "old_on_chain", "on-chain holdings are tracked per wallet, not per user")
}
