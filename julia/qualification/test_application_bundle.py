import io
from pathlib import Path
import sys
import tarfile
import unittest

sys.path.insert(0, str(Path(__file__).resolve().parent / 'application_bundle'))
import readback
import scan


class ApplicationReadbackTests(unittest.TestCase):
    def test_missing_or_substituted_compiler_library(self):
        selected = readback.DISTRIBUTION | readback.GENERATED
        for removed in ('lib/libjulia.so.1.13.3', 'lib/julia/libgcc_s.so.1', 'lib/julia/sys.so'):
            for replacement in (set(), {'lib/julia/other.so'}):
                with self.subTest(removed=removed, replacement=replacement):
                    names = selected.difference({removed}) | replacement
                    with self.assertRaisesRegex(ValueError, 'missing or unexpected'):
                        readback.source_libraries([], dict.fromkeys(names))

    def test_forbidden_archive_owner_in_application(self):
        buf = io.BytesIO()
        with tarfile.open(fileobj=buf, mode='w') as tar:
            tar.addfile(tarfile.TarInfo('libexec/7z'))
        buf.seek(0)
        with tarfile.open(fileobj=buf) as tar:
            with self.assertRaisesRegex(ValueError, 'unselected native'):
                readback.contents(tar)

    def test_missing_dependency_identity(self):
        required = {'app': {'dynamic': [('NEEDED', 'libc.so.6')]}}
        provided = {'libc': {'dynamic': [('SONAME', 'libc.so.6')]}}
        readback.validate_needed(required, provided)
        with self.assertRaisesRegex(ValueError, 'libc.so.6'):
            readback.validate_needed(required, {})

    def test_publisher_projection_covers_each_file_once(self):
        names = ['lib/julia/' + f for files in readback.JLL_LIBRARIES.values() for f in files]
        self.assertEqual(len(names), 18)
        self.assertEqual(set(names), readback.DISTRIBUTION - readback.JULIA_CORE)
        self.assertFalse(set(names) & readback.GENERATED)

    def test_spdx_uses_actual_manifest_and_uuid(self):
        package = {'name': 'ProtoBuf', 'versionInfo': '1.3.0', 'sourceInfo': 'package found in: ' + scan.MANIFEST,
                   'externalRefs': [{'referenceType': 'purl', 'referenceLocator': 'pkg:julia/ProtoBuf@1.3.0?uuid=id'}]}
        expected = {'ProtoBuf': {'uuid': 'id', 'version': '1.3.0'}}
        scan.spdx_packages({'packages': [package]}, expected)
        for change in ({'sourceInfo': 'package found in: other/Manifest.toml'}, {'versionInfo': '1.1.0'}):
            with self.subTest(change=change), self.assertRaises(ValueError):
                scan.spdx_packages({'packages': [{**package, **change}]}, expected)
