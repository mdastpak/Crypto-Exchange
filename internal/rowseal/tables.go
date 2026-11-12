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
	Users = &Table{Name: "users", Key: "hk", KeyType: "uuid", Columns: []Column{
		text("status"), text("role"), timestamptz("deleted_at"),
		secret("totp_secret"), text("totp_enabled"),
		timestamptz("security_hold_until"), text("kyc_status"), text("kyc_tier"),
		text("first_name"), text("last_name"), text("display_name"),
		optional(timestamptz("password_set_at")),
	}}
	// UserContacts covers which account a contact signs in to, whether it
	// is verified or removed, and when its ownership was last checked.
	UserContacts = &Table{Name: "user_contacts", Key: "id", KeyType: "bigint", Columns: []Column{
		text("user_hk"), text("type"), secret("value"), text("is_primary"),
		timestamptz("verified_at"), timestamptz("removed_at"), timestamptz("ownership_checked_at"),
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
)

// Tables lists the protected tables.
var Tables = []*Table{
	Users, UserContacts, Sessions, UserPasswords, UserPasskeys, UserTOTP, AccountRecoveries, SessionStepUps,
	AdminSessions, UserRestrictions, PlatformControls, UserRecoveryCodes,
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
