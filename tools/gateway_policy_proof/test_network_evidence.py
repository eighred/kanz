import copy
import json
import ast
import unittest
from unittest.mock import patch

import network_evidence
import network_fixture
import rollout_engine as engine
import bounded_process
import network_observation


def deadline_error(timeout):
    error = RuntimeError("bounded command deadline exceeded")
    error._bounded_command_observation = bounded_process._CommandObservation(
        outcome="deadline", timeoutUs=int(timeout * 1000000), elapsedUs=int(timeout * 1000000),
        stdoutBytes=0, stderrBytes=0, stdoutLimitBytes=4 << 20, stderrLimitBytes=65536,
        launched=True, killRequested=True, reaped=True, joinedStreams=3,
        cleanupFailed=False, exitCode=None, signal=9)
    return error


def diagnostic_record():
    record = complete_record()
    record["phase"] = "network_refused"
    item = record["observations"][-1]
    item.update(result="unknown", batches=[])
    item["commandFailure"] = {"phase": "tcp_exec", "kind": "command", "attempt": 1,
        "verificationElapsedUs": 169000000, "lifetimeRemainingUs": 251000000,
        "activeRemainingUs": 1000000, "observationRemainingUs": 1000000,
        "inventoryElapsedUs": 10000, "requestedTimeoutUs": 1000000,
        "process": bounded_process.command_failure(deadline_error(1))}
    return record


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
    def timed_gate(self, mode):
        clock, records, calls = [0.0], [], []
        failures = []
        def k(*args, value=None, timeout=100):
            peer, port = args[3], int(args[-1])
            calls.append((peer, port, timeout))
            negative = peer in ("untrusted", "bff", "observer", "outsider") and port in (8101, 8102, 8095)
            if (mode in ("full_timeout", "shared_timeout") and (peer, port) == ("bff", 8080)) or (
                    mode == "both_fail" and len(calls) in (4, 5)):
                error = deadline_error(timeout)
                failures.append(error)
                clock[0] += timeout
                raise error
            return json.dumps(["timeout" if negative else "connected"] * 3)
        with patch.object(engine.time, "monotonic", side_effect=lambda: clock[0]):
            probes = engine.Probes(k, None, None, "a" * 32, records.append)
            checks = [0]
            def check():
                checks[0] += 1
                if checks[0] == 43 and mode == "inventory_insufficient":
                    clock[0] += .25
                if checks[0] == 43 and mode == "inventory_timeout":
                    error = deadline_error(100)
                    failures.append(error)
                    clock[0] += 100
                    raise error
            probes.check = check
            probes.peers = {name: {"namespace": "fixture", "name": name} for name in ("gateway", "bff", "observer", "untrusted", "outsider")}
            workloads = {(engine.tx.NAMESPACE, name): {"ip": "192.0.2.1"} for name in (*engine.PORTS, "api-gateway")}
            original_tcp = probes.tcp
            def tcp(peer, address, port, expected, target, observations, deadline):
                if (peer, target) == ("bff", "api-gateway"):
                    clock[0] = {"insufficient": 169.75, "below_minimum": 169.600001, "at_minimum": 169.6,
                                "shared_timeout": 169.0, "full_timeout": 20.0,
                                "inventory_insufficient": 169.5, "inventory_timeout": 20.0}.get(mode, clock[0])
                return original_tcp(peer, address, port, expected, target, observations, deadline)
            with patch.object(probes, "tcp", side_effect=tcp):
                if mode == "at_minimum":
                    probes.verify(workloads, True)
                else:
                    with self.assertRaises((RuntimeError, engine.tx.Refusal)):
                        probes.verify(workloads, True)
            self.assertIs(probes.k, k)
        self.assertEqual(len(records), 1)
        network_evidence.validate(records[0], "a" * 32)
        return records[0], calls, failures

    def test_batch_minimum_is_derived_from_actual_unchanged_target_source(self):
        tree = ast.parse(network_observation.TCP_BATCH)
        loop = next(node for node in tree.body if isinstance(node, ast.For))
        self.assertEqual(ast.literal_eval(loop.iter.args[0]), 3)
        pause = next(node for node in loop.body if isinstance(node, ast.If))
        self.assertIsInstance(pause.test, ast.Name)
        self.assertEqual(pause.test.id, "index")
        duration = ast.literal_eval(pause.body[0].value.args[0])
        self.assertEqual((3 - 1) * duration, network_observation.TCP_BATCH_MIN_SECONDS)
        self.assertEqual(network_observation.TCP_BATCH_MIN_SECONDS, .4)

    def test_final_positive_with_insufficient_batch_budget_refuses_without_launch(self):
        record, calls, failures = self.timed_gate("insufficient")
        self.assertEqual(len(calls), 42)
        self.assertEqual(failures, [])
        self.assertEqual(len(record["observations"]), 43)
        self.assertTrue(all(item["result"] == "verified" for item in record["observations"][:-1]))
        item = record["observations"][-1]
        self.assertEqual((item["peer"], item["target"], item["batches"], item["result"]), ("bff", "api-gateway", [], "unknown"))
        value = item["commandFailure"]
        self.assertEqual(value["kind"], "insufficient_batch_budget")
        self.assertEqual(value["observationRemainingUs"], 250000)
        self.assertEqual(value["requestedTimeoutUs"], 0)
        self.assertIsNone(value["process"])

    def test_full_and_shared_deadlines_retain_distinguishing_actual_allowances(self):
        for mode, remaining, granted in (("full_timeout", 150000000, 10000000), ("shared_timeout", 1000000, 1000000)):
            with self.subTest(mode=mode):
                record, calls, failures = self.timed_gate(mode)
                value = record["observations"][-1]["commandFailure"]
                self.assertEqual(len(calls), 43)
                self.assertEqual(len(failures), 1)
                self.assertEqual((value["activeRemainingUs"], value["requestedTimeoutUs"]), (remaining, granted))
                self.assertEqual(value["process"]["timeoutUs"], granted)
                self.assertEqual(value["process"]["elapsedUs"], granted)
                self.assertEqual(record["observations"][-1]["batches"], [])

    def test_actual_batch_minimum_boundary_does_not_skip_final_positive(self):
        refused, calls, _ = self.timed_gate("below_minimum")
        self.assertEqual(len(calls), 42)
        self.assertLess(refused["observations"][-1]["commandFailure"]["observationRemainingUs"], 400000)
        passed, calls, _ = self.timed_gate("at_minimum")
        self.assertEqual(len(calls), 43)
        self.assertAlmostEqual(calls[-1][2], .4)
        self.assertEqual(passed["phase"], "network_verified")
        self.assertEqual(passed["observations"][-1]["batches"], [["connected"] * 3])

    def test_inventory_time_and_inventory_command_failure_are_not_socket_results(self):
        refused, calls, _ = self.timed_gate("inventory_insufficient")
        value = refused["observations"][-1]["commandFailure"]
        self.assertEqual(len(calls), 42)
        self.assertEqual((value["phase"], value["inventoryElapsedUs"]), ("tcp_exec", 250000))
        self.assertIsNone(value["process"])
        refused, calls, failures = self.timed_gate("inventory_timeout")
        value = refused["observations"][-1]["commandFailure"]
        self.assertEqual((len(calls), len(failures)), (42, 1))
        self.assertEqual((value["phase"], value["requestedTimeoutUs"], value["inventoryElapsedUs"]), ("inventory", 100000000, 100000000))
        self.assertEqual(refused["observations"][-1]["batches"], [])

    def test_negative_and_post_positive_command_failures_are_both_retained(self):
        record, calls, failures = self.timed_gate("both_fail")
        self.assertEqual(len(calls), 5)
        self.assertEqual(len(failures), 2)
        items = record["observations"][-2:]
        self.assertEqual([item["peer"] for item in items], ["untrusted", "gateway"])
        self.assertTrue(all(item["result"] == "unknown" and item["batches"] == [] and "commandFailure" in item for item in items))

    def test_command_diagnostic_parser_refuses_raw_null_nonfinite_and_impossible_facts(self):
        good = diagnostic_record()
        network_evidence.validate(good, "a" * 32)
        cases = [("raw", "rawArgv", ["private"]), ("null", "process", None),
                 ("bool", "requestedTimeoutUs", True), ("nan", "activeRemainingUs", float("nan")),
                 ("inf", "inventoryElapsedUs", float("inf")), ("attempt", "attempt", 2),
                 ("negative", "observationRemainingUs", -1), ("phase", "phase", None),
                 ("kind", "kind", []), ("grant", "requestedTimeoutUs", 2000000)]
        for mode, key, value in cases:
            bad = copy.deepcopy(good)
            bad["observations"][-1]["commandFailure"][key] = value
            with self.subTest(mode=mode), self.assertRaises(ValueError): network_evidence.validate(bad, "a" * 32)
        process_cases = [("raw", "stderr", "private"), ("bool", "exitCode", True), ("negative", "stderrBytes", -1),
                         ("overflow", "stdoutBytes", (4 << 20) + 65537), ("short", "elapsedUs", 999999),
                         ("both_codes", "exitCode", 1), ("signal", "signal", 65),
                         ("not_reaped", "reaped", False), ("kill", "killRequested", False),
                         ("streams", "joinedStreams", 4), ("launch", "launched", False),
                         ("outcome", "outcome", {}), ("limit", "stderrLimitBytes", 65537),
                         ("cleanup", "cleanupFailed", None)]
        for mode, key, value in process_cases:
            bad = copy.deepcopy(good)
            bad["observations"][-1]["commandFailure"]["process"][key] = value
            with self.subTest(mode=mode), self.assertRaises(ValueError): network_evidence.validate(bad, "a" * 32)
        for mode in ("record_verified", "item_verified", "insufficient_with_process", "insufficient_with_budget"):
            bad = copy.deepcopy(good)
            value = bad["observations"][-1]["commandFailure"]
            if mode == "record_verified": bad["phase"] = "network_verified"
            elif mode == "item_verified": bad["observations"][-1].update(result="verified", batches=[["connected"] * 3])
            elif mode == "insufficient_with_process": value.update(kind="insufficient_batch_budget", observationRemainingUs=250000, requestedTimeoutUs=0)
            else: value.update(kind="insufficient_batch_budget", process=None, requestedTimeoutUs=0)
            with self.subTest(mode=mode), self.assertRaises(ValueError): network_evidence.validate(bad, "a" * 32)

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
