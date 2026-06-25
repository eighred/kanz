# Data catalog (LIN-01b)

A DataHub catalog over the Kanz datasets: every event type that flows on the bus
is a dataset, stitched with its lineage, schema, ownership, and PII
classification. The catalog is the human-facing index; the `lineage` service
(LIN-01a/c/d) is the live source of truth.

## What a dataset is

One catalog dataset per `(domain, payload message type)` — identity
`kanz.{domain}` / `{MessageType}` (e.g. `kanz.risk` / `ExposureSet`). This is the
*same* identity the lineage graph (LIN-01a) and the lake-sink tables (LAKE-01a)
key on, so lineage, lakehouse tables, and the catalog all line up.

## The four facets, and where each comes from

| Facet | Source |
|---|---|
| **Lineage** (upstream/downstream) | OpenLineage RunEvents the lineage service emits (LIN-01a) → Marquez/DataHub OpenLineage receiver |
| **Schema** | EVT-16 schema registry, joined via the dataset's `payloadSchemaRef` facet |
| **Ownership** | `kanz-schemas/CODEOWNERS` — the schema owner is the dataset owner |
| **Classification** | LIN-01c governance config (PII tags) — DataHub is the read view, the governance config is the enforcement source of truth |

## Files

- `datahub-recipe.yaml` — the ingestion recipe (OpenLineage source + ownership /
  PII-tag transformers + DataHub REST sink). Run `datahub ingest -c datahub-recipe.yaml`.

## Why DataHub over hand-rolling

Lineage UIs and catalogs speak OpenLineage natively; the lineage service already
emits the standard (LIN-01a), so the catalog is mostly configuration. The
catalog never enforces access — that stays in the lineage service's governance
layer (LIN-01c), which is in the request path; the catalog only *surfaces* the
classification so a steward can see what is sensitive.
