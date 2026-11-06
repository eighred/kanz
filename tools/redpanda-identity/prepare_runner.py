"""Provision disk headroom only on the disposable GitHub-hosted Ubuntu runner.

No cache, repository, container volume, or broker policy is removed or changed.
The sole removable installation is the unrelated preinstalled Android SDK.
"""
import json
import os
from pathlib import Path
import shutil
import stat
import subprocess
import sys

SDK = Path('/usr/local/lib/android/sdk')
DOCKER = Path('/var/lib/docker')


def hosted(env):
    if (env.get('GITHUB_ACTIONS') != 'true' or env.get('RUNNER_ENVIRONMENT') != 'github-hosted'
            or env.get('RUNNER_OS') != 'Linux' or env.get('ImageOS') not in ('ubuntu24', 'ubuntu26')):
        raise ValueError('only the ephemeral hosted Ubuntu fixture is supported')


def regular_tree_root(path):
    # All ancestors must be literal directories, not symlink aliases.
    for parent in (path, *path.parents):
        if not stat.S_ISDIR(parent.lstat().st_mode):
            raise ValueError('expected literal installed directory')
    if path.resolve(strict=True) != path:
        raise ValueError('installed path differs')


def verify_sdk(env):
    if env.get('ANDROID_HOME') != str(SDK) or env.get('ANDROID_SDK_ROOT') != str(SDK):
        raise ValueError('hosted Android installation identity differs')
    regular_tree_root(SDK)
    # Bound identity to the existing installed SDK, not an arbitrary empty path.
    if not stat.S_ISREG((SDK / 'platform-tools/adb').lstat().st_mode):
        raise ValueError('installed Android platform tools absent')
    regular_tree_root(SDK / 'platform-tools')
    regular_tree_root(SDK / 'cmdline-tools')
    if SDK.stat().st_dev != SDK.parent.stat().st_dev:
        raise ValueError('Android SDK must not be a mounted filesystem')


def main(required):
    if type(required) is not int or not 0 < required < (1 << 63):
        raise ValueError('invalid runtime capacity requirement')
    hosted(os.environ)
    regular_tree_root(DOCKER)
    result = subprocess.run(['docker', 'info', '--format', '{{.DockerRootDir}}'],
                            check=True, capture_output=True, text=True, timeout=20)
    if result.stdout.strip() != str(DOCKER):
        raise ValueError('unexpected Docker storage location')
    before = shutil.disk_usage(DOCKER).free
    removed = False
    try:
        if before <= required:
            verify_sdk(os.environ)
            # Fixed argv and path; refuse nested mounts before bounded one-filesystem
            # removal. Symlinks inside the SDK are unlinked, never traversed by rm.
            mounts = subprocess.run(['findmnt', '--json', '--output', 'TARGET'], check=True,
                                    capture_output=True, text=True, timeout=20)
            if len(mounts.stdout) > 1 << 20:
                raise ValueError('mount inventory exceeds fixture bound')
            def check_mounts(items):
                for item in items:
                    target = item.get('target')
                    if not isinstance(target, str):
                        raise ValueError('invalid mount inventory')
                    if target == str(SDK) or target.startswith(str(SDK) + '/'):
                        raise ValueError('mounted SDK content cannot be removed')
                    check_mounts(item.get('children', []))
            check_mounts(json.loads(mounts.stdout)['filesystems'])
            subprocess.run(['sudo', 'timeout', '180', 'rm', '-rf', '--one-file-system',
                            '--preserve-root=all', '--', str(SDK)], check=True, timeout=190)
            if os.path.lexists(SDK):
                raise ValueError('SDK removal incomplete')
            removed = True
    finally:
        # Numeric diagnostics survive insufficient space or removal refusal.
        after = shutil.disk_usage(DOCKER).free
        print(json.dumps({'beforeBytes': before, 'afterBytes': after,
                          'requiredBytes': required, 'unrelatedAndroidSDKRemoved': removed}))
    if after <= required:
        raise ValueError('insufficient disposable runner capacity')


if __name__ == '__main__':
    if len(sys.argv) != 2 or not sys.argv[1].isascii() or not sys.argv[1].isdecimal() or len(sys.argv[1]) > 19:
        raise ValueError('one bounded numeric runtime requirement is mandatory')
    main(int(sys.argv[1]))
