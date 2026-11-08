// Command admin manages administrator accounts and the data-protection keys.
// It works on the database directly, so creating the first administrator
// needs no HTTP endpoint.
//
// Usage:
//
//	admin [-config path] [-migrations url] create -email <address>
//	admin [-config path] [-migrations url] reset-totp -email <address>
//	admin [-config path] [-migrations url] disable -email <address>
//	admin [-config path] [-migrations url] totp-secrets status
//	admin [-config path] [-migrations url] totp-secrets reseal -to <data-keys|legacy>
//	admin [-config path] [-migrations url] keys list
//	admin [-config path] [-migrations url] keys rotate -purpose <purpose>
//	admin [-config path] [-migrations url] keys retire -purpose <purpose> -version <n>
//	admin [-config path] [-migrations url] keys rewrap
//	admin [-config path] [-migrations url] outbox list
//	admin [-config path] [-migrations url] outbox retry -id <n>
//	admin [-config path] [-migrations url] outbox discard -id <n>
//	admin [-config path] [-migrations url] platform status
//	admin [-config path] [-migrations url] platform maintenance -reason <text> [flags]
//	admin [-config path] [-migrations url] platform lockdown -feature <feature> -reason <text> [flags]
//	admin [-config path] [-migrations url] platform lift -id <uuid> -note <text>
//	admin [-config path] backup status [-rpo 5m]
//	admin [-config path] [-migrations url] restore verify [-not-before t] [-not-after t] [-keys]
//	admin [-config path] [-migrations url] migrate [up|status]
//
// create and reset-totp print the authenticator secret once. They seal it
// as the server does (#146): with a field-enc data key when keys.provider is
// set, or with admin.totp_key (ADMIN_TOTP_KEY) while it is set and
// keys.legacy_secrets is "write". totp-secrets reseal moves every stored
// secret to one of the two, unchanged, so enrolled apps keep working. The keys
// commands need the keys section of the server's configuration and never
// print a key; docs/data-protection.md describes when to run them. The outbox
// commands handle the messages that the server could not publish (#192).
// The platform commands put the service under maintenance or lock a
// feature (#235) without the HTTP API, for when it is unhealthy; every
// server instance applies a change within platform.poll_interval_ms.
// backup status and restore verify check WAL archiving and a restored
// database (#236); they only read, never migrate, and exit with 1 when a
// check fails, for monitoring and restore drills. migrate applies the
// migrations with the migration role and grants the runtime role (#180),
// as the release step of postgres.migrations.mode "external".
//
// Every change is recorded in the audit log in its transaction (#195), with
// the system as the actor and, in the user agent, the operating-system user,
// the host, and the name given with -operator.
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/user"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/adminauth"
	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/db"
	"github.com/mdastpak/crypto-exchange/internal/keys"
	"github.com/mdastpak/crypto-exchange/internal/outbox"
	"github.com/mdastpak/crypto-exchange/internal/platform"
)

