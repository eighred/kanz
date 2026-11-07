"""Bound local imported-image identities through verified OCI bytes, never pulls."""
import hashlib
import json
import re

DIGEST = re.compile(r'sha256:[0-9a-f]{64}')
INDEX = {'application/vnd.oci.image.index.v1+json', 'application/vnd.docker.distribution.manifest.list.v2+json'}
MANIFEST = {'application/vnd.oci.image.manifest.v1+json', 'application/vnd.docker.distribution.manifest.v2+json'}
CONFIG = {'application/vnd.oci.image.config.v1+json', 'application/vnd.docker.container.image.v1+json'}


CODES = frozenset({
    'duplicate_image_content_key',
    'image_reference_type',
    'unsupported_image_reference',
    'docker_identity_missing',
    'unowned_image_tag',
    'cri_tag_config_custody_absent',
    'pod_reference_not_bound_by_cri',
    'content_digest_invalid',
    'image_graph_exceeds_bound',
    'image_content_exceeds_bound',
    'image_graph_exceeds_byte_bound',
    'image_content_digest_differs',
    'image_content_shape',
    'descriptor_size_differs',
    'unsupported_image_descriptor',
    'external_content_refused',
    'unsupported_configuration',
    'configuration_layer_identity_absent',
    'index_version',
    'index_bound',
    'ambiguous_platform',
    'manifest_media_type_differs',
    'unknown_image_content_type',
    'manifest_version',
    'manifest_layers_missing',
    'manifest_layer_descriptor_invalid',
    'repository_digest_cannot_name_a_configuration',
    'execution_configuration_differs',
    'platform_manifest_differs',
    'content_json_invalid',
})

class CustodyError(ValueError):
    """Only fixed local predicate codes; never exception/CRI/content text."""
    def __init__(self, code):
        if code not in CODES: raise ValueError('unknown custody code')
        self.code = code
        super().__init__(code)


def failure(error):
    if isinstance(error,CustodyError): return error.code
    if isinstance(error,TimeoutError): return 'observation_timeout'
    return 'collector_failure'


def strict(pairs):
    result = {}
    for key,value in pairs:
        if key in result: raise CustodyError('duplicate_image_content_key')
        result[key] = value
    return result


def reference_digest(value):
    if not isinstance(value,str): raise CustodyError('image_reference_type')
    if DIGEST.fullmatch(value): return value
    if not re.fullmatch(r'[a-z0-9][a-z0-9./:_-]{0,255}@sha256:[0-9a-f]{64}',value):
        raise CustodyError('unsupported_image_reference')
    return value.rsplit('@',1)[1]


