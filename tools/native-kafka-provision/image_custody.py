"""Bound local imported-image identities through verified OCI bytes, never pulls."""
import hashlib
import json
import re

DIGEST = re.compile(r'sha256:[0-9a-f]{64}')
INDEX = {'application/vnd.oci.image.index.v1+json', 'application/vnd.docker.distribution.manifest.list.v2+json'}
MANIFEST = {'application/vnd.oci.image.manifest.v1+json', 'application/vnd.docker.distribution.manifest.v2+json'}
CONFIG = {'application/vnd.oci.image.config.v1+json', 'application/vnd.docker.container.image.v1+json'}


def strict(pairs):
    result = {}
    for key,value in pairs:
        if key in result: raise ValueError('duplicate image content key')
        result[key] = value
    return result


def reference_digest(value):
    if not isinstance(value,str): raise ValueError('image reference type')
    if DIGEST.fullmatch(value): return value
    if not re.fullmatch(r'[a-z0-9][a-z0-9./:_-]{0,255}@sha256:[0-9a-f]{64}',value):
        raise ValueError('unsupported image reference')
    return value.rsplit('@',1)[1]


def verify(inspection, requested, observed, docker_id, read):
    """read returns raw bounded node content. Digest types come from its graph.

    Docker's reported ID can be a config, manifest or index. It is never assumed
    to equal CRI's configuration ID. Both observed graphs must reach the exact
    same configuration and (when present) platform manifest.
    """
    if not DIGEST.fullmatch(docker_id): raise ValueError('Docker identity missing')
    if not re.fullmatch(r'kanz-native-(?:admin|probe):[0-9a-f]{40}',requested):
        raise ValueError('unowned image tag')
    status=inspection.get('status',{})
    tags=status.get('repoTags'); digests=status.get('repoDigests')
    if (not isinstance(tags,list) or not 1<=len(tags)<=16 or 'docker.io/library/'+requested not in tags or
            not isinstance(digests,list) or not 1<=len(digests)<=16 or not DIGEST.fullmatch(status.get('id',''))):
        raise ValueError('CRI tag/config custody absent')
    pod_digest=reference_digest(observed)
    if observed != status['id'] and observed not in digests:
        raise ValueError('Pod reference not bound by CRI')
    cache={};total=0
    def content(digest,size=None):
        nonlocal total
        if not DIGEST.fullmatch(digest): raise ValueError('content digest invalid')
        if digest not in cache:
            if len(cache)>=6: raise ValueError('image graph exceeds bound')
            raw=read(digest)
            if not isinstance(raw,bytes) or not 0<len(raw)<=1<<20: raise ValueError('image content exceeds bound')
            total+=len(raw)
            if total>4<<20: raise ValueError('image graph exceeds byte bound')
            if 'sha256:'+hashlib.sha256(raw).hexdigest()!=digest: raise ValueError('image content digest differs')
            value=json.loads(raw,object_pairs_hook=strict)
            if not isinstance(value,dict): raise ValueError('image content shape')
            cache[digest]=(raw,value)
        raw,value=cache[digest]
        if size is not None and (type(size) is not int or size!=len(raw)): raise ValueError('descriptor size differs')
        return value
    def descriptor(value,allowed):
        if not isinstance(value,dict) or value.get('mediaType') not in allowed or not DIGEST.fullmatch(value.get('digest','')) or type(value.get('size')) is not int or not 0<value['size']<=1<<20:
            raise ValueError('unsupported image descriptor')
        if value.get('urls'): raise ValueError('external content refused')
        return value['digest'],value['size']
    def config(digest,size=None):
        value=content(digest,size)
        if (value.get('architecture')!='amd64' or value.get('os')!='linux' or value.get('rootfs',{}).get('type')!='layers' or
                'manifests' in value or 'layers' in value or 'schemaVersion' in value):
            raise ValueError('unsupported configuration')
        diffs=value['rootfs'].get('diff_ids')
        if not isinstance(diffs,list) or not 1<=len(diffs)<=128 or any(not isinstance(x,str) or not DIGEST.fullmatch(x) for x in diffs):
            raise ValueError('configuration layer identity absent')
        return digest
    def chain(root):
        value=content(root);kind=value.get('mediaType');manifest=None
        if kind in INDEX:
            if value.get('schemaVersion')!=2: raise ValueError('index version')
            entries=value.get('manifests')
            if not isinstance(entries,list) or not 1<=len(entries)<=8: raise ValueError('index bound')
            selected=[]
            for entry in entries:
                descriptor(entry,MANIFEST)
                platform=entry.get('platform',{})
                if platform.get('os')=='linux' and platform.get('architecture')=='amd64' and not platform.get('variant'):
                    selected.append(entry)
            if len(selected)!=1: raise ValueError('ambiguous platform')
            manifest,size=descriptor(selected[0],MANIFEST);value=content(manifest,size)
            if value.get('mediaType')!=selected[0]['mediaType']: raise ValueError('manifest media type differs')
        elif kind in MANIFEST:
            manifest=root
        elif kind is None:
            return {'type':'config','config':config(root),'manifest':None}
        else: raise ValueError('unknown image content type')
        if value.get('schemaVersion')!=2 or value.get('mediaType') not in MANIFEST: raise ValueError('manifest version')
        layers=value.get('layers')
        if not isinstance(layers,list) or not 1<=len(layers)<=128: raise ValueError('manifest layers missing')
        for layer in layers:
            if not isinstance(layer,dict) or not DIGEST.fullmatch(layer.get('digest','')) or type(layer.get('size')) is not int or layer['size']<0 or layer.get('urls'):
                raise ValueError('manifest layer descriptor invalid')
        conf,size=descriptor(value.get('config'),CONFIG)
        return {'type':'index' if kind in INDEX else 'manifest','config':config(conf,size),'manifest':manifest}
    node=chain(pod_digest);host=chain(docker_id)
    if '@' in observed and node['type']=='config':
        raise ValueError('repository digest cannot name a configuration')
    if node['config']!=status['id'] or host['config']!=node['config']:
        raise ValueError('execution configuration differs')
    if node['manifest'] and host['manifest'] and node['manifest']!=host['manifest']:
        raise ValueError('platform manifest differs')
    return {'verified':True,'dockerIdentityType':host['type'],'podIdentityType':node['type'],
            'dockerIdentity':docker_id,'podIdentity':pod_digest,
            'config':node['config'],'platformManifest':node['manifest'] or host['manifest'],'contentCount':len(cache)}
