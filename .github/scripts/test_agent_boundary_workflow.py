"""Always-run core policy refuses removal or bypass of the dedicated qualifier."""
from pathlib import Path
import unittest

import check_agent_boundary_evidence as gate

ROOT = Path(__file__).resolve().parents[2]


class AgentWorkflowTests(unittest.TestCase):
    def test_owned_workflow_is_mandatory_and_source_scoped(self):
        gate.workflow_policy((ROOT / ".github/workflows/agent-read-plane.yml").read_text(encoding="utf-8"))

    def test_core_owner_cannot_skip_its_workflow_policy(self):
        gate.unconditional_scope_policy((ROOT / ".github/workflows/kanz-ci.yml").read_text(encoding="utf-8"))

    def test_removed_disabled_conditional_or_permissive_workflow_refuses(self):
        original = (ROOT / ".github/workflows/agent-read-plane.yml").read_text(encoding="utf-8")
        mutations = {
            "missing-job": ("  agent-boundary:", "  obsolete-boundary:"),
            "conditional-job": ("    timeout-minutes: 45", "    timeout-minutes: 45\n    if: false"),
            "optional-job": ("    timeout-minutes: 45", "    timeout-minutes: 45\n    continue-on-error: true"),
            "missing-identity": ("python3 -B tools/agent_identity/proof.py", "python3 -B tools/agent_identity/absent.py"),
            "conditional-identity": ("      - name: Prove actual SPIRE and NATS agent identity", "      - name: Prove actual SPIRE and NATS agent identity\n        if: false"),
            "optional-http": ("      - name: Qualify actual isolated HTTP transports", "      - name: Qualify actual isolated HTTP transports\n        continue-on-error: true"),
            "missing-reread": ('python3 -B .github/scripts/check_agent_boundary_evidence.py --directory "$AGENT_BOUNDARY_EVIDENCE_DIR" --source', 'python3 -B .github/scripts/absent.py --directory "$AGENT_BOUNDARY_EVIDENCE_DIR" --source'),
            "untriggered-source": ("      - 'kanz/services/mcp/**'", "      - 'unused/mcp/**'"),
            "main-omitted": ("    branches: [main]", "    branches: [unused]"),
            "publisher-permission": ("  packages: read", "  packages: write"),
            "identity-exit": ("          python3 -B tools/agent_identity/proof.py", "          exit 0\n          python3 -B tools/agent_identity/proof.py"),
            "network-widened": ("--network none", "--network host"),
            "readonly-removed": ("--network none --read-only", "--network none"),
            "privileged": ("--cap-drop ALL", "--privileged --cap-drop ALL"),
            "ambient-volume": ("--pids-limit 128", "--volume /var/run/docker.sock:/var/run/docker.sock --pids-limit 128"),
            "runtime-changed": ("  AGENT_HTTP_RUNTIME: ghcr.io/", "  AGENT_HTTP_RUNTIME: registry.invalid/"),
            "commented-identity": ("          python3 -B tools/agent_identity/proof.py", "          # python3 -B tools/agent_identity/proof.py"),
            "commented-receipt": ("          python3 -B .github/scripts/check_agent_boundary_evidence.py --directory", "          # python3 -B .github/scripts/check_agent_boundary_evidence.py --directory"),
            "cleanup-disabled": ("      - name: Prove owned HTTP container cleanup\n        if: always()", "      - name: Prove owned HTTP container cleanup\n        if: false"),
            "cleanup-assertion-omitted": ('          remaining=$(docker ps -aq --filter "label=$owner")\n          test -z "$remaining"', '          remaining=$(docker ps -aq --filter "label=$owner")\n          true'),
            "artifact-directory-leak": ("            ${{ runner.temp }}/agent-boundary-evidence/http.jsonl", "            ${{ runner.temp }}/agent-boundary-evidence/"),
            "identity-cleanup-disabled": ("      - name: Prove owned identity cleanup after interruption\n        if: always()", "      - name: Prove owned identity cleanup after interruption\n        if: false"),
            "identity-cleanup-omitted": ("python3 -B tools/agent_identity/proof.py --cleanup-owned", "python3 -B tools/agent_identity/proof.py --not-cleanup"),
        }
        for name, (before, after) in mutations.items():
            with self.subTest(mutation=name):
                mutated = original.replace(before, after, 1)
                self.assertNotEqual(mutated, original, "missing mutation anchor")
                with self.assertRaises(ValueError):
                    gate.workflow_policy(mutated)

    def test_independent_core_owner_cannot_be_removed_or_conditional(self):
        original = (ROOT / ".github/workflows/kanz-ci.yml").read_text(encoding="utf-8")
        mutations = [
            ("python3 -B .github/scripts/test_agent_boundary_workflow.py", "true"),
            ("      - name: Verify mandatory agent boundary qualification policy", "      - name: Verify mandatory agent boundary qualification policy\n        if: false"),
            ("  scope:\n    runs-on:", "  scope:\n    if: false\n    runs-on:"),
            ("  pull_request:", "  pull_request:\n    paths: ['unused/**']"),
        ]
        for before, after in mutations:
            with self.subTest(mutation=before):
                mutated = original.replace(before, after, 1)
                self.assertNotEqual(mutated, original)
                with self.assertRaises(ValueError):
                    gate.unconditional_scope_policy(mutated)


if __name__ == "__main__":
    unittest.main()
