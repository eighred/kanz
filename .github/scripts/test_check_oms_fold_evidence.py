"""Missing, skipped or failed real transaction cases cannot qualify a release."""
import unittest

from check_migration_evidence import PREFIX, verify
from check_oms_fold_evidence import REQUIRED


class OMSEvidenceTests(unittest.TestCase):
    def events(self):
        return [{"Action": "pass", "Package": PREFIX + package, "Test": name}
                for package, cases in REQUIRED.items() for name in [*sorted(cases), ""]]

    def test_complete(self):
        verify(self.events(), REQUIRED)

    def test_every_case_and_package_must_complete(self):
        for index in range(len(self.events())):
            events = self.events()
            events.pop(index)
            with self.assertRaises(ValueError):
                verify(events, REQUIRED)

    def test_skips_and_failures_refuse(self):
        for index in range(len(self.events())):
            for action in ("skip", "fail"):
                events = self.events()
                events[index]["Action"] = action
                with self.assertRaises(ValueError):
                    verify(events, REQUIRED)


if __name__ == "__main__":
    unittest.main()
