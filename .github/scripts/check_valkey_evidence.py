"""Require actual pinned-Valkey compatibility evidence through production seams."""
import json
import sys

from check_migration_evidence import verify

REQUIRED = {
    "pkg/redisadapter": {
        "TestValkeyIdentityAndVersion",
        "TestValkeyAdapterAtomicClaim",
        "TestValkeyAdapterLeaseExpires",
        "TestValkeyAdapterCommitExtendsTTL",
        "TestValkeyAdapterReleaseAllowsRetry",
    },
    "services/webhook-ingest/internal/ingest": {
        "TestARealRedisAdmitsAnAlertExactlyOnce",
        "TestARealRedisHoldsTheClaimForTheWholeWindow",
        "TestARealRedisFreesAClaimNothingActedOn",
        "TestARealRedisReleasesAClaimHeldByADeadPod",
        "TestTwoPodsOverARealRedisFanOutAnAlertOnce",
        "TestTwoPodsWithPerPodCachesDOUBLE_TRADE",
    },
    "services/api-gateway/internal/middleware": {
        "TestIdempotencyIsSharedAcrossPodsOverRealRedis",
        "TestCrossTenantKeysStayDistinctInRealRedis",
    },
}


if __name__ == "__main__":
    if sys.argv[1] == "--pattern":
        print("^(" + "|".join(sorted(set.union(*REQUIRED.values()))) + ")$")
    else:
        with open(sys.argv[1], encoding="utf-8") as source:
            verify((json.loads(line) for line in source), required=REQUIRED)
        print("All 13 Valkey compatibility cases passed in 3 complete packages")
