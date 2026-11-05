# Depth compatibility oracle

Run `go run ./tools/depth-conformance > <temporary-output.json>` from `kanz`.
The command exercises the public Go Binance source using a local HTTP/websocket
fixture, then the actual Go book snapshot/delta path. It never contacts an
exchange or broker and does not expose a production parser helper.

Version 1 contains `snapshotUnixMs` and `cases`. Each case has raw REST JSON
`snapshots`, raw websocket JSON `events`, and expected received `steps` with
`kind`, canonical protobuf `wire`, resulting `bookWire`, and `quoteEligible`.
Wire fields are integer byte arrays, not base64. Identities are BTC-USD/BTCUSDT/
BINANCE; book depth is unbounded. Eligibility is hand-specified and checked
against a test-only copy of the existing ingestion predicate.

Go assigns local time to REST anchors. The oracle replaces that nondeterministic
field with `snapshotUnixMs` before folding; this is normalization, not evidence
of venue observation time. A source that privately folds the REST anchor and
first accepted websocket delta must compare the resulting post-straddler book,
whose timestamp is websocket `E`, rather than publish the normalized anchor.
The kernel can independently replay every canonical step.

Cases cover numeric sorting, equivalent price forms, last-positive snapshot
duplicates, last-write delta duplicates, zero/negative deletion, short and
invalid rows, half-unit rounding, coefficient rescaling and large exponents,
stale events, anchor straddling, gap re-anchoring, and unquotable books.
This proves compatibility with Go behavior on these fixtures, not independent
financial correctness, production latency, or live-feed recovery acceptance.

## Rust process integration

`TestRustDepthProcessThroughJetStream` opts in when `KANZ_BINANCE_DEPTH_BIN`
names an absolute built Rust executable. Without that variable it explicitly
skips, including ordinary Go CI that sets `TEST_NATS_URL` but does not build
Rust. Once opted in, missing `TEST_NATS_URL`, a missing binary or process failure
fails the test. The migration workflow must set both variables and require this
named test to report PASS, not SKIP, in its evidence check.

Run `go test -p 1 ./tools/depth-conformance -run '^TestRustDepthProcessThroughJetStream$' -count=1 -v`.
The test uses loopback exchange fixtures and the configured test JetStream
broker; it checks real Rust frames through Go validation and mark consumption.
It does not prove production SPIFFE authentication, ACLs or live venue behavior.
