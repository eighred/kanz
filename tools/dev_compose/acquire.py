"""Acquire published OCI bytes before any repository work runs in the guest."""
import json
from pathlib import Path
import subprocess
import sys

from oci import digest, pack, selected


def main(inputs):
    pins = json.loads(Path(__file__).with_name('images.json').read_bytes())
    for reference, expected in pins.items():
        directory = inputs / expected
        # The workflow stages private content with a transient registry identity.
        if not reference.startswith('ghcr.io/'):
            directory.mkdir()
            with (directory / 'index').open('wb') as stream:
                subprocess.run(['skopeo', 'inspect', '--raw', 'docker://' + reference], stdout=stream, check=True)
        if digest(directory / 'index') != expected:
            raise ValueError('the reviewed registry alias changed: ' + reference)
        child = selected(json.loads((directory / 'index').read_bytes()))
        if not reference.startswith('ghcr.io/'):
            subprocess.run(['skopeo', 'copy', '--retry-times', '3', '--preserve-digests',
                            'docker://' + reference.rsplit(':', 1)[0] + '@' + child['digest'],
                            'dir:' + str(directory / 'platform')], check=True)
    pack(pins, inputs, inputs / 'images.tar')


if __name__ == '__main__':
    main(Path(sys.argv[1]))
