"""Actual native topic/ACL qualification in a fresh owned cluster, never adoption."""
import hashlib
import json
import os
from pathlib import Path
import re
import sys
import tempfile
import time
import uuid

import yaml
import topology
import image_custody

sys.path.insert(0, str(topology.ROOT / 'tools/gateway-policy-proof'))
import bounded_process

ROOT = topology.ROOT
CASES = ('discover', 'acme-apply', 'acme-idempotent', 'bravo-apply', 'compute-apply',
         'wrong-cluster', 'tenant-a', 'tenant-b', 'compute-a', 'ungranted', 'partial',
         'acme-verify', 'bravo-verify')


def valid_cluster_id(value):
    return (isinstance(value,str) and 0<len(value)<=128 and
            re.fullmatch(r'(?:redpanda\.)?[A-Za-z0-9_-]+',value) is not None and
            value not in ('redpanda.initializing','redpanda.unknown'))


def source_identity(source, tree, run, attempt, expected):
    if source != expected or not re.fullmatch(r'[0-9a-f]{40}', source) or not re.fullmatch(r'[0-9a-f]{40}', tree):
        raise ValueError('source identity differs')
    if not re.fullmatch(r'[1-9][0-9]*', run) or not re.fullmatch(r'[1-9][0-9]*', attempt):
        raise ValueError('positive run identity required')


def job_custody(actual, desired, uid=None):
    meta = actual.get('metadata', {})
    expected = desired['metadata']
    identity = meta.get('uid', '')
    if (meta.get('name') != expected['name'] or meta.get('namespace') != expected['namespace'] or
            not re.fullmatch(r'[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}', identity) or
            (uid is not None and identity != uid)):
        raise ValueError('job custody refused')
    pod = actual.get('spec', {}).get('template', {}).get('spec', {})
    wanted = desired['spec']['template']['spec']
    if pod.get('serviceAccountName') != wanted['serviceAccountName'] or len(pod.get('containers', [])) != 1:
        raise ValueError('job principal refused')
    container = pod['containers'][0]
    target = wanted['containers'][0]
    if any(container.get(k, []) != target.get(k, []) for k in ('name', 'image', 'args', 'command', 'env')):
        raise ValueError('job executable differs')
    return identity


def pod_custody(items, desired, uid, pod_uid=None):
    # Only the exact owned Job's sole pod can contribute observations. Unknown
    # messages/reasons, container logs and runtime identifiers are never exported.
    if not isinstance(items, list) or len(items) != 1:
        raise ValueError('pod inventory unknown')
    pod = items[0]
    meta = pod.get('metadata', {})
    owners = meta.get('ownerReferences', [])
    if (not re.fullmatch(r'[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}',meta.get('uid','')) or
            (pod_uid is not None and meta.get('uid') != pod_uid) or
            not re.fullmatch(r'[a-z0-9](?:[-a-z0-9]{0,251}[a-z0-9])?',meta.get('name','')) or
            meta.get('namespace') != desired['metadata']['namespace'] or len(owners) != 1 or
            owners[0].get('uid') != uid or owners[0].get('kind') != 'Job' or
            owners[0].get('name') != desired['metadata']['name'] or owners[0].get('controller') is not True):
        raise ValueError('pod owner differs')
    spec = pod.get('spec', {})
    wanted = desired['spec']['template']['spec']
    if spec.get('serviceAccountName') != wanted['serviceAccountName'] or len(spec.get('containers', [])) != 1:
        raise ValueError('pod principal differs')
    if any(spec['containers'][0].get(k, []) != wanted['containers'][0].get(k, []) for k in ('name','image','args','command','env')):
        raise ValueError('pod executable differs')
    return pod


