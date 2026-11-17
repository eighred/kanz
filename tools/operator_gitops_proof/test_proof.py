"""Source inclusion and actual API readback must establish different boundaries."""
import copy
import io
import json
from pathlib import Path
import tempfile
import unittest
from unittest import mock
from types import SimpleNamespace

import yaml

import proof


class OperatorSourceTests(unittest.TestCase):
    def test_canonical_source_keeps_job_suspended_and_outside_selected_resources(self):
        files, resources, application, project = proof.operator_source()
        self.assertTrue(files["halt-job.yaml"][0]["spec"]["suspend"])
        self.assertFalse(any(key[0] == "Job" for key in resources))
        self.assertEqual(len(resources), 13)
        self.assertEqual(application["metadata"]["name"], "kanz-primary-operator")
        self.assertEqual(project["metadata"]["name"], "kanz")

    def source_mutation(self, mutate):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            for path in (proof.ROOT / "kanz/infra/operator").rglob("*.yaml"):
                target = root / path.relative_to(proof.ROOT)
                target.parent.mkdir(parents=True, exist_ok=True)
                target.write_text(path.read_text(encoding="utf-8"), encoding="utf-8")
            gitops = root / "kanz/infra/gitops"
            gitops.mkdir()
            for name in ("applicationset.yaml", "projects.yaml"):
                (gitops / name).write_text((proof.ROOT / "kanz/infra/gitops" / name).read_text(encoding="utf-8"), encoding="utf-8")
            mutate(root)
            with mock.patch.object(proof, "ROOT", root):
                proof.operator_source()

    @staticmethod
    def change_appset(root, edit):
        path = root / "kanz/infra/gitops/applicationset.yaml"
        appset = yaml.safe_load(path.read_text())
        edit(appset)
        path.write_text(yaml.safe_dump(appset), encoding="utf-8")

    def test_removing_exact_job_exclusion_refuses_before_cluster_creation(self):
        def mutate(root):
            self.change_appset(root, lambda value: value["spec"]["template"]["spec"]["source"]["directory"].update(exclude="{tenancy.yaml}"))
        with self.assertRaisesRegex(ValueError, "automatic reconciliation boundary"):
            self.source_mutation(mutate)

    def test_excluding_ordinary_identity_refuses(self):
        def mutate(root):
            self.change_appset(root, lambda value: value["spec"]["template"]["spec"]["source"]["directory"].update(exclude="{halt-job.yaml,cli-identities.yaml}"))
        with self.assertRaisesRegex(ValueError, "ordinary operator identity excluded"):
            self.source_mutation(mutate)

    def test_missing_identity_file_is_not_a_successful_empty_inventory(self):
        with self.assertRaisesRegex(ValueError, "identity inventory differs"):
            self.source_mutation(lambda root: (root / "kanz/infra/operator/cli-identities.yaml").unlink())

    def test_identity_in_excluded_invocation_refuses(self):
        def mutate(root):
            directory = root / "kanz/infra/operator"
            with (directory / "halt-job.yaml").open("a", encoding="utf-8") as output:
                output.write("\n---\n" + (directory / "halt-identity.yaml").read_text())
            (directory / "halt-identity.yaml").unlink()
        with self.assertRaisesRegex(ValueError, "manual invocation contains identity"):
            self.source_mutation(mutate)

    def test_automatic_halt_hook_cannot_enter_as_another_file(self):
        def mutate(root):
            directory = root / "kanz/infra/operator"
            job = yaml.safe_load((directory / "halt-job.yaml").read_text())
            job["metadata"]["name"] = "second-halt"
            job["metadata"]["annotations"] = {"argocd.argoproj.io/hook": "PostSync"}
            (directory / "second-job.yaml").write_text(yaml.safe_dump(job))
        with self.assertRaisesRegex(ValueError, "unexpected automatic operator resource"):
            self.source_mutation(mutate)


