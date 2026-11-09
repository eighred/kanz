"""Disposable development evidence; never applies to an existing Kubernetes context."""
import copy
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile
import uuid
import yaml

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / 'tools/gateway-policy-proof'))
import bounded_process
NODE = 'kindest/node:v1.36.4@sha256:099e049362a1526b2db71494e1947aae99bd16290d7c895f2b7ea312e3cbfaed'
PINS = {
 'busybox:1.36': 'busybox:1.36@sha256:73aaf090f3d85aa34ee199857f03fa3a95c8ede2ffd4cc2cdb5b94e566b11662',
 'registry.k8s.io/sig-storage/csi-node-driver-registrar:v2.10.0': 'registry.k8s.io/sig-storage/csi-node-driver-registrar:v2.10.0@sha256:c53535af8a7f7e3164609838c4b191b42b2d81238d75c1b2a2b582ada62a9780',
 'ghcr.io/spiffe/spiffe-helper:0.9.0': 'ghcr.io/spiffe/spiffe-helper:0.9.0@sha256:47f18fcd2bf1c99cbb9bdc6f4348b08fb74572f89ddd0bcf103de2a1b12ab6d9',
 'nats:2.10-alpine': 'nats:2.12.6-alpine@sha256:1cfc36e2e5e638243d8c722f72c954cd0ec4b15ee82fadbc718ce12e2b3c1652',
}


def command(args, data=None, timeout=300):
    # Capture all subprocess output. Never dump raw pod/control-plane logs on failure.
    result = subprocess.run(args, input=data, text=True, capture_output=True, timeout=timeout, cwd=ROOT)
    if result.returncode:
        raise RuntimeError(f'fixture command failed: {args[0]} {args[1]} exit={result.returncode}')
    return result.stdout.strip()


BUILD_FAILURE_SIGNALS = (
    (b"toomanyrequests", "registry rate limit"),
    (b"429 too many requests", "registry rate limit"),
    (b"no space left on device", "runner disk exhausted"),
    (b"401 unauthorized", "registry authentication refused"),
    (b"403 forbidden", "registry access refused"),
    (b"failed to fetch anonymous token", "anonymous registry token failed"),
    (b"failed to fetch oauth token", "registry token failed"),
    (b"not found", "missing build input"),
    (b"failed to load cache key", "build input resolution failed"),
    (b"no match for platform", "missing platform"),
    (b"failed to resolve source metadata", "image metadata resolution failed"),
    (b"docker.io/docker/dockerfile", "Docker Hub frontend"),
    (b"ghcr.io/eighred/base/golang", "mirrored Go base"),
    (b"ghcr.io/eighred/base/distroless", "mirrored runtime base"),
    (b"tls handshake timeout", "TLS handshake timeout"),
    (b"unexpected eof", "transport EOF"),
    (b"i/o timeout", "network timeout"),
    (b"checksum mismatch", "checksum mismatch"),
)


def build_image(path, source, image):
    try:
        bounded_process.run(
            ['docker', 'build', '--progress=plain', '--build-arg', 'VERSION='+source, '-f', path, '-t', image, '.'],
            cwd=ROOT, env=dict(os.environ), timeout=1200, stderr_limit=4 << 20,
            failure_signals=BUILD_FAILURE_SIGNALS)
    except RuntimeError as error:
        # The bounded runner emits only fixed categories, never build output.
        print(f"Fixture build {path}: {error}", flush=True)
        raise


def manifests(relative):
    return [d for d in yaml.safe_load_all((ROOT / relative).read_text()) if d]


def pin_images(value):
    if isinstance(value, dict):
        for key, item in value.items():
            if key == 'image':
                value[key] = PINS.get(item, item)
                if not re.search(r'@sha256:[0-9a-f]{64}$', value[key]):
                    raise ValueError('unreviewed fixture image')
            else:
                pin_images(item)
    elif isinstance(value, list):
        for item in value:
            pin_images(item)


def spire_objects():
    docs = []
    for name in ('namespaces.yaml', 'rbac.yaml', 'spire-server.yaml', 'spire-agent.yaml'):
        docs += manifests('kanz/infra/security/spire/' + name)
    for d in docs:
        if d['kind'] == 'StatefulSet':
            # Disposable trust domain, no production CA/key mounts or persistence.
            d['spec'].pop('volumeClaimTemplates')
            d['spec']['template']['spec']['volumes'].append({'name':'spire-data','emptyDir':{'medium':'Memory'}})
        pin_images(d)
    return docs


