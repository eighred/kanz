"""Bound transient module downloads without retrying build failures (#1353)."""

import json
import re
import subprocess
import sys
import time


TRANSIENT = re.compile(
    r"(?:stream error:.*INTERNAL_ERROR|connection reset by peer|"
    r"TLS handshake timeout|i/o timeout|unexpected EOF|"
    r"\b(?:500 Internal Server Error|502 Bad Gateway|503 Service Unavailable|504 Gateway Timeout)\b)"
)
TERMINAL = re.compile(r"checksum mismatch|SECURITY ERROR|401|403|unauthorized|forbidden", re.IGNORECASE)


def download_errors(output):
    decoder = json.JSONDecoder()
    errors = []
    remaining = output.strip()
    while remaining:
        item, end = decoder.raw_decode(remaining)
        if not isinstance(item, dict):
            raise ValueError("module download emitted a non-object result")
        if item.get("Error"):
            errors.append(str(item["Error"]))
        remaining = remaining[end:].lstrip()
    return errors


def retryable(errors, stderr):
    # An unknown extra diagnostic must not be hidden by a transient error in a
    # different module. Go emits download -json failures as Error fields.
    if not errors or stderr.strip():
        return False
    return all(
        'https://' in error and TRANSIENT.search(error) and not TERMINAL.search(error)
        for error in errors
    )


def fetch(run=subprocess.run, sleep=time.sleep, attempts=3):
    for attempt in range(1, attempts + 1):
        try:
            result = run(
                ["go", "mod", "download", "-json"],
                capture_output=True, text=True, timeout=180, check=False,
            )
        except subprocess.TimeoutExpired:
            # Only the acquisition command is timed out; no build/test or
            # policy evaluation is wrapped here. The last timeout is terminal.
            can_retry = True
            print(f"module acquisition timed out on attempt {attempt}", file=sys.stderr)
        else:
            try:
                errors = download_errors(result.stdout)
            except ValueError:
                print("::error::malformed Go module download results", file=sys.stderr)
                return 1
            if result.returncode == 0 and not errors:
                print(f"Go module acquisition completed on attempt {attempt}; checksum verification retained")
                return 0
            can_retry = retryable(errors, result.stderr)
            # Report only the failure class; dependency URLs could carry private
            # authentication in a future module configuration.
            print(f"module acquisition failed on attempt {attempt}: transient={can_retry}, module_errors={len(errors)}", file=sys.stderr)
        if not can_retry or attempt == attempts:
            print("::error::Go module acquisition failed; source verification has not run", file=sys.stderr)
            return 1
        print(f"::warning::transient module transfer failure; bounded retry {attempt}/{attempts - 1}")
        sleep(attempt * 5)
    return 1


if __name__ == "__main__":
    sys.exit(fetch())
