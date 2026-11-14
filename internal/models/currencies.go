package models

import "time"

// CurrencyInfo defines the structure of the `info` field in the Currency model
type CurrencyInfo struct {
	EnName   string `json:"en_name"`
	FaName   string `json:"fa_name"`
	IfFiat   bool   `json:"if_fiat"`
	Symbol   string `json:"symbol"`
	IsToken  bool   `json:"is_token"`
	Contract string `json:"contract"`
	// LogoURL is the https URL of the currency's logo (#215), or empty.
	LogoURL string `json:"logo_url,omitempty" example:"https://example.com/logos/usdt.svg"`
}

// Currency defines the structure of the currency model
type Currency struct {
	ID     int          `json:"id"`
	HK     string       `json:"hk"`
	Status string       `json:"status"`
	Info   CurrencyInfo `json:"info"`
	// Version counts the changes of the currency. Only administrators see
	// it; it is 0, and left out, in public responses.
	Version   int64   `json:"version,omitempty"`
	CreatedAt string  `json:"created_at"`
	UpdatedAt string  `json:"updated_at"`
	DeletedAt *string `json:"deleted_at,omitempty"`
	// Lifecycle is where the currency is in a delisting, and what users may
	// do with it now (#216).
	Lifecycle *CurrencyLifecycle `json:"lifecycle,omitempty"`
	// PreviousSymbols are the symbols the currency had before a rebrand,
	// newest first (#215). They stay reserved for it, and the lists find the
	// currency by them.
	PreviousSymbols []string `json:"previous_symbols,omitempty" example:"MATIC"`
	// Migration is the migration that moves the holders of this currency to
	// another one, from its announcement on (#215).
	Migration *CurrencyMigration `json:"migration,omitempty"`
	// Networks are the networks the currency moves on, in the public list
	// (#57).
	Networks []CurrencyNetwork `json:"networks,omitempty"`
}

// CurrencyNetwork is how a currency moves on one network, as users see it
// (#57): whether it can be deposited and withdrawn there, the limits, and
// the fee. Amounts are decimal strings.
type CurrencyNetwork struct {
	// Network names the chain, such as "tron" or "ethereum", or "internal"
	// for a fiat currency.
	Network string `json:"network" example:"tron"`
	// ContractAddress is the token's contract on the network, empty for the
	// native asset.
	ContractAddress   string `json:"contract_address,omitempty" example:"TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t"`
	Decimals          int    `json:"decimals" example:"6"`
	DepositEnabled    bool   `json:"deposit_enabled" example:"true"`
	WithdrawalEnabled bool   `json:"withdrawal_enabled" example:"true"`
	MinDeposit        string `json:"min_deposit" example:"1"`
	MinWithdrawal     string `json:"min_withdrawal" example:"10"`
	// MaxWithdrawal of "0" is no maximum.
	MaxWithdrawal string `json:"max_withdrawal" example:"0"`
	WithdrawalFee string `json:"withdrawal_fee" example:"1"`
	// FeeCurrency is the symbol of the currency that pays the network fee,
	// such as TRX for a TRC20 token.
	FeeCurrency string `json:"fee_currency,omitempty" example:"TRX"`
	// RequiresTag says that a deposit names its account with a tag or
	// memo.
	RequiresTag bool   `json:"requires_tag" example:"false"`
	ExplorerURL string `json:"explorer_url,omitempty" example:"https://tronscan.org"`
}

// CurrencyMigration is a migration of a currency into another, as its
// source sees it.
type CurrencyMigration struct {
	ID       string `json:"id" format:"uuid"`
	TargetHK string `json:"target_hk" format:"uuid"`
	// Phase is announced, deposits_paused, trading_halted, converting, or
	// completed.
	Phase string `json:"phase" example:"announced" enums:"announced,deposits_paused,trading_halted,converting,completed"`
	// Forced is true when holdings are converted, and false when the
	// conversion is offered and the currency stays.
	Forced bool `json:"forced"`
	// Ratio is the units of the target that one unit of this currency
	// becomes, as a decimal string.
	Ratio string `json:"ratio" example:"1"`
	// Next is the next scheduled phase.
	Next *CurrencyMigrationPhase `json:"next,omitempty"`
}

// CurrencyMigrationPhase is a scheduled phase of a migration.
type CurrencyMigrationPhase struct {
	Phase string    `json:"phase" example:"deposits_paused"`
	At    time.Time `json:"at"`
}

// CurrencyLifecycle is the lifecycle state of a currency and the
// capabilities that follow from it.
type CurrencyLifecycle struct {
	// State is active, or the step of a delisting that the currency is at.
	State string `json:"state" example:"active" enums:"active,delist_announced,deposits_disabled,sell_only,trading_halted,withdrawal_only,frozen,delisted"`
	// Capabilities says what users may do with the currency now: the
	// capabilities of the state, with the overrides of administrators.
	Capabilities CurrencyCapabilities `json:"capabilities"`
	// Next is the next scheduled step, when a delisting is under way.
	Next *CurrencyLifecycleStep `json:"next,omitempty"`
}

// CurrencyCapabilities are the operations that users may do with a
// currency. Every flow that moves the currency checks them.
type CurrencyCapabilities struct {
	Deposit     bool `json:"deposit"`
	Withdraw    bool `json:"withdraw"`
	Buy         bool `json:"buy"`
	Sell        bool `json:"sell"`
	ConvertInto bool `json:"convert_into"`
	ConvertFrom bool `json:"convert_from"`
	// Listed is false when the currency is left out of the public lists. It
	// can still be read by its hk, for history.
	Listed bool `json:"listed"`
}

// CurrencyLifecycleStep is a scheduled step of a delisting.
type CurrencyLifecycleStep struct {
	State string    `json:"state" example:"deposits_disabled"`
	At    time.Time `json:"at"`
}

// Currency statuses. Only approved currencies are public.
const (
	CurrencyStatusApproved = "approved"
	CurrencyStatusPending  = "pending"
	CurrencyStatusSuspend  = "suspend"
	CurrencyStatusDeleted  = "deleted"
)

// IsPublic reports whether the currency may be shown to clients that are not
// administrators: in public and user responses, and in WebSocket events.
func (c Currency) IsPublic() bool {
	return c.Status == CurrencyStatusApproved && c.DeletedAt == nil
}
