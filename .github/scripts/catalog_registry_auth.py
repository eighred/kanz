"""Host-only, exclusive registry profiles for the catalog fixture and its reader.

Docker CLI v28/v29 logout can return zero after every credential erasure fails.
Only actual file absence and removal of this exact owned profile establish cleanup.
No helper returns credential values or operates on a caller's existing profile.
"""
import argparse
from contextlib import contextmanager
import json
import os
from pathlib import Path
import re
import stat
import subprocess
import uuid


OWNER = '.kanz-owner.json'
CONFIG = 'config.json'
MAX_CONFIG = 16384


def identity_from_environment(environment=None):
    environment = os.environ if environment is None else environment
    identity = {key: environment[name] for key, name in
                (('source', 'GITHUB_SHA'), ('run', 'GITHUB_RUN_ID'), ('attempt', 'GITHUB_RUN_ATTEMPT'))}
    validate_identity(identity)
    return identity


def validate_identity(identity):
    if (not isinstance(identity, dict) or set(identity) != {'source', 'run', 'attempt'}
            or not isinstance(identity['source'], str) or not re.fullmatch('[0-9a-f]{40}', identity['source'])
            or any(not isinstance(identity[key], str) or not re.fullmatch('[1-9][0-9]*', identity[key]) for key in ('run', 'attempt'))):
        raise ValueError('registry profile identity refused')


def _prefix(identity):
    validate_identity(identity)
    return 'catalog-registry-' + identity['source'][:12] + '-' + identity['run'] + '-' + identity['attempt'] + '-'


def _marker(identity, nonce):
    return dict(identity, schema=1, kind='catalog-registry-auth', nonce=nonce)


