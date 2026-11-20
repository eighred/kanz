#!/usr/bin/env python3
"""Run the synthetic signed-custody operator workflow on a private local fixture."""

import argparse
import json
import os
from pathlib import Path
import re
import secrets
import shutil
import subprocess
import sys
import tempfile
import time

ROOT = Path(__file__).resolve().parents[2]
MODULE = ROOT / "kanz"
WEB = ROOT / "web"
CMD = MODULE / "services/accounting/cmd/custody_batch"
IMAGE = "postgres@sha256:aa6cb33cf4c19c765cefcf48d96176258800870a9a1e930ad126200f9d825139"
DATABASE = "kanz_custody_pilot"
PACKAGES = {
    "accounting": "./services/accounting/cmd/custody_batch",
    "gateway": "./services/api_gateway/cmd/api_gateway",
    "bff": "./services/web_bff/internal/server",
}


def host_environment():
    # Runtime settings for local programs, excluding ambient service URLs,
    # bearer tokens, proxy credentials and database connection variables.
    names = (
        "PATH", "SystemRoot", "ComSpec", "PATHEXT", "TEMP", "TMP", "TMPDIR",
        "HOME", "USERPROFILE", "LOCALAPPDATA", "APPDATA", "USER", "LOGNAME",
        "XDG_RUNTIME_DIR", "PLAYWRIGHT_BROWSERS_PATH",
    )
    return {name: os.environ[name] for name in names if name in os.environ}


def build(node, work, env):
    if not (MODULE.parent / "schemas/gen/go/go.mod").is_file():
        raise RuntimeError("Generate the local schema SDK before running the pilot.")
    if not (WEB / "node_modules/playwright/package.json").is_file():
        raise RuntimeError("Install the locked Web dependencies and Chromium first.")
    major = subprocess.check_output([node, "-p", "process.versions.node.split('.')[0]"], env=env, text=True)
    if major.strip() != (WEB / ".nvmrc").read_text().strip():
        raise RuntimeError("Select the Node major version in web/.nvmrc.")

    # Use the repository's prebuild, both type checkers and the Vite production
    # build. Invoke npm through the selected Node even on Windows.
    npm = shutil.which("npm")
    if npm is None:
        raise RuntimeError("npm is required for the Web production build.")
    cli = Path(npm).resolve()
    if cli.name != "npm-cli.js":
        cli = cli.parent / "node_modules/npm/bin/npm-cli.js"
    if not cli.is_file():
        raise RuntimeError("Could not locate the installed npm CLI.")
    build_env = dict(env, PATH=str(Path(node).parent) + os.pathsep + env["PATH"])
    for name in ("RUSTUP_HOME", "CARGO_HOME", "GOPATH", "GOCACHE", "GOTMPDIR"):
        if name in os.environ:
            build_env[name] = os.environ[name]
    subprocess.run([node, cli, "run", "build"], cwd=WEB, env=build_env, check=True)
    binaries = {}
    for name, package in PACKAGES.items():
        binary = work / (name + (".exe" if os.name == "nt" else ""))
        subprocess.run(["go", "test", "-mod=readonly", "-p", "1", "-c", "-o", binary, package],
                       cwd=MODULE, env=dict(build_env, GOWORK="off"), check=True)
        binaries[name] = binary
    return binaries


def local_engine(env):
    result = subprocess.check_output(
        ["docker", "context", "inspect", "--format", "{{.Endpoints.docker.Host}}"],
        env=env, text=True,
    ).strip()
    if not (result.startswith("unix:///") or result.startswith("npipe:////./pipe/")):
        raise RuntimeError("A local Docker engine socket or Windows named pipe is required.")
    return ["docker", "--host", result]


