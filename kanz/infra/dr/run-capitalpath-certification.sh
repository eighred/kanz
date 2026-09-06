#!/usr/bin/env bash
# Issue #71: one real, capped order through the governed capital path.
# No exchange credential enters this process; venue adapters retain that boundary.
set -euo pipefail

namespace="${CAPITALPATH_NAMESPACE:-tenant-acme}"
job="kanz-capitalpath"
config="kanz-capitalpath-run"
gateway_secret="${CAPITALPATH_GATEWAY_SECRET:-kanz-capitalpath-gateway}"
ledger_secret="${CAPITALPATH_LEDGER_SECRET:-kanz-capitalpath-ledger}"

required=(
  KANZ_CAPITALPATH_IMAGE KANZ_TESTNET_ECR_REGISTRY CAPITALPATH_DR_ATTESTATION_FILE
  CAPITALPATH_GATEWAY_URL CAPITALPATH_ENVIRONMENT CAPITALPATH_PORTFOLIO
  CAPITALPATH_INSTRUMENT CAPITALPATH_VENUE CAPITALPATH_VENUE_ACCOUNT
  CAPITALPATH_POSTURE CAPITALPATH_QUANTITY CAPITALPATH_LIMIT_PRICE
  CAPITALPATH_MAX_NOTIONAL
  CAPITALPATH_GATEWAY_CIDR CAPITALPATH_LEDGER_CIDR
)
for name in "${required[@]}"; do
  if [[ -z "${!name:-}" ]]; then
    echo "$name is required" >&2
    exit 2
  fi
done
for cidr_name in CAPITALPATH_GATEWAY_CIDR CAPITALPATH_LEDGER_CIDR; do
  if [[ ! "${!cidr_name}" =~ ^[0-9]{1,3}(\.[0-9]{1,3}){3}/[0-9]{1,2}$ ]]; then
    echo "$cidr_name must be one explicit IPv4 CIDR (never 0.0.0.0/0)" >&2
    exit 2
  fi
  if [[ "${!cidr_name}" == "0.0.0.0/0" ]]; then
    echo "$cidr_name must not authorize the whole internet" >&2
    exit 2
  fi
done
if [[ ! "$KANZ_TESTNET_ECR_REGISTRY" =~ ^[0-9]{12}\.dkr\.ecr\.ap-northeast-1\.amazonaws\.com$ ]]; then
  echo "KANZ_TESTNET_ECR_REGISTRY must be one private ap-northeast-1 ECR registry" >&2
  exit 2
fi
image_prefix="${KANZ_TESTNET_ECR_REGISTRY}/kanz-capitalpath@"
image_digest="${KANZ_CAPITALPATH_IMAGE#"$image_prefix"}"
if [[ "$KANZ_CAPITALPATH_IMAGE" != "$image_prefix"* ]] || [[ ! "$image_digest" =~ ^sha256:[0-9a-f]{64}$ ]]; then
  echo "KANZ_CAPITALPATH_IMAGE must be the Tokyo ECR kanz-capitalpath image pinned by sha256 digest" >&2
  exit 2
fi
if [[ ! -r "$CAPITALPATH_DR_ATTESTATION_FILE" ]]; then
  echo "CAPITALPATH_DR_ATTESTATION_FILE is not readable" >&2
  exit 2
fi
if [[ "$(kubectl get namespace "$namespace" -o jsonpath='{.metadata.labels.kanz\.internal/spiffe}')" != "enabled" ]]; then
  echo "namespace $namespace is not opted into SPIFFE issuance" >&2
  exit 2
fi
kubectl -n "$namespace" get secret "$gateway_secret" >/dev/null
kubectl -n "$namespace" get secret "$ledger_secret" >/dev/null
if kubectl -n "$namespace" get job "$job" >/dev/null 2>&1; then
  echo "job/$job already exists; preserve its logs as evidence or remove it deliberately before another run" >&2
  exit 2
fi

