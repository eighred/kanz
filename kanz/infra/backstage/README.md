# Backstage portal (DEVX-01c)

The internal developer portal: a single place to find every Kanz service, its
owner, its runbooks, and its schemas — and to create a new service on-convention.

## Files

- `catalog-info.yaml` — the software catalog: the `kanz` System, one Component
  per service (owner + source + TechDocs links), and the `risk-query-v1` API.
  Register it via Backstage `catalog.locations`.
- `template.yaml` — the "New Kanz service" software template. It runs the
  DEVX-01b scaffold generator and opens a PR, so the portal and the CLI produce
  the *same* skeleton (one source of truth).

## What it surfaces

| Facet | Source |
|---|---|
| Services + ownership | `catalog-info.yaml` Components (owners track CODEOWNERS) |
| Runbooks | TechDocs over `docs/runbooks` (the AUTO-01 + DR + SLO runbooks) |
| Schemas / APIs | the `risk-query-v1` API entity → the EVT-16 registry proto |
| Lineage / datasets | complemented by the LIN-01 lineage service + its DataHub catalog (`infra/catalog`) |

Backstage is the human index; the lineage service (LIN-01) and the schema
registry (EVT-16) remain the live sources of truth.
