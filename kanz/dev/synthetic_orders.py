"""Run the OMS checks against disposable Linux processes with no network egress."""
import argparse
import hashlib
import os
from pathlib import Path
import re
import secrets
import shutil
import subprocess
import tempfile
import time

ROOT = Path(__file__).resolve().parents[2]
PG = "postgres:16-alpine@sha256:1fb72075a1de85a1d39de23c2da81baef1b71bf5c0b61a14cbbfdb072a6246d9"
RUNTIME = "ghcr.io/eighred/base/rust@sha256:3914072ca0c3b8aad871db9169a651ccfce30cf58303e5d6f2db16d1d8a7e58f"
DB = "kanz_synthetic_oms"
OWNERS = {
    "services/api_gateway/cmd/api_gateway": [
        "TestSyntheticOrderAuthenticatedIngress", "TestSyntheticOrderControlEvidence"],
    "services/oms/internal/order": [
        "TestPostgresCreateIsAtomicAdmissionGate",
        "TestPostgresCreateFailingAtCommitLeavesNoOutboxRecord",
        "TestPostgresSaveFailingAtCommitLeavesNoOutboxRecord",
        "TestCancel_ArrivingMidExecutionCannotResurrectACancelledOrder",
        "TestCancel_UnknownWithdrawalBoundaryCannotProduceSuccess",
        "TestCancel_LostPlacementAckAndRouterCannotBecomePaperCancellation",
        "TestHandleRefusesAnotherTenantsCommand"],
    "services/oms/internal/position": ["TestTenantPositionReplayAfterProcessExit"],
}


def output(*args, **kwargs):
    return subprocess.check_output(args, text=True, **kwargs).strip()


def sql(container, statement, database="postgres"):
    # The bootstrap role is available only through the database container's
    # Unix socket. It has no network password. Never echo provisioning SQL.
    subprocess.run(["docker", "exec", "-i", "--user", "postgres", container, "psql",
                    "-Xq", "-v", "ON_ERROR_STOP=1", "-U", "postgres", "-d", database],
                   input=statement.encode("utf-8"), check=True, stdout=subprocess.DEVNULL)


def build(stage, packages):
    env = dict(os.environ, GOOS="linux", GOARCH="amd64", CGO_ENABLED="0", GOWORK="off")
    for package in packages:
        name = package.rsplit("/", 1)[-1]
        subprocess.run(["go", "test", "-mod=readonly", "-p", "1", "-c", "-o",
                        stage / name, "./" + package], cwd=ROOT / "kanz", env=env, check=True)
        (stage / package).mkdir(parents=True)
    shutil.copytree(ROOT / "kanz/services/oms/migrations", stage / "services/oms/migrations")


def require_tests(log, names):
    passed = re.findall(r"^--- PASS: (\S+)", log, flags=re.MULTILINE)
    if sorted(passed) != sorted(names) or re.search(r"^\s*--- (SKIP|FAIL):", log, re.MULTILINE):
        raise RuntimeError("missing, duplicate, skipped or failed test in synthetic fixture")
    return len(passed)


ROLES = {
    "CONTROL": "kanz_synthetic_oms_control",
    "RUNTIME": "kanz_synthetic_oms_runtime",
    "ADAPTER": "kanz_synthetic_oms_adapter",
    "VENUE": "kanz_synthetic_venue",
    "READ": "kanz_synthetic_oms_read",
    "RELAY": "kanz_synthetic_oms_relay",
}


def provision_native(container, fixture_env):
    for label, role in ROLES.items():
        password = secrets.token_hex(24)
        sql(container, f"CREATE ROLE {role} LOGIN NOSUPERUSER NOBYPASSRLS PASSWORD '{password}';")
        fixture_env[f"SYNTHETIC_OMS_{label}_URL"] = f"postgres://{role}:{password}@127.0.0.1:5432/{DB}?sslmode=disable"
    migrations = sorted((ROOT / "kanz/services/oms/migrations").glob("*.sql"))
    if not migrations:
        raise RuntimeError("no canonical OMS migrations")
    for migration in [*migrations, ROOT / "rust/services/synthetic_oms/schema.sql",
                      ROOT / "kanz/services/oms/tools/syntheticvenue/schema.sql"]:
        sql(container, migration.read_text(encoding="utf-8"), DB)
    sql(container, "GRANT SELECT ON orders TO kanz_synthetic_oms_read; "
                   "GRANT SELECT, UPDATE(published_at,attempts,last_error) ON outbox TO kanz_synthetic_oms_relay;", DB)


    # Exercise the ambiguous-COMMIT API. The first admitted order fails only
    # after the canonical order and its outbox entry have both been written.
    # A sequence is deliberately nontransactional so a verified retry succeeds.
    sql(container, """
        CREATE SEQUENCE synthetic_commit_probe;
        GRANT USAGE ON SEQUENCE synthetic_commit_probe TO kanz_synthetic_oms_runtime;
        CREATE FUNCTION synthetic_reject_first_commit() RETURNS trigger
        LANGUAGE plpgsql AS $$ BEGIN
          IF nextval('public.synthetic_commit_probe') = 1 THEN
            RAISE EXCEPTION 'synthetic deferred commit failure';
          END IF;
          RETURN NULL;
        END $$;
        CREATE CONSTRAINT TRIGGER synthetic_deferred_failure AFTER INSERT ON orders
          DEFERRABLE INITIALLY DEFERRED FOR EACH ROW
          EXECUTE FUNCTION synthetic_reject_first_commit();
    """, DB)


