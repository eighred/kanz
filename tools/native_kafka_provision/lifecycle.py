"""Qualify the golden-path infrastructure caller against both owned brokers."""
import json
import os
from pathlib import Path
import re
import shutil
import time

# Case, tenant, exit, observed Job count. Acme's first attempt loses the
# acknowledgement before quota or its second principal can be reconciled.
CASES = (
    ('onboard-bravo', 'bravo', 0, 2),
    ('lost-create-ack', 'acme', 1, 1),
    ('recover', 'acme', 0, 2),
    ('rerun', 'acme', 0, 2),
    ('onboard-partial', 'acme', 3, 2),
    ('manifest-conflict', 'acme', 1, 0),
    ('wrong-cluster', 'acme', 1, 1),
    ('offboard', 'acme', 2, 0),
)


def principals(tenant):
    return (f'User:spiffe://kanz.internal/ns/tenant-{tenant}/sa/risk-engine',
            f'User:spiffe://kanz.internal/ns/kanz-services/sa/archiver-{tenant}')


def runner_contract(docs):
    roles=[d for d in docs if d['kind']=='Role' and d['metadata']['name']=='tenant-native-observer']
    bindings=[d for d in docs if d['kind']=='RoleBinding' and d['metadata']['name']=='tenant-native-observer']
    jobs=[d for d in docs if d['kind']=='Job']
    if len(roles)!=1 or len(bindings)!=1 or len(jobs)!=1:
        raise ValueError('runner inventory differs')
    role,binding=roles[0],bindings[0]
    if (role['metadata']['namespace']!='kanz-messaging' or role['rules']!=[
            {'apiGroups':[''],'resources':['pods'],'verbs':['get','list']},
            {'apiGroups':[''],'resources':['pods/log'],'verbs':['get']}] or
            binding['metadata']['namespace']!='kanz-messaging' or binding['roleRef']!={
                'apiGroup':'rbac.authorization.k8s.io','kind':'Role','name':'tenant-native-observer'} or
            binding['subjects']!=[{'kind':'ServiceAccount','name':'tenant-provisioner','namespace':'kanz-tenancy'}]):
        raise ValueError('runner observation authority differs')
    spec=jobs[0]['spec']['template']['spec']
    if (spec['serviceAccountName']!='tenant-provisioner' or
            any(e['name']=='TENANTCTL_KAFKA_MODE' for e in spec['containers'][0].get('env',[])) or
            {'name':'native-topics','mountPath':'/kafka','readOnly':True} not in spec['containers'][0]['volumeMounts'] or
            {'name':'native-topics','configMap':{'name':'tenantctl-native-topics','optional':True}} not in spec['volumes']):
        raise ValueError('runner default or native packaging differs')


def validate(value, source, image, topic_count):
    if not isinstance(value, list) or tuple(x.get('case') for x in value) != tuple(row[0] for row in CASES):
        raise ValueError('lifecycle inventory missing')
    accounts = {}
    for item, (case, tenant, code, count) in zip(value, CASES):
        if (set(item) != {'case', 'passed', 'exit', 'jobs', 'quotaVerified', 'natsAccounts'} or
                item['passed'] is not True or item['exit'] != code or
                len(item['jobs']) != count or item['quotaVerified'] is not True):
            raise ValueError('lifecycle result missing')
        observed = item['natsAccounts']
        if (not isinstance(observed, dict) or set(observed) != (set(accounts) | {tenant}) or
                any(not re.fullmatch(r'A[A-Z2-7]{55}', key) for key in observed.values()) or
                len(set(observed.values())) != len(observed) or
                any(observed[t] != key for t, key in accounts.items())):
            raise ValueError('NATS account identity or readback differs')
        accounts = observed
        workload, compute = principals(tenant)
        for job in item['jobs']:
            first_topics = case in ('onboard-bravo', 'lost-create-ack') and job['principal'] == workload
            first_acls = case in ('onboard-bravo', 'lost-create-ack') or (case == 'recover' and job['principal'] == compute)
            if (set(job) != {'uid', 'version', 'image', 'complete', 'topicAttempts', 'aclAttempts', 'principal', 'verifiedTopics', 'verifiedACLs'} or
                    not re.fullmatch(r'[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}', job['uid']) or
                    job['version'] != source or job['image'] != image or
                    job['complete'] is not (item['case'] != 'wrong-cluster') or
                    job['verifiedTopics'] != (0 if item['case']=='wrong-cluster' else topic_count) or
                    job['verifiedACLs'] != (0 if item['case']=='wrong-cluster' else 6) or
                    job['topicAttempts'] != (topic_count if first_topics else 0) or
                    job['aclAttempts'] != (6 if first_acls else 0)):
                raise ValueError('lifecycle Job evidence differs')
        if sorted(j['principal'] for j in item['jobs']) != sorted((workload, compute)[:count]):
            raise ValueError('lifecycle principal inventory differs')
    ids = [j['uid'] for item in value for j in item['jobs']]
    if len(ids) != len(set(ids)):
        raise ValueError('lifecycle reused a Job')


