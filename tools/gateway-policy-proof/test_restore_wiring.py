"""Closed restoration recipe checks; real API behavior has a separate fixture."""
import ast
from pathlib import Path
import re
import unittest

ROOT = Path(__file__).resolve().parents[2]
FILES = {"policy_state.py", "policy_client.py", "rollout_scope.py", "recovery_receipts.py",
         "recovery_plan.py", "recovery_restore.py", "bounded_process.py", "source_loader.py"}


def verify(wrapper, source):
    required = ("param()", "user/kanz-operator", "KanzPlatformProvisioner/kanz-platform-cli",
        "-I -B -c $sourceLoader $proofRoot preflight", "-I -B -c $sourceLoader $proofRoot recovery_host $repo",
        "recovery_restore --tokyo-retained-baseline --source $source", "$encoded.Length -gt 20000",
        "$bytes.Length -gt 524288", "$parameterBytes -gt 30000", "$parameters.Length -gt 30000",
        "len(data)>65536", "git -C $repo ls-files --error-unmatch",
        "$report.provenanceSHA256 -cne $provenanceHash", "$report.legacyHistoricalSnapshots -cne 'UNKNOWN'",
        "$report.result -cne 'VERIFIED'", "$report.writeAttempts -cne 1")
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
    markers = ('DIRECTORY = "/var/tmp/kanz-gateway-reconcile-nu1ezqwo"',
        'if self.attempted:', 'self.attempted = True', 'return self.execute(',
        'phase = "restore_intent"\n        journal["intended"] = proposed\n        save({**journal, "phase": phase})',
        'stable()\n        attempted = True', 'response = acknowledged(put_once(proposed), proposed, target)',
        'journal["response"] = response\n        phase = "restore_acknowledged"\n        save({**journal, "phase": phase})\n        after = observation(',
        'policies[receipts.TARGET] != target_projection(response)', 'current_hash != receipt_hash',
        'final_hash != receipt_hash', 'raw_after != response', 'read_target(k, target) != raw_target', '"unknown" if attempted else "refused"',
        'os.mkdir(os.path.basename(directory), 0o700, dir_fd=parent)', 'os.fsync(parent)', 'journal = create_journal(DIRECTORY)',
        'return planner.inventory_command(argv, value, environment, directory)',
        'reconcile(k, PutOnce(environment, directory), payload, receipts.read_receipts, journal)',
        'rv == old_rv', 'state.semantic(obj) != state.semantic(intended)')
    if not all(marker in source for marker in markers): return False
    functions = {n.name: n for n in ast.parse(source).body if isinstance(n, ast.FunctionDef)}
    if set(functions) != {"target_projection", "read_target", "observation", "acknowledged", "reconcile", "create_journal", "main"}: return False
    projection = ast.parse('def target_projection(value):\n    result = copy.deepcopy(value)\n    result["metadata"].pop("managedFields", None)\n    return result').body[0]
    if ast.dump(functions["target_projection"], include_attributes=False) != ast.dump(projection, include_attributes=False):
        return False
    reconcile = ast.get_source_segment(source, functions["reconcile"])
    if reconcile.count("put_once(proposed)") != 1 or "rollback(" in source: return False
    main = ast.get_source_segment(source, functions["main"])
    if not main.index("planner.plan(") < main.index("create_journal(") < main.index("PutOnce("):
        return False
    return True


class RestoreWiringTests(unittest.TestCase):
    def test_closed_wrapper_and_one_shot_protocol(self):
        wrapper = (ROOT / "tools/Invoke-TokyoGatewayPolicyRetainedRestore.ps1").read_text(encoding="utf-8")
        source = (Path(__file__).parent / "recovery_restore.py").read_text(encoding="utf-8")
        self.assertTrue(verify(wrapper, source))
        for old in ("user/kanz-operator", "$proofRoot preflight", "$proofRoot recovery_host",
                    "$encoded.Length -gt 20000", "$parameterBytes -gt 30000", "$report.writeAttempts -cne 1"):
            with self.subTest(wrapper=old):
                self.assertFalse(verify(wrapper.replace(old, "removed"), source))
        for old in ('self.attempted = True', 'stable()\n        attempted = True',
                    'journal["response"] = response', 'phase = "restore_acknowledged"',
                    'policies[receipts.TARGET] != target_projection(response)', 'final_hash != receipt_hash', 'raw_after != response', 'read_target(k, target) != raw_target',
                    'os.fsync(parent)', 'rv == old_rv', 'journal = create_journal(DIRECTORY)',
                    'result["metadata"].pop("managedFields", None)'):
            with self.subTest(source=old):
                self.assertFalse(verify(wrapper, source.replace(old, "pass")))


if __name__ == "__main__":
    unittest.main()
