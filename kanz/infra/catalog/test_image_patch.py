"""Fixture overlay integrity, identity, cleanup and complete advisory refusal."""
import copy
import hashlib
import importlib.util
import json
from pathlib import Path
import tempfile
import unittest
from unittest import mock

import image_patch
import qualify
from test_qualify import IDENTITY, OWNER, OWNER_PATH

spec = importlib.util.spec_from_file_location("verify_patch", image_patch.HERE / "verify_patch.py")
verify_patch = importlib.util.module_from_spec(spec)
spec.loader.exec_module(verify_patch)


class ImagePatchControls(unittest.TestCase):
    def test_exact_package_bytes_are_required_before_offline_install(self):
        body = b"reviewed-package"
        selected = {"bytes": len(body), "sha256": hashlib.sha256(body).hexdigest()}
        image_patch.validate_package(body, selected)
        for changed in (body[:-1], body + b"x", b"X" + body[1:]):
            with self.subTest(changed=changed), self.assertRaises(ValueError):
                image_patch.validate_package(changed, selected)
        selected = image_patch.lock()
        self.assertEqual(selected["base"], json.loads((qualify.HERE / "images.json").read_text())["images"]["ingestion"])
        self.assertEqual((selected["package"], selected["before"], selected["after"], selected["architecture"]), ("linux-libc-dev", "6.8.0-147.147", "6.8.0-149.149", "amd64"))
        dockerfile = (image_patch.HERE / "Dockerfile").read_text()
        self.assertIn("FROM " + selected["base"], dockerfile)
        self.assertIn("dpkg -i /tmp/kanz-catalog-patch/linux-libc-dev.deb", dockerfile)
        self.assertNotIn("apt-get", dockerfile)

    def test_only_fixed_package_may_change_with_identical_cli_python_graph(self):
        selection = image_patch.lock()
        package = "linux-libc-dev:amd64"
        before = {"packages": {package: selection["before"], "libc6:amd64": "unchanged"}, "python": {"acryl-datahub": "1.8.0", "requests": "unchanged"}, "pythonVersion": "3.12.3"}
        after = copy.deepcopy(before)
        after["packages"][package] = selection["after"]
        proof = verify_patch.compare(before, after, selection)
        dependencies = {"python": after["pythonVersion"], "packages": after["python"]}
        image_patch.validate_proof(proof, dependencies)
        for field, key, value in (("packages", package, selection["before"]), ("packages", "libc6:amd64", "changed"), ("python", "requests", "changed"), ("python", "acryl-datahub", "1.9.0")):
            changed = copy.deepcopy(after)
            changed[field][key] = value
            with self.subTest(field=field, key=key), self.assertRaises(ValueError):
                verify_patch.compare(before, changed, selection)
        with self.assertRaises(ValueError):
            image_patch.validate_proof(proof, dict(dependencies, python="3.13"))
        with self.assertRaises(ValueError):
            image_patch.validate_proof(proof, dict(dependencies, packages={"acryl-datahub": "1.8.0"}))

    def test_derived_image_must_preserve_exact_base_behavior_layers_and_owner(self):
        record = OWNER["derivedImage"]
        base = {"Id": "sha256:" + "1" * 64, "Config": {"User": "datahub", "Entrypoint": ["datahub"], "Env": ["PUBLIC=setting"]}, "RootFS": {"Layers": ["original-layer"]}}
        derived = {"Id": "sha256:" + "2" * 64, "Os": "linux", "Architecture": "amd64", "RepoTags": [], "Config": dict(base["Config"], Labels=record["labels"]), "RootFS": {"Layers": ["original-layer", "fixed-package-layer"]}}
        proof = image_patch.validate_derived(base, derived, record)
        self.assertNotEqual(proof["baseImageID"], proof["imageID"])
        for field, value in (("Id", base["Id"]), ("RepoTags", ["foreign:tag"]), ("Architecture", "arm64"), ("RootFS", {"Layers": ["other-base", "fixed"]}), ("Config", dict(derived["Config"], User="root")), ("Config", dict(derived["Config"], Labels={}))):
            changed = dict(derived, **{field: value})
            with self.subTest(field=field), self.assertRaises(ValueError):
                image_patch.validate_derived(base, changed, record)
        for key, value in (("context", "/foreign/context"), ("packageSHA256", "f" * 64), ("baseReference", "foreign:tag")):
            owner = copy.deepcopy(OWNER)
            owner["derivedImage"][key] = value
            with self.subTest(key=key), self.assertRaises(ValueError):
                qualify.validate_owner(owner, IDENTITY, OWNER_PATH)

    def test_partial_build_cleanup_uses_only_exact_owned_untagged_image_id(self):
        image_id = "sha256:" + "2" * 64
        for variant in ("owned", "foreign-label", "foreign-tag", "registry-base", "ambiguous"):
            existing, removed = True, []
            info = {"Id": image_id, "RepoTags": ["foreign:tag"] if variant == "foreign-tag" else [], "RepoDigests": [image_patch.lock()["base"]] if variant == "registry-base" else [], "Config": {"Labels": {} if variant == "foreign-label" else OWNER["derivedImage"]["labels"]}}
            def execute(args):
                nonlocal existing
                if args[:3] == ["docker", "image", "ls"]:
                    return (image_id + ("\nsha256:" + "3" * 64 if variant == "ambiguous" else "")) if existing else ""
                if args[:3] == ["docker", "image", "inspect"]:
                    return json.dumps([info])
                if args[:4] == ["docker", "image", "rm", "--no-prune"]:
                    self.assertEqual(args, ["docker", "image", "rm", "--no-prune", image_id])
                    removed.append(args[-1])
                    existing = False
                    return ""
                raise AssertionError("cleanup may only inspect/remove the exact derived ID")
            if variant == "owned":
                image_patch.cleanup(OWNER["derivedImage"], execute)
                self.assertEqual(removed, [image_id])
            else:
                with self.subTest(variant=variant), self.assertRaises(ValueError):
                    image_patch.cleanup(OWNER["derivedImage"], execute)
                self.assertEqual(removed, [])

    def test_six_role_collection_cannot_accept_advisory_or_missing_scan(self):
        roles = tuple(json.loads((qualify.HERE / "images.json").read_text())["images"])
        completed = {role: {"scanVerdict": "passed", "scanSHA256": "a" * 64} for role in roles}
        self.assertEqual(image_patch.scan_verdicts(completed, roles), [])
        completed["ingestion"]["scanVerdict"] = "advisory"
        completed["gms"]["scanVerdict"] = "advisory"
        self.assertEqual(set(image_patch.scan_verdicts(completed, roles)), {"ingestion", "gms"})
        for changed in ({key: value for key, value in completed.items() if key != "broker"}, dict(completed, broker={"scanVerdict": "passed"})):
            with self.assertRaises(ValueError):
                image_patch.scan_verdicts(changed, roles)

    def test_structured_scan_must_bind_exact_image_and_fixed_critical_policy(self):
        reference = "sha256:" + "2" * 64
        report = {"SchemaVersion": 2, "ArtifactType": "container_image", "ArtifactName": reference, "Metadata": {"ImageID": reference}, "Results": []}
        self.assertFalse(image_patch.validate_scan(report, reference, reference))
        report["Results"] = [{"Vulnerabilities": [{"Severity": "CRITICAL", "FixedVersion": "fixed", "VulnerabilityID": "CVE-fixture", "PkgName": "fixture"}]}]
        self.assertTrue(image_patch.validate_scan(report, reference, reference))
        for key, value in (("ArtifactName", "foreign:tag"), ("Metadata", {"ImageID": "sha256:" + "3" * 64}), ("SchemaVersion", 1), ("Results", [{"Vulnerabilities": [{"Severity": "HIGH"}]}])):
            with self.subTest(key=key), self.assertRaises(ValueError):
                image_patch.validate_scan(dict(report, **{key: value}), reference, reference)

    def test_offline_build_selects_actual_derived_id_without_a_tag_or_network(self):
        with tempfile.TemporaryDirectory() as directory:
            fixture = object.__new__(qualify.Fixture)
            fixture.owner_path = Path(directory) / "catalog-owner.json"
            fixture.owner = copy.deepcopy(OWNER)
            fixture.owner["derivedImage"] = image_patch.owned_spec(fixture.owner, fixture.owner_path)
            fixture.source_images = {"ingestion": image_patch.lock()["base"]}
            fixture.images, fixture.environment = dict(fixture.source_images), {}
            base = {"Id": "sha256:" + "1" * 64, "Config": {"User": "datahub", "Entrypoint": ["datahub"]}, "RootFS": {"Layers": ["original"]}}
            image_id = "sha256:" + "2" * 64
            derived = {"Id": image_id, "Os": "linux", "Architecture": "amd64", "RepoTags": [], "Config": dict(base["Config"], Labels=fixture.owner["derivedImage"]["labels"]), "RootFS": {"Layers": ["original", "fixed"]}}
            commands = []
            def execute(args, **kwargs):
                commands.append(args)
                if args[:2] == ["docker", "version"]:
                    return json.dumps({"client": "29.0.0", "server": "29.0.0", "clientAPI": "1.52", "serverAPI": "1.52"})
                if args[:2] == ["docker", "build"]:
                    self.assertIn("--network=none", args)
                    self.assertIn("--pull=false", args)
                    self.assertNotIn("--tag", args)
                    self.assertNotIn("-t", args)
                    self.assertEqual(kwargs["environment"]["DOCKER_BUILDKIT"], "0")
                    Path(args[args.index("--iidfile") + 1]).write_text(image_id)
                    return ""
                if args[:3] == ["docker", "image", "inspect"]:
                    self.assertEqual(args[-1], image_id)
                    return json.dumps([derived])
                raise AssertionError("unexpected build operation")
            fixture.run = execute
            with mock.patch.object(image_patch, "download", return_value=b"package"):
                evidence = fixture.prepare_ingestion(base)
            self.assertEqual(evidence["reference"], image_id)
            self.assertEqual(fixture.images["ingestion"], image_id)
            self.assertEqual(fixture.source_images["ingestion"], image_patch.lock()["base"])
            self.assertEqual(len(commands), 3)
            self.assertEqual(evidence["build"]["mode"], "docker-legacy")
            for changed in ({}, dict(evidence["build"], network="default"), dict(evidence["build"], docker={}), dict(evidence["build"], memoryBytes=0)):
                with self.assertRaises(ValueError):
                    image_patch.validate_build(changed)

    def test_real_scan_loop_collects_all_advisories_then_refuses_before_startup(self):
        with tempfile.TemporaryDirectory() as directory:
            fixture = object.__new__(qualify.Fixture)
            fixture.scratch = Path(directory)
            fixture.owner_path = fixture.scratch / "catalog-owner.json"
            fixture.owner = copy.deepcopy(OWNER)
            path = fixture.scratch / "scanner-state"
            path.mkdir()
            fixture.owner["scannerMount"]["path"] = str(path)
            fixture.environment = {}
            fixture.images = json.loads((qualify.HERE / "images.json").read_text())["images"]
            fixture.source_images = dict(fixture.images)
            identities = {role: {"imageID": "sha256:" + str(index) * 64, "reference": reference} for index, (role, reference) in enumerate(fixture.images.items(), 1)}
            for folder, filename, version in (("db", "trivy.db", 2), ("java-db", "trivy-java.db", 1)):
                root = path / "cache" / folder
                root.mkdir(parents=True)
                (root / filename).write_bytes(b"bounded fixture database")
                (root / "metadata.json").write_text(json.dumps({"Version": version}))
            scanned = []
            def execute(args, **_kwargs):
                if args == ["trivy", "--version"]:
                    return "Version: 0.72.0\n"
                if "--download-db-only" in args:
                    return ""
                self.assertEqual(args[:2], ["trivy", "image"])
                role = next(role for role, reference in fixture.images.items() if reference == args[-1])
                scanned.append(role)
                findings = [{"Severity": "CRITICAL", "FixedVersion": "fixed", "VulnerabilityID": "CVE-fixture", "PkgName": "fixture"}] if role in {"gms", "broker"} else []
                scan = {"SchemaVersion": 2, "ArtifactType": "container_image", "ArtifactName": args[-1], "Metadata": {"ImageID": identities[role]["imageID"]}, "Results": [{"Vulnerabilities": findings}]}
                Path(args[args.index("--output") + 1]).write_text(json.dumps(scan))
                if findings:
                    raise qualify.CommandFailure(fixture.stage, "scanner-command-exit", 1)
                return ""
            fixture.run = execute
            fixture.prepare_scanner = mock.Mock(return_value=path)
            with mock.patch.object(qualify, "cleanup_scanner") as release, mock.patch.object(qualify, "mount_record", return_value=None), self.assertRaises(qualify.CommandFailure) as refused:
                fixture.scan_images(identities)
            self.assertEqual(refused.exception.category, "scanner-advisory")
            self.assertEqual(scanned, list(fixture.images))
            self.assertEqual(set(fixture.scanner_evidence["advisoryRoles"]), {"gms", "broker"})
            self.assertEqual(len(list(fixture.scratch.glob("scan-*.json"))), 6)
            self.assertTrue(fixture.scanner_evidence["releasedBeforeRuntime"])
            release.assert_called_once()


if __name__ == "__main__":
    unittest.main()
