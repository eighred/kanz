import copy
import base64
import hashlib
import io
import tarfile
import unittest
import proof


def workflow_boundary(workflow):
    if workflow['permissions'] != {'contents': 'read'}:
        raise ValueError('read-only workflow required')
    job = workflow['jobs']['messaging-isolation']
    if job.get('if') or job.get('continue-on-error') or job['timeout-minutes'] != 30:
        raise ValueError('mandatory bounded job required')
    step = next(s for s in job['steps'] if s.get('name') == 'Prove staged messaging isolation with real dependencies')
    if step.get('if') or step.get('continue-on-error') or step.get('run') != 'python3 -B tools/tokyo-messaging-proof/proof.py':
        raise ValueError('actual fixture cannot be skipped or substituted')


class MessagingTests(unittest.TestCase):
    def test_restore_reuses_canonical_isolated_recipe(self):
        pod = proof.restore_pod()
        self.assertEqual(pod['metadata'], {'name': 'nats-restore', 'namespace': 'kanz-recovery-drill', 'labels': {'app': 'nats-restore'}})
        self.assertFalse(pod['spec']['automountServiceAccountToken'])
        self.assertEqual([c['image'] for c in pod['spec']['containers']], [proof.NATS, proof.BOX])
        self.assertIn('--addr=127.0.0.1', pod['spec']['containers'][0]['args'])
        self.assertTrue(all('emptyDir' in volume for volume in pod['spec']['volumes']))
        self.assertFalse(any('secret' in volume or 'csi' in volume for volume in pod['spec']['volumes']))

    def test_backup_archive_requires_bounded_regular_owned_paths(self):
        def archive(name='fixture-backup/TEST/backup.json', kind=tarfile.REGTYPE, size=2):
            buf = io.BytesIO()
            with tarfile.open(fileobj=buf, mode='w:gz') as tar:
                item = tarfile.TarInfo(name)
                item.type = kind
                item.linkname = 'outside'
                item.size = size if kind == tarfile.REGTYPE else 0
                tar.addfile(item, io.BytesIO(b'{}' + b' ' * max(0, size - 2)))
            return base64.b64encode(buf.getvalue()).decode()
        encoded = archive()
        self.assertEqual(proof.checked_backup(encoded), encoded)
        for encoded in (archive('../escape'), archive('/absolute'), archive('fixture-backup/../escape'),
                        archive('fixture-backup//TEST/backup.json'), archive(kind=tarfile.SYMTYPE),
                        archive(kind=tarfile.LNKTYPE), archive(size=(8 << 20) + 1), 'invalid!'):
            with self.assertRaises((ValueError, proof.base64.binascii.Error)):
                proof.checked_backup(encoded)

    def test_v_prefixed_tool_versions(self):
        for output in ('nats-server: v2.12.6', 'v2.12.6', '2.12.6'):
            self.assertEqual(proof.tool_version(output), '2.12.6')
        for output in ('unknown', '1.2.3 and 4.5.6', 'x' * 128 + '1.2.3'):
            with self.assertRaises(ValueError):
                proof.tool_version(output)

    def test_diagnostics_are_closed_and_do_not_export_raw_values(self):
        self.assertEqual(proof.nats_failure('', 'Permissions Violation for Publish to "$JS.SNAPSHOT.RESTORE.FIXTURE.random"'),
                         'restore_upload_permission_denied')
        self.assertEqual(proof.nats_failure('', 'nats: Authorization Violation'), 'principal_authorization_denied')
        self.assertEqual(proof.nats_failure('', 'Permissions Violation for Publish to "other"'), 'other_subject_permission_denied')
        self.assertEqual(proof.nats_failure('private unknown diagnostic', ''), 'unclassified_cli_failure')
        self.assertEqual(proof.nats_failure('', 'x' * 32768 + 'Permissions Violation $JS.SNAPSHOT.RESTORE.X'), 'unclassified_cli_failure')

    def test_mandatory_read_only_workflow(self):
        workflow = proof.yaml.safe_load((proof.ROOT / '.github/workflows/tokyo-messaging-proof.yml').read_text())
        workflow_boundary(workflow)
        for mutation in ('write', 'skip-job', 'skip-step', 'ignore-error', 'substitute'):
            changed = copy.deepcopy(workflow)
            job = changed['jobs']['messaging-isolation']
            step = next(s for s in job['steps'] if s.get('name') == 'Prove staged messaging isolation with real dependencies')
            if mutation == 'write':
                changed['permissions']['contents'] = 'write'
            elif mutation == 'skip-job':
                job['if'] = 'false'
            elif mutation == 'skip-step':
                step['if'] = 'false'
            elif mutation == 'ignore-error':
                step['continue-on-error'] = True
            else:
                step['run'] = 'echo success'
            with self.assertRaises(ValueError):
                workflow_boundary(changed)

    def test_default_deny_and_exact_ports(self):
        policies = proof.staged_policies()
        self.assertEqual(len(policies), 4)
        deny = policies[0]['spec']
        self.assertEqual(deny, {'podSelector': {}, 'policyTypes': ['Ingress', 'Egress']})
        ports = {port['port'] for p in policies for direction in ('ingress', 'egress')
                 for rule in p['spec'].get(direction, []) for port in rule['ports']}
        self.assertEqual(ports, {53, 4222, 6379})
        for policy in policies:
            self.assertEqual(policy['metadata']['namespace'], 'kanz-messaging')

    def test_namespace_and_app_are_conjunctive(self):
        policies = proof.staged_policies()
        nats = policies[1]['spec']['ingress'][0]['from']
        self.assertEqual(nats[0]['namespaceSelector']['matchLabels'], {'kubernetes.io/metadata.name': 'kanz-services'})
        self.assertEqual(nats[0]['podSelector']['matchExpressions'][0]['operator'], 'In')
        self.assertEqual(nats[1], {'podSelector': {'matchLabels': {'app': 'nats-bootstrap'}}})
        redis = policies[2]['spec']['ingress'][0]['from']
        self.assertEqual(redis, [{'namespaceSelector': {'matchLabels': {'kubernetes.io/metadata.name': 'kanz-services'}},
                                 'podSelector': {'matchLabels': {'app': 'api-gateway'}}}])

    def test_canonical_identity_and_singleton(self):
        objects = proof.broker_objects()
        config = next(o for o in objects if o['kind'] == 'ConfigMap' and o['metadata']['name'] == 'nats-config')['data']['nats.conf']
        self.assertIn('verify: true', config)
        self.assertIn('verify_and_map: true', config)
        self.assertNotIn('cluster {', config)
        actual = next(o for o in objects if o['metadata']['name'] == 'nats-tenants')
        self.assertEqual(actual, proof.docs('kanz/infra/nats/tenancy.yaml')[0])

    def test_bootstrap_and_recovery_clone_preserve_label_and_identity(self):
        job = next(o for o in proof.bootstrap_objects() if o['kind'] == 'Job')
        self.assertEqual(job['metadata']['labels']['app'], 'nats-bootstrap')
        self.assertEqual(job['spec']['template']['metadata']['labels']['app'], 'nats-bootstrap')
        runner = proof.runner_from_job(job)
        self.assertEqual(runner['metadata']['labels']['app'], 'nats-bootstrap')
        self.assertEqual(runner['spec']['serviceAccountName'], 'nats-bootstrap')
        self.assertEqual(runner['spec']['initContainers'], job['spec']['template']['spec']['initContainers'])
        self.assertEqual(runner['spec']['containers'][0]['env'], job['spec']['template']['spec']['containers'][0]['env'])
        broken = copy.deepcopy(job)
        broken['metadata'].pop('labels')
        self.assertNotIn('labels', proof.runner_from_job(broken)['metadata'])

    def test_tcp_refusal_is_not_timeout_proof(self):
        self.assertIn('except TimeoutError:', proof.TCP)
        self.assertNotIn('except OSError', proof.TCP)

    def test_incomplete_or_wrong_revision_refuses(self):
        for report in ({}, {'passed': True, 'cleanupPassed': True, 'developmentOnly': True, 'sourceSHA': 'wrong', 'cases': []}):
            with self.assertRaises(ValueError):
                proof.validate_report(report, 'reviewed')

    def test_complete_report_and_security_mutations(self):
        source = 'a' * 40
        report = {'sourceSHA': source, 'passed': True, 'cleanupPassed': True, 'developmentOnly': True,
                  'restoreMode': 'canonical-isolated-loopback',
                  'policyCount': 4,
                  'policyNames': sorted(p['metadata']['name'] for p in proof.staged_policies()),
                  'policySHA256': hashlib.sha256((proof.HERE / 'policies.yaml').read_bytes()).hexdigest(),
                  'cases': ['baseline-unlabelled', 'baseline-redis', 'baseline-monitor', 'trap-control', 'bootstrap', 'nats-publish',
                            'ungranted-identity', 'backup-restore', 'redis-claim', 'redis-restart', 'unlabelled-nats', 'unlabelled-redis',
                            'wrong-namespace-nats', 'wrong-namespace-redis', 'prometheus-nats', 'prometheus-redis',
                            'prometheus-monitor', 'prometheus-scrape', 'bootstrap-redis', 'bootstrap-monitor',
                            'bootstrap-egress']}
        proof.validate_report(report, source)
        for field, value in [('sourceSHA', 'b' * 40), ('passed', False), ('cleanupPassed', False),
                             ('developmentOnly', False), ('policyCount', 5), ('policySHA256', '0' * 64),
                             ('restoreMode', 'same-account'),
                             ('policyNames', report['policyNames'] + ['unreviewed']),
                             ('cases', report['cases'] + ['bootstrap'])]:
            with self.subTest(field=field):
                changed = copy.deepcopy(report)
                changed[field] = value
                with self.assertRaises(ValueError):
                    proof.validate_report(changed, source)
        for case in report['cases']:
            changed = copy.deepcopy(report)
            changed['cases'].remove(case)
            with self.assertRaises(ValueError):
                proof.validate_report(changed, source)


if __name__ == '__main__':
    unittest.main()
