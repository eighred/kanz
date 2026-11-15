# Prometheus runtime qualification (#1140)

The Dockerfile copies the pinned upstream Prometheus binaries and notices into
the approved distroless-static image. The upstream image must first exist in
the private base-image mirror at the exact source digest (#1576).

`prometheus-runtime.yml` builds and qualifies the image on Linux. Its receipt
binds the source/tree, recipe, binary hashes and resolved image identities. The
upstream shell inventory is the affected control; the candidate must contain
none of those utilities. Both images run non-root with read-only roots, no
capabilities, bounded resources and only an isolated loopback network. A dummy
TLS pod API exercises discovery, and the same owned, size-bounded memory volume
survives a process restart to check retained queries. This does not establish
production PVC continuity or node-loss recovery.

The qualification workflow has read-only package access and publishes only a
non-secret test receipt. Image publication and signing use the canonical
build/release workflows; release qualification checks the actual immutable
image before signing. Final ECR scan evidence and a controlled Tokyo deployment
preserving the existing PVC require the operational acceptance in #1140.
