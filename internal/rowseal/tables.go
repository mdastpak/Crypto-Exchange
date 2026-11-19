package rowseal

import (
	"database/sql"
	"fmt"
	"strings"
)

// Table is a protected table: its primary key, and the columns that its
// seal covers. A column that is not listed can change without the seal,
// such as sessions.last_used_at, which a session check writes on its own.
type Table struct {
	// Name is the table, and the table_name of its findings.
	Name string
	// Key is the primary key column, and KeyType its type, for the cast
	// from text.
	Key, KeyType string
	Columns      []Column
	// SealedFromStart marks a table whose rows were sealed from the first
	// one (#404), such as the balances: a row that fails its check is
	// never one written before its seals, so it is refused in every mode,
	// and the verifier never adopts it in report mode.
	SealedFromStart bool
}

// Column is a protected column.
type Column struct {
	Name string
	// Expr yields the canonical text of the column, with %[1]s where the
	// alias goes, such as "%[1]sstatus::text". Times are formatted in UTC
	// with microseconds, so that the text depends on no setting of the
	// session, such as TimeZone or DateStyle.
	Expr string
	// Secret columns are never shown, in the audit log or the admin view.
	Secret bool
	// Optional columns, which a table gains after its rows were first
	// sealed, are left out of the seal while they are NULL, so that the
	// seals made before them still match. A value, once sealed, cannot be
	// set back to NULL unnoticed.
	Optional bool
}

// The canonical text of each kind of column.
func text(name string) Column { return Column{Name: name, Expr: "%[1]s" + name + "::text"} }

func secret(name string) Column {
	c := text(name)
	c.Secret = true
	return c
}

// timestamptz formats a TIMESTAMPTZ in UTC.
func timestamptz(name string) Column {
	return Column{Name: name, Expr: `to_char(%[1]s` + name + ` AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US')`}
}

// sealedBytes is a BYTEA column of ciphertext or of a blind index (#140),
// in hex, which depends on no setting of the session, as bytea_output does.
// It is secret: the admin view and the audit log never show it.
func sealedBytes(name string) Column {
	return Column{Name: name, Expr: "encode(%[1]s" + name + ", 'hex')", Secret: true}
}

// optional marks a column that the table gained after its rows were sealed.
func optional(c Column) Column {
	c.Optional = true
	return c
}

// Index returns the position of the column name in t.Columns, and in the
// Values of its rows. It panics for a column that t does not seal, which is
// a mistake of the code.
func (t *Table) Index(name string) int {
	for i, c := range t.Columns {
		if c.Name == name {
			return i
		}
	}
	panic(fmt.Sprintf("rowseal: %s seals no column %s", t.Name, name))
}

// The protected tables of the first step of #139: the accounts, their
// contacts, which decide who signs in to which account, and the sessions,
// which decide whose tokens still work.
var (
	// Users covers what decides whether and how an account signs in, and
	// what it may do: the role, the status, the deletion, the second
	// factor, the security hold, the KYC level, and the names.
	//
	// password_set_at records that the account has a password (#337), so
	// that deleting its user_passwords row, which no seal can show, does
	// not drop the second step of its sign-in.
	//
	// The names are sealed as their ciphertext too (#140); the plaintext
	// columns stay sealed until the release that drops them.
	Users = &Table{Name: "users", Key: "hk", KeyType: "uuid", Columns: []Column{
		text("status"), text("role"), timestamptz("deleted_at"),
		secret("totp_secret"), text("totp_enabled"),
		timestamptz("security_hold_until"), text("kyc_status"), text("kyc_tier"),
		text("first_name"), text("last_name"), text("display_name"),
		optional(timestamptz("password_set_at")),
		optional(sealedBytes("first_name_enc")), optional(sealedBytes("last_name_enc")),
		optional(sealedBytes("display_name_enc")),
	}}
	// UserContacts covers which account a contact signs in to, whether it
	// is verified or removed, and when its ownership was last checked. The
	// blind index is sealed with the ciphertext (#140): a lookup finds the
	// account by it, so an index copied from another row would sign in to
	// the wrong account.
	UserContacts = &Table{Name: "user_contacts", Key: "id", KeyType: "bigint", Columns: []Column{
		text("user_hk"), text("type"), secret("value"), text("is_primary"),
		timestamptz("verified_at"), timestamptz("removed_at"), timestamptz("ownership_checked_at"),
		optional(sealedBytes("value_enc")), optional(sealedBytes("value_bidx")),
	}}
	// Sessions covers whose session it is, whether it is still active, and
	// its tokens.
	Sessions = &Table{Name: "sessions", Key: "id", KeyType: "uuid", Columns: []Column{
		text("user_id"), text("status"), timestamptz("created_at"), timestamptz("expires_at"),
		timestamptz("revoked_at"), secret("access_token_hash"), secret("refresh_token_hash"),
		secret("previous_refresh_hash"), timestamptz("rotated_at"),
	}}
	// UserPasswords covers the hash of the account password, the second
	// step of a sign-in (#307), and when it was changed. The attempt
	// counts are outside it: a wrong password writes them on its own.
	UserPasswords = &Table{Name: "user_passwords", Key: "user_hk", KeyType: "uuid", Columns: []Column{
		secret("password_hash"), timestamptz("changed_at"),
	}}
	// UserPasskeys covers the passkeys of users (#307): whose a credential
	// is, its public key, and whether it was removed. The signature
	// counter and the last use are outside it: each use writes them.
	UserPasskeys = &Table{Name: "user_passkeys", Key: "id", KeyType: "uuid", Columns: []Column{
		text("user_hk"), text("credential_id"), text("credential"), timestamptz("removed_at"),
	}}
)

