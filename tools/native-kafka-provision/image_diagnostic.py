"""Manual non-main imported-image diagnosis; never native qualification."""
import json
import os
from pathlib import Path
import tempfile
import time
import uuid

import yaml
import qualify as owner


def inert_job(label, image):
    if label not in ('admin','probe'): raise ValueError('unknown inert image')
    # Reuse the strict existing pod security/resources, removing every workload
    # identity and manifest capability. These binaries exit before any domain IO.
    d=owner.topology.retained.shared.job('image-'+label,'default','unused','unused',image)
    d['metadata']['namespace']='default'
    d['spec']['activeDeadlineSeconds']=240
    pod=d['spec']['template']['spec']
    pod['volumes']=[]
    container=pod['containers'][0]
    container['volumeMounts']=[]
    container['env']=[]
    if label=='admin': container['args']=['-mode=unsupported']
    return d


def invocation(event, ref):
    if event!='workflow_dispatch' or not ref.startswith('refs/heads/') or ref=='refs/heads/main':
        raise ValueError('non-main manual diagnosis required')


def inert_result(label, text, source):
    if label=='admin':
        value=json.loads(text,object_pairs_hook=owner.image_custody.strict)
        if set(value)!={'version','receipt','error'} or value['version']!=source or value['error']!='input-refused':
            raise ValueError('inert admin did not refuse input')
        receipt=value['receipt']
        expected={'expected_cluster_id':'','mode':'','manifest_sha256':'','verified_topics':0,'missing_topics':0,
                  'verified_acls':0,'topic_attempts':0,'acl_attempts':0,'partial_or_unknown':False,'complete':False}
        if receipt!=expected: raise ValueError('inert admin reported domain effects')
        return 'input-refused'
    if label=='probe':
        lines=text.splitlines()
        skips=[line for line in lines if line.startswith('--- SKIP: TestNativeBrokerQualification (') and line.endswith('s)')]
        if len(skips)!=1 or lines.count('PASS')!=1 or any(line.startswith(('NATIVE_PROOF:','NATIVE_DIAGNOSTIC:','--- FAIL:','--- PASS:')) for line in lines):
            raise ValueError('inert probe did not explicitly skip')
        return 'explicit-skip'
    raise ValueError('unknown inert result')


def validate(value):
    if value.get('diagnosticOnly') is not True or value.get('qualification') is not False or value.get('cleanup') is not True:
        raise ValueError('diagnostic boundary missing')
    cases=value.get('images')
    if not isinstance(cases,list) or [x.get('case') for x in cases]!=['admin','probe']:
        raise ValueError('inert inventory incomplete')
    for item in cases:
        if item.get('custody') is not True or item.get('imageVerified') is not True or item.get('image',{}).get('verified') is not True:
            raise ValueError('image custody incomplete')
        if item.get('inertResult')!=('input-refused' if item['case']=='admin' else 'explicit-skip'):
            raise ValueError('inert execution not observed')
        expected=1 if item['case']=='admin' else 0
        if item.get('jobState')!=('Failed' if expected else 'Complete') or item.get('container',{}).get('exitCode')!=expected:
            raise ValueError('inert outcome differs')


