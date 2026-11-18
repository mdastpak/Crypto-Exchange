package app

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"net/http"
	"time"

	files "github.com/mdastpak/crypto-exchange/config"
	"github.com/mdastpak/crypto-exchange/internal/aml"
	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/notify"
	"github.com/mdastpak/crypto-exchange/internal/sanctions"
)

// amlSetup is the AML engine (#73) and how its batch runs.
type amlSetup struct {
	engine *aml.Engine
	file   aml.File
	// sanctions screens against the OFAC list, nil without it; sdnURL is
	// where it is downloaded from, every refresh.
	sanctions *sanctions.Screener
	sdnURL    string
	refresh   time.Duration
	timeout   time.Duration
}

// useAML loads the AML rules while compliance.aml.enabled is on, from
// compliance.aml.rules_path or the file built into the release, syncs
// them into the database, and gives the engine to the withdrawals and the
// desk to the admin routes; it runs before useWithdrawals. A rules file
// that does not load, or that lists the country of the deployment as
// high-risk, stops the server. A sync that fails is logged, and the rules
// in the database, sealed, stay in force; one that fails its seal refuses
// every withdrawal until it is reviewed.
func useAML(h *InfraHandlers, db *sql.DB, settings *config.Configuration, email notify.Deliverer) (*amlSetup, error) {
	if !settings.Compliance.AML.Enabled {
		log.Printf("compliance.aml is off: no AML rule evaluates the withdrawals")
		return nil, nil
	}
	path := settings.Compliance.AML.RulesPath
	var f aml.File
	var err error
	if path == "" {
		path = "the built-in rules"
		f, err = aml.Parse(files.AMLRules)
	} else {
		f, err = aml.Load(path)
	}
	if err != nil {
		return nil, fmt.Errorf("compliance.aml.rules_path: %w", err)
	}
	if err := f.CheckHome(homeCountry(settings)); err != nil {
		return nil, fmt.Errorf("compliance.aml.rules_path: %w", err)
	}
	engine := &aml.Engine{Prices: h.Prices}
	if h.Prices == nil {
		engine.Prices = nil
		log.Printf("WARNING: the AML rules read no prices: every rule with an amount holds the withdrawals for review")
	}
	if n := f.Engine.Notifications.Email; n.Enabled {
		sender, ok := email.(aml.TextSender)
		if !ok {
			return nil, fmt.Errorf("compliance.aml: the email notifications of %s need notifications.email", path)
		}
		engine.Notifier = aml.EmailNotifier{Sender: sender, Recipients: n.Recipients}
	}
	setup := &amlSetup{engine: engine, file: f}
	if err := useSanctions(setup, db, settings); err != nil {
		return nil, err
	}
	for _, r := range f.Rules {
		if r.Enabled && r.Type.Screening() && (engine.Screener == nil || !engine.Screener.Screens(r)) {
			log.Printf("WARNING: the AML rule %s (%s) needs a screening provider, which is not configured: it is not evaluated", r.Code, r.Type)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	synced, err := aml.Sync(ctx, db, f)
	if err != nil {
		log.Printf("SECURITY: the AML rules of %s were not synced, and those in the database stay in force: %v", path, err)
	} else {
		log.Printf("AML rules synced from %s: %d added, %d changed, %d retired, %d unchanged", path,
			len(synced.Added), len(synced.Changed), len(synced.Retired), len(synced.Unchanged))
	}
	h.Admin.AML = aml.Desk{DB: db}
	if setup.sanctions != nil {
		h.Admin.Sanctions = setup.sanctions
	}
	return setup, nil
}

// useSanctions gives the engine the screening against the OFAC list (#73)
// while the ofac provider of compliance.sanctions is enabled, and loads
// the version stored, if any. Without one, every operation that a
// sanctions rule screens is held for review until a download stores one.
func useSanctions(setup *amlSetup, db *sql.DB, settings *config.Configuration) error {
	cfg := settings.Compliance.Sanctions
	var ofac *config.SanctionsProviderConfig
	for i, p := range cfg.Providers {
		if p.Name == sanctions.ProviderOFAC && p.Enabled {
			ofac = &cfg.Providers[i]
		}
	}
	if ofac == nil {
		return nil
	}
	if ofac.Endpoint == "" {
		return fmt.Errorf("compliance.sanctions.providers[ofac].endpoint is required while the provider is enabled")
	}
	threshold := cfg.NameMatchThreshold
	switch {
	case threshold == 0:
		threshold = sanctions.DefaultThreshold
	case threshold < 0 || threshold > 1:
		return fmt.Errorf("compliance.sanctions.name_match_threshold must be above 0 and at most 1")
	}
	if cfg.RefreshHours < 0 || cfg.MaxListAgeHours < 0 {
		return fmt.Errorf("compliance.sanctions.refresh_hours and max_list_age_hours must not be negative")
	}
	hours := func(v, fallback int) time.Duration {
		if v == 0 {
			v = fallback
		}
		return time.Duration(v) * time.Hour
	}
	s := &sanctions.Screener{DB: db, Provider: sanctions.ProviderOFAC, Threshold: threshold, MaxAge: hours(cfg.MaxListAgeHours, 72)}
	setup.engine.Screener = s
	setup.sanctions, setup.sdnURL, setup.refresh = s, ofac.Endpoint, hours(cfg.RefreshHours, 24)
	setup.timeout = time.Duration(max(ofac.Timeout, 30)) * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.Refresh(ctx); err != nil {
		log.Printf("WARNING: no OFAC list is loaded yet (%v): the operations that a sanctions rule screens are held for review until one is", err)
	}
	return nil
}

// sanctionsReload is how often each instance looks for a newer list.
const sanctionsReload = time.Minute

// startSanctions reloads the list every minute, and downloads it every
// refresh, while the scheduler is not paused. Each instance runs it; the
// download that finds the list current only records it.
func startSanctions(setup *amlSetup, db *sql.DB, paused func() bool) worker {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	client := &http.Client{Timeout: setup.timeout}
	go func() {
		defer close(done)
		ticker := time.NewTicker(sanctionsReload)
		defer ticker.Stop()
		for {
			if !paused() {
				runCtx, stop := context.WithTimeout(ctx, 5*time.Minute)
				if v, stored, err := sanctions.Update(runCtx, db, client, setup.sdnURL, setup.refresh); err != nil {
					log.Printf("Error downloading the OFAC list: %v", err)
				} else if stored {
					log.Printf("OFAC list version %d stored: %d entries", v.ID, v.EntryCount)
				}
				if err := setup.sanctions.Refresh(runCtx); err != nil {
					log.Printf("Error loading the OFAC list: %v", err)
				}
				stop()
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return worker{"sanctions list", func(stopCtx context.Context) error {
		cancel()
		select {
		case <-done:
			return nil
		case <-stopCtx.Done():
			return stopCtx.Err()
		}
	}}
}

// homeCountry is the country of the deployment, which the AML rules must
// not list as high-risk, and of the banks of the fiat destinations: IR in
// the ir region, and none known in the international one.
func homeCountry(settings *config.Configuration) string {
	if settings.Region().Iran() {
		return "IR"
	}
	return ""
}

// startAMLBatch runs the batch of the AML rules, and the escalation of the
// alerts, every engine.evaluation_interval_seconds while the scheduler is
// not paused. Each instance runs it; an alert of a window is raised once.
func startAMLBatch(s *amlSetup, db *sql.DB, paused func() bool) worker {
	interval := time.Duration(s.file.Engine.EvaluationIntervalSeconds) * time.Second
	escalate := time.Duration(s.file.Engine.AlertEscalationHours) * time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			if paused() {
				continue
			}
			runCtx, stop := context.WithTimeout(ctx, interval)
			if out, err := s.engine.Batch(runCtx, db, escalate); err != nil {
				log.Printf("Error running the AML batch: %v", err)
			} else if out.Alerts+out.Escalated > 0 {
				log.Printf("AML batch: %d alerts, %d escalated", out.Alerts, out.Escalated)
			}
			stop()
		}
	}()
	return worker{"aml batch", func(stopCtx context.Context) error {
		cancel()
		select {
		case <-done:
			return nil
		case <-stopCtx.Done():
			return stopCtx.Err()
		}
	}}
}
