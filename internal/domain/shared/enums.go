package shared

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// UUID is a wrapper around github.com/google/uuid for JSON/db serialization
type UUID uuid.UUID

func NewUUID() UUID {
	return UUID(uuid.New())
}

func UUIDFromString(s string) (UUID, error) {
	u, err := uuid.Parse(s)
	return UUID(u), err
}

func (u UUID) String() string {
	return uuid.UUID(u).String()
}

// IsZero returns true if UUID is zero value
func (u UUID) IsZero() bool {
	return uuid.UUID(u) == uuid.UUID{}
}

func (u UUID) MarshalJSON() ([]byte, error) {
	return json.Marshal(u.String())
}

func (u *UUID) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	parsed, err := UUIDFromString(s)
	if err != nil {
		return err
	}
	*u = parsed
	return nil
}

func (u UUID) Value() (driver.Value, error) {
	return u.String(), nil
}

func (u *UUID) Scan(value interface{}) error {
	if value == nil {
		return nil
	}
	switch v := value.(type) {
	case string:
		parsed, err := UUIDFromString(v)
		*u = parsed
		return err
	case []byte:
		parsed, err := UUIDFromString(string(v))
		*u = parsed
		return err
	default:
		return fmt.Errorf("cannot scan %T into UUID", value)
	}
}

// Decimal wraps shopspring/decimal for precise financial calculations
type Decimal decimal.Decimal

func NewDecimalFromString(s string) (Decimal, error) {
	d, err := decimal.NewFromString(s)
	return Decimal(d), err
}

func MustNewDecimalFromString(s string) Decimal {
	d, err := NewDecimalFromString(s)
	if err != nil {
		panic(err)
	}
	return d
}

func NewDecimalFromInt64(v int64) Decimal {
	return Decimal(decimal.NewFromInt(v))
}

func (d Decimal) Decimal() decimal.Decimal {
	return decimal.Decimal(d)
}

func (d Decimal) String() string {
	return d.Decimal().String()
}

func (d Decimal) MarshalJSON() ([]byte, error) {
	return json.Marshal(d.String())
}

func (d *Decimal) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	parsed, err := NewDecimalFromString(s)
	if err != nil {
		return err
	}
	*d = parsed
	return nil
}

func (d Decimal) Value() (driver.Value, error) {
	return d.String(), nil
}

func (d *Decimal) Scan(value interface{}) error {
	if value == nil {
		return nil
	}
	switch v := value.(type) {
	case string:
		parsed, err := NewDecimalFromString(v)
		*d = parsed
		return err
	case []byte:
		parsed, err := NewDecimalFromString(string(v))
		*d = parsed
		return err
	case int64:
		*d = NewDecimalFromInt64(v)
		return nil
	default:
		return fmt.Errorf("cannot scan %T into Decimal", value)
	}
}

// Timestamp with microsecond precision for financial timestamps
type Timestamp time.Time

func Now() Timestamp {
	return Timestamp(time.Now().UTC())
}

func (t Timestamp) Time() time.Time {
	return time.Time(t)
}

func (t Timestamp) MarshalJSON() ([]byte, error) {
	return json.Marshal(t.Time().UTC().Format(time.RFC3339Nano))
}

func (t *Timestamp) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	parsed, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return err
	}
	*t = Timestamp(parsed.UTC())
	return nil
}

func (t Timestamp) Value() (driver.Value, error) {
	return time.Time(t), nil
}

func (t *Timestamp) Scan(value interface{}) error {
	if value == nil {
		return nil
	}
	switch v := value.(type) {
	case time.Time:
		*t = Timestamp(v.UTC())
		return nil
	case string:
		parsed, err := time.Parse(time.RFC3339Nano, v)
		if err != nil {
			return err
		}
		*t = Timestamp(parsed.UTC())
		return nil
	default:
		return fmt.Errorf("cannot scan %T into Timestamp", value)
	}
}

// Decimal arithmetic methods (delegate to underlying decimal.Decimal)

