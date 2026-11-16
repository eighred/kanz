import copy
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import image_diagnostic as diagnostic
import qualify


class ImageDiagnosticTest(unittest.TestCase):
    def test_manual_non_main_only(self):
        diagnostic.invocation('workflow_dispatch','refs/heads/eighred/diagnostic')
        for event,ref in [('push','refs/heads/x'),('pull_request','refs/heads/x'),('workflow_dispatch','refs/heads/main'),('workflow_dispatch','refs/tags/test')]:
            with self.assertRaises(ValueError):diagnostic.invocation(event,ref)

    def test_inert_jobs_have_no_identity_manifest_or_domain_inputs(self):
        for label in ('admin','probe'):
            d=diagnostic.inert_job(label,'kanz-native-'+label+':'+'a'*40)
            pod=d['spec']['template']['spec'];c=pod['containers'][0]
            self.assertEqual(pod['volumes'],[]);self.assertEqual(c['volumeMounts'],[])
            self.assertEqual(c['env'],[]);self.assertFalse(pod['automountServiceAccountToken'])
            self.assertEqual(pod['serviceAccountName'],'default')
            self.assertEqual(pod['securityContext'],{'runAsNonRoot':True,'runAsUser':65532,'seccompProfile':{'type':'RuntimeDefault'}})
            self.assertEqual(c['securityContext'],{'allowPrivilegeEscalation':False,'readOnlyRootFilesystem':True,'capabilities':{'drop':['ALL']}})
            self.assertEqual(c['resources']['limits'],{'cpu':'1','memory':'256Mi'})
            self.assertEqual(c.get('args',[]),['-mode=unsupported'] if label=='admin' else [])
            self.assertEqual(d['spec']['backoffLimit'],0)

    def test_result_refuses_domain_effects_and_non_skip(self):
        receipt={'expected_cluster_id':'','mode':'','manifest_sha256':'','verified_topics':0,'missing_topics':0,
                 'verified_acls':0,'topic_attempts':0,'acl_attempts':0,'partial_or_unknown':False,'complete':False}
        value={'version':'a'*40,'receipt':receipt,'error':'input-refused'}
        self.assertEqual(diagnostic.inert_result('admin',json.dumps(value),'a'*40),'input-refused')
        for mutate in (lambda x:x.update(version='b'*40),lambda x:x['receipt'].update(topic_attempts=1),lambda x:x.update(error='broker-unavailable')):
            bad=copy.deepcopy(value);mutate(bad)
            with self.assertRaises(ValueError):diagnostic.inert_result('admin',json.dumps(bad),'a'*40)
        skip='=== RUN   TestNativeBrokerQualification\n--- SKIP: TestNativeBrokerQualification (0.00s)\nPASS\n'
        self.assertEqual(diagnostic.inert_result('probe',skip,'a'*40),'explicit-skip')
        for bad in (skip.replace('SKIP','PASS'),skip+skip,skip+'NATIVE_PROOF: {}\n'):
            with self.assertRaises(ValueError):diagnostic.inert_result('probe',bad,'a'*40)

    def test_observer_preserves_closed_failure_before_cleanup(self):
        d=diagnostic.inert_job('admin','kanz-native-admin:'+'a'*40)
        report={}
        with self.assertRaises(ValueError):qualify.observe_image({'status':{'containerStatuses':[{'imageID':'SECRET'}]}},d,'sha256:'+'a'*64,'owned',0,report)
        self.assertEqual(report['imageFailure'],{'code':'unsupported_image_reference','phase':'pod_reference','contentCount':0,'mediaType':'UNKNOWN'})
        self.assertNotIn('SECRET',json.dumps(report))

    def test_diagnostic_receipt_cannot_claim_qualification(self):
        good={'diagnosticOnly':True,'qualification':False,'cleanup':True,'images':[]}
        for label in ('admin','probe'):
            good['images'].append({'case':label,'custody':True,'imageVerified':True,'image':{'verified':True},
                                  'jobState':'Failed' if label=='admin' else 'Complete','container':{'exitCode':1 if label=='admin' else 0},
                                  'inertResult':'input-refused' if label=='admin' else 'explicit-skip'})
        diagnostic.validate(good)
        for key in ('diagnosticOnly','cleanup'):
            bad=copy.deepcopy(good);bad[key]=False
            with self.assertRaises(ValueError):diagnostic.validate(bad)
        bad=copy.deepcopy(good);bad['qualification']=True
        with self.assertRaises(ValueError):diagnostic.validate(bad)
        for mutate in (lambda x:x['images'].pop(),lambda x:x['images'][0].update(imageVerified=False),lambda x:x['images'][0]['container'].update(exitCode=0)):
            bad=copy.deepcopy(good);mutate(bad)
            with self.assertRaises(ValueError):diagnostic.validate(bad)

    def test_failure_receipt_is_persisted_before_owned_cleanup(self):
        with tempfile.TemporaryDirectory() as directory:
            env={'GITHUB_EVENT_NAME':'workflow_dispatch','GITHUB_REF':'refs/heads/eighred/test',
                 'GITHUB_SHA':'a'*40,'GITHUB_RUN_ID':'12','GITHUB_RUN_ATTEMPT':'1','RUNNER_TEMP':directory}
            owned=False;desired=None;deleted=False
            uid='12345678-1234-1234-1234-123456789abc'
            def command(args,data=None,**kwargs):
                nonlocal owned,desired,deleted
                if args[:2]==['git','rev-parse']:return 'b'*40 if args[2].endswith('{tree}') else 'a'*40
                if args[:3]==['docker','image','inspect']:return 'sha256:'+'a'*64
                if args[:3]==['kind','get','clusters']:return ''
                if args[:3]==['kind','create','cluster']:owned=True;return ''
                if args[:3]==['kind','delete','cluster']:
                    before=json.loads((Path(directory)/'native-image-diagnostic.json').read_text())
                    self.assertFalse(before['qualification'])
                    self.assertEqual(before['images'][0]['imageFailure']['code'],'image_content_digest_differs')
                    self.assertTrue(owned);deleted=True;return ''
                if args[0]=='kubectl':
                    tail=args[5:]
                    if tail[:1]==['apply']:
                        desired=diagnostic.yaml.safe_load(data);return ''
                    if tail[:2]==['get','job']:
                        job=copy.deepcopy(desired);job['metadata']['uid']=uid
                        job['status']={'conditions':[{'type':'Failed','status':'True'}]};return json.dumps(job)
                    pod={'metadata':{'name':'image-admin-abc','namespace':'default','uid':uid,
                         'ownerReferences':[{'uid':uid,'kind':'Job','name':'image-admin','controller':True}]},
                         'spec':desired['spec']['template']['spec'],
                         'status':{'containerStatuses':[{'name':'probe','state':{'terminated':{'exitCode':1,'reason':'Error'}}}]}}
                    if tail[:2]==['get','pods']:return json.dumps({'items':[pod]})
                    if tail[:2]==['get','pod']:return json.dumps(pod)
                    if tail[0]=='logs':return 'private not retained'
                    self.fail('unexpected kubectl command')
                if args[:2] in (['docker','build'],['kind','load']):return ''
                self.fail('unexpected diagnostic command')
            def fail_image(pod,d,built,name,deadline,item):
                item['imageFailure']={'code':'image_content_digest_differs','phase':'pod_graph','mediaType':'UNKNOWN','contentCount':0}
                raise qualify.image_custody.CustodyError('image_content_digest_differs')
            with patch.dict(os.environ,env),patch.object(qualify,'command',side_effect=command),patch.object(qualify,'observe_image',side_effect=fail_image),patch.object(diagnostic,'inert_result',return_value='input-refused'):
                with self.assertRaises(RuntimeError):diagnostic.main()
            report=json.loads((Path(directory)/'native-image-diagnostic.json').read_text())
            self.assertTrue(deleted);self.assertTrue(report['cleanup']);self.assertFalse(report['diagnosticComplete'])
            self.assertEqual(report['errorCategory'],'image_content_digest_differs')
            self.assertNotIn('private',json.dumps(report))


if __name__=='__main__':unittest.main()
