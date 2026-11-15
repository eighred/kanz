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

The disposable upgrade and GMS processes share
`KAFKA_PRODUCER_COMPRESSION_TYPE=none`. Their launcher creates the signing key
and salt once and shares the same options between both role-specific starts.
In the pinned [DataHub source](https://github.com/datahub-project/datahub/tree/faab576d96cbdb27b8344a755e0bc0f42fdc9c15),
`application.yaml` line 1252 feeds `DataHubKafkaProducerFactory` lines 163–165;
the SystemUpdate Kafka messaging configuration uses that producer builder too.
The verified Confluent Kafka `8.2.2-ccs` source revision `745df1f` accepts `none`
and selects `NoCompression`. This fixture-only producer choice trades compression
for larger wire records while preserving acknowledgements, retries, idempotence,
request limits and isolation. It changes neither decoding nor any global native
policy and does not establish Snappy as the earlier failure's cause. Actual
startup, delivery and all acceptance cases still require the Linux workflow.

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
stage/category, never subprocess arguments or one-run credentials. Forty-four
qualification controls discriminate these failures alongside the 14 projection
controls: 58 cases inside the CLI sandbox, plus seven shared registry lifecycle
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
Required runtime cases cannot skip. Normal runs without the disposable catalog
skip the two externally gated tests; the dedicated workflow proves them.
Runtime logs, command arguments and generated credentials are never uploaded.
Containers and their ephemeral state are removed on exit.

Immutable verification history, prior refusals and source-bound CI artifacts
are recorded in [PR #1727](https://github.com/eighred/kanz/pull/1727).

Readiness diagnostics now bind the exact source/run/nonce/image/container ID
before executing a probe. Subsequent execution, inspection and broker log reads
use that immutable ID; a replaced name cannot redirect the observed output.
Foreign bindings collect no probe/log signals or captured hashes. The bounded
projection retains Docker's public state/exit/OOM flags, command output length
and hash, and fixed possibly-multiple text signals from only the owned broker's
or search container's last 40 startup log lines. Source-specific allowlists keep
broker and search template/resource classes separate. It retains no raw logs,
commands, environment, error
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
Search diagnostics use a finite allowlist from public OpenSearch 3.9.0 source
at [4ee42a94](https://github.com/opensearch-project/OpenSearch/tree/4ee42a94e87f66fbf1e62a9871b1b87f91e02472):
launcher/config/JVM-option messages, bootstrap/root-cause wrappers, node-lock/
data-write messages and fixed exception tokens. The sensitive message seams are
[KeyStoreWrapper](https://github.com/opensearch-project/OpenSearch/blob/4ee42a94e87f66fbf1e62a9871b1b87f91e02472/server/src/main/java/org/opensearch/common/settings/KeyStoreWrapper.java#L564)
and [JvmOptionsParser](https://github.com/opensearch-project/OpenSearch/blob/4ee42a94e87f66fbf1e62a9871b1b87f91e02472/distribution/tools/launchers/src/main/java/org/opensearch/tools/launchers/JvmOptionsParser.java#L110);
neither arbitrary exception names, JVM options nor message tails are retained.
Search resource co-occurrence classes cover only its packaged config tree,
selected data/log trees and shared temporary/proc/sys classes. Stage, level and
causal attribution remain unknown. Search suffix fingerprints follow the CLI
source-frame contract below; unclassified lines have a separate bounded sample
contract. Conditional vendor keystore creation/upgrades do not establish the
selected image's exact file state. The config relocation below supports the
documented write flow without assuming a particular missing or stale file.
Search retains
the four `-E` arguments, image pins and isolation/resource profile; its explicit
BC-FIPS Java-variant property is documented below.
The [JvmErgonomics options probe](https://github.com/opensearch-project/OpenSearch/blob/4ee42a94e87f66fbf1e62a9871b1b87f91e02472/distribution/tools/launchers/src/main/java/org/opensearch/tools/launchers/JvmErgonomics.java#L108)
starts a separate Java process before OpenSearch and wraps a nonzero result in
a RuntimeException. Only its fixed nonzero-integer wrapper and known frame
tokens are projected; its output/error sections and variable options are not.
The pinned public [bundle selection](https://github.com/opensearch-project/OpenSearch/blob/4ee42a94e87f66fbf1e62a9871b1b87f91e02472/gradle/libs.versions.toml#L5)
is Adoptium `25.0.4.1+1`. Public OpenJDK 25u tag `jdk-25.0.4.1+1` resolves to
[7b65d74b](https://github.com/openjdk/jdk25u/tree/7b65d74bcbd7c3a0b4c1f5c5bd85e64aa2e51b4a).
That source supplies finite initialization, VM-creation, logging, option/heap,
native-thread, RuntimeException, NullPointerException and OutOfMemoryError
tokens, plus known launcher/container-metrics frame classes. A frame match is
only same-line co-occurrence; it does not establish a cgroup, null-input,
memory or other cause. The public bundle selection is not proof of the selected
image's actual JDK contents/version. No large payload read or execution is
substituted for that unresolved source-to-image join. The wrapper regex cannot
cross any `splitlines()` separator and retains no numeric status or suffix.
The [public CLI error/help path](https://github.com/opensearch-project/OpenSearch/blob/4ee42a94e87f66fbf1e62a9871b1b87f91e02472/libs/cli/src/main/java/org/opensearch/cli/Command.java#L92)
supplies separate whole-line `Starts OpenSearch`, Option/Description column and
line-start `ERROR:` classes. Positional-argument and abnormal-exit messages have
fixed prefix classes. These text frames do not identify an actual CLI caller,
help request or cause; public help/usage exit codes do not assign meaning to an
observed container exit code.

Only search lines beginning with the exact case-sensitive bytes `ERROR: ` may
retain `cliErrorMessage` suffix byte length/SHA-256. There is no leading-space
or arbitrary-prefix allowance. The shared fingerprint implementation requires
one prefix, a valid UTF-8 round trip and a nonempty body of at most 4096 bytes
after explicit ASCII whitespace trimming. Malformed/repeated prefix, invalid
encoding, empty body and overflow retain closed refusal metadata with no body
size/hash. It never joins lines. This field is distinct from the broker-only
`startupMessage`. Both
use the same existing 16-record aggregation/omission budget, 40-line tail,
64 KiB capture ceiling and eight-attempt retention. An exact public-message
digest match remains informational.

Search lines beginning with exact case-sensitive `WARNING: ` receive one fixed
wrapper class. Repeated equal contexts aggregate before unknown sampling; no
individual warning-body fingerprint is added, and stage, level and cause remain
unclassified. This prefix is public in the expected-bundle source's
[ModuleBootstrap](https://github.com/openjdk/jdk25u/blob/7b65d74bcbd7c3a0b4c1f5c5bd85e64aa2e51b4a/src/java.base/share/classes/jdk/internal/module/ModuleBootstrap.java#L1018),
[Unsafe](https://github.com/openjdk/jdk25u/blob/7b65d74bcbd7c3a0b4c1f5c5bd85e64aa2e51b4a/src/jdk.unsupported/share/classes/sun/misc/Unsafe.java#L1891)
and [Module](https://github.com/openjdk/jdk25u/blob/7b65d74bcbd7c3a0b4c1f5c5bd85e64aa2e51b4a/src/java.base/share/classes/java/lang/Module.java#L323).
These public text identities do not identify an actual caller, JDK version or
fatal cause. JDK native-access flags and the isolation profile stay unchanged.

One search-only forwarding-prefix class groups six public method openings:
`NativeLibraries.load`, `NativeLibraries.loadLibrary`,
`NativeLibraries$NativeLibraryImpl.open`, `ClassLoader.loadLibrary`,
`Runtime.load0` and `System.load`. The immutable declarations are in
[NativeLibraries](https://github.com/openjdk/jdk25u/blob/7b65d74bcbd7c3a0b4c1f5c5bd85e64aa2e51b4a/src/java.base/share/classes/jdk/internal/loader/NativeLibraries.java#L527),
[ClassLoader](https://github.com/openjdk/jdk25u/blob/7b65d74bcbd7c3a0b4c1f5c5bd85e64aa2e51b4a/src/java.base/share/classes/java/lang/ClassLoader.java#L2242),
[Runtime](https://github.com/openjdk/jdk25u/blob/7b65d74bcbd7c3a0b4c1f5c5bd85e64aa2e51b4a/src/java.base/share/classes/java/lang/Runtime.java#L761)
and [System](https://github.com/openjdk/jdk25u/blob/7b65d74bcbd7c3a0b4c1f5c5bd85e64aa2e51b4a/src/java.base/share/classes/java/lang/System.java#L1644).
The allowlist requires raw line-start `at java.base/` or the documented one-TAB
form, exact class/method case and the opening parenthesis. It performs no
whitespace, loader or module-version normalization. Equal contexts aggregate
before unknown sampling and receive no individual body fingerprint. These are
forwarding text identities; stage, level, caller, library and cause remain
unclassified. The existing frame parser, sampling budgets and profiles remain.

Completed search-log captures additionally report only UTF-8 round-trip validity
and raw NUL/ESC byte counts as format metadata. They do not strip ANSI escapes,
normalize content or guess an encoding. After finishing all known context
records, unclassified search lines may fill only the remaining record slots:
`len(records) + len(samples) <= 16`. Eligible unknown lines require capture-wide
valid UTF-8 and 1–4096 bytes after explicit ASCII whitespace trimming. Each
sample contains its one-based logical-line ordinal in the full capture,
trimmed byte length/SHA-256 and optional frame digests, never raw text,
exception names, paths or a cause.
If eligible lines exceed capacity, selection takes the first ceiling-half and
last floor-half of that capacity in ordinal order. If they fit, each is retained
once. The 40-line tail remains mandatory; earlier line ordinals are not reset.
Closed counts distinguish considered, eligible, sampled, refused and omitted
lines, with separate invalid-UTF-8, empty-line and line-budget refusals. Refused
or omitted samples mark truncation along with the existing tail/known-record
omissions. Invalid UTF-8 anywhere refuses every individual unknown-line hash.
Broker, readiness-command, MySQL and GMS sources receive no such fallback.

Only already-retained search samples may receive non-null `frame` metadata:
the fixed `jdk-at-frame-v1` enum and original ASCII class/method byte lengths
and SHA-256 digests. Parsing occurs after selection and consumes no extra
sample slots. It recognises only an ASCII-trimmed logical line beginning
`at `, optionally followed by exact `java.base/`, then a fully qualified ASCII
class and method. Identifiers are bounded to 128 bytes, class names to 512 bytes
and 2–32 dot-separated components; methods may also be `<init>` or `<clinit>`.
The ending must be a source-like `.java` basename of at most 128 bytes with a
canonical decimal line number in 0–2147483647, or exact `Native Method` or
`Unknown Source`, inside one final pair of parentheses. No filename, path,
line number, loader, module or version is retained. The existing 4096-byte,
capture-wide UTF-8 and all sampling/source limits still apply.

The public formatter provenance is expected-bundle OpenJDK 25u
[StackTraceElement](https://github.com/openjdk/jdk25u/blob/7b65d74bcbd7c3a0b4c1f5c5bd85e64aa2e51b4a/src/java.base/share/classes/java/lang/StackTraceElement.java#L360)
and [Throwable](https://github.com/openjdk/jdk25u/blob/7b65d74bcbd7c3a0b4c1f5c5bd85e64aa2e51b4a/src/java.base/share/classes/java/lang/Throwable.java#L700).
Filename-without-line, other modules, module versions, loader prefixes, Unicode
identifiers, malformed/ambiguous/trailing and over-budget forms intentionally
retain `frame: null`, with no partial symbol metadata. That result establishes
neither frame absence nor a cause. A valid shape or a public symbol-digest
match establishes neither an actual caller nor the image's JDK version.

Only a broker line containing the exact case-sensitive compiled prefix
`Failure during startup: ` once may retain `startupMessage` byte length and
SHA-256 of its suffix. The capture must round-trip as valid UTF-8; trimming is
limited to ASCII space, tab, CR, LF, VT and FF, and the trimmed body must contain
1–4096 bytes. Unknown/repeated prefix framing, invalid UTF-8, an empty body or
an oversized body retains a closed refusal category with no body size/hash.
Other records and readiness-command output retain no startup fingerprint.
No suffix text, path, type or arbitrary field is retained. The fingerprint
participates in the same bounded aggregation key, and cannot join lines or
authorize readiness, cleanup or a causal conclusion. Only an exact match to a
verified public candidate can identify that public message.
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
discriminate foreign owners/name replacement, MySQL/GMS log refusal, retention
and capture overflow. The inventory contains 58 catalog controls, seven shared
auth controls, one pre-authentication diagnostic control and eight runtime cases.

The disposable broker explicitly advertises `broker:33145` for RPC while
preserving its existing bind address, bootstrap setting and resource/isolation
profile. The [published v26.2 listener contract](https://docs.redpanda.com/streaming/current/manage/security/listener-configuration/#advertise-a-listener)
rejects wildcard advertised addresses. The public v26.2.2 source
[falls back to the bound RPC address when advertisement is omitted](https://github.com/redpanda-data/redpanda/blob/fd30e45b31e0c5e9595e530121a0896e3ddd12a3/src/v/config/node_config.h#L163)
and [validates that advertised RPC address at startup](https://github.com/redpanda-data/redpanda/blob/fd30e45b31e0c5e9595e530121a0896e3ddd12a3/src/v/config/node_config.cc#L326).
Actual broker readiness and the live property proof below remain mandatory.

After broker readiness and before SystemUpdate, the controller binds the exact
source/run/nonce/image/network/name to its immutable container ID, sets only
`auto_create_topics_enabled=true` through localhost Admin API, then reinspects
that ID before reading the same single property and after the read. Both rpk
commands ignore profiles and explicitly select `127.0.0.1:9644`.
[The get command queries SingleKeyConfig and prints only its YAML value](https://github.com/redpanda-data/redpanda/blob/fd30e45b31e0c5e9595e530121a0896e3ddd12a3/src/go/rpk/pkg/cli/cluster/config/get.go#L55).
Only exact ASCII `true` followed by one LF passes; missing, false, malformed,
failed or foreign-owner results refuse setup. The mandatory receipt retains
only the fixed property/value, source/image/container identities, success flags
and readback length/hash. It retains no command output, arguments or profiles.
Each changed controller revision requires the full Linux workflow and all cases.

SystemUpdate has a separate outcome observation and receives no readiness-log
role. Before waiting, the controller binds its canonical name to the selected
image, network and complete source/run/nonce labels, then uses only that full
container ID for `docker wait` and the post-wait inspection. Inspection has a
10-second/64-KiB bound; waiting has a 300-second/64-byte bound. The target's wait
result is distinct from the Docker client's exit status: only canonical ASCII
decimal 0–255 followed by one LF is recorded. Timeout, command failure and
malformed results remain closed refusals; post-inspection still uses the bound
ID. An invalid initial binding permits neither waiting nor further inspection.

`upgradeOutcome` retains source/tree/run/attempt, closed before/after state,
the numeric wait result and fixed categories. It retains no logs, raw output,
arguments, environment or Docker error text. A fast job may already have exited
before binding. Proceeding requires wait zero plus the same owned container's
consistent exited-zero state, without OOM or Docker error; unknown or nonzero
results refuse. Final receipt validation independently checks that join.
The observation grants no cleanup authority and replaces none of the remaining
catalog cases or source/registry/resource checks.

After SystemUpdate succeeds or refuses, one informational snapshot probes the
existing MySQL, broker and Search containers. Each role independently binds the
same source/run/nonce/image to its running immutable container ID before and
after execution. A fixed read-only SQL SELECT
connects over TCP to the existing `mysql` alias as root; the in-container shell
passes its existing password through `MYSQL_PWD` and discards stderr. No secret
is expanded into host arguments. The 10-second/64-byte capture accepts only one
ASCII row of four `0`/`1` values separated by TABs and followed by one LF:
wildcard-root account match, exact `datahub` schema presence, exact
`metadata_aspect_v2` table presence and networking enabled.

`mysqlBootstrap` records the source joins, verified container states and these
four booleans only after the post-execution binding succeeds. Unknown bindings,
stopped containers, command/timeout/budget errors and malformed output retain a
fixed refusal with no values, raw output or hash. False booleans remain valid
observations. The probe preserves the prior upgrade exception, stage and failure
evidence and cannot grant or deny acceptance. This is TCP/wildcard-account and
schema/table metadata evidence, not a JDBC connection from another container.
It collects no grant lists, row data, source files or logs and changes no SQL
state, credentials or runtime profile.

`brokerBootstrap` retains only the seven configured DataHub topic slots with
presence, partition and replica counts from a complete validated list. Absent
topics have null counts; malformed or refused lists have no values. Unknown
names never enter the receipt. The adjacent public v26.2.2
[topic-list summary](https://github.com/redpanda-data/redpanda/blob/fd30e45b31e0c5e9595e530121a0896e3ddd12a3/src/go/rpk/pkg/cli/topic/list.go#L114)
defines the JSON rows; actual selected v26.2.7 output must qualify that contract.
These summaries do not establish topic health or message acceptance. A separate
read-only Admin API query retrieves `storage_min_free_bytes` through the same
explicit localhost/profile-free seam. The
[scalar getter](https://github.com/redpanda-data/redpanda/blob/fd30e45b31e0c5e9595e530121a0896e3ddd12a3/src/go/rpk/pkg/cli/cluster/config/get.go#L63)
prints its YAML integer; only canonical nonnegative decimal plus one LF in the
unsigned 64-bit range passes. `sourceConfiguredDataTmpfsBytes=1073741824` labels
the committed fixture bound, not observed free space or supported capacity.
The storage guard and all resource limits remain unchanged. Topic retention
and offsets are not collected.

`searchBootstrap` uses filtered cluster stats and cluster health APIs over
loopback. It retains API-reported index, total-shard and primary-shard counts;
at index count zero, omitted shard fields remain null rather than fabricated
zeroes. These counts are not a complete inventory. The pinned
[stats serializer](https://github.com/opensearch-project/OpenSearch/blob/4ee42a94e87f66fbf1e62a9871b1b87f91e02472/server/src/main/java/org/opensearch/action/admin/cluster/stats/ClusterStatsIndices.java#L225)
omits shard totals when there are no indices. The
[health API](https://docs.opensearch.org/3.9/api-reference/cluster-api/cluster-health/)
supplies only the closed green/yellow/red status and node count; every color is
informational. No index names, documents, messages or logs are collected.

Broker and Search use independent per-query closed outcomes, so a refused
health or storage query cannot erase a valid stats or topic result. Each
command has a 10-second/64-KiB capture bound, with a 64-byte ceiling for the
scalar storage query. All values remain withheld if the final container
binding or running state fails. The shared observation wrapper restores the
prior stage/failure/runtime evidence, preserves the original upgrade exception
and gives no metadata value acceptance or cleanup authority.

A separate failed-upgrade reducer is narrowly scoped to the selected upgrade
image after a validated, completed, nonzero `upgradeOutcome`. It freshly binds
the exact source/run/nonce/network and full terminal container ID before and
after a dedicated `docker logs --tail 256` capture. It does not extend generic
`run`, readiness roles, log sources or the GMS/MySQL log permissions. Raw bytes
remain transient inside this trusted capture/reducer boundary; they never
return to its caller, print, persist, hash or enter retained exceptions.
Only fixed literal enum codes and per-line counts may leave the reducer.

The process has a 10-second/64-KiB capture bound and process-group kill/reap on
timeout or overflow, including when the immediate parent has already exited.
The separate `FAILED_UPGRADE_LINE_LIMIT=256` permits at most 256 logical lines
and 16 enum records; the readiness window remains 40 lines. Incomplete,
nonzero-client, malformed UTF-8, ANSI/NUL, unknown, over-budget or refused
observations retain no signals. Source-specific public literals cover compact
Java, native-link/Snappy, Spring, Kafka, Search, SQL/Ebean and confirmed index
step-failure categories. Matching retains no class/step names, paths, arbitrary
captures, bodies or unknown fingerprints. A match proves only public-literal
presence, not a caller, dependency version, fatal cause or supported capacity.
The configured Snappy default is not evidence of a native-codec failure.
The pinned upgrade Logback console format is plain text. Public failure reports
precede cleanup whose source loop is unbounded, so the source provides no
last-40-line failure guarantee. This does not establish that the earlier window
actually displaced a failure message. The fixed `upgrade-failure-report` enum
recognizes only the public `Failed Step` and `Caught exception during attempt`
fragments; no dynamic step IDs or details survive. Generic `Exiting upgrade`
does not match because it also appears on the success path. Empty output has a
distinct refusal from nonempty unclassified output.

The separate `FAILED_UPGRADE_STEP_CODES` catalogue associates 20 exact public
IDs with fixed aliases only inside the source-defined manager wrappers. A
complete required-step final report emits `required-final-*`; the caught-attempt
wrapper emits `caught-attempt-*`, which may describe a retry that later succeeds.
Canonical counters are bounded by Java integer/list limits, with positive final
ordinal/total and zero permitted for attempts and retries. Bare messages or the
pinned plain Logback prefix are supported, without retaining prefix fields,
counters, IDs or exception suffixes. No log level is interpreted as a fatal cause.
Optional/success messages, altered IDs, paths, cross-line fragments and unknown
wrappers receive no step association. All generic and step records still share
the same 16-record refusal ceiling.

Coverage is deliberately partial: the pinned DataHub core ID declarations and
eight exact default blocking MCP IDs joined by `BootstrapMCPStep` and packaged
`bootstrap_mcps.yaml` are included. There is no generic bootstrap/version match;
runtime overrides, optional/nonblocking and migration-specific IDs remain
unknown. This catalogue identifies public wrapper text, not the complete step
inventory, a loaded implementation or the precise failure cause.

Public token provenance includes the existing DataHub source revision
`faab576d96cbdb27b8344a755e0bc0f42fdc9c15`, expected-bundle JDK sources above,
[Spring's bean exception](https://github.com/spring-projects/spring-framework/blob/v6.2.15/spring-beans/src/main/java/org/springframework/beans/factory/BeanCreationException.java),
[Kafka's timeout exception](https://github.com/apache/kafka/blob/3.9.0/clients/src/main/java/org/apache/kafka/common/errors/TimeoutException.java)
and [OpenSearch's status exception](https://github.com/opensearch-project/OpenSearch/blob/4ee42a94e87f66fbf1e62a9871b1b87f91e02472/server/src/main/java/org/opensearch/OpenSearchStatusException.java).
These references define finite text categories, without establishing the loaded
versions in the upgrade image. The existing source-free host diagnostic method
checks secret-bearing, classlike, path, ANSI, invalid-encoding, duplicate and
budget vectors before registry authentication. A companion lifecycle control
denies wrong roles, zero/unknown outcomes and foreign IDs and checks capture
cleanup and original-failure preservation. The reducer cannot alter upgrade,
admission or cleanup decisions; the combined metadata snapshot remains in place.

### Fixture maintenance images

The disposable fixture selects official releases
[OpenSearch 3.9.0](https://github.com/opensearch-project/OpenSearch/releases/tag/3.9.0)
and Redpanda 26.2.7, plus Canonical's maintained MySQL distribution described
below. `images.json` selects their exact linux/amd64 platform
manifests; `image-descriptors.json` binds the independently read raw manifest and
config hashes, byte counts, media types and ordered rootfs DiffIDs. The three
DataHub image selections remain unchanged.

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
Clean-scan and DataHub runtime compatibility require the full workflow.
No jar or plugin is removed or replaced to obtain a scanner pass.

The disposable Search JVM supplies
`-Dorg.bouncycastle.native.cpu_variant=java` in `OPENSEARCH_JAVA_OPTS` before
provider initialization. The [official BC-FIPS publisher page](https://www.bouncycastle.org/download/bouncy-castle-java-fips/)
links the [2.1.3 source archive](https://repo1.maven.org/maven2/org/bouncycastle/bc-fips/2.1.3/bc-fips-2.1.3-sources.jar),
whose verified SHA-256 is
`9da8a0fa619d5e46ea321e2abf6b9e2c88d9aaa04cf8dbf80c4fec6943a40aad`.
`NativeLoader.java` lines 147–161 select the Java variant before temporary-file
setup, extraction or `System.load`. `Properties.java` lines 88–101 give a Java
Security property precedence over the System property; there is no environment
property lookup. `FipsStatus.java` lines 63–100 retain `Loader.run`, checksum
validation and error-state refusal, while `CryptoServicesRegistrar.java` lines
114–126 separately handle `approved_only`. This fixture setting changes neither
that policy nor any provider bytes, native production path or isolation limit.
The focused startup control checks the exact JVM option and Docker profile.
Public source establishes the option contract; the effective loaded setting,
startup compatibility and full catalog acceptance require the Linux workflow.
It establishes no cause for a prior startup failure or FIPS qualification.

Search also seeds `-Dopensearch.logs.base_path=/usr/share/opensearch/logs`
solely to flush buffered startup diagnostics to the existing Docker-log capture.
In the pinned public source, [OpenSearch.main](https://github.com/opensearch-project/OpenSearch/blob/4ee42a94e87f66fbf1e62a9871b1b87f91e02472/server/src/main/java/org/opensearch/bootstrap/OpenSearch.java#L93)
calls `errorPrintln` when that property is present;
[Terminal](https://github.com/opensearch-project/OpenSearch/blob/4ee42a94e87f66fbf1e62a9871b1b87f91e02472/libs/cli/src/main/java/org/opensearch/cli/Terminal.java#L172)
flushes the otherwise buffered error writer. The actual log directory comes
from [Environment](https://github.com/opensearch-project/OpenSearch/blob/4ee42a94e87f66fbf1e62a9871b1b87f91e02472/server/src/main/java/org/opensearch/env/Environment.java#L180),
and [LogConfigurator](https://github.com/opensearch-project/OpenSearch/blob/4ee42a94e87f66fbf1e62a9871b1b87f91e02472/server/src/main/java/org/opensearch/common/logging/LogConfigurator.java#L162)
overwrites the property before configuring logging. This observation setting is
neither a CLI switch nor a startup repair. An early suggested filename can be
misleading and proves no file exists or is readable; the fixture collects no
logfiles. It changes no parser, diagnostic budget or isolation limit and alone
identifies no startup failure cause.

Before the vendor launcher, a fixed Bash prelude copies the complete packaged
config tree into `/tmp/kanz-catalog-search-config` as UID 1000 under the existing
512 MiB temporary filesystem. Bash built-ins traverse explicit child entries,
including hidden entries, checking for links before descending and refusing
unreadable or unsearchable directories. No `find` or globstar traversal is used.
It rejects a linked source root, symlinks or special entries below it,
creates the private directory exclusively with mode
0700 and refuses any existing destination. The copy never dereferences links;
failed validation, discovery or copying aborts before Java. It preserves
relative files, exports `OPENSEARCH_PATH_CONF` and finally execs the original
vendor launcher with the same four `-E` arguments and JVM properties. No new
mount or executable temporary file is introduced; the image configuration stays
immutable and the existing read-only root, noexec tmpfs, caps and limits remain.

The pinned public [environment script](https://github.com/opensearch-project/OpenSearch/blob/4ee42a94e87f66fbf1e62a9871b1b87f91e02472/distribution/src/bin/opensearch-env#L103)
respects an existing config-directory override, and the
[launcher](https://github.com/opensearch-project/OpenSearch/blob/4ee42a94e87f66fbf1e62a9871b1b87f91e02472/distribution/src/bin/opensearch#L59)
passes it to both JVM-option parsing and Java. `Bootstrap.loadSecureSettings`
uses that directory for missing-key creation or an existing-key upgrade;
[KeyStoreWrapper.save](https://github.com/opensearch-project/OpenSearch/blob/4ee42a94e87f66fbf1e62a9871b1b87f91e02472/server/src/main/java/org/opensearch/common/settings/KeyStoreWrapper.java#L529)
writes an adjacent temporary file before atomic replacement. A sandbox control
exercises the exact prelude on synthetic trees, including stale state, linked
entries, directory-validation/copy failure and quoted argument forwarding.
The synthetic control exposes only `mkdir` and `cp` as external preparation
tools. This supported
relocation does not identify the precise refused file, operation or original
keystore state. Actual startup and full catalog acceptance still require Linux
runtime proof.

The selected Redpanda image records `libbrotli1:amd64` version
`1.1.0-2+deb13u1`, which [Debian DSA-6582-1](https://lists.debian.org/debian-security-announce/2026/msg00498.html)
identifies as fixing CVE-2026-111497. Its three Brotli library binaries and three
SONAME symlinks match the official fixed Debian package exactly across the
verified image layers. Its RPC advertisement and live-property requirements are
defined above. Redpanda's [patch-release policy](https://docs.redpanda.com/streaming/current/upgrade/rolling-upgrade/)
preserves user-facing behavior. These source and package checks do not establish
a clean scanner verdict or DataHub runtime compatibility. The unchanged full
six-image fixed-CRITICAL/Java scan and actual catalog workflow must qualify the
selected bytes before acceptance.

The disposable SQL store selects Canonical's maintained stable
[`ubuntu/mysql:8.4-26.04_stable`](https://hub.docker.com/r/ubuntu/mysql), pinned to
amd64 manifest `b22e8fc950df66a17d4b001b28ef5b557b9572d06da25f93967ea3f455c6410e`
and config `73b12c45d61f5005fe5252c2bbb65d75d5f0e5c3725ff0f97d25a35fa9169d4f`.
Read-only data inspection verified all five compressed layer hashes/sizes:
124,184,162 bytes and 7,668 entries. There were no gosu or certifi paths and no
`.whl`, `.egg` or `.zip` entries. The final Pebble binary's Go build metadata reports
1.27.1; the MySQL packages report `8.4.11-0ubuntu0.26.04.1`. The mysqld binary
contains that full version literal; runtime readback must match it exactly.
These payload observations do not establish a
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
enter receipts or exceptions. Hosted Linux qualification enforces these checks.

DataHub 1.8's
[pinned Connector/J 9.4.0](https://github.com/datahub-project/datahub/blob/faab576d96cbdb27b8344a755e0bc0f42fdc9c15/build.gradle#L364)
supports MySQL 8.0 and later according to the
[vendor release notes](https://dev.mysql.com/doc/relnotes/connector-j/en/news-9-4-0.html).
That supports this disposable maintenance choice; all six unchanged scanner
checks and all eight actual catalog cases remain mandatory. Pebble's embedded
VCS revision was not retrievable from the public source/module endpoints during
inspection. The publisher image bytes and matching helper/configuration are
verified; each run must verify the installed Pebble's exact nonroot/copy-once
behavior. This fixture does not change Kanz PostgreSQL,
broker authority or any held production/native runtime.

### Fixture-only ingestion package correction

The official 1.8.0 locked CLI base contains `linux-libc-dev` 6.8.0-147.147.
The fixture corrects only that package to 6.8.0-149.149;
`ingestion_patch/package-lock.json` pins the official Ubuntu
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
mode and limits. The exact hosted run must verify compatibility.
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
registry/package read does not prove execution or establish a runtime failure's cause.
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
Actual dependency readback must confirm the expected installed literal.

The upstream manifest/base image ID and derived local image ID have separate
receipt fields. Only the actual untagged derived ID is scanned and used for
CLI/test execution; the base digest is never described as the executed image.
All six runtime roles retain complete fixed-CRITICAL reports before advisory
refusal, so one image cannot hide later roles' findings. The scan now requests
advisory exit 42; only that exact code plus a complete, exact-image report may
continue to the next role. Infrastructure exit 1, report, database and resource
failures stop immediately even when advisory JSON exists. Informational DB logs
cannot mask code 42. This changes the discriminator, not severity, fixed-only or
Java policy. No service/network starts
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