def qualify(docker, container, binaries, node, env):
    owner_password = secrets.token_hex(24)
    source_password = secrets.token_hex(24)

    def sql(database, script, *variables):
        # Secrets for these two fresh logins travel only over the local stdin.
        call = [*docker, "exec", "-i", "--user", "postgres", container,
                "psql", "-Xq", "-v", "ON_ERROR_STOP=1", "-U", "postgres", "-d", database, *variables]
        subprocess.run(call, input=script, check=True, env=env,
                       stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=30)

    for _ in range(80):
        check = subprocess.run([*docker, "exec", "--user", "postgres", container, "pg_isready",
                                "-h", "127.0.0.1", "-U", "postgres"], env=env, capture_output=True, timeout=5)
        if check.returncode == 0:
            break
        time.sleep(0.25)
    else:
        raise RuntimeError("Disposable PostgreSQL did not become ready.")

    sql("postgres", (
        f"CREATE ROLE pilot_check LOGIN NOSUPERUSER NOBYPASSRLS PASSWORD '{owner_password}';\n"
        f"CREATE DATABASE {DATABASE} OWNER pilot_check;\n"
    ).encode())
    for migration in sorted((CMD.parents[1] / "migrations").glob("*.sql")):
        try:
            sql(DATABASE, b"SET ROLE pilot_check;\n" + migration.read_bytes())
        except subprocess.CalledProcessError:
            raise RuntimeError(f"Accounting migration failed at {migration.name}.") from None
    authority = json.loads((CMD / "testdata/authority.json").read_bytes())
    sql(DATABASE, (CMD / "pilot_grants.sql").read_bytes(), "-v", "public_key=" + authority["public_key"])
    sql("postgres", (
        "CREATE ROLE pilot_source LOGIN NOSUPERUSER NOBYPASSRLS IN ROLE kanz_custody_batch_pilot "
        f"PASSWORD '{source_password}';"
    ).encode())

    endpoint = subprocess.check_output([*docker, "port", container, "5432/tcp"], env=env, text=True).strip()
    if not re.fullmatch(r"127\.0\.0\.1:[1-9]\d{0,4}", endpoint) or int(endpoint.split(":")[1]) > 65535:
        raise RuntimeError("PostgreSQL must have exactly one literal loopback port.")
    pilot_env = dict(env,
        TEST_POSTGRES_URL=f"postgres://pilot_check:{owner_password}@{endpoint}/{DATABASE}?sslmode=disable",
        TEST_CUSTODY_BATCH_DATABASE_URL=f"postgres://pilot_source:{source_password}@{endpoint}/{DATABASE}?sslmode=disable",
        CUSTODY_PILOT_VERIFY="1", CUSTODY_PILOT_GATEWAY_PROBE=str(binaries["gateway"]),
        CUSTODY_PILOT_BFF_PROBE=str(binaries["bff"]), TEST_MFA_NODE=node,
        TEST_MFA_STATIC_DIR=str(WEB / "dist"), TEST_MFA_PLAYWRIGHT=str(WEB / "node_modules/playwright"),
        CUSTODY_PILOT_BROWSER_SCRIPT=str(WEB / "e2e/custody-detection.cjs"),
    )
    proof = subprocess.run([binaries["accounting"], "-test.run=^TestSignedCustodyOperatorReadback$",
                            "-test.v", "-test.count=1"], cwd=CMD, env=pilot_env, capture_output=True, timeout=155)
    output = proof.stdout
    if (proof.returncode or b"--- SKIP" in output or b"--- PASS: TestSignedCustodyOperatorReadback" not in output
            or b"signed gateway and Chromium verified the persisted detection" not in output):
        stage = re.search(rb"custody-browser-stage=(input|launch|(?:authorized|foreign|excluded|unfunded)-(?:open|reload)-(?:response|payload|row|details))", output)
        suffix = " at " + stage.group(1).decode() if stage else ""
        raise RuntimeError("Signed custody operator proof failed" + suffix + ".")
    print("PASS: Chromium verified the retained signed break on load and reload, the three restricted "
          "sessions, and read-only controls. Accounting confirmed the correction cleared the queue.")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--node", default="node", help="Node executable matching web/.nvmrc")
    args = parser.parse_args()
    node = shutil.which(args.node)
    if not node:
        raise RuntimeError("Node executable unavailable.")
    env = host_environment()
    docker = local_engine(env)
    with tempfile.TemporaryDirectory(prefix="kanz-custody-pilot-") as directory:
        binaries = build(node, Path(directory), env)
        # The image initializes its own secret in tmpfs. Local SQL administration
        # uses the postgres OS identity (peer); network logins require SCRAM.
        name = "kanz-custody-pilot-" + secrets.token_hex(8)
        run = [*docker, "run", "--detach", "--rm", "--pull=never", "--name", name,
               "--publish", "127.0.0.1::5432", "--tmpfs", "/var/lib/postgresql/data",
               "--tmpfs", "/run/pilot", "--env", "POSTGRES_INITDB_ARGS=--auth-local=peer --auth-host=scram-sha-256",
               "--env", "POSTGRES_PASSWORD_FILE=/run/pilot/admin", "--entrypoint", "sh", IMAGE, "-ec",
               "umask 077; head -c 32 /dev/urandom | base64 > /run/pilot/admin; "
               "chown postgres /run/pilot/admin; exec docker-entrypoint.sh postgres"]
        # Use the exact returned container ID for cleanup. No shared DB or
        # external service connection is accepted by the entrypoint.
        container = subprocess.check_output(run, env=env, text=True).strip()
        try:
            qualify(docker, container, binaries, node, env)
        finally:
            subprocess.run([*docker, "rm", "--force", container], env=env, check=True, stdout=subprocess.DEVNULL)


if __name__ == "__main__":
    try:
        main()
    except (OSError, ValueError, RuntimeError, subprocess.SubprocessError) as error:
        sys.exit(str(error))
