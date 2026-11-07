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

sys.path.insert(0, str(topology.ROOT / 'tools/gateway-policy-proof'))
import bounded_process

ROOT = topology.ROOT
CASES = ('discover', 'acme-apply', 'acme-idempotent', 'bravo-apply', 'compute-apply',
         'wrong-cluster', 'tenant-a', 'tenant-b', 'compute-a', 'ungranted', 'partial',
         'acme-verify', 'bravo-verify')


def command(argv, data=None, timeout=300):
    return bounded_process.run(argv, cwd=ROOT, env=dict(os.environ), data=data, timeout=timeout,
                               stdout_limit=4 << 20, stderr_limit=4 << 20)


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
    if value != expected or not re.fullmatch(r'[A-Za-z0-9_-]{1,128}', value.get('cluster_id','')):
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


def validate_report(value, source, tree, run, attempt, manifest):
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
    if not re.fullmatch(r'[0-9a-f]{64}',value.get('manifestSHA','')) or not re.fullmatch(r'[A-Za-z0-9_-]{1,128}',value.get('clusterID','')):
        raise ValueError('resource custody absent')


def main():
    source=command(['git','rev-parse','HEAD']); tree=command(['git','rev-parse','HEAD^{tree}'])
    run=os.environ['GITHUB_RUN_ID']; attempt=os.environ['GITHUB_RUN_ATTEMPT']
    if not re.fullmatch(r'[0-9a-f]{40}',source) or not re.fullmatch(r'[0-9a-f]{40}',tree) or not run.isdecimal() or not attempt.isdecimal():
        raise ValueError('source/run identity required')
    evidence={'source':source,'tree':tree,'run':run,'attempt':attempt,'passed':False,'cleanup':False,'cases':[], 'brokerImage':topology.retained.REDPANDA}
    artifact=Path(os.environ['RUNNER_TEMP'])/'native-kafka-provision.json'
    name='redpanda-proof-'+uuid.uuid4().hex[:12]
    with tempfile.TemporaryDirectory(prefix='native-kafka-proof-') as directory:
        scratch=Path(directory); owned=False
        def k(*args,data=None,timeout=300):
            return command(['kubectl','--kubeconfig',str(scratch/'kubeconfig'),'--context','kind-'+name,*args],data,timeout)
        def apply(docs):
            return k('apply','-f','-',data=yaml.safe_dump_all(docs))
        def execute(d, failed=False):
            apply([d]); ns=d['metadata']['namespace']; jobname=d['metadata']['name']
            deadline=time.monotonic()+250
            while time.monotonic()<deadline:
                state=json.loads(k('-n',ns,'get','job',jobname,'-o','json'))
                conditions=state.get('status',{}).get('conditions',[])
                terminal=[c['type'] for c in conditions if c.get('status')=='True' and c.get('type') in ('Complete','Failed')]
                if terminal:
                    if terminal != ['Failed' if failed else 'Complete']:
                        raise ValueError('unexpected actual job outcome')
                    return k('-n',ns,'logs','job/'+jobname,'--limit-bytes=65536')
                time.sleep(1)
            raise TimeoutError('fixture job did not terminate')
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
