#!/usr/bin/env bash
# Push already-built image tags to a registry, retrying a refused push (#320).
#
# WHY THIS EXISTS. kanz-build reddened main on roughly two runs in five: the image
# BUILT and the PUSH was refused, with a different random 3-of-26 services each
# time and never the same service twice. Every occurrence cost a full diagnosis
# before it could be dismissed, and that is the real expense — a red that is not a
# code break trains the reflex to re-run first and read second, which is exactly
# how a genuine failure gets re-run into a merge.
#
# WHY IT RETRIES ON ANY PUSH FAILURE RATHER THAN ON "403". #320's body asks for a
# retry "on 403/denied specifically", but its own follow-up comment supersedes
# that, and is right: four distinct spellings were observed in one day —
#
#   403 Forbidden                                             (blob/manifest HEAD)
#   denied: denied                                            (GET /v2/)
#   denied: permission_denied: … 403 "Forbidden"              (from an intermediary)
#   failed to fetch oauth token: … 500                        (auth.docker.io)
#
# — and a grep for `403 Forbidden` misses the third, because of the quotes. Any
# string match silently stops covering the next variant, and a retry that quietly
# stops covering things is worse than no retry.
#
# So the discriminator is the OUTCOME, not the message: the build already
# succeeded in a separate step, therefore every failure reaching this script is a
# registry-side push failure. A Dockerfile or compile error can never get here to
# be retried, which is the property that matters — retrying a real build failure
# would be the genuinely dangerous version of this change.
#
# WHAT IT MUST NOT DO. A push refused on EVERY attempt is the real thing — an
# expired token, a revoked scope, a package that genuinely cannot be written —
# and still exits non-zero. And the retry count is REPORTED, loudly, because a
# silent retry that hides the rate would remove the evidence that anything is
# wrong while the registry degrades further. #320 is explicit that this would be
# a worse outcome than the current noise.
#
# Usage:  push-with-retry.sh <tag> [<tag>...]
# Env:    MAX_ATTEMPTS (default 4), RETRY_BASE_DELAY seconds (default 10),
#         DOCKER (default "docker") — the last exists so the retry logic itself is
#         testable against a stub that fails on demand.
set -uo pipefail

DOCKER="${DOCKER:-docker}"
MAX_ATTEMPTS="${MAX_ATTEMPTS:-4}"
RETRY_BASE_DELAY="${RETRY_BASE_DELAY:-10}"

if [ "$#" -eq 0 ]; then
  echo "push-with-retry: no tags given — the metadata step produced nothing to push," \
       "which means this would report success having pushed no image at all" >&2
  exit 2
fi

total_retries=0
failed_tags=""
last_error=""

for tag in "$@"; do
  attempt=1
  while : ; do
    echo "==> pushing ${tag} (attempt ${attempt}/${MAX_ATTEMPTS})"
    if out="$("$DOCKER" push "$tag" 2>&1)"; then
      printf '%s\n' "$out"
      break
    fi
    printf '%s\n' "$out"
    last_error="$(printf '%s' "$out" | tail -3 | tr '\n' ' ')"

    if [ "$attempt" -ge "$MAX_ATTEMPTS" ]; then
      echo "::error title=Image push failed::${tag} refused after ${MAX_ATTEMPTS} attempts. \
A push refused on every attempt is not the #320 flake — treat it as a real authorization \
or registry failure."
      failed_tags="${failed_tags} ${tag}"
      break
    fi

    total_retries=$((total_retries + 1))
    delay=$((RETRY_BASE_DELAY * attempt))
    echo "    push refused; retrying in ${delay}s (#320)"
    sleep "$delay"
    attempt=$((attempt + 1))
  done
done

# THE COUNT IS THE POINT. Exported for the workflow, annotated so it appears on the
# run without opening a log, and written to the job summary so the rate stays
# measurable after it stops being fatal — #320's "Verified when" turns on ten clean
# runs, and that is only answerable if a retried run is distinguishable from a
# first-attempt one.
if [ -n "${GITHUB_OUTPUT:-}" ]; then
  echo "retries=${total_retries}" >> "$GITHUB_OUTPUT"
fi

if [ "$total_retries" -gt 0 ]; then
  echo "::warning title=GHCR push retried::${total_retries} retry/retries needed (#320). \
Last error: ${last_error}"
  if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
    echo "- \`${GITHUB_JOB:-image}\`: **${total_retries}** push retry/retries (#320) — \`${last_error}\`" \
      >> "$GITHUB_STEP_SUMMARY"
  fi
else
  if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
    echo "- \`${GITHUB_JOB:-image}\`: pushed first attempt, 0 retries (#320)" >> "$GITHUB_STEP_SUMMARY"
  fi
fi

if [ -n "$failed_tags" ]; then
  echo "push-with-retry: FAILED for:${failed_tags}" >&2
  exit 1
fi

echo "push-with-retry: all tags pushed (${total_retries} retry/retries)"
