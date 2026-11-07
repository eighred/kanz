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
        "param([switch]$Apply)", "$rolloutProfile = 'kanz-platform'", "$region = 'ap-northeast-1'",
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
        for value in ("-I -B -c $sourceLoader $proofRoot preflight", "KanzPlatformProvisioner/kanz-platform-cli", "'kanz-operator-process'",
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
