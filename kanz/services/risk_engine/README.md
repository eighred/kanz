# Risk engine

The Go control-plane service maintains risk state, evaluates admission controls
and exposes the existing risk query APIs. The composition root is
`cmd/risk_engine/`; `internal/config/` loads environment configuration and the
module's `internal/risk/` packages own shared domain/calculation code.
Financial computation and admission remain in their explicit owners.

From `kanz/`, use `go build ./services/risk_engine/...` and
`go test -p 1 ./services/risk_engine/... -count=1`. PostgreSQL, broker and Julia
cases require their named disposable dependencies; a skipped case proves none
of those boundaries. Deployment manifests are under `kanz/infra/deploy/`.

## Portfolio spot book capture

The existing portfolio scenario query accepts `OPERATOR_SPOT_BOOKS` and a
`spot_book_instrument`. Each risk replica subscribes to `market.book.snapshot`
when its gRPC reader has configured instruments. It retains one current book
per venue and instrument. `known_at` is the first local risk-service receipt of
that book revision. Exports include the venue event times, sequences, FACT IDs,
source, producer release and data rights reference. For each returned venue,
`portfolio_account_ref` comes from the exact tenant, resolved portfolio and
MIC in `RISK_ENGINE_VENUE_ACCOUNTS`. An empty reference indicates no configured
binding. Private venue balances and fee tiers require separate observations.

`RISK_ENGINE_SPOT_BOOK_GRANTS` is a JSON array. Each entry binds a canonical
crypto spot pair to one exact publisher and release in `RISK_ENGINE_TENANT`.
Provision it for a feed whose broker publishing access, venue clocks, underlying
asset identities and tenant-wide display and export rights have been verified.
The optional RFC 3339 `valid_until` expires the grant. Example for a local demo:

```json
[{"tenant":"pilot","instrument":"BTC-USDT","product_type":"CRYPTO_SPOT","source":"market-ingest-demo","producer_version":"example-release","rights_reference":"local-demo-only","base_asset_ref":"demo/bitcoin","quote_asset_ref":"demo/tether-usd"}]
```

The Binance source needs the exchange-confirmed initial snapshot. The selected
feed must stamp the configured tenant on its FACTs. A missing grant, missing leg,
quality flag or expired observation withholds the pair. Balances, fee currency,
order limits and rebalance costs belong to the separate pricing scenario.

## Optional Julia calibration comparison

Go remains authoritative. The optional Julia worker receives one detached latest-cache
capture only after the Go curve has been installed. This is not historical as-of
reconstruction, model approval, a new pricing source, or permission to trade. The
worker cannot install curves or publish calibration facts. Missing inputs remain
in the captured coverage; the worker uses the exact resolved rates used by Go.

Default is off. Set `RISK_ENGINE_JULIA_SHADOW_ENABLED=true` and all of:

* `RISK_ENGINE_JULIA_SHADOW_EXECUTABLE`: absolute pinned Julia executable.
* `RISK_ENGINE_JULIA_SHADOW_PROJECT`: absolute prepared `julia` directory, with
  reviewed Project.toml, Manifest.toml and generated protobuf code.
* `RISK_ENGINE_JULIA_SHADOW_SCRIPT`: absolute project `bin/curve_worker.jl`.
* `RISK_ENGINE_JULIA_SHADOW_DEPOT`: absolute prebuilt Julia package depot.
* `RISK_ENGINE_JULIA_SHADOW_TIMEOUT`: optional duration, default 30s, range 1s–1m.

Scheduled curve calibration and NATS must also be configured. Partial configuration
fails startup. Runtime performs no package installation. Fixed Julia flags
`--compiled-modules=existing --pkgimages=existing` forbid creation of new module
and native-image caches. `JULIA_LOAD_PATH` contains only the active project and
standard library, excluding global environment fallback. These files and the depot
are trusted executable dependencies: offline package mode and an allowlisted child
environment do **not** provide an OS sandbox or block network access. No application
credentials or inherited application environment are passed to the worker.

One worker runs at a time, with one waiting capture. Overload drops only shadow
work. Raw protobuf stdin/stdout is bounded to 1 MiB each; stderr to 32 KiB. A timeout
or output overflow kills the direct worker process; the reviewed worker must not
spawn descendant processes. Neither worker text nor captured market data is logged.
Each request carries a random capture ID and SHA-256 of its deterministic protobuf
encoding before the digest field is populated. The response must bind both values.

Pillars must match shape and tenor, contain finite values and imply positive finite
discount factors. Comparisons precede canonical Decimal quantization: absolute zero
rate tolerance is 1e-10, discount factor tolerance 1e-12 and tenor tolerance 1e-12.
Linear-zero interpolation is the supported shadow convention. Negative rates are
allowed. A disagreement, invalid response, timeout or worker rejection is diagnostic
only. `kanz_risk_julia_shadow_results_total{status=...}` has a closed outcome vocabulary;
`kanz_risk_julia_shadow_configured` reports configuration, not worker readiness.

From `kanz`, run `go test -p 1 ./services/risk_engine/internal/calibrationshadow
./services/risk_engine/internal/config ./services/risk_engine/cmd/risk_engine -count=1`
(join the command onto one line). Subprocess tests exercise real process pipes,
timeouts, output bounds and environment scrubbing. `TestJuliaIntegration` explicitly
skips unless `TEST_JULIA_EXECUTABLE` is set. To require real Julia, also set
`TEST_JULIA_PROJECT` and `TEST_JULIA_DEPOT` to the prepared paths and run
`go test -p 1 ./services/risk_engine/internal/calibrationshadow -run '^TestJuliaIntegration$' -v -count=1`.
CI must require this test to pass without a skip. A comparison pass is shadow
conformance evidence, not production calibration or financial-model acceptance.
