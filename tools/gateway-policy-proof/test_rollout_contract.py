"""Bounded source recipe checks, not whole-program control-flow proofs."""
import ast
from pathlib import Path
import unittest

ROOT = Path(__file__).resolve().parents[2]


def recipe(wrapper, engine, workflow):
    required = [
        "param([switch]$Apply)", "$profile = 'kanz-platform'", "$region = 'ap-northeast-1'",
        "$account = '012619468098'", "$instance = 'i-0ee1235614219f089'",
        "gateway-policy-proof/preflight.py", "KanzPlatformProvisioner/kanz-platform-cli",
        "'kanz-operator-process'", "git -C $repo diff --quiet $source", "git -C $repo ls-remote origin refs/heads/main",
        "ssm send-command", "--signal=TERM --kill-after=180s 1200s", "env -i PATH=",
    ]
    if not all(part in wrapper for part in required):
        return False
    if not wrapper.index("gateway-policy-proof/preflight.py") < wrapper.index("sts get-caller-identity") < wrapper.index("ssm send-command"):
        return False
    if "--force" in wrapper or "--force" in engine:
        return False
    tree = ast.parse(engine)
    functions = {node.name: node for node in tree.body if isinstance(node, ast.FunctionDef)}
    main = ast.unparse(functions["main"])
    stable = next(node for node in functions["main"].body if isinstance(node, ast.FunctionDef) and node.name == "stable")
    if not all(part in ast.unparse(stable) for part in ("snapshot(k)", "outside_policies(k)", "service_snapshot(k)")):
        return False
    if not all(part in main for part in ("KubernetesClient(k, CLUSTER_UID)", "client.check_scope()", "transaction.prepare(",
                                        "tx.qualify_rendered_baseline(", "if args.apply:", "transaction.apply(verify_and_clean)",
                                        "signal.signal(signal.SIGTERM, interrupted)")):
        return False
    return all(part in workflow for part in ("transaction_fixture.py", "check_transaction.py", "cleanup.py --transaction"))


class RolloutContractTests(unittest.TestCase):
    def test_fixed_operator_source_gate_and_qualification_mutations(self):
        wrapper = (ROOT / "tools/Invoke-TokyoGatewayPolicy.ps1").read_text()
        engine = (ROOT / "tools/gateway-policy-proof/rollout_engine.py").read_text()
        workflow = (ROOT / ".github/workflows/gateway-policy-proof.yml").read_text()
        self.assertTrue(recipe(wrapper, engine, workflow))
        for value in ("gateway-policy-proof/preflight.py", "KanzPlatformProvisioner/kanz-platform-cli", "'kanz-operator-process'",
                      "git -C $repo diff --quiet $source", "git -C $repo ls-remote origin refs/heads/main", "env -i PATH="):
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
