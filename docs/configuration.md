# Configuration

This document describes how the server is configured and which settings it currently uses.

## Loading order

1. **File.** The YAML file passed with `-config` is read. The default is `config/config.yaml`, which is not part of the repository. If the file does not exist, the embedded copy of [`config/config.example.yaml`](../config/config.example.yaml) is used instead, and the log says so. The `app` service of `docker-compose.yml` uses `config/config.compose.yaml`, which holds no secrets.
2. **Environment variables.** The variables listed in [Environment variables](#environment-variables) override the corresponding settings. A variable that is unset or empty leaves the setting unchanged.
3. **Defaults.** Numeric and string settings that are still zero or empty receive the defaults listed below.
4. **Validation.** The server stops with one message that lists every problem, one per line, each naming the setting and its environment variable, for example `rabbitmq.username (RABBITMQ_USER)`. A problem is any of the following:
   - a value in an environment variable that cannot be parsed, or a `server.mode` that is not a known mode;
   - a setting the server cannot start without, marked **required** below;
   - a value out of range, such as a port above 65535, a negative pool size, or an access token that lives longer than a day;
   - a TLS certificate and key that do not load;
   - in release mode, an insecure setting (see [Release mode](#release-mode)).

   Some problems are only logged as warnings outside release mode, such as an empty database password.

`${VAR}` placeholders in YAML files are **not** expanded. Keep secrets out of the file and set them with the environment variables.

## Release mode

With `server.mode: release` (`SERVER_MODE=release`), the server refuses to start with settings that are fine for development but not for production. It lists them all at once:

| Setting | Release mode needs |
| --- | --- |
| `server.tls.enabled`, `server.tls.behind_proxy` | TLS: either the server serves HTTPS itself, or `behind_proxy` says that a reverse proxy terminates TLS in front of it |
| `server.trusted_proxies` | Not empty when `server.tls.behind_proxy` is set, so that clients are not all seen with the proxy's address, also by the rate limits |
| `postgres.sslmode` | `require`, `verify-ca`, or `verify-full`. `disable`, `allow`, and `prefer` can talk to PostgreSQL without encryption. |
| `postgres.password` | Not empty, and not the default `postgres` |
| `postgres.migrations.mode` | `external`: the migrations are a step of the release, `admin migrate`, never part of a server's start. See [Database roles](#database-roles). |
| `postgres.migrations.password` | Empty: only the release step holds the migration role's password, never a server instance |
| `postgres.user` | Checked at startup, against the database: a role that cannot change the schema. It owns no table and not the schema, cannot create in the schema or the database, is not a superuser and has no `CREATEROLE`, `CREATEDB`, or `BYPASSRLS`, has no `TRUNCATE`, `REFERENCES`, or `TRIGGER` on any table, and can only read and insert into `audit_events`. |
| `redis.password` | Not empty |
| `rabbitmq.username`, `rabbitmq.password` | Not the default `guest` account |
| `redis.tls.enabled`, `rabbitmq.tls.enabled` | TLS to Redis and to RabbitMQ, or `tls.private_network: true`, which states that only a trusted private network, such as a service mesh that encrypts every hop, carries the connection. Sign-in codes and raw contacts cross RabbitMQ on their way to the delivery worker, and the Redis password and the token blacklist cross Redis, so neither may travel in the clear between hosts. |
| `server.cors.allowed_origins`, `websocket.allowed_origins` | No `"*"`, and only `https://` origins |
| `jwt.ssl.user.private_key`, `jwt.ssl.admin.private_key`, `server.tls.key_file` | Files that only their owner may write to (not checked on Windows). Read access is not checked, because container platforms mount secrets readable by every user of the container. |
| `otp.auth.secret` | Set, at least 32 bytes |
| `otp.auth.dev_log_codes` | `false` |
| `keys.provider` | Not `local`, and a Transit address with `https` |
| `identity.mobile_ownership.provider` | Not `stub`, which matches every mobile number |

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
| `mode` | `debug` | `debug`, `release`, or `test`. Sets Gin's mode, and `release` turns on the [release checks](#release-mode). Any other value stops the server. `SERVER_MODE` overrides it, and so does `GIN_MODE`, its older name. |
| `trusted_proxies` | `[]` | IP addresses or CIDR ranges of reverse proxies whose `X-Forwarded-For` and `X-Real-IP` headers are trusted. With an empty list the client address is the TCP peer address, and forwarding headers are ignored. Behind a load balancer or ingress, list only its addresses; otherwise clients can spoof their address and bypass IP-based rate limits. An invalid entry stops the server at startup. |
| `swagger.enabled` | unset | Serves the Swagger UI at `/swagger/index.html`. When it is not set, the UI is served outside release mode only. |
| `max_body_bytes` | `1048576` | The largest request body accepted, in bytes. A larger body gets `413`. |
| `shutdown_timeout_seconds` | `30` | How long a shutdown may take in total, from `SIGINT` or `SIGTERM` until the connections to PostgreSQL, Redis, and RabbitMQ are closed. It includes `drain_seconds`. The orchestrator must wait longer before it kills the process, or the shutdown is cut short: Docker's `stop_grace_period` is 10 seconds by default, and `docker-compose.yml` sets it to 40; Kubernetes' `terminationGracePeriodSeconds` is 30 by default. See [Shutdown](architecture.md#startup-and-shutdown). |
| `drain_seconds` | `0` | How long the instance keeps serving after `/system/ready` turns `503` at shutdown, so that load balancers stop sending it requests before it stops accepting them. Set it to a little more than the readiness interval of the load balancer. It must be less than `shutdown_timeout_seconds`. |
| `tls.enabled` | `false` | Serve HTTPS with `ListenAndServeTLS` |
| `tls.cert_file`, `tls.key_file` | none | Certificate and key paths, used when TLS is enabled. The server checks at startup that they load as a pair. |
| `tls.behind_proxy` | `false` | A reverse proxy terminates TLS in front of the server, which then serves plain HTTP to it. Release mode needs TLS one way or the other. |
| `hsts.enabled` | unset | Sends `Strict-Transport-Security` on every response. When it is not set, the header is sent when `tls.enabled` or `tls.behind_proxy` is true. Browsers ignore it on plain HTTP. |
| `hsts.max_age_seconds` | `31536000` | How long browsers remember to use HTTPS only, one year by default |
| `hsts.include_subdomains` | `false` | Adds `includeSubDomains`. Turn it on only when every subdomain serves HTTPS. |
| `cors.allowed_origins` | `[]` | Browser origins (`scheme://host[:port]`, for example `https://app.example.com` or `http://localhost:3000`) that may call the API across origins with credentials. Matching is exact and case-insensitive. With an empty list, no cross-origin access is granted. A wildcard (`"*"`), a path, or any other malformed entry stops the server at startup, because a wildcard combined with credentials would let any site read authenticated responses. |

### `postgres`

| Key | Default | Description |
| --- | --- | --- |
| `host`, `port`, `user`, `dbname` | **required** | Connection settings. `hosts` can replace `host` and `port`. |
| `password` | none | Database password. Set it with `POSTGRES_PASSWORD`. Any character works, spaces, quotes, backslashes, and `=` included, and so in `user` and `dbname`: the values are escaped, never formatted into a connection string ([#229](https://github.com/mdastpak/Crypto-Exchange/issues/229)). |
| `sslmode` | `disable` | libpq SSL mode. Release mode needs `require`, `verify-ca`, or `verify-full`; use `verify-full` where possible. |
| `max_open_conns` | `25` | Maximum open connections |
| `max_idle_conns` | `5` | Maximum idle connections |
| `conn_max_lifetime_minutes` | `5` | Maximum connection lifetime |
| `conn_max_idle_time_minutes` | `5` | Maximum idle time per connection |
| `connect_retries` | `3` | Startup connection attempts |
| `connect_retry_delay_ms` | `500` | Delay between startup attempts |
| `hosts` | `[]` | The nodes of a high-availability cluster, as `host:port`, in place of `host` and `port` ([#230](https://github.com/mdastpak/Crypto-Exchange/issues/230)). Each new connection tries them in order and takes the first that `target_session_attrs` accepts. `POSTGRES_HOSTS` takes them comma-separated. Set `host` or `hosts`, not both. |
| `target_session_attrs` | `read-write` | Which node a connection accepts: `any`, `read-write`, `read-only`, `primary`, `standby`, or `prefer-standby`, as in libpq. With `read-write` or `primary`, the pool also drops a connection whose node turns read-only, such as a demoted primary, before its next use, so it reconnects to the new primary without a restart. |
| `statement_timeout_ms` | `30000` | The longest a statement of the server may run. `-1` turns it off. |
| `lock_timeout_ms` | `5000` | The longest a statement waits for a lock. `-1` turns it off. A request whose statement hits this or `statement_timeout_ms` gets `503 database_busy` with `Retry-After: 1`, not a request that hangs. |
| `idle_in_transaction_timeout_ms` | `60000` | Ends a session that leaves a transaction open and idle this long, which would hold its locks. `-1` turns it off. |
| `migration_lock_timeout_seconds` | `300` | The longest an instance waits at startup for the migration lock, which another instance holds while it migrates. It logs the holder every 10 seconds, and then stops with an error that names it (process ID, `application_name`, client, and for how long). |
| `migrations.mode` | `startup` | When the migrations run ([#180](https://github.com/mdastpak/Crypto-Exchange/issues/180)). `startup` migrates at the start of every instance, under the migration lock. `external` never migrates in the server: `admin migrate` does, as a step of the release, and an instance refuses to start while the database misses migrations of its build, or holds a failed one. Release mode needs `external`. |
| `migrations.user`, `migrations.password` | none | The migration role, which owns the schema. Empty, the migrations use `user` and `password`, which then own everything; that suits development, and release mode refuses it. Set the password with `POSTGRES_MIGRATIONS_PASSWORD`, only where `admin migrate` runs. |

Migrations run while a PostgreSQL advisory lock is held, on connections of their own, without the statement and lock timeouts, which would stop a long migration: at the start of every instance in the `startup` mode, and in `admin migrate` in the `external` mode.

Every connection names itself in `pg_stat_activity` with `application_name`: `crypto-exchange-server/<host>` for a server instance, `crypto-exchange-admin` for `cmd/admin`, and `crypto-exchange-migrate` for the migrations.

#### Database roles

The server and the migrations use two roles ([#180](https://github.com/mdastpak/Crypto-Exchange/issues/180)), so that neither the server nor anyone who obtains its credentials can change the schema: drop the triggers that keep the audit log append-only, alter a constraint, or drop a table.

| Role | Setting | What it may do |
| --- | --- | --- |
| Migration role | `postgres.migrations.user` | Owns the database and its `public` schema, and creates every object in it. Used only by `admin migrate`. |
| Runtime role | `postgres.user` | Owns nothing. `SELECT`, `INSERT`, `UPDATE`, and `DELETE` on the tables, only `SELECT` and `INSERT` on `audit_events`, only `SELECT` on `schema_migrations`, and `USAGE` on the schema and the sequences. No `CREATE`, `TRUNCATE`, `REFERENCES`, or `TRIGGER`. |

Create the roles once per environment, as a superuser, with passwords of their own:

```sql
CREATE ROLE exchange_owner LOGIN PASSWORD '...';
CREATE ROLE exchange_app LOGIN PASSWORD '...';
CREATE DATABASE exchange OWNER exchange_owner;
REVOKE ALL ON DATABASE exchange FROM PUBLIC;
GRANT CONNECT, TEMPORARY ON DATABASE exchange TO exchange_app;
-- In the database exchange; PostgreSQL 15 and later already do this.
REVOKE CREATE ON SCHEMA public FROM PUBLIC;
```

On a database that one role owns already, transfer it: `ALTER DATABASE exchange OWNER TO exchange_owner`, and, in the database, `REASSIGN OWNED BY <old role> TO exchange_owner`, then make the old role the runtime role or drop it.

The migrations grant the runtime role its privileges after every run, and take back any others, so a table that a migration adds is covered, and a privilege granted by hand does not last past the next release. `admin migrate status` shows the schema version and anything the runtime role could change.

**Removal marks.** Three tables record a removal with a mark, so that the runtime role will need `DELETE` on two tables only, `outbox_events` and `user_favorites` ([#248](https://github.com/mdastpak/Crypto-Exchange/issues/248)): a contact that its user removed (`user_contacts.removed_at`), a previous symbol that an administrator released (`currency_symbols.released_at` and `released_by`), and a capability override that an administrator cleared (`currency_capability_overrides.cleared_at` and `cleared_by`). A marked row stays for the record, and no reader sees it: it does not sign in, is not listed, and does not apply; the unique indexes ignore it, so its contact or symbol can be taken again. Migration 33 adds the marks, and this release reads them but still deletes the rows, because the instances of the release before it, which run while it rolls out, do not know them; the release after it writes them, and takes `DELETE` from the runtime role.

**Releases with several instances.** In the `external` mode, migrating is a step of the release: run `admin migrate` once, as a job or an init container with the migration role's credentials, and start the new instances when it has succeeded. Running it twice, or from two places at once, is safe: the migration lock lets one migrate, and the other finds nothing to do. Instances check the version at startup and refuse a database that is behind their build. A database ahead of their build is accepted, with a warning, so the instances of the previous release keep running, and can restart, while the release rolls out; so each migration must keep working for the release before it (expand first, contract in a later release). The Docker Compose setup does the same: its `migrate` service runs `admin migrate`, and the `app` service starts once it has completed.

#### High availability

Run PostgreSQL as a primary with at least one synchronous standby, managed by Patroni, repmgr, or a managed service, and list every node in `hosts`:

```yaml
postgres:
  hosts: ["pg-1.internal:5432", "pg-2.internal:5432", "pg-3.internal:5432"]
  target_session_attrs: "read-write"
```

After a failover, new connections skip the nodes that only read and find the new primary. Connections to the old primary end when it stops, or are dropped before their next use when it stays up read-only. A request that meets the change gets `503 database_busy` and may retry. A floating address or a DNS name that follows the primary works too, with the same settings. Read replicas and PgBouncer for scale are [#169](https://github.com/mdastpak/Crypto-Exchange/issues/169).

If a migration fails, golang-migrate marks its version as dirty and the server refuses to start until the flag is cleared. Fix the cause named in the error message, then reset the version to the last successful migration with the [golang-migrate CLI](https://github.com/golang-migrate/migrate/tree/master/cmd/migrate) and start the server again:

```bash
migrate -path internal/db/migrations \
  -database "postgres://MIGRATION_USER:PASSWORD@HOST:5432/exchange?sslmode=disable" \
  force 10
```

Replace `10` with the version before the failed migration, and connect as the migration role.

### `redis`

| Key | Default | Description |
| --- | --- | --- |
| `mode` | `standalone` | How the server reaches Redis (`REDIS_MODE`): `standalone` connects to `host` and `port`; `sentinel` asks the sentinels for the current primary and follows its failover; `cluster` connects to a Redis Cluster. See [Redis high availability](redis-high-availability.md). |
| `host`, `port` | **required** in the `standalone` mode | The one node of the `standalone` mode |
| `sentinel.master_name` | **required** in the `sentinel` mode | The name under which the sentinels monitor the primary (`REDIS_SENTINEL_MASTER_NAME`) |
| `sentinel.addresses` | **required** in the `sentinel` mode | The sentinels, as `host:port` entries (`REDIS_SENTINEL_ADDRESSES`, comma-separated). List all of them, usually three. |
| `sentinel.username`, `sentinel.password` | none | The sentinels' own credentials, when they require any (`REDIS_SENTINEL_USERNAME`, `REDIS_SENTINEL_PASSWORD`). The data nodes use `username` and `password`. |
| `cluster.addresses` | **required** in the `cluster` mode | Some nodes of the cluster, as `host:port` entries (`REDIS_CLUSTER_ADDRESSES`, comma-separated); the client learns the others from them. The cluster mode needs `db` 0. |
| `durable_writes.replicas` | `0` | How many replicas must hold a security counter before the server relies on it (`REDIS_DURABLE_WRITES_REPLICAS`): the OTP attempts and code requests, and the blacklist of logged-out tokens. Each such write is followed by `WAIT`; when fewer replicas acknowledge it in time, the request is answered with `503 database_busy` and `Retry-After`, and the code is not checked. `0` does not wait. Set it to 1 with a primary and two replicas, so that one replica may be down. |
| `durable_writes.timeout_ms` | `250` | How long such a write waits for its replicas (`REDIS_DURABLE_WRITES_TIMEOUT_MS`) |
| `username` | none | Redis ACL user (`REDIS_USERNAME`). Empty is the `default` user. Give the server a user of its own, limited to the commands and keys it needs. |
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
| `currency_cache_ttl_seconds` | `300` | How long the cached list of public currencies is fresh. A change through the admin routes drops it at once, and the next read loads it from PostgreSQL. After this time, one instance refreshes the list while every instance keeps serving it, so this time bounds how long the list can stay out of date when a drop fails or a currency is changed directly in the database. The key expires after twice the time if nothing reads it. See [The shared currency list](architecture.md#the-shared-currency-list). |

Redis is on the path of most requests, so these timeouts are short: while Redis cannot be reached, a request that needs it fails within a few seconds instead of waiting for the much longer defaults of the client library. The rate limiters give up even sooner, after `rate_limits.timeout_ms`.

#### `tls` of `redis` and `rabbitmq`

TLS for the connection to Redis (`redis.tls`, variables `REDIS_TLS_*`) and to RabbitMQ (`rabbitmq.tls`, variables `RABBITMQ_TLS_*`, which makes the server dial `amqps://`). The certificate of the server is always verified; there is no setting that skips it. A setting that cannot be used, such as a CA file that does not exist, stops the server at startup.

| Key | Default | Description |
| --- | --- | --- |
| `enabled` | `false` | Connect with TLS 1.2 or later (`REDIS_TLS_ENABLED`, `RABBITMQ_TLS_ENABLED`) |
| `ca_file` | none | PEM file with the certificate authorities of a private CA (`*_TLS_CA_FILE`). Without it, the system's authorities are used. |
| `server_name` | the host | The name the certificate must hold, when it differs from `host`, for example when `host` is an IP address (`*_TLS_SERVER_NAME`) |
| `cert_file`, `key_file` | none | A client certificate for mutual TLS (`*_TLS_CERT_FILE`, `*_TLS_KEY_FILE`). Set both or neither. |
| `private_network` | `false` | In release mode, accept a connection without TLS, because only a trusted private network carries it (`*_TLS_PRIVATE_NETWORK`) |

Every server instance connects with its own TLS session, so several instances behind a load balancer need nothing more than the same files.

### `rabbitmq`

| Key | Default | Description |
| --- | --- | --- |
| `host`, `port`, `username`, `password` | **required** | Connection settings. `hosts` replaces `host` and `port`. The server connects to `amqp://username:password@host:port/vhost`, with the user, the password, and the virtual host percent-encoded, so a password may hold any character, such as `@`, `/`, or `%`. Set the credentials with `RABBITMQ_USER` and `RABBITMQ_PASSWORD`. The URI holds the password and is never logged. |
| `hosts` | none | The nodes of a RabbitMQ cluster, as `host:port` entries (`RABBITMQ_HOSTS`, comma-separated), instead of `host` and `port`. The server dials them in random order, so that the instances spread over the cluster, and moves to another node when its node goes away. Every node uses the same credentials, virtual host, and `tls` settings; with TLS, each connection checks the name of the node it dials unless `tls.server_name` is set. See [RabbitMQ high availability](rabbitmq-high-availability.md). |
| `vhost` | `/` | Virtual host (`RABBITMQ_VHOST`). On a broker shared by several services, give each service a virtual host of its own. |
| `exchanges.price_updates` | none | Fan-out exchange used for currency change events |
| `exchanges.trade_events` | none | Declared at startup. Not used yet. |
| `exchanges.notifications` | none | Fan-out exchange for sign-in code jobs. The delivery worker consumes it through the `notifications.otp.quorum` queue (see [`notifications`](#notifications)). |
| `exchanges.order_events`, `exchanges.settlement` | none | Parsed, but not declared or used yet |

### `jwt`

| Key | Default | Description |
| --- | --- | --- |
| `ssl.user.private_key` | **required** | Path to the RSA private key PEM that signs user tokens, for example `"internal/ssl/user/private_key.pem"`. A relative path starts at the working directory. A list of path segments, which earlier versions required, is still accepted. |
| `ssl.user.public_key` | **required** | Path to the matching public key PEM |
| `ssl.user.retired_public_keys` | `[]` | Paths of the public keys of earlier user key pairs. Tokens they signed stay valid until they expire. See [Rotating signing keys](#rotating-signing-keys). |
| `ssl.user.next_public_keys` | `[]` | Paths of the public keys of the user key pair that will sign next. Tokens it signs are accepted before any instance signs with it, the first phase of a [rotation](#rotating-signing-keys) ([#233](https://github.com/mdastpak/Crypto-Exchange/issues/233)). |
| `ssl.admin.private_key`, `ssl.admin.public_key` | none | The key pair that signs admin tokens. Without it, admin routes reject every token, and the server logs a warning. It must differ from the user key pair. |
| `ssl.admin.retired_public_keys` | `[]` | Public keys of earlier admin key pairs |
| `ssl.admin.next_public_keys` | `[]` | Public keys of the admin key pair that will sign next |
| `issuer` | `crypto-exchange` | Issuer claim written into tokens and required on every token |
| `access_token.expiration_minutes` | **required** | Access token lifetime, at most 1440 (a day). There is no default. |
| `refresh_token.expiration_minutes` | **required** | Refresh token lifetime, and the cookie `Max-Age`. It must be at least the access token lifetime. |
| `sessions.max_lifetime_minutes` | `43200` | The absolute lifetime of a session, 30 days by default ([#144](https://github.com/mdastpak/Crypto-Exchange/issues/144)). A refresh never extends a session past its sign-in plus this; then the refresh answers `401` and the session's access tokens are refused, and the user signs in again. At least `refresh_token.expiration_minutes`, or the server does not start. Every instance checks the age of a session, so a lower value applies to the existing sessions at once. |
| `sessions.max_per_user` | `5` | From 1 to 50. The most active sessions one user may have. A sign-in past it is refused with `409 session_limit_reached` until the user ends a session; see [Session limit](api.md#session-limit). |
| `refresh_token.reuse_grace_seconds` | `10` | From 1 to 60. How long after a refresh the refresh token that it replaced still gets a new access token, for tabs that refresh at the same moment, instead of revoking the session as a reuse. See [`POST /public/auth/refresh`](api.md#post-publicauthrefresh). |

Keys must be RSA keys of at least 2048 bits, and a public key must belong to its private key. Tokens name their signing key in the `kid` header, which is derived from a SHA-256 hash of the public key, so key IDs need no configuration. The server refuses to start if a key is invalid or shared between the roles.

#### Rotating signing keys

A rotation has two phases, each a rolling restart, so that no instance ever rejects a token that another instance signed ([#233](https://github.com/mdastpak/Crypto-Exchange/issues/233)):

1. Generate a new key pair for the role.
2. **Phase 1: accept the new key everywhere.** Add the new public key to `next_public_keys`, and restart every instance. Instances still sign with the old key; restarted ones also accept the new key, which nobody signs with yet.
3. **Phase 2: sign with the new key.** Once every instance runs phase 1, point `private_key` and `public_key` at the new pair, move the old public key to `retired_public_keys`, empty `next_public_keys`, and restart every instance again. Restarted instances sign with the new key, which the others already accept; every instance still accepts the old key.
4. After `refresh_token.expiration_minutes` (or `access_token.expiration_minutes` for admin keys, which have no refresh tokens), remove the old key from `retired_public_keys`.

Skipping phase 1 signs users out at random during the restart: an instance that has not restarted rejects every token that a restarted one signed.

### `otp`

| Key | Default | Description |
| --- | --- | --- |
| `auth.length` | `6` | Number of digits, from 4 to 10 |
| `auth.ttl_seconds` | `60` | Lifetime of a code and of its sign-in session |
| `auth.retry_limit` | `3` | Verification attempts per code. After the last failed attempt, the code is erased and further attempts are refused until the session expires. |
| `auth.secret` | **required** in release mode | Key for the HMAC of each code and the encryption of contacts in Redis, so that reading Redis reveals neither. At least 32 bytes, for example from `openssl rand -base64 48`, and the same on every server instance. `OTP_SECRET` overrides it. Changing it only invalidates the codes that are pending. Outside release mode, an empty secret is replaced by a random one at startup, which works only with a single server process. |
| `auth.contact_requests_per_hour` | `5` | Codes issued to one email address or mobile number per hour, for one client. A client is its address, with IPv6 grouped by `rate_limits.ipv6_prefix_length`. |
| `auth.contact_attempts_per_hour` | `10` | Verification attempts for one contact per hour, across all of its codes, from the client that requested them, so that requesting a new code does not bring new attempts. A successful sign-in resets the count. It must be at least `retry_limit`. Because the count is per client, a stranger who knows a contact and fails its codes locks out only the stranger's own client, not the contact's owner ([#187](https://github.com/mdastpak/Crypto-Exchange/issues/187)). |
| `auth.contact_requests_total_per_hour` | `20` | Codes issued to one contact per hour from every client together, which bounds the codes, and the SMS, that strangers can send to it. A client that signed in with the contact in the last 30 days is exempt, so strangers who use up the total do not lock out its owner on the owner's usual network. At least `contact_requests_per_hour`; the default is never below it. |
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
| `notifications.otp.quorum` | Quorum queue bound to the notifications exchange | Pending sign-in code jobs. The queue is replicated to a majority of the nodes of a cluster, so it keeps working, and keeps its jobs, while any one of three nodes is down. Jobs expire with the code (`otp.auth.ttl_seconds`), so a code that waits too long is dropped by the broker. The queue hands out a job at most ten times, so that a job that crashes its worker cannot circle forever. Several server instances share the queue; a job that the broker hands out again, because its worker stopped before acknowledging it, is not sent twice (the worker records delivered jobs in Redis until shortly after they expire). |
| `notifications.dead-letter` | Durable fan-out exchange | Receives a record of every code that could not be delivered |
| `notifications.otp.dead-letter.quorum` | Quorum queue bound to `notifications.dead-letter` | Keeps those records for seven days. A record holds the contact type, the masked contact, the reason, the number of attempts, and the last error, but never the code or the raw contact. Its depth is a direct measure of delivery failures. |

Versions before [#232](https://github.com/mdastpak/Crypto-Exchange/issues/232) used the classic queues `notifications.otp` and `notifications.otp.dead-letter`. A queue cannot change its type in place, so the quorum queues have new names. The worker unbinds `notifications.otp` from the exchange, delivers the jobs left in it, and deletes it once no worker of an earlier version consumes it; it looks again every 30 seconds until then. Delete `notifications.otp.dead-letter` by hand once its records have expired, seven days after the upgrade.

Failed attempts and abandoned codes are also logged with the masked contact. Delivery metrics are part of [#50](https://github.com/mdastpak/Crypto-Exchange/issues/50).

### `admin`

| Key | Default | Description |
| --- | --- | --- |
| `totp_key` | none | Encrypts the authenticator (TOTP) secrets of administrators in the database with AES-256-GCM: 32 random bytes, base64-encoded, for example from `openssl rand -base64 32`. `ADMIN_TOTP_KEY` overrides it. Without it, admin sign-in is disabled. The server and the `cmd/admin` command must use the same key. If it changes, every administrator must be enrolled again with `reset-totp`. |
| `sign_in_min_response_ms` | `400` | The least time, from 1 to 5000 ms, that an answer to `POST /admin/auth` takes once the address is valid, so that its duration does not tell an administrator's address from an unknown one. It must be longer than the work for an administrator, which includes the confirm of the broker for the email; check the latency of that route in production and raise the setting if it comes close. |

Authenticator apps show `exchange.name` as the issuer of the account. Each encrypted secret is bound to its administrator's ID, so it cannot be copied to another user row.

Writing to the database is not enough to create an administrator: changing `users.role` to `admin` does not enable admin sign-in without a secret sealed with this key. Someone with both database access and this key could, so keep it in a secret manager, apart from database credentials.

### `contacts`

| Key | Default | Description |
| --- | --- | --- |
| `new_contact_cooldown_hours` | `72` | From 1 to 720. How long a newly added or replaced email or mobile cannot prove the replacement or removal of an older contact of the account ([#250](https://github.com/mdastpak/Crypto-Exchange/issues/250)). Such a change is refused with `403 contact_too_new` until then; a contact can always be replaced or removed with its own code. See [Contacts](api.md#contacts). Every instance compares with the database clock, so they all decide the same way. |

### `security`

| Key | Default | Description |
| --- | --- | --- |
| `change_hold_hours` | `24` | From 1 to 720. How long the operations that a second factor protects are held after a security change of a user: adding, replacing, or removing an email address or mobile number, and later a change of the second factor ([#251](https://github.com/mdastpak/Crypto-Exchange/issues/251)). Withdrawals then answer `403 security_hold` with `info.until`. A change extends the hold and never shortens it. Contact changes are not held. The hold is stored in the user's row, so it applies on every instance at once; a new value applies to holds that start after it. See [Security hold](api.md#security-hold). |

### `identity`

The mobile ownership check ([#253](https://github.com/mdastpak/Crypto-Exchange/issues/253)) asks a KYC provider whether an Iranian mobile number belongs to a national ID, as the Shahkar service of the Communications Regulatory Authority answers it. See [Mobile ownership](api.md#mobile-ownership).

| Key | Default | Description |
| --- | --- | --- |
| `mobile_ownership.provider` | `none` | `none`, `stub`, or `jibit`. With `none`, a user who has a verified national ID cannot add or replace a mobile number (`503 identity_unavailable`), since it cannot be checked; users without one are not affected. `stub` matches every number, for development; release mode refuses it. |
| `mobile_ownership.endpoint` | `https://napi.jibit.ir/ide` for `jibit` | The base URL of the provider's API, `https` only. |
| `mobile_ownership.api_key`, `mobile_ownership.secret_key` | none | The provider's credentials, required for `jibit`. Set them with `IDENTITY_MOBILE_OWNERSHIP_API_KEY` and `IDENTITY_MOBILE_OWNERSHIP_SECRET_KEY`. |
| `mobile_ownership.timeout_seconds` | `10` | From 1 to 60. How long one check may take, with its retries. A check that does not finish changes nothing and answers `503 identity_unavailable`. |
| `mobile_ownership.recheck_days` | `30` | From 1 to 365. How long a successful check of a number counts before a sign-in with it checks it again ([#252](https://github.com/mdastpak/Crypto-Exchange/issues/252)). |

### `favorites`

| Key | Default | Description |
| --- | --- | --- |
| `max_per_user` | `100` | From 1 to 1000. The most favorite currencies and pairs that one user may have, together. An add past it is refused with `409 favorites_limit_reached`; see [Favorites](api.md#favorites). |

### `delisting`

| Key | Default | Description |
| --- | --- | --- |
| `interval_seconds` | `30` | From 1 to 3600. How often the scheduler of each instance applies the steps of [delistings](api.md#delistings) and the phases of [currency migrations](api.md#currency-migrations) that are due, so a step applies at most this late. Every instance runs it; each step is applied once. |

### `platform`

The [maintenance and lockdown controls](api.md#maintenance-and-lockdowns) ([#235](https://github.com/mdastpak/Crypto-Exchange/issues/235)). The controls themselves are in the database, created through the admin API or `cmd/admin platform`; these settings say how each instance applies them.

| Key | Default | Description |
| --- | --- | --- |
| `poll_interval_ms` | `1000` | From 100 to 60000. How often each instance reads the version of the controls, and the controls when it changed. A change made on any instance, or with `cmd/admin`, applies on every instance within this interval; the instance that made it applies it at once. A control that starts or ends on schedule applies at its time, without a read. |
| `stale_after_seconds` | `30` | From 1 to 3600, and longer than `poll_interval_ms`. How long an instance keeps applying the last controls it read when it cannot read them, for example while PostgreSQL fails over. After it, the instance also closes the features of `fail_closed`, until a read succeeds. |
| `fail_closed` | `[deposits, withdrawals, trading]` | The features that an instance closes while it does not know the controls: after `stale_after_seconds` without a read, and from its start until its first read. Refusals answer `503 feature_locked` with `state_unknown`. `["none"]` closes nothing. The other routes stay open. |
| `lift_approval` | `[withdrawals, trading]` | The features whose active lockdown an administrator lifts only with a second one: the first request answers `202`, and another administrator's request lifts it. `["none"]` needs no second administrator. `cmd/admin`, which needs access to the database, lifts at once. |

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
| `reload_interval_seconds` | `60` | From 5 to 3600. How often each instance checks `data_keys`, and loads a rotation, a retirement, or a re-wrap without a restart ([#233](https://github.com/mdastpak/Crypto-Exchange/issues/233)). An instance that meets a value sealed with a version it does not hold also reloads at once, at most every 5 seconds. |
| `activation_delay_seconds` | `300` | How long after `admin keys rotate` the new version starts writing. Until then every instance only reads with it. At least twice `reload_interval_seconds`, so that every instance has loaded a version before any writes with it. `admin keys rotate` reads it from the configuration it is given. |

The key service is contacted at startup, when `data_keys` changed, and by the `admin keys` commands. A running server keeps the keys in memory: when a reload fails, because the key service is unreachable, it keeps the keys it has and logs the failure, so it does not depend on the key service between changes.

### `websocket`

| Key | Default | Description |
| --- | --- | --- |
| `allowed_origins` | `[]` | Origins accepted by the WebSocket origin check. HTTP CORS uses `server.cors.allowed_origins` instead. Entries are matched exactly, except `"*"`, which allows every origin ([#47](https://github.com/mdastpak/Crypto-Exchange/issues/47)). With an empty list, only same-origin connections are accepted. Release mode refuses `"*"` and origins that are not `https://`. |
| `ping_interval_seconds` | `30` | How often the server pings each client. A client that does not answer within two intervals is disconnected. |
| `max_message_size_bytes` | `4096` | The largest message accepted from a client, at most 1 MiB. A larger one closes the connection. Clients only send small control messages. |
| `write_timeout_seconds` | `10` | How long a write to a client may take |
| `client_messages_per_second`, `client_message_burst` | `10`, `20` | The messages each connection may send, as a token bucket. A client that sends faster is disconnected. `rate_limits.ws` limits the upgrade requests per address instead. |
| `max_connections` | `10000` | Open connections **per server instance**. Further upgrades get `503`. |
| `max_connections_per_ip` | `20` | Open connections per client address **over all instances**, at most `max_connections`. Further upgrades get `429` (`rate_limited`). The client address honors `server.trusted_proxies`, and IPv6 clients count by `rate_limits.ipv6_prefix_length`. |
| `max_connections_per_user` | `10` | Open connections per signed-in user **over all instances and addresses** ([#234](https://github.com/mdastpak/Crypto-Exchange/issues/234)). Further upgrades get `429` (`too_many_connections`), and a `reauth` that signs in an anonymous connection closes it with `4429`. Anonymous connections count only against their address. |

`max_connections_per_ip` and `max_connections_per_user` are counted in Redis, so they hold however many instances serve the gateway. Each connection holds a place under its address and its user, which its instance refreshes every 20 seconds and which expires a minute after the last refresh, so the places of an instance that was killed without shutting down are freed within a minute. While Redis cannot be reached, each instance counts only its own connections against the same limits, as the rate limiters fall back to a local decision.
| `session_check_seconds` | `60` | How often each instance checks the sessions of its authenticated connections, and closes those whose session ended or whose user can no longer sign in. A revoked session can stay connected for up to this long. Each check reads the database once per session, so a shorter interval costs more queries. |

A connection may bring a user access token, which gets the checks of the `/user/*` routes; see [WebSocket](api.md#websocket).

### `rate_limits`

| Key | Default | Applies to |
| --- | --- | --- |
| `ipv6_prefix_length` | `64` | Every per-client limit, and `websocket.max_connections_per_ip`: an IPv6 client is counted by this prefix of its address, from 32 to 128, because one subscriber holds at least a /64 and could otherwise use a fresh address, and get a fresh budget, for every request. IPv4 addresses, and IPv4-mapped IPv6 addresses, count as the IPv4 address. Logs and the audit log keep the full address. Set it with `RATE_LIMITS_IPV6_PREFIX_LENGTH`. |
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

Clients behind one address, such as a carrier-grade NAT or an office network, share these budgets, and so do the IPv6 clients of one prefix. Size `auth.otp_requests_per_hour` for the busiest shared address you expect. Raising it does not let anyone flood a single email address or mobile number, because `otp.auth.contact_requests_per_hour` limits the codes sent to each contact.

## Settings reserved for roadmap features

The following sections are parsed, and some receive defaults, but no code uses them yet. They document the intended configuration surface of the [roadmap](https://github.com/mdastpak/Crypto-Exchange/issues/85).

| Section | Purpose | Related issues |
| --- | --- | --- |
| `exchange` | Display name and time zone. The maintenance window that was reserved here is replaced by the scheduled [maintenance controls](api.md#maintenance-and-lockdowns) ([#235](https://github.com/mdastpak/Crypto-Exchange/issues/235)); `exchange.maintenance_window` is no longer read. | none |
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
| `SERVER_TLS_BEHIND_PROXY` | `server.tls.behind_proxy` |
| `SERVER_HSTS_ENABLED` | `server.hsts.enabled` |
| `SERVER_HSTS_MAX_AGE_SECONDS` | `server.hsts.max_age_seconds` |
| `SERVER_HSTS_INCLUDE_SUBDOMAINS` | `server.hsts.include_subdomains` |
| `SERVER_SWAGGER_ENABLED` | `server.swagger.enabled` |
| `SERVER_MAX_BODY_BYTES` | `server.max_body_bytes` |
| `SERVER_SHUTDOWN_TIMEOUT_SECONDS` | `server.shutdown_timeout_seconds` |
| `SERVER_DRAIN_SECONDS` | `server.drain_seconds` |
| `POSTGRES_HOST` | `postgres.host` |
| `POSTGRES_PORT` | `postgres.port` |
| `POSTGRES_USER` | `postgres.user` |
| `POSTGRES_PASSWORD` | `postgres.password` |
| `POSTGRES_DB` | `postgres.dbname` |
| `POSTGRES_SSLMODE` | `postgres.sslmode` |
| `POSTGRES_HOSTS` | `postgres.hosts`, as a comma-separated list |
| `POSTGRES_TARGET_SESSION_ATTRS` | `postgres.target_session_attrs` |
| `POSTGRES_STATEMENT_TIMEOUT_MS` | `postgres.statement_timeout_ms` |
| `POSTGRES_LOCK_TIMEOUT_MS` | `postgres.lock_timeout_ms` |
| `POSTGRES_IDLE_IN_TRANSACTION_TIMEOUT_MS` | `postgres.idle_in_transaction_timeout_ms` |
| `POSTGRES_MIGRATION_LOCK_TIMEOUT_SECONDS` | `postgres.migration_lock_timeout_seconds` |
| `POSTGRES_MIGRATIONS_MODE` | `postgres.migrations.mode` |
| `POSTGRES_MIGRATIONS_USER` | `postgres.migrations.user` |
| `POSTGRES_MIGRATIONS_PASSWORD` | `postgres.migrations.password` |
| `POSTGRES_MAX_OPEN_CONNS` | `postgres.max_open_conns` |
| `POSTGRES_MAX_IDLE_CONNS` | `postgres.max_idle_conns` |
| `POSTGRES_CONN_MAX_LIFETIME_MINUTES` | `postgres.conn_max_lifetime_minutes` |
| `POSTGRES_CONN_MAX_IDLE_TIME_MINUTES` | `postgres.conn_max_idle_time_minutes` |
| `REDIS_MODE` | `redis.mode` |
| `REDIS_HOST` | `redis.host` |
| `REDIS_PORT` | `redis.port` |
| `REDIS_USERNAME` | `redis.username` |
| `REDIS_PASSWORD` | `redis.password` |
| `REDIS_SENTINEL_MASTER_NAME` | `redis.sentinel.master_name` |
| `REDIS_SENTINEL_ADDRESSES` | `redis.sentinel.addresses` |
| `REDIS_SENTINEL_USERNAME` | `redis.sentinel.username` |
| `REDIS_SENTINEL_PASSWORD` | `redis.sentinel.password` |
| `REDIS_CLUSTER_ADDRESSES` | `redis.cluster.addresses` |
| `REDIS_DURABLE_WRITES_REPLICAS` | `redis.durable_writes.replicas` |
| `REDIS_DURABLE_WRITES_TIMEOUT_MS` | `redis.durable_writes.timeout_ms` |
| `REDIS_TLS_ENABLED` | `redis.tls.enabled` |
| `REDIS_TLS_CA_FILE` | `redis.tls.ca_file` |
| `REDIS_TLS_SERVER_NAME` | `redis.tls.server_name` |
| `REDIS_TLS_CERT_FILE` | `redis.tls.cert_file` |
| `REDIS_TLS_KEY_FILE` | `redis.tls.key_file` |
| `REDIS_TLS_PRIVATE_NETWORK` | `redis.tls.private_network` |
| `REDIS_DB` | `redis.db` |
| `REDIS_POOL_SIZE` | `redis.pool_size` |
| `REDIS_MIN_IDLE_CONNS` | `redis.min_idle_conns` |
| `REDIS_DIAL_TIMEOUT_MS` | `redis.dial_timeout_ms` |
| `REDIS_READ_TIMEOUT_MS` | `redis.read_timeout_ms` |
| `REDIS_WRITE_TIMEOUT_MS` | `redis.write_timeout_ms` |
| `REDIS_POOL_TIMEOUT_MS` | `redis.pool_timeout_ms` |
| `REDIS_MAX_RETRIES` | `redis.max_retries` |
| `REDIS_CURRENCY_CACHE_TTL_SECONDS` | `redis.currency_cache_ttl_seconds` |
| `RABBITMQ_HOST` | `rabbitmq.host` |
| `RABBITMQ_PORT` | `rabbitmq.port` |
| `RABBITMQ_HOSTS` | `rabbitmq.hosts` (comma-separated) |
| `RABBITMQ_USER` | `rabbitmq.username` |
| `RABBITMQ_PASSWORD` | `rabbitmq.password` |
| `RABBITMQ_VHOST` | `rabbitmq.vhost` |
| `RABBITMQ_TLS_ENABLED` | `rabbitmq.tls.enabled` |
| `RABBITMQ_TLS_CA_FILE` | `rabbitmq.tls.ca_file` |
| `RABBITMQ_TLS_SERVER_NAME` | `rabbitmq.tls.server_name` |
| `RABBITMQ_TLS_CERT_FILE` | `rabbitmq.tls.cert_file` |
| `RABBITMQ_TLS_KEY_FILE` | `rabbitmq.tls.key_file` |
| `RABBITMQ_TLS_PRIVATE_NETWORK` | `rabbitmq.tls.private_network` |
| `JWT_SSL_USER_PRIVATE_KEY` | `jwt.ssl.user.private_key` |
| `JWT_SSL_USER_PUBLIC_KEY` | `jwt.ssl.user.public_key` |
| `JWT_SSL_ADMIN_PRIVATE_KEY` | `jwt.ssl.admin.private_key` |
| `JWT_SSL_ADMIN_PUBLIC_KEY` | `jwt.ssl.admin.public_key` |
| `JWT_ISSUER` | `jwt.issuer` |
| `JWT_ACCESS_TOKEN_EXPIRATION_MINUTES` | `jwt.access_token.expiration_minutes` |
| `JWT_REFRESH_TOKEN_EXPIRATION_MINUTES` | `jwt.refresh_token.expiration_minutes` |
| `JWT_REFRESH_TOKEN_REUSE_GRACE_SECONDS` | `jwt.refresh_token.reuse_grace_seconds` |
| `JWT_SESSIONS_MAX_PER_USER` | `jwt.sessions.max_per_user` |
| `JWT_SESSIONS_MAX_LIFETIME_MINUTES` | `jwt.sessions.max_lifetime_minutes` |
| `OTP_AUTH_NAME` | `otp.auth.name` |
| `OTP_AUTH_TTL_SECONDS` | `otp.auth.ttl_seconds` |
| `OTP_AUTH_LENGTH` | `otp.auth.length` |
| `OTP_AUTH_RETRY_LIMIT` | `otp.auth.retry_limit` |
| `OTP_SECRET` | `otp.auth.secret` |
| `ADMIN_TOTP_KEY` | `admin.totp_key` |
| `ADMIN_SIGN_IN_MIN_RESPONSE_MS` | `admin.sign_in_min_response_ms` |
| `CONTACTS_NEW_CONTACT_COOLDOWN_HOURS` | `contacts.new_contact_cooldown_hours` |
| `SECURITY_CHANGE_HOLD_HOURS` | `security.change_hold_hours` |
| `IDENTITY_MOBILE_OWNERSHIP_PROVIDER` | `identity.mobile_ownership.provider` |
| `IDENTITY_MOBILE_OWNERSHIP_ENDPOINT` | `identity.mobile_ownership.endpoint` |
| `IDENTITY_MOBILE_OWNERSHIP_API_KEY` | `identity.mobile_ownership.api_key` |
| `IDENTITY_MOBILE_OWNERSHIP_SECRET_KEY` | `identity.mobile_ownership.secret_key` |
| `IDENTITY_MOBILE_OWNERSHIP_TIMEOUT_SECONDS` | `identity.mobile_ownership.timeout_seconds` |
| `IDENTITY_MOBILE_OWNERSHIP_RECHECK_DAYS` | `identity.mobile_ownership.recheck_days` |
| `FAVORITES_MAX_PER_USER` | `favorites.max_per_user` |
| `DELISTING_INTERVAL_SECONDS` | `delisting.interval_seconds` |
| `PLATFORM_POLL_INTERVAL_MS` | `platform.poll_interval_ms` |
| `PLATFORM_STALE_AFTER_SECONDS` | `platform.stale_after_seconds` |
| `PLATFORM_FAIL_CLOSED` | `platform.fail_closed`, as a comma-separated list |
| `PLATFORM_LIFT_APPROVAL` | `platform.lift_approval`, as a comma-separated list |
| `KEYS_PROVIDER` | `keys.provider` |
| `KEYS_LOCAL_KEK` | `keys.local.kek` |
| `VAULT_ADDR` | `keys.transit.address` |
| `VAULT_TOKEN` | `keys.transit.token` |
| `VAULT_CACERT` | `keys.transit.ca_cert` |
| `KEYS_TRANSIT_TOKEN_FILE` | `keys.transit.token_file` |
| `KEYS_TRANSIT_MOUNT` | `keys.transit.mount` |
| `KEYS_TRANSIT_KEY` | `keys.transit.key` |
| `KEYS_TRANSIT_TIMEOUT_SECONDS` | `keys.transit.timeout_seconds` |
| `KEYS_RELOAD_INTERVAL_SECONDS` | `keys.reload_interval_seconds` |
| `KEYS_ACTIVATION_DELAY_SECONDS` | `keys.activation_delay_seconds` |
| `SMTP_PASSWORD` | `notifications.email.password` |
| `KAVENEGAR_API_KEY` | `notifications.sms.kavenegar.api_key` |
| `WS_ALLOWED_ORIGINS` | `websocket.allowed_origins`, as a comma-separated list |
| `WS_PING_INTERVAL_SECONDS` | `websocket.ping_interval_seconds` |
| `WS_MAX_MESSAGE_SIZE_BYTES` | `websocket.max_message_size_bytes` |
| `WS_WRITE_TIMEOUT_SECONDS` | `websocket.write_timeout_seconds` |
| `WS_CLIENT_MESSAGES_PER_SECOND` | `websocket.client_messages_per_second` |
| `WS_CLIENT_MESSAGE_BURST` | `websocket.client_message_burst` |
| `WS_MAX_CONNECTIONS` | `websocket.max_connections` |
| `WS_MAX_CONNECTIONS_PER_IP` | `websocket.max_connections_per_ip` |
| `WS_MAX_CONNECTIONS_PER_USER` | `websocket.max_connections_per_user` |
| `WS_SESSION_CHECK_SECONDS` | `websocket.session_check_seconds` |
| `RATE_LIMITS_TIMEOUT_MS` | `rate_limits.timeout_ms` |
| `RATE_LIMITS_IPV6_PREFIX_LENGTH` | `rate_limits.ipv6_prefix_length` |
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
- Keep the KYC provider's credentials (`IDENTITY_MOBILE_OWNERSHIP_API_KEY`, `IDENTITY_MOBILE_OWNERSHIP_SECRET_KEY`) in the secret manager, with a pair of their own for every environment.
- Give every environment its own KEK, and the server a Transit token that can only `encrypt` and `decrypt` with it, never read or delete it. See [`docs/data-protection.md`](data-protection.md).
