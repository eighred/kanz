import hashlib
import json
import copy
import re
from pathlib import Path
import unittest
from unittest.mock import Mock
import yaml
import qualify


def boundary(docs, probe, workflow):
    cm=next(d for d in docs if d['kind']=='ConfigMap' and d['metadata']['name']=='redpanda-config')
    config=yaml.safe_load(cm['data']['redpanda.yaml'])['redpanda']
    bootstrap=yaml.safe_load(cm['data']['.bootstrap.yaml'])
    if config['kafka_api'] != [{'address':'0.0.0.0','port':9094,'name':'identity','authentication_method':'mtls_identity'}]:
        raise ValueError('only mTLS Kafka listener allowed')
    tls=config['kafka_api_tls']
    if len(tls)!=1 or tls[0]['require_client_auth'] is not True or tls[0]['enabled'] is not True:
        raise ValueError('client authentication required')
    if config['admin'] != [{'address':'127.0.0.1','port':9644}]:
        raise ValueError('admin must remain loopback only')
    expected=['RULE:^'+qualify.broker_subject_pattern(sa)+'$/'+sa+'/' for sa in qualify.SAS]
    if bootstrap['kafka_mtls_principal_mapping_rules'] != expected or len(set(expected))!=4:
        raise ValueError('exact unique workload subjects required')
    if bootstrap['kafka_enable_authorization'] is not True or bootstrap['superusers']!=['proof-bootstrap']:
        raise ValueError('only dedicated bootstrap has admin authority')
    for required in ('tlsconfig.AuthorizeID(', 'kafka.RequireAll', 'kafka.TopicAuthorizationFailed', 'kafka.ClusterAuthorizationFailed',
                     'SerialNumber.Cmp(', 'principal changed on renewal', 'Transport: renewedTransport', 'len(values) != 2', '"FORBIDDEN"', 'kafka.NewConn(raw, topic, 0)', 'directTopic(ctx, transport.TLS, sa, false, "before-renewal")', 'boundedReplay(ctx, transport.TLS', 'boundedReplay(ctx, renewedTransport.TLS', '!peerDenied.Load()', '"boundedReplay": mode == "publish"'):
        if required not in probe: raise ValueError('missing real identity/ACL/renewal proof')
    wf=yaml.safe_load(workflow)
    if wf['permissions']!={'contents':'read','packages':'read'} or 'continue-on-error' in workflow:
        raise ValueError('workflow authority or refusal changed')
    if 'python3 -B tools/redpanda_identity/qualify.py' not in workflow:
        raise ValueError('actual mandatory fixture missing')
    setup=[s for s in wf['jobs']['developmental-redpanda-identity']['steps'] if s.get('uses')=='./.github/actions/setup_buf']
    if len(setup)!=1 or setup[0].get('with',{}).get('version')!='1.69.0':
        raise ValueError('SDK generator must have its required pinned version')
    for token in ('protoc-gen-go@v1.36.11','protoc-gen-go-grpc@v1.5.1','$(go env GOPATH)/bin:$PATH','make -C kanz generate'):
        if token not in workflow: raise ValueError('canonical generator dependency missing')


