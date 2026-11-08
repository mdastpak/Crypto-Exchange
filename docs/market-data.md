# Global market data catalog

`cmd/market-worker` collects the metadata of crypto assets and the markets of exchanges from reputable global sources, and keeps them up to date ([#262](https://github.com/mdastpak/Crypto-Exchange/issues/262)). The data serves per-exchange price and market charts, so that users can compare exchanges, and later the blog service, as SEO data for each asset.

The catalog is separate from the currencies listed on this exchange. Its assets are **unverified**: nothing in the catalog makes an asset tradable. An administrator may link a catalog asset to a listed currency, which changes nothing on the currency.

This is not the realtime price aggregator of trading ([#58](https://github.com/mdastpak/Crypto-Exchange/issues/58)), and not the storage of this exchange's own trades and candles ([#80](https://github.com/mdastpak/Crypto-Exchange/issues/80), [#65](https://github.com/mdastpak/Crypto-Exchange/issues/65)).

## Status

| Part | Issue | Status |
| --- | --- | --- |
| Tables, store, and `cmd/market-worker` with its probes | [#263](https://github.com/mdastpak/Crypto-Exchange/issues/263) | Done |
| Job runner: each run once across workers, pause, dry run, jobs by reachable source | [#264](https://github.com/mdastpak/Crypto-Exchange/issues/264) | Done |
| Source interfaces, rate-limited HTTP client with egress per node, stub source | [#265](https://github.com/mdastpak/Crypto-Exchange/issues/265) | Done |
| Catalog from CoinGecko and CoinMarketCap | [#266](https://github.com/mdastpak/Crypto-Exchange/issues/266) | Done |
| Tickers of global and Iranian exchanges | [#267](https://github.com/mdastpak/Crypto-Exchange/issues/267) | Done |
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

## Sources

The jobs see the sources only through three interfaces of `internal/marketdata/source` ([#265](https://github.com/mdastpak/Crypto-Exchange/issues/265)), so that a source can be added, its plan changed, or a stub put in its place without touching the jobs:

| Interface | Gives | Sources |
| --- | --- | --- |
| `CatalogSource` | Assets: a listing with symbol, name, rank, and supply, and one asset in full with its description, links, and contracts | CoinGecko, CoinMarketCap ([#266](https://github.com/mdastpak/Crypto-Exchange/issues/266)) |
| `VenueSource` | The latest ticker of every pair of one exchange | Global and Iranian exchanges ([#267](https://github.com/mdastpak/Crypto-Exchange/issues/267)) |
| `CandleSource` | The closed candles of a pair | The same exchanges ([#268](https://github.com/mdastpak/Crypto-Exchange/issues/268)) |

Every source calls its API through a `Client`, which:

- keeps every amount as a decimal: JSON numbers are decoded as `json.Number` and never pass through a float;
- retries network errors, `429`, and `5xx` up to four attempts, waiting as `Retry-After` asks (a wait longer than a minute fails the request instead, and the run backs off), or 500 ms doubled per attempt with jitter; a `404` is `ErrNotFound`, and other answers fail at once;
- stops calling a source after five failed requests in a row, for a minute, and then lets one request through to see whether it has recovered (each worker keeps its own circuit);
- bounds every answer to 16 MiB and every request to the source's `timeout_seconds`;
- sends the API key in the header the source expects, never in the URL, and keeps the proxy out of its errors, since its address may hold credentials.

A stub catalog and a stub venue (`source/stub`) answer from fixed data, for tests and for development without network access or keys. Their data covers a coin, a token on several networks, two assets with one symbol, a rebranded token, and an amount that only a decimal keeps. `source/sourcetest` checks that a source keeps the contract of its interface; every source, real or stub, passes the same checks.

### Limits of the plans

The free and paid plans of the sources limit requests per minute, and some per month. `requests_per_minute` and `monthly_requests` of each source are counted in `market_source_usage` (migration 40) across every worker, in the calendar minute and month of the database's clock, so that the workers together keep within the plan however many there are. A request in a full minute waits for the next one; a request past the monthly limit fails with `ErrBudgetExhausted`, and the job backs off until the month turns or the limit is raised. The table holds one row per source and window, so it never grows.

### Egress per node

Each node says in its own configuration how it reaches the sources, so that one release runs on nodes with direct access to the internet, nodes behind a proxy or VPN gateway, and nodes in Iran that reach only the Iranian exchanges:

- `market_data.egress.default` is the route of the node: `direct`, `proxy` (through `egress.proxy_url`, `http://`, `https://`, or `socks5://`), or `none`.
- `market_data.sources.<name>.egress` overrides it for one source, such as an Iranian exchange that a node behind a foreign proxy reaches directly.
- A source that is not enabled on a node, or whose route is `none`, is not called there, and the node does not run its jobs; another node that reaches it does.
- The proxy variables of the environment (`HTTPS_PROXY`) are ignored, so that the configuration alone decides the route.

For example, a node in Iran that reaches the global sources through a SOCKS5 gateway and the Iranian exchanges directly:

```yaml
market_data:
  enabled: true
  mode: live
  egress:
    default: proxy             # MARKET_DATA_PROXY_URL=socks5://user:pass@gw.example:1080
  sources:
    coingecko:
      enabled: true
      requests_per_minute: 25
      monthly_requests: 9000
    nobitex:
      enabled: true
      egress: direct
```

## Catalog

The catalog sync ([#266](https://github.com/mdastpak/Crypto-Exchange/issues/266)) is one job per catalog source, `catalog:coingecko` and `catalog:coinmarketcap`, every `market_data.catalog.interval_hours` (24). A run:

1. reads the listing of the source, which gives the symbol, name, logo, rank, and supply of many assets per request;
2. matches each asset to the catalog, and writes what changed, in batches of 50 between two checks of its lease;
3. reads at most `details_per_run` full records (description, links, contracts, coin or token), first those never read, then those older than `details_max_age_days`, the better-ranked first, so that a catalog of every asset fills over several runs within the plan;
4. in the runs of the primary source with the `all` coverage, marks inactive the assets that no source listed for `deactivate_after_days`. An inactive asset stays, and a later listing makes it active again.

### Coverage

| `coverage` | Assets | For |
| --- | --- | --- |
| `list` (default) | A built-in set of 16 assets that give useful cases with few requests: BTC, ETH, USDT and USDC on several networks, BNB, SOL, XRP, TRX, TON, DOGE, the MATIC to POL rebrand, the LUNA and LUNC split, UNI (a symbol that other assets share), and WBTC | Development and tests |
| `top` | The first `top` (500) assets by market cap | Staging, or a small plan |
| `all` | Every asset of the sources | Production, with a paid plan |

With the free plans, `all` fills the listing in one run (about 70 requests at CoinGecko, a few credits at CoinMarketCap), and the records over many runs, `details_per_run` at a time.

### Matching assets

A symbol alone never merges two assets, since symbols collide (`UNI` is several tokens). An asset of a source is matched, in this order:

1. by its identity at the source (`market_asset_sources`);
2. by the contract of a token on a network (`market_asset_contracts`). Contracts that point at two different assets are an `identity` conflict;
3. a coin with the same symbol and the same name (`Bitcoin`) as an asset that has no identity at this source yet is the same coin;
4. otherwise it is a new asset; but a secondary source that lists a symbol the catalog already has records a `symbol` conflict and creates nothing, since it may be the same asset under another name.

`market_data.catalog.priority` orders the sources, primary first (`coingecko`, then `coinmarketcap`). The primary source writes the fields of the assets it knows; a secondary source links its identities to them, and writes the fields of the assets that only it knows. When the primary source is not enabled on any node, put the other first.

A full record that gives a token's contract that the catalog has under another asset records a `contract` conflict, and the contract stays with its first asset. Conflicts are kept in `market_identity_conflicts`, once while they are open, for an administrator to resolve ([#270](https://github.com/mdastpak/Crypto-Exchange/issues/270)).

### Sources of the catalog

| Source | Plan | Requests |
| --- | --- | --- |
| CoinGecko | The free public API or the demo plan at `api.coingecko.com`, with the key (optional) in `x-cg-demo-api-key`; a paid plan sets `base_url` to `https://pro-api.coingecko.com/api/v3` and its key goes in `x-cg-pro-api-key` | `/coins/markets` (250 per page), `/coins/{id}` (one per record) |
| CoinMarketCap | Every plan, the free Basic plan too, needs a key in `X-CMC_PRO_API_KEY`; set `MARKET_DATA_COINMARKETCAP_API_KEY` | `/v1/cryptocurrency/listings/latest` (one credit per 200 assets), `/v2/cryptocurrency/info` (100 records per request) |

Descriptions are stored as plain text, without HTML; only `https` links and logos are kept. Each asset carries the attribution that the source's terms ask for. The tests read recorded answers in the documented format of each API, since CI has no network; run a new source in `dry_run` first and check its runs.

## Tickers

The ticker jobs ([#267](https://github.com/mdastpak/Crypto-Exchange/issues/267)) are one job per venue, `tickers:binance`, `tickers:nobitex`, every `market_data.tickers.interval_seconds` (120), so that the workers share the venues and a venue that fails or is out of reach stops none of the others. A venue is collected on the nodes where its source is enabled and reachable, and only while it is enabled in `market_venues`: a new venue starts enabled, and the job never turns a venue back on that an administrator turned off.

| Venue | Region | Endpoints (public, no key) | Notes |
| --- | --- | --- | --- |
| `binance` | global | `/api/v3/ticker/24hr`, `/api/v3/exchangeInfo` (read once an hour) | Trading spot pairs only |
| `okx` | global | `/api/v5/market/tickers?instType=SPOT` | |
| `kraken` | global | `/0/public/Ticker`, `/0/public/AssetPairs` (read once an hour) | `XBT` is `BTC` and `XDG` is `DOGE`; no time and no 24-hour open |
| `coinbase` | global | `/api/v3/brokerage/market/products?product_type=SPOT` | Last price and 24-hour volume only; no time |
| `nobitex` | ir | `/market/stats` | Rial markets are reported in tomans (`IRT`), divided by 10 exactly |
| `wallex` | ir | `/v1/markets` | `TMN` markets are reported as `IRT` |

Where a venue gives no time, the time the ticker is read is its time. A ticker with no positive last price is skipped, and a bid above the ask, which a book shows between two updates, is left out.

A run reads every ticker of the venue, adds the pairs it does not know, and writes the pairs and the tickers in batches of 500, one statement each. A ticker only replaces an older observation. Pairs that the venue has not listed for `deactivate_pairs_after_hours` (24) become inactive.

### Mapping pairs

The base and quote of a new pair are mapped to the catalog by symbol: to the only asset with that symbol that is not hidden, or, when several assets share it, to the only one ranked within `map_rank_limit` (1000) by market cap. Otherwise the pair stays unmapped for review ([#270](https://github.com/mdastpak/Crypto-Exchange/issues/270)). A pair keeps its mapping once set, so that a run never undoes an administrator's mapping. Fiat quotes such as `USD`, `EUR`, and `IRT` are not catalog assets and stay unmapped.

### Stale prices and outliers

Whether a ticker is stale or an outlier is decided when it is read (`marketdata.Flag`), since both change with time and with the other venues:

- **Stale**: observed more than `stale_after_seconds` (600) ago.
- **Outlier**: its last price differs from the median of the fresh prices of the same quote by more than `outlier_percent` (5 %). The median counts one price per venue, and needs at least `min_venues` (3) venues; with fewer, nothing is an outlier. A stale price never moves the median and is never an outlier. The median is exact in decimals.

Prices in different quotes (`USDT`, `USD`, `IRT`) are never compared with each other.

## Data model

The tables have the `market_` prefix, and no other table refers to them, so that the catalog can move to a database of its own. Amounts are `NUMERIC`, never floating point. Nothing is deleted: an asset or a pair that a source drops is marked inactive.

| Table | Contents |
| --- | --- |
| `market_assets` | One row per asset, not per symbol, since symbols collide between assets. The slug names it in URLs (`bitcoin`; a second asset with the same name gets `-2`). The status is `unverified` (served with a label), `hidden` (never served), or `linked` (to a listed currency, `currency_hk`, which a constraint requires). Plain-text description, `https` links and logo, rank, supply, and the attribution that the source's license asks for. |
| `market_asset_sources` | The identity of an asset at each source, such as its CoinGecko id; one asset per identity. `details_fetched_at` is when its full record was last read. |
| `market_asset_contracts` | The contract of a token on each network, which matches the same token across sources. EVM addresses are stored in lower case, since their case is only a checksum; other networks keep their case. |
| `market_venues` | The exchanges: kind (`cex`, `dex`), region (`global`, or `ir` for the Iranian exchanges, which an international release leaves out, [#272](https://github.com/mdastpak/Crypto-Exchange/issues/272)), country, website, attribution, and whether the jobs collect it. The jobs refresh the description of a venue, never its `enabled` setting, which belongs to the administrators. |
| `market_venue_pairs` | A pair at a venue (`BTCUSDT` at `binance`), mapped to the base and quote assets of the catalog once known. |
| `market_tickers_latest` | The latest ticker of each pair: last price, bid, ask, and the 24-hour volume, high, low, and open, with the time of the venue and the time received. |
| `market_identity_conflicts` | What the catalog sync could not decide alone: the source and its id, the kind (`contract`, `identity`, `symbol`), the subject, the assets involved, and when an administrator resolved it. |
| `market_sync_runs` | One row per job run: job, worker, mode, status, items read and written, and the error. |

### Writes from several workers

- `CreateAsset` inserts the asset and its identity in one transaction. When another worker created the same identity a moment before, it rolls back and returns that asset.
- `UpdateAsset` writes only when a value differs, so that a run that finds nothing new writes nothing; supplies equal in value (`1` and `1.000`) are the same.
- `LinkSource` and `AddContract` keep the first owner of an identity or a contract and return it, so that the job reports a conflict instead of moving it.
- `UpsertTicker` replaces a ticker only with a later observation, so that a slow worker or a late response never puts an older price back.