// The state of second factors and recoveries (#337), which decides whether
// a factor proves the owner, whether a recovery may set a new password, and
// whether a session may run the operations that a factor protects.
var (
	// UserTOTP covers the authenticator apps of users: the sealed secret,
	// and whether it is confirmed or removed. The attempt counts and the
	// last time step are outside it: each code writes them.
	UserTOTP = &Table{Name: "user_totp", Key: "user_hk", KeyType: "uuid", Columns: []Column{
		secret("secret_sealed"), timestamptz("confirmed_at"), timestamptz("removed_at"),
	}}
	// AccountRecoveries covers what a recovery proved and when it may set a
	// new password: changing it in SQL would skip the proofs or the wait.
	AccountRecoveries = &Table{Name: "account_recoveries", Key: "id", KeyType: "uuid", Columns: []Column{
		text("user_hk"), text("mode"), text("status"), text("proven_contacts"),
		text("steps_required"), text("steps_passed"), text("identity_failures"),
		timestamptz("started_at"), timestamptz("ready_at"), timestamptz("expires_at"),
		timestamptz("ended_at"), text("end_reason"),
	}}
	// SessionStepUps covers until when a session may run the protected
	// operations without proving the factor again.
	SessionStepUps = &Table{Name: "session_step_ups", Key: "session_id", KeyType: "uuid", Columns: []Column{
		timestamptz("elevated_until"),
	}}
)

