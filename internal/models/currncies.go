package models

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
	ID        int          `json:"id"`
	HK        string       `json:"hk"`
	Status    string       `json:"status"`
	Info      CurrencyInfo `json:"info"`
	CreatedAt string       `json:"created_at"`
	UpdatedAt string       `json:"updated_at"`
	DeletedAt *string      `json:"deleted_at,omitempty"`
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