def broker_objects():
    canonical = manifests('kanz/infra/nats/nats.yaml')
    config = copy.deepcopy(next(d for d in canonical if d['kind']=='ConfigMap' and d['metadata']['name']=='nats-config'))
    body = config['data']['nats.conf']
    for required in ('verify: true', 'verify_and_map: true', 'include "tenants.conf"'):
        if required not in body:
            raise ValueError('canonical TLS identity boundary changed')
    # Single-node fixture has no cluster routes. Preserve canonical TLS/include/JS.
    start = body.index('cluster {')
    end = body.index('\n}', start) + 2
    config['data']['nats.conf'] = body[:start] + body[end:]
    helper = copy.deepcopy(next(d for d in canonical if d['kind']=='ConfigMap' and d['metadata']['name']=='nats-spiffe-helper'))
    users = []
    for sa in ('binance-depth','okx-depth','identity-bootstrap'):
        subjects = ['market.book.snapshot','market.crypto.quote'] if sa != 'identity-bootstrap' else ['$JS.API.STREAM.CREATE.FIXTURE','$JS.API.STREAM.INFO.FIXTURE']
        users.append({'user':'spiffe://kanz.internal/ns/kanz-services/sa/'+sa,
                      'permissions':{'publish':{'allow':subjects},'subscribe':{'allow':['_INBOX.>']}}})
    tenants = {'apiVersion':'v1','kind':'ConfigMap','metadata':{'name':'nats-tenants','namespace':'kanz-messaging'},
               'data':{'tenants.conf':'accounts: { FIXTURE: '+json.dumps({'jetstream':'enabled','users':users})+' }'}}
    # NATS configuration's enabled token is not a string.
    tenants['data']['tenants.conf'] = tenants['data']['tenants.conf'].replace('"jetstream": "enabled"','"jetstream": enabled')
    docs = manifests('kanz/infra/nats/namespace.yaml') + [config, helper, tenants]
    template = next(d for d in canonical if d['kind']=='StatefulSet')
    for name in ('nats','nats-wrong'):
        docs.append({'apiVersion':'v1','kind':'ServiceAccount','metadata':{'name':name,'namespace':'kanz-messaging'}})
        d = copy.deepcopy(template)
        d['metadata']['name'] = name
        d['spec']['replicas'] = 1
        d['spec']['serviceName'] = name
        d['spec']['selector']['matchLabels'] = {'app':name}
        d['spec'].pop('volumeClaimTemplates')
        pod = d['spec']['template']
        pod['metadata']['labels'] = {'app':name,'identity-proof-broker':'true'}
        pod['spec']['serviceAccountName'] = name
        pod['spec'].pop('affinity',None)
        pod['spec']['volumes'].append({'name':'data','emptyDir':{}})
        pin_images(d)
        docs += [d, {'apiVersion':'v1','kind':'Service','metadata':{'name':name,'namespace':'kanz-messaging'},
                     'spec':{'selector':{'app':name},'ports':[{'port':4222,'targetPort':4222}]}}]
    return docs


def job(name, sa, mode, address, image, allowed=True):
    return {'apiVersion':'batch/v1','kind':'Job','metadata':{'name':name,'namespace':'kanz-services'},
      'spec':{'backoffLimit':0,'activeDeadlineSeconds':90,'template':{'metadata':{'labels':{'identity-proof-client':str(allowed).lower()}},
        'spec':{'serviceAccountName':sa,'restartPolicy':'Never','automountServiceAccountToken':False,
          'securityContext':{'runAsNonRoot':True,'runAsUser':65532,'seccompProfile':{'type':'RuntimeDefault'}},
          'containers':[{'name':'probe','image':image,'imagePullPolicy':'IfNotPresent',
            'env':[{'name':'IDENTITY_PROOF_IDENTITY','value':'spiffe://kanz.internal/ns/kanz-services/sa/'+sa},{'name':'IDENTITY_PROOF_VENUE','value':'OKX' if sa=='okx-depth' else 'BINANCE'},{'name':'IDENTITY_PROOF_MODE','value':mode},{'name':'IDENTITY_PROOF_NATS','value':'tls://'+address}],
            'resources':{'requests':{'cpu':'50m','memory':'64Mi'},'limits':{'cpu':'1','memory':'256Mi'}},
            'securityContext':{'allowPrivilegeEscalation':False,'readOnlyRootFilesystem':True,'capabilities':{'drop':['ALL']}},
            'volumeMounts':[{'name':'spiffe','mountPath':'/run/spiffe','readOnly':True}]}],
          'volumes':[{'name':'spiffe','csi':{'driver':'csi.spiffe.io','readOnly':True}}]}}}}


