package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/contact"
	"github.com/mdastpak/crypto-exchange/internal/db"
	"github.com/mdastpak/crypto-exchange/internal/verification"

	"github.com/google/uuid"
)

// verificationArgs are the flags of verification grant.
type verificationArgs struct {
	user   string
	level  int
	reason string
	dryRun bool
}

// parseVerificationArgs parses the arguments after "verification".
func parseVerificationArgs(cmd command, args []string, stderr io.Writer) (command, error) {
	if len(args) == 0 || args[0] != "grant" {
		fmt.Fprint(stderr, usage)
		return command{}, errors.New("a verification command is required: grant")
	}
	sub := flag.NewFlagSet("verification grant", flag.ContinueOnError)
	sub.SetOutput(stderr)
	sub.StringVar(&cmd.verification.user, "user", "", "The user: their ID, or their email address or mobile number")
	sub.IntVar(&cmd.verification.level, "level", -1, "The level to set, from 0 to 2")
	sub.StringVar(&cmd.verification.reason, "reason", "", "Why, for the audit log")
	sub.BoolVar(&cmd.verification.dryRun, "dry-run", false, "Check the change and report it without making it")
	if err := sub.Parse(args[1:]); err != nil {
		return command{}, err
	}
	if sub.NArg() > 0 {
		return command{}, fmt.Errorf("unexpected argument %q", sub.Arg(0))
	}
	cmd.name = "verification grant"
	v := cmd.verification
	switch {
	case strings.TrimSpace(v.user) == "":
		return command{}, errors.New("-user is required")
	case v.level < int(verification.Level0) || v.level > int(verification.Level2):
		return command{}, fmt.Errorf("-level must be from %d to %d", verification.Level0, verification.Level2)
	case strings.TrimSpace(v.reason) == "" || utf8.RuneCountInString(strings.TrimSpace(v.reason)) > verification.MaxReason:
		return command{}, fmt.Errorf("-reason is required, at most %d characters", verification.MaxReason)
	case strings.TrimSpace(cmd.operator) == "":
		return command{}, errors.New("-operator is required before the command: the audit log names who set the level")
	}
	return cmd, nil
}

// runVerification runs verification grant. The level is a column of the
// user's sealed row, so the command needs the data keys, as the server
// has them.
func runVerification(cmd command, stdout, stderr io.Writer) int {
	if config.Config.Keys.Provider == "" {
		fmt.Fprintln(stderr, "Error: the level is sealed in the user's row: set keys.provider (KEYS_PROVIDER, see docs/data-protection.md)")
		return 1
	}
	postgres, err := db.NewPostgresService(dbOptions())
	if err != nil {
		fmt.Fprintln(stderr, "Error: connect to PostgreSQL:", err)
		return 1
	}
	defer postgres.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sealer, err := rowSealer(ctx, postgres.DB, cmd)
	if err == nil && sealer == nil {
		err = errors.New("no data keys are loaded; the server creates them on its first start")
	}
	if err != nil {
		fmt.Fprintln(stderr, "Error:", err)
		return 1
	}
	if err := executeVerification(ctx, postgres.DB, cmd, stdout); err != nil {
		fmt.Fprintln(stderr, "Error:", err)
		return 1
	}
	return 0
}

// executeVerification runs a parsed verification command against
// database. A change by an operator needs no second administrator: the
// operator has access to the database, and the audit log names them.
func executeVerification(ctx context.Context, database *sql.DB, cmd command, stdout io.Writer) error {
	v := cmd.verification
	userHK, err := findUser(ctx, database, v.user)
	if err != nil {
		return err
	}
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	change, err := verification.SetLevel(ctx, tx, userHK, verification.Level(v.level), v.reason, operator(cmd))
	if err != nil {
		return err
	}
	from, to := describeLevel(change.Before), describeLevel(change.After)
	switch {
	case !change.Changed():
		fmt.Fprintf(stdout, "User %s is at %s already; nothing changed.\n", userHK, to)
	case v.dryRun:
		fmt.Fprintf(stdout, "Dry run: user %s would go from %s to %s. Nothing was written.\n", userHK, from, to)
	default:
		if err := tx.Commit(); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "User %s went from %s to %s, audited as %s. Every instance applies it from the user's next deposit, trade, or withdrawal.\n",
			userHK, from, to, audit.ActionUserVerificationSet)
	}
	return nil
}

func describeLevel(s verification.Status) string {
	return fmt.Sprintf("level %d (%s, %s)", s.Level, s.Tier, s.Status)
}

// findUser returns the ID of the user that value names: the ID itself, or
// the user's email address or mobile number, in any usual form. The output
// names the user by ID only, never by the contact.
func findUser(ctx context.Context, database *sql.DB, value string) (string, error) {
	value = strings.TrimSpace(value)
	if id, err := uuid.Parse(value); err == nil {
		return id.String(), nil
	}
	normalized := contact.Normalize(value)
	contactType, _ := contact.Identify(normalized)
	if contactType == "" {
		return "", errors.New("-user must be a user ID, an email address, or a mobile number")
	}
	var userHK string
	err := database.QueryRowContext(ctx, `SELECT c.user_hk::text FROM user_contacts c WHERE `+db.ContactMatch,
		db.ContactArgs(contactType, normalized)...).Scan(&userHK)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("no account has that %s", contactType)
	}
	if err != nil {
		return "", fmt.Errorf("find the user: %w", err)
	}
	return userHK, nil
}
