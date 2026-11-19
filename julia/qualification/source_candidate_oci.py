"""Read the three selected ELF files directly from the parent-owned OCI archive."""
import hashlib
import json
from pathlib import PurePosixPath
import re
import tarfile


def image_files(path, image, expected):
    if path.stat().st_size > 1 << 30:
        raise ValueError('oversized candidate OCI archive')
    with tarfile.open(path, 'r:') as archive:
        lookup = {}
        for member in archive:
            if member.name in lookup or len(lookup) >= 256:
                raise ValueError('ambiguous OCI archive structure')
            lookup[member.name] = member

        def member_stream(name):
            entry = lookup[name]
            if not entry.isreg():
                raise ValueError('OCI blob must be a regular file')
            return archive.extractfile(entry)

        def blob(desc):
            key = desc['digest']
            if not re.fullmatch('sha256:[a-f0-9]{64}', key):
                raise ValueError('invalid OCI descriptor digest')
            name = 'blobs/sha256/' + key[7:]
            if lookup[name].size != desc['size']:
                raise ValueError('OCI descriptor size differs')
            with member_stream(name) as stream:
                if hashlib.file_digest(stream, 'sha256').hexdigest() != key[7:]:
                    raise ValueError('OCI descriptor bytes differ')
            return name

        def document(name):
            if not 0 < lookup[name].size <= 1 << 20:
                raise ValueError('unbounded OCI metadata')
            with member_stream(name) as stream:
                return json.load(stream)

        choices = document('index.json')['manifests']
        if len(choices) != 1 or choices[0]['digest'] != image:
            raise ValueError('OCI export is not the exact parent-built image')
        top = document(blob(choices[0]))
        if 'manifests' in top:
            platforms = [item for item in top['manifests'] if item.get('platform') == {'architecture': 'amd64', 'os': 'linux'}]
            if len(platforms) != 1:
                raise ValueError('ambiguous Linux/amd64 manifest')
            manifest_id = platforms[0]['digest']
            manifest = document(blob(platforms[0]))
        else:
            manifest_id, manifest = image, top
        config = document(blob(manifest['config']))
        if config['os'] != 'linux' or config['architecture'] != 'amd64' or not 0 < len(manifest['layers']) <= 64:
            raise ValueError('unexpected candidate platform or layer count')
        found = {}
        for layer in manifest['layers']:
            if layer['mediaType'] != 'application/vnd.oci.image.layer.v1.tar+gzip':
                raise ValueError('unexpected OCI layer encoding')
            with member_stream(blob(layer)) as stream, tarfile.open(fileobj=stream, mode='r|gz') as packed:
                layer_files(packed, expected, found)
        if found != expected:
            raise ValueError('ELF file set differs from the pinned candidate')
        return {'manifest': manifest_id, 'config': manifest['config']['digest'],
                'layers': [layer['digest'] for layer in manifest['layers']], 'files': found}


def layer_files(archive, expected, found):
    ancestors = {str(parent) for name in expected for parent in PurePosixPath(name).parents}
    total = count = 0
    for entry in archive:
        count += 1
        total += entry.size
        if total > 3 << 30 or count > 100000:
            raise ValueError('oversized OCI layer')
        name = entry.name.removeprefix('./').rstrip('/')
        path = PurePosixPath(name)
        if path.is_absolute() or '..' in path.parts or str(path) != (name or '.'):
            raise ValueError('ambiguous layer path')
        if path.name.startswith('.wh.') and str(path.parent) in ancestors:
            hidden = str(path.parent / path.name.removeprefix('.wh.'))
            if path.name == '.wh..wh..opq' or hidden in expected or hidden in ancestors:
                raise ValueError('whiteout affects a selected library')
        if name in ancestors and not entry.isdir():
            raise ValueError('selected OCI library parent is not a directory')
        if name in expected:
            if name in found or not entry.isreg() or entry.size != expected[name]['size']:
                raise ValueError('ambiguous candidate ELF file')
            raw = archive.extractfile(entry).read()
            item = {'size': len(raw), 'sha256': hashlib.sha256(raw).hexdigest()}
            if not raw.startswith(b'\x7fELF') or item != expected[name]:
                raise ValueError('selected ELF bytes differ')
            found[name] = item