def policy(allow=False):
    spec = {'podSelector':{},'policyTypes':['Ingress'],'ingress':[]}
    if allow:
        spec = {'podSelector':{'matchLabels':{'identity-proof-broker':'true'}},'policyTypes':['Ingress'],
          'ingress':[{'from':[{'namespaceSelector':{'matchLabels':{'kubernetes.io/metadata.name':'kanz-services'}},
                              'podSelector':{'matchLabels':{'identity-proof-client':'true'}}}],
                      'ports':[{'protocol':'TCP','port':4222}]}]}
    return {'apiVersion':'networking.k8s.io/v1','kind':'NetworkPolicy',
            'metadata':{'name':'feed-allow' if allow else 'default-deny','namespace':'kanz-messaging'},'spec':spec}


def create_enforcing_cluster(name, scratch):
    """Shared disposable kind/Cilium setup; accepts no existing context."""
    if not re.fullmatch(r"(?:feed-proof|cni-proof|redpanda-proof|messaging-proof)-[0-9a-f]{12}", name):
        raise ValueError("invalid disposable cluster name")
    kubeconfig = str(scratch/'kubeconfig')
    def k(*args):
        return command(['kubectl','--kubeconfig',kubeconfig,'--context','kind-'+name,*args])
    config = {'kind':'Cluster','apiVersion':'kind.x-k8s.io/v1alpha4','networking':{'disableDefaultCNI':True},'nodes':[{'role':'control-plane'}]}
    config_path = scratch/'kind.yaml'; config_path.write_text(yaml.safe_dump(config))
    command(['kind','create','cluster','--name',name,'--image',NODE,'--config',str(config_path),'--kubeconfig',kubeconfig,'--wait','0s'],timeout=300)
    command(['helm','install','cilium',str(Path(os.environ['RUNNER_TEMP'])/'cilium.tgz'),'--kubeconfig',kubeconfig,'--kube-context','kind-'+name,
             '--namespace','kube-system','--set','ipam.mode=kubernetes','--set','operator.replicas=1','--wait','--timeout','5m'],timeout=360)
    k('-n','kube-system','rollout','status','daemonset/cilium','--timeout=180s')
    k('wait','--for=condition=Ready','node','--all','--timeout=180s')


