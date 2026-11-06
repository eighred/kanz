import test_openssl as ossl
import copy
import hashlib
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch

import proof


class QualificationTests(unittest.TestCase):
    def item(self):
        return {'Image': 'sha256:' + 'a' * 64,
                'Config': {'User': '65532:65532', 'Labels': {'kanz.julia-proof': 'b' * 40}},
                'HostConfig': {'NetworkMode': 'none', 'ReadonlyRootfs': True, 'Privileged': False,
                    'Binds': [], 'Devices': [], 'Memory': 1 << 30, 'MemorySwap': 1 << 30,
                    'NanoCpus': 1000000000, 'PidsLimit': 64, 'CapDrop': ['ALL'],
                    'CapAdd': [], 'SecurityOpt': ['no-new-privileges'],
                    'Tmpfs': {'/tmp': 'rw,noexec,nosuid,size=16777216,mode=1777'}}}

    def test_actual_container_configuration_refuses_weakened_boundary(self):
        item = self.item()
        proof.container_boundary(item, item['Image'], 'b' * 40)
        mutations = [('NetworkMode', 'host'), ('ReadonlyRootfs', False), ('Privileged', True),
                     ('Binds', ['/host:/target']), ('Devices', ['device']), ('Memory', 0),
                     ('MemorySwap', -1), ('NanoCpus', 0), ('PidsLimit', 0), ('CapDrop', []),
                     ('CapAdd', ['SYS_ADMIN']), ('SecurityOpt', []), ('Tmpfs', {'/opt': 'rw'})]
        for key, value in mutations:
            changed = copy.deepcopy(item)
            changed['HostConfig'][key] = value
            with self.subTest(key=key), self.assertRaises(ValueError):
                proof.container_boundary(changed, item['Image'], 'b' * 40)
        for key, value in [('User', '0'), ('Labels', {'kanz.julia-proof': 'other'})]:
            changed = copy.deepcopy(item)
            changed['Config'][key] = value
            with self.assertRaises(ValueError):
                proof.container_boundary(changed, item['Image'], 'b' * 40)
        with self.assertRaises(ValueError):
            proof.container_boundary(item, 'sha256:' + 'c' * 64, 'b' * 40)
        for options in ('rw,exec,nosuid,size=16777216,mode=1777',
                        'rw,noexec,suid,size=16777216,mode=1777',
                        'rw,noexec,nosuid,size=1073741824,mode=1777'):
            changed = copy.deepcopy(item)
            changed['HostConfig']['Tmpfs']['/tmp'] = options
            with self.assertRaises(ValueError):
                proof.container_boundary(changed, item['Image'], 'b' * 40)

    def test_command_success_and_nonzero_are_distinct(self):
        self.assertEqual(proof.command([sys.executable, '-I', '-c', 'import sys;sys.stdout.buffer.write(b"synthetic\\n")']), (0, b'synthetic\n'))
        code, output = proof.command([sys.executable, '-I', '-c', 'raise SystemExit(3)'])
        self.assertEqual((code, output), (3, b''))

    def test_pipe_output_and_deadline_refuse(self):
        for source, kwargs in [('print("x"*10000)', {'limit': 128}),
                               ('import sys;sys.stderr.write("x"*40000)', {}),
                               ('import time;time.sleep(2)', {'timeout': .05})]:
            with self.subTest(source=source), self.assertRaises(RuntimeError):
                proof.command([sys.executable, '-I', '-c', source], **kwargs)

    def test_diagnostics_export_only_closed_categories(self):
        for stage in proof.ISOLATION_STAGES:
            raw = b"private runtime output\nKANZ_OCI_STAGE=" + stage.encode() + b"\nprivate suffix\n"
            self.assertEqual(proof.isolation_stage(raw), stage)
        for raw in (b"private", b"prefix KANZ_OCI_STAGE=runtime",
                    b"KANZ_OCI_STAGE=runtime/private", b"KANZ_OCI_STAGE=runtime\nKANZ_OCI_STAGE=unknown"):
            self.assertEqual(proof.isolation_stage(raw), "unclassified")
        diagnostic = {}
        proof.command([sys.executable, "-I", "-c",
                       "import sys;sys.stderr.write('private\\nKANZ_OCI_STAGE=cgroup\\n');sys.exit(3)"],
                      diagnostic=diagnostic)
        self.assertEqual(diagnostic, {"exitCode": 3, "isolationStage": "cgroup"})

    def test_wire_inventory_is_closed(self):
        self.assertEqual(proof.CASES, {'deposit', 'forward', 'swap', 'negative', 'stub', 'missing',
                                     'zero', 'unsupported', 'invalid', 'numerical', 'empty', 'malformed', 'oversize'})

    def test_complete_evidence_cannot_hide_missing_or_skipped_boundaries(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            project = root / 'kanz-jl'
            (project / 'gen').mkdir(parents=True)
            (project / 'Project.toml').write_bytes(b'public fixture')
            (project / 'Manifest.toml').write_text('[[deps.ProtoBuf]]\nversion="1.3.0"\ngit-tree-sha1="' + 'c' * 40 + '"\n')
            (project / 'gen/contract.jl').write_bytes(b'canonical fixture')
            inventory = {'sourceSHA': 'b' * 40, 'runtime': '1.13.3',
                         'projectSHA256': hashlib.sha256((project / 'Project.toml').read_bytes()).hexdigest(),
                         'manifestSHA256': hashlib.sha256((project / 'Manifest.toml').read_bytes()).hexdigest(),
                         'packages': {'ProtoBuf': {'version': '1.3.0', 'treeSHA1': 'c' * 40}},
                         'generated': {'contract.jl': hashlib.sha256(b'canonical fixture').hexdigest()},
                         'runtimeExecutableSHA256': 'd' * 64,
                         'excludedDevelopmentEnvironments': proof.EXCLUDED_DEVELOPMENT_ENVIRONMENTS,
                         'native7zip': proof.native_mapping.COMPONENT,
                         'nativeRuntime': {'generatedNativeImages': {}, 'files': {proof.native_mapping.PATH: proof.native_mapping.FILE}, 'links': {}, 'jll': {}, 'unassignedFiles': [proof.native_mapping.PATH], 'loadedOutsideArchive': {'/usr/lib/libc.so.6': {'size': 42, 'sha256': 'a' * 64}}, 'coverage': proof.native_inventory.COVERAGE},
                         'sources': {'qualification/isolation.jl': hashlib.sha256(Path(proof.__file__).with_name('isolation.jl').read_bytes()).hexdigest()}}
            inventory['nativeOpenSSL'] = ossl.component()
            inventory['nativeRuntime']['files'].update(proof.openssl_runtime.FILES)
            inventory['nativeRuntime']['jll']['OpenSSL_jll'] = {'version': '3.5.8+0'}
            inventory['nativeRuntime']['unassignedFiles'] = sorted(inventory['nativeRuntime']['files'])
            with patch.object(proof, 'ROOT', root):
                self.check_inventory_mutations(inventory)

    def check_inventory_mutations(self, inventory):
        report = {'sourceSHA': 'b' * 40, 'imageID': 'sha256:' + 'a' * 64, 'localOnly': True,
                  'passed': True, 'cleanupPassed': True, 'isolationPassed': True, 'goldensPassed': True,
                  'timeoutCleanupPassed': True,
                  'juliaAdvisoryCoverage': 'unqualified-release-blocker', 'archiveSHA256': proof.ARCHIVE_SHA256,
                  'cases': sorted(proof.CASES), 'inventory': inventory}
        proof.validate_report(report, 'b' * 40, 'd' * 64, {k: inventory['nativeRuntime'][k] for k in ('files', 'links', 'jll')})
        for field in ('sourceSHA', 'imageID', 'localOnly', 'passed', 'cleanupPassed', 'isolationPassed',
                      'goldensPassed', 'timeoutCleanupPassed', 'juliaAdvisoryCoverage', 'archiveSHA256', 'cases', 'inventory'):
            changed = copy.deepcopy(report)
            del changed[field]
            with self.subTest(field=field), self.assertRaises(ValueError):
                proof.validate_report(changed, 'b' * 40, 'd' * 64, {k: inventory['nativeRuntime'][k] for k in ('files', 'links', 'jll')})
        for field in ('runtime', 'sourceSHA', 'projectSHA256', 'manifestSHA256', 'packages',
                      'sources', 'generated', 'runtimeExecutableSHA256', 'excludedDevelopmentEnvironments', 'nativeRuntime', 'native7zip', 'nativeOpenSSL'):
            for replacement in (None, {}, 'wrong'):
                changed = copy.deepcopy(report)
                changed['inventory'][field] = replacement
                with self.subTest(field=field, replacement=replacement), self.assertRaises(ValueError):
                    proof.validate_report(changed, 'b' * 40, 'd' * 64, {k: inventory['nativeRuntime'][k] for k in ('files', 'links', 'jll')})
        report['cases'].append(report['cases'][0])
        with self.assertRaises(ValueError):
            proof.validate_report(report, 'b' * 40, 'd' * 64, {k: inventory['nativeRuntime'][k] for k in ('files', 'links', 'jll')})


if __name__ == '__main__':
    unittest.main()
