package admin

import (
	"context"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/adminauth"
	"github.com/mdastpak/crypto-exchange/internal/assetregistry"
	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/currencies"
	"github.com/mdastpak/crypto-exchange/internal/delisting"
	"github.com/mdastpak/crypto-exchange/internal/domain/currency"
	"github.com/mdastpak/crypto-exchange/internal/domain/market"
	"github.com/mdastpak/crypto-exchange/internal/models"
	"github.com/mdastpak/crypto-exchange/internal/notify"
	"github.com/mdastpak/crypto-exchange/internal/otp"
	"github.com/mdastpak/crypto-exchange/internal/pagination"
	"github.com/mdastpak/crypto-exchange/internal/platform"
	"github.com/mdastpak/crypto-exchange/internal/price"
	"github.com/mdastpak/crypto-exchange/internal/priceoverride"
	"github.com/mdastpak/crypto-exchange/internal/rebrand"
	"github.com/mdastpak/crypto-exchange/internal/restrictions"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"
	"github.com/mdastpak/crypto-exchange/internal/withdrawals"
)

// The dependencies of the admin routes (#48). The handler holds each one as
// an interface of what it uses, so that a test builds it with fakes;
// NewHandler and the composition root give it the stores of the database.

// CurrencyStore changes and reads the currencies: currencies.Store.
type CurrencyStore interface {
	Create(ctx context.Context, status string, info models.CurrencyInfo, origin audit.Event) (currencies.Written, error)
	Update(ctx context.Context, hk string, expected *int64, edit currencies.Edit, origin audit.Event) (currencies.Written, error)
	Delete(ctx context.Context, hk string, expected *int64, origin audit.Event) (currencies.Written, error)
	Restore(ctx context.Context, hk string, expected *int64, origin audit.Event) (currencies.Written, error)
	ReleaseSymbol(ctx context.Context, hk, symbol string, origin audit.Event) (currencies.Written, error)
	Get(ctx context.Context, hk string) (models.Currency, error)
	All(ctx context.Context) ([]models.Currency, error)
}

// RegistryStore changes and reads the networks of the currencies and the
// markets (#57): currencies.Store.
type RegistryStore interface {
	Networks(ctx context.Context, hk string) ([]currency.Network, error)
	SetNetwork(ctx context.Context, hk string, n assetregistry.Network, decimals int, origin audit.Event) (currencies.NetworkWritten, error)
	Markets(ctx context.Context) ([]market.Market, error)
	Market(ctx context.Context, base, quote string) (market.Market, error)
	SetMarket(ctx context.Context, m assetregistry.Market, origin audit.Event) (currencies.MarketWritten, error)
}

// WithdrawalReviews are the withdrawals that administrators review (#70):
// withdrawals.Service.
type WithdrawalReviews interface {
	Review(ctx context.Context, status withdrawals.Status, limit int) ([]withdrawals.Withdrawal, error)
	Approve(ctx context.Context, id, adminHK string, origin audit.Event) (withdrawals.Withdrawal, error)
	Reject(ctx context.Context, id, adminHK, reason string, origin audit.Event) (withdrawals.Withdrawal, error)
}

// PayoutDesk is where administrators pay the payouts of withdrawals by
// hand (#70): withdrawals.Payouts.
type PayoutDesk interface {
	List(ctx context.Context, status withdrawals.PayoutStatus, limit int) ([]withdrawals.Payout, error)
	Detail(ctx context.Context, id string, origin audit.Event) (withdrawals.PayoutDetail, error)
	Complete(ctx context.Context, id, trackingID string, origin audit.Event) (withdrawals.Payout, error)
	Fail(ctx context.Context, id, reason string, origin audit.Event) (withdrawals.Payout, error)
}

// CatalogCache is the cached list of public currencies, which a change
// drops: *currencies.Catalog.
type CatalogCache interface {
	Invalidate(ctx context.Context) error
}

// Accounts are the administrators and their sessions: *adminauth.Service.
type Accounts interface {
	FindActive(ctx context.Context, email string) (adminauth.Admin, error)
	IsActive(ctx context.Context, adminID string) (bool, error)
	VerifyTOTP(ctx context.Context, adminID, code string) error
	ConfirmEmail(ctx context.Context, adminID string) error
	StartSession(ctx context.Context, session adminauth.NewSession, event audit.Event) error
	VerifySession(ctx context.Context, sessionID, adminID string) error
	ListSessions(ctx context.Context, adminID, current string) ([]adminauth.Session, error)
	EndSession(ctx context.Context, sessionID, adminID, reason string, event audit.Event) error
	Elevated(ctx context.Context, sessionID, adminID string) (bool, error)
	StepUp(ctx context.Context, sessionID, adminID, code string, lifetime time.Duration, event audit.Event) (time.Time, error)
}

// CodeStore issues and checks the emailed sign-in codes: *otp.Store.
type CodeStore interface {
	Issue(ctx context.Context, contact, client string) (otp.Issued, error)
	Verify(ctx context.Context, sessionID, code string) (string, error)
}

