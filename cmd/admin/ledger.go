package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/db"
	"github.com/mdastpak/crypto-exchange/internal/ledger"
)

// errNotReconciled is the error of a reconciliation that found a problem,
// which exits with 1, for monitoring.
var errNotReconciled = errors.New("the ledger does not reconcile")

// parseLedgerArgs parses the arguments after "ledger".
func parseLedgerArgs(cmd command, args []string, stderr io.Writer) (command, error) {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return command{}, errors.New("a ledger command is required: reconcile or snapshot")
	}
	switch args[0] {
	case "reconcile", "snapshot":
	default:
		fmt.Fprint(stderr, usage)
		return command{}, fmt.Errorf("unknown ledger command %q", args[0])
	}
	sub := flag.NewFlagSet("ledger "+args[0], flag.ContinueOnError)
	sub.SetOutput(stderr)
	if err := sub.Parse(args[1:]); err != nil {
		return command{}, err
	}
	if sub.NArg() > 0 {
		return command{}, fmt.Errorf("unexpected argument %q", sub.Arg(0))
	}
	cmd.name = "ledger " + args[0]
	return cmd, nil
}

// runLedger runs a ledger command. It only reads the books, except that
// snapshot records snapshots.
func runLedger(cmd command, stdout, stderr io.Writer) int {
	postgres, err := db.NewPostgresService(dbOptions())
	if err != nil {
		fmt.Fprintln(stderr, "Error: connect to PostgreSQL:", err)
		return 1
	}
	defer postgres.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	if err := executeLedger(ctx, postgres.DB, cmd, stdout); err != nil {
		fmt.Fprintln(stderr, "Error:", err)
		return 1
	}
	return 0
}

// executeLedger runs a parsed ledger command against database.
func executeLedger(ctx context.Context, database *sql.DB, cmd command, stdout io.Writer) error {
	books := ledger.Ledger{DB: database}
	switch cmd.name {
	case "ledger snapshot":
		n, err := books.Snapshot(ctx)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Recorded %d snapshots of balances.\n", n)
		return nil
	case "ledger reconcile":
		// No source of custody holdings yet (#67): the books are checked
		// against themselves.
		report, err := books.Reconcile(ctx, nil)
		if err != nil {
			return err
		}
		printReconciliation(stdout, report)
		if !report.OK() {
			return errNotReconciled
		}
		return nil
	}
	return fmt.Errorf("unknown command %q", cmd.name)
}

// printReconciliation prints the books of each currency and every balance
// that differs from the replay of its changes.
func printReconciliation(stdout io.Writer, report ledger.Report) {
	w := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "CURRENCY\tUSERS\tPENDING\tFEES\tINVENTORY\tCUSTODY MUST HOLD\tIMBALANCE")
	for _, c := range report.Currencies {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", c.Symbol, c.Users, c.Pending, c.Accounts[ledger.AccountFees],
			c.Accounts[ledger.AccountInventory], c.Expected, c.Imbalance)
	}
	_ = w.Flush()
	fmt.Fprintf(stdout, "Replayed %d balances: %d differ from their changes.\n", report.Balances, len(report.Mismatches))
	for _, m := range report.Mismatches {
		fmt.Fprintf(stdout, "  balance %s: %s\n", m.BalanceID, m.Reason)
	}
	if report.OK() {
		fmt.Fprintln(stdout, "The ledger reconciles.")
	}
}
