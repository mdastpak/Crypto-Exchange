// Package app is the composition root of the server (#48): it reads the
// configuration, connects to PostgreSQL, Redis, and RabbitMQ, builds the
// stores, the handlers, and the background workers, registers the routes,
// and owns the lifecycle, from the start to the graceful shutdown.
// cmd/server only calls Main. The end-to-end tests of the server live here
// too, since they build the same wiring.
package app

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/mdastpak/crypto-exchange/docs"
	"github.com/mdastpak/crypto-exchange/internal/adminauth"
	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/auth"
	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/confirm"
	"github.com/mdastpak/crypto-exchange/internal/currencies"
	"github.com/mdastpak/crypto-exchange/internal/db"
	"github.com/mdastpak/crypto-exchange/internal/delisting"
	"github.com/mdastpak/crypto-exchange/internal/destinations"
	"github.com/mdastpak/crypto-exchange/internal/geoip"
	"github.com/mdastpak/crypto-exchange/internal/handlers/admin"
	marketdataapi "github.com/mdastpak/crypto-exchange/internal/handlers/marketdata"
	"github.com/mdastpak/crypto-exchange/internal/handlers/marketdataadmin"
	"github.com/mdastpak/crypto-exchange/internal/handlers/public"
	"github.com/mdastpak/crypto-exchange/internal/handlers/socket"
	"github.com/mdastpak/crypto-exchange/internal/handlers/system"
	"github.com/mdastpak/crypto-exchange/internal/handlers/user"
	"github.com/mdastpak/crypto-exchange/internal/helpers"
	"github.com/mdastpak/crypto-exchange/internal/i18n"
	"github.com/mdastpak/crypto-exchange/internal/identity"
	"github.com/mdastpak/crypto-exchange/internal/keys"
	"github.com/mdastpak/crypto-exchange/internal/logging"
	"github.com/mdastpak/crypto-exchange/internal/marketdata"
	"github.com/mdastpak/crypto-exchange/internal/metrics"
	"github.com/mdastpak/crypto-exchange/internal/middleware"
	"github.com/mdastpak/crypto-exchange/internal/models"
	"github.com/mdastpak/crypto-exchange/internal/notify"
	"github.com/mdastpak/crypto-exchange/internal/otp"
	"github.com/mdastpak/crypto-exchange/internal/outbox"
	"github.com/mdastpak/crypto-exchange/internal/passkey"
	"github.com/mdastpak/crypto-exchange/internal/passwordhash"
	"github.com/mdastpak/crypto-exchange/internal/platform"
	"github.com/mdastpak/crypto-exchange/internal/rabbitmq"
	"github.com/mdastpak/crypto-exchange/internal/rebrand"
	"github.com/mdastpak/crypto-exchange/internal/redisutil"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"
	"github.com/mdastpak/crypto-exchange/internal/signin"
	"github.com/mdastpak/crypto-exchange/internal/sites"
	"github.com/mdastpak/crypto-exchange/internal/tracing"
	"github.com/mdastpak/crypto-exchange/internal/userauth"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"github.com/shopspring/decimal"
	swaggerFiles "github.com/swaggo/files"
	ginSwagger "github.com/swaggo/gin-swagger"
)

type InfraHandlers struct {
	Socket *socket.WebSocketHandler
	System *system.SystemHandler
	Admin  *admin.AdminHandler
	Public *public.PublicHandler
	User   *user.UserHandler
	// MarketData serves the global market data catalog (#269).
	MarketData *marketdataapi.Handler
	// MarketDataAdmin administers the catalog (#270).
	MarketDataAdmin *marketdataadmin.Handler
	Redis           *db.RedisService
	TokenBlacklist  *auth.TokenBlacklist
	Sessions        auth.SessionStore
	// DataKeys holds the data-protection keys for row seals (#139) and
	// field encryption (#140). It is nil without keys.provider.
	DataKeys *keys.Live
	// Platform refuses the requests that a maintenance or a lockdown closes
	// (#235). Without a watcher, it lets every request through.
	Platform *platform.Gate
	// Locales are the supported locales (#283). RouteHandler fills it from
	// the configuration when it is nil.
	Locales *i18n.Registry
	// GeoIP finds the country of a request (#284). Nil knows no country.
	GeoIP *geoip.Resolver
	// Sites are the domains and locales per country (#285). RouteHandler
	// fills it from the configuration when it is nil.
	Sites *sites.Registry
	// The concrete stores behind the interfaces of the handlers (#48),
	// which the composition root configures after it builds them, and the
	// tests reach: the accounts of the sign-ins, the cached catalog, and the
	// account passwords.
	SignIn    *signin.Accounts
	Catalog   *currencies.Catalog
	Passwords *userauth.Passwords
	// UserStore is the database store of the user routes, and TOTP the
	// authenticator apps, which the data keys and the identity checks
	// configure.
	UserStore *user.DBStore
	TOTP      *userauth.TOTP
}

var (
	configPath     string
	migrationsPath string
)

func init() {
	// The process works in UTC, whatever the time zone of its host: times
	// read from PostgreSQL, which pgx returns in time.Local, and times from
	// time.Now reach the answers and the logs in one form (#339).
	time.Local = time.UTC
	flag.StringVar(&configPath, "config", "config/config.yaml", "Path to config file")
	flag.StringVar(&migrationsPath, "migrations", "file://internal/db/migrations", "Path to migrations")
}

// Main runs the server process: it parses the command line, starts the
// server, and serves until a SIGINT or SIGTERM. It exits the process on an
// error. cmd/server calls it.
func Main() {
	flag.Parse()
	if err := run(); err != nil {
		log.Fatal(err)
	}
	log.Println("Server stopped gracefully")
}

// flagSet reports whether the flag name was given on the command line.
func flagSet(name string) (set bool) {
	flag.Visit(func(f *flag.Flag) { set = set || f.Name == name })
	return set
}

// logPosture logs the settings that weaken an instance in one line at
// start (#306), so that an instance that runs weaker than intended is
// visible in its logs: the mode, the region, the row seal mode, whether
// sign-in codes are logged, and whether Swagger is served.
func logPosture() {
	cfg := config.Config
	log.Printf("Security posture: mode=%s region=%s row_seals=%s dev_log_codes=%t swagger=%t",
		cfg.Server.Mode, cfg.Region(), cfg.Keys.RowSeals.Mode, cfg.OTP.Auth.DevLogCodes, cfg.SwaggerEnabled())
	if cfg.Server.Mode != gin.ReleaseMode {
		log.Printf("WARNING: server.mode is %s: the release checks are off. Production instances run in release mode, the default", cfg.Server.Mode)
	} else if cfg.Keys.Provider != "" && cfg.Keys.RowSeals.Mode == config.RowSealsReport {
		log.Println("WARNING: keys.row_seals.mode is report: a row changed outside the backend is recorded but still served; set enforce once the rollout is over")
	}
}

