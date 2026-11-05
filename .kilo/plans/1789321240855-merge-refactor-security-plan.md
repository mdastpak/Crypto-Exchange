# Plan: Consolidate `main` and Harden the Exchange Core

## Current State

- Repository: `D:\Projects\dpm\crypto-exchange`
- Current worktree branch: `feat/exchange-core`
- Current HEAD: `33e80e0` (`feat: security hardening - TLS, JWT blacklist, validation, error sanitization, key rotation`)
- `main` and `origin/main`: `68ccdbe` (`refactor: comprehensive audit fixes - auth, websocket, rabbitmq, db, handlers, config`)
- `feat/exchange-core` is two commits ahead of `main` with no divergence:
  - `3458984`: consolidate branches, add domain models, migrations, configuration, and security work
  - `33e80e0`: security hardening and key rotation
- Original branches are no longer present as independent refs:
  - `feat/database-pooling` was merged into `main` at `afbdfd3` and deleted.
  - `develop` and `feat/refactore-v2` have been deleted.
  - Remote refs currently include `origin/main` and `origin/feat/exchange-core`.
- The current worktree contains 14 modified files and 9 untracked test files. These changes add domain validation/string methods, test support, and integration tests; they are not yet committed.
- Linked worktrees exist at `.kilo/worktrees/crawling-baseball`, `.kilo/worktrees/platinum-sailboat`, and `.kilo/worktrees/test-senarioes`. `platinum-sailboat` has overlapping uncommitted changes and additional untracked planning files.
- The previous plan at `.kilo/plans/1789321240855-merge-refactor-security-plan.md` is stale: it references deleted branches, an old scaffold-only `main`, and a branch layout that no longer matches the repository.

## Goal

Produce one canonical `main` branch, preserve the current exchange-core work, then complete the requested code-quality, security, and feature-optimization work. The plan stops before source changes; an implementation-capable agent must execute it.

## Decisions and Assumptions

1. **Canonical tree:** Use the current `feat/exchange-core` tree plus reviewed uncommitted changes as the source of truth. Do not reset to the old `develop` state.
2. **Consolidation method:** After the current worktree is preserved and tests pass, fast-forward `main` to `33e80e0` with `git merge --ff-only feat/exchange-core`. Use a merge commit only if preserving an explicit consolidation commit is required.
3. **Working tree ownership:** Treat the main worktree as canonical. Do not edit the same files concurrently from `platinum-sailboat` or another linked worktree.
4. **Entrypoint:** Keep `cmd/server/main.go` as the application entrypoint. Do not reintroduce the deleted top-level pooling entrypoint unless a benchmark justifies it.
5. **Database layer:** Retain the current `database/sql` + pgx driver pool initially. It already exposes configurable pool limits. Revisit `pgxpool` only after profiling; avoid a second structural migration during consolidation.
6. **Secrets:** Runtime values and key material must come from environment variables or mounted secret files. Remove `config/config.yaml` from the index, keep only the example, and remove local PEM files from the repository working tree after rotating the keys.
7. **Refresh-token contract:** Prefer an HttpOnly, Secure, SameSite cookie for the refresh token and return only the access token from refresh responses. If the API must return a refresh token for non-browser clients, define that explicitly and add rotation/replay protection.
8. **Feature scope:** Complete and test the existing exchange domain models and infrastructure before adding new trading behavior. The current domain packages are models, not a complete price, matching, custody, or settlement implementation.

## Phase 0 — Preserve Work and Complete Branch Consolidation

1. **Freeze concurrent work**
   - Stop edits in linked worktrees.
   - Record `git status --short --branch` and `git diff --check`.
   - Preserve the 14 modified files and 9 untracked tests in one canonical worktree using a backup branch or stash; do not discard them.
   - Remove or archive the overlapping untracked `FIX_PLAN.md` and `TEST_SCENARIOS.md` only after their useful content is incorporated into this plan.

2. **Run the baseline checks on the preserved tree**
   - `go build ./...`
   - `go test ./...`
   - `go vet ./...`
   - `golangci-lint run`
   - `gofmt -l .`
   - `git diff --check`
   - Record failures before changing architecture.

