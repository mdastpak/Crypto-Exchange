// Package usertest creates users for tests, stored the way the application
// stores them: a row of users with its contacts in user_contacts.
package usertest

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"
)

// User describes a test user. The zero value is an active user without
// contacts.
type User struct {
	// Status is an account_status_type value, ACTIVE when empty.
	Status string
	// Role is "user" when empty.
	Role string
	// Deleted sets deleted_at.
	Deleted bool
	// The primary contacts. Email and mobile contacts are verified, as a
	// sign-in leaves them; usernames are not.
	Email, Mobile, Username string
}

// Insert creates u and returns its hk.
func Insert(t testing.TB, db *sql.DB, u User) string {
	t.Helper()
	if u.Status == "" {
		u.Status = "ACTIVE"
	}
	if u.Role == "" {
		u.Role = "user"
	}
	tx, err := db.Begin()
	require.NoError(t, err)
	defer tx.Rollback()

	var hk string
	require.NoError(t, tx.QueryRow(
		`INSERT INTO users (status, role, deleted_at) VALUES ($1, $2, CASE WHEN $3::boolean THEN NOW() END) RETURNING hk`,
		u.Status, u.Role, u.Deleted,
	).Scan(&hk))
	for _, c := range []struct{ kind, value string }{{"email", u.Email}, {"mobile", u.Mobile}, {"username", u.Username}} {
		if c.value == "" {
			continue
		}
		_, err := tx.Exec(
			`INSERT INTO user_contacts (user_hk, type, value, verified_at)
			 VALUES ($1, $2::contact_type, $3, CASE WHEN $2 <> 'username' THEN NOW() END)`,
			hk, c.kind, c.value,
		)
		require.NoError(t, err)
	}
	require.NoError(t, tx.Commit())
	return hk
}