// run starts the server and serves until a SIGINT or SIGTERM, then stops it.
// It returns instead of exiting on an error, so that the deferred closes of
// the services run on every path.
func run() error {
	config.SetConfigPath(configPath)
	config.RequireConfigFile(flagSet("config"))
	config.SetMigrationsPath(migrationsPath)
	config.LoadConfig()
	// Every log line is a structured record from here on (#50).
	closeLog, err := logging.Setup(&config.Config)
	if err != nil {
		return fmt.Errorf("invalid logging configuration: %w", err)
	}
	defer closeLog()
	logPosture()

	// Check the whole configuration before connecting to anything, and
	// report every problem at once.
	keyProvider, err := validateConfiguration()
	if err != nil {
		return fmt.Errorf("invalid configuration:\n%s", problemList(err))
	}
	// The spans of the process go to observability.tracing.endpoint (#50);
	// they are flushed when the server stops.
	stopTracing, err := tracing.Setup(context.Background(), &config.Config)
	if err != nil {
		return fmt.Errorf("invalid tracing configuration: %w", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := stopTracing(ctx); err != nil {
			log.Printf("Error flushing the traces: %v", err)
		}
	}()

	// server.mode, which SERVER_MODE or GIN_MODE can set, selects the
	// release checks above as well as Gin's mode.
	gin.SetMode(config.Config.Server.Mode)

	if err := initAuthKeys(); err != nil {
		return fmt.Errorf("failed to initialize auth keys: %w", err)
	}

	if config.Config.Keys.Provider == "" {
		log.Println("WARNING: keys.provider is not set: sign-in codes use a random secret, so only this process can verify them, until it restarts, and admin sign-in is disabled")
	}
	if gin.Mode() == gin.ReleaseMode && config.Config.Server.AdminListenAddr == "" && len(config.Config.Admin.AllowedCIDRs) == 0 {
		log.Println("WARNING: the admin routes are served on the public address to every network; set server.admin_listen_addr or admin.allowed_cidrs, or keep them private at the proxy (docs/configuration.md)")
	}
	// Production runs several instances behind a load balancer, which keeps
	// routing to a stopping instance until it sees the readiness probe fail.
	if gin.Mode() == gin.ReleaseMode && config.Config.Server.DrainSeconds == 0 {
		log.Println("WARNING: server.drain_seconds is 0: at shutdown, this instance refuses connections as soon as its readiness probe fails, so a load balancer may still send it requests that fail; set it to a little more than the balancer's readiness interval")
	}
	otpChannels, err := notify.NewChannels(config.Config.Notifications, otpTTL())
	if err != nil {
		return fmt.Errorf("invalid OTP delivery configuration: %w", err)
	}

	// pg_stat_activity tells the sessions of each instance apart (#230).
	db.ApplicationName = db.ServerApplicationName()
	services, cleanup, err := initializeServices()
	if err != nil {
		return fmt.Errorf("failed to initialize services: %w", err)
	}
	// Closes RabbitMQ, Redis, and PostgreSQL, in that order, after shutdown
	// has stopped everything that uses them.
	defer cleanup()

	evictionWatch, err := checkRedisEviction(services.Redis.Client, config.Config.Server.Mode == gin.ReleaseMode, config.Config.Redis.AssumeNoEviction)
	if err != nil {
		return err
	}

	handlers := initializeHandlers(services)
	if evictionWatch != nil {
		handlers.System.Eviction = evictionWatch
	}
	if handlers.DataKeys, err = loadDataKeys(services, keyProvider); err != nil {
		return fmt.Errorf("failed to load the data-protection keys: %w", err)
	}
	useDataKeysForOTP(handlers)
	otpSender, otpWorker := newOTPDelivery(otpChannels, services.RabbitMQ, services.Redis.Client)
	watcher, err := startPlatform(services, handlers)
	if err != nil {
		return fmt.Errorf("invalid platform configuration: %w", err)
	}
	relay := newOutboxRelay(services, handlers, watcher)
	workers := []worker{{"outbox relay", relay.Stop}, {"platform watcher", watcher.Stop}}
	geoIP, geoIPWorker, err := startGeoIP()
	if err != nil {
		return fmt.Errorf("invalid GeoIP configuration: %w", err)
	}
	handlers.GeoIP = geoIP
	if geoIP != nil {
		handlers.System.GeoIP = geoIP
	}
	// The country of a sign-up and of an acceptance of the legal
	// documents (#316).
	handlers.Public.GeoIP, handlers.User.GeoIP = geoIP, geoIP
	if geoIPWorker != nil {
		workers = append(workers, *geoIPWorker)
	}
	// The metrics of the instance, on a port of their own (#50).
	metricsWorker, err := startMetrics(services, handlers)
	if err != nil {
		return err
	}
	if metricsWorker != nil {
		workers = append(workers, *metricsWorker)
	}
	scheduler := newDelistingScheduler(services, handlers, relay)
	scheduler.Paused = func() bool { return watcher.Paused(platform.FeatureScheduler) }
	scheduler.Start()
	workers = append(workers, worker{"delisting scheduler", scheduler.Stop})
	// The snapshots and the reconciliation of the books (#60).
	workers = append(workers, startLedgerAudit(services.Postgres.DB))
	if otpWorker != nil {
		workers = append(workers, worker{"OTP delivery worker", otpWorker.Stop})
	}
	if handlers.DataKeys != nil {
		handlers.DataKeys.Start()
		workers = append(workers, worker{"data key reloader", handlers.DataKeys.Stop})
		verifier := useRowSeals(services, handlers.DataKeys)
		workers = append(workers, worker{"row seal verifier", verifier.Stop})
		workers = append(workers, useFieldEncryption(services.Postgres.DB, handlers.DataKeys))
	}
	// Stops the workers and the WebSocket event consumer, which start below,
	// before the services close when startup fails after them. Otherwise
	// shutdown has stopped them, and stopping them again does nothing.
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout())
		defer cancel()
		for _, w := range workers {
			_ = w.stop(ctx)
		}
		_ = handlers.Socket.Shutdown(ctx)
	}()
	handlers.Public.OTPSender = otpSender
	handlers.User.OTPSender = otpSender
	useConfirmationSender(handlers, otpSender)
	if w := startAntiPhishingReminders(services.Postgres.DB, otpSender); w != nil {
		workers = append(workers, *w)
	}
	if err := configureIdentity(handlers); err != nil {
		return fmt.Errorf("failed to configure the identity checks: %w", err)
	}
	if err := configureAdminSignIn(handlers, services, otpSender); err != nil {
		return fmt.Errorf("failed to configure admin sign-in: %w", err)
	}
	// Forward currency events to WebSocket clients.
	if err := handlers.Socket.StartConsumer(context.Background()); err != nil {
		return fmt.Errorf("failed to start the WebSocket event consumer: %w", err)
	}
	handlers.Socket.StartSessionWatcher(context.Background())
	handlers.Socket.StartConnectionHeartbeat(context.Background())
	if otpWorker != nil {
		if err := otpWorker.Start(); err != nil {
			return fmt.Errorf("failed to start the OTP delivery worker: %w", err)
		}
	}
	relay.Start()

	serverAddr := fmt.Sprintf("%s:%s", config.Config.Server.IP, config.Config.Server.Port)

	r, err := newEngine()
	if err != nil {
		return fmt.Errorf("failed to create the HTTP engine: %w", err)
	}
	r.Use(logging.Requests(slog.Default(), requestUser))
	if err := RouteHandler(r, handlers); err != nil {
		return fmt.Errorf("failed to register routes: %w", err)
	}

	protocol := "http"
	if config.Config.Server.TLS.Enabled {
		protocol = "https"
	}
	log.Printf("Starting server on: %s://%s", protocol, serverAddr)
	if config.Config.SwaggerEnabled() {
		log.Printf("Swagger documentation available at: %s://%s/swagger/index.html", protocol, serverAddr)
	}

	srv := &http.Server{
		Addr:         serverAddr,
		Handler:      r,
		ReadTimeout:  time.Duration(config.Config.Server.ReadTimeout) * time.Second,
		WriteTimeout: time.Duration(config.Config.Server.WriteTimeout) * time.Second,
		IdleTimeout:  time.Duration(config.Config.Server.IdleTimeout) * time.Second,
	}
	adminAddr := config.Config.Server.AdminListenAddr
	bound, err := openListeners(serverAddr, adminAddr)
	if err != nil {
		return err
	}
	bound.attach(srv)
	if adminAddr != "" {
		log.Printf("Serving the admin routes only on: %s://%s", protocol, adminAddr)
	}
	listen := func() error {
		if config.Config.Server.TLS.Enabled {
			return bound.serve(srv, config.Config.Server.TLS.CertFile, config.Config.Server.TLS.KeyFile)
		}
		return bound.serve(srv, "", "")
	}

	ctx, stop := stopSignals()
	defer stop()
	return serve(ctx, srv, listen, handlers, workers, shutdownTimeout(), drainTime())
}

// newSiteHandler builds the handler of the site routes (#285).
func newSiteHandler(ih *InfraHandlers) *public.SiteHandler {
	h := &public.SiteHandler{
		Sites:   ih.Sites,
		Locales: ih.Locales,
	}
	// Never a typed nil: the handler resolves no country without one.
	if ih.GeoIP != nil {
		h.GeoIP = ih.GeoIP
	}
	for _, entry := range config.Config.Server.TrustedProxies {
		if prefix, err := geoip.ParsePrefix(entry); err == nil {
			h.TrustedProxies = append(h.TrustedProxies, prefix)
		}
	}
	return h
}

