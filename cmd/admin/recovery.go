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
	"github.com/mdastpak/crypto-exchange/internal/keys"
	"github.com/mdastpak/crypto-exchange/internal/recovery"
)

// DefaultRPO is the recovery point objective of production in
// docs/disaster-recovery.md, which backup status checks by default.
const DefaultRPO = 5 * time.Minute

// recoveryArgs are the flags of the backup and restore commands.
type recoveryArgs struct {
	rpo       time.Duration
	notBefore *time.Time
	notAfter  *time.Time
	keys      bool
}

// parseBackupArgs parses the arguments after "backup".
func parseBackupArgs(cmd command, args []string, stderr io.Writer) (command, error) {
	if len(args) == 0 || args[0] != "status" {
		fmt.Fprint(stderr, usage)
		return command{}, errors.New("a backup command is required: status")
	}
	sub := flag.NewFlagSet("backup status", flag.ContinueOnError)
	sub.SetOutput(stderr)
	sub.DurationVar(&cmd.recovery.rpo, "rpo", DefaultRPO, "The recovery point objective: the oldest the newest archived WAL may be")
	if err := sub.Parse(args[1:]); err != nil {
		return command{}, err
	}
	if sub.NArg() > 0 {
		return command{}, fmt.Errorf("unexpected argument %q", sub.Arg(0))
	}
	if cmd.recovery.rpo <= 0 {
		return command{}, errors.New("-rpo must be positive")
	}
	cmd.name = "backup status"
	return cmd, nil
}

// parseRestoreArgs parses the arguments after "restore".
func parseRestoreArgs(cmd command, args []string, stderr io.Writer) (command, error) {
	if len(args) == 0 || args[0] != "verify" {
		fmt.Fprint(stderr, usage)
		return command{}, errors.New("a restore command is required: verify")
	}
	sub := flag.NewFlagSet("restore verify", flag.ContinueOnError)
	sub.SetOutput(stderr)
	var notBefore, notAfter string
	sub.StringVar(&notBefore, "not-before", "", "The restore must have reached this time (RFC 3339)")
	sub.StringVar(&notAfter, "not-after", "", "The restore must stop before this time (RFC 3339)")
	sub.BoolVar(&cmd.recovery.keys, "keys", false, "Also unwrap every data key with the configured key provider")
	if err := sub.Parse(args[1:]); err != nil {
		return command{}, err
	}
	if sub.NArg() > 0 {
		return command{}, fmt.Errorf("unexpected argument %q", sub.Arg(0))
	}
	var err error
	if cmd.recovery.notBefore, err = optionalTime("-not-before", notBefore); err != nil {
		return command{}, err
	}
	if cmd.recovery.notAfter, err = optionalTime("-not-after", notAfter); err != nil {
		return command{}, err
	}
	if cmd.recovery.notBefore != nil && cmd.recovery.notAfter != nil && cmd.recovery.notAfter.Before(*cmd.recovery.notBefore) {
		return command{}, errors.New("-not-after must not be before -not-before")
	}
	cmd.name = "restore verify"
	return cmd, nil
}

func optionalTime(flagName, value string) (*time.Time, error) {
	if value == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return nil, fmt.Errorf("%s must be an RFC 3339 time, such as 2026-11-08T10:00:00Z", flagName)
	}
	return &t, nil
}

// runRecovery runs backup status or restore verify. Neither migrates or
// changes the database. The exit status is 0 when everything is in order,
// 1 when a check failed or could not run, so a cron job or a monitoring
// probe can alert on it.
func runRecovery(cmd command, stdout, stderr io.Writer) int {
	var keyStore *keys.Store
	if cmd.recovery.keys {
		provider, err := keys.FromConfig(config.Config.Keys)
		if err == nil && provider == nil {
			err = errors.New("-keys needs keys.provider (KEYS_PROVIDER); see docs/data-protection.md")
		}
		if err != nil {
			fmt.Fprintln(stderr, "Error:", err)
			return 1
		}
		keyStore = &keys.Store{Provider: provider}
	}

	postgres, err := db.ConnectPostgres()
	if err != nil {
		fmt.Fprintln(stderr, "Error: connect to PostgreSQL:", err)
		return 1
	}
	defer postgres.Close()
	if keyStore != nil {
		keyStore.DB = postgres.DB
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	ok, err := executeRecovery(ctx, postgres, keyStore, config.MigrationsPath(), cmd, stdout)
	if err != nil {
		fmt.Fprintln(stderr, "Error:", err)
		return 1
	}
	if !ok {
		return 1
	}
	return 0
}

// executeRecovery runs a parsed backup or restore command, and reports
// whether every check passed.
func executeRecovery(ctx context.Context, postgres *db.PostgresService, keyStore *keys.Store, migrations string, cmd command, stdout io.Writer) (bool, error) {
	switch cmd.name {
	case "backup status":
		archiving, err := recovery.ReadArchiving(ctx, postgres.DB)
		if err != nil {
			return false, err
		}
		if err := archiving.Report(stdout); err != nil {
			return false, err
		}
		problems := archiving.Problems(cmd.recovery.rpo)
		for _, p := range problems {
			fmt.Fprintln(stdout, "FAIL", p)
		}
		if len(problems) == 0 {
			fmt.Fprintf(stdout, "OK: WAL archiving meets the RPO of %v.\n", cmd.recovery.rpo)
		}
		return len(problems) == 0, nil
	case "restore verify":
		schema, err := recovery.LatestMigration(migrations)
		if err != nil {
			return false, err
		}
		opts := recovery.Options{Schema: schema, NotBefore: cmd.recovery.notBefore, NotAfter: cmd.recovery.notAfter}
		if keyStore != nil {
			opts.Keys = keyStore.Verify
		}
		report, err := recovery.Verify(ctx, postgres.DB, opts)
		if err != nil && !errors.Is(err, recovery.ErrNotVerified) {
			return false, err
		}
		if err := report.Print(stdout); err != nil {
			return false, err
		}
		if err != nil {
			fmt.Fprintln(stdout, "FAIL: the restored database failed verification; do not put it in service.")
			return false, nil
		}
		fmt.Fprintln(stdout, "OK: the restored database passed every check.")
		return true, nil
	default:
		return false, fmt.Errorf("unknown command %q", cmd.name)
	}
}
