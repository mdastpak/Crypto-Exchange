# Observability

The server writes structured logs, serves Prometheus metrics on a port of their own, and sends OpenTelemetry traces ([#50](https://github.com/mdastpak/Crypto-Exchange/issues/50)). The settings are in [`observability`](configuration.md#observability).

## Logs

Every line that a process writes is a record of `log/slog`: JSON by default, one object per line, with `time`, `level`, and `msg`.

- **The lines of the standard `log` package**, which most of the code writes, become records too. Their level comes from their prefix:
  - `SECURITY:` is `WARN`, with `"security": true`;
  - `WARNING:` is `WARN`;
  - a line that starts with `ERROR:`, `Failed`, or `Error` is `ERROR`;
  - every other line is `INFO`.
- **Each request** is one record, `"msg": "request"`, after it is answered. It holds:
  - `method` and `route`: the pattern, such as `/api/v1/user/contacts/:id`, never a value of the path, and never the query string;
  - `status` and `duration_ms`;
  - `request_id`, and the `user_id` of a signed-in user or administrator.

  A server error is `ERROR`; every other answer is `INFO`.
- **Redaction.** A layer between the records and the output hides what the logs must never hold, whatever writes it. A test writes each of these in every place a record carries text, and checks that none reaches the output:
  - email addresses, as `[redacted-email]`;
  - mobile numbers, Iranian or international, as `[redacted-phone]`;
  - JWTs, as `[redacted-token]`;
  - bearer tokens;
  - the password of a URL;
  - `password=…`, `secret=…`, `token=…`, and the like;
  - the value of every attribute named `password`, `secret`, `token`, `authorization`, `cookie`, `otp`, `recovery_code`, `national_id`, and the like.

  The code also masks contacts before it logs them; redaction is the layer beneath, for what a change forgets.

`cmd/admin` writes its log records in text form on stderr, apart from the output of its commands on stdout.

## Metrics

With `observability.metrics.enabled`, each instance serves its metrics at `http://<host>:<metrics.port><metrics.path>` (`:9090/metrics`), on an address of its own, never on the address of the API. Keep the port private, as the admin routes: scrape it from the internal network.

| Metric | Type | Labels | What |
| --- | --- | --- | --- |
| `http_requests_total` | counter | `method`, `route`, `status` | Requests answered. `route` is the pattern, or `unmatched`. |
| `http_request_duration_seconds` | histogram | `method`, `route` | Time to answer. |
| `rate_limit_rejections_total` | counter | `limiter` | Refusals of the limiters: `rest`, `rest-probe`, `ws`, `trading`, `auth:login`, `auth:otp`. |
| `rabbitmq_publish_duration_seconds` | histogram | `exchange`, `outcome` | Time to publish a message and receive its publisher confirm; `outcome` is `ok` or `error`. |
| `row_seal_findings_total` | counter | `table`, `check` | [Integrity findings](data-protection.md#integrity-findings) opened, by read or by the verifier. |
| `ledger_operations_total` | counter | `kind`, `outcome` | Operations of the [ledger](architecture.md#the-ledger): `applied`, `replayed` (a retry of an applied key), `insufficient`, `invalid`, `conflict` (a key of another request), `unverified` (a balance fails its seal), `failed`. |
| `ledger_operation_duration_seconds` | histogram | `kind` | Time of an operation of the ledger, to its commit. |
| `ledger_reconciliation_mismatches` | gauge | | Balances that differed from the replay of their changes at the last reconciliation, on the instance that ran it. |
| `ledger_reconciliation_imbalanced_currencies` | gauge | | Currencies whose books did not sum to zero, or whose custody held other than it must, at the last reconciliation. |
| `ledger_reconciliation_last_success_timestamp_seconds` | gauge | | Unix time of the last completed reconciliation. One instance at a time runs it, every hour. |
| `db_connections_open`, `db_connections_in_use`, `db_connections_idle` | gauge | | The PostgreSQL pool of the instance. |
| `db_connection_waits`, `db_connection_wait_seconds` | gauge | | Waits for a connection of the pool, and their time, since the start. |
| `redis_up` | gauge | | 1 when Redis answers a ping within a second. |
| `rabbitmq_up` | gauge | | 1 when the connection to RabbitMQ and its publishing channel are up. |
| `websocket_connections` | gauge | | Open WebSocket connections of the instance. |
| `notification_sms_sends_total` | counter | `provider`, `result` | Messages handed to an [SMS provider](configuration.md#sms-providers) ([#371](https://github.com/mdastpak/Crypto-Exchange/issues/371)): `accepted`, `provider_error` (the provider failed; the next one is tried), `refused` (the message itself, such as an invalid number). |
| `notification_sms_fallbacks_total` | counter | `from`, `to`, `reason` | Messages sent through another provider than the first: `reason` `send` when the first failed to take it, `delivery` when it took it and did not deliver it. |
| `notification_sms_delivery_reports_total` | counter | `provider`, `state` | Delivery reports of the messages sent: `delivered`, `failed`, `unknown` (no report, such as a sandbox message), `pending` (still on its way after the last check). |

The registry also has the metrics of the Go runtime (`go_*`) and of the process (`process_*`).

### Alerts

The metrics are meant for these alerts, at least:

| Alert | Rule | Why |
| --- | --- | --- |
| Ledger does not reconcile | `max(ledger_reconciliation_mismatches) > 0` or `max(ledger_reconciliation_imbalanced_currencies) > 0` | A balance or a currency of the books changed outside the ledger. Page someone; `admin ledger reconcile` lists them. |
| Ledger not reconciled | `time() - max(ledger_reconciliation_last_success_timestamp_seconds) > 3 * 3600` | No instance completed the hourly reconciliation for three hours. |
| Integrity finding | `increase(row_seal_findings_total[5m]) > 0` | A row changed outside the backend, or a seal missing in `enforce` mode. Page someone; see [Integrity findings](data-protection.md#integrity-findings). |
| Server errors | `sum(rate(http_requests_total{status=~"5.."}[5m])) / sum(rate(http_requests_total[5m])) > 0.01` | More than one request in a hundred fails. |
| Redis or RabbitMQ down | `min(redis_up) == 0` or `min(rabbitmq_up) == 0` for 2 minutes | Sign-in codes are not delivered, and the limits fail open or closed as configured. |
| Pool exhausted | `rate(db_connection_waits[5m]) > 0` and `db_connections_in_use == db_connections_open` | Requests wait for PostgreSQL connections. |
| Slow publishes | `histogram_quantile(0.99, sum(rate(rabbitmq_publish_duration_seconds_bucket[5m])) by (le)) > 1` | The broker confirms slowly; the outbox falls behind. |
| SMS provider failing | `sum by (provider) (increase(notification_sms_sends_total{result="provider_error"}[10m])) > 5` | A provider keeps failing: messages wait for the next one, and its breaker opens. Check its credit, key, and status page. |
| SMS not delivered | `sum by (provider) (increase(notification_sms_delivery_reports_total{state="failed"}[15m])) / sum by (provider) (increase(notification_sms_delivery_reports_total{state=~"delivered\|failed"}[15m])) > 0.2` | More than a fifth of a provider's messages do not reach the phones. In auto mode the provider goes behind the others below `min_delivery_percent`; consider the manual mode. |

## Traces

With `observability.tracing.enabled`, the server sends its spans with OTLP over HTTP to `observability.tracing.endpoint`, such as an OpenTelemetry Collector at `http://otel-collector:4318`. A share of the traces, `tracing.sample_rate`, is kept; a request whose client sends a sampled W3C `traceparent` header continues that trace instead.

| Span | Kind | Attributes |
| --- | --- | --- |
| `<METHOD> <route>`, such as `POST /api/v1/admin/currencies` | server | `http.request.method`, `http.route`, `http.response.status_code`; failed on a 5xx |
| `postgresql` | client | `db.system.name`, `db.query.text`: the statement, which holds no values, since they are parameters |
| `redis <command>`, `redis pipeline` | client | the command, never its arguments |
| `publish <exchange>` | producer | `messaging.destination.name`; the message carries the `traceparent` header |

The queries and commands of a request are children of its span. A change that writes an event to the outbox stores the `traceparent` of its request with the event (migration 15). The relay that publishes the event, after the commit and maybe on another instance, continues that trace, so the publish of a currency created through the API is part of the trace of its request. Background work, such as the verifier of the row seals, starts traces of its own.

Without `tracing.enabled`, the spans go nowhere, at almost no cost.