const usage = `Usage: admin [-config path] [-migrations url] [-operator name] <command> [flags]

-operator names the person who runs the command in the audit log, together
with the operating-system user and the host.

Administrators:
  create -email <address>      Create an administrator and print its authenticator secret.
  reset-totp -email <address>  Replace the authenticator secret of an administrator.
  disable -email <address>     Stop an administrator from signing in.
  totp-secrets status          Count the authenticator secrets sealed with admin.totp_key and with the data keys.
  totp-secrets reseal -to <data-keys|legacy>
                               Seal every authenticator secret again with the data keys, once every instance
                               runs a release that reads them, or with admin.totp_key, before a rollback.

Data-protection keys (purposes row-mac, field-enc, blind-index):
  keys list                                    List the keys, never their values.
  keys rotate -purpose <purpose>               Create the next version of a key.
  keys retire -purpose <purpose> -version <n>  Stop loading an old version.
  keys rewrap                                  Wrap every key again after the KEK was rotated.

Outbox (messages the server could not publish):
  outbox list            Show the pending messages and list the dead letters.
  outbox retry -id <n>   Publish a dead letter again, after the later messages of its topic.
  outbox discard -id <n> Delete a dead letter, which is then never published.

Platform controls (every instance applies a change within platform.poll_interval_ms):
  platform status                          Show what users see, and the controls that apply now or later.
  platform maintenance -reason <text>      Close the whole service to users.
  platform lockdown -feature <f> -reason <text>
                                           Close one feature: sign_up, sign_in, profile_changes, deposits,
                                           withdrawals, trading, api_keys, websocket, read_only, scheduler, outbox.
    Both take -starts-at and -ends-at (RFC 3339), and -message-fa and -message-en for users;
    maintenance also takes -allow-networks and -allow-users (comma-separated).
  platform lift -id <uuid> -note <text>    End a control now, or cancel one that has not started.

Backups and restores (read only; exit status 1 when a check fails; see docs/disaster-recovery.md):
  backup status [-rpo 5m]                  Check WAL archiving on the primary against the RPO.
  restore verify [-not-before t] [-not-after t] [-keys]
                                           Check a restored database before it is put in service.

Schema (see docs/configuration.md#database-roles):
  migrate [up]       Apply the migrations with postgres.migrations.user, and grant postgres.user
                     its privileges. Run it once per release, before the new instances start.
  migrate status     Show the schema version and what postgres.user could change; exit status 1
                     when the database is behind or dirty, or, in release mode, the role could
                     change the schema.
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// command is a parsed command line. The keys commands are named "keys list",
// "keys rotate", and so on.
type command struct {
	configPath     string
	migrationsPath string
	name           string
	email          string
	purpose        keys.Purpose
	version        int
	id             int64
	operator       string
	// control is the control of platform maintenance and lockdown;
	// controlID and note are those of platform lift.
	control   platform.New
	controlID string
	note      string
	// recovery holds the flags of backup status and restore verify.
	recovery recoveryArgs
	// format is the -to of totp-secrets reseal.
	format adminauth.Format
}

func parseArgs(args []string, stderr io.Writer) (command, error) {
	var cmd command
	global := flag.NewFlagSet("admin", flag.ContinueOnError)
	global.SetOutput(stderr)
	global.Usage = func() { fmt.Fprint(stderr, usage) }
	global.StringVar(&cmd.configPath, "config", "config/config.yaml", "Path to config file")
	global.StringVar(&cmd.migrationsPath, "migrations", "file://internal/db/migrations", "Path to migrations")
	global.StringVar(&cmd.operator, "operator", "", "Name of the person who runs the command, for the audit log")
	if err := global.Parse(args); err != nil {
		return command{}, err
	}
	if global.NArg() == 0 {
		global.Usage()
		return command{}, errors.New("a command is required")
	}

	cmd.name = global.Arg(0)
	switch cmd.name {
	case "create", "reset-totp", "disable":
	case "keys":
		return parseKeysArgs(cmd, global.Args()[1:], stderr)
	case "outbox":
		return parseOutboxArgs(cmd, global.Args()[1:], stderr)
	case "platform":
		return parsePlatformArgs(cmd, global.Args()[1:], stderr)
	case "backup":
		return parseBackupArgs(cmd, global.Args()[1:], stderr)
	case "restore":
		return parseRestoreArgs(cmd, global.Args()[1:], stderr)
	case "migrate":
		return parseMigrateArgs(cmd, global.Args()[1:], stderr)
	case "totp-secrets":
		return parseTOTPSecretsArgs(cmd, global.Args()[1:], stderr)
	default:
		global.Usage()
		return command{}, fmt.Errorf("unknown command %q", cmd.name)
	}
	sub := flag.NewFlagSet(cmd.name, flag.ContinueOnError)
	sub.SetOutput(stderr)
	sub.StringVar(&cmd.email, "email", "", "Email address of the administrator")
	if err := sub.Parse(global.Args()[1:]); err != nil {
		return command{}, err
	}
	if cmd.email == "" {
		return command{}, errors.New("-email is required")
	}
	return cmd, nil
}

// parseKeysArgs parses the arguments after "keys".
func parseKeysArgs(cmd command, args []string, stderr io.Writer) (command, error) {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return command{}, errors.New("a keys command is required: list, rotate, retire, or rewrap")
	}
	sub := flag.NewFlagSet("keys "+args[0], flag.ContinueOnError)
	sub.SetOutput(stderr)
	var purpose string
	var needPurpose, needVersion bool
	switch args[0] {
	case "list", "rewrap":
	case "rotate":
		needPurpose = true
	case "retire":
		needPurpose, needVersion = true, true
	default:
		fmt.Fprint(stderr, usage)
		return command{}, fmt.Errorf("unknown keys command %q", args[0])
	}
	if needPurpose {
		sub.StringVar(&purpose, "purpose", "", "Purpose of the key: "+purposeList())
	}
	if needVersion {
		sub.IntVar(&cmd.version, "version", 0, "Version of the key")
	}
	if err := sub.Parse(args[1:]); err != nil {
		return command{}, err
	}
	if sub.NArg() > 0 {
		return command{}, fmt.Errorf("unexpected argument %q", sub.Arg(0))
	}
	cmd.name = "keys " + args[0]
	cmd.purpose = keys.Purpose(purpose)
	if needPurpose && !cmd.purpose.Valid() {
		return command{}, fmt.Errorf("-purpose must be one of %s", purposeList())
	}
	if needVersion && cmd.version <= 0 {
		return command{}, errors.New("-version must be a positive number")
	}
	return cmd, nil
}

// parseTOTPSecretsArgs parses the arguments after "totp-secrets".
func parseTOTPSecretsArgs(cmd command, args []string, stderr io.Writer) (command, error) {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return command{}, errors.New("a totp-secrets command is required: status or reseal")
	}
	sub := flag.NewFlagSet("totp-secrets "+args[0], flag.ContinueOnError)
	sub.SetOutput(stderr)
	var to string
	switch args[0] {
	case "status":
	case "reseal":
		sub.StringVar(&to, "to", "", "Keys to seal with: data-keys or legacy (admin.totp_key)")
	default:
		fmt.Fprint(stderr, usage)
		return command{}, fmt.Errorf("unknown totp-secrets command %q", args[0])
	}
	if err := sub.Parse(args[1:]); err != nil {
		return command{}, err
	}
	if sub.NArg() > 0 {
		return command{}, fmt.Errorf("unexpected argument %q", sub.Arg(0))
	}
	cmd.name = "totp-secrets " + args[0]
	if args[0] == "reseal" {
		cmd.format = adminauth.Format(to)
		if cmd.format != adminauth.FormatDataKeys && cmd.format != adminauth.FormatLegacy {
			return command{}, fmt.Errorf("-to must be %s or %s", adminauth.FormatDataKeys, adminauth.FormatLegacy)
		}
	}
	return cmd, nil
}

// parseOutboxArgs parses the arguments after "outbox".
func parseOutboxArgs(cmd command, args []string, stderr io.Writer) (command, error) {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return command{}, errors.New("an outbox command is required: list, retry, or discard")
	}
	sub := flag.NewFlagSet("outbox "+args[0], flag.ContinueOnError)
	sub.SetOutput(stderr)
	needID := false
	switch args[0] {
	case "list":
	case "retry", "discard":
		needID = true
		sub.Int64Var(&cmd.id, "id", 0, "ID of the dead letter, from outbox list")
	default:
		fmt.Fprint(stderr, usage)
		return command{}, fmt.Errorf("unknown outbox command %q", args[0])
	}
	if err := sub.Parse(args[1:]); err != nil {
		return command{}, err
	}
	if sub.NArg() > 0 {
		return command{}, fmt.Errorf("unexpected argument %q", sub.Arg(0))
	}
	if needID && cmd.id <= 0 {
		return command{}, errors.New("-id must be a positive number")
	}
	cmd.name = "outbox " + args[0]
	return cmd, nil
}

func purposeList() string {
	names := make([]string, len(keys.Purposes))
	for i, p := range keys.Purposes {
		names[i] = string(p)
	}
	return strings.Join(names, ", ")
}

func run(args []string, stdout, stderr io.Writer) int {
	cmd, err := parseArgs(args, stderr)
	if err != nil {
		fmt.Fprintln(stderr, "Error:", err)
		return 2
	}

	config.SetConfigPath(cmd.configPath)
	config.SetMigrationsPath(cmd.migrationsPath)
	config.LoadConfig()
	db.ApplicationName = "crypto-exchange-admin"
	if strings.HasPrefix(cmd.name, "keys ") {
		return runKeys(cmd, stdout, stderr)
	}
	if strings.HasPrefix(cmd.name, "outbox ") {
		return runOutbox(cmd, stdout, stderr)
	}
	if strings.HasPrefix(cmd.name, "platform ") {
		return runPlatform(cmd, stdout, stderr)
	}
	if strings.HasPrefix(cmd.name, "backup ") || strings.HasPrefix(cmd.name, "restore ") {
		return runRecovery(cmd, stdout, stderr)
	}
	if strings.HasPrefix(cmd.name, "migrate ") {
		return runMigrate(cmd, stdout, stderr)
	}
	if config.Config.Admin.TOTPKey == "" && config.Config.Keys.Provider == "" {
		fmt.Fprintln(stderr, "Error: authenticator secrets need keys.provider (KEYS_PROVIDER, see docs/data-protection.md) or admin.totp_key (ADMIN_TOTP_KEY)")
		return 1
	}

	postgres, err := db.NewPostgresService()
	if err != nil {
		fmt.Fprintln(stderr, "Error: connect to PostgreSQL:", err)
		return 1
	}
	defer postgres.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	secrets, err := totpSecrets(ctx, postgres.DB, cmd)
	if err != nil {
		fmt.Fprintln(stderr, "Error:", err)
		return 1
	}
	accounts, err := adminauth.NewService(postgres.DB, secrets, config.Config.Exchange.Name)
	if err != nil {
		fmt.Fprintln(stderr, "Error:", err)
		return 1
	}
	accounts.Origin = operator(cmd)

	if cmd.name == "totp-secrets status" {
		err = printTOTPSecrets(ctx, accounts, secrets, stdout)
	} else {
		err = execute(ctx, accounts, cmd, stdout)
	}
	if err != nil {
		fmt.Fprintln(stderr, "Error:", err)
		return 1
	}
	return 0
}

// execute runs a parsed command against accounts.
func execute(ctx context.Context, accounts *adminauth.Service, cmd command, stdout io.Writer) error {
	switch cmd.name {
	case "create":
		enrollment, err := accounts.Create(ctx, cmd.email)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Created administrator %s (ID %s).\n", enrollment.Email, enrollment.AdminID)
		printEnrollment(stdout, enrollment)
	case "reset-totp":
		enrollment, err := accounts.ResetTOTP(ctx, cmd.email)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Replaced the authenticator secret of %s. The old secret no longer works.\n", enrollment.Email)
		printEnrollment(stdout, enrollment)
	case "disable":
		if err := accounts.Disable(ctx, cmd.email); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Disabled administrator %s. Admin tokens issued earlier are rejected from their next request on.\n", cmd.email)
	case "totp-secrets reseal":
		changed, err := accounts.Reseal(ctx, cmd.format)
		fmt.Fprintf(stdout, "Sealed %d authenticator secret(s) again with %s.\n", changed, formatName(cmd.format))
		if err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown command %q", cmd.name)
	}
	return nil
}

// totpSecrets returns the keys that seal authenticator secrets, as the
// server configures them: admin.totp_key when it is set, and the data keys,
// loaded once, when keys.provider is set and the server has created them.
func totpSecrets(ctx context.Context, conn *sql.DB, cmd command) (adminauth.Secrets, error) {
	secrets := adminauth.Secrets{LegacyWrites: config.Config.Keys.WritesWithLegacySecrets()}
	if config.Config.Admin.TOTPKey != "" {
		key, err := adminauth.DecodeKey(config.Config.Admin.TOTPKey)
		if err != nil {
			return secrets, err
		}
		secrets.LegacyKey = key
	}
	provider, err := keys.FromConfig(config.Config.Keys)
	if err != nil || provider == nil {
		return secrets, err
	}
	if err := keys.CheckRelease(provider, config.Config.Server.Mode == "release"); err != nil {
		return secrets, err
	}
	// Like the keys commands, it creates no keys: the server does on its
	// first start. Until then, admin.totp_key seals alone.
	store := &keys.Store{DB: conn, Provider: provider, Origin: operator(cmd)}
	ring, err := store.LoadExisting(ctx)
	if errors.Is(err, keys.ErrNoKey) && secrets.LegacyKey != nil {
		return secrets, nil
	}
	if err != nil {
		return secrets, fmt.Errorf("load the data keys: %w", err)
	}
	secrets.DataKeys = ring
	return secrets, nil
}

// printTOTPSecrets prints how the authenticator secrets are sealed, and with
// what new ones will be.
func printTOTPSecrets(ctx context.Context, accounts *adminauth.Service, secrets adminauth.Secrets, stdout io.Writer) error {
	counts, err := accounts.CountSecrets(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Sealed with admin.totp_key: %d\n", counts.Legacy)
	fmt.Fprintf(stdout, "Sealed with the data keys:  %d\n", counts.DataKeys)
	if counts.Unknown > 0 {
		fmt.Fprintf(stdout, "In an unknown format:       %d (enroll them again with reset-totp)\n", counts.Unknown)
	}
	fmt.Fprintf(stdout, "New secrets are sealed with %s.\n", formatName(secrets.WriteFormat()))
	if counts.Legacy == 0 && secrets.LegacyKey != nil && secrets.WriteFormat() == adminauth.FormatDataKeys {
		fmt.Fprintln(stdout, "No secret uses admin.totp_key anymore: it can be removed from the configuration.")
	}
	return nil
}

func formatName(format adminauth.Format) string {
	if format == adminauth.FormatLegacy {
		return "admin.totp_key"
	}
	return "the data keys"
}

// runKeys runs a keys command with the provider of the configuration.
func runKeys(cmd command, stdout, stderr io.Writer) int {
	provider, err := keys.FromConfig(config.Config.Keys)
	if err == nil && provider == nil {
		err = errors.New("keys.provider (KEYS_PROVIDER) is not set; see docs/data-protection.md")
	}
	if err == nil {
		err = keys.CheckRelease(provider, config.Config.Server.Mode == "release")
	}
	if err != nil {
		fmt.Fprintln(stderr, "Error:", err)
		return 1
	}

	postgres, err := db.NewPostgresService()
	if err != nil {
		fmt.Fprintln(stderr, "Error: connect to PostgreSQL:", err)
		return 1
	}
	defer postgres.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	store := &keys.Store{DB: postgres.DB, Provider: provider, Origin: operator(cmd),
		ActivationDelay: time.Duration(config.Config.Keys.ActivationDelaySeconds) * time.Second}
	if err := executeKeys(ctx, store, cmd, stdout); err != nil {
		fmt.Fprintln(stderr, "Error:", err)
		return 1
	}
	return 0
}

// executeKeys runs a parsed keys command against store.
func executeKeys(ctx context.Context, store *keys.Store, cmd command, stdout io.Writer) error {
	switch cmd.name {
	case "keys list":
		infos, err := store.List(ctx)
		if err != nil {
			return err
		}
		if len(infos) == 0 {
			fmt.Fprintln(stdout, "There are no data keys yet. The server creates them on its first start.")
			return nil
		}
		w := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "PURPOSE\tVERSION\tSTATE\tKEK\tCREATED\tACTIVE FROM\tRETIRED")
		now := time.Now()
		for _, info := range infos {
			state, retired := "readable", "-"
			switch {
			case info.RetiredAt != nil:
				state, retired = "retired", info.RetiredAt.UTC().Format(time.RFC3339)
			case info.Current:
				state = "current"
			case info.ActiveFrom.After(now):
				state = "pending"
			}
			fmt.Fprintf(w, "%s\t%d\t%s\t%s\t%s\t%s\t%s\n", info.Purpose, info.Version, state, info.KEKID,
				info.CreatedAt.UTC().Format(time.RFC3339), info.ActiveFrom.UTC().Format(time.RFC3339), retired)
		}
		return w.Flush()
	case "keys rotate":
		version, err := store.Rotate(ctx, cmd.purpose)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Created data key %s/v%d. Every server instance can read with it after its next reload (keys.reload_interval_seconds), and writes with it from %s, after keys.activation_delay_seconds; older versions stay readable until they are retired.\n",
			cmd.purpose, version, time.Now().Add(store.ActivationDelay).UTC().Format(time.RFC3339))
	case "keys retire":
		if err := store.Retire(ctx, cmd.purpose, cmd.version); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Retired data key %s/v%d. Server instances drop it at their next reload.\n", cmd.purpose, cmd.version)
	case "keys rewrap":
		count, err := store.Rewrap(ctx, store.Provider)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Wrapped %d data keys again with the newest version of %s. The keys themselves did not change.\n", count, store.Provider.ID())
	default:
		return fmt.Errorf("unknown command %q", cmd.name)
	}
	return nil
}

// runOutbox runs an outbox command.
func runOutbox(cmd command, stdout, stderr io.Writer) int {
	postgres, err := db.NewPostgresService()
	if err != nil {
		fmt.Fprintln(stderr, "Error: connect to PostgreSQL:", err)
		return 1
	}
	defer postgres.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := executeOutbox(ctx, postgres.DB, cmd, stdout); err != nil {
		fmt.Fprintln(stderr, "Error:", err)
		return 1
	}
	return 0
}

// deadLetterListLimit bounds the dead letters that outbox list prints.
const deadLetterListLimit = 100

// executeOutbox runs a parsed outbox command against database.
func executeOutbox(ctx context.Context, database *sql.DB, cmd command, stdout io.Writer) error {
	switch cmd.name {
	case "outbox list":
		stats, err := outbox.ReadStats(ctx, database)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "%d messages pending", stats.Pending)
		if stats.Pending > 0 {
			fmt.Fprintf(stdout, ", the oldest for %v", stats.OldestPending.Round(time.Second))
		}
		fmt.Fprintf(stdout, "; %d dead letters.\n", stats.Failed)
		letters, err := outbox.ListDeadLetters(ctx, database, deadLetterListLimit)
		if err != nil || len(letters) == 0 {
			return err
		}
		w := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tTOPIC\tATTEMPTS\tCREATED\tFAILED\tLAST ERROR")
		for _, d := range letters {
			fmt.Fprintf(w, "%d\t%s\t%d\t%s\t%s\t%s\n", d.ID, d.Topic, d.Attempts,
				d.CreatedAt.UTC().Format(time.RFC3339), d.FailedAt.UTC().Format(time.RFC3339), oneLine(d.LastError))
		}
		if err := w.Flush(); err != nil {
			return err
		}
		if stats.Failed > len(letters) {
			fmt.Fprintf(stdout, "Only the oldest %d are listed.\n", len(letters))
		}
	case "outbox retry":
		if err := outbox.RetryDeadLetter(ctx, database, cmd.id, operator(cmd)); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Message %d is pending again. The relay publishes it within seconds, after the later messages of its topic that were published meanwhile.\n", cmd.id)
	case "outbox discard":
		if err := outbox.DiscardDeadLetter(ctx, database, cmd.id, operator(cmd)); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Deleted message %d. It is never published.\n", cmd.id)
	default:
		return fmt.Errorf("unknown command %q", cmd.name)
	}
	return nil
}

// operator is the origin of the changes of cmd in the audit log: the
// system, with the command, -operator, and the user and host it runs as.
func operator(cmd command) audit.Event {
	osUser := ""
	if u, err := user.Current(); err == nil {
		osUser = u.Username
	}
	host, _ := os.Hostname()
	return audit.Operator("cmd/admin "+cmd.name, cmd.operator, osUser, host)
}

// oneLine keeps an error on one line of the table.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func printEnrollment(stdout io.Writer, enrollment adminauth.Enrollment) {
	fmt.Fprintln(stdout, "Add the account to an authenticator app now. The secret is not shown again.")
	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, "  "+enrollment.URI)
	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, "Manual entry key:", enrollment.Secret)
}
