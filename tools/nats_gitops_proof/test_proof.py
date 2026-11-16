"""Refusal controls for the receipt and source-derived assertions; no cluster."""
import base64
import copy
import fnmatch
import importlib.util
import json
from pathlib import Path
import unittest
from unittest import mock

SPEC = importlib.util.spec_from_file_location("nats_gitops_proof", Path(__file__).with_name("proof.py"))
proof = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(proof)
SHA = "a" * 40
APP_UID = "01234567-1234-1234-1234-123456789abc"
JOB_ONE = "11234567-1234-1234-1234-123456789abc"
JOB_TWO = "21234567-1234-1234-1234-123456789abc"


class SyncAPI:
    """Only fixed Application/Job reads and operation submission; no subprocess."""
    def __init__(self, frames, prior=None):
        self.frames, self.prior = copy.deepcopy(frames), copy.deepcopy(prior)
        self.current, self.token, self.now = None, None, 0.0
        self.calls = []

    def sleep(self, seconds):
        self.now += seconds

    def __call__(self, *args, **kwargs):
        self.calls.append((args, kwargs.get("timeout"), self.now, self.token is not None))
        if "patch" in args:
            value = json.loads(args[args.index("-p") + 1])
            if "operation" in value:
                self.token = value["operation"]["info"]
            return ""
        if "application" in args:
            app = {"metadata": {"name": proof.APP, "namespace": "argocd", "uid": APP_UID,
                                 "resourceVersion": "10", "annotations": {}},
                   "status": {"sync": {"revision": SHA, "status": "Synced"}}}
            if self.token is None:
                return json.dumps(app)
            self.current = self.frames.pop(0) if len(self.frames) > 1 else self.frames[0]
            frame = self.current
            app["metadata"]["uid"] = frame.get("appUID", APP_UID)
            state = {"operation": {"info": self.token if not frame.get("oldToken") else [{"name": "old", "value": "old"}]},
                     "phase": frame.get("phase", "Succeeded"), "startedAt": frame.get("startedAt", "2026-11-16T00:00:00Z"),
                     "syncResult": {"revision": frame.get("revision", SHA), "resources": [{"kind": "Job", "name": "nats-bootstrap",
                         "namespace": proof.NAMESPACE, "syncPhase": "PostSync", "hookPhase": frame.get("phase", "Succeeded")}]}}
            if state["phase"] != "Running":
                state["finishedAt"] = frame.get("finishedAt", "2026-11-16T00:00:04Z")
            app["status"]["operationState"] = state
            return json.dumps(app)
        if "job/nats-bootstrap" in args:
            job = self.prior if self.token is None else self.current.get("job")
            return json.dumps(job) if job is not None else ""
        raise AssertionError("unexpected mock API command")


