# Data catalog (LIN-01b)

An optional DataHub ingestion template over Kanz datasets. The intended catalog
combines lineage, schema, ownership and PII classification; the mappings below
still need deployment verification ([#1405](https://github.com/eighred/kanz/issues/1405)).
The catalog is a human-facing index; the `lineage` service (LIN-01a/c/d) owns
lineage queries and governance enforcement.

## What a dataset is

One catalog dataset per `(domain, payload message type)` — identity
`kanz.{domain}` / `{MessageType}` (e.g. `kanz.risk` / `ExposureSet`). This is the
*same* identity the lineage graph (LIN-01a) and the lake-sink tables (LAKE-01a)
key on, so lineage, lakehouse tables, and the catalog all line up.

## The four facets, and where each comes from

| Facet | Source |
|---|---|
| **Lineage** (upstream/downstream) | OpenLineage RunEvents the lineage service emits (LIN-01a) → Marquez/DataHub OpenLineage receiver |
| **Schema** | Intended EVT-16 registry join via `payloadSchemaRef`; the template does not configure a separate registry ingestion source. |
| **Ownership** | Explicit DataHub steward mapping in `datahub-recipe.yaml`; the current template assigns `event-platform`. Repository review routing in `.github/CODEOWNERS` is separate and is not imported automatically. |
| **Classification** | The template adds a static PII tag to every ingested dataset; it does not load LIN-01c governance config. Domain-specific classification needs a validated mapping. The governance config remains the enforcement source of truth. |

## Files

- `datahub-recipe.yaml` — the ingestion recipe (OpenLineage source + ownership /
  PII-tag transformers + DataHub REST sink). Run `datahub ingest -c datahub-recipe.yaml`.

## Why DataHub over hand-rolling

Lineage UIs and catalogs speak OpenLineage natively; the lineage service already
emits the standard (LIN-01a), so the catalog is mostly configuration. The
catalog never enforces access — that stays in the lineage service's governance
layer (LIN-01c), which is in the request path; the catalog only *surfaces* the
classification so a steward can see what is sensitive.
