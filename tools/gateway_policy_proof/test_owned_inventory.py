"""One exact owner-selected inventory preserves per-batch probe checks."""
import copy
import json
import unittest
import uuid
from unittest.mock import patch

import rollout_engine as engine
import rollout_probe as probe


class OwnedInventoryTests(unittest.TestCase):
    def fixture(self):
        nonce = "a" * 32
        calls, receipts, pods = [], [], []
        def k(*args, value=None, timeout=100):
            calls.append(args)
            if args[1] == "pods":
                self.assertEqual(args, ("get", "pods", "-A", "-l", probe.OWNER + "=" + nonce, "-o", "json"))
                return json.dumps({"items": pods})
            return '{"items":[]}'
        owner = engine.Probes(k, None, None, nonce, receipts.append)
        for index, peer in enumerate(("api-gateway", "web-bff", "policy-observer", "policy-untrusted", "api-gateway")):
            namespace = "kanz-services" if index < 4 else owner.namespace
            desired = probe.caller_pod(namespace, peer, nonce)
            actual = copy.deepcopy(desired)
            actual["metadata"].update(uid=str(uuid.uuid4()), resourceVersion="1")
            address = "192.0.2." + str(index + 1)
            actual["status"] = {"phase": "Running", "podIP": address, "podIPs": [{"ip": address}],
                                "conditions": [{"type": "Ready", "status": "False"}],
                                "containerStatuses": [{"name": "probe", "imageID": "verified-image", "state": {"running": {}}}]}
            pods.append(actual)
            owner.owned.append({"namespace": namespace, "name": desired["metadata"]["name"],
                                "uid": actual["metadata"]["uid"], "desired": desired, "verifiedImageID": "verified-image",
                                "address": address, "addresses": [address], "addressState": "observed"})
        owner.addresses = [p["address"] for p in owner.owned]
        return owner, pods, calls

    def test_one_list_keeps_all_other_checks_at_each_call(self):
        owner, pods, calls = self.fixture()
        with patch.object(probe, "service_boundary", wraps=probe.service_boundary) as service, \
             patch.object(probe, "controller_boundary", wraps=probe.controller_boundary) as controller, \
             patch.object(probe, "running_unready", wraps=probe.running_unready) as running, \
             patch.object(probe, "endpoint_boundary", wraps=probe.endpoint_boundary) as endpoints:
            owner.check(); owner.check()
            self.assertEqual(len(calls), 8)
            self.assertEqual(sum(call[1] == "pods" for call in calls), 2)
            self.assertEqual((service.call_count, controller.call_count, running.call_count, endpoints.call_count), (10, 10, 10, 2))
            self.assertEqual([entry["address"] for entry in owner.owned], [p["status"]["podIP"] for p in pods])

    def test_inventory_and_unchanged_pod_boundaries_fail_closed(self):
        for change in ("missing", "extra", "duplicate", "label", "uid", "namespace", "name", "malformed", "metadata", "labels", "list_name", "image", "ready", "address", "spec"):
            owner, pods, _ = self.fixture()
            if change == "missing": pods.pop()
            elif change == "extra": pods.append(copy.deepcopy(pods[0]))
            elif change == "duplicate": pods[-1] = copy.deepcopy(pods[0])
            elif change == "label": pods[0]["metadata"]["labels"][probe.OWNER] = "b" * 32
            elif change == "uid": pods[0]["metadata"]["uid"] = str(uuid.uuid4())
            elif change in ("namespace", "name"): pods[0]["metadata"][change] = "foreign"
            elif change == "malformed": pods[0] = None
            elif change == "metadata": pods[0]["metadata"] = None
            elif change == "labels": pods[0]["metadata"]["labels"] = None
            elif change == "list_name": pods[0]["metadata"]["name"] = []
            elif change == "image": pods[0]["status"]["containerStatuses"][0]["imageID"] = "different"
            elif change == "ready": pods[0]["status"]["conditions"][0]["status"] = "True"
            elif change == "address": pods[0]["status"].update(podIP="192.0.2.99", podIPs=[{"ip": "192.0.2.99"}])
            else: pods[0]["spec"]["hostNetwork"] = True
            with self.subTest(change=change), self.assertRaises((ValueError, engine.tx.Refusal)): owner.check()
        for value in (None, [], {"items": None}, {"items": {}}, {}):
            owner, _, _ = self.fixture()
            original = owner.k
            owner.k = lambda *args, **kwargs: json.dumps(value) if args[1] == "pods" else original(*args, **kwargs)
            with self.subTest(value=value), self.assertRaises((ValueError, engine.tx.Refusal)): owner.check()

    def test_inventory_shape_precedes_real_ownership_authority(self):
        for change in ("duplicate", "foreign", "uid", "nonce"):
            owner, pods, _ = self.fixture()
            if change == "duplicate": pods[-1] = copy.deepcopy(pods[0])
            elif change == "foreign": pods[-1]["metadata"]["name"] = "foreign"
            elif change == "uid": pods[0]["metadata"]["uid"] = str(uuid.uuid4())
            else: pods[0]["metadata"]["labels"][probe.OWNER] = "b" * 32
            with patch.object(owner, "observe", wraps=owner.observe) as observe:
                with self.assertRaises(engine.tx.Refusal): owner.check()
                self.assertEqual(observe.call_count, 0 if change in ("duplicate", "foreign") else 1)
