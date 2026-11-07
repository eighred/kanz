"""Bounded source recipe checks, not whole-program control-flow proofs."""
import ast
from pathlib import Path
import unittest
from unittest.mock import patch, Mock
import io
import json
import tempfile
import rollout_engine as engine

ROOT = Path(__file__).resolve().parents[2]


def recipe(wrapper, engine, workflow):
    required = [
        "param([switch]$Apply)", "$bundle = [ordered]@{ files = $files; payload = [ordered]@{ source = $source;", "$rolloutProfile = 'kanz-platform'", "$region = 'ap-northeast-1'",
        "$account = '012619468098'", "$instance = 'i-0ee1235614219f089'",
        "-I -B -c $sourceLoader $proofRoot preflight", "KanzPlatformProvisioner/kanz-platform-cli",
        "'kanz-operator-process'", "git -C $repo diff --quiet $source", "git -C $repo ls-remote origin refs/heads/main",
        "user/kanz-operator", "--profile $parentProfile", "'.github/workflows'", "ssm send-command", "--signal=TERM --kill-after=180s 1200s", "env -i PATH=",
    ]
    if not all(part in wrapper for part in required):
        return False
    if not wrapper.index("-I -B -c $sourceLoader $proofRoot preflight") < wrapper.index("sts get-caller-identity") < wrapper.index("ssm send-command"):
        return False
    if "--force" in wrapper or "--force" in engine:
        return False
    tree = ast.parse(engine)
    functions = {node.name: node for node in tree.body if isinstance(node, ast.FunctionDef)}
    main = ast.unparse(functions["main"])
    stable = next(node for node in ast.walk(functions["main"]) if isinstance(node, ast.FunctionDef) and node.name == "stable")
    if not all(part in ast.unparse(stable) for part in ("snapshot(k, payload['workloads'])", "outside_policies(k)", "service_snapshot(k)")):
        return False
    if not all(part in main for part in ("KubernetesClient(k, CLUSTER_UID)", "client.check_scope()", "transaction.prepare(",
                                        "tx.qualify_rendered_baseline(", "if args.apply:", "transaction.apply(verify_and_clean)",
                                        "signal.signal(signal.SIGTERM, interrupted)")):
        return False
    return all(part in workflow for part in ("transaction_fixture.py", "check_transaction.py", "cleanup.py --transaction"))


