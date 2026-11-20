#!/usr/bin/env python3
"""Run the synthetic signed-custody operator workflow on a private local fixture."""

import argparse
import hashlib
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
import uuid

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


def verify_observations(path, rerun=False):
    records = json.loads(path.read_bytes())
    revisions = [1, 1, 2] if rerun else [1, 2]
    if not isinstance(records, list) or len(records) != len(revisions):
        raise RuntimeError("Both signed statement revisions must have a retained comparison.")
    runs = set()
    for sequence, (revision, record) in enumerate(zip(revisions, records), 1):
        if not isinstance(record, dict) or not all(type(record.get(key)) is int for key in
                                                  ("revision", "outstanding_breaks", "completion_sequence")):
            raise RuntimeError("Expected an observed revision, break count and completion sequence.")
        raw = (CMD / "testdata" / f"statement-r{revision}.json").read_bytes()
        statement_id = f"custody-batch.v1.synthetic-source.r{revision}." + hashlib.sha256(raw).hexdigest()
        run_id = record.get("run_id")
        expected = {
            "revision": revision, "statement_id": statement_id, "run_id": run_id,
            "business_date": json.loads(raw)["statement"]["businessDate"].split("T")[0],
            "outcome": "breaks" if revision == 1 else "clean",
            "outstanding_breaks": 1 if revision == 1 else 0, "completion_sequence": sequence,
        }
        if rerun and sequence == 2:
            request_id = record.get("request_id", "")
            parsed = uuid.UUID(request_id)
            if parsed.version != 4 or str(parsed) != request_id or run_id != "manual:" + request_id:
                raise RuntimeError("The operator receipt does not identify its original request.")
            expected.update(request_id=request_id, actor="custody-pilot-rerun-authorized")
        if not isinstance(run_id, str) or not run_id or run_id in runs or record != expected:
            raise RuntimeError(f"Retained comparison differs from the signed pilot at revision {revision}.")
        runs.add(run_id)
    return records


def qualify(docker, container, binaries, node, env, observations, rerun):
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
        "CREATE ROLE pilot_migrator NOLOGIN NOSUPERUSER NOBYPASSRLS;\n"
        f"CREATE ROLE pilot_check LOGIN NOSUPERUSER NOBYPASSRLS PASSWORD '{owner_password}';\n"
        f"CREATE DATABASE {DATABASE} OWNER pilot_migrator;\n"
    ).encode())
    for migration in sorted((CMD.parents[1] / "migrations").glob("*.sql")):
        try:
            sql(DATABASE, b"SET ROLE pilot_migrator;\n" + migration.read_bytes())
        except subprocess.CalledProcessError:
            raise RuntimeError(f"Accounting migration failed at {migration.name}.") from None
    authority = json.loads((CMD / "testdata/authority.json").read_bytes())
    # psql resolves the read-only profile's relative include beside its entrypoint.
    grants = "signed_fixture_grants.sql" if rerun else "pilot_grants.sql"
    for filename in {"signed_fixture_grants.sql", grants}:
        subprocess.run([*docker, "cp", str(CMD / filename), f"{container}:/tmp/{filename}"], check=True, env=env)
    sql(DATABASE, b"", "-v", "public_key=" + authority["public_key"], "-f", "/tmp/" + grants,
        *(["--single-transaction"] if rerun else []))
    if not rerun:
        sql(DATABASE, b"", "-v", "review_fence_only=on", "-f", "/tmp/pilot_grants.sql")
    sql(DATABASE, b"""
        GRANT USAGE ON SCHEMA public TO pilot_check;
        GRANT SELECT ON custody_batch_receipts, ledger_snapshots TO pilot_check;
        GRANT SELECT, INSERT ON ledger_entries, ledger_append_positions, custody_runs, custody_actions TO pilot_check;
        GRANT SELECT, INSERT, UPDATE ON ledger_heads, custody_statements, custody_breaks, outbox TO pilot_check;
        GRANT SELECT, INSERT, UPDATE, DELETE ON custody_break_detections TO pilot_check;
        GRANT USAGE ON SEQUENCE outbox_id_seq TO pilot_check;
    """)
    if rerun:
        sql(DATABASE, b"GRANT UPDATE(manual_receipt) ON custody_runs TO pilot_check")
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
        CUSTODY_PILOT_BROWSER_SCRIPT=str(WEB / "e2e" / ("custody-rerun.cjs" if rerun else "custody-detection.cjs")),
        CUSTODY_PILOT_RECEIPT=str(observations),
    )
    test = "TestSignedCustodyAuthorizedRerun" if rerun else "TestSignedCustodyOperatorReadback"
    proof = subprocess.run([binaries["accounting"], "-test.run=^" + test + "$",
                            "-test.v", "-test.count=1"], cwd=CMD, env=pilot_env, capture_output=True, timeout=155)
    output = proof.stdout
    if (proof.returncode or b"--- SKIP" in output or ("--- PASS: " + test).encode() not in output
            or b"signed gateway and Chromium verified the persisted detection" not in output):
        stage = re.search(rb"custody-browser-stage=(input|launch|(?:authorized|foreign|excluded|unfunded)-(?:open|reload)-(?:response|payload|row|details))", output)
        suffix = " at " + stage.group(1).decode() if stage else ""
        raise RuntimeError(test + " failed" + suffix + ".")
    return verify_observations(observations, rerun)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--node", default="node", help="Node executable matching web/.nvmrc")
    parser.add_argument("--pull-image", action="store_true", help="Fetch the pinned PostgreSQL image before startup")
    parser.add_argument("--receipt", type=Path, help="Write the verified synthetic comparison results as JSON")
    args = parser.parse_args()
    node = shutil.which(args.node)
    if not node:
        raise RuntimeError("Node executable unavailable.")
    env = host_environment()
    docker = local_engine(env)
    if args.pull_image:
        subprocess.run([*docker, "pull", IMAGE], check=True, env=env)
    with tempfile.TemporaryDirectory(prefix="kanz-custody-pilot-") as directory:
        binaries = build(node, Path(directory), env)
        results = {"postgres_image": IMAGE}
        for key, rerun in (("comparisons", False), ("operator_comparisons", True)):
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
                results[key] = qualify(docker, container, binaries, node, env, Path(directory) / (key + ".json"), rerun)
            finally:
                subprocess.run([*docker, "rm", "--force", container], env=env, check=True, stdout=subprocess.DEVNULL)
    if args.receipt:
        args.receipt.write_text(json.dumps(results, indent=2) + "\n",
                                encoding="utf-8")
    print("PASS: Chromium verified the retained signed break on load and reload, the three restricted "
          "sessions and read-only controls. A separate authorized browser rerun produced one durable receipt and FACT. The correction cleared both queues.")


if __name__ == "__main__":
    try:
        main()
    except (OSError, ValueError, RuntimeError, subprocess.SubprocessError) as error:
        sys.exit(str(error))
