#!/usr/bin/env bash
# SEC-M3a — stand up a NATS broker configured EXACTLY as production configures it,
# so the mTLS contract is proven by execution rather than asserted by YAML.
#
# WHY THIS EXISTS. infra/nats/nats.yaml sets `tls { verify: true, verify_and_map:
# true }` on the client listener: a client must present an SVID chaining to the
# trust bundle, and its SPIFFE URI SAN maps to a NATS user + account. CI, however,
# ran `nats:2 -js` — no TLS at all — so every green build tested against a broker
# configured unlike production in the ONE dimension that decides whether a
# connection is possible. That is how the platform came to have zero DialNATS call
# sites setting TLSConfig: nothing could see it.
#
# THE CONFIG IS EXTRACTED, NEVER COPIED. nats.conf and tenants.conf are pulled out
# of the ConfigMaps production applies, exactly as kanz-ci.yml extracts bootstrap.sh
# from infra/nats/bootstrap-job.yaml. A copy here would drift from the real thing,
# and the drift would be invisible until a service met a real broker.
#
# THE BROKER ALONE IS NOT ENOUGH, which is what this script used to stop at. A
# JetStream broker with no streams accepts a connection, authorizes the publish,
# and then answers "no response from stream" — so the mTLS round-trip test failed
# in a way that reads like a transport fault and is not one. Step 4 therefore runs
# PRODUCTION'S OWN bootstrap script, extracted from bootstrap-job.yaml, over mTLS.
# That was always the intent: the DNS SANs minted below exist precisely so the
# non-SPIFFE-aware `nats` CLI can verify the broker, and NATS_REPLICAS exists in
# that script precisely so CI can run it against one node. Only the wiring was
# missing.
#
# WHAT IS OVERRIDDEN, AND WHY (the honest list — everything else is verbatim):
#   1. The `cluster {}` block is stripped. It routes to nats-0/1/2.nats-headless
#      and puts JetStream in CLUSTERED mode, which needs a 3-node quorum. On one
#      node the server boots, logs "Waiting for routing to be established..."
#      forever, and JetStream never becomes usable. Verified by running it.
#   2. $POD_NAME is supplied (server_name expands it; k8s injects it in prod).
#   3. SVIDs are minted by a throwaway CA here instead of SPIRE. The certs SPIRE
#      issues differ in provenance, not in shape: same trust domain, same URI SAN,
#      same SPIFFE-conformant key usage. This mints no production credential and
#      is built into no image — the cmd/kanz-devtoken stance.
#   4. NATS_REPLICAS=1 for the bootstrap. Every stream production creates is
#      replicas=3; a single-node JetStream refuses that outright. The script
#      already parameterises it for exactly this reason — see its own comment.
#
# The TLS block and the accounts (tenants.conf) — the two things under test — are
# used exactly as shipped, and so is the bootstrap script.
#
# Usage: test/mtls/up.sh [outdir] [port]   (defaults /tmp/kanz-mtls, 4222)
#   Then: TEST_NATS_MTLS_URL=nats://localhost:<port> TEST_NATS_MTLS_CERT_DIR=<outdir> go test ./pkg/bus/
#   Tear down: docker rm -f kanz-mtls-nats && docker network rm kanz-mtls-net
#
# The port is a parameter because CI runs this BESIDE the plaintext broker the
# rest of the suite uses: the TEST_NATS_URL tests prove bus semantics, these prove
# the production TRANSPORT contract, and neither substitutes for the other.
set -euo pipefail

