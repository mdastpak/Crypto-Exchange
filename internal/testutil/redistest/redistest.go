// Package redistest gives each test package that uses Redis a database of
// its own. The tests flush their database, so two packages that shared one
// would break each other's tests when go test runs packages in parallel, as
// it does by default.
//
// Add a package at the end of the list; iota keeps the numbers distinct.
// Database 0 is left to development servers, and the default Redis
// configuration has databases 0 to 15.
package redistest

// Addr is the Redis server of the tests, without a password.
const Addr = "localhost:6379"

// The databases of the test packages.
const (
	_ = iota
	// DB is the database of internal/db.
	DB
	// Public is the database of internal/handlers/public.
	Public
	// Middleware is the database of internal/middleware.
	Middleware
	// OTP is the database of internal/otp.
	OTP
	// Server is the database of cmd/server.
	Server
	// Admin is the database of internal/handlers/admin.
	Admin
	// Currencies is the database of internal/currencies.
	Currencies
	// RedisUtil is the database of internal/redisutil.
	RedisUtil
	// Auth is the database of internal/auth.
	Auth
	// Notify is the database of internal/notify.
	Notify
)