// startGeoIP builds the resolver of client countries (#284) and starts the
// reloader of its database, which each instance runs on its own copy of
// the file. A database that does not load stops a release, since the sites
// of the countries (#285) would silently fall back to the default site; in
// development the server starts without it.
func startGeoIP() (*geoip.Resolver, *worker, error) {
	g := config.Config.GeoIP
	if !g.Enabled {
		return nil, nil, nil
	}
	resolver, err := geoip.New(geoip.Options{
		Header:         g.Header,
		TrustedProxies: config.Config.Server.TrustedProxies,
		DatabasePath:   g.DatabasePath,
		Overrides:      g.Overrides,
		MaxAge:         time.Duration(g.MaxAgeDays) * 24 * time.Hour,
	})
	if err != nil {
		return nil, nil, err
	}
	if err := resolver.Load(); err != nil {
		if config.Config.Server.Mode == "release" {
			return nil, nil, err
		}
		log.Printf("WARNING: GeoIP starts without its database: %v", err)
	}
	if err := resolver.Check(time.Now()); err != nil {
		log.Printf("WARNING: GeoIP: %v", err)
	}
	if g.DatabasePath == "" {
		return resolver, nil, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		resolver.Watch(ctx, time.Duration(g.ReloadIntervalSeconds)*time.Second)
	}()
	return resolver, &worker{"GeoIP reloader", func(stopCtx context.Context) error {
		cancel()
		select {
		case <-done:
			return nil
		case <-stopCtx.Done():
			return stopCtx.Err()
		}
	}}, nil
}

// stopSignals returns a context that a SIGINT or SIGTERM cancels.
func stopSignals() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// shutdownTimeout is server.shutdown_timeout_seconds, the time the whole
// shutdown may take.
func shutdownTimeout() time.Duration {
	return time.Duration(config.Config.Server.ShutdownTimeout) * time.Second
}

// drainTime is server.drain_seconds, the time the instance keeps serving
// after its readiness probe fails.
func drainTime() time.Duration {
	return time.Duration(config.Config.Server.DrainSeconds) * time.Second
}

// serve runs the HTTP server with listen until ctx is done or the server
// fails, and then stops the instance with shutdown, which may take up to
// timeout. It returns the server's failure, if any, joined with the errors of
// the shutdown. A server that failed skips the drain, since nothing reaches
// it any more.
func serve(ctx context.Context, srv *http.Server, listen func() error, handlers *InfraHandlers, workers []worker, timeout, drain time.Duration) error {
	served := make(chan error, 1)
	go func() { served <- listen() }()

	var failure error
	select {
	case <-ctx.Done():
		log.Println("Shutting down the server...")
	case err := <-served:
		// Only Shutdown and Close make listen return ErrServerClosed, so any
		// return before them is a failure.
		failure = fmt.Errorf("the HTTP server stopped: %w", err)
		log.Printf("%v; shutting down", failure)
		drain = 0
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	err := shutdown(shutdownCtx, srv, handlers, workers, drain)
	if failure == nil {
		if err := <-served; !errors.Is(err, http.ErrServerClosed) {
			failure = fmt.Errorf("the HTTP server stopped: %w", err)
		}
	}
	return errors.Join(failure, err)
}

// shutdown stops this instance without losing the work in progress:
//
//  1. The readiness probe answers 503, so that load balancers stop routing
//     new requests here, and responses ask clients to close their
//     connections.
//  2. The instance keeps serving for drain, the time the load balancers take
//     to notice.
//  3. The HTTP server stops accepting connections and waits for the requests
//     in flight. When ctx expires first, it closes their connections.
//  4. The workers stop, after the requests that give them work: the outbox
//     relay, whose undelivered messages another instance relays, and the
//     OTP delivery worker.
//  5. The WebSocket clients get a close frame, and the event consumer stops.
//
// Each step runs even when an earlier one fails, and all share the deadline
// of ctx. The caller closes RabbitMQ, Redis, and PostgreSQL afterwards. The
// shared state in Redis, such as the currency cache, stays, because the
// other instances keep serving from it.
func shutdown(ctx context.Context, srv *http.Server, handlers *InfraHandlers, workers []worker, drain time.Duration) error {
	handlers.System.StartDraining()
	srv.SetKeepAlivesEnabled(false)
	if drain > 0 {
		log.Printf("Draining for %s before the HTTP server stops", drain)
		select {
		case <-time.After(drain):
		case <-ctx.Done():
		}
	}

	var errs []error
	if err := srv.Shutdown(ctx); err != nil {
		errs = append(errs, fmt.Errorf("HTTP server: %w", err))
		_ = srv.Close()
	}
	// The workers publish, so they stop before the services, and RabbitMQ
	// with them, are closed.
	for _, w := range workers {
		if err := w.stop(ctx); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", w.name, err))
		}
	}
	if err := handlers.Socket.Shutdown(ctx); err != nil {
		errs = append(errs, fmt.Errorf("WebSocket clients: %w", err))
	}
	return errors.Join(errs...)
}

// worker is a background worker that shutdown stops once the HTTP server
// has stopped.
type worker struct {
	name string
	stop func(ctx context.Context) error
}

// newOutboxRelay returns the relay of the outbox, which drops the cached
// currency list and publishes the currency events to the price updates
// exchange, and lets the admin handler nudge it after a change. Every
// instance runs one; one at a time relays.
// startPlatform starts this instance's copy of the platform controls
// (#235), and gives it to the gates of the routes, the WebSocket, and the
// admin API. The WebSocket tells its clients of every change, and closes
// those that a control closes.
func startPlatform(services *models.InfraServices, handlers *InfraHandlers) (*platform.Watcher, error) {
	settings, err := platform.SettingsOf(&config.Config)
	if err != nil {
		return nil, err
	}
	watcher := platform.NewWatcher(services.Postgres.DB, settings.Options)
	gate := &platform.Gate{Watcher: watcher}
	handlers.Platform = gate
	handlers.Public.Platform = watcher
	handlers.Socket.Platform = gate
	handlers.Admin.Platform = watcher
	handlers.Admin.LiftApproval = settings.Approval
	watcher.OnChange(handlers.Socket.ApplyPlatform)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	watcher.Start(ctx)
	return watcher, nil
}

func newOutboxRelay(services *models.InfraServices, handlers *InfraHandlers, watcher *platform.Watcher) *outbox.Relay {
	route := outbox.Route{Exchange: config.Config.RabbitMQ.Exchanges.PriceUpdates}
	if catalog := handlers.Catalog; catalog != nil {
		route.Before = catalog.Invalidate
	}
	var opts outbox.Options
	if watcher != nil {
		opts.Paused = func() bool { return watcher.Paused(platform.FeatureOutbox) }
	}
	relay := outbox.NewRelay(services.Postgres.DB, services.RabbitMQ,
		map[string]outbox.Route{models.CurrencyEventTopic: route}, opts)
	handlers.Admin.Outbox = relay
	return relay
}

// newDelistingScheduler returns the scheduler that applies the steps of
// delistings (#216) and the phases of migrations (#215) that are due. Every instance runs one; each step is
// applied once. After a change of the public view, it drops the cached
// currency list and nudges the relay, which publishes the currency event
// and drops the list again.
func newDelistingScheduler(services *models.InfraServices, handlers *InfraHandlers, relay *outbox.Relay) *currencies.Scheduler {
	interval := time.Duration(config.Config.Delisting.IntervalSeconds) * time.Second
	return currencies.NewScheduler(services.Postgres.DB, interval, func(ctx context.Context, changes []currencies.Change) {
		if !slices.ContainsFunc(changes, func(c currencies.Change) bool { return c.Public }) {
			return
		}
		if catalog := handlers.Catalog; catalog != nil {
			if err := catalog.Invalidate(ctx); err != nil {
				log.Printf("Error dropping the cached currency list after a delisting step; the outbox relay retries: %v", err)
			}
		}
		relay.Nudge()
	}, delisting.Advance, rebrand.Advance)
}