def pod_diagnostic(items, desired, uid):
    pod=pod_custody(items,desired,uid)
    wanted=desired['spec']['template']['spec']
    statuses = pod.get('status', {}).get('containerStatuses', [])
    result = {'state': 'UNKNOWN', 'exitCode': None, 'reason': 'UNKNOWN', 'imageBound': False}
    if not statuses:
        return result
    if len(statuses) != 1 or statuses[0].get('name') != wanted['containers'][0]['name']:
        raise ValueError('pod container differs')
    status = statuses[0]
    state = status.get('state', {})
    if len(state) != 1 or next(iter(state)) not in ('running', 'waiting', 'terminated'):
        raise ValueError('pod state unknown')
    kind = next(iter(state)); result['state'] = kind
    detail = state[kind]
    if kind == 'terminated':
        code = detail.get('exitCode')
        if type(code) is not int or not 0 <= code <= 255:
            raise ValueError('exit code unknown')
        result['exitCode'] = code
    if detail.get('reason') in ('Completed', 'Error', 'OOMKilled', 'ContainerCannotRun', 'CrashLoopBackOff', 'ImagePullBackOff', 'ErrImagePull', 'ContainerCreating'):
        result['reason'] = detail['reason']
    return result


def probe_diagnostic(text, mode, source):
    rows = [line[len('NATIVE_DIAGNOSTIC:'):] for line in text.splitlines() if line.startswith('NATIVE_DIAGNOSTIC:')]
    if len(rows) != 1:
        return {'state': 'UNKNOWN'}
    pairs = []
    value = json.loads(rows[0], object_pairs_hook=lambda x: pairs.extend(x) or dict(x))
    if len(pairs) != len(value) or set(value) != {'version', 'mode', 'expected_identity', 'phase', 'category'}:
        raise ValueError('diagnostic shape refused')
    identity = topology.ADMIN_ID if mode in ('discover', 'partial') else 'spiffe://kanz.internal/ns/' + '/sa/'.join(topology.CLIENTS[mode])
    if value['version'] != source or value['mode'] != mode or value['expected_identity'] != identity:
        raise ValueError('diagnostic identity differs')
    if value['phase'] not in ('workload_api', 'identity', 'uniqueid', 'metadata', 'estate', 'partial', 'acl', 'renewal', 'reconnect', 'complete') or value['category'] not in ('ok', 'input', 'broker', 'conflict', 'deadline', 'cancelled', 'unknown'):
        raise ValueError('diagnostic category refused')
    return value


def admin_diagnostic(text, source):
    """Retain only closed counters/categories, never broker errors or inputs."""
    value=json.loads(text,object_pairs_hook=image_custody.strict)
    if not isinstance(value,dict) or set(value)!={'version','receipt','error'} or value['version']!=source:
        raise ValueError('admin diagnostic identity differs')
    category=value['error']
    if category not in ('input-refused','resource-conflict','resource-missing','broker-unavailable'):
        raise ValueError('admin diagnostic category refused')
    receipt=value['receipt']
    counters=('verified_topics','missing_topics','verified_acls','topic_attempts','acl_attempts')
    flags=('partial_or_unknown','complete')
    if not isinstance(receipt,dict) or set(receipt)!=set(counters+flags+('expected_cluster_id','mode','manifest_sha256')):
        raise ValueError('admin diagnostic receipt shape')
    result={'category':category}
    for key in counters:
        if type(receipt[key]) is not int or not 0<=receipt[key]<=256:
            raise ValueError('admin diagnostic counter bound')
        result[key]=receipt[key]
    for key in flags:
        if type(receipt[key]) is not bool: raise ValueError('admin diagnostic flag type')
        result[key]=receipt[key]
    return result


def command(argv, data=None, timeout=300, *, strip=True, stdout_limit=4 << 20):
    return bounded_process.run(argv, cwd=ROOT, env=dict(os.environ), data=data, timeout=timeout,
                               stdout_limit=stdout_limit, stderr_limit=4 << 20, strip=strip)