OUT="${1:-/tmp/kanz-mtls}"
PORT="${2:-4222}"
CONTAINER="kanz-mtls-nats"
NETWORK="kanz-mtls-net"
TRUST_DOMAIN="kanz.internal"
# The SPIFFE IDs infra/security/spire/registration.yaml would issue these pods:
#   spiffeIDTemplate: spiffe://kanz.internal/ns/{namespace}/sa/{serviceAccountName}
SERVER_ID="spiffe://${TRUST_DOMAIN}/ns/kanz-messaging/sa/nats"
CLIENT_ID="spiffe://${TRUST_DOMAIN}/ns/kanz-services/sa/risk-engine"
# A SECOND client identity, because the production permission model makes one
# insufficient. tenants.conf grants risk-engine publish on its two output FACTs
# and subscribe on its three inputs — an EMPTY intersection, deliberately: a
# service emits what it produces and consumes what it needs, and never round-trips
# its own traffic. So a publish->consume proof needs two SVIDs, exactly as
# production does it. archiver is the real consumer of risk.portfolio.> (it holds
# subscribe on that prefix plus $JS.API.>/$JS.ACK.>), so this pair is a genuine
# service relationship rather than a test fixture.
CONSUMER_ID="spiffe://${TRUST_DOMAIN}/ns/kanz-services/sa/archiver"
# A THIRD identity: the bootstrap Job's. tenancy.yaml maps this SVID to the one
# account permitted to create streams ("it CREATES the streams, so it..."), and
# no service identity holds that. Provisioning the topology as risk-engine would
# either fail or — worse, if tenants.conf were ever widened to let it through —
# prove that a service can rewrite the spine's shape.
BOOTSTRAP_ID="spiffe://${TRUST_DOMAIN}/ns/kanz-messaging/sa/nats-bootstrap"
# A FOURTH AND FIFTH identity, for the MT-02 tenant bridge (#358). These two are
# the whole point of the bridge and they live in DIFFERENT ACCOUNTS: api-gateway
# maps to __system__, oms-acme to `acme`. That split cannot be faked with one
# SVID — it is the isolation boundary itself, and it is why a fake bus proves
# nothing here. tenancy.yaml admits both today.
GATEWAY_ID="spiffe://${TRUST_DOMAIN}/ns/kanz-services/sa/api-gateway"
TENANT_OMS_ID="spiffe://${TRUST_DOMAIN}/ns/kanz-services/sa/oms-acme"
# The PLATFORM tenant's OMS, in __system__. It is what proves the bridge routes
# rather than merely delivers: an order for `acme` must reach oms-acme and NOT
# this one, and the platform tenant's own order must reach this one and not
# oms-acme. One subscriber can only ever show delivery.
SYSTEM_OMS_ID="spiffe://${TRUST_DOMAIN}/ns/kanz-services/sa/oms"

# Repo root, so this runs from anywhere.
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

# Docker on Windows needs native paths for -v; Linux CI does not. Everything else
# is identical, so CI and a laptop run the SAME script.
hostpath() { if command -v cygpath >/dev/null 2>&1; then cygpath -w "$1"; else printf '%s' "$1"; fi; }
export MSYS2_ARG_CONV_EXCL="*"

rm -rf "$OUT" && mkdir -p "$OUT" && cd "$OUT"

# --- 1. A throwaway CA + two SPIFFE-conformant SVIDs -------------------------
# SPIFFE requires of a leaf: digitalSignature key usage, client+server auth EKU,
# CA:FALSE, and the SPIFFE ID as a URI SAN. go-spiffe's x509svid.Load REJECTS a
# cert missing them, so these extensions are the contract, not decoration.
svid_ext() { # svid_ext <spiffe-id> [extra-sans]
  cat <<EOF
subjectAltName=URI:$1${2:+,$2}
keyUsage=critical,digitalSignature,keyEncipherment
extendedKeyUsage=clientAuth,serverAuth
basicConstraints=critical,CA:FALSE
EOF
}

# The broker's SVID carries DNS SANs beside the URI SAN, because SPIRE's real one
# does: infra/security/spire/registration.yaml sets dnsNameTemplates to
# {{ .PodMeta.Name }} and {{ app }}.{{ namespace }}.svc, and says why — "DNS SANs
# ease any library that still checks hostnames".
#
# That is not cosmetic here. A SPIFFE-aware Go client verifies the peer's SPIFFE
# ID and skips hostname checks, so it never needed them. The `nats` CLI — which
# is what infra/nats/bootstrap-job.yaml runs to PROVISION EVERY STREAM — is not
# SPIFFE-aware and does ordinary DNS verification: against a URI-SAN-only cert it
# fails with "certificate is not valid for any names". Minting these makes the
# test broker match production and lets CI drive the real bootstrap script over
# mTLS. `localhost` is the CI-local stand-in for the in-cluster service DNS.
SERVER_SANS="DNS:nats-0,DNS:nats.kanz-messaging.svc,DNS:localhost"

openssl req -x509 -newkey rsa:2048 -keyout ca.key -out bundle.pem -nodes \
  -subj "/CN=kanz-mtls-test-ca" -days 1 2>/dev/null