class OperatorReadbackTests(unittest.TestCase):
    def setUp(self):
        _, self.resources, _, _ = proof.operator_source()
        self.accounts = [copy.deepcopy(item) for key, item in self.resources.items() if key[0] == "ServiceAccount"]
        self.accounts.append({"kind": "ServiceAccount", "metadata": {"name": "default", "namespace": proof.NAMESPACE}})
        self.policies = [copy.deepcopy(item) for key, item in self.resources.items() if key[0] == "NetworkPolicy"]
        self.namespace = copy.deepcopy(self.resources[("Namespace", "", proof.NAMESPACE)])

    def check(self, jobs=None, marker=None):
        proof.check_inventory(self.resources, self.accounts, self.policies, jobs or [], self.namespace, marker)

    def test_complete_readback_passes(self):
        self.check()

    def test_missing_actual_identity_refuses(self):
        self.accounts.pop(0)
        with self.assertRaisesRegex(ValueError, "service-account inventory differs"):
            self.check()

    def test_missing_actual_policy_refuses(self):
        self.policies.pop()
        with self.assertRaisesRegex(ValueError, "policy inventory differs"):
            self.check()

    def test_halt_image_pull_identity_cannot_disappear(self):
        halt = next(item for item in self.accounts if item["metadata"]["name"] == "kanz-halt")
        halt.pop("imagePullSecrets")
        with self.assertRaisesRegex(ValueError, "image-pull identity differs"):
            self.check()

    def test_widened_actual_egress_refuses(self):
        self.policies[0]["spec"]["egress"] = [{}]
        with self.assertRaisesRegex(ValueError, "egress policy differs"):
            self.check()

    def test_even_suspended_actual_halt_job_refuses(self):
        with self.assertRaisesRegex(ValueError, "halt Job reached automatic"):
            self.check([{"kind": "Job", "metadata": {"name": "kanz-halt"}, "spec": {"suspend": True}}])

    def test_missing_actual_spiffe_namespace_label_refuses(self):
        self.namespace["metadata"]["labels"].pop("kanz.internal/spiffe")
        with self.assertRaisesRegex(ValueError, "lost SPIRE label"):
            self.check()

    def test_rollback_requires_annotation_removal(self):
        model = next(item for item in self.accounts if item["metadata"]["name"] == "kanz-model")
        model["metadata"].setdefault("annotations", {})[proof.MARKER] = "revised"
        self.check(marker="revised")
        with self.assertRaisesRegex(ValueError, "revision marker differs"):
            self.check()


