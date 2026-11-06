import io
import json
from pathlib import Path
import re
import unittest

from go_test_evidence import BACKING, BROWSER_PROOF, check


def event(action, test="", output="", package="example/store"):
    return json.dumps({"Action": action, "Package": package, "Test": test, "Output": output}) + "\n"


def suite(*records):
    return io.StringIO("".join(records) + event("output", "TestBrowser", BROWSER_PROOF) + event("pass"))


class EvidenceTests(unittest.TestCase):
    def test_reason_precedes_skip(self):
        reason = "    store_test.go:12: set TEST_POSTGRES_URL\n"
        marker = "--- SKIP: TestPostgres (0.00s)\n"
        old_after_context = marker + "PASS\nok example/store\n"
        self.assertFalse(BACKING.search(old_after_context))
        problems, skips, packages = check(suite(
            event("output", "TestPostgres", reason),
            event("output", "TestPostgres", marker),
            event("skip", "TestPostgres"),
        ))
        self.assertIn("TEST_POSTGRES_URL", problems[0])
        self.assertEqual((skips, packages), (1, 1))

    def test_parallel_and_nested_tests_keep_their_own_reasons(self):
        problems, skips, _ = check(suite(
            event("output", "TestParent/real", "set TEST_NATS_URL"),
            event("output", "TestOther", "unavailable optional hardware"),
            event("skip", "TestOther"),
            event("skip", "TestParent/real"),
            event("pass", "TestParent"),
        ))
        self.assertEqual(skips, 2)
        self.assertEqual(len(problems), 1)
        self.assertIn("TestParent/real", problems[0])

    def test_passing_mentions_do_not_condemn_an_unrelated_skip(self):
        problems, skips, _ = check(suite(
            event("output", "TestReal", "TEST_POSTGRES_URL configured"),
            event("pass", "TestReal"),
            event("skip", "TestOptional"),
        ))
        self.assertEqual(problems, [])
        self.assertEqual(skips, 1)

    def test_equal_test_names_in_distinct_packages_remain_separate(self):
        problems, _, _ = check(suite(
            event("output", "TestReal", "TEST_NATS_URL configured", "other/package"),
            event("pass", "TestReal", package="other/package"),
            event("skip", "TestReal"),
        ))
        self.assertEqual(problems, [])

    def test_all_supplied_dependencies_are_checked(self):
        for variable in ("TEST_POSTGRES_URL", "TEST_NATS_URL", "TEST_TV_NATS_URL", "TEST_KAFKA_BROKERS", "TEST_REDIS_URL",
                         "TEST_NATS_MTLS_URL", "TEST_NATS_MTLS_CERT_DIR", "TEST_MFA_BROWSER_SCRIPT"):
            with self.subTest(variable=variable):
                problems, _, _ = check(suite(event("output", "TestReal", "set " + variable), event("skip", "TestReal")))
                self.assertIn(variable, problems[0])

    def test_missing_or_invalid_results_fail_closed(self):
        for text in ("", "not JSON\n", "[]\n", "{}\n", event("run", "TestUnfinished")):
            with self.subTest(text=text), self.assertRaises(ValueError):
                check(io.StringIO(text))

    def test_every_supplied_race_dependency_is_recognized(self):
        workflow = Path(__file__).resolve().parents[1] / "workflows" / "kanz-ci.yml"
        race_step = workflow.read_text(encoding="utf-8").split("- name: Test (race)", 1)[1].split("# -p 1 IS REQUIRED", 1)[0]
        supplied = re.findall(r"^\s+(TEST_[A-Z_]+):", race_step, re.MULTILINE)
        self.assertGreaterEqual(len(supplied), 10)
        for variable in supplied:
            with self.subTest(variable=variable):
                self.assertIsNotNone(BACKING.fullmatch(variable))

    def test_browser_proof_is_still_required(self):
        problems, _, _ = check(io.StringIO(event("pass")))
        self.assertIn("shared-session Chromium proof did not run", problems)

    def test_cached_results_do_not_count_as_fresh_service_proof(self):
        problems, _, _ = check(suite(event("output", output="ok  example/store  (cached)\n")))
        self.assertIn("cached package result is not fresh verification", problems[0])

    def test_partial_and_unexpected_package_sets_are_rejected(self):
        problems, _, _ = check(suite(), ["example/store", "example/missing"])
        self.assertEqual(problems, ["package result missing: example/missing"])
        problems, _, _ = check(suite(), ["example/other"])
        self.assertIn("unexpected package result: example/store", problems)
        with self.assertRaises(ValueError):
            check(suite(), [])

    def test_complete_inventory_includes_packages_without_tests(self):
        records = suite(event("skip", package="example/config"))
        problems, skips, count = check(records, ["example/store\n", "example/config\n"])
        self.assertEqual((problems, skips, count), ([], 1, 2))


if __name__ == "__main__":
    unittest.main()
