#!/usr/bin/env sh
# start.sh — one-command local starter for the Kanz platform.
#
# Wraps the existing dev stack (dev/docker-compose.yml + the Makefile flow) in a
# friendly, self-checking launcher so you can bring the platform up and SEE it:
# it checks prerequisites, generates the schema SDK if missing, boots NATS +
# Kafka + Postgres + risk-engine + api-gateway, waits until they are healthy,
# seeds one portfolio, and prints where everything lives and how to poke it.
#
# Usage:
#   ./start.sh            # bring the stack up, seed, and show the dashboard
#   ./start.sh status     # show what's running + the URLs (no changes)
#   ./start.sh down       # tear the stack down (and its volumes)
#   ./start.sh --help
#
# POSIX sh — runs under Git Bash on Windows and any Linux/macOS shell.

set -eu

# --- locate the repo (this script lives in kanz/) -------------------------
SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
cd "$SCRIPT_DIR"
COMPOSE_FILE="dev/docker-compose.yml"
SCHEMAS_SDK="../kanz-schemas/gen/go"

# --- pretty output (degrade gracefully without a TTY) ---------------------
if [ -t 1 ]; then
  BOLD=$(printf '\033[1m'); DIM=$(printf '\033[2m'); GREEN=$(printf '\033[32m')
  YELLOW=$(printf '\033[33m'); CYAN=$(printf '\033[36m'); RED=$(printf '\033[31m')
  RESET=$(printf '\033[0m')
else
  BOLD=''; DIM=''; GREEN=''; YELLOW=''; CYAN=''; RED=''; RESET=''
fi
say()  { printf '%s\n' "$*"; }
step() { printf '%s==>%s %s\n' "$CYAN" "$RESET" "$*"; }
ok()   { printf '  %s✓%s %s\n' "$GREEN" "$RESET" "$*"; }
warn() { printf '  %s!%s %s\n' "$YELLOW" "$RESET" "$*"; }
die()  { printf '  %s✗%s %s\n' "$RED" "$RESET" "$*" >&2; exit 1; }

# docker compose (v2 plugin) or docker-compose (v1) — pick whichever exists.
compose() {
  if docker compose version >/dev/null 2>&1; then
    docker compose -f "$COMPOSE_FILE" "$@"
  else
    docker-compose -f "$COMPOSE_FILE" "$@"
  fi
}

