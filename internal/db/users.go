package db

import (
	"context"
	"database/sql"
)

// ContactMatch is the SQL condition that finds the row c of user_contacts
// with the type in $1 and the value in $2. It compares them the way the
// index user_contacts_value_unique does, so every lookup uses that index:
// emails and usernames case-insensitively, with ASCII-only case folding
// that does not depend on the database locale. A contact that its user
// removed (removed_at, #248) matches nothing.
const ContactMatch = `c.type = $1::contact_type AND lower(c.value COLLATE "C") = lower($2 COLLATE "C") AND c.removed_at IS NULL`

// InsertContact adds a primary contact to a user within tx. verified says
// that the user has just proven control of it, as a correct sign-in code
// does. A contact that belongs to another user fails with a unique
// violation (SQLSTATE 23505).
func InsertContact(ctx context.Context, tx *sql.Tx, userHK, contactType, value string, verified bool) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO user_contacts (user_hk, type, value, verified_at)
		 VALUES ($1, $2::contact_type, $3, CASE WHEN $4::boolean THEN NOW() END)`,
		userHK, contactType, value, verified,
	)
	return err
}