# Configuration is non-secret and replaceable. The bearer and DSN are never
# rendered into this script's output or a ConfigMap; they remain Secret volumes.
kubectl -n "$namespace" create configmap "$config" \
  --from-literal=CAPITALPATH_POSTURE="$CAPITALPATH_POSTURE" \
  --from-literal=CAPITALPATH_GATEWAY_URL="$CAPITALPATH_GATEWAY_URL" \
  --from-literal=CAPITALPATH_ENVIRONMENT="$CAPITALPATH_ENVIRONMENT" \
  --from-literal=CAPITALPATH_TENANT="acme" \
  --from-literal=CAPITALPATH_PORTFOLIO="$CAPITALPATH_PORTFOLIO" \
  --from-literal=CAPITALPATH_INSTRUMENT="$CAPITALPATH_INSTRUMENT" \
  --from-literal=CAPITALPATH_VENUE="$CAPITALPATH_VENUE" \
  --from-literal=CAPITALPATH_VENUE_ACCOUNT="$CAPITALPATH_VENUE_ACCOUNT" \
  --from-literal=CAPITALPATH_QUANTITY="$CAPITALPATH_QUANTITY" \
  --from-literal=CAPITALPATH_LIMIT_PRICE="$CAPITALPATH_LIMIT_PRICE" \
  --from-literal=CAPITALPATH_MAX_NOTIONAL="$CAPITALPATH_MAX_NOTIONAL" \
  --from-literal=CAPITALPATH_NATS_URL="${CAPITALPATH_NATS_URL:-nats://nats.kanz-messaging.svc:4222}" \
  --from-literal=CAPITALPATH_SPIFFE_SOCKET="unix:///run/spiffe/spire-agent.sock" \
  --from-literal=CAPITALPATH_TIMEOUT="${CAPITALPATH_TIMEOUT:-5m}" \
  --from-file=dr.json="$CAPITALPATH_DR_ATTESTATION_FILE" \
  --dry-run=client -o yaml | kubectl apply -f -

kubectl apply -f - <<YAML
apiVersion: v1
kind: ServiceAccount
metadata:
  name: kanz-capitalpath
  namespace: ${namespace}
---
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: kanz-capitalpath-egress
  namespace: ${namespace}
spec:
  podSelector:
    matchLabels: { app: kanz-capitalpath }
  policyTypes: [Egress]
  egress:
    - to:
        - namespaceSelector:
            matchLabels: { kubernetes.io/metadata.name: kanz-messaging }
          podSelector:
            matchLabels: { app: nats }
      ports: [{ protocol: TCP, port: 4222 }]
    - to:
        - namespaceSelector: {}
          podSelector:
            matchLabels: { k8s-app: kube-dns }
      ports: [{ protocol: UDP, port: 53 }, { protocol: TCP, port: 53 }]
    # Exact operator-supplied ranges only. The runner never gets an open
    # internet route, and the DSN itself is held in the Secret volume below.
    - to: [{ ipBlock: { cidr: ${CAPITALPATH_GATEWAY_CIDR} } }]
      ports: [{ protocol: TCP, port: 443 }]
    - to: [{ ipBlock: { cidr: ${CAPITALPATH_LEDGER_CIDR} } }]
      ports: [{ protocol: TCP, port: 5432 }]
---
apiVersion: batch/v1
kind: Job
metadata:
  name: ${job}
  namespace: ${namespace}
  labels: { app.kubernetes.io/part-of: kanz }
spec:
  backoffLimit: 0
  activeDeadlineSeconds: 420
  template:
    metadata:
      labels: { app: kanz-capitalpath }
    spec:
      restartPolicy: Never
      serviceAccountName: kanz-capitalpath
      containers:
        - name: certifier
          image: ${KANZ_CAPITALPATH_IMAGE}
          envFrom:
            - configMapRef: { name: ${config} }
          env:
            - { name: CAPITALPATH_GATEWAY_TOKEN_FILE, value: /run/secrets/gateway/token }
            - { name: CAPITALPATH_GATEWAY_SIGNING_SECRET_FILE, value: /run/secrets/gateway/signing-secret }
            - { name: CAPITALPATH_LEDGER_DSN_FILE, value: /run/secrets/ledger/dsn }
            - { name: CAPITALPATH_DR_ATTESTATION_FILE, value: /run/config/dr.json }
          volumeMounts:
            - { name: gateway, mountPath: /run/secrets/gateway, readOnly: true }
            - { name: ledger, mountPath: /run/secrets/ledger, readOnly: true }
            - { name: run-config, mountPath: /run/config, readOnly: true }
            - { name: spiffe, mountPath: /run/spiffe, readOnly: true }
          securityContext:
            runAsNonRoot: true
            runAsUser: 65532
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities: { drop: ["ALL"] }
      volumes:
        - name: gateway
          secret: { secretName: ${gateway_secret}, items: [{ key: token, path: token }, { key: signing-secret, path: signing-secret }] }
        - name: ledger
          secret: { secretName: ${ledger_secret}, items: [{ key: dsn, path: dsn }] }
        - name: run-config
          configMap: { name: ${config}, items: [{ key: dr.json, path: dr.json }] }
        - name: spiffe
          csi: { driver: csi.spiffe.io, readOnly: true }
YAML

kubectl -n "$namespace" wait --for=condition=complete "job/$job" --timeout=420s || {
  kubectl -n "$namespace" logs "job/$job" >&2 || true
  exit 1
}
kubectl -n "$namespace" logs "job/$job"
