import hashlib
import json
from pathlib import Path
import tarfile
import tempfile
import unittest

from oci import pack, selected


class RegistryInputs(unittest.TestCase):
    def test_ambiguous_platform_refused(self):
        row = {'platform': {'os': 'linux', 'architecture': 'amd64'}}
        for items in ([], [row, row]):
            with self.assertRaisesRegex(ValueError, 'one linux/amd64'):
                selected({'manifests': items})

    def test_single_platform_pack_preserves_index_and_refuses_different_layer(self):
        with tempfile.TemporaryDirectory() as name:
            root = Path(name)
            platform = root / 'staging/platform'
            platform.mkdir(parents=True)
            def entry(raw, media='application/vnd.oci.image.layer.v1.tar'):
                key = hashlib.sha256(raw).hexdigest()
                (platform / key).write_bytes(raw)
                return {'mediaType': media, 'size': len(raw), 'digest': 'sha256:' + key}
            layer = entry(b'zero')
            config = entry(b'{"os":"linux","architecture":"amd64"}')
            manifest = json.dumps({'schemaVersion': 2, 'config': config, 'layers': [layer]}).encode()
            md = entry(manifest, 'application/vnd.oci.image.manifest.v1+json')
            (platform / 'manifest.json').write_bytes(manifest)
            index = json.dumps({'mediaType': 'application/vnd.oci.image.index.v1+json',
                                'manifests': [{**md, 'platform': {'os': 'linux', 'architecture': 'amd64'}}]}).encode()
            digest = hashlib.sha256(index).hexdigest()
            (platform.parent / 'index').write_bytes(index)
            platform.parent.rename(root / digest)
            ref = 'example.invalid/library/fixture:1'
            pack({ref: digest}, root, root / 'out.tar')
            with tarfile.open(root / 'out.tar') as data:
                top = json.load(data.extractfile('index.json'))['manifests'][0]
                self.assertEqual(top['digest'], 'sha256:' + digest)
                self.assertEqual(top['annotations']['io.containerd.image.name'], ref)
                self.assertEqual(data.extractfile('blobs/sha256/' + digest).read(), index)
            (root / digest / 'platform' / layer['digest'][7:]).write_bytes(b'fake')
            with self.assertRaisesRegex(ValueError, 'digest or size mismatch'):
                pack({ref: digest}, root, root / 'out.tar')