def run(stage, native):
    packages = {path: list(names) for path, names in OWNERS.items()}
    if native:
        packages["services/api_gateway/cmd/api_gateway"].append("TestSyntheticOrderCommandLifecycle")
        shutil.copyfile(native, stage / "native")
        (stage / "native").chmod(0o755)
        with native.open("rb") as binary:
            print("Rust owner SHA-256", hashlib.file_digest(binary, "sha256").hexdigest(), flush=True)
    build(stage, packages)
    # Local peer auth is for migration setup only. All other local users and
    # all remote administrators are refused. Only the test user gets a SCRAM DSN.
    hba = stage / "pg_hba.conf"
    hba.write_text("local all postgres peer\nlocal all all reject\n"
                   f"host {DB} {','.join(['kanz_synthetic_test', *ROLES.values()])} 127.0.0.1/32 scram-sha-256\n"
                   "host all all 0.0.0.0/0 reject\nhost all all ::/0 reject\n", encoding="ascii")
    with_env = dict(os.environ, POSTGRES_PASSWORD=secrets.token_hex(32))
    container = output("docker", "run", "-d", "--rm", "--network", "none",
                       "--tmpfs", "/var/lib/postgresql/data", "-e", "POSTGRES_PASSWORD",
                       "-v", f"{hba}:/fixture-hba:ro", PG,
                       "-c", "hba_file=/fixture-hba", env=with_env)
    try:
        for _ in range(60):
            ready = subprocess.run(["docker", "exec", "--user", "postgres", container,
                                    "pg_isready", "-h", "127.0.0.1"], capture_output=True)
            if ready.returncode == 0:
                break
            time.sleep(0.5)
        else:
            raise RuntimeError("disposable database did not start")
        password = secrets.token_hex(24)
        sql(container, f"CREATE ROLE kanz_synthetic_test LOGIN NOSUPERUSER NOBYPASSRLS PASSWORD '{password}';\n"
                       f"CREATE DATABASE {DB} OWNER kanz_synthetic_test;")
        posture = output("docker", "exec", "--user", "postgres", container, "psql", "-XAt", "-d", DB,
                         "-c", "SELECT current_setting('server_version'),rolsuper,rolbypassrls "
                         "FROM pg_roles WHERE rolname='kanz_synthetic_test'")
        if not posture.endswith("|f|f"):
            raise RuntimeError("test role bypasses tenant RLS")
        print("PostgreSQL", posture, flush=True)
        dsn = f"postgres://kanz_synthetic_test:{password}@127.0.0.1:5432/{DB}?sslmode=disable"
        fixture_env = {"TEST_POSTGRES_URL": dsn}
        if native:
            provision_native(container, fixture_env)
            fixture_env["KANZ_SYNTHETIC_OMS_BIN"] = "/fixture/native"
        for package, names in packages.items():
            base = package.rsplit("/", 1)[-1]
            selected_env = fixture_env if base == "api_gateway" else {"TEST_POSTGRES_URL": dsn}
            # Only the temporary test binaries, working directories and
            # migrations are mounted. Nothing from the user's home is visible.
            cmd = ["docker", "run", "--rm", "--platform", "linux/amd64", "--read-only",
                   "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--user", "65532:65532",
                   "--network", "container:" + container, "--tmpfs", "/tmp:exec,mode=1777",
                   "-v", f"{stage}:/fixture:ro", "-w", "/fixture/" + package,
                   *[arg for name in selected_env for arg in ("-e", name)],
                   "--entrypoint", "/fixture/" + base, RUNTIME if native else PG,
                   "-test.v", "-test.count=1", "-test.parallel=1", "-test.timeout=2m",
                   "-test.run", "^(" + "|".join(names) + ")$"]
            proc = subprocess.run(cmd, env=dict(os.environ, **selected_env),
                                  stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True, timeout=150)
            log = proc.stdout.replace(password, "[redacted]")
            for value in fixture_env.values():
                if value.startswith("postgres://"):
                    log = log.replace(value, "[fixture connection]")
            if proc.returncode:
                raise RuntimeError(log)
            count = require_tests(log, names)
            print(f"{package}: {count} named tests passed without skips", flush=True)
    finally:
        subprocess.run(["docker", "stop", "-t", "1", container], stdout=subprocess.DEVNULL, check=True)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    mode = parser.add_mutually_exclusive_group(required=True)
    mode.add_argument("--native", type=Path, help="run the complete slice with this checkout's Linux amd64 kanz-synthetic-oms")
    mode.add_argument("--owners-only", action="store_true", help="run existing component checks without native integration")
    args = parser.parse_args()
    with tempfile.TemporaryDirectory(prefix="kanz-synthetic-orders-") as directory:
        run(Path(directory), args.native)
