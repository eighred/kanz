import copy
import json
import os
from pathlib import Path
import shutil
import subprocess
import unittest

import lifecycle
import topology


def evidence():
    rows=[]
    n=0
    for case,count,code in zip(lifecycle.CASES,(2,2,0,1,2,1,0),(3,3,1,1,3,1,2)):
        jobs=[]
        for _ in range(count):
            n+=1
            jobs.append({'uid':f'{n:08x}-1234-1234-1234-123456789abc','version':'a'*40,
                         'image':'sha256:'+'c'*64,'complete':case!='wrong-cluster','topicAttempts':0,'aclAttempts':0})
        rows.append({'case':case,'passed':True,'exit':code,'jobs':jobs,'quotaUnchanged':True})
    return rows


class LifecycleTest(unittest.TestCase):
    def test_evidence_requires_every_case_and_distinct_attempt(self):
        value=evidence()
        lifecycle.validate(value,'a'*40,'sha256:'+'c'*64)
        for mutation in (lambda v:v.pop(),lambda v:v.reverse(),lambda v:v[0].update(exit=0),
                         lambda v:v[0].update(quotaUnchanged=False),lambda v:v[3].update(jobs=[]),
                         lambda v:v[4]['jobs'][0].update(uid=v[0]['jobs'][0]['uid']),
                         lambda v:v[4]['jobs'][0].update(topicAttempts=1),
                         lambda v:v[5]['jobs'][0].update(complete=True)):
            bad=copy.deepcopy(value);mutation(bad)
            with self.assertRaises(ValueError): lifecycle.validate(bad,'a'*40,'sha256:'+'c'*64)

    def test_receipt_validator_refuses_incomplete_foreign_and_multiple_results(self):
        jq=shutil.which('jq')
        if not jq:
            if os.environ.get('CI'): self.fail('jq is required for lifecycle qualification')
            self.skipTest('jq unavailable locally; Linux qualification requires it')
        path=topology.ROOT/'kanz/infra/tenancy/native-receipt.jq'
        value={'version':'a'*40,'receipt':{'mode':'apply','expected_cluster_id':'cluster-a','manifest_sha256':'b'*64,
               'durability_profile':'redpanda-raft','complete':True,'partial_or_unknown':False,
               'missing_topics':0,'verified_topics':60,'verified_acls':6,'topic_attempts':0,'acl_attempts':0}}
        cmd=[jq,'-se','--arg','phase','receipt','--arg','uid','','--arg','job','','--argjson','created','{}',
             '--arg','cluster','cluster-a','--arg','digest','b'*64,'--arg','profile','redpanda-raft','-f',str(path)]
        def accepted(raw):
            return subprocess.run(cmd,input=raw,text=True,capture_output=True,timeout=5).returncode==0
        self.assertTrue(accepted(json.dumps(value)))
        for key,badval in (('complete',False),('partial_or_unknown',True),('expected_cluster_id','foreign'),
                           ('manifest_sha256','0'*64),('verified_acls',5),('missing_topics',1),('mode','plan'),
                           ('durability_profile','kafka-min-isr'),('verified_topics',0),('topic_attempts',-1)):
            bad=copy.deepcopy(value);bad['receipt'][key]=badval
            self.assertFalse(accepted(json.dumps(bad)),key)
        for raw in ('', 'not-json', json.dumps(value)+'\n'+json.dumps(value)):
            self.assertFalse(accepted(raw))
        bad=copy.deepcopy(value);bad['error']='broker-unavailable'
        self.assertFalse(accepted(json.dumps(bad)))

    def test_pod_validator_refuses_other_owner_image_and_failed_process(self):
        jq=shutil.which('jq')
        if not jq:
            if os.environ.get('CI'): self.fail('jq is required for lifecycle qualification')
            self.skipTest('jq unavailable locally; Linux qualification requires it')
        uid='12345678-1234-1234-1234-123456789abc'
        container={'name':'tenant','image':'pinned@sha256:'+'a'*64,'args':['-mode=apply']}
        spec={'serviceAccountName':'kafka-provisioner','containers':[container]}
        created={'metadata':{'namespace':'kanz-messaging'},'spec':{'template':{'spec':spec}}}
        pod={'metadata':{'namespace':'kanz-messaging','name':'native-pod','uid':uid,
             'ownerReferences':[{'uid':uid,'name':'native-job','kind':'Job','controller':True}]},
             'spec':copy.deepcopy(spec),'status':{'phase':'Succeeded','containerStatuses':[
                 {'name':'tenant','restartCount':0,'state':{'terminated':{'exitCode':0}}}]}}
        cmd=[jq,'-e','--arg','phase','pod','--arg','uid',uid,'--arg','job','native-job',
             '--argjson','created',json.dumps(created),'--arg','cluster','','--arg','digest','',
             '--arg','profile','','-f',str(topology.ROOT/'kanz/infra/tenancy/native-receipt.jq')]
        def accepted(pods):
            return subprocess.run(cmd,input=json.dumps({'items':pods}),text=True,capture_output=True,timeout=5).returncode==0
        self.assertTrue(accepted([pod]))
        self.assertFalse(accepted([pod,pod]))
        for mutation in (lambda p:p['metadata']['ownerReferences'][0].update(uid='foreign'),
                         lambda p:p['spec']['containers'][0].update(image='other'),
                         lambda p:p['spec']['containers'][0].update(args=['-mode=plan']),
                         lambda p:p['status']['containerStatuses'][0].update(restartCount=1),
                         lambda p:p['status']['containerStatuses'][0]['state']['terminated'].update(exitCode=1)):
            bad=copy.deepcopy(pod);mutation(bad)
            self.assertFalse(accepted([bad]))


if __name__=='__main__': unittest.main()
