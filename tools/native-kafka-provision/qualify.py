"""Actual native topic/ACL qualification in a fresh owned cluster, never adoption."""
import copy
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
import runtime_diagnostic
import lifecycle
import nats_fixture
import system_bootstrap
import argo_bootstrap

sys.path.insert(0, str(topology.ROOT / 'tools/gateway-policy-proof'))
import bounded_process

ROOT = topology.ROOT
SYSTEM_CASES = argo_bootstrap.CASES
HISTORY_CASES = ('history-before', 'history-active', 'history-rollback')
ACTIVATION_CASES = SYSTEM_CASES[:4] + ('history-before', 'system-activate', 'history-active',
                                       'system-rollback', 'history-rollback')
REFUSAL_CASES = ('wrong-cluster', 'system-mismatch')
CASES = ('discover', 'acme-idempotent',
         'wrong-cluster', 'tenant-a', 'tenant-b', 'compute-a', 'ungranted', 'partial',
         'acme-verify', 'bravo-verify', 'quorum-healthy', 'quorum-majority', 'quorum-minority', 'quorum-restored') + ACTIVATION_CASES
ADMIN_PROBES = ('discover','partial','quorum-healthy','quorum-majority','quorum-minority','quorum-restored') + HISTORY_CASES
PROBES = ADMIN_PROBES + tuple(topology.CLIENTS)


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
    identity = topology.ADMIN_ID if mode in ADMIN_PROBES else 'spiffe://kanz.internal/ns/' + '/sa/'.join(topology.CLIENTS[mode])
    if value['version'] != source or value['mode'] != mode or value['expected_identity'] != identity:
        raise ValueError('diagnostic identity differs')
    category=value['category']
    if value['phase'] not in ('workload_api', 'identity', 'uniqueid', 'metadata', 'estate', 'partial', 'acl', 'own-topic', 'foreign-topic', 'own-group', 'foreign-group', 'admin-denial', 'renewal', 'reconnect', 'quorum', 'complete') or not isinstance(category,str) or (category not in ('ok', 'input', 'broker', 'conflict', 'deadline', 'cancelled', 'unknown') and re.fullmatch(r'kafka-(?:-1|[0-9]|[1-9][0-9]|1[01][0-9]|12[0-7])',category) is None):
        raise ValueError('diagnostic category refused')
    return value


def admin_diagnostic(text, source):
    """Retain only closed counters/categories, never broker errors or inputs."""
    value=json.loads(text,object_pairs_hook=image_custody.strict)
    if not isinstance(value,dict) or set(value)-{'version','receipt','error','failure_code'} or not {'version','receipt','error'}<=set(value) or value['version']!=source:
        raise ValueError('admin diagnostic identity differs')
    category=value['error']
    if category not in ('input-refused','resource-conflict','resource-missing','broker-unavailable'):
        raise ValueError('admin diagnostic category refused')
    receipt=value['receipt']
    counters=('verified_topics','missing_topics','verified_acls','topic_attempts','acl_attempts')
    flags=('partial_or_unknown','complete')
    if not isinstance(receipt,dict) or set(receipt)!=set(counters+flags+('expected_cluster_id','mode','manifest_sha256','durability_profile')):
        raise ValueError('admin diagnostic receipt shape')
    result={'category':category}
    if 'failure_code' in value:
        code=value['failure_code']
        if not isinstance(code,str) or (code not in ('deadline','cancelled','eof','timeout','network','unknown','unexpected-eof','tls-expired','tls-invalid','tls-authority','tls-certificate','protocol',
                'estate','broker-count','broker-shape','broker-duplicate','topic-shape','partition-shape','topic-not-ready',
                'partition-id','replication-factor','isr-count',
                'replica-shape','isr-shape','leader-shape','config-count','config-key','config-cleanup','config-retention','config-min-isr','config-write-caching') and re.fullmatch(r'kafka-(?:-1|[0-9]|[1-9][0-9]|1[01][0-9]|12[0-7])',code) is None):
            raise ValueError('admin failure code refused')
        result['failure_code']=code
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



