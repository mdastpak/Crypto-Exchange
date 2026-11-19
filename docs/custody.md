# Custody

The custody of crypto currencies on chain, TRON first ([#68](https://github.com/mdastpak/Crypto-Exchange/issues/68)).

| Part | Status |
| --- | --- |
| [Deposit addresses](api.md#deposit-addresses), derived from an xpub | Done |
| [The deposit watcher](api.md#deposits): confirmations, reorgs, holds | Done |
| The signer, `cmd/tron-signer` | Done |
| [Withdrawals paid by the signer](api.md#withdrawals) | Done |
| [Sweeps](#sweeps) by the signer, to the hot wallet or the cold storage | Done |
| Sweeps started by the server for the credited deposits | To come |

## Keys

One BIP32 seed holds every key of the custody, and only the signer holds it:

| Path | Key | Who derives it |
| --- | --- | --- |
| `m/44'/195'/0'/0/i` | Deposit address `i` | The server, from the xpub of `m/44'/195'/0'`, without the private keys; the signer, with them, for the sweeps |
| `m/44'/195'/1'/0/0` | The hot wallet, which pays the withdrawals | The signer only: the server's xpub cannot derive it |

The server is given only the xpub of the deposit account (`custody.providers[].xpub`); an extended private key stops it. The cold storage is not derived from this seed.

## The signer

`cmd/tron-signer` is a service of its own, on its own host, which the server calls over mutual TLS to pay the withdrawals on TRON: `custody.providers[].signer` names its `url` (`https://` only), the server's client certificate and key, and the CA of the signer's certificate. It holds the seed, and signs the hot wallet's payments: it builds each transaction itself, in the protobuf of the protocol, so that a node that lies cannot make it sign another transfer than the one it was asked for, signs it, and broadcasts it.

```bash
go build -o tron-signer ./cmd/tron-signer
VAULT_TOKEN=... TRONGRID_API_KEY=... ./tron-signer -config /etc/tron-signer/config.yaml
```

Its configuration is a file of its own; `config/tron-signer.example.yaml` documents it. A field it does not know stops it.

### The seed

The seed is never in a file: it is encrypted by a key of Vault's Transit engine, which decrypts it once at the start with `VAULT_TOKEN`. The signer derives the keys it needs and clears the seed. To set it up, on an offline machine:

```bash
head -c 32 /dev/urandom > seed.bin
vault write -field=ciphertext transit/encrypt/tron-seed plaintext="$(base64 < seed.bin)" > seed.enc
```

Keep a copy of `seed.bin` offline, as the recovery of the custody, and remove it from the machine. Then print what the server is configured with:

```bash
VAULT_TOKEN=... ./tron-signer -config config.yaml -xpub
```

It prints the xpub of the deposit account, for `custody.providers[].xpub`, and the hot wallet's address, for `hot_wallet_address`. Fund the hot wallet with TRX for the fees of the token payments.

### Mutual TLS

The signer serves TLS 1.3 only, and requires a client certificate signed by `tls.client_ca_file`, whose common name is one of `tls.allowed_clients`. Any other client is refused at the handshake, or with `403`.

### The API

| Route | |
| --- | --- |
| `GET /v1/hot-wallet` | `{"address"}`: the address that pays |
| `POST /v1/payments` | `{"key", "to", "asset", "amount"}`: pays `amount`, in the smallest unit, of `asset` to `to`, once by `key`. It answers the payment: `key`, `tx_id`, `status`, `expires_at` |
| `GET /v1/payments/{key}` | Where a payment is |
| `POST /v1/sweeps` | `{"key", "index", "asset"}`: takes the [sweep](#sweeps) of `asset` from deposit address `index` a step further, once by `key`, and answers where it is |
| `GET /v1/sweeps/{key}` | Where a sweep is, without taking it further |

A payment is `pending` until its transaction has `confirmations` (19) confirmations and is then `completed`. It is `failed` when its transaction failed on chain, such as a token transfer that ran out of energy, and `not_received` when its transaction expired, 10 minutes after it was signed, and no block has it.

`POST` with the key of a payment answers that payment, and broadcasts its transaction again while the network may still take it. If the transaction expired and was never taken, the same key signs a new one. A key of another payment answers `409 key_reused`.

The signer refuses, with `422 refused`:

- an asset that is not in `assets`;
- an address that is not a TRON address, its own hot wallet, or the token's contract;
- an amount that is not a positive integer;
- an amount above `max_per_transfer`;
- an amount that would take the day's payments of the asset, in UTC, above `max_per_day`.

A node that does not answer is `502`. A payment that was recorded is then answered with its transaction, which the next call sends again.

### Its state

`state_path` keeps every payment by its key, with its signed transaction, and what each day used of the caps. A payment is recorded before it is broadcast, and the file is replaced whole, written to a new file that is synced and renamed, so that a crash leaves either state. Run one instance: two would each count their own caps.

## Sweeps

A sweep moves the whole balance of an asset from a deposit address to the hot wallet. The signer signs it with the address's own key, `m/44'/195'/0'/0/<index>`, and each call of `POST /v1/sweeps` with its key takes it a step further:

1. **Top-up.** A token sweep needs TRX for its fee. When the address holds less than `sweeps.top_up_sun` (the fee limit by default), the hot wallet first sends it the difference, and the sweep waits until that transaction is final.
2. **The sweep.** The address sends its whole balance of the token, or of TRX less `sweeps.reserve_sun`, and the sweep waits until that transaction is final. It is then `completed`, with the `amount` and where it went (`to`).

An address that holds nothing of the asset completes at once, with `amount` 0. A transaction that expired untaken is signed again from the balance. One that failed on chain fails the sweep, and a new key sweeps again. The payments' caps do not apply, because a sweep only pays the custody's own addresses.

**Hot and cold.** An asset with a `hot_cap` is swept to `sweeps.cold_address` when the hot wallet would hold more than its cap. The cold storage is not derived from the signer's seed, so the signer can send to it but never from it. An asset without a cap always goes to the hot wallet, and `hot_cap` requires `cold_address`.

The hot wallet's top-ups are no deposits: set the server's `hot_wallet_address` to the address that `-xpub` prints, which the [deposit watcher](api.md#deposits) ignores.
