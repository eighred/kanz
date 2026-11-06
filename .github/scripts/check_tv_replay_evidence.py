"""Require the restored-state verifier's real PostgreSQL qualification cases."""
import json
import sys

from check_migration_evidence import verify

REQUIRED = {"services/tv-sync/internal/projection": {
    "TestReplayVerifierPostgresRestoredSnapshot",
    "TestReplayVerifierPostgresSnapshotIsolationAndReadOnly",
    "TestReplayVerifierPostgresRefusesEmptyCorruptAndOversized",
    "TestReplayVerifierPostgresBoundsTextBeforeReplay",
    "TestReplayVerifierPostgresRefusesPermissiveRLS",
}, "pkg/secret": {
    "TestReadBounded",
    "TestReadBoundedRejectsFIFOAndPreservesCSISymlinks",
}}

if __name__ == "__main__":
    if sys.argv[1] == "--pattern":
        print("^(" + "|".join(sorted(set.union(*REQUIRED.values()))) + ")$")
    else:
        with open(sys.argv[1], encoding="utf-8") as source:
            verify((json.loads(line) for line in source), required=REQUIRED)
        print("All 5 PostgreSQL verifier and 2 bounded-secret cases passed in 2 complete packages")
