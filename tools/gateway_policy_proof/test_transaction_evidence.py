import copy
import unittest
import json

import inspect
import transaction_fixture as fixture

import proof
import rollout_probe
from test_network_evidence import complete_record, diagnostic_record
from transaction_fixture import CASES, validate_report, deployment_diagnostic, runtime_failure_category, synthetic_runtime_detail


class _ControllerControlAPI:
    """In-memory fixed Kubernetes API; it never executes a command."""
    def __init__(self):
        pod = rollout_probe.caller_pod(fixture.tx.NAMESPACE, "policy-untrusted", "a" * 32)
        pod["spec"]["restartPolicy"] = "Always"
        pod["spec"].pop("activeDeadlineSeconds")
        self.controller = {"apiVersion": "apps/v1", "kind": "ReplicaSet",
                           "metadata": {"name": "probe-adoption-control", "namespace": fixture.tx.NAMESPACE},
                           "spec": {"replicas": 0, "selector": {"matchLabels": {"app": "policy-untrusted"}},
                                    "template": {"metadata": {"labels": pod["metadata"]["labels"]}, "spec": pod["spec"]}}}
        self.created = copy.deepcopy(self.controller)
        self.created["metadata"].update(uid="aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", generation=1, resourceVersion="1")
        self.created["spec"]["template"]["metadata"]["creationTimestamp"] = None
        spec = self.created["spec"]["template"]["spec"]
        spec.update(dnsPolicy="ClusterFirst", terminationGracePeriodSeconds=30, schedulerName="default-scheduler")
        spec["containers"][0].update(terminationMessagePath="/dev/termination-log", terminationMessagePolicy="File")
        self.created["status"] = {"replicas": 0}
        self.now, self.step = 0, 1
        self.calls, self.reads, self.deletes, self.absences = [], 0, 0, 0
        self.never_settles = False
        self.mutate = lambda obj: None
        self.list_mutate = lambda obj: None
        self.observe_error, self.delete_error = None, None

    def clock(self):
        return self.now

    def pause(self, seconds):
        if not 0 < seconds <= 0.1:
            raise AssertionError("unexpected fixture pause")
        self.now += seconds

    def k(self, *args, value=None, timeout=180):
        self.calls.append((args, copy.deepcopy(value), timeout, self.now))
        if args == ("create", "-f", "-", "-o", "json"):
            if value != self.controller:
                raise AssertionError("wrong intended create")
            self.now += 1
            return json.dumps(self.created)
        self.now += self.step
        if args == ("-n", fixture.tx.NAMESPACE, "get", "replicaset", "probe-adoption-control", "-o", "json"):
            if self.observe_error is not None:
                raise self.observe_error
            self.reads += 1
            obj = copy.deepcopy(self.created)
            obj["metadata"]["resourceVersion"] = str(self.reads + 1)
            if self.reads > 1 and not self.never_settles:
                obj["status"]["observedGeneration"] = 1
            self.mutate(obj)
            return json.dumps(obj)
        if args == ("delete", "--raw", "/apis/apps/v1/namespaces/" + fixture.tx.NAMESPACE + "/replicasets/probe-adoption-control", "-f", "-"):
            self.deletes += 1
            if self.delete_error is not None:
                raise self.delete_error
            if value != {"apiVersion": "v1", "kind": "DeleteOptions",
                         "preconditions": {"uid": "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", "resourceVersion": "3"}}:
                raise AssertionError("delete did not use the one settled snapshot")
            return "{}"
        if args == ("get", "--raw", "/apis/apps/v1/namespaces/" + fixture.tx.NAMESPACE
                    + "/replicasets?fieldSelector=metadata.name%3Dprobe-adoption-control"):
            self.absences += 1
            listing = {"apiVersion": "apps/v1", "kind": "ReplicaSetList", "items": []}
            if self.absences == 1:
                obj = copy.deepcopy(self.created)
                obj["metadata"].update(resourceVersion="4", deletionTimestamp="2026-01-01T00:00:00Z")
                obj["status"]["observedGeneration"] = 1
                listing["items"] = [obj]
            self.list_mutate(listing)
            return json.dumps(listing)
        raise AssertionError("unexpected controller-control API")


