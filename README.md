# Crypto Exchange

A backend for a cryptocurrency exchange, written in Go. It provides OTP-based sign-in, JWT sessions, administrative currency management with a Redis cache, RabbitMQ events, and a WebSocket gateway. A domain model and database schema for assets, markets, orders, trades, balances, accounts, KYC/AML, and custody are also included and will back the roadmap features.

> [!WARNING]
> **Pre-alpha: not production-ready.** The hardening work is tracked in the [M1 foundation and hardening epic](https://github.com/mdastpak/Crypto-Exchange/issues/84) and in the [open security issues](https://github.com/mdastpak/Crypto-Exchange/issues?q=is%3Aopen+label%3A%22type%3A+security%22). Do not deploy this code with real funds or real personal data.

## Contents

- [Project status](#project-status)
- [Architecture](#architecture)
- [Technology stack](#technology-stack)
- [Getting started](#getting-started)
- [Configuration](#configuration)
- [API overview](#api-overview)
- [Testing](#testing)
- [Project layout](#project-layout)
- [Roadmap](#roadmap)
- [Contributing](#contributing)
- [Security](#security)
- [License](#license)

## Project status

| Capability | Status | Notes |
| --- | --- | --- |
| OTP sign-in (email, mobile) | Implemented | Codes are delivered by email (SMTP) and SMS (Kavenegar) through a RabbitMQ queue with retries. Codes are stored hashed, and attempts are limited per code and per contact. |
| JWT access and refresh tokens, sessions, logout | Implemented | Typed tokens with issuer, audience, and `kid` checks, separate signing keys for users and admins, key rotation, `Bearer` authentication, refresh token rotation with reuse detection and a short grace period for concurrent refreshes, and session revocation on logout. Every user request checks that its session is active and its user can still sign in ([#120](https://github.com/mdastpak/Crypto-Exchange/issues/120)). Users list their sessions and end any of them ([#193](https://github.com/mdastpak/Crypto-Exchange/issues/193)). |
| User contacts | Implemented | A user holds one verified email address and one mobile number and signs in with either. Adding, replacing, or removing one needs a code to a verified contact of the account and to the new contact ([#199](https://github.com/mdastpak/Crypto-Exchange/issues/199)). |
| Admin currency management | Implemented | Create, list with filters and pages, read, update with optimistic versions, delete, and restore. Administrators are created with the `cmd/admin` command and sign in with an emailed code and an authenticator app (TOTP). |
| Public currency listing | Implemented | Served from the Redis cache, and from PostgreSQL while the cache is empty or unavailable ([#52](https://github.com/mdastpak/Crypto-Exchange/issues/52)). |
| Audit log | Implemented | Append-only `audit_events`, written in the transaction of each change, for sign-ins, sessions, currency changes, contact changes, and the `cmd/admin` commands. Administrators query it at `GET /admin/audit`. |
| Transactional outbox | Implemented | Events are written with their change and relayed to RabbitMQ by one instance at a time, with retries and dead letters that `cmd/admin outbox` lists, retries, or discards ([#89](https://github.com/mdastpak/Crypto-Exchange/issues/89), [#192](https://github.com/mdastpak/Crypto-Exchange/issues/192)). |
| Data-protection keys | Implemented | Keys for row seals and field encryption, wrapped by a key-encryption key in Vault or OpenBao Transit, with rotation ([#138](https://github.com/mdastpak/Crypto-Exchange/issues/138)). No table uses them yet. |
| WebSocket gateway | Partial | Ping and pong, token authentication with reauth, connections closed when their session ends, and currency events broadcast to every client. The channel protocol is tracked in [#66](https://github.com/mdastpak/Crypto-Exchange/issues/66). |
| Maintenance and lockdowns | Implemented | Administrators put the whole service under maintenance, or lock single features (sign-up, sign-in, deposits, withdrawals, trading, the WebSocket, or every change), now or on a schedule, through `/admin/platform` or `cmd/admin platform`. Every instance applies a change within a second, and money features fail closed when an instance cannot read the controls ([#235](https://github.com/mdastpak/Crypto-Exchange/issues/235)). |
| Backups and disaster recovery | Partial | The checks, the monthly restore drill, and the runbook are in the repository ([#236](https://github.com/mdastpak/Crypto-Exchange/issues/236)); WAL archiving and base backups are set up per environment with the backup tool, as [docs/disaster-recovery.md](docs/disaster-recovery.md) describes. |
| Global market data catalog | Foundation | `cmd/market-worker` and the `market_` tables of unverified assets, global and Iranian exchanges, pairs, and tickers; the jobs and sources follow in [#262](https://github.com/mdastpak/Crypto-Exchange/issues/262). See [docs/market-data.md](docs/market-data.md). |
| Languages and text direction | Partial | Supported locales with their direction (RTL or LTR), the locale of each request from its query, headers, cookie, or browser, and Persian translations of the messages users see ([#283](https://github.com/mdastpak/Crypto-Exchange/issues/283)). The country of each client from a trusted edge header or a GeoIP database ([#284](https://github.com/mdastpak/Crypto-Exchange/issues/284), [docs/geoip.md](docs/geoip.md)). A site per country with its own domain and locales, redirects that respect the visitor's choice, and the fallback to the global site in English ([#285](https://github.com/mdastpak/Crypto-Exchange/issues/285)). Saved preferences follow in [#286](https://github.com/mdastpak/Crypto-Exchange/issues/286). See [docs/i18n.md](docs/i18n.md). |
| Rate limiting (Redis) | Implemented | Token buckets per client IP or per signed-in user, fixed windows for sign-in, and a configurable policy for Redis failures. |
| Health checks | Implemented | `/system/live` for liveness, and `/system/ready`, which answers `503` while PostgreSQL or Redis is unavailable, and `200` with the status `degraded` while RabbitMQ is ([#33](https://github.com/mdastpak/Crypto-Exchange/issues/33), [#232](https://github.com/mdastpak/Crypto-Exchange/issues/232)). |
| Exchange domain model and schema | Models and migrations only | No services use them yet. |
| Price engine, OTC trading, ledger, custody, KYC/AML services | Planned | [Phase 1 epic](https://github.com/mdastpak/Crypto-Exchange/issues/85) |
| Spot trading, derivatives | Planned | [Phase 2](https://github.com/mdastpak/Crypto-Exchange/issues/86) and [Phase 3](https://github.com/mdastpak/Crypto-Exchange/issues/87) epics |

## Architecture

The service is a modular monolith with a single HTTP and WebSocket entry point.

```mermaid
flowchart LR
    client[Web and mobile clients] -->|REST| api[Gin HTTP API]
    client -->|WebSocket| ws[WebSocket gateway]
    api --> pg[(PostgreSQL)]
    api --> redis[(Redis)]
    api -->|publish events| mq[[RabbitMQ]]
    mq -->|consume events| ws
    ws --> client
```

- **PostgreSQL** stores currencies, users, and the exchange schema. Migrations run automatically at startup.
- **Redis** holds the currency cache, OTP sessions and attempt counters, the JWT blacklist, and the rate-limit buckets.
- **RabbitMQ** carries domain events. Currency changes are published to a fan-out exchange and are intended to be broadcast to WebSocket clients.

See [docs/architecture.md](docs/architecture.md) for package responsibilities, data stores, and the target architecture.

## Technology stack

| Area | Technology |
| --- | --- |
| Language | Go 1.26 |
| HTTP | [Gin](https://github.com/gin-gonic/gin), [gorilla/websocket](https://github.com/gorilla/websocket) |
| Database | PostgreSQL 16 (TimescaleDB image), [pgx v5](https://github.com/jackc/pgx) through `database/sql`, [golang-migrate](https://github.com/golang-migrate/migrate) with its pgx driver |
| Cache | Redis 7 ([go-redis v9](https://github.com/redis/go-redis)) |
| Messaging | RabbitMQ 3.13 ([amqp091-go](https://github.com/rabbitmq/amqp091-go)) |
| Authentication | RS256 JWT ([golang-jwt v5](https://github.com/golang-jwt/jwt)) |
| Money | [shopspring/decimal](https://github.com/shopspring/decimal) |
| API documentation | [swaggo](https://github.com/swaggo/swag) |

## Getting started

### Prerequisites

- Go 1.26 or later
- Docker with Docker Compose, for PostgreSQL, Redis, and RabbitMQ
- OpenSSL, to generate the JWT signing keys

### 1. Clone the repository

```bash
git clone https://github.com/mdastpak/Crypto-Exchange.git
cd Crypto-Exchange
```

### 2. Start the dependencies

```bash
docker compose up -d postgres redis rabbitmq
```

This starts only the dependencies, for running the server with Go in the next steps. To run the server in Docker as well, see [Run everything with Docker Compose](#run-everything-with-docker-compose).

### 3. Generate JWT signing keys

```bash
mkdir -p internal/ssl/user
openssl genpkey -algorithm RSA -out internal/ssl/user/private_key.pem -pkeyopt rsa_keygen_bits:2048
openssl rsa -pubout -in internal/ssl/user/private_key.pem -out internal/ssl/user/public_key.pem
```

PEM files under `internal/ssl/` are ignored by Git. Never commit key material.

### 4. Create a local configuration

```bash
cp config/config.example.yaml config/config.local.yaml
```

The example works as it is for a local setup: it reads the user keys from step 3. Keep credentials out of the file and supply them through environment variables instead. The following values match the Docker Compose defaults:

```bash
export POSTGRES_PASSWORD=postgres
export REDIS_PASSWORD=redis
export RABBITMQ_USER=guest
export RABBITMQ_PASSWORD=guest
```

### 5. Run the server

Run the server from the repository root, because the default migrations path is relative:

```bash
go run ./cmd/server -config config/config.local.yaml
```

| Flag | Default | Description |
| --- | --- | --- |
| `-config` | `config/config.yaml` | Path to the YAML configuration file |
| `-migrations` | `file://internal/db/migrations` | Migration source URL |

Without `-config`, the server reads `config/config.yaml`, or an embedded copy of the example when that file does not exist. Most settings can also be set with an environment variable, such as `SERVER_PORT` or `SERVER_MODE=release`; [docs/configuration.md](docs/configuration.md#environment-variables) lists them all. When a required setting is missing or a value is invalid, the server stops and lists every problem, each with its variable. In release mode it also refuses insecure settings, such as no TLS or default passwords ([release mode](docs/configuration.md#release-mode)).

Once the server is running:

- API: `http://localhost:8080/`
- Swagger UI: `http://localhost:8080/swagger/index.html`, outside release mode or when `server.swagger.enabled` is `true`. It serves the OpenAPI document in `docs/`, which covers every route.
- Readiness: `http://localhost:8080/system/ready`, which answers `503` while a dependency is unavailable

Sign-in codes are delivered by email and SMS once `notifications` is configured (see [docs/configuration.md](docs/configuration.md#notifications)). To try sign-in locally without a provider, set `otp.auth.dev_log_codes: true` in `config/config.local.yaml`; the codes then appear in the server log. Never enable it outside development: the server refuses to start with it in release mode.

### 6. Create an administrator (optional)

Admin sign-in needs its own signing key pair, a key that encrypts authenticator secrets, and email delivery (or `dev_log_codes` locally):

```bash
mkdir -p internal/ssl/admin
openssl genpkey -algorithm RSA -out internal/ssl/admin/private_key.pem -pkeyopt rsa_keygen_bits:2048
openssl rsa -pubout -in internal/ssl/admin/private_key.pem -out internal/ssl/admin/public_key.pem
export ADMIN_TOTP_KEY="$(openssl rand -base64 32)"
```

Add the admin key paths under `jwt.ssl.admin` in `config/config.local.yaml`, as for the user keys. Then create the administrator. The command prints an `otpauth://` URI and a key for the authenticator app, once:

```bash
go run ./cmd/admin -config config/config.local.yaml create -email admin@example.com
```

The server and the command must use the same `ADMIN_TOTP_KEY`. With [data-protection keys](docs/data-protection.md) configured (`KEYS_PROVIDER`), they seal authenticator secrets with those instead, and `ADMIN_TOTP_KEY` is only needed to read secrets sealed before; `admin totp-secrets` moves them ([#146](https://github.com/mdastpak/Crypto-Exchange/issues/146)). `reset-totp` replaces a lost authenticator, and `disable` stops an administrator from signing in. Each command records an audit event; pass `-operator <name>` before the command to name yourself in it. [docs/api.md](docs/api.md#admin-sign-in) describes the sign-in.

### Run everything with Docker Compose

The `app` service builds the server image and runs it with [`config/config.compose.yaml`](config/config.compose.yaml), which holds no secrets. Passwords come from environment variables, and the user keys from step 3 are mounted as Compose secrets. To change settings, copy [`.env.example`](.env.example) to `.env`: Compose uses it for the dependencies and passes all of it to the server.

```bash
cp .env.example .env
docker compose up -d --build
curl http://localhost:8080/system/ready
```

- The `migrate` service applies the migrations with the migration role (`POSTGRES_MIGRATIONS_USER`) and exits, and the `app` service then starts with the runtime role (`POSTGRES_USER`), which cannot change the schema ([Database roles](docs/configuration.md#database-roles)). Both roles are created when the PostgreSQL volume is created; a volume from before them keeps its single role until `docker compose down -v` removes it.
- Sign-in codes appear in the log of the `app` service (`docker compose logs app`), because the Compose configuration enables `dev_log_codes`.
- On Linux, run the server as your own user so that it can read the key files without widening their permissions: `APP_UID=$(id -u) APP_GID=$(id -g) docker compose up -d --build`.
- `JWT_USER_PRIVATE_KEY_FILE` and `JWT_USER_PUBLIC_KEY_FILE` select other key files.
- To create an administrator in the stack, set `ADMIN_TOTP_KEY` and run the `admin` command from the image: `docker compose run --rm --no-deps --entrypoint /app/admin app -config config/config.compose.yaml create -email admin@example.com`. Admin sign-in additionally needs the admin keys, which this file does not mount.

The image contains only the `server` and `admin` binaries and the migrations, and runs as an unprivileged user. Configuration and keys are mounted at runtime and never built into it. Hardening for staging and production is tracked in [#103](https://github.com/mdastpak/Crypto-Exchange/issues/103).

## Configuration

Configuration is loaded from a YAML file, missing values are filled with defaults, and selected environment variables override the result. [docs/configuration.md](docs/configuration.md) describes every section, the defaults, and the supported environment variables.

The data-protection keys, which future row seals and field encryption build on, are wrapped by a key-encryption key in HashiCorp Vault or OpenBao (Transit). [docs/data-protection.md](docs/data-protection.md) describes the key hierarchy, the setup, and how to rotate the keys.

Redis runs as one node, behind Sentinel, or as a cluster; [docs/redis-high-availability.md](docs/redis-high-availability.md) describes the deployment and what a failover may lose. RabbitMQ runs as a cluster of three nodes with quorum queues, and the server keeps working without it, except for sending sign-in codes; see [docs/rabbitmq-high-availability.md](docs/rabbitmq-high-availability.md).

Backups, point-in-time recovery, the restore drill, and the disaster-recovery runbook are in [docs/disaster-recovery.md](docs/disaster-recovery.md). `admin backup status` alerts when WAL archiving falls behind the RPO, and `admin restore verify` checks a restored database before it is put in service.

## API overview

The API is versioned under `/api/v1`: the paths of the API in the table below are relative to it, so `/public/currencies` is `/api/v1/public/currencies`. The health probes under `/system/` are not part of the API and have no version. The unversioned paths of earlier releases still answer as deprecated aliases, with `Deprecation` and `Link` headers that point to the versioned path ([docs/api.md](docs/api.md#versions)).

All JSON responses, except the system endpoints, use this envelope:

```json
{ "status": 200, "message": "Human-readable message", "info": {} }
```

Error responses also carry a stable `code`, such as `validation_failed` or `rate_limited`, and the `request_id`; [docs/api.md](docs/api.md#errors) lists the codes.

| Method | Path | Authentication | Description |
| --- | --- | --- | --- |
| `GET` | `/system/live` | None | Liveness: the process serves requests |
| `GET` | `/system/ready` | None | Readiness: `503` while PostgreSQL or Redis is unavailable; `degraded` without RabbitMQ |
| `GET` | `/system/healthcheck` | None | Earlier name of `/system/ready` |
| `GET` | `/system/ping` | None | Ping |
| `GET` | `/public/currencies` | None | List cached currencies |
| `GET` | `/public/currencies/{hk}` | None | Get a currency by its UUID |
| `POST` | `/public/auth` | None | Request an OTP |
| `POST` | `/public/auth/{uuid}` | None | Verify an OTP and receive tokens |
| `POST` | `/public/auth/refresh` | Refresh token cookie | Issue a new access token |
| `POST` | `/public/auth/logout` | Access token | End the session and clear the refresh token cookie |
| `POST` | `/admin/auth` | None | Request an admin sign-in code |
| `POST` | `/admin/auth/{uuid}` | None | Verify the emailed code and the authenticator code, and receive an admin token |
| `POST` | `/admin/logout` | Admin token | Revoke the admin token |
| `POST` | `/admin/currencies` | Admin token | Create a currency |
| `GET` | `/admin/currencies` | Admin token | List currencies of every status, with filters and pages |
| `GET` | `/admin/currencies/{hk}` | Admin token | Get a currency, whatever its status, with its version |
| `PUT` | `/admin/currencies/{hk}` | Admin token | Update a currency |
| `DELETE` | `/admin/currencies/{hk}` | Admin token | Soft-delete a currency |
| `POST` | `/admin/currencies/{hk}/restore` | Admin token | Restore a deleted currency as pending |
| `GET` | `/admin/audit` | Admin token | Query the audit log |
| `GET` | `/user/profile` | User token | Get the signed-in user's profile |
| `PATCH` | `/user/profile` | User token | Change the names in the signed-in user's profile |
| `GET` | `/user/sessions` | User token | List the active sessions |
| `DELETE` | `/user/sessions/{id}` | User token | End one session |
| `DELETE` | `/user/sessions` | User token | End every other session |
| `GET` | `/user/contacts` | User token | List the verified contacts |
| `POST` | `/user/contacts/changes` | User token | Start adding, replacing, or removing a contact |
| `POST` | `/user/contacts/changes/{id}` | User token | Confirm a contact change with its codes |
| `POST` | `/user/logout` | User token | End the session |
| `GET` | `/ws/` | None | WebSocket gateway |

Request and response details, the authentication flow, and the WebSocket protocol are described in [docs/api.md](docs/api.md).

## Testing

Run the unit tests, which need no infrastructure:

```bash
go test ./internal/domain/... ./internal/auth/... ./internal/middleware/... ./internal/helpers/... ./internal/config/... \
  ./internal/assetregistry/... ./internal/clientip/... ./internal/contact/... ./internal/pagination/... ./internal/totp/... \
  ./internal/i18n/... ./internal/geoip/... ./internal/sites/... \
  ./internal/testutil/openapitest/... ./internal/testutil/tcpproxy/... ./pkg/...
```

Integration tests need PostgreSQL, Redis, RabbitMQ, and an OpenBao dev server on the default ports:

```bash
docker run -d --name ce-test-postgres -e POSTGRES_PASSWORD=test -e POSTGRES_DB=testdb -p 5432:5432 postgres:16-alpine
docker run -d --name ce-test-redis -p 6379:6379 redis:7-alpine
docker run -d --name ce-test-rabbitmq -p 5672:5672 rabbitmq:3.13
docker run -d --name ce-test-openbao -e BAO_DEV_ROOT_TOKEN_ID=test -p 8200:8200 openbao/openbao:2.4.1
go test ./...
```

> [!CAUTION]
> The integration tests create databases whose names start with `testdb_`, empty them and build their schemas with the migrations, and they flush Redis databases 1 to 7, which [`internal/testutil/redistest`](internal/testutil/redistest/redistest.go) assigns, one per package. Never point them at an instance that holds real data. The shared test helper (`internal/testutil/pgtest`) refuses to empty any other database. Stop the Docker Compose PostgreSQL and Redis services first (`docker compose stop postgres redis`): the test containers use the same ports (5432 and 6379), and the tests expect Redis without a password.

CI runs the same suite with the race detector on every pull request, against these images, and fails when a test skips itself because a service is unreachable. See [`.github/workflows/ci.yml`](.github/workflows/ci.yml).

The key tests (`internal/keys`) use the OpenBao dev server, with the root token `test`, for the Transit engine; each test enables Transit at a mount of its own and removes it afterwards.

Each package has a PostgreSQL database and a Redis database of its own, so `go test` runs the packages in parallel. A new package that uses Redis takes the next database from `internal/testutil/redistest`.

The RabbitMQ tests (`internal/rabbitmq`) are skipped when no broker is reachable on port 5672. On some Docker Desktop and WSL 2 setups, RabbitMQ fails at startup with `Error when reading /var/lib/rabbitmq/.erlang.cookie: eacces`. Passing the Erlang cookie on the command line avoids reading that file:

```bash
docker run -d --name ce-test-rabbitmq -p 5672:5672 \
  -e RABBITMQ_SERVER_ADDITIONAL_ERL_ARGS="-setcookie local-test-cookie" \
  -e RABBITMQ_CTL_ERL_ARGS="-setcookie local-test-cookie" \
  rabbitmq:3.13
```

The expected behavior of every tested flow is specified in the [test-scenario catalog](docs/testing/test-scenarios.md).

## Project layout

```text
cmd/admin/              Operator command: administrators, data keys, outbox dead letters,
                        platform controls, backup and restore checks
cmd/market-worker/      Worker of the global market data catalog (docs/market-data.md)
cmd/server/             Application entry point, routing, and lifecycle
config/                 Configuration files: the example and Compose configurations,
                        the local config.yaml (ignored), and the asset registry and
                        AML rules (not loaded yet)
docs/                   Generated Swagger files and project documentation
internal/adminauth/     Administrator accounts and authenticator (TOTP) enrollment
internal/assetregistry/ Loading and checking the asset registry file
internal/audit/         The append-only audit log: recording and querying events
internal/auth/          JWT issuing and verification, key rotation, sessions, blacklist,
                        middleware
internal/clientip/      Client identity for per-client limits, with IPv6 prefixes
internal/config/        Configuration loading, environment variables, defaults, validation
internal/contact/       Normalizing and identifying the contacts and codes users type
internal/contactclaim/  Moving a contact off an account that no longer owns it: claims and
                        recycled mobile numbers
internal/currencies/    The public currency catalog: Redis cache with a PostgreSQL fallback
internal/db/            PostgreSQL and Redis clients, SQL migrations
internal/dberr/         Busy-database refusals of a request, answered as 503 database_busy
internal/delisting/     Delistings of currencies: lifecycle steps, applied once by every instance
internal/domain/        Exchange domain model: asset, market, order, trade, balance,
                        account, kyc, custody, shared types
internal/favorites/     Personal favorites of currencies and trading pairs
internal/geoip/         The country of a client: trusted edge header or MaxMind-format database
internal/handlers/      HTTP and WebSocket handlers (admin, public, socket, system, user)
internal/helpers/       Response helpers
internal/i18n/          Locales, text direction, negotiation, and translated messages
internal/identity/      Identity checks with a KYC provider: mobile ownership against the
                        national ID (Jibit client and a stub), sealed national IDs
internal/keys/          Data-protection keys wrapped by Vault or OpenBao Transit
internal/marketdata/    Global market data catalog: unverified assets, venues, pairs, tickers
internal/middleware/    Security headers, CORS, rate limiting, validation, recovery
internal/models/        API models for currencies and users
internal/notify/        Delivery of sign-in codes by email and SMS through RabbitMQ
internal/otp/           One-time codes: generation, hashed storage in Redis, attempt limits
internal/outbox/        Transactional outbox: relaying events to RabbitMQ, dead letters
internal/pagination/    Keyset pagination with opaque cursors for list endpoints
internal/platform/      Maintenance and feature lockdowns: controls, watcher, route gate
internal/redisutil/     Durable Redis writes, hash slots, and following cluster failovers
internal/recovery/      Backup and restore checks: WAL archiving age, restored-database
                        verification
internal/rabbitmq/      RabbitMQ connection, publishing with confirms, reconnect
internal/rebrand/        Currency migrations: rebrands, redenominations, mergers, by phase
internal/restrictions/  Compliance restrictions of users: account freeze, operation and
                        currency holds, the security hold after a security change, and
                        the check that operations call
internal/sites/         The sites per country: domains, locales, redirects
internal/ssl/           Where local signing keys go; the key files are ignored
internal/testutil/      Test helpers: PostgreSQL and Redis databases, OpenAPI contract
                        checks, users, a field sealer, and network proxies
internal/totp/          Time-based one-time passwords (RFC 6238)
scripts/dr/             The restore drill: point-in-time recovery from end to end on Docker
scripts/rabbitmq-ha/    The RabbitMQ failover drill: a three-node cluster on Docker
scripts/redis-ha/       The Redis failover drill: Sentinel and Cluster on Docker
```

## Roadmap

Work is organized into milestones. Each milestone has a tracking epic with ordered sub-issues.

| Milestone | Focus | Epic |
| --- | --- | --- |
| [M0 - Stabilize current state](https://github.com/mdastpak/Crypto-Exchange/milestone/1) | Fix defects and security issues in existing features | [#83](https://github.com/mdastpak/Crypto-Exchange/issues/83) |
| [M1 - Foundation and hardening](https://github.com/mdastpak/Crypto-Exchange/milestone/2) | CI, hermetic tests, user model, configuration, observability | [#84](https://github.com/mdastpak/Crypto-Exchange/issues/84) |
| [M2 - Phase 1: OTC MVP](https://github.com/mdastpak/Crypto-Exchange/milestone/3) | Price engine, OTC quotes and execution, ledger, custody, KYC/AML | [#85](https://github.com/mdastpak/Crypto-Exchange/issues/85) |
| [M3 - Phase 2: Spot trading](https://github.com/mdastpak/Crypto-Exchange/milestone/4) | Order gateway, matching engine, order book data, margin | [#86](https://github.com/mdastpak/Crypto-Exchange/issues/86) |
| [M4 - Phase 3: Derivatives](https://github.com/mdastpak/Crypto-Exchange/milestone/5) | Perpetual futures and options | [#87](https://github.com/mdastpak/Crypto-Exchange/issues/87) |

Issues are labeled by `type:`, `priority:` (P0 to P3), and `feature:`. [CONTRIBUTING.md](CONTRIBUTING.md#labels-and-milestones) explains the labels.

## Contributing

Contributions are welcome. Read [CONTRIBUTING.md](CONTRIBUTING.md) and the [Code of Conduct](CODE_OF_CONDUCT.md) before opening a pull request. All code, comments, commit messages, issues, and documentation must be written in English.

## Security

Do not report vulnerabilities in public issues. Follow [SECURITY.md](SECURITY.md) to report them privately.

## License

This project is licensed under the [MIT License](LICENSE).
