"""Interrupt cleanup only for the exact nonce recorded by this job."""
import os
from pathlib import Path
import re
import subprocess


def main():
    marker = Path(os.environ['RUNNER_TEMP']) / 'gateway-policy.cluster'
    if not marker.exists():
        return
    if marker.is_symlink() or not marker.is_file() or marker.stat().st_size > 64:
        raise ValueError('invalid owned cluster marker')
    name = marker.read_text()
    if not re.fullmatch('cni-proof-[0-9a-f]{12}', name):
        raise ValueError('invalid owned cluster name')
    def run(args):
        result = subprocess.run(args, capture_output=True, text=True, timeout=180, check=True)
        return result.stdout.splitlines()
    if name in run(['kind', 'get', 'clusters']):
        run(['kind', 'delete', 'cluster', '--name', name])
    if name in run(['kind', 'get', 'clusters']):
        raise ValueError('owned cluster remains')


if __name__ == '__main__':
    main()
