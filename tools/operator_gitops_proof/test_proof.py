"""Source inclusion and actual API readback must establish different boundaries."""
import copy
import json
from pathlib import Path
import tempfile
import unittest
from unittest import mock

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


if __name__ == "__main__":
    unittest.main()
