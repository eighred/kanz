import io
import json
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
        for variable in ("TEST_POSTGRES_URL", "TEST_NATS_URL", "TEST_KAFKA_BROKERS", "TEST_REDIS_URL",
                         "TEST_NATS_MTLS_URL", "TEST_NATS_MTLS_CERT_DIR", "TEST_MFA_BROWSER_SCRIPT"):
            with self.subTest(variable=variable):
                problems, _, _ = check(suite(event("output", "TestReal", "set " + variable), event("skip", "TestReal")))
                self.assertIn(variable, problems[0])

    def test_missing_or_invalid_results_fail_closed(self):
        for text in ("", "not JSON\n", "[]\n", "{}\n", event("run", "TestUnfinished")):
            with self.subTest(text=text), self.assertRaises(ValueError):
                check(io.StringIO(text))

    def test_browser_proof_is_still_required(self):
        problems, _, _ = check(io.StringIO(event("pass")))
        self.assertIn("shared-session Chromium proof did not run", problems)


if __name__ == "__main__":
    unittest.main()
