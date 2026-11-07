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
