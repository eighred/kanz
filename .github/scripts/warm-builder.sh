#!/usr/bin/env bash
# Pull the buildx BUILDER image into the local daemon, retrying a refused pull
# (#320's third case).
#
# WHY THIS EXISTS. build.yml pins the builder at ghcr.io/eighred/base/buildkit
# via driver-opts, and setup-buildx-action pulls it while creating the builder.
# That pull had no retry, and it is the one both existing retries said they could
# not reach — push-with-retry.sh "wraps the PUSH", warm-bases.sh runs after the
# builder already exists, and this fails at buildx step #1, before any step this
# repo wrote:
#
#   #1 [internal] booting buildkit
#   #1 pulling image ghcr.io/eighred/base/buildkit:buildx-stable-1
#   #1 ERROR: Error response from daemon: manifest unknown
#
# — PR #547, 2026-08-18, image (venue-okx). ONE job of thirty. The other
# twenty-nine pulled that same tag in the same run, so the tag was present and
# the registry was not, which is the signature of the flake #320 is about rather
# than a mirror that needs refreshing.
#
# WHY IT IS SMALLER THAN WHAT warm-bases.sh DECLINED. That script's header names
# this exact gap and says closing it "means replacing the action with a scripted
# create+bootstrap, which is a larger change to a step whose ordering is already
# load-bearing. Left alone deliberately." It does not have to. `docker pull` puts
# the image in the daemon, and the create+bootstrap that follows uses what is
# already there — so the action is untouched, the login-then-buildx ordering is
# untouched, and the only new thing is that the pull happens somewhere it can be
# retried.
#
# WHY IT IS SAFE TO RETRY, which is the question every one of these must answer.
# Same discipline as its two siblings: the discriminator is the OUTCOME and not
# the error text, and that only holds when the step CANNOT fail for a code
# reason. This one pulls a single image and builds nothing — no source is copied,
# no compiler runs — so every failure here is registry-side by construction.
#
# AND IT STILL FAILS WHEN IT SHOULD. The builder image is private; a pull refused
# on every attempt is a revoked scope or a genuinely missing tag, not a hiccup,
# and exiting 0 there would boot no builder and go green anyway.
#
# Usage:  warm-builder.sh <image>
# Env:    MAX_ATTEMPTS (default 4), RETRY_BASE_DELAY seconds (default 10),
#         DOCKER (default "docker") — so the retry logic is testable against a stub.
set -uo pipefail

DOCKER="${DOCKER:-docker}"
MAX_ATTEMPTS="${MAX_ATTEMPTS:-4}"
RETRY_BASE_DELAY="${RETRY_BASE_DELAY:-10}"

image="${1:-}"
if [ -z "$image" ]; then
  echo "warm-builder: usage: warm-builder.sh <image>" >&2
  exit 2
fi

total_retries=0
last_error=""

attempt=1
while : ; do
  echo "==> pulling builder image ${image} (attempt ${attempt}/${MAX_ATTEMPTS})"
  if out="$("$DOCKER" pull "$image" 2>&1)"; then
    printf '%s\n' "$out"
    break
  fi
  printf '%s\n' "$out"
  last_error="$(printf '%s' "$out" | tail -3 | tr '\n' ' ')"

  if [ "$attempt" -ge "$MAX_ATTEMPTS" ]; then
    echo "::error title=Builder image pull failed::${image} refused after ${MAX_ATTEMPTS} attempts. \
A pull refused on every attempt is not the #320 flake — the builder image is private, so treat it \
as a real authorization, visibility or mirror failure. base-image-mirror.yml is what keeps the tag \
present."
    exit 1
  fi

  total_retries=$((total_retries + 1))
  delay=$((RETRY_BASE_DELAY * attempt))
  echo "    pull refused; retrying in ${delay}s (#320)"
  sleep "$delay"
  attempt=$((attempt + 1))
done

# THE COUNT IS THE POINT, for the same reason it is on the other two: once the
# flake stops being fatal it also stops being visible, and #320 turns on a rate.
if [ -n "${GITHUB_OUTPUT:-}" ]; then
  echo "retries=${total_retries}" >> "$GITHUB_OUTPUT"
fi

if [ "$total_retries" -gt 0 ]; then
  echo "::warning title=GHCR builder pull retried::${total_retries} retry/retries needed (#320). \
Last error: ${last_error}"
  if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
    echo "- \`${GITHUB_JOB:-image}\`: **${total_retries}** builder-pull retry/retries (#320) — \`${last_error}\`" \
      >> "$GITHUB_STEP_SUMMARY"
  fi
fi

echo "warm-builder: builder image ready (${total_retries} retry/retries)"
