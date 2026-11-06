import copy
import hashlib
import io
from pathlib import Path
import tarfile
import tempfile
import unittest
from unittest.mock import patch

import native_inventory as native


class NativeInventoryTests(unittest.TestCase):
    def members(self):
        return [('lib/libfixture.so.1', b'bounded native bytes', None),
                ('lib/libfixture.so', None, 'libfixture.so.1'),
                ('share/julia/stdlib/v1.13/Fixture_jll/Project.toml',
                 b'name="Fixture_jll"\nuuid="11111111-1111-1111-1111-111111111111"\nversion="1.0.0+0"\n', None)]

    def read(self, members, maximum=None):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'runtime.tar.gz'
            with tarfile.open(path, 'w:gz') as archive:
                for name, data, link in members:
                    info = tarfile.TarInfo('julia-1.13.3/' + name)
                    if link is not None:
                        info.type = tarfile.LNKTYPE if link.startswith('hard:') else tarfile.SYMTYPE
                        info.linkname = link.removeprefix('hard:')
                    else:
                        info.size = len(data)
                    archive.addfile(info, io.BytesIO(data) if data is not None else None)
            with patch.object(native, 'ARCHIVE_SHA256', hashlib.sha256(path.read_bytes()).hexdigest()):
                if maximum is not None:
                    with patch.object(native, 'MAX_BYTES', maximum):
                        return native.expected_archive(path)
                return native.expected_archive(path)

    def test_archive_members_are_bound_without_extraction(self):
        expected = self.read(self.members())
        self.assertEqual(expected['files']['lib/libfixture.so.1']['sha256'],
                         hashlib.sha256(b'bounded native bytes').hexdigest())
        self.assertEqual(expected['jll']['Fixture_jll']['version'], '1.0.0+0')
        self.assertEqual(expected['links'], {'lib/libfixture.so': 'libfixture.so.1'})
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'wrong.tar.gz'; path.write_bytes(b'wrong')
            with self.assertRaises(ValueError):
                native.expected_archive(path)

    def test_path_link_identity_and_capacity_refusals(self):
        base = self.members()
        bad = [base + [base[0]], base + [('lib/../escape', b'x', None)],
               base + [('lib/control\nname', b'x', None)],
               [base[0], ('lib/link', None, '/etc/passwd'), base[2]],
               [base[0], ('lib/link', None, '../../outside'), base[2]],
               [base[0], ('lib/link', None, 'missing'), base[2]],
               [base[0], ('lib/link', None, 'hard:lib/libfixture.so.1'), base[2]],
               [base[0], ('lib/a', None, 'b'), ('lib/b', None, 'a'), base[2]],
               [base[0], base[1], (base[2][0], base[2][1].replace(b'Fixture_jll', b'Wrong_jll'), None)]]
        for members in bad:
            with self.subTest(members=members), self.assertRaises(ValueError):
                self.read(members)
        with self.assertRaises(ValueError):
            self.read(base, maximum=8)

    def test_complete_observed_inventory_and_unknowns_are_required(self):
        expected = self.read(self.members())
        observed = copy.deepcopy(expected)
        observed.update(generatedNativeImages={}, unassignedFiles=sorted(expected['files']), loadedOutsideArchive={'/usr/lib/libc.so.6': {'size': 42, 'sha256': 'a' * 64}}, coverage=native.COVERAGE)
        native.validate(observed, expected)
        mutations = []
        for key in observed:
            changed = copy.deepcopy(observed); del changed[key]; mutations.append(changed)
        for section in ('files', 'links', 'jll'):
            changed = copy.deepcopy(observed); changed[section] = {}; mutations.append(changed)
        changed = copy.deepcopy(observed); changed['files']['extra'] = {}; mutations.append(changed)
        changed = copy.deepcopy(observed); changed['unassignedFiles'] = []; mutations.append(changed)
        changed = copy.deepcopy(observed); changed['coverage'] = 'clear'; mutations.append(changed)
        changed = copy.deepcopy(observed); changed['generatedNativeImages'] = {'../escape.so': {'size': 1, 'sha256': 'a'*64}}; mutations.append(changed)
        changed = copy.deepcopy(observed); changed['loadedOutsideArchive'] = {}; mutations.append(changed)
        changed = copy.deepcopy(observed); changed['loadedOutsideArchive'] = {'/tmp/private': {}}; mutations.append(changed)
        for changed in mutations:
            with self.subTest(changed=changed), self.assertRaises(ValueError):
                native.validate(changed, expected)
        observed['generatedNativeImages'] = {'compiled/v1.13/Fixture/test.so': {'size': 42, 'sha256': 'b' * 64}}
        with self.assertRaises(ValueError):
            native.validate(observed, expected)
        observed['unassignedFiles'] = sorted(list(expected['files']) + ['depot/compiled/v1.13/Fixture/test.so'])
        native.validate(observed, expected)
        observed['loadedOutsideArchive'] = {'/usr/lib/x86_64-linux-gnu/libc.so.6': {'size': 42, 'sha256': 'a' * 64}}
        native.validate(observed, expected)
        observed['loadedOutsideArchive']['/usr/lib/x86_64-linux-gnu/libc.so.6']['size'] = 129 << 20
        with self.assertRaises(ValueError):
            native.validate(observed, expected)


if __name__ == '__main__':
    unittest.main()
