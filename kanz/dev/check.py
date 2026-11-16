"""Local Go gate: complete serial test accounting with timeout tree cleanup."""

import argparse
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import tempfile

ROOT = Path(__file__).resolve().parents[1]
sys.dont_write_bytecode = True
sys.path.insert(0, str(ROOT.parent / '.github' / 'scripts'))
from go_shard_evidence import validate_events  # noqa: E402


def run_tests(command, *, cwd, stdout, timeout=600):
    # go test owns child test binaries. subprocess.run's timeout kills only Go,
    # leaving those binaries able to use the shared fixture database afterward.
    # This requests owned-tree termination; it is not independent proof of
    # descendant absence or an OS-enforced execution profile.
    process = subprocess.Popen(command, cwd=cwd, stdout=stdout,
                               start_new_session=os.name == 'posix')
    try:
        return subprocess.CompletedProcess(command, process.wait(timeout=timeout))
    except BaseException:
        try:
            if os.name == 'posix':
                os.killpg(process.pid, signal.SIGKILL)
            else:
                # Keep the parent alive until taskkill has enumerated its tree.
                subprocess.run(['taskkill.exe', '/PID', str(process.pid), '/T', '/F'],
                               check=True, timeout=15, stdout=subprocess.DEVNULL,
                               stderr=subprocess.DEVNULL)
        finally:
            if process.poll() is None:
                process.kill()
            process.wait(timeout=15)
        raise


def test_packages(patterns, race=False, root=ROOT):
    flags = ['-race'] if race else []
    packages = subprocess.check_output(['go', 'list', *flags, *patterns], cwd=root, text=True).splitlines()
    if not packages or len(set(packages)) != len(packages):
        raise ValueError('empty or duplicate Go package inventory')
    skipped = 0
    # Fresh serial batches bound each invocation while keeping fixtures sequential.
    for start in range(0, len(packages), 20):
        batch = packages[start:start + 20]
        with tempfile.TemporaryFile(mode='w+', encoding='utf-8') as output:
            result = run_tests(['go', 'test', *flags, '-p', '1', '-count=1', '-json', *batch],
                               cwd=root, stdout=output)
            output.seek(0)
            raw = output.read()
        for line in raw.splitlines():
            event = json.loads(line)
            if event.get('Action') == 'skip' and event.get('Test'):
                skipped += 1
                print(f"SKIP {event['Package']}/{event['Test']}")
            if event.get('Output'):
                print(event['Output'], end='')
        if result.returncode:
            raise ValueError(f'go test failed with exit {result.returncode}')
        validate_events(raw, batch)
        print(f'Verified {start + len(batch)}/{len(packages)} fresh package results')
    print(f'PASS: {len(packages)}/{len(packages)} packages; {skipped} test/subtest skips (not dependency proof)')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('mode', choices=['test'])
    parser.add_argument('--race', action='store_true')
    parser.add_argument('patterns', nargs='*', default=['./...'])
    args = parser.parse_args()
    try:
        test_packages(args.patterns or ['./...'], args.race)
        return 0
    except (OSError, ValueError, subprocess.SubprocessError) as exc:
        print(str(exc), file=sys.stderr)
        return 1


if __name__ == '__main__':
    sys.exit(main())
