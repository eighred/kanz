"""Require the existing real-broker compatibility cases, not merely a green exit."""
import json
import sys

PREFIX = "github.com/eighred/kanz/"
REQUIRED = {
    "pkg/bus": {
        "TestKafkaPublishSubscribe",
        "TestBacklogPollerReportsKafkaLag",
        "TestKafkaBacklogRefusesToReportZeroForAnUnreachableBroker",
        "TestIntegration_KafkaHandlerFailureWithDeadDLQKeepsTheMessageReachable",
    },
    "services/archiver/internal/archive": {
        "TestArchiver_LandsEveryEventInOrder",
        "TestArchiver_KafkaOutageLosesNothing",
        "TestArchiver_AckBeforeProduceLosesEvents_RedTwin",
        "TestArchiver_UnprovisionedTenantNacksAndDoesNotCrossFile",
        "TestDrain_ReArchivesAParkedEventOntoItsRealTopic",
        "TestDrain_DryRunPublishesNothingToTheRealTopic",
    },
    "tools/replay": {"TestReaderRange"},
    "services/lake-sink/internal/cdc": {
        "TestCrash_UnflushedRowsAreLostAndOffsetsAreCommitted",
    },
}


def verify(events, required=None):
    if required is None:
        required = REQUIRED
    passed = set()
    completed = set()
    for event in events:
        action, package = event["Action"], event.get("Package", "")
        test = event.get("Test", "")
        if action in ("fail", "skip"):
            raise ValueError(f"migration proof {action}: {package}/{test}")
        if action == "pass":
            if test:
                passed.add((package, test))
            else:
                completed.add(package)
    for package, names in required.items():
        full = PREFIX + package
        if full not in completed:
            raise ValueError(f"missing completed package: {full}")
        for name in sorted(names):
            if (full, name) not in passed:
                raise ValueError(f"missing executed proof: {full}/{name}")


if __name__ == "__main__":
    with open(sys.argv[1], encoding="utf-8") as source:
        verify(json.loads(line) for line in source)
    print("All 12 required Redpanda compatibility cases passed in 4 packages")
