FROM ghcr.io/eighred/base/rust:1.90.0-bookworm@sha256:3914072ca0c3b8aad871db9169a651ccfce30cf58303e5d6f2db16d1d8a7e58f AS build
WORKDIR /src
COPY kanz-rs/ kanz-rs/
COPY kanz-schemas/proto/ kanz-schemas/proto/
RUN cargo test --locked --release --manifest-path kanz-rs/Cargo.toml -p kanz-market-feed --lib --no-run && find kanz-rs/target/release/deps -maxdepth 1 -type f -executable -name 'kanz_market_feed-*' -exec cp '{}' /probe ';'
FROM ghcr.io/eighred/base/distroless-cc-debian12:nonroot@sha256:9dac0a79194e45a7da0158a9c6da57b217585af0786db3845d1f0ec1a0dd182f
COPY --from=build /probe /probe
USER 65532:65532
ENTRYPOINT ["/probe", "--exact", "runtime::identity_proof::real_spire_nats_identity", "--ignored", "--nocapture"]
