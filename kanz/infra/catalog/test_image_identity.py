"""Discriminate raw OCI identity links, Docker stores and scanner boundaries."""
import copy
import hashlib
import json
import traceback
import unittest
from unittest import mock

import image_identity as identity
import image_patch
from test_image_patch import base_info
from test_qualify import OWNER


def fixture(config_changes=None, descriptor_changes=None):
    layers = ["sha256:" + "1" * 64, "sha256:" + "2" * 64]
    config = {"os": "linux", "architecture": "amd64", "rootfs": {"type": "layers", "diff_ids": layers}, "config": {"Env": ["PRIVATE=raw-config-marker"]}}
    config.update(config_changes or {})
    config_body = json.dumps(config).encode()
    config_digest = "sha256:" + hashlib.sha256(config_body).hexdigest()
    descriptor = {"mediaType": "application/vnd.oci.image.config.v1+json", "digest": config_digest, "size": len(config_body)}
    descriptor.update(descriptor_changes or {})
    manifest = {"schemaVersion": 2, "mediaType": "application/vnd.oci.image.manifest.v1+json", "config": descriptor, "layers": [{"digest": value, "size": 1, "mediaType": "application/vnd.oci.image.layer.v1.tar+gzip"} for value in layers]}
    manifest_body = json.dumps(manifest).encode()
    manifest_digest = "sha256:" + hashlib.sha256(manifest_body).hexdigest()
    selected = {"reference": "acryldata/datahub-ingestion:v1.8.0-locked@" + manifest_digest, "repository": "acryldata/datahub-ingestion", "manifestDigest": manifest_digest, "manifestBytes": len(manifest_body), "manifestMediaType": manifest["mediaType"], "configDigest": config_digest, "configBytes": len(config_body), "configMediaType": "application/vnd.oci.image.config.v1+json", "os": "linux", "architecture": "amd64", "rootFSType": "layers", "rootFSDiffIDs": layers}
    selected.update(publisherReference=selected["reference"], executionRepository="docker.io/" + selected["repository"])
    return selected, manifest_body, config_body


def engine(selected, kind="platform-manifest"):
    return {"Id": selected["manifestDigest" if kind == "platform-manifest" else "configDigest"], "Os": "linux", "Architecture": "amd64", "RepoDigests": [selected["repository"] + "@" + selected["manifestDigest"]], "RootFS": {"Type": "layers", "Layers": selected["rootFSDiffIDs"]}}


def metadata(selected, kind="platform-manifest"):
    info = engine(selected, kind)
    return {"ImageID": info["Id"], "RepoDigests": info["RepoDigests"], "DiffIDs": selected["rootFSDiffIDs"], "ImageConfig": {"os": "linux", "architecture": "amd64", "rootfs": {"type": "layers", "diff_ids": selected["rootFSDiffIDs"]}}}


