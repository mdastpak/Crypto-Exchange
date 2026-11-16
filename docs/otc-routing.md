# OTC routing rules

The routing rules decide where the liquidity of each OTC trade comes from:
the exchange's own inventory, an external venue ([`venues.md`](venues.md)),
or several of them ([#386](https://github.com/mdastpak/Crypto-Exchange/issues/386),
part of [#383](https://github.com/mdastpak/Crypto-Exchange/issues/383)).
Administrators set the rules through the admin API. The router applies them
twice. It applies them when a quote is priced
([#61](https://github.com/mdastpak/Crypto-Exchange/issues/61)), so that the
price includes the cost of the route. It applies them again when the quote is
executed ([#62](https://github.com/mdastpak/Crypto-Exchange/issues/62)),
against the state at that moment.

Routing is on only with [`otc.routing.enabled`](configuration.md#otc). When
it is off, the inventory takes every trade, as before routing, and the rules
are used only by the explain endpoint.

## Behavior

- **A route** is a list of legs. Each leg is the inventory, or a venue with
  its market symbol, and each leg has a quantity, a notional, and a cost. The
  route is deterministic: the same rules, inputs, and venue state give the
  same route on every node. The arithmetic is decimal, and the rules,
  venues, and sources are tried in a fixed order.
- **No route, no quote.** If no rule gives a route, the quote is refused
  with `409 otc_no_route`, and its `limit` holds the reason:
  - `no_rules`: no version has been saved yet.
  - `no_matching_rule`: no rule matches the trade.
  - `insufficient_liquidity`: the sources of every matching rule together
    cannot fill the trade.
  - `cost_too_high`: the route costs more than the rule's ceiling.

  A trade is never accepted unhedged.
- **The price includes the route.** The cost of the route, in basis points,
  is the `route_bps` part of the quote. It is added to the other parts
  ([`configuration.md#otc`](configuration.md#otc)). A quote by quote amount
  is sized again with that cost, so that the total stays within the amount.
- **The execution routes again.** At execution, the route is planned again
  against the current venue state and the current rule version. The quote is
  refused with `409 quote_not_routable` in three cases: no route remains,
  the new route costs more than the quote charged, or the quote was given
  before routing was switched on. Otherwise the trade records the route of
  the execution in its sealed `components`, and the
  [routed orders](#routed-orders) place its venue legs. The inventory limits
  ([#62](https://github.com/mdastpak/Crypto-Exchange/issues/62)) are still
  checked under their locks, on the share of the trade that the inventory
  keeps.
- **Versions.** Every save creates a new version of the whole rule set in
  `otc_routing_rules`. The save is audited as `admin.otc_routing_rules` in
  the same transaction ([#51](https://github.com/mdastpak/Crypto-Exchange/issues/51)).
  A version is sealed and never changes. A version that was changed in SQL
  fails its seal, and then nothing routes until it is reviewed: quotes are
  refused rather than routed by rules that the administrators did not save.
- **Every node, no restart.** Each instance keeps the active version for
  `otc.routing.cache_seconds` (default 5). After that, it reads the number
  of the highest version, which is one small query. It reads the rules again
  only when that number changed. A new version therefore reaches every
  instance within the cache window. The version number itself serves as the
  generation of the cache, so no Redis key is needed.
- **Region ([#272](https://github.com/mdastpak/Crypto-Exchange/issues/272)).**
  An international release refuses a rule that names an Iranian venue, such
  as `nobitex`, both when the rule is saved and when it is read.

## Rule format

A version is a JSON array of rules. An empty list matches everything.

| Field | Description |
| --- | --- |
| `name` | Unique. 1 to 64 letters, digits, spaces, or `._:-`. |
| `priority` | Lower is tried first. Rules of one priority are tried by name. |
| `markets` | Our market symbols, such as `BTC/USDT`. |
| `sides` | The user's sides: `buy` or `sell`. |
| `networks` | Our networks of the user's destination ([#57](https://github.com/mdastpak/Crypto-Exchange/issues/57)). A rule that lists networks matches only a trade with one of them. |
| `min_notional`, `max_notional` | The notional of the trade, as quantity × reference price, in the quote currency. |
| `regions` | The user's regions: `ir` or `international`. Today this is the region of the release. |
| `min_level` | The lowest verification level that the rule serves ([#157](https://github.com/mdastpak/Crypto-Exchange/issues/157)), from 0 to 4. |
| `strategy` | `own_first` (the default) or `cheapest`. See [Order of the sources](#order-of-the-sources). |
| `use_inventory` | Lets the exchange's own inventory fill the trade. |
| `venues` | The venues that may fill it. See the next table. |
| `max_cost_bps` | The ceiling on the cost of the route, in basis points of the notional. `0` means no ceiling. A venue whose own cost is above the ceiling is left out. |

A rule needs `use_inventory`, at least one venue, or both.

Each venue of a rule:

| Field | Description |
| --- | --- |
| `venue` | A venue with a connector: `binance` or `nobitex`. Each venue may appear only once in a rule. |
| `priority` | Lower fills first, within the rule. |
| `weight` | Splits a trade across the venues of one priority, in proportion to their weights. |
| `max_balance_share_bps` | The share of the venue's available balance that one trade may use. `0` means all of it. |
| `reserve` | An amount of the asset that the venue spends which no trade takes, in that asset. |
| `max_daily_notional` | The notional that the venue may take in a UTC day, in the quote currency of the trade. The routed orders add to it ([#387](https://github.com/mdastpak/Crypto-Exchange/issues/387)). It is kept in the sealed `otc_routing_usage` table. |

## Sources

**A venue.**

- A buy buys the base currency at the venue with the venue's balance of the
  quote currency.
- A sell sells the venue's balance of the base currency.

The venue is left out, with the reason shown in the explanation, in any of
these cases:

- It is not enabled.
- One of the currencies is not mapped.
- Its market of the two currencies is not listed in a fresh snapshot, or is
  not trading.
- The balance it spends is not routable ([`venues.md#state`](venues.md#state)):
  stale, held by a reconciliation, or absent.
- The trade has a network, and the network is not open for both deposits
  and withdrawals there.
- Its lot is not a multiple of ours, and ours is not a multiple of its lot.
- A leg would fall under its minimum quantity or minimum notional.

What a venue can fill is computed in this order:

1. Start from its available balance, which is the free balance less the open
   reservations.
2. Subtract the `reserve`.
3. Take the `max_balance_share_bps` share of what is left.
4. For a buy, divide by the price with the venue fee.
5. Cap the result by what `max_daily_notional` leaves for the day.
6. Round down to the lot.

**The inventory.** The inventory can give a currency up to its position plus
the `max_short` of `otc.inventory_limits`. Without a limit, there is no
bound. A release build has no inventory for a currency without a limit, as
the execution requires.

## Cost

The cost of a leg at a venue is `fee_bps` + `slippage_bps` of
[`venues.exchanges.<name>`](configuration.md#venues), plus the venue's
withdrawal fee on the trade's network. The withdrawal fee is taken over the
leg's amount of the currency the venue gives. A leg of the inventory costs
nothing. The cost of the route is the cost of its legs, weighted by their
notional and rounded up to a hundredth of a basis point.

## Order of the sources

- **`own_first`:** the inventory fills first. The venues follow by
  `priority`. Venues of one priority form a group. If any venue of a group
  has a weight, each venue first takes its share of what is left, as
  `weight / total weight`, rounded down to its lot and capped at what it can
  fill. What remains then goes to the venues of the group in order. Venues
  without weights fill in order.
- **`cheapest`:** each source fills alone, cheapest first. The inventory
  comes before a venue of the same cost. Ties are broken by priority and
  name. Weights are not used.

## Examples

Fill from the inventory up to its limits. Above 50000 USDT, or once the
inventory is short, split the trade between Binance and Nobitex 3 to 1.
Never pay more than 40 basis points, and keep 1000 USDT at each venue.

```json
[
  {
    "name": "large BTC",
    "priority": 1,
    "markets": ["BTC/USDT"],
    "min_notional": "50000",
    "venues": [
      {"venue": "binance", "weight": 3, "reserve": "1000", "max_daily_notional": "2000000"},
      {"venue": "nobitex", "weight": 1, "reserve": "1000", "max_balance_share_bps": 5000}
    ],
    "max_cost_bps": 40
  },
  {
    "name": "default",
    "priority": 10,
    "use_inventory": true,
    "venues": [{"venue": "binance"}],
    "max_cost_bps": 40
  }
]
```

Serve only verified users of level 2 and above on the venues, and route the
cheapest first:

```json
[
  {"name": "verified", "min_level": 2, "strategy": "cheapest", "use_inventory": true,
   "venues": [{"venue": "binance"}, {"venue": "nobitex"}]}
]
```

## Routed orders

Each venue leg of a trade's route becomes a routed order at its venue
([#387](https://github.com/mdastpak/Crypto-Exchange/issues/387)), kept in the
sealed `otc_routed_orders` table.

- **With the trade.** The order is planned in the transaction of the trade.
  The same transaction reserves the venue balance that the order spends
  ([`venues.md#state`](venues.md#state)), adds its notional to the venue's
  daily usage, and writes an `otc.routed_order_changed` event to the outbox.
  A trade therefore never commits without the orders that cover it. A leg
  whose venue, market, or balance can no longer be planned refuses the trade
  with `409 quote_not_routable`.
- **The slippage guard.** Every order is a limit order. A buy pays at most
  the trade's price / (1 + the venue's `fee_bps`), so that the trade stays
  whole. It also pays at most the reference price × (1 +
  `otc.routing.max_slippage_bps`). The limit is rounded down to the venue's
  tick. A sell takes at least price / (1 − fee) and at least reference × (1 −
  slippage), rounded up. A fill whose average price is beyond the limit is
  left to a review.
- **The driver.** `cmd/market-worker` runs one job per enabled venue,
  `routed_orders:<venue>`, every 2 seconds, while `otc.routing.enabled` is
  on. Any node may run it. Each order is driven by one node at a time, under
  a lease of its own row with a token. Every write carries the token, so a
  node whose lease was taken over writes nothing, and its transaction rolls
  back.
- **Once at the venue.** The client id of an order is derived from the
  trade, the leg, and the attempt. A node looks the order up by it before it
  places anything. An order that a node placed before it crashed, or whose
  answer was lost, is found and followed rather than placed twice. A
  placement with an unknown outcome is looked up for
  `otc.routing.unknown_grace_seconds` before the order is placed again,
  because a venue may not show a new order at once. A venue that still
  refuses the id after that leaves the order to a review.
- **Fills.** Each lookup posts what the order filled since the last one, in
  one transaction. It posts `venue_fill` in the ledger: the venue gets what
  it bought and gives what it spent, and the inventory the reverse. It moves
  the inventory's positions back by the same amounts and writes the new
  status. The trade took its whole quantity from the inventory's positions,
  and the fills give back what the venue covered. The inventory keeps the
  difference of the prices. The venue's fees are posted from its own
  reports ([`venues.md`](venues.md#the-venue-accounts-in-the-ledger)).
- **The deadline.** An order open longer than
  `otc.routing.order_timeout_seconds` is cancelled, then looked up again,
  since a cancel may cross a fill. What it did not fill is routed again
  by the trade's rules, without the venues that already failed the leg, as
  a new order of the next attempt. A route of the remainder to the inventory
  needs no order. After 3 attempts, or with no route, the order is left to a
  review, and the inventory keeps what nothing covered.
- **Statuses.**
  - `planned`, `placed`, and `partially_filled` are driven.
  - `filled` and `cancelled` are ended. A cancelled order's reason is
    `timed_out` or `venue_rejected`.
  - `failed` was refused by the venue.
  - `manual_review` needs a person. Its reason is one of these codes:
    `no_route`, `attempts_exhausted`, `duplicate_unresolved`,
    `order_missing`, `fill_invalid`, or `price_beyond_limit`.
  - The withdrawal states are those of
    [#388](https://github.com/mdastpak/Crypto-Exchange/issues/388).

  Every transition is kept in the append-only `otc_routed_order_events`,
  with the node and its lease token. Every ended order releases its
  reservation. A reason is a code, never the venue's own text.
- **The kill switch.** A lockdown of `otc_routing`
  ([`api.md`](api.md#maintenance-and-lockdowns)) stops every order where it is, and
  refuses, with the lockdown's `503`, new trades whose route has a venue leg. A lockdown of
  `market_data` pauses the drivers with the other jobs of the worker.
- **A changed row.** An order that fails its seal is not driven. It waits
  until it is reviewed.

## Admin API

| Route | Description |
| --- | --- |
| `GET /api/v1/admin/otc/routing/rules` | The highest version, with who saved it and why, and whether `otc.routing.enabled` routes by it. |
| `PUT /api/v1/admin/otc/routing/rules` | Saves `{"rules": [...], "reason": "..."}` as a new version. Each problem answers `400` with its field, such as `rules[0].venues[1].venue`. Needs a step-up. |
| `POST /api/v1/admin/otc/routing/explain` | Explains a trade without placing anything. The body has `market`, `side`, `quantity`, `price`, and optionally `level`, `network`, and `region`. The answer lists each rule in order, why it matched or not, what each source of a matching rule can fill and at what cost, and the route of the first rule that gives one, or the reason there is none. It reads the latest version, not the cache. |

See [`api.md`](api.md#otc-routing-rules) for the full answers.
