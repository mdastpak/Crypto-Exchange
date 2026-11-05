# Configuration

This document describes how the server is configured and which settings it currently uses.

## Loading order

1. **File.** The YAML file passed with `-config` is read. The default is `config/config.yaml`, which is not part of the repository. If the file does not exist, the embedded copy of [`config/config.example.yaml`](../config/config.example.yaml) is used instead, and the log says so. The `app` service of `docker-compose.yml` uses `config/config.compose.yaml`, which holds no secrets.
2. **Environment variables.** The variables listed in [Environment variables](#environment-variables) override the corresponding settings. A variable that is unset or empty leaves the setting unchanged.
3. **Defaults.** Numeric and string settings that are still zero or empty receive the defaults listed below.
4. **Validation.** The server stops with one message that names every problem when a value in an environment variable cannot be parsed, when `server.mode` is not a known mode, or when a setting it cannot start without is not set. Each message names the setting and its environment variable, for example `rabbitmq.username (RABBITMQ_USER)`. The required settings are marked **required** below. Other problems, such as an empty database password, are logged as warnings. Stricter validation is tracked in [#41](https://github.com/mdastpak/Crypto-Exchange/issues/41).

`${VAR}` placeholders in YAML files are **not** expanded. Keep secrets out of the file and set them with the environment variables.

## Command-line flags and process environment

| Name | Default | Description |
| --- | --- | --- |
| `-config` | `config/config.yaml` | Path to the configuration file |
| `-migrations` | `file://internal/db/migrations` | golang-migrate source URL. The default is relative to the working directory, so start the server from the repository root. |

## Settings in use

### `server`

| Key | Default | Description |
| --- | --- | --- |
| `ip` | none | Listen address, for example `0.0.0.0`. Empty listens on every interface. |
| `port` | **required** | Listen port, for example `8080` |
| `read_timeout_seconds` | `10` | HTTP read timeout |
| `write_timeout_seconds` | `10` | HTTP write timeout |
| `idle_timeout_seconds` | `120` | HTTP keep-alive idle timeout |
| `mode` | `debug` | `debug`, `release`, or `test`. Sets Gin's mode, and `release` turns on the release checks: `otp.auth.secret` is required, and `otp.auth.dev_log_codes` is refused. Any other value stops the server. `SERVER_MODE` overrides it, and so does `GIN_MODE`, its older name. |
| `trusted_proxies` | `[]` | IP addresses or CIDR ranges of reverse proxies whose `X-Forwarded-For` and `X-Real-IP` headers are trusted. With an empty list the client address is the TCP peer address, and forwarding headers are ignored. Behind a load balancer or ingress, list only its addresses; otherwise clients can spoof their address and bypass IP-based rate limits. An invalid entry stops the server at startup. |
| `swagger.enabled` | unset | Serves the Swagger UI at `/swagger/index.html`. When it is not set, the UI is served outside release mode only. |
| `max_body_bytes` | `1048576` | The largest request body accepted, in bytes. A larger body gets `413`. |
| `tls.enabled` | `false` | Serve HTTPS with `ListenAndServeTLS` |
| `tls.cert_file`, `tls.key_file` | none | Certificate and key paths, used when TLS is enabled |
| `cors.allowed_origins` | `[]` | Browser origins (`scheme://host[:port]`, for example `https://app.example.com` or `http://localhost:3000`) that may call the API across origins with credentials. Matching is exact and case-insensitive. With an empty list, no cross-origin access is granted. A wildcard (`"*"`), a path, or any other malformed entry stops the server at startup, because a wildcard combined with credentials would let any site read authenticated responses. |

### `postgres`

| Key | Default | Description |
| --- | --- | --- |
| `host`, `port`, `user`, `dbname` | **required** | Connection settings |
| `password` | none | Database password. Set it with `POSTGRES_PASSWORD`. |
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
| `host`, `port` | **required** | Connection settings |
| `password` | none | Redis password. Set it with `REDIS_PASSWORD`. |
| `db` | `0` | Database index |
| `pool_size` | `10` | Connection pool size |
| `min_idle_conns` | `2` | Minimum idle connections |
| `connect_retries` | `3` | Startup connection attempts |
| `connect_retry_delay_ms` | `500` | Delay between startup attempts |
| `dial_timeout_ms` | `1000` | How long a new connection may take, name resolution included |
| `read_timeout_ms`, `write_timeout_ms` | `1000`, `1000` | How long a command may wait for Redis to answer, or to accept the command |
| `pool_timeout_ms` | `2000` | How long a command may wait for a free connection when all of them are in use |
| `max_retries` | `1` | How often a command is retried after a network error. `-1` turns retries off. |

Redis is on the path of most requests, so these timeouts are short: while Redis cannot be reached, a request that needs it fails within a few seconds instead of waiting for the much longer defaults of the client library. The rate limiters give up even sooner, after `rate_limits.timeout_ms`.

### `rabbitmq`

| Key | Default | Description |
| --- | --- | --- |
| `host`, `port`, `username`, `password` | **required** | Connection settings. The server connects to `amqp://username:password@host:port/`. Set the credentials with `RABBITMQ_USER` and `RABBITMQ_PASSWORD`. |
| `exchanges.price_updates` | none | Fan-out exchange used for currency change events |
| `exchanges.trade_events` | none | Declared at startup. Not used yet. |
| `exchanges.notifications` | none | Fan-out exchange for sign-in code jobs. The delivery worker consumes it through the `notifications.otp` queue (see [`notifications`](#notifications)). |
| `exchanges.order_events`, `exchanges.settlement` | none | Parsed, but not declared or used yet |

### `jwt`

| Key | Default | Description |
| --- | --- | --- |
| `ssl.user.private_key` | **required** | Path to the RSA private key PEM that signs user tokens, for example `"internal/ssl/user/private_key.pem"`. A relative path starts at the working directory. A list of path segments, which earlier versions required, is still accepted. |
| `ssl.user.public_key` | **required** | Path to the matching public key PEM |
| `ssl.user.retired_public_keys` | `[]` | Paths of the public keys of earlier user key pairs. Tokens they signed stay valid until they expire. See [Rotating signing keys](#rotating-signing-keys). |
| `ssl.admin.private_key`, `ssl.admin.public_key` | none | The key pair that signs admin tokens. Without it, admin routes reject every token, and the server logs a warning. It must differ from the user key pair. |
| `ssl.admin.retired_public_keys` | `[]` | Public keys of earlier admin key pairs |
| `issuer` | `crypto-exchange` | Issuer claim written into tokens and required on every token |
| `access_token.expiration_minutes` | **required** | Access token lifetime. There is no default. |
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
| `auth.dev_log_codes` | `false` | Development only. Writes each sign-in code to the log, with a masked contact, instead of delivering it through [`notifications`](#notifications), so that sign-in can be tried locally without an email or SMS provider. The server refuses to start with `true` in release mode (`server.mode: release`). |
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

### `keys`

The data-protection keys seal rows and encrypt fields, so that someone with access to the database alone can neither change protected rows unnoticed nor read protected data. They are wrapped by a key-encryption key (KEK) that stays in a key management service. [`docs/data-protection.md`](data-protection.md) describes the key hierarchy and how to rotate keys. No feature uses the keys yet: row seals ([#139](https://github.com/mdastpak/Crypto-Exchange/issues/139)) and field encryption ([#140](https://github.com/mdastpak/Crypto-Exchange/issues/140)) will, and will then require a provider in release mode.

| Key | Default | Description |
| --- | --- | --- |
| `provider` | none | `transit` for the Transit engine of HashiCorp Vault or OpenBao, `local` for development, or empty. Empty, the server logs a warning and loads no keys. With a provider, the server unwraps the keys at startup and creates the first version of each on the first start; it refuses to start when a key cannot be unwrapped. |
| `local.kek` | none | Development only: the KEK itself, 32 random bytes, base64-encoded (`openssl rand -base64 32`). Whoever reads it can unwrap every data key, so release mode refuses `local`. |
| `transit.address` | none | Base URL of the Vault or OpenBao server, such as `https://vault.internal:8200`. Release mode requires `https`. |
| `transit.token` | none | Token with permission to use `encrypt` and `decrypt` of the key. `VAULT_TOKEN` overrides it. |
| `transit.token_file` | none | File that holds the token, read on every request instead of `token`, so that Vault Agent or OpenBao Agent can renew it in place |
| `transit.mount` | `transit` | Path of the Transit engine |
| `transit.key` | none | Name of the Transit key that wraps the data keys |
| `transit.ca_cert` | none | PEM file of the certificate authorities to trust for the server, instead of the system ones |
| `transit.timeout_seconds` | `10` | Timeout of each request to the server |

The key service is only contacted at startup and by the `admin keys` commands: a running server keeps the keys in memory and does not depend on it.

### `websocket`

| Key | Default | Description |
| --- | --- | --- |
| `allowed_origins` | `[]` | Origins accepted by the WebSocket origin check. HTTP CORS uses `server.cors.allowed_origins` instead. Entries are matched exactly, except `"*"`, which allows every origin ([#47](https://github.com/mdastpak/Crypto-Exchange/issues/47)). With an empty list, only same-origin connections are accepted. Never use `"*"`. |
| `ping_interval_seconds` | `30` | Parsed, but not used yet. The interval is hard-coded ([#47](https://github.com/mdastpak/Crypto-Exchange/issues/47)). |
| `max_message_size_bytes` | `1048576` | Parsed, but not used yet. The read limit is hard-coded to 512 bytes. |

### `rate_limits`

| Key | Default | Applies to |
| --- | --- | --- |
| `timeout_ms` | `250` | Every limiter: how long it waits for Redis. A limiter that gets no answer in time treats Redis as unreachable, and its `on_error` decides. The limiters of one request wait at most once together: once one of them finds Redis unreachable, the others decide at once. |
| `rest.requests_per_second`, `rest.burst` | `100`, `200` | Every route, as a token bucket per client IP. The health probes under `/system` are counted in the memory of each instance instead of in Redis, so that they answer while Redis is down. |
| `ws.messages_per_second`, `ws.burst` | `50`, `100` | The WebSocket upgrade request |
| `trading.orders_per_second`, `trading.burst` | `10`, `20` | `/admin/*` and `/user/*`, as a token bucket per signed-in user or administrator, identified by the subject and role of the access token |
| `rest.on_error`, `ws.on_error`, `trading.on_error` | `allow` | What the limiter does with a request while Redis cannot be reached: `allow` lets it through, `deny` rejects it with `503` and `Retry-After: 1` |
| `auth.on_error` | `deny` | The same for the two sign-in limits, which fail closed by default, because they protect the codes against guessing |
| `auth.login_attempts_per_minute` | `5` | Sign-in code verifications: `POST /public/auth/{uuid}` and `POST /admin/auth/{uuid}`, per client IP and route |
| `auth.otp_requests_per_hour` | `10` | Sign-in code requests: `POST /public/auth` and `POST /admin/auth`, per client IP and route |

The token buckets refill continuously and count fractions of a token, so any rate works, also one above 1000 per second. Every response of a limited route carries `X-RateLimit-Limit` and `X-RateLimit-Remaining`, and a `429` carries `Retry-After` in whole seconds, rounded up.

The two `auth` limits are counted in fixed windows of one minute and one hour, which start with a client's first request, so a blocked client is allowed again when its window ends. User and admin sign-in count separately, and the other `/public/*` routes, such as the currency reads, are only subject to the `rest` limit.

Clients behind one address, such as a carrier-grade NAT or an office network, share these budgets. Size `auth.otp_requests_per_hour` for the busiest shared address you expect. Raising it does not let anyone flood a single email address or mobile number, because `otp.auth.contact_requests_per_hour` limits the codes sent to each contact.

## Settings reserved for roadmap features

The following sections are parsed, and some receive defaults, but no code uses them yet. They document the intended configuration surface of the [roadmap](https://github.com/mdastpak/Crypto-Exchange/issues/85).

| Section | Purpose | Related issues |
| --- | --- | --- |
| `exchange` | Display name, time zone, maintenance window | none |
| `assets` | Asset registry path (`config/assets.yaml`) and automatic synchronization | [#57](https://github.com/mdastpak/Crypto-Exchange/issues/57) |
| `markets` | Default maker and taker fees, order type switches | [#57](https://github.com/mdastpak/Crypto-Exchange/issues/57), [#77](https://github.com/mdastpak/Crypto-Exchange/issues/77) |
| `price_aggregator` | Price providers, VWAP windows, outlier threshold, quorum, staleness | [#58](https://github.com/mdastpak/Crypto-Exchange/issues/58) |
| `custody` | Custody providers per chain | [#68](https://github.com/mdastpak/Crypto-Exchange/issues/68), [#69](https://github.com/mdastpak/Crypto-Exchange/issues/69) |
| `fiat` | Banking providers | [#74](https://github.com/mdastpak/Crypto-Exchange/issues/74) |
| `compliance` | Sanctions providers, AML rules path (`config/aml_rules.yaml`), KYC providers | [#72](https://github.com/mdastpak/Crypto-Exchange/issues/72), [#73](https://github.com/mdastpak/Crypto-Exchange/issues/73) |
| `observability` | Logging, metrics, tracing | [#50](https://github.com/mdastpak/Crypto-Exchange/issues/50) |

## Environment variables

Each variable overrides one setting, and an unset or empty variable leaves it unchanged. Integers and `true` or `false` are parsed strictly. Lists are comma-separated. [`.env.example`](../.env.example) lists the variables for a local setup, and a test checks that each of them is read.

| Variable | Setting |
| --- | --- |
| `GIN_MODE` | `server.mode`. Older name of `SERVER_MODE`, which takes precedence. |
| `SERVER_MODE` | `server.mode` |
| `SERVER_IP` | `server.ip` |
| `SERVER_PORT` | `server.port` |
| `SERVER_READ_TIMEOUT_SECONDS` | `server.read_timeout_seconds` |
| `SERVER_WRITE_TIMEOUT_SECONDS` | `server.write_timeout_seconds` |
| `SERVER_IDLE_TIMEOUT_SECONDS` | `server.idle_timeout_seconds` |
| `SERVER_TRUSTED_PROXIES` | `server.trusted_proxies`, as a comma-separated list |
| `SERVER_CORS_ALLOWED_ORIGINS` | `server.cors.allowed_origins`, as a comma-separated list |
| `SERVER_TLS_ENABLED` | `server.tls.enabled` |
| `SERVER_TLS_CERT_FILE` | `server.tls.cert_file` |
| `SERVER_TLS_KEY_FILE` | `server.tls.key_file` |
| `SERVER_SWAGGER_ENABLED` | `server.swagger.enabled` |
| `SERVER_MAX_BODY_BYTES` | `server.max_body_bytes` |
| `POSTGRES_HOST` | `postgres.host` |
| `POSTGRES_PORT` | `postgres.port` |
| `POSTGRES_USER` | `postgres.user` |
| `POSTGRES_PASSWORD` | `postgres.password` |
| `POSTGRES_DB` | `postgres.dbname` |
| `POSTGRES_SSLMODE` | `postgres.sslmode` |
| `POSTGRES_MAX_OPEN_CONNS` | `postgres.max_open_conns` |
| `POSTGRES_MAX_IDLE_CONNS` | `postgres.max_idle_conns` |
| `POSTGRES_CONN_MAX_LIFETIME_MINUTES` | `postgres.conn_max_lifetime_minutes` |
| `POSTGRES_CONN_MAX_IDLE_TIME_MINUTES` | `postgres.conn_max_idle_time_minutes` |
| `REDIS_HOST` | `redis.host` |
| `REDIS_PORT` | `redis.port` |
| `REDIS_PASSWORD` | `redis.password` |
| `REDIS_DB` | `redis.db` |
| `REDIS_POOL_SIZE` | `redis.pool_size` |
| `REDIS_MIN_IDLE_CONNS` | `redis.min_idle_conns` |
| `REDIS_DIAL_TIMEOUT_MS` | `redis.dial_timeout_ms` |
| `REDIS_READ_TIMEOUT_MS` | `redis.read_timeout_ms` |
| `REDIS_WRITE_TIMEOUT_MS` | `redis.write_timeout_ms` |
| `REDIS_POOL_TIMEOUT_MS` | `redis.pool_timeout_ms` |
| `REDIS_MAX_RETRIES` | `redis.max_retries` |
| `RABBITMQ_HOST` | `rabbitmq.host` |
| `RABBITMQ_PORT` | `rabbitmq.port` |
| `RABBITMQ_USER` | `rabbitmq.username` |
| `RABBITMQ_PASSWORD` | `rabbitmq.password` |
| `JWT_SSL_USER_PRIVATE_KEY` | `jwt.ssl.user.private_key` |
| `JWT_SSL_USER_PUBLIC_KEY` | `jwt.ssl.user.public_key` |
| `JWT_SSL_ADMIN_PRIVATE_KEY` | `jwt.ssl.admin.private_key` |
| `JWT_SSL_ADMIN_PUBLIC_KEY` | `jwt.ssl.admin.public_key` |
| `JWT_ISSUER` | `jwt.issuer` |
| `JWT_ACCESS_TOKEN_EXPIRATION_MINUTES` | `jwt.access_token.expiration_minutes` |
| `JWT_REFRESH_TOKEN_EXPIRATION_MINUTES` | `jwt.refresh_token.expiration_minutes` |
| `OTP_AUTH_NAME` | `otp.auth.name` |
| `OTP_AUTH_TTL_SECONDS` | `otp.auth.ttl_seconds` |
| `OTP_AUTH_LENGTH` | `otp.auth.length` |
| `OTP_AUTH_RETRY_LIMIT` | `otp.auth.retry_limit` |
| `OTP_SECRET` | `otp.auth.secret` |
| `ADMIN_TOTP_KEY` | `admin.totp_key` |
| `KEYS_PROVIDER` | `keys.provider` |
| `KEYS_LOCAL_KEK` | `keys.local.kek` |
| `VAULT_ADDR` | `keys.transit.address` |
| `VAULT_TOKEN` | `keys.transit.token` |
| `VAULT_CACERT` | `keys.transit.ca_cert` |
| `KEYS_TRANSIT_TOKEN_FILE` | `keys.transit.token_file` |
| `KEYS_TRANSIT_MOUNT` | `keys.transit.mount` |
| `KEYS_TRANSIT_KEY` | `keys.transit.key` |
| `KEYS_TRANSIT_TIMEOUT_SECONDS` | `keys.transit.timeout_seconds` |
| `SMTP_PASSWORD` | `notifications.email.password` |
| `KAVENEGAR_API_KEY` | `notifications.sms.kavenegar.api_key` |
| `WS_ALLOWED_ORIGINS` | `websocket.allowed_origins`, as a comma-separated list |
| `WS_PING_INTERVAL_SECONDS` | `websocket.ping_interval_seconds` |
| `WS_MAX_MESSAGE_SIZE_BYTES` | `websocket.max_message_size_bytes` |
| `RATE_LIMITS_TIMEOUT_MS` | `rate_limits.timeout_ms` |
| `RATE_LIMITS_REST_REQUESTS_PER_SECOND` | `rate_limits.rest.requests_per_second` |
| `RATE_LIMITS_REST_BURST` | `rate_limits.rest.burst` |
| `RATE_LIMITS_WS_MESSAGES_PER_SECOND` | `rate_limits.ws.messages_per_second` |
| `RATE_LIMITS_WS_BURST` | `rate_limits.ws.burst` |
| `RATE_LIMITS_TRADING_ORDERS_PER_SECOND` | `rate_limits.trading.orders_per_second` |
| `RATE_LIMITS_TRADING_BURST` | `rate_limits.trading.burst` |
| `RATE_LIMITS_AUTH_LOGIN_ATTEMPTS_PER_MINUTE` | `rate_limits.auth.login_attempts_per_minute` |
| `RATE_LIMITS_AUTH_OTP_REQUESTS_PER_HOUR` | `rate_limits.auth.otp_requests_per_hour` |
| `RATE_LIMITS_REST_ON_ERROR` | `rate_limits.rest.on_error` |
| `RATE_LIMITS_WS_ON_ERROR` | `rate_limits.ws.on_error` |
| `RATE_LIMITS_TRADING_ON_ERROR` | `rate_limits.trading.on_error` |
| `RATE_LIMITS_AUTH_ON_ERROR` | `rate_limits.auth.on_error` |
| `LOG_LEVEL` | `observability.logging.level` |
| `OTEL_ENDPOINT` | `observability.tracing.endpoint` |
| `BINANCE_API_KEY` | `api_key` of the `binance` provider in `price_aggregator` |
| `BINANCE_API_SECRET` | `api_secret` of the `binance` provider in `price_aggregator` |
| `FIAT_PROVIDER_API_KEY` | `api_key` of every provider in `fiat` |
| `FIAT_PROVIDER_API_SECRET` | `api_secret` of every provider in `fiat` |
| `CHAINALYSIS_API_KEY` | `api_key` of the `chainalysis` provider in `compliance.sanctions` |
| `TRM_API_KEY` | `api_key` of the `trm` provider in `compliance.sanctions` |
| `SUMSUB_API_KEY` | `api_key` of the `sumsub` provider in `compliance.kyc` |
| `ONFIDO_API_KEY` | `api_key` of the `onfido` provider in `compliance.kyc` |

## Secrets

- Never commit credentials or key material. `config/config.local.yaml`, `.env` files, and `internal/ssl/**/*.pem` are ignored by Git. `config/config.yaml` is ignored as well.
- Prefer environment variables, or files mounted from a secret manager, for passwords and keys.
- Generate separate key pairs for every environment, and rotate them if they are ever exposed.
- Use a separate `OTP_SECRET` for every environment, and rotate it if it is ever exposed.
- Give every environment its own KEK, and the server a Transit token that can only `encrypt` and `decrypt` with it, never read or delete it. See [`docs/data-protection.md`](data-protection.md).
