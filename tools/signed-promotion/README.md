# Signed release pair promotion

This main-only manual workflow copies the existing signed `tv-sync` and
`kanz-migrate` release graphs from GHCR into Tokyo ECR. It does not build, sign,
deploy, migrate a database, or change a running workload. The source delivery
belongs to #1410/#1413; #1441 provisions the destination repositories separately.

Both immutable inputs must have the same exact Git release tag and source SHA.
The tag must resolve to that SHA, which must be an ancestor of reviewed main.
Cosign 3.0.6 verifies the GitHub issuer, exact `release.yml@refs/tags/<tag>`
certificate identity and workflow SHA, plus both SPDX and vulnerability
attestations. Claims and transparency verification remain enabled. Verification
success is required independently at GHCR and ECR; a registry copy alone is not
acceptance. No claim is made that an old vulnerability report is a current scan.

`release.yml` publishes the semver image tag without the Git tag's leading `v`.
For example, Git `v2026.4.0` resolves GHCR `2026.4.0`. ECR uses the explicit,
immutable `release-v2026.4.0` tag. Existing unsigned main-build full-SHA tags may
name different bytes and are never overwritten. This release namespace is not
an automatic installer fallback: a separately reviewed, exact digest/source/tag
custody mapping is required before activation. Ordinary main-image provenance
rules remain unchanged.

## Preconditions and execution

Merge the workflow first. Dispatch only from `main`, with two SHA256 digests,
the exact supported semver Git tag, and its full source SHA. Repository variables
`AWS_TESTNET_ECR_ROLE_ARN` must name the approved Tokyo publisher. The destination
account is fixed to `012619468098`; STS must return that account. The existing IAM
subject remains `repo:eighred/kanz:ref:refs/heads/main`.

The separate Terraform statement `VerifySignedTVSyncPairLayers` grants only
`ecr:GetDownloadUrlForLayer` on the two fixed repository ARNs. Fresh destination
blob verification needs this read permission; the previous push-only publisher
does not have it. Review/apply that prerequisite and #1441 repository provisioning
through the normal infrastructure process before dispatch. This PR does not
apply Terraform, create repositories, or grant tag deletion.

All source proofs, complete source graphs and destination tag conflicts for
both images are checked before any copy. Missing repositories, unavailable
authorization, wrong identities, missing attestations, and conflicting tags
fail closed. A failure during copy can leave immutable partial artifacts in ECR;
it never emits a verified pair. Do not activate from those artifacts. A later
dispatch may reuse only byte-identical existing tags.

ORAS 1.3.0 copies every platform and recursive OCI referrer without a platform
filter. The verifier freshly downloads and hashes every referenced manifest,
configuration and layer at source and destination. Legacy Cosign `.sig`, `.att`
and `.sbom` tags are explicitly enumerated, resolved, copied by digest and
verified too: recursive OCI copying alone does not cover legacy tags. External
descriptor URLs and unsupported artifact manifest formats are refused. Bounded
OCI inline `data` is decoded strictly and checked against its digest and size;
those bytes are freshly read inside the containing hashed manifest. The
source inventory is read again after copy to detect additions or movement.
OCI `subject` descriptors in hash-verified manifests also establish the reverse
referrer edge after every endpoint has been verified. This makes an index-contained
attestation compare consistently when one registry omits its discovery entry.
Every discovered edge is retained: extra artifacts, missing content and conflicting
descriptors still fail exact graph equality. No relationship is inferred from
unverified annotations or from registry names.
Destination legacy tags use exact `BatchGetImage` lookups, so no `ListImages`
permission is needed. AWS documents referrer discovery as using the existing
`BatchGetImage` permission too; the only added read action is layer download.

Bounds are 8 MiB metadata/tool output, 4 GiB per blob, 16 GiB cumulative graph
bytes and 4096 descriptors. Tool calls have explicit deadlines. Content is
hashed from bounded pipes and is not retained as an artifact. Only descriptor
metadata, immutable identities and successful paired evidence are uploaded.

The fixed Docker credential helper supports only `get` for the two approved
registries. GHCR reads use the job's package-reader token; ECR authentication
comes from `aws ecr get-login-password` through a captured process pipe. The
temporary Docker config contains helper names only, never credential values.
No login command, token argument, raw tool stderr, credential artifact or image
configuration is logged. Temporary config cleanup finishes before success
evidence is written. The helper is trusted code, not an OS sandbox.

## Verification and evidence

Run offline tests with:

```sh
python3 -B -m unittest discover -s tools/signed-promotion -p 'test_*.py' -v
```

These are controlled API/graph fixtures, not proof of real GHCR/ECR acceptance.
The Linux offline job also runs `cli_fixture.py` with the actual checksum-pinned
ORAS 1.3.0 and Distribution 3.0.0 binaries. It copies a two-platform index, OCI
referrer and legacy tag through a loopback-only registry, hashes actual blob
stdout and exercises the unchanged credential helper with a dummy token and a
wrong-token negative control. An index-contained dummy attestation has its source
discovery edge hidden by the proxy, while the destination exposes it; canonical
graphs must still match. A subsequent destination-only attachment must make the
graphs differ. Test-only DNS resolution and a temporary TLS proxy
map the synthetic GHCR name to loopback; production flags remain strict. The
registry and its children are cleaned up before this fixture reports success.
This checks real CLI compatibility, not ECR IAM or keyless release signatures.
The manual job must pass fresh registry verification before operational use.
Its sole successful artifact is `signed-ecr-pair-<run_id>/signed-pair.json`:
`status=verified-pair`, release tag/SHA, explicit destination tag, trusted workflow
SHA, and both source/destination digest references with complete graph metadata.
It is evidence for review, not an unsigned artifact that grants deployment
authority by itself. The importer/installer must retain its own reviewed custody
boundary. No production promotion has been demonstrated by local unit tests.

## Pinned tool sources

- [ORAS 1.3.0 copy](https://oras.land/docs/commands/oras_cp/) and
  [release](https://github.com/oras-project/oras/releases/tag/v1.3.0): Linux amd64
  archive SHA256 `6cdc692f929100feb08aa8de584d02f7bcc30ec7d88bc2adc2054d782db57c64`.
- [ORAS 1.3.0 credential integration](https://github.com/oras-project/oras/blob/v1.3.0/internal/credential/store.go)
  uses [oras-go 2.6.0 Docker credential helpers](https://github.com/oras-project/oras-go/blob/v2.6.0/registry/remote/credentials/store.go).
- [Cosign 3.0.6 copy deprecation](https://github.com/sigstore/cosign/blob/v3.0.6/cmd/cosign/cli/copy.go)
  recommends ORAS recursive copying; the old copier handles legacy tags only.
- [ECR manifest formats](https://docs.aws.amazon.com/AmazonECR/latest/userguide/image-manifest-formats.html):
  digest pulls do not translate manifest bytes.
- [OCI 1.1.1 subject](https://github.com/opencontainers/image-spec/blob/v1.1.1/manifest.md#image-manifest-property-descriptions)
  defines the relationship represented by referrer discovery.
