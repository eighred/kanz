"""Require actual Rust fill folding through both corpus and OMS supervisor."""
import json
import sys

from check_migration_evidence import verify

REQUIRED = {
    "services/oms/tools/fill-conformance": {
        "TestFillCorpusIndependentOracle", "TestRustFillFoldProcessConformance",
    },
    "services/oms/internal/foldshadow": {"TestRustOMSFillShadowActualProcess"},
    "internal/shadowprocess": set(),
}

if __name__ == "__main__":
    with open(sys.argv[1], encoding="utf-8") as source:
        verify((json.loads(line) for line in source), REQUIRED)
    print("OMS fill corpus and actual Rust supervisor proof completed without skips")
