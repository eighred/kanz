import copy
import io
import json
import os
from pathlib import Path
import struct
import tarfile
import tempfile
import unittest
from unittest.mock import patch

import produce as p


def library(soname='libcrypto.so.3', extra_tag=None):
    """Small ELF64 fixture, with independently stated ABI expectations."""
    strings = b'\0' + soname.encode() + b'\0libc.so.6\0OpenSSL_version\0'
    libc = strings.index(b'libc.so.6')
    symbol = strings.index(b'OpenSSL_version')
    names = b'\0.shstrtab\0.dynstr\0.dynamic\0.dynsym\0.gnu.version_d\0'
    payloads = [b'', names, strings,
                struct.pack('<qQqQqQ', 14, 1, 1, libc, 0, 0),
                bytes(24) + struct.pack('<IBBHQQ', symbol, 18, 0, 1, 4096, 4), b'versions']
    if extra_tag is not None:
        payloads[3] += struct.pack('<qQ', extra_tag, 1)
    output = bytearray(b'\x7fELF\x02\x01\x01' + bytes(57))
    offsets = []
    for data in payloads:
        offsets.append(len(output))
        output.extend(data)
    section_offset = len(output)
    for i, data in enumerate(payloads):
        label = ['', '.shstrtab', '.dynstr', '.dynamic', '.dynsym', '.gnu.version_d'][i]
        output.extend(struct.pack('<IIQQQQIIQQ', names.index(label.encode() + b'\0'),
                                  [0, 3, 3, 6, 11, 0x6ffffffd][i], 0, 0,
                                  offsets[i], len(data), 2 if i in (3, 4) else 0, 0, 1, 0))
    struct.pack_into('<HHIQQQIHHHHHH', output, 16, 3, 62, 1, 0, 0,
                     section_offset, 0, 64, 0, 0, 64, 6, 1)
    return bytes(output)


class SourceIdentityTests(unittest.TestCase):
    def test_commit_tree_and_epoch_must_match(self):
        commit = {'sha': p.PINS['source'], 'tree': {'sha': p.PINS['tree']},
                  'committer': {'date': '2026-11-17T16:11:53Z'}}
        raw = json.dumps(commit).encode()
        with patch.object(p, 'fetch', return_value=raw):
            self.assertEqual(p.upstream_commit()['sha256'], p.digest(raw))
        for key, value in [('sha', '0' * 40), ('tree', {'sha': '0' * 40}),
                           ('committer', {'date': '2026-11-17T16:11:52Z'})]:
            with self.subTest(key=key), patch.object(p, 'fetch', return_value=json.dumps(
                    {**commit, key: value}).encode()), self.assertRaisesRegex(ValueError, 'upstream commit'):
                p.upstream_commit()

    def test_only_the_published_maintenance_patch_can_establish_ancestry(self):
        ident, fix = 'CVE-2026-124709', p.PINS['fixes']['CVE-2026-124709']
        compare = {'status': 'ahead', 'base_commit': {'sha': fix}, 'merge_base_commit': {'sha': fix}}
        ref = {'name': '3.5.10 git commit', 'tags': ['patch'],
               'url': 'https://github.com/openssl/openssl/commit/' + fix}
        with patch.dict(p.PINS, {'fixes': {ident: fix}}):
            for label, refs, state, ancestor in [
                ('valid', [ref], 'PUBLISHED', compare),
                ('different branch', [{**ref, 'name': '4.0.4 git commit'}], 'PUBLISHED', compare),
                ('not a patch', [{**ref, 'tags': ['related']}], 'PUBLISHED', compare),
                ('ambiguous', [ref, ref], 'PUBLISHED', compare),
                ('unpublished', [ref], 'RESERVED', compare),
                ('not an ancestor', [ref], 'PUBLISHED', {**compare, 'status': 'behind'}),
            ]:
                record = {'cveMetadata': {'cveId': ident, 'state': state},
                          'containers': {'cna': {'providerMetadata': {'shortName': 'openssl'},
                                                 'references': refs}}}
                with self.subTest(label=label), patch.object(p, 'fetch', side_effect=[
                        json.dumps(ancestor).encode(), json.dumps(record).encode()]):
                    if label == 'valid':
                        self.assertEqual(p.provenance()[ident]['fix'], fix)
                    else:
                        with self.assertRaises(ValueError):
                            p.provenance()