class QualificationTests(unittest.TestCase):
    def setUp(self):
        self.docs=qualify.broker_objects(cold_recovery=True)
        self.probe=(qualify.ROOT/'kanz/tools/redpandaidentity/main.go').read_text()
        self.workflow=(qualify.ROOT/'.github/workflows/redpanda-identity-proof.yml').read_text()

    def test_boundary(self): boundary(self.docs,self.probe,self.workflow)

    def test_cold_recovery_mount_belongs_to_single_broker(self):
        for docs, cold in ((self.docs, True), (qualify.broker_objects(), False)):
            with self.subTest(cold_recovery=cold):
                pod = next(d for d in docs if d['kind'] == 'Deployment')['spec']['template']['spec']
                volume = next(v for v in pod['volumes'] if v['name'] == 'data')
                config = next(d for d in docs if d['metadata']['name'] == 'redpanda-config')
                directory = yaml.safe_load(config['data']['redpanda.yaml'])['redpanda']['data_directory']
                claims = [d['metadata']['name'] for d in docs if d['kind'] == 'PersistentVolumeClaim']
                self.assertEqual('data-init' in [c['name'] for c in pod['initContainers']], cold)
                if cold:
                    self.assertEqual(claims, [qualify.cold_restore.SOURCE])
                    self.assertEqual(volume, {'name': 'data', 'persistentVolumeClaim': {'claimName': claims[0]}})
                    self.assertEqual(directory, '/var/lib/redpanda/data/state')
                else:
                    self.assertEqual(claims, [])
                    self.assertEqual(volume, {'name': 'data', 'emptyDir': {'sizeLimit': '2Gi'}})
                    self.assertEqual(directory, '/var/lib/redpanda/data')

    def test_config_mutations(self):
        for field,value in [('kafka_enable_authorization',False),('superusers',['proof-bootstrap','proof-a']),('kafka_mtls_principal_mapping_rules',['DEFAULT'])]:
            docs=copy.deepcopy(self.docs)
            cm=next(d for d in docs if d['kind']=='ConfigMap' and d['metadata']['name']=='redpanda-config')
            b=yaml.safe_load(cm['data']['.bootstrap.yaml']); b[field]=value; cm['data']['.bootstrap.yaml']=yaml.safe_dump(b)
            with self.assertRaises(ValueError): boundary(docs,self.probe,self.workflow)
        for field,value in [('kafka_api',[{'address':'0.0.0.0','port':9092,'authentication_method':'none'}]),('admin',[{'address':'0.0.0.0','port':9644}])]:
            docs=copy.deepcopy(self.docs); cm=next(d for d in docs if d['kind']=='ConfigMap' and d['metadata']['name']=='redpanda-config')
            b=yaml.safe_load(cm['data']['redpanda.yaml']); b['redpanda'][field]=value; cm['data']['redpanda.yaml']=yaml.safe_dump(b)
            with self.assertRaises(ValueError): boundary(docs,self.probe,self.workflow)

    def test_source_mutations(self):
        for token in ('tlsconfig.AuthorizeID(', 'kafka.TopicAuthorizationFailed', 'SerialNumber.Cmp(', 'Transport: renewedTransport', 'len(values) != 2', 'kafka.NewConn(raw, topic, 0)', 'directTopic(ctx, transport.TLS, sa, false, "before-renewal")', 'boundedReplay(ctx, transport.TLS', 'boundedReplay(ctx, renewedTransport.TLS', '!peerDenied.Load()', '"boundedReplay": mode == "publish"'):
            with self.assertRaises(ValueError): boundary(self.docs,self.probe.replace(token,'REMOVED'),self.workflow)
        with self.assertRaises(ValueError): boundary(self.docs,self.probe,self.workflow.replace('packages: read','packages: write'))
        with self.assertRaises(ValueError): boundary(self.docs,self.probe,self.workflow.replace('python3 -B tools/redpanda_identity/qualify.py','true'))
        with self.assertRaises(ValueError): boundary(self.docs,self.probe,self.workflow.replace("version: '1.69.0'", "version: ''"))
        for token in ('protoc-gen-go@v1.36.11','protoc-gen-go-grpc@v1.5.1','make -C kanz generate'):
            with self.assertRaises(ValueError): boundary(self.docs,self.probe,self.workflow.replace(token,'REMOVED'))

    def test_composer_is_fixture_only(self):
        docs=qualify.spire_objects()
        cm=next(d for d in docs if d['kind']=='ConfigMap' and d['metadata']['name']=='spire-server')
        self.assertEqual(cm['data']['server.conf'].count('CredentialComposer "uniqueid" {}'),1)
        self.assertNotIn('CredentialComposer',(qualify.ROOT/'kanz/infra/security/spire/spire-server.yaml').read_text())

    def test_no_workload_credentials_or_host_mounts(self):
        d=qualify.job('test','proof-a','publish','fixture:local')
        pod=d['spec']['template']['spec']
        self.assertFalse(pod['automountServiceAccountToken'])
        self.assertEqual(d['spec']['activeDeadlineSeconds'],180)
        self.assertTrue(pod['containers'][0]['securityContext']['readOnlyRootFilesystem'])
        self.assertFalse(any('hostPath' in v for v in pod['volumes']))
        self.assertEqual({e['name'] for e in pod['containers'][0]['env']},{'PROOF_SA','PROOF_MODE'})

    def test_broker_diagnostics_only_known_public_subjects(self):
        dn = 'C=US,O=SPIRE,CN=bootstrap-abcde,' + qualify.subject('proof-bootstrap')
        marker = 'failed to extract principal from distinguished name: '
        output = qualify.broker_diagnostics('INFO ' + marker + dn)
        self.assertEqual(output['unmappedSubjects'], [dn])
        for bad in (dn + ',secret=private', dn.replace('C=US', 'C=private'),
                    dn.replace('bootstrap-abcde', '/private/key'), 'private arbitrary text'):
            self.assertEqual(qualify.broker_diagnostics(marker + bad)['unmappedSubjects'], [])
        result = qualify.broker_diagnostics('Ignoring invalid property: superusers=private arbitrary text')
        self.assertEqual(result['invalidProperties'], ['superusers'])
        self.assertNotIn('private', str(result))

    def test_storage_diagnostics_are_numeric_and_do_not_leak_logs(self):
        logs = 'storage space alert: private mount\nNo space left on device: private'
        result = qualify.broker_diagnostics(logs)
        self.assertTrue(result['storageSpaceAlert'])
        self.assertTrue(result['noSpaceLeftOnDevice'])
        self.assertNotIn('private', str(result))
        self.assertFalse(qualify.broker_diagnostics('ordinary log')['storageSpaceAlert'])
        row = 'Filesystem 1024-blocks Used Available Capacity Mounted on\nprivate 100 60 40 60% /private'
        self.assertEqual(qualify.storage_capacity(row),
                         {'totalBytes':102400, 'usedBytes':61440, 'availableBytes':40960})
        for bad in ('private', row + '\nextra', row.replace('100', '-1'),
                    row.replace('100', '10'), row.replace('60 40', 'secret 40'), 'x'*4097):
            with self.assertRaises(ValueError):
                qualify.storage_capacity(bad)

    def test_exact_dn_serializations(self):
        self.assertIsNotNone(re.fullmatch(qualify.broker_subject_pattern('proof-bootstrap'),
            'C=US,O=SPIRE,CN=bootstrap-nfkrt,x500UniqueIdentifier=5038771c76e00e553a0eee62370950a6'))
        uid = qualify.subject('proof-a')
        for pattern, value in (
            (qualify.subject_pattern('proof-a'), 'CN=client-a-abcde,O=SPIRE,C=US,' + uid),
            (qualify.broker_subject_pattern('proof-a'), 'C=US,O=SPIRE,CN=client-a-abcde,x500UniqueIdentifier=' + hashlib.sha256(qualify.identity('proof-a').encode()).digest()[:16].hex()),
        ):
            self.assertIsNotNone(re.fullmatch(pattern, value))
            for bad in (value + ',OU=extra', value + ',' + uid,
                        value.replace(uid, qualify.subject('proof-b')) if uid in value else value[:-1] + ('0' if value[-1] != '0' else '1'),
                        value.replace('C=US', 'C=GB'), value.replace('client-a-abcde', 'a,OU=x'),
                        value.replace('client-a-abcde', 'a' * 254)):
                self.assertIsNone(re.fullmatch(pattern, bad))

    def test_records_are_verified_on_both_sides_of_one_broker_restart(self):
        self.assertEqual(qualify.CASES[4:], (
            ('verify', 'proof-bootstrap', 'verify'),
            ('reprovision', 'proof-bootstrap', 'reprovision'),
            ('verify-reprovision', 'proof-bootstrap', 'verify'),
            ('checkpoint', 'proof-a', 'checkpoint'),
            ('verify-after-restart', 'proof-bootstrap', 'verify'),
            ('resume', 'proof-a', 'resume'),
            ('verify-after-restore', 'proof-bootstrap', 'verify'),
            ('restored-offset', 'proof-a', 'restored-offset'),
            ('restored-denial', 'proof-ungranted', 'ungranted')))
        uid = '00000000-0000-0000-0000-000000000001'
        old, new = ('containerd://' + v * 64 for v in ('a', 'b'))
        meta = {'uid': uid, 'name': 'redpanda-aaa-bbb'}
        first = {'name': 'redpanda', 'containerID': old, 'restartCount': 0,
                 'ready': True, 'state': {'running': {}}}
        second = dict(first, containerID=new, restartCount=1,
                      lastState={'terminated': {'containerID': old, 'exitCode': 0}})

        def pod(meta, state):
            return {'metadata': meta, 'status': {'containerStatuses': [state]},
                    'spec': {'serviceAccountName': 'redpanda', 'containers': [{'name': 'redpanda',
                              'image': qualify.REDPANDA, 'command': ['/opt/redpanda/bin/redpanda']}]}}

        def fixture(metadata, state):
            return Mock(side_effect=[json.dumps({'items': [pod(meta, first)]}), '', '', '',
                                     json.dumps(pod(metadata, state))])

        k = fixture(meta, second)
        result = qualify.restart_broker(k)
        self.assertEqual(result['after']['previousContainerID'], result['before']['containerID'])
        self.assertEqual([call.args[2] for call in k.call_args_list], ['get', 'exec', 'wait', 'wait', 'get'])
        self.assertIn('pod/redpanda-aaa-bbb', k.call_args_list[1].args)
        self.assertIn('redpanda', k.call_args_list[1].args)
        for label, metadata, state in (
                ('replacement pod', dict(meta, uid=uid[:-1]+'2'), second),
                ('old container', meta, dict(second, containerID=old)),
                ('not ready', meta, dict(second, ready=False)),
                ('second restart', meta, dict(second, restartCount=2)),
                ('unrelated exit', meta, dict(second, lastState={'terminated': {'containerID': new, 'exitCode': 0}})),
                ('failed exit', meta, dict(second, lastState={'terminated': {'containerID': old, 'exitCode': 137}}))):
            with self.subTest(label=label), self.assertRaises(ValueError):
                qualify.restart_broker(fixture(metadata, state))
        preexisting = Mock(return_value=json.dumps({'items': [pod(meta, second)]}))
        with self.assertRaises(ValueError):
            qualify.restart_broker(preexisting)
        preexisting.assert_called_once()
        for field in ('hostPID', 'shareProcessNamespace'):
            wrong = pod(meta, first)
            wrong['spec'][field] = True
            client = Mock(return_value=json.dumps({'items': [wrong]}))
            with self.subTest(field=field), self.assertRaises(ValueError):
                qualify.restart_broker(client)
            client.assert_called_once()

    def test_exact_subjects_are_distinct_and_bounded(self):
        values=[qualify.subject(sa) for sa in qualify.SAS]
        self.assertEqual(len(set(values)),4)
        for v in values: self.assertRegex(v,r'^2\.5\.4\.45=#1320[0-9a-f]{64}$')
        with self.assertRaises(ValueError): qualify.subject('other')


if __name__=='__main__': unittest.main()