3. **Resolve blockers before promotion**
   - Fix the integration-test RSA fixture and assert `auth.InitKeys` succeeds.
   - Resolve the refresh-token cookie/header contract and expected response body.
   - Return an empty JSON array instead of `null` when no currencies exist.
   - Fix any compile errors in the uncommitted domain/test changes.

4. **Promote the canonical tree**
   - `git switch main`
   - Verify `main` has no unique uncommitted work.
   - `git merge --ff-only feat/exchange-core`
   - Verify `git log -1 --oneline main` is `33e80e0` and `git status --short --branch` is clean.
   - Push `main` to `origin/main` only after explicit approval.
   - Delete redundant local/remote branches only after confirming they contain no unique commits and after approval.

## Phase 1 — Correctness and Test Foundation

1. **Fix authentication integration tests**
   - Generate a real RSA key pair at test setup; never use placeholder PEM text.
   - Assert key initialization errors.
   - Test access-token verification, refresh-token verification, blacklist behavior, issuer validation, and key rotation.
   - Use an isolated Redis/PostgreSQL test environment; remove the shared-DB `DROP TABLE` behavior from `NewTestPostgresService` or restrict it to a disposable test database.

2. **Define and test the refresh flow**
   - Decide cookie-only versus response-body refresh delivery.
   - Add refresh-token rotation, one-time use, revocation, and replay tests.
   - Ensure logout revokes the active access token and the refresh token/session according to the chosen contract.
   - Set explicit `Secure`, `HttpOnly`, `SameSite`, `Path`, and `MaxAge` cookie attributes.

3. **Fix OTP correctness and security**
   - Replace the time-derived OTP generator in `internal/handlers/public/auth.go:263-270` with `crypto/rand`.
   - Stop logging OTP values or raw contact information at `internal/handlers/public/auth.go:109`.
   - Do not place the OTP in the Redis key; use an opaque random key and store an encrypted payload.
   - Make retry increments atomic and apply a bounded lockout TTL.
   - Add tests for invalid length, expiry, replay, rate limiting, and malformed Redis data.

4. **Repair migrations and startup behavior**
   - Correct `internal/db/migrations/2_create_users_table.up.sql:22-23` so the username/contact unique index targets `users`, not `currencies`.
   - Align the user JSONB constraint with the actual `contact` document used by `checkOrCreateUser` at `internal/handlers/public/auth.go:192-240`.
   - Verify all migrations from an empty PostgreSQL database and verify downgrade behavior where supported.
   - Add the missing `//go:embed config.example.yaml` directive for `configEmbed` in `config/config.go:14-19`, or remove the unused embed fallback.
   - Require and validate JWT key paths before starting the server; `cmd/server/main.go:64-66` must fail clearly instead of reading an empty path.

5. **Fix route and lifecycle defects**
   - Replace `/user/profile` returning currencies with a real profile handler, or rename the route to match its behavior.
   - Remove or implement `/user/update`; a `nil` Gin handler at `cmd/server/main.go:268` is a runtime panic risk.
   - Start and stop the RabbitMQ WebSocket consumer from the server lifecycle; `StartConsumer` at `internal/handlers/socket/handler.go:259-265` is currently unused.
   - Correct the WebSocket `WaitGroup` ownership: `readPump` and `writePump` must not both call `Done` for one `Add`, and `Shutdown` must be idempotent.
   - Add tests for server startup, shutdown, health checks, and dependency failures.

## Phase 2 — Security Hardening

1. **Secrets and key management**
   - Remove `config/config.yaml` from version control while retaining `config/config.example.yaml` and `.env.example`.
   - Move local `internal/ssl/**/*.pem` files to a mounted secret location or secret manager; rotate the exposed development keys.
   - Validate key file permissions and fail closed when required secrets are absent in production.
   - Implement explicit key IDs, issuer/audience validation, rotation, and retirement in `internal/auth/keys.go:23-142`.

2. **Authentication and authorization**
   - Make admin and user middleware use the rotation-aware verifier in `internal/auth/keys.go:115-142`, not the single-key verifier.
   - Use role constants instead of string literals and reject unknown roles.
   - Check token blacklist on every protected endpoint, including refresh and logout.
   - Add authorization tests for missing, malformed, expired, wrong-role, blacklisted, and rotated tokens.
   - Add session/account lockout and audit events for authentication mutations.

