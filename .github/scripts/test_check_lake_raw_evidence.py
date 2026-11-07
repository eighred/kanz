"""Missing, skipped, failed or incomplete broker proofs refuse."""
import unittest
from check_migration_evidence import PREFIX, verify
from check_lake_raw_evidence import REQUIRED

class RawEvidenceTests(unittest.TestCase):
    def events(self):
        return [event for package, names in REQUIRED.items() for event in [
            *({"Action": "pass", "Package": PREFIX + package, "Test": name}
              for name in sorted(names)),
            {"Action": "pass", "Package": PREFIX + package}]]

    def test_complete(self):
        self.assertEqual(sum(map(len, REQUIRED.values())), 7)
        verify(self.events(), required=REQUIRED)

    def test_each_case_and_completion_required(self):
        events = self.events()
        for index in range(len(events)):
            with self.subTest(index=index), self.assertRaises(ValueError):
                verify(events[:index] + events[index+1:], required=REQUIRED)

    def test_skip_or_failure_refuses(self):
        for action in ("skip", "fail"):
            with self.subTest(action=action), self.assertRaises(ValueError):
                verify(self.events() + [{"Action": action}], required=REQUIRED)

    def test_handoff_skip_refuses(self):
        for name in ("TestRawRestartReadPhaseHandoff", "TestRawCrashRecoveryPhaseHandoff"):
            events = self.events()
            for event in events:
                if event.get("Test") == name:
                    event["Action"] = "skip"
            with self.subTest(name=name), self.assertRaises(ValueError):
                verify(events, required=REQUIRED)
