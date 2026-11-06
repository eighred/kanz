import os
import json
import copy
from pathlib import Path
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch

import prepare_runner
import qualify


class CapacityTests(unittest.TestCase):
    def test_host_space_cannot_replace_post_provision_pod_readback(self):
        low = {'availableBytes': 5 << 30, 'producerReserveBytes': 5 << 30}
        high = dict(low, availableBytes=9 << 30)
        host = {'beforeBytes': 5 << 30, 'afterBytes': 50 << 30,
                'requiredBytes': 7 << 30, 'unrelatedAndroidSDKRemoved': True}
        for final, passed in ((low, False), (high, True)):
            evidence = {}
            with patch.object(qualify, 'broker_storage', side_effect=[low, final]) as observed, \
                 patch.object(qualify, 'prepare_host', return_value=(host, True)) as prepare:
                if passed:
                    self.assertEqual(qualify.preflight_storage(None, evidence, 'bootstrap'), high)
                else:
                    with self.assertRaises(ValueError): qualify.preflight_storage(None, evidence, 'bootstrap')
                self.assertEqual(observed.call_count, 2)
                prepare.assert_called_once_with(7 << 30)
                self.assertEqual(evidence['hostCapacityPreparation'], host)
        with patch.object(qualify, 'broker_storage', return_value=high), \
             patch.object(qualify, 'prepare_host') as prepare:
            qualify.preflight_storage(None, {}, 'bootstrap')
            prepare.assert_not_called()

    def test_failed_preparation_preserves_numeric_diagnostic(self):
        low = {'availableBytes': 5 << 30, 'producerReserveBytes': 5 << 30}
        report = {'beforeBytes': 1, 'afterBytes': 2, 'requiredBytes': 7 << 30,
                  'unrelatedAndroidSDKRemoved': True}
        evidence = {}
        with patch.object(qualify, 'broker_storage', return_value=low), \
             patch.object(qualify, 'prepare_host', return_value=(report, False)):
            with self.assertRaises(RuntimeError): qualify.preflight_storage(None, evidence, 'bootstrap')
        self.assertEqual(evidence['hostCapacityPreparation'], report)

    def test_host_diagnostic_closed_shape_and_failure_retention(self):
        report = {'beforeBytes': 1, 'afterBytes': 2, 'requiredBytes': 7 << 30,
                  'unrelatedAndroidSDKRemoved': True}
        result = SimpleNamespace(stdout=json.dumps(report), stderr='unretained private detail', returncode=1)
        with patch.object(qualify.subprocess, 'run', return_value=result):
            self.assertEqual(qualify.prepare_host(7 << 30), (report, False))
        for value in (dict(report, afterBytes=True), dict(report, requiredBytes=1),
                      dict(report, extra='private')):
            result.stdout = json.dumps(value)
            with patch.object(qualify.subprocess, 'run', return_value=result):
                with self.assertRaises(ValueError): qualify.prepare_host(7 << 30)

    def test_report_requires_every_bound_capacity_observation(self):
        sha = 'a' * 40
        evidence = {'sourceSHA': sha, 'passed': True, 'cleanupPassed': True,
                    'developmentOnly': True, 'brokerImage': qualify.REDPANDA,
                    'observations': [], 'storageObservations': []}
        for case, sa, mode in qualify.CASES:
            evidence['observations'].append({'case': case, 'passed': True, 'mode': mode,
                'identity': qualify.identity(sa), 'subjectDN': 'CN=fixture,O=SPIRE,C=US,' + qualify.subject(sa),
                'renewed': mode == 'publish', 'boundedReplay': mode == 'publish', 'sourceVersion': sha})
            evidence['storageObservations'].append({'case': case, 'totalBytes': 40 << 30,
                'usedBytes': 10 << 30, 'availableBytes': 30 << 30, 'producerReserveBytes': 5 << 30,
                'requiredHeadroomBytes': qualify.HEADROOM})
        qualify.validate_report(evidence, sha)
        for field, value in [('case', 'other'), ('availableBytes', 7 << 30),
                             ('producerReserveBytes', 0), ('availableBytes', True),
                             ('requiredHeadroomBytes', 0), ('usedBytes', 40 << 30)]:
            bad = copy.deepcopy(evidence); bad['storageObservations'][2][field] = value
            with self.assertRaises(ValueError): qualify.validate_report(bad, sha)
        for index in range(5):
            bad = copy.deepcopy(evidence); del bad['storageObservations'][index]
            with self.assertRaises(ValueError): qualify.validate_report(bad, sha)
        bad = copy.deepcopy(evidence); bad['storageObservations'].append(bad['storageObservations'][0])
        with self.assertRaises(ValueError): qualify.validate_report(bad, sha)
        with self.assertRaises(ValueError): qualify.validate_report(evidence, 'b' * 40)

    def test_numeric_reserve_and_strict_boundary(self):
        reserve = qualify.storage_reserve('5368709120\n')
        self.assertEqual(reserve, 5 << 30)
        for raw in ('', 'private', '-1', '0', '1e9', '1\n2', '9' * 21, str(1 << 63), ' ' * 65 + '1'):
            with self.assertRaises(ValueError):
                qualify.storage_reserve(raw)
        for available in (reserve - 1, reserve, reserve + qualify.HEADROOM):
            with self.assertRaises(ValueError):
                qualify.require_capacity({'availableBytes': available}, reserve)
        qualify.require_capacity({'availableBytes': reserve + qualify.HEADROOM + 1}, reserve)

    def test_actual_broker_commands_are_read_only_and_narrow(self):
        calls = []
        def command(*args):
            calls.append(args)
            if 'rpk' in args:
                return '5368709120'
            return 'Filesystem 1024-blocks Used Available Capacity Mounted on\nx 20000000 1000000 19000000 5% /var/lib/redpanda/data'
        result = qualify.broker_storage(command)
        self.assertEqual(result['producerReserveBytes'], 5 << 30)
        self.assertIn('admin.hosts=127.0.0.1:9644', calls[0])
        self.assertIn('get', calls[0])
        self.assertNotIn('set', calls[0])
        self.assertEqual(calls[1][-3:], ('df', '-Pk', '/var/lib/redpanda/data'))
        self.assertTrue(all(type(x) is int for x in result.values()))

    def test_hosted_only(self):
        env = {'GITHUB_ACTIONS': 'true', 'RUNNER_ENVIRONMENT': 'github-hosted',
               'RUNNER_OS': 'Linux', 'ImageOS': 'ubuntu26'}
        prepare_runner.hosted(env)
        for key in env:
            bad = dict(env, **{key: 'other'})
            with self.assertRaises(ValueError):
                prepare_runner.hosted(bad)

    def test_sdk_identity_and_symlink_refusal(self):
        with self.assertRaises(ValueError):
            prepare_runner.verify_sdk({'ANDROID_HOME': '/tmp/other'})
        # No host SDK path is created or removed by tests.
        with tempfile.TemporaryDirectory() as scratch:
            root = Path(scratch).resolve()
            regular = root / 'directory'; regular.mkdir()
            prepare_runner.regular_tree_root(regular)
            file = root / 'file'; file.write_text('fixture')
            with self.assertRaises(ValueError):
                prepare_runner.regular_tree_root(file)
            if os.name != 'nt':
                link = root / 'link'; link.symlink_to(regular, target_is_directory=True)
                with self.assertRaises(ValueError):
                    prepare_runner.regular_tree_root(link)

    def test_capacity_gate_source_and_workflow_mutations(self):
        root = qualify.ROOT
        source = (root / 'tools/redpanda-identity/qualify.py').read_text(encoding='utf-8')
        workflow = (root / '.github/workflows/redpanda-identity-proof.yml').read_text(encoding='utf-8')
        def boundary(src, wf):
            required = "require_capacity(storage, storage['producerReserveBytes'])"
            if required not in src or src.index(required) > src.index('apply([job(case,sa,mode,image)])'):
                raise ValueError('capacity must precede each broker probe')
            prep = 'storage = preflight_storage(k, evidence, case)'
            if prep not in src or src.index(prep) > src.index('apply([job(case,sa,mode,image)])'):
                raise ValueError('actual capacity preparation absent')
        boundary(source, workflow)
        with self.assertRaises(ValueError):
            boundary(source.replace("require_capacity(storage, storage['producerReserveBytes'])", 'pass'), workflow)
        with self.assertRaises(ValueError):
            boundary(source.replace('storage = preflight_storage(k, evidence, case)', 'storage = {}'), workflow)
        docs = qualify.broker_objects()
        cm = next(x for x in docs if x['kind'] == 'ConfigMap' and x['metadata']['name'] == 'redpanda-config')
        self.assertNotIn('storage_min_free_bytes', str(cm))

    def test_fixed_sdk_removal_and_checked_readback(self):
        # All host effects are mocked. No SDK or Docker state is changed here.
        calls = []
        def run(args, **kwargs):
            calls.append(args)
            self.assertTrue(kwargs['check'])
            self.assertLessEqual(kwargs['timeout'], 190)
            if args[0] == 'docker':
                return SimpleNamespace(stdout=str(prepare_runner.DOCKER) + '\n')
            if args[0] == 'findmnt':
                return SimpleNamespace(stdout='{"filesystems":[{"target":"/"}]}')
            return SimpleNamespace(stdout='')
        with patch.object(prepare_runner, 'hosted'), patch.object(prepare_runner, 'regular_tree_root'), \
             patch.object(prepare_runner, 'verify_sdk') as verified, \
             patch.object(prepare_runner.subprocess, 'run', side_effect=run), \
             patch.object(prepare_runner.shutil, 'disk_usage', side_effect=[SimpleNamespace(free=1), SimpleNamespace(free=(8 << 30))]), \
             patch.object(prepare_runner.os.path, 'lexists', return_value=False), patch('builtins.print'):
            prepare_runner.main(7 << 30)
        verified.assert_called_once()
        self.assertEqual(calls[-1], ['sudo', 'timeout', '180', 'rm', '-rf', '--one-file-system',
                                    '--preserve-root=all', '--', str(prepare_runner.SDK)])

    def test_nested_mount_and_incomplete_removal_refuse(self):
        for nested, remains in ((True, False), (False, True)):
            with self.subTest(nested=nested, remains=remains):
                calls = []
                def run(args, **kwargs):
                    calls.append(args)
                    if args[0] == 'docker': return SimpleNamespace(stdout=str(prepare_runner.DOCKER))
                    if args[0] == 'findmnt':
                        target = str(prepare_runner.SDK) + '/nested' if nested else '/'
                        return SimpleNamespace(stdout=json.dumps({'filesystems': [{'target': target}]}))
                    return SimpleNamespace(stdout='')
                with patch.object(prepare_runner, 'hosted'), patch.object(prepare_runner, 'regular_tree_root'), \
                     patch.object(prepare_runner, 'verify_sdk'), \
                     patch.object(prepare_runner.subprocess, 'run', side_effect=run), \
                     patch.object(prepare_runner.shutil, 'disk_usage', return_value=SimpleNamespace(free=1)), \
                     patch.object(prepare_runner.os.path, 'lexists', return_value=remains):
                    with self.assertRaises(ValueError): prepare_runner.main(7 << 30)
                if nested: self.assertFalse(any(args[0] == 'sudo' for args in calls))


if __name__ == '__main__':
    unittest.main()