func (d Decimal) IsZero() bool {
	return d.Decimal().IsZero()
}

func (d Decimal) IsNegative() bool {
	return d.Decimal().IsNegative()
}

func (d Decimal) IsPositive() bool {
	return d.Decimal().IsPositive()
}

func (d Decimal) Sign() int {
	return d.Decimal().Sign()
}

func (d Decimal) Cmp(other Decimal) int {
	return d.Decimal().Cmp(other.Decimal())
}

func (d Decimal) Equal(other Decimal) bool {
	return d.Decimal().Equal(other.Decimal())
}

func (d Decimal) LessThan(other Decimal) bool {
	return d.Decimal().LessThan(other.Decimal())
}

func (d Decimal) LessThanOrEqual(other Decimal) bool {
	return d.Decimal().LessThanOrEqual(other.Decimal())
}

func (d Decimal) GreaterThan(other Decimal) bool {
	return d.Decimal().GreaterThan(other.Decimal())
}

func (d Decimal) GreaterThanOrEqual(other Decimal) bool {
	return d.Decimal().GreaterThanOrEqual(other.Decimal())
}

func (d Decimal) Add(other Decimal) Decimal {
	return Decimal(d.Decimal().Add(other.Decimal()))
}

func (d Decimal) Sub(other Decimal) Decimal {
	return Decimal(d.Decimal().Sub(other.Decimal()))
}

func (d Decimal) Mul(other Decimal) Decimal {
	return Decimal(d.Decimal().Mul(other.Decimal()))
}

func (d Decimal) Div(other Decimal) Decimal {
	return Decimal(d.Decimal().Div(other.Decimal()))
}

func (d Decimal) QuoRem(other Decimal) (Decimal, Decimal) {
	q, r := d.Decimal().QuoRem(other.Decimal(), 0)
	return Decimal(q), Decimal(r)
}

func (d Decimal) Pow(n Decimal) Decimal {
	return Decimal(d.Decimal().Pow(n.Decimal()))
}

func (d Decimal) Abs() Decimal {
	return Decimal(d.Decimal().Abs())
}

func (d Decimal) Neg() Decimal {
	return Decimal(d.Decimal().Neg())
}

func (d Decimal) Round(places int32) Decimal {
	return Decimal(d.Decimal().Round(places))
}

func (d Decimal) Truncate(places int32) Decimal {
	return Decimal(d.Decimal().Truncate(places))
}

func (d Decimal) Ceil() Decimal {
	return Decimal(d.Decimal().Ceil())
}

func (d Decimal) Floor() Decimal {
	return Decimal(d.Decimal().Floor())
}

func (d Decimal) InexactFloat64() float64 {
	f, _ := d.Decimal().Float64()
	return f
}

// AssetType represents the type of asset
type AssetType string

const (
	AssetTypeFiat    AssetType = "FIAT"
	AssetTypeCoin    AssetType = "COIN"    // Native blockchain coin (BTC, ETH, TRX)
	AssetTypeToken   AssetType = "TOKEN"   // Smart contract token (ERC20, TRC20, SPL)
	AssetTypeStable  AssetType = "STABLE"  // Stablecoin (USDT, USDC, etc.)
	AssetTypeWrapped AssetType = "WRAPPED" // Wrapped asset (WBTC, WETH)
)

func (a AssetType) String() string {
	return string(a)
}

// AssetStatus represents the operational status of an asset
type AssetStatus string

const (
	AssetStatusActive     AssetStatus = "ACTIVE"
	AssetStatusInactive   AssetStatus = "INACTIVE"
	AssetStatusDeprecated AssetStatus = "DEPRECATED"
	AssetStatusDelisted   AssetStatus = "DELISTED"
)

func (a AssetStatus) String() string {
	return string(a)
}

// OrderSide represents buy or sell
type OrderSide string

const (
	OrderSideBuy  OrderSide = "BUY"
	OrderSideSell OrderSide = "SELL"
)

func (s OrderSide) String() string {
	return string(s)
}

