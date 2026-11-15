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
stage/category, never subprocess arguments or one-run credentials. Thirty-five
qualification controls discriminate these failures alongside the 14 projection
controls: 49 cases inside the CLI sandbox, plus seven shared registry lifecycle
controls and one synthetic diagnostic control on the host before authentication.
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

Actual [run 43674164917](https://github.com/eighred/kanz/actions/runs/43674164917)
passed all six fixed-CRITICAL/Java image scans and the MySQL container-profile
check, then refused at the first broker readiness wait. The retained receipt and
job logs contained no broker state or startup output; they establish no cause.
The runtime cases and actual MySQL process/SQL observations were not reached.

The subsequent [run 43675904332](https://github.com/eighred/kanz/actions/runs/43675904332)
bound the same owned broker container ID before and after its first failed
probe. It had exited with code 1, without an OOM or Docker state error. Its
complete bounded startup-log capture matched only the permission signal; that
receipt identifies neither an operation/path nor a fatal cause. All six scans
and the MySQL container profile passed; process/SQL and runtime cases were not
reached. The cleanup and credential-absence evidence does not supply acceptance.

Readiness diagnostics now bind the exact source/run/nonce/image/container ID
before executing a probe. Subsequent execution, inspection and broker log reads
use that immutable ID; a replaced name cannot redirect the observed output.
Foreign bindings collect no probe/log signals or captured hashes. The bounded
projection retains Docker's public state/exit/OOM flags, command output length
and hash, and fixed possibly-multiple text signals from only the owned broker's
last 40 startup log lines. It retains no raw logs, commands, environment, error
text or credentials. Probe/inspection/log capture has a streaming 64 KiB ceiling
and process-group termination at overflow, inside the unchanged command limits.
Only eight recent failed attempts per selected role survive; terminal state,
unknown inspection and readiness deadline remain distinct. Signals are
informational and cannot grant readiness, acceptance or cleanup authority.
Within the last 40 captured lines, at most 16 aggregated context records retain
fixed enum values and counts, with omitted-line counts and explicit truncation.
Only an adjacent operation/path/errno sequence matching
[Go 1.27.3's PathError format](https://github.com/golang/go/blob/go1.27.3/src/io/fs/fs.go#L250)
binds a supported operation to a resource class. This recognises a text format,
not an actual caller. Paths are classified only as the selected broker config
or data tree, the exact bounded temporary root, the packaged vendor config or
coredump tree, standard proc/sys classes, or unclassified. Exact config names
and directory boundaries matter; case changes, traversal components and
appended filename suffixes are not
normalised into selected resources. Multiple sequences remain ambiguous.
Fixed standard errno literals are also retained independently of that format
binding. An unclassified C++ or other caller can therefore report an
`operation-not-permitted` text signal while its attributed operation/resource/
error fields remain unclassified. Explicit `coOccurrence` lists retain only
fixed resource, API and template classes from the same line; they do not bind an
API to a resource or an errno, and they never join separate lines.
The selected compiled broker payload supplies distinct perf-event stall and
CPU-profiler fallback templates, a startup-failure wrapper, filesystem/API
wrappers and bounded storage, memory, diskstats, disk-read and PID message
classes. A fallback is not a startup-failure wrapper; either may coexist with
other signals. PID unlink text may concern cleanup. These template matches
assert neither severity nor reachability nor a fatal cause. Storage-alert
configuration descriptions are not classified as refusal messages. Projection
is deliberately non-exhaustive, so an absent match proves no global absence.
The compiled-template provenance is the selected broker manifest
`sha256:d5cc5fa94b1caf1cee182f4ab6305d73e7ba89ad58ed806bb31f7ae0ae038442`,
whose verified final layer is 84,020,836 compressed bytes with SHA-256
`f264d70df1666110d3f1dc3340a432d07dc258651331f052c646dad07c42bf71`
and 264,802,304 uncompressed bytes with DiffID
`sha256:341dad0a981197b2fb3c23a22853ec1c57417c489c9233bd4faf65916fba738a`.
The layer's broker ELF is 132,123,312 bytes with SHA-256
`d979b88bb0dc20dfbae899f317f0ece73b3688827afcc9ffe4019e062e1fa65f`.
This binds literal presence in the selected binary, without establishing its
exact public vendor-source revision or which message was emitted at runtime.
Selected config/data paths come from the committed launch; packaged paths come
from the selected image payload, which establishes presence rather than writes.
The selected broker's exact public startup-source join remains unresolved;
vendor stage and log level therefore remain explicitly unclassified. No raw
line, path, environment, argument or arbitrary capture enters these records.
The existing pre-authentication synthetic diagnostic control also exercises
secret-bearing runtime projection, same-line binding/co-occurrence, independent
errno text, fallback/wrapper discrimination, unknown path/operation/level
refusal, ambiguity and both context limits; target-side controls
discriminate foreign owners/name replacement, non-broker log refusal, retention
and capture overflow. The inventory remains 49 catalog controls, seven shared
auth controls, one pre-authentication diagnostic control and eight runtime cases.
No guessed broker configuration fix or weakened default follows from the earlier
generic refusal. The changed controller still requires a fresh full Linux run.

### Fixture maintenance images

The disposable fixture selects official releases
[OpenSearch 3.9.0](https://github.com/opensearch-project/OpenSearch/releases/tag/3.9.0)
and Redpanda 26.2.7, plus Canonical's maintained MySQL distribution described
below. `images.json` selects their exact linux/amd64 platform
manifests; `image-descriptors.json` binds the independently read raw manifest and
config hashes, byte counts, media types and ordered rootfs DiffIDs. The three
DataHub image selections remain unchanged.

The previous OpenSearch 2.19.7 candidate still reported two fixed-CRITICAL
`bcprov-jdk15to18` 1.79 findings in
[run 43671955287](https://github.com/eighred/kanz/actions/runs/43671955287).
DataHub 1.8.0's immutable
[shim factory](https://github.com/datahub-project/datahub/blob/faab576d96cbdb27b8344a755e0bc0f42fdc9c15/metadata-service/factories/src/main/java/com/linkedin/gms/factory/search/SearchClientShimFactory.java)
uses engine autodetection by default; its
[dispatch and detection](https://github.com/datahub-project/datahub/blob/faab576d96cbdb27b8344a755e0bc0f42fdc9c15/metadata-io/src/main/java/com/linkedin/metadata/search/elasticsearch/client/shim/SearchClientShimUtil.java)
recognize `3.*` as `OPENSEARCH_3` and select the unified OpenSearch client consumed
by search, index and graph factories. The fixture retains that default. The
older 2.x-only shim guide is stale; upstream's 3.2.0 integration target does not
prove this 3.9.0 image. OpenSearch's
[versioned breaking changes](https://docs.opensearch.org/3.9/breaking-changes/#performance-analyzer-plugin)
record RCA removal in 3.0 while retaining the Performance Analyzer plugin.
Actual clean-scan and DataHub runtime compatibility still require the full workflow.
No jar or plugin is removed or replaced to obtain a scanner pass.

The selected Redpanda image records `libbrotli1:amd64` version
`1.1.0-2+deb13u1`, which [Debian DSA-6582-1](https://lists.debian.org/debian-security-announce/2026/msg00498.html)
identifies as fixing CVE-2026-111497. Its three Brotli library binaries and three
SONAME symlinks match the official fixed Debian package exactly across the
verified image layers. Its runtime configuration matches the previous image;
Redpanda's [patch-release policy](https://docs.redpanda.com/streaming/current/upgrade/rolling-upgrade/)
preserves user-facing behavior. These source and package checks do not establish
a clean scanner verdict or DataHub runtime compatibility. The unchanged full
six-image fixed-CRITICAL/Java scan and actual catalog workflow must qualify the
selected bytes before acceptance. The earlier 2.19.7 refusal remains failed
evidence and does not qualify the new 3.9.0 bytes.

The disposable SQL store selects Canonical's maintained stable
[`ubuntu/mysql:8.4-26.04_stable`](https://hub.docker.com/r/ubuntu/mysql), pinned to
amd64 manifest `b22e8fc950df66a17d4b001b28ef5b557b9572d06da25f93967ea3f455c6410e`
and config `73b12c45d61f5005fe5252c2bbb65d75d5f0e5c3725ff0f97d25a35fa9169d4f`.
Read-only data inspection verified all five compressed layer hashes/sizes:
124,184,162 bytes and 7,668 entries. There were no gosu or certifi paths and no
`.whl`, `.egg` or `.zip` entries. The final Pebble binary's Go build metadata reports
1.27.1; the MySQL packages report `8.4.11-0ubuntu0.26.04.1`. The mysqld binary
contains that full version literal; its actual SQL response remains unproved.
These observations address the old payload selection and do not establish a
clean scan, vulnerability absence or DataHub runtime compatibility.

The embedded vendor initialization helper and MySQL configuration match
[Canonical source revision 2436b6a2](https://github.com/canonical/mysql-rock/tree/2436b6a2b44249d4a8f80fa80e8a60b857e62330).
The selected image's actual mysql UID/GID is `584788:584788`. Its unchanged
`/usr/bin/pebble enter` entrypoint starts the vendor service plan as that user.
The fixture sets the same explicit user, retains the 1 GiB data and 16 MiB
socket tmpfs ceilings with that owner, and uses the existing bounded `/tmp` for
Pebble state. Documented
[`PEBBLE_COPY_ONCE` and `PEBBLE_PERSIST`](https://ubuntu.com/docs/pebble/reference/environment-variables/)
seed the immutable vendor plan into `/tmp/catalog-pebble` and keep daemon state
in memory. The root filesystem, configuration and binaries remain read-only;
capabilities remain dropped, no-new-privileges remains enabled, and all CPU,
memory, process, network and host-credential boundaries remain unchanged. No
helper package is replaced or removed, and no new image is built for MySQL.

The source helper initializes an empty data directory, configures the provided
one-run password and `datahub` database through a temporary socket-only server,
then executes the final server. The fixture inspects the actual image/user,
vendor entrypoint and isolation/resource profile before trusting readiness. A
bounded internal probe requires the final network-enabled server, checks the
actual Pebble/mysqld effective UID/GID and reads SQL `VERSION()`. Only matching
public version data, numeric identities, output length/hash and verified profile
flags enter the receipt. Missing/wrong identities, changed mounts/privileges or
an unexpected version refuse; raw output, environment and credentials do not
enter receipts or exceptions. These checks still require hosted Linux proof.

DataHub 1.8's
[pinned Connector/J 9.4.0](https://github.com/datahub-project/datahub/blob/faab576d96cbdb27b8344a755e0bc0f42fdc9c15/build.gradle#L364)
supports MySQL 8.0 and later according to the
[vendor release notes](https://dev.mysql.com/doc/relnotes/connector-j/en/news-9-4-0.html).
That supports this disposable maintenance choice; all six unchanged scanner
checks and all eight actual catalog cases remain mandatory. Pebble's embedded
VCS revision was not retrievable from the public source/module endpoints during
inspection. The publisher image bytes and matching helper/configuration are
verified, but the installed Pebble's exact nonroot/copy-once behavior remains a
runtime qualification boundary. This fixture does not change Kanz PostgreSQL,
broker authority or any held production/native runtime.

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
Those validated public Docker/client/API and builder-limit fields are preserved
before launch, including on failure. A bounded parser records only output length
and SHA, closed signal categories, exact recipe phases and projected package/Python
observations. It retains multiple matching signals as ambiguity; none is promoted
to a proven root cause. Raw output, commands, environment and credential values
are never retained. The synthetic redaction control runs before authentication
and its count/output hash enter the source-bound receipt.

These diagnostics are informational. Process capture completion and printed
recipe-completed/proof markers cannot acknowledge daemon completion. Only the
unchanged successful Docker response, actual returned IID and verified full
Parent/ownership chain can create the exclusive completion marker. Unknown
completion still refuses cleanup even if diagnostic markers report progress.

`image-descriptors.json` pins all six selected platform manifests, their config
descriptors and ordered rootfs diff IDs. Before pull acceptance, the controller
verifies raw manifest bytes/hash, its exact config descriptor/size, raw config
bytes/hash and linux/amd64 platform/rootfs. Indices are unsupported inputs and
refuse. Only projected public identity fields survive; raw configuration and
public registry tokens are never recorded. Canonical repository plus manifest
binding is required; a matching digest suffix from another repository is refused.

Docker store formats can report either the platform-manifest digest or its
linked config digest as `inspect.Id`. The controller accepts only exact matches
to those verified descriptors and records the kind. Trivy is explicitly limited
to `--image-src docker`; its image ID must equal that actual Docker ID, not the
other known digest. Exact artifact name, platform, ordered diff IDs and canonical
repository/manifest binding must also agree. No default-source fallback is used.

The source owner record retains immutable descriptor authority. Launch and
completion markers bind one actual verified base engine ID/kind. Cleanup still
requires its complete parent chain to end at that single observed ID, never at
either member of an interchangeable allow-list. Missing parent metadata refuses.
This preserves the existing builder/daemon and package correction.

Preparation records pulled actual IDs before patching and separates selection,
base validation, context creation/input copying, download, package write, Docker
version and build stages. Its public projection contains only the base image ID,
config/manifest match flags, `OnBuild` count/empty flag and package HTTP status,
received size/hash and closed refusal category. It never records raw image
configuration, environment, URLs from errors or command logs. A current primary
registry/package read cannot establish the cause of an earlier generic CI refusal.
Inherited `OnBuild` triggers must be empty: Docker executes them before the
first explicit instruction. The template renders one complete six-label owner
instruction immediately after `FROM`, before any `ARG`, `USER`, `COPY` or `RUN`;
CLI tail labels are not used. Thus every new build image/container inherits the
complete nonce/source/run/attempt/package identity. Both template and rendered
Dockerfile hashes enter the owner record and receipt.

Before and after `dpkg -i`, the verifier records the complete installed package
inventory and Python dependency graph. Exactly `linux-libc-dev:amd64` must change
to the pinned version; the Python graph/version must remain identical. The
installed `acryl-datahub` version must exactly match `cliPackageVersion`, pinned
separately as `1.8.0+docker`. Image inspection also requires the exact base layer prefix
and unchanged user, entrypoint, command, environment and other execution config.
The executed CLI rereads this build proof and must match its dependency hash.

The selected product release remains 1.8.0. The verified immutable base config
`a9d4daa22bb41a875400152e2b03724ffb1a07fb5616675785586b2d0a5c8348`
records `RELEASE_VERSION=1.8.0+docker`; the release's
[Dockerfile](https://github.com/datahub-project/datahub/blob/faab576d96cbdb27b8344a755e0bc0f42fdc9c15/docker/datahub-ingestion/Dockerfile#L147)
passes that argument to its
[version updater](https://github.com/datahub-project/datahub/blob/faab576d96cbdb27b8344a755e0bc0f42fdc9c15/python-build/version_updater.py#L29)
before the editable installation. That source-backed packaging literal is
required everywhere without stripping or normalizing build metadata. A plain
`1.8.0` dependency refuses even if its before/after graph is otherwise identical.
The observed earlier equality mismatch supports this causal inference; actual
dependency readback must still confirm the expected installed literal.

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
removed chain/container IDs and rechecks enter the receipt. Nineteen added controls
discriminate package/Python drift, config/OnBuild/owner mismatch, complete chain
cleanup, late interrupted resources, actual offline build selection and advisory
versus infrastructure exits, raw descriptor tampering, engine kind, canonical
repository binding and Docker-only scan identity.

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

## Private registry lifecycle

The workflow completes generation, Go compilation/vet/architecture checks,
source-free registry controls and audit-tool installation before introducing
registry credentials. It then verifies the clean source against independent
Actions identity. Only afterward does the shared
`.github/scripts/catalog_registry_auth.py` prepare a fresh exclusive host
profile and the pinned login action authenticate with packages-read authority.
Its automatic logout is disabled; the shared helper owns real credential absence
verification and removal. No host build or installer runs after authentication.

The controller verifies that profile's exact source/run ownership and file-only
backend. The profile is outside every target scratch/source/binary/workflow mount
and the build context. Only reviewed host Docker commands receive its path;
target arguments/environment and build inputs never receive the profile or token.
The controller does not borrow a developer/runner's preexisting Docker profile.
Public descriptor retrieval uses anonymous publisher-scoped access independently.

Target teardown runs on the always path before shared registry cleanup. The
shared cleanup receipt binds current source/tree/run/attempt, verifies actual
credential absence after logout and removes the verified owned directory.
`acceptanceComplete` remains false until both teardown and that cleanup pass,
the source is rechecked and all mandatory metadata/authorization evidence is
validated again. Exit zero alone cannot prove logout. Missing target evidence,
residual credentials, stale ownership or failed profile removal refuse closure.
Only public projected cleanup/control records are uploaded.

The dedicated architecture check includes both the base-image mirror invariant
and the existing authentication guard. Catalog controls independently check the
indirect controller's permissions, host-build-before-login ordering, explicit
logout ownership and always cleanup, including mutations of each. Both PR and
main filters select the shared helper and its controls. The wider source-selected
repository checks remain independent mandatory gates.

Publisher descriptor provenance and execution repository are explicit separate
fields. A private mirror execution reference must retain the exact verified
publisher manifest/config bytes and match its own canonical repository in Docker
and scanner readback. Populate and privately read-verify the existing mirror
producer before repointing the Dockerfile/base lock/selected reference together;
the mirror guard must pass before an overlay build is attempted.
