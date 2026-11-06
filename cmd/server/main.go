package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/mdastpak/crypto-exchange/docs"
	"github.com/mdastpak/crypto-exchange/internal/adminauth"
	"github.com/mdastpak/crypto-exchange/internal/auth"
	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/db"
	"github.com/mdastpak/crypto-exchange/internal/handlers/admin"
	"github.com/mdastpak/crypto-exchange/internal/handlers/public"
	"github.com/mdastpak/crypto-exchange/internal/handlers/socket"
	"github.com/mdastpak/crypto-exchange/internal/handlers/system"
	"github.com/mdastpak/crypto-exchange/internal/handlers/user"
	"github.com/mdastpak/crypto-exchange/internal/helpers"
	"github.com/mdastpak/crypto-exchange/internal/keys"
	"github.com/mdastpak/crypto-exchange/internal/middleware"
	"github.com/mdastpak/crypto-exchange/internal/models"
	"github.com/mdastpak/crypto-exchange/internal/notify"
	"github.com/mdastpak/crypto-exchange/internal/otp"
	"github.com/mdastpak/crypto-exchange/internal/outbox"
	"github.com/mdastpak/crypto-exchange/internal/rabbitmq"

	"github.com/gin-gonic/gin"
	swaggerFiles "github.com/swaggo/files"
	ginSwagger "github.com/swaggo/gin-swagger"
)

type InfraHandlers struct {
	Socket         *socket.WebSocketHandler
	System         *system.SystemHandler
	Admin          *admin.AdminHandler
	Public         *public.PublicHandler
	User           *user.UserHandler
	Redis          *db.RedisService
	TokenBlacklist *auth.TokenBlacklist
	Sessions       auth.SessionStore
	// DataKeys holds the data-protection keys for row seals (#139) and
	// field encryption (#140). It is nil without keys.provider.
	DataKeys *keys.Keyring
}

var (
	configPath     string
	migrationsPath string
)

func init() {
	flag.StringVar(&configPath, "config", "config/config.yaml", "Path to config file")
	flag.StringVar(&migrationsPath, "migrations", "file://internal/db/migrations", "Path to migrations")
}

func main() {
	flag.Parse()
	if err := run(); err != nil {
		log.Fatal(err)
	}
	log.Println("Server stopped gracefully")
}

