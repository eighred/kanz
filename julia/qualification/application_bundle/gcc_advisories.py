"""Recorded GCC package-owner mappings for the two backports without CVE IDs in GCC's commits."""
import hashlib
from urllib.request import Request, urlopen

CENTOS = ('https://gitlab.com/api/v4/projects/redhat%2Fcentos-stream%2Frpms%2Fgcc-toolset-15-gcc/repository/files/',
          'fdd71bbc8ef8471f6cb59f2d5f41e6f6b44103a1')
HUMMINGBIRD = ('https://gitlab.com/api/v4/projects/redhat%2Fhummingbird%2Frpms/repository/files/',
               '788f8b533336c083251f1701f58c37453c78d066')
FROM_CHARS = '816c1a39ca9b656ba2d001c7b6fa421ebe44b3a0'
FILEBUF = '68b1b9056c70acb31f2964963b51c480ff820602'


def raw(owner, file):
    base, revision = owner
    return base + file + '/raw?ref=' + revision


SPECS = {
    'centos-gcc.spec': (raw(CENTOS, 'gcc-toolset-15-gcc.spec'),
                        120106, '1489acd7f6d04176b36c29afc385752dc919cdfb34b765a02a88554a7d9593b3'),
    'hummingbird-gcc.spec': (raw(HUMMINGBIRD, 'rpms%2Fgcc%2Fgcc.spec'),
                             235603, 'b72570e1958a91d6a7d3c4b552cf89e5ab29a10f477fc361af2a4dd4ccd2ca2b'),
}
HEADERS = {
    'gcc15-pr127666.header': (raw(CENTOS, 'gcc15-pr127666.patch'), 241,
                             '06264da444420e17c724c64e9b5d6a95bae3546fa79586330d953cb6ea5fbe63'),
    'gcc16-pr118099.header': (raw(HUMMINGBIRD, 'rpms%2Fgcc%2Fgcc16-pr118099-filebuf-overflow.patch'), 271,
                             'b9e600adb4cf194cf56f9cd92d8780994a036883ba0c445e72a7e0e2ef3c2089'),
}


def collect_headers(directory):
    for name, (url, length, digest) in HEADERS.items():
        request = Request(url, headers={'User-Agent': 'Kanz GCC source qualification',
                                       'Range': 'bytes=0-' + str(length - 1)})
        with urlopen(request, timeout=30) as response:
            # The raw endpoint can return 200 or 206. Only the pinned metadata prefix is consumed.
            if response.geturl() != url or response.status not in (200, 206):
                raise ValueError('unexpected compiler packaging transport')
            header = response.read(length)
        if len(header) != length or hashlib.sha256(header).hexdigest() != digest:
            raise ValueError('GCC packaging header differs')
        with (directory / name).open('xb') as stream:
            stream.write(header)


def mappings(directory):
    records = {}
    for name, (_, length, digest) in (SPECS | HEADERS).items():
        path = directory / name
        if path.stat().st_size != length:
            raise ValueError('GCC packaging record size differs')
        data = path.read_bytes()
        if hashlib.sha256(data).hexdigest() != digest:
            raise ValueError('GCC packaging record differs: ' + name)
        records[name] = set(data.decode('utf-8').splitlines())
    required = {
        'centos-gcc.spec': {'Patch18: gcc15-pr127666.patch', '%patch -P18 -p1 -b .pr127666~',
                            '- fix CVE-2026-115456, PR libstdc++/127666 (RHEL-300043)'},
        'gcc15-pr127666.header': {'From ' + FROM_CHARS + ' Mon Sep 17 00:00:00 2001', ' [PR127666]'},
        'hummingbird-gcc.spec': {'Patch13: gcc16-pr118099-filebuf-overflow.patch', '%autopatch -p0 -m 11 -M 99'},
        'gcc16-pr118099.header': {'CVE-2026-125709', 'Upstream: https://github.com/gcc-mirror/gcc/commit/' + FILEBUF,
                                  'libstdc++: Fix buffer overflows in basic_filebuf::overflow [PR118099]'},
    }
    if any(not expected <= records[name] for name, expected in required.items()):
        raise ValueError('GCC advisory to producer source mapping differs')
    return {'CVE-2026-115456': FROM_CHARS, 'CVE-2026-125709': FILEBUF}
