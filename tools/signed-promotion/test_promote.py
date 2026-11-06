import hashlib
import importlib.machinery
import importlib.util
import json
from pathlib import Path
import subprocess
import unittest
from unittest import mock

import promote as p


def h(raw):
    return "sha256:" + hashlib.sha256(raw).hexdigest()


def encoded(value):
    return json.dumps(value, separators=(",", ":")).encode()


class GraphTests(unittest.TestCase):
    def setUp(self):
        self.config = b'{"architecture":"amd64","private-label":"not-artifact"}'
        self.layer = b"fixture layer"
        self.desc = lambda raw, media: {"digest": h(raw), "size": len(raw), "mediaType": media}
        self.manifest = encoded({"schemaVersion": 2, "mediaType": "application/vnd.oci.image.manifest.v1+json",
            "config": self.desc(self.config, "application/vnd.oci.image.config.v1+json"),
            "layers": [self.desc(self.layer, "application/vnd.oci.image.layer.v1.tar")]})
        self.index = encoded({"schemaVersion": 2, "mediaType": "application/vnd.oci.image.index.v1+json",
            "manifests": [self.desc(self.manifest, "application/vnd.oci.image.manifest.v1+json")]})
        self.artifact = encoded({"schemaVersion": 2, "mediaType": "application/vnd.oci.image.manifest.v1+json",
            "config": self.desc(b"{}", "application/vnd.oci.empty.v1+json"), "layers": [],
            "subject": self.desc(self.index, "application/vnd.oci.image.index.v1+json")})
        self.raw = {h(x): x for x in [self.config, self.layer, self.manifest, self.index, self.artifact, b"{}"]}
        self.legacy = h(self.index).replace(":", "-") + ".sig"

    def fake(self, *args, **kwargs):
        if args[:3] == ("oras", "repo", "tags"):
            return (self.legacy + "\nv1\n").encode()
        if args[:2] == ("oras", "resolve"):
            return h(self.artifact).encode()
        identity = args[-1].split("@")[-1]
        if args[:3] == ("oras", "manifest", "fetch"):
            return self.raw[identity]
        if args[:3] == ("oras", "blob", "fetch"):
            raw = self.raw[identity]
            return h(raw), len(raw)
        if args[:2] == ("oras", "discover"):
            raw = self.raw[identity]
            body = json.loads(raw)
            result = self.desc(raw, body["mediaType"])
            result["referrers"] = [self.desc(self.artifact, "application/vnd.oci.image.manifest.v1+json")] if identity == h(self.index) else []
            return encoded(result)
        self.fail("unexpected command")

    def test_complete_multi_platform_and_legacy_graph(self):
        with mock.patch.object(p, "command", side_effect=self.fake):
            result = p.graph("ghcr.io/eighred/tv-sync", h(self.index))
        self.assertEqual(set(result["descriptors"]), set(self.raw))
        self.assertEqual(result["legacy"], {self.legacy: h(self.artifact)})
        self.assertNotIn("private-label", json.dumps(result))

    def test_manifest_tamper(self):
        self.raw[h(self.index)] += b" "
        with mock.patch.object(p, "command", side_effect=self.fake), self.assertRaises(p.Refused):
            p.graph("repo", h(self.index))

    def test_destination_legacy_lookup_does_not_require_list_images(self):
        with mock.patch.object(p, "command", side_effect=self.fake) as run:
            result = p.graph("destination", h(self.index),
                             legacy_lookup=lambda tag: h(self.artifact) if tag == self.legacy else None)
        self.assertEqual(result["legacy"], {self.legacy: h(self.artifact)})
        self.assertFalse(any(call.args[:3] == ("oras", "repo", "tags") for call in run.call_args_list))

    def test_blob_tamper(self):
        self.raw[h(self.layer)] = b"bad"
        with mock.patch.object(p, "command", side_effect=self.fake), self.assertRaises(p.Refused):
            p.graph("repo", h(self.index))

    def test_descriptor_refuses_remote_urls_and_embedded_data(self):
        for extra in ({"urls": ["https://example.invalid/blob"]}, {"data": "eA=="}, {"size": True}, {"size": p.MAX_BLOB + 1}):
            with self.subTest(extra=extra), self.assertRaises(p.Refused):
                p.descriptor(self.desc(self.layer, "layer") | extra)

    def test_count_and_total_bounds(self):
        for key, value in (("MAX_NODES", 1), ("MAX_GRAPH", 1)):
            with self.subTest(key=key), mock.patch.object(p, key, value), mock.patch.object(p, "command", side_effect=self.fake), self.assertRaises(p.Refused):
                p.graph("repo", h(self.index))

    def test_unsupported_graph_shape_refused(self):
        bad = encoded({"schemaVersion": 2, "mediaType": "application/vnd.oci.image.manifest.v1+json", "blobs": []})
        self.raw[h(bad)] = bad
        with mock.patch.object(p, "command", side_effect=self.fake), self.assertRaises(p.Refused):
            p.graph("repo", h(bad))

    def test_referrer_root_substitution_refused(self):
        original = self.fake
        def fake(*args, **kwargs):
            result = original(*args, **kwargs)
            if args[:2] == ("oras", "discover"):
                body = json.loads(result)
                body["size"] += 1
                return encoded(body)
            return result
        with mock.patch.object(p, "command", side_effect=fake), self.assertRaises(p.Refused):
            p.graph("repo", h(self.index))