def qualify(api, scratch, name, images, source, digest, cluster, built, command, observe_image, diagnostic, topic_count, nats):
    import topology
    import bounded_process
    root = topology.ROOT
    diagnostic.update(case='setup',phase='rbac',exit=None)
    # The canonical runner's RBAC, exercised by impersonation; never cluster-admin
    # authority for tenantctl itself. The fixture owner retains cleanup authority.
    docs = topology.retained.shared.manifests('kanz/infra/tenancy/onboard-job.yaml')
    runner_contract(docs)
    import yaml
    api('apply', '-f', '-', data=yaml.safe_dump_all([d for d in docs if d['kind'] != 'Job']))
    diagnostic['phase']='image-alias'
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
    env = dict(nats.env, PATH=str(wrapper)+os.pathsep+os.environ['PATH'])
    env.update({'KUBECONFIG':str(scratch/'kubeconfig'), 'TENANT_SAS':'risk-engine',
           'COMPUTE_KAFKA_SAS':'archiver', 'TENANTCTL_KAFKA_MODE':'native',
           'NATIVE_KAFKA_IMAGE':pinned,
           'NATIVE_KAFKA_MANIFEST_SHA256':digest, 'NATIVE_KAFKA_CLUSTER_ID':cluster,
           'NATIVE_KAFKA_BROKER':'native-redpanda-0.kanz-messaging.svc:9094',
           'NATIVE_KAFKA_BROKER_SPIFFE_ID':topology.BROKER_ID,
           'NATIVE_KAFKA_PRINCIPAL_ENCODING':'redpanda-rule-escaped-uri',
           'NATIVE_KAFKA_DURABILITY_PROFILE':'redpanda-raft',
           'LIFECYCLE_KUBECTL':real,'LIFECYCLE_LOST_ACK':str(scratch/'lost-ack')})
    def jobs():
        return {j['metadata']['uid']:j for j in json.loads(api('-n','kanz-messaging','get','jobs','-l',
                'app.kubernetes.io/managed-by=kanz-tenantctl-native','-o','json'))['items']}
    def quota():
        raw=api('-n','kanz-services','get','cm','api-gateway-quotas','--ignore-not-found','-o','json')
        return json.loads(raw).get('data') if raw else None
    # Preserve another tenant's quota through actual onboarding and reruns.
    initial={'quotas.json':json.dumps({'neighbor':{'rate_per_sec':7,'burst':9,'max_in_flight':3}})}
    api('create','-f','-',data=json.dumps({'apiVersion':'v1','kind':'ConfigMap','metadata':{
        'name':'api-gateway-quotas','namespace':'kanz-services'},'data':initial}))
    result=[]
    accounts = {}
    for case, tenant, code, _ in CASES:
        diagnostic.update(case=case,phase='snapshot',exit=None)
        before=jobs(); before_quota=json.loads(quota()['quotas.json'])
        attempt=dict(env, TENANT=tenant, LIFECYCLE_FAULT=case, STEP='infra')
        if case=='onboard-partial':
            del attempt['NATS_OPERATOR']
            attempt['TENANTCTL_MANUAL_NATS']='true'
        cm='kafka-native-'+digest[:40]
        if case=='manifest-conflict':
            api('-n','kanz-messaging','annotate','cm',cm,'kanz.internal/native-topic-manifest-sha256='+'0'*64,'--overwrite')
        if case=='wrong-cluster': attempt['NATIVE_KAFKA_CLUSTER_ID']='deliberately-wrong'
        diagnostic['phase']='caller'
        onboarding = root/'kanz/infra/onboarding'
        caller = ['bash', '../tenancy/tenantctl.sh', 'offboard'] if case=='offboard' else ['sh', './provision-tenant.sh']
        output=bounded_process.run(['bash','-c',
            '"$@"; status=$?; printf "\\nCALLER_EXIT:%s\\n" "$status"',
            'lifecycle', *caller], cwd=str(onboarding),env=attempt,timeout=680,stdout_limit=32768,stderr_limit=32768)
        last=output.splitlines()[-1] if output.splitlines() else ''
        match=re.fullmatch(r'CALLER_EXIT:([0-9]{1,3})',last)
        diagnostic['exit']=int(match[1]) if match and int(match[1])<=255 else None
        if output.splitlines()[-1:] != ['CALLER_EXIT:'+str(code)] or ('>> [done]' in output) != (code==0):
            raise ValueError('caller exit or completion claim differs: '+case)
        if case=='offboard' and '>> [' in output:
            raise ValueError('offboard reached a lifecycle effect')
        if case=='manifest-conflict':
            api('-n','kanz-messaging','annotate','cm',cm,'kanz.internal/native-topic-manifest-sha256='+digest,'--overwrite')
        after=jobs()
        diagnostic['phase']='jobs'
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
            diagnostic['phase']='image'
            graph=observe_image(pod,job,built,name,time.monotonic()+60,{},source_tag=images['admin'])
            diagnostic['phase']='receipt'
            receipt=json.loads(api('-n','kanz-messaging','logs',pod['metadata']['name'],'-c','tenant','--limit-bytes=16384'))
            r=receipt['receipt']
            if (r['manifest_sha256']!=digest or r['expected_cluster_id']!=attempt['NATIVE_KAFKA_CLUSTER_ID'] or
                    r['durability_profile']!='redpanda-raft' or graph['verified'] is not True):
                raise ValueError('caller receipt custody differs')
            observations.append({'uid':uid,'version':receipt['version'],'image':built,'complete':r['complete'],
                                 'topicAttempts':r['topic_attempts'],'aclAttempts':r['acl_attempts'],
                                 'verifiedTopics':r['verified_topics'],'verifiedACLs':r['verified_acls'],
                                 'principal':next(a.removeprefix('-principal=') for a in job['spec']['template']['spec']['containers'][0]['args'] if a.startswith('-principal='))})
        diagnostic['phase']='nats-readback'
        if tenant not in accounts:
            accounts[tenant] = nats.account(tenant, create_probe=True)
        observed = {t: nats.account(t) for t in accounts}
        if observed != accounts:
            raise ValueError('NATS account identity changed')
        if case in ('onboard-bravo', 'recover'):
            if tenant in before_quota:
                raise ValueError('first successful onboarding already had a quota')
            before_quota[tenant] = {'rate_per_sec':50,'burst':100,'max_in_flight':64}
        result.append({'case':case,'passed':True,'exit':code,'jobs':observations,
                       'quotaVerified':json.loads(quota()['quotas.json'])==before_quota,'natsAccounts':observed})
    diagnostic['phase']='validation'
    validate(result,source,built,topic_count)
    diagnostic['phase']='complete'
    return result