# --- the project map (the "see it better" part) ---------------------------
overview() {
  cat <<EOF

${BOLD}Kanz — event-sourced institutional risk/portfolio platform${RESET}
${DIM}A Go modular monolith + edge services over a NATS spine (live) and Kafka
(durable log), with PostgreSQL behind the durable book-of-record stores.${RESET}

  ${BOLD}Where things live${RESET}
    services/        the deployable services (risk-engine, api-gateway,
                     accounting, alternatives, wealth, datamaster, copilot, …)
    internal/, pkg/  shared analytics + platform libraries
    services/*/migrations/   PostgreSQL schema per durable store (PARITY-02)
    infra/           k8s, GitOps, security, DR (CloudNativePG PITR)
    docs/            onboarding.md + runbooks/

  ${BOLD}The local dev stack this script boots${RESET} ${DIM}(dev/docker-compose.yml)${RESET}
    NATS (live spine) · Kafka (durable log) · PostgreSQL · risk-engine · api-gateway
EOF
}

# --- prerequisite checks --------------------------------------------------
check_prereqs() {
  step "Checking prerequisites"
  command -v docker >/dev/null 2>&1 || die "docker not found — install Docker Desktop, then re-run."
  docker info >/dev/null 2>&1 || die "Docker is installed but not running — start Docker Desktop and re-run."
  ok "docker is running"
  if command -v go >/dev/null 2>&1; then ok "go $(go version | awk '{print $3}')"; else warn "go not found (only needed for 'make generate' + the seed; the stack still builds in Docker)"; fi
}

# --- generate the schema SDK if it isn't there ----------------------------
ensure_sdk() {
  step "Schema SDK"
  if [ -d "$SCHEMAS_SDK" ]; then
    ok "generated SDK present ($SCHEMAS_SDK)"
    return
  fi
  warn "generated SDK missing — the service Dockerfiles COPY it, so generating now"
  if command -v buf >/dev/null 2>&1; then
    ( cd ../kanz-schemas && buf generate ) && ok "buf generate done" || die "buf generate failed — see docs/onboarding.md"
  else
    die "buf not found and the SDK is missing. Install buf (https://buf.build/docs/installation), then run 'make generate'."
  fi
}

# --- wait for a container healthcheck / an HTTP endpoint ------------------
wait_http() {
  # wait_http <name> <url> <attempts>
  name=$1; url=$2; tries=${3:-40}
  i=1
  while [ "$i" -le "$tries" ]; do
    if curl -fsS -o /dev/null "$url" 2>/dev/null; then ok "$name is up ($url)"; return 0; fi
    printf '  %s…%s waiting for %s (%d/%d)\r' "$DIM" "$RESET" "$name" "$i" "$tries"
    sleep 2; i=$((i + 1))
  done
  printf '\n'; warn "$name did not answer at $url yet — check 'docker compose logs $name'"
  return 1
}

# --- bring the stack up ---------------------------------------------------
up() {
  overview
  check_prereqs
  ensure_sdk
  step "Building + starting the stack (first run pulls images + compiles — give it a few minutes)"
  compose up -d --build
  ok "containers started"

  step "Waiting for services to become healthy"
  wait_http "risk-engine" "http://localhost:8082/healthz" 60 || true
  wait_http "api-gateway" "http://localhost:8080/healthz" 30 || true

  step "Seeding one portfolio (PF1)"
  if command -v go >/dev/null 2>&1; then
    if go run ./test/load/seed >/dev/null 2>&1; then ok "published a PortfolioSnapshot for PF1"; else warn "seed failed — run 'go run ./test/load/seed' manually to see why"; fi
  else
    warn "go not installed locally — skip seeding, or run it from a Go-capable shell"
  fi

  dashboard
}

# --- the running dashboard (the "see it better" payoff) -------------------
dashboard() {
  cat <<EOF

${GREEN}${BOLD}Kanz is up.${RESET}

  ${BOLD}Open / poke it${RESET}
    api-gateway      ${CYAN}http://localhost:8080${RESET}   ${DIM}(authenticated — it refuses to run otherwise)${RESET}
      get a token:   export TOKEN=\$(go run ./cmd/kanz-devtoken --secret dev-secret --tenant acme)
      try:           curl -H "Authorization: Bearer \$TOKEN" localhost:8080/v1/portfolios/PF1/exposure
    risk-engine      ${CYAN}http://localhost:8082/healthz${RESET}   ${DIM}(gRPC query on :8081)${RESET}
    NATS monitor     ${CYAN}http://localhost:8222${RESET}
    PostgreSQL       ${CYAN}localhost:5432${RESET}   ${DIM}(user/pass/db: kanz/kanz/kanz)${RESET}
      look inside:   docker compose -f $COMPOSE_FILE exec postgres psql -U kanz -c '\\dt'

  ${BOLD}Manage${RESET}
    logs (follow)    docker compose -f $COMPOSE_FILE logs -f
    what's running   ./start.sh status
    stop everything  ./start.sh down

  ${DIM}Auth/signing/quotas are OFF and upstream is plaintext — this is a dev loop,
  not a security posture. Tilt users: 'tilt up' gives a live UI (see dev/README.md).${RESET}
EOF
}

status() {
  step "Containers"
  compose ps
  dashboard
}

down() {
  step "Tearing down the stack (with volumes)"
  compose down -v
  ok "stopped"
}

case "${1:-up}" in
  up|"")        up ;;
  status|ps)    status ;;
  down|stop)    down ;;
  -h|--help|help)
    cat <<EOF
start.sh — one-command local starter for the Kanz platform.

Boots the dev stack (NATS + Kafka + PostgreSQL + risk-engine + api-gateway),
waits for health, seeds a portfolio, and prints where everything lives.

Usage:
  ./start.sh            bring the stack up, seed, and show the dashboard
  ./start.sh status     show what's running + the URLs (no changes)
  ./start.sh down       tear the stack down (and its volumes)
  ./start.sh --help     this message
EOF
    ;;
  *)
    die "unknown command '$1' — try: up | status | down | --help" ;;
esac