class OperatorOperationTests(unittest.TestCase):
    def application(self):
        return {"status": {"sync": {"status": "Synced", "revision": "a" * 40},
            "operationState": {"phase": "Succeeded", "finishedAt": "2026-11-17T00:00:00Z",
                               "syncResult": {"revision": "a" * 40}}}}

    def test_exact_completed_operation_passes(self):
        self.assertTrue(proof.check_operation(self.application(), "a" * 40))

    def test_stale_operation_does_not_prove_rollback(self):
        self.assertFalse(proof.check_operation(self.application(), "b" * 40))

    def test_active_operation_is_not_completed(self):
        app = self.application()
        app["operation"] = {"sync": {}}
        self.assertFalse(proof.check_operation(app, "a" * 40))

    def test_out_of_sync_self_heal_waits_for_readback(self):
        app = self.application()
        app["status"]["sync"]["status"] = "OutOfSync"
        self.assertFalse(proof.check_operation(app, "a" * 40))

    def test_failed_exact_operation_refuses(self):
        app = self.application()
        app["status"]["operationState"]["phase"] = "Failed"
        with self.assertRaisesRegex(ValueError, "operation refused"):
            proof.check_operation(app, "a" * 40)

    def test_same_revision_previous_success_cannot_prove_new_request(self):
        token = [{"name": "operator-gitops-request", "value": "b" * 32}]
        self.assertFalse(proof.check_operation(self.application(), "a" * 40, token))

    def test_exact_request_token_and_completion_pass(self):
        token = [{"name": "operator-gitops-request", "value": "b" * 32}]
        app = self.application()
        app["status"]["operationState"]["operation"] = {"info": token}
        self.assertTrue(proof.check_operation(app, "a" * 40, token))

    def test_other_completed_request_cannot_prove_new_request(self):
        token = [{"name": "operator-gitops-request", "value": "b" * 32}]
        app = self.application()
        app["status"]["operationState"]["operation"] = {"info": [{"name": "operator-gitops-request", "value": "c" * 32}]}
        self.assertFalse(proof.check_operation(app, "a" * 40, token))

    def observed_fixture(self, seconds_per_call):
        _, resources, _, _ = proof.operator_source()
        accounts = [copy.deepcopy(item) for key, item in resources.items() if key[0] == "ServiceAccount"]
        accounts.append({"kind": "ServiceAccount", "metadata": {"name": "default", "namespace": proof.NAMESPACE}})
        policies = [copy.deepcopy(item) for key, item in resources.items() if key[0] == "NetworkPolicy"]
        app = self.application()
        app["status"]["resources"] = [{"kind": key[0], "namespace": key[1], "name": key[2]} for key in resources]
        clock = [0]
        fixture = proof.Fixture.__new__(proof.Fixture)

        def k(*args, timeout=30):
            self.assertGreater(timeout, 0)
            self.assertLessEqual(timeout, 10)
            clock[0] += seconds_per_call
            if "application" in args:
                return json.dumps(app)
            if "serviceaccounts" in args:
                return json.dumps({"items": accounts})
            if "networkpolicies" in args:
                return json.dumps({"items": policies})
            if "jobs" in args:
                return json.dumps({"items": []})
            return json.dumps(resources[("Namespace", "", proof.NAMESPACE)])

        fixture.k = k
        return fixture, resources, clock

    def test_complete_observation_within_deadline_passes(self):
        fixture, resources, clock = self.observed_fixture(5)
        with mock.patch.object(proof.time, "monotonic", side_effect=lambda: clock[0]):
            fixture.observe(resources, "a" * 40, None)

    def test_late_complete_readback_is_not_success(self):
        fixture, resources, clock = self.observed_fixture(26)
        with mock.patch.object(proof.time, "monotonic", side_effect=lambda: clock[0]):
            with self.assertRaisesRegex(TimeoutError, "late operator reconciliation success refused"):
                fixture.observe(resources, "a" * 40, None)

    def test_requested_sync_waits_for_its_token_before_readback(self):
        fixture, resources, clock = self.observed_fixture(5)
        original = fixture.k
        requested = []

        def k(*args, timeout=30):
            if "patch" in args:
                payload = json.loads(args[-1])
                # Changing the desired revision separately lets automatic sync
                # finish before this particular manual operation is recorded.
                self.assertEqual(payload["spec"]["source"]["targetRevision"], "a" * 40)
                self.assertEqual(payload["operation"]["sync"]["revision"], "a" * 40)
                requested.append(payload["operation"]["info"])
                return "{}"
            value = json.loads(original(*args, timeout=timeout))
            if "application" in args:
                value["status"]["operationState"]["operation"] = {"info": requested[-1]}
            return json.dumps(value)

        fixture.k = k
        with mock.patch.object(proof.time, "monotonic", side_effect=lambda: clock[0]):
            fixture.observe(resources, "a" * 40, None, request_sync=True)
            fixture.observe(resources, "a" * 40, None, request_sync=True)
        self.assertEqual(len(requested), 2)
        self.assertNotEqual(requested[0], requested[1])

    def test_requested_same_revision_stale_success_times_out_without_inventory(self):
        fixture, resources, clock = self.observed_fixture(5)
        inventory = []

        def k(*args, timeout=30):
            self.assertGreater(timeout, 0)
            self.assertLessEqual(timeout, 10)
            if "patch" in args:
                return "{}"
            clock[0] += 40
            if "application" not in args:
                inventory.append(args)
                raise AssertionError("stale operation reached inventory readback")
            return json.dumps(self.application())

        fixture.k = k
        with mock.patch.object(proof.time, "monotonic", side_effect=lambda: clock[0]), mock.patch.object(proof.time, "sleep"):
            with self.assertRaisesRegex(TimeoutError, "reconciliation deadline expired"):
                fixture.observe(resources, "a" * 40, None, request_sync=True)
        self.assertEqual(inventory, [])
        self.assertTrue(fixture.last_observation["operationRevisionMatches"])
        self.assertFalse(fixture.last_observation["requestInfoMatches"])
        self.assertIsNone(fixture.last_observation["readbackMatches"])