def main():
    name = 'feed-proof-' + uuid.uuid4().hex[:12]
    artifact = Path(os.environ['RUNNER_TEMP']) / 'feed-identity.json'
    evidence = {'developmentOnly':True,'passed':False,'cluster':name,'nodeImage':NODE,'observations':[]}
    with tempfile.TemporaryDirectory(prefix='kanz-feed-identity-') as directory:
        scratch = Path(directory)
        kubeconfig = str(scratch/'kubeconfig')
        def k(*args, data=None):
            return command(['kubectl','--kubeconfig',kubeconfig,'--context','kind-'+name,*args],data=data)
        def apply(docs):
            evidence['stage']='apply '+','.join(d['kind']+'/'+d['metadata']['name'] for d in docs)
            k('apply','-f','-',data=yaml.safe_dump_all(docs))
        def probe(label, sa, mode, address, allowed=True):
            apply([job(label,sa,mode,address,image,allowed)])
            evidence['stage']='probe '+label
            k('-n','kanz-services','wait','--for=condition=complete','job/'+label,'--timeout=100s')
            logs = k('-n','kanz-services','logs','job/'+label)
            if 'IDENTITY_PROOF_PASS:'+mode not in logs or '1 passed' not in logs:
                raise ValueError('missing positive Rust test evidence')
            evidence['observations'].append({'case':label,'serviceAccount':sa,'mode':mode,'passed':True})
        try:
            evidence['stage']='build fixture'
            sha = command(['git','rev-parse','HEAD'])
            evidence['sourceSHA'] = sha
            image = 'kanz-feed-proof:'+sha
            command(['docker','build','-f','tools/feed-identity/Dockerfile','-t',image,'.'],timeout=1200)
            evidence['fixtureDockerID'] = command(['docker','image','inspect','--format','{{.Id}}',image])
            evidence['stage']='create enforcing kind/Cilium cluster'
            create_enforcing_cluster(name, scratch)
            command(['kind','load','docker-image',image,'--name',name])
            crds=[]
            for path in sorted((ROOT/'kanz/infra/security/spire/crds').glob('*.yaml')):
                crds += manifests(str(path.relative_to(ROOT)))
            apply(crds)
            for crd in crds:
                k('wait','--for=condition=Established','crd/'+crd['metadata']['name'],'--timeout=90s')
            evidence['stage']='install SPIRE'
            apply(spire_objects())
            evidence['stage']='wait SPIRE server/agent/CSI'
            k('-n','spire-system','rollout','status','statefulset/spire-server','--timeout=240s')
            k('-n','spire-system','rollout','status','daemonset/spire-agent','--timeout=240s')
            apply(manifests('kanz/infra/security/spire/registration.yaml'))
            evidence['stage']='install NATS brokers'
            apply(broker_objects())
            for broker in ('nats','nats-wrong'):
                evidence['stage']='wait NATS '+broker
                k('-n','kanz-messaging','rollout','status','statefulset/'+broker,'--timeout=240s')
            for sa in ('binance-depth','okx-depth','identity-bootstrap','unregistered-feed'):
                apply([{'apiVersion':'v1','kind':'ServiceAccount','metadata':{'name':sa,'namespace':'kanz-services'}}])
            addr = k('-n','kanz-messaging','get','service/nats','-o','jsonpath={.spec.clusterIP}')+':4222'
            wrong = k('-n','kanz-messaging','get','service/nats-wrong','-o','jsonpath={.spec.clusterIP}')+':4222'
            probe('baseline','binance-depth','reachable',addr,False)
            apply([policy()])
            probe('deny','binance-depth','blocked',addr,False)
            apply([policy(True)])
            probe('allowed','binance-depth','reachable',addr)
            probe('still-denied','binance-depth','blocked',addr,False)
            probe('bootstrap','identity-bootstrap','bootstrap',addr)
            probe('binance','binance-depth','publish',addr)
            probe('okx','okx-depth','publish',addr)
            previous_auth_errors=k('-n','kanz-messaging','logs','nats-0','-c','nats').lower().count('authentication error')
            probe('unregistered','unregistered-feed','unregistered',addr)
            current_auth_errors=k('-n','kanz-messaging','logs','nats-0','-c','nats').lower().count('authentication error')
            if current_auth_errors <= previous_auth_errors:
                raise ValueError('missing new explicit broker authentication denial')
            evidence['unregisteredBrokerAuthenticationDenied']=True
            probe('wrong-server','binance-depth','wrong-server',wrong)
            probe('forbidden','binance-depth','forbidden',addr)
            logs = k('-n','kanz-messaging','logs','nats-0','-c','nats')
            if not re.search(r'Publish.*fixture\.forbidden|fixture\.forbidden.*Publish',logs,re.I):
                raise ValueError('missing explicit broker permission denial')
            evidence['forbiddenSubjectDeniedByBroker'] = True
            probe('verify','identity-bootstrap','verify',addr)
            pods=json.loads(k('get','pods','-A','-o','json'))['items']
            evidence['runningImages']=[{'namespace':p['metadata']['namespace'],'pod':p['metadata']['name'],
               'images':[{'name':c['name'],'imageID':c.get('imageID','')} for c in p.get('status',{}).get('containerStatuses',[])]} for p in pods]
            evidence['passed']=True
        except Exception as error:
            evidence['failureStage']=evidence.get('stage','initialization')
            evidence['errorType']=type(error).__name__
            raise
        finally:
            try:
                command(['kind','delete','cluster','--name',name,'--kubeconfig',kubeconfig],timeout=180)
                remaining=command(['kind','get','clusters']).splitlines()
                evidence['cleanupPassed']=name not in remaining
            except Exception as error:
                evidence['cleanupPassed']=False
                evidence['cleanupErrorType']=type(error).__name__
            evidence['passed'] &= evidence['cleanupPassed']
            artifact.write_text(json.dumps(evidence,indent=2)+'\n')
    if not evidence['passed']:
        raise RuntimeError('fixture did not pass')


if __name__ == '__main__':
    main()
