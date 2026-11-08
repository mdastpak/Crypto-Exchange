# Global market data catalog

`cmd/market-worker` collects the metadata of crypto assets and the markets of exchanges from reputable global sources, and keeps them up to date ([#262](https://github.com/mdastpak/Crypto-Exchange/issues/262)). The data serves per-exchange price and market charts, so that users can compare exchanges, and later the blog service, as SEO data for each asset.

The catalog is separate from the currencies listed on this exchange. Its assets are **unverified**: nothing in the catalog makes an asset tradable. An administrator may link a catalog asset to a listed currency, which changes nothing on the currency.

This is not the realtime price aggregator of trading ([#58](https://github.com/mdastpak/Crypto-Exchange/issues/58)), and not the storage of this exchange's own trades and candles ([#80](https://github.com/mdastpak/Crypto-Exchange/issues/80), [#65](https://github.com/mdastpak/Crypto-Exchange/issues/65)).

## Status

| Part | Issue | Status |
| --- | --- | --- |
| Tables, store, and `cmd/market-worker` with its probes | [#263](https://github.com/mdastpak/Crypto-Exchange/issues/263) | Done |
| Job runner: each run once across workers, pause, dry run, jobs by reachable source | [#264](https://github.com/mdastpak/Crypto-Exchange/issues/264) | Done |
| Source interfaces, rate-limited HTTP client with egress per node, stub source | [#265](https://github.com/mdastpak/Crypto-Exchange/issues/265) | Planned |
| Catalog from CoinGecko and CoinMarketCap | [#266](https://github.com/mdastpak/Crypto-Exchange/issues/266) | Planned |
| Tickers of global and Iranian exchanges | [#267](https://github.com/mdastpak/Crypto-Exchange/issues/267) | Planned |
| Candles | [#268](https://github.com/mdastpak/Crypto-Exchange/issues/268) | Planned |
| Public read API | [#269](https://github.com/mdastpak/Crypto-Exchange/issues/269) | Planned |
| Administration | [#270](https://github.com/mdastpak/Crypto-Exchange/issues/270) | Planned |
| Feed for the blog | [#271](https://github.com/mdastpak/Crypto-Exchange/issues/271) | Planned |

## Running the worker

```bash
go run ./cmd/market-worker -config config/config.local.yaml
```

The Docker image holds the worker as `/app/market-worker`; run it with `--entrypoint /app/market-worker`. It reads the same configuration file as the server, and needs only the `postgres` and [`market_data`](configuration.md#market_data) sections. Like the server, it applies the migrations at startup under the migration lock, unless `postgres.migrations.mode` is `external`.

| Probe | Answer |
| --- | --- |
| `GET /system/live` | `200` while the process runs |
| `GET /system/ready` | `200` while PostgreSQL answers; `503` with `postgres_unavailable` while it does not, and with `draining` once the worker stops |

The probes listen on `market_data.health_addr` (`:8081`).

### Rollout

1. Deploy the worker with `market_data.enabled: false`: it only answers its probes.
2. Enable it with `market_data.mode: dry_run`: the jobs collect and check what they would write, and record only their runs.
3. Switch to `live` once the runs look right.

Several workers may run at once, on different nodes; every write is safe when they race, and each job run happens on one of them.

## Jobs

Every worker runs the same jobs, and each run of a job happens on one worker ([#264](https://github.com/mdastpak/Crypto-Exchange/issues/264)). A job whose work splits, such as the tickers of each exchange, is one job per shard (`tickers:binance`, `tickers:nobitex`), so that the workers share the work.

- **Leases.** `market_job_leases` holds one row per job: who holds it, a token, until when, and when it is due next. A worker claims a due job that nobody holds by raising the token, in one `UPDATE`, so that of several workers that try at once one wins. It renews the lease every third of `market_data.lease_seconds` while the run goes on. A worker that dies stops renewing, and another worker claims the job once the lease has passed. Every time is the database's, so that the clocks of the workers do not matter. Session advisory locks are not used, since they do not work behind PgBouncer in transaction mode.
- **Fencing.** A job calls `Run.Fence` before each batch of writes. It fails with `ErrLeaseLost` when the token changed, because another worker took the job after this one could not renew it, and the run stops. A worker that lost its lease also leaves the new holder's lease alone. The writes of the store are idempotent and never put an older value back, so a batch written just as a lease passes does no harm.
- **Reachable sources.** A job names the sources it calls, and a worker runs only the jobs whose sources it reaches with its configuration, so that a node in Iran without a proxy still collects the Iranian exchanges while a node with global access collects the rest ([#265](https://github.com/mdastpak/Crypto-Exchange/issues/265)).
- **Schedule.** A job is due again its interval after a successful run. After a failed run it is due after `market_data.retry_base_seconds`, doubled with each further failure in a row up to its interval, with a jitter of ±20 % so that the workers do not retry a source in step. A panic fails the run, not the worker.
- **Pause.** A [lockdown](api.md#maintenance-and-lockdowns) of `market_data`, through `/admin/platform` or `cmd/admin platform lockdown -feature market_data`, pauses every job on every worker within `platform.poll_interval_ms`: no run starts, and a run in progress stops at its next `Fence` and is due again once the lockdown ends. Users never see it. Unlike the money features, an unknown state of the controls pauses nothing.
- **Dry run.** In `dry_run` mode a job collects and checks, writes nothing, and counts what it would write; the runs are recorded with their mode.
- **Shutdown.** A stopping worker waits for its runs until the shutdown deadline (30 seconds), then cancels them; their jobs are due at once for another worker.

Each run is recorded in `market_sync_runs`, and logged with its time and counts.

## Data model

The tables have the `market_` prefix, and no other table refers to them, so that the catalog can move to a database of its own. Amounts are `NUMERIC`, never floating point. Nothing is deleted: an asset or a pair that a source drops is marked inactive.

| Table | Contents |
| --- | --- |
| `market_assets` | One row per asset, not per symbol, since symbols collide between assets. The slug names it in URLs (`bitcoin`; a second asset with the same name gets `-2`). The status is `unverified` (served with a label), `hidden` (never served), or `linked` (to a listed currency, `currency_hk`, which a constraint requires). Plain-text description, `https` links and logo, rank, supply, and the attribution that the source's license asks for. |
| `market_asset_sources` | The identity of an asset at each source, such as its CoinGecko id; one asset per identity. |
| `market_asset_contracts` | The contract of a token on each network, which matches the same token across sources. EVM addresses are stored in lower case, since their case is only a checksum; other networks keep their case. |
| `market_venues` | The exchanges: kind (`cex`, `dex`), region (`global`, or `ir` for the Iranian exchanges, which an international release leaves out, [#272](https://github.com/mdastpak/Crypto-Exchange/issues/272)), country, website, attribution, and whether the jobs collect it. The jobs refresh the description of a venue, never its `enabled` setting, which belongs to the administrators. |
| `market_venue_pairs` | A pair at a venue (`BTCUSDT` at `binance`), mapped to the base and quote assets of the catalog once known. |
| `market_tickers_latest` | The latest ticker of each pair: last price, bid, ask, and the 24-hour volume, high, low, and open, with the time of the venue and the time received. |
| `market_sync_runs` | One row per job run: job, worker, mode, status, items read and written, and the error. |

### Writes from several workers

- `CreateAsset` inserts the asset and its identity in one transaction. When another worker created the same identity a moment before, it rolls back and returns that asset.
- `UpdateAsset` writes only when a value differs, so that a run that finds nothing new writes nothing; supplies equal in value (`1` and `1.000`) are the same.
- `LinkSource` and `AddContract` keep the first owner of an identity or a contract and return it, so that the job reports a conflict instead of moving it.
- `UpsertTicker` replaces a ticker only with a later observation, so that a slow worker or a late response never puts an older price back.
