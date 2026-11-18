# Venues

The venues are the external exchanges where the OTC trades are covered ([#384](https://github.com/mdastpak/Crypto-Exchange/issues/384), epic [#383](https://github.com/mdastpak/Crypto-Exchange/issues/383)). Package `internal/venues` is their authenticated side: our balances at a venue, its markets, our orders, and the moves of funds out of it and between its accounts. The market data service reads only their public data, through `internal/marketdata/source` (see [market-data.md](market-data.md)).

Nothing uses the venues yet. The router of the OTC trades ([#386](https://github.com/mdastpak/Crypto-Exchange/issues/386), [#387](https://github.com/mdastpak/Crypto-Exchange/issues/387)) and the reconciliation of the venues ([#385](https://github.com/mdastpak/Crypto-Exchange/issues/385)) build on this interface. The configuration is in [`venues`](configuration.md#venues).

## The interface

Every connector implements `venues.Venue`:

| Method | What it does |
| --- | --- |
| `Balances` | Our free and locked amount of each asset. |
| `Instruments` | The markets, with the step of the price and the quantity, and the smallest order; a zero step is unknown. |
| `Networks(asset)` | The networks of an asset, with their fees, minimums, and whether they need a tag. |
| `PlaceOrder`, `CancelOrder`, `GetOrder` | Orders, by **our** client order id. |
| `Withdraw`, `GetWithdrawal` | Withdrawals, by our withdrawal id. |
| `DepositAddress(asset, network)` | Our address at the venue. |
| `InternalTransfer`, `GetTransfer` | Moves between the main account and a sub-account, by our transfer id. |
| `Whitelisted(asset, network, address, tag)` | Whether an address is on the withdrawal whitelist of our account. |

What a venue does not offer, or not in its mode, answers `venues.ErrUnsupported`, and `Supports(capability)` says so beforehand: `withdraw`, `deposit_address`, `networks`, `internal_transfer`, `withdrawal_whitelist`.

Each connector speaks the venue's own symbols and units, such as `BTCUSDT` or `btc-rls`. Which of our currencies and networks an asset is, comes from `venues.exchanges.<name>.assets`, never from its symbol: two venues may call one token by different names, or two tokens by one.

## Our ids, and what a caller does after a crash

Every call that changes something carries an id that we choose: a client order id, a withdrawal id, a transfer id. The caller writes it down before the call. After a crash, or after a call whose answer was lost, the caller looks the id up before it does anything else:

- found: the venue did it, and the caller goes on from what the venue says;
- `ErrNotFound`: the venue did not do it, and the caller may send it again with the same id.

A caller never places an order again before it looked its id up: a venue may know only its **open** orders by client id, as Binance does, so a filled order would be placed twice. A `PlaceOrder` whose id the venue already holds as an open order is refused with the code `duplicate_client_id`.

Every error of a call is a `*venues.Error`, of one of three kinds (`venues.KindOf`):

| Kind | Meaning | What the caller does |
| --- | --- | --- |
| `retryable` | It failed without effect: not sent, refused for the rate, a read that failed. | Tries again after a wait. |
| `rejected` | The venue refused it: funds, filters, a bad address, an unknown order. | Does not try again. |
| `unknown_outcome` | It was sent, and its answer was lost: a timeout, a dropped connection, a 5xx, or an answer that cannot be read. The venue may have done it. | Looks the id up. |

The transport, `source.Client.Do`, tells them apart: a request that is not idempotent is sent again only when it was never written to the connection, or when the venue answered 429; once it was written, a lost answer is `source.ErrOutcomeUnknown`. Reads are retried as before. The client keeps the route of the node, the rate limit over every instance, the circuit breaker, and the bounds of the answers of the market data client.

## Credentials

The keys of a venue are read from OpenBao or Vault (KV version 2), one secret per venue at `<mount>/data/<prefix><venue>`, with the fields `api_key` and `api_secret` (Binance) or `token` (Nobitex). For development, a JSON file of the same fields per venue can stand in; release mode refuses it.

- They are cached for `credentials.cache_seconds`, so that a rotation applies without a restart; credentials that the venue refuses are dropped at once, and read again on the next call. While OpenBao cannot be reached, the last credentials are kept.
- They are held as `venues.Secret`, which prints, formats, and marshals as `[redacted]`. Only the signer reads them.
- No error names a URL with a signature, a header, or an answer body of a venue: errors carry the path, the status, and the venue's error code.
- Give each environment keys of its own, bound to the addresses of our nodes at the venue, with trading and, where the venue allows it, withdrawals to whitelisted addresses only.

## Modes

| Mode | What it does |
| --- | --- |
| `testnet` | Calls the venue's test environment, where it has one. The default. |
| `live` | Calls the venue with real funds. |
| `dry_run` | Reads the live venue, and logs the orders, withdrawals, and transfers without sending them. What it logged is kept in memory and found by its id, as at the venue, until a restart. The address of a withdrawal is never logged. |

## Binance

Binance spot, at `https://api.binance.com`, or its testnet at `https://testnet.binance.vision`.

- **Signing.** Every signed request carries `timestamp` and `recvWindow` (`recv_window_ms`, 5 s by default) and the hex HMAC-SHA256 of its query with the secret, as `signature`; the key travels in `X-MBX-APIKEY`, never in the query. Every attempt is signed again, with a fresh timestamp.
- **Clock.** The timestamp is of the venue's clock: the offset is read from `/api/v3/time`, kept for ten minutes, and read again when the venue refuses a timestamp (`-1021`).
- **Orders.** `newClientOrderId` is our id, 1 to 36 of letters, digits, and `.:/_-`. `GetOrder` reads `/api/v3/order` by `origClientOrderId`; `-2013` is `ErrNotFound`. A refused cancel (`-2011`) is a rejection, as Binance answers it for an unknown order and a done one alike: the caller reads the order.
- **Errors.** `-1007` and `-1006` ("execution status unknown") are `unknown_outcome` on a change; `-1021`, `-1003`, `-1008`, `-1001`, 429, and 418 are `retryable`; any other 4xx is `rejected`, with the code. A key or signature refused (`-1022`, `-2014`, `-2015`) drops the cached credentials.
- **Live only.** The testnet has no `/sapi`, so it supports none of the capabilities. Live mode reads the networks from `/sapi/v1/capital/config/getall`, withdraws with `withdrawOrderId` as our id and finds the withdrawal in the history by it, reads deposit addresses, moves funds between sub-accounts with `/sapi/v1/sub-account/universalTransfer` and `clientTranId` (which needs the key of the master account), and reads the whitelist from the withdrawal address book (`whiteStatus`).

## Nobitex

Nobitex, at `https://apiv2.nobitex.ir`. Only a release of the `ir` region may enable it ([region](configuration.md#region-of-a-release)).

- **Authentication.** The token travels as `Authorization: Token …`.
- **Orders.** `/market/orders/add` with `clientOrderId`, 1 to 32 of letters, digits, `_`, and `-`, as our id; `/market/orders/status` finds it by that id. The symbol is `src-dst` as Nobitex names its markets, such as `btc-rls`, and rial markets are in rials.
- **Errors.** Nobitex answers a refused request with `status: failed` and a code, often on HTTP 200: that is a rejection, with its code. An order it does not have (`NotFound`) is `ErrNotFound`.
- **What it lacks.** Nobitex has no testnet, so a Nobitex venue runs `live` or `dry_run`. It has no withdrawals without a one-time code, no sub-accounts, no list of networks, and no whitelist API, so those capabilities are absent. It publishes no filters of its markets, so the steps and minimums of `Instruments` are zero, which is unknown.

The endpoints and codes of Nobitex are from its public documentation; no answer of a live account was recorded for the fixtures, so the first run against the live venue in `dry_run` should confirm the order status and error codes before `live`.

## State

The router (#386) decides from the state of the venues, in PostgreSQL so that every node sees the same ([#385](https://github.com/mdastpak/Crypto-Exchange/issues/385)). `internal/venues/state` keeps it.

### Snapshots

A job of `cmd/market-worker`, `venue_state:<venue>`, runs every `venues.state.interval_seconds` under its lease ([#264](https://github.com/mdastpak/Crypto-Exchange/issues/264)), so that one node at a time calls a venue for its state. Only a node that enables the venue in `venues.exchanges` has its job.

The venue jobs run apart from the market data ([#501](https://github.com/mdastpak/Crypto-Exchange/issues/501)). They are this job and the [routed orders](otc-routing.md#routed-orders) of each venue, and they have a runner of their own:

- They run whenever a venue is enabled, also with `market_data.enabled: false`.
- They are always live. The mode of a venue is `venues.exchanges.<name>.mode`, so a `dry_run` venue still reads its state but sends nothing.
- `market_data.mode` does not apply to them.
- A lockdown of `otc_routing` pauses them, and a lockdown of `market_data` does not.
- They take the poll, lease, and retry timing of `market_data` where it is set: by default 1 second, 30 seconds, and 5 seconds.

One run, in order:

1. The markets (`venue_instruments`): the steps, the minimums, and whether each trades.
2. The networks of each mapped asset (`venue_networks`): deposits and withdrawals on or off, the withdrawal fee and minimum.
3. The fees that the venue reports apart from its fills, when its connector implements `venues.FeeReporter`, each posted once by its id (see below). The fees are read from a cursor, the snapshot of kind `fees`, as described in [The fees of the venues](#the-fees-of-the-venues).
4. The balances (`venue_balances`): free and locked.
5. The reconciliation.

A row that the venue no longer reports is kept: a balance is zeroed, a market stops trading, a network turns its deposits and withdrawals off. Nothing is deleted ([#248](https://github.com/mdastpak/Crypto-Exchange/issues/248)). Each kind of snapshot records its time and its last failure in `venue_snapshots`, by the kind of the error only (`retryable`, `rejected`, `unknown_outcome`), never the venue's text. A snapshot that fails leaves the last one, which goes stale on its own. In `market_data.mode: dry_run` the job reads the venue and writes nothing.

A value older than `venues.state.max_age_seconds` is unknown, and an unknown value is not routable. Every time of the venue state is on the database's clock, which every node shares ([#502](https://github.com/mdastpak/Crypto-Exchange/issues/502)). This covers the time of a snapshot, its freshness, and the times of the reservations and holds. It is the clock of the ledger entries too, so a node whose clock is off neither ages a snapshot wrongly nor misorders it against a move. The time of a snapshot is read before the venue is called. `Store.Routable(venue, asset, network)` answers whether a currency is routable at a venue, and why not: `no_balance`, `stale_balance`, `reconciliation_hold`, and for a network `network_unknown`, `network_stale`, or `network_suspended` (deposits or withdrawals off). `Store.MarketRoutable` answers for a market: `market_unknown`, `market_stale`, or `market_halted`.

### The venue accounts in the ledger

Each venue has a house account in the ledger, `venue:<name>`, one per currency, which holds the negative of what we hold there, as `custody` does of what custody holds. The entries of the venues move only the exchange's accounts, never a user's balance:

| Entry | Postings | When |
| --- | --- | --- |
| `venue_deposit` | `custody +`, `venue:<name> -` | the venue credited a deposit of ours |
| `venue_withdrawal` | `venue:<name> +`, `custody -` | custody received a withdrawal of ours from the venue |
| `venue_fee` | `venue:<name> +`, `inventory -` | the venue charged a fee, which the exchange's own funds pay |

PostgreSQL refuses any other shape of these kinds, and a venue account on an entry of any other kind ([#404](https://github.com/mdastpak/Crypto-Exchange/issues/404)). The reconciliation of the books counts every account, the venue accounts included, and flags a venue account above zero. The fills of routed orders are posted by the router (#386).

### Reconciliation

After the balances, the job compares, for every mapped currency, what the venue reports holding (free and locked) with what the books say it holds (the negative of `venue:<name>`). A difference beyond `venues.state.tolerances` of the currency, found by `venues.state.confirm_runs` runs in a row, opens a hold (`venue_holds`): the currency is not routable at the venue, an alert is logged as `SECURITY`, and the metric `venue_open_holds` rises, until an operator reviews it. A balance snapshot that is not fresh, or older than the last venue entry, is not compared, as the move booked after it is not in it. One reconciliation of a venue runs at a time across the nodes, under an advisory lock.

A run with no difference posts nothing. The fees are posted before the balances are taken, so a run with a venue fee posts exactly that fee, once, and then finds no difference.

#### The fees of the venues

The fees are read from a cursor ([#503](https://github.com/mdastpak/Crypto-Exchange/issues/503)):

- A run reads from the time of the last read less 3 days. A fee that the venue reports late, such as the fee of a withdrawal that completes after it was asked for, is therefore still read. A fee read twice is posted once, by its id.
- The first read goes back 30 days.
- A dry run of the worker reads the fees and moves no cursor.
- A read that fails is recorded as a failure of the `fees` snapshot. The run then compares no balances, since the unread fees would show as differences.

Which venues report fees:

- **Binance:** the fee (`transactionFee`) of each completed withdrawal, from `GET /sapi/v1/capital/withdraw/history`, within the 90 days that Binance answers. Each fee has the id `withdraw:<id>`. A testnet has no withdrawals and reports none. Transfers between our accounts are free.
- **Nobitex:** none. Its connector makes no withdrawals.
- The trading commissions are not in either list: they belong to the fills of the routed orders.

An operator clears a hold with `POST /api/v1/admin/venues/holds/{id}/clear` and a reason, with a step-up, audited as `admin.venue_hold_clear`. If the difference stays, the next runs hold the currency again. A hold means funds may be missing at the venue, so clearing one will need two administrators, as a re-seal does ([#521](https://github.com/mdastpak/Crypto-Exchange/issues/521), decided in [#504](https://github.com/mdastpak/Crypto-Exchange/issues/504)); until then, review every `admin.venue_hold_clear` in the audit log.

### Reservations

A routed order reserves the venue balance that it plans to spend before it is sent (`Store.Reserve`, or `ReserveIn` in the router's transaction). The row of the balance is locked first, so two nodes that reserve the same balance at once take turns, and the second sees the first's reservation: the free balance less the open reservations must cover the amount, or the reservation is refused with `ErrInsufficient`. The currency must be routable. A reference (`reference_type`, `reference_id`) reserves once: the same amount again returns the first reservation, another is `ErrReferenceReused`. `Store.Release` ends a reservation by its `released_at` mark, never by a delete.

The reservations and the holds are sealed from their first row ([#404](https://github.com/mdastpak/Crypto-Exchange/issues/404)): a reservation lowered or released in SQL refuses the reservations of its balance, and a hold cleared in SQL still holds. The snapshots are not sealed: a balance raised in SQL lets the router plan an order that the venue then rejects for its funds, and the reconciliation holds the currency.

### Admin and metrics

`GET /api/v1/admin/venues` shows each enabled venue: the time, freshness, and last failure of each snapshot; each balance with what the books say, the difference, the open reservations, what is still available, and whether it is routable; the networks; the count of markets and of those trading; and the open holds.

An open reservation that fails its seal, changed outside the backend, marks only its balance: it shows `unverified_reservation` among the reasons it is not routable, with nothing reserved or available, as the router refuses it ([#504](https://github.com/mdastpak/Crypto-Exchange/issues/504)). The other balances and venues, and the metrics, still show.

Each API instance exports, from the database, so every one reports the same. A read of the state costs about six queries per venue, with the holdings of the books, one more per open hold, and about five per balance; a read serves every scrape for 15 seconds, and concurrent scrapes wait for one read, so an instance reads at most once in 15 seconds however often it is scraped. A snapshot age is of the time of the read, so it lags by at most 15 seconds: keep the alerts above that. A failed read exports nothing and is not kept, so the next scrape reads again.

| Metric | Labels | |
| --- | --- | --- |
| `venue_snapshot_age_seconds` | `venue`, `kind` | Alert above `max_age_seconds`. |
| `venue_reconciliation_difference` | `venue`, `asset` | What the venue reports less what the books say. |
| `venue_routable_balance` | `venue`, `asset` | The free balance less the open reservations, `0` while not routable. |
| `venue_open_holds` | `venue` | Alert above 0. |

## Tests

`internal/venues/venuetest` is the contract of every connector: an unknown id is not found, a market order is found by our id, an open id again is a duplicate, a lost answer is `unknown_outcome` and the order is found after it, a cancel, an order beyond the funds is rejected and not found, the balances and markets, the capabilities a venue lacks, and that no error holds a credential or a signature.

`internal/venues/state` is tested against PostgreSQL: parallel reservations of one balance never reserve more than it holds, a stale snapshot, a suspended network, and a reconciliation difference each make the currency not routable, a run with no difference posts nothing and one with a fee posts exactly that fee, and the tampered reservations and holds are refused.

It runs against the stub (`internal/venues/stub`, for the tests of the router), and against each connector over a fake server of the venue's API (`fake_test.go`), which checks the key, the signature, and the timestamp as the venue does, answers from the fixtures in `testdata`, and can drop the connection after it did a change. The signature of Binance is also checked against the example of its documentation.

## Adding a venue

1. A package under `internal/venues/` that implements `venues.Venue` over `venues.Caller`, with a `Classifier` of the venue's errors.
2. A fake server of its API with fixtures, and `venuetest.Run` over it.
3. Its name in `config.KnownVenues`, in `config.IranianVenues` when only the `ir` region may use it, and in `registry.build`.
4. Its section in this file, and in [configuration](configuration.md#venues).
