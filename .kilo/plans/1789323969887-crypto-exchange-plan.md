# Crypto Exchange Implementation Plan

**Branch:** `feat/exchange-core` (from `main`)
**Architecture:** Modular monolith with clear service boundaries
**Approach:** Phased - MVP (OTC) → Spot → Derivatives

---

## Phase 0: Foundation & Infrastructure (Week 1-2)

### 0.1 Domain Models & Type System
- [ ] Define `Asset` entity: `id, symbol, name, type(FIAT|COIN|TOKEN|STABLE), decimals, network, contract_address, is_active`
- [ ] Define `Market` entity: `base_asset, quote_asset, min_order_size, tick_size, lot_size, status`
- [ ] Define `Order` entity: `id, user_id, sub_account_id, market_id, side, type, price, quantity, filled, status, time_in_force`
- [ ] Define `Trade` entity: `id, order_id, market_id, price, quantity, fee, fee_asset, side, timestamp`
- [ ] Define `Balance` entity: `user_id, sub_account_id, asset_id, available, locked, on_chain_address`
- [ ] Define `SubAccount` entity: `id, user_id, label, api_keys[], permissions[], is_active`
- [ ] Define `KYCProfile` entity: `user_id, status, tier, documents[], risk_score, verified_at`

### 0.2 Database Migrations
- [ ] Create all tables with proper indexes (partial indexes for active markets, user balances)
- [ ] Add partitioning strategy for `trades` and `orders` (by month)
- [ ] Add advisory locks for critical sections

### 0.3 Configuration System Enhancement
- [ ] Asset registry config (YAML/DB-driven): chain RPCs, confirmation requirements, deposit/withdrawal fees
- [ ] Market config: trading fees, maker/taker, min notional
- [ ] Provider config: WS endpoints, REST endpoints, auth, symbols mapping
- [ ] Feature flags for gradual rollout

### 0.4 Observability Foundation
- [ ] Structured logging (slog + JSON)
- [ ] Prometheus metrics: HTTP latency, order throughput, price latency, queue depths
- [ ] Distributed tracing (OpenTelemetry)
- [ ] Health checks with dependency status

---

## Phase 1: MVP - Automated OTC (Week 3-6)

### 1.1 Price Engine & Provider Integration
- [ ] `PriceAggregator` service:
  - WebSocket clients for each provider (Binance, Coinbase, Kraken, etc.)
  - REST fallback with exponential backoff
  - Symbol normalization (provider symbol → internal symbol)
  - Microsecond-precision timestamps (nanosecond storage)
  - VWAP/TWAP calculation windows (1s, 5s, 1m, 5m)
  - Outlier detection (median absolute deviation)
  - Publisher: Redis Streams + RabbitMQ for real-time distribution
- [ ] `PriceCache` in Redis:
  - `price:{market_id}` → `{bid, ask, mid, timestamp, source}`
  - TTL: 500ms (auto-expire stale prices)
  - PubSub for WebSocket broadcast

### 1.2 OTC Quote Engine (RFQ + Auto-Match)
- [ ] `QuoteService`:
  - `RequestQuote(user_id, market_id, side, quantity)` → returns firm quote with expiry (5-10s)
  - Quote = mid_price ± spread + slippage_model(quantity)
  - Spread config per market (basis points)
  - Slippage model: sqrt(k * quantity / liquidity)
  - Quote caching with Redis (TTL = quote expiry)
- [ ] `OTCMatchingEngine`:
  - Accept quote → create atomic trade
  - Reserve balances (lock available → locked)
  - Generate trade record with microsecond timestamp
  - Emit `TradeExecuted` event (RabbitMQ)
  - Update balances atomically (DB transaction)
  - Publish price update if trade impacts reference price

### 1.3 Wallet & Settlement (Hybrid)
- [ ] `CustodyProvider` interface:
  - `InternalLedgerProvider`: off-chain balance movements
  - `EVMCustodyProvider`: Ethereum/EVM chains (deposit detection, withdrawal signing)
  - `UTXOCustodyProvider`: Bitcoin/UTXO chains
  - `FiatProvider`: Bank API integration (stub for MVP)
