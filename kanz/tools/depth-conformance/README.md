# Depth compatibility oracle

`TestFeedRuntimeControlledHandoff/{binance,okx}` is a Linux-only controlled
process rehearsal. It requires absolute `KANZ_MARKET_INGEST_BIN`,
`KANZ_BINANCE_DEPTH_BIN`, `KANZ_OKX_DEPTH_BIN` paths and an exact 40-hex
`KANZ_FEED_HANDOFF_VERSION` shared by the built binaries. It launches the actual
service entrypoints, loopback exchange protocol servers, a disposable JetStream
server, and ephemeral workload API identities. Broker connections authenticate
the distinct Go, Rust and observer SPIFFE principals. Rust development endpoint
selection still uses the real mTLS path when its workload socket is supplied;
the same configuration without that identity must refuse this broker.
The observer can issue JetStream API/ACK requests and receive inbox replies,
but cannot publish market facts; an actual denied publish checks that boundary.
An independently owned deny-publish broker authenticates the Rust runtime and
then refuses its real publication: the test requires a server-side denial,
unready status, failed process exit and zero accepted facts. A duplicate Go
external-depth assignment must refuse startup before any exchange connections
or accepted facts; the valid process path is its positive control.

The controller joins the Go full-depth process before starting its retained
trade-only mode and the Rust depth process. Actual exchange trades close prior
minute buckets through Go's existing bar collector. Canonical broker frames,
source/version/tenant binding, sequence-gap reanchor, stale readiness and broker
reconnection are checked. Rollback joins Rust and retained Go before restarting
the Go full-depth owner; workload streams and authenticated broker connections
must disappear before the next owner starts. A final bounded broker inventory
rejects selected depth emitted by the retained Go process.

This fixture is not a distributed publisher fence, production SPIRE or broker
ACL qualification, live exchange/capacity proof, or authorization to activate
Rust feeds. Local cross-compilation does not execute the process cases. This
fixture does not prove downstream mark consumption or cross-process fencing.
The shared CI workflow and mandatory evidence inventory must separately require
actual Linux passes before this rehearsal is qualified.
The compatibility job builds three separately stamped service binaries and a
race-enabled test binary, records all four SHA-256 values with source/tree and
run/attempt, and requires all seven named parent/child outcomes without skips.
The original development-binary and broker tests remain separate and unchanged.
The 150-second child lifetime is fixed; refusal observation allows up to 35
seconds for the existing runtime's publication failure and joined shutdown,
rather than treating a still-draining process as successful cleanup.
Rust uses the existing required `service/instance` producer-source format.
Observers are explicitly owned durable pull consumers bound to `HANDOFF`, not
ephemerals whose inactivity expiry can erase a reader between process phases.
Cleanup deletes each consumer and requires typed consumer-not-found readback.
Failure-only diagnostics expose closed transport/exit categories and numeric
workload/principal/exchange/stream counts, never raw child logs or credentials.
The existing producers differ in advisory `Symbol`: the Go alpha runner sets
it to the canonical instrument ID, while Rust uses the venue symbol. The test
asserts each exact existing value and does not normalize either payload.
Common authoritative instrument/MIC/sequence and economic values remain exact.
This known metadata gap is tracked in #1413 and prevents claiming full producer
compatibility; production ownership still needs preservation or explicitly
approved consumer compatibility. Repeated retained trade pairs also validate
the exact prior pair's second-bucket bar before the newly requested bucket,
including all OHLC values, volume, count and bucket boundaries.

Use `go run ./tools/depth-conformance --okx` for the OKX corpus, generated through
the public Go OKX source and book using local websocket frames. It covers positive
sequence snapshots/updates, sorting, equivalent prices, deletion and decimal
rounding/rescaling. The replacement runtime's zero-sequence reset is verified
separately because the legacy Go book treats zero delta sequence as omitted.

`TestRustOKXDepthProcessThroughJetStream` opts in with an absolute
`KANZ_OKX_DEPTH_BIN` plus `TEST_NATS_URL`. It validates real Rust publication into
JetStream and Go mark consumption, including initial readiness, zero reset,
continuity failure and replacement snapshot recovery. CI must require this named
test to PASS rather than SKIP. The exchange endpoint is a loopback fixture;
production workload identity, ACLs and ownership handoff remain separate proof.

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
