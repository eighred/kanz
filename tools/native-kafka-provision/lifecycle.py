"""Exercise the tracked tenantctl caller against the owned cluster and real Jobs.

NATS is explicitly manual: successful Kafka work must still exit PARTIAL (3),
never claim whole-tenant onboarding. No fake broker or fake Kubernetes success.
"""
import json
import os
from pathlib import Path
import re
import shutil
import time

CASES = ('onboard-partial', 'rerun', 'manifest-conflict', 'lost-create-ack',
         'recover', 'wrong-cluster', 'offboard')


def validate(value, source, image):
    if not isinstance(value, list) or tuple(x.get('case') for x in value) != CASES:
        raise ValueError('lifecycle inventory missing')
    counts = (2, 2, 0, 1, 2, 1, 0)
    codes = (3, 3, 1, 1, 3, 1, 2)
    for item, count, code in zip(value, counts, codes):
        if (set(item) != {'case', 'passed', 'exit', 'jobs', 'quotaUnchanged'} or
                item['passed'] is not True or item['exit'] != code or
                len(item['jobs']) != count or item['quotaUnchanged'] is not True):
            raise ValueError('lifecycle result missing')
        for job in item['jobs']:
            if (set(job) != {'uid', 'version', 'image', 'complete', 'topicAttempts', 'aclAttempts'} or
                    not re.fullmatch(r'[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}', job['uid']) or
                    job['version'] != source or job['image'] != image or
                    job['complete'] is not (item['case'] != 'wrong-cluster') or
                    job['topicAttempts'] != 0 or job['aclAttempts'] != 0):
                raise ValueError('lifecycle Job evidence differs')
    ids = [j['uid'] for item in value for j in item['jobs']]
    if len(ids) != len(set(ids)):
        raise ValueError('lifecycle reused a Job')