def main():
    invocation(os.environ.get('GITHUB_EVENT_NAME',''),os.environ.get('GITHUB_REF',''))
    source=owner.command(['git','rev-parse','HEAD']);tree=owner.command(['git','rev-parse','HEAD^{tree}'])
    run=os.environ['GITHUB_RUN_ID'];attempt=os.environ['GITHUB_RUN_ATTEMPT']
    owner.source_identity(source,tree,run,attempt,os.environ.get('GITHUB_SHA',''))
    evidence={'source':source,'tree':tree,'run':run,'attempt':attempt,'diagnosticOnly':True,
              'qualification':False,'diagnosticComplete':False,'cleanup':False,'images':[],'stage':'build'}
    artifact=Path(os.environ['RUNNER_TEMP'])/'native-image-diagnostic.json'
    def persist(): artifact.write_text(json.dumps(evidence,sort_keys=True)+'\n',encoding='utf-8')
    persist()
    deadline=time.monotonic()+900
    def call(args,data=None,limit=300):
        remaining=deadline-time.monotonic()
        if remaining<=0: raise TimeoutError('diagnostic budget expired')
        return owner.command(args,data,timeout=min(limit,remaining))
    name='redpanda-proof-'+uuid.uuid4().hex[:12]
    with tempfile.TemporaryDirectory(prefix='native-image-diagnostic-') as directory:
        scratch=Path(directory);owned=False
        def k(*args,data=None):
            return call(['kubectl','--kubeconfig',str(scratch/'kubeconfig'),'--context','kind-'+name,*args],data)
        try:
            images={}
            for label,path in (('admin','kanz/cmd/kanz-kafka-provision/Dockerfile'),('probe','tools/native-kafka-provision/Dockerfile')):
                tag='kanz-native-'+label+':'+source
                call(['docker','build','--build-arg','VERSION='+source,'-f',path,'-t',tag,'.'],limit=900)
                images[label]=(tag,call(['docker','image','inspect','--format','{{.Id}}',tag]))
            evidence['stage']='cluster';persist()
            if name in call(['kind','get','clusters']).splitlines() or (scratch/'kubeconfig').exists():
                raise ValueError('cluster collision')
            claim=Path(os.environ['RUNNER_TEMP'])/'native-kafka-claim.json'
            with claim.open('x',encoding='utf-8') as stream:
                json.dump({'source':source,'run':run,'attempt':attempt,'cluster':name},stream)
                stream.flush();os.fsync(stream.fileno())
            owned=True
            # No SPIRE, broker, workload credentials or domain service is created.
            call(['kind','create','cluster','--name',name,'--image',owner.topology.retained.shared.NODE,
                  '--kubeconfig',str(scratch/'kubeconfig'),'--wait','120s'])
            for tag,_ in images.values():call(['kind','load','docker-image',tag,'--name',name])
            for label,(tag,built) in images.items():
                evidence['stage']=label
                item={'case':label,'custody':False,'imageVerified':False,'jobState':'UNKNOWN','builtImage':built}
                evidence['images'].append(item);persist()
                d=inert_job(label,tag);uid=None
                case_deadline=min(deadline,time.monotonic()+250)
                def observe(*args,data=None):
                    remaining=case_deadline-time.monotonic()
                    if remaining<=0:raise TimeoutError('inert observation expired')
                    return owner.command(['kubectl','--kubeconfig',str(scratch/'kubeconfig'),'--context','kind-'+name,*args],data,timeout=min(300,remaining))
                try:
                    observe('apply','-f','-',data=yaml.safe_dump(d))
                    while time.monotonic()<case_deadline:
                        state=json.loads(observe('get','job',d['metadata']['name'],'-o','json'))
                        uid=owner.job_custody(state,d,uid);item['custody']=True
                        terminal=[c['type'] for c in state.get('status',{}).get('conditions',[]) if c.get('status')=='True' and c.get('type') in ('Complete','Failed')]
                        if terminal:
                            item['jobState']=terminal[0] if len(terminal)==1 else 'UNKNOWN'
                            pods=json.loads(observe('get','pods','-l','batch.kubernetes.io/controller-uid='+uid,'-o','json'))
                            pod=owner.pod_custody(pods.get('items'),d,uid)
                            item['container']=owner.pod_diagnostic(pods['items'],d,uid)
                            selected=pod['metadata']
                            logs=observe('logs',selected['name'],'-c','probe','--limit-bytes=65536')
                            after=json.loads(observe('get','pod',selected['name'],'-o','json'))
                            owner.pod_custody([after],d,uid,selected['uid'])
                            item['inertResult']=inert_result(label,logs,source)
                            item['image']=owner.observe_image(pod,d,built,name,case_deadline,item)
                            item['imageVerified']=True
                            item['container']['imageBound']=True
                            break
                        time.sleep(1)
                    else: raise TimeoutError('inert job did not terminate')
                finally: persist()
            evidence['diagnosticComplete']=True
        except Exception as error:
            evidence['errorCategory']=owner.image_custody.failure(error)
        finally:
            if owned:
                try:
                    owner.command(['kind','delete','cluster','--name',name,'--kubeconfig',str(scratch/'kubeconfig')],timeout=180)
                    evidence['cleanup']=name not in owner.command(['kind','get','clusters']).splitlines()
                except Exception: evidence['cleanup']=False
            if evidence['diagnosticComplete']:
                try: validate(evidence)
                except ValueError:
                    evidence['diagnosticComplete']=False;evidence['errorCategory']='report_refused'
            persist()
    if not evidence['diagnosticComplete']:raise RuntimeError('image diagnostic incomplete')


if __name__=='__main__':main()
