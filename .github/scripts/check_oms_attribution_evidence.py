"""Source-bound pure attribution qualification; no trading authority."""
import argparse
from collections import Counter
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
from check_migration_evidence import verify
from check_oms_recovery_evidence import read_bounded, strict, metadata

PACKAGE = "services/oms/internal/order"
NAMES = set(['TestAttributionCanonicalOutputDomainWitness', 'TestAttributionIndependentPureOracle', 'TestAttributionIndependentPureOracle/buy-decomposed', 'TestAttributionIndependentPureOracle/cancelled-partial', 'TestAttributionIndependentPureOracle/child-ignored', 'TestAttributionIndependentPureOracle/copied-known-number-wrong-wire', 'TestAttributionIndependentPureOracle/copied-unknown-metadata', 'TestAttributionIndependentPureOracle/crossed-quote-total-only', 'TestAttributionIndependentPureOracle/duplicate-copied-message-unknown-only', 'TestAttributionIndependentPureOracle/expired-partial', 'TestAttributionIndependentPureOracle/future-side-preserved', 'TestAttributionIndependentPureOracle/go-wire-exponent114-outside-domain', 'TestAttributionIndependentPureOracle/missing-arrival', 'TestAttributionIndependentPureOracle/missing-price', 'TestAttributionIndependentPureOracle/no-fills', 'TestAttributionIndependentPureOracle/nonterminal-ignored', 'TestAttributionIndependentPureOracle/scheduled-holes-no-tape', 'TestAttributionIndependentPureOracle/scheduled-partial-tape', 'TestAttributionIndependentPureOracle/scheduled-tie-first-child', 'TestAttributionIndependentPureOracle/sell-decomposed', 'TestAttributionIndependentPureOracle/sell-go-wire-exponent114-outside-domain', 'TestAttributionIndependentPureOracle/small-value-quantization', 'TestAttributionIndependentPureOracle/unquoted-total-only', 'TestAttributionIndependentPureOracle/zero-arrival', 'TestAttributionProjectionInputMetadataWitness', 'TestAttributionProjectionInputMetadataWitness/copied-known-number-wrong-wire', 'TestAttributionProjectionInputMetadataWitness/copied-unknown-metadata', 'TestAttributionProjectionInputMetadataWitness/duplicate-copied-message-unknown-only', 'TestAttributionProjectionInputMetadataWitness/go-wire-exponent114-outside-domain', 'TestAttributionProjectionInputMetadataWitness/sell-go-wire-exponent114-outside-domain', 'TestRustAttributionBoundedRefusals', 'TestRustAttributionBoundedRefusals/duplicate-children', 'TestRustAttributionBoundedRefusals/duplicate-tape-observation', 'TestRustAttributionBoundedRefusals/foreign-child', 'TestRustAttributionBoundedRefusals/invalid-timestamp', 'TestRustAttributionBoundedRefusals/malformed-state', 'TestRustAttributionBoundedRefusals/oversized-state', 'TestRustAttributionBoundedRefusals/oversized-tape-rational', 'TestRustAttributionBoundedRefusals/schedule-index-capacity', 'TestRustAttributionDifferential', 'TestRustAttributionDifferential/buy-decomposed', 'TestRustAttributionDifferential/cancelled-partial', 'TestRustAttributionDifferential/child-ignored', 'TestRustAttributionDifferential/copied-known-number-wrong-wire', 'TestRustAttributionDifferential/copied-unknown-metadata', 'TestRustAttributionDifferential/crossed-quote-total-only', 'TestRustAttributionDifferential/duplicate-copied-message-unknown-only', 'TestRustAttributionDifferential/expired-partial', 'TestRustAttributionDifferential/future-side-preserved', 'TestRustAttributionDifferential/go-wire-exponent114-outside-domain', 'TestRustAttributionDifferential/missing-arrival', 'TestRustAttributionDifferential/missing-price', 'TestRustAttributionDifferential/no-fills', 'TestRustAttributionDifferential/nonterminal-ignored', 'TestRustAttributionDifferential/scheduled-holes-no-tape', 'TestRustAttributionDifferential/scheduled-partial-tape', 'TestRustAttributionDifferential/scheduled-tie-first-child', 'TestRustAttributionDifferential/sell-decomposed', 'TestRustAttributionDifferential/sell-go-wire-exponent114-outside-domain', 'TestRustAttributionDifferential/small-value-quantization', 'TestRustAttributionDifferential/unquoted-total-only', 'TestRustAttributionDifferential/zero-arrival', 'TestRustAttributionScheduleEdges', 'TestRustAttributionScheduleEdges/duration-saturation', 'TestRustAttributionScheduleEdges/empty-window', 'TestRustAttributionScheduleEdges/floor', 'TestRustAttributionScheduleEdges/last-index', 'TestRustAttributionScheduleEdges/long-parent-last-supported-index', 'TestRustAttributionScheduleEdges/negative-epoch', 'TestRustAttributionScheduleEdges/negative-index', 'TestRustAttributionScheduleEdges/outside-index', 'TestRustAttributionScheduleEdges/zero-count'])
MAX_STREAM = 16 * 1024 * 1024

def checked(raw):
    events = [json.loads(line, object_pairs_hook=strict) for line in raw.decode("utf-8").splitlines() if line]
    if any(e.get("Package") != "github.com/eighred/kanz/" + PACKAGE for e in events):
        raise ValueError("unexpected attribution package")
    verify(events, {PACKAGE: NAMES})
    counts = Counter(e.get("Test") for e in events if e.get("Action") == "pass" and e.get("Test"))
    if set(counts) != NAMES or any(n != 1 for n in counts.values()):
        raise ValueError("unexpected, duplicate or missing attribution outcome")
    return {"required_passes": len(NAMES)}

def qualify(directory, expected, create=False):
    raw = read_bounded(directory / "attribution.jsonl", MAX_STREAM)
    result = checked(raw)
    binary = read_bounded(directory / "binary.sha256", 65).decode("ascii").strip()
    if not re.fullmatch(r"[0-9a-f]{64}", binary):
        raise ValueError("invalid executable digest")
    bound = {**expected, "stream_sha256": hashlib.sha256(raw).hexdigest(), "result": result,
             "build": {"profile": "debug", "features": [], "target": "kanz-rs/target/debug/examples/order-attribution-fixture", "sha256": binary}}
    path = directory / "proof.json"
    if create:
        path.write_text(json.dumps(bound, sort_keys=True) + "\n", encoding="utf-8")
    else:
        saved = json.loads(read_bounded(path, 16384), object_pairs_hook=strict)
        if saved != bound or type(saved.get("version")) is not int or type(saved.get("result", {}).get("required_passes")) is not int:
            raise ValueError("attribution source/run/content differs")
    return result

if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("directory", type=Path)
    parser.add_argument("--create", action="store_true")
    for name in ("source", "tree", "run", "attempt"):
        parser.add_argument("--" + name, required=True)
    args = parser.parse_args()
    expected = metadata(args.source, args.tree, args.run, args.attempt)
    if args.create:
        head = subprocess.check_output(["git", "rev-parse", "HEAD"], text=True).strip()
        tree = subprocess.check_output(["git", "rev-parse", "HEAD^{tree}"], text=True).strip()
        if (head, tree, os.environ.get("GITHUB_RUN_ID"), os.environ.get("GITHUB_RUN_ATTEMPT")) != (args.source, args.tree, args.run, args.attempt):
            raise ValueError("runner identity differs")
    print(json.dumps(qualify(args.directory, expected, args.create), sort_keys=True))
