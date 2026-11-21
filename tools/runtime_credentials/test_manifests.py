"""Dummy/source-only projection and independent migration-identity oracles."""

import copy
import json
from pathlib import Path
import unittest

import yaml

import manifests


class ManifestTests(unittest.TestCase):
    def setUp(self):
        self.assertEqual(yaml.__version__, "6.0.3")
        self.documents = manifests.base_inputs(Path(__file__).resolve().parents[2])

    def test_base_separates_all_three_roles_and_preserves_callers(self):
        before = copy.deepcopy(self.documents)
        prerequisites, cutover, receipt = manifests.phase_one(self.documents)
        self.assertEqual(self.documents, before)
        self.assertEqual(receipt["phase"], "preparation-only")
        self.assertTrue(receipt["runtime_policies_require_separate_operator_authorization"])
        by_key = {manifests.identity(item): item for item in prerequisites + cutover}
        for service in manifests.SERVICES:
            with self.subTest(service=service):
                pod = by_key[("Deployment", "kanz-services", service)]["spec"]["template"]["spec"]
                self.assertEqual(pod["serviceAccountName"], service)
                self.assertNotIn("initContainers", pod)
                job = by_key[("Job", "kanz-services", service + "-credential-migration")]
                migration = job["spec"]["template"]["spec"]
                self.assertEqual(migration["serviceAccountName"], service + "-migrate")
                self.assertEqual(migration["volumes"][0]["csi"]["volumeAttributes"]["secretProviderClass"], service + "-migration-db")
                for suffix, role, key in (("-db", service, "dsn"), ("-migration-db", service + "-migrate", "migrate_dsn")):
                    provider = by_key[("SecretProviderClass", "kanz-services", service + suffix)]
                    self.assertEqual(provider["spec"]["parameters"]["roleName"], role)
                    fields = yaml.safe_load(provider["spec"]["parameters"]["objects"])
                    self.assertEqual([item["secretKey"] for item in fields], [key])
                    expected_path = "kv/data/kanz/" + service + ("-migration" if key == "migrate_dsn" else "-runtime")
                    self.assertEqual(fields[0]["secretPath"], expected_path)
                original = next(item for item in before if manifests.identity(item) == ("Deployment", "kanz-services", service))
                self.assertEqual(pod["containers"], original["spec"]["template"]["spec"]["containers"])
        signing = by_key[("SecretProviderClass", "kanz-services", "identity-signing-key")]
        self.assertEqual(yaml.safe_load(signing["spec"]["parameters"]["objects"])[0]["secretPath"], "kv/data/kanz/identity-runtime")

    def test_tokyo_preserves_transformed_immutable_images_and_scopes_database_peer(self):
        for item in self.documents:
            if item.get("kind") == "Deployment":
                pod = item["spec"]["template"]["spec"]
                pod["initContainers"][0]["image"] = "012619468098.dkr.ecr.ap-northeast-1.amazonaws.com/kanz-migrate@sha256:" + "a" * 64
                item["metadata"].setdefault("annotations", {})["kanz.io/release-commit"] = "b" * 40
        prerequisites, _, receipt = manifests.phase_one(self.documents, "tokyo")
        self.assertEqual(receipt["database_plane"], "tokyo")
        for item in prerequisites:
            if item["kind"] == "Job":
                self.assertEqual(item["metadata"]["annotations"]["kanz.io/release-commit"], "b" * 40)
                self.assertTrue(item["spec"]["template"]["spec"]["containers"][0]["image"].endswith("a" * 64))
            if item["kind"] == "NetworkPolicy":
                peer = item["spec"]["egress"][1]["to"][0]
                self.assertEqual(peer["namespaceSelector"]["matchLabels"]["kubernetes.io/metadata.name"], "kanz-data")
                self.assertEqual(peer["podSelector"]["matchLabels"]["cnpg.io/cluster"], "kanz-testnet-postgres")
                self.assertEqual(item["spec"]["ingress"], [])

    def test_changed_contracts_fail_closed(self):
        mutations = (
            lambda pod: pod.update(serviceAccountName="identity-migrate"),
            lambda pod: pod["initContainers"][0].update(image="unqualified:latest"),
            lambda pod: pod["initContainers"][0].update(args=["--dir", "/migrations/oms"]),
            lambda pod: pod["initContainers"][0]["volumeMounts"].append({"name": "signing-key", "mountPath": "/key"}),
            lambda pod: pod["initContainers"].append(copy.deepcopy(pod["initContainers"][0])),
        )
        for mutate in mutations:
            with self.subTest(mutation=mutate):
                documents = copy.deepcopy(self.documents)
                deployment = next(item for item in documents if manifests.identity(item) == ("Deployment", "kanz-services", "audit"))
                mutate(deployment["spec"]["template"]["spec"])
                with self.assertRaises(ValueError):
                    manifests.phase_one(documents)
        for name in ("identity-db", "identity-signing-key"):
            documents = copy.deepcopy(self.documents)
            provider = next(item for item in documents if manifests.identity(item) == ("SecretProviderClass", "kanz-services", name))
            provider["spec"]["parameters"]["roleName"] = "identity-migrate"
            with self.assertRaises(ValueError):
                manifests.phase_one(documents)
        with self.assertRaises(ValueError):
            manifests.phase_one(self.documents + [self.documents[0]])


if __name__ == "__main__":
    suite = unittest.defaultTestLoader.loadTestsFromTestCase(ManifestTests)
    names = sorted(test.id().rsplit(".", 1)[1] for test in suite)
    result = unittest.TextTestRunner(verbosity=2).run(suite)
    if not result.wasSuccessful() or result.skipped or result.testsRun != 3:
        raise SystemExit(1)
    print(json.dumps({"tests": names, "passed": result.testsRun, "skipped": [], "yaml_version": yaml.__version__}))
