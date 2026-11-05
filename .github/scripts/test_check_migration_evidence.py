import unittest

from check_migration_evidence import PREFIX, REQUIRED, verify


def successful_events():
    return [
        {"Action": "pass", "Package": PREFIX + package, "Test": name}
        for package, names in REQUIRED.items()
        for name in [*sorted(names), ""]
    ]


class EvidenceTest(unittest.TestCase):
    def test_complete(self):
        verify(successful_events())

    def test_truncated_package_and_missing_case(self):
        for index in range(len(successful_events())):
            events = successful_events()
            events.pop(index)
            with self.assertRaises(ValueError):
                verify(events)

    def test_skip_or_failure_is_not_proof(self):
        for action in ("skip", "fail"):
            events = successful_events()
            events[0]["Action"] = action
            with self.assertRaises(ValueError):
                verify(events)

    def test_empty_is_not_proof(self):
        with self.assertRaises(ValueError):
            verify([])


if __name__ == "__main__":
    unittest.main()