class ELFTests(unittest.TestCase):
    def test_known_abi_and_exports(self):
        value = p.elf(library(), 'libcrypto.so.3')
        self.assertEqual(value['needed'], ['libc.so.6'])
        self.assertEqual(value['exports'], ['OpenSSL_version'])

    def test_truncated_or_foreign_elf_refused(self):
        for data in (b'ELF', library()[:80], library().replace(b'\x7fELF\x02', b'\x7fELF\x01', 1)):
            with self.subTest(data=data[:8]), self.assertRaises((ValueError, KeyError)):
                p.elf(data, 'libcrypto.so.3')

    def test_wrong_soname_or_dependency_refused(self):
        for data in (library('libcrypto.so.9'), library().replace(b'libc.so.6', b'evil.so.6')):
            with self.assertRaises(ValueError):
                p.elf(data, 'libcrypto.so.3')

    def test_rpath_and_runpath_refused(self):
        for tag in (15, 29):
            with self.subTest(tag=tag), self.assertRaises(ValueError):
                p.elf(library(extra_tag=tag), 'libcrypto.so.3')

    def test_unbounded_section_table_refused(self):
        data = bytearray(library())
        struct.pack_into('<Q', data, 40, 1 << 60)
        with self.assertRaises(ValueError):
            p.elf(bytes(data), 'libcrypto.so.3')


class TestResults(unittest.TestCase):
    def test_required_recipe_pass_is_not_a_generic_green_summary(self):
        good = b'../test/recipes/15-test_ec.t ........ ok\nAll tests successful.\nFiles=1, Tests=4\nResult: PASS\n'
        self.assertEqual(p.tests(good, ['test_ec'])['summaries'], ['Files=1, Tests=4'])
        for bad in (good.replace(b' ok', b' skipped: unsupported'), good.replace(b'test_ec', b'test_genec'),
                    good.replace(b'Result: PASS', b'Result: FAIL'), good + good):
            with self.assertRaises(ValueError):
                p.tests(bad, ['test_ec'])

    def test_skip_inventory_retained(self):
        log = b'15-test_ec.t .... ok\nok 2 # SKIP platform\nAll tests successful.\nResult: PASS\n'
        self.assertEqual(p.tests(log, ['test_ec'])['skips'], ['ok 2 # SKIP platform'])

    def test_named_regression_must_pass_without_skip(self):
        log = b'15-test_ec.t ....\nok 84 - keygen_invalid_order_test\nAll tests successful.\nResult: PASS\n'
        p.tests(log, ['test_ec'], ['keygen_invalid_order_test'])
        for bad in (log.replace(b'ok 84', b'not ok 84'), log.replace(b'_test\n', b'_test # SKIP\n')):
            with self.assertRaises(ValueError):
                p.tests(bad, ['test_ec'], ['keygen_invalid_order_test'])


