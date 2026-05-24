# SPIRE — workload identity issuance + rotation (SEC-01a)

Issues every Kanz workload a short-lived, auto-rotated **X.509-SVID** keyed on
its Kubernetes ServiceAccount. This is the root of the zero-trust transport
work: SEC-01b's mTLS helpers (`pkg/transport/tls.go`) fetch SVIDs from the
agent's Workload API and assert peer SPIFFE IDs; SEC-01c puts TLS/SASL on
NATS/Kafka; AUTH-01c binds the SPIFFE ID to the command issuer.

Chosen over cert-manager because the requirement is **workload identity** (a
verifiable per-service identity with automatic, no-human rotation), not just
certificate provisioning. SPIRE attests *what* a workload is (node + pod
attestation) before issuing; cert-manager would issue to whoever holds the
right secret.

## Trust domain & identity

- Trust domain: `kanz.internal`
- SPIFFE ID: `spiffe://kanz.internal/ns/<namespace>/sa/<service-account>`

## Layout

| File | Purpose |
|---|---|
| `namespaces.yaml` | `spire-system` (control plane) + `kanz-services` (workloads); labels `kanz-messaging` is opted in from `infra/nats` |
| `rbac.yaml` | ServiceAccounts + ClusterRoles: node attestation (TokenReview), workload discovery, bundle ConfigMap write |
| `spire-server.yaml` | SPIRE server StatefulSet (CA + datastore) with the controller-manager sidecar; server/CA config |
| `spire-agent.yaml` | Per-node agent DaemonSet + SPIFFE CSI driver + node-driver-registrar; `CSIDriver` object |
| `registration.yaml` | `ClusterSPIFFEID` — declarative issuance to every pod in a `kanz.internal/spiffe: enabled` namespace |

## Rotation (the point of SEC-01a)

| Knob | Value | Effect |
|---|---|---|
| `ca_ttl` | 24h | intermediate CA rotates daily; agents track the new bundle |
| `default_x509_svid_ttl` | 1h | workload leaf certs; go-spiffe re-fetches at ~30m half-life |
| `default_jwt_svid_ttl` | 5m | JWT-SVIDs for future token auth (AUTH-01) |

A compromised leaf is valid for ≤1h and **no human ever touches a cert** — the
agent renews continuously and SEC-01b's client reloads on the Workload API
stream. Short by design.

## Consuming an SVID from a workload

Mount the CSI volume; the agent socket appears at the mount path and go-spiffe
dials it:

```yaml
volumes:
  - name: spiffe
    csi:
      driver: csi.spiffe.io
      readOnly: true
containers:
  - name: app
    volumeMounts:
      - { name: spiffe, mountPath: /run/spiffe, readOnly: true }
    env:
      - name: SPIFFE_ENDPOINT_SOCKET
        value: unix:///run/spiffe/spire-agent.sock
```

## Deploy

```sh
kubectl apply -f namespaces.yaml -f rbac.yaml
kubectl apply -f spire-server.yaml
kubectl -n spire-system rollout status statefulset/spire-server
kubectl apply -f spire-agent.yaml
kubectl -n spire-system rollout status daemonset/spire-agent
kubectl apply -f registration.yaml          # needs the CRDs (below)
```

The `ClusterSPIFFEID` / `ControllerManagerConfig` CRDs ship with
spire-controller-manager; apply them once per cluster from the pinned release
(`github.com/spiffe/spire-controller-manager` `config/crd`) before
`registration.yaml`.

## Verify

```sh
# Registration entries created from the ClusterSPIFFEID:
kubectl -n spire-system exec statefulset/spire-server -c spire-server -- \
  /opt/spire/bin/spire-server entry show

# A workload's rotating SVID:
kubectl -n kanz-services exec deploy/<svc> -- \
  /opt/spire/bin/spire-agent api fetch x509 -socketPath /run/spiffe/spire-agent.sock
```

## Bootstrap scope (follow-ups)

Single-replica server on a sqlite PVC — mirrors the NATS bootstrap cluster
posture. HA needs the `postgres` DataStore (reuse the PERS-01 / schema-registry
cluster) + ≥2 server replicas, and a KMS-backed `UpstreamAuthority` for the root
key (SEC-01d). Federation across trust domains is out of scope until there's a
second domain.
