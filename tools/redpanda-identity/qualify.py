"""Disposable SPIRE uniqueid -> Redpanda DN principal qualification, never deployment."""
import hashlib
import json
import os
from pathlib import Path
import re
import sys
import tempfile
import uuid
import yaml

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / 'feed-identity'))
import proof as shared

ROOT = shared.ROOT
REDPANDA = 'docker.redpanda.com/redpandadata/redpanda:v26.2.6@sha256:8b1076e0d61bfceb66594dc01536427ce0b529f8ead35fdab9763ab6085891ce'
SAS = ('proof-bootstrap', 'proof-a', 'proof-b', 'proof-ungranted')


def identity(sa):
    if sa not in SAS:
        raise ValueError('unknown fixture identity')
    return 'spiffe://kanz.internal/ns/kanz-services/sa/' + sa


def subject(sa):
    value = hashlib.sha256(identity(sa).encode()).digest()[:16].hex()
    # SPIRE's Go x509 signer encodes this printable string as ASN.1 tag 19.
    # Unknown OIDs are serialized as DER hex by the DN libraries. Actual TLS
    # ACL successes must qualify this exact representation; no wildcard fallback.
    return '2.5.4.45=#1320' + value.encode().hex()


def subject_pattern(sa):
    # Go pkix.Name.String places unknown attributes after reversed known RDNs.
    return 'CN=[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?,O=SPIRE,C=US,' + subject(sa).replace('.', '[.]')


def broker_subject_pattern(sa):
    # Pinned Redpanda26.2.6 run42120249766 observed this named/raw-hash form.
    # Go's reconstructed numeric-OID DER representation is not the broker DN.
    value = hashlib.sha256(identity(sa).encode()).digest()[:16].hex()
    return 'C=US,O=SPIRE,CN=[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?,x500UniqueIdentifier=' + value


def broker_diagnostics(logs):
    # Consume bounded broker logs privately; emit only verified public fixture DN
    # attributes and fixed property names. Never retain arbitrary log lines.
    subjects = set()
    invalid = set()
    missing_bootstrap = False
    for line in logs.splitlines():
        if "Can't load config bootstrap file:" in line:
            missing_bootstrap = True
        for key in ('kafka_enable_authorization', 'superusers', 'kafka_mtls_principal_mapping_rules'):
            if 'Ignoring invalid property: ' + key + '=' in line:
                invalid.add(key)
        marker = 'failed to extract principal from distinguished name: '
        if marker not in line:
            continue
        dn = line.split(marker, 1)[1]
        if len(dn) > 512:
            continue
        parts = dn.split(',')
        if len(parts) != 4:
            continue
        fields = {}
        for part in parts:
            key, separator, value = part.strip().partition('=')
            if not separator or key in fields:
                break
            fields[key] = value
        else:
            if fields.pop('C', None) != 'US' or fields.pop('O', None) != 'SPIRE':
                continue
            if not re.fullmatch(r'[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?', fields.pop('CN', '')):
                continue
            if len(fields) != 1:
                continue
            key, value = next(iter(fields.items()))
            if key not in ('2.5.4.45', 'OID.2.5.4.45', 'x500UniqueIdentifier', 'UID'):
                continue
            for sa in SAS:
                digest = hashlib.sha256(identity(sa).encode()).digest()[:16].hex()
                if value in (digest, '#1320' + digest.encode().hex(), '#0c20' + digest.encode().hex()):
                    subjects.add(dn)
    return {'unmappedSubjects': sorted(subjects), 'invalidProperties': sorted(invalid),
            'missingBootstrapFile': missing_bootstrap}


def spire_objects():
    docs = shared.spire_objects()
    config = next(d for d in docs if d['kind']=='ConfigMap' and d['metadata']['name']=='spire-server')
    text = config['data']['server.conf']
    if text.count('plugins {') != 1 or 'CredentialComposer' in text:
        raise ValueError('unexpected composer configuration')
    config['data']['server.conf'] = text.replace('plugins {','plugins {\n      CredentialComposer "uniqueid" {}',1)
    return docs