// The rest of the state that decides access (#337): who is an
// administrator's session, which restrictions and platform controls apply,
// and which recovery codes reset a password.
var (
	// AdminSessions covers whose session it is, until when it works, and
	// until when it may use the sensitive routes. last_used_at and the
	// count of wrong step-up codes are outside it: requests write them.
	AdminSessions = &Table{Name: "admin_sessions", Key: "id", KeyType: "uuid", Columns: []Column{
		text("admin_id"), timestamptz("created_at"), timestamptz("expires_at"),
		timestamptz("revoked_at"), text("revoked_reason"), timestamptz("elevated_until"),
	}}
	// UserRestrictions covers what a restriction blocks, for whom, and
	// until when. The case reference and the notes are for administrators.
	UserRestrictions = &Table{Name: "user_restrictions", Key: "hk", KeyType: "uuid", Columns: []Column{
		text("user_hk"), text("scope"), text("operations"), text("currency_hk"), text("reason"),
		timestamptz("ends_at"), text("created_by"), timestamptz("created_at"),
		timestamptz("lifted_at"), text("lifted_by"),
	}}
	// PlatformControls covers what a maintenance window or a lockdown
	// closes, for whom, when, what users read about it, and the request
	// and the lift of the two administrators that a lockdown may need.
	PlatformControls = &Table{Name: "platform_controls", Key: "hk", KeyType: "uuid", Columns: []Column{
		text("kind"), text("feature"), timestamptz("starts_at"), timestamptz("ends_at"),
		timestamptz("announce_at"), text("message_fa"), text("message_en"), text("reason"),
		text("allow_networks"), text("allow_users"), text("created_by"), timestamptz("created_at"),
		text("lift_requested_by"), timestamptz("lift_requested_at"), timestamptz("lifted_at"), text("lifted_by"),
	}}
	// UserRecoveryCodes covers the hash of each code and whether it is
	// spent: a code inserted, or put back, in SQL would reset a password.
	UserRecoveryCodes = &Table{Name: "user_recovery_codes", Key: "id", KeyType: "bigint", Columns: []Column{
		text("user_hk"), secret("code_hash"), timestamptz("retired_at"), text("retired_reason"),
	}}
	// Balances covers the funds of users (#60): whose balance it is, of
	// which currency, its components, and the version of the ledger that
	// wrote them. Only the ledger writes a balance, and seals it in the
	// transaction of its entry; a balance changed in SQL is refused before
	// the ledger moves it, and the reconciliation replays it besides.
	Balances = &Table{Name: "balances", Key: "id", KeyType: "uuid", Columns: []Column{
		text("user_id"), text("sub_account_id"), text("currency_hk"),
		text("available"), text("locked"), text("pending_deposit"), text("version"),
	}, SealedFromStart: true}
	// WithdrawalDestinations covers whose destination it is, its number or
	// address and memo, encrypted, and when it is usable (#160): a
	// destination swapped or made usable in SQL is refused, not paid.
	WithdrawalDestinations = &Table{Name: "withdrawal_destinations", Key: "id", KeyType: "uuid", Columns: []Column{
		text("user_hk"), text("kind"), sealedBytes("value_enc"), sealedBytes("value_bidx"), text("masked"),
		text("bank"), text("network"), sealedBytes("memo_enc"), text("verified_by"),
		timestamptz("usable_from"), timestamptz("removed_at"),
		// A destination pending its ownership check (#158) is unusable
		// while this is set: cleared in SQL, it would pass for verified.
		optional(timestamptz("ownership_pending_since")),
	}, SealedFromStart: true}
	// UserNationalIDs covers a user's verified national ID (#158): the
	// value, encrypted, its blind index, which keeps one account per
	// person, the date of birth, and who verified it and when. A national
	// ID swapped in SQL is not relied on. Rows written without a seal are
	// adopted in report mode, as in every table sealed after its rows.
	UserNationalIDs = &Table{Name: "user_national_ids", Key: "user_hk", KeyType: "uuid", Columns: []Column{
		sealedBytes("national_id_sealed"), timestamptz("verified_at"), text("provider"), text("provider_ref"),
		optional(sealedBytes("national_id_bidx")), optional(sealedBytes("birth_date_sealed")),
	}}
	// IdentityChecks covers a check of a national ID and its date of birth
	// (#158): what was submitted, and its outcome. A check marked matched
	// in SQL verifies nothing.
	IdentityChecks = &Table{Name: "identity_checks", Key: "id", KeyType: "uuid", Columns: []Column{
		text("user_hk"), sealedBytes("national_id_sealed"), sealedBytes("national_id_bidx"), sealedBytes("birth_date_sealed"),
		text("status"), text("provider"), text("provider_ref"), timestamptz("decided_at"),
	}, SealedFromStart: true}
	// AMLRules covers each AML rule as the sync wrote it from
	// config/aml_rules.yaml (#73): a rule disabled, loosened, or retired in
	// SQL is refused, and nothing is evaluated until it is reviewed.
	AMLRules = &Table{Name: "aml_rules", Key: "id", KeyType: "uuid", Columns: []Column{
		text("code"), text("name"), text("rule_type"), text("config"), text("threshold_amount"), text("threshold_count"),
		text("time_window_hours"), text("action"), text("severity"), text("enabled"), text("applies_to"),
		text("rule_version"), timestamptz("retired_at"),
	}, SealedFromStart: true}
	// AMLRuleHistory covers each version of an AML rule (#73), which the
	// alerts name.
	AMLRuleHistory = &Table{Name: "aml_rule_history", Key: "id", KeyType: "uuid", Columns: []Column{
		text("rule_id"), text("rule_version"), text("definition"), timestamptz("created_at"),
	}, SealedFromStart: true}
	// AMLEvents covers the operations that the AML rules count over a
	// window (#73): one deleted or lowered in SQL would let structuring
	// pass under its threshold.
	AMLEvents = &Table{Name: "aml_events", Key: "id", KeyType: "uuid", Columns: []Column{
		text("user_hk"), text("kind"), text("ref"), text("currency_hk"), text("amount"), text("value_usdt"),
		text("counterparty"), text("ip_country"), text("counterparty_country"), timestamptz("occurred_at"),
		text("refused"),
	}, SealedFromStart: true}
	// AMLAlerts covers an AML alert and its lifecycle (#73): one closed or
	// reassigned in SQL is detected.
	AMLAlerts = &Table{Name: "aml_alerts", Key: "id", KeyType: "uuid", Columns: []Column{
		text("rule_id"), text("rule_version"), text("user_id"), text("event_id"), text("action"), text("trigger_data"),
		text("risk_score"), text("status"), text("assigned_to"), timestamptz("resolved_at"), text("resolved_by"),
		text("resolution"), text("dedupe_key"), timestamptz("escalated_at"), timestamptz("created_at"),
	}, SealedFromStart: true}
	// SanctionsLists covers a version of a sanctions list (#73): its body,
	// by its SHA-256, and when a download last found it current. An entry
	// removed in SQL would let a sanctioned address through.
	SanctionsLists = &Table{Name: "sanctions_lists", Key: "id", KeyType: "bigint", Columns: []Column{
		text("provider"), sealedBytes("source_digest"),
		{Name: "body", Expr: `encode(sha256(%[1]sbody), 'hex')`, Secret: true}, text("entry_count"),
		timestamptz("fetched_at"), timestamptz("checked_at"),
	}, SealedFromStart: true}
	// KYCApplications covers a KYC application (#72): who asked for which
	// tier, its form, its status, and who reviewed it. One approved in SQL
	// grants nothing.
	KYCApplications = &Table{Name: "kyc_applications", Key: "id", KeyType: "uuid", Columns: []Column{
		text("user_id"), text("requested_tier"), text("status"), sealedBytes("form_data_enc"), timestamptz("submitted_at"),
		timestamptz("reviewed_at"), text("reviewed_by"), text("reason"), text("provider"), text("provider_ref_id"),
		timestamptz("created_at"),
	}, SealedFromStart: true}
	// KYCDocuments covers a document of a KYC application (#72): its
	// application, type, kind of file, and the object and the digest of its
	// encrypted content. An object swapped in the storage, or a document
	// moved in SQL, is refused.
	KYCDocuments = &Table{Name: "kyc_documents", Key: "id", KeyType: "uuid", Columns: []Column{
		text("application_id"), text("type"), text("status"), text("file_size"), text("mime_type"),
		text("storage_path"), sealedBytes("content_digest"), timestamptz("created_at"),
	}, SealedFromStart: true}
	// DepositAddresses covers a deposit address of a user (#68): its owner,
	// network, address, and the index it is derived by. An address swapped
	// in SQL would credit another user's deposits.
	DepositAddresses = &Table{Name: "deposit_addresses", Key: "id", KeyType: "uuid", Columns: []Column{
		text("user_id"), text("sub_account_id"), text("currency_hk"), text("network"), text("address"),
		text("derivation_path"), text("index"), text("active"), timestamptz("created_at"),
	}, SealedFromStart: true}
	// OperationConfirmations covers what a confirmation confirms, how, and
	// whether it was confirmed, consumed, or denied by its link (#161,
	// #374): one marked confirmed in SQL runs nothing, and a denial cleared
	// in SQL is detected.
	OperationConfirmations = &Table{Name: "operation_confirmations", Key: "id", KeyType: "uuid", Columns: []Column{
		text("user_hk"), text("purpose"), sealedBytes("digest"), text("method"), secret("code_session"),
		text("failed_attempts"), timestamptz("expires_at"), timestamptz("confirmed_at"), timestamptz("consumed_at"),
		optional(timestamptz("denied_at")), optional(text("fee")),
	}, SealedFromStart: true}
	// UserConfirmationMethods covers the method a user chose to confirm
	// withdrawals (#161): one changed in SQL is ignored.
	UserConfirmationMethods = &Table{Name: "user_confirmation_methods", Key: "user_hk", KeyType: "uuid", Columns: []Column{
		text("method"),
	}}
	// AntiPhishingCodes covers the anti-phishing code of a user (#375),
	// encrypted: one changed or swapped in SQL is not shown, and its set_at,
	// from which its age counts. Its use count and reminder times are not
	// sealed: every email changes the count.
	AntiPhishingCodes = &Table{Name: "user_anti_phishing_codes", Key: "user_hk", KeyType: "uuid", Columns: []Column{
		sealedBytes("phrase_enc"), timestamptz("set_at"), timestamptz("removed_at"),
	}}
	// SMSRouting covers the routing of SMS that administrators set (#371):
	// one changed in SQL is ignored, and the auto mode applies.
	SMSRouting = &Table{Name: "sms_routing", Key: "id", KeyType: "bigint", Columns: []Column{
		text("mode"), text("provider_order"), text("updated_by"),
	}}
	// CurrencyNetworks covers how a currency moves on a network (#57): the
	// contract that custody credits deposits by, the decimals that scale
	// them, whether deposits and withdrawals are open, the limits, the
	// fees, and the explorer that users follow. A contract swapped in SQL
	// for another token's is not acted on.
	CurrencyNetworks = &Table{Name: "currency_networks", Key: "id", KeyType: "uuid", Columns: []Column{
		text("currency_hk"), text("network"), text("contract_address"), text("decimals"), text("custody_mode"),
		text("deposit_enabled"), text("withdrawal_enabled"), text("min_deposit"), text("min_withdrawal"),
		text("max_withdrawal"), text("withdrawal_fee"), text("fee_currency_hk"), text("requires_tag"), text("explorer_url"),
	}}
	// Markets covers the pair, the status, the rules, and the fees of a
	// market (#57): a market opened or made free in SQL is not acted on.
	Markets = &Table{Name: "markets", Key: "id", KeyType: "uuid", Columns: []Column{
		text("base_currency_hk"), text("quote_currency_hk"), text("status"), text("min_order_size"), text("max_order_size"),
		text("min_notional"), text("tick_size"), text("lot_size"), text("maker_fee_bps"), text("taker_fee_bps"),
		text("max_position_size"), text("max_leverage"), text("market_maker_fee_bps"), text("price_band_bps"),
		timestamptz("deleted_at"),
	}}
	// Withdrawals covers a withdrawal of a user (#70): whose it is, what,
	// where, how much, its status, and who reviewed it. A withdrawal sent to
	// another destination, raised, or approved in SQL is not paid.
	Withdrawals = &Table{Name: "withdrawals", Key: "id", KeyType: "uuid", Columns: []Column{
		text("user_id"), text("currency_hk"), text("network"), text("destination_id"), text("amount"), text("fee"),
		text("status"), text("confirmation_id"), text("reviewed_by"), timestamptz("reviewed_at"),
		timestamptz("completed_at"), timestamptz("cancelled_at"),
	}, SealedFromStart: true}
	// OTCTrades covers an OTC trade (#62): whose it is, the quote it took,
	// what it traded at which price and fee, what it settled, and its ledger
	// entry. A trade is written once and sealed in its transaction; one
	// changed in SQL is never shown as its user's or replayed.
	OTCTrades = &Table{Name: "otc_trades", Key: "id", KeyType: "uuid", Columns: []Column{
		text("user_id"), text("sub_account_id"), text("quote_id"), text("market"), text("base_currency_hk"),
		text("quote_currency_hk"), text("side"), text("quantity"), text("price"), text("total"), text("fee"), text("fee_bps"),
		text("fee_currency_hk"), text("give_amount"), text("receive_amount"), text("components"), text("override_id"),
		text("entry_id"), timestamptz("executed_at"),
	}, SealedFromStart: true}
	// OTCHousePositions covers the net position of the exchange's inventory
	// in a currency (#62), which the inventory limits of the OTC trades
	// check. A position lowered in SQL, to let more trades through, refuses
	// the trades of its currency in enforce mode.
	OTCHousePositions = &Table{Name: "otc_house_positions", Key: "currency_hk", KeyType: "uuid", Columns: []Column{
		text("position"),
	}}
	// VenueReservations covers a reservation of a balance at a venue by a
	// routed order (#385): which balance, how much, for what, and whether
	// it was released. A reservation released or lowered in SQL, which
	// would let the router spend the same funds twice, is refused.
	VenueReservations = &Table{Name: "venue_reservations", Key: "id", KeyType: "uuid", Columns: []Column{
		text("venue"), text("asset"), text("amount"), text("reference_type"), text("reference_id"),
		timestamptz("created_at"), timestamptz("released_at"),
	}, SealedFromStart: true}
	// VenueHolds covers a hold of a currency at a venue (#385), which a
	// reconciliation opens on a difference with the books. A hold cleared
	// in SQL, which would route through a venue that may have lost funds,
	// keeps the currency not routable.
	VenueHolds = &Table{Name: "venue_holds", Key: "id", KeyType: "uuid", Columns: []Column{
		text("venue"), text("asset"), text("currency_hk"), text("reported"), text("booked"), text("difference"),
		timestamptz("detected_at"), timestamptz("cleared_at"), text("cleared_by"), text("clear_reason"),
	}, SealedFromStart: true}
	// OTCRoutingRules covers a version of the routing rules of the OTC
	// trades (#386): the rules, why, by whom, and when. A version changed
	// in SQL, such as a rule that sends trades to a venue the
	// administrators did not choose, routes nothing: the quotes are
	// refused until it is reviewed.
	OTCRoutingRules = &Table{Name: "otc_routing_rules", Key: "version", KeyType: "bigint", Columns: []Column{
		text("rules"), text("reason"), text("created_by"), timestamptz("created_at"),
	}, SealedFromStart: true}
	// OTCRoutingUsage covers the notional that the routed orders took from
	// a venue in a day (#386): lowered in SQL, it would let more through
	// than the daily limit of a rule.
	OTCRoutingUsage = &Table{Name: "otc_routing_usage", Key: "id", KeyType: "uuid", Columns: []Column{
		text("venue"), text("day"), text("currency_hk"), text("notional"),
	}, SealedFromStart: true}
	// OTCRoutedOrders covers a routed order of an OTC trade (#387): what it
	// buys or sells at which venue, its slippage guard, its id there, its
	// status, and what it filled. An order changed in SQL, such as a limit
	// raised or a fill lowered, is not driven further: it waits for review.
	OTCRoutedOrders = &Table{Name: "otc_routed_orders", Key: "id", KeyType: "uuid", Columns: []Column{
		text("trade_id"), text("leg"), text("attempt"), text("parent_id"), text("venue"), text("symbol"),
		text("base_asset"), text("quote_asset"), text("base_currency_hk"), text("quote_currency_hk"), text("side"),
		text("quantity"), text("limit_price"), text("trade_price"), text("reference_price"), text("client_id"), text("venue_order_id"), text("status"),
		text("filled"), text("filled_quote"), text("reservation_id"), text("reason"), timestamptz("created_at"),
	}, SealedFromStart: true}
	// IntegrityFindings covers the resolution of a finding (#404): which
	// row it is about, and whether, when, how, and by whom it was closed. A
	// finding closed in SQL, which would hide the row from the
	// administrators, is itself a finding. Its counts and the request of a
	// re-seal are not sealed: every detection changes the counts, and a
	// request counts only with its sealed audit event.
	IntegrityFindings = &Table{Name: "integrity_findings", Key: "id", KeyType: "bigint", Columns: []Column{
		text("table_name"), text("row_key"), timestamptz("detected_at"), timestamptz("resolved_at"),
		text("resolution"), text("resolved_by"), text("reason"),
	}}
	// WithdrawalPayouts covers a transfer that pays a withdrawal (#70): its
	// withdrawal, rail, provider, amount, fee, status, and the provider's
	// references. A payout raised, completed, or released in SQL is not
	// acted on. Its rows are sealed from the first one.
	WithdrawalPayouts = &Table{Name: "withdrawal_payouts", Key: "id", KeyType: "uuid", Columns: []Column{
		text("withdrawal_id"), text("seq"), text("rail"), text("provider"), text("amount"), text("fee"), text("status"),
		text("idempotency_key"), text("provider_ref"), text("tracking_id"), timestamptz("submitted_at"),
		timestamptz("completed_at"), timestamptz("failed_at"),
	}, SealedFromStart: true}
	// DenyLinks covers a "this was not me" link (#374): the hash of its
	// token, whose confirmation it denies, until when, and whether it was
	// used. A link raised or renewed in SQL denies nothing. Its rows are
	// sealed from the first one.
	DenyLinks = &Table{Name: "deny_links", Key: "id", KeyType: "uuid", Columns: []Column{
		sealedBytes("token_hash"), text("user_hk"), text("confirmation_id"), timestamptz("expires_at"), timestamptz("used_at"),
	}, SealedFromStart: true}
	// PriceOverrides covers a price that an administrator set for a market
	// (#59): its market, prices, expiry, and end, and its approval by a
	// second administrator (#453). An override set, moved,
	// or lengthened in SQL is not served. Its rows are sealed from the
	// first one.
	PriceOverrides = &Table{Name: "price_overrides", Key: "id", KeyType: "uuid", Columns: []Column{
		text("market"), text("bid"), text("ask"), text("mid"), text("reason"), text("created_by"),
		timestamptz("created_at"), timestamptz("expires_at"), timestamptz("ended_at"), text("ended_by"),
		optional(text("reference_mid")), optional(timestamptz("approve_by")), optional(text("approved_by")),
		optional(timestamptz("approved_at")),
	}, SealedFromStart: true}
)