// useRowSeals seals the protected rows with the row-mac data keys (#139),
// and starts the background verifier. Without data keys, no row is sealed.
func useRowSeals(services *models.InfraServices, dataKeys *keys.Live) *rowseal.Verifier {
	mode := config.Config.Keys.RowSeals.Mode
	sealer := rowseal.New(services.Postgres.DB, dataKeys, mode)
	rowseal.Use(sealer)
	// The audit events are sealed with the same keys, at insert (#337).
	audit.UseSealer(sealer)
	log.Printf("Row seals are on, in %s mode", sealer.Mode())
	verifier := rowseal.NewVerifier(sealer, time.Duration(config.Config.Keys.RowSeals.VerifyIntervalMinutes)*time.Minute)
	verifier.Start()
	return verifier
}

// useDataKeysForOTP makes every sign-in code store protect its sessions with
// the otp-mac data keys, which every instance shares.
func useDataKeysForOTP(h *InfraHandlers) {
	if h.DataKeys == nil {
		return
	}
	// The handlers hold their stores as interfaces (#48); the stores of
	// the server are *otp.Store.
	stores := []any{h.Public.OTP, h.User.ContactOTP, h.Admin.OTP}
	if service, ok := h.User.Confirmations.(confirm.Service); ok {
		stores = append(stores, service.Codes)
	}
	for _, store := range stores {
		if s, ok := store.(*otp.Store); ok && s != nil {
			s.UseDataKeys(h.DataKeys)
		}
	}
	// The secrets of the authenticator apps of users are sealed with the
	// same keys (#307). Without keys, none is enrolled or checked.
	if h.TOTP != nil {
		h.TOTP.Sealer = h.DataKeys
	}
}

// validateConfiguration checks the loaded configuration as the server needs
// it: the settings of internal/config, the sign-in settings, and the key
// provider, with the release checks in release mode. It returns every
// problem at once, and the key provider when there is none.
func validateConfiguration() (keys.Provider, error) {
	release := config.Config.Server.Mode == gin.ReleaseMode
	provider, keyErr := dataKeyProvider(release)
	err := errors.Join(
		config.ValidateServer(),
		otp.SettingsOf(&config.Config).Validate(),
		notify.CheckDevLogCodes(config.Config.OTP.Auth.DevLogCodes, release),
		keyErr,
		platformSettingsErr(),
	)
	if err != nil {
		return nil, err
	}
	return provider, nil
}

// problemList formats the problems of an error from validateConfiguration,
// one per line.
// platformSettingsErr reports the unknown features of the platform
// settings, with the other problems of the configuration.
func platformSettingsErr() error {
	_, err := platform.SettingsOf(&config.Config)
	return err
}

func problemList(err error) string {
	lines := strings.Split(err.Error(), "\n")
	for i, line := range lines {
		lines[i] = "  - " + line
	}
	return strings.Join(lines, "\n")
}

// errKeysProviderRequired refuses a release without the data keys.
var errKeysProviderRequired = errors.New("keys.provider (KEYS_PROVIDER) is not set; release mode needs the data keys (docs/data-protection.md)")

// dataKeyProvider returns the provider of the key-encryption key that
// keys.provider selects, or nil when it is not set. Release mode refuses a
// missing provider, a development provider, and a key service reached
// without TLS: the data keys protect the sign-in codes, the authenticator
// secrets of administrators, and the row seals, and every instance must
// share them.
func dataKeyProvider(releaseMode bool) (keys.Provider, error) {
	provider, err := keys.FromConfig(config.Config.Keys)
	if err != nil {
		return nil, err
	}
	if provider == nil {
		if releaseMode {
			return nil, errKeysProviderRequired
		}
		return nil, nil
	}
	if err := keys.CheckRelease(provider, releaseMode); err != nil {
		return nil, err
	}
	return provider, nil
}

