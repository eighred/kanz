"""Execute the same fixed installer against disposable archive-derived roots."""
import hashlib
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tarfile
import tempfile
import unittest

import curl_runtime
import native_inventory
import openssl_runtime


@unittest.skipUnless(sys.platform == 'linux', 'actual native installer requires Linux; Windows runtime is untouched')
class NativeInstallerTests(unittest.TestCase):
    def test_verified_recipe_and_prewrite_refusals(self):
        directory = Path(os.environ['RUNNER_TEMP'])
        archives = [directory / name for name in ('julia.tar.gz', 'openssl.tar.gz', 'libcurl.tar.gz')]
        with archives[0].open('rb') as stream:
            self.assertEqual(hashlib.file_digest(stream, 'sha256').hexdigest(), native_inventory.ARCHIVE_SHA256)
        script = Path(__file__).with_name('install-native-runtime.sh').resolve()
        with tempfile.TemporaryDirectory(prefix='kanz-native-installer-') as temporary:
            base = Path(temporary) / 'original'; base.mkdir()
            names = {'bin/julia', *openssl_runtime.BASE_FILES, curl_runtime.PATH}
            found = set()
            with tarfile.open(archives[0], 'r:gz') as archive:
                for member in archive:
                    name = member.name.removeprefix('julia-1.13.3/')
                    if name not in names:
                        continue
                    self.assertTrue(member.isfile() and 0 < member.size <= 16 << 20)
                    self.assertNotIn(name, found); found.add(name)
                    target = base / name; target.parent.mkdir(parents=True, exist_ok=True)
                    with archive.extractfile(member) as source, target.open('xb') as output:
                        shutil.copyfileobj(source, output, length=65536)
            self.assertEqual(found, names)
            (base / 'share').mkdir()
            def invoke(root):
                return subprocess.run(['bash', str(script), str(root), *map(str, archives)],
                                      capture_output=True, timeout=90, check=False)
            good = Path(temporary) / 'good'; shutil.copytree(base, good)
            result = invoke(good)
            self.assertEqual(result.returncode, 0, 'actual verified installer refused')
            for path, item in {**openssl_runtime.FILES, curl_runtime.PATH: curl_runtime.FILE}.items():
                self.assertEqual(hashlib.sha256((good/path).read_bytes()).hexdigest(), item['sha256'])
            # Altering one original member refuses before replacing either other library.
            bad = Path(temporary) / 'tampered'; shutil.copytree(base, bad)
            (bad / curl_runtime.PATH).write_bytes(b'changed original')
            self.assertNotEqual(invoke(bad).returncode, 0)
            for path, item in openssl_runtime.BASE_FILES.items():
                self.assertEqual(hashlib.sha256((bad/path).read_bytes()).hexdigest(), item['sha256'])
            linked = Path(temporary) / 'linked'; linked.symlink_to(base, target_is_directory=True)
            self.assertNotEqual(invoke(linked).returncode, 0)
            # Reapplying to a mixed or already-repaired tree is not silent success.
            self.assertNotEqual(invoke(good).returncode, 0)
