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
| Runbooks | **nothing** — see below |
| Schemas / APIs | the `risk-query-v1` API entity → the EVT-16 registry proto |
| Lineage / datasets | complemented by the LIN-01 lineage service + its DataHub catalog (`infra/catalog`) |

Backstage is the human index; the lineage service (LIN-01) and the schema
registry (EVT-16) remain the live sources of truth.

## Runbooks are NOT surfaced here, and were never rendered

Two Components carried `backstage.io/techdocs-ref: dir:../../docs/runbooks`.
TechDocs renders an mkdocs site, and that directory never contained an
`mkdocs.yml` — `git log --all --diff-filter=A` finds no such file anywhere in
this repository's history — so the annotation named a facet the portal could not
build. The table above claimed it worked.

The `docs/` tree was retired on 2026-08-26: runbooks now live in the README of
the component each one is about (`infra/observability/alerts/` for the generic
pages, `infra/nats/`, `infra/dr/`, `infra/tenancy/`, `services/autopilot/`,
`services/audit/`). The annotations were removed rather than repointed, because
repointing a pointer that never resolved would preserve the appearance of a
working integration.

Restoring runbooks in the portal needs a TechDocs source — an `mkdocs.yml` and a
docs tree it names — which is a decision about publishing, not a path edit.