def observe_image(pod, desired, built, name, deadline, diagnostic):
    """Single bounded image observer for full proof and inert diagnostics."""
    progress={'phase':'pod_reference','contentCount':0,'mediaType':'UNKNOWN'}
    try:
        statuses=pod.get('status',{}).get('containerStatuses',[])
        if len(statuses)!=1: raise image_custody.CustodyError('image_reference_type')
        observed=statuses[0].get('imageID','')
        image_custody.reference_digest(observed)
        requested=desired['spec']['template']['spec']['containers'][0]['image']
        def node(*args,raw=False):
            remaining=deadline-time.monotonic()
            if remaining<=0: raise TimeoutError('job observation expired')
            return command(['docker','exec',name+'-control-plane',*args],timeout=min(300,remaining),strip=not raw,stdout_limit=1<<20)
        progress['phase']='cri_lookup'
        inspection=json.loads(node('crictl','inspecti',requested))
        return image_custody.verify(inspection,requested,observed,built,
            lambda digest:node('ctr','--namespace=k8s.io','content','get',digest,raw=True).encode('utf-8'),progress)
    except Exception as error:
        diagnostic['imageFailure']={'code':image_custody.failure(error),**progress}
        raise


def job(name, namespace, sa, image, *, args=None, mode=None, cluster=None):
    d = topology.retained.shared.job(name, sa, 'unused', 'unused', image)
    d['metadata']['namespace'] = namespace
    d['spec']['activeDeadlineSeconds'] = 240
    pod = d['spec']['template']['spec']
    container = pod['containers'][0]
    container['env'] = []
    if args is not None:
        container['args'] = args
    else:
        container['env'] = [{'name':'KANZ_NATIVE_FIXTURE_MODE','value':mode}]
        if cluster:
            container['env'].append({'name':'KANZ_NATIVE_EXPECTED_CLUSTER','value':cluster})
    pod['volumes'].append({'name':'manifest','configMap':{'name':'native-topics'}})
    container['volumeMounts'].append({'name':'manifest','mountPath':'/manifest/topics-job.yaml',
                                     'subPath':'topics-job.yaml','readOnly':True})
    return d


def parse_probe(text, mode, source, cluster=None):
    # Test framework chatter has no authority. Require exactly one closed result.
    rows = [line.removeprefix('NATIVE_PROOF:') for line in text.splitlines() if line.startswith('NATIVE_PROOF:')]
    if len(rows) != 1:
        raise ValueError('missing closed probe observation')
    value = json.loads(rows[0])
    identity = topology.ADMIN_ID if mode in ('discover','partial') else 'spiffe://kanz.internal/ns/' + '/sa/'.join(topology.CLIENTS[mode])
    expected = {'version':source, 'mode':mode, 'cluster_id':value.get('cluster_id'),
                'identity':identity,'unique_id':True,'passed':True,
                'renewed':mode in ('tenant-a','tenant-b','compute-a')}
    if value != expected or not valid_cluster_id(value.get('cluster_id','')):
        raise ValueError('probe identity or result differs')
    if cluster is not None and value['cluster_id'] != cluster:
        raise ValueError('probe changed estate')
    return value


def canonical_count(raw):
    # Independent fixture expectation from the existing data table, never a
    # copied topic inventory or execution of its surrounding shell program.
    if not isinstance(raw,bytes) or not 0<len(raw)<=128<<10: raise ValueError('manifest bound')
    lines=raw.decode('utf-8').splitlines()
    if lines.count("    done <<'EOF'")!=1 or lines.count('    EOF')!=1: raise ValueError('table boundary')
    start,end=lines.index("    done <<'EOF'"),lines.index('    EOF')
    if start>=end: raise ValueError('table order')
    expanded=set(); rows=set()
    for line in lines[start+1:end]:
        if not line.strip() or line.strip().startswith('#'): continue
        fields=line.split()
        if len(fields)!=5: raise ValueError('table row')
        name,partitions,cleanup,retention,dlq=fields
        if not re.fullmatch(r'[a-z][a-z0-9_]*(?:[.][a-z0-9_]+)+',name) or name in rows or not partitions.isdecimal() or not 1<=int(partitions)<=128 or cleanup not in ('delete','compact') or not re.fullmatch(r'-1|[1-9][0-9]*',retention) or dlq not in ('yes','no'):
            raise ValueError('table shape')
        rows.add(name)
        if name=='dlq.archiver': continue # existing sole system-only exemption
        for topic in [name]+(['dlq.'+name] if dlq=='yes' else []):
            if topic in expanded: raise ValueError('duplicate expanded topic')
            expanded.add(topic)
    if 'order.order' not in rows or not 0<len(expanded)<=256: raise ValueError('table inventory')
    return hashlib.sha256(raw).hexdigest(),len(expanded)


