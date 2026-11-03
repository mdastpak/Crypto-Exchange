package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"encrypted-db/config"
	"encrypted-db/docs"
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

	if os.Getenv("GIN_MODE") == "release" {
		gin.SetMode(gin.ReleaseMode)
	}

	if err := initAuthKeys(); err != nil {
		log.Fatalf("Failed to initialize auth keys: %v", err)
	}

	if err := otp.SettingsFromConfig().Validate(gin.Mode() == gin.ReleaseMode); err != nil {
		log.Fatalf("Invalid OTP configuration: %v", err)
	}
	if config.Config.OTP.Auth.Secret == "" {
		log.Println("WARNING: otp.auth.secret is not set: using a random secret, so sign-in codes can only be verified by this process until it restarts")
	}

	otpSender, err := notify.NewOTPSender(config.Config.OTP.Auth.DevLogCodes, gin.Mode() == gin.ReleaseMode)
	if err != nil {
		log.Fatalf("Invalid OTP delivery configuration: %v", err)
	}

	services, cleanup, err := initializeServices()
	if err != nil {
		log.Fatalf("Failed to initialize services: %v", err)
	}
	defer cleanup()

	handlers := initializeHandlers(services)
	handlers.Public.OTPSender = otpSender

	if err := initializeCache(handlers); err != nil {
		log.Fatalf("Failed to initialize cache: %v", err)
	}

	serverAddr := fmt.Sprintf("%s:%s", config.Config.Server.IP, config.Config.Server.Port)

	r, err := newEngine()
	if err != nil {
		log.Fatalf("Failed to create the HTTP engine: %v", err)
	}
	r.Use(gin.Recovery())
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

	if err := handlers.Socket.Shutdown(shutdownCtx); err != nil {
		log.Printf("Error during WebSocket shutdown: %v", err)
	}

	if err := handlers.Admin.CleanupBaseDefinitions(shutdownCtx); err != nil {
		log.Printf("Error during cache cleanup: %v", err)
	}

	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Fatalf("Server forced to shutdown: %v", err)
	}
	log.Println("Server stopped gracefully")
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

func initAuthKeys() error {
	privateKeyPath := filepath.Join(config.Config.JWT.SSL.User.PrivateKey...)
	publicKeyPath := filepath.Join(config.Config.JWT.SSL.User.PublicKey...)

	privateKeyPEM, err := os.ReadFile(privateKeyPath)
	if err != nil {
		return fmt.Errorf("failed to read private key: %w", err)
	}

	publicKeyPEM, err := os.ReadFile(publicKeyPath)
	if err != nil {
		return fmt.Errorf("failed to read public key: %w", err)
	}

	return auth.InitKeys(privateKeyPEM, publicKeyPEM)
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

	publicGroup := r.Group("/public")
	publicGroup.Use(rl.AuthRateLimit("login"))
	{
		publicGroup.GET("/currencies", ih.Public.GetActiveCurrencies)
		publicGroup.GET("/currencies/:hk", ih.Public.GetCurrencyByHK)
		publicGroup.POST("/auth", ih.Public.RequestOTP)
		publicGroup.POST("/auth/:uuid", ih.Public.VerifyOTP)
		publicGroup.POST("/auth/refresh", ih.Public.RefreshToken)
		publicGroup.POST("/auth/logout", func(c *gin.Context) {
			auth.LogoutHandler(c, ih.TokenBlacklist, ih.Sessions)
		})
	}

	adminGroup := r.Group("/admin")
	adminGroup.Use(auth.JWTAdminVerification(ih.TokenBlacklist))
	adminGroup.Use(rl.TradingRateLimit())
	{
		adminGroup.POST("/currencies", ih.Admin.CreateCurrency)
		adminGroup.PUT("/currencies/:hk", ih.Admin.UpdateCurrency)
		adminGroup.DELETE("/currencies/:hk", ih.Admin.DeleteCurrency)
	}

	userGroup := r.Group("/user")
	userGroup.Use(auth.JWTUserVerification(ih.TokenBlacklist))
	userGroup.Use(rl.TradingRateLimit())
	{
		userGroup.GET("/profile", ih.User.GetActiveCurrencies)
		userGroup.POST("/update", ih.User.UpdateProfile)
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