class ImageIdentityControls(unittest.TestCase):
    def test_raw_manifest_to_config_bytes_platform_and_order_are_mandatory(self):
        selected, manifest, config = fixture()
        projected = identity.verify_raw(manifest, config, selected)
        self.assertTrue(projected["rawManifestVerified"] and projected["rawConfigVerified"])
        self.assertNotIn("raw-config-marker", json.dumps(projected))
        for manifest_body, config_body in ((manifest + b"x", config), (manifest, config + b"x")):
            with self.assertRaises(ValueError):
                identity.verify_raw(manifest_body, config_body, selected)
        for config_changes, descriptor_changes in (({"architecture": "arm64"}, None), ({"rootfs": {"type": "layers", "diff_ids": list(reversed(selected["rootFSDiffIDs"]))}}, None), (None, {"digest": "sha256:" + "f" * 64}), (None, {"size": len(config) + 1})):
            changed, manifest_body, config_body = fixture(config_changes, descriptor_changes)
            with self.assertRaises(ValueError):
                identity.verify_raw(manifest_body, config_body, changed)
        index = json.dumps({"schemaVersion": 2, "mediaType": selected["manifestMediaType"], "manifests": []}).encode()
        with self.assertRaises(ValueError):
            identity.verify_raw(index, config, dict(selected, manifestBytes=len(index), manifestDigest="sha256:" + hashlib.sha256(index).hexdigest()))

    def test_engine_kind_is_exact_and_canonical_repository_is_not_a_suffix(self):
        selected, _, _ = fixture()
        for kind in ("platform-manifest", "config"):
            info = engine(selected, kind)
            actual = identity.verify_engine(info, selected)
            self.assertEqual(actual["engineIDKind"], kind)
            identity.verify_projected(actual, selected)
            for alias in ("docker.io/", "index.docker.io/", "registry-1.docker.io/"):
                self.assertTrue(identity.manifest_bound([alias + info["RepoDigests"][0]], selected))
            for changed in (dict(info, Id="sha256:" + "f" * 64), dict(info, Architecture="arm64"), dict(info, RootFS={"Type": "layers", "Layers": list(reversed(selected["rootFSDiffIDs"]))}), dict(info, RepoDigests=["evil.example/" + info["RepoDigests"][0]]), dict(info, RepoDigests=["other/repo@" + selected["manifestDigest"]])):
                with self.assertRaises(ValueError):
                    identity.verify_engine(changed, selected)
        private = dict(selected, reference="ghcr.io/eighred/base/datahub-ingestion:v1.8.0-locked@" + selected["manifestDigest"], executionRepository="ghcr.io/eighred/base/datahub-ingestion")
        private_info = dict(engine(selected), RepoDigests=[private["executionRepository"] + "@" + selected["manifestDigest"]])
        self.assertEqual(identity.verify_engine(private_info, private)["executionRepository"], private["executionRepository"])
        for digests in (engine(selected)["RepoDigests"], ["ghcr.io/eighred/base/other@" + selected["manifestDigest"]], ["ghcr.io/other/base/datahub-ingestion@" + selected["manifestDigest"]]):
            with self.assertRaises(ValueError):
                identity.verify_engine(dict(private_info, RepoDigests=digests), private)

    def test_docker_scan_id_must_equal_observed_engine_even_if_other_digest_known(self):
        selected, _, _ = fixture()
        actual = identity.verify_engine(engine(selected), selected)
        report = metadata(selected)
        identity.verify_scan(report, actual)
        for changed in (dict(report, ImageID=selected["configDigest"]), dict(report, DiffIDs=list(reversed(report["DiffIDs"]))), dict(report, RepoDigests=["other/repo@" + selected["manifestDigest"]]), dict(report, ImageConfig=dict(report["ImageConfig"], architecture="arm64"))):
            with self.assertRaises(ValueError):
                identity.verify_scan(changed, actual)

    def test_public_fetch_returns_no_config_or_registry_token_and_redacts_errors(self):
        selected, manifest, config = fixture()
        responses = []
        for body in (json.dumps({"token": "public-read-token-marker"}).encode(), manifest, config):
            response = mock.MagicMock()
            response.__enter__.return_value = response
            response.read.return_value = body
            responses.append(response)
        opener = mock.Mock()
        opener.open.side_effect = responses
        with mock.patch.object(identity.urllib.request, "build_opener", return_value=opener):
            projected = identity.fetch(selected)
        self.assertNotIn("marker", json.dumps(projected))
        opener.open.side_effect = OSError("public-read-token-marker")
        with mock.patch.object(identity.urllib.request, "build_opener", return_value=opener):
            try:
                identity.fetch(selected)
            except ValueError as error:
                diagnostic = str(error) + traceback.format_exc()
            else:
                self.fail("network failure must refuse")
        self.assertNotIn("public-read-token-marker", diagnostic)

    def test_all_six_pins_and_cleanup_bind_one_actual_base_not_both_digest_kinds(self):
        selected = identity.selections()
        self.assertEqual(set(selected), {"gms", "upgrade", "ingestion", "mysql", "search", "broker"})
        for item in selected.values():
            self.assertTrue(identity.DIGEST.fullmatch(item["manifestDigest"]))
            self.assertTrue(identity.DIGEST.fullmatch(item["configDigest"]))
            self.assertTrue(item["rootFSDiffIDs"])
        record = OWNER["derivedImage"]
        base = dict(base_info(record), Id=record["baseManifestDigest"])
        bound = image_patch.validate_base(base, record)
        self.assertEqual(bound["engineIDKind"], "platform-manifest")
        marker = image_patch.marker_body(record, "started", base=bound)
        calls = []
        def execute(args, **_kwargs):
            calls.append(args)
            self.assertEqual(args[:3], ["docker", "image", "inspect"])
            # Even a known config digest may not replace the one actual base
            # engine identity acknowledged in this launch marker.
            return json.dumps([base_info(record)])
        with mock.patch.object(image_patch, "read_marker", side_effect=[marker, None]), self.assertRaises(ValueError):
            image_patch.cleanup(record, execute)
        self.assertEqual(len(calls), 1)
        image_id = "sha256:" + "8" * 64
        child = {image_id: {"Id": image_id, "Parent": record["baseConfigDigest"], "Config": {"Labels": record["labels"]}}}
        with self.assertRaises(ValueError):
            image_patch.image_chain(child, record, bound["engineID"])


if __name__ == "__main__":
    unittest.main()
