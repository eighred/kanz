import copy
import unittest

from delivery_evidence import assess

HEAD = "a" * 40
MAIN = "b" * 40


def context(name, sha, status="SUCCESS", **details):
    return dict(id=name, sha=sha, status=status, **details)


def task(name, path, *, deps=(), resources=(), blockers=(), priority=1):
    return dict(id=name, owner=name, paths=[path], depends_on=deps,
                resources=resources, blockers=blockers, priority=priority)


def snapshot():
    pr = dict(number=1, sha=HEAD, base_sha=MAIN, merge_state="CLEAN", draft=False, inventory_complete=True,
              risk=0, depends_on=[], blockers=[], expected=[dict(id="go"), dict(id="julia")],
              observed=[context("go", HEAD), context("julia", HEAD)])
    return dict(main=dict(sha=MAIN, inventory_complete=True, expected=[dict(id="go")], observed=[context("go", MAIN)]),
                prs=[pr], verified_dependencies=[], tasks=[], active_tasks=[], builders=[])


class DeliveryEvidenceTests(unittest.TestCase):
    def test_selects_highest_priority_feasible_task_and_rechecks_holds(self):
        data = snapshot()
        high = task("high", "risk", blockers=["signed upstream input"], priority=0)
        medium = task("medium", "observability", priority=1)
        low = task("low", "docs", priority=2)
        data["tasks"] = [low, medium, high]
        self.assertEqual(assess(data)["plan"]["next_actionable"], "medium")
        high["blockers"] = []
        self.assertEqual(assess(data)["plan"]["next_actionable"], "high")
        for item in data["tasks"]:
            item["blockers"] = ["required check pending"]
        self.assertIsNone(assess(data)["plan"]["next_actionable"])

    def test_backlog_does_not_close_on_pr_link_or_incomplete_inventory(self):
        data = snapshot()
        issue = dict(number=42, status="ready for verification or closure",
                     priority="P1", component="infra", security_impact="unknown",
                     next_action="verify acceptance", updated_at="2026-08-01T00:00:00Z",
                     blocked_since="2026-11-09T00:00:00Z", active_prs=[11],
                     acceptance_documented=True)
        data["backlog"] = dict(as_of="2026-11-10T00:00:00Z", inventory_complete=True,
                               open_total=1, issues=[issue])
        result = assess(data)["issue_backlog"]
        self.assertEqual(result["awaiting_verification"], [42])
        self.assertEqual(result["closure_candidates"], [])
        self.assertEqual(result["issues"][0]["blocker_age_hours"], 24)
        self.assertTrue(result["issues"][0]["stale_review"])
        issue["closure_proof"] = dict(merge_sha=MAIN, verified_on_main=True,
                                      all_acceptance_passed=True, evidence="run/5")
        self.assertEqual(assess(data)["issue_backlog"]["closure_candidates"], [42])
        data["backlog"]["open_total"] = 2
        self.assertEqual(assess(data)["issue_backlog"]["closure_candidates"], [])

    def test_backlog_prioritizes_actionable_work_and_preserves_holds(self):
        data = snapshot()
        issues = [dict(number=n, status="ready to implement", priority=priority,
                       component="ci", security_impact="unknown", next_action="implement",
                       updated_at="2026-11-09T00:00:00Z", acceptance_documented=True,
                       unresolved_dependencies=deps)
                  for n, priority, deps in [(1, "P0", [9]), (2, "P2", []), (3, "P1", [])]]
        data["backlog"] = dict(as_of="2026-11-10T00:00:00Z", inventory_complete=True,
                               open_total=3, issues=issues)
        self.assertEqual(assess(data)["issue_backlog"]["top_five"], [3, 2])

    def test_required_green_cannot_hide_applicable_failure(self):
        data = snapshot()
        data["prs"][0]["observed"][1] = context("julia", HEAD, "FAILURE", kind="security",
                                               cause="Affected native library", evidence="job/1")
        report = assess(data)
        self.assertTrue(report["main"]["verified_checks"])
        pr = report["queue"][0]
        self.assertFalse(pr["candidate"])
        self.assertEqual(pr["checks"][1]["kind"], "security")
        self.assertEqual(len(pr["checks"][1]["next"]), 2)

    def test_exact_revision_expected_skips_and_unresolved_checks(self):
        for status, sha, skip, state in [
            ("SUCCESS", MAIN, None, "stale"),
            ("IN_PROGRESS", HEAD, None, "pending"),
            ("SKIPPED", HEAD, None, "failed"),
            ("SKIPPED", HEAD, "manual-only on this event", "expected-skip"),
        ]:
            with self.subTest(state=state):
                data = snapshot()
                pr = data["prs"][0]
                pr["expected"][1]["skip_reason"] = skip
                pr["observed"][1] = context("julia", sha, status)
                row = assess(data)["queue"][0]
                self.assertEqual(row["checks"][1]["state"], state)
                self.assertEqual(row["candidate"], state == "expected-skip")
        data = snapshot()
        data["main"]["observed"] = [context("go", HEAD)]
        self.assertFalse(assess(data)["main"]["verified_checks"])
        data["prs"][0]["observed"] += [context("go", HEAD)]
        self.assertEqual(assess(data)["queue"][0]["checks"][0]["state"], "ambiguous")

    def test_queue_orders_low_risk_and_respects_holds(self):
        data = snapshot()
        high = copy.deepcopy(data["prs"][0])
        high.update(number=2, risk=3, blockers=["operator hold"], depends_on=["integration"])
        data["prs"].insert(0, high)
        queue = assess(data)["queue"]
        self.assertEqual([row["number"] for row in queue], [1, 2])
        self.assertTrue(queue[0]["candidate"])
        self.assertEqual(queue[1]["reasons"], ["unverified-dependencies", "open-blockers"])

    def test_work_continues_around_hard_blocker_and_active_ownership(self):
        data = snapshot()
        data["active_tasks"] = [task("worker", "Kanz/Infra", resources=["fixture-db"])]
        data["tasks"] = [task("julia", "julia", blockers=["upstream release"]),
                         task("lake", "lake", deps=["julia"]),
                         task("deployment", r"kanz\infra\testnet"),
                         task("broker", "broker", resources=["fixture-db"]),
                         task("guard", "kanz/test/arch", priority=0),
                         task("guard2", "kanz/test/arch/guard_test.go"),
                         task("forge", ".agents")]
        plan = assess(data)["plan"]
        self.assertEqual(plan["next_actionable"], "guard")
        self.assertEqual(plan["independent"], ["guard", "forge"])
        self.assertEqual(plan["deferred"]["deployment"], [{"overlap": ["worker"]}])
        self.assertEqual(plan["deferred"]["broker"], [{"overlap": ["worker"]}])
        self.assertEqual(plan["deferred"]["guard2"], [{"overlap": ["guard"]}])
        self.assertEqual(plan["deferred"]["lake"], [{"dependencies": ["julia"]}])

    def test_builder_requires_observed_inputs_and_isolation(self):
        data = snapshot()
        builder = dict(id="wsl", existing_local=True, platform=True, capacity=True,
                       inputs=False, isolation=True, probe_evidence="local probe")
        data["builders"] = [builder]
        build = assess(data)["build"]
        self.assertIsNone(build["verified_local"])
        self.assertEqual(build["local_candidates"], [{"id": "wsl", "missing": ["inputs"]}])
        self.assertNotIn("AWS", build["next"])
        builder["inputs"] = True
        self.assertEqual(assess(data)["build"]["verified_local"], "wsl")

    def test_empty_inventory_cannot_be_healthy(self):
        data = snapshot()
        data["main"]["expected"] = []
        with self.assertRaises(ValueError):
            assess(data)

    def test_partial_inventory_cannot_be_green(self):
        data = snapshot()
        data["main"]["inventory_complete"] = False
        data["prs"][0]["inventory_complete"] = False
        report = assess(data)
        self.assertFalse(report["main"]["verified_checks"])
        self.assertEqual(report["queue"][0]["reasons"], ["incomplete-check-inventory"])

    def test_blocker_catalog_preserves_evidence_and_rejects_unknown_class(self):
        data = snapshot()
        data["blockers"] = {"runner": dict(kind="permissions", cause="Inventory denied", evidence="HTTP 403")}
        row = assess(data)["blockers"]["runner"]
        self.assertEqual(row["evidence"], "HTTP 403")
        self.assertEqual(len(row["next"]), 2)
        data["blockers"]["runner"]["kind"] = "probably-infrastructure"
        with self.assertRaises(ValueError):
            assess(data)

    def test_daily_progress_distinguishes_merge_from_exact_main_acceptance(self):
        data = snapshot()
        data["blockers"] = {"openssl": dict(kind="dependency", cause="Release missing",
                                          evidence="404", owner="release",
                                          blocked_since="2026-11-09T00:00:00Z")}
        data["period"] = dict(start="2026-11-10T00:00:00Z", end="2026-11-11T00:00:00Z",
                              merges=[dict(number=1, merge_sha=MAIN,
                                           opened_at="2026-11-09T00:00:00Z",
                                           merged_at="2026-11-10T12:00:00Z",
                                           main_verification=dict(sha=HEAD, acceptance_passed=True,
                                                                  verified_at="2026-11-10T13:00:00Z",
                                                                  evidence="run/1"))])
        report = assess(data)["daily_progress"]
        self.assertEqual(report["merged"], [1])
        self.assertEqual(report["verified_on_main"], [])
        self.assertEqual(report["median_pr_cycle_hours"], 36)
        self.assertEqual(report["open_blockers"]["openssl"], {"owner": "release", "age_hours": 48})
        data["period"]["merges"][0]["main_verification"]["sha"] = MAIN
        self.assertEqual(assess(data)["daily_progress"]["verified_on_main"], [1])


if __name__ == "__main__":
    unittest.main()