def observe_image(pod, desired, built, name, deadline, diagnostic, *, source_tag=None):
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
            lambda digest:node('ctr','--namespace=k8s.io','content','get',digest,raw=True).encode('utf-8'),progress,source_tag=source_tag)
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
    identity = topology.ADMIN_ID if mode in ADMIN_PROBES else 'spiffe://kanz.internal/ns/' + '/sa/'.join(topology.CLIENTS[mode])
    expected = {'version':source, 'mode':mode, 'cluster_id':value.get('cluster_id'),
                'identity':identity,'unique_id':True,'passed':True,
                'renewed':mode in ('tenant-a','tenant-b','compute-a')}
    if value != expected or not valid_cluster_id(value.get('cluster_id','')):
        raise ValueError('probe identity or result differs')
    if cluster is not None and value['cluster_id'] != cluster:
        raise ValueError('probe changed estate')
    return value


def canonical_inventory(raw, system=False):
    # Independent fixture expectation from the existing data table, never a
    # copied topic inventory or execution of its surrounding shell program.
    if not isinstance(raw,bytes) or not 0<len(raw)<=128<<10: raise ValueError('manifest bound')
    lines=raw.decode('utf-8').splitlines()
    if lines.count("    done <<'EOF'")!=1 or lines.count('    EOF')!=1: raise ValueError('table boundary')
    start,end=lines.index("    done <<'EOF'"),lines.index('    EOF')
    if start>=end: raise ValueError('table order')
    expanded={}; rows=set()
    for line in lines[start+1:end]:
        if not line.strip() or line.strip().startswith('#'): continue
        fields=line.split()
        if len(fields)!=5: raise ValueError('table row')
        name,partitions,cleanup,retention,dlq=fields
        if not re.fullmatch(r'[a-z][a-z0-9_]*(?:[.][a-z0-9_]+)+',name) or name in rows or not partitions.isdecimal() or not 1<=int(partitions)<=128 or cleanup not in ('delete','compact') or not re.fullmatch(r'-1|[1-9][0-9]*',retention) or dlq not in ('yes','no'):
            raise ValueError('table shape')
        rows.add(name)
        if name=='dlq.archiver' and not system: continue # existing sole system-only exemption
        for topic in [name]+(['dlq.'+name] if dlq=='yes' else []):
            if topic in expanded: raise ValueError('duplicate expanded topic')
            expanded[topic]=int(partitions) if topic==name else 3
    if 'order.order' not in rows or not 0<len(expanded)<=256: raise ValueError('table inventory')
    return hashlib.sha256(raw).hexdigest(),len(expanded),sum(expanded.values())


def canonical_count(raw):
    return canonical_inventory(raw)[:2]


def fixture_capacity(partitions, system_partitions=0):
    if type(partitions) is not int or partitions<=0: raise ValueError('partition capacity shape')
    replicas=3*partitions+system_partitions # acme, bravo, partial and system; compute reuses acme.
    if replicas*(2<<20)>topology.BROKER_MEMORY_BYTES or replicas*topology.FALLOC_BYTES+(256<<20)>=topology.DATA_VOLUME_BYTES:
        raise ValueError('full fixture exceeds broker memory or volume budget')
    return {'replicasPerBroker':replicas,'brokerMemoryBytes':topology.BROKER_MEMORY_BYTES,
            'volumeBytes':topology.DATA_VOLUME_BYTES,'segmentAllocationBytes':topology.FALLOC_BYTES}


def mismatched_system_manifest(raw):
    pattern = rb"(?m)^(    order\.order +[0-9]+ +delete +)([0-9]+)( +(?:yes|no)\r?$)"
    changed, count = re.subn(pattern, lambda m: m[1]+str(int(m[2])+1).encode()+m[3], raw)
    if count != 1:
        raise ValueError("order topic mismatch probe anchor absent")
    return changed


