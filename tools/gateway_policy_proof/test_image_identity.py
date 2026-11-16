import copy
import hashlib
import json
import unittest
from unittest.mock import patch

import image_identity as image


def raw(value):
    return json.dumps(value, sort_keys=True).encode()


def digest(value):
    return "sha256:" + hashlib.sha256(value).hexdigest()


def fixture(index=False, platform="amd64"):
    config = raw({"os": "linux", "architecture": platform})
    manifest = raw({"schemaVersion": 2, "mediaType": "application/vnd.oci.image.manifest.v1+json",
                    "config": {"mediaType": "application/vnd.oci.image.config.v1+json", "digest": digest(config), "size": len(config)}, "layers": []})
    data = {"blobs/" + digest(config): config, "manifests/" + digest(manifest): manifest}
    root = manifest
    if index:
        root = raw({"schemaVersion": 2, "mediaType": "application/vnd.oci.image.index.v1+json", "manifests": [
            {"digest": digest(manifest), "size": len(manifest), "platform": {"os": "linux", "architecture": "amd64"}}]})
        data["manifests/" + digest(root)] = root
    return root, manifest, config, data


class ImageIdentityTests(unittest.TestCase):
    def test_direct_manifest_and_index_both_bind_config_platform(self):
        for index in (False, True):
            root, manifest, config, data = fixture(index)
            with self.subTest(index=index), patch.object(image, "ROOT_DIGEST", digest(root)):
                result = image.resolve(data.__getitem__)
                self.assertEqual(result["manifest"], digest(manifest))
                self.assertEqual(result["config"], digest(config))
                self.assertEqual(result["platform"], "linux/amd64")

    def test_wrong_hash_size_platform_duplicate_fields_and_ambiguous_index_refuse(self):
        for mutation in ("hash", "size", "platform", "duplicate", "ambiguous"):
            root, manifest, config, data = fixture(True, "arm64" if mutation == "platform" else "amd64")
            if mutation == "hash":
                data["blobs/" + digest(config)] = b"{}"
            elif mutation == "size":
                obj = json.loads(root); obj["manifests"][0]["size"] += 1
                root = raw(obj); data["manifests/" + digest(root)] = root
            elif mutation == "duplicate":
                root = b'{"schemaVersion":2,"schemaVersion":2}'
                data["manifests/" + digest(root)] = root
            elif mutation == "ambiguous":
                obj = json.loads(root); obj["manifests"].append(copy.deepcopy(obj["manifests"][0]))
                root = raw(obj); data["manifests/" + digest(root)] = root
            with self.subTest(mutation=mutation), patch.object(image, "ROOT_DIGEST", digest(root)), self.assertRaises(ValueError):
                image.resolve(data.__getitem__)

    def test_cri_config_and_repository_both_required(self):
        identity = {"root": image.ROOT_DIGEST, "manifest": "sha256:" + "a" * 64,
                    "config": "sha256:" + "b" * 64, "platform": "linux/amd64"}
        inspection = {"status": {"id": identity["config"], "repoDigests": ["docker.io/library/python@" + identity["manifest"]]}}
        image.verify_cri(identity["manifest"], inspection, identity)
        for mutation in ("config", "repository", "root"):
            obj = copy.deepcopy(inspection); target = copy.deepcopy(identity)
            if mutation == "config": obj["status"]["id"] = identity["manifest"]
            elif mutation == "repository": obj["status"]["repoDigests"] = ["docker.io/library/not-python@" + identity["manifest"]]
            else: target["root"] = "sha256:" + "c" * 64
            with self.subTest(mutation=mutation), self.assertRaises(ValueError):
                image.verify_cri(identity["manifest"], obj, target)


if __name__ == "__main__":
    unittest.main()