def validate_receipt(receipt, case, cluster, digest, count):
    keys={'expected_cluster_id','mode','manifest_sha256','verified_topics','missing_topics',
          'verified_acls','topic_attempts','acl_attempts','partial_or_unknown','complete'}
    if not isinstance(receipt,dict) or set(receipt)!=keys:
        raise ValueError('receipt shape')
    expected='deliberately-wrong' if case=='wrong-cluster' else cluster
    mode='verify' if case.endswith('verify') else 'apply'
    if receipt['expected_cluster_id']!=expected or receipt['manifest_sha256']!=digest or receipt['mode']!=mode:
        raise ValueError('receipt resource differs')
    for key in ('verified_topics','missing_topics','verified_acls','topic_attempts','acl_attempts'):
        if type(receipt[key]) is not int or not 0<=receipt[key]<=256:
            raise ValueError('receipt count')
    if type(receipt['complete']) is not bool or type(receipt['partial_or_unknown']) is not bool:
        raise ValueError('receipt truth')
    if case=='wrong-cluster':
        if receipt['complete'] or receipt['partial_or_unknown'] or any(receipt[k] for k in ('verified_topics','missing_topics','verified_acls','topic_attempts','acl_attempts')):
            raise ValueError('wrong estate effected or accepted')
    else:
        if not receipt['complete'] or receipt['partial_or_unknown'] or receipt['missing_topics']!=0 or receipt['verified_topics']!=count or receipt['verified_acls']!=6:
            raise ValueError('incomplete CLI readback')
        if case.endswith(('idempotent','verify')) or case=='compute-apply':
            if receipt['topic_attempts']!=0: raise ValueError('existing topic rewritten')
        if case.endswith(('idempotent','verify')) and receipt['acl_attempts']!=0:
            raise ValueError('existing ACL rewritten')
        if case in ('acme-apply','bravo-apply') and receipt['topic_attempts']!=count:
            raise ValueError('initial creation inventory differs')
        if case in ('acme-apply','bravo-apply','compute-apply') and receipt['acl_attempts']!=6:
            raise ValueError('initial grant inventory differs')


