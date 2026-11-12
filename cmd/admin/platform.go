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

	"github.com/mdastpak/crypto-exchange/internal/db"
	"github.com/mdastpak/crypto-exchange/internal/platform"
)

// parsePlatformArgs parses the arguments after "platform".
func parsePlatformArgs(cmd command, args []string, stderr io.Writer) (command, error) {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return command{}, errors.New("a platform command is required: status, maintenance, lockdown, or lift")
	}
	sub := flag.NewFlagSet("platform "+args[0], flag.ContinueOnError)
	sub.SetOutput(stderr)
	var feature, startsAt, endsAt, networks, users string
	switch args[0] {
	case "status":
	case "maintenance", "lockdown":
		if args[0] == "lockdown" {
			sub.StringVar(&feature, "feature", "", "The feature to close")
		} else {
			sub.StringVar(&networks, "allow-networks", "", "Comma-separated addresses or CIDR networks that may still use the service")
			sub.StringVar(&users, "allow-users", "", "Comma-separated UUIDs of users who may still use the service")
		}
		sub.StringVar(&cmd.control.Reason, "reason", "", "Why, for administrators")
		sub.StringVar(&startsAt, "starts-at", "", "When it starts (RFC 3339); now when empty")
		sub.StringVar(&endsAt, "ends-at", "", "When it ends (RFC 3339); never when empty")
		sub.StringVar(&cmd.control.Message.FA, "message-fa", "", "The message for users, in Persian")
		sub.StringVar(&cmd.control.Message.EN, "message-en", "", "The message for users, in English")
	case "lift":
		sub.StringVar(&cmd.controlID, "id", "", "ID of the control, from platform status")
		sub.StringVar(&cmd.note, "note", "", "Why, for the audit log")
	default:
		fmt.Fprint(stderr, usage)
		return command{}, fmt.Errorf("unknown platform command %q", args[0])
	}
	if err := sub.Parse(args[1:]); err != nil {
		return command{}, err
	}
	if sub.NArg() > 0 {
		return command{}, fmt.Errorf("unexpected argument %q", sub.Arg(0))
	}
	cmd.name = "platform " + args[0]
	switch args[0] {
	case "maintenance", "lockdown":
		cmd.control.Kind = platform.KindMaintenance
		if args[0] == "lockdown" {
			cmd.control.Kind, cmd.control.Feature = platform.KindLockdown, platform.Feature(feature)
			if !cmd.control.Feature.Valid() {
				return command{}, fmt.Errorf("-feature must be one of %s", featureNames())
			}
		}
		if strings.TrimSpace(cmd.control.Reason) == "" {
			return command{}, errors.New("-reason is required")
		}
		var err error
		if cmd.control.StartsAt, err = parseTime("-starts-at", startsAt); err != nil {
			return command{}, err
		}
		if cmd.control.EndsAt, err = parseTime("-ends-at", endsAt); err != nil {
			return command{}, err
		}
		cmd.control.AllowNetworks, cmd.control.AllowUsers = splitFlag(networks), splitFlag(users)
	case "lift":
		if cmd.controlID == "" || strings.TrimSpace(cmd.note) == "" {
			return command{}, errors.New("-id and -note are required")
		}
	}
	return cmd, nil
}

func parseTime(name, value string) (*time.Time, error) {
	if value == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return nil, fmt.Errorf("%s must be an RFC 3339 time, such as 2026-11-08T02:00:00Z: %w", name, err)
	}
	return &t, nil
}

func splitFlag(value string) []string {
	var items []string
	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			items = append(items, item)
		}
	}
	return items
}

func featureNames() string {
	names := make([]string, len(platform.Features))
	for i, f := range platform.Features {
		names[i] = string(f)
	}
	return strings.Join(names, ", ")
}

// runPlatform runs a platform command.
func runPlatform(cmd command, stdout, stderr io.Writer) int {
	postgres, err := db.NewPostgresService(dbOptions())
	if err != nil {
		fmt.Fprintln(stderr, "Error: connect to PostgreSQL:", err)
		return 1
	}
	defer postgres.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := executePlatform(ctx, postgres.DB, cmd, stdout); err != nil {
		fmt.Fprintln(stderr, "Error:", err)
		return 1
	}
	return 0
}

// executePlatform runs a parsed platform command against database. A
// change by an operator needs no second administrator: the operator has
// access to the database, and the audit log names them.
func executePlatform(ctx context.Context, database *sql.DB, cmd command, stdout io.Writer) error {
	switch cmd.name {
	case "platform status":
		controls, err := platform.Open(ctx, database)
		if err != nil {
			return err
		}
		status := platform.StatusOf(controls, time.Now())
		fmt.Fprintf(stdout, "Mode: %s. Locked: %s.\n", status.Mode, lockdownList(status.Lockdowns))
		if len(controls) == 0 {
			fmt.Fprintln(stdout, "No control applies now or later.")
			return nil
		}
		w := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tKIND\tFEATURE\tSTATE\tSTARTS\tENDS\tREASON")
		for _, c := range controls {
			feature, state, ends := "-", "scheduled", "-"
			if c.Feature != nil {
				feature = string(*c.Feature)
			}
			if c.Active {
				state = "active"
			}
			if c.LiftRequestedBy != nil {
				state += ", lift requested"
			}
			if c.EndsAt != nil {
				ends = c.EndsAt.UTC().Format(time.RFC3339)
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", c.HK, c.Kind, feature, state,
				c.StartsAt.UTC().Format(time.RFC3339), ends, oneLine(c.Reason))
		}
		return w.Flush()
	case "platform maintenance", "platform lockdown":
		c, err := platform.Create(ctx, database, cmd.control, operator(cmd))
		if err != nil {
			return err
		}
		what := "Maintenance"
		if c.Feature != nil {
			what = "Lockdown of " + string(*c.Feature)
		}
		when := "now"
		if c.StartsAt.After(time.Now()) {
			when = "at " + c.StartsAt.UTC().Format(time.RFC3339)
		}
		fmt.Fprintf(stdout, "%s %s created; it applies %s on every instance within platform.poll_interval_ms.\n", what, c.HK, when)
	case "platform lift":
		r, err := platform.Lift(ctx, database, platform.LiftRequest{HK: cmd.controlID, Note: cmd.note}, operator(cmd))
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Lifted control %s; every instance applies it within platform.poll_interval_ms.\n", r.Control.HK)
	default:
		return fmt.Errorf("unknown command %q", cmd.name)
	}
	return nil
}

func lockdownList(features []platform.Feature) string {
	if len(features) == 0 {
		return "none"
	}
	names := make([]string, len(features))
	for i, f := range features {
		names[i] = string(f)
	}
	return strings.Join(names, ", ")
}
