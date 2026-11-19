import hashlib
import json
import unittest
from unittest.mock import patch

import qualify


class ImageProfileTests(unittest.TestCase):
    def check(self, layers, bundle="bundle"):
        candidate = {"Id": "image", "Os": "linux", "Architecture": "amd64", "RootFS": {"Layers": layers},
                     "Config": {"Entrypoint": [qualify.ENTRY], "User": "65532:65532",
                                "Labels": {"org.opencontainers.image.revision": "source",
                                           "org.eighred.calibration.test-bundle-sha256": "bundle"}}}
        calls = (json.dumps([candidate]), json.dumps([{"RootFS": {"Layers": ["base"]}}]))
        with patch.object(qualify, "required", side_effect=calls):
            return qualify.inspect_image("image", "source", bundle)

    def test_workdir_can_add_empty_tar(self):
        empty_tar = "sha256:" + hashlib.sha256(bytes(1024)).hexdigest()
        for layers in (["base", "application"], ["base", "application", empty_tar]):
            self.assertEqual(self.check(layers), "application")
        for layers in (["other", "application"], ["base"], ["base", empty_tar],
                       ["base", "application", "unexpected"]):
            with self.subTest(layers=layers), self.assertRaises(ValueError):
                self.check(layers)

    def test_bundle_identity_must_match(self):
        with self.assertRaises(ValueError):
            self.check(["base", "application"], "other bundle")
