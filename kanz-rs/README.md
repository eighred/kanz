# Rust decimal compatibility foundation

This workspace contains canonical decimal primitives and the source-first [Binance depth runtime](services/binance-depth/README.md). The decimal library has no transport, persistence, admission authority or venue credentials. Existing Go financial implementations remain authoritative. Migration scope and acceptance are tracked in [issue #1413](https://github.com/eighred/kanz/issues/1413).

The build generates `common.v1.Decimal` directly from `../kanz-schemas/proto/common/v1/decimal.proto` into Cargo OUT_DIR. Generated Rust is not committed. Coefficient and signed exponent semantics remain unchanged. Missing values are in-domain and compare as zero, matching Go `internal/dec`; callers remain responsible for requiredness. Domain validation accepts exponent -64..64 even for zero. Comparison and sign predicates remain total outside that domain and do not authorize conversion.

Comparison checks signs and decimal orders before alignment. Equal orders bound the exponent gap to 18; u128 holds the aligned magnitude. No exponent-sized allocation or arbitrary power exists. This slice does not implement addition, multiplication, rounding, conversion, money, or an OMS.

## Verification

Generate the existing Go SDK first using the repository schema workflow. From the repository root on a supported build host:

```sh
(cd kanz && go run ./tools/decimal-conformance) > /tmp/kanz-decimal-conformance.json
cargo +1.90.0 fmt --manifest-path kanz-rs/Cargo.toml -- --check
cargo +1.90.0 test --locked --manifest-path kanz-rs/Cargo.toml --lib
DECIMAL_CONFORMANCE_FIXTURE=/tmp/kanz-decimal-conformance.json cargo +1.90.0 test --locked --manifest-path kanz-rs/Cargo.toml --test go_conformance -- --ignored
cargo +1.90.0 clippy --locked --manifest-path kanz-rs/Cargo.toml --all-targets -- -D warnings
```

The cross-language test is explicitly ignored in ordinary unit runs because it requires a fresh Go oracle export. CI must invoke it with `--ignored`; once invoked, a missing path, missing file, malformed corpus or mismatch fails. The corpus has 141 values (including absent, present zero, equivalent representations, extreme coefficients and full signed exponents), 19,881 ordered comparisons and 423 predicate expectations. Five Rust unit tests cover missing semantics, domain endpoints, equivalent encodings, extremes and antisymmetry. Protobuf wire bytes are produced by Go and decoded by generated Rust types, preserving the canonical field contract.

No latency improvement, runtime rollout, financial integration or operational acceptance is claimed. Rust guards and CI are prerequisites for later adoption; existing Go AST guards do not by themselves cover Rust.
