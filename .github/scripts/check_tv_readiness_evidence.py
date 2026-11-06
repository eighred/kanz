"""Require actual durable catch-up and fail-closed Broker API evidence."""
import json
import sys
from check_migration_evidence import verify

REQUIRED = {
    "pkg/bus": {"TestNATSDurableReadyWaitsForPendingAndAckPending",
                "TestConsumerDurableReadyPreservesValidationAndDedup",
                "TestConsumerDurableReadyRefusesUnsupportedTransport"},
    "services/tv-sync/cmd/tv-sync": {"TestTVReadinessWaitsForEverySubscription",
                                      "TestTVReadinessRefusesSubscriptionFailureAndEmptySet",
                                      "TestTVReadinessClosesBeforeSiblingDrain"},
    "services/tv-sync/internal/server": {"TestBrokerRefusesBeforeReadinessAndAfterCancellation"},
}
if __name__ == "__main__":
    if sys.argv[1] == "--pattern":
        print("^(" + "|".join(sorted(set.union(*REQUIRED.values()))) + ")$")
    else:
        with open(sys.argv[1], encoding="utf-8") as source:
            verify((json.loads(line) for line in source), required=REQUIRED)
        print("All 7 durable readiness cases passed in 3 complete packages")
