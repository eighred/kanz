# Source-only OCI packaging; no production supervisor or release-matrix change.
FROM ghcr.io/eighred/base/rust:1.90.0-bookworm@sha256:3914072ca0c3b8aad871db9169a651ccfce30cf58303e5d6f2db16d1d8a7e58f AS build
RUN curl --fail --location --retry 3 --output /tmp/julia.tar.gz https://julialang-s3.julialang.org/bin/linux/x64/1.13/julia-1.13.3-linux-x86_64.tar.gz \
 && echo '5c61cb4b40332cb5a3504e7779e2f5dac093caa4a1116ded19f01413f85c63e4  /tmp/julia.tar.gz' | sha256sum --check --strict \
 && mkdir /opt/julia && tar -xzf /tmp/julia.tar.gz --strip-components=1 -C /opt/julia
ENV JULIA_DEPOT_PATH=/opt/julia-depot JULIA_LOAD_PATH=@:@stdlib JULIA_CPU_TARGET=generic JULIA_NUM_THREADS=1 OPENBLAS_NUM_THREADS=1
WORKDIR /opt/kanz-jl
COPY kanz-jl/Project.toml kanz-jl/Manifest.toml ./
RUN sha256sum Project.toml Manifest.toml > /tmp/locked.sha256 \
 && /opt/julia/bin/julia --startup-file=no --history-file=no --project=. -e 'using Pkg; Pkg.instantiate(); Pkg.precompile()' \
 && sha256sum --check --strict /tmp/locked.sha256
COPY kanz-schemas/proto/ /opt/kanz-schemas/proto/
COPY kanz-jl/generate.jl ./
RUN /opt/julia/bin/julia --startup-file=no --history-file=no --project=. generate.jl
COPY kanz-jl/src/ ./src/
COPY kanz-jl/bin/ ./bin/
COPY kanz-jl/test/ ./test/
COPY kanz-jl/qualification/isolation.jl ./qualification/isolation.jl
ARG VERSION
RUN printf '%s' "$VERSION" | grep -Eq '^[0-9a-f]{40}$' && printf '%s' "$VERSION" > source.sha

FROM ghcr.io/eighred/base/distroless-cc-debian12:nonroot@sha256:9dac0a79194e45a7da0158a9c6da57b217585af0786db3845d1f0ec1a0dd182f
ARG VERSION
LABEL org.opencontainers.image.revision=$VERSION
COPY --from=build /opt/julia /opt/julia
COPY --from=build /opt/julia-depot /opt/julia-depot
COPY --from=build /opt/kanz-jl /opt/kanz-jl
ENV JULIA_DEPOT_PATH=/opt/julia-depot JULIA_LOAD_PATH=@:@stdlib JULIA_PKG_OFFLINE=true JULIA_CPU_TARGET=generic JULIA_NUM_THREADS=1 OPENBLAS_NUM_THREADS=1 HOME=/tmp
WORKDIR /opt/kanz-jl
USER 65532:65532
ENTRYPOINT ["/opt/julia/bin/julia", "--startup-file=no", "--history-file=no", "--compiled-modules=existing", "--pkgimages=existing", "--project=/opt/kanz-jl", "/opt/kanz-jl/bin/curve_worker.jl"]
