"""Verify an immutable application candidate with the standard Go oracle and runtime limits."""
import argparse
import contextlib
import json
from pathlib import Path
import re
import sys
import tempfile
import uuid

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from proof import CASES, CommandTimeout, command, container_boundary, oracle, required


# Ask the kernel for the first byte beyond the tmpfs limit. Julia's buffered
# write can return a short count and drop the underlying error.
QUOTA = r"""
@assert VERSION == v"1.13.3"
@assert Set(pkg.name for pkg in Base._sysimage_modules) ==
    Set(["Base", "Core", "Main", "KanzCurveCandidate", "ProtoBuf", "EnumX", "Unicode", "Dates", "Printf", "BufferedStreams", "TOML"])
@assert ccall(:geteuid, Cuint, ()) == 65532
@assert readdir("/sys/class/net") == ["lo"]
@assert strip(read("/sys/fs/cgroup/memory.max", String)) == "1073741824"
@assert strip(read("/sys/fs/cgroup/pids.max", String)) == "64"
@assert split(read("/sys/fs/cgroup/cpu.max", String)) == ["100000", "100000"]
status = read("/proc/self/status", String)
@assert occursin(r"(?m)^CapEff:\s+0000000000000000$", status)
@assert occursin(r"(?m)^NoNewPrivs:\s+1$", status)
@assert ENV["JULIA_PKG_OFFLINE"] == "true"
buffer = zeros(UInt8, 1 << 20)
open("/tmp/bounded", "w") do io
    for _ in 1:16
        n = ccall(:write, Cssize_t, (Cint, Ptr{UInt8}, Csize_t), fd(io), buffer, length(buffer))
        @assert n == length(buffer)
    end
    n = ccall(:write, Cssize_t, (Cint, Ptr{UInt8}, Csize_t), fd(io), buffer, 1)
    reason = Libc.errno()
    @assert n == -1 && reason == Libc.ENOSPC
end
@assert filesize("/tmp/bounded") == 16 << 20
println("ENOSPC at 16777216 bytes")
"""


@contextlib.contextmanager
def worker(image, source, quota=False):
    name = 'kanz-julia-app-' + uuid.uuid4().hex
    label = 'kanz.julia-proof=' + source
    try:
        args = ['docker', 'create', '--pull=never', '--name', name, '--label', label,
                  '--platform', 'linux/amd64', '--network', 'none', '--read-only', '--user', '65532:65532',
                  '--cap-drop', 'ALL', '--security-opt', 'no-new-privileges', '--cpus', '1',
                  '--memory', '1g', '--memory-swap', '1g', '--pids-limit', '64',
                  '--tmpfs', '/tmp:rw,noexec,nosuid,size=16777216,mode=1777', '-i']
        entrypoint = '/opt/kanz/bin/curve-worker'
        if quota:
            entrypoint = '/opt/kanz/bin/julia'
            args += ['--entrypoint', entrypoint, image, '--startup-file=no', '--history-file=no', '-e', QUOTA]
        else:
            args += [image]
        required(args)
        info, = json.loads(required(['docker', 'inspect', name]))
        container_boundary(info, image, source)
        external = [m for m in info['Mounts'] if (m['Type'], m['Destination']) != ('tmpfs', '/tmp')]
        if info['Config']['Entrypoint'] != [entrypoint] or external:
            raise ValueError('unexpected application entrypoint or external mount')
        yield name
    finally:
        selector = ['docker', 'ps', '-aq', '--filter', 'name=^/' + name + '$']
        cid = required(selector).decode().strip()
        if cid:
            info, = json.loads(required(['docker', 'inspect', cid]))
            if info['Config']['Labels'].get('kanz.julia-proof') != source or info['Name'] != '/' + name:
                raise ValueError('refusing to clean up an unowned container')
            required(['docker', 'rm', '--force', cid])
        if required(selector).strip():
            raise RuntimeError('owned application candidate survived cleanup')


def bounded_controls(image, source):
    with worker(image, source, quota=True) as name:
        if required(['docker', 'start', '--attach', name]).strip() != b'ENOSPC at 16777216 bytes':
            raise ValueError('tmpfs upper bound differed')
    # Keep stdin open on the real entrypoint. The owning parent must be able to
    # time out and reap a worker that is still waiting for a complete request.
    with worker(image, source) as name:
        required(['docker', 'start', name])
        try:
            required(['docker', 'wait', name], timeout=2)
        except CommandTimeout:
            info, = json.loads(required(['docker', 'inspect', name]))
            container_boundary(info, image, source)
            if not info['State']['Running'] or info['State']['OOMKilled']:
                raise ValueError('timed-out application was not running')
        else:
            raise ValueError('open application request did not time out')


def main(image, source):
    if not re.fullmatch('sha256:[0-9a-f]{64}', image) or not re.fullmatch('[0-9a-f]{40}', source):
        raise ValueError('expected immutable image and Git identities')
    info, = json.loads(required(['docker', 'image', 'inspect', image]))
    if (info['Config'].get('Labels') or {}).get('org.opencontainers.image.revision') != source:
        raise ValueError('application source revision differs')
    bounded_controls(image, source)
    with tempfile.TemporaryDirectory() as directory:
        scratch = Path(directory)
        oracle(scratch, 'prepare')
        rows = json.loads((scratch / 'cases.json').read_bytes())
        if len(rows) != len(CASES) or {case['name'] for case in rows} != CASES:
            raise ValueError('independent wire corpus differs')
        for case in rows:
            key = case['name']
            with worker(image, source) as name:
                code, response = command(['docker', 'start', '--attach', '--interactive', name],
                                         data=(scratch / (key + '.input')).read_bytes())
            if (code == 0) != case['exitZero'] or (code and response):
                raise ValueError('application wire exit contract differs: ' + key)
            (scratch / (key + '.output')).write_bytes(response)
        oracle(scratch, 'verify')
    print('Passed 13 real application wire cases, Go oracle, runtime isolation, tmpfs quota, timeout and cleanup.')


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('image')
    parser.add_argument('source')
    args = parser.parse_args()
    main(args.image, args.source)
