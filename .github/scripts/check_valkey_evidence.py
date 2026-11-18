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
    "services/webhook_ingest/internal/ingest": {
        "TestARealRedisAdmitsAnAlertExactlyOnce",
        "TestWebhookRetainsReplayClaimsAcrossValkeyOutage",
        "TestARealRedisHoldsTheClaimForTheWholeWindow",
        "TestARealRedisFreesAClaimNothingActedOn",
        "TestARealRedisReleasesAClaimHeldByADeadPod",
        "TestTwoPodsOverARealRedisFanOutAnAlertOnce",
        "TestTwoPodsWithPerPodCachesDOUBLE_TRADE",
    },
    "services/api_gateway/internal/middleware": {
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
        print(f"All {sum(map(len, REQUIRED.values()))} Valkey compatibility cases passed in {len(REQUIRED)} complete packages")
