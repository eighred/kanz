"""Qualify the Go order lineage gate in a disposable generation-one OMS database.

Build prerequisites and the native Linux amd64 executable as described in
kanz/dev/synthetic-orders.md. The pinned database and runtime images must be
present in Docker. No host database, published port or venue is used.
"""
import argparse
import hashlib
import os
from pathlib import Path
import secrets
import shutil
import subprocess
import tempfile
import time

import synthetic_orders as fixture


TEST = "TestSyntheticGoWriterLineageHandoff"
ROLES = ("RUNTIME", "NEXT_RUNTIME", "SWITCH", "RELAY")


def run(stage, native):
    fixture.build(stage, ["services/api_gateway/cmd/api_gateway", "services/oms/internal/order"])
    staged_native = stage / "native"
    shutil.copyfile(native, staged_native)
    staged_native.chmod(0o755)
    with staged_native.open("rb") as f:
        print("Rust owner SHA-256", hashlib.file_digest(f, "sha256").hexdigest(), flush=True)

    # Migration access stays on the Unix socket. Only these four independent
    # logins need loopback access to test a retained order and attempt a switch.
    hba = stage / "pg_hba.conf"
    logins = ",".join(fixture.ROLES[name] for name in ROLES)
    hba.write_text("local all postgres peer\nlocal all all reject\n"
                   f"host {fixture.DB} {logins} 127.0.0.1/32 scram-sha-256\n"
                   "host all all 0.0.0.0/0 reject\nhost all all ::/0 reject\n", encoding="ascii")
    password = secrets.token_hex(32)
    container = fixture.output("docker", "run", "--pull=never", "--rm", "-d", "--network", "none",
                               "--tmpfs", "/var/lib/postgresql/data", "-e", "POSTGRES_PASSWORD",
                               "-v", f"{hba}:/fixture-hba:ro", fixture.PG, "-c", "hba_file=/fixture-hba",
                               env=dict(os.environ, POSTGRES_PASSWORD=password))
    try:
        for _ in range(60):
            ready = subprocess.run(["docker", "exec", "--user", "postgres", container,
                                    "pg_isready", "-h", "127.0.0.1"], capture_output=True)
            if ready.returncode == 0:
                break
            time.sleep(0.5)
        else:
            raise RuntimeError("disposable database did not start")
        fixture.sql(container, f"CREATE DATABASE {fixture.DB}")
        env = {}
        for migration in fixture.provision_native(container, env):
            fixture.sql(container, migration.read_text(encoding="utf-8"), fixture.DB)
        names = [f"SYNTHETIC_OMS_{role}_URL" for role in ROLES]
        env = {name: env[name] for name in names}
        env.update(KANZ_SYNTHETIC_GO_LINEAGE="1", KANZ_SYNTHETIC_OMS_BIN="/fixture/native")
        cmd = ["docker", "run", "--pull=never", "--rm", "--platform", "linux/amd64", "--read-only",
               "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--user", "65532:65532",
               "--network", "container:" + container, "--tmpfs", "/tmp:exec,mode=1777",
               "-v", f"{stage}:/fixture:ro", *[arg for name in env for arg in ("-e", name)],
               "--entrypoint", "/fixture/api_gateway", fixture.RUNTIME,
               "-test.v", "-test.count=1", "-test.parallel=1", "-test.timeout=1m"]
        print(fixture.run_fixture_tests(cmd, [TEST], env, password), end="", flush=True)
    finally:
        subprocess.run(["docker", "stop", "-t", "1", container], stdout=subprocess.DEVNULL, check=True)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--native", type=Path, required=True, help="Linux amd64 kanz-synthetic-oms executable")
    args = parser.parse_args()
    if not args.native.is_file():
        parser.error("--native requires a built Linux executable")
    with tempfile.TemporaryDirectory(prefix="kanz-go-writer-lineage-") as directory:
        run(Path(directory), args.native)
