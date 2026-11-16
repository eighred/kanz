# Canonical Protobuf wire normalization

`kanz-proto-wire` normalizes known Protobuf fields with the existing Go wire
semantics while preserving unknown-value byte ordering and representation.
It supplies canonical schema/normalization helpers to Rust consumers and owns
no financial model, persistence, identity or admission decision.

`normalize` uses the standard bounded input budget; the explicit recovery-history
budget supports the existing larger recovery contract. Invalid input, depth and
capacity limits remain refusals. Normalization is not authorization or proof
that an envelope meets a domain contract. `src/canonical.rs` owns the schema
selection helper; `src/lib.rs` owns wire handling and budgets.

From the repository root with the pinned Rust toolchain:

```sh
cargo +1.90.0 test --locked --manifest-path rust/Cargo.toml -p kanz-proto-wire
```

Cross-language checks require the fresh Go oracle/SDK inputs specified by the
owning consumer. Keep the Cargo package identity, canonical schema identity and
wire contracts stable during source moves. See [the workspace guide](../../README.md).
