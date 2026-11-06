"""Select the official HTTPS Ubuntu archive on disposable GitHub Linux runners.

Only URI bytes change; signed suites/components/architectures remain untouched.
No apt authentication flags, packages, or Playwright commands are changed.
"""
import os
from pathlib import Path
import re
import stat

SOURCE = Path('/etc/apt/sources.list.d/ubuntu.sources')
TARGET = b'https://archive.ubuntu.com/ubuntu/'
ALLOWED = {b'mirror+file:/etc/apt/apt-mirrors.txt'} | {
    (scheme + host + '/ubuntu' + tail).encode('ascii')
    for scheme in ('http://', 'https://')
    for host in ('azure.archive.ubuntu.com', 'archive.ubuntu.com', 'security.ubuntu.com')
    for tail in ('', '/')
}
LIMIT = 65536


def rewrite(raw):
    if not raw or len(raw) > LIMIT or b'\x00' in raw:
        raise ValueError('unsupported Ubuntu source size/content')
    raw.decode('utf-8', errors='strict')
    output = []
    fields = {}
    stanzas = 0

    def finish():
        nonlocal stanzas
        if not fields:
            return
        if (fields.get(b'Signed-By') != b'/usr/share/keyrings/ubuntu-archive-keyring.gpg'
                or not all(fields.get(key) for key in (b'Types', b'URIs', b'Suites', b'Components'))):
            raise ValueError('Ubuntu source lacks explicit archive signature or repository fields')
        stanzas += 1
        fields.clear()

    for line in raw.splitlines(keepends=True):
        value = line.rstrip(b'\r\n')
        if not value.strip():
            finish()
        elif not value.startswith(b'#'):
            match = re.fullmatch(rb'([A-Za-z][A-Za-z-]*):([ \t]*)([^\r\n]*)', value)
            if (not match or match[1] in fields or match[1] not in
                    {b'Types', b'URIs', b'Suites', b'Components', b'Signed-By', b'Architectures'}):
                raise ValueError('unsupported or duplicate Ubuntu source field')
            key, space, content = match.groups()
            fields[key] = content.strip()
            if key == b'URIs':
                if fields[key] not in ALLOWED:
                    raise ValueError('unsupported Ubuntu source URI')
                line = key + b':' + space + TARGET + line[len(value):]
        output.append(line)
    finish()
    if not stanzas:
        raise ValueError('Ubuntu source has no repository stanza')
    return b''.join(output)


def apply():
    if os.environ.get('GITHUB_ACTIONS') != 'true' or os.environ.get('RUNNER_OS') != 'Linux':
        raise ValueError('archive selection is restricted to disposable GitHub Linux jobs')
    # Never follow a substituted source symlink; validate everything before write.
    fd = os.open(SOURCE, os.O_RDWR | os.O_NOFOLLOW | os.O_NONBLOCK)
    with os.fdopen(fd, 'r+b') as source:
        info = os.fstat(source.fileno())
        if not stat.S_ISREG(info.st_mode) or info.st_size > LIMIT:
            raise ValueError('Ubuntu source is not a bounded regular file')
        raw = source.read(LIMIT + 1)
        changed = rewrite(raw)
        source.seek(0)
        source.write(changed)
        source.truncate()
        source.flush()
        os.fsync(source.fileno())
        source.seek(0)
        if source.read(LIMIT + 1) != changed:
            raise ValueError('Ubuntu source readback differs')
    print('Ubuntu archive uses official HTTPS; repository fields and signature policy preserved')


if __name__ == '__main__':
    apply()
