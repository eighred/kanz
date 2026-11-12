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

    def inventory_api(self, *, issues=None, prs=None, total=None):
        def api(path):
            result = self.api(path)
            if '/issues?' in path and issues is not None:
                return copy.deepcopy(issues)
            if '/pulls?' in path and prs is not None:
                return [copy.deepcopy(prs)]
            if path.endswith('/kanz') and total is not None:
                result[0]['open_issues_count'] = total
            return result
        return api

    def test_ordinary_only_and_mixed_issue_responses_preserve_the_same_evidence(self):
        mixed = self.api('repos/eighred/kanz/issues?state=open&per_page=100')
        ordinary = [[], mixed[1]]
        expected = collect('eighred/kanz', self.api, self.contract())
        actual = collect('eighred/kanz', self.inventory_api(issues=ordinary), self.contract())
        for result in (expected, actual):
            result.pop('collected_at')
        self.assertEqual(actual, expected)
        self.assertEqual(set(actual['observations']), {'main', 'pr:1'})
        self.assertEqual(actual['assessment']['nodes']['feature']['state'], 'BLOCKED')

    def test_empty_inventory_is_valid(self):
        result = collect('eighred/kanz', self.inventory_api(issues=[[]], prs=[], total=0))
        self.assertEqual(result['issues'], [])
        self.assertEqual(result['prs'], [])
        self.assertEqual(set(result['observations']), {'main'})

    def test_missing_issue_or_pr_cannot_be_hidden_by_another_endpoint(self):
        mixed = self.api('repos/eighred/kanz/issues?state=open&per_page=100')
        for name, issues, prs in (
                ('missing ordinary issue', [mixed[0]], [self.pr]),
                ('missing PR from both sources', [mixed[1]], []),
                ('PR listed as issue but missing head', mixed, [])):
            with self.subTest(name=name), self.assertRaises(ValueError):
                collect('eighred/kanz', self.inventory_api(issues=issues, prs=prs))

    def test_duplicate_or_conflicting_inventory_identities_refuse(self):
        mixed = self.api('repos/eighred/kanz/issues?state=open&per_page=100')
        ordinary = mixed[1][0]
        conflict = copy.deepcopy(ordinary)
        conflict['number'] = self.pr['number']
        for name, issues, prs, total in (
                ('duplicate ordinary issue', mixed + [[ordinary]], [self.pr], 3),
                ('duplicate issue-side PR', mixed + [mixed[0]], [self.pr], 3),
                ('duplicate pull request', mixed, [self.pr, self.pr], 2),
                ('ordinary issue conflicts with PR', [[conflict], [ordinary]], [self.pr], 2)):
            with self.subTest(name=name), self.assertRaises(ValueError):
                collect('eighred/kanz', self.inventory_api(issues=issues, prs=prs, total=total))

    def test_invalid_inventory_numbers_and_metadata_counts_refuse(self):
        mixed = self.api('repos/eighred/kanz/issues?state=open&per_page=100')
        for number in (True, 0, -1, '2', None):
            issues = copy.deepcopy(mixed)
            issues[1][0]['number'] = number
            with self.subTest(number=number), self.assertRaises(ValueError):
                collect('eighred/kanz', self.inventory_api(issues=issues))
            pr = copy.deepcopy(self.pr)
            pr['number'] = number
            with self.subTest(pr_number=number), self.assertRaises(ValueError):
                collect('eighred/kanz', self.inventory_api(prs=[pr]))
        for total in (True, -1, '2', 2.0):
            with self.subTest(total=total), self.assertRaises(ValueError):
                collect('eighred/kanz', self.inventory_api(total=total))
        for marker in (None, {}, 'pr/1'):
            issues = copy.deepcopy(mixed)
            issues[0][0]['pull_request'] = marker
            with self.subTest(marker=marker), self.assertRaises(ValueError):
                collect('eighred/kanz', self.inventory_api(issues=issues))

    def test_changing_issue_identity_or_metadata_requires_recollection(self):
        for change in ('issue', 'metadata', 'default branch'):
            self.calls = []
            def api(path):
                result = self.api(path)
                if self.calls.count(path) > 1:
                    if change == 'issue' and '/issues?' in path:
                        result[1][0]['number'] = 3
                    elif change == 'metadata' and path.endswith('/kanz'):
                        result[0]['open_issues_count'] = 3
                    elif change == 'default branch' and path.endswith('/kanz'):
                        result[0]['default_branch'] = 'release'
                return result
            with self.subTest(change=change), self.assertRaises(ValueError):
                collect('eighred/kanz', api)

    def test_changing_pr_head_or_duplicate_final_pr_requires_recollection(self):
        for change in ('head', 'base', 'duplicate'):
            self.calls = []
            def api(path):
                result = self.api(path)
                if '/pulls?' in path and self.calls.count(path) > 1:
                    if change == 'head':
                        result[0][0]['head']['sha'] = 'c' * 40
                    elif change == 'base':
                        result[0][0]['base']['sha'] = 'c' * 40
                    else:
                        result[0].append(copy.deepcopy(result[0][0]))
                return result
            with self.subTest(change=change), self.assertRaises(ValueError):
                collect('eighred/kanz', api)

    def test_invalid_pr_revision_cannot_enter_the_snapshot(self):
        for field in ('head', 'base'):
            for ref in (None, {}, {'sha': 'short'}, {'sha': True}):
                pr = copy.deepcopy(self.pr)
                pr[field] = ref
                with self.subTest(field=field, ref=ref), self.assertRaises(ValueError):
                    collect('eighred/kanz', self.inventory_api(prs=[pr]))

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
