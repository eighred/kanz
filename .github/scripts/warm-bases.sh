#!/usr/bin/env bash
# Pull a Dockerfile's base images into the builder's cache, retrying a refused
# pull (#320).
#
# WHY THIS EXISTS, AND WHY THE EXISTING RETRY DOES NOT COVER IT. push-with-retry.sh
# fixed the half of #320 that reddened main on a refused PUSH. The other half is a
# refused PULL, and build.yml says so in its own comments: that retry "wraps the
# PUSH", and a base-image failure happens upstream of it, inside the build. main
# went red on it again on 2026-08-11 —
#
#   #21 ERROR: failed to copy: httpReadSeeker: failed open: unexpected status from
#   GET request to https://ghcr.io/v2/eighred/base/distroless-static/blobs/…: 403
#
# — on image (wealth), whose Dockerfile had not changed, in a run whose kanz-ci
# was green. Every such occurrence costs a full diagnosis before it can be
# dismissed, which is the expense a red-that-is-not-a-break actually carries.
#
# WHY IT IS SAFE TO RETRY THIS AND NOT THE BUILD. The same discipline
# push-with-retry.sh is built on: the discriminator is the OUTCOME, not the error
# text, and that only works when the step CANNOT fail for a code reason. This step
# compiles nothing and copies no source — it builds a Dockerfile consisting of the
# base image and nothing else. A compile error, a bad COPY, a missing SDK cannot
# reach it, so every failure here is registry-side by construction. Retrying the
# real build instead would be the genuinely dangerous version of this change, for
# exactly the reason that script spells out.
#
# WHY `COPY --from`, RATHER THAN A BARE `FROM`. Measured, not assumed: a bare
#
#   FROM <image>
#
# built with --output=type=cacheonly RESOLVES the manifest and stops — 12 kB in the
# builder's cache, no blobs. The failure being prevented is a blob GET, so that
# would warm nothing that matters. Copying the image's root into a scratch stage
# forces every layer to be fetched and extracted (20.6 MB for the same alpine),
# and it works for a base with no shell — distroless has no /bin/sh for a `RUN`
# to warm it with, and distroless-static is the image these 403s land on.
#
# WHAT IT DOES NOT COVER, AND WHERE THAT WENT. The buildx BUILDER image
# (ghcr.io/eighred/base/buildkit, pinned in build.yml's driver-opts) is pulled by
# setup-buildx-action before this script can run, and has 403'd too. This header
# used to say retrying it "means replacing the action with a scripted
# create+bootstrap … Left alone deliberately."
#
# IT DID NOT NEED THAT, AND THE GAP BIT FIRST. PR #547, 2026-08-18, image
# (venue-okx): "manifest unknown" pulling the builder, one job of thirty, at
# buildx step #1 where neither retry could see it. warm-builder.sh closes it by
# pulling the image BEFORE the action runs, so the create+bootstrap finds it in
# the daemon — the action is untouched and so is the load-bearing ordering.
#
# Usage:  warm-bases.sh <dockerfile>
# Env:    MAX_ATTEMPTS (default 4), RETRY_BASE_DELAY seconds (default 10),
#         DOCKER (default "docker") — so the retry logic is testable against a stub.
set -uo pipefail

DOCKER="${DOCKER:-docker}"
MAX_ATTEMPTS="${MAX_ATTEMPTS:-4}"
RETRY_BASE_DELAY="${RETRY_BASE_DELAY:-10}"

dockerfile="${1:-}"
if [ -z "$dockerfile" ] || [ ! -f "$dockerfile" ]; then
  echo "warm-bases: usage: warm-bases.sh <dockerfile> (got '${dockerfile}')" >&2
  exit 2
fi

# EXTERNAL BASES ONLY. A multi-stage Dockerfile's later stages name earlier ones
# (`COPY --from=build`, `FROM build AS x`), and those are not images to pull —
# asking a registry for them would fail for a reason that is not a flake. Stage
# aliases are collected from `AS <name>` and excluded.
stages=""
bases=""
while read -r _ image rest; do
  alias_name="$(printf '%s' "$rest" | awk '{ if (tolower($1) == "as") print $2 }')"
  case " ${stages} " in
    *" ${image} "*) ;;                       # a previous stage, not an image
    *) case "${bases} " in
         *" ${image} "*) ;;                  # already queued
         *) bases="${bases} ${image}" ;;
       esac ;;
  esac
  [ -n "$alias_name" ] && stages="${stages} ${alias_name}"
done <<EOF
$(grep -iE '^[[:space:]]*FROM[[:space:]]' "$dockerfile" | sed 's/^[[:space:]]*//')
EOF

# shellcheck disable=SC2086 # bases is a space-separated list; splitting is intended
set -- $bases
if [ "$#" -eq 0 ]; then
  echo "warm-bases: ${dockerfile} declares no external base image — that is not a" \
       "Dockerfile this repo builds, and warming nothing while reporting success" \
       "would hide that" >&2
  exit 2
fi

total_retries=0
failed=""
last_error=""

for image in "$@"; do
  attempt=1
  while : ; do
    echo "==> warming ${image} (attempt ${attempt}/${MAX_ATTEMPTS})"
    if out="$(printf 'FROM %s AS warm\nFROM scratch\nCOPY --from=warm / /\n' "$image" \
        | "$DOCKER" buildx build --output=type=cacheonly - 2>&1)"; then
      printf '%s\n' "$out"
      break
    fi
    printf '%s\n' "$out"
    last_error="$(printf '%s' "$out" | tail -3 | tr '\n' ' ')"

    if [ "$attempt" -ge "$MAX_ATTEMPTS" ]; then
      echo "::error title=Base image pull failed::${image} refused after ${MAX_ATTEMPTS} attempts. \
A pull refused on every attempt is not the #320 flake — treat it as a real authorization, \
visibility or registry failure."
      failed="${failed} ${image}"
      break
    fi

    total_retries=$((total_retries + 1))
    delay=$((RETRY_BASE_DELAY * attempt))
    echo "    pull refused; retrying in ${delay}s (#320)"
    sleep "$delay"
    attempt=$((attempt + 1))
  done
done

# THE COUNT IS THE POINT, for the same reason it is on the push side: once the
# flake stops being fatal it also stops being visible, and #320 turns on a rate.
if [ -n "${GITHUB_OUTPUT:-}" ]; then
  echo "retries=${total_retries}" >> "$GITHUB_OUTPUT"
fi

if [ "$total_retries" -gt 0 ]; then
  echo "::warning title=GHCR base pull retried::${total_retries} retry/retries needed (#320). \
Last error: ${last_error}"
  if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
    echo "- \`${GITHUB_JOB:-image}\`: **${total_retries}** base-pull retry/retries (#320) — \`${last_error}\`" \
      >> "$GITHUB_STEP_SUMMARY"
  fi
fi

if [ -n "$failed" ]; then
  echo "warm-bases: FAILED for:${failed}" >&2
  exit 1
fi

echo "warm-bases: all bases warmed (${total_retries} retry/retries)"