@unittest.skipUnless(os.name == 'posix' and hasattr(os, 'O_NOFOLLOW'), 'protected Linux filesystem checks')
class PromotionTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.path = Path(self.temp.name)
        self.fd = os.open(self.path, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)

    def tearDown(self):
        os.close(self.fd)
        self.temp.cleanup()

    def test_regular_exactly_bounded_output(self):
        (self.path / 'member').write_bytes(b'public')
        self.assertEqual(p.read_regular(self.fd, 'member', 6), b'public')
        with self.assertRaises(ValueError):
            p.read_regular(self.fd, 'member', 5)

    def test_links_and_fifo_never_promoted(self):
        (self.path / 'source').write_bytes(b'public')
        os.symlink('source', self.path / 'symlink')
        os.link(self.path / 'source', self.path / 'hardlink')
        os.mkfifo(self.path / 'fifo')
        for name in ('symlink', 'hardlink', 'fifo'):
            with self.subTest(name=name), self.assertRaises((ValueError, OSError)):
                p.read_regular(self.fd, name, 100)

    def test_protected_output_is_exclusive(self):
        p.write_new(self.fd, 'receipt.json', b'{}')
        with self.assertRaises(FileExistsError):
            p.write_new(self.fd, 'receipt.json', b'changed')
        with self.assertRaises(ValueError):
            p.write_new(self.fd, '../redirect', b'changed')

    def test_archive_and_output_parent_links_refused(self):
        (self.path / 'real').mkdir()
        (self.path / 'real' / 'archive').write_bytes(b'public')
        os.symlink('real', self.path / 'redirect')
        with self.assertRaises(OSError):
            p.read_path(self.path / 'redirect' / 'archive', 100)
        with self.assertRaises(OSError):
            p.new_directory(self.path / 'redirect' / 'evidence')

    def test_real_scratch_with_wrong_owner_is_refused_before_target_start(self):
        self.assertNotEqual(os.fstat(self.fd).st_uid, 65532)
        with self.assertRaises(ValueError):
            p.scratch_boundary(self.fd, self.fd)

    def test_staging_copies_only_bounded_allowlisted_files(self):
        staged = self.path / 'staged'
        p.stage_tests(staged)
        self.assertEqual({x.name for x in staged.iterdir()},
                         {'produce.py', 'test_produce.py', 'pins.json', 'VERSION.dat'})
        self.assertEqual(staged.stat().st_mode & 0o777, 0o755)
        for item in staged.iterdir():
            self.assertEqual(item.read_bytes(), (p.HERE / item.name).read_bytes())
            self.assertEqual(item.stat().st_mode & 0o777, 0o644)

    def test_staging_refuses_linked_source(self):
        source = self.path / 'fake_source'
        source.mkdir()
        os.symlink(p.HERE / 'produce.py', source / 'produce.py')
        with patch.object(p, 'HERE', source), self.assertRaises(OSError):
            p.stage_tests(self.path / 'refused')

    def test_log_refusal_still_closes_descriptors_and_unmounts(self):
        work = self.path / 'scratch'
        result = work / 'result'
        result.mkdir(parents=True)
        os.symlink('missing', result / 'build.log')
        work_fd = os.open(work, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
        source_fd = os.open(result, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
        cleanup = {'remaining': 0}
        with patch.object(p, 'checked', return_value=b'') as commands, patch.object(p.os, 'rmdir'):
            with self.assertRaises(OSError):
                p.release_scratch(source_fd, work_fd, work, True, self.fd, 'one',
                                  RuntimeError('original build failure'), cleanup)
        commands.assert_called_once_with(['sudo', 'umount', str(work)])
        self.assertTrue(cleanup['scratchUnmounted'])
        for fd in (source_fd, work_fd):
            with self.assertRaises(OSError):
                os.fstat(fd)


class BoundaryTests(unittest.TestCase):
    def test_missing_network_memory_or_readonly_control_refuses(self):
        image = {'Id': 'sha256:expected', 'Config': {'Env': ['PATH=/usr/bin:/bin']}}
        value = {'Image': image['Id'], 'Config': {'User': '65532:65532',
                 'Entrypoint': ['/usr/bin/env'], 'Cmd': p.build_command(), 'Env': image['Config']['Env']},
                 'Mounts': [{'Source': '/work', 'Destination': '/work', 'RW': True},
                            {'Source': '/input', 'Destination': '/input', 'RW': False}],
                 'HostConfig': {'NetworkMode': 'none', 'IpcMode': 'none', 'ReadonlyRootfs': True,
                                'Privileged': False, 'Memory': 2 << 30, 'MemorySwap': 2 << 30,
                                'NanoCpus': 2000000000, 'PidsLimit': 128, 'CapDrop': ['ALL'],
                                'SecurityOpt': ['no-new-privileges'],
                                'Ulimits': [{'Name': name, 'Soft': limit, 'Hard': limit} for name, limit
                                            in [('nproc', 128), ('nofile', 512), ('fsize', 33554432), ('cpu', 1200)]]}}
        p.inspect_boundary(value, image, Path('/work'), Path('/input'))
        for key, bad in [('NetworkMode', 'host'), ('ReadonlyRootfs', False), ('Memory', 0), ('PidsLimit', 0), ('Ulimits', [])]:
            changed = copy.deepcopy(value)
            changed['HostConfig'][key] = bad
            with self.subTest(key=key), self.assertRaises(ValueError):
                p.inspect_boundary(changed, image, Path('/work'), Path('/input'))
        for key, bad in [('Cmd', ['/bin/sh']), ('Entrypoint', ['/bin/sh']), ('Env', ['TOKEN=dummy'])]:
            changed = copy.deepcopy(value)
            changed['Config'][key] = bad
            with self.subTest(key=key), self.assertRaises(ValueError):
                p.inspect_boundary(changed, image, Path('/work'), Path('/input'))

    def test_archive_hash_tamper_refused_before_parsing(self):
        with self.assertRaises(ValueError):
            p.source_archive(b'not the source')

    def test_source_member_limit_accepts_boundary_and_refuses_one_more_byte(self):
        for size in (32 << 20, (32 << 20) + 1):
            buffer = io.BytesIO()
            prefix = 'openssl-' + p.PINS['source'] + '/'
            with tarfile.open(fileobj=buffer, mode='w:gz') as archive:
                for name, data in [('Configure', bytes(size)), ('LICENSE.txt', b'fixture'),
                                   ('VERSION.dat', (p.HERE / 'VERSION.dat').read_bytes())]:
                    member = tarfile.TarInfo(prefix + name)
                    member.size = len(data)
                    archive.addfile(member, io.BytesIO(data))
            raw = buffer.getvalue()
            with patch.dict(p.PINS, {'size': len(raw), 'sha256': p.digest(raw)}):
                if size == 32 << 20:
                    p.source_archive(raw)
                else:
                    with self.assertRaisesRegex(ValueError, 'unsafe source archive member'):
                        p.source_archive(raw)


if __name__ == '__main__':
    unittest.main()