def validate_case_receipt(receipt, case, cluster, manifest):
    system = case in SYSTEM_CASES
    raw = mismatched_system_manifest(manifest) if case == 'system-mismatch' else manifest
    digest, count, _ = canonical_inventory(raw, system)
    validate_receipt(receipt, case, cluster, digest, count)


def validate_receipt(receipt, case, cluster, digest, count):
    keys={'expected_cluster_id','mode','manifest_sha256','durability_profile','verified_topics','missing_topics',
          'verified_acls','topic_attempts','acl_attempts','partial_or_unknown','complete'}
    if not isinstance(receipt,dict) or set(receipt)!=keys:
        raise ValueError('receipt shape')
    expected='deliberately-wrong' if case=='wrong-cluster' else cluster
    mode='verify' if case.endswith('verify') else 'apply'
    if receipt['expected_cluster_id']!=expected or receipt['manifest_sha256']!=digest or receipt['mode']!=mode or receipt['durability_profile']!='redpanda-raft':
        raise ValueError('receipt resource differs')
    for key in ('verified_topics','missing_topics','verified_acls','topic_attempts','acl_attempts'):
        if type(receipt[key]) is not int or not 0<=receipt[key]<=256:
            raise ValueError('receipt count')
    if type(receipt['complete']) is not bool or type(receipt['partial_or_unknown']) is not bool:
        raise ValueError('receipt truth')
    if case=='system-mismatch':
        if receipt['complete'] or receipt['partial_or_unknown'] or receipt['topic_attempts'] or receipt['acl_attempts'] or receipt['verified_acls']:
            raise ValueError('system mismatch accepted or mutated')
    elif case=='wrong-cluster':
        if receipt['complete'] or receipt['partial_or_unknown'] or any(receipt[k] for k in ('verified_topics','missing_topics','verified_acls','topic_attempts','acl_attempts')):
            raise ValueError('wrong estate effected or accepted')
    else:
        if not receipt['complete'] or receipt['partial_or_unknown'] or receipt['missing_topics']!=0 or receipt['verified_topics']!=count or receipt['verified_acls']!=(0 if case in SYSTEM_CASES else 6):
            raise ValueError('incomplete CLI readback')
        if case.endswith(('idempotent','verify')) or case in ('system-repeat','system-activate','system-rollback'):
            if receipt['topic_attempts']!=0: raise ValueError('existing topic rewritten')
        if case.endswith(('idempotent','verify')) and receipt['acl_attempts']!=0:
            raise ValueError('existing ACL rewritten')
        if case in SYSTEM_CASES and receipt['acl_attempts']:
            raise ValueError('system scope attempted ACL writes')
        if case == 'system-create' and receipt['topic_attempts']!=count:
            raise ValueError('initial creation inventory differs')


def validate_diagnostics(value, source):
    diagnostics=value.get('diagnostics')
    if not isinstance(diagnostics,list) or len(diagnostics)!=len(CASES):
        raise ValueError('diagnostic inventory missing')
    for case,item in zip(CASES,diagnostics):
        if not isinstance(item,dict) or set(item)!={'case','jobState','custody','collection','probe','container','imageVerified','image'}:
            raise ValueError('diagnostic shape differs')
        if (item['case']!=case or item['jobState']!=('Failed' if case in REFUSAL_CASES else 'Complete') or
                item['custody'] is not True or item['collection']!='complete' or item['imageVerified'] is not True):
            raise ValueError('diagnostic custody incomplete')
        state=item['container']
        if (not isinstance(state,dict) or set(state)!={'state','exitCode','reason','imageBound'} or
                state['state']!='terminated' or state['imageBound'] is not True or
                type(state['exitCode']) is not int or not 0<=state['exitCode']<=255 or
                (state['exitCode']!=0)!=(case in REFUSAL_CASES) or state['reason'] not in ('Completed','Error','UNKNOWN')):
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
        probe=case in PROBES
        if graph['dockerIdentity']!=value.get('probeImage' if probe else 'adminImage'):
            raise ValueError('built image identity differs')
        if probe:
            closed=probe_diagnostic('NATIVE_DIAGNOSTIC:'+json.dumps(item['probe']),case,source)
            if closed.get('phase')!='complete' or closed.get('category')!='ok': raise ValueError('probe did not complete')
        elif item['probe']!={'state':'UNKNOWN'}:
            raise ValueError('unexpected CLI probe projection')


