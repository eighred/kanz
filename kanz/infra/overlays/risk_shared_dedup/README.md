# Optional risk dedup component

This unselected Kustomize component prepares one dedicated risk-engine Valkey
endpoint. No canonical ApplicationSet, base deployment or Tokyo overlay imports
it. It creates no Valkey workload, grants no new Vault policy, pins no unverified
image and changes no active estate. #111 owns consumer qualification; #1541 owns
actual retained-state migration and rollback.

Before selection, qualify #1753's logical-dispatch namespace repair. A shared raw
event key can suppress independently required sharded groups. A risk-only prefix
does not replace that shared bus boundary. Also qualify the actual source's two
instances, canonical NATS, Valkey 9.1.3, restart, outage/recovery and idempotent
financial state. The optional component is not acceptance for those conditions.

Build the source with the repository-root context and explicitly select
`--target shared-dedup` in `kanz/services/risk_engine/Dockerfile`. A normal build
selects the final `default` target and excludes the Redis adapter. Existing image
builds and release selection remain unchanged. Record and verify the resulting
immutable image digest through the existing release gates; the caller must bind
that actual qualified digest when importing this component. There is no invented
digest or fallback to a mutable tag here. The default binary refuses a declared
shared endpoint before opening HTTP or broker connections.

Provision and independently verify a dedicated authenticated/TLS endpoint for
this single-tenant service, with reviewed per-consumer key permissions and bounded
timeouts. Endpoint URLs select address/authentication/database only; query and
fragment options refuse startup. The client owns fixed one-second dial/read/write/
pool budgets and disables SDK retries. These socket budgets are not an absolute
OS wall-clock limit for dispatch. Its workload selector is `app: valkey-risk-dedup` in `kanz-messaging`;
the component grants only TCP 6379 between that selector and risk-engine. Existing
deny policies remain necessary. Do not point this optional consumer at an
unreviewed shared credential, general administration endpoint or session store.

The separate Vault role `risk-engine-dedup` must bind only the existing
`risk-engine` ServiceAccount in `kanz-services` and read only
`kv/data/kanz/risk-engine-dedup`. That dedicated record contains only the `url`
field; it must not contain unrelated credentials or grant migration authority.
The CSI class projects only that field, read-only, into the risk-engine runtime.
No credential value belongs in this component, a command line, logs or evidence.
Use a `rediss://` URL with a trust chain accepted by the qualified image; dependency
availability and the URL file alone do not prove TLS/authentication or ACLs.

Add the component to a separately reviewed caller only after its exact image,
identity, endpoint, network policy and rollback have been qualified. Retain the
previous image/configuration and Valkey state/expiry evidence before any selection.
Rollback removes the component and restores that image/configuration together;
handler idempotency remains mandatory in both modes. This source preparation
authorizes no deployment, broker administration, secret rotation or trade.