// The rows that decide how much the ledger credits the holders of a
// currency that changes or goes (#451): a ratio, a target, a settlement
// price, or a settlement currency changed in SQL would convert every
// holder at that rate. The tables were created before they moved money,
// so their rows are not sealed from the first one: the verifier seals the
// rows that exist. The conversions refuse a row that fails its check in
// every mode (Require), since they move money.
var (
	// CurrencyMigrations covers a migration of currencies (#215): its
	// target, whether it is forced, its phase, who made it, its end, and,
	// as one column, its sources and their ratios. The sources have no key
	// of their own, so their seal is the migration's: a ratio raised, or a
	// source added or removed in SQL, breaks the seal of the migration.
	CurrencyMigrations = &Table{Name: "currency_migrations", Key: "hk", KeyType: "uuid", Columns: []Column{
		text("target_hk"), text("forced"), text("phase"), text("created_by"),
		timestamptz("cancelled_at"), text("cancelled_by"), timestamptz("completed_at"),
		{Name: "sources", Expr: migrationSources},
	}}
	// CurrencyDelistings covers a delisting (#216): its currency, kind,
	// status, settlement, settlement currency and price source, the fixed
	// price, the days of a claim, who made it, its end, and the settlement
	// price once it is recorded.
	CurrencyDelistings = &Table{Name: "currency_delistings", Key: "hk", KeyType: "uuid", Columns: []Column{
		text("currency_hk"), text("kind"), text("status"), text("settlement"), text("settlement_currency_hk"),
		text("price_source"), text("fixed_price"), text("claim_days"), text("created_by"),
		timestamptz("cancelled_at"), text("cancelled_by"), timestamptz("completed_at"),
		optional(text("settlement_price")), optional(text("settlement_price_detail")),
		optional(timestamptz("settlement_priced_at")),
	}}
)