def validate_report(value, source, tree, run, attempt, manifest):
    source_identity(source, tree, run, attempt, source)
    digest,count,partitions=canonical_inventory(manifest)
    lifecycle.validate(value.get('lifecycle'), source, value.get('adminImage'), count)
    if value.get('natsVersions') != {tool: version for tool, (_, version) in nats_fixture.MODULES.items()}:
        raise ValueError('NATS fixture tool versions differ')
    if value.get('natsSeedTenants') != ['acme', 'bravo']:
        raise ValueError('NATS seed readback missing')
    argo_bootstrap.validate(value.get('argoSyncs'))
    hook_diagnostics = value.get('argoHookDiagnostics')
    if not isinstance(hook_diagnostics, list) or len(hook_diagnostics) != len(SYSTEM_CASES):
        raise ValueError('Argo Job custody inventory differs')
    for sync, hook in zip(value['argoSyncs'], hook_diagnostics):
        case = sync['case']
        raw = mismatched_system_manifest(manifest) if case == 'system-mismatch' else manifest
        expected = hashlib.sha256(raw).hexdigest()
        if (hook.get('case') != case or hook.get('jobUID') != sync['jobUID']
                or any(hook.get(key) is not True for key in
                       ('freshJob','argsMatch','mountMatches','immutableTable','revisionMatches','resultMatches'))
                or any(hook.get(key) != expected for key in ('expectedDigest','jobDigest','configDigest'))):
            raise ValueError('Argo Job or manifest custody differs')

    if value.get('fixtureCapacity')!=fixture_capacity(partitions, canonical_inventory(manifest, True)[2]): raise ValueError('fixture capacity differs')
    before,after=value.get('brokerPodsBefore'),value.get('brokerPodsAfter')
    if not isinstance(before,dict) or set(before)!=set(topology.BROKERS) or after!=before or len(set(before.values()))!=3:
        raise ValueError('broker continuity missing')
    if any(not isinstance(uid,str) or not re.fullmatch(r'[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}',uid) for uid in before.values()):
        raise ValueError('broker continuity shape')
    if value.get("manifestSHA")!=digest: raise ValueError("canonical manifest differs")
    if value.get("brokerImage")!=topology.retained.REDPANDA: raise ValueError("broker image differs")
    storage=value.get("storage")
    if not isinstance(storage,list) or len(storage)!=3 or {x.get("broker") for x in storage}!=set(topology.BROKERS): raise ValueError("broker storage inventory")
    for observation in storage:
        if set(observation)!={"broker","totalBytes","usedBytes","availableBytes","producerReserveBytes","segmentAllocationBytes"}: raise ValueError("storage shape")
        if type(observation['segmentAllocationBytes']) is not int or observation['segmentAllocationBytes']!=topology.FALLOC_BYTES: raise ValueError('allocation readback differs')
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
        if case in PROBES:
            if set(item)!={'case','passed','observation'}: raise ValueError('observation missing')
            parse_probe('NATIVE_PROOF:'+json.dumps(item['observation']),case,source,value.get('clusterID'))
        else:
            if set(item)!={'case','passed','receipt'}: raise ValueError('receipt missing')
            validate_case_receipt(item['receipt'],case,value.get('clusterID'),manifest)

    for name in ('adminImage','probeImage'):
        if not re.fullmatch(r'sha256:[0-9a-f]{64}',value.get(name,'')):
            raise ValueError('image custody absent')
    if not re.fullmatch(r'[0-9a-f]{64}',value.get('manifestSHA','')) or not valid_cluster_id(value.get('clusterID','')):
        raise ValueError('resource custody absent')