def validate_diagnostics(value, source):
    diagnostics=value.get('diagnostics')
    if not isinstance(diagnostics,list) or len(diagnostics)!=len(CASES):
        raise ValueError('diagnostic inventory missing')
    for case,item in zip(CASES,diagnostics):
        if not isinstance(item,dict) or set(item)!={'case','jobState','custody','collection','probe','container','imageVerified','image'}:
            raise ValueError('diagnostic shape differs')
        if (item['case']!=case or item['jobState']!=('Failed' if case=='wrong-cluster' else 'Complete') or
                item['custody'] is not True or item['collection']!='complete' or item['imageVerified'] is not True):
            raise ValueError('diagnostic custody incomplete')
        state=item['container']
        if (not isinstance(state,dict) or set(state)!={'state','exitCode','reason','imageBound'} or
                state['state']!='terminated' or state['imageBound'] is not True or
                type(state['exitCode']) is not int or not 0<=state['exitCode']<=255 or
                (state['exitCode']!=0)!=(case=='wrong-cluster') or state['reason'] not in ('Completed','Error','UNKNOWN')):
            raise ValueError('container outcome differs')
        graph=item['image']
        if not isinstance(graph,dict) or set(graph)!={'verified','dockerIdentityType','podIdentityType','dockerIdentity','podIdentity','config','platformManifest','contentCount'}:
            raise ValueError('image graph summary missing')
        if (graph['verified'] is not True or graph['dockerIdentityType'] not in ('config','manifest','index') or
                graph['podIdentityType'] not in ('config','manifest','index') or
                type(graph['contentCount']) is not int or not 1<=graph['contentCount']<=6):
            raise ValueError('image graph summary invalid')
        for field in ('dockerIdentity','podIdentity','config'):
            if not isinstance(graph[field],str) or not image_custody.DIGEST.fullmatch(graph[field]): raise ValueError('image graph digest absent')
        manifest=graph['platformManifest']
        if manifest is not None and (not isinstance(manifest,str) or not image_custody.DIGEST.fullmatch(manifest)):
            raise ValueError('platform manifest invalid')
        for side in ('docker','pod'):
            kind=graph[side+'IdentityType'];identity=graph[side+'Identity']
            if (kind=='config' and identity!=graph['config']) or (kind=='manifest' and identity!=manifest):
                raise ValueError('typed graph identity differs')
        if (graph['podIdentityType']==graph['dockerIdentityType']=='config') != (manifest is None): raise ValueError('platform graph absent')
        probe=case in ('discover','tenant-a','tenant-b','compute-a','ungranted','partial')
        if graph['dockerIdentity']!=value.get('probeImage' if probe else 'adminImage'):
            raise ValueError('built image identity differs')
        if probe:
            closed=probe_diagnostic('NATIVE_DIAGNOSTIC:'+json.dumps(item['probe']),case,source)
            if closed.get('phase')!='complete' or closed.get('category')!='ok': raise ValueError('probe did not complete')
        elif item['probe']!={'state':'UNKNOWN'}:
            raise ValueError('unexpected CLI probe projection')


def validate_report(value, source, tree, run, attempt, manifest):
    source_identity(source, tree, run, attempt, source)
    digest,count=canonical_count(manifest)
    if value.get("manifestSHA")!=digest: raise ValueError("canonical manifest differs")
    if value.get("brokerImage")!=topology.retained.REDPANDA: raise ValueError("broker image differs")
    storage=value.get("storage")
    if not isinstance(storage,list) or len(storage)!=3 or {x.get("broker") for x in storage}!=set(topology.BROKERS): raise ValueError("broker storage inventory")
    for observation in storage:
        if set(observation)!={"broker","totalBytes","usedBytes","availableBytes","producerReserveBytes"}: raise ValueError("storage shape")
        for key in ("totalBytes","usedBytes","availableBytes","producerReserveBytes"):
            if type(observation[key]) is not int or not 0<=observation[key]<2**63: raise ValueError("storage bound")
        if not 0<observation["totalBytes"] or observation["usedBytes"]>observation["totalBytes"] or observation["availableBytes"]>observation["totalBytes"] or observation["producerReserveBytes"]==0: raise ValueError("storage values")
        topology.retained.require_capacity(observation,observation["producerReserveBytes"])
    if value.get('source') != source or value.get('tree') != tree or value.get('run') != run or value.get('attempt') != attempt:
        raise ValueError('source identity differs')
    if value.get('passed') is not True or value.get('cleanup') is not True or tuple(x.get('case') for x in value.get('cases',[])) != CASES:
        raise ValueError('incomplete native proof')
    if not all(x.get('passed') is True for x in value['cases']):
        raise ValueError('case refused')
    validate_diagnostics(value,source)
    for item in value['cases']:
        case=item['case']
        if case in ('discover','tenant-a','tenant-b','compute-a','ungranted','partial'):
            if set(item)!={'case','passed','observation'}: raise ValueError('observation missing')
            parse_probe('NATIVE_PROOF:'+json.dumps(item['observation']),case,source,value.get('clusterID'))
        else:
            if set(item)!={'case','passed','receipt'}: raise ValueError('receipt missing')
            validate_receipt(item['receipt'],case,value.get('clusterID'),digest,count)

    for name in ('adminImage','probeImage'):
        if not re.fullmatch(r'sha256:[0-9a-f]{64}',value.get(name,'')):
            raise ValueError('image custody absent')
    if not re.fullmatch(r'[0-9a-f]{64}',value.get('manifestSHA','')) or not valid_cluster_id(value.get('clusterID','')):
        raise ValueError('resource custody absent')