// ControlStore changes and reads the platform controls: platform.Store.
type ControlStore interface {
	Open(ctx context.Context) ([]*platform.Control, error)
	List(ctx context.Context, limit int) ([]*platform.Control, error)
	Create(ctx context.Context, n platform.New, origin audit.Event) (*platform.Control, error)
	Update(ctx context.Context, hk string, changes platform.Changes, expected *int64, origin audit.Event) (*platform.Control, error)
	Lift(ctx context.Context, r platform.LiftRequest, origin audit.Event) (platform.LiftResult, error)
}

// ControlRefresher is this instance's copy of the platform controls, which
// a change refreshes at once: *platform.Watcher.
type ControlRefresher interface {
	Refresh(ctx context.Context) error
}

// RestrictionStore changes and reads the restrictions of users:
// restrictions.Store.
type RestrictionStore interface {
	Create(ctx context.Context, n restrictions.New, origin audit.Event) (restrictions.Created, error)
	List(ctx context.Context, userHK string, activeOnly bool, limit int) ([]restrictions.Restriction, error)
	Lift(ctx context.Context, userHK, hk, liftedBy, note string, expected *int64, origin audit.Event) (restrictions.Restriction, error)
}

// DelistingStore changes and reads the delistings: delisting.Store.
type DelistingStore interface {
	Schedule(ctx context.Context, currencyHK string, p delisting.Plan, origin audit.Event) (delisting.Delisting, delisting.Change, error)
	List(ctx context.Context, currencyHK string) ([]delisting.Delisting, error)
	Cancel(ctx context.Context, currencyHK, hk string, version *int64, origin audit.Event) (delisting.Delisting, delisting.Change, error)
	Override(ctx context.Context, currencyHK string, k currencies.Capability, allowed *bool, note string, origin audit.Event) (*models.CurrencyLifecycle, delisting.Change, error)
	Report(ctx context.Context, currencyHK, hk string) (delisting.SettlementReport, error)
	Notices(ctx context.Context, currencyHK, hk string) ([]delisting.NoticeReport, error)
	Retry(ctx context.Context, currencyHK, hk string, origin audit.Event) (int, error)
}

// RebrandStore changes and reads the rebrands: rebrand.Store.
type RebrandStore interface {
	Schedule(ctx context.Context, targetHK string, p rebrand.Plan, origin audit.Event) (rebrand.Migration, error)
	List(ctx context.Context, currencyHK string) ([]rebrand.Migration, error)
	Cancel(ctx context.Context, targetHK, hk string, version *int64, origin audit.Event) (rebrand.Migration, []currencies.Change, error)
	Progress(ctx context.Context, targetHK, migrationHK string) ([]rebrand.SourceProgress, error)
	Retry(ctx context.Context, targetHK, migrationHK string, origin audit.Event) (int, error)
}

// AuditLog records the events of the admin routes and lists the log:
// audit.Log.
type AuditLog interface {
	Record(ctx context.Context, e audit.Event) error
	List(ctx context.Context, f audit.Filter, page pagination.Request) (pagination.Page[audit.Entry], error)
}

// FindingStore lists the integrity findings: rowseal.Findings.
type FindingStore interface {
	List(ctx context.Context, o rowseal.ListOptions) ([]rowseal.Finding, error)
}

// The stores of the database implement the interfaces.
var (
	_ CurrencyStore    = currencies.Store{}
	_ CatalogCache     = (*currencies.Catalog)(nil)
	_ Accounts         = (*adminauth.Service)(nil)
	_ CodeStore        = (*otp.Store)(nil)
	_ ControlStore     = platform.Store{}
	_ ControlRefresher = (*platform.Watcher)(nil)
	_ RestrictionStore = restrictions.Store{}
	_ DelistingStore   = delisting.Store{}
	_ RebrandStore     = rebrand.Store{}
	_ AuditLog         = audit.Log{}
	_ FindingStore     = rowseal.Findings{}
)

// SMSRouter routes SMS through the providers (#371): *notify.SMSRouter.
type SMSRouter interface {
	Status(ctx context.Context) notify.SMSStatus
	CheckRouting(routing notify.SMSRouting) error
	Refresh()
}

// SMSRoutingStore keeps the routing that administrators set:
// smsrouting.Store.
type SMSRoutingStore interface {
	Set(ctx context.Context, routing notify.SMSRouting, adminHK string, origin audit.Event) error
}

// PriceReader reads the current price of a market: *price.Reader.
type PriceReader interface {
	Get(ctx context.Context, market string) (price.Price, error)
}

// PriceOverrideStore keeps the overrides: priceoverride.Store.
type PriceOverrideStore interface {
	Set(ctx context.Context, n priceoverride.New, adminHK string, origin audit.Event) (priceoverride.Override, error)
	End(ctx context.Context, id, adminHK string, origin audit.Event) (priceoverride.Override, error)
	Approve(ctx context.Context, id, adminHK string, origin audit.Event) (priceoverride.Override, error)
	List(ctx context.Context, inForce bool, limit int) ([]priceoverride.Override, error)
}

// OverrideRefresher reads the overrides again on this instance, so that a
// change applies here at once: *priceoverride.Watcher.
type OverrideRefresher interface {
	Refresh(ctx context.Context) error
}
