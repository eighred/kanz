"""Exact native libcurl repair, independent from original compatibility metadata."""
import copy
import re

import openssl_runtime

VERSION = '8.23.0'
ARCHIVE_SHA256 = '97a3b0d65c9e03a30bfdc89185ddf1ef0fe14b7e1a95d93d8ffc9cf57fb2a161'
PATH = 'lib/julia/libcurl.so.4.8.0'
BASE = {'size': 1084728, 'sha256': 'a184796bc4b04a91ec47938d10ded0727b5d6d21fb96c3942ce32eb45faec8a2'}
FILE = {'size': 1101032, 'sha256': '1d977428a3d57ca07b091b2b8e18881119cb5822162fd345f24e94dae16e5fc2'}
LICENSE_SHA256 = '82f2f4427d6545ee5aaac4f0b80428da6cc8ba41c2cf5da3a03680ec327b9681'


def overlay(base, path):
    openssl_runtime.verified_artifact(path, ARCHIVE_SHA256, {
        'lib/libcurl.so.4.8.0': FILE,
        'share/licenses/LibCURL/COPYING': {'size': 1088, 'sha256': LICENSE_SHA256}})
    if (base['files'].get(PATH) != BASE or PATH in base['links']
            or base['jll'].get('LibCURL_jll', {}).get('version') != '8.22.0+0'):
        raise ValueError('libcurl original archive identity differs')
    result = copy.deepcopy(base)
    result['files'][PATH] = FILE.copy()
    return result


def validate_component(inventory):
    item = inventory.get('nativeLibCURL')
    if not isinstance(item, dict) or set(item) != {
            'version', 'versionAPI', 'versionNumber', 'sslBackend', 'features', 'protocols',
            'path', 'file', 'archiveSHA256', 'licenseSHA256', 'originalJLLVersion'}:
        raise ValueError('incomplete native libcurl component evidence')
    protocols = item['protocols']
    if (not isinstance(protocols, list) or not 2 <= len(protocols) <= 32
            or any(not isinstance(p, str) or not re.fullmatch('[a-z0-9]{1,32}', p) for p in protocols)
            or sorted(set(protocols)) != protocols or not {'http', 'https'} <= set(protocols)):
        raise ValueError('libcurl protocol inventory differs')
    features = item['features']
    if type(features) is not int or not 0 <= features < 1 << 31 or features & ((1 << 2) | (1 << 16)) != ((1 << 2) | (1 << 16)):
        raise ValueError('libcurl TLS/HTTP2 feature proof missing')
    expected = {'version': VERSION, 'versionAPI': 'curl_version_info', 'versionNumber': 0x081700,
                'sslBackend': 'OpenSSL/3.5.9', 'features': features, 'protocols': protocols,
                'path': PATH, 'file': FILE, 'archiveSHA256': ARCHIVE_SHA256,
                'licenseSHA256': LICENSE_SHA256, 'originalJLLVersion': '8.22.0+0'}
    native = inventory.get('nativeRuntime', {})
    if (item != expected or native.get('files', {}).get(PATH) != FILE
            or native.get('jll', {}).get('LibCURL_jll', {}).get('version') != '8.22.0+0'):
        raise ValueError('libcurl native and original metadata identities differ')
