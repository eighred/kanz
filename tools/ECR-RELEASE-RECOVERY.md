# ECR release custody

Tokyo commit tags are immutable release artifacts. Keep tagged images until an
explicit retirement review proves that neither active manifests nor approved
rollback manifests reference them. A "newest N builds" or age limit is unsafe:
deployment cadence is independent of build cadence. Terraform expires only
untagged images after seven days. Preview policies on **every** repository before
applying them and reject a preview that expires any tagged artifact.

## Exact artifact recovery

Do not restart the estate to test a missing image. Inventory all namespaces,
including init containers, completed migration jobs, edge/observability images,
and the approved rollback set. Bind digests to reviewed Git manifests and release
revisions before trusting a cache export. A new build is a new release, never a
replacement for a missing digest.

`ecr_release_recovery.py` accepts a JSON array. Each recovery entry contains
`repository`, `digest`, `manifest` (base64 of the **original manifest bytes**),
and `labels.org.opencontainers.image.revision` (full source commit). Obtain these
from the reviewed cache/export, not an untrusted registry tag alone. Keep this
inventory and the resulting archive outside the source checkout.

```powershell
python tools/ecr_release_recovery.py --inventory INVENTORY.json --archive ARCHIVE
python tools/ecr_release_recovery.py --inventory INVENTORY.json --archive ARCHIVE --apply
python tools/ecr_release_recovery.py --inventory INVENTORY.json --archive FRESH_ARCHIVE --verify-ecr
```

AWS CLI v2 uses `kanz-platform` in Tokyo by default; `gh auth` must have package
read access. Credentials stay in memory. No Docker login file is written.
An optional `--cache-url http://127.0.0.1:PORT` can retrieve original blobs through
an SSM port forward to a loopback-only, digest-allowlisted cache reader. Never
expose the containerd content directory or credentials through a public server.
Stop the reader and SSM session after export.

Recovery requires the independent GHCR build's configuration descriptor to match
the pinned original. It verifies each compressed blob's size/hash, the verified
configuration's source/revision, and every uncompressed layer against its
configuration diff ID. This permits byte-exact recovery when the original ECR
publication recompressed layers. It does not rebuild or recompress anything.
`--apply` refuses conflicting release tags and tagged-expiration policies.
Recovery currently supports the project's single-platform Docker schema-2
application images; it deliberately refuses manifest lists. Fresh ECR verification
supports recursive manifest lists, including the separately mirrored Prometheus
image, and downloads every blob from ECR even if a local copy exists.

The archive is an OCI image layout with original manifests and content-addressed
blobs. Retain an independent copy off the workload node; record archive hashes
and custody location in the change record. Do not rely on node containerd caches
or mutable upstream tags as durable release custody. The verifier is an integrity
and retrieval proof, not a claim of a publisher signature. Enforce any additional
signature/attestation policy applicable to the release separately.

After merging, run `Install-TestnetWorkloads.ps1 -InstanceId INSTANCE` **without
`-Apply`** from exact `origin/main`. This verifies per-workload release tags and
server-side dry-run validation. Record the command ID and result. Do not resume
trading or advance deployed versions as part of registry recovery.

## Regression checks

```powershell
python -m unittest discover -s tools -p test_ecr_release_recovery.py
./tools/Test-EcrReleaseDigest.ps1
# From kanz/infra/terraform-testnet after terraform init:
./Invoke-Terraform.ps1 test
```

The Windows installers check the shared digest helper against merged main before
loading it. Empty output, malformed digests, missing images, permission failures,
and nonzero CLI exits all refuse deployment; mixed workload releases remain bound
to each workload's own annotation and pinned digest.
