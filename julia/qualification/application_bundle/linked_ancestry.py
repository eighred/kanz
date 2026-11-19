"""Acquire commit-only native source graphs and replay cryptographically bound parent paths offline."""
import base64
from collections import deque
import hashlib
import io
import json
from pathlib import Path
import subprocess
import tempfile


UV_HEAD = 'b21d6d84e46f6c97ecbc8e4e8a8ea6ad98049ea8'
UTF8_HEAD = 'e5e799221b45bbb90f5fdc5c69b6b8dfbf017e78'
# The v1.48 release notes enumerate three changes. The CNA additionally
# identifies their upstream merge. The two historical CVEs have separate fixes.
UV_RELEASE_FIXES = ('0f2d7e784a256b54b2385043438848047bc2a629', '3530bcc30350d4a6ccf35d2f7b33e23292b9de70',
                    'e0327e1d508b8207c9150b6e582f0adf26213c39')
UV_MERGE = 'c858a147643de38a09dd4164758ae5b685f2b488'
UV_WINDOWS = '3eb6764acd2d708f6873c177a77f9bef3b266fa9'
UV_UNIX = '66ab38918c911bcff025562cf06237d7fedaba0c'
UV_147 = 'be6b81a352d17513c95be153afcb3148f1a451cd'
# Each negative control is a real pre-fix release, or the parent immediately
# preceding the 2015 Unix fix on the same ancestry line.
COMPONENTS = {
    'utf8proc': ('JuliaStrings/utf8proc', UTF8_HEAD, {
        '2cf0bf5ba10c41eb47166e666fd1ad21289397c9': '90daf9f396cfec91668758eb9cc54bd5248a6b89',
    }),
    'libuv': ('JuliaLang/libuv', UV_HEAD, {
        **dict.fromkeys((*UV_RELEASE_FIXES, UV_MERGE), UV_147),
        UV_WINDOWS: '93877b11c8b86e0a6befcda83a54555c1e36e4f0',
        UV_UNIX: '3901ec49767a5b66bebe0a46cf851579f5f040c0',
    }),
}


def path_to(parents, child, ancestor):
    if child == ancestor:
        raise ValueError('negative control cannot be the same commit')
    visited = {child: None}
    queue = deque([child])
    while queue:
        node = queue.popleft()
        if node == ancestor:
            path = [node]
            while path[-1] != child:
                path.append(visited[path[-1]])
            return list(reversed(path))
        for parent in parents.get(node, ()):
            if parent not in visited:
                queue.append(parent)
                visited[parent] = node
    raise ValueError('expected immutable native ancestry was not established')


def collect(output, repo, head, fixes):
    revisions = sorted({head, *fixes, *fixes.values()})
    with tempfile.TemporaryDirectory() as temp:
        database = Path(temp) / 'public.git'
        subprocess.run(['git', 'init', '-q', '--bare', database], check=True)
        git = ['git', '--no-replace-objects', '--git-dir', str(database)]
        subprocess.run([*git, '-c', 'credential.helper=', 'fetch', '--filter=tree:0', '--no-tags', '--quiet',
                        'https://github.com/' + repo + '.git', *revisions], timeout=150, check=True)
        graph = subprocess.check_output([*git, 'rev-list', '--parents', *revisions], timeout=60).decode('ascii')
        parents = {words[0]: words[1:] for words in (line.split() for line in graph.splitlines())}
        selected = set()
        for fix, old in fixes.items():
            selected.update(path_to(parents, head, fix))
            selected.update(path_to(parents, fix, old))
            status = subprocess.run([*git, 'merge-base', '--is-ancestor', fix, old], timeout=15).returncode
            if status != 1:
                raise ValueError('pre-fix reference unexpectedly contains the fix')
        nodes = sorted(selected)
        batch = subprocess.check_output([*git, 'cat-file', '--batch'], input=('\n'.join(nodes) + '\n').encode(), timeout=30)
        stream = io.BytesIO(batch)
        commits = {}
        for wanted in nodes:
            name, kind, size = stream.readline().decode('ascii').split()
            if name != wanted or kind != 'commit' or not 0 < int(size) < 65536:
                raise ValueError('unexpected public Git object')
            commits[name] = base64.b64encode(stream.read(int(size))).decode('ascii')
            if stream.read(1) != b'\n':
                raise ValueError('invalid public Git stream')
        if stream.read(1):
            raise ValueError('unexpected additional Git data')
    output.write_text(json.dumps(commits, sort_keys=True) + '\n')
    verify(output, head, fixes)


def verify(path, head, fixes):
    if path.stat().st_size > 8 << 20:
        raise ValueError('unbounded native commit evidence')
    objects = json.loads(path.read_bytes())
    if not 0 < len(objects) <= 4096:
        raise ValueError('unbounded native commit graph')
    parents = {}
    for name, encoded in objects.items():
        data = base64.b64decode(encoded, validate=True)
        if len(data) > 65536 or hashlib.sha1(b'commit ' + str(len(data)).encode() + b'\0' + data).hexdigest() != name:
            raise ValueError('native Git commit content differs')
        header = data.split(b'\n\n', 1)[0].splitlines()
        parents[name] = [line[7:].decode('ascii') for line in header if line.startswith(b'parent ')]
    for fix, old in fixes.items():
        if not {head, fix, old} <= parents.keys():
            raise ValueError('native source or control absent')
        path_to(parents, head, fix)
        # A strict parent chain from the fixed commit back to the earlier
        # control establishes the direction without trusting a producer Boolean.
        path_to(parents, fix, old)
