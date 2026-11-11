import copy
import unittest

from validation_graph import assess_graph

SHA = "a" * 40
MAIN = "b" * 40


def gate(key="julia", status="FAILURE", **kwargs):
    return dict(id=key, kind="gate", revision=SHA, critical=True,
                validation=dict(sha=SHA, inventory_complete=True, inventory_evidence="reviewed workflow/source",
                                expected=[dict(id="test")],
                                observed=[dict(id="test", sha=SHA, status=status, evidence="job/1")]), **kwargs)


def task(key, **kwargs):
    return dict(id=key, kind="task", scope_evidence="reviewed callers and validation scope",
                stage=dict(state="READY", evidence="issue acceptance and source reviewed"), **kwargs)


def edge(source, target, kind="requires"):
    return dict(**{"from": source, "to": target}, kind=kind, evidence="workflow/path:10")


def fixture():
    return dict(nodes=[gate(), gate("forge", "SUCCESS"), task("feature", priority=0),
                       task("repair", priority=2), task("independent", priority=1)],
                edges=[edge("feature", "julia"), edge("independent", "forge"), edge("repair", "julia", "recovers")])


class GraphTests(unittest.TestCase):
    def test_shared_failure_blocks_delivery_but_not_independent_work(self):
        result = assess_graph(fixture())
        self.assertEqual(result["nodes"]["julia"]["state"], "VALIDATION_FAILED")
        self.assertEqual(result["nodes"]["feature"]["state"], "BLOCKED")
        self.assertEqual(result["nodes"]["feature"]["root_blockers"], ["julia"])
        self.assertEqual(result["nodes"]["independent"]["state"], "READY")
        self.assertIn("feature", result["execution_only"])

    def test_critical_recovery_precedes_higher_priority_unrelated_feature(self):
        result = assess_graph(fixture())
        self.assertEqual(result["next_actionable"], "repair")
        self.assertIn("critical", result["nodes"]["repair"]["selection_reason"])

    def test_transitive_runtime_and_pr_dependencies_propagate(self):
        data = fixture()
        runtime = dict(id="openssl", kind="runtime", holds=[dict(reason="fixed archive unavailable", evidence="release listing")])
        data["nodes"].append(runtime)
        data["edges"].append(edge("julia", "openssl"))
        data["nodes"].append(dict(id="pr:1", kind="pr"))
        data["edges"].append(edge("pr:1", "julia"))
        result = assess_graph(data)
        self.assertEqual(result["nodes"]["pr:1"]["state"], "BLOCKED")
        self.assertIn("openssl", result["nodes"]["pr:1"]["root_blockers"])
        self.assertIn("julia", result["nodes"]["pr:1"]["root_blockers"])

    def test_recovered_gate_reassesses_queue_and_records_transition(self):
        data = fixture()
        before = assess_graph(data)
        data["previous_states"] = {k: row["state"] for k, row in before["nodes"].items()}
        data["nodes"][0]["validation"]["observed"][0]["status"] = "SUCCESS"
        after = assess_graph(data)
        self.assertEqual(after["next_actionable"], "feature")
        self.assertEqual(after["nodes"]["feature"]["state"], "READY")
        self.assertTrue(after["reassessment_required"])
        self.assertTrue(all(row["evidence"] for row in after["transitions"]))

    def test_no_deliverable_without_inventory_revision_review_and_integration(self):
        base = dict(nodes=[dict(gate(status="SUCCESS"), kind="pr", review=dict(passed=True, sha=SHA, evidence="review/1"),
                               integration=dict(base_sha=MAIN, head_sha=SHA, evidence="merge-test/1"))],
                    edges=[], main_revision=MAIN)
        self.assertEqual(assess_graph(base)["nodes"]["julia"]["state"], "DELIVERABLE")
        for mutation in ("inventory", "revision", "review", "integration", "missing", "duplicate", "cancelled", "skip"):
            data = copy.deepcopy(base)
            node = data["nodes"][0]
            if mutation == "inventory": node["validation"]["inventory_complete"] = False
            elif mutation == "revision": node["validation"]["observed"][0]["sha"] = "c" * 40
            elif mutation == "review": node["review"]["sha"] = "c" * 40
            elif mutation == "integration": data["main_revision"] = "c" * 40
            elif mutation == "missing": node["validation"]["observed"] = []
            elif mutation == "duplicate": node["validation"]["observed"] *= 2
            elif mutation == "cancelled": node["validation"]["observed"][0]["status"] = "CANCELLED"
            else: node["validation"]["observed"][0]["status"] = "SKIPPED"
            with self.subTest(mutation=mutation):
                self.assertNotEqual(assess_graph(data)["nodes"]["julia"]["state"], "DELIVERABLE")

    def test_pending_unavailable_and_in_progress_are_distinct(self):
        data = fixture()
        data["nodes"][0]["validation"]["observed"][0]["status"] = "IN_PROGRESS"
        data["nodes"][3]["stage"]["state"] = "IN_PROGRESS"
        result = assess_graph(data)
        self.assertEqual(result["nodes"]["julia"]["state"], "VALIDATION_PENDING")
        self.assertEqual(result["nodes"]["repair"]["state"], "IN_PROGRESS")
        data["nodes"][0]["validation"]["observed"] = []
        self.assertEqual(assess_graph(data)["nodes"]["julia"]["state"], "BLOCKED")

    def test_execution_prerequisite_prevents_dispatch(self):
        data = fixture()
        data["edges"].append(edge("repair", "julia", "execution-requires"))
        result = assess_graph(data)
        self.assertFalse(result["nodes"]["repair"]["execution_ready"])
        self.assertEqual(result["next_actionable"], "independent")

    def test_completed_recovery_held_by_another_gate_is_not_selected_again(self):
        data = fixture()
        data['nodes'].append(gate('upstream'))
        data['edges'].append(edge('repair', 'upstream'))
        result = assess_graph(data)
        self.assertEqual(result['nodes']['repair']['state'], 'BLOCKED')
        self.assertEqual(result['next_actionable'], 'independent')
        self.assertIn('repair', result['execution_only'])

    def test_root_grouping_requires_consistent_source_evidence(self):
        data = fixture()
        cause = dict(id="openssl:3.5.9", kind="security", evidence="advisory artifact")
        data["nodes"][0]["failure"] = cause
        data["nodes"].append(gate("other", failure=cause))
        self.assertEqual(len(assess_graph(data)["failure_groups"][cause["id"]]["nodes"]), 2)
        data["nodes"][-1]["failure"] = dict(cause, kind="code")
        with self.assertRaises(ValueError): assess_graph(data)

    def test_cycle_unknown_edge_and_unreviewed_scope_fail_closed(self):
        for mutate in (lambda d: d["edges"].append(edge("julia", "feature")),
                       lambda d: d["edges"].append(edge("feature", "missing"))):
            data = fixture(); mutate(data)
            with self.assertRaises(ValueError): assess_graph(data)
        data = fixture()
        del data["nodes"][3]["scope_evidence"]
        self.assertNotEqual(assess_graph(data)["next_actionable"], "repair")


if __name__ == "__main__":
    unittest.main()
