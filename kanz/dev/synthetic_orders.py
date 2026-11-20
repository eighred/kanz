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
FUNDING_DIAGNOSTIC = "0026_funded_order_refusal.sql"
OWNERS = {
    "services/api_gateway/cmd/api_gateway": [
        "TestSyntheticOrderAuthenticatedIngress", "TestSyntheticOrderControlEvidence"],
    "services/oms/internal/order": [
        "TestPostgresCapitalActivationRefusesLegacyAndOldWriter",
        "TestPostgresFundedAdmissionCompetitionAndRedelivery",
        "TestPostgresFundedAdmissionOutboxFailureRollsBackCash",
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
    "SWITCH": "kanz_synthetic_handoff_probe",
    "NEXT_RUNTIME": "kanz_synthetic_oms_runtime_g2",
    "NEXT_ADAPTER": "kanz_synthetic_oms_adapter_g2",
    "RECOVERY_RUNTIME": "kanz_synthetic_oms_runtime_g3",
    "RECOVERY_ADAPTER": "kanz_synthetic_oms_adapter_g3",
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
    diagnostic = next(i for i, path in enumerate(migrations) if path.name == FUNDING_DIAGNOSTIC)
    for migration in migrations[:diagnostic]:
        sql(container, migration.read_text(encoding="utf-8"), DB)
    owner_schema = (ROOT / "rust/services/synthetic_oms/schema.sql").read_text(encoding="utf-8")
    sql(container, owner_schema, DB)
    # Populate the actual pre-fence schema. The atomic upgrade must preserve
    # these bytes and must never invent a generation for a legacy command.
    sql(container, """
        BEGIN;
        SET LOCAL app.tenant_id='synthetic-legacy-probe';
        INSERT INTO orders(tenant_id,order_id,status,state,portfolio_id,parent_order_id)
            VALUES('synthetic-legacy-probe','legacy',1,decode('00','hex'),'legacy','');
        INSERT INTO synthetic_oms_commands(order_id,principal_id,portfolio_id,idempotency_key,command_sha256,command,evidence_ids)
            VALUES('legacy','user:legacy','legacy','legacy',sha256(decode('00','hex')),decode('00','hex'),ARRAY['a','b','c','d','e']);
        COMMIT;
    """, DB)
    writer_schema = ROOT / "rust/services/synthetic_oms/writer.sql"
    sql(container, writer_schema.read_text(encoding="utf-8"), DB)
    sql(container, """
        BEGIN; SET LOCAL app.tenant_id='synthetic-legacy-probe';
        DO $$ BEGIN
            BEGIN
                PERFORM synthetic_oms_handoff(0,'kanz_synthetic_oms_runtime','kanz_synthetic_oms_adapter');
                RAISE EXCEPTION 'unowned legacy state admitted';
            EXCEPTION WHEN SQLSTATE 'P1533' THEN NULL;
            END;
            IF (SELECT count(*) FROM orders WHERE order_id='legacy' AND state=decode('00','hex'))<>1
               OR (SELECT count(*) FROM synthetic_oms_commands WHERE order_id='legacy' AND writer_epoch IS NULL)<>1
               OR (SELECT epoch FROM synthetic_oms_writer WHERE singleton)<>0 THEN
                RAISE EXCEPTION 'legacy state mutated';
            END IF;
        END $$;
        DELETE FROM synthetic_oms_commands WHERE tenant_id='synthetic-legacy-probe' AND order_id='legacy';
        DELETE FROM orders WHERE tenant_id='synthetic-legacy-probe' AND order_id='legacy';
        COMMIT;
    """, DB)
    venue_schema = ROOT / "kanz/services/oms/tools/syntheticvenue/schema.sql"
    sql(container, venue_schema.read_text(encoding="utf-8"), DB)
    sql(container, """
        GRANT kanz_synthetic_oms_runtime TO kanz_synthetic_oms_runtime_g2;
        GRANT kanz_synthetic_oms_adapter TO kanz_synthetic_oms_adapter_g2;
        GRANT kanz_synthetic_oms_runtime TO kanz_synthetic_oms_runtime_g3;
        GRANT kanz_synthetic_oms_adapter TO kanz_synthetic_oms_adapter_g3;
        DO $$ BEGIN
            BEGIN
                PERFORM synthetic_oms_assert_writer();
                RAISE EXCEPTION 'unenrolled generation admitted';
            EXCEPTION WHEN SQLSTATE 'P1533' THEN NULL;
            END;
        END $$;
        SELECT synthetic_oms_handoff(0,'kanz_synthetic_oms_runtime','kanz_synthetic_oms_adapter');
        BEGIN;
        DELETE FROM synthetic_oms_writer_history WHERE epoch=1;
        SET LOCAL SESSION AUTHORIZATION kanz_synthetic_oms_runtime;
        DO $$ BEGIN
            BEGIN
                PERFORM synthetic_oms_assert_writer();
                RAISE EXCEPTION 'incomplete writer history admitted';
            EXCEPTION WHEN SQLSTATE 'P1533' THEN NULL;
            END;
        END $$;
        ROLLBACK;
        CREATE FUNCTION synthetic_test_handoff() RETURNS bigint LANGUAGE sql SECURITY DEFINER
            SET search_path=pg_catalog,public,pg_temp AS $$
            SELECT public.synthetic_oms_handoff(1,'kanz_synthetic_oms_runtime_g2','kanz_synthetic_oms_adapter_g2') $$;
        REVOKE ALL ON FUNCTION synthetic_test_handoff() FROM PUBLIC;
        GRANT EXECUTE ON FUNCTION synthetic_test_handoff() TO kanz_synthetic_handoff_probe;
        CREATE FUNCTION synthetic_test_recovery_handoff() RETURNS bigint LANGUAGE sql SECURITY DEFINER
            SET search_path=pg_catalog,public,pg_temp AS $$
            SELECT public.synthetic_oms_handoff(2,'kanz_synthetic_oms_runtime_g3','kanz_synthetic_oms_adapter_g3') $$;
        REVOKE ALL ON FUNCTION synthetic_test_recovery_handoff() FROM PUBLIC;
        GRANT EXECUTE ON FUNCTION synthetic_test_recovery_handoff() TO kanz_synthetic_handoff_probe;
        GRANT SELECT ON synthetic_oms_writer_history TO kanz_synthetic_handoff_probe;
    """, DB)
    sql(container, "GRANT SELECT ON orders TO kanz_synthetic_oms_read; "
                   "GRANT SELECT, UPDATE(published_at,attempts,last_error) ON outbox TO kanz_synthetic_oms_relay;", DB)

    # An activated tenant has known cash but no reservation for its next
    # command. Only the fixture operator seeds this independent estate.
    sql(container, """
        ALTER TABLE orders ADD CONSTRAINT synthetic_refuse_order CHECK (tenant_id<>'synthetic-store-refusal');
        BEGIN; SET LOCAL app.tenant_id='synthetic-legacy-armed';
        INSERT INTO capital_activation(enabled,activated_at) VALUES(true,clock_timestamp());
        SET LOCAL app.tenant_id='synthetic-armed';
        INSERT INTO capital_activation(enabled,activated_at) VALUES(true,clock_timestamp());
        INSERT INTO capital_balances(portfolio_id,currency,revision,coverage_complete,total)
            VALUES('synthetic-portfolio','USD',1,true,'1000');
        DO $$ DECLARE principal name; BEGIN
            FOR principal IN SELECT rolname FROM pg_roles WHERE NOT rolsuper AND (
                    pg_has_role(oid,'kanz_synthetic_oms_runtime','MEMBER') OR
                    pg_has_role(oid,'kanz_synthetic_oms_adapter','MEMBER')) LOOP
                IF pg_has_role(principal,'kanz_synthetic_activation_guard','MEMBER') OR EXISTS (
                    SELECT FROM pg_class WHERE relnamespace='public'::regnamespace
                      AND starts_with(relname,'capital_') AND relkind='r' AND
                      (has_any_column_privilege(principal,oid,'SELECT,INSERT,UPDATE,REFERENCES') OR
                       has_table_privilege(principal,oid,'DELETE,TRUNCATE,TRIGGER'))) THEN
                    RAISE EXCEPTION 'synthetic writer has direct capital privileges';
                END IF;
            END LOOP;
        END $$;
        COMMIT;
    """, DB)

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
    return migrations[diagnostic:]


def run_fixture_tests(cmd, names, selected_env, password):
    proc = subprocess.run(cmd + ["-test.run", "^(" + "|".join(names) + ")$"],
                          env=dict(os.environ, **selected_env),
                          stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True, timeout=150)
    log = proc.stdout.replace(password, "[redacted]")
    for value in selected_env.values():
        if value.startswith("postgres://"):
            log = log.replace(value, "[fixture connection]")
    if proc.returncode:
        raise RuntimeError(log)
    require_tests(log, names)
    return log


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
            pending_migrations = provision_native(container, fixture_env)
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
                   "-test.v", "-test.count=1", "-test.parallel=1", "-test.timeout=2m"]
            if native and base == "api_gateway":
                run_fixture_tests(cmd, ["TestSyntheticLegacyFundingRefusal"], selected_env, password)
                print(f"{package}: legacy funding refusal passed without skips", flush=True)
                for migration in pending_migrations:
                    sql(container, migration.read_text(encoding="utf-8"), DB)
            log = run_fixture_tests(cmd, names, selected_env, password)
            if native and base == "api_gateway":
                for name in ("armed_without_reservation", "unrelated_order_constraint", "fenced_restart", "unknown_across_handoff"):
                    case = rf"^\s*--- PASS: TestSyntheticOrderCommandLifecycle/{name} "
                    if len(re.findall(case, log, re.MULTILINE)) != 1:
                        raise RuntimeError("missing writer recovery proof: " + name)
                sql(container, """
                    DO $$ BEGIN
                        IF NOT EXISTS (SELECT FROM pg_proc
                                WHERE oid='public.require_funded_order_after_activation()'::regprocedure
                                  AND proowner='kanz_synthetic_activation_guard'::regrole AND prosecdef
                                  AND proconfig=ARRAY['search_path=pg_catalog, public, pg_temp'])
                           OR has_function_privilege('kanz_synthetic_oms_runtime',
                                'public.require_funded_order_after_activation()', 'EXECUTE') THEN
                            RAISE EXCEPTION 'funding diagnostic changed trigger authority';
                        END IF;
                        IF (SELECT count(*) FROM capital_balances WHERE tenant_id='synthetic-armed')<>1
                           OR NOT EXISTS (SELECT FROM capital_balances WHERE tenant_id='synthetic-armed'
                            AND portfolio_id='synthetic-portfolio' AND currency='USD'
                            AND revision=1 AND total='1000' AND reserved='0')
                           OR EXISTS (SELECT FROM capital_members WHERE tenant_id='synthetic-armed')
                           OR EXISTS (SELECT FROM capital_commitments WHERE tenant_id='synthetic-armed')
                           OR NOT EXISTS (SELECT FROM capital_activation WHERE tenant_id='synthetic-armed' AND enabled)
                           OR NOT EXISTS (SELECT FROM capital_activation WHERE tenant_id='synthetic-tenant' AND NOT enabled) THEN
                            RAISE EXCEPTION 'unexpected capital state after synthetic submissions';
                        END IF;
                    END $$;
                """, DB)
            print(f"{package}: {len(names)} named tests passed without skips", flush=True)
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