mint() { # mint <name> <spiffe-id> [extra-sans]
  openssl req -newkey rsa:2048 -keyout "$1.key" -out "$1.csr" -nodes -subj "/CN=$1" 2>/dev/null
  svid_ext "$2" "${3:-}" > "$1.ext"
  openssl x509 -req -in "$1.csr" -CA bundle.pem -CAkey ca.key -CAcreateserial \
    -out "$1.pem" -days 1 -extfile "$1.ext" 2>/dev/null
}
mint server "$SERVER_ID" "$SERVER_SANS"
mint client "$CLIENT_ID"
mint consumer "$CONSUMER_ID"
mint bootstrap "$BOOTSTRAP_ID"
mint gateway "$GATEWAY_ID"
mint tenantoms "$TENANT_OMS_ID"
mint systemoms "$SYSTEM_OMS_ID"
# spiffe-helper writes the broker's SVID under these exact names (see the
# nats-spiffe-helper ConfigMap); nats.conf reads them by path.
cp server.pem svid.pem && cp server.key svid_key.pem

# --- 2. The REAL config, extracted from the ConfigMaps production applies -----
awk '/^  nats\.conf: \|/{f=1;next} f&&/^---/{exit} f{sub(/^    /,"");print}' \
  "$ROOT/infra/nats/nats.yaml" > nats.conf.orig
awk '/^  tenants\.conf: \|/{f=1;next} f&&/^---/{exit} f{sub(/^    /,"");print}' \
  "$ROOT/infra/nats/tenancy.yaml" > tenants.conf
awk '/^  bootstrap\.sh: \|/{f=1;next} f&&/^---/{exit} f{sub(/^    /,"");print}' \
  "$ROOT/infra/nats/bootstrap-job.yaml" > bootstrap.sh
test -s nats.conf.orig || { echo "FAIL: could not extract nats.conf from infra/nats/nats.yaml"; exit 1; }
test -s tenants.conf   || { echo "FAIL: could not extract tenants.conf from infra/nats/tenancy.yaml"; exit 1; }
test -s bootstrap.sh   || { echo "FAIL: could not extract bootstrap.sh from infra/nats/bootstrap-job.yaml"; exit 1; }

# The nats-box image is READ from the Job, not repeated here, for the same reason
# the scripts are extracted: a pinned version duplicated in a test harness drifts
# from the one production runs, and the drift is invisible until the CLI's
# behaviour differs on something that matters.
NATS_BOX_IMAGE="$(awk '/image: *natsio\/nats-box/{sub(/^[[:space:]]*image:[[:space:]]*/,"");print;exit}' \
  "$ROOT/infra/nats/bootstrap-job.yaml")"
test -n "$NATS_BOX_IMAGE" || { echo "FAIL: could not read the nats-box image from infra/nats/bootstrap-job.yaml"; exit 1; }

# Override 1: strip cluster{} (see the header). Fail loudly if the block's shape
# changed — a silent no-op strip would hang the broker on quorum, and the cause
# would look like anything but this line.
awk '/^cluster \{/{s=1;next} s&&/^\}/{s=0;next} !s' nats.conf.orig > nats.conf
grep -q '^cluster' nats.conf && { echo "FAIL: cluster block survived the strip — its shape changed"; exit 1; }

# The two things under test MUST have survived verbatim. If an edit to nats.yaml
# ever drops them, this script must fail rather than quietly prove nothing.
grep -q 'verify: true'         nats.conf || { echo "FAIL: tls verify:true absent — the gate would be vacuous"; exit 1; }
grep -q 'verify_and_map: true' nats.conf || { echo "FAIL: verify_and_map absent — SVIDs would not map to accounts"; exit 1; }
grep -q 'include "tenants.conf"' nats.conf || { echo "FAIL: accounts include absent"; exit 1; }

