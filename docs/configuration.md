# Configuration

This document describes how the server is configured and which settings it currently uses.

## Loading order

1. **File.** The YAML file passed with `-config` is read. The default is `config/config.yaml`. If the file does not exist, the embedded copy of `config/config.example.yaml` is used instead.
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
| `exchanges.notifications` | none | Declared at startup. Not used yet. |
| `exchanges.order_events`, `exchanges.settlement` | none | Parsed, but not declared or used yet |

### `jwt`

| Key | Default | Description |
| --- | --- | --- |
| `ssl.user.private_key` | **required** | Path to the RSA private key PEM, as a list of path segments, for example `["internal", "ssl", "user", "private_key.pem"]` |
| `ssl.user.public_key` | **required** | Path to the RSA public key PEM, as a list of path segments |
| `ssl.admin.private_key`, `ssl.admin.public_key` | none | Parsed, but not loaded yet. Admin tokens are signed with the user key ([#17](https://github.com/mdastpak/Crypto-Exchange/issues/17)). |
| `issuer` | none | Issuer claim written into tokens |
| `access_token.expiration_minutes` | **required** | Access token lifetime. There is no default; `0` produces tokens that expire immediately. |
| `refresh_token.expiration_minutes` | **required** | Refresh token lifetime, and the cookie `Max-Age` |

### `otp`

| Key | Default | Description |
| --- | --- | --- |
| `auth.length` | **required** | Number of digits. There is no default. |
| `auth.ttl_seconds` | **required** | Time-to-live of an OTP session |
| `auth.retry_limit` | **required** | Failed verification attempts allowed per OTP session |
| `auth.name` | none | Parsed, but not used yet |

### `websocket`

| Key | Default | Description |
| --- | --- | --- |
| `allowed_origins` | `[]` | Origins accepted by the WebSocket origin check **and** by the HTTP CORS middleware. Entries are matched exactly, except `"*"`, which allows every origin in both checks; with CORS it also allows credentialed requests from any site. With an empty list, the WebSocket only accepts same-origin requests, but CORS currently reflects any origin ([#12](https://github.com/mdastpak/Crypto-Exchange/issues/12)). Always set this list to explicit origins, and never use `"*"`. |
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