- [ ] `SettlementService`:
  - `SettleTrade(trade_id, mode: OFF_CHAIN | ON_CHAIN)`
  - Off-chain: instant balance transfer
  - On-chain: create withdrawal request, monitor confirmations
  - Config-driven per asset: `asset.custody.mode`

### 1.4 Account & API System
- [ ] `UserService`: registration, KYC tiers, profile management
- [ ] `SubAccountService`: CRUD, API key generation (HMAC-SHA256 + Ed25519), permission scopes
- [ ] `AuthMiddleware`: JWT + API key auth, rate limiting per sub-account
- [ ] Rate limits: REST (100/s), WS (50/s), Trading (10/s) per sub-account

### 1.5 Public API (REST + WebSocket)
- [ ] REST endpoints:
  - `GET /api/v1/assets` - list all assets with metadata
  - `GET /api/v1/markets` - list markets with params
  - `GET /api/v1/markets/{market}/ticker` - 24h stats
  - `GET /api/v1/markets/{market}/price` - current bid/ask/mid
  - `GET /api/v1/markets/{market}/candles` - OHLCV (1m, 5m, 1h, 1d)
  - `POST /api/v1/otc/quote` - request OTC quote
  - `POST /api/v1/otc/execute` - execute quote
- [ ] WebSocket channels:
  - `price.{market}` - real-time bid/ask/mid
  - `trade.{market}` - executed trades
  - `ticker.{market}` - 24h rolling stats
  - Authenticated: `account.orders`, `account.balances`, `account.trades`

### 1.6 Admin API
- [ ] `GET /api/v1/admin/assets` - CRUD assets
- [ ] `GET /api/v1/admin/markets` - CRUD markets, toggle status
- [ ] `POST /api/v1/admin/prices/override` - manual price override (with audit log)
- [ ] `GET /api/v1/admin/users` - user management, KYC review
- [ ] `GET /api/v1/admin/trades` - trade surveillance

### 1.6 KYC/AML Foundation
- [ ] `KYCService`: document upload, verification workflow, tier assignment
- [ ] `AMLService`: transaction monitoring rules (velocity, counterparty, sanctions screening stub)
- [ ] Integration points for external providers (Sumsub, Onfido, etc.)

---

## Phase 2: Spot Trading (Post-MVP)

### 2.1 Order Book & Matching Engine
- [ ] `OrderBook` per market: price-time priority, lock-free reads
- [ ] `MatchingEngine`:
  - Limit orders (GTC, IOC, FOK, GTX)
  - Market orders with protection (max slippage %)
  - Self-trade prevention (cancel oldest/newest/both)
  - Partial fills, fee calculation per tier
- [ ] `OrderGateway`: validate, risk-check, persist, publish to matching

### 2.2 Market Data Enhancements
- [ ] Level 2 (top 50) and Level 3 (full) order book snapshots + deltas
- [ ] Trade feed with aggressive/passive side
- [ ] Funding rate calculation (for future perp)
- [ ] Index price / mark price for risk

### 2.3 Margin Engine (Foundation)
- [ ] Cross/isolated margin accounts
- [ ] Liquidation engine (price-based, partial liquidation)
- [ ] Insurance fund

---

## Phase 3: Derivatives (Future)

### 3.1 Perpetual Futures
- [ ] Funding rate engine (8h intervals)
- [ ] Mark price = index + premium
- [ ] Auto-deleveraging (ADL)
- [ ] Risk limits per user/position

### 3.2 Options (Future)
- [ ] European cash-settled
- [ ] Greek calculations, IV surface

---

## Data Flow Summary (MVP)

```
External Providers (WS/REST)
        ↓
PriceAggregator (normalization, VWAP, outlier filter)
        ↓
Redis Streams (price:{market}) + RabbitMQ (price.updates)
        ↓
├── QuoteService (RFQ pricing)
├── OTCMatchingEngine (execution)
└── Public WebSocket (broadcast to clients)
        ↓
Database (trades, balances, orders)
        ↓
SettlementService (InternalLedger | EVM | UTXO | Fiat)
```

---

## Key Technical Decisions

