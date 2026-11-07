"""Pure ownership/source controls; actual PID1 behavior is a mandatory Linux step."""
import ast
import copy
import inspect
import json
from pathlib import Path
import unittest
import types
import uuid

import yaml
from unittest.mock import patch

import migration
import proof
import storage_signal


def mandatory_control(value):
    job = value["jobs"]["disposable-transition"]
    expected = {"name": "Prove storage PID1 graceful termination", "run": "python3 -B tools/cni-proof/storage_signal.py"}
    steps = job["steps"]
    matches = [i for i, step in enumerate(steps) if step.get("name") == expected["name"]]
    migration_step = next(i for i, step in enumerate(steps) if step.get("run") == "python3 -B tools/cni-proof/migration.py")
    if len(matches) != 1 or steps[matches[0]] != expected or matches[0] >= migration_step:
        raise ValueError("mandatory PID1 control differs")


def diagnostic_seam(value):
    full = value["jobs"]["disposable-transition"]
    diag = value["jobs"]["storage-pid1-diagnostic"]
    dispatch = value.get("on", value.get(True))["workflow_dispatch"]
    if (dispatch != {"inputs": {"mode": {"type": "choice", "default": "full", "options": ["full", "storage-pid1-diagnostic"]}}}
            or full.get("if") != "github.event_name != 'workflow_dispatch' || inputs.mode != 'storage-pid1-diagnostic'"
            or diag.get("if") != "github.event_name == 'workflow_dispatch' && inputs.mode == 'storage-pid1-diagnostic' && github.ref != 'refs/heads/main'"
            or full["timeout-minutes"] != 30 or diag["timeout-minutes"] != 5
            or value["permissions"] != {"contents": "read"}):
        raise ValueError("diagnostic job boundary differs")
    for job in (full, diag):
        if "permissions" in job or "continue-on-error" in job:
            raise ValueError("diagnostic job authority differs")
    if any("migration.py" in step.get("run", "") for step in diag["steps"]):
        raise ValueError("diagnostic executes migration")


