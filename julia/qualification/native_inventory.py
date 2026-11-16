"""Bound installed runtime bytes to the verified archive, not advisory clearance."""
import hashlib
from pathlib import PurePosixPath
import posixpath
import re
import tarfile
import tomllib

ARCHIVE_SHA256 = '5c61cb4b40332cb5a3504e7779e2f5dac093caa4a1116ded19f01413f85c63e4'
MAX_FILES = 4096
MAX_BYTES = 2 << 30
COVERAGE = 'native-library-advisory-ownership-unqualified'


def safe_path(name):
    return (isinstance(name, str) and 0 < len(name) <= 1024 and re.fullmatch(r'[A-Za-z0-9._+/-]+', name)
            and not name.startswith('/') and all(p not in ('', '.', '..') for p in name.split('/')))


def selected(name):
    return name.startswith(('bin/', 'lib/', 'libexec/', 'share/julia/compiled/'))


def safe_link(name, target):
    if (not isinstance(target, str) or not 0 < len(target) <= 1024
            or target.startswith('/') or not re.fullmatch(r'[A-Za-z0-9._+/-]+', target)):
        return False
    resolved = posixpath.normpath(str(PurePosixPath(name).parent / target))
    return safe_path(resolved) and selected(resolved)


def expected_archive(path):
    if path.stat().st_size > 512 << 20:
        raise ValueError('oversized runtime archive')
    with path.open('rb') as stream:
        if hashlib.file_digest(stream, 'sha256').hexdigest() != ARCHIVE_SHA256:
            raise ValueError('runtime archive identity differs')
    files, links, packages, seen = {}, {}, {}, set()
    total = 0
    with tarfile.open(path, 'r:gz') as archive:
        for member in archive:
            prefix, separator, name = member.name.partition('/')
            if prefix != 'julia-1.13.3' or not separator:
                if member.isdir() and member.name.rstrip('/') == 'julia-1.13.3':
                    continue
                raise ValueError('unexpected runtime archive root')
            name = name.rstrip('/') if member.isdir() else name
            if not safe_path(name):
                raise ValueError('unsafe runtime archive path')
            jll = re.fullmatch(r'share/julia/stdlib/v1\.13/([A-Za-z0-9_]+_jll)/Project\.toml', name)
            if not selected(name) and not jll:
                continue
            if name in seen or len(seen) >= MAX_FILES:
                raise ValueError('duplicate or excessive runtime archive members')
            seen.add(name)
            if member.isdir():
                continue
            if member.issym() and not jll:
                if not safe_link(name, member.linkname):
                    raise ValueError('unsafe runtime link')
                links[name] = member.linkname
                continue
            if not member.isfile() or member.size < 0 or member.size > 768 << 20:
                raise ValueError('unsupported runtime member')
            total += member.size
            if total > MAX_BYTES:
                raise ValueError('runtime inventory exceeds byte bound')
            stream = archive.extractfile(member)
            if jll:
                if member.size > 65536:
                    raise ValueError('oversized JLL metadata')
                raw = stream.read()
                meta = tomllib.loads(raw.decode())
                package = {k: meta[k] for k in ('name', 'uuid', 'version')}
                if (package['name'] != jll[1] or package['name'] in packages
                        or not re.fullmatch('[0-9a-f]{8}(-[0-9a-f]{4}){3}-[0-9a-f]{12}', package['uuid'])
                        or not re.fullmatch('[0-9][0-9A-Za-z.+-]{0,127}', package['version'])):
                    raise ValueError('JLL identity differs')
                package.update(sha256=hashlib.sha256(raw).hexdigest(), size=member.size)
                packages[package['name']] = package
            else:
                files[name] = {'size': member.size, 'sha256': hashlib.file_digest(stream, 'sha256').hexdigest()}
    if not files or not links or not packages:
        raise ValueError('incomplete runtime archive inventory')
    for name, target in links.items():
        # Resolve every chain without following host filesystem links.
        visited = {name}
        resolved = posixpath.normpath(str(PurePosixPath(name).parent / target))
        while resolved in links:
            if resolved in visited:
                raise ValueError('cyclic runtime link')
            visited.add(resolved)
            resolved = posixpath.normpath(str(PurePosixPath(resolved).parent / links[resolved]))
        if resolved not in files:
            raise ValueError('runtime link has no inventoried regular target')
    return {'files': files, 'links': links, 'jll': packages}


def validate(observed, expected):
    if not isinstance(observed, dict) or set(observed) != {
            'files', 'links', 'jll', 'generatedNativeImages', 'unassignedFiles', 'loadedOutsideArchive', 'coverage'}:
        raise ValueError('missing native runtime evidence')
    if any(observed[k] != expected[k] for k in ('files', 'links', 'jll')):
        raise ValueError('installed runtime differs from verified archive')
    generated = observed['generatedNativeImages']
    if not isinstance(generated, dict) or len(generated) > MAX_FILES:
        raise ValueError('unbounded generated native images')
    for path, item in generated.items():
        if not re.fullmatch(r'compiled/[A-Za-z0-9_./+-]{1,256}\.so', path) or not safe_path(path):
            raise ValueError('unexpected generated native image path')
        validate_file(item)
    if sum(item['size'] for item in generated.values()) > MAX_BYTES:
        raise ValueError('generated native image byte bound')
    if observed['unassignedFiles'] != sorted(list(expected['files']) + ['depot/' + p for p in generated]) or observed['coverage'] != COVERAGE:
        raise ValueError('unqualified native ownership must remain explicit')
    outside = observed['loadedOutsideArchive']
    if not isinstance(outside, dict) or not outside or len(outside) > 64:
        raise ValueError('invalid external runtime inventory')
    for path, item in outside.items():
        if (not re.fullmatch(r'/(?:usr/)?lib(?:64)?/[A-Za-z0-9_./+-]{1,256}', path)
                or '..' in path.split('/') or not isinstance(item, dict)
                or set(item) != {'size', 'sha256'} or type(item['size']) is not int
                or not 0 < item['size'] <= 128 << 20
                or not re.fullmatch('[0-9a-f]{64}', item['sha256'])):
            raise ValueError('unbounded or unidentified external runtime library')


def validate_file(item):
    if (not isinstance(item, dict) or set(item) != {'size', 'sha256'}
            or type(item['size']) is not int or not 0 < item['size'] <= 768 << 20
            or not re.fullmatch('[0-9a-f]{64}', item['sha256'])):
        raise ValueError('invalid generated native image identity')