def main():
    source=command(['git','rev-parse','HEAD']); tree=command(['git','rev-parse','HEAD^{tree}'])
    run=os.environ['GITHUB_RUN_ID']; attempt=os.environ['GITHUB_RUN_ATTEMPT']
    source_identity(source, tree, run, attempt, os.environ.get('GITHUB_SHA', ''))
    evidence={'source':source,'tree':tree,'run':run,'attempt':attempt,'passed':False,'cleanup':False,'cases':[], 'diagnostics':[], 'cause':'UNKNOWN', 'brokerImage':topology.retained.REDPANDA}
    artifact=Path(os.environ['RUNNER_TEMP'])/'native-kafka-provision.json'
    name='redpanda-proof-'+uuid.uuid4().hex[:12]
    with tempfile.TemporaryDirectory(prefix='native-kafka-proof-') as directory:
        scratch=Path(directory); owned=False
        def k(*args,data=None,timeout=300):
            return command(['kubectl','--kubeconfig',str(scratch/'kubeconfig'),'--context','kind-'+name,*args],data,timeout)
        def apply(docs):
            return k('apply','-f','-',data=yaml.safe_dump_all(docs))
        def execute(d, failed=False):
            ns=d['metadata']['namespace']; jobname=d['metadata']['name']
            diagnostic={'case':jobname,'jobState':'UNKNOWN','custody':False,'collection':'job','probe':{'state':'UNKNOWN'}}
            evidence['diagnostics'].append(diagnostic)
            deadline=time.monotonic()+250
            def observe(*args,data=None):
                remaining=deadline-time.monotonic()
                if remaining<=0: raise TimeoutError('job observation expired')
                return k(*args,data=data,timeout=min(300,remaining))
            uid=None
            try:
                observe('apply','-f','-',data=yaml.safe_dump_all([d]))
                while time.monotonic()<deadline:
                    state=json.loads(observe('-n',ns,'get','job',jobname,'-o','json'))
                    uid=job_custody(state,d,uid); diagnostic['custody']=True
                    conditions=state.get('status',{}).get('conditions',[])
                    terminal=[c['type'] for c in conditions if c.get('status')=='True' and c.get('type') in ('Complete','Failed')]
                    if terminal:
                        diagnostic['jobState']=terminal[0] if len(terminal)==1 else 'UNKNOWN'
                        diagnostic['collection']='pod'
                        pods=json.loads(observe('-n',ns,'get','pods','-l','batch.kubernetes.io/controller-uid='+uid,'-o','json'))
                        label='probe' if jobname in ('discover','tenant-a','tenant-b','compute-a','ungranted','partial') else 'admin'
                        pod_custody(pods.get('items'),d,uid)
                        diagnostic['container']=pod_diagnostic(pods.get('items'),d,uid)
                        # Self-reported phase is advisory until the image graph is
                        # verified. Persist it even if the collector later fails.
                        diagnostic['imageVerified']=False
                        diagnostic['collection']='closed-probe'
                        selected=pods['items'][0]['metadata']
                        logs=observe('-n',ns,'logs',selected['name'],'-c','probe','--limit-bytes=65536')
                        after=json.loads(observe('-n',ns,'get','pod',selected['name'],'-o','json'))
                        pod_custody([after],d,uid,selected['uid'])
                        if label=='probe': diagnostic['probe']=probe_diagnostic(logs,jobname,source)
                        artifact.write_text(json.dumps(evidence,sort_keys=True)+'\n',encoding='utf-8')
                        diagnostic['collection']='image'
                        diagnostic['image']=observe_image(pods['items'][0],d,evidence[label+'Image'],name,deadline,diagnostic)
                        diagnostic['imageVerified']=True
                        diagnostic['container']['imageBound']=True
                        diagnostic['collection']='complete'
                        if terminal != ['Failed' if failed else 'Complete']:
                            if label=='admin' and terminal==['Failed']:
                                diagnostic['adminFailure']=admin_diagnostic(logs,source)
                            raise ValueError('unexpected actual job outcome')
                        return logs
                    time.sleep(1)
                raise TimeoutError('fixture job did not terminate')
            finally:
                # Persist even the failing case before owned cluster cleanup.
                artifact.write_text(json.dumps(evidence,sort_keys=True)+'\n',encoding='utf-8')
        try:
            evidence['stage']='build'
            images={}
            for label,path in (('admin','kanz/cmd/kanz-kafka-provision/Dockerfile'),('probe','tools/native-kafka-provision/Dockerfile')):
                image='kanz-native-'+label+':'+source
                command(['docker','build','--build-arg','VERSION='+source,'-f',path,'-t',image,'.'],timeout=1200)
                images[label]=image; evidence[label+'Image']=command(['docker','image','inspect','--format','{{.Id}}',image])
            evidence['stage']='cluster'
            # Absence before ownership; a random name never authorizes deleting a collision.
            if name in command(['kind','get','clusters']).splitlines() or (scratch/'kubeconfig').exists():
                raise ValueError('cluster collision')
            claim=Path(os.environ['RUNNER_TEMP'])/'native-kafka-claim.json'
            with claim.open('x', encoding='utf-8') as stream:
                json.dump({'source':source,'run':run,'attempt':attempt,'cluster':name},stream)
                stream.flush(); os.fsync(stream.fileno())
            owned=True
            topology.retained.shared.create_enforcing_cluster(name,scratch)
            for image in images.values(): command(['kind','load','docker-image',image,'--name',name])
            evidence['stage']='spire'
            crds=[]
            for path in sorted((ROOT/'kanz/infra/security/spire/crds').glob('*.yaml')):
                crds+=topology.retained.shared.manifests(str(path.relative_to(ROOT)))
            apply(crds)
            for crd in crds: k('wait','--for=condition=Established','crd/'+crd['metadata']['name'],'--timeout=90s')
            apply(topology.retained.spire_objects())
            k('-n','spire-system','rollout','status','statefulset/spire-server','--timeout=240s')
            k('-n','spire-system','rollout','status','daemonset/spire-agent','--timeout=240s')
            registration=topology.retained.shared.manifests('kanz/infra/security/spire/registration.yaml')
            registration[0]['spec']['ttl']='60s'; apply(registration)
            evidence['stage']='brokers'
            apply(topology.broker_objects()); apply(topology.policies())
            for broker in topology.BROKERS: k('-n','kanz-messaging','rollout','status','deployment/'+broker,'--timeout=240s')
            raw=(ROOT/'kanz/infra/kafka/topics-job.yaml').read_bytes()
            if not 0<len(raw)<=128<<10: raise ValueError('manifest bound')
            digest,count=canonical_count(raw); evidence['manifestSHA']=digest
            for ns in ('kanz-messaging','kanz-services','tenant-acme','tenant-bravo'):
                apply([{'apiVersion':'v1','kind':'ConfigMap','metadata':{'name':'native-topics','namespace':ns},'immutable':True,'data':{'topics-job.yaml':raw.decode()}}])
            evidence['storage']=[]
            for broker in topology.BROKERS:
                reserve=topology.retained.storage_reserve(k('-n','kanz-messaging','exec','deployment/'+broker,'-c','redpanda','--','rpk','cluster','config','get','storage_min_free_bytes','--ignore-profile','-X','admin.hosts=127.0.0.1:9644'))
                capacity=topology.retained.storage_capacity(k('-n','kanz-messaging','exec','deployment/'+broker,'-c','redpanda','--','df','-Pk','/var/lib/redpanda/data'))
                topology.retained.require_capacity(capacity,reserve)
                evidence['storage'].append(dict(capacity,broker=broker,producerReserveBytes=reserve))
            evidence['stage']='discover'
            observation=parse_probe(execute(job('discover','kanz-messaging','kafka-provisioner',images['probe'],mode='discover')),'discover',source)
            cluster=observation['cluster_id']; evidence['clusterID']=cluster
            evidence['cases'].append({'case':'discover','passed':True,'observation':observation})
            principals={'acme':'User:'+topology.IDENTITIES[1], 'bravo':'User:'+topology.IDENTITIES[2], 'compute':'User:'+topology.IDENTITIES[3]}
            def cli(case,tenant,principal,mode,wrong=False):
                evidence['stage']=case
                expected='deliberately-wrong' if wrong else cluster
                args=['-mode='+mode,'-manifest=/manifest/topics-job.yaml','-manifest-sha256='+digest,'-tenant='+tenant,'-principal='+principal,
                      '-broker=native-redpanda-0.kanz-messaging.svc:9094','-broker-spiffe-id='+topology.BROKER_ID,'-expected-cluster-id='+expected,
                      '-workload-socket=unix:///run/spiffe/spire-agent.sock','-timeout=240s']
                result=json.loads(execute(job(case,'kanz-messaging','kafka-provisioner',images['admin'],args=args),failed=wrong))
                receipt=result.get('receipt',{})
                if result.get('version')!=source or receipt.get('expected_cluster_id')!=expected or receipt.get('manifest_sha256')!=digest:
                    raise ValueError('CLI custody differs')
                if wrong:
                    if result.get('error')!='resource-conflict' or receipt.get('complete') is not False or receipt.get('topic_attempts')!=0 or receipt.get('acl_attempts')!=0:
                        raise ValueError('wrong estate accepted')
                elif result.get('error') or receipt.get('complete') is not True or receipt.get('missing_topics')!=0 or receipt.get('verified_acls')!=6:
                    raise ValueError('CLI acceptance incomplete')
                if case.endswith(('idempotent','verify')) and (receipt.get('topic_attempts')!=0 or receipt.get('acl_attempts')!=0):
                    raise ValueError('idempotency repeated effects')
                validate_receipt(receipt,case,cluster,digest,count)
                evidence['cases'].append({'case':case,'passed':True,'receipt':receipt})
            cli('acme-apply','acme',principals['acme'],'apply')
            cli('acme-idempotent','acme',principals['acme'],'apply')
            cli('bravo-apply','bravo',principals['bravo'],'apply')
            cli('compute-apply','acme',principals['compute'],'apply')
            cli('wrong-cluster','acme',principals['acme'],'apply',True)
            for case in ('tenant-a','tenant-b','compute-a','ungranted','partial'):
                evidence['stage']=case
                ns,sa=('kanz-messaging','kafka-provisioner') if case=='partial' else topology.CLIENTS[case]
                observation=parse_probe(execute(job(case,ns,sa,images['probe'],mode=case,cluster=cluster)),case,source,cluster)
                evidence['cases'].append({'case':case,'passed':True,'observation':observation})
            cli('acme-verify','acme',principals['acme'],'verify')
            cli('bravo-verify','bravo',principals['bravo'],'verify')
            evidence['passed']=True
        except Exception as error:
            evidence['errorCategory']=type(error).__name__ if isinstance(error,(ValueError,RuntimeError,TimeoutError)) else 'unexpected'
            raise RuntimeError('native qualification failed') from None
        finally:
            if owned:
                try:
                    command(['kind','delete','cluster','--name',name,'--kubeconfig',str(scratch/'kubeconfig')],timeout=180)
                    evidence['cleanup']=name not in command(['kind','get','clusters']).splitlines()
                except Exception:
                    evidence['cleanup']=False
            evidence['passed'] &= evidence['cleanup']
            if evidence['passed']:
                try: validate_report(evidence,source,tree,run,attempt,raw)
                except ValueError:
                    evidence['passed']=False
                    evidence['errorCategory']='report-refused'
            artifact.write_text(json.dumps(evidence,sort_keys=True)+'\n',encoding='utf-8')
    if not evidence['passed']: raise RuntimeError('native cleanup unconfirmed')


if __name__=='__main__':
    main()