// loadDataKeys unwraps the data-protection keys, creating the first version
// of each purpose on the first start. The keys stay in memory; the returned
// keyring reloads them when data_keys changes, once started (#233), so a
// rotation reaches every instance without a restart.
func loadDataKeys(services *models.InfraServices, provider keys.Provider) (*keys.Live, error) {
	if provider == nil {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store := &keys.Store{DB: services.Postgres.DB, Provider: provider}
	live, err := keys.NewLive(ctx, store, keys.LiveOptions{
		ReloadInterval: time.Duration(config.Config.Keys.ReloadIntervalSeconds) * time.Second,
	})
	if err != nil {
		return nil, err
	}
	var current []string
	for _, purpose := range keys.Purposes {
		key, _ := live.Current(purpose)
		current = append(current, key.String())
	}
	log.Printf("Loaded the data-protection keys with %s; current versions: %s; reloading every %v", provider.ID(),
		strings.Join(current, ", "), live.Interval())
	return live, nil
}

// configureIdentity sets the mobile ownership provider of the contact
// changes (#253), and the keys that read national IDs.
func configureIdentity(h *InfraHandlers) error {
	ownership, err := identity.FromConfig(config.Config.Identity.MobileOwnership)
	if err != nil {
		return err
	}
	h.User.Ownership, h.Public.Ownership = ownership, ownership
	if h.UserStore != nil {
		h.UserStore.Ownership = ownership
	}
	if h.SignIn != nil {
		h.SignIn.Ownership = ownership
	}
	if ownership == nil {
		log.Println("WARNING: identity.mobile_ownership.provider is none: users with a verified national ID cannot add or replace a mobile number")
	}
	if h.DataKeys != nil {
		if h.UserStore != nil {
			h.UserStore.Sealer = h.DataKeys
		}
		if h.SignIn != nil {
			h.SignIn.Sealer = h.DataKeys
		}
	}
	return configureDestinations(h)
}

// useConfirmationSender gives the confirmations of withdrawals (#161) the
// delivery of their codes by SMS and email.
func useConfirmationSender(h *InfraHandlers, sender notify.OTPSender) {
	if service, ok := h.User.Confirmations.(confirm.Service); ok {
		service.Sender = sender
		h.User.Confirmations = service
	}
}

// configureDestinations sets the provider that checks the owner of a
// card, an IBAN, or a bank account before it joins a user's withdrawal
// destinations (#160), and the keys that read the national IDs it is
// checked against.
func configureDestinations(h *InfraHandlers) error {
	bank, err := identity.BankFromConfig(config.Config.Identity.BankOwnership)
	if err != nil {
		return err
	}
	store, ok := h.User.Destinations.(destinations.Store)
	if !ok {
		return nil
	}
	store.Ownership, store.OwnershipTimeout = bank, config.Config.Identity.BankOwnership.Timeout()
	if h.DataKeys != nil {
		store.NationalIDs = h.DataKeys
	}
	h.User.Destinations = store
	if bank == nil {
		log.Println("WARNING: identity.bank_ownership.provider is none: users cannot add cards, IBANs, or bank accounts as withdrawal destinations")
	}
	return nil
}

// configureAdminSignIn enables admin sign-in when the data keys are loaded:
// they seal the authenticator secrets. Codes are emailed through sender,
// like user sign-in codes.
func configureAdminSignIn(h *InfraHandlers, services *models.InfraServices, sender notify.OTPSender) error {
	h.Admin.OTPSender = sender
	if h.DataKeys == nil {
		return nil // run has logged that keys.provider is not set
	}
	accounts, err := adminauth.NewService(services.Postgres.DB, h.DataKeys, config.Config.Exchange.Name)
	if err != nil {
		return err
	}
	h.Admin.Accounts = accounts
	if !auth.KeysLoaded(auth.RoleAdmin) {
		log.Println("WARNING: admin sign-in needs jwt.ssl.admin keys and is unavailable until they are set")
	}
	if !sender.Supports(notify.ContactEmail) {
		log.Println("WARNING: admin sign-in needs email delivery (notifications.email) and is unavailable until it is configured")
	}
	return nil
}

// otpTTL is the lifetime of sign-in codes, including the default that the OTP
// store applies when otp.auth.ttl_seconds is not set, so that queued jobs and
// email templates agree with the store.
func otpTTL() time.Duration {
	return otp.SettingsOf(&config.Config).WithDefaults().TTL
}

// newOTPDelivery selects how sign-in codes are delivered: to the log in
// development when otp.auth.dev_log_codes is set, and otherwise through the
// notifications exchange to a worker that sends them over the configured
// channels. The worker is nil when it is not needed.
//
// The worker remembers the delivered jobs in Redis, so that a job that the
// broker hands out again is not sent twice.
func newOTPDelivery(channels notify.Channels, broker *rabbitmq.RabbitMQService, redisClient redis.UniversalClient) (notify.OTPSender, *notify.Worker) {
	if config.Config.OTP.Auth.DevLogCodes {
		log.Println("WARNING: otp.auth.dev_log_codes is enabled: sign-in codes are written to the log instead of being delivered")
		return notify.DevLogSender{Logger: log.Default()}, nil
	}

	exchange := config.Config.RabbitMQ.Exchanges.Notifications
	sender := notify.NewQueueSender(broker, exchange, channels, otpTTL())
	if channels.Empty() {
		log.Println("WARNING: no OTP delivery channel is configured (notifications.email, notifications.sms): sign-in is unavailable")
		return sender, nil
	}
	if channels.Email == nil {
		log.Println("WARNING: notifications.email is not configured: sign-in by email is unavailable")
	}
	if channels.SMS == nil {
		log.Println("WARNING: notifications.sms is not configured: sign-in by mobile is unavailable")
	}
	var delivered notify.DeliveryLog
	if redisClient != nil {
		delivered = notify.NewRedisDeliveryLog(redisClient)
	}
	worker := notify.NewWorker(broker, channels, notify.WorkerOptions{
		Exchange:    exchange,
		Concurrency: config.Config.Notifications.Worker.Concurrency,
		MaxAttempts: config.Config.Notifications.Worker.MaxAttempts,
		Delivered:   delivered,
	})
	return sender, worker
}

// newEngine creates the Gin engine. Gin trusts forwarding headers from every
// peer by default; restricting them to server.trusted_proxies (none unless
// configured) stops clients from choosing the address that rate limiting and
// logging see.
func newEngine() (*gin.Engine, error) {
	r := gin.New()
	if err := r.SetTrustedProxies(config.Config.Server.TrustedProxies); err != nil {
		return nil, fmt.Errorf("invalid server.trusted_proxies: %w", err)
	}
	return r, nil
}

// initAuthKeys loads the signing keys of each role. User keys are required.
// Admin keys are optional: without them, admin routes reject every token.
func initAuthKeys() error {
	userKeys, err := readKeyPair(config.Config.JWT.SSL.User)
	if err != nil {
		return fmt.Errorf("jwt.ssl.user: %w", err)
	}
	pairs := map[string]auth.KeyPair{auth.RoleUser: userKeys}

	admin := config.Config.JWT.SSL.Admin
	switch {
	case admin.PrivateKey == "" && admin.PublicKey == "":
		log.Println("WARNING: jwt.ssl.admin keys are not configured: admin routes reject every token")
	default:
		adminKeys, err := readKeyPair(admin)
		if err != nil {
			return fmt.Errorf("jwt.ssl.admin: %w", err)
		}
		pairs[auth.RoleAdmin] = adminKeys
	}
	return auth.SetKeys(pairs)
}

func readKeyPair(paths config.JWTKeyPaths) (auth.KeyPair, error) {
	read := func(name string, path config.KeyPath) ([]byte, error) {
		if path == "" {
			return nil, fmt.Errorf("%s is not configured", name)
		}
		data, err := os.ReadFile(string(path))
		if err != nil {
			return nil, fmt.Errorf("failed to read %s: %w", name, err)
		}
		return data, nil
	}

	var pair auth.KeyPair
	var err error
	if pair.PrivateKey, err = read("private_key", paths.PrivateKey); err != nil {
		return auth.KeyPair{}, err
	}
	if pair.PublicKey, err = read("public_key", paths.PublicKey); err != nil {
		return auth.KeyPair{}, err
	}
	for i, path := range paths.RetiredPublicKeys {
		retired, err := read(fmt.Sprintf("retired_public_keys[%d]", i), path)
		if err != nil {
			return auth.KeyPair{}, err
		}
		pair.Retired = append(pair.Retired, retired)
	}
	for i, path := range paths.NextPublicKeys {
		next, err := read(fmt.Sprintf("next_public_keys[%d]", i), path)
		if err != nil {
			return auth.KeyPair{}, err
		}
		pair.Next = append(pair.Next, next)
	}
	return pair, nil
}

// checkRedisEviction checks that Redis keeps every key (#305). Under any
// maxmemory-policy but noeviction, Redis may drop the blacklist of
// logged-out tokens, the attempt counters of sign-in codes, and the rate
// limits, and their checks then pass without an error. In release mode a
// policy that evicts, or one that cannot be read, stops the server, unless
// assumeNoEviction says that it was checked elsewhere; otherwise it is
// logged. It returns the watch of the readiness probe, or nil when Redis
// does not tell how many keys it evicted.
func checkRedisEviction(client redis.UniversalClient, release, assumeNoEviction bool) (*redisutil.EvictionWatch, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	switch err := redisutil.CheckNoEviction(ctx, client); {
	case err == nil:
	case assumeNoEviction:
		log.Printf("WARNING: redis.assume_noeviction is set, and the policy of Redis could not be confirmed: %v", err)
	case release && errors.Is(err, redisutil.ErrEvictingPolicy):
		return nil, fmt.Errorf("redis may evict the keys that security depends on (docs/redis-high-availability.md#memory-and-eviction): %w", err)
	case release:
		return nil, fmt.Errorf("cannot read the maxmemory-policy of Redis: %w; set redis.assume_noeviction (REDIS_ASSUME_NOEVICTION) once you have checked that it is %s", err, redisutil.NoEviction)
	default:
		log.Printf("WARNING: Redis may evict the keys that security depends on, which release mode refuses: %v", err)
	}
	watch, err := redisutil.NewEvictionWatch(ctx, client)
	if err != nil {
		log.Printf("WARNING: cannot read the count of keys that Redis evicted, so /system/ready does not report evictions: %v", err)
		return nil, nil
	}
	return watch, nil
}

// configurePackages installs the configuration of the process in the
// packages whose functions read it without an object to carry it: the
// tokens and sessions of auth, and the security settings of userauth (#48).
func configurePackages(cfg *config.Configuration) {
	auth.Configure(cfg)
	userauth.Configure(cfg)
}

func initializeServices() (*models.InfraServices, func(), error) {
	configurePackages(&config.Config)
	postgresService, err := db.NewPostgresService(dbOptions())
	if err != nil {
		return nil, nil, err
	}

	redisService, err := db.NewRedisService(&config.Config)
	if err != nil {
		postgresService.Close()
		return nil, nil, err
	}

	exchanges := []string{
		config.Config.RabbitMQ.Exchanges.PriceUpdates,
		config.Config.RabbitMQ.Exchanges.TradeEvents,
		config.Config.RabbitMQ.Exchanges.Notifications,
	}
	rabbitMQService, err := rabbitmq.NewRabbitMQService(&config.Config, exchanges...)
	if err != nil {
		postgresService.Close()
		redisService.Close()
		return nil, nil, err
	}

	services := &models.InfraServices{
		Postgres: postgresService,
		Redis:    redisService,
		RabbitMQ: rabbitMQService,
		Config:   &config.Config,
	}

	cleanup := func() {
		rabbitMQService.Close()
		redisService.Close()
		postgresService.Close()
	}

	return services, cleanup, nil
}

func initializeHandlers(services *models.InfraServices) *InfraHandlers {
	tb := auth.NewTokenBlacklist(services.Redis.Client)
	sessions := auth.NewPostgresSessionStore(services.Postgres.DB)
	ws := socket.NewWebSocketHandler(services)
	// A WebSocket connection that brings a user access token gets the
	// checks of the /user routes.
	ws.Authenticate = func(ctx context.Context, token string) (*auth.Claims, error) {
		return auth.AuthenticateUserToken(ctx, token, tb, sessions)
	}
	// An open connection keeps those checks: it is closed once its session
	// ends or its user can no longer sign in (#189).
	ws.VerifySession = sessions.Verify
	// The connection limits per address and per user hold across the
	// instances, counted in Redis (#234).
	ws.Counter = socket.NewRedisConnectionCounter(services.Redis.Client)
	userHandler := user.NewHandler(services)
	userStore, _ := userHandler.Store.(*user.DBStore)
	userHandler.Sessions = sessions
	publicHandler := public.NewHandler(services)
	publicHandler.AuthenticateToken = func(ctx context.Context, token string) (*auth.Claims, error) {
		return auth.AuthenticateUserToken(ctx, token, tb, sessions)
	}
	// The concrete stores of the handlers, which the composition root
	// configures later and the tests reach.
	signIn, _ := publicHandler.Accounts.(*signin.Accounts)
	catalog, _ := publicHandler.Currencies.(*currencies.Catalog)
	// The account passwords (#307). The slow hash is wired in here, and
	// runs only behind the checks of userauth: a challenge that a correct
	// code issued, and an attempt counted against the lock.
	passwords := userauth.NewPasswords(services.Postgres.DB, passwordhash.Argon2id{})
	userHandler.Passwords, publicHandler.Passwords = passwords, passwords
	// The authenticator apps; useDataKeysForOTP gives them the keys.
	totpStore := &userauth.TOTP{DB: services.Postgres.DB, Passwords: passwords, Issuer: config.Config.Exchange.Name}
	userHandler.TOTP, publicHandler.TOTP = totpStore, totpStore
	// The passkeys of users. Without security.passkeys.rp_id the service
	// is disabled, and its routes say so.
	passkeys, err := passkey.New(services.Postgres.DB, services.Redis.Client, passkeyConfig())
	if err != nil {
		log.Printf("WARNING: passkeys are off: %v", err)
		passkeys, _ = passkey.New(services.Postgres.DB, services.Redis.Client, passkey.Config{})
	}
	userHandler.Passkeys, publicHandler.Passkeys = passkeys, passkeys
	// The confirmations of withdrawals (#161): by passkey (#373), by the
	// authenticator app, or by codes of their own namespace, so that a
	// confirmation code cannot sign in and a sign-in code cannot confirm.
	// Run gives them the delivery of the codes.
	confirmCodes := otp.SettingsOf(&config.Config)
	confirmCodes.Namespace = confirm.Namespace
	// The anti-phishing code that the emails to verified contacts show
	// (#375): the codes of sign-ins, contact changes, and confirmations.
	antiPhishing := antiPhishingStore(services.Postgres.DB)
	publicHandler.AntiPhishing, userHandler.AntiPhishing = antiPhishing, antiPhishing
	userHandler.Confirmations = confirm.Service{DB: services.Postgres.DB,
		Passkeys:          passkeys,
		Codes:             otp.NewStore(services.Redis.Client, confirmCodes),
		AntiPhishing:      antiPhishing,
		Authenticator:     totpStore,
		SMSForWithdrawals: config.Config.Notifications.SMS.Kavenegar.WithdrawalTemplate != "" || config.Config.OTP.Auth.DevLogCodes}
	guard := &userauth.Guard{DB: services.Postgres.DB}
	userHandler.Guard, publicHandler.Guard = guard, guard
	marketData := newMarketDataAPI(services)
	return &InfraHandlers{
		Socket:     ws,
		System:     system.NewHandler(services),
		Admin:      admin.NewHandler(services),
		Public:     publicHandler,
		User:       userHandler,
		MarketData: marketData,
		// A change of the administrators drops the cache of the public
		// routes on every instance.
		MarketDataAdmin: marketdataadmin.New(marketdata.NewStore(services.Postgres.DB), marketData.Invalidate),
		Redis:           services.Redis,
		TokenBlacklist:  tb,
		Sessions:        sessions,
		SignIn:          signIn,
		Catalog:         catalog,
		Passwords:       passwords,
		UserStore:       userStore,
		TOTP:            totpStore,
	}
}

// newMarketDataAPI builds the public routes of the market data catalog,
// which cmd/market-worker fills. Their answers are cached in Redis, shared
// by every instance.
func newMarketDataAPI(services *models.InfraServices) *marketdataapi.Handler {
	md := config.Config.MarketData
	return marketdataapi.New(marketdata.NewStore(services.Postgres.DB),
		marketdataapi.RedisCache{Client: services.Redis.Client},
		marketdataapi.Options{
			Regions:  md.API.Regions,
			CacheTTL: time.Duration(md.API.CacheSeconds) * time.Second,
			Rules: marketdata.QualityRules{
				StaleAfter:   time.Duration(md.Tickers.StaleAfterSeconds) * time.Second,
				MaxDeviation: decimal.New(int64(md.Tickers.OutlierPercent), -2),
				MinVenues:    md.Tickers.MinVenues,
			},
		})
}

// userLogout ends the session of a user access token: it revokes the
// session, its refresh token, and the access token, and clears the refresh
// token cookie.
//
// @Summary Sign out
// @Description Both routes end the session. POST /api/v1/public/auth/logout exists because the refresh token cookie is only sent to /api/v1/public/auth, so only that route can clear it in a browser.
// @Tags sign-in
// @Produce json
// @Security BearerAuth
// @Success 200 {object} models.APIResponse
// @Failure 401 {object} models.APIResponse
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/user/logout [post]
// @Router /api/v1/public/auth/logout [post]
func userLogout(ih *InfraHandlers) gin.HandlerFunc {
	return func(c *gin.Context) {
		auth.LogoutHandler(c, ih.TokenBlacklist, ih.Sessions)
	}
}

// adminLogout ends the session of an admin access token.
//
// @Summary Sign out an administrator
// @Tags admin sign-in
// @Produce json
// @Security BearerAuth
// @Success 200 {object} models.APIResponse
// @Failure 401 {object} models.APIResponse
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/admin/logout [post]
func adminLogout(ih *InfraHandlers) gin.HandlerFunc {
	return ih.Admin.Logout(ih.TokenBlacklist)
}

// registerAPI registers the routes of the API under g.
func registerAPI(g *gin.RouterGroup, ih *InfraHandlers, rl *middleware.RateLimiter, adminAccess middleware.AdminAccess) {
	// The platform controls (#235): a full maintenance closes every route
	// of users but the status, sign-out, and ending sessions; a lockdown
	// closes its feature. The WebSocket checks them at the upgrade, after
	// the token, so that a maintenance can let allowed users through.
	gate := ih.Platform
	service := gate.Require(platform.UseService)
	signIn := gate.Require(platform.UseService, platform.FeatureSignIn)

	socketGroup := g.Group("/ws")
	{
		socketGroup.Use(rl.WSRateLimit())
		socketGroup.GET("/", ih.Socket.ServeWSGin)
	}

	// Public routes fall under the REST limit. Sign-in adds limits of its own:
	// code requests per hour and code verifications per minute.
	publicGroup := g.Group("/public")
	{
		publicGroup.GET("/status", ih.Public.PlatformStatus)
		publicGroup.GET("/locales", public.Locales(ih.Locales))
		siteHandler := newSiteHandler(ih)
		publicGroup.GET("/site", siteHandler.Site)
		publicGroup.GET("/site/redirect", siteHandler.Redirect)
		publicGroup.POST("/site/choice", siteHandler.Choice)
		publicGroup.GET("/currencies", service, ih.Public.GetActiveCurrencies)
		publicGroup.GET("/currencies/:hk", service, ih.Public.GetCurrencyByHK)
		publicGroup.POST("/auth", rl.AuthRateLimit(middleware.AuthOTP), signIn, ih.Public.RequestOTP)
		publicGroup.POST("/auth/:uuid", rl.AuthRateLimit(middleware.AuthLogin), signIn, ih.Public.VerifyOTP)
		publicGroup.POST("/auth/second-step", rl.AuthRateLimit(middleware.AuthLogin), signIn, ih.Public.SecondStep)
		publicGroup.POST("/auth/second-step/passkey", rl.AuthRateLimit(middleware.AuthLogin), signIn, ih.Public.SecondStepPasskeyOptions)
		publicGroup.POST("/auth/password-reset", rl.AuthRateLimit(middleware.AuthLogin), signIn, ih.Public.ResetPassword)
		publicGroup.POST("/auth/sign-up", rl.AuthRateLimit(middleware.AuthLogin), signIn, ih.Public.SignUp)
		publicGroup.POST("/auth/recovery", rl.AuthRateLimit(middleware.AuthLogin), signIn, ih.Public.Recover)
		publicGroup.POST("/auth/recovery/complete", rl.AuthRateLimit(middleware.AuthLogin), signIn, ih.Public.CompleteRecovery)
		publicGroup.POST("/auth/sessions/replace", rl.AuthRateLimit(middleware.AuthLogin), signIn, ih.Public.ReplaceSession)
		publicGroup.POST("/auth/refresh", service, ih.Public.RefreshToken)
		// The refresh token cookie is scoped to /public/auth, so logout lives
		// here too, behind the same authentication as /user/logout.
		publicGroup.POST("/auth/logout", auth.JWTUserVerification(ih.TokenBlacklist), userLogout(ih))
	}

	// Admin sign-in is public, rate-limited like user sign-in, and separate
	// from the admin group, which requires an admin token.
	// Where the admin routes can be reached from (#308) is checked before
	// a token or a sign-in request is read.
	adminAuthGroup := g.Group("/admin/auth")
	adminAuthGroup.Use(adminAccess.Middleware())
	{
		adminAuthGroup.POST("", rl.AuthRateLimit(middleware.AuthOTP), ih.Admin.RequestSignIn)
		adminAuthGroup.POST("/:uuid", rl.AuthRateLimit(middleware.AuthLogin), ih.Admin.VerifySignIn)
	}

	// The market data catalog (#269) is public and read only. Its answers
	// are cached, so the REST limit is enough.
	ih.MarketData.Register(g, service)

	adminGroup := g.Group("/admin")
	adminGroup.Use(adminAccess.Middleware())
	adminGroup.Use(auth.JWTAdminVerification(ih.TokenBlacklist))
	adminGroup.Use(ih.Admin.RequireActiveAdmin())
	adminGroup.Use(rl.TradingRateLimit())
	// Every admin route that changes something has a class in adminRoutes;
	// the sensitive ones need a step-up (#308).
	adminGroup.Use(ih.Admin.RequireStepUp(adminRouteClass))
	{
		adminGroup.POST("/step-up", ih.Admin.StepUp)
		adminGroup.POST("/currencies", ih.Admin.CreateCurrency)
		adminGroup.GET("/currencies", ih.Admin.ListCurrencies)
		adminGroup.GET("/currencies/:hk", ih.Admin.GetCurrency)
		adminGroup.GET("/audit", ih.Admin.ListAuditEvents)
		adminGroup.GET("/integrity/findings", ih.Admin.ListIntegrityFindings)
		adminGroup.POST("/integrity/findings/:id/reseal", ih.Admin.ResealIntegrityFinding)
		adminGroup.PUT("/currencies/:hk", ih.Admin.UpdateCurrency)
		adminGroup.DELETE("/currencies/:hk", ih.Admin.DeleteCurrency)
		adminGroup.POST("/currencies/:hk/restore", ih.Admin.RestoreCurrency)
		adminGroup.DELETE("/currencies/:hk/symbols/:symbol", ih.Admin.ReleaseSymbol)
		adminGroup.GET("/currencies/:hk/migrations", ih.Admin.ListMigrations)
		adminGroup.POST("/currencies/:hk/migrations", ih.Admin.ScheduleMigration)
		adminGroup.POST("/currencies/:hk/migrations/:id/cancel", ih.Admin.CancelMigration)
		adminGroup.GET("/currencies/:hk/delistings", ih.Admin.ListDelistings)
		adminGroup.POST("/currencies/:hk/delistings", ih.Admin.ScheduleDelisting)
		adminGroup.POST("/currencies/:hk/delistings/:id/cancel", ih.Admin.CancelDelisting)
		adminGroup.PUT("/currencies/:hk/capabilities/:capability", ih.Admin.OverrideCapability)
		adminGroup.DELETE("/currencies/:hk/capabilities/:capability", ih.Admin.RemoveCapabilityOverride)
		adminGroup.GET("/users/:hk/restrictions", ih.Admin.ListRestrictions)
		adminGroup.POST("/users/:hk/restrictions", ih.Admin.CreateRestriction)
		adminGroup.POST("/users/:hk/restrictions/:id/lift", ih.Admin.LiftRestriction)
		adminGroup.GET("/platform", ih.Admin.GetPlatform)
		adminGroup.GET("/platform/controls", ih.Admin.ListControls)
		adminGroup.POST("/platform/controls", ih.Admin.CreateControl)
		adminGroup.PATCH("/platform/controls/:id", ih.Admin.UpdateControl)
		adminGroup.POST("/platform/controls/:id/lift", ih.Admin.LiftControl)
		adminGroup.POST("/logout", adminLogout(ih))
		adminGroup.GET("/sessions", ih.Admin.ListSessions)
		adminGroup.DELETE("/sessions/:id", ih.Admin.EndSession)
		// The administration of the market data catalog (#270). A change
		// drops the cache of the public routes on every instance.
		if ih.MarketDataAdmin != nil {
			ih.MarketDataAdmin.Register(adminGroup)
		}
	}

	userGroup := g.Group("/user")
	userGroup.Use(auth.JWTUserVerification(ih.TokenBlacklist))
	userGroup.Use(rl.TradingRateLimit())
	{
		// Logout needs no active session, so that a user whose account was
		// disabled can still end the session.
		userGroup.POST("/logout", userLogout(ih))

		// Every other user route requires an active session and a user who
		// can still sign in.
		activeGroup := userGroup.Group("", auth.RequireActiveSession(ih.Sessions))
		// Ending sessions stays open during a maintenance, as signing out
		// does: a user who lost a device must be able to end its session.
		activeGroup.DELETE("/sessions", ih.User.RevokeOtherSessions)
		activeGroup.DELETE("/sessions/:id", ih.User.RevokeSession)
		// The rest closes during a maintenance, and its changes during a
		// read-only lockdown.
		activeGroup.Use(service, gate.Writes())
		// A new version of a legal document closes the changes of a user
		// until they accept it (#316). Accepting is registered first.
		activeGroup.GET("/agreements", ih.User.GetAgreements)
		activeGroup.POST("/agreements", ih.User.AcceptAgreements)
		activeGroup.Use(ih.User.RequireAgreements())
		profileChanges := gate.Require(platform.FeatureProfileChanges)
		activeGroup.GET("/profile", ih.User.GetProfile)
		activeGroup.PATCH("/profile", profileChanges, ih.User.UpdateProfile)
		activeGroup.GET("/sessions", ih.User.ListSessions)
		activeGroup.GET("/security", ih.User.GetSecurity)
		// A password change checks the current password, so it has the
		// limit of a sign-in on top of the lock of the password.
		activeGroup.PUT("/password", rl.AuthRateLimit(middleware.AuthLogin), profileChanges, ih.User.SetPassword)
		activeGroup.POST("/step-up", rl.AuthRateLimit(middleware.AuthLogin), ih.User.StepUp)
		activeGroup.POST("/recovery-codes", profileChanges, ih.User.RequireSecondFactor(false), ih.User.GenerateRecoveryCodes)
		activeGroup.POST("/totp", profileChanges, ih.User.RequireSecondFactor(false), ih.User.BeginTOTP)
		activeGroup.POST("/totp/confirm", rl.AuthRateLimit(middleware.AuthLogin), profileChanges, ih.User.RequireSecondFactor(false), ih.User.ConfirmTOTP)
		activeGroup.DELETE("/totp", profileChanges, ih.User.RequireSecondFactor(false), ih.User.RemoveTOTP)
		activeGroup.GET("/passkeys", ih.User.ListPasskeys)
		activeGroup.POST("/passkey-registration", profileChanges, ih.User.RequireSecondFactor(false), ih.User.BeginPasskey)
		activeGroup.POST("/passkeys", profileChanges, ih.User.RequireSecondFactor(false), ih.User.AddPasskey)
		activeGroup.DELETE("/passkeys/:id", profileChanges, ih.User.RequireSecondFactor(false), ih.User.RemovePasskey)
		activeGroup.POST("/step-up/passkey", rl.AuthRateLimit(middleware.AuthLogin), ih.User.StepUpPasskeyOptions)
		activeGroup.GET("/contacts", ih.User.ListContacts)
		activeGroup.GET("/restrictions", ih.User.ListRestrictions)
		activeGroup.GET("/verification", ih.User.GetVerification)
		// The withdrawal destinations (#160). Adding one waits for a
		// lockdown of withdrawals to end, and both changes need a fresh
		// proof of the second factor. A lockdown of withdrawals does not hold
		// a removal, so that a user can take out an address that is no
		// longer safe at once.
		activeGroup.GET("/withdrawal-destinations", ih.User.ListDestinations)
		activeGroup.POST("/withdrawal-destinations", gate.Require(platform.FeatureWithdrawals), ih.User.RequireSecondFactor(true), ih.User.AddDestination)
		activeGroup.DELETE("/withdrawal-destinations/:id", ih.User.RequireSecondFactor(true), ih.User.RemoveDestination)
		// The confirmations of withdrawals with the second factor (#161).
		// Choosing the method needs a fresh proof of it; a code has the
		// limits of a sign-in, and a new code those of a code request.
		activeGroup.GET("/confirmation-method", ih.User.GetConfirmationMethod)
		activeGroup.PUT("/confirmation-method", profileChanges, ih.User.RequireSecondFactor(true), ih.User.SetConfirmationMethod)
		activeGroup.POST("/confirmations/:id", rl.AuthRateLimit(middleware.AuthLogin), ih.User.ConfirmOperation)
		activeGroup.POST("/confirmations/:id/resend", rl.AuthRateLimit(middleware.AuthOTP), ih.User.ResendConfirmation)
		// The anti-phishing code that the emails show (#375). Changing it
		// needs a fresh proof of the second factor.
		activeGroup.GET("/anti-phishing-code", ih.User.GetAntiPhishingCode)
		activeGroup.PUT("/anti-phishing-code", profileChanges, ih.User.RequireSecondFactor(false), ih.User.SetAntiPhishingCode)
		activeGroup.DELETE("/anti-phishing-code", profileChanges, ih.User.RequireSecondFactor(false), ih.User.RemoveAntiPhishingCode)
		activeGroup.GET("/favorites", ih.User.ListFavorites)
		activeGroup.PUT("/favorites/order", ih.User.ReorderFavorites)
		activeGroup.PUT("/favorites/currencies/:hk", ih.User.AddFavoriteCurrency)
		activeGroup.DELETE("/favorites/currencies/:hk", ih.User.RemoveFavoriteCurrency)
		activeGroup.PUT("/favorites/pairs/:hk", ih.User.AddFavoritePair)
		activeGroup.DELETE("/favorites/pairs/:hk", ih.User.RemoveFavoritePair)
		// Starting a change sends codes, so it has the limit of a code
		// request, on top of the limits of the codes themselves.
		// A user who has a second factor proves it before changing the
		// contacts that sign in to the account (#307).
		stepUp := ih.User.RequireSecondFactor(false)
		activeGroup.POST("/contacts/changes", rl.AuthRateLimit(middleware.AuthOTP), profileChanges, stepUp, ih.User.StartContactChange)
		activeGroup.POST("/contacts/changes/:id", rl.AuthRateLimit(middleware.AuthLogin), profileChanges, stepUp, ih.User.ConfirmContactChange)
	}
}

// probeRoutes are the health probes. They must answer while Redis is down,
// so the REST limit counts them in memory instead of in Redis.
var probeRoutes = []string{"/system/live", "/system/ready"}

// RouteHandler registers the global middleware and then every route. Gin
// applies middleware only to the routes registered after it, so no route may
// be registered before this function runs.
func RouteHandler(r *gin.Engine, ih *InfraHandlers) error {
	rl := middleware.NewRateLimiter(&config.Config, ih.Redis)
	// The pages of every site call the API (#285).
	origins := append(append([]string(nil), config.Config.Server.CORS.AllowedOrigins...), config.Config.SiteOrigins()...)
	cors, err := middleware.NewCORSMiddleware(origins)
	if err != nil {
		return fmt.Errorf("invalid server.cors.allowed_origins: %w", err)
	}

	adminAccess, err := middleware.ParseAdminAccess(config.Config.Server.AdminListenAddr != "", config.Config.Admin.AllowedCIDRs)
	if err != nil {
		return fmt.Errorf("invalid admin.allowed_cidrs: %w", err)
	}

	if ih.Locales == nil {
		locales, err := config.Config.Locales()
		if err != nil {
			return fmt.Errorf("invalid i18n settings: %w", err)
		}
		ih.Locales = locales
	}
	if ih.User != nil && ih.User.Locales == nil {
		ih.User.Locales = ih.Locales
	}
	if ih.Sites == nil {
		registry, err := config.Config.SiteRegistry(ih.Locales)
		if err != nil {
			return fmt.Errorf("invalid sites: %w", err)
		}
		ih.Sites = registry
	}

	// The span and the metrics of each request (#50) wrap everything else,
	// so that a recovered panic still ends them as a 500.
	r.Use(tracing.Middleware())
	r.Use(metrics.Middleware())
	r.Use(middleware.RecoveryMiddleware())
	r.Use(middleware.RequestID())
	// Every answer after this point, refusals of the limits included, is
	// in the visitor's language where it has a translation (#283).
	// A request that names no locale gets the default locale of the site
	// it comes from (#285).
	r.Use(i18n.Middleware(ih.Locales, ih.Sites.LocaleOfHost))
	security := middleware.SecurityOptions{
		HSTS:                  config.Config.HSTSEnabled(),
		HSTSMaxAge:            config.Config.Server.HSTS.MaxAgeSeconds,
		HSTSIncludeSubdomains: config.Config.Server.HSTS.IncludeSubdomains,
	}
	if security.HSTSMaxAge <= 0 {
		security.HSTSMaxAge = config.DefaultHSTSMaxAgeSeconds
	}
	if config.Config.SwaggerEnabled() {
		security.DocsPrefix = "/swagger/"
	}
	r.Use(middleware.SecurityHeaders(security))
	r.Use(cors)
	// The rate limit comes before the body limit, so that a flood of
	// requests is rejected before their bodies are read, and before the
	// answer to preflights, so that they are counted too.
	r.Use(rl.RESTRateLimit(probeRoutes...))
	r.Use(middleware.Preflight())
	bodyLimit := config.Config.Server.MaxBodyBytes
	if bodyLimit <= 0 {
		bodyLimit = config.DefaultMaxBodyBytes
	}
	r.Use(middleware.BodyLimit(bodyLimit))

	// Unknown routes and methods answer in the envelope too, with a code
	// and the request ID, instead of Gin's plain-text answers (#53).
	r.HandleMethodNotAllowed = true
	r.NoRoute(func(c *gin.Context) {
		helpers.SendResponse(c, http.StatusNotFound, i18n.M("NOT_FOUND"), nil)
	})
	r.NoMethod(func(c *gin.Context) {
		helpers.SendResponse(c, http.StatusMethodNotAllowed, i18n.M("METHOD_NOT_ALLOWED"), nil)
	})

	if config.Config.SwaggerEnabled() {
		// The paths of the document are absolute.
		docs.SwaggerInfo.BasePath = "/"
		r.GET("/swagger/*any", ginSwagger.WrapHandler(swaggerFiles.Handler))
	}

	systemGroup := r.Group("/system")
	{
		systemGroup.GET("/live", ih.System.LivenessHandler)
		systemGroup.GET("/ready", ih.System.ReadinessHandler)
	}

	// Every route of the API is under /api/v1. The health probes above are
	// not part of the API: orchestrators and load balancers call them.
	// Swagger, only where it is enabled (never in release mode), is the one
	// other path.
	registerAPI(r.Group(helpers.APIPrefix), ih, rl, adminAccess)
	return nil
}

// passkeyConfig returns the relying party of the passkeys from
// security.passkeys. The name falls back to exchange.name.
func passkeyConfig() passkey.Config {
	cfg := config.Config.Security.Passkeys
	name := cfg.RPName
	if name == "" {
		name = config.Config.Exchange.Name
	}
	return passkey.Config{RPID: cfg.RPID, RPName: name, Origins: cfg.Origins}
}

// dbOptions are the options of the db package: the configuration of this
// process, and its migrations.
func dbOptions() db.Options {
	return db.Options{Config: &config.Config, MigrationsPath: config.MigrationsPath()}
}
