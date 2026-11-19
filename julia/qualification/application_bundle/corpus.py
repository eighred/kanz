"""Verify an immutable application candidate with the standard Go oracle and runtime limits."""
import argparse
import contextlib
import json
from pathlib import Path
import re
import sys
import tempfile
import uuid

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from proof import CASES, command, container_boundary, oracle, required


@contextlib.contextmanager
def worker(image, source):
    name = 'kanz-julia-app-' + uuid.uuid4().hex
    label = 'kanz.julia-proof=' + source
    try:
        required(['docker', 'create', '--pull=never', '--name', name, '--label', label,
                  '--platform', 'linux/amd64', '--network', 'none', '--read-only', '--user', '65532:65532',
                  '--cap-drop', 'ALL', '--security-opt', 'no-new-privileges', '--cpus', '1',
                  '--memory', '1g', '--memory-swap', '1g', '--pids-limit', '64',
                  '--tmpfs', '/tmp:rw,noexec,nosuid,size=16777216,mode=1777', '-i', image])
        info, = json.loads(required(['docker', 'inspect', name]))
        container_boundary(info, image, source)
        external = [m for m in info['Mounts'] if (m['Type'], m['Destination']) != ('tmpfs', '/tmp')]
        if info['Config']['Entrypoint'] != ['/opt/kanz/bin/curve-worker'] or external:
            raise ValueError('unexpected application entrypoint or external mount')
        yield name
    finally:
        selector = ['docker', 'ps', '-aq', '--filter', 'name=^/' + name + '$']
        cid = required(selector).decode().strip()
        if cid:
            info, = json.loads(required(['docker', 'inspect', cid]))
            if info['Config']['Labels'].get('kanz.julia-proof') != source or info['Name'] != '/' + name:
                raise ValueError('refusing to clean up an unowned container')
            required(['docker', 'rm', '--force', cid])
        if required(selector).strip():
            raise RuntimeError('owned application candidate survived cleanup')


def main(image, source):
    if not re.fullmatch('sha256:[0-9a-f]{64}', image) or not re.fullmatch('[0-9a-f]{40}', source):
        raise ValueError('expected immutable image and Git identities')
    with tempfile.TemporaryDirectory() as directory:
        scratch = Path(directory)
        oracle(scratch, 'prepare')
        rows = json.loads((scratch / 'cases.json').read_bytes())
        if len(rows) != len(CASES) or {case['name'] for case in rows} != CASES:
            raise ValueError('independent wire corpus differs')
        for case in rows:
            key = case['name']
            with worker(image, source) as name:
                code, response = command(['docker', 'start', '--attach', '--interactive', name],
                                         data=(scratch / (key + '.input')).read_bytes())
            if (code == 0) != case['exitZero'] or (code and response):
                raise ValueError('application wire exit contract differs: ' + key)
            (scratch / (key + '.output')).write_bytes(response)
        oracle(scratch, 'verify')
    print('Passed 13 real application wire cases, Go oracle, runtime isolation and cleanup.')


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('image')
    parser.add_argument('source')
    args = parser.parse_args()
    main(args.image, args.source)
