package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/db"
	"github.com/mdastpak/crypto-exchange/internal/marketdata"
)

// The market-data commands administer the market data catalog (#270), as
// the admin routes under /api/v1/admin/market-data do. They work on the
// database alone: the public API shows a change once its cached answers
// expire, within market_data.api.cache_seconds, while a change through the
// admin routes drops the cache at once.

// marketArgs are the flags of a market-data command.
type marketArgs struct {
	source, venue, currency, job, status, query, reason, tier string
	asset, pair, base, quote, id                              int64
	limit                                                     int
}

// marketCommands are the market-data commands, with their usage.
var marketCommands = []string{"sources", "pause", "resume", "jobs", "run", "runs", "venues", "venue-enable", "venue-disable",
	"assets", "hide", "unhide", "link", "unlink", "tier", "map", "conflicts", "resolve"}

// parseMarketArgs parses the arguments after "market-data".
func parseMarketArgs(cmd command, args []string, stderr io.Writer) (command, error) {
	if len(args) == 0 || !slices.Contains(marketCommands, args[0]) {
		fmt.Fprint(stderr, usage)
		if len(args) == 0 {
			return command{}, errors.New("a market-data command is required: " + strings.Join(marketCommands, ", "))
		}
		return command{}, fmt.Errorf("unknown market-data command %q", args[0])
	}
	m := &cmd.market
	sub := flag.NewFlagSet("market-data "+args[0], flag.ContinueOnError)
	sub.SetOutput(stderr)
	reason := func() { sub.StringVar(&m.reason, "reason", "", "Why, for the audit log") }
	assetFlag := func() { sub.Int64Var(&m.asset, "asset", 0, "ID of the asset, from market-data assets") }
	switch args[0] {
	case "sources", "jobs", "venues":
	case "pause", "resume":
		sub.StringVar(&m.source, "source", "", "The source, such as coingecko or binance")
		reason()
	case "run":
		sub.StringVar(&m.job, "job", "", "The job, such as tickers:binance, from market-data jobs")
	case "runs":
		sub.StringVar(&m.job, "job", "", "Only the runs of this job")
		sub.StringVar(&m.status, "status", "", "Only the runs of this status: running, succeeded, or failed")
		sub.IntVar(&m.limit, "limit", 20, "The number of runs, at most 200")
	case "venue-enable", "venue-disable":
		sub.StringVar(&m.venue, "venue", "", "The venue, such as binance")
		reason()
	case "assets":
		sub.StringVar(&m.status, "status", "", "Only the assets of this status: unverified, hidden, or linked")
		sub.StringVar(&m.query, "q", "", "A symbol, or the start of a slug or name")
		sub.IntVar(&m.limit, "limit", 50, "The number of assets, at most 200")
	case "hide", "unhide", "unlink":
		assetFlag()
		reason()
	case "link":
		assetFlag()
		sub.StringVar(&m.currency, "currency", "", "The hk of the listed currency")
		reason()
	case "tier":
		assetFlag()
		sub.Int64Var(&m.pair, "pair", 0, "ID of the pair, instead of an asset")
		sub.StringVar(&m.tier, "tier", "", "hot, active, other, or none to clear it")
	case "map":
		sub.Int64Var(&m.pair, "pair", 0, "ID of the pair")
		sub.Int64Var(&m.base, "base", 0, "ID of the base asset")
		sub.Int64Var(&m.quote, "quote", 0, "ID of the quote asset")
	case "conflicts":
		sub.StringVar(&m.source, "source", "", "Only the conflicts of this source")
	case "resolve":
		sub.Int64Var(&m.id, "id", 0, "ID of the conflict, from market-data conflicts")
		sub.StringVar(&m.reason, "note", "", "What was decided, for the audit log")
	}
	if err := sub.Parse(args[1:]); err != nil {
		return command{}, err
	}
	if sub.NArg() > 0 {
		return command{}, fmt.Errorf("unexpected argument %q", sub.Arg(0))
	}
	cmd.name = "market-data " + args[0]
	var problem string
	switch args[0] {
	case "pause", "resume":
		if m.source == "" {
			problem = "-source is required"
		} else if args[0] == "pause" && strings.TrimSpace(m.reason) == "" {
			problem = "-reason is required"
		}
	case "run":
		if m.job == "" {
			problem = "-job is required"
		}
	case "runs", "assets":
		if m.limit < 1 || m.limit > 200 {
			problem = "-limit must be from 1 to 200"
		}
	case "venue-enable", "venue-disable":
		if m.venue == "" {
			problem = "-venue is required"
		}
	case "hide", "unhide", "unlink", "link":
		switch {
		case m.asset <= 0:
			problem = "-asset must be a positive number"
		case args[0] == "hide" && strings.TrimSpace(m.reason) == "":
			problem = "-reason is required"
		case args[0] == "link" && m.currency == "":
			problem = "-currency is required"
		}
	case "tier":
		switch {
		case (m.asset > 0) == (m.pair > 0):
			problem = "give one of -asset and -pair"
		case m.tier != "none" && !slices.Contains(marketdata.Tiers, m.tier):
			problem = "-tier must be hot, active, other, or none"
		}
	case "map":
		if m.pair <= 0 || m.base <= 0 || m.quote <= 0 {
			problem = "-pair, -base, and -quote must be positive numbers"
		}
	case "resolve":
		if m.id <= 0 || strings.TrimSpace(m.reason) == "" {
			problem = "-id and -note are required"
		}
	}
	if problem != "" {
		return command{}, errors.New(problem)
	}
	return cmd, nil
}

