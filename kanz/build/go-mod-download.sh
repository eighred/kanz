#!/bin/sh
# Download this module's dependencies, retrying a TRANSPORT failure (#776).
#
# WHY THIS EXISTS. Every image ran a bare `RUN go mod download`. The Go module
# proxy and checksum database intermittently drop HTTP/2 streams mid-transfer:
#
#   go: github.com/charmbracelet/bubbletea@v1.3.10: read
#     "https://proxy.golang.org/.../v1.3.10.zip": stream error; INTERNAL_ERROR
#
# Three of those in one working session, on three different modules and three
# different services, each fixed by `gh run rerun --failed` with no code change.
# A red main that is routinely "just a flake" trains the habit of re-running
# without reading, and the next red — a real one — gets the same reflex.
#
# WHAT IT DELIBERATELY DOES NOT DO. It never relaxes verification. GONOSUMDB,
# GOFLAGS=-insecure and GONOSUMCHECK would each turn this flake into a
# supply-chain hole. The checksum database is consulted on whatever bytes finally
# arrive, so retrying changes how many attempts are made and never whether the
# result is verified.
#
# THE LAST ATTEMPT IS UNGUARDED, which is the point. A loop that ends in
# `|| true`, or whose last iteration is swallowed, exits 0 on a proxy that is
# genuinely down and hands the build a half-populated module cache — trading a
# loud failure for a confusing one. When the network is really gone this script
# fails with the real error text and a non-zero status.
#
# IT REPORTS THE RETRY COUNT when it retried, following push-with-retry.sh: a
# silent retry hides the RATE, which removes the evidence that anything is wrong
# and lets a proxy degrading from "occasional" to "usually" pass unnoticed
# because every build still goes green.
#
# IT IS BOUNDED IN WALL-CLOCK, not only in attempts. A raised retry count with no
# deadline turns a red build into a stuck one, which is worse: a job that fails
# is read, a job that hangs is waited on. DEADLINE_SECONDS caps the whole thing.
set -eu

ATTEMPTS="${GO_MOD_DOWNLOAD_ATTEMPTS:-4}"
DEADLINE_SECONDS="${GO_MOD_DOWNLOAD_DEADLINE:-180}"

started="$(date +%s)"
attempt=1

while [ "$attempt" -lt "$ATTEMPTS" ]; do
	if go mod download; then
		if [ "$attempt" -gt 1 ]; then
			echo "go mod download: SUCCEEDED after $attempt attempt(s). This build was saved by a" \
				"retry — if that is not rare, the proxy is degrading and the retry is hiding it." >&2
		fi
		exit 0
	fi

	backoff="$((attempt * 5))"
	elapsed="$(($(date +%s) - started))"
	if [ "$((elapsed + backoff))" -ge "$DEADLINE_SECONDS" ]; then
		echo "go mod download: ${elapsed}s spent and the next backoff would pass the ${DEADLINE_SECONDS}s" \
			"deadline — making one final attempt so the build fails with the real error rather than" \
			"hanging" >&2
		break
	fi

	echo "go mod download: attempt ${attempt}/${ATTEMPTS} failed, retrying in ${backoff}s" \
		"(a dropped proxy stream is transient; a genuine outage will still fail below)" >&2
	sleep "$backoff"
	attempt="$((attempt + 1))"
done

# UNGUARDED. Its exit status is this script's exit status.
echo "go mod download: ${attempt} attempt(s) failed; making a final unguarded attempt so a genuine" \
	"outage fails this build with its real error rather than passing on a half-populated cache" >&2
go mod download
