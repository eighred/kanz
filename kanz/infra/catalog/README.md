# Optional data catalog (LIN-01b)

The catalog contains projected dataset metadata. The lineage service remains
the authority for governed provenance queries. Catalog ownership or tags cannot
grant `lineage.pii.read`, replace the mounted governance policy or activate a
financial path. No estate deployment is added by this directory.

## Explicit ingestion

DataHub 1.8.0 has a supported metadata `file` source; it does not register an
`openlineage` ingestion source. `projection.py` translates the existing Kanz
OpenLineage shape into that source, then the official REST sink ingests it:

```sh
python3 projection.py --mapping /mounted/catalog-mapping.json \
  --events /authorized/openlineage.json --output /scratch/metadata.json
export KANZ_CATALOG_METADATA_FILE=/scratch/metadata.json
export DATAHUB_GMS_URL=https://approved-catalog.example
# Supply DATAHUB_GMS_AUTHORIZATION through the approved secret environment.
datahub ingest -c datahub-recipe.yaml
```

The mapping names each exact namespace/name, versioned schema reference,
steward, PII/public sensitivity and approved schema fields. The namespace/name
identity uses the bijective `namespace/name` tuple encoding. Neither validated
component permits slash, so separate later batches cannot collide through dots. There is
no default steward, blanket PII tag, CODEOWNERS import, arbitrary dataset
discovery or schema-registry query. `fixture_mapping.json` is exclusively a dummy
two-dataset example, not an institutional ownership decision. Real deployment
mapping and source authorization remain the deploying owner's responsibility.

The projection checks the complete batch before creating a fresh output file.
Unknown datasets, schema-reference disagreement, conflicting mappings, malformed
inputs and oversized documents refuse before ingestion. Current root
`inputs: null` means an empty upstream set; repeated events union their dataset
edges like the Go graph. Event payloads and arbitrary facets are rejected.
No field values, financial amounts, tokens or credentials enter the projection.

Each dataset receives ownership/tag metadata, its exact `payloadSchemaRef`,
explicit `schemaMetadata` and the observed upstream set. This is an explicit
schema mapping; it does not assert registry or retained-history completeness.
Removing the optional catalog changes no Kanz domain contract. Re-ingestion
replaces projected aspects for named datasets; historical cleanup/deletion is
not inferred from an absent input row.

## Disposable acceptance

`.github/workflows/catalog-verification.yml` compiles actual lineage server tests
with the race detector, then runs the binaries and Python checks inside bounded
containers. The Linux fixture pulls immutable linux/amd64 images from
`images.json`, scans them using the existing fixed-CRITICAL release policy,
and creates one internal network without published ports. GMS uses real
one-run system authentication; MySQL credentials and signing inputs are generated
in memory. Containers have read-only roots, bounded tmpfs/state, dropped
capabilities and no host credentials/socket. Source and test binaries are mounted
read-only. Containers cannot download dependencies; the locked official ingestion
base pins the CLI and its transitive environment. The broker is disposable
Redpanda, not an additional authoritative Kanz writer or JVM Kafka runtime.

The fixture exports OpenLineage events through the actual Go harvester, runs
`datahub ingest -c /catalog/datahub-recipe.yaml`, and reads actual GMS aspects.
Two dummy datasets must retain distinct stewards, distinct sensitivity tags,
exact schema references/fields and both edges of a legitimate bounded dataset
cycle produced by the Go harvester. An unauthenticated catalog
read must refuse. A second real ingestion changes catalog ownership to a viewer
and tags to PUBLIC. Actual lineage HTTP handlers must still refuse anonymous
and viewer PII reads, allow the governed steward and redact upstream PII.

The receipt binds source/tree SHA, GitHub run/attempt, actual image IDs/digests,
CLI/Python dependencies, scanner outcomes, mandatory cases and test hashes.
The clean source must match independent `GITHUB_SHA` before resource creation and
after proof. An exclusive no-follow owner record binds nonce labels and exact
resource names to that source/run/attempt. Normal and interrupted cleanup inspect
those labels and delete verified IDs, including a container created before a
failed start; foreign names/owners cannot authorize deletion. Cleanup or final
receipt refusal fails acceptance. Timeout diagnostics contain only a closed
stage/category, never subprocess arguments or one-run credentials. Twenty-four
qualification controls discriminate these failures alongside the 14 projection
controls.
The PR/main trigger contract includes the consumed authorization/lineage
dependencies, canonical generation inputs, Go runtime pins, setup action and
reviewed advisory installer. A control reads the real workflow through a
read-only mount and rejects omitted scanner/auth/schema selectors.

Trivy's [pinned OCI implementation](https://github.com/aquasecurity/trivy/blob/v0.72.0/pkg/oci/artifact.go#L188)
writes the compressed database layer to a temporary
file before extraction. The reference main/Java DB layers observed during this
repair were 140,857,902 and 999,527,167 bytes; an 8 MiB file ceiling cannot admit
those legitimate inputs. Scanner commands now have an 8 GiB per-file ceiling
and an independently enforced 12 GiB private tmpfs for cache plus temporary
files. Ordinary command file limits and all target-container caps remain unchanged.
The tmpfs uses nosuid/nodev/noexec, the runner's uid/gid and an exact source/nonce
mount identity in the exclusive owner record. Normal/interrupted cleanup verifies
that identity before unmounting. Scans preserve the existing fixed-CRITICAL
policy and Java analysis; no advisory exception is added. The database hashes,
sizes, safe metadata and limits enter the receipt. The cache is released before
GMS/stores start. The 12 GiB ceiling is not a capacity or extracted-size pass;
the actual hosted Linux run must qualify available memory and the complete scan.

