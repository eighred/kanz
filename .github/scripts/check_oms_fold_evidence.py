"""Require actual Rust fill folding through both corpus and OMS supervisor."""
import json
import sys

from check_migration_evidence import verify

REQUIRED = {
    "services/oms/tools/fill-conformance": {
        "TestFillCorpusIndependentOracle", "TestRustFillFoldProcessConformance",
        "TestTerminalCorpusIndependentState", "TestRustTerminalFoldProcessConformance",
        "TestRustFillThenTerminalReplay",
    },
    "services/oms/internal/foldshadow": {
        "TestRustOMSFillShadowActualProcess", "TestRustTransactionCandidateBinding",
        "TestRustOMSTerminalShadowActualProcess", "TestTerminalResultBindingAndPresence", "TestTerminalSharesBoundedQueue",
        "TestRustOMSPostgresTransactions",
        *{"TestRustOMSPostgresTransactions/" + case for case in (
            "durable-restart-duplicate-conflict", "concurrent-duplicate",
            "cancel-wins-stale-fill", "fill-wins-stale-cancel",
            "commit-failure-rolls-back-all-owners", "tenant-isolation",
        )},
    },
    "internal/shadowprocess": set(),
    "services/oms/tools/store-conformance": {
        "TestExecutionKeyIndependentOracle", "TestRustExecutionIdentity",
        "TestRustExecutionEconomics", "TestRustRepositoryTransactions",
        "TestRustRepositoryLifecycle",
        *{"TestRustRepositoryLifecycle/" + case for case in (
            "concurrent-create-one-fact", "create-restart-load-go-save",
            "create-invalid-fact-rollback", "create-deferred-commit-rollback",
            "create-tenant-same-id", "applied-fill-raw-aliases",
            "read-notfound-unavailable-corruption", "claim-capacity-no-prefix",
            "claim-read-failure-no-prefix", "load-concurrent-version",
            "read-oversized-bytes",
        )},
        *{"TestRustRepositoryTransactions/" + case for case in (
            "durable-restart-exact-bytes", "duplicate-conflict-and-alias",
            "concurrent-duplicate", "fill-wins-cancel", "cancel-wins-fill",
            "commit-failure-rollback-reclaim", "tenant-and-missing-scope",
            "legacy-alias", "fee-head-duplicate-and-corruption",
            "unowned-outbox-refusal", "unsafe-rls-and-invalid-record",
        )},
    },
}

if __name__ == "__main__":
    with open(sys.argv[1], encoding="utf-8") as source:
        verify((json.loads(line) for line in source), REQUIRED)
    print("OMS fill corpus and actual Rust supervisor proof completed without skips")
