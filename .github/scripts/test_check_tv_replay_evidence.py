"""Missing, skipped and unfinished restored-state evidence must refuse."""
import unittest
from check_migration_evidence import PREFIX, verify
from check_tv_replay_evidence import REQUIRED

class ReplayEvidenceTests(unittest.TestCase):
    def events(self):
        return [event for package, names in REQUIRED.items() for event in [
            *({"Action": "pass", "Package": PREFIX + package, "Test": name}
              for name in sorted(names)),
            {"Action": "pass", "Package": PREFIX + package}]]

    def test_complete(self):
        self.assertEqual(sum(map(len, REQUIRED.values())), 10)
        self.assertEqual(len(REQUIRED), 2)
        verify(self.events(), required=REQUIRED)

    def test_each_case_and_completion_required(self):
        events = self.events()
        for index in range(len(events)):
            with self.assertRaises(ValueError):
                verify(events[:index] + events[index+1:], required=REQUIRED)

    def test_skip_or_failure_refuses(self):
        for action in ("skip", "fail"):
            with self.assertRaises(ValueError):
                verify(self.events() + [{"Action": action}], required=REQUIRED)
