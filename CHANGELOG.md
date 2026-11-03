# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/). No versions have been tagged yet, so entries are grouped by the date the changes reached `main`. Semantic versioning will start with the first tagged release.

## [Unreleased]

### Added

- Project documentation: `README.md`, `SECURITY.md`, `CONTRIBUTING.md`, `CODE_OF_CONDUCT.md`, `docs/architecture.md`, `docs/configuration.md`, and `docs/api.md`.
- Issue templates and a pull request template.
- Test-scenario catalog with stable scenario IDs and a finding-status table, in [`docs/testing/test-scenarios.md`](docs/testing/test-scenarios.md) ([#90](https://github.com/mdastpak/Crypto-Exchange/pull/90)).

### Changed

- Moved the technical audit notes of 2026-09-14 from this file to [`docs/audits/2026-09-14-technical-audit.md`](docs/audits/2026-09-14-technical-audit.md).
- `config/config.local.yaml` is now ignored by Git.

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