// migrationSources is the canonical text of the sources of a migration:
// each currency and its ratio, in the order of the currencies. A
// migration has at least one source, so the text is never NULL for one
// that the backend made.
const migrationSources = `(SELECT string_agg(src.currency_hk::text || '=' || src.ratio::text, ',' ORDER BY src.currency_hk)
	FROM currency_migration_sources src WHERE src.migration_hk = %[1]shk)`

// Tables lists the protected tables.
var Tables = []*Table{
	Users, UserContacts, Sessions, UserPasswords, UserPasskeys, UserTOTP, AccountRecoveries, SessionStepUps,
	AdminSessions, UserRestrictions, PlatformControls, UserRecoveryCodes, Balances, WithdrawalDestinations,
	OperationConfirmations, UserConfirmationMethods, AntiPhishingCodes, SMSRouting, CurrencyNetworks, Markets, Withdrawals,
	IntegrityFindings, WithdrawalPayouts, DenyLinks, PriceOverrides, CurrencyMigrations, CurrencyDelistings,
	OTCTrades, OTCHousePositions,
	VenueReservations, VenueHolds,
	OTCRoutingRules, OTCRoutingUsage, OTCRoutedOrders,
	UserNationalIDs, IdentityChecks,
	AMLRules, AMLRuleHistory, AMLEvents, AMLAlerts,
	SanctionsLists,
	KYCApplications, KYCDocuments,
	DepositAddresses,
}

