# Price engine

`cmd/price-aggregator` computes the reference price of each market from the books and trades of several providers ([#58](https://github.com/mdastpak/Crypto-Exchange/issues/58)). The reference prices quote the OTC trades ([#61](https://github.com/mdastpak/Crypto-Exchange/issues/61)) and feed the market-making bots ([#369](https://github.com/mdastpak/Crypto-Exchange/issues/369)). The code is in `internal/price`.

| Part | Issue | State |
| --- | --- | --- |
| Providers, aggregation, `cmd/price-aggregator`, metrics | [#58](https://github.com/mdastpak/Crypto-Exchange/issues/58) | Done |
| Price cache in Redis, distribution, consumer library, admin override | [#59](https://github.com/mdastpak/Crypto-Exchange/issues/59) | Done |

## Running

```bash
go run ./cmd/price-aggregator -config config/config.local.yaml
```

The Docker image holds it as `/app/price-aggregator`; run it with `--entrypoint /app/price-aggregator`. It reads the same configuration file as the server, and needs only the [`price_aggregator`](configuration.md#price_aggregator) section, with `market_data.egress.proxy_url` for a provider reached through a proxy. It needs no database; to publish, it needs Redis and RabbitMQ.

It answers on `price_aggregator.health_addr` (`:8082`), an internal address that must not be reachable from the public network. Release mode needs it to name a host, such as `127.0.0.1:8082` or the private address of the pod, never every interface ([#454](https://github.com/mdastpak/Crypto-Exchange/issues/454)):

| Route | What |
| --- | --- |
| `GET /system/live` | `200` while the process runs. |
| `GET /system/ready` | `200`, and `503` once it stops. |
| `GET /metrics` | The [metrics](#metrics). |
| `GET /prices` | The prices of the last interval, for operators. Needs `price_aggregator.admin_token`. |
| `GET /providers` | The state of each provider: stream open, REST fallback on, last tick, lag, reconnections, last error. Needs `price_aggregator.admin_token`. |

The probes and the metrics are open on that address. `/prices` and `/providers` need the bearer token `price_aggregator.admin_token` when it is set, and answer `401` without it. Without a token, release mode turns them off (`404`), and development leaves them open.

In release mode, the aggregator refuses to start with the settings of Redis and RabbitMQ that the server refuses: no Redis password, the `guest` account, or a connection without TLS that is not acknowledged as a private network. See [Release mode](configuration.md#release-mode).

## Several replicas

The engine keeps no shared state. Each replica follows the providers and computes the prices on its own, from the same configuration, so that a replica that dies loses nothing and needs no takeover. Two replicas may differ by the ticks that reached one of them first, so one replica at a time publishes the updates, and consumers see one series (see [Distribution](#distribution)). Each replica opens its own connections to the providers, so run two or three, not one per node.

## Providers

A provider is a venue, named as in the [market data catalog](market-data.md#tickers). Each follows the markets of its `symbols`, written `BASE/QUOTE` (or `BASE-QUOTE`). The aliases of the venues are read as the assets they name: `XBT/USD` is `BTC/USD`.

| Provider | Type | Stream | REST |
| --- | --- | --- | --- |
| `binance` | `websocket` | `<symbol>@bookTicker` and `<symbol>@trade` | 24-hour tickers |
| `coinbase` | `websocket` | The `ticker` channel of the Exchange feed, each a trade with the best bid and ask after it | Tickers |
| `kraken` | `websocket` | The `ticker` channel of WebSocket v2 on each change of the best bid and ask, and the `trade` channel without its snapshot | Tickers |
| `okx`, `nobitex`, `wallex`, `ramzinex` | `rest` | None | Tickers |
| `tabdeal` | `rest` | None | The depth of each followed market only, a request each ([#443](https://github.com/mdastpak/Crypto-Exchange/issues/443)) |

A `websocket` provider follows its stream. It reconnects when the connection drops or stays silent for 30 seconds, after a wait that starts at a tenth of `reconnect_delay_seconds` and doubles with each failure in a row, up to 30 seconds, with a jitter of ±20 %. While its stream sent no tick for 3 seconds, its REST API is read every `poll_ms` instead, and the reads stop once the stream sends again. A `rest` provider is read every `poll_ms` only. A REST ticker without both sides of its book gives nothing, and REST gives no trades, so a provider read through REST adds to the mid and the best bid and ask, not to the VWAP.

The Iranian venues give the reference of the rial markets, since no global venue quotes IRR. They have no stream, so their `stale_threshold_ms` is longer than the default. Ramzinex and Tabdeal were written from their documented answers and are disabled in the example: check them from a node in Iran before enabling them. Tetherland and Abantether wait for API access ([#443](https://github.com/mdastpak/Crypto-Exchange/issues/443)). They are providers of the [Iran region](configuration.md#region-of-a-release) only: an international release refuses to start with one enabled. They often answer only from Iran: give them `egress: direct` on a node there, and `proxy` elsewhere.

## Aggregation

Every `aggregation_interval_ms` (100 ms), for each market:

1. The **fresh** providers are those whose last book is at most `stale_threshold_ms` old, or the provider's own `stale_threshold_ms`. A book's time is the provider's, unless the provider gives none or its clock is ahead of ours; then it is the time of receipt.
2. The **outliers** are rejected by the median absolute deviation (MAD) of the fresh providers' mids: a mid further from their median than `outlier_threshold_mad` MADs is out, unless it is within `outlier_floor_bps` of the median, so that providers that agree to the tick are not rejected for noise when the MAD is zero. Of two providers, neither is an outlier: each is as far from their median.
3. The **quorum**: with at least `min_providers` providers left, the price is `live`. Release mode needs at least 2, so that one venue never sets the price alone; use 3 for a market that quotes real money, because of two providers neither is an outlier, and the median moves halfway towards a wrong one. Below it, the price is `stale`. It keeps the figures of its last live price, with that price's time in `updated`, and must not be served as current. A market with no live price yet is stale with zero figures.
4. The figures of a live price:
   - `bid` and `ask`: the best bid and the best ask of the providers. They may cross when the venues disagree.
   - `mid`: the median of the providers' mids, the reference price.
   - `vwap`: for each of `vwap_windows_seconds`, the volume-weighted average price of the providers' trades in the window. A window without trades has none.
   - `twap`: for each window, the time-weighted average of the mid over the live time in it. The time below the quorum does not count.
   - `sources` and `outliers`: the providers of the figures, and those rejected.
   - `at`: when it was computed; `updated`: the time of its newest book.

The windows keep running sums in buckets of a tenth of the shortest window, so an interval costs the same whatever the number of trades in the longest window. Each window must be a multiple of that bucket. Every amount is a decimal, never a floating-point number, and VWAP and TWAP are rounded to 18 places. Times are kept to the nanosecond and reported to the microsecond.

A tick is rejected, and counted in `price_ticks_rejected_total`, when its market is not followed (`unknown_market`), an amount is not positive (`invalid`), its bid is above its ask (`crossed_book`), a book is older than the provider's last one (`out_of_order`), or a trade is older than the longest window (`too_old`).

### Performance

`TestAggregator_Load` runs three providers at 1,000 ticks per second each over 20 markets, for the 300 seconds of the longest window: an aggregation of every market takes about 0.1 ms at the median and well under 1 ms at the 99th percentile, within the budget of 10 ms, and a tick about 0.5 µs.

## Distribution

[#59](https://github.com/mdastpak/Crypto-Exchange/issues/59). Every interval, with `publish_to_redis`:

1. **Every replica** writes each live price to its key `price:<BASE>/<QUOTE>` in Redis, as JSON, with a life of `cache_ttl_ms` (500 ms). A stale price is not written: its key expires, so no reader can take a price that lost its quorum. Since every replica writes, the keys stay fresh while any replica runs.
2. **One replica**, the holder of the lease `price:publisher` (a token in Redis that lives `lease_ttl_ms`, 2 s, renewed every third of it), publishes the update of the interval: every price, live or stale, as `{"event": "prices.update", "prices": [...], "at": "..."}`.
   - to the Redis stream `price:stream`, capped at about `stream_max_len` (10,000) entries, for a consumer that reads from where it stopped;
   - with `publish_to_rabbitmq`, to the `price_updates` exchange, as a transient message that expires after 2 s, so that a slow queue never holds old prices. The server's WebSocket forwards it to the [clients](api.md#prices).

When the leader dies, its lease expires and another replica takes it within `lease_ttl_ms`; the keys never lapse meanwhile. A replica that loses the lease, as when Redis failed over, stops publishing at once: a leader always checks its token before it publishes. A publish that fails counts in `price_publish_errors_total` and is not retried; the next interval publishes newer prices. Publishing never blocks the aggregation: a slow Redis or broker drops the older pending update.

### Reading a price

`price.Reader` is the consumer library: `Get(ctx, "BTC/USDT")` returns the reference price that quotes use ([#61](https://github.com/mdastpak/Crypto-Exchange/issues/61)).

1. An administrator's override in force, with status `override` and the override's `id` and whether a second administrator `approved` it. An override that waits for its approval is not served.
2. Otherwise the cached price, when it is live, of that market, with a mid above 0, and at most one second old (`price.DefaultMaxAge`). An older one is `price.ErrStale`, and none `price.ErrNoPrice`.

A Redis error is returned as is: a price is never guessed, so a caller fails closed and quotes nothing.

### Integrity of the prices

[#450](https://github.com/mdastpak/Crypto-Exchange/issues/450). Redis is not trusted with a price. Whoever can write to it, through a leaked password, an open network, or a compromised neighbour, could otherwise set the reference price of every OTC quote and the price at which a [delisting](api.md#delistings) converts every balance, and keep it fresh, since the age is a field of the same value. The ledger would not notice: the loss is a price, not a sum.

So the aggregator signs each price with an Ed25519 private key that only it holds (`price_aggregator.signing_key`), and every server holds the public keys (`price.verify_keys`): a server can verify a price but never make one. The aggregator has no database, so it cannot use the data keys.

- The signature (`sig`, with the key id `kid`, the first 8 bytes of the SHA-256 of the public key in hex) covers a canonical encoding of every other field: `price/v1`, the key id, the market, the status, `bid`, `ask`, `mid`, the VWAP and TWAP by window in order, the sources, the outliers, and `at` and `updated` in nanoseconds. A change to any of them breaks it.
- `price.Reader` verifies a price before it believes anything else of it, its age included. A price that is unsigned, malformed, signed by an unknown key, or changed is `price.ErrUnverifiedPrice`, which is also `ErrStale`: a quote answers as for a stale price, and a settlement waits. It is never replaced by an older value. Each counts in `price_signature_failures_total{reason}` (`unsigned`, `unknown_key`, `malformed`, `bad_signature`) and is logged once a minute per market.
- The updates of `price:stream` carry the same signature in each price. The `price_updates` exchange feeds the WebSocket for display only, and is not a source that prices anything.
- Overrides are rows of PostgreSQL, sealed, and not affected.
- Release mode refuses an aggregator that publishes to Redis without `signing_key`, and a server without `verify_keys` or with `price.signatures: off`. Outside release mode, a server without keys reads the prices unverified, with a warning at startup.

To make a key pair:

```sh
openssl genpkey -algorithm ed25519 -out price-signing.pem   # the aggregator's secret
openssl pkey -in price-signing.pem -pubout -out price-verify.pem   # every server
```

To rotate it: add the new public key to `price.verify_keys` on every server, then give the aggregators the new private key, then remove the old public key. Both keys verify in between, so there is no gap.

### Overrides

An administrator sets the price of a market in place of the engine's for a time, through the [admin API](api.md#reference-prices), when the providers are wrong or down. The overrides are rows of PostgreSQL, sealed, and never in Redis, so that a write to Redis cannot set a price. Each server instance reads those in force every second (`priceoverride.Watcher`) and at once after its own change, and keeps the last ones it read while PostgreSQL cannot be read; none is served past its expiry, even before the next read. An override that fails its seal is not served.

An override is bounded by the live reference price ([#453](https://github.com/mdastpak/Crypto-Exchange/issues/453)), so that one administrator, or a stolen session with a step-up, cannot set a price far from the market. Its mid within `price.overrides.max_deviation_bps` of the live mid applies at once. One beyond it, or set while the market has no live price, waits for a second administrator for `price.overrides.approval_minutes`, is not served meanwhile, and lapses unapproved. A delisting settles only at an approved override. The `override` of a price is not signed, so `price.Reader` drops it from a price read from Redis: only an override read from PostgreSQL carries one.

### Latency budget

From a provider's tick to a WebSocket client, at the default settings:

| Step | Budget |
| --- | --- |
| Provider to the aggregator (network and parsing) | 50 ms |
| Wait for the next aggregation | 100 ms (`aggregation_interval_ms`) |
| Aggregation | 1 ms |
| Publish to RabbitMQ, and to the server instance | 20 ms |
| Throttle and send to the client | 30 ms |
| **Total, 99th percentile** | **200 ms** |

`price_delivery_latency_seconds` measures it on each server instance, from the `updated` time of each price sent, the receipt of its newest tick. Alert when `histogram_quantile(0.99, rate(price_delivery_latency_seconds_bucket[5m])) > 0.2`.

## Metrics

On `GET /metrics`, with the metrics of the Go runtime and of the process:

| Metric | Type | Labels | What |
| --- | --- | --- | --- |
| `price_ticks_total` | counter | `provider`, `kind` | Ticks kept, `book` or `trade`. Their rate is the message rate of the provider. |
| `price_ticks_rejected_total` | counter | `provider`, `reason` | Ticks rejected, by the reasons above. |
| `price_provider_lag_seconds` | gauge | `provider` | Delay between the provider's time of its last tick and its receipt. |
| `price_provider_last_tick_age_seconds` | gauge | `provider` | Time since the last tick of the provider. |
| `price_provider_connected` | gauge | `provider` | 1 while its stream is open. |
| `price_provider_rest_fallback` | gauge | `provider` | 1 while its REST API is read. |
| `price_aggregation_duration_seconds` | histogram | | Time of one aggregation of every market. |
| `price_market_live` | gauge | `market` | 1 while the price of the market is live, 0 while it is stale. |
| `price_market_sources` | gauge | `market` | Providers that make the price. |
| `price_publisher_leader` | gauge | | 1 while this replica holds the lease and publishes. |
| `price_publish_errors_total` | counter | `target` | Failed writes, by `cache`, `stream`, `rabbitmq`, or `lease`. |
| `price_delivery_latency_seconds` | histogram | | On the server: from the newest tick of a price to its send to the WebSocket clients. |

The labels take only the configured providers and markets.

| Alert | Expression | What to do |
| --- | --- | --- |
| Market stale | `max by (market) (price_market_live) == 0` for 1 minute | No replica has the quorum of the market: quotes of it stop. Check `GET /providers` for the providers that are down. |
| Provider silent | `min by (provider) (price_provider_last_tick_age_seconds) > 30` | No replica receives the provider, through its stream or REST. |
| No publisher | `max(price_publisher_leader) == 0` for 1 minute | No replica publishes: check that the replicas reach Redis. |
| Delivery slow | `histogram_quantile(0.99, rate(price_delivery_latency_seconds_bucket[5m])) > 0.2` | Past the [latency budget](#latency-budget). |
| Aggregation slow | `histogram_quantile(0.99, rate(price_aggregation_duration_seconds_bucket[5m])) > 0.01` | The 99th percentile is past the budget of 10 ms. |
