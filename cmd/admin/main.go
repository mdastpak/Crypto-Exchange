// Command admin manages administrator accounts and the data-protection keys.
// It works on the database directly, so creating the first administrator
// needs no HTTP endpoint.
//
// Usage:
//
//	admin [-config path] [-migrations url] create -email <address>
//	admin [-config path] [-migrations url] reset-totp -email <address>
//	admin [-config path] [-migrations url] disable -email <address>
//	admin [-config path] [-migrations url] keys list
//	admin [-config path] [-migrations url] keys rotate -purpose <purpose>
//	admin [-config path] [-migrations url] keys retire -purpose <purpose> -version <n>
//	admin [-config path] [-migrations url] keys rewrap
//
// create and reset-totp print the authenticator secret once. The server
// needs admin.totp_key (or ADMIN_TOTP_KEY) set to the same value. The keys
// commands need the keys section of the server's configuration and never
// print a key; docs/data-protection.md describes when to run them.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/adminauth"
	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/db"
	"github.com/mdastpak/crypto-exchange/internal/keys"
)

const usage = `Usage: admin [-config path] [-migrations url] <command> [flags]

Administrators:
  create -email <address>      Create an administrator and print its authenticator secret.
  reset-totp -email <address>  Replace the authenticator secret of an administrator.
  disable -email <address>     Stop an administrator from signing in.

Data-protection keys (purposes row-mac, field-enc, blind-index):
  keys list                                    List the keys, never their values.
  keys rotate -purpose <purpose>               Create the next version of a key.
  keys retire -purpose <purpose> -version <n>  Stop loading an old version.
  keys rewrap                                  Wrap every key again after the KEK was rotated.
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
}

func parseArgs(args []string, stderr io.Writer) (command, error) {
	var cmd command
	global := flag.NewFlagSet("admin", flag.ContinueOnError)
	global.SetOutput(stderr)
	global.Usage = func() { fmt.Fprint(stderr, usage) }
	global.StringVar(&cmd.configPath, "config", "config/config.yaml", "Path to config file")
	global.StringVar(&cmd.migrationsPath, "migrations", "file://internal/db/migrations", "Path to migrations")
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
	if strings.HasPrefix(cmd.name, "keys ") {
		return runKeys(cmd, stdout, stderr)
	}
	if config.Config.Admin.TOTPKey == "" {
		fmt.Fprintln(stderr, "Error: admin.totp_key (or ADMIN_TOTP_KEY) is required; generate one with: openssl rand -base64 32")
		return 1
	}
	key, err := adminauth.DecodeKey(config.Config.Admin.TOTPKey)
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
	accounts, err := adminauth.NewService(postgres.DB, key, config.Config.Exchange.Name)
	if err != nil {
		fmt.Fprintln(stderr, "Error:", err)
		return 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := execute(ctx, accounts, cmd, stdout); err != nil {
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
	default:
		return fmt.Errorf("unknown command %q", cmd.name)
	}
	return nil
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
	if err := executeKeys(ctx, &keys.Store{DB: postgres.DB, Provider: provider}, cmd, stdout); err != nil {
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
		fmt.Fprintln(w, "PURPOSE\tVERSION\tSTATE\tKEK\tCREATED\tRETIRED")
		for _, info := range infos {
			state, retired := "readable", "-"
			if info.Current {
				state = "current"
			}
			if info.RetiredAt != nil {
				state, retired = "retired", info.RetiredAt.UTC().Format(time.RFC3339)
			}
			fmt.Fprintf(w, "%s\t%d\t%s\t%s\t%s\t%s\n", info.Purpose, info.Version, state, info.KEKID,
				info.CreatedAt.UTC().Format(time.RFC3339), retired)
		}
		return w.Flush()
	case "keys rotate":
		version, err := store.Rotate(ctx, cmd.purpose)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Created data key %s/v%d. Each server instance uses it after its next restart; older versions stay readable until they are retired.\n", cmd.purpose, version)
	case "keys retire":
		if err := store.Retire(ctx, cmd.purpose, cmd.version); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Retired data key %s/v%d. Server instances stop loading it at their next restart.\n", cmd.purpose, cmd.version)
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

func printEnrollment(stdout io.Writer, enrollment adminauth.Enrollment) {
	fmt.Fprintln(stdout, "Add the account to an authenticator app now. The secret is not shown again.")
	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, "  "+enrollment.URI)
	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, "Manual entry key:", enrollment.Secret)
}
