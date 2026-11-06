"""Execute the unchanged operator engine against a disposable Kubernetes API stub.

The stub supplies only synthetic metadata. This proves refusal/ownership flow,
not actual Kubernetes, Vault, database, registry, or policy acceptance.
"""
import base64
import gzip
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parent
PG = "ghcr.io/cloudnative-pg/postgresql@sha256:533e45e0a00c156bdab7c4d9a84fffa87259594725a3909c898e8645727d9555"
MIGRATE = "012619468098.dkr.ecr.ap-northeast-1.amazonaws.com/kanz-migrate@sha256:147db5e4edbb6151f44f1b2c799d14d44437298dc00e19d8ad983d54ab333a24"
UID = "11111111-1111-1111-1111-111111111111"


def obj(kind, name, spec=None):
    value = {"apiVersion": "v1", "kind": kind, "metadata": {"name": name, "namespace": "kanz-data"}}
    if spec is not None:
        value["spec"] = spec
    return value


def desired():
    cm = obj("ConfigMap", "postgres-provisioner")
    cm["data"] = {"provision.sh": "reviewed synthetic script"}
    jobs = []
    for name, container, image in [("tv-sync-provisioner", "provisioner", PG), ("tv-sync-migration", "migrate-tv-sync", MIGRATE)]:
        jobs.append(obj("Job", name, {"template": {"spec": {"containers": [{"name": container, "image": image}]}}}))
    return {"apiVersion": "v1", "kind": "List", "items": [cm, *jobs, obj("SecretProviderClass", "tv-sync-role-passwords", {"provider": "vault"})]}


@unittest.skipUnless(os.name == "posix", "operator engine executes on Linux; Windows source checks are separate")
class PreparationEngine(unittest.TestCase):
    def invoke(self, scenario="success", apply="1"):
        for binary in ("bash", "jq"):
            self.assertIsNotNone(shutil.which(binary), f"required fixture binary {binary} missing")
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            fixture = desired()
            cm = fixture["items"][0].copy()
            cm["data"] = {"provision.sh": "original script"}
            cm["metadata"] = {**cm["metadata"], "uid": UID, "resourceVersion": "1", "labels": {"preserve": "yes"}}
            state = {"scenario": scenario, "objects": {"configmap/postgres-provisioner": cm}, "mutations": []}
            if scenario in ("complete-reuse", "immutable-mismatch"):
                cm["data"] = fixture["items"][0]["data"]
                for index, job in enumerate(fixture["items"][1:3]):
                    job = json.loads(json.dumps(job))
                    job["metadata"]["uid"] = "33333333-3333-3333-3333-" + str(index).zfill(12)
                    job["status"] = {"active": 0, "conditions": [{"type": "Complete", "status": "True"}]}
                    if scenario == "immutable-mismatch":
                        job["spec"]["template"]["spec"]["containers"][0]["image"] = "unreviewed"
                    state["objects"]["job/" + job["metadata"]["name"]] = job
            (root / "state.json").write_text(json.dumps(state))
            (root / "desired.json").write_text(json.dumps(fixture))
            stub = root / "k3s"
            stub.write_text("#!/bin/sh\nexec python3 " + str(ROOT / "tv_sync_preparation_fixture.py") + ' "$@"\n')
            stub.chmod(0o700)
            raw = json.dumps(fixture).encode()
            payload, manifest = root / "payload", root / "manifest"
            payload.write_bytes(base64.b64encode(gzip.compress(raw)))
            env = {**os.environ, "PATH": str(root) + os.pathsep + os.environ["PATH"], "TV_PREP_FIXTURE": str(root)}
            command = 'payload="$1"; manifest="$2"; expected_hash="$3"; apply="$4"; source "$5"'
            result = subprocess.run(["bash", "-c", command, "fixture", str(payload), str(manifest), hashlib.sha256(raw).hexdigest(), apply, str(ROOT / "prepare-testnet-tv-sync.sh")], env=env, capture_output=True, text=True, timeout=30)
            state = json.loads((root / "state.json").read_text())
            return result, state

    def test_success_binds_current_owned_jobs(self):
        result, state = self.invoke()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("tv-sync-database-preparation-verified", result.stdout)
        self.assertEqual(len([x for x in state["mutations"] if x.startswith("create:job/")]), 2)
        self.assertEqual(state["objects"]["configmap/postgres-provisioner"]["metadata"]["labels"], {"preserve": "yes"})

    def test_multi_document_kubectl_json_stream(self):
        result, _ = self.invoke("json-stream")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("tv-sync-database-preparation-verified", result.stdout)

    def test_dry_run_never_mutates(self):
        result, state = self.invoke(apply="0")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(state["mutations"], [])

    def test_preflight_unknown_or_active_refuses_before_mutation(self):
        for scenario in ("wrong-cluster", "busy-base", "api-error", "busy-existing", "immutable-mismatch"):
            with self.subTest(scenario=scenario):
                result, state = self.invoke(scenario)
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(state["mutations"], [])

    def test_stale_owner_or_image_cannot_attest_current_job(self):
        for scenario in ("stale-pod", "wrong-image", "failed-migration", "create-response-lost"):
            with self.subTest(scenario=scenario):
                result, state = self.invoke(scenario)
                self.assertNotEqual(result.returncode, 0)
                self.assertNotIn("tv-sync-database-preparation-verified", result.stdout)
                self.assertIn("preparation-rollback-configuration-restored", result.stdout)
                self.assertEqual(set(state["objects"]), {"configmap/postgres-provisioner"})
                self.assertEqual(state["objects"]["configmap/postgres-provisioner"]["data"], {"provision.sh": "original script"})

    def test_complete_matching_jobs_are_reused(self):
        result, state = self.invoke("complete-reuse")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse(any(x.startswith("create:job/") or x.startswith("delete:job/") for x in state["mutations"]))

    def test_concurrent_configmap_change_is_not_overwritten(self):
        result, state = self.invoke("concurrent-configmap")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("preparation-rollback-unverified", result.stderr)
        self.assertEqual(state["objects"]["configmap/postgres-provisioner"]["data"], {"provision.sh": "concurrent change"})

    def test_replaced_job_uid_is_not_deleted(self):
        result, state = self.invoke("wrong-job-uid")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("preparation-rollback-unverified", result.stderr)
        self.assertIn("job/tv-sync-provisioner", state["objects"])
        self.assertNotIn("delete:job/tv-sync-provisioner", state["mutations"])

    def test_foreign_replacement_is_never_deleted(self):
        result, state = self.invoke("foreign-replacement")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("preparation-rollback-unverified", result.stderr)
        self.assertIn("job/tv-sync-provisioner", state["objects"])
        self.assertNotIn("delete:job/tv-sync-provisioner", state["mutations"])


if __name__ == "__main__":
    unittest.main()
