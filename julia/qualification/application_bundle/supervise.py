"""Run the existing bounded Go parent inside the verified compiled application sandbox."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import tempfile

from corpus import worker
from proof import CASES, ROOT, required


def main(receipt, report):
    image = receipt['image']
    source = receipt['source']
    launcher = receipt['native']['bin/curve-worker']['sha256']
    if (not re.fullmatch(r'sha256:[0-9a-f]{64}', image) or not re.fullmatch(r'[0-9a-f]{40}', source)
            or not re.fullmatch(r'[0-9a-f]{64}', launcher)):
        raise ValueError('invalid trusted application receipt')
    with tempfile.TemporaryDirectory() as directory:
        binary = Path(directory) / 'bounded-parent'
        env = {**os.environ, 'GOOS': 'linux', 'GOARCH': 'amd64', 'CGO_ENABLED': '0', 'GOPROXY': 'off'}
        required(['go', 'test', '-p', '1', '-c', '-tags', 'julia_application_qualification',
                  '-trimpath', '-o', str(binary), './tools/julia_conformance'], cwd=ROOT / 'kanz', env=env, timeout=300)
        supervisor_sha = hashlib.sha256(binary.read_bytes()).hexdigest()
        with worker(image, source, supervisor=(binary, launcher)) as name:
            output = required(['docker', 'start', '--attach', name], timeout=90).decode('utf-8')
            info, = json.loads(required(['docker', 'inspect', name]))
            if info['State']['ExitCode'] != 0 or info['State']['OOMKilled']:
                raise ValueError('bounded Go supervisor failed')
        if hashlib.sha256(binary.read_bytes()).hexdigest() != supervisor_sha:
            raise ValueError('test supervisor changed')
    matches = re.findall(r'--- PASS: TestCompiledApplicationSupervisor/supervise/(\w+) \(', output)
    if len(matches) != len(CASES) or set(matches) != CASES or not output.rstrip().endswith('PASS'):
        raise ValueError('actual Go supervisor did not exercise the full corpus')
    report.write_text(json.dumps({'image': image, 'source': source, 'launcherSHA256': launcher,
                                  'supervisorSHA256': supervisor_sha, 'cases': sorted(matches)}, sort_keys=True) + '\n')
    print('Bounded Go parent and 13 wire cases passed in the immutable candidate; timeout and cleanup verified.')


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('receipt', type=Path)
    parser.add_argument('report', type=Path)
    args = parser.parse_args()
    main(json.loads(args.receipt.read_bytes()), args.report)
