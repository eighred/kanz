"""Quota lifecycle models using fixed data, fake APIs and stdlib-only child fixtures.

No Docker, kind, Kubernetes or application binaries run in these models.
"""
from contextlib import contextmanager
import copy
import json
import os
import sys
import time
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import owned_lifecycle as lifecycle


SOURCE = "1" * 40
TREE = "2" * 40
NODE = "kindest/node:v1.36.4@sha256:" + "3" * 64
CID = "4" * 64
NID = "5" * 64
IDENTITY = dict(source=SOURCE, tree=TREE, run="12", attempt="1")
INPUTS = {name: "6" * 64 for name in lifecycle.INPUTS}


class DummyCommands:
    def __init__(self):
        self.reserve = lifecycle.Reserve()
        self.groups_absent = True
        self.group_cleanup_refused = False
        self.before_spawn = self.after_spawn = None
        self.last_exit = 0
        self.timed_out = False
        self.cluster = self.cid = False
        self.keep_container = self.keep_endpoint = self.exact_id_survives = False
        self.capture_unknown = self.delete_fails = self.create_times_out = False
        self.observed = []
        self.reconciled = []
        self.name = ""

    def reconcile_group(self, record):
        self.reconciled.append(record)

    def run(self, argv, seconds, **options):
        # Low-level data models still use finite dummy responses, but exercise
        # the real journal/Reserve hooks. Separate transport models launch real
        # stdlib fixture processes and never fake command completion.
        with self.reserve.observe(options.get("observation", False)):
            ticket = self.before_spawn() if self.before_spawn is not None else None
            if self.after_spawn is not None:
                record = {"pid": 100000 + ticket, "starttime": 456 + ticket, "uid": os.getuid()}
                self.after_spawn(ticket, record)
                if options.get("on_spawn") is not None:
                    options["on_spawn"](record)
            return self._respond(argv, seconds, **options)

    def _respond(self, argv, seconds, **options):
        self.observed.append((argv, seconds))
        self.last_exit, self.timed_out = 0, False
        if argv[:3] == ["kind", "get", "clusters"]:
            return 0, (self.name + "\n").encode() if self.cluster else b""
        if argv[:3] == ["kind", "create", "cluster"]:
            self.name = argv[argv.index("--name") + 1]
            self.cluster = self.cid = True
            if self.create_times_out:
                self.last_exit, self.timed_out = -15, True
                raise lifecycle.Refused("timeout")
            return 0, b""
        if argv[:3] == ["kind", "delete", "cluster"]:
            if self.delete_fails:
                raise lifecycle.Refused("command_nonzero")
            self.cluster = False
            if not self.keep_container:
                self.cid = False
            return 0, b""
        if argv[:3] == ["docker", "image", "inspect"]:
            return 1, b""  # Nonzero image observation remains UNKNOWN.
        if argv[:3] == ["docker", "container", "ls"]:
            exact = "id=" + CID in argv
            return 0, (CID + "\n").encode() if self.cid or (exact and self.exact_id_survives) else b""
        if argv[:3] == ["docker", "container", "inspect"]:
            return 0, json.dumps(dict(id=CID, name="/" + self.name + "-control-plane",
                cluster="foreign" if self.capture_unknown else self.name, role="control-plane",
                networks={"kind": {"NetworkID": NID}})).encode()
        if argv[:3] == ["docker", "network", "inspect"]:
            return 0, (CID + "\n").encode() if self.keep_endpoint else b""
        raise AssertionError("unexpected finite tool call")