// runMarket runs a market-data command.
func runMarket(cmd command, stdout, stderr io.Writer) int {
	postgres, err := db.NewPostgresService()
	if err != nil {
		fmt.Fprintln(stderr, "Error: connect to PostgreSQL:", err)
		return 1
	}
	defer postgres.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := executeMarket(ctx, postgres.DB, cmd, stdout); err != nil {
		fmt.Fprintln(stderr, "Error:", err)
		return 1
	}
	return 0
}

// cacheNote tells when the public API shows a change.
const cacheNote = "The public API shows it once its cached answers expire, within market_data.api.cache_seconds."

func when(t *time.Time) string {
	if t == nil {
		return "-"
	}
	return t.UTC().Format(time.RFC3339)
}

func id(p *int64) string {
	if p == nil {
		return "-"
	}
	return fmt.Sprint(*p)
}

func text(p *string) string {
	if p == nil {
		return "-"
	}
	return *p
}

// executeMarket runs a parsed market-data command against database.
func executeMarket(ctx context.Context, database *sql.DB, cmd command, stdout io.Writer) error {
	store := marketdata.NewStore(database)
	m := cmd.market
	origin := operator(cmd)
	w := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	switch cmd.name {
	case "market-data sources":
		controls, err := store.SourceControls(ctx)
		if err != nil {
			return err
		}
		usage, err := store.SourceUsages(ctx)
		if err != nil {
			return err
		}
		jobs, err := store.JobStates(ctx)
		if err != nil {
			return err
		}
		type row struct {
			paused        bool
			reason        string
			jobs, failing int
			minute, month int
			lastSuccess   *time.Time
		}
		rows := map[string]*row{}
		get := func(name string) *row {
			if rows[name] == nil {
				rows[name] = &row{}
			}
			return rows[name]
		}
		for name, c := range controls {
			get(name).paused, get(name).reason = c.Paused, c.Reason
		}
		for name, u := range usage {
			get(name).minute, get(name).month = u.Minute, u.Month
		}
		for _, j := range jobs {
			name := marketdata.SourceOf(j.Job)
			if name == "" {
				continue
			}
			r := get(name)
			r.jobs++
			if j.Failures > 0 {
				r.failing++
			}
			if j.LastSuccessAt != nil && (r.lastSuccess == nil || j.LastSuccessAt.After(*r.lastSuccess)) {
				r.lastSuccess = j.LastSuccessAt
			}
		}
		names := make([]string, 0, len(rows))
		for name := range rows {
			names = append(names, name)
		}
		slices.Sort(names)
		fmt.Fprintln(w, "SOURCE\tSTATE\tJOBS\tFAILING\tREQUESTS/MIN\tREQUESTS/MONTH\tLAST SUCCESS\tREASON")
		for _, name := range names {
			r := rows[name]
			state := "active"
			if r.paused {
				state = "paused"
			}
			fmt.Fprintf(w, "%s\t%s\t%d\t%d\t%d\t%d\t%s\t%s\n", name, state, r.jobs, r.failing, r.minute, r.month, when(r.lastSuccess), oneLine(r.reason))
		}
	case "market-data pause", "market-data resume":
		paused := cmd.name == "market-data pause"
		if _, err := store.SetSourcePaused(ctx, m.source, paused, m.reason, origin); err != nil {
			return err
		}
		if paused {
			fmt.Fprintf(stdout, "Paused %s: the workers stop claiming its jobs at their next poll, and a run in progress stops at its next write.\n", m.source)
		} else {
			fmt.Fprintf(stdout, "Resumed %s: the workers claim its jobs again at their next poll.\n", m.source)
		}
		return nil
	case "market-data jobs":
		jobs, err := store.JobStates(ctx)
		if err != nil {
			return err
		}
		fmt.Fprintln(w, "JOB\tSTATE\tFAILURES\tNEXT RUN\tLAST SUCCESS")
		for _, j := range jobs {
			state := "waiting"
			if j.Running {
				state = "running on " + j.Holder
			}
			fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\n", j.Job, state, j.Failures, when(&j.NextRunAt), when(j.LastSuccessAt))
		}
	case "market-data run":
		if err := store.RunJobNow(ctx, m.job, origin); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "%s is due now; a worker that reaches its sources runs it at its next poll.\n", m.job)
		return nil
	case "market-data runs":
		runs, err := store.ListRuns(ctx, marketdata.RunFilter{Job: m.job, Status: m.status, Limit: m.limit})
		if err != nil {
			return err
		}
		fmt.Fprintln(w, "ID\tJOB\tNODE\tMODE\tSTATUS\tSTARTED\tFINISHED\tREAD\tWRITTEN\tERROR")
		for _, r := range runs {
			fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\t%s\t%d\t%d\t%s\n", r.ID, r.Job, r.Node, r.Mode, r.Status,
				when(&r.StartedAt), when(r.FinishedAt), r.ItemsRead, r.ItemsWritten, oneLine(r.Error))
		}
	case "market-data venues":
		venues, err := store.Venues(ctx)
		if err != nil {
			return err
		}
		fmt.Fprintln(w, "VENUE\tNAME\tREGION\tSTATE")
		for _, v := range venues {
			state := "disabled"
			if v.Enabled {
				state = "enabled"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", v.ID, v.Name, v.Region, state)
		}
	case "market-data venue-enable", "market-data venue-disable":
		v, err := store.SetVenueEnabled(ctx, m.venue, cmd.name == "market-data venue-enable", m.reason, origin)
		if err != nil {
			return err
		}
		state := "disabled: its jobs collect nothing"
		if v.Enabled {
			state = "enabled: its jobs collect it again"
		}
		fmt.Fprintf(stdout, "%s is %s. %s\n", v.ID, state, cacheNote)
		return nil
	case "market-data assets":
		assets, err := store.AdminAssets(ctx, marketdata.AssetFilter{Status: m.status, Query: m.query, Limit: m.limit})
		if err != nil {
			return err
		}
		fmt.Fprintln(w, "ID\tSLUG\tSYMBOL\tNAME\tSTATUS\tCURRENCY\tTIER")
		for _, a := range assets {
			fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\t%s\n", a.ID, a.Slug, a.Symbol, a.Name, a.Status, text(a.CurrencyHK), text(a.TierOverride))
		}
	case "market-data hide", "market-data unhide", "market-data link", "market-data unlink":
		var a marketdata.AdminAsset
		var err error
		switch cmd.name {
		case "market-data hide", "market-data unhide":
			a, err = store.SetAssetHidden(ctx, m.asset, cmd.name == "market-data hide", m.reason, origin)
		case "market-data link":
			a, err = store.LinkAsset(ctx, m.asset, m.currency, m.reason, origin)
		default:
			a, err = store.UnlinkAsset(ctx, m.asset, m.reason, origin)
		}
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Asset %d (%s) is %s. %s\n", a.ID, a.Slug, a.Status, cacheNote)
		return nil
	case "market-data tier":
		var tier *string
		if m.tier != "none" {
			tier = &m.tier
		}
		var err error
		what := fmt.Sprintf("asset %d", m.asset)
		if m.pair > 0 {
			what = fmt.Sprintf("pair %d", m.pair)
			err = store.SetPairTierOverride(ctx, m.pair, tier, origin)
		} else {
			err = store.SetAssetTierOverride(ctx, m.asset, tier, origin)
		}
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "The tier of %s is %s; the tiers job applies it at its next run.\n", what, m.tier)
		return nil
	case "market-data map":
		if err := store.MapPair(ctx, m.pair, m.base, m.quote, origin); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Pair %d is mapped to the assets %d and %d. %s\n", m.pair, m.base, m.quote, cacheNote)
		return nil
	case "market-data conflicts":
		conflicts, err := store.ListConflicts(ctx, marketdata.ConflictFilter{Source: m.source, Limit: 200})
		if err != nil {
			return err
		}
		fmt.Fprintln(w, "ID\tSOURCE\tEXTERNAL ID\tKIND\tSUBJECT\tASSET\tCLAIMED BY\tDETAIL")
		for _, c := range conflicts {
			fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", c.ID, c.Source, c.ExternalID, c.Kind, c.Subject,
				id(c.AssetID), id(c.ClaimedBy), oneLine(c.Detail))
		}
	case "market-data resolve":
		if err := store.ResolveConflict(ctx, m.id, m.reason, origin); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Conflict %d is resolved.\n", m.id)
		return nil
	default:
		return fmt.Errorf("unknown command %q", cmd.name)
	}
	return w.Flush()
}
