"""Checks for the separately compiled Julia internal shared library."""
import copy
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parent / 'application_bundle'))
import core_runtime


class CoreRuntimeTests(unittest.TestCase):
    def test_profile_preserves_symbols_types_dependencies_loader_and_glibc(self):
        old = {'symbols': [(str(i), 'T') for i in range(1698)],
               'needed': [str(i) for i in range(11)], 'soname': ['libjulia-internal.so.1.13'],
               'runpath': ['$ORIGIN:$ORIGIN/..'], 'glibc': ['GLIBC_2.14']}
        current = {**old, 'glibc': [*old['glibc'], 'GLIBC_2.17']}
        core_runtime.check_profiles(old, current)
        for key, value in [('symbols', [('0', 'D'), *old['symbols'][1:]]), ('needed', old['needed'][:-1]),
                           ('soname', ['libjulia.so']), ('runpath', ['$ORIGIN']), ('glibc', ['GLIBC_2.39'])]:
            with self.subTest(key=key), self.assertRaisesRegex(ValueError, 'ABI differs'):
                core_runtime.check_profiles(old, {**current, key: value})

    def test_unexpected_elf_refused_before_readelf(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / 'other.so'
            path.write_bytes(b'other')
            with patch.object(core_runtime, 'profile') as profile, self.assertRaisesRegex(ValueError, 'ELF identity'):
                core_runtime.verify_abi(path, path, path, path, path)
            profile.assert_not_called()

    def test_gcc9_shard_selection_does_not_change_gcc15(self):
        existing = copy.deepcopy(core_runtime.gcc_runtime.inputs()['shards'])
        with patch.object(core_runtime, 'validate_file') as check:
            core_runtime.verify_shards(Path('/fixture'))
        observed = {args.args[0].name: args.args[1] for args in check.call_args_list}
        self.assertEqual(set(existing), set(observed))
        self.assertEqual(observed['gcc-gnu-bootstrap.tar.gz'], core_runtime.inputs()['files']['gcc9.tar.gz'])
        self.assertNotEqual(observed['gcc-gnu-bootstrap.tar.gz'], existing['gcc-gnu-bootstrap.tar.gz'])
        for name in ('gcc-gnu-platform.tar.gz', 'gcc-musl-root.tar.gz'):
            self.assertEqual(observed[name], existing[name])
