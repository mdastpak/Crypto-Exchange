# Configuration

This document describes how the server is configured and which settings it currently uses.

## Loading order

1. **File.** The YAML file passed with `-config` is read. The default is `config/config.yaml`. If the file does not exist, the embedded copy of `config/config.example.yaml` is used instead. The `app` service of `docker-compose.yml` uses `config/config.compose.yaml`, which holds no secrets.
2. **Defaults.** Numeric and string settings that are still zero or empty after loading receive the defaults listed below.
3. **Environment variables.** The variables listed in [Environment variables](#environment-variables) override the corresponding settings.
4. **Validation.** Missing credentials and similar problems are logged as warnings. The server does not refuse to start on them yet. Fail-closed validation is tracked in [#41](https://github.com/mdastpak/Crypto-Exchange/issues/41).

> [!IMPORTANT]
> `${VAR}` placeholders in YAML files are **not** expanded. A value such as `password: "${POSTGRES_PASSWORD}"` is used literally unless the environment variable overrides it ([#29](https://github.com/mdastpak/Crypto-Exchange/issues/29)).

## Command-line flags and process environment

| Name | Default | Description |
| --- | --- | --- |
| `-config` | `config/config.yaml` | Path to the configuration file |
| `-migrations` | `file://internal/db/migrations` | golang-migrate source URL. The default is relative to the working directory, so start the server from the repository root. |
| `GIN_MODE` | `debug` | Set to `release` to enable Gin release mode. The `server.mode` setting is not used for this yet. |

## Settings in use

### `server`

| Key | Default | Description |
| --- | --- | --- |
| `ip` | none | Listen address, for example `0.0.0.0` |
| `port` | none | Listen port, for example `8080` |
| `read_timeout_seconds` | `10` | HTTP read timeout |
| `write_timeout_seconds` | `10` | HTTP write timeout |
| `idle_timeout_seconds` | `120` | HTTP keep-alive idle timeout |
| `mode` | `debug` | Parsed, but not used by the active middleware yet |
| `trusted_proxies` | `[]` | IP addresses or CIDR ranges of reverse proxies whose `X-Forwarded-For` and `X-Real-IP` headers are trusted. With an empty list the client address is the TCP peer address, and forwarding headers are ignored. Behind a load balancer or ingress, list only its addresses; otherwise clients can spoof their address and bypass IP-based rate limits. An invalid entry stops the server at startup. |
| `tls.enabled` | `false` | Serve HTTPS with `ListenAndServeTLS` |
| `tls.cert_file`, `tls.key_file` | none | Certificate and key paths, used when TLS is enabled |
| `cors.allowed_origins` | `[]` | Browser origins (`scheme://host[:port]`, for example `https://app.example.com` or `http://localhost:3000`) that may call the API across origins with credentials. Matching is exact and case-insensitive. With an empty list, no cross-origin access is granted. A wildcard (`"*"`), a path, or any other malformed entry stops the server at startup, because a wildcard combined with credentials would let any site read authenticated responses. |

### `postgres`

| Key | Default | Description |
| --- | --- | --- |
| `host`, `port`, `user`, `password`, `dbname` | none | Connection settings |
| `sslmode` | `disable` | libpq SSL mode. Use `verify-full` outside local development. |
| `max_open_conns` | `25` | Maximum open connections |
| `max_idle_conns` | `5` | Maximum idle connections |
| `conn_max_lifetime_minutes` | `5` | Maximum connection lifetime |
| `conn_max_idle_time_minutes` | `5` | Maximum idle time per connection |
| `connect_retries` | `3` | Startup connection attempts |
| `connect_retry_delay_ms` | `500` | Delay between startup attempts |

Migrations are applied automatically at startup while a PostgreSQL advisory lock is held.

If a migration fails, golang-migrate marks its version as dirty and the server refuses to start until the flag is cleared. Fix the cause named in the error message, then reset the version to the last successful migration with the [golang-migrate CLI](https://github.com/golang-migrate/migrate/tree/master/cmd/migrate) and start the server again:

```bash
migrate -path internal/db/migrations \
  -database "postgres://USER:PASSWORD@HOST:5432/exchange?sslmode=disable" \
  force 10
```

Replace `10` with the version before the failed migration.

### `redis`

| Key | Default | Description |
| --- | --- | --- |
| `host`, `port`, `password` | none | Connection settings |
| `db` | `0` | Database index |
| `pool_size` | `10` | Connection pool size |
| `min_idle_conns` | `2` | Minimum idle connections |
| `connect_retries` | `3` | Startup connection attempts |
| `connect_retry_delay_ms` | `500` | Delay between startup attempts |

### `rabbitmq`

| Key | Default | Description |
| --- | --- | --- |
| `host`, `port`, `username`, `password` | none | Connection settings. The server connects to `amqp://username:password@host:port/`. |
| `exchanges.price_updates` | none | Fan-out exchange used for currency change events |
| `exchanges.trade_events` | none | Declared at startup. Not used yet. |
| `exchanges.notifications` | none | Fan-out exchange for sign-in code jobs. The delivery worker consumes it through the `notifications.otp` queue (see [`notifications`](#notifications)). |
| `exchanges.order_events`, `exchanges.settlement` | none | Parsed, but not declared or used yet |

### `jwt`

| Key | Default | Description |
| --- | --- | --- |
| `ssl.user.private_key` | **required** | Path to the RSA private key PEM that signs user tokens, as a list of path segments, for example `["internal", "ssl", "user", "private_key.pem"]` |
| `ssl.user.public_key` | **required** | Path to the matching public key PEM, as a list of path segments |
| `ssl.user.retired_public_keys` | `[]` | Public keys of earlier user key pairs, each as a list of path segments. Tokens they signed stay valid until they expire. See [Rotating signing keys](#rotating-signing-keys). |
| `ssl.admin.private_key`, `ssl.admin.public_key` | none | The key pair that signs admin tokens. Without it, admin routes reject every token, and the server logs a warning. It must differ from the user key pair. |
| `ssl.admin.retired_public_keys` | `[]` | Public keys of earlier admin key pairs |
| `issuer` | `encrypted-db-exchange` | Issuer claim written into tokens and required on every token |
| `access_token.expiration_minutes` | **required** | Access token lifetime. There is no default; `0` produces tokens that expire immediately. |
| `refresh_token.expiration_minutes` | **required** | Refresh token lifetime, and the cookie `Max-Age` |

Keys must be RSA keys of at least 2048 bits, and a public key must belong to its private key. Tokens name their signing key in the `kid` header, which is derived from a SHA-256 hash of the public key, so key IDs need no configuration. The server refuses to start if a key is invalid or shared between the roles.

#### Rotating signing keys

1. Generate a new key pair for the role.
2. Point `private_key` and `public_key` at the new pair, and add the old public key to `retired_public_keys`.
3. Restart every server instance. New tokens are signed with the new key, and tokens signed with the old key keep working.
4. After `refresh_token.expiration_minutes` (or `access_token.expiration_minutes` for admin keys, which have no refresh tokens), remove the old key from `retired_public_keys`.

### `otp`

| Key | Default | Description |
| --- | --- | --- |
| `auth.length` | `6` | Number of digits, from 4 to 10 |
| `auth.ttl_seconds` | `60` | Lifetime of a code and of its sign-in session |
| `auth.retry_limit` | `3` | Verification attempts per code. After the last failed attempt, the code is erased and further attempts are refused until the session expires. |
| `auth.secret` | **required** in release mode | Key for the HMAC of each code and the encryption of contacts in Redis, so that reading Redis reveals neither. At least 32 bytes, for example from `openssl rand -base64 48`, and the same on every server instance. `OTP_SECRET` overrides it. Changing it only invalidates the codes that are pending. Outside release mode, an empty secret is replaced by a random one at startup, which works only with a single server process. |
| `auth.contact_requests_per_hour` | `5` | Codes issued to one email address or mobile number per hour |
| `auth.contact_attempts_per_hour` | `10` | Verification attempts for one contact per hour, across all of its codes, so that requesting a new code does not bring new attempts. A successful sign-in resets the count. It must be at least `retry_limit`. Anyone who knows a contact can use up its attempts and block its sign-in for up to an hour, so keep it well above `retry_limit`. |
| `auth.dev_log_codes` | `false` | Development only. Writes each sign-in code to the log, with a masked contact, instead of delivering it through [`notifications`](#notifications), so that sign-in can be tried locally without an email or SMS provider. The server refuses to start with `true` in release mode (`GIN_MODE=release`). |
| `auth.name` | none | Parsed, but not used yet |

The server refuses to start when a value is out of range or the secret is shorter than 32 bytes.

### `notifications`

Sign-in codes are published to the `rabbitmq.exchanges.notifications` exchange and delivered by a worker in the server process. The worker sends codes for email addresses by SMTP and codes for mobile numbers by SMS. A channel without configuration is disabled, and sign-in with that contact type is refused with `400`. With no channel at all, sign-in is unavailable unless `otp.auth.dev_log_codes` is enabled for development.

| Key | Default | Description |
| --- | --- | --- |
| `email.host` | none | SMTP server. Empty disables email. |
| `email.port` | `587` | SMTP port |
| `email.username`, `email.password` | none | SMTP credentials, sent with `AUTH PLAIN`. Leave both empty for a relay without authentication. `SMTP_PASSWORD` overrides the password. |
| `email.from` | **required** with `email.host` | Sender address, optionally with a display name, for example `Exchange <no-reply@example.com>` |
| `email.implicit_tls` | `false` | Connect with TLS from the start, usually on port `465`. Otherwise the connection is upgraded with `STARTTLS`. Codes and credentials are only sent over TLS; a server without `STARTTLS` is refused unless it runs on the local machine. |
| `email.timeout_seconds` | `10` | Limit for one SMTP session |
| `email.otp_subject`, `email.otp_body` | An English subject and body | [Go templates](https://pkg.go.dev/text/template) for the email. They can use `{{.Code}}` and `{{.TTLSeconds}}`, and can be written in any language. The server refuses to start if a template is invalid or the subject spans several lines. |
| `sms.provider` | none | `kavenegar`, or empty to disable SMS |
| `sms.kavenegar.api_key` | **required** with the provider | Kavenegar API key. `KAVENEGAR_API_KEY` overrides it. |
| `sms.kavenegar.otp_template` | **required** with the provider | A verify template approved in the Kavenegar panel. The code fills its `%token` placeholder, so the panel defines the text and language of the message. |
| `sms.kavenegar.base_url` | `https://api.kavenegar.com` | API base URL. Codes are sent with the [verify lookup](https://kavenegar.com/rest.html) method. |
| `sms.kavenegar.timeout_seconds` | `10` | Limit for one API request |
| `worker.concurrency` | `4` | Codes delivered in parallel by each server instance |
| `worker.max_attempts` | `3` | Delivery attempts per code. Temporary failures, such as timeouts or SMTP `4xx` replies, are retried with exponential backoff starting at one second. Permanent failures, such as an unknown recipient or invalid credentials, are not retried. Delivery also stops once the code expires. |

The worker declares this RabbitMQ topology at startup:

| Name | Type | Purpose |
| --- | --- | --- |
| `notifications.otp` | Durable queue bound to the notifications exchange | Pending sign-in code jobs. Jobs are not persisted to disk and expire with the code (`otp.auth.ttl_seconds`), so a code that waits too long is dropped by the broker. Several server instances share the queue. |
| `notifications.dead-letter` | Durable fan-out exchange | Receives a record of every code that could not be delivered |
| `notifications.otp.dead-letter` | Durable queue bound to `notifications.dead-letter` | Keeps those records for seven days. A record holds the contact type, the masked contact, the reason, the number of attempts, and the last error, but never the code or the raw contact. Its depth is a direct measure of delivery failures. |

Failed attempts and abandoned codes are also logged with the masked contact. Delivery metrics are part of [#50](https://github.com/mdastpak/Crypto-Exchange/issues/50).

### `admin`

| Key | Default | Description |
| --- | --- | --- |
| `totp_key` | none | Encrypts the authenticator (TOTP) secrets of administrators in the database with AES-256-GCM: 32 random bytes, base64-encoded, for example from `openssl rand -base64 32`. `ADMIN_TOTP_KEY` overrides it. Without it, admin sign-in is disabled. The server and the `cmd/admin` command must use the same key. If it changes, every administrator must be enrolled again with `reset-totp`. |

Authenticator apps show `exchange.name` as the issuer of the account. Each encrypted secret is bound to its administrator's ID, so it cannot be copied to another user row.

Writing to the database is not enough to create an administrator: changing `users.role` to `admin` does not enable admin sign-in without a secret sealed with this key. Someone with both database access and this key could, so keep it in a secret manager, apart from database credentials.

### `websocket`

| Key | Default | Description |
| --- | --- | --- |
| `allowed_origins` | `[]` | Origins accepted by the WebSocket origin check. HTTP CORS uses `server.cors.allowed_origins` instead. Entries are matched exactly, except `"*"`, which allows every origin ([#47](https://github.com/mdastpak/Crypto-Exchange/issues/47)). With an empty list, only same-origin connections are accepted. Never use `"*"`. |
| `ping_interval_seconds` | `30` | Parsed, but not used yet. The interval is hard-coded ([#47](https://github.com/mdastpak/Crypto-Exchange/issues/47)). |
| `max_message_size_bytes` | `1048576` | Parsed, but not used yet. The read limit is hard-coded to 512 bytes. |

### `rate_limits`

| Key | Default | Applies to |
| --- | --- | --- |
| `rest.requests_per_second`, `rest.burst` | `100`, `200` | Every route except `/` and `/swagger/*` ([#32](https://github.com/mdastpak/Crypto-Exchange/issues/32)), as a token bucket per client IP |
| `ws.messages_per_second`, `ws.burst` | `50`, `100` | The WebSocket upgrade request |
| `trading.orders_per_second`, `trading.burst` | `10`, `20` | `/admin/*` and `/user/*` |
| `auth.login_attempts_per_minute` | `5` | Every `/public/*` route ([#26](https://github.com/mdastpak/Crypto-Exchange/issues/26)). This is not a fixed window: every request, including a rejected one, restarts the counter's one-minute expiry, so a blocked client is only allowed again after a full minute without requests ([#34](https://github.com/mdastpak/Crypto-Exchange/issues/34)). |
| `auth.otp_requests_per_hour` | `10` | Parsed, but not applied yet |

## Settings reserved for roadmap features

The following sections are parsed, and some receive defaults, but no code uses them yet. They document the intended configuration surface of the [roadmap](https://github.com/mdastpak/Crypto-Exchange/issues/85).

| Section | Purpose | Related issues |
| --- | --- | --- |
| `exchange` | Display name, time zone, maintenance window | none |
| `assets` | Asset registry path (`configs/assets.yaml`) and automatic synchronization | [#57](https://github.com/mdastpak/Crypto-Exchange/issues/57) |
| `markets` | Default maker and taker fees, order type switches | [#57](https://github.com/mdastpak/Crypto-Exchange/issues/57), [#77](https://github.com/mdastpak/Crypto-Exchange/issues/77) |
| `price_aggregator` | Price providers, VWAP windows, outlier threshold, quorum, staleness | [#58](https://github.com/mdastpak/Crypto-Exchange/issues/58) |
| `custody` | Custody providers per chain | [#68](https://github.com/mdastpak/Crypto-Exchange/issues/68), [#69](https://github.com/mdastpak/Crypto-Exchange/issues/69) |
| `fiat` | Banking providers | [#74](https://github.com/mdastpak/Crypto-Exchange/issues/74) |
| `compliance` | Sanctions providers, AML rules path (`configs/aml_rules.yaml`), KYC providers | [#72](https://github.com/mdastpak/Crypto-Exchange/issues/72), [#73](https://github.com/mdastpak/Crypto-Exchange/issues/73) |
| `observability` | Logging, metrics, tracing | [#50](https://github.com/mdastpak/Crypto-Exchange/issues/50) |

## Environment variables

Only the variables below are read. Several variables in `.env.example` are not read yet ([#29](https://github.com/mdastpak/Crypto-Exchange/issues/29)).

| Variable | Overrides |
| --- | --- |
| `POSTGRES_HOST`, `POSTGRES_PORT`, `POSTGRES_USER`, `POSTGRES_PASSWORD` | `postgres.*` |
| `POSTGRES_DB` | `postgres.dbname`. Note that `.env.example` currently lists `POSTGRES_DBNAME`, which is not read. |
| `REDIS_HOST`, `REDIS_PORT`, `REDIS_PASSWORD` | `redis.*` |
| `RABBITMQ_HOST`, `RABBITMQ_PORT`, `RABBITMQ_USER`, `RABBITMQ_PASSWORD` | `rabbitmq.*` |
| `JWT_ISSUER` | `jwt.issuer` |
| `OTP_SECRET` | `otp.auth.secret` |
| `ADMIN_TOTP_KEY` | `admin.totp_key` |
| `SMTP_PASSWORD` | `notifications.email.password` |
| `KAVENEGAR_API_KEY` | `notifications.sms.kavenegar.api_key` |
| `LOG_LEVEL` | `observability.logging.level` |
| `OTEL_ENDPOINT` | `observability.tracing.endpoint` |
| `BINANCE_API_KEY`, `BINANCE_API_SECRET` | Credentials of the `binance` price provider |
| `FIAT_PROVIDER_API_KEY`, `FIAT_PROVIDER_API_SECRET` | Credentials of every fiat provider |
| `CHAINALYSIS_API_KEY`, `TRM_API_KEY` | Credentials of the matching sanctions provider |
| `SUMSUB_API_KEY`, `ONFIDO_API_KEY` | Credentials of the matching KYC provider |

## Secrets

- Never commit credentials or key material. `config/config.local.yaml`, `.env` files, and `internal/ssl/**/*.pem` are ignored by Git. `config/config.yaml` is listed in `.gitignore` but is still tracked in the repository ([#29](https://github.com/mdastpak/Crypto-Exchange/issues/29)), so never put secrets in it.
- Prefer environment variables, or files mounted from a secret manager, for passwords and keys.
- Generate separate key pairs for every environment, and rotate them if they are ever exposed.
- Use a separate `OTP_SECRET` for every environment, and rotate it if it is ever exposed.