def verify(inspection, requested, observed, docker_id, read, progress=None):
    """read returns raw bounded node content. Digest types come from its graph.

    Docker's reported ID can be a config, manifest or index. It is never assumed
    to equal CRI's configuration ID. Both observed graphs must reach the exact
    same configuration and (when present) platform manifest.
    """
    progress = progress if progress is not None else {}
    progress.update(phase='cri', contentCount=0, mediaType='UNKNOWN')
    if not DIGEST.fullmatch(docker_id): raise CustodyError('docker_identity_missing')
    if not re.fullmatch(r'kanz-native-(?:admin|probe):[0-9a-f]{40}',requested):
        raise CustodyError('unowned_image_tag')
    status=inspection.get('status',{})
    tags=status.get('repoTags'); digests=status.get('repoDigests')
    if (not isinstance(tags,list) or not 1<=len(tags)<=16 or 'docker.io/library/'+requested not in tags or
            not isinstance(digests,list) or not 1<=len(digests)<=16 or not DIGEST.fullmatch(status.get('id',''))):
        raise CustodyError('cri_tag_config_custody_absent')
    pod_digest=reference_digest(observed)
    if observed != status['id'] and observed not in digests:
        raise CustodyError('pod_reference_not_bound_by_cri')
    cache={};total=0
    def content(digest,size=None):
        nonlocal total
        if not DIGEST.fullmatch(digest): raise CustodyError('content_digest_invalid')
        if digest not in cache:
            if len(cache)>=6: raise CustodyError('image_graph_exceeds_bound')
            raw=read(digest)
            if not isinstance(raw,bytes) or not 0<len(raw)<=1<<20: raise CustodyError('image_content_exceeds_bound')
            total+=len(raw)
            if total>4<<20: raise CustodyError('image_graph_exceeds_byte_bound')
            if 'sha256:'+hashlib.sha256(raw).hexdigest()!=digest: raise CustodyError('image_content_digest_differs')
            try: value=json.loads(raw,object_pairs_hook=strict)
            except (json.JSONDecodeError,UnicodeError): raise CustodyError('content_json_invalid') from None
            if not isinstance(value,dict): raise CustodyError('image_content_shape')
            cache[digest]=(raw,value)
            progress['contentCount']=len(cache)
        raw,value=cache[digest]
        if size is not None and (type(size) is not int or size!=len(raw)): raise CustodyError('descriptor_size_differs')
        return value
    def descriptor(value,allowed):
        if not isinstance(value,dict) or value.get('mediaType') not in allowed or not DIGEST.fullmatch(value.get('digest','')) or type(value.get('size')) is not int or not 0<value['size']<=1<<20:
            raise CustodyError('unsupported_image_descriptor')
        if value.get('urls'): raise CustodyError('external_content_refused')
        return value['digest'],value['size']
    def config(digest,size=None):
        value=content(digest,size)
        if (value.get('architecture')!='amd64' or value.get('os')!='linux' or value.get('rootfs',{}).get('type')!='layers' or
                'manifests' in value or 'layers' in value or 'schemaVersion' in value):
            raise CustodyError('unsupported_configuration')
        diffs=value['rootfs'].get('diff_ids')
        if not isinstance(diffs,list) or not 1<=len(diffs)<=128 or any(not isinstance(x,str) or not DIGEST.fullmatch(x) for x in diffs):
            raise CustodyError('configuration_layer_identity_absent')
        return digest
    def chain(root):
        value=content(root);kind=value.get('mediaType');manifest=None
        progress['mediaType']='index' if kind in INDEX else 'manifest' if kind in MANIFEST else 'absent' if kind is None else 'UNKNOWN'
        if kind in INDEX:
            if value.get('schemaVersion')!=2: raise CustodyError('index_version')
            entries=value.get('manifests')
            if not isinstance(entries,list) or not 1<=len(entries)<=8: raise CustodyError('index_bound')
            selected=[]
            for entry in entries:
                descriptor(entry,MANIFEST)
                platform=entry.get('platform',{})
                if platform.get('os')=='linux' and platform.get('architecture')=='amd64' and not platform.get('variant'):
                    selected.append(entry)
            if len(selected)!=1: raise CustodyError('ambiguous_platform')
            manifest,size=descriptor(selected[0],MANIFEST);value=content(manifest,size)
            if value.get('mediaType')!=selected[0]['mediaType']: raise CustodyError('manifest_media_type_differs')
        elif kind in MANIFEST:
            manifest=root
        elif kind is None:
            return {'type':'config','config':config(root),'manifest':None}
        else: raise CustodyError('unknown_image_content_type')
        if value.get('schemaVersion')!=2 or value.get('mediaType') not in MANIFEST: raise CustodyError('manifest_version')
        layers=value.get('layers')
        if not isinstance(layers,list) or not 1<=len(layers)<=128: raise CustodyError('manifest_layers_missing')
        for layer in layers:
            if not isinstance(layer,dict) or not DIGEST.fullmatch(layer.get('digest','')) or type(layer.get('size')) is not int or layer['size']<0 or layer.get('urls'):
                raise CustodyError('manifest_layer_descriptor_invalid')
        conf,size=descriptor(value.get('config'),CONFIG)
        return {'type':'index' if kind in INDEX else 'manifest','config':config(conf,size),'manifest':manifest}
    progress['phase']='pod_graph'
    node=chain(pod_digest)
    progress['phase']='build_graph'
    host=chain(docker_id)
    progress['phase']='convergence'
    if '@' in observed and node['type']=='config':
        raise CustodyError('repository_digest_cannot_name_a_configuration')
    if node['config']!=status['id'] or host['config']!=node['config']:
        raise CustodyError('execution_configuration_differs')
    if node['manifest'] and host['manifest'] and node['manifest']!=host['manifest']:
        raise CustodyError('platform_manifest_differs')
    progress['phase']='complete'
    return {'verified':True,'dockerIdentityType':host['type'],'podIdentityType':node['type'],
            'dockerIdentity':docker_id,'podIdentity':pod_digest,
            'config':node['config'],'platformManifest':node['manifest'] or host['manifest'],'contentCount':len(cache)}
