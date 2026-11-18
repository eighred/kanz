"""Render Go JSON events and reject skipped backing-service proofs (#1352)."""

import argparse
import json
import re
import sys
from collections import defaultdict


BACKING = re.compile(
    r"\bTEST_(?:POSTGRES_URL|NATS_URL|TV_NATS_URL|KAFKA_BROKERS|REDIS_URL|"
    r"NATS_MTLS_URL|NATS_MTLS_CERT_DIR|OMS_ON_BUS|OMS_POSTGRES_URL|ACCOUNTING_MIGRATION_POSTGRES_URL|"
    r"INFERENCE_ON_BUS|SESSION_BROWSER_SCRIPT|"
    r"MFA_(?:BROWSER_SCRIPT|PLAYWRIGHT|NODE|BFF|STATIC_DIR))\b"
)
BROWSER_PROOF = "Chromium shared-session inventory and revocation PASS"
INTEGER_BROWSER_TEST = (
    "github.com/eighred/kanz/services/identity/internal/server",
    "TestMFABrowserEnrollmentLoginStepUpAndFactorRemoval",
)
INTEGER_BROWSER_PROOF = (
    "integer HTTP/BFF/browser proof: adjacent signed revisions, exact mutation requests, "
    "stale/malformed/exhausted refusal and unchanged authority"
)


def events(lines):
    for number, line in enumerate(lines, 1):
        event = json.loads(line)
        if not isinstance(event, dict) or not isinstance(event.get("Action"), str):
            raise ValueError(f"invalid Go event on line {number}")
        yield event


def check(lines, expected_packages=None):
    reasons = defaultdict(set)
    skipped = set()
    packages = set()
    cached = set()
    browser_proven = False
    integer_browser_phase, integer_markers, integer_browser_refused = "absent", 0, False
    for event in events(lines):
        key = (event.get("Package", ""), event.get("Test", ""))
        output = event.get("Output", "")
        if not key[1] and re.match(r"^ok\s+\S+\s+\(cached\)", output):
            cached.add(key[0])
        reasons[key].update(BACKING.findall(output))
        browser_proven |= BROWSER_PROOF in output
        if key == INTEGER_BROWSER_TEST:
            action = event["Action"]
            if action == "run":
                integer_browser_refused |= integer_browser_phase != "absent"
                integer_browser_phase = "running"
            elif action == "output":
                markers = sum(re.fullmatch(
                    r"(?:mfa_browser_test\.go:[1-9][0-9]*: )?" + re.escape(INTEGER_BROWSER_PROOF), line.strip()
                ) is not None for line in output.splitlines())
                if markers:
                    integer_browser_refused |= integer_browser_phase != "running"
                    integer_markers += markers
            elif action == "pass":
                integer_browser_refused |= integer_browser_phase != "running" or integer_markers != 1
                integer_browser_phase = "passed"
            elif action in ("skip", "fail"):
                integer_browser_refused = True
        if event["Action"] == "skip":
            skipped.add(key)
        if not key[1] and event["Action"] in ("pass", "fail", "skip") and key[0]:
            packages.add(key[0])
    if not packages:
        raise ValueError("no completed package results; suite proof is absent")
    problems = [
        f"backing-service test skipped: {package}/{test} ({', '.join(sorted(reasons[(package, test)]))})"
        for package, test in sorted(skipped)
        if reasons[(package, test)]
    ]
    problems.extend(f"cached package result is not fresh verification: {package}" for package in sorted(cached))
    if expected_packages is not None:
        expected = {package.strip() for package in expected_packages if package.strip()}
        if not expected:
            raise ValueError("empty expected package inventory")
        problems.extend(f"package result missing: {package}" for package in sorted(expected - packages))
        problems.extend(f"unexpected package result: {package}" for package in sorted(packages - expected))
    if not browser_proven:
        problems.append("shared-session Chromium proof did not run")
    if integer_browser_phase != "passed" or integer_markers != 1 or integer_browser_refused:
        problems.append("integer HTTP/BFF/Chromium proof lacks its exact owning test run, marker and fresh pass")
    return problems, len(skipped), len(packages)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--render", action="store_true")
    parser.add_argument("--packages", help="go list inventory for this test invocation")
    parser.add_argument("path", nargs="?")
    args = parser.parse_args()
    try:
        if args.render:
            # Keep normal -v text in Actions while retaining exact Package/Test
            # attribution in the JSONL artifact read by the following gate.
            for event in events(sys.stdin):
                sys.stdout.write(event.get("Output", ""))
                sys.stdout.flush()
            return 0
        if not args.path or not args.packages:
            parser.error("path and --packages are required without --render")
        with open(args.packages, encoding="utf-8") as inventory:
            expected = list(inventory)
        with open(args.path, encoding="utf-8") as source:
            problems, skipped, packages = check(source, expected)
        for problem in problems:
            print(f"::error::{problem}")
        if problems:
            return 1
        print(f"{packages} fresh package results match go list; {skipped} skips, none citing supplied backing services; browser proof present")
        return 0
    except (OSError, ValueError, TypeError) as exc:
        print(f"::error::invalid Go test evidence: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
