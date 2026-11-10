# Redis high availability

Redis is shared by every server instance and holds state that sign-in depends on: pending sign-in codes and their attempt counters, the per-contact limits, the blacklist of logged-out tokens, the rate-limit buckets, and the public currency cache. A single Redis node is therefore a single point of failure for sign-in. This document describes how to run Redis without one, and what a failover may lose ([#231](https://github.com/mdastpak/Crypto-Exchange/issues/231)).

## Deployment

`redis.mode` selects how the server reaches Redis. Every mode uses the same `username`, `password`, timeouts, pool, and `tls` settings ([configuration](configuration.md#redis)).

| Mode | Settings | When to use it |
| --- | --- | --- |
| `standalone` | `host`, `port` | Development, and a managed Redis that fails over behind one address |
| `sentinel` | `sentinel.master_name`, `sentinel.addresses` | **Recommended** for a self-managed deployment: a primary, two replicas, and three sentinels on three hosts |
| `cluster` | `cluster.addresses` | When one primary is not enough. Three primaries, each with two replicas |

In the `sentinel` mode the client asks the sentinels for the current primary and follows a failover without a restart. In the `cluster` mode it learns the slots from the nodes, and reloads them after a network error, so it finds a promoted replica as soon as the cluster has promoted it, instead of after go-redis's minute; it also reloads them every five seconds. Every command goes to a primary, also in these modes: a replica can lag behind, and a counter must be read where it is written.

With TLS in the `sentinel` and `cluster` modes, each connection checks the name of the node it dials, unless `tls.server_name` names one for all of them. The certificates of the nodes must therefore carry the names that the sentinels and the cluster announce.

A recommended Sentinel setup:

```yaml
redis:
  mode: sentinel
  sentinel:
    master_name: crypto-exchange
    addresses: ["redis-1:26379", "redis-2:26379", "redis-3:26379"]
  durable_writes:
    replicas: 1
    timeout_ms: 250
```

On the Redis side:

- `sentinel monitor crypto-exchange <primary> 6379 2`, so that two of the three sentinels must agree that the primary is down.
- `min-replicas-to-write 1` and `min-replicas-max-lag 10` on the primary, so that a primary cut off from its replicas stops accepting writes instead of collecting writes that the failover will discard.
- Persistence is not needed for availability, but an AOF with `appendfsync everysec` keeps the data when every node restarts together.

## Hash slots

Redis Cluster runs a Lua script or a `MULTI` only on keys of one hash slot. Every script and transaction of the server stays in one slot:

| Store | Keys of one script or transaction | How |
| --- | --- | --- |
| Sign-in codes (`internal/otp`) | The counters and the known-client marker of a contact | They share the hash tag of the contact: `otp:contact:{<contact-id>}:...` |
| Sign-in codes | A session | One key per script. The attempts of the session and of its contact are counted by two scripts, since a session key does not name its contact. |
| Currency cache (`internal/currencies`) | The list, its generation, and its lock | The hash tag `{currencies}` |
| Rate limiters (`internal/middleware`) | One bucket or counter | One key per script |
| Token blacklist (`internal/auth`) | One token | One key per command |
| Contact changes (`internal/handlers/user`) | One pending change | One key per transaction |

`TestStore_EveryScriptStaysInOneHashSlot` records every script and transaction of the sign-in store and fails when one of them mixes slots.

The keys of a contact were renamed to carry the hash tag. After the upgrade, the counters of the contacts start over, at most an hour's worth, and a client that signed in with a contact during the last 30 days is no longer exempt from the contact's total code limit until it signs in again.

## What a failover may lose

Redis replicates asynchronously: a write that the primary acknowledged may be missing on the replica that a failover promotes. Each kind of data is decided as follows.

| Data | Effect of a lost write | Decision |
| --- | --- | --- |
| Attempts at a sign-in code, per code and per contact | The attempts go back, giving whoever made them more guesses | **Not acceptable.** With `durable_writes.replicas` above 0, each attempt is followed by `WAIT`, and the code is checked only once that many replicas hold the attempt. When fewer acknowledge it within `durable_writes.timeout_ms`, the request is answered with `503 database_busy` and `Retry-After`, and the code is not checked, not even a right one. A promoted replica therefore holds every attempt whose code was checked. |
| Codes requested per contact | More codes, and SMS, for the contact | Same as the attempts: a request is counted durably before its code is sent. |
| A sign-in session | The code no longer works; the user requests a new one | Acceptable |
| Access-token blacklist | A logged-out token works again until it expires | For a user token, the session check of every request rejects it anyway ([#120](https://github.com/mdastpak/Crypto-Exchange/issues/120)). So does the session check of an administrator's token ([#308](https://github.com/mdastpak/Crypto-Exchange/issues/308)). The blacklist entry is still a durable write, as a second check. |
| Pending contact changes | The change is started again | Acceptable |
| Rate-limit buckets | A short burst allowance | Acceptable |
| Currency cache | Reloaded from PostgreSQL | Acceptable |

With `durable_writes.replicas: 0`, the default, nothing waits, and a failover may give back the attempts made in the moment before it: as many as an attacker can send within the replication lag, usually a few milliseconds. Set it to 1 in production, with two replicas per primary, so that one replica may be down without stopping sign-in. When no replica can acknowledge, sign-in stops with `503` until one can: the service prefers that to handing out attempts.

## Memory and eviction

A failover is not the only way to lose a key: when Redis reaches `maxmemory`, its `maxmemory-policy` decides what happens next ([#305](https://github.com/mdastpak/Crypto-Exchange/issues/305)). Under every policy but `noeviction`, Redis drops keys to make room, and the checks that read them pass without an error: a logged-out administrator token works again, the attempts at a sign-in code and the codes sent to a contact start over, and so do the rate limits and the WebSocket connection counts. The `volatile-*` policies do not help, since every one of these keys has a TTL. The caches that share the instance, the currency list and the market data answers, are what fills the memory, and the security keys are what would be dropped.

Run every Redis that the server uses with:

```text
maxmemory <a limit that leaves headroom>
maxmemory-policy noeviction
```

A full Redis then refuses writes, which the server answers with `503 service_unavailable` ([While Redis is away](#while-redis-is-away)): sign-in stops instead of failing open. Size `maxmemory` so that this does not happen, and alert on `used_memory` before it does.

The server enforces it:

- At start, it reads `CONFIG GET maxmemory-policy` of each primary (every primary of a cluster). In release mode, any other policy stops the server. A policy that cannot be read, as on a managed Redis that refuses `CONFIG`, stops it too, unless `redis.assume_noeviction` (`REDIS_ASSUME_NOEVICTION`) says that you have checked the policy in the provider's settings; the server then logs a warning at every start. Outside release mode, both are logged.
- `/system/ready` reports `redis_eviction` from `evicted_keys` of `INFO stats`: any key evicted since the instance started makes it `degraded`, with `200`, so that an operator looks into it without the instances being taken out of service.

## While Redis is away

A request that needs Redis while it cannot be reached, or cannot serve, as while a replica is promoted, answers `503 service_unavailable` with `Retry-After: 1`, not `500` ([#130](https://github.com/mdastpak/Crypto-Exchange/issues/130)). Every command of the server's client goes through a hook that notes such an error on its request, and the response helpers turn the request's `500` into that answer, in every mode. The checks still fail closed: a token whose revocation cannot be checked is refused, and a sign-in code is not checked. What does not need Redis keeps working: the currency reads fall back to PostgreSQL, and the rate limits follow their `on_error`.

`durable_writes` applies to every mode. In the `cluster` mode, `WAIT` runs on the primary of the key's slot.

## The failover drill

`scripts/redis-ha/failover-drill.sh` starts, on Docker, a Sentinel setup (a primary, two replicas, three sentinels) and a cluster (three primaries with two replicas each), and runs the `TestHA` tests of `internal/redisutil` with `durable_writes.replicas: 1`:

- **Sentinel**: three wrong attempts at a code and an administrator's logout, then the primary is killed. After the promotion, the session still holds three attempts, so two more lock it; the token is still blacklisted; and a new sign-in works.
- **Cluster**: sixty sign-ins whose keys spread over every primary, which fails with `CROSSSLOT` if a script mixes slots. Then the primary of one session is killed: its replica serves the session within seconds, with the attempts made, and sign-in works again.

The **Redis failover drill** workflow runs it on the pull requests that change the Redis code, every month, and on demand. Run it locally from the repository root with Docker and Go.
