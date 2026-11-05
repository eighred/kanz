# Disposable tv-sync listener isolation

This harness runs the released tv-sync image in a disposable Linux Docker/K3s
cluster. It does not connect to an existing kubeconfig or deploy to Tokyo.
Merge the harness workflow first, then dispatch it from main against the exact
unmerged promotion commit. Do not merge active listener manifests just to make
this workflow available.

The workflow accepts a full candidate SHA and immutable tv-sync and kanz-migrate release images,
and their shared version tag and source SHA. Trusted code stays in the main checkout; the
candidate is never checked out. The harness reads only regular YAML/YML/JSON
blobs under `kanz/infra` from the trusted checkout's Git object database into a
temporary directory. Symlinks, nonregular objects, escaping paths and oversized
blobs fail closed; evidence records each source blob and SHA-256 hash. Cosign must verify the
signature, SPDX and vulnerability attestations for both images against the exact release tag
identity and workflow SHA. The release SHA must be an ancestor of current main
and the candidate. The candidate Deployment must name those same app and init-container image digests.

Each registry image is pulled by digest, exported and imported into the isolated
node without exposing registry credentials to fixture pods. Its Docker config
ID, imported CRI config ID and running pod image identity must agree. All
Postgres roles, tenant headers and data are disposable fixtures. The runtime
role is explicitly NOSUPERUSER/NOBYPASSRLS. The verified migration image runs
the real tv-sync migration directory; its completed init-container image identity
is checked against the imported image, and its exit code must be zero.

The fixture preserves candidate listener ports, Service selection, pod labels
and HTTP probes. It replaces production CSI, SPIFFE and database/broker
bindings. It therefore does not certify production secret delivery, workload
identity, or tenant isolation over the production NATS topology.

Every NetworkPolicy document in YAML/YML/JSON, including nested resource Lists,
under candidate `kanz/infra` is inventoried with
its path and spec hash. Same-name overlay policies receive distinct fixture
names, making the installed set a conservative additive union. This can combine
mutually exclusive overlays: a pass is evidence about this union for the
fixture's labels, not a claim that any particular live estate was rendered or
verified. Unsupported relevant YAML and unresolved namespaces fail closed.
Only policy objects and narrowly reconstructed tv-sync resources are applied;
other candidate workloads and CRDs are never applied.

The probe first proves all ordinary client pods can reach the actual API on
both Pod IP and Service IP. After policy application, the gateway pod must still
receive the tenant-scoped accounts response while observability and unrelated
pods cannot connect to API8113. Monitoring must receive metrics/health/readiness
on 8091 and a 404 for Broker routes there. An absent principal must receive 401 on
the authorized API path. These assertions do not place or cancel orders.

Local trust-input tests need no Docker or network. The Go probe uses the canonical
`pkg/auth` principal-header helper and requires the generated schema SDK; follow
`kanz-schemas/README.md` before compiling it:

```sh
python3 -B -m unittest discover -s kanz/services/tv-sync/test -p 'test_*.py'
go -C kanz test -p 1 ./services/tv-sync/test/probe
```

On a disposable Linux runner with Docker, Go, Python/PyYAML 6.0.3 and cosign 3.0.6,
a trusted main checkout with full Git history (including the candidate commit)
and registry read authentication. Generate the SDK from that trusted checkout
before running the harness. The workflow uses pinned buf 1.69.0 and protoc plugins
with the existing SDK bootstrap; the probe uses the approved distroless static base:

```sh
python3 -B kanz/services/tv-sync/test/isolation.py \
  --candidate-sha REVIEWED_CANDIDATE_SHA \
  --image ghcr.io/eighred/tv-sync@sha256:REVIEWED_DIGEST \
  --migration-image ghcr.io/eighred/kanz-migrate@sha256:REVIEWED_MIGRATION_DIGEST \
  --release-tag REVIEWED_VERSION_TAG --release-sha REVIEWED_SOURCE_SHA \
  --evidence /absolute/scratch/tv-sync-isolation.json
```

The K3s image freezes the existing Terraform version v1.36.4+k3s1. Postgres 16,
NATS 2 and nats-box 0.14.5 freeze existing CI dependency selections at DockerHub
manifest digests resolved on 2026-11-05; `IMAGES` holds the exact identities.
The workflow uses existing pinned checkout/setup/upload/cosign actions and
read-only contents/packages permissions. It neither signs nor publishes images.

A successful offline test is not runtime evidence. Retain the workflow result
and its sanitized JSON for the exact candidate before promotion. Failed image
custody, role checks, bootstrap, connections or cleanup remain failures.
