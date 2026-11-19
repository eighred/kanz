"""Pack the exact published indexes and their selected linux/amd64 blobs for offline import."""
import hashlib
import io
import json
from pathlib import Path
import re
import tarfile


def digest(path):
    with path.open('rb') as stream:
        return hashlib.file_digest(stream, 'sha256').hexdigest()


def selected(index):
    matches = [row for row in index['manifests'] if row.get('platform', {}).get('os') == 'linux'
               and row['platform'].get('architecture') == 'amd64' and not row['platform'].get('variant')]
    if len(matches) != 1:
        raise ValueError('registry index does not identify one linux/amd64 manifest')
    return matches[0]


def pack(pins, inputs, target):
    descriptors = []
    written = set()
    with tarfile.open(target, 'w') as output:
        def json_file(name, document):
            data = json.dumps(document, separators=(',', ':')).encode()
            member = tarfile.TarInfo(name)
            member.size = len(data)
            output.addfile(member, io.BytesIO(data))

        def add(path, desc):
            key = desc['digest'].removeprefix('sha256:')
            if (not re.fullmatch(r'[a-f0-9]{64}', key) or path.stat().st_size != desc['size']
                    or digest(path) != key):
                raise ValueError('registry blob digest or size mismatch')
            if key not in written:
                output.add(path, arcname='blobs/sha256/' + key, recursive=False)
                written.add(key)

        for reference, expected in pins.items():
            directory = inputs / expected
            index_file = directory / 'index'
            index = json.loads(index_file.read_bytes())
            desc = {'mediaType': index['mediaType'], 'digest': 'sha256:' + expected, 'size': index_file.stat().st_size,
                    'annotations': {'io.containerd.image.name': reference,
                                    'org.opencontainers.image.ref.name': reference.rsplit(':', 1)[1]}}
            add(index_file, desc)
            platform = directory / 'platform'
            image = selected(index)
            manifest_file = platform / 'manifest.json'
            add(manifest_file, image)
            manifest = json.loads(manifest_file.read_bytes())
            for blob in [manifest['config'], *manifest['layers']]:
                if not re.fullmatch(r'sha256:[0-9a-f]{64}', blob['digest']):
                    raise ValueError('unsupported registry digest')
                add(platform / blob['digest'][7:], blob)
            config = json.loads((platform / manifest['config']['digest'][7:]).read_bytes())
            if (config['os'], config['architecture']) != ('linux', 'amd64'):
                raise ValueError('registry config platform mismatch')
            descriptors.append(desc)
            print(reference, desc['digest'], image['digest'], manifest['config']['digest'], flush=True)
        json_file('oci-layout', {'imageLayoutVersion': '1.0.0'})
        json_file('index.json', {'schemaVersion': 2, 'mediaType': 'application/vnd.oci.image.index.v1+json',
                                 'manifests': descriptors})