| Decision | Choice | Rationale |
|----------|--------|-----------|
| Price precision | `int64` (scaled by 1e8) or `decimal.Decimal` | Avoid float errors; microsecond timestamps |
| Order ID | UUID v7 (timestamp-ordered) | Sortable, distributed-friendly |
| Trade ID | UUID v7 | Same |
| Balance locking | DB advisory lock + row lock | Prevent race conditions |
| WebSocket scaling | Redis PubSub + single consumer per pod | Horizontal scaling ready |
| Config | DB + YAML hybrid | Runtime updates + version control |
| Auth | JWT (access) + API keys (HMAC/Ed25519) | Standard, supports sub-accounts |

---

## Risks & Mitigations

| Risk | Impact | Mitigation |
|------|--------|------------|
| Price feed latency | Stale quotes, arbitrage | Multi-provider, outlier filter, sub-ms processing |
| Balance race conditions | Double-spend | DB transactions + advisory locks |
| WebSocket connection storm | OOM, CPU spike | Connection limits, backpressure, rate limiting |
| Provider outage | No prices | REST fallback, cached last-known, circuit breaker |
| Settlement failures | Stuck funds | Idempotent retries, dead letter queue, manual review |

---

## Validation Plan

### Unit Tests (per package)
- [ ] Price aggregation math (VWAP, spread, slippage)
- [ ] Quote expiry and signature verification
- [ ] Balance locking/unlocking atomicity
- [ ] Order matching logic (price-time priority)

### Integration Tests (Testcontainers)
- [ ] Full OTC flow: quote → execute → settle → notify
- [ ] Price feed failover (WS → REST → cache)
- [ ] WebSocket broadcast to multiple clients
- [ ] KYC workflow

### Load Tests (k6)
- [ ] 10k concurrent WS connections
- [ ] 1k RFQ/s with <10ms p99
- [ ] 5k trades/s throughput

### Chaos Tests
- [ ] Provider disconnect/reconnect
- [ ] Redis failover
- [ ] DB primary failover
- [ ] Network partition

---

## Open Questions (Resolve Before Implementation)

1. **Database**: Stay with PostgreSQL or evaluate TimescaleDB for time-series (candles, trades)?
2. **Message broker**: RabbitMQ sufficient or need Kafka for event sourcing/audit trail?
3. **Custody**: Which chains first? (EVM, BTC, TRON, Solana?)
4. **Fiat**: Banking partner API spec available?
5. **Compliance**: Sanctions list provider (OFAC, Chainalysis, TRM)?
6. **Deployment**: Kubernetes? Need Helm charts?
7. **Multi-region**: Active-active or active-passive?

---

## Immediate Next Steps

1. Create `feat/exchange-core` branch from `main`
2. Implement Phase 0 (domain models, migrations, config)
3. Implement Phase 1.1 (Price Aggregator) - core of everything
4. Implement Phase 1.2 (OTC Quote + Matching)
5. Implement Phase 1.3 (Wallet + Settlement)
6. Implement Phase 1.4-1.6 (API, Admin, KYC)
7. Load test → iterate → Phase 2

---

## File Structure (Target)

```
crypto-exchange/
├── cmd/
│   ├── server/           # Main API server
│   ├── price-aggregator/ # Standalone price service (future)
│   └── settlement/       # Standalone settlement worker (future)
├── internal/
│   ├── domain/           # Core entities, interfaces
│   │   ├── asset/
│   │   ├── market/
│   │   ├── order/
│   │   ├── trade/
│   │   ├── balance/
│   │   └── account/
│   ├── price/            # Price aggregation & distribution
│   │   ├── aggregator/
│   │   ├── cache/
│   │   └── provider/
│   ├── otc/              # OTC trading engine
│   │   ├── quote/
│   │   ├── matching/
│   │   └── settlement/
│   ├── spot/             # Future: order book, matching
│   ├── wallet/           # Multi-custody abstraction
│   │   ├── internal/
│   │   ├── evm/
│   │   ├── utxo/
│   │   └── fiat/
│   ├── auth/             # Enhanced: API keys, sub-accounts
│   ├── kyc/              # KYC/AML
│   ├── api/              # HTTP handlers, WS hubs
│   │   ├── rest/
│   │   ├── ws/
│   │   └── admin/
│   └── config/           # Enhanced config
├── configs/              # YAML configs per environment
├── migrations/           # SQL migrations
└── deploy/               # Docker, K8s, Helm
```