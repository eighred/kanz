#!/usr/bin/env bash
# Authenticate to a registry, retrying a refused login (#320's fourth case).
#
# WHY THIS EXISTS. build.yml fans out ~30 image jobs that all authenticate to
# ghcr.io within the same second. One of them is periodically refused:
#
#   Logging into ghcr.io...
#   Error response from daemon: Get "https://ghcr.io/v2/": denied: denied
#
# — main, 2026-08-18, image (audit) on one run and image (web-bff) on the next.
# A DIFFERENT SERVICE EACH TIME, never the same twice, green on re-run: the #320
# signature exactly, on the last registry operation in this workflow that had no
# retry. push-with-retry.sh wraps the push, warm-bases.sh the base pull,
# warm-builder.sh the builder pull, and the LOGIN in front of all three had none.
#
# WHY docker/login-action IS STILL ABOVE THIS, AND MUST STAY. Two guards depend
# on it and both encode real outages:
#
#   supplychain_test.go  every workflow that builds an image must contain an
#                        UNCONDITIONAL docker/login-action — #161 shipped
#                        `if: github.event_name == 'push'` and made main green
#                        while every pull_request failed on a base pull.
#   image_push_retry_test.go  the login must precede the warm and buildx steps.
#
# The action also removes the credential in its post step, which a bare
# `docker login` here would not. So it keeps its job; it is marked
# continue-on-error and this script is the one that has to succeed. When the
# action worked, the first attempt below is a no-op success and costs one call.
#
# WHY IT IS SAFE TO RETRY. Same discipline as its three siblings: the
# discriminator is the OUTCOME, not the error text, and that holds only because
# this step CANNOT fail for a code reason. It authenticates and builds nothing —
# no source is copied, no compiler runs — so every failure here is registry-side
# by construction.
#
# AND IT STILL FAILS WHEN IT SHOULD. A login refused on every attempt is a
# revoked token or a permissions change, not a hiccup. Exiting 0 there would
# hand the job an unauthenticated daemon and move the red to the base pull, one
# layer below where anyone reads — which is the exact failure shape #161's
# comment above this step warns about.
#
# Usage:  login-with-retry.sh <registry> <username>
# Env:    REGISTRY_PASSWORD (required — passed by env, never argv, so it cannot
#         reach the process table or a trace)
#         MAX_ATTEMPTS (default 4), RETRY_BASE_DELAY seconds (default 5),
#         DOCKER (default "docker") — so the retry logic is testable against a stub.
set -uo pipefail

DOCKER="${DOCKER:-docker}"
MAX_ATTEMPTS="${MAX_ATTEMPTS:-4}"
RETRY_BASE_DELAY="${RETRY_BASE_DELAY:-5}"

registry="${1:-}"
username="${2:-}"
if [ -z "$registry" ] || [ -z "$username" ]; then
  echo "login-with-retry: usage: login-with-retry.sh <registry> <username>" >&2
  exit 2
fi
if [ -z "${REGISTRY_PASSWORD:-}" ]; then
  echo "login-with-retry: REGISTRY_PASSWORD is empty — refusing to attempt an anonymous login" \
       "and call it authenticated" >&2
  exit 2
fi

total_retries=0
last_error=""

attempt=1
while : ; do
  echo "==> authenticating to ${registry} as ${username} (attempt ${attempt}/${MAX_ATTEMPTS})"
  # --password-stdin, never --password: the latter puts the token in argv.
  if out="$(printf '%s' "$REGISTRY_PASSWORD" \
      | "$DOCKER" login "$registry" --username "$username" --password-stdin 2>&1)"; then
    printf '%s\n' "$out"
    break
  fi
  printf '%s\n' "$out"
  last_error="$(printf '%s' "$out" | tail -3 | tr '\n' ' ')"

  if [ "$attempt" -ge "$MAX_ATTEMPTS" ]; then
    echo "::error title=Registry login failed::${registry} refused the login on ${MAX_ATTEMPTS} \
attempts. That is not the #320 flake — treat it as a revoked token, a changed package permission, \
or a registry outage. Do NOT proceed unauthenticated: every base image is private, so the build \
would fail on a base pull one layer below where this is readable."
    exit 1
  fi

  total_retries=$((total_retries + 1))
  delay=$((RETRY_BASE_DELAY * attempt))
  echo "    login refused; retrying in ${delay}s (#320)"
  sleep "$delay"
  attempt=$((attempt + 1))
done

# THE COUNT IS THE POINT, for the same reason it is on the other three: once the
# flake stops being fatal it also stops being visible, and #320 turns on a rate.
if [ -n "${GITHUB_OUTPUT:-}" ]; then
  echo "retries=${total_retries}" >> "$GITHUB_OUTPUT"
fi

if [ "$total_retries" -gt 0 ]; then
  echo "::warning title=GHCR login retried::${total_retries} retry/retries needed (#320). \
Last error: ${last_error}"
  if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
    echo "- \`${GITHUB_JOB:-image}\`: **${total_retries}** login retry/retries (#320) — \`${last_error}\`" \
      >> "$GITHUB_STEP_SUMMARY"
  fi
fi

echo "login-with-retry: authenticated to ${registry} (${total_retries} retry/retries)"
