"""Require separate-role runtime access, isolation and checkpoint recovery."""
import json
import sys

from check_migration_evidence import verify

ROOT = "TestTokyoTVSyncSeparateRolesAndRecovery"
REQUIRED = {
    "services/tv-sync/internal/projection": {
        ROOT,
        ROOT + "/RuntimeCannotDDL",
        ROOT + "/FutureTableGrant",
        ROOT + "/CheckpointAndTail",
        ROOT + "/TenantIsolation",
    }
}

if __name__ == "__main__":
    with open(sys.argv[1], encoding="utf-8") as source:
        verify((json.loads(line) for line in source), required=REQUIRED)
    print("All five tv-sync role/recovery cases passed in one complete package")
