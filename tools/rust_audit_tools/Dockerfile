# Public third-party tools only. Build with Dockerfile-only stdin, never repo context.
FROM ghcr.io/eighred/base/rust:1.90.0-bookworm@sha256:3914072ca0c3b8aad871db9169a651ccfce30cf58303e5d6f2db16d1d8a7e58f AS tools
ARG SOURCE_SHA
RUN test "${#SOURCE_SHA}" = 40 && ! printf '%s' "$SOURCE_SHA" | grep -q '[^0-9a-f]'
RUN cargo install --locked --version 0.22.2 --root /out cargo-audit
RUN cargo install --locked --version 0.5.4 --root /out rust-audit-info
# Preserve bundled public dependency notices, not registry sources/build caches.
RUN set -eu; mkdir -p /payload/tools /payload/notices; \
    cp /out/bin/cargo-audit /out/bin/rust-audit-info /payload/tools/; \
    for package in /usr/local/cargo/registry/src/*/*; do \
      name="$(basename "$package")"; mkdir -p "/payload/notices/$name"; \
      cp "$package/Cargo.toml" "/payload/notices/$name/package.toml"; \
      (cd "$package"; find . -type f \( -iname '*license*' -o -iname '*copying*' -o -iname '*notice*' -o -iname '*copyright*' \) -exec cp --parents '{}' "/payload/notices/$name/" ';'); \
    done; \
    printf '{"version":1,"source_sha":"%s","platform":"linux/amd64","rust":"1.90.0","cargo-audit":"0.22.2","rust-audit-info":"0.5.4"}\n' "$SOURCE_SHA" > /payload/manifest.json
# rust-audit-info's published crate omits its workspace license files.
# Its .cargo_vcs_info.json binds this exact public upstream revision.
RUN set -eu; base=https://raw.githubusercontent.com/rust-secure-code/cargo-auditable/17fc93a92a952cc5e715bc51afc25e11049d7036; \
    curl --fail --silent --show-error --location "$base/LICENSE-MIT" -o /payload/notices/rust-audit-info-0.5.4/LICENSE-MIT; \
    curl --fail --silent --show-error --location "$base/LICENSE-APACHE" -o /payload/notices/rust-audit-info-0.5.4/LICENSE-APACHE; \
    cd /payload/notices/rust-audit-info-0.5.4; \
    printf '%s\n' 'e3a8938c0304a5d418b6f7453c2f60fd6b590d486c798879063373e3acc94bf1  LICENSE-MIT' 'a60eea817514531668d7e00765731449fe14d059d3249e0bc93b36de45f759f2  LICENSE-APACHE' | sha256sum --check
RUN set -eu; rustc --version | grep '^rustc 1.90.0 '; \
    docs="$(rustc --print sysroot)/share/doc/rust"; \
    mkdir -p /payload/notices/rust-1.90.0; \
    cp "$docs"/COPYRIGHT*.html /payload/notices/rust-1.90.0/; \
    cp -R "$docs/licenses" /payload/notices/rust-1.90.0/
FROM scratch
ARG SOURCE_SHA
LABEL org.opencontainers.image.revision=$SOURCE_SHA
COPY --from=tools /payload/ /
CMD ["/tools/cargo-audit", "--version"]
