import copy
import unittest
from unittest.mock import patch

import check_migration as checker
import migration
import proof
from test_proof import Fixture


class MigrationEvidenceTests(unittest.TestCase):
    def test_each_backend_requires_exact_source_and_all_gateway_controls(self):
        gateway = checker.gateway_module()
        network = gateway.network_fixture()
        network.load("network_observation")
        sequence = network.load("network_evidence").sequence(True)
        enforced = {"format": "kanz-gateway-network-v1", "nonce": "a" * 32, "phase": "network_verified", "isolated": True,
                    "observations": [{"target": target, "peer": peer, "port": port, "expected": expected,
                        "batches": [["connected" if expected == "connected" else "timeout"] * 3], "result": "verified"}
                        for target, peer, port, expected in sequence]}
        closed = copy.deepcopy(enforced); closed["phase"] = "network_refused"; closed["observations"] = closed["observations"][:1]
        closed["observations"][0].update(batches=[["refused"] * 3], result="not_converged")
        no_policy = copy.deepcopy(enforced); no_policy["phase"] = "network_refused"; no_policy["observations"] = no_policy["observations"][:5]
        no_policy["observations"][3].update(batches=[["connected"] * 3], result="not_converged")
        policies = [{"metadata": {"name": name, "namespace": gateway.SERVICES}} for name in gateway.POLICY_NAMES.values()]
        inventory = [{"name": p["metadata"]["name"], "namespace": gateway.SERVICES, "sha256": gateway.digest(p)} for p in policies]
        with patch.object(proof.time, "sleep"):
            canonical = proof.run_proof(Fixture(), "a" * 40)
        report = {"source": "a" * 40, "tree": "b" * 40, "runID": "123", "runAttempt": "1", "nodeImage": migration.NODE,
                  "verdict": "PASS", "cleanup": "verified_absent", "phases": {}}
        for phase, backend in (("flannel", "flannel"), ("cilium", "cilium"), ("rollback", "flannel")):
            report["phases"][phase] = {"state": "preserved", "dns": "PASS", "newPVC": "PASS", "policy": copy.deepcopy(canonical),
                "gateway": {"sourceSHA": "a" * 40, "sourceTree": "b" * 40, "runID": "123", "runAttempt": "1",
                    "phase": phase, "backend": backend, "renderSHA256": "c" * 64, "policies": inventory,
                    "unionCleanupPassed": True, "restoredExact": True, "initialPolicyReadback": True,
                    "policyRemovalSHA256": "d" * 64, "policyRestoreSHA256": "d" * 64,
                    "networkControls": copy.deepcopy({"closed": closed, "no_policy": no_policy, "enforced": enforced}),
                    "observations": {name: {"result": [outcome] * 3, **({"before": ["allowed"] * 3, "after": ["allowed"] * 3} if outcome == "blocked" else {})}
                                     for name, outcome in gateway.cases().items()}}}
        def validate(value): checker.validate(value, "a" * 40, "b" * 40, "123", "1", policies, "c" * 64)
        validate(report)
        for phase in report["phases"]:
            for key, value in (("sourceSHA", "e" * 40), ("sourceTree", "e" * 40), ("runID", "124"), ("runAttempt", "2"),
                               ("phase", "other"), ("backend", "other"), ("renderSHA256", "f" * 64), ("policies", []),
                               ("unionCleanupPassed", False), ("observations", {}), ("networkControls", {})):
                bad = copy.deepcopy(report); bad["phases"][phase]["gateway"][key] = value
                with self.subTest(phase=phase, key=key), self.assertRaises(ValueError): validate(bad)
            bad = copy.deepcopy(report); del bad["phases"][phase]["gateway"]
            with self.assertRaises(ValueError): validate(bad)
            bad = copy.deepcopy(report); bad["phases"][phase]["policy"]["cases"].pop()
            with self.assertRaises(proof.ProofError): validate(bad)


if __name__ == "__main__":
    unittest.main()