def broker_objects():
    metadata = lambda name: {'name':name,'namespace':'kanz-messaging'}
    config = {'redpanda':{
      'data_directory':'/var/lib/redpanda/data','node_id':0,'seed_servers':[],
      'rpc_server':{'address':'127.0.0.1','port':33145},
      'advertised_rpc_api':{'address':'127.0.0.1','port':33145},
      'admin':[{'address':'127.0.0.1','port':9644}],
      'kafka_api':[{'address':'0.0.0.0','port':9094,'name':'identity','authentication_method':'mtls_identity'}],
      'advertised_kafka_api':[{'address':'redpanda.kanz-messaging.svc','port':9094,'name':'identity'}],
      'kafka_api_tls':[{'name':'identity','enabled':True,'require_client_auth':True,
                        'cert_file':'/certs/svid.pem','key_file':'/certs/svid_key.pem','truststore_file':'/certs/bundle.pem'}]}}
    bootstrap = {'kafka_enable_authorization':True, 'superusers':['proof-bootstrap'],
      'auto_create_topics_enabled':False,'default_topic_replications':1,
      'kafka_mtls_principal_mapping_rules':['RULE:^'+broker_subject_pattern(sa)+'$/'+sa+'/' for sa in SAS]}
    helper_config = '\n'.join(['agent_address = "/run/spiffe/spire-agent.sock"','cert_dir = "/certs"',
      'svid_file_name = "svid.pem"','svid_key_file_name = "svid_key.pem"','svid_bundle_file_name = "bundle.pem"'])
    helper = {'name':'svid','image':shared.PINS['ghcr.io/spiffe/spiffe-helper:0.9.0'],
      'args':['-config','/etc/helper/helper.conf'],
      'resources':{'requests':{'cpu':'25m','memory':'32Mi'},'limits':{'cpu':'250m','memory':'128Mi'}},
      'securityContext':{'allowPrivilegeEscalation':False,'readOnlyRootFilesystem':True,'capabilities':{'drop':['ALL']}},
      'volumeMounts':[{'name':'helper','mountPath':'/etc/helper','readOnly':True},{'name':'certs','mountPath':'/certs'}, {'name':'spiffe','mountPath':'/run/spiffe','readOnly':True}]}
    init = dict(helper, name='svid-init', args=helper['args']+['-daemon-mode=false'])
    broker = {'name':'redpanda','image':REDPANDA,'command':['/opt/redpanda/bin/redpanda'],
      'args':['--redpanda-cfg','/etc/redpanda/redpanda.yaml','--smp','1','--memory','768M','--reserve-memory','0M','--overprovisioned'],
      'resources':{'requests':{'cpu':'500m','memory':'1Gi'},'limits':{'cpu':'2','memory':'1536Mi'}},
      'securityContext':{'allowPrivilegeEscalation':False,'readOnlyRootFilesystem':True,'capabilities':{'drop':['ALL']}},
      'volumeMounts':[{'name':'config','mountPath':'/etc/redpanda','readOnly':True},{'name':'data','mountPath':'/var/lib/redpanda/data'},
                      {'name':'certs','mountPath':'/certs','readOnly':True},{'name':'tmp','mountPath':'/tmp'}],
      'readinessProbe':{'tcpSocket':{'port':9094},'initialDelaySeconds':3,'periodSeconds':2}}
    docs = shared.manifests('kanz/infra/nats/namespace.yaml') + [
      {'apiVersion':'v1','kind':'ServiceAccount','metadata':metadata('redpanda')},
      {'apiVersion':'v1','kind':'ConfigMap','metadata':metadata('redpanda-config'),
       'data':{'redpanda.yaml':yaml.safe_dump(config),'.bootstrap.yaml':yaml.safe_dump(bootstrap)}},
      {'apiVersion':'v1','kind':'ConfigMap','metadata':metadata('redpanda-helper'),'data':{'helper.conf':helper_config}},
      {'apiVersion':'v1','kind':'Service','metadata':metadata('redpanda'),
       'spec':{'selector':{'app':'redpanda'},'ports':[{'port':9094,'targetPort':9094}]}},
      {'apiVersion':'apps/v1','kind':'Deployment','metadata':metadata('redpanda'),
       'spec':{'replicas':1,'selector':{'matchLabels':{'app':'redpanda'}},'template':{'metadata':{'labels':{'app':'redpanda'}},
         'spec':{'serviceAccountName':'redpanda','automountServiceAccountToken':False,
           'securityContext':{'runAsNonRoot':True,'runAsUser':101,'runAsGroup':101,'fsGroup':101,'seccompProfile':{'type':'RuntimeDefault'}},
           'initContainers':[init],'containers':[broker,helper],
           'volumes':[{'name':'config','configMap':{'name':'redpanda-config'}},{'name':'helper','configMap':{'name':'redpanda-helper'}},
                      {'name':'certs','emptyDir':{'medium':'Memory'}},{'name':'data','emptyDir':{'sizeLimit':'2Gi'}},{'name':'tmp','emptyDir':{}},
                      {'name':'spiffe','csi':{'driver':'csi.spiffe.io','readOnly':True}}]}}}}]
    shared.pin_images(docs)
    return docs


