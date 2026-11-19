"""Acquire published OCI bytes before any repository work runs in the guest."""
import json
from pathlib import Path
import re
import subprocess
import sys

from oci import digest, pack, selected


# The private entries are already staged by the credential-scoped workflow step.
# The Redpanda publisher documents docker.io as the source behind its analytics alias.
PUBLISHERS = {
    ('ghcr.io', 'eighred/base/golang'): 'ghcr.io',
    ('ghcr.io', 'eighred/base/distroless-static'): 'ghcr.io',
    ('docker.io', 'docker/dockerfile'): 'docker.io',
    ('docker.io', 'library/nats'): 'docker.io',
    ('docker.io', 'library/postgres'): 'docker.io',
    ('docker.io', 'natsio/nats-box'): 'docker.io',
    ('docker.redpanda.com', 'redpandadata/redpanda'): 'docker.io',
}


def publisher(reference):
    host, slash, image = reference.partition('/')
    repository, colon, tag = image.rpartition(':')
    owner = (host, repository)
    if not slash or not colon or owner not in PUBLISHERS or not re.fullmatch(r'[\w][\w.-]{0,127}', tag, flags=re.ASCII):
        raise ValueError('unreviewed registry, repository or tag')
    return owner, PUBLISHERS[owner] + '/' + repository, tag


def main(inputs):
    pins = json.loads(Path(__file__).with_name('images.json').read_bytes())
    references = {reference: publisher(reference) for reference in pins}
    if (len(references) != len(PUBLISHERS) or {row[0] for row in references.values()} != PUBLISHERS.keys()
            or any(not re.fullmatch(r'[0-9a-f]{64}', value) for value in pins.values())):
        raise ValueError('the reviewed registry inventory or digest is incomplete')
    for reference, (owner, source, tag) in references.items():
        expected = pins[reference]
        directory = inputs / expected
        staged = owner[0] == 'ghcr.io'
        if not staged:
            directory.mkdir()
            with (directory / 'index').open('wb') as stream:
                subprocess.run(['skopeo', 'inspect', '--raw', 'docker://' + source + ':' + tag], stdout=stream, check=True)
        if digest(directory / 'index') != expected:
            raise ValueError('the reviewed registry alias changed: ' + reference)
        child = selected(json.loads((directory / 'index').read_bytes()))
        if not staged:
            subprocess.run(['skopeo', 'copy', '--retry-times', '3', '--preserve-digests',
                            'docker://' + source + '@' + child['digest'], 'dir:' + str(directory / 'platform')], check=True)
    pack(pins, inputs, inputs / 'images.tar')


if __name__ == '__main__':
    main(Path(sys.argv[1]))
