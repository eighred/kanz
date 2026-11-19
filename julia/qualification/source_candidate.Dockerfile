# Build an isolated development-source candidate from public, checksum-pinned offline inputs.
FROM ghcr.io/eighred/base/rust:1.90.0-bookworm@sha256:3914072ca0c3b8aad871db9169a651ccfce30cf58303e5d6f2db16d1d8a7e58f AS build
RUN install -d -o 65532 -g 65532 /opt/julia /opt/kanz-jl && install -d -m 0700 -o 65532 -g 65532 /opt/julia-depot
USER 65532:65532
ENV JULIA_DEPOT_PATH=/opt/julia-depot JULIA_LOAD_PATH=@:@stdlib JULIA_PKG_OFFLINE=true JULIA_PKG_SERVER="" JULIA_CPU_TARGET=generic JULIA_NUM_THREADS=1 OPENBLAS_NUM_THREADS=1 HOME=/tmp
WORKDIR /opt/kanz-jl
COPY --chown=65532:65532 julia/Project.toml julia/Manifest.toml julia/generate.jl ./
COPY julia/qualification/offline_depot.py julia/qualification/source_candidate_install.sh /tmp/
COPY julia/qualification/source_candidate.toml julia/qualification/isolation.jl ./qualification/
COPY schemas/proto/ /opt/schemas/proto/
COPY julia/src/ ./src/
COPY julia/bin/ ./bin/
COPY julia/test/ ./test/
RUN --mount=from=candidate_inputs,target=/inputs,ro bash /tmp/source_candidate_install.sh
ARG VERSION
RUN printf '%s' "$VERSION" | grep -Eq '^[0-9a-f]{40}$' && printf '%s' "$VERSION" > source.sha

FROM ghcr.io/eighred/base/distroless-cc-debian12:nonroot@sha256:9dac0a79194e45a7da0158a9c6da57b217585af0786db3845d1f0ec1a0dd182f
ARG VERSION
LABEL org.opencontainers.image.revision=$VERSION io.eighred.qualification=disposable-native-candidate
COPY --from=build /opt/julia /opt/julia
COPY --from=build /opt/julia-depot /opt/julia-depot
COPY --from=build /opt/kanz-jl /opt/kanz-jl
ENV JULIA_DEPOT_PATH=/opt/julia-depot JULIA_LOAD_PATH=@:@stdlib JULIA_PKG_OFFLINE=true JULIA_CPU_TARGET=generic JULIA_NUM_THREADS=1 OPENBLAS_NUM_THREADS=1 HOME=/tmp
WORKDIR /opt/kanz-jl
USER 65532:65532
ENTRYPOINT ["/opt/julia/bin/julia", "--startup-file=no", "--history-file=no", "--compiled-modules=existing", "--pkgimages=existing", "--project=/opt/kanz-jl", "/opt/kanz-jl/bin/curve_worker.jl"]
