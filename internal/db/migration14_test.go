package db

import (
	"database/sql"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// legacyUser is a users row as versions before migration 14 wrote it.
type legacyUser struct {
	info, status string
	deleted      bool
}

// insertLegacyUsers inserts users at version 13 and returns their hk values.
func insertLegacyUsers(t *testing.T, conn *sql.DB, users []legacyUser) []string {
	t.Helper()
	hks := make([]string, len(users))
	for i, u := range users {
		deletedAt := sql.NullString{String: "2026-03-04 05:06:07", Valid: u.deleted}
		require.NoError(t, conn.QueryRow(
			`INSERT INTO users (info, status, created_at, updated_at, deleted_at)
			 VALUES ($1::jsonb, $2, '2026-01-02 03:04:05', '2026-02-03 04:05:06', $3::timestamp) RETURNING hk`,
			u.info, u.status, deletedAt,
		).Scan(&hks[i]))
	}
	return hks
}

func uniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// TestMigration14_MovesUsersWithoutLoss migrates users in every form that
// the application wrote: each contact type, both contacts at once, and each
// legacy status, including a deleted user. It checks the contacts and the
// statuses after the migration, that the database enforces the contact
// rules, and that the down migration gives back info and status unchanged.
func TestMigration14_MovesUsersWithoutLoss(t *testing.T) {
	m := newMigrationsTestMigrator(t)
	require.NoError(t, m.Migrate(13))
	conn, err := sql.Open("pgx", migrationsTestDBConnStr())
	require.NoError(t, err)
	defer conn.Close()

	legacy := []legacyUser{
		{info: `{"contact": {"email": "Mixed.Case@Example.com"}}`, status: "approved"},
		{info: `{"contact": {"mobile": "+989120000001"}}`, status: "suspend"},
		{info: `{"contact": {"username": "legacy-user"}}`, status: "unverified"},
		{info: `{"contact": {"email": "both@example.com", "mobile": "+989120000002"}}`, status: "disabled"},
		{info: `{"contact": {"email": "gone@example.com"}}`, status: "deleted", deleted: true},
	}
	hks := insertLegacyUsers(t, conn, legacy)

	require.NoError(t, m.Migrate(14))

	type contact struct {
		user, kind, value string
		verified, primary bool
	}
	var contacts []contact
	rows, err := conn.Query(`SELECT user_hk, type, value, verified_at IS NOT NULL, is_primary FROM user_contacts ORDER BY user_hk, type`)
	require.NoError(t, err)
	for rows.Next() {
		var c contact
		require.NoError(t, rows.Scan(&c.user, &c.kind, &c.value, &c.verified, &c.primary))
		contacts = append(contacts, c)
	}
	require.NoError(t, rows.Err())
	assert.ElementsMatch(t, []contact{
		{hks[0], "email", "Mixed.Case@Example.com", true, true},
		{hks[1], "mobile", "+989120000001", true, true},
		{hks[2], "username", "legacy-user", false, true},
		{hks[3], "email", "both@example.com", true, true},
		{hks[3], "mobile", "+989120000002", true, true},
		{hks[4], "email", "gone@example.com", true, true},
	}, contacts, "every contact moves, emails keep their case, and only usernames stay unverified")

	wantStatus := []string{"ACTIVE", "SUSPENDED", "PENDING_VERIFICATION", "INACTIVE", "INACTIVE"}
	for i, hk := range hks {
		var status string
		var deleted bool
		require.NoError(t, conn.QueryRow(`SELECT status::text, deleted_at IS NOT NULL FROM users WHERE hk = $1`, hk).Scan(&status, &deleted))
		assert.Equal(t, wantStatus[i], status, legacy[i].status)
		assert.Equal(t, legacy[i].deleted, deleted, "%s: deleted_at stays the mark of deletion", legacy[i].status)
	}

	_, err = conn.Exec(`INSERT INTO user_contacts (user_hk, type, value, is_primary) VALUES ($1, 'email', 'MIXED.case@example.COM', FALSE)`, hks[1])
	assert.True(t, uniqueViolation(err), "a contact belongs to one user, emails compared case-insensitively: %v", err)
	_, err = conn.Exec(`INSERT INTO user_contacts (user_hk, type, value) VALUES ($1, 'email', 'second@example.com')`, hks[0])
	assert.True(t, uniqueViolation(err), "a user has one primary contact of each type: %v", err)
	_, err = conn.Exec(`INSERT INTO user_contacts (user_hk, type, value) VALUES ($1, 'email', '')`, hks[1])
	assert.Error(t, err, "an empty contact is refused")

	require.NoError(t, m.Migrate(13))

	for i, hk := range hks {
		var same bool
		var status string
		require.NoError(t, conn.QueryRow(`SELECT info = $2::jsonb, status::text FROM users WHERE hk = $1`, hk, legacy[i].info).Scan(&same, &status))
		assert.True(t, same, "%s: info comes back unchanged", legacy[i].info)
		assert.Equal(t, legacy[i].status, status, "the status comes back unchanged")
	}
}

// TestMigration14_PreflightStopsOnDataItCannotMove checks that migration 14
// stops with an actionable message, before changing the schema, when users
// holds data that no application code writes and that it would drop.
func TestMigration14_PreflightStopsOnDataItCannotMove(t *testing.T) {
	cases := map[string]struct {
		insert, wantErr string
	}{
		"other keys in info": {
			insert:  `INSERT INTO users (info) VALUES ('{"contact": {"email": "a@example.com"}, "password": "secret"}')`,
			wantErr: `keys other than "contact"`,
		},
		"unknown contact type": {
			insert:  `INSERT INTO users (info) VALUES ('{"contact": {"telegram": "@someone"}}')`,
			wantErr: "not an email, mobile, or username string",
		},
		"contact that is not a string": {
			insert:  `INSERT INTO users (info) VALUES ('{"contact": {"mobile": 989120000001}}')`,
			wantErr: "not an email, mobile, or username string",
		},
		"value in the email column of migration 8": {
			insert:  `INSERT INTO users (info, email) VALUES ('{"contact": {}}', 'column@example.com')`,
			wantErr: "values in the email or phone columns",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			m := newMigrationsTestMigrator(t)
			require.NoError(t, m.Migrate(13))
			conn, err := sql.Open("pgx", migrationsTestDBConnStr())
			require.NoError(t, err)
			defer conn.Close()
			_, err = conn.Exec(tc.insert)
			require.NoError(t, err)

			err = m.Migrate(14)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
			assert.Contains(t, err.Error(), `run "migrate force 13"`, "the error must explain how to recover from the dirty version")

			var infoExists bool
			require.NoError(t, conn.QueryRow(`SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'users' AND column_name = 'info')`).Scan(&infoExists))
			assert.True(t, infoExists, "a failed preflight must leave the schema unchanged")
		})
	}
}
