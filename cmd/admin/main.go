// Command admin manages administrator accounts. It works on the database
// directly, so creating the first administrator needs no HTTP endpoint.
//
// Usage:
//
//	admin [-config path] [-migrations url] create -email <address>
//	admin [-config path] [-migrations url] reset-totp -email <address>
//	admin [-config path] [-migrations url] disable -email <address>
//
// create and reset-totp print the authenticator secret once. The server
// needs admin.totp_key (or ADMIN_TOTP_KEY) set to the same value.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"encrypted-db/config"
	"encrypted-db/internal/adminauth"
	"encrypted-db/internal/db"
)

const usage = `Usage: admin [-config path] [-migrations url] <command> -email <address>

Commands:
  create      Create an administrator and print its authenticator secret.
  reset-totp  Replace the authenticator secret of an administrator.
  disable     Stop an administrator from signing in.
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// command is a parsed command line.
type command struct {
	configPath     string
	migrationsPath string
	name           string
	email          string
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

func run(args []string, stdout, stderr io.Writer) int {
	cmd, err := parseArgs(args, stderr)
	if err != nil {
		fmt.Fprintln(stderr, "Error:", err)
		return 2
	}

	config.SetConfigPath(cmd.configPath)
	config.SetMigrationsPath(cmd.migrationsPath)
	config.LoadConfig()
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
		fmt.Fprintf(stdout, "Disabled administrator %s. Admin tokens issued earlier stay valid until they expire.\n", cmd.email)
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
