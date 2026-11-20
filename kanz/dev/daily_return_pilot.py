#!/usr/bin/env python3
"""Qualify the signed daily-return operator path on disposable loopback services."""
import argparse
import json
import os
from pathlib import Path
import re
import secrets
import shutil
import subprocess
import tempfile
import time

ROOT = Path(__file__).resolve().parents[2]
MODULE = ROOT / "kanz"
WEB = ROOT / "web"
IMAGE = "postgres@sha256:aa6cb33cf4c19c765cefcf48d96176258800870a9a1e930ad126200f9d825139"


def build(node, directory, env):
    npm = shutil.which("npm")
    if not npm:
        raise RuntimeError("npm unavailable")
    cli = Path(npm).resolve()
    if os.name == "nt":
        cli = cli.parent / "node_modules/npm/bin/npm-cli.js"
    subprocess.run([node, cli, "run", "build"], cwd=WEB, env=env, check=True)
    packages = {"ACCOUNTING_BIN": ("build", "./services/accounting/cmd/accounting"),
                "IMPORTER_BIN": ("build", "./services/accounting/cmd/return_basis"),
                "GATEWAY_PROBE": ("test", "./services/api_gateway/cmd/api_gateway"),
                "BFF_PROBE": ("test", "./services/web_bff/internal/server")}
    binaries = {}
    for name, (mode, package) in packages.items():
        path = directory / (name.lower() + (".exe" if os.name == "nt" else ""))
        compile_only = ["-c"] if mode == "test" else []
        subprocess.run(["go", mode, *compile_only, "-p", "1", "-o", path, package],
                       cwd=MODULE, env=env, check=True)
        binaries["RETURN_PILOT_" + name] = str(path)
    return binaries


