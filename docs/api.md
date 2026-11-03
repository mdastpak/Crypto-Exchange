# API reference

This document describes the HTTP and WebSocket API as currently implemented on `main`. The API is not versioned yet. Moving it under `/api/v1` with a stable error contract is tracked in [#53](https://github.com/mdastpak/Crypto-Exchange/issues/53). The generated Swagger UI at `/swagger/index.html` currently covers only the currency endpoints ([#54](https://github.com/mdastpak/Crypto-Exchange/issues/54)).

## Conventions

### Response envelope

Every endpoint except `/system/*` responds with:

```json
{
  "status": 200,
  "message": "Human-readable message",
  "info": {}
}
```

`status` repeats the HTTP status code. `info` holds the payload and is omitted when there is none. Error responses use the same envelope and contain only a message.

### Authentication

Protected routes expect the JWT access token in the `Authorization` header.

> [!NOTE]
> The `/admin/*` and `/user/*` middleware currently expects the **raw token without a `Bearer` prefix**, and rejects a prefixed token with `401` before the handler runs. Only `POST /public/auth/logout`, which has no authentication middleware, accepts both forms. Consistent `Bearer` support is tracked in [#17](https://github.com/mdastpak/Crypto-Exchange/issues/17).

Tokens are RS256-signed JWTs with these claims:

| Claim | Description |
| --- | --- |
| `uuid` | The user's public identifier (`users.hk`) |
| `role` | `user` or `admin` |
| `typ` | `access` or `refresh`. Protected routes accept only `access` tokens, and the refresh endpoint accepts only `refresh` tokens. |
| `sid` | The sign-in session the token belongs to |
| `jti` | A unique token ID |
| `iss` | `jwt.issuer` from the configuration |
| `iat`, `exp` | Issue and expiry time |

Each sign-in creates a session in the `sessions` table. Only SHA-256 hashes of the tokens are stored.

### Rate limiting

Rate-limited responses return `429 Too Many Requests` with the `X-RateLimit-Limit`, `X-RateLimit-Remaining`, and `Retry-After` headers. [docs/configuration.md](configuration.md#rate_limits) lists the limits per route group.

## Sign-in flow

```mermaid
sequenceDiagram
    participant C as Client
    participant A as API
    participant R as Redis
    participant P as PostgreSQL
    C->>A: POST /public/auth {username}
    A->>R: Store OTP session (TTL = otp.auth.ttl_seconds)
    A-->>C: 200 {type, username (masked), uuid}
    C->>A: POST /public/auth/{uuid} {otp}
    A->>R: Verify the code and count failed attempts
    A->>P: Find or create the user by contact, create a session
    A-->>C: 200 {access_token} and Set-Cookie refresh_token
    C->>A: POST /public/auth/refresh (cookie)
    A->>P: Rotate the session's refresh token
    A-->>C: 200 {access_token} and Set-Cookie with the new refresh_token
```

Codes are not delivered by email or SMS yet ([#14](https://github.com/mdastpak/Crypto-Exchange/issues/14)). For local development, `otp.auth.dev_log_codes` writes them to the server log (see [configuration.md](configuration.md#otp)). Codes and raw contacts are never logged otherwise.

### `POST /public/auth`

Requests an OTP.

```json
{ "username": "user@example.com" }
```

`username` accepts one of the following:

| Contact type | Format |
| --- | --- |
| `email` | A standard email address |
| `mobile` | An Iranian mobile number: `09` followed by 9 digits |
| `username` | 6 to 20 characters of `A-Z`, `a-z`, `0-9`, and `_`. It must not start with a digit or `_` and must not contain `__`. |

Response `200`:

```json
{
  "status": 200,
  "message": "OTP sent to your contact",
  "info": { "type": "email", "username": "use*****@example.com", "uuid": "1b9d6bcd-bbfd-4b2d-9b5d-ab8dfbbd4bed" }
}
```

Errors: `400` for a missing or invalid contact, `429` when the rate limit is exceeded.

### `POST /public/auth/{uuid}`

Verifies the OTP for the session `uuid` returned by the previous call.

```json
{ "otp": "123456" }
```

Response `200`:

```json
{
  "status": 200,
  "message": "OTP verified successfully",
  "info": { "access_token": "<jwt>" }
}
```

The refresh token is **only** delivered in a cookie, never in the body, so scripts running in the page cannot read it:

```http
Set-Cookie: refresh_token=<jwt>; Path=/public/auth; Max-Age=<refresh_token.expiration_minutes × 60>; HttpOnly; Secure; SameSite=Strict
```

The browser sends the cookie only to `/public/auth/*`, which covers the refresh and logout routes. Because it is `Secure`, browsers only store it over HTTPS. Chrome and Firefox also store it on `http://localhost` during development.

Errors:

- `400`: invalid UUID or missing code
- `401`: wrong or expired code
- `429`: too many failed attempts (`otp.auth.retry_limit`)
- `500`: the user could not be created or loaded. This currently includes disabled, deleted, and suspended accounts ([#15](https://github.com/mdastpak/Crypto-Exchange/issues/15)).

The first successful verification for a contact creates the user. Each email address, mobile number, and username belongs to at most one user, and concurrent first sign-ins for the same contact resolve to the same user. Email addresses and usernames are matched case-insensitively, so `Alice@Example.com` and `alice@example.com` are the same contact.

### `POST /public/auth/refresh`

Exchanges the `refresh_token` cookie for a new access token. No request body is needed.

Response `200`: `{"info": {"access_token": "<jwt>"}}`, together with a `Set-Cookie` header that carries a **new** refresh token. Each refresh token can be used only once:

- The session's refresh token is replaced on every refresh (rotation), and its expiry moves forward by `refresh_token.expiration_minutes`.
- Presenting a refresh token that was already rotated is treated as theft: the whole session is revoked, and its newest refresh token stops working too. This also applies when a client sends the same refresh token in two concurrent requests, so clients must not refresh in parallel.

Errors: `401` when the cookie is missing, invalid, or expired, when it holds an access token, or when the session has been revoked. A `401` also clears the cookie.

### `POST /public/auth/logout` and `POST /user/logout`

Ends the session of the access token sent in the `Authorization` header:

- The session is revoked, so its refresh token stops working.
- The access token is blacklisted until it expires.
- The refresh token cookie is cleared.

Other access tokens issued earlier for the same session stay valid until they expire (`access_token.expiration_minutes`).

| Route | Accepted `Authorization` value |
| --- | --- |
| `POST /public/auth/logout` | The raw token, or `Bearer <token>` |
| `POST /user/logout` | The raw token only. A `Bearer` prefix is rejected with `401` by the user middleware ([#17](https://github.com/mdastpak/Crypto-Exchange/issues/17)). |

Response `200`. Errors: `401` for a missing or invalid token.

## Currencies

### Currency object

```json
{
  "id": 1,
  "hk": "0d4f2a7e-8c1b-4f0e-9a51-3c2f6f1b7d10",
  "status": "approved",
  "info": {
    "en_name": "Tether",
    "fa_name": "<name in Persian>",
    "symbol": "USDT",
    "if_fiat": false,
    "is_token": true,
    "contract": "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t"
  },
  "created_at": "",
  "updated_at": ""
}
```

`status` is one of `approved`, `pending`, `suspend`, or `deleted`. `symbol` and `contract` must be unique. The field name `if_fiat` is a known typo ([#35](https://github.com/mdastpak/Crypto-Exchange/issues/35)).

### `GET /public/currencies`

Lists the currencies held in the Redis cache. Response `200` with an array in `info`, which is empty when there are none.

### `GET /public/currencies/{hk}`

Returns one cached currency. Errors: `404` when it is not cached.

### `POST /admin/currencies` (admin)

Creates a currency. The body is a currency object without `id` and `hk`. Always send `status`. Omitting it currently returns `500` ([#23](https://github.com/mdastpak/Crypto-Exchange/issues/23)).

Response `201`. Errors: `400` for an invalid body, `500` for duplicates or other database errors ([#22](https://github.com/mdastpak/Crypto-Exchange/issues/22)).

### `PUT /admin/currencies/{hk}` (admin)

Partially updates a currency. Only non-empty fields are applied, so boolean flags cannot be reset to `false` yet ([#35](https://github.com/mdastpak/Crypto-Exchange/issues/35)).

Response `200`. Errors: `400` for an invalid UUID or body, `404` when the currency is not found.

### `DELETE /admin/currencies/{hk}` (admin)

Soft-deletes a currency (sets `deleted_at` and `status = deleted`) and removes it from the cache.

Response `200`. Errors: `400` for an invalid UUID, `404` when the currency is not found or already deleted.

Create, update, and delete publish an event to the `price_updates` exchange:

```json
{
  "entity": "currencies",
  "action": "create",
  "hk": "0d4f2a7e-8c1b-4f0e-9a51-3c2f6f1b7d10",
  "info": { "symbol": "USDT" },
  "server_time": "2026-11-02T10:00:00Z"
}
```

## User

| Route | Current behavior |
| --- | --- |
| `GET /user/profile` | Returns the currency list instead of the profile ([#27](https://github.com/mdastpak/Crypto-Exchange/issues/27)) |
| `POST /user/update` | Returns `501 Not Implemented` |

## System

These endpoints return plain JSON without the envelope.

### `GET /system/healthcheck`

```json
{
  "status": "ok",
  "details": {
    "postgres": { "status": "ok", "last_check": "2026-11-02T10:00:00Z", "latency_ms": 1 },
    "redis": { "status": "ok", "last_check": "2026-11-02T10:00:00Z", "latency_ms": 1 },
    "rabbitmq": { "status": "ok", "last_check": "2026-11-02T10:00:00Z" }
  }
}
```

`status` is `degraded` when any dependency fails. The HTTP status is currently always `200` ([#33](https://github.com/mdastpak/Crypto-Exchange/issues/33)).

### `GET /system/ping`

```json
{ "message": "pong", "timestamp": "2026-11-02T10:00:00Z" }
```

## WebSocket

Connect to `ws://<host>:<port>/ws/`. The trailing slash is required. Browser origins must match `websocket.allowed_origins`, or the request must be same-origin when the list is empty.

### Connection behavior

| Setting | Value |
| --- | --- |
| Maximum inbound message size | 512 bytes |
| Server ping interval | 30 seconds |
| Read deadline | 60 seconds, extended on every pong |
| Per-client outbound buffer | 256 messages. Messages are dropped when the buffer is full. |

### Client messages

Ping, optionally with an IANA time zone for the reply timestamp:

```json
{ "event": "ping", "timezone": "Europe/Berlin" }
```

The server replies:

```json
{ "event": "pong", "timestamp": "2026-11-02T11:00:00+01:00" }
```

`{"event": "subscribe", "channels": [...]}` is accepted but has no effect yet. The channel protocol is tracked in [#66](https://github.com/mdastpak/Crypto-Exchange/issues/66).

### Server messages

Currency events (see [Currencies](#currencies)) are meant to be broadcast to every client, with a `server_time` field added. The broadcast consumer is not started by the server yet ([#19](https://github.com/mdastpak/Crypto-Exchange/issues/19)). When several messages are queued, they can currently arrive concatenated in a single frame ([#20](https://github.com/mdastpak/Crypto-Exchange/issues/20)).
