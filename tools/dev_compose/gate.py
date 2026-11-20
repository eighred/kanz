"""The normal Make prerequisites have completed. Wait for the host's network handoff."""
from collections import Counter
import hashlib
from pathlib import Path
import time


root = Path('/work/repo')
generated = root / 'schemas/gen'
files = sorted(p for p in generated.rglob('*') if p.is_file())
languages = Counter(p.relative_to(generated).parts[0] for p in files)
if not (languages['go'] and languages['python'] and languages['ts']
        and (generated / 'go/go.mod').is_file() and (generated / 'go/go.sum').stat().st_size
        and any(p.name.endswith('_grpc.pb.go') for p in files)
        and any(p.name.endswith('_pb2.py') for p in files)
        and any(p.name.endswith('_pb2.pyi') for p in files)
        and any(p.name.endswith('_pb2_grpc.py') for p in files)
        and any(p.name.endswith('.d.ts') for p in files)):
    raise ValueError('full SDK generation is incomplete')
tree = hashlib.sha256()
for path in files:
    tree.update(path.relative_to(generated).as_posix().encode() + b'\0' + hashlib.sha256(path.read_bytes()).digest())
print('Generated SDK files', dict(languages), 'tree SHA-256', tree.hexdigest(), flush=True)
Path('/home/developer/sdk-ready').touch()
deadline = time.monotonic() + 900
while not Path('/run/kanz-lifecycle-authorized').is_file():
    if time.monotonic() >= deadline:
        raise TimeoutError('owned network handoff timed out')
    time.sleep(1)
