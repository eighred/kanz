#!/bin/sh
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
