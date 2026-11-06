"""The TVSync inventory must be complete; use the shared verifier's semantics."""
import unittest

from check_migration_evidence import PREFIX, verify
from check_tv_sync_evidence import REQUIRED


class TVSyncEvidenceTests(unittest.TestCase):
    def events(self):
        return [
            event
            for package, names in REQUIRED.items()
            for event in [
                *({"Action": "pass", "Package": PREFIX + package, "Test": name}
                  for name in sorted(names)),
                {"Action": "pass", "Package": PREFIX + package},
            ]
        ]

    def test_complete_inventory(self):
        self.assertEqual(sum(map(len, REQUIRED.values())), 5)
        self.assertEqual(len(REQUIRED), 1)
        verify(self.events(), required=REQUIRED)

    def test_each_case_and_package_completion_is_required(self):
        events = self.events()
        for index in range(len(events)):
            with self.subTest(event=events[index]):
                with self.assertRaises(ValueError):
                    verify(events[:index] + events[index + 1:], required=REQUIRED)

    def test_skip_and_failure_are_not_evidence(self):
        for action in ("skip", "fail"):
            with self.assertRaises(ValueError):
                verify(self.events() + [{"Action": action}], required=REQUIRED)


if __name__ == "__main__":
    unittest.main()
