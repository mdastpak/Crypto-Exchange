# Test Scenarios and Review

> [!NOTE]
> **Origin and status.** This catalog was written on 2026-09-16 during a test review of commit `33e80e0`, before pull requests [#1](https://github.com/mdastpak/Crypto-Exchange/pull/1), [#4](https://github.com/mdastpak/Crypto-Exchange/pull/4), and [#5](https://github.com/mdastpak/Crypto-Exchange/pull/5) were merged. File and line references point to that commit and may be outdated. The "Current review" columns and the [Known baseline](#known-baseline) describe that earlier state.
>
> The scenario IDs (`U-`, `C-`, `H-`, `A-`, `D-`, `M-`, `W-`, `S-`) are stable. Reference them from tests and issues. The current status of every review finding is listed in [Finding status](#finding-status). The work to automate these scenarios in hermetic tests is tracked in [#37](https://github.com/mdastpak/Crypto-Exchange/issues/37).

## Purpose

This document defines the test contract for the user, authentication, currency-cache, middleware, WebSocket, and infrastructure paths in the current worktree. Expected behavior is the target product contract. Review notes identify places where the current implementation differs from that contract.

## Scope

- `GET /user/profile`
- `POST /user/logout`
- `GET /public/currencies`
- `GET /public/currencies/:hk`
- OTP, JWT, refresh, and logout flows
- Redis currency-cache lifecycle
- CORS, validation, request IDs, and rate limiting
- WebSocket and RabbitMQ currency updates
- health, migrations, configuration, and shutdown behavior

The matching engine, custody providers, KYC providers, and fiat providers are out of scope unless they are directly involved in one of the flows above.

## Test Conventions

| Label | Meaning |
|---|---|
| P0 | Blocks a safe release; automate in CI |
| P1 | Important regression or security behavior; automate before the next feature change |
| P2 | Useful edge case, performance check, or documentation contract |
| Unit | No external service; use fakes or interfaces |
| Contract | HTTP handler behavior with controlled dependencies |
| Integration | Real Redis/PostgreSQL/RabbitMQ behavior in an isolated environment |
| E2E | Started application with real dependencies and HTTP/WebSocket clients |

Every automated test must:

1. Use a generated RSA key pair for that test run. Never commit or reuse a production key.
2. Use unique UUIDs, client IDs, Redis keys, and database rows.
3. Clean up Redis keys and database rows after the test.
4. Avoid parallel tests while mutating the package-level `config.Config`.
5. Assert exact status codes, response messages, JSON types, and security headers.
6. Treat Redis `SCAN` order as nondeterministic; compare currency sets by `hk`, not array order.

## Shared Test Data

Use these values unless a scenario specifies otherwise:

```text
User UUID:       11111111-1111-1111-1111-111111111111
Admin UUID:      22222222-2222-2222-2222-222222222222
Currency HK #1: 33333333-3333-3333-3333-333333333333
Currency HK #2: 44444444-4444-4444-4444-444444444444
Invalid UUID:    not-a-uuid
```

Approved currency fixture:

```json
{
  "id": 1,
  "hk": "33333333-3333-3333-3333-333333333333",
  "status": "approved",
  "info": {
    "en_name": "Tether USD",
    "fa_name": "<name in Persian>",
    "if_fiat": false,
    "symbol": "USDT",
    "is_token": true,
    "contract": "test-contract"
  },
  "created_at": "2026-09-16T00:00:00Z",
  "updated_at": "2026-09-16T00:00:00Z"
}
```

Redis keys:

```text
{currencies}:public:v1
{currencies}:public:v1:generation
blacklist:token:<sha256-of-token>
```

Protected routes require `Authorization: Bearer <token>` since [#17](https://github.com/mdastpak/Crypto-Exchange/issues/17). A bare token is rejected with `401`. The scenarios below that say "raw" JWT send it with the `Bearer` scheme.

---

## 1. User Handler and `/user/profile`

Implementation: `internal/handlers/user/users.go`, routes in `RouteHandler` in `cmd/server/main.go`.

> [!IMPORTANT]
> `/user/profile` returns the authenticated user's profile since [#27](https://github.com/mdastpak/Crypto-Exchange/issues/27), and the user handler no longer lists currencies. Therefore:
>
> - U-01 keeps its user token but asserts the caller's profile, as U-30 does. U-02 still expects `401` for an admin token.
> - U-03 to U-10, U-28, U-29, and U-31 to U-35 (authentication, CORS, and rate limiting) apply to the profile route unchanged.
> - U-11 to U-26 (currency listing) apply to `GET /public/currencies`, the only currency listing.

| ID | Priority / level | Input and setup | Expected behavior | Current review |
|---|---|---|---|---|
| U-01 | P0 / contract | Valid user access token | `200`; `status=200`; `info` is the caller's profile | Passes ([#27](https://github.com/mdastpak/Crypto-Exchange/issues/27)) |
| U-02 | P0 / contract | Valid admin JWT; one currency | `401`; user routes accept only tokens signed with a user key | Passes ([#17](https://github.com/mdastpak/Crypto-Exchange/issues/17)) |
| U-03 | P0 / contract | Standard `Authorization: Bearer <valid-token>` | `200` | Passes ([#17](https://github.com/mdastpak/Crypto-Exchange/issues/17)); a bare token gets `401` |
| U-04 | P0 / contract | No `Authorization` header | `401`; message `Unauthorized - Token not provided`; handler not entered | Covered by middleware |
| U-05 | P0 / contract | Malformed JWT | `401`; message `Unauthorized - Invalid token` | Covered by middleware |
| U-06 | P0 / contract | Valid claims but expired `exp` | `401`; no currency read | Covered by JWT parser |
| U-07 | P0 / contract | Valid JWT signed with any algorithm other than RS256, including RS512 and HS256 keyed with the public key | `401`; signing algorithm must not be accepted | Covered by `ParseToken` |
| U-08 | P0 / contract | Valid JWT with role `system` | `401`; message `Unauthorized - Invalid token` | Covered by `ParseToken`: the role must match the route ([#17](https://github.com/mdastpak/Crypto-Exchange/issues/17)) |
| U-09 | P0 / contract | Valid JWT already present in the blacklist | `401`; message `Unauthorized - Token is blacklisted` | Requires a real Redis-backed test |
| U-10 | P0 / contract | Blacklist Redis returns an error | `500`; message `Error checking blacklist`; handler not entered | Current behavior; verify no currency data is returned |
| U-11 | P0 / contract | Empty Redis currency cache | `200`; `info` is an empty JSON array `[]` | Passes: the list is loaded from PostgreSQL, and an empty one is `[]` ([#52](https://github.com/mdastpak/Crypto-Exchange/issues/52)) |
| U-12 | P0 / contract | One valid cached currency | `200`; all currency fields map exactly from the fixture | Passes |
| U-13 | P1 / contract | Two valid cached currencies | `200`; both HKs are present exactly once; order is not asserted | Passes: the list is one key, ordered oldest first ([#52](https://github.com/mdastpak/Crypto-Exchange/issues/52)) |
| U-14 | P1 / contract | Duplicate HKs returned by the cache scan | Response contains one currency per HK | **Gap:** current code does not deduplicate |
| U-15 | P0 / contract | Cached currency has `status: suspended` | It is excluded from the active list | Passes: only approved currencies are cached, and reads skip any other entry ([#24](https://github.com/mdastpak/Crypto-Exchange/issues/24)) |
| U-16 | P0 / contract | Cached currency has `deleted_at` or `status: deleted` | It is excluded from the active list | Passes ([#24](https://github.com/mdastpak/Crypto-Exchange/issues/24)) |
| U-17 | P1 / contract | One cached value is invalid JSON | Valid entries are returned; corrupt entry is logged and skipped | **Gap:** user handler fails the whole request with `500` |
| U-18 | P1 / contract | One cache key returns a Redis GET error | Valid entries are returned; failed entry is skipped | **Gap:** user handler fails the whole request with `500` |
| U-19 | P0 / contract | Redis SCAN returns an error | `500`; message `Failed to retrieve currencies from cache.` | Current behavior |
| U-20 | P0 / contract | Redis is unavailable | `500` with a bounded response time | **Gap:** no request-context timeout is used |
| U-21 | P0 / contract | Client cancels the request while Redis is slow | Redis work stops and no goroutine remains blocked on the request | **Gap:** `context.Background()` is used |
| U-22 | P1 / contract | Cached JSON omits `deleted_at` | Field is omitted from the response | Current model uses `omitempty`; prior scenarios incorrectly expected `null` |
| U-23 | P1 / contract | Cached JSON contains unknown fields | Unknown fields do not break known-field decoding | Current `encoding/json` behavior |
| U-24 | P1 / contract | Cached JSON has wrong types for `id`, `status`, or `info` | Invalid entry is rejected or skipped without leaking an internal error | **Gap:** unmarshal errors currently produce a generic `500` |
| U-25 | P1 / contract | `?limit=-1`, `?offset=-1`, or non-numeric pagination values | `400` with a validation error after pagination is introduced | **Gap:** query parameters are currently ignored |
| U-26 | P1 / performance | More than 10,000 cached currencies | Response is paginated/capped and completes within the latency budget | **Gap:** current handler streams an unbounded result into memory |
| U-27 | P1 / contract | `POST`, `PUT`, or `DELETE /user/profile` | `405` with an `Allow: GET, PATCH` header | **Gap:** the router does not enable `HandleMethodNotAllowed`, so these get `404` |
| U-28 | P1 / contract | Valid request with no body and no query | `200`; body content type is `application/json` | Passes |
| U-29 | P1 / contract | Disallowed CORS origin | No permissive `Access-Control-Allow-Origin`; credentials are not exposed | **Gap:** default CORS reflects arbitrary origins |
| U-30 | P0 / product | Valid user JWT and user-specific profile data in PostgreSQL | Response contains the authenticated user's profile, never the global currency list | Passes: each user reads and changes only their own profile, and a body cannot name another user, tested end to end with two signed-in users ([#27](https://github.com/mdastpak/Crypto-Exchange/issues/27)) |
| U-31 | P1 / contract | Valid request under the rate limit | `200`; `X-RateLimit-Limit` and `X-RateLimit-Remaining` are present | Passes: every limited response carries both headers ([#34](https://github.com/mdastpak/Crypto-Exchange/issues/34)) |
| U-32 | P0 / contract | Requests exceed the trading burst | `429`; `Retry-After` is positive; handler is not entered | Passes: `Retry-After` is rounded up to whole seconds, so it is never `0`; the bucket is tested with controlled times ([#34](https://github.com/mdastpak/Crypto-Exchange/issues/34)) |
| U-33 | P1 / contract | Two users send requests concurrently | Limits are isolated by user; one user cannot consume another user's quota | Passes: the limit is keyed by the subject and role of the access token, tested with two users behind one address ([#34](https://github.com/mdastpak/Crypto-Exchange/issues/34)) |
| U-34 | P0 / security | Untrusted client supplies `X-Forwarded-For` | Rate-limit identity is based on a trusted proxy-derived IP, not a spoofable header | **Gap:** `getClientIP` trusts the header unconditionally |
| U-35 | P0 / security | Redis rate-limit backend fails | Protected endpoint fails closed or returns a defined degradation response | Passes: `rate_limits.<limiter>.on_error` selects `allow` or `deny` (`503`); the user and admin limits allow by default ([#34](https://github.com/mdastpak/Crypto-Exchange/issues/34)) |

---

## 2. Public Active Currencies

Implementation: `internal/handlers/public/auth.go:317`, route at `cmd/server/main.go:241`.

> [!IMPORTANT]
> **Cache-only reads versus the planned database fallback.** The expectations below assume that Redis is the only source, as it is today. [#52](https://github.com/mdastpak/Crypto-Exchange/issues/52) adds a read-through cache backed by PostgreSQL. Once it lands, the cache-miss and Redis-failure scenarios change: C-03 expects an empty list only when the database has no active currency either; H-02 expects `404` only when the database has no matching active row; and U-20 and C-09 (Redis unavailable) expect the data from the database instead of `500`, with `500` only when PostgreSQL is unavailable as well. The same applies to U-11 and U-19 for the shared listing.

| ID | Priority / level | Input and setup | Expected behavior | Current review |
|---|---|---|---|---|
| C-01 | P0 / contract | No authorization header; one cached currency | `200`; currency is returned | Passes |
| C-02 | P1 / contract | Arbitrary invalid authorization header | `200`; public endpoint ignores the header | Passes, subject to rate limiting |
| C-03 | P0 / contract | Empty cache | `200`; `info` is `[]` | **Gap:** current response has `info: null` |
| C-04 | P0 / contract | One approved cached currency | `200`; exact fixture mapping | Passes |
| C-05 | P1 / contract | Multiple approved cached currencies | `200`; all unique HKs are returned | Passes functionally |
| C-06 | P1 / contract | One invalid JSON cache value and one valid value | Valid value is returned; invalid value is logged and skipped | Passes |
| C-07 | P1 / contract | One cache key returns a GET error | Valid values are returned; failed key is skipped | Passes |
| C-08 | P0 / contract | Redis SCAN fails | `500`; message `Failed to retrieve currencies from cache.` | Passes |
| C-09 | P0 / contract | Redis is unavailable | `500` within the configured request timeout | Passes with a 10-second context |
| C-10 | P0 / contract | Client cancels the request during SCAN/GET | Work stops promptly and no leaked goroutine remains | Current context support should be verified under a slow fake |
| C-11 | P0 / contract | Suspended or deleted currency is cached | It is excluded from active currencies | Passes ([#24](https://github.com/mdastpak/Crypto-Exchange/issues/24)) |
| C-12 | P1 / contract | Duplicate HKs are observed | Each HK appears once | **Gap:** no deduplication |
| C-13 | P1 / contract | `limit=2&offset=1` after pagination is introduced | Correct page is returned with stable ordering | **Gap:** query parameters are ignored |
| C-14 | P1 / contract | `limit` exceeds the configured maximum | Request is capped or rejected according to the documented API contract | No maximum exists today |
| C-15 | P1 / contract | `GET /public/currencies` with a body | Body is ignored for the GET contract | Passes |
| C-16 | P1 / contract | Wrong HTTP method | `405` | Gin default behavior |
| C-17 | P0 / contract | Requests remain below the public REST limit | `200`; rate headers are accurate | Passes: only the REST limit applies ([#26](https://github.com/mdastpak/Crypto-Exchange/issues/26)) |
| C-18 | P0 / contract | Same IP exceeds the public limit | `429`; `Retry-After` is positive | Passes with the REST limit, which is no longer shared with sign-in ([#26](https://github.com/mdastpak/Crypto-Exchange/issues/26)) |
| C-19 | P0 / contract | Two public clients use different IPs | One client's limit does not affect the other | Passes: limits are kept per client IP, and sign-in limits also per route ([#26](https://github.com/mdastpak/Crypto-Exchange/issues/26)) |
| C-20 | P1 / contract | Wait until the fixed window expires | Next request is allowed and the counter is reset | Passes, also when the client keeps retrying while blocked: the window is no longer extended by each request ([#26](https://github.com/mdastpak/Crypto-Exchange/issues/26), item 2 of [#34](https://github.com/mdastpak/Crypto-Exchange/issues/34)) |
| C-21 | P0 / security | Redis rate-limit backend fails | Public endpoint fails closed or returns an explicit degraded response | Passes: the sign-in limits fail closed with `503` by default, and the other public routes fail open as configured ([#34](https://github.com/mdastpak/Crypto-Exchange/issues/34)) |
| C-22 | P1 / contract | Allowed origin from configuration | CORS headers match the configured origin | Passes when config is set |
| C-23 | P0 / security | Disallowed origin | No reflected `Access-Control-Allow-Origin` or credentials header | **Gap:** empty configuration reflects any origin |
| C-24 | P1 / contract | `OPTIONS` preflight from an allowed origin | `204`; allowed methods and headers are present | Passes; verify disallowed-origin behavior too |
| C-25 | P1 / contract | Response schema is compared with Swagger | Actual JSON matches the documented schema | **Gap:** Swagger documents a raw array, while handlers return `models.APIResponse` |

### Currency by HK

Implementation: `internal/handlers/public/auth.go:350`.

| ID | Priority / level | Input and setup | Expected behavior | Current review |
|---|---|---|---|---|
| H-01 | P0 / contract | Valid HK with a cached approved currency | `200`; exact currency object | Passes |
| H-02 | P0 / contract | Valid HK with no cache entry | `404`; message `Currency not found` | Passes |
| H-03 | P1 / contract | Non-UUID HK | `400`; message identifies invalid UUID format | **Gap:** current code treats it as a cache miss and returns `404` |
| H-04 | P0 / contract | Redis GET returns a non-`Nil` error | `500`; message `Failed to retrieve currency from cache.` | Passes |
| H-05 | P1 / contract | Cached value is invalid JSON | `500`; message `Failed to process currency information.` | Passes |
| H-06 | P0 / contract | Cached currency is suspended/deleted | `404` or an explicit inactive response | Passes: `404` with the message of a missing currency ([#24](https://github.com/mdastpak/Crypto-Exchange/issues/24)) |
| H-07 | P0 / contract | Client cancels during Redis GET | Request terminates within the timeout | Current 5-second context should be tested |
| H-08 | P1 / contract | Wrong HTTP method | `405` | Gin default behavior |

---

## 3. OTP, JWT, Refresh, and Logout

Relevant code: `internal/handlers/public/auth.go:62`, `internal/auth/auth.go:31`, `internal/auth/middleware.go:99`, `internal/auth/blacklist.go:29`.

| ID | Priority / level | Input and setup | Expected behavior | Current review |
|---|---|---|---|---|
| A-01 | P0 / contract | Valid email, OTP length 6, Redis available | `200`; contact type is `email`; returned contact is masked; encrypted Redis value has the configured TTL | Passes functionally, but assertions must inspect Redis |
| A-02 | P0 / contract | Valid mobile matching the configured pattern | `200`; type `mobile`; masked value | Passes functionally |
| A-03 | P0 / security | Username instead of an email address or mobile number | `400 Invalid contact information`; no OTP key is created and no user can be created for a username | Passes ([#14](https://github.com/mdastpak/Crypto-Exchange/issues/14)) |
| A-04 | P0 / contract | Empty, overlong, or invalid contact | `400`; no OTP key is created | Current validation occurs after OTP generation but before Redis write |
| A-05 | P0 / security | Generate many OTPs rapidly | Values are unpredictable, uniformly distributed, and not all identical | Passes: `otp.GenerateCode` uses `crypto/rand` with rejection sampling, checked by a chi-squared test over 100,000 codes ([#15](https://github.com/mdastpak/Crypto-Exchange/issues/15)) |
| A-06 | P0 / security | Capture application logs after OTP request | Logs contain neither the plaintext OTP nor the full contact | **Gap:** `RequestOTP` logs both |
| A-07 | P0 / contract | Redis SET fails | `500`; message `Failed to store OTP`; no success response | Passes |
| A-08 | P0 / contract | Valid UUID, matching OTP, new user | `200`; access and refresh tokens are returned; secure HttpOnly cookie is set; OTP key is deleted | Passes when JWT keys are initialized |
| A-09 | P0 / contract | Valid UUID, matching OTP, existing approved user | `200`; existing HK is reused; no duplicate row is created | Passes functionally |
| A-10 | P0 / contract | Wrong OTP | `401`; message `Invalid or expired OTP code`; attempt counter increments | Passes |
| A-11 | P0 / contract | Retry counter reaches the configured limit, including with parallel requests | `429`; message `Too many failed attempts...`; OTP cannot be retried; a new code does not reset the contact's attempts | Passes: attempts are reserved atomically per code and per contact ([#15](https://github.com/mdastpak/Crypto-Exchange/issues/15)) |
| A-12 | P0 / contract | Expired OTP session | `401`; the session expires naturally | Passes |
| A-13 | P0 / security | Redis holds an unexpected session value | No panic; client receives a defined `401/500`; attempt is recorded | Passes: sessions are Redis hashes read without type assertions ([#15](https://github.com/mdastpak/Crypto-Exchange/issues/15)) |
| A-14 | P0 / contract | Redis keys and values after an OTP request | Neither the code nor the raw contact appears in a key or a value | Passes: the code is stored as an HMAC and the contact with AES-GCM ([#15](https://github.com/mdastpak/Crypto-Exchange/issues/15)) |
| A-15 | P0 / contract | Decryption fails for a stored contact, for example after the secret changed | No panic; `500 Failed to verify OTP`; attempt is recorded | Passes |
| A-16 | P0 / contract | Existing user is `INACTIVE`, `SUSPENDED`, or `BANNED`, or deleted (`deleted_at` set) | Authentication is rejected with a stable client error and no token | Passes: `403` with no session ([#15](https://github.com/mdastpak/Crypto-Exchange/issues/15), [#38](https://github.com/mdastpak/Crypto-Exchange/issues/38)) |
| A-17 | P0 / contract | Database create fails | `500`; no token or cookie is issued; OTP cleanup is attempted | Current behavior; verify cleanup |
| A-18 | P0 / contract | Valid refresh cookie and initialized RSA keys | `200`; a new access token has the configured issuer, role, UUID, and expiry | Current handler parses the cookie only |
| A-19 | P0 / contract | Missing refresh cookie | `401`; message `Refresh token not provided` | Passes |
| A-20 | P0 / contract | Expired or invalid refresh token | `401`; no new access token | Passes |
| A-21 | P0 / security | Refresh token is blacklisted after logout | `401`; no new access token can be minted | **Gap:** `RefreshTokenHandler` does not consult the blacklist |
| A-22 | P0 / contract | Valid raw access token sent to logout | Token is stored under the blacklist prefix with a TTL not exceeding its expiry | Passes with Redis |
| A-23 | P0 / contract | Missing logout token | `401`; message `Unauthorized - Token not provided` | Passes |
| A-24 | P0 / contract | Invalid logout token | `401`; message `Unauthorized - Invalid token` | Passes |
| A-25 | P0 / contract | Blacklist Redis fails during logout | `500`; message `Failed to logout`; token is not silently considered logged out | Passes |
| A-26 | P1 / contract | Old token after a configured key rotation | Verification uses the registered historical public key until retirement | Passes: `ParseToken` selects the key by `kid` and accepts `retired_public_keys` ([#17](https://github.com/mdastpak/Crypto-Exchange/issues/17)) |
| A-27 | P0 / security | Two concurrent VerifyOTP requests use the same OTP | Exactly one succeeds; the second is rejected | Passes: only the request that deletes the session may sign in ([#15](https://github.com/mdastpak/Crypto-Exchange/issues/15)) |
| A-28 | P1 / contract | Refresh cookie is set over plain HTTP in a browser test | Test explicitly records whether `Secure` prevents storage; production requires HTTPS | Current cookie is always `Secure=true` |
| A-29 | P0 / security | After logout, an access token of the same session that was issued before the last refresh calls `/user/*` | `401 Unauthorized - Session is not active`; another session of the same user keeps working | Passes: user routes check the session on every request ([#120](https://github.com/mdastpak/Crypto-Exchange/issues/120)) |
| A-30 | P0 / security | Refresh token reuse revokes a session; its access tokens, the attacker's and the user's, call `/user/*` | `401` for both tokens; the newest refresh token is refused too | Passes ([#120](https://github.com/mdastpak/Crypto-Exchange/issues/120)) |
| A-31 | P0 / security | The user becomes `INACTIVE`, `SUSPENDED`, or `BANNED`, or is deleted, also by a change made directly in the database, while their tokens are valid | `403 This account is not available. Please contact support.` on the next `/user/*` request and on refresh, which clears the cookie and issues no token; logout still revokes the session, and it stays revoked when the account is restored | Passes ([#120](https://github.com/mdastpak/Crypto-Exchange/issues/120)) |

---

## 4. Admin Currency and Cache Lifecycle

Implementation: `internal/handlers/admin/currencies.go:71`.

| ID | Priority / level | Input and setup | Expected behavior | Current review |
|---|---|---|---|---|
| D-01 | P0 / integration | Valid approved currency JSON; admin JWT; empty DB/cache | DB row is created; cache key is written with the documented TTL; create event is published | Current cache/event failures are logged but the response remains `201` |
| D-02 | P0 / contract | `status: pending` or `status: suspended` | Row is created but currency is not exposed in active-cache reads | Passes: it is neither cached nor announced ([#24](https://github.com/mdastpak/Crypto-Exchange/issues/24)) |
| D-03 | P0 / contract | Malformed JSON, invalid status, invalid field values, or server-controlled fields (`id`, `hk`, timestamps) | `400`; field errors for invalid values; no DB row, cache entry, or event | Passes; a missing `status` defaults to `pending` ([#23](https://github.com/mdastpak/Crypto-Exchange/issues/23)) |
| D-04 | P1 / integration | Symbol or non-empty contract violates uniqueness | `409` with `duplicate_symbol` or `duplicate_contract`; no partial cache/event side effect | Passes; several currencies without a contract are allowed ([#22](https://github.com/mdastpak/Crypto-Exchange/issues/22)) |
| D-05 | P0 / integration | Valid update to an approved currency | DB and cache contain the same final `status` and `info` | Passes: an update changes only the fields it sends, tested for every field alone ([#35](https://github.com/mdastpak/Crypto-Exchange/issues/35)) |
| D-06 | P0 / integration | Update changes approved currency to suspended/deleted | Cache entry is removed or marked inactive; public/user reads exclude it | Passes: the entry is removed and a `delete` event is published. Every status transition is tested, and visibility is the same after a restart ([#24](https://github.com/mdastpak/Crypto-Exchange/issues/24)) |
| D-07 | P0 / contract | Update changes a boolean from `true` to `false` | The requested false value is persisted | Passes: only the fields sent are changed, in both directions of each flag ([#35](https://github.com/mdastpak/Crypto-Exchange/issues/35)) |
| D-08 | P0 / contract | Update uses a non-UUID HK | `400`; message identifies invalid UUID | Passes |
| D-09 | P0 / integration | Update uses an unknown or already deleted HK | `404`; no cache/event side effect | Passes for the DB lookup |
| D-10 | P0 / integration | Delete an existing currency | DB row is soft-deleted; cache key is removed; delete event is published | Passes: the cached list is dropped, and the next read loads it without the currency ([#52](https://github.com/mdastpak/Crypto-Exchange/issues/52)). Cache and event failures are still non-fatal ([#89](https://github.com/mdastpak/Crypto-Exchange/issues/89)) |
| D-11 | P0 / contract | Delete an unknown or already deleted currency | `404`; no repeated side effects | Passes, also for concurrent deletes: the row is locked, so one delete succeeds and publishes one event ([#24](https://github.com/mdastpak/Crypto-Exchange/issues/24)) |
| D-12 | P1 / integration | Cache write fails after DB commit | System returns a defined consistency/degradation result and schedules reconciliation | Passes: the change succeeds, and the outbox relay drops the cached list once Redis is back, before it publishes the event (`TestCachedList_CatchesUpOnceRedisRecovers`, [#89](https://github.com/mdastpak/Crypto-Exchange/issues/89)) |
| D-13 | P1 / integration | RabbitMQ publish fails after DB/cache commit | Event is retried or reconciled; client receives a documented result | Passes: the event is written to the outbox with the change and published, in order, once the broker is back (`TestEvents_ReachTheBrokerOnceItRecovers`, [#89](https://github.com/mdastpak/Crypto-Exchange/issues/89)) |
| D-14 | P0 / integration | The public list is loaded with approved, pending, suspended, and deleted rows | Only approved, non-deleted rows enter the cache | Passes: `TestList_ShowsThePublicCurrenciesOfTheDatabase` ([#52](https://github.com/mdastpak/Crypto-Exchange/issues/52)) |
| D-15 | P1 / integration | An instance starts or stops with unrelated Redis keys | No key is deleted | Passes. Neither startup nor shutdown touches the cache: `LoadAndCacheCurrencies` was replaced by loading the list on a miss ([#52](https://github.com/mdastpak/Crypto-Exchange/issues/52)), and `CleanupBaseDefinitions` by nothing ([#25](https://github.com/mdastpak/Crypto-Exchange/issues/25)) |
| D-16 | P1 / concurrency | Two updates target the same HK concurrently | Final DB/cache state is deterministic and no change is lost | Passes for the database: the row is locked from read to write ([#22](https://github.com/mdastpak/Crypto-Exchange/issues/22)). The events of one currency are published in commit order through the outbox ([#89](https://github.com/mdastpak/Crypto-Exchange/issues/89)). |

---

## 5. Middleware, Security, and API Contract

Relevant code: `internal/middleware/security.go:11`, `internal/middleware/validation.go:34`, `internal/middleware/ratelimit.go:65`, `cmd/server/main.go:275`.

| ID | Priority / level | Input and setup | Expected behavior | Current review |
|---|---|---|---|---|
| M-01 | P0 / contract | Any successful or error response | `Content-Type: application/json` where `SendResponse` is used | Passes for helper responses |
| M-02 | P1 / security | Any response | Security headers include nosniff, frame, referrer, permissions, COOP, and CORP policies | Current headers are present; add CSP/HSTS after TLS is enforced |
| M-03 | P0 / security | Origin in `AllowedOrigins` | Exact origin is reflected and credentials policy is consistent | Passes |
| M-04 | P0 / security | Origin not in `AllowedOrigins` | Origin is not reflected; preflight is not granted | **Gap:** empty config reflects every origin |
| M-05 | P1 / contract | Allowed-origin `OPTIONS` request | `204`; methods and headers match the documented contract | Passes |
| M-06 | P0 / contract | Required, email, min, max, UUID, and oneof tags | Each violation returns `400` with the field-specific message | Existing test covers a subset |
| M-07 | P0 / contract | Invalid JSON body | `400`; no partial object is passed to the handler | Gin binding behavior; verify all handlers |
| M-08 | P1 / security | Strings containing NUL, CR, LF, and surrounding spaces | Sanitization removes control characters and normalizes whitespace | Existing unit test covers basic cases |
| M-09 | P1 / contract | Nested maps and slices are supplied to sanitization | Only supported string values change; nested structures follow a documented rule | Current `SanitizeMap` handles only the top-level string values |
| M-10 | P0 / contract | Client supplies a valid `X-Request-ID` | Same value is returned | Passes |
| M-11 | P0 / contract | Client omits `X-Request-ID`, or sends one with spaces, quotes, control characters, or more than 64 characters | Server generates a new ID (UUIDv7) and returns it | Passes ([#32](https://github.com/mdastpak/Crypto-Exchange/issues/32)) |
| M-12 | P0 / security | Panic in a handler, in debug or release mode | `500` with no stack trace or internal value | Passes: the router uses `RecoveryMiddleware`, which never returns panic details ([#16](https://github.com/mdastpak/Crypto-Exchange/issues/16)) |
| M-13 | P0 / security | Error text contains a password, token, connection string, or stack trace | The response does not contain it | Passes for the OTP endpoints: handlers return fixed messages and only log errors ([#16](https://github.com/mdastpak/Crypto-Exchange/issues/16)). The unused `SanitizeError` helper is tracked in [#56](https://github.com/mdastpak/Crypto-Exchange/issues/56). |
| M-14 | P0 / contract | Token-bucket request is allowed | Limit and remaining quota are accurate | Current allowed path omits `X-RateLimit-Remaining` |
| M-15 | P0 / contract | Token-bucket request is rejected | `429`; limit, remaining `0`, and positive `Retry-After` | Current header is `Retry-After`, not `X-RateLimit-Retry-After` |
| M-16 | P0 / security | Redis script returns an error | Protected request does not silently pass | Passes: the error reaches the limiter, which follows its `on_error` policy ([#34](https://github.com/mdastpak/Crypto-Exchange/issues/34)) |
| M-17 | P0 / security | Fixed-window Redis pipeline returns an error | Protected request does not silently pass | Passes: as M-16 ([#34](https://github.com/mdastpak/Crypto-Exchange/issues/34)) |
| M-18 | P1 / contract | Trading limiter receives a context with `user_id` | Key is `ratelimit:trading:user:<id>` | Current route does not set `user_id`; JWT claims are stored under `claims` only |
| M-19 | P1 / contract | Trading limiter has no `user_id` | Documented fallback uses trusted client IP | Current fallback is client IP, but XFF handling is unsafe |

---

## 6. WebSocket and RabbitMQ Currency Updates

Relevant code: `internal/handlers/socket/handler.go:354`, `internal/handlers/socket/currencies.go:9`, `internal/rabbitmq/rabbitmq.go:227`.

| ID | Priority / level | Input and setup | Expected behavior | Current review |
|---|---|---|---|---|
| W-01 | P0 / contract | WebSocket upgrade with no Origin | Upgrade succeeds for same-origin/no-origin policy | Passes |
| W-02 | P0 / security | Upgrade with a configured allowed origin | Upgrade succeeds | Passes |
| W-03 | P0 / security | Upgrade with a disallowed origin | Upgrade is rejected | Passes when config is non-empty |
| W-04 | P1 / contract | Client sends valid `ping` with a valid timezone | Pong contains `event=pong` and server time in that timezone | Passes |
| W-05 | P1 / contract | Client sends a valid timezone name that does not exist | Server falls back to UTC without disconnecting | Passes |
| W-06 | P0 / security | Message exceeds the configured read limit | Connection is closed or rejected; no unbounded allocation | Passes: `websocket.max_message_size_bytes` (4 KiB) is the read limit, and a larger message closes the connection with `1009` ([#47](https://github.com/mdastpak/Crypto-Exchange/issues/47)) |
| W-07 | P1 / contract | Malformed JSON client message | Message is ignored/logged; connection remains usable | Passes |
| W-08 | P1 / contract | JSON has no string `event` | Message is ignored | Passes |
| W-09 | P1 / contract | Unknown event type | Message is ignored/logged; no broadcast | Passes |
| W-10 | P0 / contract | Valid RabbitMQ currency event | One broadcast is sent to each connected client with a UTC `server_time` | Passes, end to end: a currency created through the admin API reaches every connected client ([#19](https://github.com/mdastpak/Crypto-Exchange/issues/19)) |
| W-11 | P0 / contract | RabbitMQ body is invalid JSON | No broadcast; the event is not redelivered forever | Passes: the event is logged and acknowledged, because redelivering a malformed event cannot succeed |
| W-12 | P1 / security | RabbitMQ event contains unexpected or malicious fields | Schema is validated/sanitized before broadcast | Passes: the gateway rebuilds each event from the currency event schema, so unknown fields are dropped, and it drops and logs events with an unknown entity or action, an invalid `hk` or `info`, or more than 16 KiB ([#121](https://github.com/mdastpak/Crypto-Exchange/issues/121)) |
| W-13 | P1 / concurrency | Client send buffer is full | Broadcast skips that client without blocking other clients | Passes |
| W-14 | P0 / contract | Client disconnects | Client is removed; send channel is closed once; connection closes | Passes; race-test cleanup |
| W-15 | P0 / contract | Shutdown context is active | New connections stop, existing connections close, pumps return, and WaitGroup completes | Passes if shutdown is called once |
| W-16 | P1 / integration | RabbitMQ connection closes unexpectedly, or the broker closes the publishing channel | Service reconnects with bounded backoff, redeclares exchanges, restores the channel, and reports `degraded` meanwhile | Passes with a TCP proxy that simulates broker restarts, also under the race detector ([#21](https://github.com/mdastpak/Crypto-Exchange/issues/21)) |
| W-17 | P0 / integration | Broker does not confirm a publish | `Publish` returns an error and the caller retries or reconciles | Publisher confirms are implemented |
| W-18 | P1 / integration | Broker is unavailable for several attempts | Retry count and final error are deterministic; no goroutine leak remains | Verify with a fake or isolated broker |
| W-19 | P0 / integration | Two server instances consume the same exchange | Clients on both instances receive every event exactly once | Passes: each instance has an exclusive queue ([#19](https://github.com/mdastpak/Crypto-Exchange/issues/19)) |
| W-20 | P0 / security | A client sends messages faster than `websocket.client_messages_per_second` | The connection is closed with `1008`; other clients are not affected | Passes ([#47](https://github.com/mdastpak/Crypto-Exchange/issues/47)) |
| W-21 | P0 / security | More connections than `websocket.max_connections_per_ip` from one address, or `websocket.max_connections` in total | Further upgrades get `429` or `503`; a closed connection frees its place | Passes ([#47](https://github.com/mdastpak/Crypto-Exchange/issues/47)) |
| W-22 | P0 / security | Upgrade with a user access token in `Authorization` or the `access_token` subprotocol; with an invalid token, a token of an ended session, or a disabled user | A valid token is bound to the connection; the others get `401` or `403` before the upgrade; a token in the URL is ignored | Passes, end to end with a real sign-in and logout ([#47](https://github.com/mdastpak/Crypto-Exchange/issues/47)) |
| W-20 | P0 / contract | Several messages are queued before the write pump runs | Every frame holds exactly one JSON document | Passes ([#20](https://github.com/mdastpak/Crypto-Exchange/issues/20)) |

---

## 7. Health, Migrations, Configuration, and Shutdown

| ID | Priority / level | Input and setup | Expected behavior | Current review |
|---|---|---|---|---|
| S-01 | P0 / integration | PostgreSQL and Redis healthy | Health response has `status: ok` and both service details `ok` | Passes |
| S-02 | P0 / integration | PostgreSQL unavailable, Redis healthy | Response is `degraded`; postgres detail is `error`; HTTP policy is documented | Passes: `/system/ready` answers `503`, also when PostgreSQL stops behind a TCP proxy, and `200` again when it is back ([#33](https://github.com/mdastpak/Crypto-Exchange/issues/33)) |
| S-03 | P0 / integration | Redis unavailable, PostgreSQL healthy | Response is `degraded`; redis detail is `error` | Passes: `503` ([#33](https://github.com/mdastpak/Crypto-Exchange/issues/33)) |
| S-04 | P0 / integration | Both dependencies unavailable | Both details are present; response is not a misleading `ok` | Passes: `degraded` with `503`, and every dependency is checked at once within a timeout ([#33](https://github.com/mdastpak/Crypto-Exchange/issues/33)) |
| S-05 | P1 / contract | `/system/ping` | `200`; JSON contains `pong` and a UTC timestamp | Passes |
| S-06 | P0 / integration | Fresh PostgreSQL database | All migrations apply once and startup succeeds | Passes: the index is on `users` since [#4](https://github.com/mdastpak/Crypto-Exchange/pull/4), and the full and step-wise migration tests start from an empty database |
| S-07 | P0 / integration | Startup is repeated against an already migrated database | `migrate.ErrNoChange` is treated as success | Passes |
| S-08 | P0 / integration | A migration fails halfway | Startup fails, resources close, and the error is actionable | Verify transaction/rollback behavior |
| S-09 | P1 / contract | Config file is absent | Embedded example config is loaded and defaults are applied | Passes |
| S-10 | P0 / contract | Config contains invalid YAML or unsafe values | Startup fails before opening external services | Current validation is partial; add negative tests |
| S-11 | P0 / security | Config and logs are inspected | Private keys, passwords, OTPs, and tokens are never committed or logged | Current OTP logging violates this |
| S-12 | P0 / integration | SIGTERM is sent with active WebSocket clients | HTTP server, WebSocket pumps, Redis, PostgreSQL, and RabbitMQ shut down within the deadline; shared Redis state is kept | Passes. The shutdown sequence keeps the shared currency cache, tested with two instances ([#25](https://github.com/mdastpak/Crypto-Exchange/issues/25)). `TestServe_StopsGracefullyOnSIGTERM` sends SIGTERM to the test process with a request in flight: readiness answers `503` while the instance drains, new connections are refused, the request completes, and the WebSocket gateway stops. A server that fails to listen goes through the same shutdown ([#49](https://github.com/mdastpak/Crypto-Exchange/issues/49)) |
| S-13 | P0 / integration | Each migration is rolled back and applied again, one step at a time, from the latest version down to the first | Every down migration restores the schema of the version before it, every up migration restores its own schema, and the users of a rolled-back version keep their data | Passes: `TestMigrations_EachStepIsReversible` compares the schema after every step ([#30](https://github.com/mdastpak/Crypto-Exchange/issues/30)) |

---

## 8. Critical Review Findings

### Finding status

Status as of 2026-11-05. The tables after this one are the original findings, unchanged.

| Finding | Status |
| --- | --- |
| `/user/profile` is not a profile endpoint | Fixed in [#27](https://github.com/mdastpak/Crypto-Exchange/issues/27) |
| Empty currency responses are `null` | Fixed in [#1](https://github.com/mdastpak/Crypto-Exchange/pull/1) and [#4](https://github.com/mdastpak/Crypto-Exchange/pull/4) |
| OTP generation is predictable | Fixed in [#1](https://github.com/mdastpak/Crypto-Exchange/pull/1) (`crypto/rand`) and [#15](https://github.com/mdastpak/Crypto-Exchange/issues/15) (no modulo bias) |
| OTP and contact are logged in plaintext | Fixed in [#13](https://github.com/mdastpak/Crypto-Exchange/issues/13) |
| Refresh tokens bypass blacklist | Fixed in [#10](https://github.com/mdastpak/Crypto-Exchange/issues/10) |
| Protected rate limiting fails open | Fixed in [#34](https://github.com/mdastpak/Crypto-Exchange/issues/34) |
| User currency reads can block indefinitely | Fixed in [#27](https://github.com/mdastpak/Crypto-Exchange/issues/27): the user handler no longer lists currencies |
| Stale suspended/deleted currencies remain active | Fixed in [#24](https://github.com/mdastpak/Crypto-Exchange/issues/24) |
| Malformed OTP data can panic | Fixed in [#15](https://github.com/mdastpak/Crypto-Exchange/issues/15) (together with A-15, A-16, and A-27) |
| User migration references the wrong table | Fixed in [#4](https://github.com/mdastpak/Crypto-Exchange/pull/4) |
| Public currency traffic consumes the login quota | Fixed in [#26](https://github.com/mdastpak/Crypto-Exchange/issues/26) |
| CORS reflects arbitrary origins with credentials | Fixed in [#12](https://github.com/mdastpak/Crypto-Exchange/issues/12) |
| List handlers have no pagination or response cap | Open: [#52](https://github.com/mdastpak/Crypto-Exchange/issues/52) and [#53](https://github.com/mdastpak/Crypto-Exchange/issues/53) |
| Redis has no database fallback | Open: [#52](https://github.com/mdastpak/Crypto-Exchange/issues/52) |
| Duplicate SCAN results are not deduplicated | Open: [#52](https://github.com/mdastpak/Crypto-Exchange/issues/52) |
| Standard Bearer header is not normalized | Fixed in [#17](https://github.com/mdastpak/Crypto-Exchange/issues/17): every protected route, including logout, requires the `Bearer` scheme |
| Key rotation verifier is not used by role middleware | Fixed in [#17](https://github.com/mdastpak/Crypto-Exchange/issues/17): `ParseToken` selects the key by `kid`, including retired keys |
| Swagger does not match actual responses | Open: [#54](https://github.com/mdastpak/Crypto-Exchange/issues/54) |
| Cache and event failures are silently non-fatal | Open: [#89](https://github.com/mdastpak/Crypto-Exchange/issues/89) |
| Global config and RSA keys make tests order-dependent | Open: [#48](https://github.com/mdastpak/Crypto-Exchange/issues/48) and [#37](https://github.com/mdastpak/Crypto-Exchange/issues/37) |
| Observability and maintainability (P2) | Open: [#50](https://github.com/mdastpak/Crypto-Exchange/issues/50) |
| Minimum CI gates (section 9) | Open: [#36](https://github.com/mdastpak/Crypto-Exchange/issues/36) |

### P0 — Fix before merge

| Finding | Evidence | Affected scenarios | Recommended correction |
|---|---|---|---|
| `/user/profile` is not a profile endpoint | `cmd/server/main.go:267` routes it to `UserHandler.GetActiveCurrencies`; the handler never reads claims | U-30, U-01–U-35 | Implement a user-specific handler or rename the route and update Swagger |
| Empty currency responses are `null` | `var currencies []models.Currency` in both list handlers marshals nil as `null` | U-11, C-03 | Initialize with `make([]models.Currency, 0)` |
| OTP generation is predictable | `internal/handlers/public/auth.go:263` uses `time.Now().UnixNano()%10` | A-05 | Use `crypto/rand` and reject unsupported lengths |
| OTP and contact are logged in plaintext | `internal/handlers/public/auth.go:109` | A-06, S-11 | Log only request ID, contact type, and non-sensitive outcome |
| Refresh tokens bypass blacklist | `internal/auth/auth.go:112` validates but never checks `TokenBlacklist` | A-21 | Pass the blacklist into refresh handling and reject revoked tokens |
| Protected rate limiting fails open | `internal/middleware/ratelimit.go:144` and `:159` turn Redis errors into allowed requests | U-35, C-21, M-16, M-17 | Define fail-closed behavior and test backend errors |
| User currency reads can block indefinitely | `internal/handlers/user/users.go:20` uses `context.Background()` | U-20, U-21 | Use `c.Request.Context()` with a bounded timeout |
| Stale suspended/deleted currencies remain active | List handlers do not filter status/deletion; admin writes every status to cache | U-15, U-16, C-11, H-06, D-02, D-06 | Filter on read and remove inactive entries on lifecycle changes |
| Malformed OTP data can panic | `otpInfo["auth"].(string)` at `internal/handlers/public/auth.go:155` | A-13 | Type-check and handle missing/wrong types |
| User migration references the wrong table | `internal/db/migrations/2_create_users_table.up.sql:23` indexes `currencies` | S-06 | Change the index target to `users` and test a fresh migration |

### P1 — Fix in the next hardening pass

| Finding | Evidence | Affected scenarios | Recommended correction |
|---|---|---|---|
| Public currency traffic consumes the login quota | `cmd/server/main.go:242` applies `AuthRateLimit("login")` | C-17–C-21 | Use a dedicated public REST limiter |
| CORS reflects arbitrary origins with credentials | `internal/middleware/security.go:41` | U-29, C-23, M-04 | Require explicit production origins and set `Vary: Origin` |
| List handlers have no pagination or response cap | `Scan` loops append without a limit | U-25, U-26, C-13, C-14 | Add validated cursor/limit pagination and a maximum page size |
| Redis has no database fallback | Both list handlers read cache only | U-20, C-09 | Add cache-aside loading from PostgreSQL with reconciliation |
| Duplicate SCAN results are not deduplicated | Handlers append every key | U-14, C-12 | Deduplicate by HK or use a deterministic query |
| Standard Bearer header is not normalized | Middleware reads the header verbatim | U-03, A-25 | Accept exactly one standard Bearer form and reject malformed forms |
| Key rotation verifier is not used by role middleware | Admin/user middleware call `VerifyRSAToken`; rotation helper exists separately | A-26 | Use the rotation-aware verifier consistently |
| Swagger does not match actual responses | `docs/docs.go:185` documents a raw array; handlers use `APIResponse` | U-31, C-25 | Regenerate annotations and contract tests from one schema |
| Cache and event failures are silently non-fatal | Admin handlers log cache/publish errors and still return success | D-01, D-12, D-13 | Add an outbox/reconciliation path and explicit degradation semantics |
| Global config and RSA keys make tests order-dependent | `config.Config` and auth key variables are package-level | All auth/rate-limit tests | Inject configuration/keys or provide a test reset helper |

### P2 — Observability and maintainability

- Add scenario assertions for structured logs, request IDs, latency metrics, and rate-limit metrics.
- Add benchmarks for large cache scans, JSON marshaling, and WebSocket fanout.
- Add race-enabled tests for balance/order state only when those domains enter the test scope.
- Keep test-only Redis commands such as `KEYS` out of performance-sensitive paths; use `SCAN` with a bounded test cursor.

---

## 9. Automation Plan

### Test layers

| Layer | Recommended harness | Scenarios |
|---|---|---|
| Unit | `testing`, `httptest`, generated RSA keys, deterministic clock, small fakes | U-04–U-10, A-04–A-06, M-06–M-13, W-04–W-09 |
| Contract | Gin `httptest.Router`, Redis fake/interface, SQL fake or transactional DB | U-01–U-35, C-01–C-25, H-01–H-08, D-03–D-11 |
| Integration | Isolated PostgreSQL, Redis, and RabbitMQ containers; unique schema/database | A-01–A-27, D-01–D-16, W-10–W-18, S-01–S-12 |
| E2E | Start the server, call HTTP/WebSocket clients, exercise lifecycle and shutdown | U-30, C-17–C-24, A-08–A-21, W-01–W-15, S-12 |

### Minimum CI gates

1. `go test ./...`
2. `go test -race ./internal/auth ./internal/middleware ./internal/handlers/...`
3. `go vet ./...`
4. `gofmt -l .` must be empty
5. `git diff --check`
6. Integration suite with fresh PostgreSQL/Redis/RabbitMQ services
7. Contract test comparing actual JSON schemas with Swagger/OpenAPI

### Coverage targets

| Area | Target |
|---|---|
| Auth and middleware | 90%+ branch coverage, including Redis failure and blacklist paths |
| Currency handlers | 90%+ branch coverage, including empty/corrupt cache and timeout paths |
| Admin cache lifecycle | 85%+ branch coverage, including partial side-effect failures |
| WebSocket/RabbitMQ | 80%+ branch coverage plus race and shutdown tests |
| Domain models | Focus on business invariants; do not count wrapper delegation to external libraries as meaningful coverage |

### Known baseline

The current clean worktree has passing tests only in `internal/domain/shared` and `internal/middleware`; handler, auth, database, RabbitMQ, and WebSocket packages have no tests. The candidate integration suite in the related worktree requires external services, has uninitialized JWT state, and contains a nil-response panic on an empty currency list. Those issues are represented by the P0 scenarios above.
