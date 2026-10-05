"""Render Go JSON events and reject skipped backing-service proofs (#1352)."""

import argparse
import json
import re
import sys
from collections import defaultdict


BACKING = re.compile(
    r"\bTEST_(?:POSTGRES_URL|NATS_URL|KAFKA_BROKERS|REDIS_URL|"
    r"NATS_MTLS_URL|NATS_MTLS_CERT_DIR|OMS_ON_BUS|OMS_POSTGRES_URL|SESSION_BROWSER_SCRIPT|"
    r"MFA_(?:BROWSER_SCRIPT|PLAYWRIGHT|NODE|BFF|STATIC_DIR))\b"
)
BROWSER_PROOF = "Chromium shared-session inventory and revocation PASS"


def events(lines):
    for number, line in enumerate(lines, 1):
        event = json.loads(line)
        if not isinstance(event, dict) or not isinstance(event.get("Action"), str):
            raise ValueError(f"invalid Go event on line {number}")
        yield event


def check(lines):
    reasons = defaultdict(set)
    skipped = set()
    packages = set()
    browser_proven = False
    for event in events(lines):
        key = (event.get("Package", ""), event.get("Test", ""))
        output = event.get("Output", "")
        reasons[key].update(BACKING.findall(output))
        browser_proven |= BROWSER_PROOF in output
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
    if not browser_proven:
        problems.append("shared-session Chromium proof did not run")
    return problems, len(skipped), len(packages)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--render", action="store_true")
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
        if not args.path:
            parser.error("path is required without --render")
        with open(args.path, encoding="utf-8") as source:
            problems, skipped, packages = check(source)
        for problem in problems:
            print(f"::error::{problem}")
        if problems:
            return 1
        print(f"{packages} package results; {skipped} skips, none citing supplied backing services; browser proof present")
        return 0
    except (OSError, ValueError, TypeError) as exc:
        print(f"::error::invalid Go test evidence: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
