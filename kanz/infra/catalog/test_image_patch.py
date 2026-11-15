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
import image_identity
import qualify
from test_qualify import IDENTITY, OWNER, OWNER_PATH

spec = importlib.util.spec_from_file_location("verify_patch", image_patch.HERE / "verify_patch.py")
verify_patch = importlib.util.module_from_spec(spec)
spec.loader.exec_module(verify_patch)


def base_info(record):
    return {"Id": record["baseConfigDigest"], "RepoDigests": [image_identity.selections()["ingestion"]["executionRepository"] + "@" + record["baseManifestDigest"]],
            "Os": "linux", "Architecture": "amd64",
            "Config": {"User": "datahub", "Entrypoint": ["datahub"], "Env": ["PUBLIC=setting"]},
            "RootFS": {"Type": "layers", "Layers": image_identity.selections()["ingestion"]["rootFSDiffIDs"]}}


def marker_body(record, phase, image_id=None, chain=None):
    return image_patch.marker_body(record, phase, image_id, chain, base=image_patch.validate_base(base_info(record), record))


def write_marker(record, phase, image_id=None, chain=None):
    return image_patch.write_marker(record, phase, image_id, chain, base=image_patch.validate_base(base_info(record), record))


def scan_identity(image_id):
    return {"engineID": image_id, "engineIDKind": "owned-build", "os": "linux", "architecture": "amd64", "rootFSType": "layers", "rootFSDiffIDs": ["sha256:" + "9" * 64]}


