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

    def test_exact_quantity_outcomes_are_mandatory(self):
        aggregate = REQUIRED["services/oms/tools/aggregate-conformance"]
        for case in ("sequence-10", "domain--60", "domain-60", "refusal-ApplyFill",
                     "refusal-Amend", "refusal-filled-sum", "refusal-OVERFILL"):
            self.assertIn("TestRustOrderAggregateDifferential/exact-quantity-" + case, aggregate)
        self.assertIn("TestRustFillFoldProcessConformance/exact-quantity-sequence-10",
                      REQUIRED["services/oms/tools/fill-conformance"])

    def test_every_case_and_package_must_complete(self):
        for index in range(len(self.events())):
            events = self.events()
            events.pop(index)
            with self.assertRaises(ValueError):
                verify(events, REQUIRED)

    def test_position_precision_requires_both_real_owners_and_control_path(self):
        cases = REQUIRED["services/oms/tools/store-conformance"]
        for owner in ("go", "rust"):
            for name in ("TestRustPositionPrecisionPostgresNATS/" + owner,
                         "TestRustPositionPrecisionTransactions/" + owner + "/finite-overflow-rolls-back",
                         "TestRustPositionPrecisionTransactions/" + owner + "/repeating-cost-keeps-executions",
                         "TestRustPositionPrecisionTransactions/" + owner + "/tiny-repeating-cost-refuses-snapshot"):
                self.assertIn(name, cases)

    def test_skips_and_failures_refuse(self):
        for index in range(len(self.events())):
            for action in ("skip", "fail"):
                events = self.events()
                events[index]["Action"] = action
                with self.assertRaises(ValueError):
                    verify(events, REQUIRED)


if __name__ == "__main__":
    unittest.main()
