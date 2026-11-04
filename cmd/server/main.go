package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"encrypted-db/config"
	"encrypted-db/docs"
	"encrypted-db/internal/adminauth"
	"encrypted-db/internal/auth"
	"encrypted-db/internal/db"
	"encrypted-db/internal/handlers/admin"
	"encrypted-db/internal/handlers/public"
	"encrypted-db/internal/handlers/socket"
	"encrypted-db/internal/handlers/system"
	"encrypted-db/internal/handlers/user"
	"encrypted-db/internal/helpers"
	"encrypted-db/internal/middleware"
	"encrypted-db/internal/models"
	"encrypted-db/internal/notify"
	"encrypted-db/internal/otp"
	"encrypted-db/internal/rabbitmq"

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
	config.SetConfigPath(configPath)
	config.SetMigrationsPath(migrationsPath)
	config.LoadConfig()
	if err := config.ValidateServer(); err != nil {
		log.Fatalf("Invalid configuration: %v", err)
	}

	// server.mode, which SERVER_MODE or GIN_MODE can set, selects the
	// release checks below as well as Gin's mode.
	gin.SetMode(config.Config.Server.Mode)

	if err := initAuthKeys(); err != nil {
		log.Fatalf("Failed to initialize auth keys: %v", err)
	}

	// Validate the sign-in configuration before connecting to anything.
	if err := otp.SettingsFromConfig().Validate(gin.Mode() == gin.ReleaseMode); err != nil {
		log.Fatalf("Invalid OTP configuration: %v", err)
	}
	if config.Config.OTP.Auth.Secret == "" {
		log.Println("WARNING: otp.auth.secret is not set: using a random secret, so sign-in codes can only be verified by this process until it restarts")
	}
	if err := notify.CheckDevLogCodes(config.Config.OTP.Auth.DevLogCodes, gin.Mode() == gin.ReleaseMode); err != nil {
		log.Fatalf("Invalid OTP delivery configuration: %v", err)
	}
	otpChannels, err := notify.NewChannels(config.Config.Notifications, otpTTL())
	if err != nil {
		log.Fatalf("Invalid OTP delivery configuration: %v", err)
	}
	adminTOTPKey, err := adminSignInKey()
	if err != nil {
		log.Fatalf("Invalid admin sign-in configuration: %v", err)
	}

	services, cleanup, err := initializeServices()
	if err != nil {
		log.Fatalf("Failed to initialize services: %v", err)
	}
	defer cleanup()

	handlers := initializeHandlers(services)
	otpSender, otpWorker := newOTPDelivery(otpChannels, services.RabbitMQ)
	handlers.Public.OTPSender = otpSender
	if err := configureAdminSignIn(handlers, services, adminTOTPKey, otpSender); err != nil {
		log.Fatalf("Failed to configure admin sign-in: %v", err)
	}
	// Forward currency events to WebSocket clients. Socket.Shutdown stops
	// the consumer before RabbitMQ is closed.
	if err := handlers.Socket.StartConsumer(context.Background()); err != nil {
		log.Fatalf("Failed to start the WebSocket event consumer: %v", err)
	}
	if otpWorker != nil {
		if err := otpWorker.Start(); err != nil {
			log.Fatalf("Failed to start the OTP delivery worker: %v", err)
		}
	}

	if err := initializeCache(handlers); err != nil {
		log.Fatalf("Failed to initialize cache: %v", err)
	}

	serverAddr := fmt.Sprintf("%s:%s", config.Config.Server.IP, config.Config.Server.Port)

	r, err := newEngine()
	if err != nil {
		log.Fatalf("Failed to create the HTTP engine: %v", err)
	}
	r.Use(middleware.RecoveryMiddleware())
	r.Use(requestIDMiddleware())
	r.Use(gin.Logger())

	docs.SwaggerInfo.BasePath = "/"
	r.GET("/swagger/*any", ginSwagger.WrapHandler(swaggerFiles.Handler))

	r.GET("/", func(c *gin.Context) {
		helpers.SendResponse(c, http.StatusOK, "Welcome to the Encrypted-DB API", nil)
	})

	if err := RouteHandler(r, handlers); err != nil {
		log.Fatalf("Failed to register routes: %v", err)
	}

	protocol := "http"
	if config.Config.Server.TLS.Enabled {
		protocol = "https"
	}
	log.Printf("Starting server on: %s://%s", protocol, serverAddr)
	log.Printf("Swagger documentation available at: %s://%s/swagger/index.html", protocol, serverAddr)

	srv := &http.Server{
		Addr:         serverAddr,
		Handler:      r,
		ReadTimeout:  time.Duration(config.Config.Server.ReadTimeout) * time.Second,
		WriteTimeout: time.Duration(config.Config.Server.WriteTimeout) * time.Second,
		IdleTimeout:  time.Duration(config.Config.Server.IdleTimeout) * time.Second,
	}

	go func() {
		var err error
		if config.Config.Server.TLS.Enabled {
			err = srv.ListenAndServeTLS(config.Config.Server.TLS.CertFile, config.Config.Server.TLS.KeyFile)
		} else {
			err = srv.ListenAndServe()
		}
		if err != nil && err != http.ErrServerClosed {
			log.Fatalf("Could not start server: %v", err)
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	<-ctx.Done()

	log.Println("Shutting down server...")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := shutdown(shutdownCtx, srv, handlers, otpWorker); err != nil {
		log.Fatalf("Server forced to shutdown: %v", err)
	}
	log.Println("Server stopped gracefully")
}

// shutdown stops this instance: its WebSocket clients, its HTTP server, and
// its OTP delivery worker. It leaves the shared state in Redis, such as the
// currency cache, untouched, because the other instances keep serving from
// it. The caller closes the services afterwards.
func shutdown(ctx context.Context, srv *http.Server, handlers *InfraHandlers, otpWorker *notify.Worker) error {
	if err := handlers.Socket.Shutdown(ctx); err != nil {
		log.Printf("Error during WebSocket shutdown: %v", err)
	}

	if err := srv.Shutdown(ctx); err != nil {
		return err
	}
	// The worker publishes dead-letter records, so it stops before the
	// services, and RabbitMQ with them, are closed.
	if otpWorker != nil {
		if err := otpWorker.Stop(ctx); err != nil {
			log.Printf("Error while stopping the OTP delivery worker: %v", err)
		}
	}
	return nil
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
	return &InfraHandlers{
		Socket:         socket.NewWebSocketHandler(services),
		System:         system.NewHandler(services),
		Admin:          admin.NewHandler(services),
		Public:         public.NewHandler(services),
		User:           user.NewHandler(services),
		Redis:          services.Redis,
		TokenBlacklist: tb,
		Sessions:       auth.NewPostgresSessionStore(services.Postgres.DB),
	}
}

func initializeCache(ih *InfraHandlers) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return ih.Admin.LoadAndCacheCurrencies(ctx)
}

