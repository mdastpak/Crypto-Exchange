# AML rules

The anti-money-laundering rules ([#73](https://github.com/mdastpak/Crypto-Exchange/issues/73)) evaluate each operation that moves money, in its transaction, and raise alerts that administrators work. This document describes the rules file, what each rule type means, how a decision is enforced, and how the alerts move. The admin routes are in [the API](api.md#aml-alerts).

The `SANCTIONS` rules that name the `ofac` provider screen against the OFAC list (see [Sanctions screening](#sanctions-screening)). Mixer, darknet, and PEP screening, and sanctions rules that name only Chainalysis or TRM, need those providers, which implement the same interface (`aml.Screener`); until one is configured, those rules are loaded, synced, and listed, but not evaluated, and the server names each one at startup.

## The rules file

[`config/aml_rules.yaml`](../config/aml_rules.yaml) is the source of the rules. It is built into the release; `compliance.aml.rules_path` names another file ([configuration](configuration.md#settings-reserved-for-roadmap-features)). While `compliance.aml.enabled` is on, the server:

1. loads the file and checks every key: an unknown key, a field that the rule's type does not use, a missing threshold, or a rule listed twice keeps it from starting, with every problem listed at once;
2. refuses a file that lists the country of the deployment as high-risk: an Iranian deployment (`deployment.region: ir`) does not start with `IR` in a country rule, which would refuse nearly every user;
3. syncs the file into the `aml_rules` table, under one lock over every instance. A new rule is version 1; a rule whose definition changed gets its next version; a rule that left the file is retired, never deleted, and comes back at its next version. Every version is kept in `aml_rule_history`, and every change is audited as `aml.rule_sync`.

A sync that fails is logged, and the rules already in the database stay in force. Every rule and every version is [sealed](data-protection.md#row-seals): a rule disabled or loosened in SQL fails its seal, with its finding, and while it does no withdrawal is evaluated, so none runs (`503 withdrawals_unavailable`) until an administrator reviews the finding. The sync does not overwrite it either.

## Rules

Every rule has:

| Key | Meaning |
| --- | --- |
| `id` | Its code, 1 to 64 lower-case letters, digits, or underscores; the alerts name it |
| `name`, `description` | For the administrators |
| `rule_type` | One of the types below |
| `enabled` | A disabled rule is synced but not evaluated |
| `applies_to` | The operations it evaluates: `deposit`, `withdrawal`, `trade` |
| `config` | The settings of its type, and only those |
| `threshold_count`, `threshold_amount` | What its type compares; amounts are in USDT |
| `time_window_hours` | The window, 1 to 2160 hours, over which it counts the user's operations |
| `action` | `ALERT`, `REVIEW`, `BLOCK`, or `FREEZE` |
| `severity` | `LOW`, `MEDIUM`, `HIGH`, or `CRITICAL`: the risk score of its alerts, 25 to 100 |

The window holds the user's operations that committed and that the rule applies to, with the one evaluated. An operation that a rule refused is recorded apart and not counted.

### Amounts

An amount is valued in USDT at the mid of the live price of `SYMBOL/USDT` of the [price engine](price-engine.md), or of `USDT/SYMBOL` inverted, such as toman; USDT is its own value. The value of an operation is kept with it. A past operation recorded without one is valued at the current price.

A rule that needs a value that no live price gives fails closed: the operation is held for review (`REVIEW`, with `reason: no_price` in the alert), whatever the rule's own action. A rule that would refuse holds instead, as it cannot know.

### `VELOCITY`

`config.metric` is one of:

| Metric | Hits when |
| --- | --- |
| `count` | the operations of the window reach `threshold_count` |
| `volume` | their value reaches `threshold_amount` |
| `single_amount` | the value of the operation alone reaches `threshold_amount` |
| `deposit_withdrawal_cycles` | the withdrawals of the window that follow a deposit of the window reach `threshold_count`. It applies to withdrawals; the deposits count whatever it applies to |

### `STRUCTURING`

Several operations just below a reporting threshold. The band is `threshold_amount` up to `config.ceiling`, excluded. The rule hits an operation in the band when the operations of the window in the band, with it, reach `threshold_count`, at least 2. An operation at or above the ceiling is reported, not structured. Past operations without a value that could complete the pattern hold it for review.

### `HIGH_RISK_COUNTRY`

`config.check_fields` names the countries of an operation that the rule reads: `ip_country`, the country of the request (see [GeoIP](geoip.md)), and `counterparty_country`, that of the other side; for a fiat withdrawal, the bank of the destination, `IR` in an Iranian deployment. A country that is not known matches nothing.

- With `config.countries`, an operation matches when a checked country is listed.
- With `config.cross_border: true` and `config.home_country`, it matches when a checked country is known and is not the home country.

Without a threshold, the rule hits each operation that matches. With `threshold_amount`, it hits one that matches once the value of the operations of the window that match reaches it.

### `UNUSUAL_PATTERN`

`config.pattern` is one of:

- `withdrawal_profile`: a withdrawal unlike the user's own of the last `config.history_days`. It runs `config.checks`, and hits when `threshold_count` of them fire:
  - `new_destination`: the destination was added less than `new_destination_days` ago;
  - `amount_deviation`: the value is at least `amount_deviation_multiplier` times the average of the history;
  - `time_deviation`: with at least three in the history, every one of them was more than `time_deviation_hours` from the hour of the day of this one, in UTC;
  - `velocity_spike`: the withdrawals of the window, with this one, are at least two, and at least `velocity_spike_multiplier` times as many as the history has in a window on average.
  A check without a history does not fire.
- `dormant_account`: the first operation after `config.dormant_days` without one. An account without any operation counts from when it joined, or from when the engine recorded its first operation of anyone, whichever is later, so that the accounts of before the engine are not all dormant at once.

### Screening types

`SANCTIONS` (`config.providers`: `ofac`, `chainalysis`, `trm`; `config.screen`: `user` or `counterparty`), `MIXER` and `DARKNET` (`config.list`), and `PEP` (`config.providers`) ask a screening provider. The engine has one screening provider: `ofac` while it is enabled, which screens the `SANCTIONS` rules that name it; the other screening rules are not evaluated.

## Sanctions screening

While the `ofac` provider of `compliance.sanctions` is enabled ([configuration](configuration.md#settings-reserved-for-roadmap-features)), each instance:

- downloads the OFAC list of Specially Designated Nationals, `sdn.csv` from the provider's `endpoint` and `alt.csv`, the other names, beside it, when the stored list was last found current more than `refresh_hours` (24) ago. Every entry keeps its names, its programs, its type, and the addresses on chain that its remarks list (`Digital Currency Address - XBT ...`). A list that differs from the last is stored as a new version in `sanctions_lists`, compressed; the same list only records that it is current (`checked_at`). The first instance to store wins; the others find it current;
- loads the newest version every minute, checked against its seal, which covers the body by its SHA-256: an entry removed in SQL fails the load, with its finding;
- screens in memory, by `config.screen`:
  - `counterparty`: the address of a crypto destination against the addresses of the list, exactly, and hex addresses in any case. A match takes the rule's action, such as `BLOCK`.
  - `user`: the user's first and last name, as the profile has them, against the names and other names of the list, by Jaro-Winkler similarity from `name_match_threshold` (0.92), over the words in any order and without accents or punctuation. A name of one word matches nothing, nor do vessels and aircraft. A name alone does not tell a person, so a match holds the operation for a review, never more, whatever the rule's action. The names are read with the seal of the user's row: a name changed in SQL to slip past the list is refused. A user without a name is not screened by name; the names are not transliterated, so Persian names are compared as they are written.

Without a list, or with one that no download found current for `max_list_age_hours` (72), every operation that a sanctions rule screens is held for review (`reason: list_unavailable`): the screening fails closed. `GET /admin/aml/sanctions` shows the version, when it was downloaded and found current, its entries, and whether it is stale. An alert of a match names the entries (id, name, type, programs, score), at most five, never the user's name.

## Decisions

Every enabled rule that applies to the operation is evaluated; the strongest action of those that hit decides:

| Action | The operation | Also |
| --- | --- | --- |
| `ALERT` | runs | an alert |
| `REVIEW` | waits for an administrator: a withdrawal has `status: review` and `review_reason: aml`, approved or rejected on [its routes](api.md#withdrawals) | an alert, and the notification |
| `BLOCK` | is refused | an alert, and the notification |
| `FREEZE` | is refused | an alert, the notification, and a freeze of the account: a [restriction](api.md#restrictions) of scope `account` and reason `aml_review`, created by the system (the nil UUID), whose `case_ref` the alert names |

Each rule that hit raises its own alert, with what it saw (`trigger_data`), the version of the rule, and the operation. The operation and its alerts commit with it; a refused one rolls back, and is then recorded as refused, with its alerts and the freeze, in one transaction. The user is told only that the operation is refused (`403 withdrawal_refused`), never the rule. While the rules cannot be evaluated, such as for a row changed in SQL, the operation is refused (`503 withdrawals_unavailable`).

### Where the rules run

- **Withdrawals** evaluate at both steps, with the request's country and the destination: at step one, so that no code is sent for a withdrawal that would be refused, and at step two, which records it.
- **Deposits** on chain evaluate once their block is confirmed, in the transaction that credits them, with the address they came from as the counterparty, which the sanctions rules screen. `REVIEW` holds the deposit, pending in the user's balance, until an administrator credits it; `BLOCK` and `FREEZE` hold it too, after the refusal is recorded, as the funds are already in custody ([#68](https://github.com/mdastpak/Crypto-Exchange/issues/68), see [deposits](api.md#deposits)).
- **Trades** call the same `aml.Engine.Evaluate` in their transaction, with the operation, once the path of the OTC trades wires it in.

## The batch

Every `engine.evaluation_interval_seconds`, each instance evaluates the `VELOCITY` rules over a window and the `STRUCTURING` rules again, over the windows of the users who had operations in them, for the patterns that no single operation completed, such as operations of a kind whose path does not evaluate yet. It raises an alert once per rule, user, and window, and none when the rule raised one for the user within the window. It holds and refuses nothing, as the operations ran: its alerts are `ALERT`, except that a `FREEZE` rule freezes the account. It pauses with the scheduler (`scheduler` [control](api.md#maintenance-and-lockdowns)).

It also escalates the alerts left `OPEN` for `engine.alert_escalation_hours`.

## Alerts

An alert is `OPEN`, `INVESTIGATING`, `ESCALATED`, `RESOLVED`, or `DISMISSED`:

| From | To |
| --- | --- |
| `OPEN` | `INVESTIGATING`, `ESCALATED`, `RESOLVED`, `DISMISSED` |
| `INVESTIGATING` | `ESCALATED`, `RESOLVED`, `DISMISSED` |
| `ESCALATED` | `INVESTIGATING`, `RESOLVED`, `DISMISSED` |

`INVESTIGATING` assigns the alert to the administrator who moves it. `RESOLVED` and `DISMISSED` are final, and need a resolution. Moving an alert releases nothing: a held withdrawal is approved or rejected, and a freeze lifted, on their own routes. Each move is audited as `aml.alert_status`, and each alert as `aml.alert`, by the system, with the IP of the request that raised it. Alerts are sealed: one closed or reassigned in SQL is refused with its finding.

## Notifications

With `engine.notifications.email.enabled`, the alerts that hold, refuse, or freeze, and the escalations, are emailed to `recipients` through `notifications.email`, once their transaction committed: the alert, the rule, and the user's id, never the user's contacts or amounts. A failure is logged; the alerts are kept whatever it does. Every alert is also logged.
