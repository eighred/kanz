# OKX public depth

This component owns one configured instrument's public JSON `books` subscription.
Go retains trades, quiet-market coverage, bars and volume profiles. Enabling this
binary alone does not transfer ownership: the matching Go external-depth selection
and verified deployment must be promoted together, with exactly one depth writer.

The shared `kanz-market-feed` crate owns canonical generated protobuf types,
decimal/book arithmetic, FACT framing, SPIFFE/NATS publication and readiness.
No venue credentials, private channels, order operations or alternative financial
types are introduced. Published subjects remain `market.book.snapshot` and
`market.crypto.quote`; publication requires fresh venue time and the current source
epoch. Go's historical malformed-time fallback is deliberately not reproduced.

Required environment: `OKX_DEPTH_INSTRUMENT_ID`, `OKX_DEPTH_SYMBOL` (venue instId),
`OKX_DEPTH_TENANT`, `OKX_DEPTH_SOURCE`, `OKX_DEPTH_NATS_URL` and production
`SPIFFE_ENDPOINT_SOCKET`. The build embeds `KANZ_BUILD_VERSION`; Docker requires
`--build-arg VERSION=<reviewed revision>`. `OKX_DEPTH_LISTEN` defaults to port 8088.
Use the shared runner's development-only version override `OKX_DEPTH_VERSION`
only with `OKX_DEPTH_DEVELOPMENT=true` and loopback NATS.

Production WebSocket is fixed to `wss://ws.okx.com:8443/ws/v5/public`.
`OKX_DEPTH_WS_URL` may select explicit loopback `ws` at the same path only in
development. Redirects, URL credentials, queries and fragments are not accepted.
The source waits for the matching subscription acknowledgement and a full snapshot.
Every frame's identity, action, numeric rows and timestamps are validated before
any of that frame is emitted. Bad rows refuse the whole frame, rather than dropping
unreadable liquidity and publishing a partial book. Limits: 4 MiB WS frame/message,
32 updates per message, 1000 rows per side per update, bounded downstream queue and
10s connect/write/queue/initial-anchor deadlines. Fifteen seconds of application
silence sends text `ping`; text `pong` is required within 5s. A heartbeat does not
fabricate a new book timestamp or restore stale-book readiness.

Sequence continuity uses `prevSeqId`, including valid maintenance resets and zero.
Repeated sequence numbers are accepted only as empty, correctly chained keepalives.
A gap revokes publication validity before waiting on the queue, reconnects after
250ms and requires a new subscription snapshot. Transport failure, malformed data,
timeout or source cancellation terminates the runtime and revokes validity.

The deprecated checksum is not an integrity gate. OKX's June 23, 2026 change fixed
the field to zero and requires sequence validation instead:
https://www.okx.com/docs-v5/log_en/
This component does not select authenticated tick-by-tick, RPI or SBE channels.

From `kanz-rs`: `cargo +1.90.0 test --locked -p kanz-okx-depth` and
`cargo +1.90.0 clippy --locked -p kanz-okx-depth --all-targets -- -D warnings`.
Local WebSocket fixtures prove protocol/cancellation behavior; separate actual
JetStream tests and signed image/deployment checks are required before promotion.