// AuditEvents covers every column of an audit event (#337), so that an
// event changed after the append-only triggers were turned off is detected.
// Events never change: audit.Record seals each one in its INSERT, at
// row_version 0, through the Sealer's audit methods, and nothing re-seals
// them. So the table is not in Tables, which the verifier and the re-seals
// of findings walk.
//
// Its columns are fixed for good: the seal covers their names and order,
// and the events written before cannot be sealed again. A column added to
// audit_events later stays outside the seal, or needs a seal format of its
// own that the check tells apart.
var AuditEvents = &Table{Name: "audit_events", Key: "hk", KeyType: "uuid", Columns: []Column{
	timestamptz("occurred_at"), text("actor_type"), text("actor_id"), text("action"),
	text("target_type"), text("target_id"), text("outcome"), text("reason"),
	text("request_id"), text("ip"), text("user_agent"), text("changes"),
}}

// TableNamed returns the protected table name, or nil.
func TableNamed(name string) *Table {
	for _, t := range Tables {
		if t.Name == name {
			return t
		}
	}
	return nil
}

// Fields returns the select list of a Row of t: the key, the version, the
// key version, the seal, and the columns, qualified by alias ("u" makes
// u.status), or unqualified for "".
func (t *Table) Fields(alias string) string { return t.fields(alias, "") }