class LifecycleModels(unittest.TestCase):
    @contextmanager
    def owner(self):
        with tempfile.TemporaryDirectory() as temporary:
            os.chmod(temporary, 0o700)
            commands = DummyCommands()
            with patch.dict(os.environ, {"RUNNER_TEMP": temporary}), \
                    patch.object(lifecycle, "identity", return_value=IDENTITY), \
                    patch.object(lifecycle, "node_pin", return_value=NODE), \
                    patch.object(lifecycle, "boot_id", return_value="11111111-1111-1111-1111-111111111111"), \
                    patch.object(lifecycle, "process_birth", return_value=456), \
                    patch.object(lifecycle, "input_hashes", return_value=INPUTS):
                owner = lifecycle.OwnedLifecycle(commands=commands)
                commands.name = owner.name
                try:
                    yield owner, commands, Path(temporary)
                finally:
                    owner.close()

    def create(self, owner):
        owner.prepare()
        owner.create(owner.work / "kubeconfig")

    def test_positive_owned_cleanup_and_report(self):
        with self.owner() as (owner, commands, root):
            self.create(owner)
            owner.cleanup()
            owner.seal(11, "success")
            report = json.loads((root / lifecycle.REPORT).read_bytes())
            self.assertTrue(report["passed"])
            self.assertEqual(report["cleanup"], "verified_absent")
            self.assertEqual((report["cleanup_phase"], report["cleanup_category"]), ("completed", "none"))
            self.assertEqual(report["image"], dict(present=None, id=None, requested_digest_match=None))
            self.assertEqual([seconds for argv, seconds in commands.observed if argv[:3] == ["kind", "create", "cluster"]], [240])
            deletion = [seconds for argv, seconds in commands.observed if argv[:3] == ["kind", "delete", "cluster"]]
            self.assertEqual(len(deletion), 1)
            self.assertTrue(0 < deletion[0] <= 90)

    def test_existing_cluster_is_not_deleted(self):
        with self.owner() as (owner, commands, _):
            commands.cluster = True
            with self.assertRaises(lifecycle.Refused):
                owner.prepare()
            with self.assertRaises(lifecycle.Refused):
                owner.cleanup()
            self.assertFalse(any(argv[:3] == ["kind", "delete", "cluster"] for argv, _ in commands.observed))

    def test_existing_state_claim_is_preserved(self):
        with self.owner() as (owner, _, root):
            prior = (root / lifecycle.STATE / "claim.json").read_bytes()
            with self.assertRaises(FileExistsError):
                lifecycle.OwnedLifecycle(commands=DummyCommands())
            self.assertEqual((root / lifecycle.STATE / "claim.json").read_bytes(), prior)

    def test_missing_claim_never_becomes_absence(self):
        with tempfile.TemporaryDirectory() as temporary:
            os.chmod(temporary, 0o700)
            with patch.dict(os.environ, {"RUNNER_TEMP": temporary}), \
                    patch.object(lifecycle, "identity", return_value=IDENTITY), \
                    patch.object(lifecycle, "node_pin", return_value=NODE), \
                    patch.object(lifecycle, "input_hashes", return_value=INPUTS), \
                    patch.object(lifecycle.sys, "argv", ["owned_lifecycle.py", "cleanup"]):
                with self.assertRaises(SystemExit):
                    lifecycle.main()
            self.assertFalse((Path(temporary) / "quota-owned-cleanup.json").exists())

    def test_preexisting_label_is_not_adopted(self):
        with self.owner() as (owner, commands, _):
            commands.cid = True
            with self.assertRaises(lifecycle.Refused):
                owner.prepare()
            self.assertFalse(owner.created)

    def test_partial_create_timeout_is_failed_after_cleanup(self):
        with self.owner() as (owner, commands, root):
            owner.prepare()
            commands.create_times_out = True
            with self.assertRaises(lifecycle.Refused):
                owner.create(owner.work / "kubeconfig")
            owner.cleanup()
            owner.seal(0, "timeout")
            value = json.loads((root / lifecycle.REPORT).read_bytes())
            self.assertFalse(value["passed"])
            self.assertTrue(value["create_timed_out"])
            self.assertEqual(value["create_exit"], -15)
            self.assertEqual(value["primary"], "timeout")

    def test_delete_success_with_container_remaining_refuses(self):
        with self.owner() as (owner, commands, _):
            self.create(owner)
            commands.keep_container = True
            with self.assertRaises(lifecycle.Refused):
                owner.cleanup()
            self.assertFalse(owner.cleaned)

    def test_exact_captured_id_surviving_without_label_refuses(self):
        with self.owner() as (owner, commands, _):
            self.create(owner)
            commands.exact_id_survives = True
            with self.assertRaises(lifecycle.Refused):
                owner.cleanup()

    def test_owned_network_endpoint_remaining_refuses(self):
        with self.owner() as (owner, commands, _):
            self.create(owner)
            commands.keep_endpoint = True
            with self.assertRaises(lifecycle.Refused):
                owner.cleanup()
            self.assertFalse(owner.cleaned)

    def test_shared_network_is_never_removed(self):
        with self.owner() as (owner, commands, _):
            self.create(owner)
            owner.cleanup()
            self.assertTrue(any(argv[:3] == ["docker", "network", "inspect"] for argv, _ in commands.observed))
            self.assertFalse(any(argv[:3] in (["docker", "network", "rm"], ["docker", "container", "rm"]) for argv, _ in commands.observed))

    def test_foreign_container_identity_blocks_deletion(self):
        with self.owner() as (owner, commands, _):
            self.create(owner)
            commands.capture_unknown = True
            with self.assertRaises(lifecycle.Refused):
                owner.cleanup()
            self.assertFalse(any(argv[:3] == ["kind", "delete", "cluster"] for argv, _ in commands.observed))

    def test_missing_launch_record_refuses_absence(self):
        with self.owner() as (owner, _, _):
            lifecycle.write_json(owner.fd, "launch-01.json", {"claim": owner.claim_hash, "launch": 1})
            with self.assertRaises(FileNotFoundError):
                owner.cleanup()
            self.assertFalse(owner.cleaned)

    def test_launch_record_requires_exact_claim(self):
        with self.owner() as (owner, _, _):
            index = owner.begin_launch()
            lifecycle.write_json(owner.fd, "child-01.json", {"claim": "0" * 64, "launch": index,
                "process": {"pid": 123, "starttime": 456, "uid": os.getuid()}})
            with self.assertRaises(lifecycle.Refused):
                owner.reconcile_launches()

    def test_primary_and_cleanup_failure_remain_separate(self):
        with self.owner() as (owner, commands, root):
            self.create(owner)
            commands.delete_fails = True
            with self.assertRaises(lifecycle.Refused) as error:
                owner.cleanup()
            owner.seal(0, "timeout", error.exception)
            value = json.loads((root / lifecycle.REPORT).read_bytes())
            self.assertEqual(value["primary"], "timeout")
            self.assertTrue(value["cleanup_error"])
            self.assertEqual(value["cleanup"], "unknown")
            self.assertEqual((value["cleanup_phase"], value["cleanup_category"]), ("delete", "command_nonzero"))
            self.assertFalse(value["passed"])

    def test_changed_source_refuses_retention(self):
        with self.owner() as (owner, _, _):
            with patch.object(lifecycle, "input_hashes", return_value={}):
                with self.assertRaises(lifecycle.Refused):
                    owner.seal(0, "unknown")

    def test_existing_report_preserved(self):
        with self.owner() as (owner, _, root):
            original = b"foreign"
            (root / lifecycle.REPORT).write_bytes(original)
            with self.assertRaises(FileExistsError):
                owner.seal(0, "unknown")
            self.assertEqual((root / lifecycle.REPORT).read_bytes(), original)

    def test_missing_create_cannot_be_published_as_success(self):
        with self.owner() as (owner, _, root):
            owner.prepare()
            owner.cleanup()
            self.assertEqual(lifecycle.cleanup_diagnostic(owner, None),
                dict(cleanup_phase="completed", cleanup_category="none"))
            with self.assertRaises(lifecycle.Refused):
                owner.seal(11, "success")
            self.assertFalse((root / lifecycle.REPORT).exists())

    def test_claim_regular_single_link_and_0600_required(self):
        with self.owner() as (owner, _, root):
            os.link(root / lifecycle.STATE / "claim.json", root / "outside")
            with self.assertRaises(lifecycle.Refused):
                lifecycle.read_json(owner.fd, "claim.json")

    def test_claim_symlink_is_not_followed(self):
        with self.owner() as (owner, _, root):
            os.symlink(root / lifecycle.STATE / "claim.json", root / lifecycle.STATE / "foreign.json")
            with self.assertRaises(OSError):
                lifecycle.read_json(owner.fd, "foreign.json")

    def test_claim_wrong_mode_refuses(self):
        with self.owner() as (owner, _, root):
            os.chmod(root / lifecycle.STATE / "claim.json", 0o640)
            with self.assertRaises(lifecycle.Refused):
                lifecycle.read_json(owner.fd, "claim.json")

    def test_destination_symlink_preserved(self):
        with self.owner() as (owner, _, root):
            outside = root / "outside"
            outside.write_bytes(b"retain")
            os.symlink(outside, root / lifecycle.REPORT)
            with self.assertRaises(FileExistsError):
                owner.seal(0, "unknown")
            self.assertEqual(outside.read_bytes(), b"retain")

    def test_mode_is_not_weakened_by_restrictive_umask(self):
        with self.owner() as (owner, _, root):
            previous = os.umask(0o777)
            try:
                lifecycle.write_json(owner.fd, "mode.json", {"fixed": True})
            finally:
                os.umask(previous)
            value = os.stat(root / lifecycle.STATE / "mode.json")
            self.assertEqual(value.st_mode & 0o777, 0o600)

    def test_claim_binding_mutations(self):
        with self.owner() as (owner, _, _):
            for key in ("source", "tree", "run", "attempt", "node", "inputs", "cluster", "format"):
                with self.subTest(key=key):
                    changed = copy.deepcopy(owner.claim)
                    changed[key] = "foreign"
                    with self.assertRaises(lifecycle.Refused):
                        lifecycle.validate_claim(changed, IDENTITY, NODE, INPUTS)

    def test_duplicate_json_keys_refuse(self):
        with self.assertRaises(lifecycle.Refused):
            json.loads('{"source":1,"source":2}', object_pairs_hook=lifecycle.unique_object)

    def test_oversize_retention_refuses_before_create(self):
        with self.owner() as (owner, _, root):
            with self.assertRaises(lifecycle.Refused):
                lifecycle.write_json(owner.fd, "oversize.json", {"x": "x" * lifecycle.MAX_JSON})
            self.assertFalse((root / lifecycle.STATE / "oversize.json").exists())

    def test_inventory_duplicates_and_malformed_ids_refuse(self):
        for raw in ((CID + "\n" + CID + "\n").encode(), b"foreign\n", b"\xff"):
            with self.subTest(raw=raw):
                with self.assertRaises(lifecycle.Refused):
                    lifecycle.lines(raw, lifecycle.HEX64)

    def test_shared_reserve_does_not_reset_between_observations(self):
        now = [0.0]
        reserve = lifecycle.Reserve(lambda: now[0])
        with reserve.observe():
            now[0] = 20
            self.assertEqual(reserve.left(), 10)
            with reserve.observe():
                now[0] = 21
        now[0] = 1000  # A separate bounded delete does not consume this reserve.
        with reserve.observe():
            self.assertEqual(reserve.left(), 9)
            now[0] += 10
            with self.assertRaises(lifecycle.Refused):
                reserve.left()

    def test_observation_allowance_is_carried_to_new_parent(self):
        with self.owner() as (owner, _, _):
            now = [1000000000000]
            with patch.object(lifecycle.time, "monotonic_ns", side_effect=lambda: now[0]):
                token, remaining = owner.begin_budget("observe")
                self.assertEqual(remaining, 30)
                now[0] += 11000000000
                owner.finish_budget(token)
                second = lifecycle.OwnedLifecycle(cleanup_only=True, commands=DummyCommands())
                try:
                    token, remaining = second.begin_budget("observe")
                    self.assertEqual(remaining, 19)
                    now[0] += 20000000000
                    second.finish_budget(token)
                    with self.assertRaises(lifecycle.Refused):
                        second.begin_budget("observe")
                finally:
                    second.close()

    def test_unfinished_live_holder_refuses_without_kill(self):
        with self.owner() as (owner, _, root):
            owner.begin_budget("observe")
            with patch.object(lifecycle.os, "killpg") as kill:
                with self.assertRaises(lifecycle.Refused):
                    owner.begin_budget("observe")
                kill.assert_not_called()
            self.assertFalse((root / lifecycle.STATE / "budget-observe-01-end.json").exists())

    def test_canceled_delete_interval_never_grants_fresh_90_seconds(self):
        with self.owner() as (owner, _, root):
            now = [1000000000000]
            with patch.object(lifecycle.time, "monotonic_ns", side_effect=lambda: now[0]):
                owner.begin_budget("delete")
                now[0] += 91000000000
                with patch.object(lifecycle, "process_birth", return_value=None):
                    with self.assertRaises(lifecycle.Refused):
                        owner.begin_budget("delete")
            self.assertFalse((root / lifecycle.STATE / "budget-delete-02-begin.json").exists())

    def test_reused_group_identity_never_killed(self):
        commands = lifecycle.Commands()
        with patch.object(lifecycle, "process_group", return_value=[(123, False, 999)]), \
                patch.object(lifecycle.os, "killpg") as kill:
            with self.assertRaises(lifecycle.Refused):
                commands.reconcile_group({"pid": 123, "starttime": 456, "uid": os.getuid()})
            kill.assert_not_called()

    def test_orphan_group_without_bound_leader_refuses(self):
        commands = lifecycle.Commands()
        with patch.object(lifecycle, "process_group", return_value=[(124, False, 456)]), \
                patch.object(lifecycle.os, "killpg") as kill:
            with self.assertRaises(lifecycle.Refused):
                commands.reconcile_group({"pid": 123, "starttime": 456, "uid": os.getuid()})
            kill.assert_not_called()

    def test_group_absence_is_observed(self):
        commands = lifecycle.Commands()
        with patch.object(lifecycle, "process_group", return_value=[]) as probe, \
                patch.object(lifecycle.os, "killpg") as kill:
            commands.reconcile_group({"pid": 123, "starttime": 456, "uid": os.getuid()})
            probe.assert_called_once_with(123)
            kill.assert_not_called()

    def test_exited_leader_does_not_hide_live_descendant(self):
        commands = lifecycle.Commands()
        members = [(123, True, 456), (124, False, 457)]
        process = unittest.mock.Mock(pid=123)
        def terminate(group, requested_signal):
            self.assertEqual(group, 123)
            self.assertEqual(requested_signal, lifecycle.signal.SIGTERM)
            members[:] = [(123, True, 456), (124, True, 457)]
        def joined(**kwargs):
            self.assertTrue(all(zombie for _, zombie, _ in members))
            members.clear()
            process.returncode = -15
        process.wait.side_effect = joined
        with patch.object(lifecycle, "process_group", side_effect=lambda group: list(members)), \
                patch.object(lifecycle.os, "killpg", side_effect=terminate) as kill:
            commands._stop(process)
            kill.assert_called_once_with(123, lifecycle.signal.SIGTERM)
            process.wait.assert_called_once()
        self.assertTrue(commands.groups_absent)

    def test_prior_cleanup_refusal_is_sticky(self):
        commands = lifecycle.Commands()
        commands.group_cleanup_refused = True
        with patch.object(lifecycle, "process_group", return_value=[]):
            commands.reconcile_group({"pid": 123, "starttime": 456, "uid": os.getuid()})
        self.assertFalse(commands.groups_absent)

    def test_cancel_cleanup_failure_still_retains_closed_failure(self):
        with self.owner() as (owner, commands, root):
            self.create(owner)
            commands.delete_fails = True
            with patch.object(lifecycle, "OwnedLifecycle", return_value=owner), \
                    patch.object(lifecycle.sys, "argv", ["owned_lifecycle.py", "cleanup"]):
                with self.assertRaises(SystemExit):
                    lifecycle.main()
            report = json.loads((root / lifecycle.REPORT).read_bytes())
            cleanup = json.loads((root / "quota-owned-cleanup.json").read_bytes())
            self.assertFalse(report["passed"])
            self.assertTrue(report["cleanup_error"])
            self.assertEqual(cleanup["cleanup"], "unknown")
            self.assertTrue(cleanup["cleanup_error"])
            for value in (report, cleanup):
                self.assertEqual((value["cleanup_phase"], value["cleanup_category"]), ("delete", "command_nonzero"))
            self.assertNotIn("cluster", cleanup)
            self.assertNotIn("stderr", report)

    def test_private_work_replaced_by_symlink_is_preserved(self):
        with self.owner() as (owner, _, root):
            owner.prepare()
            owner.work.rmdir()
            outside = root / "foreign"
            outside.mkdir()
            sentinel = outside / "preserve"
            sentinel.write_bytes(b"foreign")
            owner.work.symlink_to(outside)
            with self.assertRaises(lifecycle.Refused):
                owner.cleanup()
            self.assertEqual(sentinel.read_bytes(), b"foreign")

    def test_cleanup_phase_causal_refusals_retained_in_lifecycle_report(self):
        cases = (("journal", "launch_inventory"), ("allocation", "allocation_custody"),
            ("capture", "container_identity"), ("inventory", "inventory_changed"),
            ("delete", "command_nonzero"), ("container_absence", "owned_container_remains"),
            ("network_absence", "owned_endpoint_remains"), ("work_retire", "work_custody"))
        for phase, category in cases:
            with self.subTest(phase=phase), self.owner() as (owner, commands, root):
                self.create(owner)
                if phase == "journal":
                    (root / lifecycle.STATE / "launch-01.json").write_bytes(
                        lifecycle.canonical({"claim": "0" * 64, "launch": 1}))
                elif phase == "allocation":
                    (root / lifecycle.STATE / "started.json").write_bytes(
                        lifecycle.canonical({"claim": owner.claim_hash, "create_started": False}))
                elif phase == "capture":
                    commands.capture_unknown = True
                elif phase == "inventory":
                    (root / lifecycle.STATE / "inventory.json").write_bytes(lifecycle.canonical(
                        {"claim": owner.claim_hash, "containers": [], "networks": []}))
                elif phase == "delete":
                    commands.delete_fails = True
                elif phase == "container_absence":
                    commands.keep_container = True
                elif phase == "network_absence":
                    commands.keep_endpoint = True
                elif phase == "work_retire":
                    os.chmod(owner.work, 0o750)
                result = lifecycle.finish_owner(owner, 0, lifecycle.Refused("timeout"))
                report = json.loads((root / lifecycle.REPORT).read_bytes())
                self.assertEqual(result, dict(format="kanz-quota-outcome-v1", primary="timeout",
                    cleanup="refused", retention="retained", cleanup_phase=phase, cleanup_category=category))
                self.assertEqual((report["cleanup_phase"], report["cleanup_category"]), (phase, category))
                self.assertEqual(report["primary"], "timeout")
                self.assertTrue(report["cleanup_error"])
                self.assertFalse(report["passed"])
                self.assertFalse(owner.cleaned)

    def test_cleanup_diagnostic_refuses_unlisted_and_nonexact_metadata(self):
        class Label(str):
            pass
        class OtherRefusal(lifecycle.Refused):
            pass
        cases = (
            ("network_absence", lifecycle.Refused("network_observation_unknown"), "network_absence", "network_observation_unknown"),
            ("private diagnostic text", lifecycle.Refused("network_observation_unknown"), "unknown", "network_observation_unknown"),
            (Label("delete"), lifecycle.Refused("command_nonzero"), "unknown", "command_nonzero"),
            (False, lifecycle.Refused("timeout"), "unknown", "timeout"),
            ([], lifecycle.Refused("timeout"), "unknown", "timeout"),
            ("delete", lifecycle.Refused("private diagnostic text"), "delete", "unknown"),
            ("delete", lifecycle.Refused(Label("command_nonzero")), "delete", "unknown"),
            ("delete", lifecycle.Refused(["private diagnostic text"]), "delete", "unknown"),
            ("delete", OtherRefusal("command_nonzero"), "delete", "unknown"),
            ("work_retire", PermissionError("private diagnostic text"), "work_retire", "unknown"),
            ("work_retire", None, "completed", "none"),
            ("completed", None, "unknown", "none"))
        for phase, error, expected_phase, expected_category in cases:
            with self.subTest(phase=repr(phase), category=expected_category):
                owner = unittest.mock.Mock(cleanup_phase=phase, cleaned=True)
                value = lifecycle.cleanup_diagnostic(owner, error)
                self.assertEqual(value, dict(cleanup_phase=expected_phase, cleanup_category=expected_category))
                self.assertNotIn("private diagnostic text", json.dumps(value))
        owner = unittest.mock.Mock(cleanup_phase="work_retire", cleaned=False)
        self.assertEqual(lifecycle.cleanup_diagnostic(owner, None),
            dict(cleanup_phase="work_retire", cleanup_category="none"))

    def test_causal_cleanup_and_retention_errors_preserve_primary_diagnostic(self):
        with self.owner() as (owner, commands, _):
            self.create(owner)
            commands.delete_fails = True
            with patch.object(owner, "seal", side_effect=PermissionError("private retention text")) as seal:
                result = lifecycle.finish_owner(owner, 0, lifecycle.Refused("timeout"))
            self.assertEqual(result, dict(format="kanz-quota-outcome-v1", primary="timeout",
                cleanup="refused", retention="refused", cleanup_phase="delete", cleanup_category="command_nonzero"))
            self.assertEqual(seal.call_args.args[:2], (0, "timeout"))
            self.assertIs(type(seal.call_args.args[2]), lifecycle.Refused)
            self.assertEqual(seal.call_args.args[2].category, "command_nonzero")
            self.assertIsNone(owner.fd)
            self.assertIsNone(owner.parent)
            self.assertNotIn("private retention text", json.dumps(result))

    def test_work_retirement_unexpected_error_keeps_unknown_category(self):
        with self.owner() as (owner, _, root):
            self.create(owner)
            with patch.object(owner, "retire_work", side_effect=PermissionError("private retirement text")):
                result = lifecycle.finish_owner(owner, 11, None)
            report = json.loads((root / lifecycle.REPORT).read_bytes())
            self.assertEqual(result, dict(format="kanz-quota-outcome-v1", primary="success",
                cleanup="refused", retention="retained", cleanup_phase="work_retire", cleanup_category="unknown"))
            self.assertFalse(report["passed"])
            self.assertTrue(report["cleanup_error"])
            self.assertEqual((report["cleanup_phase"], report["cleanup_category"]), ("work_retire", "unknown"))
            self.assertNotIn("private retirement text", json.dumps([result, report]))

    def test_final_observation_accounting_error_is_not_completed(self):
        with self.owner() as (owner, _, root):
            self.create(owner)
            original = owner.commands.reserve.finish
            def finish(token):
                original(token)
                if owner.cleaned:
                    raise lifecycle.Refused("receipt_write")
            with patch.object(owner.commands.reserve, "finish", side_effect=finish):
                result = lifecycle.finish_owner(owner, 11, None)
            report = json.loads((root / lifecycle.REPORT).read_bytes())
            self.assertTrue(owner.cleaned)  # Resource oracles completed; accounting still refused.
            self.assertEqual(result, dict(format="kanz-quota-outcome-v1", primary="success",
                cleanup="refused", retention="retained", cleanup_phase="work_retire", cleanup_category="receipt_write"))
            self.assertTrue(report["cleanup_error"])
            self.assertFalse(report["passed"])
            self.assertNotEqual(report["cleanup_phase"], "completed")