# nats.conf sets `pid_file` so the spiffe-helper sidecar can SIGHUP the server on SVID
# rotation. In-cluster the StatefulSet supplies that directory as an emptyDir volume
# (nats-run -> /var/run/nats). Here the config arrives with bind-mounted FILES only, so
# the directory does not exist — and nats-server treats an unwritable PidFile as FATAL,
# exits immediately, and the readiness loop below then fails after 30s with "did not
# become ready", pointing at everything except the missing directory.
#
# Fixed by supplying the directory rather than by editing the config: the whole premise
# of this script is that the broker runs PRODUCTION'S nats.conf verbatim, so a tmpfs is
# the faithful stand-in for the emptyDir and stripping pid_file would be a real
# divergence in the one file under test. PID_DIR is derived from the config rather than
# repeated, so moving the path in nats.yaml cannot silently strand the mount.
PID_DIR=""
if grep -q '^[[:space:]]*pid_file:' nats.conf; then
  # awk with `exit` rather than `sed | head -1`: under this script's `set -o pipefail` a
  # SIGPIPE'd sed would abort the run for no reason. Quotes are stripped so an unquoted
  # rewrite in nats.yaml keeps working.
  PID_PATH="$(awk '/^[[:space:]]*pid_file:/{v=$2; gsub(/^"|"$/,"",v); print v; exit}' nats.conf)"
  PID_DIR="$(dirname "$PID_PATH")"
  case "$PID_DIR" in
    /*) ;;
    # Loud, not best-effort: the pid_file line is present but unreadable, so the mount
    # below would be silently skipped and the broker would die at boot on a config this
    # script claims to run verbatim.
    *) echo "FAIL: nats.conf declares a pid_file this script cannot parse (got '$PID_PATH')."
       echo "      nats-server treats an unwritable PidFile as fatal, so without a tmpfs at"
       echo "      its directory the broker exits before it listens and the readiness loop"
       echo "      below blames the TLS config. Teach this extraction the new shape."; exit 1;;
  esac
fi

# --- 3. Run it ---------------------------------------------------------------
docker rm -f "$CONTAINER" >/dev/null 2>&1 || true
# A user-defined network, and the alias is NOT arbitrary. The bootstrap container
# in step 4 runs the `nats` CLI, which is not SPIFFE-aware and does ordinary DNS
# hostname verification — so the host it dials must appear in the server SVID's
# DNS SANs. `nats-0` is one that production's SPIRE registration also mints, so
# the CLI verifies the broker exactly as it does in-cluster rather than against a
# name invented for this script. The published port stays for the Go tests, which
# dial localhost and are SPIFFE-aware.
docker network create "$NETWORK" >/dev/null 2>&1 || true
docker run -d --name "$CONTAINER" --network "$NETWORK" --network-alias nats-0 \
  -p "${PORT}:4222" -e POD_NAME=nats-0 \
  ${PID_DIR:+--tmpfs "$PID_DIR:rw,mode=1777"} \
  -v "$(hostpath "$OUT/nats.conf"):/etc/nats/nats.conf:ro" \
  -v "$(hostpath "$OUT/tenants.conf"):/etc/nats/tenants.conf:ro" \
  -v "$(hostpath "$OUT/svid.pem"):/etc/nats-certs/svid.pem:ro" \
  -v "$(hostpath "$OUT/svid_key.pem"):/etc/nats-certs/svid_key.pem:ro" \
  -v "$(hostpath "$OUT/bundle.pem"):/etc/nats-certs/bundle.pem:ro" \
  nats:2 -c /etc/nats/nats.conf -js >/dev/null

for _ in $(seq 1 30); do
  if docker logs "$CONTAINER" 2>&1 | grep -q "Server is ready"; then
    # Not merely up — up WITH the gate armed. Without this line the broker is
    # accepting plaintext and every test against it is worthless.
    #
    # POLLED, NOT CHECKED ONCE. This used to grep for the TLS line at the instant
    # "Server is ready" first appeared, which made the result depend on the order
    # nats-server happened to flush two log lines. It failed spuriously twice —
    # once on feat/ops-m2f/p1-4 and once on the ci-actions bump — and passed on
    # rerun both times, which is the signature of a race rather than a config
    # fault. The ASSERTION IS UNCHANGED: the TLS line must appear, and if it never
    # does this still fails with the same message. Only the deadline moved, from
    # "this instant" to a bounded window.
    for _ in $(seq 1 15); do
      if docker logs "$CONTAINER" 2>&1 | grep -q "TLS required for client connections"; then
        READY=1
        break
      fi
      sleep 1
    done
    if [ -z "${READY:-}" ]; then
      echo "FAIL: broker is ready but NOT requiring TLS — the config did not take"
      docker logs "$CONTAINER"
      exit 1
    fi
    break
  fi
  sleep 1
done
[ -n "${READY:-}" ] || { echo "FAIL: NATS did not become ready"; docker logs "$CONTAINER"; exit 1; }

# --- 4. Provision the topology by running PRODUCTION'S bootstrap script ------
#
# SEC-M3b — CAN THE TOPOLOGY EVEN BE PROVISIONED? bootstrap-job.yaml is the Job
# that creates every stream, and it once dialed nats://nats:4222 with no certs:
# against the production broker it was refused exactly like the services, so the
# cluster could not create the streams those services then fail to find. It is
# not a Go client, so the M3a wire does not apply — it runs the `nats` CLI, which
# is not SPIFFE-aware and does ordinary DNS hostname verification (against a
# URI-SAN-only cert it fails outright). It needs a client SVID via
# NATS_CERT/KEY/CA and a URL that MATCHES a DNS SAN.
#
# RUN AS THE BOOTSTRAP SVID, which is the part worth being exact about. This step
# lived in kanz-ci.yml and passed `client.pem` — risk-engine's SVID. That works,
# but only incidentally: every service holds `$JS.API.>` because a JetStream
# CONSUMER cannot function without it (see tenancy.yaml's SEC-M3e note), and
# stream creation rides the same prefix. So the check proved that SOME client can
# create streams, not that the bootstrap Job's identity can — and if
# nats-bootstrap were dropped from __system__, it would have stayed green while
# production could no longer provision anything. Using the real identity is the
# difference between proving the path and proving a path.
#
# A broker without streams is also not a stand-in for the spine: JetStream
# authorizes the publish and then answers "no response from stream", which reads
# as a transport fault and is not one.
docker run --rm --network "$NETWORK" \
  -e NATS_URL="nats://nats-0:4222" \
  -e NATS_CERT=/etc/nats-certs/svid.pem \
  -e NATS_KEY=/etc/nats-certs/svid_key.pem \
  -e NATS_CA=/etc/nats-certs/bundle.pem \
  -e NATS_REPLICAS=1 \
  -v "$(hostpath "$OUT/bootstrap.sh"):/scripts/bootstrap.sh:ro" \
  -v "$(hostpath "$OUT/bootstrap.pem"):/etc/nats-certs/svid.pem:ro" \
  -v "$(hostpath "$OUT/bootstrap.key"):/etc/nats-certs/svid_key.pem:ro" \
  -v "$(hostpath "$OUT/bundle.pem"):/etc/nats-certs/bundle.pem:ro" \
  "$NATS_BOX_IMAGE" sh /scripts/bootstrap.sh > bootstrap.log 2>&1 || {
    echo "FAIL: production's bootstrap script did not complete over mTLS"
    cat bootstrap.log
    exit 1
  }

# THE EXPECTED STREAMS ARE DERIVED FROM THE SCRIPT, NEVER LISTED HERE. A copy of
# the stream names in this harness is the same drift the extraction above exists
# to prevent: a stream added to bootstrap-job.yaml would be provisioned and go
# unchecked, and the first thing to notice would be a service finding its subject
# unbound. Deriving it means a new stream is required here the moment it is added
# there.
EXPECTED="$(awk '/^[[:space:]]*ensure_stream[[:space:]]+[A-Z]/{print $2}' bootstrap.sh | sort -u)"
# Non-vacuity: an extraction that yields no stream names would make every check
# below trivially true, and the harness would report success having proven none.
test -n "$EXPECTED" || { echo "FAIL: parsed no stream names out of bootstrap.sh — the check would be vacuous"; exit 1; }

docker run --rm --network "$NETWORK" \
  -e NATS_CERT=/etc/nats-certs/svid.pem \
  -e NATS_KEY=/etc/nats-certs/svid_key.pem \
  -e NATS_CA=/etc/nats-certs/bundle.pem \
  -v "$(hostpath "$OUT/bootstrap.pem"):/etc/nats-certs/svid.pem:ro" \
  -v "$(hostpath "$OUT/bootstrap.key"):/etc/nats-certs/svid_key.pem:ro" \
  -v "$(hostpath "$OUT/bundle.pem"):/etc/nats-certs/bundle.pem:ro" \
  "$NATS_BOX_IMAGE" nats --server nats://nats-0:4222 stream ls > streams.txt 2>&1 || {
    echo "FAIL: could not list streams over mTLS after bootstrap"; cat streams.txt; exit 1; }

MISSING=""
for s in $EXPECTED; do
  grep -qw "$s" streams.txt || MISSING="$MISSING $s"
done
[ -z "$MISSING" ] || {
  echo "FAIL: bootstrap ran but these streams are absent:$MISSING"
  echo "--- stream ls ---"; cat streams.txt
  echo "--- bootstrap log ---"; cat bootstrap.log
  exit 1
}

echo "NATS up on nats://localhost:${PORT} (mTLS required), certs in $OUT"
echo "Topology provisioned over mTLS by production's bootstrap script: $(echo $EXPECTED | wc -w) streams"
