package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/crypto/field"
	"github.com/mdastpak/crypto-exchange/internal/db"
	"github.com/mdastpak/crypto-exchange/internal/keys"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"
)

// rowSealArgs are the flags of the row-seals commands.
type rowSealArgs struct {
	all    bool
	limit  int
	reason string
}

// parseRowSealArgs parses the arguments after "row-seals".
func parseRowSealArgs(cmd command, args []string, stderr io.Writer) (command, error) {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return command{}, errors.New("a row-seals command is required: findings, reseal, or verify")
	}
	sub := flag.NewFlagSet("row-seals "+args[0], flag.ContinueOnError)
	sub.SetOutput(stderr)
	switch args[0] {
	case "findings":
		sub.BoolVar(&cmd.rowSeals.all, "all", false, "Include the resolved findings")
		sub.IntVar(&cmd.rowSeals.limit, "limit", 50, "How many findings to show, newest first")
	case "reseal":
		sub.Int64Var(&cmd.id, "id", 0, "ID of the finding, from row-seals findings")
		sub.StringVar(&cmd.rowSeals.reason, "reason", "", "Why the current values of the row are right, for the audit log")
	case "verify":
	default:
		fmt.Fprint(stderr, usage)
		return command{}, fmt.Errorf("unknown row-seals command %q", args[0])
	}
	if err := sub.Parse(args[1:]); err != nil {
		return command{}, err
	}
	if sub.NArg() > 0 {
		return command{}, fmt.Errorf("unexpected argument %q", sub.Arg(0))
	}
	cmd.name = "row-seals " + args[0]
	if args[0] == "reseal" {
		if cmd.id <= 0 {
			return command{}, errors.New("-id must be a positive number")
		}
		if strings.TrimSpace(cmd.rowSeals.reason) == "" {
			return command{}, errors.New("-reason is required")
		}
	}
	if args[0] == "findings" && (cmd.rowSeals.limit < 1 || cmd.rowSeals.limit > 1000) {
		return command{}, errors.New("-limit must be from 1 to 1000")
	}
	return cmd, nil
}

// runRowSeals runs a row-seals command with the row-mac keys that the
// server created.
func runRowSeals(cmd command, stdout, stderr io.Writer) int {
	postgres, err := db.NewPostgresService(dbOptions())
	if err != nil {
		fmt.Fprintln(stderr, "Error: connect to PostgreSQL:", err)
		return 1
	}
	defer postgres.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	if err := executeRowSeals(ctx, postgres.DB, cmd, stdout); err != nil {
		fmt.Fprintln(stderr, "Error:", err)
		return 1
	}
	return 0
}

// rowSealer returns a sealer with the data keys of the configuration, as
// the server has them, and makes it the sealer of the process, so that the
// other commands seal what they write. It returns nil, and no error,
// without keys.provider.
func rowSealer(ctx context.Context, conn *sql.DB, cmd command) (*rowseal.Sealer, error) {
	provider, err := keys.FromConfig(config.Config.Keys)
	if err != nil || provider == nil {
		return nil, err
	}
	if err := keys.CheckRelease(provider, config.Config.Server.Mode == "release"); err != nil {
		return nil, err
	}
	store := &keys.Store{DB: conn, Provider: provider, Origin: operator(cmd)}
	ring, err := store.LoadExisting(ctx)
	if err != nil {
		return nil, fmt.Errorf("load the data keys: %w", err)
	}
	return useRing(conn, ring), nil
}

// useRing makes a sealer of ring the sealer of the process, of its rows and,
// when ring has an audit-mac key, of its audit events (#337). The server
// creates the first audit-mac key; until it has started once, the events of
// the commands are written without a seal.
func useRing(conn *sql.DB, ring *keys.Keyring) *rowseal.Sealer {
	sealer := rowseal.New(conn, rowseal.KeyringKeys{Ring: ring}, config.Config.Keys.RowSeals.Mode)
	rowseal.Use(sealer)
	// The personal fields that the commands write, such as the address of
	// a new administrator, are encrypted as the server encrypts them (#140).
	field.Use(field.New(ring))
	if _, err := ring.Current(keys.PurposeAuditMAC); err == nil {
		audit.UseSealer(sealer)
	}
	return sealer
}

// executeRowSeals runs a parsed row-seals command against database.
func executeRowSeals(ctx context.Context, database *sql.DB, cmd command, stdout io.Writer) error {
	if cmd.name == "row-seals findings" {
		findings, err := rowseal.ListFindings(ctx, database, rowseal.ListOptions{All: cmd.rowSeals.all, Limit: cmd.rowSeals.limit})
		if err != nil {
			return err
		}
		if len(findings) == 0 {
			fmt.Fprintln(stdout, "No findings.")
			return nil
		}
		w := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tTABLE\tROW\tCHECK\tBY\tDETECTED\tSEEN\tRESOLVED")
		for _, f := range findings {
			resolved := "-"
			if f.ResolvedAt != nil {
				resolved = f.ResolvedAt.UTC().Format(time.RFC3339) + " " + f.Resolution
			}
			fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\t%d\t%s\n", f.ID, f.Table, f.RowKey, f.Check, f.DetectedBy,
				f.DetectedAt.UTC().Format(time.RFC3339), f.SeenCount, resolved)
		}
		return w.Flush()
	}

	sealer, err := rowSealer(ctx, database, cmd)
	if err != nil {
		return err
	}
	if sealer == nil {
		return errors.New("row seals need keys.provider (KEYS_PROVIDER, see docs/data-protection.md)")
	}
	switch cmd.name {
	case "row-seals reseal":
		f, err := sealer.ResealFinding(ctx, cmd.id, operator(cmd), cmd.rowSeals.reason)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Sealed %s %s again, and closed finding %d as %s.\n", f.Table, f.RowKey, f.ID, f.Resolution)
	case "row-seals verify":
		report, err := sealer.VerifyAll(ctx)
		if err != nil {
			return err
		}
		if report.Skipped {
			fmt.Fprintln(stdout, "Another instance is verifying now; try again later.")
			return nil
		}
		fmt.Fprintf(stdout, "Checked %d rows in %s mode: %d findings, %d sealed again in report mode, %d sealed with the current key.\n",
			report.Checked, sealer.Mode(), report.Findings, report.Resealed, report.Rotated)
	default:
		return fmt.Errorf("unknown command %q", cmd.name)
	}
	return nil
}
