# Backstage portal (DEVX-01c)

Catalog and scaffolder templates for an optional internal developer portal.
They describe service ownership and schemas and call the same service generator
as the CLI. A registered catalog is an index, not operational acceptance.

## Files

- `catalog-info.yaml` — the software catalog: the `kanz` System, one Component
  per service (owner and source links), and the `risk-query-v1` API.
  Register it via Backstage `catalog.locations`.
- `template.yaml` — the "New Kanz service" software template. It runs the
  DEVX-01b scaffold generator and opens a PR, so the portal and the CLI produce
  the *same* skeleton (one source of truth).

## What it surfaces

| Facet | Source |
|---|---|
| Services + ownership | `catalog-info.yaml` Component owners; these catalog groups are separate from GitHub review routing in `.github/CODEOWNERS` |
| Runbooks | **nothing** — see below |
| Schemas / APIs | the `risk-query-v1` API entity → the EVT-16 registry proto |
| Lineage / datasets | complemented by the LIN-01 lineage service + its DataHub catalog (`infra/catalog`) |

Backstage is the human index; the lineage service (LIN-01) and the schema
registry (EVT-16) remain the live sources of truth.

## Setup and limits

This directory supplies catalog and scaffolder templates, not a bundled or
verified Backstage server. Register `catalog-info.yaml` and `template.yaml` in
an existing approved Backstage deployment. The template's `run:command` action
must be installed and allowed by that deployment; source alone does not prove
portal execution or PR publishing authority.

The generator runs in the `kanz/` Go module and maps a deployed `my-service`
identity to `services/my_service/cmd/my_service/`. Test it independently with
`go test -p 1 ./tools/scaffold/... -count=1` from `kanz/`.

Component runbooks remain in their READMEs. There is no configured TechDocs
site or rendered runbook integration here; restoring it needs a real publishing
owner and source, not an annotation pointing at a nonexistent docs tree.
