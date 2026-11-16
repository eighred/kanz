"""Fail before fixtures if the kernel does not enforce the declared sandbox."""

import errno
import os
from pathlib import Path
import resource


def require(condition, message):
    if not condition:
        raise SystemExit(message)


require(set(os.environ) == {"PATH", "HOME", "TMPDIR", "LANG", "PYTHONPATH", "PYTHONDONTWRITEBYTECODE"}, "unexpected worker environment name")
require(os.environ["HOME"] == os.environ["TMPDIR"] == "/scratch", "scratch environment mismatch")
require(os.getuid() != 0, "worker is root")
require({item.name for item in Path("/sys/class/net").iterdir()} == {"lo"}, "external network interface present")
status = dict(line.split(":", 1) for line in Path("/proc/self/status").read_text().splitlines() if ":" in line)
require(int(status["CapEff"].strip(), 16) == 0, "effective capability present")
require(status["NoNewPrivs"].strip() == "1" and status["Seccomp"].strip() == "2", "kernel privilege/seccomp guard absent")
require(resource.getrlimit(resource.RLIMIT_FSIZE) == (100663296, 100663296), "file limit missing")
require(resource.getrlimit(resource.RLIMIT_NOFILE) == (256, 256), "descriptor limit missing")
require(resource.getrlimit(resource.RLIMIT_CORE) == (0, 0), "core dump limit missing")
cgroup = Path("/sys/fs/cgroup")
require(int((cgroup / "memory.max").read_text().strip()) <= 512 << 20, "memory limit missing")
require(int((cgroup / "pids.max").read_text().strip()) <= 64, "process limit missing")
quota, period = (cgroup / "cpu.max").read_text().split()
require(quota != "max" and int(quota) <= int(period), "CPU limit missing")
space = os.statvfs("/scratch")
require(space.f_blocks * space.f_frsize <= 128 << 20, "scratch disk limit missing")
for directory in ("/source", "/modules", "/tools", "/usr/local/bin", "/tmp", "/dev/shm"):
    try:
        descriptor = os.open(directory + "/must-not-write", os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
    except OSError as error:
        require(error.errno in (errno.EROFS, errno.EACCES), "unexpected input write refusal")
    else:
        os.close(descriptor)
        raise SystemExit("input/toolchain write was permitted")
scratch = Path("/scratch/positive-control")
scratch.write_bytes(b"scratch-only")
require(scratch.read_bytes() == b"scratch-only", "scratch write positive control failed")
scratch.unlink()
print("Sandbox kernel controls passed")
