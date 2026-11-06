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
        "TestRustProposalSharedContract", "TestRustProposalTransactions",
        *{"TestRustProposalSharedContract/" + case.replace(" ", "_") for case in (
            'the dual-control record survives the round trip',
            'an unknown proposal is not an error',
            'reading a proposal does not consume it',
            'a second proposal for one id is refused and the first survives',
            'claim elects exactly one caller',
            'a claimed proposal is no longer offered as work',
            "an expired proposal leaves the approver's queue",
            'an expired proposal is discoverable rather than silent',
            'a live proposal is not treated as expired',
            'a claimed proposal never enters the expiry queue',
            'a malformed record is refused rather than stored',
            'a self-approval is refused and leaves the proposal claimable',
            'an unnamed approver cannot claim',
        )},
        *{"TestRustProposalSharedContract/a_malformed_record_is_refused_rather_than_stored/" + case.replace(" ", "_") for case in (
            'no proposer',
            'blank proposer',
            'no digest',
            'no act',
            'no expiry',
            'born expired',
        )},
        *{"TestRustProposalTransactions/" + case for case in (
            'whole-result-row-and-aggregate-bounds',
            'concurrent-claim-expiry-one-terminal',
            'command-and-required-fact-refusal',
            'inclusive-expiry-and-default-page',
            'hold-fact-duplicate-and-rollback',
            'concurrent-go-rust-claim-one-fact',
            'claim-expiry-exclusion-and-caller-clock',
            'mutation-outbox-and-commit-rollback',
            'tenant-rls-and-bounded-storage',
            'go-roundtrip-raw-command-refusal',
        )},
        "TestRustOutboxQueue",
        *{"TestRustOutboxQueue/" + case for case in (
            "ordered-pages-and-schema-lock",
            "raw-page-mark-age-parity", "owner-tenant-and-legacy-refusal",
            "bounded-page-no-prefix", "mark-deferred-commit-unconfirmed",
            "go-rust-lock-contention-cancel", "rust-go-lock-session-loss",
            "cancel-transport-server-fallback",
            "rust-enqueue-connected-owner",
        )},
        "TestExecutionKeyIndependentOracle", "TestRustExecutionIdentity",
        "TestRustExecutionEconomics", "TestRustRepositoryTransactions",
        "TestRustRepositoryLifecycle",
        "TestFeeEconomicIndependentOracle", "TestRustFeeEconomicCompatibility", "TestRustFeeTermsCompatibility",
        *{"TestRustFeeEconomicCompatibility/" + case for case in (
            "1970-01-01T00:00:01Z", "2000-02-29T12:34:56.123Z", "2100-03-01T00:00:00.000000001Z",
            "2400-02-29T23:59:59.12Z", "9999-12-31T23:59:59.999999999Z",
            "fractional-refund", "additional-debit", "rebate-change", "scale-alias", "beyond-int64-delta",
        )},
        *{"TestRustFeeTermsCompatibility/" + case for case in (
            "same-actor", "blank-actor", "case-binding", "malformed-digest", "previous-digest-binding",
            "approval-clock", "bad-nanos", "unknown-price", "currency-change", "unchanged-fee", "outside-decimal-domain",
        )},
        "TestRustRecoveryTransactions",
        *{"TestRustRecoveryTransactions/" + case for case in (
            "go-differential-terminal-restart", "zero-history-corrected", "approved-fee-go-differential",
            "mandatory-facts-and-provenance", "case-cas-rollback-all-owners", "fee-and-claims-rollback", "order-cas-rollback", "unsafe-recovery-rls",
            "order-cas-and-push-race", "concurrent-recovery-one-checkpoint", "target-outbox-commit-rollback", "tenant-same-case-and-execution",
        )},
        *{"TestRustRecoveryTransactions/mandatory-facts-and-provenance/" + case for case in (
            "missing-status", "missing-execution", "wrong-tenant", "wrong-class", "wrong-state", "wrong-digest", "duplicate-execution", "wrong-checkpoint",
        )},
        *{"TestRustRecoveryTransactions/target-outbox-commit-rollback/" + case for case in ("target", "outbox", "commit")},
        "TestRustFeeJournalTransactions",
        *{"TestRustFeeJournalTransactions/" + case for case in (
            "commit-restart-aba-replay", "case-conflict-stale-head", "concurrent-cas-one-revision",
            "explicit-rollback", "outbox-failure-atomic", "deferred-commit-atomic",
            "tenant-same-execution-case", "other-book-not-oms-head", "corrupt-bounded-head", "scope-and-order-refusal",
        )},
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