class StorageSignalTests(unittest.TestCase):
    def test_diagnostic_mode_is_not_release_evidence(self):
        root = Path(__file__).resolve().parents[2]
        source = root / ".github/scripts/release_ci.py"
        release = types.ModuleType("private_release_ci")
        exec(compile(source.read_bytes(), str(source), "exec"), release.__dict__)
        self.assertEqual(release.POLICY["k3s-cni-migration.yml"], {"disposable-transition"})
        sha = "a" * 40
        run = {"id": 2, "workflow_id": 1, "path": ".github/workflows/k3s-cni-migration.yml", "head_sha": sha,
               "head_branch": "main", "event": "workflow_dispatch", "run_number": 3, "run_attempt": 1,
               "repository": {"id": 7, "full_name": "eighred/kanz"}, "head_repository": {"id": 7, "full_name": "eighred/kanz"},
               "status": "completed", "conclusion": "success"}
        class API:
            def __init__(self, full): self.full = full
            def get(self, path, **_):
                if path == "": return run["repository"]
                if path == "/actions/workflows/k3s-cni-migration.yml":
                    return {"id": 1, "path": run["path"], "state": "active", "url": release.API + "/actions/workflows/1"}
                if path == "/actions/workflows/1/runs": return {"total_count": 1, "workflow_runs": [run]}
                if path == "/actions/runs/2": return run
                if path == "/actions/runs/2/attempts/1/jobs":
                    return {"total_count": 2, "jobs": [{"id": i, "run_id": 2, "head_sha": sha, "run_attempt": 1,
                            "status": "completed", "name": name, "conclusion": conclusion} for i, name, conclusion in
                            ((8, "disposable-transition", self.full), (9, "storage-pid1-diagnostic", "success"))]}
                raise AssertionError("unexpected offline API route")
        with patch.object(release, "requirements", return_value={"k3s-cni-migration.yml": release.POLICY["k3s-cni-migration.yml"]}):
            self.assertEqual(len(release.verify_ci(API("success"), sha)), 1)
            with self.assertRaisesRegex(release.EvidenceError, "required job did not succeed uniquely"):
                release.verify_ci(API("skipped"), sha)

    def test_runtime_requires_direct_private_pid_namespace(self):
        for mode in ("", "private"):
            storage_signal.runtime_preconditions({"HostConfig": {"Init": False, "PidMode": mode}})
        for host in ({}, {"Init": False}, {"Init": None, "PidMode": "private"},
                     {"Init": True, "PidMode": ""}, {"Init": False, "PidMode": "host"},
                     {"Init": False, "PidMode": "container:secret"}, {"Init": False, "PidMode": "unknown"}):
            with self.assertRaises(ValueError): storage_signal.runtime_preconditions({"HostConfig": host})

    def test_closed_diagnostic_projection(self):
        obj = {"State": {"Status": "exited", "Running": False, "ExitCode": 1, "OOMKilled": False, "Error": "secret"},
               "HostConfig": {"Init": False, "PidMode": "", "Secret": "secret"}}
        value = storage_signal.state_projection(obj)
        self.assertEqual(value, {"status": "exited", "running": False, "exitCode": 1, "oomKilled": False, "init": False, "pidMode": "private"})
        for mode in ("", "private"):
            obj["HostConfig"]["PidMode"] = mode
            self.assertEqual(storage_signal.state_projection(obj)["pidMode"], "private")
        for mode, expected in (("host", "host"), ("container:secret", "unknown"), (None, "unknown")):
            obj["HostConfig"]["PidMode"] = mode
            self.assertEqual(storage_signal.state_projection(obj)["pidMode"], expected)
        obj["State"].update(Status="secret", Running="secret", ExitCode=True, OOMKilled="secret")
        obj["HostConfig"].update(Init="secret", PidMode="container:secret")
        self.assertNotIn("secret", json.dumps(storage_signal.state_projection(obj)))
        for raw, expected in (("pid1\npid1_ready", "pid1_ready"), ("pid1", "pid1"), ("non_pid1", "non_pid1"), ("", "empty"), ("secret", "unknown"), ("pid1\npid1_ready\nsecret", "pid1"), ("non_pid1\nTraceback: secret paths and fields\nAssertionError", "non_pid1")):
            self.assertEqual(storage_signal.sentinel(raw), expected)

    def test_diagnostic_workflow_is_nonmain_manual_only(self):
        value = yaml.safe_load((Path(__file__).resolve().parents[2] / ".github/workflows/k3s-cni-migration.yml").read_text(encoding="utf-8"))
        diagnostic_seam(value)
        for change in ("full_condition", "diag_condition", "default", "budget", "permissions", "continue"):
            mutated = copy.deepcopy(value)
            full = mutated["jobs"]["disposable-transition"]
            diag = mutated["jobs"]["storage-pid1-diagnostic"]
            if change == "full_condition": full["if"] = "true"
            if change == "diag_condition": diag["if"] = "true"
            if change == "default": mutated[True]["workflow_dispatch"]["inputs"]["mode"]["default"] = "storage-pid1-diagnostic"
            if change == "budget": full["timeout-minutes"] = 31
            if change == "permissions": diag["permissions"] = {"contents": "write"}
            if change == "continue": diag["continue-on-error"] = True
            with self.assertRaises(ValueError): diagnostic_seam(mutated)

    def test_image_stop_signal_requires_observed_default_or_term(self):
        for config in ({}, {"StopSignal": ""}, {"StopSignal": "SIGTERM"}, {"StopSignal": "15"}):
            with patch.object(migration, "command", return_value=json.dumps([{"Id": "sha256:" + "c" * 64, "Config": config}])) as call:
                self.assertEqual(storage_signal.image_stop_signal(), "sha256:" + "c" * 64)
                self.assertEqual(call.call_args.args[0], ["docker", "image", "inspect", proof.IMAGE])
        for value in ([], [{"Config": None}], [{"Config": {"StopSignal": v}} for v in ("SIGINT", "2")]):
            with patch.object(migration, "command", return_value=json.dumps(value)):
                with self.assertRaises(ValueError): storage_signal.image_stop_signal()
        for signal in (None, 15, "TERM", "SIGINT", "SIGQUIT", "2", "unknown"):
            with patch.object(migration, "command", return_value=json.dumps([{"Config": {"StopSignal": signal}}])):
                with self.assertRaises(ValueError): storage_signal.image_stop_signal()
        with patch.object(migration, "command", side_effect=proof.ProofError("unknown")):
            with self.assertRaises(proof.ProofError): storage_signal.image_stop_signal()

    def test_lost_create_response_reconciles_only_exact_owned_container(self):
        nonce = "b" * 32
        identity = "a" * 64
        name = "kanz-storage-signal-" + nonce
        obj = {"Id": identity, "Name": "/" + name,
               "Config": {"Image": proof.IMAGE, "Labels": {storage_signal.LABEL: nonce}}}
        for mode in ("owned", "foreign_nonce", "missing_ownership", "unknown_ps", "unknown_inspect"):
            with self.subTest(mode=mode):
                observed = copy.deepcopy(obj)
                if mode == "foreign_nonce": observed["Config"]["Labels"][storage_signal.LABEL] = "foreign"
                if mode == "missing_ownership": observed["Config"].pop("Labels")
                replies = ["", proof.ProofError("create_response_lost"), json.dumps([observed]), identity, json.dumps([observed]), "", ""]
                if mode == "unknown_ps": replies[3] = proof.ProofError("inventory_unknown")
                if mode == "unknown_inspect": replies[2] = replies[4] = proof.ProofError("ownership_unknown")
                progress = {}
                with patch.object(storage_signal.uuid, "uuid4", return_value=uuid.UUID(hex=nonce)), \
                        patch.object(migration, "command", side_effect=replies) as call:
                    with self.assertRaises((ValueError, proof.ProofError)):
                        storage_signal.case(False, progress)
                removals = [item.args[0] for item in call.call_args_list if item.args[0][:2] == ["docker", "rm"]]
                if mode == "owned":
                    self.assertEqual(removals, [["docker", "rm", "-f", identity]])
                    self.assertEqual(progress["cleanup"], "verified_absent")
                    self.assertEqual(call.call_count, 7)
                else:
                    self.assertEqual(removals, [])
                    self.assertEqual(progress["cleanup"], "unknown")

    def test_mandatory_actual_step_cannot_skip_or_move(self):
        value = yaml.safe_load((Path(__file__).resolve().parents[2] / ".github/workflows/k3s-cni-migration.yml").read_text(encoding="utf-8"))
        mandatory_control(value)
        for change in ("skip", "continue", "missing", "after"):
            mutated = copy.deepcopy(value)
            steps = mutated["jobs"]["disposable-transition"]["steps"]
            index = next(i for i, step in enumerate(steps) if step.get("name") == "Prove storage PID1 graceful termination")
            if change == "skip": steps[index]["if"] = False
            if change == "continue": steps[index]["continue-on-error"] = True
            if change == "missing": steps.pop(index)
            if change == "after": steps.append(steps.pop(index))
            with self.assertRaises(ValueError): mandatory_control(mutated)

    def test_exact_fragment_precedes_storage_work(self):
        self.assertEqual(migration.STORAGE_SHUTDOWN,
                         "import signal,sys;signal.signal(signal.SIGTERM,lambda *_:sys.exit(0));")
        tree = ast.parse(inspect.getsource(migration))
        method = next(node for node in ast.walk(tree) if isinstance(node, ast.FunctionDef) and node.name == "storage")
        assignment = next(node for node in ast.walk(method) if isinstance(node, ast.Assign)
                          and any(isinstance(target, ast.Name) and target.id == "script" for target in node.targets))
        # The shared prefix must be the first operation, before DNS/marker work.
        left = assignment.value
        while isinstance(left, ast.BinOp):
            left = left.left
        self.assertIsInstance(left, ast.Name)
        self.assertEqual(left.id, "STORAGE_SHUTDOWN")
        self.assertIn("time.sleep(120)", ast.unparse(assignment))
        self.assertIn("assert os.getpid()==1;print('pid1_ready',flush=True)", storage_signal.BODY)

    def test_exact_ownership_and_unknown_never_absence(self):
        obj = {"Id": "a" * 64, "Name": "/test", "Config": {"Image": proof.IMAGE, "Labels": {storage_signal.LABEL: "nonce"}}}
        with patch.object(migration, "command", return_value=json.dumps([obj])):
            self.assertEqual(storage_signal.owned("test", "nonce", "a" * 64), obj)
        for field in ("id", "name", "image", "nonce"):
            other = copy.deepcopy(obj)
            if field == "id": other["Id"] = "b" * 64
            if field == "name": other["Name"] = "/foreign"
            if field == "image": other["Config"]["Image"] = "foreign"
            if field == "nonce": other["Config"]["Labels"][storage_signal.LABEL] = "foreign"
            with patch.object(migration, "command", return_value=json.dumps([other])):
                with self.assertRaises(ValueError): storage_signal.owned("test", "nonce", "a" * 64)
        with patch.object(migration, "command", side_effect=proof.ProofError("unknown")):
            with self.assertRaises(proof.ProofError): storage_signal.owned("test", "nonce")


if __name__ == "__main__":
    unittest.main()
