#!/usr/bin/env sh
# Local developer launcher. Make owns generation, readiness, tenant-dev seeding
# and authenticated smoke; this wrapper preserves status and human controls.
#
#   ./start.sh
#   ./start.sh status
#   ./start.sh down
#   CONFIRM=kanz-dev ./start.sh reset
#
# Public disposable identity and plaintext upstreams: never production.

set -eu

SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
cd "$SCRIPT_DIR"
COMPOSE_FILE="dev/docker-compose.yml"

if [ -t 1 ]; then
  BOLD=$(printf '\033[1m'); DIM=$(printf '\033[2m')
  CYAN=$(printf '\033[36m'); RESET=$(printf '\033[0m')
else
  BOLD=''; DIM=''; CYAN=''; RESET=''
fi
step() { printf '%s==>%s %s\n' "$CYAN" "$RESET" "$*"; }
die() { printf '%s\n' "$*" >&2; exit 1; }

require_make() {
  command -v make >/dev/null 2>&1 || die "GNU make is required; see dev/README.md."
}

check_prereqs() {
  require_make
  for tool in docker go buf bash protoc-gen-go protoc-gen-go-grpc; do
    command -v "$tool" >/dev/null 2>&1 || die "$tool is required; follow dev/README.md and make bootstrap."
  done
  command -v "${PYTHON:-python3}" >/dev/null 2>&1 || die "Python 3 is required; set PYTHON=python on Windows if needed."
  docker compose version >/dev/null 2>&1 || die "Docker Compose v2 is required."
  docker info >/dev/null 2>&1 || die "A working Linux Docker engine is required."
}

overview() {
  cat <<EOF

${BOLD}Kanz developer smoke stack${RESET}
  NATS event spine, Redpanda (Kafka API), PostgreSQL, risk-engine and api-gateway.
  Source: dev/docker-compose.yml; setup: dev/README.md.
  This stack lacks OMS, compliance, mandates and venue adapters.
  It does not certify the capital path or production authority.
EOF
}

dashboard() {
  cat <<EOF

${BOLD}Local endpoints and controls${RESET}
  api-gateway    http://127.0.0.1:8080  (authenticated reads)
    Public dev identity, tenant dev, portfolio PF1, read role kanz-user:
    export TOKEN=\$(go run ./cmd/kanz_devtoken --secret dev-secret --tenant dev --role kanz-user --portfolio PF1 --ttl 5m)
    curl -H "Authorization: Bearer \$TOKEN" http://127.0.0.1:8080/v1/portfolios/PF1/exposure
  risk-engine    http://127.0.0.1:8082/readyz  (gRPC :8081 inside Compose)
  NATS monitor   http://127.0.0.1:8222
  Kafka API      127.0.0.1:9092; container clients use kafka:9092
  PostgreSQL     127.0.0.1:5432; public fixture role/password kanzapp/kanz
    Service database: kanz_risk_dev; destructive test database: kanzapp.
    Schema metadata:
    docker compose -f $COMPOSE_FILE exec postgres psql -U kanzapp -d kanz_risk_dev -c '\\dt'

  Logs:       docker compose -f $COMPOSE_FILE logs -f
  Status:     ./start.sh status
  Stop:       ./start.sh down
  Reset:      CONFIRM=kanz-dev ./start.sh reset

  ${DIM}Authentication uses explicit public dev HS256. Upstreams are plaintext;
  mTLS and quotas are absent. Status output is not a readiness proof.${RESET}
EOF
}

up() {
  overview
  check_prereqs
  step "Generating, building, initializing and checking the developer stack"
  make up
  step "Authenticated tenant dev/PF1 smoke completed"
  dashboard
}

status() {
  step "Containers"
  docker compose -f "$COMPOSE_FILE" ps
  dashboard
}

down() {
  require_make
  step "Stopping the developer stack while retaining named volumes"
  make down
}

reset() {
  require_make
  step "Requesting the explicit destructive reset"
  make reset CONFIRM="${CONFIRM:-}"
}

[ "$#" -le 1 ] || die "Pass one command; use CONFIRM=kanz-dev for reset."
case "${1:-up}" in
  up|"") up ;;
  status|ps) status ;;
  down|stop) down ;;
  reset) reset ;;
  -h|--help|help)
    cat <<EOF
start.sh — canonical Make lifecycle for the disposable developer stack.

Prerequisites and first setup: dev/README.md; run make bootstrap first.
Usage:
  ./start.sh                         generate, start, seed dev/PF1 and verify reads
  ./start.sh status                  show containers and controls; no readiness claim
  ./start.sh down                    stop while retaining named volumes
  CONFIRM=kanz-dev ./start.sh reset   explicitly delete this Compose project's volumes
  ./start.sh --help                  show this message
EOF
    ;;
  *) die "Unknown command '$1'; use up | status | down | reset | --help." ;;
esac
