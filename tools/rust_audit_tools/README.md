# Source-free Rust audit tools

This producer packages cargo-audit 0.22.2 and rust-audit-info 0.5.4, built with
Rust 1.90.0 and their published locked dependencies. The final image contains
only the binaries, a source/platform/version manifest and bundled public crate
licensing notices and metadata. No Kanz module, schema, generated SDK, application
source, application inventory or build cache is an input or published layer.

The host builds using a Dockerfile on stdin with no filesystem context and the
existing mirrored BuildKit container driver. Explicit uncompressed OCI export
produces one index entry, one image manifest, one configuration and one final
COPY layer. Every descriptor hash and size is checked; extra blobs are refused.
Build metadata must bind to the actual manifest bytes. Configuration identity is
derived from that verified graph, independently of Docker's exporter-specific IID
semantics. The source/platform label, DiffID and closed payload inventory are
checked before loading anything into Docker.

Read-only qualification then loads those exact OCI bytes under the fixed
source-SHA local tag and saves them again. The saved configuration, layer and
complete payload must pass the same source/platform/inventory checks before ABI,
metadata extraction bounds and fresh clean/vulnerable RustSec audit controls run.
Only canonical main can publish the exact qualified OCI artifact. Main repeats
validation and the local roundtrip, then checks registry descriptors and the
pulled saved payload. It signs the actual published digest and verifies the source
annotation, fixed workflow identity and GitHub OIDC issuer. Registry format
conversion may change the root digest; the configuration and payload must remain
identical. Only the publisher has OIDC write permission.

This producer alone disables automatic BuildKit provenance/SBOM attachments to
retain its closed single-image graph; existing release/SBOM checks are unchanged.
Package visibility and credentials follow the existing private base-image boundary.

The artifact is linux/amd64 only. Copy its verified binaries onto the qualified
runner; do not use `docker run` on this scratch payload. Registry descriptor checks
precede Docker pull and saved-layer validation; this is not an independent blob
download implementation. Tool extraction must be qualified on the actual
runner ABI; the scratch payload is not itself a runnable application image.
The consumer below pins an actually published and verified digest and retains
fresh audits. Producer qualification is separate from actual consumer ABI and
read-only registry qualification.

Top-level tools use MIT OR Apache-2.0. Bundled dependency license files and public
package metadata are retained under `/notices`; this metadata describes public
tooling, not the application. No audit verdict or advisory database is cached.

## Signed immutable consumer

The shared release/architecture installer consumes the reviewed main producer at
`ghcr.io/eighred/base/rust-audit-tools@sha256:0579c6075afce18b98b97864a056d0e553d0281e6d12c598ab8e3042ae88de71`.
`consume.py` binds producer source `a2dbaf41fd77da6dc30380f133e80802488fe7c2`,
config `sha256:e523902a82786dd3dfb6aa9c5b155b9516c11fd22f86cbb7db6d9bb36f762532`,
the main base-image-mirror workflow certificate identity, GitHub OIDC issuer and
source annotation. Cosign verification precedes pull, extraction and execution.
The local source tag only names the verified digest for strict SAVE readback;
it is never a registry lookup or mutable fallback.

The consumer retains its own `GITHUB_SHA`; the install receipt records both
source identities and its validated GitHub run/attempt. Linux nofollow/nonblocking bounded reading and the existing
one-layer/config/payload/notice checks precede execution. Actual ABI, extraction
limits and fresh clean/vulnerable advisory controls precede PATH publication.
Per-image fresh lock/binary audits, SPDX and cargo-auditable remain unchanged.
Architecture qualification uses existing read-only package access. There is no
source-build or mutable-tag fallback. Windows parser tests do not qualify the
Linux opener or registry access.