3. **Transport and cross-origin controls**
   - Require TLS in production and validate certificate paths before listening.
   - Change the PostgreSQL default from `sslmode=disable` to a secure mode appropriate for the deployment.
   - Require authenticated RabbitMQ credentials and support AMQPS where required.
   - Make CORS fail closed when `allowed_origins` is empty; never reflect an arbitrary origin with credentials.
   - Keep WebSocket origin checks exact and add authentication/authorization before subscribing to protected channels.

4. **Input, error, and logging controls**
   - Wire `ValidationMiddleware` and `RecoveryMiddleware` into the real Gin router; add body-size limits and structured request validation.
   - Never return stack traces or recovered values to clients, including debug deployments exposed outside localhost.
   - Sanitize request IDs and reject oversized or control-character values.
   - Replace raw `log` usage in security-sensitive paths with structured, redacted logging.
   - Audit admin currency mutations and authentication events with request ID, actor, action, outcome, and timestamp.

5. **Dependency hygiene**
   - Remove duplicate/unused Redis, RabbitMQ, OpenTelemetry, and Docker dependencies where possible.
   - Pin and review indirect dependencies after `go mod tidy`.
   - Run vulnerability scanning as part of validation.

## Phase 3 — Refactoring and Architecture

1. **Separate composition from business logic**
   - Replace concrete `*db.PostgresService`, `*db.RedisService`, and `*rabbitmq.RabbitMQService` fields in handlers with small interfaces.
   - Move service construction and cleanup into a dedicated composition root while keeping `cmd/server/main.go` readable.
   - Inject context, logger, clock, and random source where tests need deterministic behavior.

2. **Simplify handlers**
   - Split `internal/handlers/public/auth.go`, admin currency handlers, and socket handling into focused files by responsibility.
   - Remove duplicated currency-list logic between public and user handlers.
   - Return typed errors and map them to stable API responses instead of scattering `SendResponse` calls.
   - Add request-scoped tracing/audit metadata without putting sensitive values in logs.

3. **Make configuration typed and strict**
   - Replace the global mutable `config.Config` where practical with an immutable `Configuration` passed through the composition root.
   - Validate ranges, required endpoints, credentials, TLS settings, origins, token lifetimes, and provider configuration at startup.
   - Support explicit config and migration flags and make paths independent of the process working directory.
   - Keep environment overlays explicit and test them.

4. **Clarify domain boundaries**
   - Keep financial values in `shared.Decimal`; reject panicking constructors in request paths.
   - Add invariants and table-driven tests for asset, balance, order, trade, market, account, KYC, and custody models.
   - Remove the unused GraphQL/gqlgen artifacts or document a future implementation; do not leave a misleading stub.
   - Decide whether `internal/models` is a legacy API layer or remove it in favor of domain types and explicit DTOs.

## Phase 4 — Feature Optimization

1. **Currency cache**
   - Give cached currency entries a configured TTL and invalidate them on create/update/delete.
   - Replace unbounded Redis `SCAN` list reads with an indexed key set or a bounded database-backed listing path.
   - Add cache-miss fallback, stale-data behavior, and invalidation tests.

2. **RabbitMQ**
   - Keep publisher confirms, but make reconnect/channel recreation race-safe and test connection loss.
   - Use unique durable consumer queues per consumer instance or a single coordinated consumer, depending on broadcast semantics.
   - Add dead-letter/retry policy for failed publications and validate manual acknowledgements.

3. **WebSocket**
   - Use per-client write deadlines, ping/pong, bounded message sizes, and backpressure.
   - Authenticate clients before protected subscriptions and validate channel names.
   - Ensure broadcasts do not block on slow clients and shutdown closes each connection exactly once.

4. **Database and shutdown**
   - Benchmark the current pool settings under representative load before changing drivers.
   - Add connection acquisition timeouts and dependency-aware health checks.
   - Make shutdown order explicit: stop accepting traffic, stop consumers, close WebSockets, flush/close RabbitMQ, close Redis, then close PostgreSQL.