def qualify(api, scratch, name, images, source, digest, cluster, built, command, observe_image):
    import topology
    import bounded_process
    root = topology.ROOT
    # The canonical runner's RBAC, exercised by impersonation; never cluster-admin
    # authority for tenantctl itself. The fixture owner retains cleanup authority.
    docs = topology.retained.shared.manifests('kanz/infra/tenancy/onboard-job.yaml')
    import yaml
    api('apply', '-f', '-', data=yaml.safe_dump_all([d for d in docs if d['kind'] != 'Job']))
    inspection = json.loads(command(['docker', 'exec', name+'-control-plane', 'crictl', 'inspecti', images['admin']]))
    refs = inspection['status']['repoDigests']
    if not refs or not re.fullmatch(r'.+@sha256:[0-9a-f]{64}', refs[0]):
        raise ValueError('local image digest unavailable')
    pinned = 'ghcr.io/eighred/kanz-kafka-provision@'+refs[0].split('@')[1]
    tags=inspection['status'].get('repoTags',[])
    if not tags or not tags[0].endswith(images['admin']):
        raise ValueError('local source tag unavailable')
    command(['docker', 'exec', name+'-control-plane', 'ctr', '--namespace=k8s.io', 'images', 'tag', tags[0], pinned])
    wrapper = scratch/'caller-bin'
    wrapper.mkdir()
    real = shutil.which('kubectl')
    if not real or not shutil.which('jq'):
        raise ValueError('real caller dependencies required')
    shim = wrapper/'kubectl'
    # Only one deliberate lost response after a real accepted Job create.
    # All other operations exec the real CLI with the real runner principal.
    shim.write_text('''#!/usr/bin/env python3
import os,sys,subprocess
from pathlib import Path
args=sys.argv[1:]
cmd=[os.environ['LIFECYCLE_KUBECTL'],'--as=system:serviceaccount:kanz-tenancy:tenant-provisioner',*args]
mark=Path(os.environ['LIFECYCLE_LOST_ACK'])
if os.environ.get('LIFECYCLE_FAULT')=='lost-create-ack' and not mark.exists() and 'create' in args and '-f' in args:
    data=sys.stdin.buffer.read(131073)
    if len(data)>131072: sys.exit(1)
    if b'kind: Job' in data:
        result=subprocess.run(cmd,input=data,stdout=subprocess.PIPE,stderr=subprocess.DEVNULL,timeout=30)
        if result.returncode==0: mark.write_text('accepted')
        sys.exit(1)
    result=subprocess.run(cmd,input=data,timeout=30)
    sys.exit(result.returncode)
os.execv(cmd[0],cmd)
''', encoding='utf-8')
    shim.chmod(0o700)
    env = {'PATH':str(wrapper)+os.pathsep+os.environ['PATH'], 'HOME':str(scratch),
           'KUBECONFIG':str(scratch/'kubeconfig'), 'TENANT':'acme', 'TENANT_SAS':'risk-engine',
           'COMPUTE_KAFKA_SAS':'archiver', 'TENANTCTL_KAFKA_MODE':'native',
           'TENANTCTL_MANUAL_NATS':'true', 'NATIVE_KAFKA_IMAGE':pinned,
           'NATIVE_KAFKA_MANIFEST_SHA256':digest, 'NATIVE_KAFKA_CLUSTER_ID':cluster,
           'NATIVE_KAFKA_BROKER':'native-redpanda-0.kanz-messaging.svc:9094',
           'NATIVE_KAFKA_BROKER_SPIFFE_ID':topology.BROKER_ID,
           'NATIVE_KAFKA_PRINCIPAL_ENCODING':'redpanda-rule-escaped-uri',
           'NATIVE_KAFKA_DURABILITY_PROFILE':'redpanda-raft',
           'LIFECYCLE_KUBECTL':real,'LIFECYCLE_LOST_ACK':str(scratch/'lost-ack')}
    def jobs():
        return {j['metadata']['uid']:j for j in json.loads(api('-n','kanz-messaging','get','jobs','-l',
                'app.kubernetes.io/managed-by=kanz-tenantctl-native','-o','json'))['items']}
    def quota():
        raw=api('-n','kanz-services','get','cm','api-gateway-quotas','--ignore-not-found','-o','json')
        return json.loads(raw).get('data') if raw else None
    # Preserve another tenant's quota through actual onboarding and reruns.
    initial={'quotas.json':json.dumps({'bravo':{'rate_per_sec':7,'burst':9,'max_in_flight':3},
                                     'acme':{'rate_per_sec':50,'burst':100,'max_in_flight':64}})}
    api('create','-f','-',data=json.dumps({'apiVersion':'v1','kind':'ConfigMap','metadata':{
        'name':'api-gateway-quotas','namespace':'kanz-services'},'data':initial}))
    result=[]
    for case, code in zip(CASES,(3,3,1,1,3,1,2)):
        before=jobs(); before_quota=json.loads(quota()['quotas.json'])
        attempt=dict(env, LIFECYCLE_FAULT=case)
        cm='kafka-native-'+digest[:40]
        if case=='manifest-conflict':
            api('-n','kanz-messaging','annotate','cm',cm,'kanz.internal/native-topic-manifest-sha256='+'0'*64,'--overwrite')
        if case=='wrong-cluster': attempt['NATIVE_KAFKA_CLUSTER_ID']='deliberately-wrong'
        output=bounded_process.run(['bash','-c',
            'bash "$1" "$2"; status=$?; printf "\\nCALLER_EXIT:%s\\n" "$status"',
            'lifecycle',str(root/'kanz/infra/tenancy/tenantctl.sh'), 'offboard' if case=='offboard' else 'onboard'],
            cwd=str(root),env=attempt,timeout=680,stdout_limit=32768,stderr_limit=32768)
        if output.splitlines()[-1:] != ['CALLER_EXIT:'+str(code)] or '>> [done]' in output:
            raise ValueError('caller exit or completion claim differs: '+case)
        if case=='offboard' and '>> [' in output:
            raise ValueError('offboard reached a lifecycle effect')
        if case=='manifest-conflict':
            api('-n','kanz-messaging','annotate','cm',cm,'kanz.internal/native-topic-manifest-sha256='+digest,'--overwrite')
        after=jobs()
        if not set(before)<=set(after): raise ValueError('caller deleted a prior attempt')
        observations=[]
        for uid, job in after.items():
            if uid in before: continue
            jobname=job['metadata']['name']
            # Even a lost create ACK must leave one recoverable, finite Job.
            api('-n','kanz-messaging','wait','--for=condition='+('failed' if case=='wrong-cluster' else 'complete'),
                'job/'+jobname,'--timeout=280s',timeout=290)
            pods=json.loads(api('-n','kanz-messaging','get','pods','-l','batch.kubernetes.io/controller-uid='+uid,'-o','json'))['items']
            from qualify import pod_custody
            pod=pod_custody(pods,job,uid)
            graph=observe_image(pod,job,built,name,time.monotonic()+60,{})
            receipt=json.loads(api('-n','kanz-messaging','logs',pod['metadata']['name'],'-c','tenant','--limit-bytes=16384'))
            r=receipt['receipt']
            if (r['manifest_sha256']!=digest or r['expected_cluster_id']!=attempt['NATIVE_KAFKA_CLUSTER_ID'] or
                    r['durability_profile']!='redpanda-raft' or graph['verified'] is not True):
                raise ValueError('caller receipt custody differs')
            observations.append({'uid':uid,'version':receipt['version'],'image':built,'complete':r['complete'],
                                 'topicAttempts':r['topic_attempts'],'aclAttempts':r['acl_attempts']})
        result.append({'case':case,'passed':True,'exit':code,'jobs':observations,
                       'quotaUnchanged':json.loads(quota()['quotas.json'])==before_quota})
    validate(result,source,built)
    return result