def _unique(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError('duplicate registry profile field')
        result[key] = value
    return result


@contextmanager
def _root(root):
    if os.name != 'posix' or not all(hasattr(os, name) for name in ('O_NOFOLLOW', 'O_NONBLOCK', 'O_DIRECTORY')):
        raise ValueError('Linux no-follow registry profile required')
    root = Path(root)
    if not root.is_absolute() or any(c in str(root) for c in '\x00\r\n') or root.resolve(strict=True) != root:
        raise ValueError('registry profile root refused')
    descriptor = os.open(root, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    try:
        if os.fstat(descriptor).st_uid != os.getuid():
            raise ValueError('registry root owner refused')
        yield root, descriptor
    finally:
        os.close(descriptor)


def _regular(info, limit, allow_empty=False):
    if (not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid() or info.st_nlink != 1
            or stat.S_IMODE(info.st_mode) != 0o600 or not (0 if allow_empty else 1) <= info.st_size <= limit):
        raise ValueError('registry profile regular file refused')


def _read(directory, name, limit):
    descriptor = os.open(name, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK, dir_fd=directory)
    try:
        before = os.fstat(descriptor)
        _regular(before, limit)
        raw = os.read(descriptor, limit + 1)
        after = os.fstat(descriptor)
        if len(raw) != before.st_size or (before.st_dev, before.st_ino, before.st_size, before.st_mtime_ns) != (after.st_dev, after.st_ino, after.st_size, after.st_mtime_ns):
            raise ValueError('registry profile changed during read')
        try:
            return json.loads(raw, object_pairs_hook=_unique)
        except (UnicodeError, ValueError):
            raise ValueError('registry profile document refused') from None
    finally:
        os.close(descriptor)


def _create(directory, name, value):
    descriptor = os.open(name, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600, dir_fd=directory)
    with os.fdopen(descriptor, 'w', encoding='utf-8') as output:
        json.dump(value, output, sort_keys=True)
        output.flush()
        os.fsync(output.fileno())


@contextmanager
def _profile(root, profile, identity):
    profile = Path(profile)
    prefix = _prefix(identity)
    with _root(root) as (root, root_fd):
        if profile.parent != root or not profile.name.startswith(prefix) or not re.fullmatch('[0-9a-f]{16}', profile.name[len(prefix):]):
            raise ValueError('foreign registry profile path refused')
        descriptor = os.open(profile.name, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=root_fd)
        try:
            info = os.fstat(descriptor)
            if info.st_uid != os.getuid() or stat.S_IMODE(info.st_mode) != 0o700:
                raise ValueError('registry profile directory owner refused')
            if _read(descriptor, OWNER, 2048) != _marker(identity, profile.name[len(prefix):]):
                raise ValueError('registry profile ownership refused')
            yield root_fd, descriptor
        finally:
            os.close(descriptor)


def prepare(root, identity):
    if os.environ.get('DOCKER_AUTH_CONFIG'):
        raise ValueError('ambient registry auth override refused')
    with _root(root) as (root, root_fd):
        nonce = uuid.uuid4().hex[:16]
        name = _prefix(identity) + nonce
        os.mkdir(name, 0o700, dir_fd=root_fd)  # Never reuse a preexisting profile.
        descriptor = os.open(name, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=root_fd)
        try:
            _create(descriptor, OWNER, _marker(identity, nonce))
            # ContainsAuth counts map entries, so this suppresses native-store
            # autodetection before login without introducing any credential.
            _create(descriptor, CONFIG, {'auths': {'ghcr.io': {}}})
        except Exception:
            # Only names created in this fresh held directory are removable.
            for filename in (CONFIG, OWNER):
                try:
                    info = os.stat(filename, dir_fd=descriptor, follow_symlinks=False)
                    _regular(info, MAX_CONFIG, allow_empty=True)
                    os.unlink(filename, dir_fd=descriptor)
                except FileNotFoundError:
                    pass
            os.rmdir(name, dir_fd=root_fd)
            raise
        finally:
            os.close(descriptor)
        return root / name


def _file_backend(config):
    if (not isinstance(config, dict) or set(config) - {'auths', 'credsStore', 'credHelpers'}
            or config.get('credsStore', '') != '' or config.get('credHelpers', {}) != {}
            or not isinstance(config.get('auths'), dict) or set(config['auths']) - {'ghcr.io'}
            or any(not isinstance(value, dict) for value in config['auths'].values())):
        raise ValueError('registry file backend refused')
    return config['auths']


def verify_backend(root, profile, identity, authenticated=True):
    if os.environ.get('DOCKER_AUTH_CONFIG'):
        raise ValueError('ambient registry auth override refused')
    with _profile(root, profile, identity) as (_, descriptor):
        auths = _file_backend(_read(descriptor, CONFIG, MAX_CONFIG))
        if authenticated and not isinstance(auths.get('ghcr.io', {}).get('auth'), str):
            raise ValueError('registry file credential absent')
        if authenticated and not auths['ghcr.io']['auth']:
            raise ValueError('registry file credential absent')


def _logout(profile):
    environment = {'PATH': os.environ['PATH'], 'HOME': str(profile), 'DOCKER_CONFIG': str(profile)}
    return subprocess.run(['docker', '--config', str(profile), 'logout', 'ghcr.io'], env=environment,
                          stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=30, check=False).returncode


def _remove(root_fd, directory, name):
    info = os.fstat(directory)
    current = os.stat(name, dir_fd=root_fd, follow_symlinks=False)
    if (info.st_dev, info.st_ino) != (current.st_dev, current.st_ino):
        raise ValueError('registry directory changed before removal')
    names = set(os.listdir(directory))
    allowed = {OWNER, CONFIG}
    # Refuse symlinks/hardlinks before removing either known file.
    snapshots = {}
    for filename in names & allowed:
        found = os.stat(filename, dir_fd=directory, follow_symlinks=False)
        _regular(found, MAX_CONFIG, allow_empty=True)
        snapshots[filename] = (found.st_dev, found.st_ino)
    for filename in (CONFIG, OWNER):
        if filename not in snapshots or (filename == OWNER and names - allowed):
            continue
        found = os.stat(filename, dir_fd=directory, follow_symlinks=False)
        _regular(found, MAX_CONFIG, allow_empty=True)
        if snapshots[filename] != (found.st_dev, found.st_ino):
            raise ValueError('registry file changed before removal')
        os.unlink(filename, dir_fd=directory)
    if names - allowed:
        return False  # Preserve ownership and every unknown entry.
    os.rmdir(name, dir_fd=root_fd)
    try:
        os.stat(name, dir_fd=root_fd, follow_symlinks=False)
    except FileNotFoundError:
        return True
    return False


def cleanup(root, profile, identity, logout=None):
    result = {'ownershipVerified': False, 'fileBackend': False,
              'logout': {'attempted': False, 'exitCode': None},
              'credentialsAbsent': False, 'directoryRemoved': False, 'passed': False}
    try:
        with _profile(root, profile, identity) as (root_fd, descriptor):
            result['ownershipVerified'] = True
            try:
                auths = _file_backend(_read(descriptor, CONFIG, MAX_CONFIG))
                result['fileBackend'] = True
                if auths.get('ghcr.io') is not None:
                    result['logout']['attempted'] = True
                    code = (logout or _logout)(Path(profile))
                    if type(code) is not int:
                        raise ValueError('registry logout result refused')
                    result['logout']['exitCode'] = code
            except Exception as error:
                result['backendErrorType'] = type(error).__name__
            try:
                result['credentialsAbsent'] = _file_backend(_read(descriptor, CONFIG, MAX_CONFIG)) == {}
            except FileNotFoundError:
                result['credentialsAbsent'] = result['fileBackend']
            except Exception as error:
                result['absenceErrorType'] = type(error).__name__
            finally:
                result['directoryRemoved'] = _remove(root_fd, descriptor, Path(profile).name)
    except Exception as error:
        result['errorType'] = type(error).__name__
    result['passed'] = (result['ownershipVerified'] and result['fileBackend'] and result['logout']['attempted']
                        and result['logout']['exitCode'] == 0 and result['credentialsAbsent'] and result['directoryRemoved'])
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('operation', choices=('prepare', 'verify', 'cleanup'))
    args = parser.parse_args()
    try:
        identity = identity_from_environment()
        root = Path(os.environ['RUNNER_TEMP'])
        if args.operation == 'prepare':
            profile = prepare(root, identity)
            with open(os.environ['GITHUB_ENV'], 'a', encoding='utf-8') as output:
                output.write('DOCKER_CONFIG=' + str(profile) + '\n')
        elif args.operation == 'verify':
            verify_backend(root, Path(os.environ['DOCKER_CONFIG']), identity)
        else:
            report = cleanup(root, Path(os.environ['DOCKER_CONFIG']), identity)
            print(json.dumps(report, sort_keys=True))
            return 0 if report['passed'] else 1
    except Exception:
        raise SystemExit('owned registry profile refused') from None
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
