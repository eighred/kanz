# Rust migration components

This workspace contains canonical decimal primitives and source-first [Binance](services/binance-depth/README.md) and [OKX](services/okx-depth/README.md) depth runtimes. Both use the shared `kanz-market-feed` book, framing, identity and lifecycle implementation. The [bond-risk](services/bond-risk-shadow/README.md) and [OMS fill-fold](services/oms-fold-shadow/README.md) workers compare bounded calculations with Go through default-off diagnostic supervisors. The decimal library has no transport, persistence, admission authority or venue credentials. Existing Go financial implementations remain authoritative. Migration scope and acceptance are tracked in [issue #1413](https://github.com/eighred/kanz/issues/1413).

The build generates `common.v1.Decimal` and `common.v1.Money` directly from the canonical schemas in `../kanz-schemas/proto/common/v1/` into Cargo OUT_DIR. Generated Rust is not committed. Coefficient and signed exponent semantics remain unchanged. Missing values are in-domain and compare as zero in the decimal primitive API, matching Go `internal/dec`; callers remain responsible for requiredness. Domain validation accepts exponent -64..64 even for zero. Comparison and sign predicates remain total outside that domain and do not authorize conversion.

Comparison checks signs and decimal orders before alignment. Equal orders bound the exponent gap to 18; u128 holds the aligned magnitude. No exponent-sized allocation or arbitrary power exists in that comparison. The shared decimal primitive API provides predicates and comparison. Component workers implement bounded diagnostic calculations; this workspace does not replace the authoritative OMS.

## Decimal-library verification

Generate the existing Go SDK first using the repository schema workflow. From the repository root on a supported build host:

```sh
(cd kanz && go run ./tools/decimal-conformance) > /tmp/kanz-decimal-conformance.json
cargo +1.90.0 fmt --manifest-path kanz-rs/Cargo.toml -- --check
cargo +1.90.0 test --locked --manifest-path kanz-rs/Cargo.toml --lib
DECIMAL_CONFORMANCE_FIXTURE=/tmp/kanz-decimal-conformance.json cargo +1.90.0 test --locked --manifest-path kanz-rs/Cargo.toml --test go_conformance -- --ignored
cargo +1.90.0 clippy --locked --manifest-path kanz-rs/Cargo.toml --all-targets -- -D warnings
```

The cross-language test is explicitly ignored in ordinary unit runs because it requires a fresh Go oracle export. CI must invoke it with `--ignored`; once invoked, a missing path, missing file, malformed corpus or mismatch fails. The corpus has 141 values (including absent, present zero, equivalent representations, extreme coefficients and full signed exponents), 19,881 ordered comparisons and 423 predicate expectations. Five Rust unit tests cover missing semantics, domain endpoints, equivalent encodings, extremes and antisymmetry. Protobuf wire bytes are produced by Go and decoded by generated Rust types, preserving the canonical field contract.

The component READMEs describe their additional runtime workflows and integration checks. Diagnostic integration does not establish financial authority or production acceptance. No latency improvement or runtime rollout is claimed. Rust guards and CI are prerequisites for later adoption; existing Go AST guards do not by themselves cover Rust.
