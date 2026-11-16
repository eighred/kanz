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

The workflow has read-only package access and publishes only a non-secret test
receipt. Build/release matrix integration, final signed ECR image/scan evidence
and an authorized Tokyo deployment preserving the existing PVC remain on
#1140. Adding the image to the fixed build inventory requires the corresponding
architecture guard update and its full applicable qualification; #1564 currently
blocks that Julia advisory gate. Preserve the gate.