class TransactionEvidenceTests(unittest.TestCase):
    def test_command_diagnostic_is_hash_bound_failure_not_transaction_acceptance(self):
        record = diagnostic_record()
        fixture.network_evidence.validate(record, "a" * 32)
        receipt = {"record": record, "sha256": fixture.tx.fingerprint(record), "durableSequence": 28, "receiptCount": 35}
        with self.assertRaises(ValueError):
            fixture.validate_network_receipt(receipt)
        # The normal durable serializer includes every numeric diagnostic field.
        before = fixture.tx.fingerprint(record)
        record["observations"][-1]["commandFailure"]["process"]["stderrBytes"] = 1
        self.assertNotEqual(fixture.tx.fingerprint(record), before)
        with self.assertRaises(ValueError):
            fixture.validate_network_receipt(receipt)
        record["phase"] = "network_verified"
        with self.assertRaises(ValueError):
            fixture.network_evidence.validate(record, "a" * 32)

    def test_durable_network_evidence_refuses_missing_tampered_or_unfinished(self):
        record = complete_record()
        good = {"record": record, "sha256": fixture.tx.fingerprint(record), "durableSequence": 25, "receiptCount": 32}
        fixture.validate_network_receipt(good)
        baseline = complete_record(False)
        bad = {**good, "record": baseline, "sha256": fixture.tx.fingerprint(baseline)}
        with self.assertRaises(ValueError): fixture.validate_network_receipt(bad)
        for field, value in (("sha256", "a" * 64), ("durableSequence", 0), ("receiptCount", 25),
                             ("receiptCount", 65), ("receiptCount", True), ("record", {})):
            bad = copy.deepcopy(good); bad[field] = value
            with self.subTest(field=field), self.assertRaises((ValueError, KeyError)):
                fixture.validate_network_receipt(bad)
        source = inspect.getsource(fixture.run_fixture)
        self.assertLess(source.index("durable(value)"), source.index('raw = (receipt_directory'))
        self.assertIn('if value["format"] == "kanz-gateway-network-v1":', source)
        self.assertIn('durable.close()', source)

    def test_actual_ipv6_control_requires_exact_api_shape_and_refusal(self):
        created = []
        mode = "valid"
        def k(*args, value=None):
            if args[0] == "create":
                created.append(copy.deepcopy(value)); return ""
            if args[3] == "service": return "present" if mode == "service" else ""
            item = copy.deepcopy(created[-1])
            item["metadata"]["uid"] = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
            if mode == "expanded": item["endpoints"][0]["addresses"] = [fixture.IPV6_EXPANDED]
            if mode == "target": item["endpoints"][0]["targetRef"] = {"uid": "other"}
            if mode == "unknown": item["endpoints"][0]["conditions"] = {}
            return json.dumps(item)
        control = fixture.ipv6_slice_control(k)
        self.assertTrue(control["absenceRefused"])
        self.assertEqual(control["shape"]["endpoints"][0]["addresses"], ["2001:db8::1"])
        for mode in ("service", "expanded", "target", "unknown"):
            with self.subTest(mode=mode), self.assertRaises((ValueError, AssertionError)):
                fixture.ipv6_slice_control(k)
        mode = "valid"
        from unittest.mock import patch
        with patch.object(fixture.rollout_probe, "endpoint_boundary", return_value=None):
            with self.assertRaises(ValueError): fixture.ipv6_slice_control(k)
        source = inspect.getsource(fixture.run_fixture)
        call = 'report["ipv6EndpointSlice"] = ipv6_slice_control(k)'
        self.assertLess(source.index('report["cases"].append("empty_endpoint_slice_null")'), source.index(call))
        self.assertLess(source.index(call), source.index('report["cases"].append("ipv6_endpoint_metadata")'))
        self.assertLess(source.index(call), source.index("probes.create()"))

    def test_synthetic_runtime_detail_removes_variables_and_bounds(self):
        raw = 'failed to resolve image "private-name": https://example.invalid/token /private/path token=secret 127.0.0.1:123 sha256:' + 'a' * 64
        value = synthetic_runtime_detail(raw)
        for private in ('private-name', 'example.invalid', '/private', 'secret', '127.0.0.1', 'a' * 64):
            self.assertNotIn(private, value)
        self.assertIn('failed to resolve image', value)
        self.assertLessEqual(len(synthetic_runtime_detail('x' * 4096)), 512)
        self.assertEqual(synthetic_runtime_detail('x' * 4097), 'unavailable')

    def test_runtime_message_only_exports_closed_category(self):
        private = " /private/token/credential=https://example.invalid/secret "
        for text, category in (("no space left on device", "disk_exhausted"),
                ("executable file not found", "missing_executable"),
                ("content digest sha256:abc not found", "missing_image_content"),
                ("error mounting volume", "mount_setup"), ("seccomp failed", "security_setup"),
                ("read-only file system", "read_only"), ("permission denied", "permission"),
                ("no such file or directory", "missing_path"), ("unrecognized", "unknown")):
            self.assertEqual(runtime_failure_category(private + text), category)
        for value in (None, {}, "no space left on device" + "x" * 4096):
            self.assertEqual(runtime_failure_category(value), "unknown")

    def test_diagnostic_is_closed_and_bounded(self):
        private = "private-message-https://example.invalid/token"
        deployment = {"status": {"replicas": 1, "readyReplicas": False, "conditions": [
            {"type": "Progressing", "status": "False", "reason": "FailedCreate", "message": private},
            {"type": private, "status": private, "reason": private}]}}
        pods = [{"metadata": {"name": private}, "status": {"phase": "Pending", "conditions": [
            {"type": "PodScheduled", "status": "False", "reason": "Unschedulable", "message": private}], "containerStatuses": [
            {"ready": False, "restartCount": 2, "state": {"waiting": {"reason": "ImagePullBackOff", "message": private}}}]}}]
        result = deployment_diagnostic(deployment, pods)
        self.assertNotIn(private, json.dumps(result))
        self.assertIsNone(result["replicas"]["readyReplicas"])
        self.assertEqual(result["pods"][0]["containers"][0]["waiting"], "ImagePullBackOff")
        self.assertEqual(result["pods"][0]["conditions"][0]["reason"], "Unschedulable")
        for bad in (pods * 17, [{"status": {"containerStatuses": [{}] * 5}}]):
            with self.assertRaises(ValueError): deployment_diagnostic(deployment, bad)

    def test_actual_null_control_shape_ownership_and_wiring(self):
        service_uid = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
        item = {"apiVersion": "discovery.k8s.io/v1", "kind": "EndpointSlice", "addressType": "IPv4", "endpoints": None,
                "metadata": {"namespace": fixture.tx.NAMESPACE, "uid": "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb",
                             "ownerReferences": [{"kind": "Service", "name": "empty-endpoint-control", "uid": service_uid, "controller": True}]}}
        created = []
        def k(*args, value=None):
            if args[0] == "create":
                created.append(copy.deepcopy(value)); return ""
            if args[3] == "pods": return '{"items":[]}'
            if args[3] == "service":
                return json.dumps({"metadata": {"uid": service_uid}, "spec": created[-1]["spec"]})
            if args[3] == "endpointslices": return json.dumps({"items": [item]})
            raise AssertionError("unexpected operation")
        result = fixture.empty_slice_control(k)
        self.assertIn("endpoints", result["shape"])
        self.assertIsNone(result["shape"]["endpoints"])
        for change in ("array", "missing", "owner"):
            bad = copy.deepcopy(item)
            if change == "array": bad["endpoints"] = []
            elif change == "missing": del bad["endpoints"]
            else: bad["metadata"]["ownerReferences"][0]["uid"] = "foreign"
            original = item
            item = bad
            with self.subTest(change=change), self.assertRaises((ValueError, KeyError, AssertionError)):
                fixture.empty_slice_control(k)
            item = original
        text = inspect.getsource(fixture.run_fixture)
        required = ('report["emptyEndpointSlice"] = empty_slice_control(k)',
                    'report["cases"].append("empty_endpoint_slice_null")',
                    'rollout_probe.endpoint_boundary(slices, probes.addresses)')
        def wired(source):
            return all(source.count(part) >= 1 for part in required) and source.index(required[0]) < source.index("probes.create()")
        self.assertTrue(wired(text))
        for part in required:
            with self.subTest(part=part): self.assertFalse(wired(text.replace(part, "removed")))
        self.assertEqual(len(CASES), 29)
        self.assertEqual(CASES[1:9], ["response_loss_" + str(i) for i in range(1, 9)])

    def test_exact_source_attempt_complete_cases_cleanup_and_identity_required(self):
        network_record = complete_record()
        report = {"networkReceipt": {"record": network_record, "sha256": fixture.tx.fingerprint(network_record), "durableSequence": 25, "receiptCount": 32}, "format": "kanz-gateway-policy-transaction-proof-v1", "source": "a" * 40, "tree": "b" * 40,
                  "runID": "123", "runAttempt": "2", "renderSHA256": "c" * 64, "nodeImage": proof.feed.NODE,
                  "cluster": "cni-proof-" + "d" * 12, "cases": list(CASES), "passed": True, "cleanupPassed": True,
                  "emptyEndpointSlice": {"serviceUID": "a"*8+"-"+"a"*4+"-"+"a"*4+"-"+"a"*4+"-"+"a"*12,
                      "sliceUID": "b"*8+"-"+"b"*4+"-"+"b"*4+"-"+"b"*4+"-"+"b"*12,
                      "shape": {"apiVersion": "discovery.k8s.io/v1", "kind": "EndpointSlice", "addressType": "IPv4", "endpoints": None}},
                  "ipv6EndpointSlice": {"sliceUID": "cccccccc-cccc-cccc-cccc-cccccccccccc", "serviceName": "ipv6-control-" + "a"*32,
                      "serviceAbsent": True, "observedAddress": fixture.IPV6_EXPANDED, "literalComparisonMiss": True, "absenceRefused": True,
                      "shape": {"apiVersion": "discovery.k8s.io/v1", "kind": "EndpointSlice", "addressType": "IPv6",
                                "endpoints": [{"addresses": ["2001:db8::1"], "conditions": {"ready": False}}]}},
                  "retainedRestore": {"cases": list(fixture.restore_fixture.CASES), "syntheticLegacyReceipts": True,
                      "historicalProvenanceQualified": False, "actualAppendOnlyJournals": True, "singleAttemptEnforced": True, "actualManagedFieldsProjection": True},
                  "annotationTransport": {key: True for key in ("oldClientMutation", "rawMetadataPreserved", "dryRunUnchanged", "staleRVRefused", "wrongUIDRefused", "rollbackPreserved", "finalizePreserved")},
                  "imageIdentity": {"root": rollout_probe.IMAGE.split("@", 1)[1], "manifest": "sha256:" + "e" * 64,
                                    "config": "sha256:" + "f" * 64, "platform": "linux/amd64", "rootMediaType": "application/vnd.oci.image.manifest.v1+json"}}
        def check(value):
            validate_report(value, "a" * 40, "b" * 40, "123", "2", "c" * 64)
        check(report)
        for key, value in (("source", "e" * 40), ("tree", "e" * 40), ("runID", "124"), ("runAttempt", "1"),
                           ("renderSHA256", "d" * 64), ("controllerControlFailure", {}), ("cleanupFailure", False), ("cases", CASES[:-1]), ("cases", list(reversed(CASES))),
                           ("passed", 1), ("cleanupPassed", False), ("imageIdentity", {}), ("emptyEndpointSlice", None), ("emptyEndpointSlice", {}),
                           ("ipv6EndpointSlice", None), ("ipv6EndpointSlice", {}), ("annotationTransport", None), ("annotationTransport", {}), ("retainedRestore", None), ("retainedRestore", {})):
            bad = copy.deepcopy(report); bad[key] = value
            with self.subTest(key=key, value=value), self.assertRaises(ValueError):
                check(bad)
        for key in report["annotationTransport"]:
            bad = copy.deepcopy(report); bad["annotationTransport"][key] = False
            with self.subTest(control=key), self.assertRaises(ValueError): check(bad)
        for key, value in (("endpoints", []), ("endpoints", {}), ("endpoints", False), ("addressType", "IPv6")):
            bad = copy.deepcopy(report); bad["emptyEndpointSlice"]["shape"][key] = value
            with self.assertRaises(ValueError): check(bad)
        bad = copy.deepcopy(report); del bad["emptyEndpointSlice"]["shape"]["endpoints"]
        with self.assertRaises(ValueError): check(bad)
        for key, value in (("serviceAbsent", False), ("absenceRefused", False), ("literalComparisonMiss", False),
                           ("observedAddress", "2001:db8::1"), ("sliceUID", "unknown"), ("sliceUID", "-" * 36),
                           ("serviceName", "other")):
            bad = copy.deepcopy(report); bad["ipv6EndpointSlice"][key] = value
            with self.subTest(key=key), self.assertRaises(ValueError): check(bad)

    def test_controller_control_settles_then_deletes_once_under_one_joint_budget(self):
        api, report = _ControllerControlAPI(), {}
        fixture.controller_adoption_control(api.k, api.controller, report, clock=api.clock, pause=api.pause)
        self.assertEqual((api.reads, api.deletes, api.absences), (2, 1, 2))
        self.assertEqual(report, {})
        bounded = api.calls[1:]
        self.assertEqual(len(bounded), 5)
        raw_list = ("get", "--raw", "/apis/apps/v1/namespaces/" + fixture.tx.NAMESPACE
                    + "/replicasets?fieldSelector=metadata.name%3Dprobe-adoption-control")
        self.assertEqual([args for args, _, _, _ in bounded if args[:2] == ("get", "--raw")],
                         [raw_list, raw_list])
        self.assertFalse(any(args[:4] == ("-n", fixture.tx.NAMESPACE, "get", "replicasets")
                             for args, _, _, _ in bounded))
        for _, _, timeout, started in bounded:
            self.assertAlmostEqual(timeout, 31 - started)
        self.assertLess(api.now, 31)
        self.assertEqual(bounded[2][1]["preconditions"]["resourceVersion"], "3")
        text = inspect.getsource(fixture.run_fixture)
        call = "controller_adoption_control(k, controller, report)"
        self.assertEqual(text.count(call), 1)
        self.assertLess(text.index(call), text.index('report["cases"].append("controller_adoption_refusal")'))
        self.assertNotIn('"--for=delete", "replicaset/probe-adoption-control"', text)
        self.assertEqual(len(CASES), 29)

    def test_controller_control_creation_must_match_intent_and_only_pinned_defaults(self):
        changes = {
            "replicas": lambda obj: obj["spec"].update(replicas=1),
            "selector": lambda obj: obj["spec"]["selector"]["matchLabels"].update(app="foreign"),
            "labels": lambda obj: obj["spec"]["template"]["metadata"]["labels"].update(app="foreign"),
            "image": lambda obj: obj["spec"]["template"]["spec"]["containers"][0].update(image="foreign"),
            "sidecar": lambda obj: obj["spec"]["template"]["spec"]["containers"].append({"name": "foreign"}),
            "init": lambda obj: obj["spec"]["template"]["spec"].update(initContainers=[{"name": "foreign"}]),
            "default": lambda obj: obj["spec"]["template"]["spec"].update(dnsPolicy="Default"),
            "bool": lambda obj: obj["spec"].update(replicas=False),
        }
        for name, change in changes.items():
            api = _ControllerControlAPI()
            change(api.created)
            with self.subTest(name=name), self.assertRaises(ValueError):
                fixture.controller_adoption_control(api.k, api.controller, {}, clock=api.clock, pause=api.pause)
            self.assertEqual(len(api.calls), 1)
            self.assertEqual(api.deletes, 0)
        api = _ControllerControlAPI()
        api.controller["spec"]["replicas"] = 1
        with self.assertRaises(ValueError):
            fixture.controller_adoption_control(api.k, api.controller, {}, clock=api.clock, pause=api.pause)
        self.assertEqual(api.calls, [])

    def test_controller_control_foreign_identity_or_spec_refuses_before_delete(self):
        changes = {
            "uid": lambda obj: obj["metadata"].update(uid="bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"),
            "generation": lambda obj: obj["metadata"].update(generation=2),
            "namespace": lambda obj: obj["metadata"].update(namespace="foreign"),
            "name": lambda obj: obj["metadata"].update(name="foreign"),
            "deleting": lambda obj: obj["metadata"].update(deletionTimestamp="2026-01-01T00:00:00Z"),
            "finalizer": lambda obj: obj["metadata"].update(finalizers=["foreign"]),
            "owner": lambda obj: obj["metadata"].update(ownerReferences=[{"uid": "foreign"}]),
            "label": lambda obj: obj["metadata"].update(labels={"foreign": "value"}),
            "annotation": lambda obj: obj["metadata"].update(annotations={"foreign": "value"}),
            "spec": lambda obj: obj["spec"]["template"]["spec"].update(schedulerName="foreign"),
        }
        for name, change in changes.items():
            api = _ControllerControlAPI()
            api.mutate = change
            with self.subTest(name=name), self.assertRaises(ValueError):
                fixture.controller_adoption_control(api.k, api.controller, {}, clock=api.clock, pause=api.pause)
            self.assertEqual(api.deletes, 0)

    def test_controller_control_status_requires_explicit_zero_and_typed_optional_counts(self):
        changes = {
            "missing": lambda obj: obj["status"].pop("replicas"),
            "replicas": lambda obj: obj["status"].update(replicas=1),
            "bool": lambda obj: obj["status"].update(replicas=False),
            "ready": lambda obj: obj["status"].update(readyReplicas=1),
            "null": lambda obj: obj["status"].update(availableReplicas=None),
            "terminating": lambda obj: obj["status"].update(terminatingReplicas=-1),
            "conditions": lambda obj: obj["status"].update(conditions=[{"type": "ReplicaFailure"}]),
            "condition-type": lambda obj: obj["status"].update(conditions={}),
            "unknown": lambda obj: obj["status"].update(unknown=0),
            "observed-ahead": lambda obj: obj["status"].update(observedGeneration=2),
            "observed-bool": lambda obj: obj["status"].update(observedGeneration=True),
            "observed-negative": lambda obj: obj["status"].update(observedGeneration=-1),
        }
        for name, change in changes.items():
            api = _ControllerControlAPI()
            api.mutate = change
            with self.subTest(name=name), self.assertRaises(ValueError):
                fixture.controller_adoption_control(api.k, api.controller, {}, clock=api.clock, pause=api.pause)
            self.assertEqual(api.deletes, 0)
        api = _ControllerControlAPI()
        api.created["status"].update(fullyLabeledReplicas=0, readyReplicas=0, availableReplicas=0,
                                     terminatingReplicas=0, conditions=[], observedGeneration=0)
        fixture.controller_adoption_control(api.k, api.controller, {}, clock=api.clock, pause=api.pause)
        self.assertEqual(api.deletes, 1)

    def test_controller_control_unobserved_generation_or_slow_read_cannot_reset_budget(self):
        for mode in ("never", "slow"):
            api = _ControllerControlAPI()
            api.never_settles = True
            if mode == "slow":
                api.step = 30
            with self.subTest(mode=mode), self.assertRaisesRegex(RuntimeError, "joint deadline"):
                fixture.controller_adoption_control(api.k, api.controller, {}, clock=api.clock, pause=api.pause)
            self.assertEqual(api.deletes, 0)
            self.assertTrue(all(timeout <= 31 - started for _, _, timeout, started in api.calls[1:]))
            self.assertLessEqual(api.reads, 28 if mode == "never" else 1)


    def test_controller_control_late_delete_or_absence_response_cannot_claim_success(self):
        from unittest.mock import patch
        for stage in ("delete", "absence", "parsed-absence"):
            api = _ControllerControlAPI()
            original, loads = api.k, json.loads
            def late(*args, **kwargs):
                result = original(*args, **kwargs)
                if ((stage == "delete" and args[0] == "delete")
                        or (stage == "absence" and api.absences == 2)):
                    api.now = 31
                return result
            def parsed(*args, **kwargs):
                result = loads(*args, **kwargs)
                if (stage == "parsed-absence" and type(result) is dict
                        and result.get("kind") == "ReplicaSetList" and result.get("items") == []):
                    api.now = 31
                return result
            with self.subTest(stage=stage), patch.object(fixture.json, "loads", side_effect=parsed), \
                 self.assertRaisesRegex(RuntimeError, "joint deadline"):
                fixture.controller_adoption_control(late, api.controller, {}, clock=api.clock, pause=api.pause)
            self.assertEqual(api.deletes, 1)
            self.assertEqual(api.absences, 0 if stage == "delete" else 2)

    def test_controller_control_delete_failure_preserves_primary_and_never_refreshes_or_retries(self):
        import bounded_process
        for where in ("observe", "delete"):
            api, report = _ControllerControlAPI(), {}
            private = RuntimeError("private-token-and-child-output")
            values = dict(outcome="nonzero", timeoutUs=30000000, elapsedUs=1, stdoutBytes=0, stderrBytes=123,
                          stdoutLimitBytes=4194304, stderrLimitBytes=65536, launched=True, killRequested=False,
                          reaped=True, joinedStreams=3, cleanupFailed=False, exitCode=1, signal=None)
            private._bounded_command_observation = bounded_process._CommandObservation(**values)
            if where == "observe":
                api.observe_error = private
            else:
                api.delete_error = private
            with self.subTest(where=where), self.assertRaises(RuntimeError) as raised:
                fixture.controller_adoption_control(api.k, api.controller, report, clock=api.clock, pause=api.pause)
            self.assertIs(raised.exception, private)
            self.assertEqual(report, {"controllerControlFailure": {
                "operation": "controller_control_" + where, "process": values}})
            self.assertNotIn("private", json.dumps(report))
            self.assertEqual(api.absences, 0)
            self.assertEqual(api.deletes, 0 if where == "observe" else 1)
            self.assertEqual(api.reads, 0 if where == "observe" else 2)
        api, report = _ControllerControlAPI(), {}
        api.delete_error = RuntimeError("unclassified-private-output")
        with self.assertRaises(RuntimeError):
            fixture.controller_adoption_control(api.k, api.controller, report, clock=api.clock, pause=api.pause)
        self.assertEqual(report["controllerControlFailure"], {"operation": "controller_control_delete", "process": None})

    def test_controller_control_absence_must_be_typed_empty_and_not_a_replacement(self):
        changes = {
            "kind": lambda obj: obj.update(kind="List"),
            "old-projection": lambda obj: obj.update(apiVersion="v1", kind="List"),
            "version": lambda obj: obj.update(apiVersion="v1"),
            "items": lambda obj: obj.update(items=None),
            "duplicate": lambda obj: obj["items"].append(copy.deepcopy(obj["items"][0])),
            "replacement": lambda obj: obj["items"][0]["metadata"].update(uid="bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"),
            "foreign-name": lambda obj: obj["items"][0]["metadata"].update(name="foreign"),
            "spec": lambda obj: obj["items"][0]["spec"].update(replicas=1),
            "finalizer": lambda obj: obj["items"][0]["metadata"].update(finalizers=["foreign"]),
        }
        for name, change in changes.items():
            api = _ControllerControlAPI()
            api.list_mutate = change
            with self.subTest(name=name), self.assertRaises(ValueError):
                fixture.controller_adoption_control(api.k, api.controller, {}, clock=api.clock, pause=api.pause)
            self.assertEqual((api.deletes, api.absences), (1, 1))

    def test_controller_control_cleanup_failure_does_not_mask_fixture_primary(self):
        from unittest.mock import patch
        primary, secondary = RuntimeError("fixture-primary"), RuntimeError("fixture-cleanup")
        class Artifact:
            text = None
            fail = False
            def write_text(self, text):
                self.text = text
                if self.fail:
                    raise OSError("fixture-artifact-failure")
        artifact = Artifact()
        rendered = [{"metadata": {"name": fixture.tx.NAMES[-1]}, "spec": {"egress": [
            {"to": [{"podSelector": {"matchLabels": {"app": app}}}], "ports": [{"protocol": "TCP", "port": port}]}
            for app, port in (("accounting", 8101), ("audit", 8102))]}}]
        def run(argv, **kwargs):
            if argv == ["git", "rev-parse", "HEAD"]:
                return "a" * 40
            if argv == ["git", "rev-parse", "HEAD^{tree}"]:
                return "b" * 40
            if argv[:3] == ["kind", "delete", "cluster"]:
                raise secondary
            raise AssertionError("unexpected fixture operation")
        for write_failure in (False, True):
            artifact.fail = write_failure
            with self.subTest(artifact_write_failure=write_failure), \
                 patch.object(fixture, "qualify_codec"), patch.object(fixture.proof, "render", return_value=(rendered, None, "c" * 64)), \
                 patch.object(fixture.proof, "claim_cluster"), patch.object(fixture.proof.feed, "command"), \
                 patch.object(fixture.proof.feed, "create_enforcing_cluster", side_effect=primary), \
                 patch.object(fixture, "bounded_run", side_effect=run), \
                 patch.dict(fixture.os.environ, {"GITHUB_RUN_ID": "123", "GITHUB_RUN_ATTEMPT": "1"}):
                with self.assertRaises(RuntimeError) as raised:
                    fixture.run_fixture(artifact)
            self.assertIs(raised.exception, primary)
            notes = getattr(primary, "__notes__", [])
            self.assertIn("transaction fixture cleanup failed", notes)
            if write_failure:
                self.assertIn("transaction artifact retention failed", notes)
            else:
                self.assertNotIn("transaction artifact retention failed", notes)
            saved = json.loads(artifact.text)
            self.assertIs(saved["passed"], False)
            self.assertIs(saved["cleanupPassed"], False)
            self.assertIs(saved["cleanupFailure"], True)
            self.assertNotIn("fixture-primary", artifact.text)
            self.assertNotIn("fixture-cleanup", artifact.text)
            self.assertNotIn("fixture-artifact-failure", artifact.text)



if __name__ == "__main__":
    unittest.main()
