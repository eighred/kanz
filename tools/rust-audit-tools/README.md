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
The existing source installer remains authoritative. A future consumer change
must pin an actually published and verified digest and preserve fresh audits;
producer qualification alone does not authorize a consumer switch.

Top-level tools use MIT OR Apache-2.0. Bundled dependency license files and public
package metadata are retained under `/notices`; this metadata describes public
tooling, not the application. No audit verdict or advisory database is cached.
