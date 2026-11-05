# Binance depth runtime

This is a source-first Rust migration of the existing public Binance depth feed.
It synchronizes WebSocket updates with a REST anchor, folds an exact in-memory
book, and publishes bounded canonical `market.book.snapshot` and
`market.crypto.quote` FACTs through JetStream. It has no order interface,
exchange credential, database or financial admission authority. #1413 tracks
the full architecture transition and operational acceptance.

The Go trade, coverage, candle and volume-profile paths remain in market-ingest.
`MARKET_INGEST_BINANCE_EXTERNAL_DEPTH_INSTRUMENTS` selects canonical instrument
IDs whose Binance depth publisher is handed off. Go refuses external depth for
alpha-engine runners, conflicting local depth, or missing trade sources. OKX
and unassigned Binance feeds remain unchanged. This is static configuration,
not a distributed ownership lease.

## Contracts and intentional refusals

The build uses existing Protobuf sources and the shared generated
`common.v1.Decimal`; no parallel schema exists. Numeric conversion preserves
Go's eight-place half-away rounding and repeated rescaling to int64, including
large quantities and rounding-to-zero deletions. The network parser deliberately
accepts only bounded decimal/scientific venue strings, not Go's broader rational,
hexadecimal or binary syntax. Invalid rows, excess resources and values outside
the canonical exponent domain refuse the complete frame. Kernel wire parity is
tested separately from this stricter network boundary.

A REST anchor has no exchange timestamp. It is kept private until a valid
straddling WebSocket update arrives; only their merged result can be published,
with the venue's timestamp. Source invalidation is independent of the event
queue, and revoked generations cannot start another publication. In-flight
messages sent before a discovered gap cannot be recalled. Readiness requires
the current generation, broker connectivity and a venue timestamp no more than
five seconds old or one second ahead. Both publication and shutdown are bounded.
No latency improvement is claimed without recorded-feed benchmarks.

## Configuration

Required environment:

- `BINANCE_DEPTH_INSTRUMENT_ID`, `BINANCE_DEPTH_SYMBOL`: one canonical instrument
  and its uppercase Binance symbol per process.
- `BINANCE_DEPTH_TENANT`, `BINANCE_DEPTH_SOURCE` (`service/instance`),
  `BINANCE_DEPTH_VERSION`: explicit event attribution.
- `BINANCE_DEPTH_NATS_URL`: broker endpoint without embedded credentials.
- `SPIFFE_ENDPOINT_SOCKET`: required outside explicit loopback development.

`BINANCE_DEPTH_LISTEN` defaults to `0.0.0.0:8088` and exposes only `/healthz`,
`/readyz` and bounded-cardinality `/metrics`. Public exchange defaults are
`wss://stream.binance.com:9443` and `https://api.binance.com`.
`BINANCE_DEPTH_DEVELOPMENT=true` permits loopback fixture endpoints via
`BINANCE_DEPTH_WS_BASE` and `BINANCE_DEPTH_REST_BASE`, and plaintext loopback NATS.
It does not permit an arbitrary remote plaintext broker.

Production uses a rotating SPIFFE source with local trust-domain policy and an
exact NATS peer allowlist: `spiffe://kanz.internal/ns/kanz-messaging/sa/nats`.
Publish waits for JetStream acknowledgement and supplies the existing
`Nats-Msg-Id` and `Kanz-Partition-Key` headers. This source does not create
streams, accounts, ACLs or workload registrations.

## Verification and promotion

From the repository root, generate the existing Go SDK, then:

```sh
(cd kanz && go run ./tools/depth-conformance) > /tmp/kanz-depth.json
cargo test --locked --manifest-path kanz-rs/Cargo.toml --workspace --lib --bins
KANZ_DEPTH_CORPUS=/tmp/kanz-depth.json cargo test --locked --manifest-path kanz-rs/Cargo.toml -p kanz-binance-depth --test go_conformance
cargo build --locked --manifest-path kanz-rs/Cargo.toml -p kanz-binance-depth
```

The migration CI also runs a real Rust subprocess against loopback exchange
fixtures and the actual development NATS broker. Go validates framing, headers,
attribution, venue clocks and the resulting mark width. Its evidence checker
rejects skips and missing results. Those tests do not prove deployed SPIRE
rotation, tenant broker ACLs, workload scheduling or performance acceptance.

Build the container using the repository root as context. No active deployment
is included. Promotion requires a signed immutable image, narrowly scoped NATS
publish/ACK permissions, workload identity, deny-by-default network policy and
resource limits. Stop the previous Binance depth owner for each assignment,
preserve its Go trade path using the exact external-depth list, then start Rust
and verify readiness plus canonical downstream observations. Never run both
publishers for one assignment. Rollback stops Rust before restoring Go depth;
do not roll back the Go trade/coverage capability. Review the gap/catch-up window
and test this handoff in a controlled environment before production promotion.
