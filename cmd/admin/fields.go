package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/crypto/field"
	"github.com/mdastpak/crypto-exchange/internal/db"
)

// parseFieldsArgs parses the arguments after "fields".
func parseFieldsArgs(cmd command, args []string, stderr io.Writer) (command, error) {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return command{}, errors.New("a fields command is required: status, rekey, or decrypt")
	}
	switch args[0] {
	case "status", "rekey", "decrypt":
	default:
		fmt.Fprint(stderr, usage)
		return command{}, fmt.Errorf("unknown fields command %q", args[0])
	}
	sub := flag.NewFlagSet("fields "+args[0], flag.ContinueOnError)
	sub.SetOutput(stderr)
	if err := sub.Parse(args[1:]); err != nil {
		return command{}, err
	}
	if sub.NArg() > 0 {
		return command{}, fmt.Errorf("unexpected argument %q", sub.Arg(0))
	}
	cmd.name = "fields " + args[0]
	return cmd, nil
}

// runFields runs a fields command with the data keys that the server
// created.
func runFields(cmd command, stdout, stderr io.Writer) int {
	postgres, err := db.NewPostgresService(dbOptions())
	if err != nil {
		fmt.Fprintln(stderr, "Error: connect to PostgreSQL:", err)
		return 1
	}
	defer postgres.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
	defer cancel()
	if err := executeFields(ctx, postgres.DB, cmd, stdout); err != nil {
		fmt.Fprintln(stderr, "Error:", err)
		return 1
	}
	return 0
}

// executeFields runs a parsed fields command against database.
func executeFields(ctx context.Context, database *sql.DB, cmd command, stdout io.Writer) error {
	sealer, err := rowSealer(ctx, database, cmd)
	if err != nil {
		return err
	}
	if sealer == nil {
		return errors.New("the personal fields need the data keys: set keys.provider (KEYS_PROVIDER, see docs/data-protection.md)")
	}
	switch cmd.name {
	case "fields status":
		sealing, indexing, err := field.Active().CurrentVersions()
		if err != nil {
			return err
		}
		status, err := db.ReadFieldsStatus(ctx, database)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Current keys: field-enc/v%d, blind-index/v%d.\n", sealing, indexing)
		fmt.Fprintf(stdout, "To encrypt, or to encrypt again with the current keys: %d contacts, %d users.\n",
			status.ContactsToRekey, status.UsersToRekey)
		fmt.Fprintf(stdout, "Stored encrypted only, without plaintext: %d contacts, %d users.\n",
			status.ContactsEncryptedOnly, status.UsersEncryptedOnly)
	case "fields rekey":
		report, err := db.RekeyFields(ctx, database)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Encrypted %d contacts and %d users with the current keys; %d rows skipped.\n",
			report.Contacts, report.Users, report.Skipped)
		printSkipped(stdout, report.Skipped)
	case "fields decrypt":
		report, err := db.DecryptFields(ctx, database)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Wrote the plaintext of %d contacts and %d users; %d rows skipped.\n",
			report.Contacts, report.Users, report.Skipped)
		printSkipped(stdout, report.Skipped)
	default:
		return fmt.Errorf("unknown command %q", cmd.name)
	}
	return nil
}

// printSkipped explains the rows that a fields command left as they are.
func printSkipped(stdout io.Writer, skipped int) {
	if skipped > 0 {
		fmt.Fprintln(stdout, "The skipped rows fail their seal check, or their ciphertext does not open: see admin row-seals findings and the log.")
	}
}