class OperatorCreationOwnershipTests(unittest.TestCase):
    source = "a" * 40
    name = "messaging-proof-bbbbbbbbbbbb"

    def fixture(self, scratch):
        process = SimpleNamespace(command_failure=lambda error: None)
        with mock.patch.object(proof, "load_module", return_value=process):
            fixture = proof.Fixture(scratch, self.name)
        return fixture

    def environment(self, directory):
        return mock.patch.dict(proof.os.environ, {"PATH": "/fixture/bin", "RUNNER_TEMP": directory,
            "GITHUB_ACTIONS": "true", "GITHUB_SHA": self.source, "GITHUB_RUN_ID": "123", "GITHUB_RUN_ATTEMPT": "1"})

    def test_preexisting_cluster_main_writes_no_owner_and_never_deletes(self):
        with tempfile.TemporaryDirectory() as directory, self.environment(directory):
            commands = []
            feed = SimpleNamespace(create_enforcing_cluster=mock.Mock())
            process = SimpleNamespace(command_failure=lambda error: None)

            def command(fixture, args, data=None, timeout=60):
                commands.append(args)
                if args[:2] == ["git", "rev-parse"]:
                    return self.source
                if args == ["kind", "get", "clusters"]:
                    return self.name
                raise AssertionError("preflight refusal reached an effect")

            def load(name, path):
                return feed if path == "tools/feed_identity/proof.py" else process

            with mock.patch.object(proof.uuid, "uuid4", return_value=SimpleNamespace(hex="b" * 32)), \
                    mock.patch.object(proof, "load_module", side_effect=load), \
                    mock.patch.object(proof.Fixture, "command", command), \
                    mock.patch("sys.stdout", new_callable=io.StringIO):
                with self.assertRaises(SystemExit) as result:
                    proof.main()
            self.assertEqual(result.exception.code, 1)
            self.assertFalse((Path(directory) / "operator-gitops-owner.json").exists())
            self.assertFalse(any(args[:3] == ["kind", "delete", "cluster"] for args in commands))
            feed.create_enforcing_cluster.assert_not_called()

    def test_preflight_without_create_command_issues_no_cleanup_authorization(self):
        with tempfile.TemporaryDirectory() as directory, self.environment(directory):
            fixture = self.fixture(Path(directory))
            fixture.command = mock.Mock(return_value="")
            feed = SimpleNamespace(create_enforcing_cluster=mock.Mock(side_effect=RuntimeError("before create")))
            with mock.patch.object(proof, "load_module", return_value=feed):
                with self.assertRaisesRegex(RuntimeError, "before create"):
                    fixture.install({}, {}, {}, self.source)
            self.assertFalse(fixture.owner.exists())
            self.assertFalse(fixture.cleanup())
            self.assertFalse(any(call.args[0][:3] == ["kind", "delete", "cluster"] for call in fixture.command.call_args_list))

    def test_ambiguous_create_keeps_matching_cleanup_authorization(self):
        with tempfile.TemporaryDirectory() as directory, self.environment(directory):
            fixture = self.fixture(Path(directory))
            commands = []

            def command(args, data=None, timeout=60):
                commands.append(args)
                if args[:3] == ["kind", "create", "cluster"]:
                    raise RuntimeError("lost create response")
                return ""

            fixture.command = command
            feed = SimpleNamespace()

            def create(name, scratch):
                feed.command(["kind", "create", "cluster", "--name", name,
                              "--kubeconfig", str(scratch / "kubeconfig")])

            feed.create_enforcing_cluster = create
            with mock.patch.object(proof, "load_module", return_value=feed):
                with self.assertRaisesRegex(RuntimeError, "lost create response"):
                    fixture.install({}, {}, {}, self.source)
            recorded = json.loads(fixture.owner.read_text())
            proof.validate_creation_authorization(recorded, self.source, "123", "1")
            self.assertEqual(recorded, fixture.creation_authorization)
            self.assertTrue(fixture.cleanup())
            self.assertFalse(fixture.owner.exists())
            self.assertEqual(sum(args[:3] == ["kind", "delete", "cluster"] for args in commands), 1)

    def test_tampered_creation_authorization_cannot_delete(self):
        with tempfile.TemporaryDirectory() as directory, self.environment(directory):
            fixture = self.fixture(Path(directory))
            fixture.command = mock.Mock(return_value="")
            fixture.authorize_creation(self.source)
            identity = json.loads(fixture.owner.read_text())
            identity["source"] = "c" * 40
            fixture.owner.write_text(json.dumps(identity))
            with self.assertRaisesRegex(ValueError, "creation authorization refused"):
                fixture.cleanup()
            self.assertFalse(any(call.args[0][:3] == ["kind", "delete", "cluster"] for call in fixture.command.call_args_list))

    def test_cleanup_without_creation_authorization_never_deletes(self):
        with tempfile.TemporaryDirectory() as directory, self.environment(directory):
            fixture = self.fixture(Path(directory))
            fixture.command = mock.Mock()
            self.assertFalse(fixture.cleanup())
            fixture.command.assert_not_called()

    def test_create_edge_refuses_collision_or_wrong_target_without_cleanup_rights(self):
        for conflict in ("collision", "foreign-name"):
            with self.subTest(conflict=conflict), tempfile.TemporaryDirectory() as directory, self.environment(directory):
                fixture = self.fixture(Path(directory))
                commands = []
                inspections = [0]

                def command(args, data=None, timeout=60):
                    commands.append(args)
                    if args == ["kind", "get", "clusters"]:
                        inspections[0] += 1
                        return self.name if conflict == "collision" and inspections[0] == 2 else ""
                    raise AssertionError("refused create reached an effect")

                fixture.command = command
                feed = SimpleNamespace()

                def create(name, scratch):
                    target = "messaging-proof-cccccccccccc" if conflict == "foreign-name" else name
                    feed.command(["kind", "create", "cluster", "--name", target,
                                  "--kubeconfig", str(scratch / "kubeconfig")])

                feed.create_enforcing_cluster = create
                with mock.patch.object(proof, "load_module", return_value=feed):
                    with self.assertRaises(ValueError):
                        fixture.install({}, {}, {}, self.source)
                self.assertFalse(fixture.owner.exists())
                self.assertFalse(fixture.cleanup())
                self.assertFalse(any(args[:3] in (["kind", "create", "cluster"], ["kind", "delete", "cluster"]) for args in commands))

    def test_authorization_rejects_wrong_job_or_cluster_binding(self):
        identity = {"schema": 1, "cluster": self.name, "source": self.source, "runID": "123",
                    "runAttempt": "1", "creationID": "b" * 32, "creationAuthorized": True}
        proof.validate_creation_authorization(identity, self.source, "123", "1")
        for field, value in (("schema", True), ("cluster", "messaging-proof-cccccccccccc"),
                             ("source", "c" * 40), ("runID", "124"), ("runAttempt", "2"),
                             ("creationID", "c" * 32), ("creationAuthorized", False)):
            with self.subTest(field=field):
                changed = dict(identity, **{field: value})
                with self.assertRaisesRegex(ValueError, "creation authorization refused"):
                    proof.validate_creation_authorization(changed, self.source, "123", "1")


if __name__ == "__main__":
    unittest.main()
