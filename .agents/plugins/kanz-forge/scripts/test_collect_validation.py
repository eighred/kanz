import copy
import json
import subprocess
import unittest
from unittest.mock import patch

from collect_validation import collect, github
from test_validation_graph import SHA, MAIN, edge, gate, task


class CollectorTests(unittest.TestCase):
    def setUp(self):
        self.calls = []
        self.pr = dict(number=1, head=dict(sha=SHA), base=dict(sha=MAIN), draft=False, html_url="pr/1")

    def api(self, path):
        self.calls.append(path)
        if path.endswith('/kanz'):
            return [dict(default_branch="main", open_issues_count=2)]
        if '/branches/' in path:
            return [dict(commit=dict(sha=MAIN))]
        if '/pulls?' in path:
            return [[copy.deepcopy(self.pr)]]
        if '/issues?' in path:
            return [[dict(number=1, title="PR", html_url="pr/1", labels=[], pull_request={"url":"pr/1"})],
                    [dict(number=2, title="Recovery", html_url="issue/2", labels=[dict(name="P1")])]]
        if '/check-runs?' in path:
            sha = path.split('/commits/')[1].split('/')[0]
            return [dict(total_count=1, check_runs=[dict(name="test", id=12, head_sha=sha,
                            status="completed", conclusion="failure" if sha == SHA else "success", html_url="job/12")])]
        raise AssertionError(path)

    def contract(self):
        node = gate()
        node["subject"] = "pr:1"
        return dict(main_revision=MAIN, nodes=[node, task("repair"), task("feature")],
                    edges=[edge("feature", "julia"), edge("repair", "julia", "recovers")])

    def test_contract_must_bind_current_main(self):
        for main in (None, SHA):
            contract = self.contract()
            contract["main_revision"] = main
            with self.subTest(main=main), self.assertRaisesRegex(ValueError, "main revision"):
                collect("eighred/kanz", self.api, contract)

    def test_unbound_historical_pr_and_gate_checks_refuse(self):
        # Historical green checks must not survive a changed live PR head.
        self.pr["head"]["sha"] = "c" * 40
        for kind in ("pr", "gate"):
            node = dict(gate(status="SUCCESS"), kind=kind,
                        review=dict(passed=True, sha=SHA, evidence="historical review"),
                        integration=dict(base_sha=MAIN, head_sha=SHA, evidence="historical integration"))
            contract = dict(main_revision=MAIN, nodes=[node], edges=[])
            with self.subTest(kind=kind), self.assertRaisesRegex(ValueError, "live GitHub subject"):
                collect("eighred/kanz", self.api, contract)

    def test_pr_cannot_use_main_as_its_live_binding(self):
        contract = self.contract()
        contract["nodes"][0].update(kind="pr", subject="main", revision=MAIN)
        contract["nodes"] = contract["nodes"][:1]
        contract["edges"] = []
        with self.assertRaisesRegex(ValueError, "live PR subject"):
            collect("eighred/kanz", self.api, contract)

    def test_live_collection_to_graph_blocks_feature_and_selects_repair(self):
        result = collect("eighred/kanz", self.api, self.contract())
        self.assertEqual(len(result["issues"]), 1)
        self.assertEqual(result["assessment"]["nodes"]["feature"]["state"], "BLOCKED")
        self.assertEqual(result["assessment"]["next_actionable"], "repair")
        self.assertEqual(result["observations"]["main"]["sha"], MAIN)

    def test_unavailable_checks_do_not_become_green(self):
        def api(path):
            if '/check-runs?' in path: raise ValueError("access denied")
            return self.api(path)
        result = collect("eighred/kanz", api, self.contract())
        self.assertFalse(result["observations"]["pr:1"]["available"])
        self.assertEqual(result["assessment"]["nodes"]["julia"]["state"], "BLOCKED")

    def test_stale_contract_and_truncated_inventory_refuse(self):
        contract = self.contract()
        contract["nodes"][0]["revision"] = MAIN
        with self.assertRaisesRegex(ValueError, "stale"):
            collect("eighred/kanz", self.api, contract)
        def api(path):
            if path.endswith('/kanz'): return [dict(default_branch="main", open_issues_count=3)]
            return self.api(path)
        with self.assertRaisesRegex(ValueError, "inventory"):
            collect("eighred/kanz", api)

    def test_main_or_pr_moving_requires_recollection(self):
        def api(path):
            count = self.calls.count(path)
            result = self.api(path)
            if '/branches/' in path and count: result[0]['commit']['sha'] = SHA
            return result
        with self.assertRaisesRegex(ValueError, "main moved"):
            collect("eighred/kanz", api)

    def test_cli_only_uses_read_only_paginated_calls(self):
        process = subprocess.CompletedProcess([], 0, json.dumps([{"ok": True}]), "")
        with patch('collect_validation.subprocess.run', return_value=process) as run:
            self.assertEqual(github('repos/eighred/kanz'), [{"ok": True}])
        args = run.call_args.args[0]
        self.assertEqual(args[:4], ['gh', 'api', '--method', 'GET'])
        self.assertIn('--paginate', args)
        self.assertIn('--slurp', args)
        self.assertNotIn('shell', run.call_args.kwargs)


if __name__ == "__main__":
    unittest.main()
