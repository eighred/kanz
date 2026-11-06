"""Validate downloaded evidence against this exact checkout and workflow attempt."""
import argparse
import json
from pathlib import Path
import subprocess

import proof


def check(path, source, tree, run_id, attempt):
    if path.is_symlink() or not path.is_file() or path.stat().st_size > 1 << 20:
        raise ValueError('invalid evidence file')
    report = json.loads(path.read_text())
    proof.validate_report(report, source, tree, run_id, attempt)
    def run(args):
        return subprocess.check_output(args, text=True, cwd=proof.ROOT, timeout=30).strip()
    if run(['git', 'rev-parse', 'HEAD']) != source or run(['git', 'rev-parse', 'HEAD^{tree}']) != tree:
        raise ValueError('checker checkout differs')
    policies, _, rendered = proof.render(run)
    expected = [{'name': p['metadata']['name'], 'namespace': p['metadata']['namespace'], 'sha256': proof.digest(p)} for p in policies]
    if report['policies'] != expected or report['renderSHA256'] != rendered:
        raise ValueError('evidence policy inventory differs from actual rendered source')


if __name__ == '__main__':
    parser = argparse.ArgumentParser()
    parser.add_argument('path', type=Path)
    for name in ('source', 'tree', 'run-id', 'attempt'):
        parser.add_argument('--' + name, required=True)
    args = parser.parse_args()
    check(args.path, args.source, args.tree, args.run_id, args.attempt)
    print('All rendered-policy cases and cleanup verified for exact source/run/attempt')
