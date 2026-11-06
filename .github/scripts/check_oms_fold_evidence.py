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
        "TestPinnedGoUnicodeTable", "TestFeeWireIndependentSemantics",
        "TestRustFeeUnicodeCompatibility", "TestRustFeeWireCompatibility", "TestRustFeeWireRefusals",
        *{"TestRustFeeWireCompatibility/" + case for case in (
            "known-field-order", "distinct-unknown-order", "same-unknown-order",
            "unknown-value-encoding", "unknown-tag-normalized", "nested-unknown-order",
            "nested-unknown-difference", "duplicate-embedded-merge", "duplicate-embedded-unknown", "duplicate-embedded-unknown-only", "nested-unknown-tag-normalized",
            "unknown-group", "unknown-group-depth-boundary", "unknown-group-inner-order", "unknown-group-inner-noncanonical-tag", "known-enum-wrong-wire", "known-wrong-wire-is-unknown",
            "known-wrong-wire-not-dropped", "optional-absent-present", "scalar-default", "duplicate-scalar-last",
        )},
        *{"TestRustFeeWireRefusals/" + case for case in (
            "zero-field", "truncated-varint", "unterminated-group", "unexpected-end-group", "wrong-end-group", "oversized-input", "unknown-group-excess-depth",
        )},
        "TestRustRepositoryQueries",
        *{"TestRustRepositoryQueries/" + case for case in (
            "list-status-parent-exact", "portfolio-page-count-backfill",
            "tenant-same-ids", "corruption-capacity-no-prefix",
            "row-cap-no-prefix", "canonical-index-plans", "sql-failure-no-prefix", "scope-selector-refusal", "unavailable-no-result",
        )},
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
