import copy
import json
import unittest
from unittest.mock import patch

import network_evidence
import network_fixture
import rollout_engine as engine


def complete_record(isolated=True):
    return {"format": "kanz-gateway-network-v1", "nonce": "a" * 32, "phase": "network_verified", "isolated": isolated,
            "observations": [{"target": target, "peer": peer, "port": port, "expected": expected,
                              "batches": [["connected" if expected == "connected" else "timeout"] * 3], "result": "verified"}
                             for target, peer, port, expected in network_evidence.sequence(isolated)]}


def complete_controls():
    enforced = complete_record()
    closed = copy.deepcopy(enforced); closed["phase"] = "network_refused"
    closed["observations"] = closed["observations"][:1]
    closed["observations"][0].update(batches=[["refused"] * 3], result="not_converged")
    no_policy = copy.deepcopy(enforced); no_policy["phase"] = "network_refused"
    no_policy["observations"] = no_policy["observations"][:5]
    no_policy["observations"][3].update(batches=[["connected"] * 3], result="not_converged")
    return {"closed": closed, "no_policy": no_policy, "enforced": enforced}


class NetworkEvidenceTests(unittest.TestCase):
    def test_exact_boolean_modes_have_distinct_closed_sequences(self):
        for isolated, length in ((False, 7), (True, 43)):
            good = complete_record(isolated)
            self.assertEqual(len(good["observations"]), length)
            network_evidence.validate(good, "a" * 32)
            refused = copy.deepcopy(good)
            refused["phase"] = "network_refused"
            refused["observations"] = refused["observations"][:1]
            refused["observations"][0].update(result="unknown", batches=[["unknown"] * 3])
            network_evidence.validate(refused, "a" * 32)
            for mode in (not isolated, None, 0, 1, "false"):
                bad = copy.deepcopy(good); bad["isolated"] = mode
                with self.subTest(isolated=isolated, mode=mode), self.assertRaises(ValueError):
                    network_evidence.validate(bad, "a" * 32)
            bad = copy.deepcopy(good); del bad["isolated"]
            with self.assertRaises(ValueError): network_evidence.validate(bad, "a" * 32)
        with self.assertRaises(ValueError): network_fixture.validate_control("enforced", complete_record(False))

    def test_actual_baseline_gate_success_and_refusal(self):
        for fail in (False, True):
            records, calls = [], []
            def k(*args, value=None, timeout=100):
                calls.append(args)
                return json.dumps(["unknown" if fail else "connected"] * 3)
            probes = engine.Probes(k, None, None, "a" * 32, records.append)
            probes.check = lambda: None
            probes.peers = {name: {"namespace": "fixture", "name": name} for name in ("gateway", "bff", "observer")}
            workloads = {(engine.tx.NAMESPACE, name): {"ip": "192.0.2.1"} for name in (*engine.PORTS, "api-gateway")}
            if fail:
                with self.assertRaises(engine.tx.Refusal): probes.verify(workloads, False)
            else: probes.verify(workloads, False)
            self.assertEqual(len(records), 1)
            self.assertIs(records[0]["isolated"], False)
            self.assertEqual(len(calls), 1 if fail else 7)
            network_evidence.validate(records[0], "a" * 32)
            for invalid in (None, 0, 1, "false"):
                with self.assertRaises(engine.tx.Refusal): probes.verify(workloads, invalid)
            self.assertEqual(len(records), 1)

    def test_closed_receipt_inventory_nonce_and_observation_bounds(self):
        good = complete_record()
        self.assertEqual(len(good["observations"]), 43)
        network_evidence.validate(good, "a" * 32)
        self.assertLess(len(json.dumps(good).encode()), 2 * 1024 * 1024)
        for mode in ("format", "nonce", "phase", "field", "target", "peer", "port", "bool_port", "batch_bound", "observation_bound", "unknown", "reset", "missing"):
            bad = copy.deepcopy(good)
            if mode in ("format", "nonce", "phase"): bad[mode] = "foreign"
            elif mode == "field": bad["rawError"] = "private"
            elif mode in ("target", "peer"): bad["observations"][3][mode] = "foreign"
            elif mode == "port": bad["observations"][3]["port"] = 443
            elif mode == "bool_port": bad["observations"][3]["port"] = True
            elif mode == "batch_bound": bad["observations"][3]["batches"] *= 7
            elif mode == "observation_bound": bad["observations"][3]["batches"][0] *= 2
            elif mode == "unknown": bad["observations"][3]["batches"][0][1] = "unknown"
            elif mode == "reset": bad["observations"][3]["batches"][0][1] = "connected"
            else: bad["observations"].pop()
            with self.subTest(mode=mode), self.assertRaises(ValueError): network_evidence.validate(bad, "a" * 32)

    def test_actual_control_checker_requires_each_discriminator(self):
        good = complete_controls(); network_fixture.validate(good)
        for mode in ("missing", "closed_timeout", "closed_empty", "no_policy_denied", "after_missing", "enforced_failed"):
            bad = copy.deepcopy(good)
            if mode == "missing": del bad["closed"]
            elif mode == "closed_timeout": bad["closed"]["observations"][0]["batches"] = [["timeout"] * 3]
            elif mode == "closed_empty": bad["closed"]["observations"][0]["batches"] = []
            elif mode == "no_policy_denied": bad["no_policy"]["observations"][3]["batches"] = [["refused"] * 3]
            elif mode == "after_missing": bad["no_policy"]["observations"].pop()
            else: bad["enforced"]["phase"] = "network_refused"
            with self.subTest(mode=mode), self.assertRaises(ValueError): network_fixture.validate(bad)

    def test_gate_converges_or_records_failure_and_runs_after_positive(self):
        for mode in ("pass", "unknown", "open", "reset", "post_failure"):
            calls, records = [], []
            negative_calls = 0
            def k(*args, value=None, timeout=100):
                nonlocal negative_calls
                peer, port = args[3], int(args[-1]); calls.append((peer, port, timeout))
                values = ["connected"] * 3
                if peer in ("untrusted", "bff", "observer", "outsider") and port in (8101, 8102, 8095):
                    negative_calls += 1; values = ["timeout", "refused", "host_unreachable"]
                    if mode in ("unknown", "post_failure") and peer == "untrusted": values = ["unknown"] * 3
                    if mode == "open" and peer == "untrusted": values = ["connected"] * 3
                    if mode == "reset" and negative_calls == 1: values = ["timeout", "connected", "refused"]
                elif mode == "post_failure" and negative_calls: values = ["unknown"] * 3
                return json.dumps(values)
            probes = engine.Probes(k, None, None, "a" * 32, records.append)
            probes.check = lambda: None
            probes.peers = {name: {"namespace": "fixture", "name": name} for name in ("gateway", "bff", "observer", "untrusted", "outsider")}
            workloads = {(engine.tx.NAMESPACE, name): {"ip": "192.0.2.1"} for name in (*engine.PORTS, "api-gateway")}
            if mode in ("unknown", "open", "post_failure"):
                with self.assertRaises(engine.tx.Refusal): probes.verify(workloads, True)
                self.assertEqual(calls[-1][:2], ("gateway", 8101))
            else: probes.verify(workloads, True)
            self.assertEqual(len(records), 1)
            network_evidence.validate(records[0], "a" * 32)
            self.assertIs(probes.k, k)
            self.assertTrue(all(0 < timeout <= 100 for _, _, timeout in calls))
            if mode in ("unknown", "post_failure"): self.assertEqual(negative_calls, 1)
            if mode == "open": self.assertEqual(negative_calls, 6)
            if mode == "reset": self.assertEqual(len(records[0]["observations"][3]["batches"]), 2)


if __name__ == "__main__":
    unittest.main()