class ProofTests(unittest.TestCase):
    def hook(self, uid=JOB_ONE, state="Complete", offset=0):
        job = copy.deepcopy(next(d for d in proof.documents("kanz/infra/nats/bootstrap-job.yaml") if d["kind"] == "Job"))
        job["metadata"].update(uid=uid, resourceVersion="20", creationTimestamp=f"2026-11-16T00:00:{offset + 1:02d}Z")
        job["metadata"]["annotations"]["argocd.argoproj.io/tracking-id"] = proof.APP + ":batch/Job:" + proof.NAMESPACE + "/nats-bootstrap"
        job["status"] = {"active": 0, "failed": 0, "succeeded": 0}
        if state == "Complete":
            job["status"].update(succeeded=1, startTime=f"2026-11-16T00:00:{offset + 2:02d}Z", completionTime=f"2026-11-16T00:00:{offset + 3:02d}Z",
                                 conditions=[{"type": "Complete", "status": "True"}])
        elif state == "Failed":
            job["status"].update(failed=1, conditions=[{"type": "Failed", "status": "True"}])
        elif state == "Running":
            job["status"]["active"] = 1
        return job

    def sync_fixture(self, frames, prior=None):
        fixture = object.__new__(proof.Fixture)
        fixture.application_uid, fixture.previous_hook_uid = None, None if prior is None else prior["metadata"]["uid"]
        fixture.hook_custody = []
        api = SyncAPI(frames, prior)
        fixture.k = api
        return fixture, api

    def run_sync(self, fixture, api):
        with mock.patch.object(proof.time, "monotonic", side_effect=lambda: api.now), mock.patch.object(proof.time, "sleep", side_effect=api.sleep):
            return fixture.sync(SHA, "first" if api.prior is None else "restore-empty-stream")

    def test_sync_waits_for_one_actual_fresh_completed_hook(self):
        fixture, first_api = self.sync_fixture([{"job": self.hook()}])
        first = self.run_sync(fixture, first_api)
        pending = self.hook(JOB_TWO, "Pending", offset=10)
        new = {"startedAt": "2026-11-16T00:00:10Z", "finishedAt": "2026-11-16T00:00:14Z"}
        api = SyncAPI([{"oldToken": True, "job": self.hook()},
            new | {"phase": "Running", "job": pending}, new | {"phase": "Running", "job": self.hook(JOB_TWO, offset=10)},
            new | {"job": self.hook(JOB_TWO, offset=10)}], self.hook())
        fixture.k = api
        result = self.run_sync(fixture, api)
        self.assertEqual(result["jobUID"], JOB_TWO)
        self.assertEqual(result["custody"]["priorJobUID"], first["jobUID"])
        self.assertEqual(result["custody"]["applicationUID"], first["custody"]["applicationUID"])
        self.assertEqual(result["custody"]["samples"], 4)
        self.assertFalse(result["custody"]["operationHookInconsistent"])
        self.assertEqual(result["custody"]["jobState"], "Complete")
        self.assertEqual(api.now, 6)
        self.assertTrue(all(0 < timeout <= 15 and now < 600 for _, timeout, now, requested in api.calls if requested))

    def test_sync_never_accepts_previous_hook_or_unfinished_hook(self):
        for state in ("old-token", "previous-job", "Pending", "Running", "succeeded-without-complete", "absent"):
            with self.subTest(state=state):
                prior = self.hook()
                frame = {"phase": "Running", "oldToken": state == "old-token", "job": prior if state in ("old-token", "previous-job") else
                         None if state == "absent" else self.hook(JOB_TWO, state)}
                if state == "succeeded-without-complete":
                    frame["job"]["status"]["succeeded"] = 1
                fixture, api = self.sync_fixture([frame], prior)
                with self.assertRaises(TimeoutError):
                    self.run_sync(fixture, api)
                self.assertEqual(api.now, 600)
                self.assertEqual(fixture.hook_custody[-1]["state"], "DeadlineExpired")
                self.assertEqual(fixture.previous_hook_uid, JOB_ONE)

    def test_sync_requires_new_owned_job_and_stable_application_operation(self):
        for mutation in ("replacement", "disappearance", "application", "operation-token", "operation-start", "operation-finish", "failed", "owner", "spec", "extra-spec-drift", "source"):
            with self.subTest(mutation=mutation):
                pending = self.hook(JOB_TWO, "Pending")
                first = {"phase": "Running", "job": pending}
                second = {"job": self.hook(JOB_TWO)}
                if mutation == "replacement":
                    second["job"] = self.hook()
                elif mutation == "disappearance":
                    second["job"] = None
                elif mutation == "application":
                    second["appUID"] = JOB_ONE
                elif mutation == "operation-token":
                    second["oldToken"] = True
                elif mutation == "operation-start":
                    second["startedAt"] = "2026-11-16T00:00:01Z"
                elif mutation == "operation-finish":
                    second["finishedAt"] = "2026-11-15T23:59:59Z"
                elif mutation == "failed":
                    second["job"] = self.hook(JOB_TWO, "Failed")
                elif mutation == "owner":
                    second["job"]["metadata"]["annotations"]["argocd.argoproj.io/tracking-id"] = "unrelated-owner"
                elif mutation == "spec":
                    second["job"]["spec"]["template"]["spec"]["serviceAccountName"] = "unrelated-credential-example"
                elif mutation == "extra-spec-drift":
                    second["job"]["spec"]["suspend"] = True
                else:
                    second["revision"] = "b" * 40
                fixture, api = self.sync_fixture([first, second])
                with self.assertRaises(ValueError):
                    self.run_sync(fixture, api)
                retained = json.dumps(fixture.hook_custody)
                self.assertNotIn("unrelated-owner", retained)
                self.assertNotIn("unrelated-credential-example", retained)
                self.assertNotIn("containers", retained)

    def test_sync_refuses_preexisting_or_changed_prior_hook(self):
        for prior, recorded in ((self.hook(), None), (None, JOB_ONE), (self.hook(), JOB_TWO), (self.hook(state="Pending"), JOB_ONE)):
            with self.subTest(prior=prior is not None, recorded=recorded):
                fixture, api = self.sync_fixture([{"job": self.hook(JOB_TWO)}], prior)
                fixture.previous_hook_uid = recorded
                with self.assertRaises(ValueError):
                    self.run_sync(fixture, api)
                self.assertIsNone(api.token, "refused prior custody still submitted an operation")

    def test_sync_refuses_one_nanosecond_and_other_timestamp_contradictions(self):
        for mutation in ("one-ns", "creation-before-operation", "completion-before-start", "invalid", "leap-second", "offset", "too-precise"):
            with self.subTest(mutation=mutation):
                job = self.hook()
                frame = {"job": job}
                if mutation == "one-ns":
                    frame["finishedAt"] = "2026-11-16T00:00:03.000000001Z"
                    job["status"]["completionTime"] = "2026-11-16T00:00:03.000000002Z"
                elif mutation == "creation-before-operation":
                    job["metadata"]["creationTimestamp"] = "2026-11-15T23:59:59Z"
                elif mutation == "completion-before-start":
                    job["status"]["completionTime"] = "2026-11-16T00:00:01Z"
                elif mutation == "invalid":
                    job["metadata"]["creationTimestamp"] = "unclassified-secret-looking-input"
                elif mutation == "leap-second":
                    frame["finishedAt"] = "2026-11-16T00:00:60Z"
                elif mutation == "too-precise":
                    frame["finishedAt"] = "2026-11-16T00:00:04.1234567890Z"
                else:
                    frame["finishedAt"] = "2026-11-16T00:00:04+00:00"
                frames = [frame]
                if mutation == "one-ns":
                    pending = copy.deepcopy(frame)
                    pending["phase"] = "Running"
                    pending["job"]["status"] = {"active": 0, "failed": 0, "succeeded": 0}
                    frames = [pending, frame]
                fixture, api = self.sync_fixture(frames)
                with self.assertRaises(ValueError):
                    self.run_sync(fixture, api)
                self.assertNotIn("unclassified-secret-looking-input", json.dumps(fixture.hook_custody))
        self.assertEqual(proof.custody_time("2026-11-16T00:00:03.000000002Z") - proof.custody_time("2026-11-16T00:00:03.000000001Z"), 1)

    def test_sync_never_waits_away_success_with_an_incomplete_actual_hook(self):
        for state in ("Pending", "Running", "succeeded-without-complete"):
            for precision in ("same-second", "one-ns"):
                with self.subTest(state=state, precision=precision):
                    pending = self.hook(state=state)
                    pending["metadata"]["creationTimestamp"] = "2026-11-16T00:00:03Z"
                    if state == "succeeded-without-complete":
                        pending["status"]["succeeded"] = 1
                    complete = self.hook()
                    complete["metadata"]["creationTimestamp"] = "2026-11-16T00:00:03Z"
                    complete["status"]["startTime"] = "2026-11-16T00:00:03Z"
                    finished = "2026-11-16T00:00:03Z" if precision == "same-second" else "2026-11-16T00:00:03.000000001Z"
                    fixture, api = self.sync_fixture([{"job": pending, "finishedAt": finished}, {"job": complete, "finishedAt": finished}])
                    with self.assertRaisesRegex(ValueError, "no completed actual fresh hook"):
                        self.run_sync(fixture, api)
                    self.assertEqual(fixture.hook_custody[-1]["samples"], 1)
                    self.assertEqual(fixture.hook_custody[-1]["state"], "Refused")
                    self.assertTrue(fixture.hook_custody[-1]["operationHookInconsistent"])
                    self.assertEqual(api.now, 0, "terminal inconsistency was waited away")
                    self.assertEqual(len(api.frames), 1, "later Complete frame was consumed")

    def test_go_writer_uses_readonly_socket_while_cli_keeps_canonical_pem_path(self):
        job = next(d for d in proof.documents("kanz/infra/nats/bootstrap-job.yaml") if d["kind"] == "Job")
        inspector = proof.client_spec(job, "nats-bootstrap")
        writer = proof.client_spec(job, "market-ingest", workload_api=True)
        proof.check_writer_socket(writer)
        self.assertFalse(any(m["mountPath"] == "/run/spiffe" for m in inspector["containers"][0]["volumeMounts"]))
        self.assertTrue(any(m["mountPath"] == "/etc/nats-certs" for m in inspector["containers"][0]["volumeMounts"]))
        for mutation in ("missing", "writable", "wrong-path", "wrong-identity"):
            with self.subTest(mutation=mutation):
                broken = copy.deepcopy(writer)
                mounts = broken["containers"][0]["volumeMounts"]
                socket = next(m for m in mounts if m["mountPath"] == "/run/spiffe")
                if mutation == "missing":
                    mounts.remove(socket)
                elif mutation == "writable":
                    socket["readOnly"] = False
                elif mutation == "wrong-path":
                    socket["mountPath"] = "/run/other"
                else:
                    broken["serviceAccountName"] = "nats-bootstrap"
                with self.assertRaises(ValueError):
                    proof.check_writer_socket(broken)

    def test_workflow_selects_resolved_canonical_publisher_inputs(self):
        workflow = proof.documents(".github/workflows/nats-gitops-verification.yml")[0]
        # PyYAML's YAML 1.1 resolver represents the workflow `on` key as True.
        triggers = workflow.get("on", workflow.get(True))
        # These edges are code and generated-SDK consumers, even though the
        # diagnostic publishes a FACT. bus/command.go still compiles with bus;
        # consumer.go and producer.go call the observability trace helpers.
        inputs = ("kanz/tools/nats_gitops_proof/main.go", "kanz/pkg/bus/consumer.go",
                  "kanz/pkg/transport/tls.go", "kanz/pkg/observability/trace.go",
                  "kanz-schemas/proto/command/v1/command.proto", "kanz-schemas/proto/common/v1/decimal.proto",
                  "kanz/go.mod", "kanz/go.sum")
        for event in ("pull_request", "push"):
            patterns = triggers[event]["paths"]
            def selected(values):
                return {path for path in inputs if any(fnmatch.fnmatchcase(path, pattern) for pattern in values)}
            for path in inputs:
                with self.subTest(event=event, path=path):
                    self.assertTrue((proof.ROOT / path).is_file())
            self.assertEqual(selected(patterns), set(inputs), "a resolved publisher input does not select its actual proof")
            for selector in ("kanz/pkg/observability/**", "kanz-schemas/proto/command/**"):
                # Delete only the reviewed edge, keeping every other filter.
                # The original missing-consumer defect must become observable.
                narrowed = [pattern for pattern in patterns if pattern != selector]
                self.assertNotEqual(selected(narrowed), set(inputs))
            self.assertFalse(any(fnmatch.fnmatchcase("kanz/internal/risk/pricing/curve/curve.go", pattern) for pattern in patterns))

    def test_diagnostics_keep_categories_without_raw_content(self):
        for raw, category in (("lookup secret-host: no such host", "dns-unavailable"),
                              ("unknown flag: credential-example", "unsupported-cli-flag"),
                              ("unknown long flag '--jetstream'", "unsupported-cli-flag"),
                              ('{"fixtureError":"fixture-publish-ack-refused"}', "fixture-publish-ack-refused"),
                              ("ImagePullBackOff private-registry", "image-pull-unavailable"),
                              ("opaque token-looking diagnostic", "unclassified-refusal")):
            self.assertEqual(proof.diagnostic_category(raw), category)

    def canonical(self):
        docs = proof.documents("kanz/infra/nats/bootstrap-job.yaml")
        script = next(d for d in docs if d["kind"] == "ConfigMap" and d["metadata"]["name"] == "nats-bootstrap")["data"]["bootstrap.sh"]
        return proof.expected_streams(script, 3)

    def test_full_source_inventory_and_semantics(self):
        expected = self.canonical()
        self.assertEqual(len(expected), 18)
        self.assertEqual(expected["AUTHORITY_AUDIT"]["discard"], "new")
        self.assertEqual(expected["AUTHORITY_AUDIT"]["max_age"], 0)
        self.assertEqual(expected["POSITION"]["max_msgs_per_subject"], 1)
        actual = {name: {"config": value} for name, value in expected.items()}
        proof.check_inventory(expected, actual)
        for mutation in ("missing", "extra", "memory", "subjects", "retention", "replicas", "age", "discard", "dedup", "cap"):
            with self.subTest(mutation=mutation):
                broken = copy.deepcopy(actual)
                if mutation == "missing":
                    broken.pop("RISK")
                elif mutation == "extra":
                    broken["UNTRACKED"] = broken["RISK"]
                else:
                    field, value = {"memory": ("storage", "memory"), "subjects": ("subjects", [">"]),
                        "retention": ("retention", "workqueue"), "replicas": ("num_replicas", 1),
                        "age": ("max_age", 1), "discard": ("discard", "old"), "dedup": ("duplicate_window", 0),
                        "cap": ("max_msgs_per_subject", 9)}[mutation]
                    name = "POSITION" if mutation == "cap" else "AUTHORITY_AUDIT"
                    broken[name]["config"][field] = value
                with self.assertRaises(ValueError):
                    proof.check_inventory(expected, broken)

    def test_empty_unresolved_and_duplicate_source_refused(self):
        for source in ("", 'ensure_stream X "x.>" 1h', 'ensure_stream X "x.>" mystery',
                       'ensure_stream X "x.>" 1h\nensure_stream X "x.>" 1h'):
            with self.assertRaises(ValueError):
                proof.expected_streams(source, 3)

    def test_exact_pr_merge_and_main_sources_only(self):
        self.assertEqual(proof.source_ref("pull_request", {"number": 123}, SHA), "refs/pull/123/merge")
        self.assertEqual(proof.source_ref("push", {"ref": "refs/heads/main", "after": SHA}, SHA), SHA)
        for name, event in (("workflow_dispatch", {}), ("pull_request", {"number": "123"}),
                            ("push", {"ref": "refs/heads/main", "after": "b" * 40})):
            with self.assertRaises(ValueError):
                proof.source_ref(name, event, SHA)

    def operation(self):
        token = [{"name": "case", "value": "one"}]
        hook = {"kind": "Job", "name": "nats-bootstrap", "namespace": proof.NAMESPACE,
                "syncPhase": "PostSync", "hookPhase": "Succeeded"}
        state = {"operation": {"info": token}, "phase": "Succeeded", "startedAt": "one", "finishedAt": "two",
                 "syncResult": {"revision": SHA, "resources": [hook]}}
        return {"status": {"operationState": state}}, token

    def test_controller_result_requires_source_hook_and_phase(self):
        app, token = self.operation()
        self.assertEqual(proof.check_operation(app, SHA, token)["revision"], SHA)
        for mutation in ("source", "missing-hook", "phase", "manual-job", "wrong-namespace"):
            broken = copy.deepcopy(app)
            state = broken["status"]["operationState"]
            if mutation == "source":
                state["syncResult"]["revision"] = "b" * 40
            elif mutation == "missing-hook":
                state["syncResult"]["resources"] = []
            elif mutation == "phase":
                state["phase"] = "Failed"
            else:
                hook = state["syncResult"]["resources"][0]
                hook["syncPhase" if mutation == "manual-job" else "namespace"] = "Other"
            with self.assertRaises(ValueError):
                proof.check_operation(broken, SHA, token)
        self.assertIsNone(proof.check_operation(app, SHA, [{"name": "other"}]))

    def record_fixture(self):
        fact = {"subject": "market.crypto.quote", "frame": base64.b64encode(b"canonical-fixture").decode(),
                "headers": {"Nats-Msg-Id": "fixture", "X-Kanz-Fixture": "nats-gitops"}}
        record = {"subject": fact["subject"], "seq": 1, "data": fact["frame"], "time": "2026-11-15T00:00:00Z",
                  "hdrs": base64.b64encode(b"NATS/1.0\r\nNats-Msg-Id:fixture\r\nX-Kanz-Fixture:nats-gitops\r\n\r\n").decode()}
        return fact, record

    def test_exact_record_payload_headers_and_sequence(self):
        fact, record = self.record_fixture()
        self.assertEqual(proof.checked_record(record, fact), record)
        for key, value in (("seq", 2), ("subject", "order.order.submit"), ("data", "b3RoZXI="), ("hdrs", "")):
            with self.assertRaises(ValueError):
                proof.checked_record(record | {key: value}, fact)

    def test_cold_inventory_waits_for_missing_and_transient_responses(self):
        expected = self.canonical()
        complete = {name: {"config": value} for name, value in expected.items()}
        missing = {name: row for name, row in complete.items() if name != "RISK"}
        fixture = object.__new__(proof.Fixture)
        fact, record = self.record_fixture()
        observations, clock = {}, [10.0]
        responses = iter([missing, None, complete])
        def inventory(deadline, **kwargs):
            self.assertEqual(deadline, 10 + proof.RECOVERY_SECONDS)
            value = next(responses)
            if value is None:
                fixture.last_failure = {"category": "dependency-timeout"}
                raise RuntimeError("raw diagnostic must not be retained")
            return value
        with mock.patch.object(fixture, "inventory", side_effect=inventory), \
                mock.patch.object(fixture, "nats", return_value=json.dumps(record)) as read, \
                mock.patch.object(proof.time, "monotonic", side_effect=lambda: clock[0]), \
                mock.patch.object(proof.time, "sleep", side_effect=lambda seconds: clock.__setitem__(0, clock[0] + seconds)):
            self.assertEqual(fixture.await_recovery(expected, fact, record, observations), record)
        read.assert_called_once()
        self.assertLessEqual(read.call_args.kwargs["timeout"], 15)
        self.assertEqual(observations["attempts"], 3)
        self.assertEqual(observations["elapsedSeconds"], 4)
        self.assertEqual(observations["observedNames"], sorted(expected))
        self.assertEqual(observations["missingNames"], [])
        self.assertEqual(observations["firstInventory"]["missingNames"], ["RISK"])
        self.assertTrue(observations["passed"])
        self.assertNotIn("raw diagnostic", json.dumps(observations))

    def test_cold_inventory_deadline_refuses_missing_streams_or_unavailable_reads(self):
        expected = self.canonical()
        complete = {name: {"config": value} for name, value in expected.items()}
        fact, record = self.record_fixture()
        for condition in ("missing", "dependency-timeout", "listener-unavailable", "command-timeout", "fact-unavailable"):
            with self.subTest(condition=condition):
                fixture = object.__new__(proof.Fixture)
                observations, clock = {}, [0.0]
                def unavailable(*args, **kwargs):
                    fixture.last_failure = {"category": "dependency-timeout" if condition == "fact-unavailable" else condition}
                    raise RuntimeError("raw broker detail")
                def inventory(**kwargs):
                    if condition == "missing":
                        return {name: row for name, row in complete.items() if name != "RISK"}
                    return complete if condition == "fact-unavailable" else unavailable()
                with mock.patch.object(fixture, "inventory", side_effect=inventory), \
                        mock.patch.object(fixture, "nats", side_effect=unavailable), \
                        mock.patch.object(proof.time, "monotonic", side_effect=lambda: clock[0]), \
                        mock.patch.object(proof.time, "sleep", side_effect=lambda seconds: clock.__setitem__(0, clock[0] + seconds)):
                    with self.assertRaises(TimeoutError):
                        fixture.await_recovery(expected, fact, record, observations)
                self.assertEqual(clock[0], proof.RECOVERY_SECONDS)
                self.assertEqual(observations["elapsedSeconds"], proof.RECOVERY_SECONDS)
                self.assertEqual(observations["state"], "deadline-expired")
                self.assertTrue(observations["deadlineExpired"])
                self.assertFalse(observations["passed"])
                self.assertEqual(observations["missingNames"], ["RISK"] if condition == "missing" else [] if condition == "fact-unavailable" else None)
                self.assertNotIn("raw broker", json.dumps(observations))

    def test_cold_inventory_refuses_configuration_fact_and_auth_drift_without_retry(self):
        expected = self.canonical()
        fact, record = self.record_fixture()
        for mutation in ("extra", "storage", "missing-field", "fact", "timestamp", "auth"):
            with self.subTest(mutation=mutation):
                actual = {name: {"config": copy.deepcopy(value)} for name, value in expected.items()}
                fixture = object.__new__(proof.Fixture)
                observations = {}
                if mutation == "extra":
                    actual["UNTRACKED"] = actual["RISK"]
                elif mutation == "storage":
                    actual["RISK"]["config"]["storage"] = "memory"
                elif mutation == "missing-field":
                    del actual["RISK"]["config"]["num_replicas"]
                def inventory(**kwargs):
                    if mutation == "auth":
                        fixture.last_failure = {"category": "broker-grant-refused"}
                        raise RuntimeError("private auth diagnostic")
                    return actual
                changed = record | ({"data": "b3RoZXI="} if mutation == "fact" else {"time": "changed"})
                with mock.patch.object(fixture, "inventory", side_effect=inventory) as probe, \
                        mock.patch.object(fixture, "nats", return_value=json.dumps(changed)), \
                        mock.patch.object(proof.time, "sleep") as pause:
                    with self.assertRaises(RuntimeError if mutation == "auth" else ValueError):
                        fixture.await_recovery(expected, fact, record, observations)
                probe.assert_called_once()
                pause.assert_not_called()
                self.assertFalse(observations["passed"])
                if mutation in ("storage", "missing-field"):
                    self.assertEqual(observations["state"], "configuration-refused")
                    self.assertEqual(observations["mismatchedFields"], {"RISK": ["storage" if mutation == "storage" else "num_replicas"]})

    def test_inventory_commands_share_one_recovery_deadline(self):
        fixture = object.__new__(proof.Fixture)
        expected, clock, budgets = self.canonical(), [0.0], []
        def read(*args, timeout):
            budgets.append(timeout)
            clock[0] += 0.2
            return json.dumps(sorted(expected) if args[1] == "ls" else {"config": expected[args[2]]})
        with mock.patch.object(fixture, "nats", side_effect=read), \
                mock.patch.object(proof.time, "monotonic", side_effect=lambda: clock[0]):
            with self.assertRaises(TimeoutError):
                fixture.inventory(deadline=0.5)
        self.assertEqual(len(budgets), 3)
        self.assertTrue(all(0 < budget <= 0.5 for budget in budgets))
        self.assertAlmostEqual(budgets[-1], 0.1)
        for invalid in (None, ["RISK", "RISK"], [{}], ["risky"], ["RISK" * 30]):
            with self.subTest(invalid=invalid), mock.patch.object(fixture, "nats", return_value=json.dumps(invalid)):
                with self.assertRaises(ValueError):
                    fixture.inventory()
        for mutation in ("extra", "storage"):
            with self.subTest(mutation=mutation):
                observations = {}
                first = sorted(expected)[0]
                names = sorted(expected) + (["UNTRACKED"] if mutation == "extra" else [])
                changed = {"config": expected[first] | {"storage": "memory"}}
                with mock.patch.object(fixture, "nats", side_effect=[json.dumps(names), json.dumps(changed)]) as probe:
                    with self.assertRaises(ValueError):
                        fixture.inventory(expected=expected, observation=observations)
                self.assertEqual(probe.call_count, 1 if mutation == "extra" else 2)
                self.assertEqual(observations["state"], "configuration-refused")
                self.assertEqual(observations["unexpectedNames"], ["UNTRACKED"] if mutation == "extra" else [])

    def receipt(self):
        operation = {"revision": SHA, "phase": "Succeeded", "hookPhase": "Succeeded", "syncPhase": "PostSync"}
        wanted = next(d for d in proof.documents("kanz/infra/nats/bootstrap-job.yaml") if d["kind"] == "Job")
        syncs = []
        for offset, uid, prior in ((0, JOB_ONE, None), (10, JOB_TWO, JOB_ONE)):
            token = [{"name": "nats-gitops-case", "value": "owned-" + uid}]
            row = operation | {"jobUID": uid, "operation": token, "startedAt": f"2026-11-16T00:00:{offset:02d}Z",
                               "finishedAt": f"2026-11-16T00:00:{offset + 4:02d}Z"}
            row["custody"] = {"state": "Complete", "jobState": "Complete", "jobUID": uid, "latchedJobUID": uid, "priorJobUID": prior,
                "priorObservedJobUID": prior, "priorHookState": "Absent" if prior is None else "Complete",
                "applicationUID": APP_UID, "boundApplicationUID": APP_UID, "applicationResourceVersion": "10", "jobResourceVersion": "20",
                "ownerMatches": True, "specMatches": True, "declaredSpecSHA256": proof.spec_hash(wanted["spec"]),
                "observedSpecSHA256": proof.spec_hash(wanted["spec"]), "operationTokenSHA256": proof.spec_hash(token),
                "jobSpecSHA256": proof.spec_hash(self.hook(uid, offset=offset)["spec"]),
                "latchedJobSpecSHA256": proof.spec_hash(self.hook(uid, offset=offset)["spec"]),
                "operationStartedAt": row["startedAt"], "operationFinishedAt": row["finishedAt"],
                "jobCreatedAt": f"2026-11-16T00:00:{offset + 1:02d}Z", "jobStartedAt": f"2026-11-16T00:00:{offset + 2:02d}Z",
                "jobCompletedAt": f"2026-11-16T00:00:{offset + 3:02d}Z", "jobCounts": {"active": 0, "failed": 0, "succeeded": 1},
                "samples": 1, "operationHookInconsistent": False}
            syncs.append(row)
        before = {f"nats-{i}": {"podUID": f"pod-{i}", "pvc": f"data-nats-{i}", "pvcUID": f"pvc-{i}", "pv": f"pv-{i}", "pvUID": f"pv-uid-{i}"} for i in range(3)}
        stopped = {"replicas": 0, "pods": [], "claims": {row["pvc"]: {key: value for key, value in row.items() if key != "podUID"} for row in before.values()}}
        after = {name: row | {"podUID": row["podUID"] + "-new"} for name, row in before.items()}
        return {"source": SHA, "tree": "b" * 40, "passed": True, "cleanupPassed": True, "cases": proof.CASES[:],
                "syncs": syncs, "hookCustody": [row["custody"] for row in syncs],
                "coldRecovery": {"before": before, "stopped": stopped, "after": after},
                "streams": self.canonical(),
                "postColdRecovery": {"passed": True, "state": "recovered", "budgetSeconds": proof.RECOVERY_SECONDS,
                    "elapsedSeconds": 2, "deadlineExpired": False, "attempts": 2, "observedNames": sorted(self.canonical()),
                    "missingNames": [], "unexpectedNames": [], "mismatchedFields": {}},
                "pubAck": {"source": SHA, "stream": "MARKET", "sequence": 1, "duplicate": False, "acknowledged": True}, "factBefore": {"record": "one"},
                "factAfter": {"record": "one"}, "canonicalValidated": True, "negativeControlPassed": True}

    def test_actual_puback_cannot_be_substituted_by_a_publish_exit(self):
        ack = self.receipt()["pubAck"]
        proof.check_puback(ack, SHA)
        for key, value in (("acknowledged", False), ("stream", "EXECUTION"), ("sequence", True),
                           ("sequence", 2), ("duplicate", True), ("source", "b" * 40)):
            with self.assertRaises(ValueError):
                proof.check_puback(ack | {key: value}, SHA)

    def test_receipt_requires_completed_bound_hook_custody(self):
        for mutation in ("missing", "pending", "sticky", "stale-job", "application", "owner", "spec", "token", "latched-spec", "counter", "one-ns", "inventory"):
            with self.subTest(mutation=mutation):
                report = self.receipt()
                row = report["syncs"][1]
                custody = row["custody"]
                if mutation == "missing":
                    del row["custody"]
                elif mutation == "pending":
                    custody["jobState"] = "Pending"
                elif mutation == "sticky":
                    custody["operationHookInconsistent"] = True
                elif mutation == "stale-job":
                    custody["priorJobUID"] = JOB_TWO
                elif mutation == "application":
                    custody["applicationUID"] = JOB_ONE
                elif mutation == "owner":
                    custody["ownerMatches"] = False
                elif mutation == "spec":
                    custody["observedSpecSHA256"] = "0" * 64
                elif mutation == "token":
                    custody["operationTokenSHA256"] = "0" * 64
                elif mutation == "latched-spec":
                    custody["latchedJobSpecSHA256"] = "0" * 64
                elif mutation == "counter":
                    custody["jobCounts"]["succeeded"] = True
                elif mutation == "one-ns":
                    row["finishedAt"] = custody["operationFinishedAt"] = "2026-11-16T00:00:14.000000001Z"
                    custody["jobCompletedAt"] = "2026-11-16T00:00:14.000000002Z"
                else:
                    report["hookCustody"] = []
                with self.assertRaises(ValueError):
                    proof.check_report(report)

    def test_cold_recovery_rejects_a_live_replica_or_replaced_claim(self):
        proof.check_cold_recovery(self.receipt()["coldRecovery"])
        for mutation in ("surviving-process", "observed-live-pod", "replicas-not-zero", "missing-claim", "new-pvc", "new-pv", "missing-replica"):
            with self.subTest(mutation=mutation):
                recovery = self.receipt()["coldRecovery"]
                if mutation == "surviving-process":
                    recovery["after"]["nats-1"]["podUID"] = recovery["before"]["nats-1"]["podUID"]
                elif mutation == "observed-live-pod":
                    recovery["stopped"]["pods"] = ["nats-2"]
                elif mutation == "replicas-not-zero":
                    recovery["stopped"]["replicas"] = 1
                elif mutation == "missing-claim":
                    del recovery["stopped"]["claims"]["data-nats-0"]
                elif mutation in ("new-pvc", "new-pv"):
                    recovery["after"]["nats-0"]["pvcUID" if mutation == "new-pvc" else "pvUID"] = "replacement"
                else:
                    del recovery["before"]["nats-2"]
                with self.assertRaises(ValueError):
                    proof.check_cold_recovery(recovery)

    def test_receipt_refuses_partial_skipped_or_reused_proof(self):
        proof.check_report(self.receipt())
        for key, value in (("cases", proof.CASES[:-1]), ("cleanupPassed", False), ("canonicalValidated", False),
                           ("negativeControlPassed", False), ("passed", False), ("factAfter", {"record": "other"})):
            with self.assertRaises(ValueError):
                proof.check_report(self.receipt() | {key: value})
        for group, key in (("syncs", "jobUID"), ("syncs", "operation")):
            broken = self.receipt()
            broken[group][1][key] = broken[group][0][key]
            with self.assertRaises(ValueError):
                proof.check_report(broken)
        broken = self.receipt()
        broken["coldRecovery"]["after"]["nats-0"]["pvcUID"] = "replacement-volume"
        with self.assertRaises(ValueError):
            proof.check_report(broken)
        for key, value in (("passed", False), ("state", "deadline-expired"), ("budgetSeconds", 999),
                           ("deadlineExpired", True), ("attempts", 0), ("elapsedSeconds", proof.RECOVERY_SECONDS + 1),
                           ("observedNames", []), ("missingNames", ["RISK"]), ("unexpectedNames", ["OTHER"]),
                           ("mismatchedFields", {"RISK": ["storage"]})):
            broken = self.receipt()
            broken["postColdRecovery"][key] = value
            with self.assertRaises(ValueError):
                proof.check_report(broken)


if __name__ == "__main__":
    unittest.main()
