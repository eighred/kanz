# Focused Kanz security review

Use this local guide for a Kanz code change in guidance mode. The parent
security skill owns audit modes, isolation, validation and reporting. This guide
selects concrete source questions; it does not relax that policy or authorize
live probes. Repository paths below are locators from the checkout root: inspect
their current implementation, not just the filename.

## Establish a reachable violation

Write one short hypothesis: which untrusted principal/input can reach which
resource, through which missing or incorrect check, with what consequence?
Trace registration and deployment wiring before calling a helper reachable.
Inspect the allowed case and at least one refused case. Identify whether the
check happens before the side effect, and whether errors/unknown state preserve
the refusal. A framework name or an unused validation function is not a control.

| Boundary | Follow in current source | Failure to investigate |
|---|---|---|
| Gateway identity | `kanz/services/api-gateway/internal/middleware/`, `kanz/services/api-gateway/internal/authz/`, upstream proxy and NetworkPolicy | Forged principal headers, route registered outside auth, missing resource/tenant scope, revocation not checked |
| Tenant data | Service query/store, migration/RLS, pool transaction setup; `kanz/test/arch/tenant_rls_coverage_test.go` | Cross-tenant lookup, missing FORCE RLS, privileged test role bypass, pooled-session leakage |
| Commands/events | Producer, envelope validator, consumer, outbox and durable idempotency key | Tenant mismatch, replayed effect, commit/publish gap, conflicting duplicate accepted as new |
| Admission/execution | Risk/compliance/mandate → OMS → typed venue adapter; `kanz/test/arch/every_admission_leaves_a_record_test.go` | Unaccounted permit, critical unknown coerced to success, ambiguous remote outcome blindly retried |
| Agent/read surface | `kanz/services/mcp/`, domain authorization and projection; `kanz/test/arch/mcp_is_a_read_plane_test.go` | Tool discovery exposes another tenant, mutation through a read route, source text treated as tool authority |
| Release/configuration | Workflow trigger/token scope, checkout/ref, artifact digest and installer; `kanz/test/arch/a_release_refuses_a_mutable_tag_test.go` | Untrusted PR code reaches credentials, mutable artifact accepted, tested bytes differ from applied bytes |

Select the corresponding imported companion through
[the attack-class index](../ATTACK-CLASSES.md). Do not read all companions for
one local change. Check indirect callers and configuration outside the diff
before concluding that a boundary is unaffected.

## Compliance means a policy actually gates behavior

For a policy-related change, locate the policy version, principal, tenant,
resource, effective time, decision owner and audit record. Trace the enforcement
point and prove the denied action cannot occur before that point or through an
alternate route. Logging a violation after execution is not admission control.
Verify stale/missing policy and evaluation errors according to the owner's
fail-closed contract; preserve the distinction between rejected and unknown.
Do not invent legal requirements or certify regulatory compliance from tests.
An unspecified jurisdiction, policy owner or interpretation is a missing input
for the policy decision, not authority for the agent to supply its own rule.

## Return a finding that can be acted on

State the exact revision, entrypoint and violated boundary, affected principal
and resource, source locations, preconditions, expected/observed behavior,
smallest repair and discriminating regression check. Separate confirmed source
facts from inferred reachability and runtime behavior needing validation.
If evidence disproves the hypothesis, say which control defeats it; do not turn
an unfamiliar pattern into a vulnerability. If reproduction is unsafe or lacks
the mandated sandbox, retain the hypothesis as needs-validation with the precise
missing proof. Offline validator tests only validate report tooling.
