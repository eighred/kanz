# Datamaster listener isolation

| Listener | Kubernetes port | Intended caller |
|---|---:|---|
| Metrics, readiness and liveness | 8080 | Monitoring and kubelet |
| Tenant API, including override and approval | 8111 | API gateway |
| GET /v1/securities/{id} only | 8112 | OMS, compliance and risk-engine classifiers |

The three sockets bind before service readiness. A bind conflict refuses startup.
A tenant header is still required on classifier reads. The classifier cannot
authenticate an override, even if it sends gateway-shaped headers.

## Controlled rollout and verification (#232)

Coordinate the binary, Service, both traffic directions and caller addresses in
a maintenance window. Old pods serve the API on 8080: do not certify isolation
while any remain. Clients cut over before new endpoints are ready will fail
closed or lose classification availability. Drain callers before replacement
where that interruption is unacceptable. Do not restore API traffic on the
scrape port as a fallback. If reverting, isolate old pods from monitoring before
restoring the old configuration; a binary-only rollback reopens the exposure.

On an authorized enforcing-CNI test cluster, capture the rendered policies,
pod labels, image revision and endpoints. Verify monitoring gets /metrics:8080
and 404 for /v1/exceptions on that port. From the same monitoring pod, a TCP
connection to 8111 or 8112 must fail; a 401/403 response is already evidence
of unwanted network reachability. From the gateway, use an authorized test
tenant and GET /v1/exceptions on 8111. From each classifier, verify a fixture
lookup on 8112 and denied TCP access to 8111. An override path on 8112 must
return 404. Use known healthy endpoints as positive controls before interpreting
timeouts. Keep the broader issue open until live evidence is recorded.
