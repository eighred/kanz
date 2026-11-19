"""Checks for the signed GCC application-candidate rebuild and its ABI bounds."""
import copy
import sys
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parent / 'application_bundle'))
import gcc_advisories
import gcc_runtime
import compiler_owners


class GCCRuntimeTests(unittest.TestCase):
    def test_shard_substitution_is_rejected(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            (root / 'bootstrap').write_bytes(b'public controls')
            record = {'bytes': 15, 'sha256': gcc_runtime.digest(root / 'bootstrap')}
            with patch.object(gcc_runtime, 'inputs', return_value={'shards': {'bootstrap': record}}):
                gcc_runtime.verify_shards(root)
                (root / 'bootstrap').write_bytes(b'public control?')
                with self.assertRaisesRegex(ValueError, 'shard differs'):
                    gcc_runtime.verify_shards(root)

    def test_matching_abi_and_negative_controls(self):
        baseline = {'symbols': [(str(i) + '@GLIBCXX_3.4', 'T') for i in range(6097)],
                    'needed': ['libgcc_s.so.1'], 'soname': ['libstdc++.so.6'], 'glibc': ['GLIBC_2.17']}
        def check(current):
            with patch.object(gcc_runtime, 'digest', side_effect=[gcc_runtime.ORIGINAL, gcc_runtime.REPLACEMENT]), \
                    patch.object(gcc_runtime, 'abi', side_effect=[baseline, current]):
                gcc_runtime.validate_abi(None, None)
        check(baseline)
        for field, value in [('symbols', baseline['symbols'][1:]), ('soname', ['libstdc++.so.7']),
                             ('symbols', [('0@GLIBCXX_3.4', 'B'), *baseline['symbols'][1:]]),
                             ('needed', ['libextra.so']), ('glibc', ['GLIBC_2.39'])]:
            with self.subTest(field=field):
                variant = copy.deepcopy(baseline)
                variant[field] = value
                with self.assertRaisesRegex(ValueError, 'ABI differs'):
                    check(variant)

    def test_unrecognized_rebuild_fails_before_symbol_comparison(self):
        with patch.object(gcc_runtime, 'digest', side_effect=[gcc_runtime.ORIGINAL, '0' * 64]), \
                patch.object(gcc_runtime, 'abi') as read_symbols:
            with self.assertRaisesRegex(ValueError, 'rebuilt libstdc'):
                gcc_runtime.validate_abi(None, None)
            read_symbols.assert_not_called()

    def test_wrong_application_bytes_fail_before_comparison_with_publisher(self):
        with self.assertRaisesRegex(ValueError, 'packaged C\\+\\+ runtime'):
            gcc_runtime.publisher_view({'lib/julia/' + gcc_runtime.LIB: {'sha256': gcc_runtime.ORIGINAL}}, None, None)

    def test_only_reviewed_native_replacement_can_narrow_compiler_findings(self):
        wrapper = {'publisherArchiveSHA256': compiler_owners.GCC_BUNDLE,
                   'publisherFiles': {'lib/' + gcc_runtime.LIB: gcc_runtime.ORIGINAL}}
        legacy = compiler_owners.gcc_pending(wrapper, gcc_runtime.ORIGINAL, {})
        self.assertEqual(len(legacy['libstdc++']), 7)
        rebuilt = {**wrapper, 'localRebuild': gcc_runtime.rebuild_record()}
        mapped = {'CVE-2026-115456': gcc_advisories.FROM_CHARS, 'CVE-2026-125709': gcc_advisories.FILEBUF}
        with self.assertRaisesRegex(ValueError, 'mappings missing'):
            compiler_owners.gcc_pending(rebuilt, gcc_runtime.REPLACEMENT, {})
        pending = compiler_owners.gcc_pending(rebuilt, gcc_runtime.REPLACEMENT, mapped)
        self.assertEqual(len(pending['libstdc++ header consumers']), 5)
        self.assertEqual(set(pending), {'libstdc++ header consumers'})
        for row, elf in [(wrapper, gcc_runtime.REPLACEMENT), (rebuilt, gcc_runtime.ORIGINAL),
                         ({**wrapper, 'localRebuild': {}}, gcc_runtime.REPLACEMENT)]:
            with self.assertRaisesRegex(ValueError, 'runtime differs'):
                compiler_owners.gcc_pending(row, elf, mapped)

    def test_incomplete_packager_patch_metadata_is_rejected(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            for name, (_, length, _) in (gcc_advisories.SPECS | gcc_advisories.HEADERS).items():
                (root / name).write_bytes(bytes(length))
            with self.assertRaisesRegex(ValueError, 'packaging record differs'):
                gcc_advisories.mappings(root)
            (root / 'centos-gcc.spec').write_bytes(b'')
            with self.assertRaisesRegex(ValueError, 'record size differs'):
                gcc_advisories.mappings(root)