class RolloutContractTests(unittest.TestCase):
    def test_dry_run_never_opens_durable_receipts_or_constructs_probes(self):
        source = "a" * 40
        payload = {"source": source, "policies": [], "identity": {}, "workloads": {}}
        for failure in (False, True):
            with self.subTest(failure=failure), tempfile.TemporaryDirectory() as parent:
                original_temp = tempfile.TemporaryDirectory
                def transient(**kwargs):
                    return original_temp(prefix=kwargs["prefix"], dir=parent)
                client = Mock()
                if failure:
                    client.check_scope.side_effect = engine.tx.Refusal("fixture preflight refusal")
                transaction = Mock(receipt={"phase": "dry_run_passed"})
                before = {(engine.tx.NAMESPACE, app): {"labels": {"app": app}}
                          for app in ("api-gateway", "web-bff")}
                output = io.StringIO()
                with patch.object(engine.sys, "argv", ["engine", "--tokyo", "--source", source]), \
                     patch.object(engine.sys, "platform", "linux"), \
                     patch.object(engine.os, "geteuid", return_value=0, create=True), \
                     patch.object(engine.signal, "signal"), \
                     patch.object(engine.sys, "stdin", Mock(buffer=io.BytesIO(json.dumps(payload).encode()))), \
                     patch.object(engine.sys, "stdout", output), \
                     patch.object(engine.tempfile, "TemporaryDirectory", side_effect=transient), \
                     patch.object(engine.tx, "Receipts") as durable, \
                     patch.object(engine, "Probes") as probes, \
                     patch.object(engine, "KubernetesClient", return_value=client), \
                     patch.object(engine, "snapshot", return_value=before), \
                     patch.object(engine, "outside_policies", return_value={}), \
                     patch.object(engine, "service_snapshot", return_value={}), \
                     patch.object(engine.probe, "equivalent_source_selectors"), \
                     patch.object(engine.tx, "Transaction", return_value=transaction):
                    if failure:
                        with self.assertRaises(engine.tx.Refusal): engine.main()
                    else:
                        engine.main()
                        transaction.prepare.assert_called_once()
                    durable.assert_not_called()
                    probes.assert_not_called()
                    transaction.apply.assert_not_called()
                self.assertIsNone(json.loads(output.getvalue())["receipt"])
                self.assertEqual(list(Path(parent).iterdir()), [])

    def test_probe_receipt_preserves_uid_address_on_check_and_cleanup_failure(self):
        import copy
        pods, records = {}, []
        namespace = None
        def kubectl(*args, value=None):
            nonlocal namespace
            if args[:2] in (("get", "services"), ("get", "replicasets,replicationcontrollers,statefulsets,daemonsets,jobs")):
                return '{"items":[]}'
            if args[:2] == ("get", "endpointslices"):
                return '{"items":[{"endpoints":"malformed"}]}'
            if args[:2] == ("get", "namespace"):
                return json.dumps(namespace)
            if args[0] == "create":
                obj = copy.deepcopy(value)
                obj["metadata"]["uid"] = "aaaaaaaa-aaaa-aaaa-aaaa-" + str(len(pods)).zfill(12)
                obj["metadata"]["resourceVersion"] = "1"
                if obj["kind"] == "Namespace": namespace = obj
                else:
                    obj["status"] = {"phase": "Running", "podIP": "10.0.0." + str(len(pods)+1),
                        "conditions": [{"type": "Ready", "status": "False"}],
                        "containerStatuses": [{"name": "probe", "imageID": "fixture-image", "state": {"running": {}}}]}
                    pods[(obj["metadata"]["namespace"], obj["metadata"]["name"])] = obj
                return ""
            if args[0] == "-n" and args[2] == "get":
                obj = pods.get((args[1], args[4]))
                if args[-1] == "name": return "" if obj is None else "present"
                return json.dumps(obj)
            if args[:2] == ("delete", "--raw"):
                parts = args[2].split("/")
                if "pods" in parts: pods.pop((parts[4], parts[6]))
                else: namespace = None
                return ""
            if "wait" in args: return ""
            raise AssertionError("unexpected fixture operation")
        owned = engine.Probes(kubectl, Mock(), {}, "a"*32, records.append)
        with patch.object(engine.probe, "verify_cri", return_value="fixture-image"):
            with self.assertRaises(ValueError): owned.create()
        observed = records[-1]
        self.assertEqual(observed["phase"], "pod_running_unready_observed")
        self.assertEqual(len(observed["pods"]), 5)
        self.assertTrue(all(p["uid"] and p["address"] for p in observed["pods"]))
        with patch.object(owned, "endpoints", return_value=[{"endpoints": None}]):
            owned.check()
            first = next(iter(pods.values()))
            address = first["status"]["podIP"]
            first["status"]["podIP"] = "10.0.0.99"
            with self.assertRaises(engine.tx.Refusal): owned.check()
            first["status"]["podIP"] = address
        with patch.object(engine.time, "monotonic", side_effect=[0, 31]):
            with self.assertRaises(ValueError): owned.close()
        self.assertFalse(pods)
        self.assertIsNone(namespace)
        receipt = records[-1]
        self.assertNotEqual(receipt["phase"], "probe_cleanup_verified")
        self.assertEqual(len(receipt["pods"]), 5)
        self.assertTrue(all(p["deleted"] and p["uid"] and p["address"] for p in receipt["pods"]))
        self.assertEqual(receipt["namespaceUID"], observed["namespaceUID"])
        self.assertFalse(any(r["phase"] == "probe_cleanup_verified" for r in records))
        with patch.object(owned, "endpoints", return_value=[{"endpoints": None}]):
            owned.close()
        self.assertEqual(records[-1]["phase"], "probe_cleanup_verified")
        self.assertEqual(records[-1]["pods"], receipt["pods"])

    def test_replacement_between_create_and_running_never_acquires_probe_custody(self):
        nonce = "a" * 32
        uid = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
        def obj(value, status=None):
            result = {"metadata": {"uid": value, "resourceVersion": "1",
                      "labels": {engine.probe.OWNER: nonce}}}
            if status is not None: result["status"] = status
            return json.dumps(result)
        replacement = obj("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb",
                          {"phase": "Running", "podIP": "10.0.0.99"})
        api = Mock(side_effect=["null", "", obj(uid), "", "",
                               obj(uid, {"phase": "Pending"}), replacement])
        records = []
        probes = engine.Probes(api, Mock(), {}, nonce, records.append)
        with patch.object(probes, "services", return_value=[]), \
             patch.object(probes, "controllers", return_value=[]), \
             patch.object(engine.probe, "verify_cri") as verify:
            with self.assertRaises(engine.tx.Refusal): probes.create()
            verify.assert_not_called()
        self.assertEqual(probes.owned[0]["uid"], uid)
        self.assertEqual(probes.owned[0]["addresses"], [])
        self.assertEqual(probes.addresses, [])
        self.assertEqual(probes.peers, {})
        api.side_effect = None
        api.return_value = replacement
        with self.assertRaises(engine.tx.Refusal): probes.close()
        self.assertFalse(any(call.args[0] == "delete" for call in api.call_args_list))
        self.assertFalse(any(r["phase"] == "probe_cleanup_verified" for r in records))

    def test_cleanup_custody_is_uid_bound_and_never_invents_unknown_addresses(self):
        import copy
        uid = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
        nonce = "a" * 32
        def owner(status):
            return {"metadata": {"uid": uid, "resourceVersion": "1",
                    "labels": {engine.probe.OWNER: nonce}}, "status": status}
        for status, accepted in (({"phase": "Pending"}, True),
                                 ({"phase": "Running", "podIP": "10.0.0.7"}, True),
                                 ({"phase": "Running"}, False),
                                 ({"phase": "Running", "podIP": "invalid"}, False)):
            with self.subTest(status=status):
                records, deletes = [], []
                current = owner(status)
                def api(*args, value=None):
                    nonlocal current
                    if args[:2] == ("get", "endpointslices"):
                        return '{"items":[]}'
                    if args[0] == "-n" and args[2] == "get":
                        return json.dumps(current)
                    if args[:2] == ("delete", "--raw"):
                        deletes.append(value)
                        current = None
                    return ""
                probes = engine.Probes(api, Mock(), {}, nonce, records.append)
                entry = {"namespace": "fixture", "name": "owned", "uid": uid}
                probes.owned.append(entry)
                # No CRI/spec qualification has occurred; cleanup alone observes custody.
                if accepted:
                    probes.close()
                    self.assertEqual(records[-1]["phase"], "probe_cleanup_verified")
                    self.assertEqual(deletes[0]["preconditions"]["uid"], uid)
                else:
                    with self.assertRaises((ValueError, engine.tx.Refusal)): probes.close()
                    self.assertFalse(any(r["phase"] == "probe_cleanup_verified" for r in records))
                    self.assertIsNone(current)
                    self.assertEqual(len(deletes), 1)
                self.assertNotIn("address", entry)
                self.assertEqual(probes.peers, {})

        records = []
        probes = engine.Probes(Mock(return_value="null"), Mock(), {}, nonce, records.append)
        entry = {"namespace": "fixture", "name": "owned", "uid": uid}
        probes.owned.append(entry)
        replacement = owner({"phase": "Running", "podIP": "10.0.0.99"})
        replacement["metadata"]["uid"] = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
        with self.assertRaises(engine.tx.Refusal): probes.observe(entry, replacement)
        self.assertNotIn("addresses", entry)
        probes.k.return_value = json.dumps(replacement)
        with self.assertRaises(engine.tx.Refusal): probes.close()
        self.assertFalse(any(c.args[0] == "delete" for c in probes.k.call_args_list))
        probes.k.return_value = "null"
        with patch.object(probes, "endpoints", return_value=[]):
            with self.assertRaises(engine.tx.Refusal): probes.close()
        self.assertFalse(any(r["phase"] == "probe_cleanup_verified" for r in records))

        with self.assertRaises(ValueError):
            probes.observe(entry, owner({"phase": "Running", "podIP": "10.0.0.77", "podIPs": "malformed"}))
        probes.observe(entry, owner({"phase": "Running", "podIP": "10.0.0.1"}))
        self.assertTrue(entry["addressUncertain"])
        with patch.object(probes, "endpoints", return_value=[]):
            with self.assertRaises(engine.tx.Refusal): probes.close()
        self.assertFalse(any(r["phase"] == "probe_cleanup_verified" for r in records))

        for last in range(1, 9):
            probes.observe(entry, owner({"phase": "Running", "podIP": f"10.0.0.{last}"}))
        self.assertEqual(len(probes.addresses), 8)
        with self.assertRaises(engine.tx.Refusal):
            probes.observe(entry, owner({"phase": "Running", "podIP": "10.0.0.9"}))
        self.assertEqual(len(entry["addresses"]), 8)
        self.assertTrue(records[-1]["pods"][0]["addressUncertain"])
        probes.k.reset_mock()
        probes.k.side_effect = [json.dumps(owner({"phase": "Running", "podIP": "10.0.0.9"})), "", ""]
        with patch.object(probes, "endpoints", return_value=[]):
            with self.assertRaises(engine.tx.Refusal): probes.close()
        self.assertTrue(any(c.args[0] == "delete" for c in probes.k.call_args_list))
        self.assertTrue(entry["deleted"])
        probes.k.side_effect = None
        probes.observe(entry, owner({"phase": "Pending"}))
        with patch.object(probes, "endpoints", return_value=[]):
            with self.assertRaises(engine.tx.Refusal): probes.close()
        self.assertFalse(any(r["phase"] == "probe_cleanup_verified" for r in records))

    def test_release_gate_executes_source_not_cached_bytecode(self):
        import marshal
        import importlib.util
        import types
        text = (ROOT / "tools/gateway-policy-proof/preflight.py").read_text()
        tree = ast.parse(text)
        # Execute only the actual loader statements with an isolated source tree.
        start = next(i for i, n in enumerate(tree.body) if isinstance(n, ast.Assign)
                     and any(isinstance(t, ast.Name) and t.id == "release_source" for t in n.targets))
        end = next(i for i, n in enumerate(tree.body) if isinstance(n, ast.ClassDef))
        loader = compile(ast.Module(body=tree.body[start:end], type_ignores=[]), "loader", "exec")
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            source = root / ".github/scripts/release_ci.py"
            source.parent.mkdir(parents=True)
            source.write_text("accepted = False\n")
            cache = Path(importlib.util.cache_from_source(str(source)))
            cache.parent.mkdir()
            import struct
            info = source.stat()
            # A timestamp-valid malicious pyc would override source under importlib.
            cache.write_bytes(importlib.util.MAGIC_NUMBER + struct.pack("<III", 0, int(info.st_mtime), info.st_size)
                              + marshal.dumps(compile("accepted = True", str(source), "exec")))
            namespace = {"ROOT": root, "types": types}
            exec(loader, namespace)
            self.assertIs(namespace["release_ci"].accepted, False)
            self.assertTrue(cache.exists())

    def test_fixed_operator_source_gate_and_qualification_mutations(self):
        wrapper = (ROOT / "tools/Invoke-TokyoGatewayPolicy.ps1").read_text()
        engine = (ROOT / "tools/gateway-policy-proof/rollout_engine.py").read_text()
        workflow = (ROOT / ".github/workflows/gateway-policy-proof.yml").read_text()
        self.assertTrue(recipe(wrapper, engine, workflow))
        for value in ("$bundle = [ordered]@{ files = $files; payload = [ordered]@{ source = $source;", "-I -B -c $sourceLoader $proofRoot preflight", "KanzPlatformProvisioner/kanz-platform-cli", "'kanz-operator-process'",
                      "git -C $repo diff --quiet $source", "git -C $repo ls-remote origin refs/heads/main", "env -i PATH=", "user/kanz-operator", "--profile $parentProfile", "'.github/workflows'"):
            with self.subTest(value=value):
                self.assertFalse(recipe(wrapper.replace(value, "removed"), engine, workflow))
        for value in ("client.check_scope()", "transaction.apply(verify_and_clean)", "signal.signal(signal.SIGTERM, interrupted)"):
            with self.subTest(value=value):
                self.assertFalse(recipe(wrapper, engine.replace(value, "pass"), workflow))
        for value in ("transaction_fixture.py", "check_transaction.py", "cleanup.py --transaction"):
            with self.subTest(value=value):
                self.assertFalse(recipe(wrapper, engine, workflow.replace(value, "removed")))


if __name__ == "__main__":
    unittest.main()
