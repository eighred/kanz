"""Require actual raw Kafka custody, crash recovery and unchanged parking cases."""
import json
import sys
from check_migration_evidence import verify

REQUIRED = {"services/lake-sink/internal/cdc": {
    "TestRawCrashRecoveryPhaseHandoff",
    "TestRawRestartReadPhaseHandoff",
    "TestRawKafkaCustodyAndOffsets",
    "TestRawKafkaCustodyAndOffsets/process-crash",
    "TestRawKafkaCustodyAndOffsets/flush-failure",
    "TestRawKafkaCustodyAndOffsets/flush-failure-dlq-failure",
    "TestRawKafkaCustodyAndOffsets/flush-failure-dlq-success",
}}

if __name__ == "__main__":
    with open(sys.argv[1], encoding="utf-8") as source:
        verify((json.loads(line) for line in source), required=REQUIRED)
    print("All four actual Kafka custody cases and both phase-handoff regressions passed in one complete package")