// run starts the server and serves until a SIGINT or SIGTERM, then stops it.
// It returns instead of exiting on an error, so that the deferred closes of
// the services run on every path.
func run() error {
	config.SetConfigPath(configPath)
	config.SetMigrationsPath(migrationsPath)
	config.LoadConfig()

	// Check the whole configuration before connecting to anything, and
	// report every problem at once.
	keyProvider, err := validateConfiguration()
	if err != nil {
		return fmt.Errorf("invalid configuration:\n%s", problemList(err))
	}

	// server.mode, which SERVER_MODE or GIN_MODE can set, selects the
	// release checks above as well as Gin's mode.
	gin.SetMode(config.Config.Server.Mode)

	if err := initAuthKeys(); err != nil {
		return fmt.Errorf("failed to initialize auth keys: %w", err)
	}

	if config.Config.OTP.Auth.Secret == "" {
		log.Println("WARNING: otp.auth.secret is not set: using a random secret, so sign-in codes can only be verified by this process until it restarts")
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
	adminTOTPKey, err := adminSignInKey()
	if err != nil {
		return fmt.Errorf("invalid admin sign-in configuration: %w", err)
	}

	services, cleanup, err := initializeServices()
	if err != nil {
		return fmt.Errorf("failed to initialize services: %w", err)
	}
	// Closes RabbitMQ, Redis, and PostgreSQL, in that order, after shutdown
	// has stopped everything that uses them.
	defer cleanup()

	handlers := initializeHandlers(services)
	if handlers.DataKeys, err = loadDataKeys(services, keyProvider); err != nil {
		return fmt.Errorf("failed to load the data-protection keys: %w", err)
	}
	otpSender, otpWorker := newOTPDelivery(otpChannels, services.RabbitMQ)
	relay := newOutboxRelay(services, handlers)
	workers := []worker{{"outbox relay", relay.Stop}}
	if otpWorker != nil {
		workers = append(workers, worker{"OTP delivery worker", otpWorker.Stop})
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
	if err := configureAdminSignIn(handlers, services, adminTOTPKey, otpSender); err != nil {
		return fmt.Errorf("failed to configure admin sign-in: %w", err)
	}
	// Forward currency events to WebSocket clients.
	if err := handlers.Socket.StartConsumer(context.Background()); err != nil {
		return fmt.Errorf("failed to start the WebSocket event consumer: %w", err)
	}
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
	r.Use(gin.Logger())
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
	listen := srv.ListenAndServe
	if config.Config.Server.TLS.Enabled {
		listen = func() error {
			return srv.ListenAndServeTLS(config.Config.Server.TLS.CertFile, config.Config.Server.TLS.KeyFile)
		}
	}

	ctx, stop := stopSignals()
	defer stop()
	return serve(ctx, srv, listen, handlers, workers, shutdownTimeout(), drainTime())
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
func newOutboxRelay(services *models.InfraServices, handlers *InfraHandlers) *outbox.Relay {
	route := outbox.Route{Exchange: config.Config.RabbitMQ.Exchanges.PriceUpdates}
	if catalog := handlers.Public.Currencies; catalog != nil {
		route.Before = catalog.Invalidate
	}
	relay := outbox.NewRelay(services.Postgres.DB, services.RabbitMQ,
		map[string]outbox.Route{models.CurrencyEventTopic: route}, outbox.Options{})
	handlers.Admin.Outbox = relay
	return relay
}

// adminSignInKey decodes admin.totp_key. Without it, admin sign-in is
// disabled and the key is nil.
func adminSignInKey() ([]byte, error) {
	if config.Config.Admin.TOTPKey == "" {
		log.Println("WARNING: admin.totp_key is not set: admin sign-in is disabled")
		return nil, nil
	}
	return adminauth.DecodeKey(config.Config.Admin.TOTPKey)
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
		otp.SettingsFromConfig().Validate(release),
		notify.CheckDevLogCodes(config.Config.OTP.Auth.DevLogCodes, release),
		keyErr,
	)
	if err != nil {
		return nil, err
	}
	return provider, nil
}

// problemList formats the problems of an error from validateConfiguration,
// one per line.
func problemList(err error) string {
	lines := strings.Split(err.Error(), "\n")
	for i, line := range lines {
		lines[i] = "  - " + line
	}
	return strings.Join(lines, "\n")
}

// dataKeyProvider returns the provider of the key-encryption key that
// keys.provider selects, or nil when it is not set. Release mode refuses a
// development provider and a key service reached without TLS.
func dataKeyProvider(releaseMode bool) (keys.Provider, error) {
	provider, err := keys.FromConfig(config.Config.Keys)
	if err != nil {
		return nil, err
	}
	if provider == nil {
		log.Println("WARNING: keys.provider is not set: the data-protection keys are not loaded")
		return nil, nil
	}
	if err := keys.CheckRelease(provider, releaseMode); err != nil {
		return nil, err
	}
	return provider, nil
}

// loadDataKeys unwraps the data-protection keys, creating the first version
// of each purpose on the first start. The key service is only needed here:
// the keys stay in memory until the process ends.
func loadDataKeys(services *models.InfraServices, provider keys.Provider) (*keys.Keyring, error) {
	if provider == nil {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ring, err := (&keys.Store{DB: services.Postgres.DB, Provider: provider}).Load(ctx)
	if err != nil {
		return nil, err
	}
	var current []string
	for _, purpose := range keys.Purposes {
		key, _ := ring.Current(purpose)
		current = append(current, key.String())
	}
	log.Printf("Loaded the data-protection keys with %s; current versions: %s", provider.ID(), strings.Join(current, ", "))
	return ring, nil
}

// configureAdminSignIn enables admin sign-in when totpKey is set. Codes are
// emailed through sender, like user sign-in codes.
func configureAdminSignIn(h *InfraHandlers, services *models.InfraServices, totpKey []byte, sender notify.OTPSender) error {
	h.Admin.OTPSender = sender
	if totpKey == nil {
		return nil
	}
	accounts, err := adminauth.NewService(services.Postgres.DB, totpKey, config.Config.Exchange.Name)
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
	return otp.SettingsFromConfig().WithDefaults().TTL
}

// newOTPDelivery selects how sign-in codes are delivered: to the log in
// development when otp.auth.dev_log_codes is set, and otherwise through the
// notifications exchange to a worker that sends them over the configured
// channels. The worker is nil when it is not needed.
func newOTPDelivery(channels notify.Channels, broker *rabbitmq.RabbitMQService) (notify.OTPSender, *notify.Worker) {
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
	worker := notify.NewWorker(broker, channels, notify.WorkerOptions{
		Exchange:    exchange,
		Concurrency: config.Config.Notifications.Worker.Concurrency,
		MaxAttempts: config.Config.Notifications.Worker.MaxAttempts,
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
	return pair, nil
}

func initializeServices() (*models.InfraServices, func(), error) {
	postgresService, err := db.NewPostgresService()
	if err != nil {
		return nil, nil, err
	}

	redisService, err := db.NewRedisService()
	if err != nil {
		postgresService.Close()
		return nil, nil, err
	}

	exchanges := []string{
		config.Config.RabbitMQ.Exchanges.PriceUpdates,
		config.Config.RabbitMQ.Exchanges.TradeEvents,
		config.Config.RabbitMQ.Exchanges.Notifications,
	}
	rabbitMQService, err := rabbitmq.NewRabbitMQService(exchanges...)
	if err != nil {
		postgresService.Close()
		redisService.Close()
		return nil, nil, err
	}

	services := &models.InfraServices{
		Postgres: postgresService,
		Redis:    redisService,
		RabbitMQ: rabbitMQService,
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
	return &InfraHandlers{
		Socket:         ws,
		System:         system.NewHandler(services),
		Admin:          admin.NewHandler(services),
		Public:         public.NewHandler(services),
		User:           user.NewHandler(services),
		Redis:          services.Redis,
		TokenBlacklist: tb,
		Sessions:       sessions,
	}
}

// probeRoutes are the health probes. They must answer while Redis is down,
// so the REST limit counts them in memory instead of in Redis.
var probeRoutes = []string{"/system/live", "/system/ready", "/system/healthcheck", "/system/ping"}

// RouteHandler registers the global middleware and then every route. Gin
// applies middleware only to the routes registered after it, so no route may
// be registered before this function runs.
func RouteHandler(r *gin.Engine, ih *InfraHandlers) error {
	rl := middleware.NewRateLimiter(ih.Redis)
	cors, err := middleware.NewCORSMiddleware(config.Config.Server.CORS.AllowedOrigins)
	if err != nil {
		return fmt.Errorf("invalid server.cors.allowed_origins: %w", err)
	}

	r.Use(middleware.RecoveryMiddleware())
	r.Use(middleware.RequestID())
	r.Use(middleware.SecurityHeaders())
	r.Use(cors)
	// The rate limit comes before the body limit, so that a flood of
	// requests is rejected before their bodies are read.
	r.Use(rl.RESTRateLimit(probeRoutes...))
	bodyLimit := config.Config.Server.MaxBodyBytes
	if bodyLimit <= 0 {
		bodyLimit = config.DefaultMaxBodyBytes
	}
	r.Use(middleware.BodyLimit(bodyLimit))

	r.GET("/", func(c *gin.Context) {
		helpers.SendResponse(c, http.StatusOK, "Welcome to the Crypto Exchange API", nil)
	})
	if config.Config.SwaggerEnabled() {
		docs.SwaggerInfo.BasePath = "/"
		r.GET("/swagger/*any", ginSwagger.WrapHandler(swaggerFiles.Handler))
	}

	socketGroup := r.Group("/ws")
	{
		socketGroup.Use(rl.WSRateLimit())
		socketGroup.GET("/", ih.Socket.ServeWSGin)
	}

	systemGroup := r.Group("/system")
	{
		systemGroup.GET("/live", ih.System.LivenessHandler)
		systemGroup.GET("/ready", ih.System.ReadinessHandler)
		systemGroup.GET("/healthcheck", ih.System.HealthCheckHandler)
		systemGroup.GET("/ping", ih.System.PingPongHandler)
	}

	// Public routes fall under the REST limit. Sign-in adds limits of its own:
	// code requests per hour and code verifications per minute.
	publicGroup := r.Group("/public")
	{
		publicGroup.GET("/currencies", ih.Public.GetActiveCurrencies)
		publicGroup.GET("/currencies/:hk", ih.Public.GetCurrencyByHK)
		publicGroup.POST("/auth", rl.AuthRateLimit(middleware.AuthOTP), ih.Public.RequestOTP)
		publicGroup.POST("/auth/:uuid", rl.AuthRateLimit(middleware.AuthLogin), ih.Public.VerifyOTP)
		publicGroup.POST("/auth/refresh", ih.Public.RefreshToken)
		// The refresh token cookie is scoped to /public/auth, so logout lives
		// here too, behind the same authentication as /user/logout.
		publicGroup.POST("/auth/logout", auth.JWTUserVerification(ih.TokenBlacklist), func(c *gin.Context) {
			auth.LogoutHandler(c, ih.TokenBlacklist, ih.Sessions)
		})
	}

	// Admin sign-in is public, rate-limited like user sign-in, and separate
	// from the admin group, which requires an admin token.
	adminAuthGroup := r.Group("/admin/auth")
	{
		adminAuthGroup.POST("", rl.AuthRateLimit(middleware.AuthOTP), ih.Admin.RequestSignIn)
		adminAuthGroup.POST("/:uuid", rl.AuthRateLimit(middleware.AuthLogin), ih.Admin.VerifySignIn)
	}

	adminGroup := r.Group("/admin")
	adminGroup.Use(auth.JWTAdminVerification(ih.TokenBlacklist))
	adminGroup.Use(ih.Admin.RequireActiveAdmin())
	adminGroup.Use(rl.TradingRateLimit())
	{
		adminGroup.POST("/currencies", ih.Admin.CreateCurrency)
		adminGroup.GET("/currencies/:hk", ih.Admin.GetCurrency)
		adminGroup.PUT("/currencies/:hk", ih.Admin.UpdateCurrency)
		adminGroup.DELETE("/currencies/:hk", ih.Admin.DeleteCurrency)
		adminGroup.POST("/logout", func(c *gin.Context) {
			// Admin tokens have no session or refresh token to revoke.
			auth.LogoutHandler(c, ih.TokenBlacklist, nil)
		})
	}

	userGroup := r.Group("/user")
	userGroup.Use(auth.JWTUserVerification(ih.TokenBlacklist))
	userGroup.Use(rl.TradingRateLimit())
	{
		// Logout needs no active session, so that a user whose account was
		// disabled can still end the session.
		userGroup.POST("/logout", func(c *gin.Context) {
			auth.LogoutHandler(c, ih.TokenBlacklist, ih.Sessions)
		})

		// Every other user route requires an active session and a user who
		// can still sign in.
		activeGroup := userGroup.Group("", auth.RequireActiveSession(ih.Sessions))
		activeGroup.GET("/profile", ih.User.GetProfile)
		activeGroup.PATCH("/profile", ih.User.UpdateProfile)
	}
	return nil
}