def broker_health(broker, items):
    """Advisory failure projection; no broker logs, messages or credentials."""
    if broker not in topology.BROKERS or not isinstance(items,list) or len(items)!=1:
        raise ValueError('broker inventory unknown')
    pod=items[0]
    if pod.get('metadata',{}).get('labels',{}).get('native-broker')!=broker:
        raise ValueError('broker selector differs')
    containers=pod.get('spec',{}).get('containers',[])
    if len(containers)!=2 or {c.get('name') for c in containers}!={'redpanda','svid'} or next(c for c in containers if c['name']=='redpanda').get('image')!=topology.retained.REDPANDA:
        raise ValueError('broker image or container differs')
    statuses=pod.get('status',{}).get('containerStatuses',[])
    if not isinstance(statuses,list) or len(statuses)>2 or len({s.get('name') for s in statuses})!=len(statuses) or any(s.get('name') not in ('redpanda','svid') for s in statuses):
        raise ValueError('broker status shape')
    result={'broker':broker,'collection':'complete','containers':[], 'eviction':runtime_diagnostic.eviction(pod.get('status',{}))}
    reasons=('Completed','Error','OOMKilled','ContainerCannotRun','CrashLoopBackOff','ImagePullBackOff','ErrImagePull','ContainerCreating')
    for status in statuses:
        count=status.get('restartCount')
        if type(count) is not int or not 0<=count<=100000: raise ValueError('restart bound')
        state=status.get('state',{})
        kind=next(iter(state)) if isinstance(state,dict) and len(state)==1 else 'UNKNOWN'
        if kind not in ('running','waiting','terminated'): kind='UNKNOWN'
        reason=state.get(kind,{}).get('reason') if kind!='UNKNOWN' else None
        last=status.get('lastState',{}).get('terminated',{}).get('reason')
        result['containers'].append({'name':status['name'],'restarts':count,'state':kind,
            'ready':status['ready'] if type(status.get('ready')) is bool else 'UNKNOWN',
            'reason':reason if reason in reasons else 'UNKNOWN','lastReason':last if last in reasons else 'UNKNOWN'})
    return result


def stable_broker(broker,items,expected=None):
    health=broker_health(broker,items)
    uid=items[0].get('metadata',{}).get('uid','')
    if not re.fullmatch(r'[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}',uid) or (expected is not None and uid!=expected):
        raise ValueError('broker pod replaced')
    if health['eviction']!='not-observed' or len(health['containers'])!=2 or any(c['restarts']!=0 or c['state']!='running' or c['ready'] is not True for c in health['containers']):
        raise ValueError('broker continuity unavailable')
    return uid


