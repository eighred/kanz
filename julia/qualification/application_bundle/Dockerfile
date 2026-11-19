# Local qualification only. The source is the upstream, keyless-signed glibc base.
FROM gcr.io/distroless/base-nossl-debian12:nonroot@sha256:be40c00dfabd86576d92666e87e406714d5618342de1a0c213ad232de255172e
ADD bundle.tar /opt/kanz/
WORKDIR /opt/kanz
ENV HOME=/tmp JULIA_PKG_OFFLINE=true JULIA_NUM_THREADS=1 OPENBLAS_NUM_THREADS=1
USER 65532:65532
ENTRYPOINT ["/opt/kanz/bin/curve-worker"]
