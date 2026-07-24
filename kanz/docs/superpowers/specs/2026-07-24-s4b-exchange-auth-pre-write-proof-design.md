# S4b — Shared exchange-auth + pre-write account proof (design)

## The gap S4a left

S4a made the operator able to **write** a venue API key set (k8s Secret in dev, Vault
KV-v2 in prod). Nothing checks the keys before they land. A typo, a key from the wrong
sub-account, or a revoked key is written happily; the failure surfaces later, at a venue
adapter's boot, as `accountproof: could not ask the exchange...` — far from the human who
typed it, and after the bad value is already the deployed truth.

The platform already knows how to ask an exchange "whose key is this?" —
`internal/venueadapter/accountproof` does it at adapter startup (SOV-02a). But the code
that actually *makes the signed call* lives inside each venue service's own internal
package (`services/venue-okx/internal/okx/okx_rest.go`,
`services/venue-binance/internal/binance/binance_rest.go`), which Go's `internal` rule
makes unimportable from `services/operator`. The capability exists and cannot be reused.

## Shape

Three layers, one signer per venue, no duplication:

```
accountproof (policy: is this the account we CLAIM?)  ← unchanged, adapter-only
      │ uses
exchangeauth (transport: which account IS this?)      ← NEW, kanz/internal/venueadapter/exchangeauth
      ↑ used by                    ↑ used by
venue adapters (okx/binance REST)  operator (pre-write proof)
```

`exchangeauth` owns, for each venue, exactly one implementation of:

- the **signature scheme** — OKX `base64(HMAC-SHA256(ts+method+path+body))` in
  `OK-ACCESS-*` headers plus the API passphrase; Binance `hex(HMAC-SHA256(query))` appended
  to the query string with `X-MBX-APIKEY`;
- the **account endpoint** and its decode — OKX `GET /api/v5/account/config` → `data[0].uid`,
  Binance `GET /api/v3/account` → `uid`.

The venue REST clients keep their own trading calls, their weight buckets and their
DNS-caching transport, and **delegate** signing and `ExchangeAccountID` to `exchangeauth`.
Net effect: the HMAC code exists once per venue instead of twice, and the operator gains
the capability without importing a venue service.

## What the operator proves

The operator has **no bound UID** at key-entry time (nothing has said which exchange
account this deployment is meant to be — that binding is the adapter's
`*_VENUE_ACCOUNT_UID`, out of scope here). So the operator does **not** call
`accountproof.Resolve`. It calls `exchangeauth.AccountID` and proves the weaker, still
decisive fact:

> These credentials authenticate against this exchange, and the exchange names account
> `<uid>` as their owner.

That kills the typo, the revoked key, the wrong-exchange paste, and the missing
passphrase — before the write. Binding `<uid>` to an expected value is a later slice.

## Behaviour

- Order at the handler: **validate → prove → write**. A failed proof means **no write at
  all**; the previously stored key set is untouched.
- Failure is `codes.FailedPrecondition` with a **fixed, sanitized reason** — the exchange's
  own response body never reaches the client, and key material never reaches a log, an
  error string, or a response. S4a's write-only invariant is unchanged and unweakened.
- Success returns the resolved `exchange_account_id`. This is a public fact (it is already
  stamped on every OrderState and Fill as the account label's binding), not key material.
- **Degradation:** proof is deploy-time configuration. Unconfigured ⇒ the operator behaves
  exactly as S4a (write, no proof). There is **no per-request bypass** — a caller cannot
  ask for the check to be skipped.
- Proof requires an **explicit per-venue base URL**. There is no default: an operator
  defaulting to the venue adapters' Binance testnet URL would reject valid production keys,
  and one defaulting to mainnet would send a rig's dummy keys to the real exchange. With
  proof enabled and no URL for that venue, the write is refused, not silently unproven.

## New egress

The operator has never made an outbound call. Its NetworkPolicy is ingress-only (health
`:8091`; the gRPC listener is reached through a kubeconfig-gated port-forward). Proof adds
the operator's first egress: DNS, plus `:443` to the public internet with RFC1918 and the
cluster CIDRs excluded — so a compromised operator cannot use this new permission to reach
in-cluster services. An arch guard pins that shape, mirroring
`operator_venue_secret_rbac_test.go`.

## Out of scope

UID bound enforcement (comparing against an expected account); unifying the **trading**
signers (only signing primitives and the account call are shared); key rotation; proving on
`ListVenueKeys` (presence stays value-blind and call-free).
