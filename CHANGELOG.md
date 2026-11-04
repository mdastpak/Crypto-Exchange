# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/). No versions have been tagged yet, so entries are grouped by the date the changes reached `main`. Semantic versioning will start with the first tagged release.

## [Unreleased]

### Added

- Project documentation: `README.md`, `SECURITY.md`, `CONTRIBUTING.md`, `CODE_OF_CONDUCT.md`, `docs/architecture.md`, `docs/configuration.md`, and `docs/api.md`.
- Issue templates and a pull request template.
- Test-scenario catalog with stable scenario IDs and a finding-status table, in [`docs/testing/test-scenarios.md`](docs/testing/test-scenarios.md) ([#90](https://github.com/mdastpak/Crypto-Exchange/pull/90)).
- Sign-in codes are delivered by email (SMTP) and by SMS (Kavenegar verify lookup). Codes are queued on the `notifications` exchange and delivered by a worker that retries temporary failures with exponential backoff until `notifications.worker.max_attempts` or the code expiry, and records codes it cannot deliver on the `notifications.otp.dead-letter` queue without the code or the raw contact. Channels are configured in the new `notifications` section, with the `SMTP_PASSWORD` and `KAVENEGAR_API_KEY` environment variables for secrets ([#14](https://github.com/mdastpak/Crypto-Exchange/issues/14)).
- Separate signing keys for admin tokens (`jwt.ssl.admin`), which were configured but never loaded, and `retired_public_keys` for each role, so that signing keys can be rotated without signing everyone out. Tokens name their key in the `kid` header ([#17](https://github.com/mdastpak/Crypto-Exchange/issues/17)).
- Admin sign-in with two factors: a one-time code sent by email, and a code from an authenticator app (TOTP). Operators create administrators with the new `cmd/admin` command (`create`, `reset-totp`, `disable`), which prints the authenticator enrollment once. Authenticator secrets are encrypted with the new `admin.totp_key` (`ADMIN_TOTP_KEY`), and each authenticator code is accepted once. Migration 12 adds `users.role` and `users.totp_last_step`. New routes: `POST /admin/auth`, `POST /admin/auth/{uuid}`, and `POST /admin/logout` ([#18](https://github.com/mdastpak/Crypto-Exchange/issues/18)).
- A CI workflow builds the Docker image on every pull request and checks that the Dockerfile uses the Go version of `go.mod`, that the image holds only the binaries and the migrations, and that it runs as a non-root user ([#28](https://github.com/mdastpak/Crypto-Exchange/issues/28)).

### Changed

- Moved the technical audit notes of 2026-09-14 from this file to [`docs/audits/2026-09-14-technical-audit.md`](docs/audits/2026-09-14-technical-audit.md).
- `config/config.local.yaml` is now ignored by Git.
- **Breaking:** HTTP CORS is configured with the new `server.cors.allowed_origins` setting. `websocket.allowed_origins` now only applies to WebSocket connections ([#12](https://github.com/mdastpak/Crypto-Exchange/issues/12)).
- **Breaking:** OTP verification no longer returns `refresh_token` in the response body. The refresh token is only delivered in the `refresh_token` cookie, which is now `SameSite=Strict` and scoped to `/public/auth`. Tokens issued before this change have no token type and are rejected, so users must sign in again ([#10](https://github.com/mdastpak/Crypto-Exchange/issues/10)).
- **Breaking:** OTP sign-in accepts only email addresses and mobile numbers. Usernames are rejected with `400`, and sign-in with a contact type whose channel is not configured is refused with `400` instead of reporting a code that is never sent. Existing accounts whose only contact is a username can no longer sign in ([#14](https://github.com/mdastpak/Crypto-Exchange/issues/14)).
- **Breaking:** release mode requires the new `otp.auth.secret` setting (or `OTP_SECRET`) of at least 32 bytes, and the server refuses to start with OTP settings out of range. `otp.auth.length`, `ttl_seconds`, and `retry_limit` now default to 6, 60, and 3. Sign-in codes issued before the upgrade can no longer be verified, so users must request a new one ([#15](https://github.com/mdastpak/Crypto-Exchange/issues/15)).
- **Breaking:** protected routes, including `POST /public/auth/logout`, now require `Authorization: Bearer <token>`. A bare token gets `401`. User routes accept only user tokens and admin routes only admin tokens, so admin tokens no longer reach `/user/*`, and a user token on an admin route gets `401` instead of `403`. Tokens issued before the upgrade have no `kid` and no audience and are rejected, so users must sign in again. `jwt.issuer` defaults to `encrypted-db-exchange` and is required on every token ([#17](https://github.com/mdastpak/Crypto-Exchange/issues/17)).
- Admin tokens are only issued to users whose stored role is `admin`, and every admin request checks that the administrator is still active, so demoting or disabling an administrator revokes its tokens at once. Administrators cannot use the user sign-in (`403`) ([#18](https://github.com/mdastpak/Crypto-Exchange/issues/18)).

### Fixed

- First-time OTP sign-in failed with `500 Failed to create user`, because the `users.info` constraint from migration 2 required `username` and `password` keys that OTP sign-in never writes. Migration 11 requires a contact object in `users.info`, makes each contact value unique (email addresses and usernames case-insensitively), and stops with an actionable message if manually inserted rows conflict with these rules. Concurrent first sign-ins for the same contact now resolve to one user ([#8](https://github.com/mdastpak/Crypto-Exchange/issues/8)).
- RabbitMQ publishing stalled after about three messages, and graceful shutdown could hang, because every `Publish` call registered a new publisher-confirm listener that was never removed, and the library delivers each confirmation to every listener with a blocking send. Each channel now has a single listener whose confirmations are routed to the waiting publish by delivery tag, so concurrent publishes are pipelined and each one waits only for its own confirmation. A channel closed by the broker is replaced on the next publish ([#9](https://github.com/mdastpak/Crypto-Exchange/issues/9)).
- Signing in to a disabled, deleted, or suspended account returned `500 Failed to create user`. It now returns `403` with a stable message ([#15](https://github.com/mdastpak/Crypto-Exchange/issues/15)).
- The Docker image could not be built, because the Dockerfile used Go 1.22 while `go.mod` requires Go 1.26, and the `app` service of `docker-compose.yml` could not reach its dependencies or read its keys. The image now builds both `server` and `admin` and includes the migrations. The `app` service uses `config/config.compose.yaml`, receives the user keys as Compose secrets, and starts with every dependency healthy. The Redis health check uses `PING` instead of `INCR`, RabbitMQ receives its Erlang cookie on the command line, which avoids the `.erlang.cookie` `eacces` failure on some Docker Desktop and WSL 2 setups, and the obsolete `version` key is gone ([#28](https://github.com/mdastpak/Crypto-Exchange/issues/28)).
- WebSocket clients never received currency events, because the server never started the RabbitMQ consumer. The consumer now starts with the server and stops before RabbitMQ closes. It uses a channel of its own with a prefetch limit, and an exclusive, auto-delete queue per server instance instead of the shared durable `ws_price_updates` queue, so clients on every instance receive every event. It subscribes before startup completes and subscribes again with backoff when its channel closes ([#19](https://github.com/mdastpak/Crypto-Exchange/issues/19)).
- When several messages were queued for a WebSocket client, they were written into one frame back to back, which is not valid JSON. Each message is now a frame of its own ([#20](https://github.com/mdastpak/Crypto-Exchange/issues/20)).
- `RabbitMQService` raced on its connection and channel during reconnects: they were guarded by two different mutexes, the close notification was replaced while being read, the replaced connection was never closed, and a channel closed by the broker was reported as healthy and never replaced. One supervisor goroutine now owns reconnection, watches both the connection and the publishing channel, closes replaced connections, and restores the channel after a channel-level error without waiting for a publish. The health check reports RabbitMQ as degraded while either is unavailable, and publishes wait up to five seconds for the channel instead of failing at once ([#21](https://github.com/mdastpak/Crypto-Exchange/issues/21)).
- Creating or updating a currency with a duplicate symbol or contract, or with invalid data, returned `500`: the handlers matched `lib/pq` errors, while the driver is pgx, and they did not validate requests, so a missing `status` failed in the database. Database errors are now matched by SQLSTATE code: a duplicate returns `409` with a stable code (`duplicate_symbol` or `duplicate_contract`) and the field. Requests are validated with field-level errors, `status` defaults to `pending`, symbols are stored in upper case, and server-controlled or unknown fields are rejected ([#22](https://github.com/mdastpak/Crypto-Exchange/issues/22), [#23](https://github.com/mdastpak/Crypto-Exchange/issues/23)).
- Currency updates could not set `is_token` or `if_fiat` back to `false`; only the fields sent are changed now (part of [#35](https://github.com/mdastpak/Crypto-Exchange/issues/35)). The stored row is locked while an update is applied, so concurrent partial updates of one currency no longer overwrite each other.
- Currencies that were not approved were public: create and update cached every currency, whatever its status, and public and user routes returned everything in the cache, so a `pending` or `suspend` currency was visible until the next restart, and every change was announced to WebSocket clients. Only approved currencies are cached now, and a currency that is suspended, set back to pending, or deleted leaves the cache at once. Public and user routes also skip any cached currency that is not approved. WebSocket events follow what public clients can see: a currency that becomes approved is announced as `create`, one that stops being approved as `delete`, and changes to currencies that are not approved are not announced ([#24](https://github.com/mdastpak/Crypto-Exchange/issues/24)).
- Only one currency could have an empty contract, because the unique index on `contract` covered empty strings. Migration 13 makes only non-empty contracts unique, so native coins and fiat currencies can coexist ([#22](https://github.com/mdastpak/Crypto-Exchange/issues/22)).

### Security

- Refresh tokens were accepted as access tokens, giving seven days of API access, and were never revoked, not even by logout. Tokens now carry a type (`typ`), so protected routes accept only access tokens and the refresh endpoint accepts only refresh tokens. Each sign-in creates a session in the `sessions` table. Refresh tokens are rotated on every use, and presenting an already rotated refresh token revokes the session. Logout revokes the session and clears the cookie ([#10](https://github.com/mdastpak/Crypto-Exchange/issues/10)).
- Every IP-based rate limit, including the OTP sign-in limit, could be bypassed by sending a different `X-Forwarded-For` value on each request, because the limiter read that header directly and Gin trusted forwarding headers from every peer. The limiter now uses the client address that Gin derives from the connection, and forwarding headers are trusted only from the proxies listed in the new `server.trusted_proxies` setting (none by default) ([#11](https://github.com/mdastpak/Crypto-Exchange/issues/11)).
- With no allowed origins configured (the default), the CORS middleware reflected every `Origin` together with `Access-Control-Allow-Credentials: true`, so any website could make credentialed requests and read the responses. A `"*"` entry had the same effect. The middleware now fails closed: only listed origins are allowed, `"*"` and malformed origins stop the server at startup, and responses carry `Vary: Origin` ([#12](https://github.com/mdastpak/Crypto-Exchange/issues/12)).
- Every OTP request wrote the sign-in code together with the raw email address, phone number or username to the application log, so anyone with log access could sign in as any user. Codes are now handed to an `OTPSender`, which discards them by default. The new `otp.auth.dev_log_codes` setting writes them to the log with a masked contact for local development only, and the server refuses to start with it in release mode ([#13](https://github.com/mdastpak/Crypto-Exchange/issues/13)).
- Anyone could create or take over a username-only account, because a username cannot receive a code, so the code proved nothing about who was signing in. Username sign-in is removed ([#14](https://github.com/mdastpak/Crypto-Exchange/issues/14)).
- OTP verification allowed more guesses than `otp.auth.retry_limit`: the attempt counter was read and written back without atomicity, so parallel requests all saw the same count, and every new code came with a fresh session and a fresh budget. Each attempt is now reserved atomically before the code is checked, both for the code and for its contact, and the new `otp.auth.contact_requests_per_hour` and `otp.auth.contact_attempts_per_hour` settings bound codes and attempts per contact across sessions. A code also signs in only once when it is sent in parallel. Redis keys contained the code itself; sessions now store an HMAC of the code and the contact encrypted with AES-GCM, both keyed by the new `otp.auth.secret`. Code digits were slightly biased towards 0 to 5; they are now uniform ([#15](https://github.com/mdastpak/Crypto-Exchange/issues/15)).
- The contact stored during OTP sign-in was encrypted with a key derived from the session UUID, which the client receives, so the encryption protected nothing, and its 100,000 PBKDF2 iterations ran twice per sign-in on unauthenticated endpoints, which made them a cheap way to exhaust the CPU. The contact is now encrypted with a key derived from `otp.auth.secret`, without a per-request KDF, and a test keeps password-hashing KDFs out of the code behind unauthenticated requests. A panic returned its value and stack trace outside release mode when `RecoveryMiddleware` was used; it now returns only `500 Internal server error`, in every mode, and the router uses it [#16](https://github.com/mdastpak/Crypto-Exchange/issues/16).
- Token verification accepted any RSA algorithm and did not check the issuer, the audience, or the presence of an expiry, and admin and user tokens were signed with the same key, so anyone holding the user key could mint admin tokens. Tokens are now verified by a single `ParseToken` that requires RS256, the configured issuer, the audience of the route's role, an expiry, the expected token type and role, and a `kid` that names a key of that role. The key state was written outside its mutex; it is now replaced atomically ([#17](https://github.com/mdastpak/Crypto-Exchange/issues/17)).
- The repository had no `.dockerignore`, so `.env` files, private keys, and local configuration were copied into the image build, and the final image copied the whole `config/` directory, including any local configuration with secrets. A `.dockerignore` now keeps them out of the build context, and the image holds no configuration. It runs as an unprivileged user (UID 10001) and installs no packages: the time zone database is embedded in the binaries ([#28](https://github.com/mdastpak/Crypto-Exchange/issues/28)).

## 2026-09-16

### Added

- Exchange domain model: assets, markets, orders, trades, balances, accounts, KYC/AML, and custody, with database migrations 3 to 10 ([#2](https://github.com/mdastpak/Crypto-Exchange/pull/2)).
- Extended configuration for exchange, markets, price aggregation, custody, fiat, compliance, observability, and rate limits, plus `configs/assets.yaml` and `configs/aml_rules.yaml` ([#2](https://github.com/mdastpak/Crypto-Exchange/pull/2)).
- Redis-backed rate limiting, security headers, CORS, token blacklist, public key registry for key rotation, and optional TLS ([#2](https://github.com/mdastpak/Crypto-Exchange/pull/2)).
- Startup connection retries for PostgreSQL and Redis (`connect_retries`, `connect_retry_delay_ms`) and a PostgreSQL advisory lock around migrations ([#4](https://github.com/mdastpak/Crypto-Exchange/pull/4)).
- RabbitMQ status in the health check ([#4](https://github.com/mdastpak/Crypto-Exchange/pull/4)).
- Tests for configuration, database, migrations (full up, down, up cycle), RabbitMQ, and system handlers ([#4](https://github.com/mdastpak/Crypto-Exchange/pull/4)); authentication edge cases ([#1](https://github.com/mdastpak/Crypto-Exchange/pull/1)); and admin, WebSocket, user handlers, helpers, and utilities ([#5](https://github.com/mdastpak/Crypto-Exchange/pull/5)).

### Fixed

- Migrations 5 to 10 failed on an empty database because of forward references, `users(id)` type mismatches, a colliding enum name, and missing types and columns ([#4](https://github.com/mdastpak/Crypto-Exchange/pull/4)).
- The unique username index of migration 2 was created on the wrong table ([#4](https://github.com/mdastpak/Crypto-Exchange/pull/4)).
- The embedded fallback configuration lacked its `//go:embed` directive ([#4](https://github.com/mdastpak/Crypto-Exchange/pull/4)).
- `/user/update` was registered with a nil handler and panicked. It now returns `501 Not Implemented` ([#4](https://github.com/mdastpak/Crypto-Exchange/pull/4)).
- Currency lists returned `null` instead of an empty array ([#1](https://github.com/mdastpak/Crypto-Exchange/pull/1), [#4](https://github.com/mdastpak/Crypto-Exchange/pull/4)).
- WebSocket disconnects panicked because of a negative `WaitGroup` counter and a double close of the send channel ([#5](https://github.com/mdastpak/Crypto-Exchange/pull/5)).

### Security

- OTP codes are generated with `crypto/rand` instead of the system clock ([#1](https://github.com/mdastpak/Crypto-Exchange/pull/1)).
- Logout accepts the `Bearer` authorization scheme, so the intended token is revoked ([#1](https://github.com/mdastpak/Crypto-Exchange/pull/1)).

## 2026-09-13 to 2026-09-14

### Changed

- Comprehensive refactor of authentication, WebSocket, RabbitMQ, database, handlers, and configuration (`68ccdbe`). This covered the migration from `dgrijalva/jwt-go` to `golang-jwt/jwt/v5`, loading RSA keys once at startup, hashing blacklisted tokens, WebSocket origin checks with heartbeats and backpressure, RabbitMQ publisher confirms and reconnects, the pgx driver, context propagation, server timeouts, and graceful shutdown. See the [audit notes](docs/audits/2026-09-14-technical-audit.md) for details.
- Configurable database pool settings and migrations path (`1624352`). Configuration flags, embedded configuration, and linter configuration (`d24514d`).
- Database connection pooling (`afbdfd3`).

### Security

- Removed committed secrets, and added environment variable overrides and configuration flags (`01c7ce6`).

## 2024-10-28 to 2024-11-11

### Added

- Initial HTTP server (`c045184`), Swagger documentation, and currency endpoints (`516e732`).
- Currency CRUD with database migrations (`50d3e99`).
- RabbitMQ integration and the WebSocket broadcast of currency changes (`40c8039`, `f346605`).
- JWE-based authentication (`140fb51`), sign-up and sign-in (`d77b992`), and JWT access and refresh tokens (`0df1473`).

[Unreleased]: https://github.com/mdastpak/Crypto-Exchange/compare/85578fc...HEAD