MODEL_TOOL = r'''import json,os,sys,time
from pathlib import Path
role,filename,name,mode=sys.argv[1:]
state=Path(filename)
exists=json.loads(state.read_text())["exists"] if state.exists() else False
cid="4"*64;nid="5"*64
def witness():
    fields=Path("/proc/self/stat").read_bytes().rsplit(b")",1)[1].split()
    path=state.with_name("child-start.json")
    fd=os.open(path,os.O_WRONLY|os.O_CREAT|os.O_EXCL|os.O_NOFOLLOW,0o600)
    try:
        raw=json.dumps({"pid":os.getpid(),"starttime":int(fields[19])}).encode()
        os.write(fd,raw);os.fsync(fd)
    finally:os.close(fd)
if role=="image":sys.exit(1)
elif role=="clusters":
    if exists:print(name)
elif role=="ids":
    if exists:print(cid)
elif role=="inspect":
    print(json.dumps({"id":cid,"name":"/"+name+"-control-plane","cluster":name,
        "role":"control-plane","networks":{"kind":{"NetworkID":nid}}}))
elif role=="create":
    state.write_text(json.dumps({"exists":True}))
    if mode=="timeout":
        child=os.fork()
        if child==0:
            witness()
            while True:time.sleep(1)
        while True:time.sleep(1)
elif role=="delete":state.write_text(json.dumps({"exists":False}))
elif role=="endpoints":
    if mode=="network-error":
        print("private model network response")
        sys.exit(17)
elif role=="wait":
    witness()
    while True:time.sleep(1)
else:sys.exit(7)
'''


