"""Pure ownership/source controls; actual PID1 behavior is a mandatory Linux step."""
import ast
import copy
import inspect
import json
from pathlib import Path
import unittest
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


class StorageSignalTests(unittest.TestCase):
    def test_image_stop_signal_requires_observed_default_or_term(self):
        for config in ({}, {"StopSignal": ""}, {"StopSignal": "SIGTERM"}, {"StopSignal": "15"}):
            with patch.object(migration, "command", return_value=json.dumps([{"Config": config}])) as call:
                self.assertEqual(storage_signal.image_stop_signal(), "TERM")
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
                replies = ["", proof.ProofError("create_response_lost"), identity, json.dumps([observed]), "", ""]
                if mode == "unknown_ps": replies[2] = proof.ProofError("inventory_unknown")
                if mode == "unknown_inspect": replies[3] = proof.ProofError("ownership_unknown")
                progress = {}
                with patch.object(storage_signal.uuid, "uuid4", return_value=uuid.UUID(hex=nonce)), \
                        patch.object(migration, "command", side_effect=replies) as call:
                    with self.assertRaises((ValueError, proof.ProofError)):
                        storage_signal.case(False, progress)
                removals = [item.args[0] for item in call.call_args_list if item.args[0][:2] == ["docker", "rm"]]
                if mode == "owned":
                    self.assertEqual(removals, [["docker", "rm", "-f", identity]])
                    self.assertEqual(progress["cleanup"], "verified_absent")
                    self.assertEqual(call.call_count, 6)
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
