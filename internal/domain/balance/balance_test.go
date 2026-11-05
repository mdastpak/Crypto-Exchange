package balance

import (
	"testing"

	"encrypted-db/internal/domain/shared"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBalance_LockAmount(t *testing.T) {
	balance := &Balance{
		ID:             shared.NewUUID(),
		UserID:         shared.NewUUID(),
		AssetID:        shared.NewUUID(),
		Available:      shared.MustNewDecimalFromString("100"),
		Locked:         shared.Decimal{},
		OnChain:        shared.Decimal{},
		PendingDeposit: shared.Decimal{},
		Version:        1,
	}

	// Lock valid amount
	err := balance.LockAmount(shared.MustNewDecimalFromString("30"))
	require.NoError(t, err)
	assert.Equal(t, "70", balance.Available.String())
	assert.Equal(t, "30", balance.Locked.String())
	assert.Equal(t, "100", balance.Total.String())
	assert.Equal(t, 2, balance.Version)

	// Lock more
	err = balance.LockAmount(shared.MustNewDecimalFromString("20"))
	require.NoError(t, err)
	assert.Equal(t, "50", balance.Available.String())
	assert.Equal(t, "50", balance.Locked.String())
}

func TestBalance_LockAmount_Insufficient(t *testing.T) {
	balance := &Balance{
		Available: shared.MustNewDecimalFromString("50"),
		Locked:    shared.Decimal{},
	}

	err := balance.LockAmount(shared.MustNewDecimalFromString("100"))
	assert.Error(t, err)
	assert.Equal(t, ErrInsufficientBalance, err)
}

func TestBalance_LockAmount_Negative(t *testing.T) {
	balance := &Balance{
		Available: shared.MustNewDecimalFromString("100"),
	}

	err := balance.LockAmount(shared.MustNewDecimalFromString("-10"))
	assert.Error(t, err)
	assert.Equal(t, ErrNegativeAmount, err)
}

func TestBalance_UnlockAmount(t *testing.T) {
	balance := &Balance{
		Available: shared.MustNewDecimalFromString("50"),
		Locked:    shared.MustNewDecimalFromString("50"),
		Version:   1,
	}

	err := balance.UnlockAmount(shared.MustNewDecimalFromString("20"))
	require.NoError(t, err)
	assert.Equal(t, "70", balance.Available.String())
	assert.Equal(t, "30", balance.Locked.String())
	assert.Equal(t, "100", balance.Total.String())
	assert.Equal(t, 2, balance.Version)
}

func TestBalance_UnlockAmount_Insufficient(t *testing.T) {
	balance := &Balance{
		Locked: shared.MustNewDecimalFromString("30"),
	}

	err := balance.UnlockAmount(shared.MustNewDecimalFromString("50"))
	assert.Error(t, err)
	assert.Equal(t, ErrInsufficientLocked, err)
}

func TestBalance_UnlockAmount_Negative(t *testing.T) {
	balance := &Balance{
		Locked: shared.MustNewDecimalFromString("50"),
	}

	err := balance.UnlockAmount(shared.MustNewDecimalFromString("-10"))
	assert.Error(t, err)
	assert.Equal(t, ErrNegativeAmount, err)
}

func TestBalance_AddAvailable(t *testing.T) {
	balance := &Balance{
		Available: shared.MustNewDecimalFromString("50"),
		Locked:    shared.MustNewDecimalFromString("50"),
		Version:   1,
	}

	err := balance.AddAvailable(shared.MustNewDecimalFromString("25"))
	require.NoError(t, err)
	assert.Equal(t, "75", balance.Available.String())
	assert.Equal(t, "50", balance.Locked.String())
	assert.Equal(t, "125", balance.Total.String())
	assert.Equal(t, 2, balance.Version)
}

func TestBalance_AddLocked(t *testing.T) {
	balance := &Balance{
		Available: shared.MustNewDecimalFromString("50"),
		Locked:    shared.MustNewDecimalFromString("50"),
		Version:   1,
	}

	err := balance.AddLocked(shared.MustNewDecimalFromString("25"))
	require.NoError(t, err)
	assert.Equal(t, "50", balance.Available.String())
	assert.Equal(t, "75", balance.Locked.String())
	assert.Equal(t, "125", balance.Total.String())
	assert.Equal(t, 2, balance.Version)
}

func TestBalance_AddOnChain(t *testing.T) {
	balance := &Balance{
		Available: shared.MustNewDecimalFromString("50"),
		OnChain:   shared.MustNewDecimalFromString("50"),
		Version:   1,
	}

	err := balance.AddOnChain(shared.MustNewDecimalFromString("25"))
	require.NoError(t, err)
	assert.Equal(t, "50", balance.Available.String())
	assert.Equal(t, "75", balance.OnChain.String())
	assert.Equal(t, "125", balance.Total.String())
	assert.Equal(t, 2, balance.Version)
}

func TestBalance_AddPendingDeposit(t *testing.T) {
	balance := &Balance{
		Available:      shared.MustNewDecimalFromString("50"),
		PendingDeposit: shared.MustNewDecimalFromString("50"),
		Version:        1,
	}

	err := balance.AddPendingDeposit(shared.MustNewDecimalFromString("25"))
	require.NoError(t, err)
	assert.Equal(t, "50", balance.Available.String())
	assert.Equal(t, "75", balance.PendingDeposit.String())
	assert.Equal(t, "125", balance.Total.String())
	assert.Equal(t, 2, balance.Version)
}

func TestBalance_ConfirmDeposit(t *testing.T) {
	balance := &Balance{
		OnChain:        shared.MustNewDecimalFromString("50"),
		PendingDeposit: shared.MustNewDecimalFromString("50"),
		Version:        1,
	}

	err := balance.ConfirmDeposit(shared.MustNewDecimalFromString("20"))
	require.NoError(t, err)
	assert.Equal(t, "30", balance.PendingDeposit.String())
	assert.Equal(t, "70", balance.OnChain.String())
	assert.Equal(t, "100", balance.Total.String())
	assert.Equal(t, 2, balance.Version)
}

func TestBalance_ConfirmDeposit_Insufficient(t *testing.T) {
	balance := &Balance{
		PendingDeposit: shared.MustNewDecimalFromString("10"),
	}

	err := balance.ConfirmDeposit(shared.MustNewDecimalFromString("50"))
	assert.Error(t, err)
	assert.Equal(t, ErrInsufficientPending, err)
}

func TestBalance_SettleWithdrawal(t *testing.T) {
	balance := &Balance{
		OnChain: shared.MustNewDecimalFromString("100"),
		Version: 1,
	}

	err := balance.SettleWithdrawal(shared.MustNewDecimalFromString("30"))
	require.NoError(t, err)
	assert.Equal(t, "70", balance.OnChain.String())
	assert.Equal(t, "70", balance.Total.String())
	assert.Equal(t, 2, balance.Version)
}

func TestBalance_SettleWithdrawal_Insufficient(t *testing.T) {
	balance := &Balance{
		OnChain: shared.MustNewDecimalFromString("50"),
	}

	err := balance.SettleWithdrawal(shared.MustNewDecimalFromString("100"))
	assert.Error(t, err)
	assert.Equal(t, ErrInsufficientOnChain, err)
}

func TestBalance_CanWithdraw(t *testing.T) {
	balance := &Balance{
		Available: shared.MustNewDecimalFromString("100"),
		Locked:    shared.MustNewDecimalFromString("50"),
	}

	assert.True(t, balance.CanWithdraw(shared.MustNewDecimalFromString("50")))
	assert.True(t, balance.CanWithdraw(shared.MustNewDecimalFromString("100")))
	assert.False(t, balance.CanWithdraw(shared.MustNewDecimalFromString("150")))
}

func TestBalance_CanTrade(t *testing.T) {
	balance := &Balance{
		Available: shared.MustNewDecimalFromString("100"),
		Locked:    shared.MustNewDecimalFromString("50"),
	}

	assert.True(t, balance.CanTrade(shared.MustNewDecimalFromString("50")))
	assert.True(t, balance.CanTrade(shared.MustNewDecimalFromString("100")))
	assert.False(t, balance.CanTrade(shared.MustNewDecimalFromString("150")))
}

func TestBalance_TotalCalculation(t *testing.T) {
	balance := &Balance{
		Available:      shared.MustNewDecimalFromString("100"),
		Locked:         shared.MustNewDecimalFromString("50"),
		OnChain:        shared.MustNewDecimalFromString("25"),
		PendingDeposit: shared.MustNewDecimalFromString("10"),
	}

	balance.recalcTotal()
	assert.Equal(t, "185", balance.Total.String())
}

func TestBalance_VersionIncrement(t *testing.T) {
	balance := &Balance{
		Available: shared.MustNewDecimalFromString("100"),
		Version:   1,
	}

	initialVersion := balance.Version
	balance.LockAmount(shared.MustNewDecimalFromString("10"))
	assert.Equal(t, initialVersion+1, balance.Version)

	balance.UnlockAmount(shared.MustNewDecimalFromString("10"))
	assert.Equal(t, initialVersion+2, balance.Version)

	balance.AddAvailable(shared.MustNewDecimalFromString("10"))
	assert.Equal(t, initialVersion+3, balance.Version)
}

func TestBalanceChangeType_String(t *testing.T) {
	assert.Equal(t, "LOCK", BalanceChangeLock.String())
	assert.Equal(t, "UNLOCK", BalanceChangeUnlock.String())
	assert.Equal(t, "TRADE_CREDIT", BalanceChangeTradeCredit.String())
	assert.Equal(t, "TRADE_DEBIT", BalanceChangeTradeDebit.String())
	assert.Equal(t, "FEE", BalanceChangeFee.String())
	assert.Equal(t, "DEPOSIT", BalanceChangeDeposit.String())
	assert.Equal(t, "PENDING_DEPOSIT", BalanceChangePendingDep.String())
	assert.Equal(t, "WITHDRAWAL", BalanceChangeWithdrawal.String())
	assert.Equal(t, "SETTLEMENT", BalanceChangeSettlement.String())
}

func TestBalanceSnapshot(t *testing.T) {
	snapshot := BalanceSnapshot{
		BalanceID:      shared.NewUUID(),
		UserID:         shared.NewUUID(),
		AssetID:        shared.NewUUID(),
		Available:      shared.MustNewDecimalFromString("100"),
		Locked:         shared.MustNewDecimalFromString("50"),
		OnChain:        shared.MustNewDecimalFromString("25"),
		PendingDeposit: shared.MustNewDecimalFromString("10"),
		Total:          shared.MustNewDecimalFromString("185"),
		Version:        5,
		Timestamp:      shared.Timestamp{},
	}

	assert.Equal(t, "185", snapshot.Total.String())
	assert.Equal(t, 5, snapshot.Version)
}

func TestBalanceChangeEvent(t *testing.T) {
	event := BalanceChangeEvent{
		EventID:       shared.NewUUID(),
		BalanceID:     shared.NewUUID(),
		UserID:        shared.NewUUID(),
		AssetID:       shared.NewUUID(),
		ChangeType:    BalanceChangeLock,
		Amount:        shared.MustNewDecimalFromString("50"),
		OldAvailable:  shared.MustNewDecimalFromString("100"),
		NewAvailable:  shared.MustNewDecimalFromString("50"),
		OldLocked:     shared.Decimal{},
		NewLocked:     shared.MustNewDecimalFromString("50"),
		ReferenceID:   func() *shared.UUID { id := shared.NewUUID(); return &id }(),
		ReferenceType: "ORDER",
		Timestamp:     shared.Timestamp{},
	}

	assert.Equal(t, BalanceChangeLock, event.ChangeType)
	assert.Equal(t, "50", event.Amount.String())
	assert.NotNil(t, event.ReferenceID)
}