5. **Observability and UX**
   - Implement the configured structured logging, metrics, and tracing hooks instead of leaving them as unused configuration.
   - Return consistent empty arrays, stable error codes, pagination/filtering for list endpoints, and useful profile data.
   - Add API documentation for the actual routes and authentication contract.

## Validation and Acceptance Criteria

- Branch consolidation:
  - `main` points to the reviewed canonical commit.
  - No original branch contains unique commits.
  - `git status --short --branch` is clean before push.
- Quality gates:
  - `go build ./...` passes.
  - `go vet ./...` passes.
  - `go test ./...` and `go test -race ./...` pass.
  - `golangci-lint run` passes with the repository configuration.
  - `gofmt -l .` and `git diff --check` are empty.
- Integration:
  - Clean PostgreSQL migration succeeds and produces the expected users/currencies schema.
  - Redis and RabbitMQ integration tests pass in isolated containers.
  - OTP, refresh, logout, blacklist, CORS, WebSocket origin, and key-rotation tests pass.
  - Server startup, health, and graceful shutdown are exercised end to end.
- Security:
  - No tracked PEM/private-key material or runtime secret values remain.
  - Production defaults fail closed for missing credentials, TLS, and allowed origins.
  - No client response contains stack traces, secrets, raw OTPs, or raw contact data.
- Performance:
  - Cache operations have bounded latency and TTL behavior.
  - RabbitMQ and WebSocket reconnect/backpressure behavior is demonstrated under a controlled failure test.
  - Any database-pool change is supported by a before/after benchmark.

## Risks and Approval Gates

- **Uncommitted work:** The current 14 modified files and 9 tests must be preserved before switching branches. Do not use `reset --hard`, `clean -fd`, or force-push without explicit approval.
- **Canonical branch:** `main` is currently behind `feat/exchange-core`; the stale plan's `reset --hard develop` instruction must not be used.
- **Secret rotation:** Removing local PEMs is safe only after generating and deploying replacement keys through the chosen secret mechanism.
- **Refresh-token behavior:** The implementation must not silently change browser and API clients to incompatible contracts.
- **Feature completeness:** Domain models and configuration do not yet constitute a trading engine; do not claim price aggregation, matching, custody, or settlement functionality until their services and tests exist.

## Relevant Files

- `cmd/server/main.go:54-145` — startup, TLS, cleanup, and shutdown.
- `cmd/server/main.go:222-273` — middleware, routes, and nil/incomplete handlers.
- `config/config.go:14-19` — embedded configuration fallback.
- `config/config.go:308-332` — config loading and validation.
- `config/config.go:450-620` — environment overlays, secret warnings, and RabbitMQ URL construction.
- `internal/auth/auth.go:87-165` — JWT verification and role checks.
- `internal/auth/keys.go:23-142` — RSA key loading and rotation.
- `internal/auth/middleware.go:57-139` — admin/user authorization.
- `internal/auth/blacklist.go:29-54` — token revocation.
- `internal/handlers/public/auth.go:62-190` — OTP and token issuance.
- `internal/handlers/public/auth.go:192-252` — user persistence and OTP retry logic.
- `internal/handlers/public/auth.go:263-270` — OTP generation.
- `internal/handlers/public/auth.go:317-348` — currency cache listing.
- `internal/handlers/admin/currencies.go:45-69` — event publication.
- `internal/handlers/admin/currencies.go:287-342` — cache invalidation/loading.
- `internal/handlers/socket/handler.go:115-191` — WebSocket pump lifecycle.
- `internal/handlers/socket/handler.go:259-393` — consumer and shutdown.
- `internal/rabbitmq/rabbitmq.go:46-288` — connection, confirms, retries, and reconnect.
- `internal/db/postgres.go:22-56` — database pool and migrations.
- `internal/db/postgres.go:83-127` — test database helper.
- `internal/db/migrations/2_create_users_table.up.sql:1-24` — user schema defect.
- `internal/db/migrations/5_create_orders_table.up.sql:9-18` — order/user foreign key.
- `internal/domain/{asset,balance,order,trade,account,kyc,custody,market,shared}/*.go` — exchange domain model layer.
- `.gitignore:23-30` — intended config and PEM exclusions.
