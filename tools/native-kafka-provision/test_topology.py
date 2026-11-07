import hashlib
import unittest
import yaml
import topology
import qualify
import owned_cleanup as cleanup
import copy
import json


class TopologyTest(unittest.TestCase):
    def test_admin_failure_projects_only_closed_category_and_counters(self):
        receipt=dict(expected_cluster_id='SECRET',mode='SECRET',manifest_sha256='SECRET',
                     verified_topics=0,missing_topics=60,verified_acls=0,topic_attempts=1,
                     acl_attempts=0,partial_or_unknown=True,complete=False)
        value={'version':'a'*40,'receipt':receipt,'error':'broker-unavailable'}
        result=qualify.admin_diagnostic(json.dumps(value),'a'*40)
        self.assertEqual(result,dict(category='broker-unavailable',verified_topics=0,missing_topics=60,
                                    verified_acls=0,topic_attempts=1,acl_attempts=0,partial_or_unknown=True,complete=False))
        self.assertNotIn('SECRET',json.dumps(result))
        coded=dict(value,failure_code='kafka-31')
        self.assertEqual(qualify.admin_diagnostic(json.dumps(coded),'a'*40)['failure_code'],'kafka-31')
        for code in ('SECRET','kafka-128','kafka--2',True):
            with self.assertRaises(ValueError):qualify.admin_diagnostic(json.dumps(dict(value,failure_code=code)),'a'*40)
        for mutate in (lambda x:x.update(error='SECRET'),lambda x:x.update(version='b'*40),
                       lambda x:x['receipt'].update(topic_attempts=True),lambda x:x['receipt'].update(topic_attempts=257),
                       lambda x:x['receipt'].update(complete='SECRET'),lambda x:x['receipt'].update(extra='SECRET')):
            bad=copy.deepcopy(value);mutate(bad)
            with self.assertRaises(ValueError):qualify.admin_diagnostic(json.dumps(bad),'a'*40)
        with self.assertRaises(ValueError):qualify.admin_diagnostic(json.dumps(value)[:-1]+',"error":"broker-unavailable"}','a'*40)

    def test_cluster_grammar_preserves_exact_initialized_pin(self):
        for value in ('cluster-a','Ab_09-x','redpanda.12345678-1234-1234-1234-123456789abc','redpanda.safe_custom-1'):
            self.assertTrue(qualify.valid_cluster_id(value))
        for value in ('','redpanda.','redpanda.initializing','redpanda.unknown','redpanda.x.y','other.cluster','redpanda.x\n','redpanda.$(id)','x'*129):
            self.assertFalse(qualify.valid_cluster_id(value))
    def test_source_identity_before_effects(self):
        qualify.source_identity('a'*40,'b'*40,'1','2','a'*40)
        for source,run,attempt,expected in [('a'*40,'1','1',''),('a'*40,'1','1','c'*40),('a'*40,'0','1','a'*40),('a'*40,'1','0','a'*40),('a'*40,'١','1','a'*40),('a'*40,'+1','1','a'*40)]:
            with self.assertRaises(ValueError): qualify.source_identity(source,'b'*40,run,attempt,expected)
        import inspect
        body=inspect.getsource(qualify.main)
        self.assertLess(body.index('source_identity('),body.index('TemporaryDirectory('))
        self.assertIn("os.environ.get('GITHUB_SHA', '')",body)

    def test_owned_job_and_pod_projection(self):
        wanted=qualify.job('discover','kanz-messaging','kafka-provisioner','fixture:source',mode='discover')
        uid='12345678-1234-1234-1234-123456789abc'
        actual=copy.deepcopy(wanted);actual['metadata']['uid']=uid
        self.assertEqual(qualify.job_custody(actual,wanted),uid)
        for mutate in (lambda x:x['metadata'].update(uid='other'),lambda x:x['metadata'].update(name='foreign'),lambda x:x['spec']['template']['spec'].update(serviceAccountName='other'),lambda x:x['spec']['template']['spec']['containers'][0].update(image='other')):
            bad=copy.deepcopy(actual);mutate(bad)
            with self.assertRaises(ValueError):qualify.job_custody(bad,wanted)
        digest='sha256:'+'a'*64
        pod={'metadata':{'uid':'87654321-1234-1234-1234-123456789abc','namespace':'kanz-messaging','name':'discover-owned','ownerReferences':[{'uid':uid,'name':'discover','kind':'Job','controller':True}]},'spec':copy.deepcopy(wanted['spec']['template']['spec']),
             'status':{'containerStatuses':[{'name':'probe','imageID':digest,'state':{'terminated':{'exitCode':1,'reason':'Error','message':'SECRET'}}}]}}
        result=qualify.pod_diagnostic([pod],wanted,uid)
        self.assertEqual(result,{'state':'terminated','exitCode':1,'reason':'Error','imageBound':False})
        self.assertNotIn('SECRET',json.dumps(result))
        replaced=copy.deepcopy(pod);replaced['metadata']['uid']=uid
        with self.assertRaises(ValueError):qualify.pod_custody([replaced],wanted,uid,pod['metadata']['uid'])
        for mutate in (lambda x:x['metadata']['ownerReferences'][0].update(uid='other'),lambda x:x['spec'].update(serviceAccountName='other')):
            bad=copy.deepcopy(pod);mutate(bad)
            with self.assertRaises(ValueError):qualify.pod_diagnostic([bad],wanted,uid)
        for key,replacement in [('args',['foreign']),('command',['foreign']),('env',[]),('name','other')]:
            bad=copy.deepcopy(pod);bad['spec']['containers'][0][key]=replacement
            with self.assertRaises(ValueError):qualify.pod_diagnostic([bad],wanted,uid)
        for key,replacement in [('uid',''),('uid','malformed'),('name','../foreign')]:
            bad=copy.deepcopy(pod);bad['metadata'][key]=replacement
            with self.assertRaises(ValueError):qualify.pod_diagnostic([bad],wanted,uid)

    def test_closed_probe_diagnostic_never_copies_error(self):
        value={'version':'a'*40,'mode':'discover','expected_identity':topology.ADMIN_ID,'phase':'metadata','category':'broker'}
        raw='NATIVE_DIAGNOSTIC:'+json.dumps(value)
        self.assertEqual(qualify.probe_diagnostic(raw,'discover','a'*40),value)
        self.assertEqual(qualify.probe_diagnostic('SECRET','discover','a'*40),{'state':'UNKNOWN'})
        for key,replacement in [('version','b'*40),('expected_identity',topology.IDENTITIES[1]),('phase','SECRET'),('category','SECRET')]:
            bad=dict(value);bad[key]=replacement
            with self.assertRaises(ValueError):qualify.probe_diagnostic('NATIVE_DIAGNOSTIC:'+json.dumps(bad),'discover','a'*40)
        with self.assertRaises(ValueError):qualify.probe_diagnostic(raw[:-1]+',"category":"broker"}','discover','a'*40)
    def test_three_distinct_real_brokers_and_full_principals(self):
        docs = topology.broker_objects()
        deployments = [d for d in docs if d['kind'] == 'Deployment']
        self.assertEqual(len(deployments), 3)
        self.assertEqual({d['metadata']['name'] for d in deployments}, set(topology.BROKERS))
        for d in deployments:
            spec = d['spec']['template']['spec']
            self.assertEqual(spec['serviceAccountName'], 'redpanda')
            self.assertEqual(next(c for c in spec['containers'] if c['name'] == 'redpanda')['image'], topology.retained.REDPANDA)
            self.assertEqual(next(v for v in spec['volumes'] if v['name'] == 'data'), {'name':'data','emptyDir':{'sizeLimit':'2Gi'}})
        configs = [d for d in docs if d['kind'] == 'ConfigMap' and d['metadata']['name'] in topology.BROKERS]
        self.assertEqual(len(configs), 3)
        for i, cm in enumerate(configs):
            config = yaml.safe_load(cm['data']['redpanda.yaml'])['redpanda']
            self.assertEqual(config['node_id'], i)
            self.assertEqual(len(config['seed_servers']), 3)
            self.assertEqual(config['advertised_rpc_api']['address'], topology.BROKERS[i] + '.kanz-messaging.svc')
            self.assertTrue(config['kafka_api_tls'][0]['require_client_auth'])
            bootstrap = yaml.safe_load(cm['data']['.bootstrap.yaml'])
            self.assertEqual(bootstrap['superusers'], [r'spiffe:\/\/kanz.internal\/ns\/kanz-messaging\/sa\/kafka-provisioner'])
            self.assertEqual(bootstrap['default_topic_replications'], 3)
            self.assertFalse(bootstrap['auto_create_topics_enabled'])
            self.assertEqual(bootstrap['kafka_mtls_principal_mapping_rules'], [topology.mapping(x) for x in topology.IDENTITIES])

    def test_mapping_is_full_uri_and_uniqueid_not_common_name_authority(self):
        for identity in topology.IDENTITIES:
            rule = topology.mapping(identity)
            self.assertTrue(rule.endswith('$/' + identity.replace('/', r'\/') + '/'))
            self.assertIn('x500UniqueIdentifier=' + hashlib.sha256(identity.encode()).digest()[:16].hex(), rule)
            self.assertNotIn('/proof-', rule)
        with self.assertRaises(ValueError):
            topology.mapping('spiffe://kanz.internal/ns/tenant-acme/sa/other')

    def test_closed_probe_rejects_identity_cluster_and_renewal_mutations(self):
        expected = {'version':'a'*40,'mode':'tenant-a','cluster_id':'real-cluster',
                    'identity':topology.IDENTITIES[1],'unique_id':True,'passed':True,'renewed':True}
        text = 'NATIVE_PROOF:' + json.dumps(expected)
        self.assertEqual(qualify.parse_probe(text,'tenant-a','a'*40,'real-cluster'), expected)
        for key,value in (('identity',topology.IDENTITIES[2]),('cluster_id','other'),('unique_id',False),('renewed',False),('passed',False)):
            bad=copy.deepcopy(expected);bad[key]=value
            with self.assertRaises(ValueError):
                qualify.parse_probe('NATIVE_PROOF:'+json.dumps(bad),'tenant-a','a'*40,'real-cluster')
        with self.assertRaises(ValueError):
            qualify.parse_probe(text+'\n'+text,'tenant-a','a'*40,'real-cluster')

    def test_mandatory_inventory_cleanup_and_source(self):
        manifest=(topology.ROOT/'kanz/infra/kafka/topics-job.yaml').read_bytes()
        digest,count=qualify.canonical_count(manifest)
        value={'source':'a'*40,'tree':'b'*40,'run':'1','attempt':'1','passed':True,'cleanup':True,
               'cases':[{'case':case,'passed':True} for case in qualify.CASES],
               'adminImage':'sha256:'+'c'*64,'probeImage':'sha256:'+'d'*64,'manifestSHA':digest,'clusterID':'real-cluster','brokerImage':topology.retained.REDPANDA,
               'storage':[{'broker':b,'totalBytes':20<<30,'usedBytes':1<<30,'availableBytes':19<<30,'producerReserveBytes':5<<30} for b in topology.BROKERS]}
        for item in value['cases']:
            case=item['case']
            if case in ('discover','tenant-a','tenant-b','compute-a','ungranted','partial'):
                identity=topology.ADMIN_ID if case in ('discover','partial') else 'spiffe://kanz.internal/ns/'+'/sa/'.join(topology.CLIENTS[case])
                item['observation']={'version':'a'*40,'mode':case,'cluster_id':'real-cluster','identity':identity,'unique_id':True,'passed':True,'renewed':case in ('tenant-a','tenant-b','compute-a')}
            else:
                wrong=case=='wrong-cluster'
                item['receipt']={'expected_cluster_id':'deliberately-wrong' if wrong else 'real-cluster','mode':'verify' if case.endswith('verify') else 'apply',
                                 'manifest_sha256':digest,'verified_topics':0 if wrong else count,'missing_topics':0,'verified_acls':0 if wrong else 6,
                                 'topic_attempts':count if case in ('acme-apply','bravo-apply') else 0,'acl_attempts':6 if case in ('acme-apply','bravo-apply','compute-apply') else 0,'partial_or_unknown':False,'complete':not wrong}
        value['diagnostics']=[]
        for case in qualify.CASES:
            probe=case in ('discover','tenant-a','tenant-b','compute-a','ungranted','partial')
            observation=next(x['observation'] for x in value['cases'] if x['case']==case) if probe else None
            value['diagnostics'].append({'case':case,'jobState':'Failed' if case=='wrong-cluster' else 'Complete','custody':True,'collection':'complete','imageVerified':True,
                'container':{'state':'terminated','exitCode':1 if case=='wrong-cluster' else 0,'reason':'Error' if case=='wrong-cluster' else 'Completed','imageBound':True},
                'probe':{'version':'a'*40,'mode':case,'expected_identity':observation['identity'],'phase':'complete','category':'ok'} if probe else {'state':'UNKNOWN'},
                'image':{'verified':True,'dockerIdentityType':'index','podIdentityType':'manifest','dockerIdentity':value['probeImage' if probe else 'adminImage'],'podIdentity':'sha256:'+'e'*64,'config':'sha256:'+'f'*64,'platformManifest':'sha256:'+'e'*64,'contentCount':3}})
        qualify.validate_report(value,'a'*40,'b'*40,'1','1',manifest)
        for mutate in (lambda x:x.pop('diagnostics'),lambda x:x['diagnostics'].pop(),lambda x:x['diagnostics'].reverse(),
                       lambda x:x['diagnostics'][0].update(imageVerified=False),lambda x:x['diagnostics'][0].update(collection='image'),
                       lambda x:x['diagnostics'][0]['container'].update(imageBound=False),lambda x:x['diagnostics'][0]['container'].update(exitCode=1),
                       lambda x:x['diagnostics'][0]['image'].update(dockerIdentity='sha256:'+'1'*64),lambda x:x['diagnostics'][0]['image'].update(contentCount=7),
                       lambda x:x['diagnostics'][0]['image'].update(platformManifest=None),lambda x:x['diagnostics'][0]['probe'].update(phase='metadata'),
                       lambda x:x['diagnostics'][0]['probe'].update(version='b'*40),lambda x:x['diagnostics'][qualify.CASES.index('wrong-cluster')].update(jobState='Complete')):
            bad=copy.deepcopy(value);mutate(bad)
            with self.assertRaises(ValueError):qualify.validate_report(bad,'a'*40,'b'*40,'1','1',manifest)
        bad=copy.deepcopy(value);bad['cases'][1]['receipt']['expected_cluster_id']='other'
        with self.assertRaises(ValueError): qualify.validate_report(bad,'a'*40,'b'*40,'1','1',manifest)
        bad=copy.deepcopy(value);bad['cases'][1]['receipt']['verified_topics']=count-1
        with self.assertRaises(ValueError): qualify.validate_report(bad,'a'*40,'b'*40,'1','1',manifest)
        for key,replacement in [('brokerImage','other'),('storage',value['storage'][:2]),('storage',[value['storage'][0]]*3)]:
            bad=copy.deepcopy(value);bad[key]=replacement
            with self.assertRaises(ValueError): qualify.validate_report(bad,'a'*40,'b'*40,'1','1',manifest)
        for case in qualify.CASES:
            bad=copy.deepcopy(value);bad['cases']=[x for x in bad['cases'] if x['case']!=case]
            with self.assertRaises(ValueError): qualify.validate_report(bad,'a'*40,'b'*40,'1','1',manifest)
        for key,replacement in (('cleanup',False),('source','f'*40),('adminImage','unknown'),('passed',False)):
            bad=copy.deepcopy(value);bad[key]=replacement
            with self.assertRaises(ValueError): qualify.validate_report(bad,'a'*40,'b'*40,'1','1',manifest)

    def test_cleanup_refuses_other_run_and_cluster(self):
        claim={'source':'a'*40,'run':'1','attempt':'1','cluster':'redpanda-proof-'+'b'*12}
        self.assertEqual(cleanup.validate_claim(claim,'a'*40,'1','1'),claim['cluster'])
        for key,value in (('source','c'*40),('run','2'),('attempt','2'),('cluster','production')):
            bad=copy.deepcopy(claim);bad[key]=value
            with self.assertRaises(ValueError): cleanup.validate_claim(bad,'a'*40,'1','1')


if __name__ == '__main__':
    unittest.main()
