FROM rust:1.90.0-bookworm@sha256:3914072ca0c3b8aad871db9169a651ccfce30cf58303e5d6f2db16d1d8a7e58f AS build
WORKDIR /src
COPY kanz-rs/ kanz-rs/
COPY kanz-schemas/proto/ kanz-schemas/proto/
RUN cargo build --locked --release --manifest-path kanz-rs/Cargo.toml -p kanz-binance-depth

FROM gcr.io/distroless/cc-debian12:nonroot@sha256:9dac0a79194e45a7da0158a9c6da57b217585af0786db3845d1f0ec1a0dd182f
COPY --from=build /src/kanz-rs/target/release/kanz-binance-depth /kanz-binance-depth
USER 65532:65532
EXPOSE 8088
ENTRYPOINT ["/kanz-binance-depth"]