func (s OrderSide) Opposite() OrderSide {
	if s == OrderSideBuy {
		return OrderSideSell
	}
	return OrderSideBuy
}

// OrderType represents the order type
type OrderType string

const (
	OrderTypeMarket    OrderType = "MARKET"
	OrderTypeLimit     OrderType = "LIMIT"
	OrderTypeStop      OrderType = "STOP"
	OrderTypeStopLimit OrderType = "STOP_LIMIT"
)

func (o OrderType) String() string {
	return string(o)
}

// OrderStatus represents the order lifecycle status
type OrderStatus string

const (
	OrderStatusNew             OrderStatus = "NEW"
	OrderStatusPartiallyFilled OrderStatus = "PARTIALLY_FILLED"
	OrderStatusFilled          OrderStatus = "FILLED"
	OrderStatusCancelled       OrderStatus = "CANCELLED"
	OrderStatusRejected        OrderStatus = "REJECTED"
	OrderStatusExpired         OrderStatus = "EXPIRED"
)

func (o OrderStatus) String() string {
	return string(o)
}

// TimeInForce represents order time in force
type TimeInForce string

const (
	TimeInForceGTC TimeInForce = "GTC" // Good Till Cancelled
	TimeInForceIOC TimeInForce = "IOC" // Immediate Or Cancel
	TimeInForceFOK TimeInForce = "FOK" // Fill Or Kill
	TimeInForceGTX TimeInForce = "GTX" // Good Till Crossing (Post-Only)
)

func (t TimeInForce) String() string {
	return string(t)
}

// TradeSide represents the aggressive side of a trade
type TradeSide string

const (
	TradeSideBuy  TradeSide = "BUY"
	TradeSideSell TradeSide = "SELL"
)

func (t TradeSide) String() string {
	return string(t)
}

// SettlementMode represents how trades settle
type SettlementMode string

const (
	SettlementModeOffChain SettlementMode = "OFF_CHAIN"
	SettlementModeOnChain  SettlementMode = "ON_CHAIN"
	SettlementModeLayer2   SettlementMode = "LAYER2"
	SettlementModeBatch    SettlementMode = "BATCH"
)

func (s SettlementMode) String() string {
	return string(s)
}

// KYCStatus represents KYC verification status
type KYCStatus string

const (
	KYCStatusPending     KYCStatus = "PENDING"
	KYCStatusApproved    KYCStatus = "APPROVED"
	KYCStatusRejected    KYCStatus = "REJECTED"
	KYCStatusExpired     KYCStatus = "EXPIRED"
	KYCStatusUnderReview KYCStatus = "UNDER_REVIEW"
)

// KYCTier represents KYC verification tier
type KYCTier string

const (
	KYCTierNone          KYCTier = "NONE"
	KYCTierBasic         KYCTier = "BASIC"         // Email/phone only
	KYCTierStandard      KYCTier = "STANDARD"      // ID document + selfie
	KYCTierEnhanced      KYCTier = "ENHANCED"      // Full KYC + source of funds
	KYCTierInstitutional KYCTier = "INSTITUTIONAL" // Corporate KYC
)

// kycTierRanks orders the tiers from no verification to corporate KYC.
var kycTierRanks = map[KYCTier]int{
	KYCTierNone:          0,
	KYCTierBasic:         1,
	KYCTierStandard:      2,
	KYCTierEnhanced:      3,
	KYCTierInstitutional: 4,
}

// Rank returns the position of t from NONE (0) to INSTITUTIONAL (4), and
// false for a tier that does not exist. Tiers must be compared by rank: as
// strings, "BASIC" < "NONE".
func (t KYCTier) Rank() (int, bool) {
	rank, ok := kycTierRanks[t]
	return rank, ok
}

// SanctionAction represents the action to take on sanctions match
type SanctionAction string

const (
	SanctionActionAllow  SanctionAction = "ALLOW"
	SanctionActionReview SanctionAction = "REVIEW"
	SanctionActionBlock  SanctionAction = "BLOCK"
)
