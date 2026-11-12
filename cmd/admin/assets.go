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

	"github.com/mdastpak/crypto-exchange/internal/assetregistry"
	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/currencies"
	"github.com/mdastpak/crypto-exchange/internal/db"
)

// assetArgs are the flags of the assets commands.
type assetArgs struct {
	file   string
	dryRun bool
}

// parseAssetArgs parses the arguments after "assets".
func parseAssetArgs(cmd command, args []string, stderr io.Writer) (command, error) {
	if len(args) == 0 || args[0] != "import" {
		fmt.Fprint(stderr, usage)
		if len(args) == 0 {
			return command{}, errors.New("an assets command is required: import")
		}
		return command{}, fmt.Errorf("unknown assets command %q", args[0])
	}
	sub := flag.NewFlagSet("assets import", flag.ContinueOnError)
	sub.SetOutput(stderr)
	sub.StringVar(&cmd.assets.file, "file", "", "Registry file; assets.registry_path when left out")
	sub.BoolVar(&cmd.assets.dryRun, "dry-run", false, "Report the changes without making them")
	if err := sub.Parse(args[1:]); err != nil {
		return command{}, err
	}
	if sub.NArg() > 0 {
		return command{}, fmt.Errorf("unexpected argument %q", sub.Arg(0))
	}
	cmd.name = "assets import"
	return cmd, nil
}

// runAssets runs an assets command.
func runAssets(cmd command, stdout, stderr io.Writer) int {
	postgres, err := db.NewPostgresService(dbOptions())
	if err != nil {
		fmt.Fprintln(stderr, "Error: connect to PostgreSQL:", err)
		return 1
	}
	defer postgres.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := executeAssets(ctx, postgres.DB, cmd, stdout); err != nil {
		fmt.Fprintln(stderr, "Error:", err)
		return 1
	}
	return 0
}

// executeAssets imports the registry file into the currencies (#309).
func executeAssets(ctx context.Context, database *sql.DB, cmd command, stdout io.Writer) error {
	path := cmd.assets.file
	if path == "" {
		path = config.Config.Assets.RegistryPath
	}
	if path == "" {
		return errors.New("name the registry file with -file, or set assets.registry_path")
	}
	reg, err := assetregistry.Load(path)
	if err != nil {
		return err
	}
	// The audit events are sealed as the server seals them, when the
	// configuration has data keys.
	if _, err := rowSealer(ctx, database, cmd); err != nil {
		return err
	}
	report, err := currencies.Store{DB: database}.Import(ctx, reg, operator(cmd), cmd.assets.dryRun)
	if err != nil {
		return err
	}
	verb := "Imported"
	if cmd.assets.dryRun {
		verb = "Dry run, nothing written:"
	}
	fmt.Fprintf(stdout, "%s %s.\n", verb, path)
	fmt.Fprintf(stdout, "Currencies: %d created (%s), %d existing (%s).\n",
		len(report.Created), strings.Join(report.Created, ", "), len(report.Existing), strings.Join(report.Existing, ", "))
	fmt.Fprintf(stdout, "Networks: %d created, %d updated, %d unchanged.\n",
		report.Networks.Created, report.Networks.Updated, report.Networks.Unchanged)
	fmt.Fprintf(stdout, "Markets: %d created, %d updated, %d unchanged.\n",
		report.Markets.Created, report.Markets.Updated, report.Markets.Unchanged)
	for _, note := range report.Notes {
		fmt.Fprintln(stdout, "Note:", note)
	}
	return nil
}
