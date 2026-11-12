package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/db"
)

// migrateTimeout bounds "admin migrate". A migration of a large table can
// take long, but a release step must not hang forever; the migration lock
// wait (postgres.migration_lock_timeout_seconds) is part of it.
const migrateTimeout = 2 * time.Hour

// parseMigrateArgs parses the arguments after "migrate": "up", which
// "migrate" alone means too, or "status".
func parseMigrateArgs(cmd command, args []string, stderr io.Writer) (command, error) {
	name := "up"
	if len(args) > 0 {
		name, args = args[0], args[1:]
	}
	switch name {
	case "up", "status":
	default:
		fmt.Fprint(stderr, usage)
		return command{}, fmt.Errorf("unknown migrate command %q: use up or status", name)
	}
	sub := flag.NewFlagSet("migrate "+name, flag.ContinueOnError)
	sub.SetOutput(stderr)
	if err := sub.Parse(args); err != nil {
		return command{}, err
	}
	if sub.NArg() > 0 {
		return command{}, fmt.Errorf("unexpected argument %q", sub.Arg(0))
	}
	cmd.name = "migrate " + name
	return cmd, nil
}

// runMigrate runs a migrate command (#180).
//
// "migrate up" is the release step of postgres.migrations.mode "external":
// it applies the pending migrations with the migration role
// (postgres.migrations.user), under the migration lock, and grants the
// runtime role (postgres.user) its privileges. Run it once per release,
// before the instances of the release start; running it again, or from
// several places at once, is safe.
//
// "migrate status" prints the schema version next to the migrations of
// this build, and what the runtime role could change. It exits with 1 when
// the database is behind, dirty, or the role could change the schema, so a
// deployment can check it before it starts instances.
func runMigrate(cmd command, stdout, stderr io.Writer) int {
	ctx, cancel := context.WithTimeout(context.Background(), migrateTimeout)
	defer cancel()
	db.ApplicationName = "crypto-exchange-admin"

	if cmd.name == "migrate up" {
		if err := db.Migrate(ctx, dbOptions()); err != nil {
			fmt.Fprintln(stderr, "Error: migrate:", err)
			return 1
		}
	}

	// The status, and the result of up, as the runtime role sees them.
	postgres, err := db.ConnectPostgres(dbOptions())
	if err != nil {
		fmt.Fprintln(stderr, "Error: connect to PostgreSQL:", err)
		return 1
	}
	defer postgres.Close()
	state, err := db.ReadSchemaState(ctx, postgres.DB, config.MigrationsPath())
	if err != nil {
		fmt.Fprintln(stderr, "Error:", err)
		return 1
	}
	problems, err := db.RoleProblems(ctx, postgres.DB)
	if err != nil {
		fmt.Fprintln(stderr, "Error:", err)
		return 1
	}

	failed := false
	dirty := ""
	if state.Dirty {
		dirty, failed = " (dirty: a migration failed half-way)", true
	}
	fmt.Fprintf(stdout, "Schema version: %d%s\nMigrations of this build: up to %d\n", state.Version, dirty, state.Latest)
	switch {
	case state.Behind():
		fmt.Fprintln(stdout, "The database is behind: run \"admin migrate up\".")
		failed = true
	case state.Ahead():
		fmt.Fprintln(stdout, "The database is ahead of this build, as while a newer release rolls out.")
	default:
		if !state.Dirty {
			fmt.Fprintln(stdout, "The database is current.")
		}
	}
	if len(problems) == 0 {
		fmt.Fprintf(stdout, "Runtime role %s: cannot change the schema.\n", config.Config.Postgres.User)
	} else {
		fmt.Fprintf(stdout, "Runtime role %s: can change the schema, which release mode refuses:\n", config.Config.Postgres.User)
		for _, problem := range problems {
			fmt.Fprintln(stdout, "  "+problem)
		}
		if config.Config.Server.Mode == "release" {
			failed = true
		}
	}
	if failed {
		fmt.Fprintln(stderr, "Error:", errors.New("the database is not ready for this build"))
		return 1
	}
	return 0
}