def qualify(env):
    host = subprocess.check_output(["docker", "context", "inspect", "--format", "{{.Endpoints.docker.Host}}"],
                                   env=env, text=True).strip()
    if not host.startswith(("unix:///", "npipe:////./pipe/")):
        raise RuntimeError("use a local Docker engine")
    command = ["docker", "run", "-d", "--rm", "--pull=never", "--name", "kanz-return-" + secrets.token_hex(8),
               "-p", "127.0.0.1::5432", "--tmpfs", "/var/lib/postgresql/data", "--tmpfs", "/run/fixture",
               "-e", "POSTGRES_PASSWORD_FILE=/run/fixture/admin",
               "-e", "POSTGRES_INITDB_ARGS=--auth-local=peer --auth-host=scram-sha-256",
               "--entrypoint", "sh", IMAGE, "-ec",
               "umask 077; head -c 32 /dev/urandom | base64 > /run/fixture/admin; "
               "chown postgres /run/fixture/admin; exec docker-entrypoint.sh postgres"]
    container = subprocess.check_output(command, env=env, text=True).strip()

    def sql(database, statement):
        result = subprocess.run(["docker", "exec", "-i", "--user", "postgres", container,
                                 "psql", "-Xq", "-v", "ON_ERROR_STOP=1", "-U", "postgres", "-d", database],
                                input=statement.encode(), env=env, timeout=30,
                                stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        if result.returncode:
            raise RuntimeError("disposable Accounting fixture SQL failed")

    try:
        for _ in range(80):
            cmd = ["docker", "exec", "--user", "postgres", container, "pg_isready", "-h", "127.0.0.1"]
            if subprocess.run(cmd, env=env, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL).returncode == 0:
                break
            time.sleep(0.25)
        else:
            raise RuntimeError("disposable PostgreSQL unavailable")
        passwords = {role: secrets.token_hex(24) for role in ("owner", "source", "reader", "runtime")}
        sql("postgres", "\n".join(f"CREATE ROLE basis_{role} LOGIN NOSUPERUSER NOBYPASSRLS PASSWORD '{pw}';"
                                 for role, pw in passwords.items()) + "\nCREATE DATABASE basis OWNER basis_owner;")
        for path in sorted((MODULE / "services/accounting/migrations").glob("*.sql")):
            try:
                sql("basis", "SET ROLE basis_owner;\n" + path.read_text(encoding="utf8"))
            except RuntimeError:
                raise RuntimeError("Accounting migration failed: " + path.name) from None
        sql("basis", """
            GRANT SELECT ON return_basis_authorities, return_basis_receipts, return_basis_publications, return_basis_benchmark_authorities
                TO basis_source,basis_reader,basis_runtime;
            GRANT INSERT ON return_basis_receipts TO basis_source;
            GRANT INSERT (portfolio_id,currency,close_date,as_of) ON return_basis_publications TO basis_source;
            GRANT SELECT,INSERT ON outbox TO basis_source;
            GRANT USAGE ON SEQUENCE outbox_id_seq TO basis_source;
            GRANT SELECT,INSERT,UPDATE ON outbox TO basis_runtime;
            GRANT USAGE ON SEQUENCE outbox_id_seq TO basis_runtime;
            GRANT SELECT (tenant_id,portfolio_id,entry_id,venue_account_id,entry_type,cash_currency,
                cash,effective_time,knowledge_time,source_ref,created_at,instrument_id,quantity,price,
                action,settlement_status,settlement_date,execution_evidence) ON ledger_entries TO basis_source;
            GRANT SELECT (tenant_id,portfolio_id,movement_id,digest,payload) ON cash_commands TO basis_source;
            GRANT SELECT,INSERT ON cash_commands TO basis_runtime;
            GRANT SELECT ON execution_fee_revisions TO basis_runtime;
            GRANT INSERT ON ledger_entries,ledger_append_positions,ledger_heads TO basis_runtime;
            GRANT UPDATE ON ledger_heads TO basis_runtime;
            GRANT UPDATE (observed_payload) ON return_basis_publications TO basis_runtime;
            GRANT SELECT ON ledger_entries,ledger_heads,ledger_snapshots,ledger_append_positions,
                custody_runs,custody_breaks,custody_statements TO basis_runtime;
        """)
        endpoint = subprocess.check_output(["docker", "port", container, "5432/tcp"], env=env, text=True).strip()
        match = re.fullmatch(r"127\.0\.0\.1:([1-9]\d{0,4})", endpoint)
        if not match or int(match[1]) > 65535:
            raise RuntimeError("require exactly one loopback PostgreSQL port")
        test_env = dict(env, RETURN_PILOT_VERIFY="1")
        for role, pw in passwords.items():
            test_env["TEST_RETURN_BASIS_" + role.upper()] = f"postgres://basis_{role}:{pw}@{endpoint}/basis?sslmode=disable"
        proof = subprocess.run(["go", "test", "-p", "1", "-count=1", "-json", "./services/accounting/cmd/return_basis",
                                "-run", "^TestSignedReturnBasisAdmissionAndReplay$"], cwd=MODULE, env=test_env,
                               capture_output=True, timeout=120)
        records = [json.loads(line) for line in proof.stdout.splitlines()]
        names = [r.get("Test") for r in records if r["Action"] == "pass"]
        if (proof.returncode or "TestSignedReturnBasisAdmissionAndReplay" not in names
                or any(r["Action"] in {"fail", "skip"} for r in records)
                or b"real JetStream replay and signed browser read passed with 18 FACTs" not in proof.stdout
                or b"observed PostgreSQL bytes matched each original FACT before and after replay" not in proof.stdout):
            locations = re.findall(rb"\b(?:main_test|operator_test)\.go:\d+(?=: )", proof.stdout)
            stage = re.search(rb"\breturn-browser-stage=[a-z-]+", proof.stdout)
            # Only print the source location and browser stage. Runtime errors may carry a DSN.
            detail = f" at {locations[-1].decode()}" if locations else ""
            if stage:
                detail += f" ({stage[0].decode()})"
            raise RuntimeError("return pilot failed" + detail)
        return proof.stdout
    finally:
        subprocess.run(["docker", "rm", "--force", container], env=env, check=True, stdout=subprocess.DEVNULL)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--node", default="node")
    parser.add_argument("--receipt", type=Path, help="write the synthetic Go JSONL verification")
    args = parser.parse_args()
    node = shutil.which(args.node)
    if not node:
        raise RuntimeError("Node unavailable")
    names = ("PATH", "SystemRoot", "SystemDrive", "ComSpec", "PATHEXT", "TEMP", "TMP", "TMPDIR", "HOME", "USERPROFILE",
             "LOCALAPPDATA", "APPDATA", "USER", "LOGNAME", "XDG_RUNTIME_DIR", "PLAYWRIGHT_BROWSERS_PATH",
             "RUSTUP_HOME", "CARGO_HOME", "GOPATH", "GOCACHE", "GOTMPDIR")
    env = {name: os.environ[name] for name in names if name in os.environ}
    env["PATH"] = str(Path(node).parent) + os.pathsep + env["PATH"]
    env.update(TEST_MFA_NODE=node, TEST_MFA_PLAYWRIGHT=str(WEB / "node_modules/playwright"),
               TEST_MFA_STATIC_DIR=str(WEB / "dist"), RETURN_PILOT_BROWSER_SCRIPT=str(WEB / "e2e/daily-return.cjs"))
    with tempfile.TemporaryDirectory(prefix="kanz-daily-return-") as directory:
        env.update(build(node, Path(directory), env))
        receipt = qualify(env)
    if args.receipt:
        args.receipt.write_bytes(receipt)
    print("PASS: Signed daily returns, benchmark comparisons and reconciled position contributions, scoped browser and retained replay.")


if __name__ == "__main__":
    main()
