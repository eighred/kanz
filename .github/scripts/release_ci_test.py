"""Offline API fixtures; never dispatch, publish, or read real credentials."""
import copy
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import release_ci as gate
import release_source

SHA = "a" * 40


def run(number=2, attempt=1):
    return {"id": number, "run_number": number, "run_attempt": attempt,
            "workflow_id": 42, "path": ".github/workflows/kanz-ci.yml",
            "head_sha": SHA, "head_branch": "main", "event": "push",
            "repository": {"id": 123, "full_name": gate.REPO},
            "head_repository": {"id": 123, "full_name": gate.REPO},
            "status": "completed", "conclusion": "success"}


class API:
    def __init__(self, filename="kanz-ci.yml"):
        self.filename = filename
        self.workflow = {"id": 42, "path": ".github/workflows/" + filename, "state": "active",
                         "url": gate.API + "/actions/workflows/42"}
        self.runs = [run()]
        self.current = run()
        self.runs[0]["path"] = self.current["path"] = self.workflow["path"]
        self.jobs = [{"id": 50, "run_id": 2, "run_attempt": 1, "head_sha": SHA,
                      "name": "kanz-ci-required", "status": "completed", "conclusion": "success"}]
        self.calls = []
        self.jobs = [{**self.jobs[0], "id": 50 + i, "name": name}
                     for i, name in enumerate(sorted(gate.POLICY[filename]))]
        self.page_size = 100

    def get(self, path, **query):
        self.calls.append((path, query))
        if not path:
            return {"id": 123, "full_name": gate.REPO}
        if path == "/actions/workflows/" + self.filename:
            return copy.deepcopy(self.workflow)
        if path == "/actions/workflows/42/runs":
            assert query["head_sha"] == SHA and query["branch"] == "main"
            items, key = self.runs, "workflow_runs"
        elif path == "/actions/runs/2":
            return copy.deepcopy(self.current)
        elif path == f"/actions/runs/2/attempts/{self.current['run_attempt']}/jobs":
            items, key = self.jobs, "jobs"
        else:
            raise gate.EvidenceError("workflow evidence missing")
        start = (query["page"] - 1) * self.page_size
        return {"total_count": len(items), key: copy.deepcopy(items[start:start+self.page_size])}


class EvidenceTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.root = Path(self.directory.name)
        (self.root / ".github/workflows").mkdir(parents=True)
        for filename in gate.MANDATORY:
            (self.root / ".github/workflows" / filename).write_text("fixture")
        self.required = {"kanz-ci.yml": gate.POLICY["kanz-ci.yml"]}
        self.api = API()

    def verify(self):
        with patch.object(gate, "requirements", return_value=self.required):
            return gate.verify_ci(self.api, SHA, self.root)

    def test_exact_success_and_dispatch(self):
        for event in ("push", "workflow_dispatch"):
            self.api.runs[0]["event"] = event
            self.api.current["event"] = event
            self.assertEqual(self.verify(), [{"workflow": "kanz-ci.yml", "run_id": 2, "attempt": 1}])

    def test_workflow_identity_spoof(self):
        for key, value in (("id", None), ("path", ".github/workflows/other.yml"),
                           ("url", "https://api.github.com/repos/other/repo/actions/workflows/42"),
                           ("state", "disabled_manually")):
            with self.subTest(key=key):
                self.api = API(); self.api.workflow[key] = value
                with self.assertRaises(gate.EvidenceError): self.verify()

    def test_run_identity_spoof(self):
        mutations = {"head_sha": "b"*40, "head_branch": "topic", "event": "pull_request",
                     "path": ".github/workflows/other.yml", "workflow_id": 99,
                     "repository": {"id": 999, "full_name": gate.REPO},
                     "head_repository": {"id": 123, "full_name": "fork/kanz"}}
        for key, value in mutations.items():
            for target in ("runs", "current"):
                with self.subTest(key=key, target=target):
                    self.api = API()
                    (self.api.runs[0] if target == "runs" else self.api.current)[key] = value
                    with self.assertRaises(gate.EvidenceError): self.verify()

    def test_missing_exact_source_is_not_ancestor_evidence(self):
        self.api.runs = []
        with self.assertRaises(gate.EvidenceError): self.verify()

    def test_latest_run_and_attempt_cannot_fall_back(self):
        for status, conclusion in (("in_progress", None), ("queued", None),
                                   ("completed", "cancelled"), ("completed", "failure"),
                                   ("completed", "skipped"), ("completed", "neutral")):
            with self.subTest(status=status, conclusion=conclusion):
                self.api = API(); self.api.runs = [run(1), run()]
                self.api.current.update(run_attempt=2, status=status, conclusion=conclusion)
                with self.assertRaises(gate.EvidenceError): self.verify()

    def test_latest_successful_attempt_uses_attempt_endpoint(self):
        self.api.current["run_attempt"] = 3
        self.api.runs[0]["run_attempt"] = 3
        self.api.jobs[0]["run_attempt"] = 3
        self.assertEqual(self.verify()[0]["attempt"], 3)
        self.assertTrue(any("/attempts/3/jobs" in p for p, _ in self.api.calls))

    def test_job_identity_and_outcome(self):
        for key, value in (("run_id", 9), ("run_attempt", 2), ("head_sha", "b"*40),
                           ("status", "in_progress"), ("conclusion", "cancelled"),
                           ("conclusion", "skipped"), ("name", "unrelated-success")):
            with self.subTest(key=key, value=value):
                self.api = API(); self.api.jobs[0][key] = value
                with self.assertRaises(gate.EvidenceError): self.verify()
        self.api = API(); self.api.jobs = []
        with self.assertRaises(gate.EvidenceError): self.verify()

    def test_pagination_does_not_lose_later_failed_job(self):
        self.api.page_size = 1
        self.api.runs.insert(0, run(1))
        self.api.jobs.insert(0, {**self.api.jobs[0], "id": 49, "name": "setup"})
        self.assertEqual(self.verify()[0]["run_id"], 2)
        self.api.jobs[1]["conclusion"] = "failure"
        with self.assertRaises(gate.EvidenceError): self.verify()

    def test_duplicate_and_truncated_inventory(self):
        self.api.jobs.append(copy.deepcopy(self.api.jobs[0]))
        with self.assertRaises(gate.EvidenceError): self.verify()
        for response in ({"total_count": 1, "jobs": []}, {"total_count": 1000, "jobs": []}):
            with patch.object(self.api, "get", return_value=response):
                with self.assertRaises(gate.EvidenceError): gate.pages(self.api, "/fixed", "jobs")

    def test_rerun_started_during_verification(self):
        original = self.api.get
        reads = 0
        def changing(path, **query):
            nonlocal reads
            value = original(path, **query)
            if path == "/actions/runs/2":
                reads += 1
                if reads == 2: value["run_attempt"] = 2
            return value
        with patch.object(self.api, "get", side_effect=changing):
            with self.assertRaises(gate.EvidenceError): self.verify()

    def test_present_runtime_workflows_require_exact_named_proofs(self):
        for filename, jobs in gate.POLICY.items():
            (self.root / ".github/workflows" / filename).write_text("fixture")
        self.assertEqual(gate.requirements(self.root), gate.POLICY)
        self.assertEqual(gate.POLICY["rust-bond-shadow.yml"], {"bond-shadow"})
        self.assertEqual(gate.POLICY["testnet-cni-proof.yml"], {"disposable-cni"})
        self.assertEqual(gate.POLICY["k3s-cni-migration.yml"], {"disposable-transition"})
        self.assertEqual(gate.POLICY["redpanda-identity-proof.yml"], {"developmental-redpanda-identity"})
        self.assertEqual(gate.POLICY["valkey-migration.yml"], {"retained-state"})
        self.assertEqual(gate.POLICY["tokyo-messaging-proof.yml"], {"messaging-isolation"})
        self.assertEqual(gate.POLICY["envoy-edge.yml"], {"actual-edge"})
        self.assertEqual(gate.POLICY["gateway-policy-proof.yml"], {"gateway-isolation"})
        with self.assertRaises(gate.EvidenceError): gate.verify_ci(self.api, SHA, self.root)
        for filename, jobs in gate.POLICY.items():
            with self.subTest(filename=filename), patch.object(gate, "requirements", return_value={filename: jobs}):
                self.required = {filename: jobs}
                self.api = API(filename)
                self.assertEqual(self.verify()[0]["workflow"], filename)
                self.api.jobs[0]["name"] = "unrelated-success"
                with self.assertRaises(gate.EvidenceError): self.verify()

    def test_security_and_four_language_proofs_cannot_be_skipped(self):
        self.assertEqual(len(gate.POLICY), 16)
        self.assertEqual(gate.POLICY["security.yml"], {"gitleaks", "secret-scan-selftest", "dependency-scan"})
        self.assertEqual(gate.POLICY["codeql.yml"], {"Analyze (actions)", "Analyze (go)", "Analyze (javascript-typescript)", "Analyze (python)"})
        self.assertEqual(gate.POLICY["terraform-testnet.yml"], {"terraform-testnet"})
        for filename in ("security.yml", "codeql.yml", "terraform-testnet.yml"):
            with patch.object(gate, "requirements", return_value={filename: gate.POLICY[filename]}):
                self.required = {filename: gate.POLICY[filename]}
                for name in gate.POLICY[filename]:
                    for status in ("skipped", "failure", "cancelled"):
                        self.api = API(filename)
                        next(j for j in self.api.jobs if j["name"] == name)["conclusion"] = status
                        with self.subTest(filename=filename, name=name, status=status), self.assertRaises(gate.EvidenceError): self.verify()
                self.api = API(filename)
                self.api.current["event"] = self.api.runs[0]["event"] = "workflow_dispatch"
                self.assertEqual(self.verify()[0]["workflow"], filename)

    def test_deleted_security_workflow_cannot_erase_required_gate(self):
        for filename in gate.MANDATORY:
            path = self.root / ".github/workflows" / filename
            path.unlink()
            with self.subTest(filename=filename), self.assertRaises(gate.EvidenceError): gate.requirements(self.root)
            path.write_text("fixture")

    def test_duplicate_required_job_names_fail(self):
        self.api.jobs.append({**self.api.jobs[0], "id": 51})
        with self.assertRaises(gate.EvidenceError): self.verify()

    def test_newer_run_after_job_read_fails(self):
        original = self.api.get
        lists = 0
        def changing(path, **query):
            nonlocal lists
            value = original(path, **query)
            if path.endswith("/runs"):
                lists += 1
                if lists == 2: value = {"total_count": 2, "workflow_runs": [run(), run(3)]}
            return value
        with patch.object(self.api, "get", side_effect=changing):
            with self.assertRaises(gate.EvidenceError): self.verify()

    def test_main_cannot_skip_evidence_or_issue_success_after_failure(self):
        with patch.object(release_source, "verify_source", return_value=("refs/tags/v1.2.3", SHA, SHA)), \
             patch.object(release_source, "GitHub") as client, \
             patch.object(release_source, "verify_ci", side_effect=gate.EvidenceError("missing proof")) as verify:
            self.assertEqual(release_source.main(), 1)
            verify.assert_called_once_with(client.return_value, SHA)

    def test_no_token_and_redirect_refused(self):
        with self.assertRaises(gate.EvidenceError): gate.GitHub("")
        with self.assertRaises(gate.EvidenceError): gate.NoRedirect().redirect_request(None, None, 302, None, None, "https://elsewhere")


if __name__ == "__main__":
    unittest.main()
