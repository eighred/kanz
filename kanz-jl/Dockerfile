# Source-only OCI packaging; no production supervisor or release-matrix change.
FROM ghcr.io/eighred/base/rust:1.90.0-bookworm@sha256:3914072ca0c3b8aad871db9169a651ccfce30cf58303e5d6f2db16d1d8a7e58f AS build
RUN curl --fail --location --retry 3 --output /tmp/julia.tar.gz https://julialang-s3.julialang.org/bin/linux/x64/1.13/julia-1.13.3-linux-x86_64.tar.gz \
 && echo '5c61cb4b40332cb5a3504e7779e2f5dac093caa4a1116ded19f01413f85c63e4  /tmp/julia.tar.gz' | sha256sum --check --strict \
 && mkdir /opt/julia && tar -xzf /tmp/julia.tar.gz --strip-components=1 -C /opt/julia
# Retain Julia's original JLL metadata; qualify this explicit native repair
# against both independent archives and the executed OpenSSL version API.
RUN curl --fail --location --proto '=https' --proto-redir '=https' --retry 3 --output /tmp/openssl.tar.gz https://github.com/JuliaBinaryWrappers/OpenSSL_jll.jl/releases/download/OpenSSL-v3.5.9%2B0/OpenSSL.v3.5.9.x86_64-linux-gnu.tar.gz \
 && echo '8e2c75378726017fc66fb7fbbd10fb53522417c3e45a88af77b8853d46f2b063  /tmp/openssl.tar.gz' | sha256sum --check --strict \
 && test ! -L /opt/julia/lib && test ! -L /opt/julia/lib/julia \
 && test ! -L /opt/julia/lib/julia/libcrypto.so.3 && test ! -L /opt/julia/lib/julia/libssl.so.3 \
 && echo '443c0345cb2a10e55d41c385ef6dbf8af9209b82a2aaaeccf9772c04e398cfae  /opt/julia/lib/julia/libcrypto.so.3' | sha256sum --check --strict \
 && echo '81fd035bfa9f36291cab3c3cfe172dc082a3ba439a3f25ebd125003d66c15602  /opt/julia/lib/julia/libssl.so.3' | sha256sum --check --strict \
 && tar -xOf /tmp/openssl.tar.gz lib/libcrypto.so.3 > /opt/julia/lib/julia/libcrypto.so.3 \
 && tar -xOf /tmp/openssl.tar.gz lib/libssl.so.3 > /opt/julia/lib/julia/libssl.so.3 \
 && echo '09ac6b8640f13b13badf5d2430a8cd326d27ad77668868ad6c9a36de87c4857d  /opt/julia/lib/julia/libcrypto.so.3' | sha256sum --check --strict \
 && echo '044e0615a9f42e6e02298eec073325f528bee55c56901134788220fc5e96a098  /opt/julia/lib/julia/libssl.so.3' | sha256sum --check --strict \
 && mkdir -p /opt/julia/share/licenses/kanz-openssl-runtime \
 && tar -xOf /tmp/openssl.tar.gz share/licenses/OpenSSL/LICENSE.txt > /opt/julia/share/licenses/kanz-openssl-runtime/LICENSE.txt \
 && echo '7d5450cb2d142651b8afa315b5f238efc805dad827d91ba367d8516bc9d49e7a  /opt/julia/share/licenses/kanz-openssl-runtime/LICENSE.txt' | sha256sum --check --strict
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
# Upstream documentation/benchmark/test environments are not worker dependencies.
# Remove their complete directories, not individual manifests or scanner findings.
RUN for path in /opt/julia/share/julia/base/JuliaSyntax/docs \
                /opt/julia/share/julia/stdlib/v1.13/TOML/docs \
                /opt/julia/share/julia/stdlib/v1.13/TOML/benchmark \
                /opt/julia/share/julia/stdlib/v1.13/Tar/test \
                /opt/julia/share/julia/test; do \
      test -d "$path" && test ! -L "$path" && rm -rf -- "$path" && test ! -e "$path" || exit 1; \
    done
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