class TransportCommands(lifecycle.Commands):
    """Real Popen/selector/WNOWAIT/journal path; only fixed stdlib dummy tools."""
    def __init__(self, root, mode="normal"):
        super().__init__()
        self.root, self.mode, self.name = root, mode, ""
        self.requested = []

    def run(self, argv, seconds, **options):
        self.requested.append(tuple(argv[:3]))
        prefixes = {("kind","get","clusters"):"clusters", ("kind","create","cluster"):"create",
            ("kind","delete","cluster"):"delete", ("docker","image","inspect"):"image",
            ("docker","container","ls"):"ids", ("docker","container","inspect"):"inspect",
            ("docker","network","inspect"):"endpoints"}
        role = prefixes[tuple(argv[:3])]
        actual = [sys.executable,"-I","-S","-c",MODEL_TOOL,role,
            str(self.root/"dummy-state.json"),self.name,self.mode]
        # A short test-only create deadline reaches the real timeout path;
        # production callers/constants and every other command bound stay intact.
        cap = min(seconds,1.0) if self.mode=="timeout" and role=="create" else seconds
        return super().run(actual,cap,**options)


class TransportLifecycleModels(unittest.TestCase):
    @contextmanager
    def owned(self, mode="normal"):
        with tempfile.TemporaryDirectory() as temporary:
            root=Path(temporary);os.chmod(root,0o700)
            with patch.dict(os.environ,{"RUNNER_TEMP":temporary}), \
                    patch.object(lifecycle,"identity",return_value=IDENTITY), \
                    patch.object(lifecycle,"node_pin",return_value=NODE), \
                    patch.object(lifecycle,"input_hashes",return_value=INPUTS):
                commands=TransportCommands(root,mode)
                owner=lifecycle.OwnedLifecycle(commands=commands);commands.name=owner.name
                try:yield owner,commands,root
                finally:owner.close()

    def second(self, root):
        commands=TransportCommands(root)
        owner=lifecycle.OwnedLifecycle(cleanup_only=True,commands=commands)
        commands.name=owner.name
        return owner,commands

    def absent_witness(self, root):
        raw=(root/"child-start.json").read_bytes()
        self.assertLessEqual(len(raw),256)
        value=json.loads(raw)
        self.assertEqual(set(value),{"pid","starttime"})
        self.assertTrue(all(type(v)is int and v>0 for v in value.values()))
        # Observation only. Never kill or wait on an arbitrary witness PID.
        self.assertNotEqual(lifecycle.process_birth(value["pid"]),value["starttime"])

    def test_two_parent_full_transport_normal_inventory(self):
        with self.owned()as(owner,commands,root):
            owner.prepare();owner.create(owner.work/"kubeconfig");owner.cleanup()
            self.assertTrue(owner.cleaned)
            owner.close()
            second,other=self.second(root)
            try:
                second.cleanup()
                self.assertTrue(second.cleaned)
                self.assertEqual(len(second.launch_records()),20)
                intervals=list((root/lifecycle.STATE).glob("budget-observe-*-begin.json"))
                self.assertEqual(len(intervals),17)
                self.assertEqual(sum(c==("kind","create","cluster")for c in commands.requested+other.requested),1)
                self.assertEqual(sum(c==("kind","delete","cluster")for c in commands.requested+other.requested),1)
                self.assertTrue(all((root/lifecycle.STATE/f"child-{i:02}.json").is_file()for i in second.launch_records()))
            finally:second.close()

    def test_two_parent_real_timeout_descendant_cleanup(self):
        with self.owned("timeout")as(owner,_,root):
            owner.prepare()
            with self.assertRaises(lifecycle.Refused)as error:
                owner.create(owner.work/"kubeconfig")
            self.assertEqual(error.exception.category,"timeout")
            self.absent_witness(root)
            self.assertTrue(owner.commands.groups_absent)
            owner.cleanup();owner.seal(0,"timeout");owner.close()
            second,_=self.second(root)
            try:
                second.cleanup()
                self.assertTrue(second.cleaned)
                self.assertLessEqual(len(second.launch_records()),32)
                self.assertLessEqual(len(list((root/lifecycle.STATE).glob("budget-observe-*-begin.json"))),32)
                self.assertFalse(json.loads((root/lifecycle.REPORT).read_bytes())["passed"])
            finally:second.close()

    def test_exhausted_budget_refuses_before_new_process(self):
        with self.owned()as(owner,_,root):
            owner.prepare();owner.create(owner.work/"kubeconfig");owner.cleanup()
            token,remaining=owner.begin_budget("observe")
            now=time.monotonic_ns()+int((remaining+1)*1000000000)
            with patch.object(lifecycle.time,"monotonic_ns",return_value=now):
                owner.finish_budget(token)
                owner.close()
                second,commands=self.second(root)
                before=len(second.launch_records())
                try:
                    with self.assertRaises(lifecycle.Refused)as error:second.cleanup()
                    self.assertEqual(error.exception.category,"cleanup_deadline")
                    self.assertEqual(commands.requested,[])
                    self.assertEqual(len(second.launch_records()),before)
                finally:second.close()

    def test_cross_parent_live_group_never_check_then_kills(self):
        commands=lifecycle.Commands()
        observed=[(123,False,456)]
        with patch.object(lifecycle,"process_group",side_effect=[observed,[(123,False,999)]] )as probe, \
                patch.object(lifecycle.os,"killpg")as kill:
            with self.assertRaises(lifecycle.Refused):commands.reconcile_group(
                {"pid":123,"starttime":456,"uid":os.getuid()})
            kill.assert_not_called()
            probe.assert_called_once_with(123)

    def test_preclaim_selector_failure_joins_real_held_child(self):
        with tempfile.TemporaryDirectory()as temporary:
            root=Path(temporary);os.chmod(root,0o700)
            original=lifecycle.subprocess.Popen
            def spawn(argv,**kwargs):
                self.assertEqual(argv,["git","rev-parse","HEAD","HEAD^{tree}"])
                return original([sys.executable,"-I","-S","-c",MODEL_TOOL,"wait",
                    str(root/"dummy-state.json"),"quota-proof-123456789abc","normal"],**kwargs)
            def refuse_selector():
                end=time.monotonic()+2
                while not(root/"child-start.json").exists():
                    if time.monotonic()>=end:raise AssertionError("missing child-start witness")
                    time.sleep(.01)
                raise RuntimeError("fixed selector setup refusal")
            commands=lifecycle.Commands()
            with patch.object(lifecycle.subprocess,"Popen",side_effect=spawn), \
                    patch.object(lifecycle.selectors,"DefaultSelector",side_effect=refuse_selector):
                with self.assertRaises(RuntimeError):
                    commands.run(["git","rev-parse","HEAD","HEAD^{tree}"],10,preclaim=True)
            self.absent_witness(root)
            self.assertTrue(commands.groups_absent)
            self.assertEqual(commands.reserve.remaining,30)
            self.assertIsNone(commands.reserve.begin)

    def test_seal_failure_preserves_primary_cleanup_and_retention(self):
        owner=unittest.mock.Mock(cleaned=False)
        cleanup=lifecycle.Refused("cleanup_refused")
        owner.cleanup.side_effect=cleanup
        owner.seal.side_effect=PermissionError("fixed retention refusal")
        result=lifecycle.finish_owner(owner,0,lifecycle.Refused("timeout"))
        self.assertEqual(result,dict(format="kanz-quota-outcome-v1",primary="timeout",
            cleanup="refused",retention="refused",
            cleanup_phase="unknown",cleanup_category="unknown"))
        owner.seal.assert_called_once_with(0,"timeout",cleanup)
        owner.close.assert_called_once()
        self.assertNotIn("fixed retention",json.dumps(result))


    def test_network_nonzero_diagnostic_retained_by_both_parent_reports(self):
        with self.owned("network-error") as (owner, commands, root):
            owner.prepare()
            owner.create(owner.work / "kubeconfig")
            result = lifecycle.finish_owner(owner, 11, None)
            self.assertEqual(commands.last_exit, 17)
            self.assertEqual(result, dict(format="kanz-quota-outcome-v1", primary="success",
                cleanup="refused", retention="retained", cleanup_phase="network_absence",
                cleanup_category="network_observation_unknown"))
            second, other = self.second(root)
            other.mode = "network-error"
            with patch.object(lifecycle, "OwnedLifecycle", return_value=second), \
                    patch.object(lifecycle.sys, "argv", ["owned_lifecycle.py", "cleanup"]):
                with self.assertRaises(SystemExit) as error:
                    lifecycle.main()
            self.assertEqual(str(error.exception), "quota lifecycle: cleanup refused")
            self.assertEqual(other.last_exit, 17)
            report = json.loads((root / lifecycle.REPORT).read_bytes())
            cleanup = json.loads((root / "quota-owned-cleanup.json").read_bytes())
            for value in (report, cleanup):
                self.assertEqual((value["cleanup_phase"], value["cleanup_category"]),
                    ("network_absence", "network_observation_unknown"))
                self.assertTrue(value["cleanup_error"])
                self.assertNotIn("private model network response", json.dumps(value))
            self.assertFalse(report["passed"])
            self.assertEqual(sum(prefix == ("docker", "network", "inspect")
                for prefix in commands.requested + other.requested), 2)
            self.assertEqual(len(list((root / lifecycle.STATE).glob("launch-*.json"))), 20)
            self.assertIsNone(second.fd)
            self.assertIsNone(second.parent)


if __name__ == "__main__":
    unittest.main()
