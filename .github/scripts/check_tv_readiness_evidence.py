"""Require actual durable catch-up and fail-closed Broker API evidence."""
import json
import sys
from check_migration_evidence import verify

REQUIRED = {
    "pkg/bus": {"TestNATSDurableReadyWaitsForPendingAndAckPending",
                "TestConsumerDurableReadyPreservesValidationAndDedup",
                "TestConsumerDurableReadyRefusesUnsupportedTransport",
                "TestRetainedRetryValidatesEffectiveDurableProfile",
                "TestRetainedProfilePreservesExistingDurableCursor",
                "TestRetainedAuthorityRetriesReleaseClaimsAndKeepTerminalDLQ"},
    "services/tv-sync/cmd/tv-sync": {"TestTVReadinessWaitsForEverySubscription",
                                      "TestTVReadinessRefusesSubscriptionFailureAndEmptySet",
                                      "TestTVReadinessClosesBeforeSiblingDrain",
                                      "TestTVRefusesOverlappingPriceFilters",
                                      "TestTVConsumerRouterSeparatesFactsAndPrices",
                                      "TestGuardedFoldSerializesOutcomeAndRevokesOnPanic",
                                      "TestTVRetainedFactsRecoverWithNATSAndPostgres",
                                      "TestTVRetainedFactsRecoverWithNATSAndPostgres/committed-response",
                                      "TestTVRetainedFactsRecoverWithNATSAndPostgres/ambiguous-committed-response",
                                      "TestTVRetainedFactsRecoverWithNATSAndPostgres/database-timeout",
                                      "TestTVRetainedFactsRecoverWithNATSAndPostgres/restart-unacked"},
    "services/tv-sync/internal/server": {"TestBrokerRefusesBeforeReadinessAndAfterCancellation",
                                                 "TestServingGenerationRejectsOldLeasesAfterRecovery",
                                                 "TestServingRevocationOrdersBoundedInflightWrite",
                                                 "TestEstablishedStreamClosesOnFailureAndRequiresNewGeneration",
                                                 "TestStreamHeaderAdmissionCannotRaceRevocation",
                                                 "TestFoldReadinessRequiresMatchingRecovery",
                                                 "TestFoldReadinessOverflowAndMissingIdentityStayClosed"},
    "services/tv-sync/internal/brokerapi": {"TestStreamRefusesUnwiredServingGate", "TestStream_DeliversDeltas", "TestStream_SurvivesTheServerWriteTimeout"},
}
if __name__ == "__main__":
    if sys.argv[1] == "--pattern":
        print("^(" + "|".join(sorted({name.split("/")[0] for name in set.union(*REQUIRED.values())})) + ")$")
    else:
        with open(sys.argv[1], encoding="utf-8") as source:
            verify((json.loads(line) for line in source), required=REQUIRED)
        print(f"All {sum(map(len, REQUIRED.values()))} required durable readiness outcomes "
              f"passed in {len(REQUIRED)} complete packages")
