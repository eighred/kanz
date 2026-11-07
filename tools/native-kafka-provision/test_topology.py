import hashlib
import unittest
import yaml
import topology
import qualify
import owned_cleanup as cleanup
import copy
import json


class TopologyTest(unittest.TestCase):
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
            self.assertEqual(bootstrap['superusers'], [topology.ADMIN_ID])
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
        qualify.validate_report(value,'a'*40,'b'*40,'1','1',manifest)
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