def job(name, sa, mode, image):
    d = shared.job(name,sa,mode,'unused',image)
    pod = d['spec']['template']['spec']
    d['spec']['activeDeadlineSeconds']=180
    pod['containers'][0]['env']=[{'name':'PROOF_SA','value':sa},{'name':'PROOF_MODE','value':mode}]
    return d


def policies():
    deny=shared.policy()
    allow=shared.policy(True)
    allow['metadata']['name']='redpanda-allow'
    allow['spec']['podSelector']={'matchLabels':{'app':'redpanda'}}
    allow['spec']['ingress'][0]['ports']=[{'protocol':'TCP','port':9094}]
    return [deny,allow]


def main():
    name='redpanda-proof-'+uuid.uuid4().hex[:12]
    evidence={'passed':False,'developmentOnly':True,'cluster':name,'brokerImage':REDPANDA,'observations':[]}
    artifact=Path(os.environ['RUNNER_TEMP'])/'redpanda-identity.json'
    with tempfile.TemporaryDirectory(prefix='kanz-redpanda-identity-') as directory:
        scratch=Path(directory); kubeconfig=str(scratch/'kubeconfig')
        def k(*args,data=None):
            return shared.command(['kubectl','--kubeconfig',kubeconfig,'--context','kind-'+name,*args],data=data)
        def apply(docs):
            k('apply','-f','-',data=yaml.safe_dump_all(docs))
        try:
            evidence['stage']='build probe'
            sha=shared.command(['git','rev-parse','HEAD']); evidence['sourceSHA']=sha
            image='kanz-redpanda-proof:'+sha
            shared.command(['docker','build','--build-arg','VERSION='+sha,'-f','tools/redpanda-identity/Dockerfile','-t',image,'.'],timeout=1200)
            evidence['probeImageID']=shared.command(['docker','image','inspect','--format','{{.Id}}',image])
            config=scratch/'kind.yaml'
            config.write_text(yaml.safe_dump({'kind':'Cluster','apiVersion':'kind.x-k8s.io/v1alpha4','networking':{'disableDefaultCNI':True},'nodes':[{'role':'control-plane'}]}))
            evidence['stage']='create kind'
            shared.command(['kind','create','cluster','--name',name,'--image',shared.NODE,'--config',str(config),'--kubeconfig',kubeconfig,'--wait','0s'])
            evidence['stage']='install Cilium'
            shared.command(['helm','install','cilium',str(Path(os.environ['RUNNER_TEMP'])/'cilium.tgz'),'--kubeconfig',kubeconfig,'--kube-context','kind-'+name,'--namespace','kube-system','--set','ipam.mode=kubernetes','--set','operator.replicas=1','--wait','--timeout','5m'],timeout=360)
            k('wait','--for=condition=Ready','node','--all','--timeout=180s')
            shared.command(['kind','load','docker-image',image,'--name',name])
            evidence['stage']='install SPIRE'
            crds=[]
            for path in sorted((ROOT/'kanz/infra/security/spire/crds').glob('*.yaml')):
                crds+=shared.manifests(str(path.relative_to(ROOT)))
            apply(crds)
            for crd in crds: k('wait','--for=condition=Established','crd/'+crd['metadata']['name'],'--timeout=90s')
            apply(spire_objects())
            k('-n','spire-system','rollout','status','statefulset/spire-server','--timeout=240s')
            k('-n','spire-system','rollout','status','daemonset/spire-agent','--timeout=240s')
            registration=shared.manifests('kanz/infra/security/spire/registration.yaml')
            # Short-lived client SVIDs make actual renewal mandatory within the job bound.
            registration[0]['spec']['ttl']='60s'
            apply(registration)
            evidence['stage']='install Redpanda'
            apply(broker_objects())
            apply(policies())
            k('-n','kanz-messaging','rollout','status','deployment/redpanda','--timeout=240s')
            for sa in SAS: apply([{'apiVersion':'v1','kind':'ServiceAccount','metadata':{'name':sa,'namespace':'kanz-services'}}])
            for case,sa,mode in [('bootstrap','proof-bootstrap','bootstrap'),('client-a','proof-a','publish'),('client-b','proof-b','publish'),('ungranted','proof-ungranted','ungranted'),('verify','proof-bootstrap','verify')]:
                evidence['stage']='probe '+case
                apply([job(case,sa,mode,image)])
                k('-n','kanz-services','wait','--for=condition=complete','job/'+case,'--timeout=190s')
                result=json.loads(k('-n','kanz-services','logs','job/'+case))
                dn=result.get('subjectDN','')
                if not isinstance(dn,str) or not re.fullmatch(subject_pattern(sa),dn):
                    raise ValueError('unexpected observed workload subject')
                if result!={'passed':True,'mode':mode,'identity':identity(sa),'subjectDN':dn,'renewed':mode=='publish','sourceVersion':sha}:
                    raise ValueError('missing exact positive identity evidence')
                evidence['observations'].append(dict(result,case=case))
            pods=json.loads(k('get','pods','-A','-o','json'))['items']
            evidence['runningImages']=[{'namespace':p['metadata']['namespace'],'pod':p['metadata']['name'],'images':[{'name':c['name'],'imageID':c.get('imageID','')} for c in p.get('status',{}).get('containerStatuses',[])]} for p in pods]
            evidence['passed']=True
        except Exception as error:
            evidence['failureStage']=evidence.get('stage','initialization'); evidence['errorType']=type(error).__name__
            if evidence['failureStage'].startswith('probe '):
                try:
                    logs=k('-n','kanz-services','logs','job/'+case)
                    match=re.fullmatch(r'PROBE_FAILED_STAGE:([a-z-]+) CLASS:(kafka--?[0-9]{1,5}|tls-certificate|timeout|eof|unexpected-success|other)',logs)
                    allowed={'configuration','svid','kafka','tls-handshake','bootstrap-topics','bootstrap-acls','own-topic','own-topic-direct','cross-topic-write','cross-topic-read','acl-admin-denial','renewal','renewed-topic','renewed-topic-denial','ungranted-denial','ungranted-write','ungranted-read','exact-records'}
                    if match and match[1] in allowed:
                        evidence['probeFailureStage']=match[1]
                        evidence['probeFailureClass']=match[2]
                except Exception:
                    pass  # Diagnostic collection can never change the failed verdict.
            try:
                logs=k('-n','kanz-messaging','logs','deployment/redpanda','-c','redpanda','--tail=200','--limit-bytes=65536')
                evidence['brokerDiagnostics']=broker_diagnostics(logs)
            except Exception:
                pass  # Optional public diagnosis cannot turn a failure into success.
            raise
        finally:
            try:
                shared.command(['kind','delete','cluster','--name',name,'--kubeconfig',kubeconfig],timeout=180)
                evidence['cleanupPassed']=name not in shared.command(['kind','get','clusters']).splitlines()
            except Exception as error:
                evidence['cleanupPassed']=False; evidence['cleanupErrorType']=type(error).__name__
            evidence['passed'] &= evidence['cleanupPassed']
            artifact.write_text(json.dumps(evidence,indent=2)+'\n')
    if not evidence['passed']: raise RuntimeError('qualification failed')


if __name__=='__main__': main()