An independent 8 MiB stdout/stderr pipe budget kills and reaps the process group
at overflow, with a separate deadline. Scanner storage, database download,
network diagnostics and actual structured advisory refusal have closed error
categories. No raw command arguments/logs or credentials enter diagnostics.
The earlier CI stopped before per-image scans; its exact errno was not retained
and is not relabelled as an observed EFBIG or advisory result.
Required runtime cases cannot skip. Normal runs without the disposable catalog
skip the two externally gated tests; the dedicated workflow proves them.
Runtime logs, command arguments and generated credentials are never uploaded.
Containers and their ephemeral state are removed on exit.

### Fixture-only ingestion package correction

The official 1.8.0 locked CLI manifest selected in `images.json` was unchanged
when checked on 2026-11-15. Actual run 43652444758 refused its 25 fixed-CRITICAL
`linux-libc-dev` findings (installed 6.8.0-147.147, fixed 6.8.0-149.149) before
service startup. `ingestion_patch/package-lock.json` pins the official Ubuntu
amd64 replacement: 1,698,546 bytes, SHA256
`e4b7b9c5023ba727908868661e1113d28ed9b68e47aed00e314723dad4b2b31d`.
The reviewed publisher HTTPS package indexes and exact downloaded bytes agreed;
GPG verification was not executed. This is an explicit retrieval limitation,
not a signature-validation claim. No package resolver or advisory exception is
introduced, and this fixture correction does not promote a production image.

The fresh private build context contains only the tracked Dockerfile, verifier,
lock and verified public `.deb`, outside credential-bearing runtime scratch.
The offline legacy Docker build uses `DOCKER_BUILDKIT=0`, no network/pull/tag,
1 GiB memory/swap, two CPUs, a 300-second deadline and `--force-rm` for its
intermediate containers. That flag does not remove intermediate images.
Unsupported legacy builders refuse; no fallback is
attempted. The receipt records the actual client/server/API versions, builder
mode and limits. Hosted compatibility remains unproved until the exact CI run.

The exact base config image ID is pinned and checked against the pulled image.
Inherited `OnBuild` triggers must be empty: Docker executes them before the
first explicit instruction. The template renders one complete six-label owner
instruction immediately after `FROM`, before any `ARG`, `USER`, `COPY` or `RUN`;
CLI tail labels are not used. Thus every new build image/container inherits the
complete nonce/source/run/attempt/package identity. Both template and rendered
Dockerfile hashes enter the owner record and receipt.

Before and after `dpkg -i`, the verifier records the complete installed package
inventory and Python dependency graph. Exactly `linux-libc-dev:amd64` must change
to the pinned version; the Python graph/version and `acryl-datahub` 1.8.0 must
remain identical. Image inspection also requires the exact base layer prefix
and unchanged user, entrypoint, command, environment and other execution config.
The executed CLI rereads this build proof and must match its dependency hash.

The upstream manifest/base image ID and derived local image ID have separate
receipt fields. Only the actual untagged derived ID is scanned and used for
CLI/test execution; the base digest is never described as the executed image.
All six runtime roles retain complete fixed-CRITICAL reports before advisory
refusal, so one image cannot hide later roles' findings. The scan now requests
advisory exit 42; only that exact code plus a complete, exact-image report may
continue to the next role. Infrastructure exit 1, report, database and resource
failures stop immediately even when advisory JSON exists. Informational DB logs
cannot mask code 42. This changes the discriminator, not severity, fixed-only or
Java policy. The historical run above used advisory exit 1. No service/network starts
if any advisory remains, and Java analysis remains enabled.

The exclusive owner record additionally binds the exact nonce/source/run/attempt,
package hash, Dockerfile/verifier/lock hashes and all build-image labels. Before
launch, an exclusive no-follow marker outside the build context records that
identity. Only a normal successful Docker response, valid returned image ID and
verified ancestry can create its exclusive completion marker. A killed client
is not daemon-terminal proof: normal/interruption cleanup performs bounded
owned-resource rechecks, but unknown completion always prevents a clean pass,
even after empty inventories. A finite quiet window cannot prove future absence.

Cleanup validates at most 16 fully labeled immutable image IDs as one unbranched
`Parent` chain ending at the exact pinned base, and inspects at most four owned
build containers per pass. Missing/unsupported parent metadata, disconnected or
cyclic ancestry, foreign labels/tags/registry references and unexpected terminal
IDs refuse. Only verified build containers and child-to-parent image IDs are
removed, followed by repeated inventories. Image deletion uses `--no-prune`,
never force, the base, foreign tags or shared cache deletion. The completion ID,
removed chain/container IDs and rechecks enter the receipt. Eleven added controls
discriminate package/Python drift, config/OnBuild/owner mismatch, complete chain
cleanup, late interrupted resources, actual offline build selection and advisory
versus infrastructure exits.

Relevant pinned source: [Trivy configured finding exit](https://github.com/aquasecurity/trivy/blob/v0.72.0/pkg/commands/operation/operation.go#L112),
[fatal exit 1](https://github.com/aquasecurity/trivy/blob/v0.72.0/pkg/log/logger.go#L78),
[Moby inherited triggers](https://github.com/moby/moby/blob/v28.5.2/builder/dockerfile/dispatchers.go#L142),
and [Docker successful stream/IID completion](https://github.com/docker/cli/blob/v28.5.2/cli/command/image/build.go#L334).
These source references explain the contract; actual recorded runner versions
must qualify it in CI.

Repository/fixture acceptance is distinct from production catalog activation,
source rights, complete schema-registry integration or capital approval. Close
[#1405](https://github.com/eighred/kanz/issues/1405) only after the exact merged
main workflow and its full projected receipt are verified.

Selected upstream source: [DataHub v1.8.0](https://github.com/datahub-project/datahub/tree/v1.8.0),
including its metadata file source, REST sink and GMS system authenticator.
