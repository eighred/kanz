# Accounting replay and valuation candidates

This pure library implements ordinary posting and numeric NAV families of #1535
under #1413. Financial ownership remains in `kanz/services/accounting/internal`; there is
no service, database writer, bus consumer, shadow toggle or capital-path wiring.
Inputs must already belong to an authorized portfolio scope. Empty portfolio IDs
retain Go's legacy caller-scope behavior; filtering is not tenant authorization.

The method is the existing Go ledger and `kanz/internal/costbasis` convention:
signed quantities, exact rational weighted-average cost, gross realized P&L,
caller-supplied cash legs and separate accrued, traded and settled balances.
Fees do not silently change gross position P&L. There is no rounding in this
kernel; canonical Decimal conversion remains at existing service boundaries.
OMS and this candidate share `kanz-order-core::cost_basis`, promoted from the
existing Rust OMS position implementation when this second consumer was added.

`replay` supports trade, cash, fee, accrual and legacy unspecified entry kinds.
It follows stable `(effective, knowledge, entry_id)` order, first-entry
idempotency, inclusive cutoffs and Go-zero/unbounded reads. No wall clock is
consulted. Unknown settlement, including an empty entry, prevents a complete
settled-basis answer. A pending date never makes a posting settled. Accruals
have no settled cash twin. Nil legs, ignored foreign portfolios and zero map
entries follow the Go owner. A zero price is retained where Go uses a nil price;
this compatibility rule grants no admission authority.

Any known relevant corporate action or undeclared entry kind returns
`Unsupported`. Action refusal precedes the effective cutoff because action
revision resolution can supersede an earlier effective event. This deliberately
conservative boundary includes cancelled actions. Future-knowledge and foreign
portfolio actions do not affect the requested book. Corporate-action lifecycle,
NAV attribution/input selection, custody, tax lots, store checkpoints/receipts, execution-evidence bytes,
transaction/outbox integration and cutover/rollback are not implemented here.
Opaque journal provenance remains owned by Go; this arithmetic capture neither
serializes nor replaces immutable records. Full #1535 acceptance remains open.

Inputs are bounded to 65,536 entries, 256 UTF-8 bytes per relevant key and 16,384
bits per rational numerator/denominator, including fold results. Rationals must
be reduced with a positive denominator; noncanonical `BigRational::new_raw` values
are refused before arithmetic. Capacity, invalid-rational or unsupported errors
return no partial book. UTC timestamps use seconds and nanos
over years 1–9999; they do not overflow through a Unix-nanosecond conversion.
This is a finite qualification domain, not an unbounded historical-store API.

From the repository root, generate a fresh test-only capture from the Go owner:

```sh
(cd kanz && go run ./services/accounting/tools/ledger_conformance) > /tmp/kanz-ledger.json
cargo +1.90.0 test --locked --manifest-path kanz-rs/Cargo.toml -p kanz-ledger-replay --all-targets
KANZ_LEDGER_CORPUS=/tmp/kanz-ledger.json cargo +1.90.0 test --locked --manifest-path kanz-rs/Cargo.toml -p kanz-ledger-replay --test go_conformance -- --ignored --nocapture
```

The explicit differential gate fails without its capture. It compares every
position quantity, average cost and realized P&L; each currency's cash/accrual;
both settled maps; and settlement quality/counts at 215 bitemporal reads across
18 deterministic histories (over 17,000 captured entries). Independent exact
goldens cover a closed trade's cash/P&L conservation, offsetting correction,
short-to-long crossing, nanosecond knowledge boundaries, settlement ambiguity,
accrual isolation and capacity refusal. Mutation controls exercise changed cash
and forbidden source/dependency ownership. These checks prove arithmetic and
replay compatibility in this domain, not database, service, production or scale
capacity acceptance. CI retains source SHA, toolchain, corpus digest and results
in the `ledger-replay` artifact.

## Multi-currency numeric valuation

`valuation::value` consumes the traded `Book` and an already selected price,
instrument-currency and FX-table snapshot. It mirrors Go's
`ComputeNAVInCurrency` with `FXTable`: signed security market value plus all cash
and accrued buckets, exact reporting units per local currency unit, and retained
pre-conversion local exposures. There is no rounding or wall-clock lookup.
Portfolio identity, as-of/knowledge selection, quote freshness, security-master
authority, source completeness and output metadata remain with the Go owner.
This function neither values the settled book nor authorizes available cash.

An absent converter refuses even an empty book; an empty configured table has
identity rates only. Missing, nil, zero or negative foreign FX rates refuse the
whole value and name all affected currencies. The Go `FXTable.Rate` applies the
same positive-rate rule already used by the live FX feed, including to request
overrides. Its regression checks include the served HTTP refusal. Positive
rates remain exact, including values smaller than output display precision.

Zero holdings need no price. Nonzero holdings need a price and FX even at a zero
mark. Offset exposures cannot make an unknown FX rate known. Zero cash/accrual
buckets are ignored; a local exposure that sums to zero remains present. The
existing Go conventions for blank cash currency (identity) and missing/blank
instrument currency (reporting) are preserved, not new assertions of source
quality. Multiple missing prices select the lexically first instrument in Rust;
Go map order does not specify which one is named. A nil mark is refused in Rust
rather than entering Go's invalid in-memory input domain. Custom FX converters,
FX P&L attribution and live input collection are outside this family.

Valuation input is bounded to 65,536 aggregate rows across relevant book maps,
prices, currency joins and rates, with the shared 256-byte keys and 16,384-bit
canonical rational budget. Intermediates are checked; an error returns no partial
value and never modifies the input book. These limits are a qualification domain,
not a demonstrated production latency or portfolio-capacity budget.

```sh
(cd kanz && go run ./services/accounting/tools/nav_conformance) > /tmp/kanz-nav.json
KANZ_NAV_CORPUS=/tmp/kanz-nav.json cargo +1.90.0 test --locked --manifest-path kanz-rs/Cargo.toml -p kanz-ledger-replay --test nav_conformance -- --include-ignored --nocapture
```

The fresh oracle covers 2,061 views with 139,269 holding inputs, including one
8,192-position book. Every total, component, local exposure and named refusal is
compared. Independent worked rational examples, unknown FX despite cancellation,
unusable-rate refusals, capacity/input preservation and replay knowledge-cutoff
composition supply separate controls. The `ledger-replay` CI job runs both
posting and valuation gates and retains both captures and their digests.
