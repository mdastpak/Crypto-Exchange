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

// The protected tables of the first step of #139: the accounts, their
// contacts, which decide who signs in to which account, and the sessions,
// which decide whose tokens still work.
var (
	// Users covers what decides whether and how an account signs in, and
	// what it may do: the role, the status, the deletion, the second
	// factor, the security hold, the KYC level, and the names.
	Users = &Table{Name: "users", Key: "hk", KeyType: "uuid", Columns: []Column{
		text("status"), text("role"), timestamptz("deleted_at"),
		secret("totp_secret"), text("totp_enabled"),
		timestamptz("security_hold_until"), text("kyc_status"), text("kyc_tier"),
		text("first_name"), text("last_name"), text("display_name"),
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
)

// Tables lists the protected tables.
var Tables = []*Table{Users, UserContacts, Sessions, UserPasswords}

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
