# tv-sync HTTP listeners

The Broker API is served on `TV_SYNC_API_LISTEN` (default `:8113`). Only the
API gateway may reach this port: the handlers trust its injected
`X-Kanz-Principal-*` identity headers. NetworkPolicy is part of that trust
boundary; the listener itself does not authenticate a gateway workload.

`TV_SYNC_LISTEN` (default `:8091`) serves only `/healthz`, `/readyz` and
`/metrics`. Monitoring must use this listener. Broker routes are absent from it,
and operational routes are absent from the API listener. Both sockets must bind
before startup can report ready, and both drain on shutdown. Existing fact-log
rehydration and consumer readiness semantics are otherwise unchanged.

## Rollout and verification

The Deployment is a singleton with `Recreate`; preserve that constraint. A
listener migration requires a rebuilt, approved image containing the split,
the two Service ports, gateway URL, and base/overlay NetworkPolicies together.
The checked-in image digest is not evidence that this source change is deployed.
Applying the new gateway port to an old binary causes Broker API unavailability.
Use a controlled maintenance window, and retain the matching prior image and
manifest revision for rollback. Rolling back restores the old shared-listener
exposure and must not be described as isolated.

Before certifying #232 on an enforcing-CNI test cluster, verify:

- The gateway reaches representative authorized Broker API reads on 8113;
  absent identity is refused and tenant scoping remains intact.
- Monitoring reaches metrics and probes on 8091, but Broker routes on that
  listener return 404 even with forged principal headers.
- Monitoring and an unrelated pod cannot connect to 8113, including by pod IP.
- Restart preserves fact-log recovery and the existing single-consumer topology;
  no second replica is introduced as a rollout shortcut.

Local HTTP tests and static policy mutation tests cover the source boundary.
They do not establish live CNI enforcement, published-image custody or production
acceptance; #232 remains open until its remaining evidence is recorded.
