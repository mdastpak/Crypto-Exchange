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

## Tests

`internal/venues/venuetest` is the contract of every connector: an unknown id is not found, a market order is found by our id, an open id again is a duplicate, a lost answer is `unknown_outcome` and the order is found after it, a cancel, an order beyond the funds is rejected and not found, the balances and markets, the capabilities a venue lacks, and that no error holds a credential or a signature.

It runs against the stub (`internal/venues/stub`, for the tests of the router), and against each connector over a fake server of the venue's API (`fake_test.go`), which checks the key, the signature, and the timestamp as the venue does, answers from the fixtures in `testdata`, and can drop the connection after it did a change. The signature of Binance is also checked against the example of its documentation.

## Adding a venue

1. A package under `internal/venues/` that implements `venues.Venue` over `venues.Caller`, with a `Classifier` of the venue's errors.
2. A fake server of its API with fixtures, and `venuetest.Run` over it.
3. Its name in `config.KnownVenues`, in `config.IranianVenues` when only the `ir` region may use it, and in `registry.build`.
4. Its section in this file, and in [configuration](configuration.md#venues).
