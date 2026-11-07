"""Closed source recipes for the read-only planner, not a general dataflow proof."""
import ast
from pathlib import Path
import re
import unittest

import yaml

ROOT = Path(__file__).resolve().parents[2]
READER = "test_recovery_plan.RecoveryTests.test_linux_receipt_reader_rejects_links_fifo_and_inventory_changes"
FILES = {"transaction.py", "policy_client.py", "rollout_scope.py", "recovery_receipts.py", "recovery_plan.py", "bounded_process.py", "source_loader.py"}


def verify(wrapper, loader, workflow):
    required = ("param()", "user/kanz-operator", "KanzPlatformProvisioner/kanz-platform-cli",
        "-I -B -c $sourceLoader $proofRoot preflight", "-I -B -c $sourceLoader $proofRoot recovery_host $repo",
        "recovery_plan --tokyo-plan --source $source", "$encoded.Length -gt 20000", "$bytes.Length -gt 524288",
        "$parameterBytes -gt 30000", "$parameters.Length -gt 30000", "len(data)>65536",
        "git -C $repo ls-files --error-unmatch", "$report.provenanceSHA256 -cne $provenanceHash",
        "$report.legacyHistoricalSnapshots -cne 'UNKNOWN'", "$report.restoreAuthorized -cne $false",
        "$report.persistentPolicyWrites -cne 0")
    if not all(part in wrapper for part in required) or "--apply" in wrapper or "rollout_engine.py" in wrapper:
        return False
    if not wrapper.index("$proofRoot preflight") < wrapper.index("sts get-caller-identity") < wrapper.index("$proofRoot recovery_host") < wrapper.index("ssm send-command"):
        return False
    transfer = re.search(r"foreach \(\$name in @\((.*?)\)\)", wrapper)
    bootstrap = re.search(r"b=json\.loads\(raw\); names=(\{[^\n]+\})", wrapper)
    if transfer is None or bootstrap is None:
        return False
    if set(re.findall(r"'([^']+)'", transfer[1])) != FILES or ast.literal_eval(bootstrap[1]) != FILES:
        return False
    if any('"$work/' + name + '"' not in wrapper for name in FILES):
        return False
    dependencies = next((n.value for n in ast.walk(ast.parse(loader)) if isinstance(n, ast.Assign)
        and len(n.targets) == 1 and isinstance(n.targets[0], ast.Name) and n.targets[0].id == "dependencies"), None)
    expected = {
        "preflight": ("bounded_process",),
        "image_identity": ("transaction", "rollout_scope", "rollout_probe"),
        "rollout_engine": ("bounded_process", "transaction", "policy_client", "rollout_scope", "rollout_probe"),
        "recovery_host": ("bounded_process", "transaction", "transport_codec", "recovery_receipts", "recovery_provenance"),
        "recovery_plan": ("bounded_process", "transaction", "policy_client", "rollout_scope", "recovery_receipts"),
    }
    if dependencies is None or ast.literal_eval(dependencies) != expected:
        return False
    if "if not sys.flags.isolated:" not in loader or "exec(compile(source, str(path)," not in loader:
        return False
    document = yaml.load(workflow, Loader=yaml.BaseLoader)
    if document["permissions"] != {"contents": "read"} or set(document["jobs"]) != {"gateway-isolation"}:
        return False
    job = document["jobs"]["gateway-isolation"]
    if job.get("if") or job.get("continue-on-error") or job.get("runs-on") != "ubuntu-latest":
        return False
    for event in ("pull_request", "push"):
        if "tools/Invoke-TokyoGatewayPolicyRecoveryPlan.ps1" not in document["on"][event]["paths"]:
            return False
    steps = job["steps"]
    pure = [s for s in steps if s.get("run") == "python3 -B -m unittest discover -s tools/gateway-policy-proof -p 'test_*.py'"]
    reader = [s for s in steps if s.get("name") == "Qualify retained receipt reader on root Linux"]
    if len(pure) != 1 or len(reader) != 1:
        return False
    test = reader[0]
    if test.get("working-directory") != "tools/gateway-policy-proof" or any(s.get("if") or s.get("continue-on-error") for s in [pure[0], test]):
        return False
    command = test.get("run", "")
    required_command = ("timeout --kill-after=5s 30s sudo -- env -i PATH=", 'assert sys.platform == "linux" and os.geteuid() == 0', READER,
        "assert result.testsRun == 1 and not result.skipped and result.wasSuccessful()")
    if not all(part in command for part in required_command) or "||" in command:
        return False
    # Old real policy/transaction executors and checkers remain mandatory.
    for script in ("proof.py", "check.py", "transaction_fixture.py", "check_transaction.py"):
        found = [s for s in steps if "tools/gateway-policy-proof/" + script in s.get("run", "")]
        if len(found) != 1 or found[0].get("if") or found[0].get("continue-on-error"):
            return False
    return True


class RecoveryWiringTests(unittest.TestCase):
    def test_closed_wrapper_loader_and_mandatory_linux_reader(self):
        wrapper = (ROOT / "tools/Invoke-TokyoGatewayPolicyRecoveryPlan.ps1").read_text()
        loader = (ROOT / "tools/gateway-policy-proof/source_loader.py").read_text()
        workflow = (ROOT / ".github/workflows/gateway-policy-proof.yml").read_text()
        self.assertTrue(verify(wrapper, loader, workflow))
        for target, old, new in (
            (0, "param()", "param([switch]$Apply)"),
            (0, "$proofRoot recovery_host $repo", "$proofRoot preflight"),
            (0, "-gt 20000", "-gt 21000"),
            (0, "-gt 524288", "-gt 1048576"),
            (0, "len(data)>65536", "len(data)>131072"),
            (0, "$parameterBytes -gt 30000", "$parameterBytes -gt 40000"),
            (0, "'recovery_plan.py',", "'rollout_engine.py',"),
            (1, '"recovery_receipts"),', '"recovery_receipts", "extra"),'),
            (1, "if not sys.flags.isolated:", "if False:"),
            (2, "    runs-on: ubuntu-latest", "    if: false\n    runs-on: ubuntu-latest"),
            (2, "contents: read", "contents: write"),
            (2, "sudo -- env -i PATH=", "env PATH="),
            (2, "timeout --kill-after=5s 30s ", ""),
            (2, "and os.geteuid() == 0", ""),
            (2, "and not result.skipped", ""),
            (2, "and result.wasSuccessful()", ""),
            (2, READER, "test_recovery_plan.RecoveryTests.test_strict_json_and_platform_refusal"),
            (2, "tools/Invoke-TokyoGatewayPolicyRecoveryPlan.ps1", "tools/not-the-plan.ps1"),
            (2, "python3 -B tools/gateway-policy-proof/transaction_fixture.py", "true"),
        ):
            values = [wrapper, loader, workflow]
            self.assertIn(old, values[target])
            values[target] = values[target].replace(old, new)
            with self.subTest(old=old): self.assertFalse(verify(*values))


if __name__ == "__main__":
    unittest.main()