func RouteHandler(r *gin.Engine, ih *InfraHandlers) error {
	rl := middleware.NewRateLimiter(ih.Redis)
	cors, err := middleware.NewCORSMiddleware(config.Config.Server.CORS.AllowedOrigins)
	if err != nil {
		return fmt.Errorf("invalid server.cors.allowed_origins: %w", err)
	}

	r.Use(middleware.SecurityHeaders())
	r.Use(cors)
	r.Use(rl.RESTRateLimit())

	socketGroup := r.Group("/ws")
	{
		socketGroup.Use(rl.WSRateLimit())
		socketGroup.GET("/", ih.Socket.ServeWSGin)
	}

	systemGroup := r.Group("/system")
	{
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
		userGroup.GET("/profile", ih.User.GetProfile)
		userGroup.PATCH("/profile", ih.User.UpdateProfile)
		userGroup.POST("/logout", func(c *gin.Context) {
			auth.LogoutHandler(c, ih.TokenBlacklist, ih.Sessions)
		})
	}
	return nil
}

func requestIDMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		requestID := c.GetHeader("X-Request-ID")
		if requestID == "" {
			requestID = fmt.Sprintf("%d", time.Now().UnixNano())
		}
		c.Set("request_id", requestID)
		c.Header("X-Request-ID", requestID)
		c.Next()
	}
}
