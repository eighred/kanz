# Repository and operator tooling

These tools span Kanz components or operate a host/disposable qualification
environment. Go-module-local tools remain under `kanz/tools/`; service-specific
conformance tools remain with their service. A script's presence does not
authorize its infrastructure or financial effects.

Start at a tool's component README and `--help`/parameter declaration. Operator
PowerShell scripts require the explicitly selected estate and authority described
by their owning issue/runbook. Do not use production credentials for verification
or print them while diagnosing configuration.

| Area | Entry guidance |
|---|---|
| Documentation integrity | `python -B tools/check_readmes.py --inventory` checks local links and headings in READMEs, contributor guides and Kanz Forge instructions. |
| Disposable messaging/workload identity | [Feed identity](feed_identity/README.md), [Redpanda identity](redpanda_identity/README.md), [NATS GitOps](nats_gitops_proof/README.md). |
| Browser/network and observability qualification | [Gateway policy](gateway_policy_proof/README.md), [Grafana](grafana_proof/README.md), [Tokyo messaging policy](tokyo_messaging_proof/README.md). |
| Image and release evidence | [Rust audit tooling](rust_audit_tools/README.md), [signed promotion](signed_promotion/README.md). |
| Retained ephemeral-state transition | [Valkey migration](valkey_migration/README.md). |

Qualification fixtures create their own environments where documented. An
operator wrapper targeting an existing estate is a different boundary. Preserve
their scope, refusal controls, pinned inputs and cleanup ownership during moves.
Local source tests do not establish rollout or capital-path acceptance.

Run the README checker regressions with
`python -B -m unittest discover -s tools -p test_check_readmes.py -v`.
CI and component runbooks select each tool's other checks. Follow
[CONTRIBUTING.md](../CONTRIBUTING.md) for placement and evidence requirements.