def scan_metadata(identity):
    return {"ImageID": identity["engineID"], "DiffIDs": identity["rootFSDiffIDs"], "ImageConfig": {"os": identity["os"], "architecture": identity["architecture"], "rootfs": {"type": identity["rootFSType"], "diff_ids": identity["rootFSDiffIDs"]}}}


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
        observation = {}
        selection = dict(selected, bytes=len(body), sha256=hashlib.sha256(body).hexdigest())
        response = mock.MagicMock(status=200)
        response.__enter__.return_value = response
        response.read.return_value = body
        opener = mock.Mock()
        opener.open.return_value = response
        with mock.patch.object(image_patch.urllib.request, "build_opener", return_value=opener):
            self.assertEqual(image_patch.download(selection, evidence=observation), body)
        self.assertEqual(observation, {"category": "passed", "httpStatus": 200, "receivedBytes": len(body), "receivedSHA256": selection["sha256"]})
        for failure, category in ((image_patch.urllib.error.HTTPError(selection["url"], 302, "secret-marker", {}, None), "package-redirect-refused"), (image_patch.urllib.error.URLError("secret-marker"), "package-network-refused"), (TimeoutError("secret-marker"), "package-timeout")):
            opener.open.side_effect = failure
            with self.subTest(category=category), mock.patch.object(image_patch.urllib.request, "build_opener", return_value=opener), self.assertRaises(ValueError) as refused:
                image_patch.download(selection, evidence=observation)
            self.assertEqual(observation["category"], category)
            self.assertNotIn("secret-marker", str(refused.exception))
            self.assertIsNone(observation["receivedBytes"])
        opener.open.side_effect = None
        response.read.return_value = body + b"x"
        with mock.patch.object(image_patch.urllib.request, "build_opener", return_value=opener), self.assertRaises(ValueError):
            image_patch.download(selection, evidence=observation)
        self.assertEqual(observation["category"], "package-integrity-refused")
        self.assertEqual(observation["receivedBytes"], len(body) + 1)
        self.assertEqual(observation["receivedSHA256"], hashlib.sha256(body + b"x").hexdigest())

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
        base = base_info(record)
        self.assertEqual(image_patch.observe_base(base, record), {"imageID": record["baseConfigDigest"], "engineIDKind": "config", "onBuildCount": 0, "onBuildEmpty": True, "repoDigestCount": 1, "selectedManifestDigestMatches": True})
        derived = {"Id": "sha256:" + "2" * 64, "Os": "linux", "Architecture": "amd64", "RepoTags": [], "Config": dict(base["Config"], Labels=record["labels"]), "RootFS": {"Type": "layers", "Layers": base["RootFS"]["Layers"] + ["sha256:" + "8" * 64]}}
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
        parent, image_id, container_id = "sha256:" + "2" * 64, "sha256:" + "3" * 64, "4" * 64
        for variant in ("owned", "foreign-label", "foreign-tag", "registry-base", "branch", "missing-parent", "cycle", "foreign-container"):
            record = OWNER["derivedImage"]
            removed = []
            state = {parent: {"Id": parent, "Parent": record["baseConfigDigest"], "Config": {"Labels": record["labels"]}}, image_id: {"Id": image_id, "Parent": parent, "Config": {"Labels": record["labels"]}}}
            containers = {container_id: {"Id": container_id, "Image": parent, "Config": {"Labels": record["labels"]}}}
            if variant == "foreign-label":
                state[parent]["Config"] = {"Labels": {}}
            elif variant == "foreign-tag":
                state[parent]["RepoTags"] = ["foreign:tag"]
            elif variant == "registry-base":
                state[parent]["RepoDigests"] = [record["baseReference"]]
            elif variant == "branch":
                state[image_id]["Parent"] = record["baseConfigDigest"]
            elif variant == "missing-parent":
                state[parent]["Parent"] = ""
            elif variant == "cycle":
                state[parent]["Parent"] = image_id
            elif variant == "foreign-container":
                containers[container_id]["Config"] = {"Labels": {}}
            def execute(args, **_kwargs):
                if args[:3] == ["docker", "image", "ls"]:
                    return "\n".join(state)
                if args[:3] == ["docker", "ps", "--all"]:
                    return "\n".join(containers)
                if args[:3] == ["docker", "image", "inspect"]:
                    return json.dumps([base_info(record) if args[-1] == record["baseConfigDigest"] else state[args[-1]]])
                if args[:3] == ["docker", "inspect", "--type"]:
                    return json.dumps([containers[args[-1]]])
                if args[:3] == ["docker", "rm", "--force"]:
                    removed.append(args[-1])
                    del containers[args[-1]]
                    return ""
                if args[:4] == ["docker", "image", "rm", "--no-prune"]:
                    self.assertNotEqual(args[-1], record["baseConfigDigest"])
                    removed.append(args[-1])
                    del state[args[-1]]
                    return ""
                raise AssertionError("cleanup may only inspect/remove exact owned IDs")
            def marker(_record, phase):
                return marker_body(record, phase, image_id if phase == "completed" else None, [image_id, parent] if phase == "completed" else None)
            with mock.patch.object(image_patch, "read_marker", side_effect=marker), mock.patch.object(image_patch.time, "sleep"):
                if variant == "owned":
                    evidence = image_patch.cleanup(record, execute)
                    self.assertEqual(removed, [container_id, image_id, parent])
                    self.assertTrue(evidence["completionAcknowledged"])
                    derived = {"imageID": image_id, "baseImageID": record["baseConfigDigest"], "baseIdentity": image_patch.validate_base(base_info(record), record), "imageChain": [image_id, parent]}
                    image_patch.validate_cleanup(evidence, derived)
                    for field, value in (("completionAcknowledged", False), ("imageID", parent), ("imageIDsRemoved", [parent]), ("buildContainerIDsRemoved", ["foreign"]), ("emptyRechecks", 0)):
                        with self.subTest(field=field), self.assertRaises(ValueError):
                            image_patch.validate_cleanup(dict(evidence, **{field: value}), derived)
                    with self.assertRaises(ValueError):
                        image_patch.validate_cleanup(evidence, dict(derived, imageChain=None))
                else:
                    with self.subTest(variant=variant), self.assertRaises(ValueError):
                        image_patch.cleanup(record, execute)
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
        identity = scan_identity(reference)
        report = {"SchemaVersion": 2, "ArtifactType": "container_image", "ArtifactName": reference, "Metadata": scan_metadata(identity), "Results": []}
        self.assertFalse(image_patch.validate_scan(report, reference, identity))
        report["Results"] = [{"Vulnerabilities": [{"Severity": "CRITICAL", "FixedVersion": "fixed", "VulnerabilityID": "CVE-fixture", "PkgName": "fixture"}]}]
        self.assertTrue(image_patch.validate_scan(report, reference, identity))
        for key, value in (("ArtifactName", "foreign:tag"), ("Metadata", {"ImageID": "sha256:" + "3" * 64}), ("SchemaVersion", 1), ("Results", [{"Vulnerabilities": [{"Severity": "HIGH"}]}])):
            with self.subTest(key=key), self.assertRaises(ValueError):
                image_patch.validate_scan(dict(report, **{key: value}), reference, identity)

    def test_offline_build_selects_actual_derived_id_without_a_tag_or_network(self):
        with tempfile.TemporaryDirectory() as directory:
            fixture = object.__new__(qualify.Fixture)
            fixture.owner_path = Path(directory) / "catalog-owner.json"
            fixture.owner = copy.deepcopy(OWNER)
            fixture.owner["derivedImage"] = image_patch.owned_spec(fixture.owner, fixture.owner_path)
            fixture.source_images = {"ingestion": image_patch.lock()["base"]}
            fixture.images, fixture.environment = dict(fixture.source_images), {}
            base = base_info(fixture.owner["derivedImage"])
            image_id = "sha256:" + "2" * 64
            derived = {"Id": image_id, "Parent": base["Id"], "Os": "linux", "Architecture": "amd64", "RepoTags": [], "Config": dict(base["Config"], Labels=fixture.owner["derivedImage"]["labels"]), "RootFS": {"Type": "layers", "Layers": base["RootFS"]["Layers"] + ["sha256:" + "8" * 64]}}
            commands = []
            def execute(args, **kwargs):
                commands.append(args)
                if args[:2] == ["docker", "version"]:
                    # Go-template exported fields differ from JSON's ApiVersion tag.
                    self.assertEqual(args, ["docker", "version", "--format", '{"client":{{json .Client.Version}},"server":{{json .Server.Version}},"clientAPI":{{json .Client.APIVersion}},"serverAPI":{{json .Server.APIVersion}}}'])
                    return json.dumps({"client": "29.0.0", "server": "29.0.0", "clientAPI": "1.52", "serverAPI": "1.52"})
                if args[:2] == ["docker", "build"]:
                    self.assertIn("--network=none", args)
                    self.assertIn("--pull=false", args)
                    self.assertNotIn("--tag", args)
                    self.assertNotIn("-t", args)
                    self.assertNotIn("--label", args)
                    rendered = (Path(args[-1]) / "Dockerfile").read_text().splitlines()
                    self.assertTrue(rendered[1].startswith("LABEL "))
                    for key, value in fixture.owner["derivedImage"]["labels"].items():
                        self.assertIn(key + "=" + json.dumps(value), rendered[1])
                    self.assertEqual(image_patch.read_marker(fixture.owner["derivedImage"], "started")["phase"], "started")
                    self.assertEqual(kwargs["environment"]["DOCKER_BUILDKIT"], "0")
                    Path(args[args.index("--iidfile") + 1]).write_text(image_id)
                    return ""
                if args[:3] == ["docker", "image", "inspect"]:
                    self.assertEqual(args[-1], image_id)
                    return json.dumps([derived])
                if args[:3] == ["docker", "image", "ls"]:
                    return image_id
                raise AssertionError("unexpected build operation")
            fixture.run = execute
            with mock.patch.object(image_patch, "download", return_value=b"package"):
                evidence = fixture.prepare_ingestion(base)
            self.assertEqual(evidence["reference"], image_id)
            self.assertEqual(fixture.images["ingestion"], image_id)
            self.assertEqual(fixture.source_images["ingestion"], image_patch.lock()["base"])
            self.assertEqual(len(commands), 5)
            self.assertEqual(image_patch.read_marker(fixture.owner["derivedImage"], "completed")["imageID"], image_id)
            self.assertEqual(evidence["derived"]["build"]["mode"], "docker-legacy")
            for changed in ({}, dict(evidence["derived"]["build"], network="default"), dict(evidence["derived"]["build"], docker={}), dict(evidence["derived"]["build"], memoryBytes=0)):
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
            for item in identities.values():
                item["identity"] = scan_identity(item["imageID"])
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
                self.assertEqual(args[args.index("--image-src") + 1], "docker")
                role = next(role for role, reference in fixture.images.items() if reference == args[-1])
                scanned.append(role)
                findings = [{"Severity": "CRITICAL", "FixedVersion": "fixed", "VulnerabilityID": "CVE-fixture", "PkgName": "fixture"}] if role in {"gms", "broker"} else []
                scan = {"SchemaVersion": 2, "ArtifactType": "container_image", "ArtifactName": args[-1], "Metadata": scan_metadata(identities[role]["identity"]), "Results": [{"Vulnerabilities": findings}]}
                Path(args[args.index("--output") + 1]).write_text(json.dumps(scan))
                if findings:
                    raise qualify.CommandFailure(fixture.stage, "scanner-advisory-exit", image_patch.ADVISORY_EXIT)
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

    def test_first_literal_owner_label_and_empty_inherited_onbuild_are_mandatory(self):
        record = OWNER["derivedImage"]
        rendered = image_patch.render_dockerfile(record["labels"])
        lines = rendered.decode().splitlines()
        self.assertEqual(lines[0], "FROM " + record["baseReference"])
        self.assertTrue(lines[1].startswith("LABEL "))
        self.assertEqual(lines[1].count("kanz.catalog."), 6)
        self.assertEqual(hashlib.sha256(rendered).hexdigest(), record["renderedDockerfileSHA256"])
        selection = image_patch.lock()
        with mock.patch.object(image_patch, "lock", return_value=selection), mock.patch.object(Path, "read_text", return_value="FROM " + selection["base"] + "\nARG UNOWNED\nLABEL __KANZ_CATALOG_OWNERSHIP__\n"), self.assertRaises(ValueError):
            image_patch.render_dockerfile(record["labels"])
        fixture = object.__new__(qualify.Fixture)
        fixture.owner, fixture.owner_path = OWNER, OWNER_PATH
        fixture.source_images = {"ingestion": record["baseReference"]}
        fixture.run = mock.Mock()
        for changed in (dict(base_info(record), Id="sha256:" + "f" * 64), dict(base_info(record), Config={"OnBuild": ["RUN unowned-trigger"]})):
            with self.assertRaises(ValueError):
                fixture.prepare_ingestion(changed)
        fixture.run.assert_not_called()

    def test_exclusive_terminal_markers_and_late_resources_cannot_clear_interruption(self):
        with tempfile.TemporaryDirectory() as directory:
            record = image_patch.owned_spec(OWNER, Path(directory) / "owner.json")
            write_marker(record, "started")
            self.assertEqual(image_patch.read_marker(record, "started"), marker_body(record, "started"))
            with self.assertRaises(FileExistsError):
                write_marker(record, "started")
            link = Path(directory) / "foreign-link"
            link.symlink_to(record["startedPath"])
            with self.assertRaises(OSError):
                image_patch.read_marker(dict(record, startedPath=str(link)), "started")
            image_id, container_id = "sha256:" + "2" * 64, "3" * 64
            write_marker(record, "completed", image_id, [image_id])
            complete = image_patch.read_marker(record, "completed")
            self.assertEqual(complete["imageID"], image_id)
            changed = dict(complete, renderedDockerfileSHA256="f" * 64)
            Path(record["completedPath"]).write_text(json.dumps(changed))
            with self.assertRaises(ValueError):
                image_patch.read_marker(record, "completed")
            images, containers, removed, snapshots = {}, {}, [], 0
            def execute(args, **_kwargs):
                nonlocal snapshots
                if args[:3] == ["docker", "image", "inspect"]:
                    return json.dumps([base_info(record) if args[-1] == record["baseConfigDigest"] else images[args[-1]]])
                if args[:3] == ["docker", "image", "ls"]:
                    snapshots += 1
                    if snapshots == 2:
                        images[image_id] = {"Id": image_id, "Parent": record["baseConfigDigest"], "Config": {"Labels": record["labels"]}}
                        containers[container_id] = {"Id": container_id, "Image": image_id, "Config": {"Labels": record["labels"]}}
                    return "\n".join(images)
                if args[:3] == ["docker", "ps", "--all"]:
                    return "\n".join(containers)
                if args[:3] == ["docker", "inspect", "--type"]:
                    return json.dumps([containers[args[-1]]])
                if args[:3] == ["docker", "rm", "--force"]:
                    removed.append(args[-1])
                    del containers[args[-1]]
                    return ""
                if args[:4] == ["docker", "image", "rm", "--no-prune"]:
                    removed.append(args[-1])
                    del images[args[-1]]
                    return ""
                raise AssertionError("unexpected interruption cleanup command")
            with mock.patch.object(image_patch, "read_marker", side_effect=lambda _record, phase: marker_body(record, phase) if phase == "started" else None), mock.patch.object(image_patch.time, "sleep"), self.assertRaisesRegex(ValueError, "completion remains unknown"):
                image_patch.cleanup(record, execute)
            self.assertEqual(removed, [container_id, image_id])
            self.assertFalse(images or containers)
            self.assertGreaterEqual(snapshots, 4)
            # The first LABEL commit can be interrupted while its fully owned
            # temporary container still references the exact untouched base.
            for owned in (True, False):
                first_step, deleted = True, []
                def first_label(args, **_kwargs):
                    nonlocal first_step
                    if args[:3] == ["docker", "image", "inspect"]:
                        self.assertEqual(args[-1], record["baseConfigDigest"])
                        return json.dumps([base_info(record)])
                    if args[:3] == ["docker", "image", "ls"]:
                        return ""
                    if args[:3] == ["docker", "ps", "--all"]:
                        return container_id if first_step else ""
                    if args[:3] == ["docker", "inspect", "--type"]:
                        return json.dumps([{"Id": container_id, "Image": record["baseConfigDigest"], "Config": {"Labels": record["labels"] if owned else {"kanz.catalog.owner": "foreign"}}}])
                    if args[:3] == ["docker", "rm", "--force"]:
                        self.assertEqual(args[-1], container_id)
                        deleted.append(args[-1])
                        first_step = False
                        return ""
                    raise AssertionError("the base image must never be deleted")
                with self.subTest(owned=owned), mock.patch.object(image_patch, "read_marker", side_effect=lambda _record, phase: marker_body(record, phase) if phase == "started" else None), mock.patch.object(image_patch.time, "sleep"), self.assertRaises(ValueError) as refused:
                    image_patch.cleanup(record, first_label)
                self.assertEqual(deleted, [container_id] if owned else [])
                self.assertEqual(first_step, not owned)
                if owned:
                    self.assertIn("completion remains unknown", str(refused.exception))

    def test_advisory_json_never_clears_an_infrastructure_exit(self):
        self.assertEqual(qualify.failure_category(b"INFO Java DB downloaded", image_patch.ADVISORY_EXIT, True), "scanner-advisory-exit")
        for category in ("scanner-db-download", "scanner-java-db-download", "scanner-network", "scanner-file-budget", "scanner-disk-budget"):
            with self.subTest(category=category), tempfile.TemporaryDirectory() as directory:
                fixture = object.__new__(qualify.Fixture)
                fixture.scratch = Path(directory)
                fixture.owner_path = fixture.scratch / "owner.json"
                path = fixture.scratch / "scanner-state"
                path.mkdir()
                fixture.owner = copy.deepcopy(OWNER)
                fixture.owner["scannerMount"]["path"] = str(path)
                fixture.environment = {}
                fixture.images = json.loads((qualify.HERE / "images.json").read_text())["images"]
                fixture.source_images = dict(fixture.images)
                identities = {role: {"imageID": "sha256:" + "1" * 64} for role in fixture.images}
                for item in identities.values():
                    item["identity"] = scan_identity(item["imageID"])
                scanned = []
                def execute(args, **_kwargs):
                    if args == ["trivy", "--version"]:
                        return "Version: 0.72.0\n"
                    if "--download-db-only" in args:
                        return ""
                    scanned.append(args[-1])
                    self.assertEqual(args[args.index("--exit-code") + 1], "42")
                    scan = {"SchemaVersion": 2, "ArtifactType": "container_image", "ArtifactName": args[-1], "Metadata": scan_metadata(scan_identity("sha256:" + "1" * 64)), "Results": [{"Vulnerabilities": [{"Severity": "CRITICAL", "FixedVersion": "fixed", "VulnerabilityID": "CVE-fixture", "PkgName": "fixture"}]}]}
                    Path(args[args.index("--output") + 1]).write_text(json.dumps(scan))
                    raise qualify.CommandFailure(fixture.stage, category, 1)
                fixture.run = execute
                fixture.prepare_scanner = mock.Mock(return_value=path)
                with mock.patch.object(qualify, "cleanup_scanner"), mock.patch.object(qualify, "mount_record", return_value=None), self.assertRaises(qualify.CommandFailure) as refused:
                    fixture.scan_images(identities)
                self.assertEqual(refused.exception.category, category)
                self.assertEqual(len(scanned), 1)


if __name__ == "__main__":
    unittest.main()