class PairTests(unittest.TestCase):
    def setUp(self):
        self.identity = "sha256:" + "a" * 64
        self.images = dict.fromkeys(p.SERVICES, self.identity)
        self.calls = []

    def command(self, *args, **kwargs):
        self.calls.append(args)
        if args[:3] == ("aws", "sts", "get-caller-identity"):
            return encoded({"Account": "012619468098"})
        if "rev-parse" in args:
            return b"b" * 40
        return b""

    def invoke(self):
        return p.promote("012619468098", "v1.2.3", "b" * 40, self.images)

    def patches(self):
        return mock.patch.multiple(p, command=mock.DEFAULT, graph=mock.DEFAULT,
                                   verify_release=mock.DEFAULT, resolve=mock.DEFAULT, target_available=mock.DEFAULT)

    def test_both_sources_preflight_before_any_copy(self):
        with self.patches() as mocks:
            mocks["command"].side_effect = self.command
            mocks["resolve"].return_value = self.identity
            mocks["graph"].return_value = {"root": self.identity, "legacy": {}}
            mocks["verify_release"].side_effect = lambda *args: self.calls.append(("verify", args[0]))
            result = self.invoke()
        first = next(i for i, x in enumerate(self.calls) if x[:2] == ("oras", "cp"))
        self.assertEqual(sum(x[0] == "verify" for x in self.calls[:first]), 2)
        self.assertEqual(result["status"], "verified-pair")
        self.assertEqual(result["destination_tag"], "release-v1.2.3")
        copies = [x for x in self.calls if x[:2] == ("oras", "cp")]
        self.assertTrue(all(x[-1].endswith(":release-v1.2.3") for x in copies))
        self.assertTrue(all("--platform" not in x for x in copies))

    def test_second_source_failure_has_no_writes(self):
        with self.patches() as mocks:
            mocks["command"].side_effect = self.command
            mocks["resolve"].return_value = self.identity
            mocks["graph"].return_value = {"root": self.identity, "legacy": {}}
            mocks["verify_release"].side_effect = [None, p.Refused("signature missing")]
            with self.assertRaises(p.Refused):
                self.invoke()
        self.assertFalse(any(x[:2] == ("oras", "cp") for x in self.calls))

    def test_destination_or_racing_source_omission_refused(self):
        for mismatch in (2, 3):
            with self.subTest(mismatch=mismatch), self.patches() as mocks:
                mocks["command"].side_effect = self.command
                mocks["resolve"].return_value = self.identity
                graphs = [{"root": self.identity, "legacy": {}} for _ in range(6)]
                graphs[mismatch] = {"root": self.identity, "legacy": {}, "omitted": True}
                mocks["graph"].side_effect = graphs
                with self.assertRaises(p.Refused):
                    self.invoke()

    def test_exact_signature_and_both_predicates(self):
        with mock.patch.object(p, "command") as run:
            p.verify_release("registry/repo@" + self.identity, "v1", "b" * 40)
        calls = [c.args for c in run.call_args_list]
        self.assertEqual([c[1] for c in calls], ["verify", "verify-attestation", "verify-attestation"])
        for call in calls:
            self.assertIn("https://github.com/eighred/kanz/.github/workflows/release.yml@refs/tags/v1", call)
            self.assertIn("--certificate-github-workflow-sha", call)
            self.assertNotIn("--check-claims=false", call)
        self.assertIn("spdxjson", calls[1])
        self.assertIn("vuln", calls[2])

    def test_copy_failure_cannot_return_acceptance(self):
        base = self.command
        def command(*args, **kwargs):
            if args[:2] == ("oras", "cp") and "kanz-migrate" in args[-1]:
                raise p.Refused("copy failed")
            return base(*args, **kwargs)
        with self.patches() as mocks:
            mocks["command"].side_effect = command
            mocks["resolve"].return_value = self.identity
            mocks["graph"].return_value = {"root": self.identity, "legacy": {}}
            with self.assertRaises(p.Refused):
                self.invoke()

    def test_cleanup_failure_prevents_evidence(self):
        # Even an otherwise successful copy must not create acceptance if the
        # controlled temporary configuration cannot be disposed of.
        from tempfile import TemporaryDirectory
        with TemporaryDirectory() as directory:
            path = Path(directory, "evidence.json")
            env = {"GITHUB_REPOSITORY": "eighred/kanz", "GITHUB_REF": "refs/heads/main",
                   "GITHUB_EVENT_NAME": "workflow_dispatch", "PATH": "fixture"}
            argv = ["promote.py", "--account", "012619468098", "--release-tag", "v1.2.3",
                    "--release-sha", "b" * 40, "--tv-sync", self.identity,
                    "--kanz-migrate", self.identity, "--evidence", str(path)]
            with mock.patch.dict(p.os.environ, env, clear=True), mock.patch("sys.argv", argv), \
                 mock.patch.object(p, "promote", return_value={"status": "verified-pair"}), \
                 mock.patch.object(p, "command", return_value=b"b" * 40), \
                 mock.patch.object(p.tempfile, "TemporaryDirectory") as temp:
                temp.return_value.__enter__.return_value = directory
                temp.return_value.__exit__.side_effect = OSError("cleanup failed")
                with self.assertRaises(OSError):
                    p.main()
            self.assertFalse(path.exists())

    def test_destination_conflict_or_unavailable_closed(self):
        for result in ({}, {"images": [{"imageId": {"imageDigest": "sha256:" + "c" * 64}}]},
                       {"failures": [{"failureCode": "AccessDenied"}]}):
            with mock.patch.object(p, "document", return_value=result), self.assertRaises(p.Refused):
                p.target_available("012619468098", "tv-sync", "v1", self.identity)

    def test_destination_lookup_is_exact_and_existing_digest_reusable(self):
        image = {"registryId": "012619468098", "repositoryName": "tv-sync",
                 "imageId": {"imageTag": "release-v1.2.3", "imageDigest": self.identity}}
        with mock.patch.object(p, "document", return_value={"images": [image]}):
            p.target_available("012619468098", "tv-sync", "release-v1.2.3", self.identity)
        for field, value in (("registryId", "999999999999"), ("repositoryName", "other")):
            with mock.patch.object(p, "document", return_value={"images": [image | {field: value}]}), self.assertRaises(p.Refused):
                p.target_available("012619468098", "tv-sync", "release-v1.2.3", self.identity)

    def test_invalid_release_inputs_refused_before_tools(self):
        for account, tag, sha in [("evil", "v1", "b" * 40), ("012619468098", "v1;echo", "b" * 40), ("012619468098", "v1", "main")]:
            with mock.patch.object(p, "command") as run, self.assertRaises(p.Refused):
                p.promote(account, tag, sha, self.images)
            run.assert_not_called()


class CredentialTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        path = str(Path(__file__).with_name("docker-credential-kanz-promotion"))
        loader = importlib.machinery.SourceFileLoader("credential", path)
        spec = importlib.util.spec_from_loader(loader.name, loader)
        cls.module = importlib.util.module_from_spec(spec)
        loader.exec_module(cls.module)

    def test_helper_exact_registry_and_get_only(self):
        env = {"PROMOTION_AWS_ACCOUNT": "012619468098", "GH_TOKEN": "dummy"}
        self.assertEqual(self.module.credential("get", "ghcr.io", env)["Secret"], "dummy")
        for action, host in [("store", "ghcr.io"), ("get", "ghcr.io.evil.invalid"), ("get", "999999999999.dkr.ecr.ap-northeast-1.amazonaws.com")]:
            with self.assertRaises(ValueError):
                self.module.credential(action, host, env)

    def test_ecr_token_uses_pipe_not_argv(self):
        env = {"PROMOTION_AWS_ACCOUNT": "012619468098"}
        with mock.patch.object(self.module.subprocess, "run", return_value=subprocess.CompletedProcess([], 0, b"dummy-token\n", b"")) as run:
            result = self.module.credential("get", "012619468098.dkr.ecr.ap-northeast-1.amazonaws.com", env)
        self.assertEqual(result["Secret"], "dummy-token")
        self.assertNotIn("dummy-token", repr(run.call_args))


class FixtureCleanupTests(unittest.TestCase):
    def test_proxy_initialization_failure_stops_registry(self):
        import cli_fixture as fixture
        backend = mock.Mock()
        backend.poll.side_effect = [None, 0]
        required = p.require
        def require(condition, message):
            if message != "Linux CLI fixture required":
                required(condition, message)
        with mock.patch.object(p, "require", side_effect=require), \
             mock.patch.object(fixture.shutil, "which", return_value="fixture-registry"), \
             mock.patch.object(p, "command", return_value=b"oras1.3.0 registry3.0.0"), \
             mock.patch.object(fixture.subprocess, "Popen", return_value=backend), \
             mock.patch.object(fixture, "ThreadingHTTPServer", side_effect=RuntimeError("bind failed")):
            with self.assertRaises(RuntimeError):
                fixture.fixture()
        backend.terminate.assert_called_once()
        backend.wait.assert_called_once_with(timeout=10)


if __name__ == "__main__":
    unittest.main()