// FieldsAs is Fields with each item named prefix_0, prefix_1, and so on, so
// that the rows of two tables fit in one select list; Names lists them.
func (t *Table) FieldsAs(alias, prefix string) string { return t.fields(alias, prefix) }

func (t *Table) fields(alias, prefix string) string {
	p := ""
	if alias != "" {
		p = alias + "."
	}
	items := []string{p + t.Key + "::text", p + "row_version", p + "seal_key_version::bigint", p + "row_seal"}
	for _, c := range t.Columns {
		items = append(items, fmt.Sprintf(c.Expr, p))
	}
	if prefix != "" {
		for i := range items {
			items[i] += fmt.Sprintf(" AS %s_%d", prefix, i)
		}
	}
	return strings.Join(items, ", ")
}

// ColumnFields returns the expressions of the canonical text of the sealed
// columns of t alone, in their order, qualified by alias as Fields does.
func (t *Table) ColumnFields(alias string) []string {
	p := ""
	if alias != "" {
		p = alias + "."
	}
	items := make([]string, len(t.Columns))
	for i, c := range t.Columns {
		items[i] = fmt.Sprintf(c.Expr, p)
	}
	return items
}

// Names lists the names that FieldsAs gave, for a select from a CTE.
func (t *Table) Names(prefix string) string {
	names := make([]string, 4+len(t.Columns))
	for i := range names {
		names[i] = fmt.Sprintf("%s_%d", prefix, i)
	}
	return strings.Join(names, ", ")
}

// Row is a row of a protected table as Fields reads it.
type Row struct {
	Key        string
	Version    int64
	KeyVersion sql.NullInt64
	Seal       []byte
	Values     []sql.NullString
}

// NewRow returns a Row to scan a row of t into.
func (t *Table) NewRow() *Row { return &Row{Values: make([]sql.NullString, len(t.Columns))} }

// Dest returns the scan destinations of r, in the order of Fields.
func (r *Row) Dest() []any {
	dest := []any{&r.Key, &r.Version, &r.KeyVersion, &r.Seal}
	for i := range r.Values {
		dest = append(dest, &r.Values[i])
	}
	return dest
}

// Shown returns the columns of r that may be shown, with secret ones
// marked, for the audit log and the admin view.
func (t *Table) Shown(r *Row) map[string]interface{} {
	shown := map[string]interface{}{}
	for i, c := range t.Columns {
		switch {
		case !r.Values[i].Valid:
			shown[c.Name] = nil
		case c.Secret:
			shown[c.Name] = "[secret]"
		default:
			shown[c.Name] = r.Values[i].String
		}
	}
	return shown
}
