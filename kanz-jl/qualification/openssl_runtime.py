"""Explicit two-library runtime repair; original JLL metadata is retained."""
import copy
import hashlib
import tarfile

VERSION = '3.5.9'
ARCHIVE_SHA256 = '8e2c75378726017fc66fb7fbbd10fb53522417c3e45a88af77b8853d46f2b063'
URL = 'https://github.com/JuliaBinaryWrappers/OpenSSL_jll.jl/releases/download/OpenSSL-v3.5.9%2B0/OpenSSL.v3.5.9.x86_64-linux-gnu.tar.gz'
FILES = {
    'lib/julia/libcrypto.so.3': {'size': 6634160, 'sha256': '09ac6b8640f13b13badf5d2430a8cd326d27ad77668868ad6c9a36de87c4857d'},
    'lib/julia/libssl.so.3': {'size': 1332784, 'sha256': '044e0615a9f42e6e02298eec073325f528bee55c56901134788220fc5e96a098'},
}
LICENSE_SHA256 = '7d5450cb2d142651b8afa315b5f238efc805dad827d91ba367d8516bc9d49e7a'
BASE_FILES = {
    'lib/julia/libcrypto.so.3': {'size': 6628904, 'sha256': '443c0345cb2a10e55d41c385ef6dbf8af9209b82a2aaaeccf9772c04e398cfae'},
    'lib/julia/libssl.so.3': {'size': 1328688, 'sha256': '81fd035bfa9f36291cab3c3cfe172dc082a3ba439a3f25ebd125003d66c15602'},
}


def verified_members(path):
    if path.stat().st_size > 32 << 20:
        raise ValueError('oversized OpenSSL archive')
    with path.open('rb') as stream:
        if hashlib.file_digest(stream, 'sha256').hexdigest() != ARCHIVE_SHA256:
            raise ValueError('OpenSSL archive identity differs')
    expected = {name.replace('lib/julia/', 'lib/'): item for name, item in FILES.items()}
    expected['share/licenses/OpenSSL/LICENSE.txt'] = {'size': 10175, 'sha256': LICENSE_SHA256}
    found = {}
    # Never extract archive paths or follow their links on the host.
    with tarfile.open(path, 'r:gz') as archive:
        for member in archive:
            if member.name not in expected:
                continue
            if member.name in found or not member.isfile() or member.size != expected[member.name]['size']:
                raise ValueError('ambiguous OpenSSL artifact member')
            stream = archive.extractfile(member)
            found[member.name] = {'size': member.size, 'sha256': hashlib.file_digest(stream, 'sha256').hexdigest()}
    if found != expected:
        raise ValueError('OpenSSL artifact members differ')
    return found


def overlay(base, path):
    verified_members(path)
    result = copy.deepcopy(base)
    if base['jll'].get('OpenSSL_jll', {}).get('version') != '3.5.8+0':
        raise ValueError('unexpected original OpenSSL wrapper')
    for name, item in FILES.items():
        if base['files'].get(name) != BASE_FILES[name] or name in base['links']:
            raise ValueError('OpenSSL replacement target missing or linked')
        result['files'][name] = item.copy()
    return result


def validate_component(inventory):
    expected = {'version': VERSION, 'versionAPI': 'OpenSSL_version(0)',
                'archiveSHA256': ARCHIVE_SHA256, 'files': FILES,
                'licenseSHA256': LICENSE_SHA256, 'originalJLLVersion': '3.5.8+0'}
    if inventory.get('nativeOpenSSL') != expected:
        raise ValueError('actual OpenSSL component not bound to repaired artifact')
    native = inventory.get('nativeRuntime', {})
    if any(native.get('files', {}).get(name) != item for name, item in FILES.items()):
        raise ValueError('installed OpenSSL bytes differ')
    if native.get('jll', {}).get('OpenSSL_jll', {}).get('version') != '3.5.8+0':
        raise ValueError('original OpenSSL wrapper metadata not retained')
