"""Require real Rust execution, including the configured production supervisor."""
import json
import sys

from check_migration_evidence import verify

REQUIRED = {
    "services/risk-engine/tools/bond-risk-conformance": {"TestRustBondRiskProcessConformance", "TestBondCorpusIndependentAnalytics"},
    "services/risk-engine/internal/bondshadow": {"TestRustBondShadowActualProcess"},
    "internal/shadowprocess": set(),
}

if __name__ == "__main__":
    with open(sys.argv[1], encoding="utf-8") as source:
        verify((json.loads(line) for line in source), REQUIRED)
    print("Bond numerical corpus and actual Rust supervisor proof completed without skips")
