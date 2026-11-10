# Keep the source equal to the Prometheus release selected in prometheus.yaml.
FROM quay.io/prometheus/prometheus:v3.13.3@sha256:6976aa8a60fec930796ce5772b8d12da7a318a5daa8d40d69c5c7819a05eeed7 AS upstream
FROM ghcr.io/eighred/base/distroless-static:nonroot@sha256:25ac8f0c3b2334a9eb05452266bd4697864a8c8ef0ef71e5e6480d7f92868329

COPY --from=upstream /bin/prometheus /bin/promtool /bin/
COPY --from=upstream /etc/prometheus/prometheus.yml /etc/prometheus/prometheus.yml
COPY --from=upstream /LICENSE /NOTICE /
COPY --from=upstream --chown=65534:65534 /prometheus /prometheus

# Match the deployed and upstream numeric identity; the base retains its CA store.
USER 65534:65534
WORKDIR /prometheus
EXPOSE 9090
VOLUME ["/prometheus"]
ENTRYPOINT ["/bin/prometheus"]
CMD ["--config.file=/etc/prometheus/prometheus.yml", "--storage.tsdb.path=/prometheus"]