def main():
    source=command(['git','rev-parse','HEAD']); tree=command(['git','rev-parse','HEAD^{tree}'])
    run=os.environ['GITHUB_RUN_ID']; attempt=os.environ['GITHUB_RUN_ATTEMPT']
    source_identity(source, tree, run, attempt, os.environ.get('GITHUB_SHA', ''))
    evidence={'source':source,'tree':tree,'run':run,'attempt':attempt,'passed':False,'cleanup':False,'cases':[], 'diagnostics':[], 'cause':'UNKNOWN', 'brokerImage':topology.retained.REDPANDA}
    raw=(ROOT/'kanz/infra/kafka/topics-job.yaml').read_bytes()
    digest,count,partitions=canonical_inventory(raw)
    evidence['manifestSHA']=digest
    evidence['fixtureCapacity']=fixture_capacity(partitions, canonical_inventory(raw, True)[2])
    artifact=Path(os.environ['RUNNER_TEMP'])/'native-kafka-provision.json'
    name='redpanda-proof-'+uuid.uuid4().hex[:12]
    with tempfile.TemporaryDirectory(prefix='native-kafka-proof-') as directory:
        scratch=Path(directory); owned=False
        def k(*args,data=None,timeout=300):
            return command(['kubectl','--kubeconfig',str(scratch/'kubeconfig'),'--context','kind-'+name,*args],data,timeout)
        def apply(docs):
            return k('apply','-f','-',data=yaml.safe_dump_all(docs))
        def execute(d, failed=False, case=None, *, observe_only=False, expected_uid=None):
            ns=d['metadata']['namespace']; jobname=d['metadata']['name']
            diagnostic={'case':case or jobname,'jobState':'UNKNOWN','custody':False,'collection':'job','probe':{'state':'UNKNOWN'}}
            evidence['diagnostics'].append(diagnostic)
            deadline=time.monotonic()+250
            def observe(*args,data=None):
                remaining=deadline-time.monotonic()
                if remaining<=0: raise TimeoutError('job observation expired')
                return k(*args,data=data,timeout=min(300,remaining))
            uid=expected_uid
            try:
                if not observe_only:
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
                        label='probe' if jobname in PROBES else 'admin'
                        pod_custody(pods.get('items'),d,uid)
                        diagnostic['container']=pod_diagnostic(pods.get('items'),d,uid)
                        # Self-reported phase is advisory until the image graph is
                        # verified. Persist it even if the collector later fails.
                        diagnostic['imageVerified']=False
                        diagnostic['collection']='closed-probe'
                        selected=pods['items'][0]['metadata']
                        logs=observe('-n',ns,'logs',selected['name'],'-c',d['spec']['template']['spec']['containers'][0]['name'],'--limit-bytes=65536')
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
                try:
                    topology.retained.build_image(path, source, image)
                except RuntimeError as error:
                    print(str(error), flush=True)
                    raise
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
            try:
                evidence['nodeMemoryBefore']=runtime_diagnostic.memory_events(command(['docker','exec',name+'-control-plane','cat','/sys/fs/cgroup/memory.events'],timeout=5,stdout_limit=4096))
            except Exception as error:
                evidence['nodeMemoryBefore']={'collectionError':runtime_diagnostic.collection_error(error)}
            evidence['brokerPodsBefore']={}
            for broker in topology.BROKERS:
                pods=json.loads(k('-n','kanz-messaging','get','pods','-l','native-broker='+broker,'-o','json'))
                evidence['brokerPodsBefore'][broker]=stable_broker(broker,pods.get('items'))
            for ns in ('kanz-messaging','kanz-services','tenant-acme','tenant-bravo'):
                apply([{'apiVersion':'v1','kind':'ConfigMap','metadata':{'name':'native-topics','namespace':ns},'immutable':True,'data':{'topics-job.yaml':raw.decode()}}])
            evidence['storage']=[]
            for broker in topology.BROKERS:
                allocation=k('-n','kanz-messaging','exec','deployment/'+broker,'-c','redpanda','--','rpk','cluster','config','get','segment_fallocation_step','--ignore-profile','-X','admin.hosts=127.0.0.1:9644')
                if allocation!=str(topology.FALLOC_BYTES): raise ValueError('fixture segment allocation readback differs')
                reserve=topology.retained.storage_reserve(k('-n','kanz-messaging','exec','deployment/'+broker,'-c','redpanda','--','rpk','cluster','config','get','storage_min_free_bytes','--ignore-profile','-X','admin.hosts=127.0.0.1:9644'))
                capacity=topology.retained.storage_capacity(k('-n','kanz-messaging','exec','deployment/'+broker,'-c','redpanda','--','df','-Pk','/var/lib/redpanda/data'))
                topology.retained.require_capacity(capacity,reserve)
                evidence['storage'].append(dict(capacity,broker=broker,producerReserveBytes=reserve,segmentAllocationBytes=int(allocation)))
            evidence['stage']='discover'
            observation=parse_probe(execute(job('discover','kanz-messaging','kafka-provisioner',images['probe'],mode='discover')),'discover',source)
            cluster=observation['cluster_id']; evidence['clusterID']=cluster
            evidence['cases'].append({'case':'discover','passed':True,'observation':observation})
            principals={'acme':'User:'+topology.IDENTITIES[1], 'bravo':'User:'+topology.IDENTITIES[2], 'compute':'User:'+topology.IDENTITIES[3]}
            def cli(case,tenant,principal,mode,wrong=False):
                evidence['stage']=case
                expected='deliberately-wrong' if wrong else cluster
                args=['-mode='+mode,'-manifest=/manifest/topics-job.yaml','-manifest-sha256='+digest,'-tenant='+tenant,'-principal='+principal,'-principal-encoding=redpanda-rule-escaped-uri','-durability-profile=redpanda-raft',
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
            evidence['stage']='golden-path-infrastructure'
            evidence['lifecycleDiagnostic']={}
            with tempfile.TemporaryDirectory(dir=scratch) as nats_scratch:
                with nats_fixture.resolver(Path(nats_scratch)/'nats', ROOT) as nats:
                    evidence['natsVersions']=nats.versions
                    evidence['lifecycle']=lifecycle.qualify(k,scratch,name,images,source,digest,cluster,evidence['adminImage'],command,observe_image,evidence['lifecycleDiagnostic'],count,nats)
                    evidence['stage']='golden-path-seed'
                    evidence['natsSeedTenants']=nats.seed_portfolios(ROOT)
            cli('acme-idempotent','acme',principals['acme'],'apply')
            cli('wrong-cluster','acme',principals['acme'],'apply',True)
            for case in ('tenant-a','tenant-b','compute-a','ungranted','partial'):
                evidence['stage']=case
                ns,sa=('kanz-messaging','kafka-provisioner') if case=='partial' else topology.CLIENTS[case]
                observation=parse_probe(execute(job(case,ns,sa,images['probe'],mode=case,cluster=cluster)),case,source,cluster)
                evidence['cases'].append({'case':case,'passed':True,'observation':observation})
            cli('acme-verify','acme',principals['acme'],'verify')
            cli('bravo-verify','bravo',principals['bravo'],'verify')
            # Revoke only the owned inter-broker replication policy; Kafka TLS
            # remains reachable. Pods and their retained volumes never restart.
            for case,connected in (('quorum-healthy',topology.BROKERS),('quorum-majority',topology.BROKERS[:2]),
                                   ('quorum-minority',()),('quorum-restored',topology.BROKERS)):
                evidence['stage']=case
                apply(topology.policies(connected))
                observation=parse_probe(execute(job(case,'kanz-messaging','kafka-provisioner',images['probe'],mode=case,cluster=cluster)),case,source,cluster)
                evidence['cases'].append({'case':case,'passed':True,'observation':observation})
            evidence['stage'] = 'argo-install'
            variants = {}
            for case in SYSTEM_CASES:
                manifest = mismatched_system_manifest(raw) if case == 'system-mismatch' else raw
                account, config, hook = system_bootstrap.render(
                    manifest, 'ghcr.io/eighred/kanz-kafka-provision@'+evidence['adminImage'],
                    'native-redpanda-0.kanz-messaging.svc:9094', topology.BROKER_ID, cluster, 'redpanda-raft')
                container = hook['spec']['template']['spec']['containers'][0]
                container.update(image=images['admin'], imagePullPolicy='IfNotPresent')
                if case == 'system-verify':
                    container['args'][1] = '-mode=verify'
                if case == 'system-activate':
                    # Model an estate revision that changes the hook execution
                    # budget while retaining the canonical topic settings.
                    hook['spec']['activeDeadlineSeconds'] = 230
                    container['args'][-1] = '-timeout=200s'
                variants[case] = [account, config, hook]
            variants['system-rollback'] = variants['system-create']
            revisions = argo_bootstrap.install(k, apply, command, scratch, variants)
            evidence['argoSyncs'] = []
            evidence['argoHookDiagnostics'] = []
            prior_uid = None
            for case in ACTIVATION_CASES:
                evidence['stage'] = case
                if case in HISTORY_CASES:
                    probe = job(case, 'kanz-messaging', 'kafka-provisioner', images['probe'], mode=case, cluster=cluster)
                    observation = parse_probe(execute(probe), case, source, cluster)
                    evidence['cases'].append({'case':case,'passed':True,'observation':observation})
                    continue
                conflict = case == 'system-mismatch'
                hook = copy.deepcopy(variants[case][2])
                diagnostic = {'case': case}
                evidence['argoHookDiagnostics'].append(diagnostic)
                sync = argo_bootstrap.sync(k, case, revisions[case], diagnostic)
                hook["metadata"]["name"] = sync["jobName"]
                uid = argo_bootstrap.observe_hook(k, hook, prior_uid, diagnostic)
                if not diagnostic['resultMatches']:
                    raise ValueError('Argo PostSync result differs')
                prior_uid = uid
                result = json.loads(execute(hook, failed=conflict, case=case, observe_only=True, expected_uid=uid))
                sync['owner'] = argo_bootstrap.observe_owner(k, sync['jobName'])
                sync['jobUID'] = uid
                if result.get('version') != source or (conflict and
                    (result.get('error') != 'resource-conflict' or result.get('failure_code') != 'config-retention')) or (not conflict and result.get('error')):
                    raise ValueError('system hook outcome differs')
                validate_case_receipt(result['receipt'], case, cluster, raw)
                evidence['argoSyncs'].append(sync)
                evidence['cases'].append({'case':case,'passed':True,'receipt':result['receipt']})
            evidence['brokerPodsAfter']={}
            for broker in topology.BROKERS:
                pods=json.loads(k('-n','kanz-messaging','get','pods','-l','native-broker='+broker,'-o','json'))
                evidence['brokerPodsAfter'][broker]=stable_broker(broker,pods.get('items'),evidence['brokerPodsBefore'][broker])
            evidence['passed']=True
        except Exception as error:
            evidence['errorCategory']=type(error).__name__ if isinstance(error,(ValueError,RuntimeError,TimeoutError)) else 'unexpected'
            if owned:
                evidence['brokerHealth']=[]
                for broker in topology.BROKERS:
                    try:
                        pods=json.loads(k('-n','kanz-messaging','get','pods','-l','native-broker='+broker,'-o','json',timeout=5))
                        items=pods.get('items')
                        if not isinstance(items,list) or not 0<len(items)<=4: raise ValueError('broker pod inventory bound')
                        evidence['brokerHealth'].append({'broker':broker,'pods':[broker_health(broker,[pod]) for pod in items]})
                    except Exception as diagnostic_error:
                        evidence['brokerHealth'].append({'broker':broker,'collection':'unavailable','collectionError':runtime_diagnostic.collection_error(diagnostic_error)})
                try:
                    evidence['nodeMemoryAfter']=runtime_diagnostic.memory_events(command(['docker','exec',name+'-control-plane','cat','/sys/fs/cgroup/memory.events'],timeout=5,stdout_limit=4096))
                except Exception as diagnostic_error:
                    evidence['nodeMemoryAfter']={'collectionError':runtime_diagnostic.collection_error(diagnostic_error)}
                evidence['brokerLogClasses']=[]
                for broker in topology.BROKERS:
                    try:
                        raw=k('-n','kanz-messaging','logs','deployment/'+broker,'-c','redpanda','--tail=100','--limit-bytes=32768',timeout=5)
                        evidence['brokerLogClasses'].append({'broker':broker,**runtime_diagnostic.broker_log(raw)})
                    except Exception as diagnostic_error:
                        evidence['brokerLogClasses'].append({'broker':broker,'collectionError':runtime_diagnostic.collection_error(diagnostic_error)})
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
