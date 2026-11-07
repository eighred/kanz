# Source-free Rust audit tools

This producer packages cargo-audit 0.22.2 and rust-audit-info 0.5.4, built with
Rust 1.90.0 and their published locked dependencies. The final image contains
only the binaries, a source/platform/version manifest and bundled public crate
licensing notices and metadata. No Kanz module, schema, generated SDK, application
source, application inventory or build cache is an input or published layer.

The host builds using a Dockerfile on stdin with no filesystem context. Explicit
BuildKit metadata binds the configuration digest to the IID and saved
configuration bytes. Its separate manifest descriptor must bind the root digest
with a supported image media type and bounded positive byte size. A fixed source-SHA local tag
is the load/save work reference; it is not a published trust identity. This
producer alone disables automatic BuildKit provenance/SBOM attachments to retain
its closed single-image graph; existing release/SBOM checks are unchanged. The
receipt retains both build identities. Registry format conversion may change the
root digest: the published root is independently pinned and signed after the
same configuration, single layer and payload have been read back. PRs
qualify the actual binaries, metadata extraction limits, a fresh clean RustSec
audit and a known-vulnerable refusal. Only canonical main can publish the exact
qualified image artifact. The saved archive must contain exactly one final COPY layer: configuration and
uncompressed layer hashes are checked before scanning the closed payload. Container
runtime injections are never inventoried. Registry readback verifies its immutable digest,
platform, source label and complete file inventory. Main then keylessly signs that
exact digest and verifies its source annotation against the fixed workflow identity
and GitHub OIDC issuer. Only the publisher has OIDC write permission. Package visibility and
credentials follow the existing private base-image boundary.

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
