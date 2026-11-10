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
| Candles | [#268](https://github.com/mdastpak/Crypto-Exchange/issues/268) | Done |
| Public read API | [#269](https://github.com/mdastpak/Crypto-Exchange/issues/269) | Done |
| Collection tiers: finer candles for the pairs that matter | [#279](https://github.com/mdastpak/Crypto-Exchange/issues/279) | Done |
| Live prices of hot pairs from WebSocket streams | [#281](https://github.com/mdastpak/Crypto-Exchange/issues/281) | Done |
| Administration | [#270](https://github.com/mdastpak/Crypto-Exchange/issues/270) | Done |
| Feed for the blog | [#271](https://github.com/mdastpak/Crypto-Exchange/issues/271) | Done |

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

The free and paid plans of the sources limit requests per minute, and some per month. `requests_per_minute` and `monthly_requests` of each source are counted in `market_source_usage` across every worker, in the calendar minute and month of the database's clock, so that the workers together keep within the plan however many there are. A request in a full minute waits for the next one; a request past the monthly limit fails with `ErrBudgetExhausted`, and the job backs off until the month turns or the limit is raised. The table holds one row per source and window, so it never grows.

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

A full record that gives a token's contract that the catalog has under another asset records a `contract` conflict, and the contract stays with its first asset. Conflicts are kept in `market_identity_conflicts`, once while they are open, for an administrator to [resolve](#administration).

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

The base and quote of a new pair are mapped to the catalog by symbol: to the only asset with that symbol that is not hidden, or, when several assets share it, to the only one ranked within `map_rank_limit` (1000) by market cap. Otherwise the pair stays unmapped, for an administrator to [map](#administration). A pair keeps its mapping once set, so that a run never undoes an administrator's mapping. Fiat quotes such as `USD`, `EUR`, and `IRT` are not catalog assets and stay unmapped.

### Stale prices and outliers

Whether a ticker is stale or an outlier is decided when it is read (`marketdata.Flag`), since both change with time and with the other venues:

- **Stale**: observed more than `stale_after_seconds` (600) ago.
- **Outlier**: its last price differs from the median of the fresh prices of the same quote by more than `outlier_percent` (5 %). The median counts one price per venue, and needs at least `min_venues` (3) venues; with fewer, nothing is an outlier. A stale price never moves the median and is never an outlier. The median is exact in decimals.

Prices in different quotes (`USDT`, `USD`, `IRT`) are never compared with each other.

## Candles

The candle jobs ([#268](https://github.com/mdastpak/Crypto-Exchange/issues/268)) keep the OHLCV history of the pairs for the price charts of each venue: one job per venue and resolution, `candles:binance:1h`, every `interval_minutes` of the resolution. Each resolution keeps the pairs of some [collection tiers](#collection-tiers) only:

| Resolution | Tiers | Runs every | History | Kept |
| --- | --- | --- | --- | --- |
| `1d` | all | 6 hours | 10 years | Always |
| `1h` | all | hour | 180 days | 180 days |
| `15m` | hot, active | 15 minutes | 90 days | 90 days |
| `5m` | hot, active | 5 minutes | 30 days | 30 days |
| `1m` | hot | minute | 1 day | 7 days |

The pairs are the active pairs of the venue whose base is in the catalog, whose quote is in `candles.quotes` (`USDT`, `USD`, `IRT`), and whose tier the resolution keeps, at most `max_pairs_per_venue` (200), the best-ranked bases first. A run asks the venue for ranges of at most one request's candles per pair, within `requests_per_run` (300), round-robin over the pairs:

1. A new pair gets its latest candles first, so that it charts at once.
2. Forward: from what was last asked up to the last closed candle.
3. Backward: down to the history depth, in later runs if the budget runs out.

`market_candle_cursors` keeps, per pair and resolution, the range `[from_time, until_time)` that was asked, with the candles in the same transaction. So a restart resumes where the last run stopped, a repeated request does no harm (candles upsert on pair, resolution, and open time), and a range the venue had no candles for, such as before a pair was listed, is not asked again. A pair the venue no longer knows is skipped. Only closed candles are stored, aligned to UTC.

| Venue | Endpoint | Candles per request | Notes |
| --- | --- | --- | --- |
| `binance` | `/api/v3/klines` | 1000 | |
| `okx` | `/api/v5/market/history-candles` | 100 | UTC days (`1Dutc`) |
| `kraken` | `/0/public/OHLC` | 720 | Only the last 720 candles, so no deeper history |
| `coinbase` | `/api/v3/brokerage/market/products/{id}/candles` | 350 | |
| `nobitex` | `/market/udf/history` | 500 | Intraday resolutions only, since its days are Tehran's; rials in tomans |
| `wallex` | `/v1/udf/history` | 500 | Intraday resolutions only |

### Partitions and retention

`market_candles` is partitioned by resolution, then by day (`1m`), month (`5m`, `15m`, `1h`), or year (`1d`), such as `market_candles_1h_2026_11` or `market_candles_1m_2026_11_08`. The job `candles:partitions`, every 6 hours on any worker, creates the partitions of the current and the next period, and drops those whose candles all fall outside `retention_days`, so old candles go without a `DELETE` and without bloat; a write also creates the partition it needs. The runtime role cannot create or drop tables: it calls `market_candles_ensure_partition` and `market_candles_drop_partitions`, which run as the migration role and touch only these partitions (`internal/db.RuntimeFunctions`). Retention is in whole partitions, so candles are kept up to a day (`1m`), a month, or a year (`1d`) longer than `retention_days`.

### Rate limits

With the default tiers, a venue with 30 hot pairs, 150 active ones (of the 200 over every venue), and 200 pairs in all asks, per hour:

| Resolution | Pairs | Runs per hour | Requests per hour |
| --- | --- | --- | --- |
| `1m` | 30 hot | 60 | 1,800 |
| `5m` | 180 hot and active | 12 | 2,160 |
| `15m` | 180 hot and active | 4 | 720 |
| `1h` | 200 | 1 | 200 |
| `1d` | 200 | 1/6 | 33 |
| | | | about 4,900, or 82 per minute |

That fits Binance (600 per minute), OKX and Coinbase (300), but not Kraken at about 50 per minute: there, set fewer pairs, or a longer `interval_minutes` for `5m`, or leave `1m` to [#281](https://github.com/mdastpak/Crypto-Exchange/issues/281)'s stream. The first backfill adds to it for a few days: 30 days of `5m` is 9 requests per pair at Binance, 87 at OKX.

A day of `1h` candles for 1,000 pairs of one venue, with `max_pairs_per_venue: 1000` and `requests_per_run: 1000`: each hourly run asks one request per pair for the one new candle, so 1,000 requests per run and 24,000 per day. The client spreads them within the source's `requests_per_minute`:

| Venue | Public limit | `requests_per_minute` | One run of 1,000 requests |
| --- | --- | --- | --- |
| Binance | 6,000 weight per minute; `klines` weighs 2 | 600 | under 2 minutes, 2,000 weight |
| OKX | 20 requests per 2 seconds for `history-candles` | 300 | under 4 minutes |
| Coinbase | 10 requests per second | 300 | under 4 minutes |
| Kraken | about 1 request per second | 50 | 20 minutes |

Each run fits well within the hour, and the 30-minute timeout of a run, except Kraken at 1,000 pairs, which should keep `max_pairs_per_venue` at about 500 or run every two hours. The Iranian venues publish no limits; start them at 60 per minute (about 17 minutes per run). The first backfill of 180 days of `1h` is 4,320 candles per pair: 5 requests per pair at Binance, 44 at OKX, spread over the runs by `requests_per_run`.

## Collection tiers

The pairs are in collection tiers ([#279](https://github.com/mdastpak/Crypto-Exchange/issues/279)), which decide at which [candle resolutions](#candles) a pair is collected, so that the pairs users watch most get fine candles, and the rest only hourly and daily ones, within the venues' request limits and the size of the database. The job `tiers`, every `tiers.interval_hours` (24) on any worker, puts each active pair whose base is in the catalog in a tier:

| Tier | Pairs |
| --- | --- |
| `hot` | The base is one of `tiers.hot_assets` (`bitcoin`, `ethereum`, `tether`), or, with `hot_listed`, is linked to a currency listed on this exchange; and the quote is one of `candles.quotes`. So BTC/USDT, ETH/IRT, and USDT/IRT are hot. |
| `active` | The next `tiers.active_top` (200) pairs over every venue by 24-hour volume in USD, with a quote of `candles.quotes`. |
| `other` | The rest. |

- **Volume in USD.** The quote volume of the latest ticker, or the base volume at the last price, times the price of the quote in USD: one for `USD`, `USDT`, `USDC`, and other dollar coins; for `IRT`, from the USDT/IRT price at the same venue; for a coin such as `BTC`, from its dollar pair at the same venue. Without such a pair at the venue, the median over the venues is used.
- **Hysteresis.** A pair enters `active` within the top `active_top`, and leaves only when its rank passes `active_top` by `hysteresis_percent` (20 %): with the defaults, it stays until rank 240. So a pair near the boundary does not move between tiers every day, and the tier can hold up to 20 % more pairs than `active_top`.
- **Overrides.** An administrator's tier on a pair, or on its base asset for all its pairs, overrides the automatic one (`tier_override`, set through the [administration](#administration)). The pair's own override wins over its asset's. The job still records the automatic tier, which applies again once the override is cleared.
- **Tickers.** The REST ticker endpoints of the venues give every pair in one request, so the tiers change nothing in what tickers cost. The hot pairs get second-level prices from the venues' [WebSocket streams](#live-tickers).

A pair that moves to a finer tier starts its finer candles from the latest ones, then backfills, as a new pair does. A pair that leaves a tier keeps the candles it has until their retention.

## Live tickers

The hot pairs get their tickers seconds old from the venues' public WebSocket streams ([#281](https://github.com/mdastpak/Crypto-Exchange/issues/281)), where the REST ticker job gives them every two minutes.

| Venue | Stream | Channel |
| --- | --- | --- |
| `binance` | `wss://stream.binance.com:9443/ws` | `<symbol>@ticker`, every second |
| `okx` | `wss://ws.okx.com:8443/ws/v5/public` | `tickers`, with a `ping` every 20 seconds |
| `kraken` | `wss://ws.kraken.com/v2` | `ticker`, by `BTC/USD` |
| `coinbase` | `wss://advanced-trade-ws.coinbase.com` | `ticker`, with `heartbeats` |

Nobitex and Wallex have no public ticker stream that this follows; their pairs keep their REST tickers. The protocols are written from the venues' documentation and tested against recorded messages only, so try a stream in `dry_run` first.

- **One worker per stream.** Each venue's stream is a job, `stream:binance`, so its lease lets one worker at a time hold it, and another worker takes it over once that worker dies or loses the database: the old holder's next write fails its fence, and it stops. A run holds the connection for `streams.run_minutes` (60), then ends, and the stream is due again at once on whichever worker claims it, so that the workers share the streams.
- **Own slots.** The streams run in a runner of their own, at most `streams.max_per_worker` (2) on a worker, so that they never take the slots of `max_concurrent_jobs`.
- **Reconnects.** A dropped connection, or one quiet for `streams.quiet_seconds` (60), reconnects after a backoff that doubles from a second up to a minute, with jitter, and subscribes again. After 5 connections in a row that failed before any ticker, the run fails and the runner backs off, so that another worker, perhaps with another route, may try.
- **Tiers.** A stream follows the pairs of `streams.tiers` (`hot`), at most `streams.max_pairs` (200). It reads them every 5 minutes, and reconnects with the new subscription when they changed.
- **Throttled writes.** The tickers received are kept per pair, the latest only, and written together every `streams.flush_millis` (1000), so a pair is written at most once a second, however often the venue sends, after a check of the lease.
- **Fallback.** The REST ticker job keeps running for every pair, the hot ones too, so while a stream is down its pairs still get a ticker every `tickers.interval_seconds`. A ticker replaces only an older one, so the stream and the REST job never put an older price back.
- **Egress.** A stream goes the route of its venue's source: direct, or through `egress.proxy_url` (`http`, `https` with CONNECT, or `socks5`). A failure through a proxy is logged without its address.
- **Candles.** The `1m` candles of the hot pairs stay the venue's closed candles over REST, a minute behind, so that the charts hold the venue's own figures.

## Public API

The server serves the catalog read only, without sign-in, under `/api/v1/market-data` ([#269](https://github.com/mdastpak/Crypto-Exchange/issues/269)); the [API reference](api.md#market-data) and the OpenAPI document describe each route.

| Route | Answer |
| --- | --- |
| `GET /market-data/assets?q=&sort=&limit=&cursor=` | The assets, [paginated](api.md#pagination): by market cap rank, the unranked last (`sort=rank`, the default), or the newest first (`sort=newest`, [Newest assets](#newest-assets)). `q` matches the exact symbol, or a part of the name or slug, in either order. |
| `GET /market-data/assets/{slug}` | An asset with its description, links, supply, contracts, and attribution. Every asset also carries `created_at`, when the catalog first collected it. |
| `GET /market-data/assets/{slug}/venues` | The latest ticker at each venue, with `stale`, `outlier`, and the `median` of its quote ([Stale prices and outliers](#stale-prices-and-outliers)). |
| `GET /market-data/assets/{slug}/candles?venue=&quote=&interval=&from=&to=` | The candles of the asset's pair at a venue, at most 1000; the last 500 by default. |
| `GET /market-data/feed/assets?updated_since=&limit=&cursor=` | The assets in the order they changed, for a service that keeps a copy, such as the blog ([Feed for the blog](#feed-for-the-blog)). |

- **Unverified by default.** Every asset carries `verified` and `label`: `unverified`, unless an administrator linked it to a listed currency. A hidden asset is not found, in the list, the search, or by its slug.
- **Regions.** `market_data.api.regions` limits the venues shown, so that an international release leaves out the Iranian venues. An international release (`deployment.region: international`) defaults it to `[global]` and refuses `ir`, the Iranian exchanges, and `IRT` candles ([#272](https://github.com/mdastpak/Crypto-Exchange/issues/272), see [Region of a release](configuration.md#region-of-a-release)).
- **Cache.** Each answer is cached in Redis for `market_data.api.cache_seconds` (30), shared by every instance, under a key that holds a generation number. `Invalidate` raises the generation (`INCR`), so that every instance stops serving the answers cached before at once; the [administration](#administration) calls it after each change. Only successful answers are cached. Without Redis, the answers are built on every request.
- **ETag.** Every answer has an `ETag`, the same on every instance for the same answer, and `Cache-Control: public, max-age=<cache_seconds>`; a request with a matching `If-None-Match` gets `304 Not Modified`.
- **Limits.** The routes fall under the REST rate limit of the public routes, and close during a full maintenance, as the other public routes do.

### Newest assets

`sort=newest` lists the assets that are not hidden by `created_at`, the time the catalog first collected them, newest first, then by slug ([#304](https://github.com/mdastpak/Crypto-Exchange/issues/304)); the blog's "newest coins" page reads it. It is the time this catalog first saw the asset, not the launch date of the coin: a source added later brings its older assets in as new. `market_assets_newest` (migration 50) indexes the order.

- **Cursor.** The cursor holds the time and slug of the last asset, so the next page starts right after it. An asset added between two pages is newer than the cursor, so it is neither repeated nor does it push another one out; it shows on the first page the next time. `created_at` is the start of the inserting transaction, so an asset whose transaction started before a page was read but committed after it can be missed by that walk through the pages, and appears on the next one from the start; the list is for browsing, and a service that must see every asset uses the [feed](#feed-for-the-blog).
- **One cursor, one order.** A cursor of one order is refused by the other with `400 validation_failed`, as is an unknown `sort`. The rank cursors given before `sort` existed still work.

## Feed for the blog

The blog service keeps its own copy of the catalog for its pages, and follows the changes with `GET /market-data/feed/assets` ([#271](https://github.com/mdastpak/Crypto-Exchange/issues/271)), a read API rather than events, so that it needs no queue and any server instance can answer it:

1. The first time, it reads from the beginning, or from `updated_since`, page after page while `has_more` is true.
2. It keeps the `next_cursor` of the last page, which every page has, and asks with it again later, such as every few minutes, for what changed since.

Each item is an asset that changed, in the order of `updated_at`, then the slug:

- **An asset** has every field of `GET /market-data/assets/{slug}`, its `attribution` with the link that the license of its source asks for (CoinGecko or CoinMarketCap), and `structured_data`, a schema.org `Thing` for the JSON-LD of its page, with its name, symbol, description, logo, official links as `sameAs`, and contracts as identifiers. The blog adds the URL of its page. Every asset is still `unverified` unless linked, and the page must say so.
- **A removed asset** is one that an administrator hid: only `slug`, `updated_at`, and `removed: true`. The blog takes its page down; if the asset is shown again, it comes back in the feed.

An asset comes again each time it changes, so the blog writes each item over its copy. The writes of the catalog set `updated_at` to the start of their transaction, so a change appears in the feed only once it is a minute old: a transaction that started earlier and commits later then still lands after the cursor, and no change is missed. A new contract of an asset changes it too. Answers are cached like the other routes, which delays a change by at most `market_data.api.cache_seconds` more.

## Administration

Administrators run the catalog through the admin routes under `/api/v1/admin/market-data` ([#270](https://github.com/mdastpak/Crypto-Exchange/issues/270)), which the [API reference](api.md#market-data-administration) describes, or `cmd/admin market-data` when the server is unhealthy. Both work on the same store, and every change is written with its audit event in one transaction (`market_data.*`, see [the audit log](architecture.md#the-audit-log)). A change that would change nothing, such as pausing a paused source, records nothing.

| Task | Route | Command |
| --- | --- | --- |
| See each source: paused, its jobs and their failures, its requests this minute and month | `GET /sources` | `market-data sources` |
| Pause a source, or resume it | `POST /sources/{source}/pause`, `/resume` | `market-data pause -source s -reason …`, `resume` |
| See the jobs: who runs each, its failures, its next run | `GET /jobs` | `market-data jobs` |
| Run a job now | `POST /jobs/{job}/run` | `market-data run -job j` |
| See the runs, newest first | `GET /runs?job=&status=` | `market-data runs` |
| Enable or disable a venue | `GET /venues`, `POST /venues/{id}/enable`, `/disable` | `market-data venues`, `venue-enable`, `venue-disable` |
| Find assets, hidden ones too | `GET /assets?status=&q=`, `GET /assets/{id}` | `market-data assets` |
| Hide an asset, or show it again | `POST /assets/{id}/hide`, `/unhide` | `market-data hide -asset n -reason …`, `unhide` |
| Link an asset to a listed currency, or unlink it | `POST /assets/{id}/link`, `/unlink` | `market-data link -asset n -currency hk`, `unlink` |
| Set or clear the tier of an asset or a pair | `PUT /assets/{id}/tier`, `PUT /pairs/{id}/tier` | `market-data tier -asset n` or `-pair n -tier …` |
| Map a pair that the jobs could not map | `GET /pairs?unmapped=true`, `PUT /pairs/{id}/mapping` | `market-data map -pair n -base n -quote n` |
| Resolve a conflict of identity | `GET /conflicts`, `POST /conflicts/{id}/resolve` | `market-data conflicts`, `resolve -id n -note …` |

- **Pausing a source.** A pause, in `market_source_controls`, stops every worker from claiming the jobs that call the source, `catalog:coingecko` or `tickers:binance`, `candles:binance:1h`, and `stream:binance` alike, at its next poll; a run in progress, a stream too, stops at its next batch of writes, and is not counted as a failure. The jobs stay due, and run once the source is resumed. Pausing a source on every node at once, whatever its configuration, is for a source that must be left alone now: a changed API, a complaint, a plan running out. The configuration still decides which sources a node uses; a lockdown of `market_data` pauses every job.
- **Disabling a venue.** The jobs of a disabled venue collect nothing, and the public API leaves it out. Its data is kept.
- **Hiding an asset.** The public API never serves a hidden asset, in the list, the search, or by its slug. The jobs keep collecting it, and it can be shown again.
- **Linking an asset.** A link says that a catalog asset is the same as a currency listed on this exchange; the public API then labels it verified, and with `tiers.hot_listed` its pairs are hot. The link only reads the currency, which must exist and not be deleted, and never changes it. A currency is linked to one asset at most, a linked asset is unlinked before it is linked to another currency or hidden, and a hidden asset is shown before it is linked.
- **Mapping a pair.** A pair that the [tickers job](#mapping-pairs) could not map to one asset by its symbols waits, unmapped, out of the comparison. An administrator maps it to its base and quote assets; the jobs keep a mapping once set.
- **Conflicts.** A conflict of identity is closed with a note of what was decided; the fix itself, such as hiding an asset or mapping a pair, is its own change. A conflict that the sync meets again later opens anew.
- **Running a job now.** The job becomes due now, and the first worker that polls and reaches its sources runs it, within `poll_seconds`. A job that a worker runs now answers `409 market_job_running`, since the end of that run sets the next one.
- **Cache.** A change through the routes drops the cache of the public API on every instance at once. `cmd/admin` does not reach Redis, so the public API shows its changes once the cached answers expire, within `api.cache_seconds`.

## Data model

The tables have the `market_` prefix, and no other table refers to them, so that the catalog can move to a database of its own. Amounts are `NUMERIC`, never floating point. Nothing is deleted: an asset or a pair that a source drops is marked inactive. Only the candles leave, a whole partition at a time, past their retention.

| Table | Contents |
| --- | --- |
| `market_assets` | One row per asset, not per symbol, since symbols collide between assets. The slug names it in URLs (`bitcoin`; a second asset with the same name gets `-2`). The status is `unverified` (served with a label), `hidden` (never served), or `linked` (to a listed currency, `currency_hk`, which a constraint requires). Plain-text description, `https` links and logo, rank, supply, and the attribution that the source's license asks for. |
| `market_asset_sources` | The identity of an asset at each source, such as its CoinGecko id; one asset per identity. `details_fetched_at` is when its full record was last read. |
| `market_asset_contracts` | The contract of a token on each network, which matches the same token across sources. EVM addresses are stored in lower case, since their case is only a checksum; other networks keep their case. |
| `market_venues` | The exchanges: kind (`cex`, `dex`), region (`global`, or `ir` for the Iranian exchanges, which an international release leaves out, [#272](https://github.com/mdastpak/Crypto-Exchange/issues/272)), country, website, attribution, and whether the jobs collect it. The jobs refresh the description of a venue, never its `enabled` setting, which belongs to the administrators. |
| `market_venue_pairs` | A pair at a venue (`BTCUSDT` at `binance`), mapped to the base and quote assets of the catalog once known, with its automatic [collection tier](#collection-tiers) and an administrator's `tier_override`. |
| `market_tickers_latest` | The latest ticker of each pair: last price, bid, ask, and the 24-hour volume, high, low, and open, with the time of the venue and the time received. |
| `market_identity_conflicts` | What the catalog sync could not decide alone: the source and its id, the kind (`contract`, `identity`, `symbol`), the subject, the assets involved, and when an administrator resolved it. |
| `market_candles` | The OHLCV candles of a pair at a resolution (`1m`, `5m`, `15m`, `1h`, `1d`), partitioned by resolution and time. |
| `market_candle_cursors` | Per pair and resolution, the range of candles that was asked of the venue. |
| `market_sync_runs` | One row per job run: job, worker, mode, status, items read and written, and the error. |
| `market_source_controls` | The sources that an administrator paused, with the reason ([Administration](#administration)). |

### Writes from several workers

- `CreateAsset` inserts the asset and its identity in one transaction. When another worker created the same identity a moment before, it rolls back and returns that asset.
- `UpdateAsset` writes only when a value differs, so that a run that finds nothing new writes nothing; supplies equal in value (`1` and `1.000`) are the same.
- `LinkSource` and `AddContract` keep the first owner of an identity or a contract and return it, so that the job reports a conflict instead of moving it.
- `UpsertTicker` replaces a ticker only with a later observation, so that a slow worker or a late response never puts an older price back.
