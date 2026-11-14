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
image pins the CLI and its transitive environment. The broker is disposable
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
stage/category, never subprocess arguments or one-run credentials. Nine
qualification controls discriminate these failures alongside the 14 projection
controls.
The PR/main trigger contract includes the consumed authorization/lineage
dependencies, canonical generation inputs, Go runtime pins, setup action and
reviewed advisory installer. A control reads the real workflow through a
read-only mount and rejects omitted scanner/auth/schema selectors.
Required runtime cases cannot skip. Normal runs without the disposable catalog
skip the two externally gated tests; the dedicated workflow proves them.
Runtime logs, command arguments and generated credentials are never uploaded.
Containers and their ephemeral state are removed on exit.

Repository/fixture acceptance is distinct from production catalog activation,
source rights, complete schema-registry integration or capital approval. Close
[#1405](https://github.com/eighred/kanz/issues/1405) only after the exact merged
main workflow and its full projected receipt are verified.

Selected upstream source: [DataHub v1.8.0](https://github.com/datahub-project/datahub/tree/v1.8.0),
including its metadata file source, REST sink and GMS system authenticator.
