"""Warm only the shared, reviewed Docker dependency layer on the disposable daemon."""
import json
from pathlib import Path
import subprocess
import tempfile


root = Path('/work/repo')
pins = json.loads((root / 'tools/dev_compose/images.json').read_bytes())
go = 'ghcr.io/eighred/base/golang:1.27.4'
runtime = 'ghcr.io/eighred/base/distroless-static:nonroot'
stop = 'RUN sh /usr/local/bin/go-mod-download.sh'
expected = [f'FROM {go}@sha256:{pins[go]} AS build', 'WORKDIR /src', 'ENV GOFLAGS=-mod=mod',
            'COPY schemas/gen/go/go.mod schemas/gen/go/go.sum ./schemas/gen/go/',
            'COPY kanz/go.mod kanz/go.sum ./kanz/', 'WORKDIR /src/kanz',
            'COPY kanz/build/go-mod-download.sh /usr/local/bin/go-mod-download.sh', stop]
for name in ('kanz/cmd/kanz_migrate', 'kanz/services/risk_engine', 'kanz/services/api_gateway'):
    lines = (root / name / 'Dockerfile').read_text().splitlines()
    if lines[0] != '# syntax=docker/dockerfile:1' or lines.count('FROM ' + runtime) != 1:
        raise ValueError('unexpected canonical Dockerfile frontend or runtime')
    instructions = [line for line in lines[:lines.index(stop) + 1] if line and not line.startswith('#')]
    if instructions != expected:
        raise ValueError('unreviewed dependency build instructions')
# BuildKit reuses the exact intermediate layer. No repository service or test is built
# before isolation; the added verification layer checks the downloaded module graph.
with tempfile.NamedTemporaryFile('w', suffix='.Dockerfile', dir='/opt/proof') as build:
    build.write('\n'.join([lines[0], *expected, 'RUN go mod verify', 'FROM ' + runtime,
                           'COPY --from=build /usr/local/go/VERSION /COMPILER_VERSION']) + '\n')
    build.flush()
    subprocess.run(['docker', 'build', '--progress=plain', '-t', 'kanz-dev-prefetch:local',
                    '-f', build.name, '.'], cwd=root, check=True)
