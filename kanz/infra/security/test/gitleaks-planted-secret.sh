#!/bin/sh
# SEC-02e: prove the gitleaks gate actually catches a secret. Plants a fake
# credential at a NON-allowlisted repo path and asserts gitleaks — using the REAL
# repo config — detects it (exit code 1 = leaks found). A misconfigured gate
# (over-broad allowlist, broken rules) would pass the planted secret (exit 0) and
# this self-test fails loudly. Wired into .github/workflows/security.yml.
#
#   sh kanz/infra/security/test/gitleaks-planted-secret.sh
set -eu

repo_root=$(git rev-parse --show-toplevel)
planted_dir="$repo_root/.sec-selftest"
trap 'rm -rf "$planted_dir"' EXIT
mkdir -p "$planted_dir"
# A canonical, NON-allowlisted secret (no example/test/dummy token in it).
printf 'github_pat=ghp_a1b2c3d4e5f6g7h8i9j0k1l2m3n4o5p6q7r8\n' > "$planted_dir/planted.txt"

# Resolve a runner: native gitleaks, else its container, else skip.
run_gitleaks() {
  if command -v gitleaks >/dev/null 2>&1; then
    gitleaks detect --no-git --source "$planted_dir" \
      --config "$repo_root/.gitleaks.toml" --redact --no-banner
  elif command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1; then
    # Daemon must be live: a down daemon makes `docker run` exit 1, which is
    # indistinguishable from gitleaks' "leaks found" exit 1 — so skip rather than
    # risk a false pass.
    docker run --rm -v "$repo_root:/repo" ghcr.io/gitleaks/gitleaks:latest \
      detect --no-git --source=/repo/.sec-selftest \
      --config=/repo/.gitleaks.toml --redact --no-banner
  else
    return 99 # no usable gitleaks (native binary or running docker daemon)
  fi
}

echo "running gitleaks against a planted secret (expect detection)..."
set +e
run_gitleaks
code=$?
set -e

case "$code" in
  1) echo "OK: planted secret detected — the gitleaks gate works" ;;
  0) echo "FAIL: gitleaks did NOT detect the planted secret — the gate is broken" >&2; exit 1 ;;
  99) echo "SKIP: neither gitleaks nor docker available" >&2; exit 0 ;;
  *) echo "ERROR: gitleaks exited $code (not a clean detect run)" >&2; exit "$code" ;;
esac
